// SPDX-License-Identifier: AGPL-3.0-only

package ledgerpub

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerlog"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
)

// MemStore is a Store in memory with the checks of the stored one (the term guard, the rules of PutCheckpoint and SaveFrontier),
// for the tests of the collector, which must run where there is no PostgreSQL. Its hooks fail a write at a chosen point, which is
// how the tests cut a round at each of its writes.
type MemStore struct {
	mu sync.Mutex
	// Term is the stored term of the leader; KeysRevision the stored revision of the key set.
	term         int64
	keysRevision int64
	nextLog      int64
	nextDigest   int64
	logs         map[int64]*LogRow
	tiles        map[int64]*ledgerlog.MemStore
	checkpoints  map[int64]map[int64]Checkpoint
	digests      []DigestView
	firstAt      bool
	prefix       string
	writes       int

	// Keyring and Events are served as keyring.json and events/<seq>.
	Keyring []byte
	Events  [][]byte
	// BeforeWrite, when set, is called before every write with its name ("tile", "frontier", "pending", "checkpoint", "note",
	// "state", "retired", "unread", "digest", "log") and the number of the write (from 1) and may return an error that the write
	// returns without storing anything.
	BeforeWrite func(kind string, n int) error
	// Clock is the time of created rows (default time.Now).
	Clock func() time.Time
}

// NewMemStore returns an empty store whose stored term is term.
func NewMemStore(term scheduler.Term) *MemStore {
	return &MemStore{term: int64(term), logs: map[int64]*LogRow{}, tiles: map[int64]*ledgerlog.MemStore{}, checkpoints: map[int64]map[int64]Checkpoint{}, nextLog: 1, nextDigest: 1}
}

// SetTerm changes the stored term (a new leader took over).
func (m *MemStore) SetTerm(t scheduler.Term) { m.mu.Lock(); m.term = int64(t); m.mu.Unlock() }

// SetKeysRevision changes the stored revision of the key set.
func (m *MemStore) SetKeysRevision(r int64) { m.mu.Lock(); m.keysRevision = r; m.mu.Unlock() }

// Writes is the number of writes made so far (including the ones a hook refused).
func (m *MemStore) Writes() int { m.mu.Lock(); defer m.mu.Unlock(); return m.writes }

// FirstCheckpointMarked tells whether a checkpoint locked the prefix, and the prefix recorded.
func (m *MemStore) FirstCheckpointMarked() (bool, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.firstAt, m.prefix
}

func (m *MemStore) now() time.Time {
	if m.Clock != nil {
		return m.Clock()
	}
	return time.Now()
}

// guard is called with m.mu held: the term guard and the write hook.
func (m *MemStore) guard(term scheduler.Term, kind string) error {
	if int64(term) != m.term {
		return scheduler.ErrDeposed
	}
	m.writes++
	if m.BeforeWrite != nil {
		if err := m.BeforeWrite(kind, m.writes); err != nil {
			return err
		}
	}
	return nil
}

func clone(r *LogRow) LogRow {
	c := *r
	c.ChainID = append([]byte(nil), r.ChainID...)
	c.Root = append([]byte(nil), r.Root...)
	c.Frontier = append([]byte(nil), r.Frontier...)
	c.HeadBlockHash = append([]byte(nil), r.HeadBlockHash...)
	c.PendingEntry = append([]byte(nil), r.PendingEntry...)
	return c
}

// Logs implements Store.
func (m *MemStore) Logs(context.Context) ([]LogRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []LogRow
	for _, r := range m.logs {
		out = append(out, clone(r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// LogByOrigin implements Store.
func (m *MemStore) LogByOrigin(_ context.Context, origin string) (LogRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.logs {
		if r.Origin == origin {
			return clone(r), nil
		}
	}
	return LogRow{}, ErrNotFound
}

// CreateLog implements Store.
func (m *MemStore) CreateLog(_ context.Context, term scheduler.Term, row LogRow) (LogRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.logs {
		if r.Origin == row.Origin {
			return clone(r), nil
		}
	}
	if err := m.guard(term, "log"); err != nil {
		return LogRow{}, err
	}
	for _, r := range m.logs {
		if r.State == StateClosed || r.Kind != row.Kind {
			continue
		}
		if (row.Kind == KindChain && bytes.Equal(r.ChainID, row.ChainID)) || (row.Kind == KindAggregate && r.Database == row.Database) {
			return LogRow{}, fmt.Errorf("%w: another log of this chain or database is open", ErrConflict)
		}
	}
	row.ID = m.nextLog
	m.nextLog++
	row.State, row.Size, row.UpdatedAt = StateActive, 0, m.now()
	row.Root = emptyRoot()
	row.Frontier = ledgerlog.Frontier{}.Marshal()
	m.logs[row.ID] = &row
	m.tiles[row.ID] = ledgerlog.NewMemStore()
	c := clone(&row)
	return c, nil
}

type memTiles struct {
	m    *MemStore
	id   int64
	term scheduler.Term
}

func (t memTiles) GetTile(ctx context.Context, k ledgerlog.TileKey) ([]byte, error) {
	t.m.mu.Lock()
	s := t.m.tiles[t.id]
	t.m.mu.Unlock()
	if s == nil {
		return nil, ledgerlog.ErrNotFound
	}
	return s.GetTile(ctx, k)
}

func (t memTiles) PutTile(ctx context.Context, k ledgerlog.TileKey, data []byte) error {
	t.m.mu.Lock()
	s := t.m.tiles[t.id]
	if err := t.m.guard(t.term, "tile"); err != nil {
		t.m.mu.Unlock()
		return err
	}
	t.m.mu.Unlock()
	if s == nil {
		return ledgerlog.ErrNotFound
	}
	return s.PutTile(ctx, k, data)
}

func (t memTiles) Widths(ctx context.Context, level int, index uint64) ([]int, error) {
	t.m.mu.Lock()
	s := t.m.tiles[t.id]
	t.m.mu.Unlock()
	if s == nil {
		return nil, nil
	}
	return s.Widths(ctx, level, index)
}

// Tiles implements Store.
func (m *MemStore) Tiles(logID int64, term scheduler.Term) ledgerlog.Store {
	return memTiles{m: m, id: logID, term: term}
}

// RawTiles returns the in-memory tile store of a log (for damage in tests).
func (m *MemStore) RawTiles(logID int64) *ledgerlog.MemStore {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tiles[logID]
}

// SaveFrontier implements Store.
func (m *MemStore) SaveFrontier(_ context.Context, term scheduler.Term, logID, size int64, root, frontier, head []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.guard(term, "frontier"); err != nil {
		return err
	}
	r := m.logs[logID]
	if r == nil {
		return ErrNotFound
	}
	if r.State != StateActive {
		return fmt.Errorf("%w: the log is %s", ErrConflict, r.State)
	}
	if size < r.Size || (size == r.Size && !bytes.Equal(root, r.Root)) {
		return fmt.Errorf("%w: size %d after %d, or another root for the stored size", ErrConflict, size, r.Size)
	}
	r.Size, r.Root, r.Frontier, r.UpdatedAt = size, append([]byte(nil), root...), append([]byte(nil), frontier...), m.now()
	if head != nil {
		r.HeadBlockHash = append([]byte(nil), head...)
	}
	r.PendingEntry = nil
	return nil
}

// PutPending implements Store.
func (m *MemStore) PutPending(_ context.Context, term scheduler.Term, logID int64, entry []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.guard(term, "pending"); err != nil {
		return err
	}
	r := m.logs[logID]
	if r == nil {
		return ErrNotFound
	}
	r.PendingEntry = append([]byte(nil), entry...)
	return nil
}

// PutCheckpoint implements Store.
func (m *MemStore) PutCheckpoint(_ context.Context, term scheduler.Term, c Checkpoint, rev int64, markFirst bool, prefix string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.guard(term, "checkpoint"); err != nil {
		return err
	}
	if rev != m.keysRevision {
		return ErrKeysChanged
	}
	r := m.logs[c.LogID]
	if r == nil {
		return ErrNotFound
	}
	if r.State != StateActive || c.Size != r.Size || !bytes.Equal(c.Root, r.Root) {
		return fmt.Errorf("%w: the note is not for the stored tree of the log", ErrConflict)
	}
	cps := m.checkpoints[c.LogID]
	if cps == nil {
		cps = map[int64]Checkpoint{}
		m.checkpoints[c.LogID] = cps
	}
	if old, ok := cps[c.Size]; ok {
		if !bytes.Equal(old.Root, c.Root) {
			return fmt.Errorf("%w: another root for size %d", ErrConflict, c.Size)
		}
		return nil // the same tree, already signed: the note stays (a re-signing is ReplaceNote)
	}
	for s := range cps {
		if s > c.Size {
			return fmt.Errorf("%w: a smaller tree than one signed", ErrConflict)
		}
	}
	c.Note = append([]byte(nil), c.Note...)
	cps[c.Size] = c
	r.SignedAt = &c.SignedAt
	if markFirst && !m.firstAt {
		m.firstAt, m.prefix = true, prefix
	}
	return nil
}

// ReplaceNote implements Store.
func (m *MemStore) ReplaceNote(_ context.Context, term scheduler.Term, logID, size int64, note []byte, rev int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.guard(term, "note"); err != nil {
		return err
	}
	if rev != m.keysRevision {
		return ErrKeysChanged
	}
	old, ok := m.checkpoints[logID][size]
	if !ok {
		return ErrNotFound
	}
	if err := sameText(old.Note, note); err != nil {
		return err
	}
	old.Note = append([]byte(nil), note...)
	m.checkpoints[logID][size] = old
	return nil
}

// Checkpoint implements Store.
func (m *MemStore) Checkpoint(_ context.Context, logID, size int64) (Checkpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.checkpoints[logID][size]
	if !ok {
		return Checkpoint{}, ErrNotFound
	}
	return c, nil
}

// LatestCheckpoint implements Store.
func (m *MemStore) LatestCheckpoint(_ context.Context, logID int64) (Checkpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var best *Checkpoint
	for _, c := range m.checkpoints[logID] {
		c := c
		if best == nil || c.Size > best.Size {
			best = &c
		}
	}
	if best == nil {
		return Checkpoint{}, ErrNotFound
	}
	return *best, nil
}

// Checkpoints implements Store.
func (m *MemStore) Checkpoints(_ context.Context, logID, before int64, limit int) ([]Checkpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Checkpoint
	for _, c := range m.checkpoints[logID] {
		if before == 0 || c.Size < before {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Size > out[j].Size })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// SetState implements Store.
func (m *MemStore) SetState(_ context.Context, term scheduler.Term, logID int64, state, detail string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.guard(term, "state"); err != nil {
		return err
	}
	r := m.logs[logID]
	if r == nil {
		return ErrNotFound
	}
	if r.State == StateClosed && state != StateClosed {
		return fmt.Errorf("%w: a closed log stays closed", ErrConflict)
	}
	r.State, r.Detail = state, detail
	if state == StateClosed && r.ClosedAt == nil {
		t := m.now()
		r.ClosedAt = &t
	}
	return nil
}

// SetRetired implements Store.
func (m *MemStore) SetRetired(_ context.Context, term scheduler.Term, logID, seq int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.guard(term, "retired"); err != nil {
		return err
	}
	r := m.logs[logID]
	if r == nil {
		return ErrNotFound
	}
	r.RetiredInSeq = &seq
	return nil
}

// SetUnread implements Store.
func (m *MemStore) SetUnread(_ context.Context, term scheduler.Term, logID int64, since *time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.guard(term, "unread"); err != nil {
		return err
	}
	r := m.logs[logID]
	if r == nil {
		return ErrNotFound
	}
	r.UnreadSince = since
	return nil
}

// PutDigest implements Store.
func (m *MemStore) PutDigest(_ context.Context, term scheduler.Term, v DigestView) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.guard(term, "digest"); err != nil {
		return 0, err
	}
	v.ID = m.nextDigest
	m.nextDigest++
	m.digests = append(m.digests, v)
	return v.ID, nil
}

// Digest implements Store.
func (m *MemStore) Digest(_ context.Context, database string, id int64) (DigestView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, v := range m.digests {
		if v.ID == id && v.Database == database {
			return v, nil
		}
	}
	return DigestView{}, ErrNotFound
}

// Digests implements Store.
func (m *MemStore) Digests(_ context.Context, database string, before int64, limit int) ([]DigestView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []DigestView
	for i := len(m.digests) - 1; i >= 0; i-- {
		v := m.digests[i]
		if v.Database == database && (before == 0 || v.ID < before) {
			out = append(out, v)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// LastDigestAt implements Store.
func (m *MemStore) LastDigestAt(_ context.Context, database string) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var t time.Time
	for _, v := range m.digests {
		if v.Database == database && v.GeneratedAt.After(t) {
			t = v.GeneratedAt
		}
	}
	return t, nil
}

// Object implements Store.
func (m *MemStore) Object(ctx context.Context, origin, rest string) (Object, error) {
	if origin == "" {
		switch {
		case rest == "logs.json":
			logs, err := m.Logs(ctx)
			if err != nil {
				return Object{}, err
			}
			return Object{Data: LogsJSON(logs), Mutable: true, ContentType: "application/json"}, nil
		case rest == "keyring.json":
			if m.Keyring == nil {
				return Object{}, ErrNotFound
			}
			return Object{Data: m.Keyring, Mutable: true, ContentType: "application/json"}, nil
		case len(rest) == len("events/000000") && rest[:7] == "events/":
			n, err := strconv.Atoi(rest[7:])
			if err != nil || n < 0 || n >= len(m.Events) {
				return Object{}, ErrNotFound
			}
			return Object{Data: m.Events[n], ContentType: "text/plain; charset=utf-8"}, nil
		}
		return Object{}, ErrNotFound
	}
	row, err := m.LogByOrigin(ctx, origin)
	if err != nil {
		return Object{}, err
	}
	r, err := ParseRest(rest)
	if err != nil {
		return Object{}, ErrNotFound
	}
	if r.Checkpoint {
		c, err := m.LatestCheckpoint(ctx, row.ID)
		if err != nil {
			return Object{}, err
		}
		return Object{Data: c.Note, Mutable: true, ContentType: "text/plain; charset=utf-8"}, nil
	}
	d, err := m.Tiles(row.ID, 0).GetTile(ctx, r.Tile)
	if err != nil {
		if errors.Is(err, ledgerlog.ErrNotFound) {
			return Object{}, ErrNotFound
		}
		return Object{}, err
	}
	return Object{Data: d, ContentType: "application/octet-stream"}, nil
}

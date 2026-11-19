// SPDX-License-Identifier: AGPL-3.0-only

package ledgeranchor

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
)

// MemStore is a Store in memory, with the behaviour of the one in PostgreSQL that the tests of the jobs rely on:
// the term guard (a write under another term is scheduler.ErrDeposed), the unique key of a record, the version
// of a row and the compare and swap on it. Hooks lets a test end a write at a named point (a crash).
type MemStore struct {
	mu      sync.Mutex
	term    scheduler.Term
	targets map[int64]*TargetRow
	rows    map[int64]*Row
	aggs    map[int64]*memAgg
	nextT   int64
	nextR   int64
	set     Settings
	// Hook, when set, is called at the start of every write with its kind ("target", "enqueue", "begin", "apply",
	// ...); an error it returns is the error of the write, which then changes nothing.
	Hook func(kind string) error
}

type memAgg struct {
	Aggregate
	cps map[int64]time.Time // size -> signed_at
}

// NewMemStore returns an empty store holding term 1 and the default settings.
func NewMemStore() *MemStore {
	return &MemStore{term: 1, targets: map[int64]*TargetRow{}, rows: map[int64]*Row{}, aggs: map[int64]*memAgg{},
		set: Settings{Interval: time.Hour, Minimum: 1}}
}

// SetTerm sets the stored term (a new leader).
func (m *MemStore) SetTerm(t scheduler.Term) { m.mu.Lock(); m.term = t; m.mu.Unlock() }

// SetSettings sets the anchoring settings.
func (m *MemStore) SetSettings(s Settings) { m.mu.Lock(); m.set = s; m.mu.Unlock() }

// PutCheckpoint adds a checkpoint to the aggregate log of a database (the log is made on the first call).
func (m *MemStore) PutCheckpoint(db, origin, aggregateID string, size int64, note []byte, signedAt time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var a *memAgg
	for _, x := range m.aggs {
		if x.Database == db {
			a = x
		}
	}
	if a == nil {
		a = &memAgg{Aggregate: Aggregate{LogID: int64(len(m.aggs) + 1), Database: db, Origin: origin, AggregateID: aggregateID}, cps: map[int64]time.Time{}}
		m.aggs[a.LogID] = a
	}
	a.cps[size] = signedAt
	if size >= a.Size {
		a.Size, a.Note, a.SignedAt = size, append([]byte(nil), note...), signedAt
	}
}

func (m *MemStore) guard(term scheduler.Term, kind string) error {
	if m.Hook != nil {
		if err := m.Hook(kind); err != nil {
			return err
		}
	}
	if term != m.term {
		return scheduler.ErrDeposed
	}
	return nil
}

func cloneRow(r Row) Row {
	r.Note = append([]byte(nil), r.Note...)
	r.Evidence = append(json.RawMessage(nil), r.Evidence...)
	return r
}

// Targets implements Store.
func (m *MemStore) Targets(context.Context) ([]TargetRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []TargetRow
	for _, t := range m.targets {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Target implements Store.
func (m *MemStore) Target(_ context.Context, id int64) (TargetRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.targets[id]; ok {
		return *t, nil
	}
	return TargetRow{}, ErrNotFound
}

func (m *MemStore) nameTaken(name string, except int64) bool {
	for _, t := range m.targets {
		if t.Name == name && t.ID != except {
			return true
		}
	}
	return false
}

// CreateTarget implements Store.
func (m *MemStore) CreateTarget(_ context.Context, term scheduler.Term, t TargetRow) (TargetRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.guard(term, "target"); err != nil {
		return TargetRow{}, err
	}
	if m.nameTaken(t.Name, 0) {
		return TargetRow{}, ErrExists
	}
	m.nextT++
	t.ID, t.Revision, t.CreatedAt, t.UpdatedAt = m.nextT, 1, time.Now().UTC(), time.Now().UTC()
	m.targets[t.ID] = &t
	return t, nil
}

// UpdateTarget implements Store.
func (m *MemStore) UpdateTarget(_ context.Context, term scheduler.Term, t TargetRow, expect int64) (TargetRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.guard(term, "target"); err != nil {
		return TargetRow{}, err
	}
	cur, ok := m.targets[t.ID]
	if !ok {
		return TargetRow{}, ErrNotFound
	}
	if cur.Revision != expect {
		return TargetRow{}, ErrConflict
	}
	if m.nameTaken(t.Name, t.ID) {
		return TargetRow{}, ErrExists
	}
	cur.Name, cur.Enabled, cur.EnabledSince, cur.Config, cur.CredentialsSet, cur.LastTest = t.Name, t.Enabled, t.EnabledSince, t.Config, t.CredentialsSet, t.LastTest
	cur.Revision++
	cur.UpdatedAt = time.Now().UTC()
	return *cur, nil
}

// DeleteTarget implements Store.
func (m *MemStore) DeleteTarget(_ context.Context, term scheduler.Term, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.guard(term, "target"); err != nil {
		return err
	}
	if _, ok := m.targets[id]; !ok {
		return ErrNotFound
	}
	delete(m.targets, id)
	for _, r := range m.rows {
		if r.TargetID == id {
			r.TargetGone = true
		}
	}
	return nil
}

// SetLastTest implements Store.
func (m *MemStore) SetLastTest(_ context.Context, term scheduler.Term, id int64, test json.RawMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.guard(term, "target"); err != nil {
		return err
	}
	t, ok := m.targets[id]
	if !ok {
		return ErrNotFound
	}
	t.LastTest = append(json.RawMessage(nil), test...)
	return nil
}

// Settings implements Store.
func (m *MemStore) Settings(context.Context) (Settings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.set, nil
}

// Aggregates implements Store.
func (m *MemStore) Aggregates(context.Context) ([]Aggregate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Aggregate
	for _, a := range m.aggs {
		if a.Size > 0 {
			out = append(out, a.Aggregate)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LogID < out[j].LogID })
	return out, nil
}

// AggregateOf implements Store.
func (m *MemStore) AggregateOf(_ context.Context, db string) (Aggregate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.aggs {
		if a.Database == db {
			if a.Size == 0 {
				return Aggregate{}, ErrNoAggregate
			}
			return a.Aggregate, nil
		}
	}
	return Aggregate{}, ErrNotFound
}

// FirstAfter implements Store.
func (m *MemStore) FirstAfter(_ context.Context, logID, size int64) (time.Time, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.aggs[logID]
	if !ok {
		return time.Time{}, false, nil
	}
	var best time.Time
	found := false
	for s, at := range a.cps {
		if s > size && (!found || at.Before(best)) {
			best, found = at, true
		}
	}
	return best, found, nil
}

func (m *MemStore) match(r *Row, f Filter) bool {
	if f.Database != "" && r.Database != f.Database {
		return false
	}
	if f.TargetID != 0 && (r.TargetGone || r.TargetID != f.TargetID) {
		return false
	}
	if f.State != "" && apiState(r.State) != f.State {
		return false
	}
	return f.Before == 0 || r.ID < f.Before
}

// Records implements Store.
func (m *MemStore) Records(_ context.Context, f Filter) ([]Row, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Row
	for _, r := range m.rows {
		if m.match(r, f) {
			c := cloneRow(*r)
			c.Note = nil
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

// Record implements Store.
func (m *MemStore) Record(_ context.Context, id int64) (Row, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.rows[id]; ok {
		return cloneRow(*r), nil
	}
	return Row{}, ErrNotFound
}

// Enqueue implements Store.
func (m *MemStore) Enqueue(_ context.Context, term scheduler.Term, e Enqueue) (Row, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.guard(term, "enqueue"); err != nil {
		return Row{}, false, err
	}
	var newest *Row
	for _, r := range m.rows {
		if !r.TargetGone && r.TargetID == e.Target.ID && logID(r.Record.LogID) == e.Agg.LogID {
			if r.Size == uint64(e.Agg.Size) { //nolint:gosec // checked by the migration
				return cloneRow(*r), false, nil
			}
			if newest == nil || r.Size > newest.Size {
				newest = r
			}
		}
	}
	if newest != nil && newest.Size > uint64(e.Agg.Size) { //nolint:gosec // as above
		return cloneRow(*newest), false, nil
	}
	digest, err := anchor.ParsePayload(e.Agg.AggregateID, e.Agg.Note)
	if err != nil {
		return Row{}, false, err
	}
	for _, r := range m.rows {
		if !r.TargetGone && r.TargetID == e.Target.ID && logID(r.Record.LogID) == e.Agg.LogID && r.State == anchor.StatePending {
			if s, err := r.Record.Supersede(); err == nil {
				r.Record = s
				r.Version++
			}
		}
	}
	m.nextR++
	rec := anchor.NewRecord(e.Target.ID, logKey(e.Agg.LogID), uint64(e.Agg.Size), digest.Digest, e.Now) //nolint:gosec // as above
	rec.ID = m.nextR
	row := &Row{Record: rec, TargetName: e.Target.Name, Kind: e.Target.Kind, Database: e.Agg.Database, AggregateID: e.Agg.AggregateID,
		Origin: e.Agg.Origin, Note: append([]byte(nil), e.Agg.Note...), Version: 1}
	m.rows[row.ID] = row
	return cloneRow(*row), true, nil
}

// Begin implements Store.
func (m *MemStore) Begin(_ context.Context, term scheduler.Term, id int64, now time.Time) (Row, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.guard(term, "begin"); err != nil {
		return Row{}, err
	}
	r, ok := m.rows[id]
	if !ok {
		return Row{}, ErrNotFound
	}
	if r.State != anchor.StatePending || r.TargetGone {
		return Row{}, ErrConflict
	}
	if r.SubmitStart.IsZero() {
		rec, err := r.Record.WithSubmitStart(now)
		if err != nil {
			return Row{}, err
		}
		r.Record = rec
		r.Version++
	}
	return cloneRow(*r), nil
}

// Apply implements Store.
func (m *MemStore) Apply(_ context.Context, term scheduler.Term, w Row, expect int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.guard(term, "apply"); err != nil {
		return err
	}
	r, ok := m.rows[w.ID]
	if !ok {
		return ErrNotFound
	}
	if r.Version != expect {
		return ErrConflict
	}
	keep := r.Note
	gone := r.TargetGone
	*r = cloneRow(w)
	r.Note, r.TargetGone = keep, gone
	r.Version = expect + 1
	return nil
}

// Due implements Store.
func (m *MemStore) Due(_ context.Context, now time.Time, limit int) ([]Row, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Row
	for _, r := range m.rows {
		t, ok := m.targets[r.TargetID]
		if r.TargetGone || !ok || !t.Enabled {
			continue
		}
		switch r.State {
		case anchor.StatePending, anchor.StateSubmitted, anchor.StateClaimed:
			if r.NextAttemptAt == nil || !r.NextAttemptAt.After(now) || r.State == anchor.StateClaimed {
				out = append(out, cloneRow(*r))
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// NewestAnchored implements Store.
func (m *MemStore) NewestAnchored(context.Context) ([]Anchored, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	best := map[[2]int64]Anchored{}
	for _, r := range m.rows {
		if r.State != anchor.StateAnchored || r.TargetGone || r.AnchoredAt == nil {
			continue
		}
		k := [2]int64{r.TargetID, logID(r.Record.LogID)}
		if cur, ok := best[k]; !ok || int64(r.Size) > cur.Size { //nolint:gosec // checked by the migration
			best[k] = Anchored{TargetID: r.TargetID, LogID: k[1], Size: int64(r.Size), AnchoredAt: *r.AnchoredAt, EvidenceTime: r.EvidenceTime} //nolint:gosec // as above
		}
	}
	var out []Anchored
	for _, a := range best {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TargetID < out[j].TargetID })
	return out, nil
}

// Failures implements Store.
func (m *MemStore) Failures(context.Context) ([]Row, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Row
	for _, r := range m.rows {
		if !r.TargetGone && (r.State == anchor.StateFailed || r.State == anchor.StateExpired) && r.Alert != "" {
			c := cloneRow(*r)
			c.Note = nil
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

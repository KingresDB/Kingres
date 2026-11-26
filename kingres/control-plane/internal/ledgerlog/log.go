// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ledgerlog

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Errors of opening a log. They say which side is ahead after a crash.
var (
	// ErrFrontierAhead: the frontier says the log has more leaves than the
	// tiles hold (a tile the frontier needs is missing). The tiles were lost or
	// the frontier came from a newer copy; the log must not be extended.
	ErrFrontierAhead = errors.New("ledgerlog: the frontier is ahead of the tiles")
	// ErrTilesAhead: the store holds tiles beyond the frontier (a crash between
	// writing tiles and saving the frontier). Appending the same leaves again
	// repairs it, because storing an identical tile is not an error; open with
	// Options.AllowTilesAhead to do so. Appending other leaves fails with
	// ErrConflict.
	ErrTilesAhead = errors.New("ledgerlog: the tiles are ahead of the frontier")
	// ErrTilesMismatch: tiles the frontier needs do not agree with it (damage).
	ErrTilesMismatch = errors.New("ledgerlog: the tiles do not match the frontier")
)

// Options of Open.
type Options struct {
	// AllowTilesAhead opens a log whose store holds tiles beyond the frontier
	// instead of failing with ErrTilesAhead.
	AllowTilesAhead bool
}

// Log is an append-only log in tile form: the frontier, the right edge of
// the tiles (the partial tile of each level and the partial entry bundle, which
// is all that a new leaf needs to read), and a Store. Append is serialised by an
// internal lock, but the caller must also serialise whole rounds (append, then
// persist the frontier, then sign and store the checkpoint).
type Log struct {
	mu     sync.Mutex
	origin string
	store  Store
	fr     Frontier
	edge   [maxLevel + 1][]Hash // the hashes of the partial tile of each level
	bundle [][]byte             // the entries of the partial entry bundle
}

// Create returns an empty log in the store. It fails when the store already
// holds tiles of a log (ErrTilesAhead).
func Create(ctx context.Context, origin string, store Store) (*Log, error) {
	l := &Log{origin: origin, store: store}
	if !ValidOrigin(origin) {
		return nil, fmt.Errorf("ledgerlog: the origin %q is not valid", clipStr(origin))
	}
	if err := l.checkAhead(ctx, false); err != nil {
		return nil, err
	}
	return l, nil
}

// Open returns the log whose frontier was stored with the given size and root:
// the frontier is parsed and its root must be the stored root (ErrFrontierRoot),
// then the right edge of the tiles is read and checked against the frontier
// (ErrFrontierAhead when a tile is missing, ErrTilesMismatch when one differs,
// each partial entry bundle against the leaf hashes), and the store is checked
// for tiles beyond the frontier (ErrTilesAhead). Only the tiles of the right
// edge are read.
func Open(ctx context.Context, origin string, store Store, frontier []byte, wantSize uint64, wantRoot Hash, opts Options) (*Log, error) {
	if !ValidOrigin(origin) {
		return nil, fmt.Errorf("ledgerlog: the origin %q is not valid", clipStr(origin))
	}
	fr, err := LoadFrontier(frontier, wantSize, wantRoot)
	if err != nil {
		return nil, err
	}
	l := &Log{origin: origin, store: store, fr: fr}
	if err := l.loadEdge(ctx); err != nil {
		return nil, err
	}
	if err := l.checkAhead(ctx, opts.AllowTilesAhead); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *Log) loadEdge(ctx context.Context) error {
	size := l.fr.size
	for lv := 0; lv <= maxLevel; lv++ {
		w := PartialWidth(size, lv)
		if w == 0 {
			continue
		}
		k := TileKey{lv, (size >> (TileHeight * uint(lv))) / TileWidth, w}
		data, err := l.store.GetTile(ctx, k)
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("%w: the tile %s of the log of size %d is missing", ErrFrontierAhead, k.Path(), size)
		}
		if err != nil {
			return err
		}
		hs, err := ParseTile(data, w)
		if err != nil {
			return fmt.Errorf("%w: %s: %v", ErrTilesMismatch, k.Path(), err)
		}
		l.edge[lv] = hs
	}
	if w := PartialWidth(size, 0); w > 0 {
		k := TileKey{EntryLevel, size / TileWidth, w}
		data, err := l.store.GetTile(ctx, k)
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("%w: the entry bundle %s of the log of size %d is missing", ErrFrontierAhead, k.Path(), size)
		}
		if err != nil {
			return err
		}
		es, err := ParseBundle(data, w)
		if err != nil {
			return fmt.Errorf("%w: %s: %v", ErrTilesMismatch, k.Path(), err)
		}
		for i, e := range es {
			if LeafHash(e) != l.edge[0][i] {
				return fmt.Errorf("%w: entry %d of %s does not have the leaf hash of the tile", ErrTilesMismatch, i, k.Path())
			}
		}
		l.bundle = es
	}
	// The partial tiles must produce exactly the hashes of the frontier.
	var want []Hash
	for lv := maxLevel; lv >= 0; lv-- {
		w, pos := len(l.edge[lv]), 0
		for b := TileHeight - 1; b >= 0; b-- {
			if w>>uint(b)&1 == 1 {
				n := 1 << uint(b)
				want = append(want, RootOf(l.edge[lv][pos:pos+n]))
				pos += n
			}
		}
	}
	if len(want) != len(l.fr.hashes) {
		return fmt.Errorf("%w: the right edge has %d subtrees, the frontier %d", ErrTilesMismatch, len(want), len(l.fr.hashes))
	}
	for i := range want {
		if want[i] != l.fr.hashes[i] {
			return fmt.Errorf("%w: subtree %d of the right edge differs from the frontier", ErrTilesMismatch, i)
		}
	}
	return nil
}

// checkAhead looks for tiles at or after the position of the next leaf: a
// level 0 tile or entry bundle wider than the frontier has, or any in the
// following tile.
func (l *Log) checkAhead(ctx context.Context, allow bool) error {
	size := l.fr.size
	cur := size / TileWidth
	have := PartialWidth(size, 0)
	for _, lv := range []int{EntryLevel, 0} {
		for _, idx := range []uint64{cur, cur + 1} {
			ws, err := l.store.Widths(ctx, lv, idx)
			if err != nil {
				return err
			}
			for _, w := range ws {
				if idx == cur && w <= have {
					continue
				}
				if !allow {
					return fmt.Errorf("%w: %s exists and the frontier has size %d", ErrTilesAhead, TileKey{lv, idx, w}.Path(), size)
				}
			}
		}
	}
	return nil
}

// Origin returns the origin of the log.
func (l *Log) Origin() string { return l.origin }

// Size returns the number of leaves.
func (l *Log) Size() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.fr.size
}

// Root returns the root of the tree.
func (l *Log) Root() Hash {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.fr.Root()
}

// Frontier returns a copy of the frontier, to be persisted by the caller after
// each Append.
func (l *Log) Frontier() Frontier {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.fr.Clone()
}

// Checkpoint returns the checkpoint text data of the log as it is now.
func (l *Log) Checkpoint() Checkpoint {
	l.mu.Lock()
	defer l.mu.Unlock()
	return Checkpoint{Origin: l.origin, Size: l.fr.size, Root: l.fr.Root()}
}

// Appended is the result of an Append.
type Appended struct {
	// Size and Root are those of the log after the append.
	Size uint64
	Root Hash
	// Frontier is the serialised frontier to persist.
	Frontier []byte
	// Tiles is the number of tiles and bundles put (equal ones count).
	Tiles int
}

type put struct {
	key  TileKey
	data []byte
}

// chunks cuts the hashes or entries that start at position base%256 of tile
// base/256 into tiles.
func chunkHashes(prefix, add []Hash, level int, firstTile uint64) ([]put, []Hash, error) {
	all := append(append([]Hash(nil), prefix...), add...)
	var out []put
	var last []Hash
	for i := 0; i < len(all); i += TileWidth {
		c := all[i:min(i+TileWidth, len(all))]
		data, err := EncodeTile(c)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, put{TileKey{level, firstTile + uint64(i/TileWidth), len(c)}, data})
		last = c
	}
	if len(last) == TileWidth {
		last = nil
	}
	return out, append([]Hash(nil), last...), nil
}

func chunkEntries(prefix, add [][]byte, firstTile uint64) ([]put, [][]byte, error) {
	all := append(append([][]byte(nil), prefix...), add...)
	var out []put
	var last [][]byte
	for i := 0; i < len(all); i += TileWidth {
		c := all[i:min(i+TileWidth, len(all))]
		data, err := EncodeBundle(c)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, put{TileKey{EntryLevel, firstTile + uint64(i/TileWidth), len(c)}, data})
		last = c
	}
	if len(last) == TileWidth {
		last = nil
	}
	cp := make([][]byte, len(last))
	for i, e := range last {
		cp[i] = append([]byte(nil), e...)
	}
	return out, cp, nil
}

// Append adds entries (leaf data) to the log: it computes the new tiles and
// bundles from the frontier and the right edge held in memory, stores them
// (bundles first, then level 0 upwards, each in index order), and only then
// updates its own state. The caller persists the returned frontier afterwards,
// and then signs and stores the checkpoint. If Append fails, its state is
// unchanged and calling it again with the same entries is safe: tiles that were
// stored already are identical and not an error. 1 to MaxBatch entries of 1 to
// MaxEntrySize bytes, MaxBatchBytes in all.
func (l *Log) Append(ctx context.Context, entries [][]byte) (Appended, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(entries) < 1 || len(entries) > MaxBatch {
		return Appended{}, fmt.Errorf("ledgerlog: %d entries, expected 1 to %d", len(entries), MaxBatch)
	}
	total := 0
	for _, e := range entries {
		if len(e) < 1 || len(e) > MaxEntrySize {
			return Appended{}, fmt.Errorf("ledgerlog: an entry has %d bytes, expected 1 to %d", len(e), MaxEntrySize)
		}
		total += len(e)
	}
	if total > MaxBatchBytes {
		return Appended{}, fmt.Errorf("ledgerlog: %d entry bytes, expected at most %d", total, MaxBatchBytes)
	}
	old := l.fr.size
	nf := l.fr.Clone()
	var newHashes [maxLevel + 1][]Hash
	emit := func(level int, idx uint64, h Hash) {
		if level <= maxLevel {
			newHashes[level] = append(newHashes[level], h)
		}
	}
	for _, e := range entries {
		if err := nf.appendLeaf(LeafHash(e), emit); err != nil {
			return Appended{}, err
		}
	}
	puts, newBundle, err := chunkEntries(l.bundle, entries, old/TileWidth)
	if err != nil {
		return Appended{}, err
	}
	var newEdge [maxLevel + 1][]Hash
	var touched [maxLevel + 1]bool
	for lv := 0; lv <= maxLevel; lv++ {
		if len(newHashes[lv]) == 0 {
			continue
		}
		first := (old >> (TileHeight * uint(lv))) / TileWidth
		p, edge, err := chunkHashes(l.edge[lv], newHashes[lv], lv, first)
		if err != nil {
			return Appended{}, err
		}
		puts = append(puts, p...)
		newEdge[lv], touched[lv] = edge, true
	}
	for _, p := range puts {
		if err := ctx.Err(); err != nil {
			return Appended{}, err
		}
		if err := l.store.PutTile(ctx, p.key, p.data); err != nil {
			return Appended{}, fmt.Errorf("ledgerlog: storing %s: %w", p.key.Path(), err)
		}
	}
	l.fr, l.bundle = nf, newBundle
	for lv := range newEdge {
		if touched[lv] {
			l.edge[lv] = newEdge[lv]
		}
	}
	return Appended{Size: nf.size, Root: nf.Root(), Frontier: nf.Marshal(), Tiles: len(puts)}, nil
}

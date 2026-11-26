// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ledgerlog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Errors of stores.
var (
	// ErrNotFound is returned by GetTile for a tile that is not stored.
	ErrNotFound = errors.New("ledgerlog: not found")
	// ErrConflict is returned by PutTile for a key that holds other content:
	// a published tile is never replaced. For a log it means that the leaves
	// differ from what was published, which is a divergence.
	ErrConflict = errors.New("ledgerlog: the tile exists with different content")
)

// Store keeps the tiles and entry bundles of one log. Implementations are safe
// for concurrent use.
//
//   - GetTile returns the content of the key or ErrNotFound.
//   - PutTile stores the content; storing the same content again is not an
//     error, other content for an existing key is ErrConflict and changes
//     nothing. The content is checked against the key (the length of a tile,
//     the form of a bundle) and refused when it is not valid.
//   - Widths lists, ascending, the widths stored for the tile with this level
//     and index (TileWidth for the full tile). Open uses it to find tiles that
//     are ahead of a frontier after a crash.
type Store interface {
	GetTile(ctx context.Context, k TileKey) ([]byte, error)
	PutTile(ctx context.Context, k TileKey, data []byte) error
	Widths(ctx context.Context, level int, index uint64) ([]int, error)
}

// CheckpointStore keeps the latest signed checkpoint of a log, the one mutable
// object of a publication: PutCheckpoint replaces it.
type CheckpointStore interface {
	GetCheckpoint(ctx context.Context) ([]byte, error)
	PutCheckpoint(ctx context.Context, note []byte) error
}

// MemStore is a Store (and CheckpointStore) in memory.
type MemStore struct {
	mu         sync.Mutex
	tiles      map[TileKey][]byte
	checkpoint []byte
	// BeforePut, when set, is called before every PutTile with the key and
	// may return an error that PutTile returns without storing (a hook for
	// tests of crashes).
	BeforePut func(k TileKey) error
}

// NewMemStore returns an empty MemStore.
func NewMemStore() *MemStore { return &MemStore{tiles: map[TileKey][]byte{}} }

// GetTile implements Store.
func (m *MemStore) GetTile(ctx context.Context, k TileKey) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.tiles[k]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, k.Path())
	}
	return append([]byte(nil), d...), nil
}

// PutTile implements Store.
func (m *MemStore) PutTile(ctx context.Context, k TileKey, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkTileData(k, data); err != nil {
		return err
	}
	if m.BeforePut != nil {
		if err := m.BeforePut(k); err != nil {
			return err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.tiles[k]; ok {
		if !bytes.Equal(old, data) {
			return fmt.Errorf("%w: %s", ErrConflict, k.Path())
		}
		return nil
	}
	m.tiles[k] = append([]byte(nil), data...)
	return nil
}

// Widths implements Store.
func (m *MemStore) Widths(ctx context.Context, level int, index uint64) ([]int, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []int
	for k := range m.tiles {
		if k.Level == level && k.Index == index {
			out = append(out, k.Width)
		}
	}
	sort.Ints(out)
	return out, nil
}

// GetCheckpoint implements CheckpointStore.
func (m *MemStore) GetCheckpoint(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.checkpoint == nil {
		return nil, fmt.Errorf("%w: checkpoint", ErrNotFound)
	}
	return append([]byte(nil), m.checkpoint...), nil
}

// PutCheckpoint implements CheckpointStore.
func (m *MemStore) PutCheckpoint(ctx context.Context, note []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(note) == 0 || len(note) > MaxNoteSize {
		return fmt.Errorf("ledgerlog: a checkpoint note has 1 to %d bytes", MaxNoteSize)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.checkpoint = append([]byte(nil), note...)
	return nil
}

// Keys returns the keys stored, ordered by level, index and width.
func (m *MemStore) Keys() []TileKey {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]TileKey, 0, len(m.tiles))
	for k := range m.tiles {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Level != b.Level {
			return a.Level < b.Level
		}
		if a.Index != b.Index {
			return a.Index < b.Index
		}
		return a.Width < b.Width
	})
	return out
}

// Delete removes a tile (a test of damage, and the pruning of partial tiles).
func (m *MemStore) Delete(k TileKey) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.tiles, k)
}

// Corrupt replaces the content of a stored tile without the checks of PutTile
// (a test of damage).
func (m *MemStore) Corrupt(k TileKey, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tiles[k] = append([]byte(nil), data...)
}

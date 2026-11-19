// SPDX-License-Identifier: AGPL-3.0-only

package tlog

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/merkle"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/pubsrc"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/safefile"
)

// The Reader of a tree of size N can also answer for every prefix tree of size n <= N: the
// hashes of the aligned subtrees inside the prefix are the same in both trees, and the right
// edge of the prefix is combined from them. An aggregate member names a chain log at the size
// height+1 while the tiles of the chain are served for a later size; these methods are how a
// member is checked against those tiles without any tile that exists only for the older size.

// RootAt returns the root of the prefix tree of size n (n <= the size of the Reader).
func (r *Reader) RootAt(n uint64) (Hash, error) {
	if n > r.size {
		return Hash{}, fmt.Errorf("prefix size %d is larger than tree size %d", n, r.size)
	}
	if n == 0 {
		return merkle.EmptyRoot(), nil
	}
	return r.MTH(0, n)
}

// InclusionProofIn returns the audit path of RFC 9162 2.1.3.1 for the leaf at index in the prefix
// tree of size n.
func (r *Reader) InclusionProofIn(index, n uint64) ([]Hash, error) {
	if n > r.size {
		return nil, fmt.Errorf("prefix size %d is larger than tree size %d", n, r.size)
	}
	if index >= n {
		return nil, fmt.Errorf("leaf index %d is not smaller than tree size %d", index, n)
	}
	return r.path(index, 0, n)
}

// ConsistencyProofBetween returns PROOF(first, D_n) of RFC 9162 2.1.4.1 for the prefix tree of
// size n: empty when first is 0 or equal to n.
func (r *Reader) ConsistencyProofBetween(first, n uint64) ([]Hash, error) {
	if n > r.size {
		return nil, fmt.Errorf("prefix size %d is larger than tree size %d", n, r.size)
	}
	if first > n {
		return nil, fmt.Errorf("old size %d is larger than tree size %d", first, n)
	}
	if first == 0 || first == n {
		return nil, nil
	}
	return r.subproof(first, 0, n, true)
}

// LeafHash returns the hash of leaf i as the tiles hold it.
func (r *Reader) LeafHash(i uint64) (Hash, error) {
	if i >= r.size {
		return Hash{}, fmt.Errorf("leaf index %d is not smaller than tree size %d", i, r.size)
	}
	return r.node(0, i)
}

// BindRange checks that the tiles that hold the leaf hashes lo to hi-1 belong to the tree: every
// full tile of those leaves, at every level, hashes to the entry that the tile of the next level
// holds for it. The right edge, which no tile of a higher level holds, is bound through the
// root (Root combines it from the same tiles), so a caller that also compares Root with a signed
// root has bound every leaf hash of the range to that root. It reads one tile of each level
// for every 256 leaves, which is the work that the leaves themselves cost.
func (r *Reader) BindRange(lo, hi uint64) error {
	if lo >= hi || hi > r.size {
		return fmt.Errorf("range %d to %d is outside a tree of size %d", lo, hi, r.size)
	}
	for level := 0; TileHeight*(level+1) < 63; level++ {
		span := uint(TileHeight * (level + 1)) // leaves one tile of this level covers: 2^span
		fullAt := r.size >> span
		first, last := lo>>span, (hi-1)>>span
		if first >= fullAt {
			break
		}
		for n := first; n <= last && n < fullAt; n++ {
			hs, err := r.tile(level, n)
			if err != nil {
				return err
			}
			got := merkle.Root(hs)
			want, err := r.node(span, n)
			if err != nil {
				return err
			}
			if got != want {
				return fmt.Errorf("%w: the tile %d of level %d does not hash to the entry that level %d holds for it", ErrBadTile, n, level, level+1)
			}
		}
	}
	return nil
}

// EntriesAt reads the entry bundle n of a tree of the given size: all 256 entries of a full
// bundle, otherwise the partial bundle of the exact width. A partial bundle that is absent is
// read as the first entries of the full bundle (tlog-tiles lets a log delete a partial bundle
// once the full one exists). Each entry is at most maxEntry bytes, and the file at most
// 256 * (2 + maxEntry) bytes.
func EntriesAt(files FileReader, size, n uint64, maxEntry int) ([][]byte, error) {
	have := size
	if n*TileWidth >= have {
		return nil, fmt.Errorf("entry bundle %d is outside a tree of size %d", n, size)
	}
	width := TileWidth
	if left := have - n*TileWidth; left < TileWidth {
		width = int(left)
	}
	read := func(w int) ([][]byte, error) {
		name := EntriesPath(n, w)
		data, err := files.ReadFile(name, int64(w)*int64(2+maxEntry))
		switch {
		case err == nil:
		case errors.Is(err, fs.ErrNotExist):
			return nil, fmt.Errorf("%w: %s", ErrMissing, name)
		case errors.Is(err, pubsrc.ErrUnreachable):
			return nil, fmt.Errorf("%w: %s: %v", ErrMissing, name, err)
		case safefile.Unusable(err):
			return nil, fmt.Errorf("%w: %v", ErrBadTile, err)
		default:
			return nil, fmt.Errorf("reading %s: %w", name, err)
		}
		es, perr := ParseEntryBundle(data, w)
		if perr != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrBadTile, name, perr)
		}
		for _, e := range es {
			if len(e) > maxEntry {
				return nil, fmt.Errorf("%w: %s holds an entry of %d bytes, at most %d", ErrBadTile, name, len(e), maxEntry)
			}
		}
		return es, nil
	}
	if width == TileWidth {
		return read(TileWidth)
	}
	es, err := read(width)
	if err == nil || !errors.Is(err, ErrMissing) {
		return es, err
	}
	full, ferr := read(TileWidth)
	if ferr != nil {
		if errors.Is(ferr, ErrMissing) {
			return nil, err
		}
		return nil, ferr
	}
	return full[:width], nil
}

// Tile returns the hashes of tile n at level for the size of the Reader: the full tile when 256 hashes
// exist, otherwise the partial tile of the exact width.
func (r *Reader) Tile(level int, n uint64) ([]Hash, error) { return r.tile(level, n) }

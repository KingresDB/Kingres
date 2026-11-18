// SPDX-License-Identifier: AGPL-3.0-only

package ledgerlog

import (
	"context"
	"errors"
	"fmt"
	"math/bits"
)

// TileGetter is the part of a Store that a Reader needs.
type TileGetter interface {
	GetTile(ctx context.Context, k TileKey) ([]byte, error)
}

// Reader computes hashes, entries and proofs of the tree of a fixed size from
// tiles. Like a reader of the publication it asks for the partial tile of the
// width the size needs and, when that is not stored (a partial tile may be
// deleted once the full tile exists), takes the first hashes of the full tile.
// It keeps every tile it parses for as long as it lives (no eviction): use one
// for a request or a batch and drop it. It does not trust a tile: the caller compares the root and every proof with a
// signed checkpoint, or uses Audit.
type Reader struct {
	ctx   context.Context
	src   TileGetter
	size  uint64
	cache map[readKey][]Hash
	// Trace, when set, is called for each range of hashes of a tile that a
	// result depends on: the tile, the first hash and the count (a hook for
	// tests that damage the tiles a proof reads).
	Trace func(k TileKey, first, count int)
}

type readKey struct {
	level int
	n     uint64
}

// NewReader returns a Reader for the tree of the given size (below MaxTreeSize).
func NewReader(ctx context.Context, src TileGetter, size uint64) (*Reader, error) {
	if size >= MaxTreeSize {
		return nil, fmt.Errorf("%w: size %d", ErrTooLarge, size)
	}
	return &Reader{ctx: ctx, src: src, size: size, cache: map[readKey][]Hash{}}, nil
}

// Size returns the tree size of the reader.
func (r *Reader) Size() uint64 { return r.size }

func (r *Reader) get(k TileKey) ([]byte, error) {
	data, err := r.src.GetTile(r.ctx, k)
	if err == nil || !errors.Is(err, ErrNotFound) || k.Width == TileWidth {
		return data, err
	}
	full := k
	full.Width = TileWidth
	fdata, ferr := r.src.GetTile(r.ctx, full)
	if ferr != nil {
		if errors.Is(ferr, ErrNotFound) {
			return nil, err // name the tile that was asked for
		}
		return nil, ferr
	}
	// A getter is not trusted: the full tile must have exactly its length
	// before the first hashes are cut from it.
	if len(fdata) != TileWidth*HashSize {
		return nil, fmt.Errorf("%w: %s has %d bytes, expected %d", ErrTileMalformed, full.Path(), len(fdata), TileWidth*HashSize)
	}
	return fdata[:k.Width*HashSize], nil
}

// widthAt is the width of tile n at level (or of the bundle n) for the tree.
func (r *Reader) widthAt(level int, n uint64) (int, error) {
	count := r.size
	if level > 0 {
		count = r.size >> (TileHeight * uint(level))
	}
	if n*TileWidth >= count {
		return 0, fmt.Errorf("ledgerlog: tile %d at level %d is outside a tree of size %d", n, level, r.size)
	}
	if left := count - n*TileWidth; left < TileWidth {
		return int(left), nil
	}
	return TileWidth, nil
}

func (r *Reader) tile(level int, n uint64) ([]Hash, error) {
	k := readKey{level, n}
	if hs, ok := r.cache[k]; ok {
		return hs, nil
	}
	w, err := r.widthAt(level, n)
	if err != nil {
		return nil, err
	}
	data, err := r.get(TileKey{level, n, w})
	if err != nil {
		return nil, err
	}
	hs, err := ParseTile(data, w)
	if err != nil {
		return nil, err
	}
	r.cache[k] = hs
	return hs, nil
}

// node returns the hash of the aligned subtree of the given height whose first
// leaf is index<<height; it must lie inside the tree.
func (r *Reader) node(height uint, index uint64) (Hash, error) {
	if height > 62 || index+1 > r.size>>height {
		return Hash{}, fmt.Errorf("ledgerlog: subtree of height %d at index %d is outside a tree of size %d", height, index, r.size)
	}
	level, k := int(height/TileHeight), height%TileHeight
	first := index << k
	hs, err := r.tile(level, first/TileWidth)
	if err != nil {
		return Hash{}, err
	}
	off := int(first % TileWidth)
	if r.Trace != nil {
		w, _ := r.widthAt(level, first/TileWidth)
		r.Trace(TileKey{level, first / TileWidth, w}, off, 1<<k)
	}
	return RootOf(hs[off : off+(1<<k)]), nil
}

// MTH returns the tree hash of leaves [lo, hi), which must be a subtree in the
// sense of RFC 9162 (as in every proof).
func (r *Reader) MTH(lo, hi uint64) (Hash, error) {
	if hi > r.size || lo > hi {
		return Hash{}, fmt.Errorf("ledgerlog: range %d to %d is outside a tree of size %d", lo, hi, r.size)
	}
	n := hi - lo
	if n == 0 {
		return EmptyRoot(), nil
	}
	if n&(n-1) == 0 && lo%n == 0 {
		h := uint(bits.TrailingZeros64(n))
		return r.node(h, lo>>h)
	}
	k := largestPowerBelow(n)
	left, err := r.MTH(lo, lo+k)
	if err != nil {
		return Hash{}, err
	}
	right, err := r.MTH(lo+k, hi)
	if err != nil {
		return Hash{}, err
	}
	return NodeHash(left, right), nil
}

// Root returns the root of the whole tree.
func (r *Reader) Root() (Hash, error) { return r.MTH(0, r.size) }

// LeafHash returns the leaf hash at index, from the level 0 tile.
func (r *Reader) LeafHash(index uint64) (Hash, error) {
	if index >= r.size {
		return Hash{}, fmt.Errorf("ledgerlog: leaf %d is outside a tree of size %d", index, r.size)
	}
	return r.node(0, index)
}

// bundle returns the entries of entry bundle n of width w, from the partial
// bundle or, when that is not stored, from the first entries of the full one.
func (r *Reader) bundle(n uint64, w int) ([][]byte, error) {
	data, err := r.src.GetTile(r.ctx, TileKey{EntryLevel, n, w})
	if err != nil && errors.Is(err, ErrNotFound) && w < TileWidth {
		fdata, ferr := r.src.GetTile(r.ctx, TileKey{EntryLevel, n, TileWidth})
		if ferr != nil {
			if errors.Is(ferr, ErrNotFound) {
				return nil, err
			}
			return nil, ferr
		}
		es, perr := ParseBundle(fdata, TileWidth)
		if perr != nil {
			return nil, perr
		}
		return es[:w], nil
	}
	if err != nil {
		return nil, err
	}
	return ParseBundle(data, w)
}

// Entry returns the entry (leaf data) at index from the entry bundle, and checks
// that its leaf hash is the one of the level 0 tile.
func (r *Reader) Entry(index uint64) ([]byte, error) {
	if index >= r.size {
		return nil, fmt.Errorf("ledgerlog: entry %d is outside a tree of size %d", index, r.size)
	}
	n := index / TileWidth
	w, err := r.widthAt(0, n)
	if err != nil {
		return nil, err
	}
	es, err := r.bundle(n, w)
	if err != nil {
		return nil, err
	}
	e := es[index%TileWidth]
	h, err := r.LeafHash(index)
	if err != nil {
		return nil, err
	}
	if LeafHash(e) != h {
		return nil, fmt.Errorf("%w: entry %d does not have the leaf hash of the tile", ErrTilesMismatch, index)
	}
	return e, nil
}

// InclusionProof returns the audit path of RFC 9162 section 2.1.3.1 for the
// leaf at index: the missing nodes from the leaf upwards (at most
// ceil(log2 size) hashes).
func (r *Reader) InclusionProof(index uint64) ([]Hash, error) {
	if index >= r.size {
		return nil, fmt.Errorf("ledgerlog: leaf index %d is not smaller than the tree size %d", index, r.size)
	}
	return r.path(index, 0, r.size)
}

func (r *Reader) path(m, lo, hi uint64) ([]Hash, error) {
	n := hi - lo
	if n == 1 {
		return nil, nil
	}
	k := largestPowerBelow(n)
	if m < k {
		sub, err := r.path(m, lo, lo+k)
		if err != nil {
			return nil, err
		}
		h, err := r.MTH(lo+k, hi)
		if err != nil {
			return nil, err
		}
		return append(sub, h), nil
	}
	sub, err := r.path(m-k, lo+k, hi)
	if err != nil {
		return nil, err
	}
	h, err := r.MTH(lo, lo+k)
	if err != nil {
		return nil, err
	}
	return append(sub, h), nil
}

// ConsistencyProof returns PROOF(first, D_n) of RFC 9162 section 2.1.4.1 for
// the tree of the reader (n is its size): empty when first is 0 or equal to n,
// otherwise at most ceil(log2 n) + 1 hashes.
func (r *Reader) ConsistencyProof(first uint64) ([]Hash, error) {
	if first > r.size {
		return nil, fmt.Errorf("ledgerlog: the old size %d is larger than the tree size %d", first, r.size)
	}
	if first == 0 || first == r.size {
		return nil, nil
	}
	return r.subproof(first, 0, r.size, true)
}

func (r *Reader) subproof(m, lo, hi uint64, b bool) ([]Hash, error) {
	n := hi - lo
	if m == n {
		if b {
			return nil, nil
		}
		h, err := r.MTH(lo, hi)
		if err != nil {
			return nil, err
		}
		return []Hash{h}, nil
	}
	k := largestPowerBelow(n)
	if m <= k {
		sub, err := r.subproof(m, lo, lo+k, b)
		if err != nil {
			return nil, err
		}
		h, err := r.MTH(lo+k, hi)
		if err != nil {
			return nil, err
		}
		return append(sub, h), nil
	}
	sub, err := r.subproof(m-k, lo+k, hi, false)
	if err != nil {
		return nil, err
	}
	h, err := r.MTH(lo, lo+k)
	if err != nil {
		return nil, err
	}
	return append(sub, h), nil
}

// Audit reads every tile and entry bundle of the tree of the given size and
// checks them against each other and against the root: each entry has the leaf
// hash of the level 0 tile, each hash of a higher tile is the root of the 256
// hashes below it, and the root of the whole tree is wantRoot. It uses memory
// for one tile per level and time linear in the size. It is the check that the
// verifier's consistency command does not make (it reads the tiles of a proof
// path only, and no entry bundle). It looks at ctx at the start and at least once
// per tile (see the package documentation).
func Audit(ctx context.Context, src TileGetter, size uint64, wantRoot Hash) error {
	if size >= MaxTreeSize {
		return fmt.Errorf("%w: size %d", ErrTooLarge, size)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r := &Reader{ctx: ctx, src: src, size: size}
	var fr Frontier
	var cur [maxLevel + 1][]Hash // the tile being compared at each level
	var curN [maxLevel + 1]uint64
	var bundle [][]byte
	var bad error
	load := func(level int, n uint64) ([]Hash, error) {
		if err := ctx.Err(); err != nil { // once per tile, whatever the getter does
			return nil, err
		}
		w, err := r.widthAt(level, n)
		if err != nil {
			return nil, err
		}
		data, err := r.get(TileKey{level, n, w})
		if err != nil {
			return nil, err
		}
		return ParseTile(data, w)
	}
	emit := func(level int, idx uint64, h Hash) {
		if level == 0 || bad != nil {
			return // leaves are checked below, against the level 0 tile
		}
		n := idx / TileWidth
		if cur[level] == nil || curN[level] != n {
			hs, err := load(level, n)
			if err != nil {
				bad = err
				return
			}
			cur[level], curN[level] = hs, n
		}
		if cur[level][idx%TileWidth] != h {
			bad = fmt.Errorf("%w: hash %d of the tile at level %d index %d differs from the tree below it", ErrTilesMismatch, idx%TileWidth, level, n)
		}
	}
	var leafTile []Hash
	var leafTileN uint64
	for i := uint64(0); i < size; i++ {
		n := i / TileWidth
		if leafTile == nil || leafTileN != n {
			if err := ctx.Err(); err != nil { // once per tile of leaves, which also covers its bundle
				return err
			}
			hs, err := load(0, n)
			if err != nil {
				return err
			}
			leafTile, leafTileN = hs, n
			w, _ := r.widthAt(0, n)
			es, err := r.bundle(n, w)
			if err != nil {
				return err
			}
			bundle = es
		}
		leaf := leafTile[i%TileWidth]
		if LeafHash(bundle[i%TileWidth]) != leaf {
			return fmt.Errorf("%w: entry %d does not have the leaf hash of the tile", ErrTilesMismatch, i)
		}
		if err := fr.appendLeaf(leaf, emit); err != nil {
			return err
		}
		if bad != nil {
			return bad
		}
	}
	if fr.Root() != wantRoot {
		return fmt.Errorf("%w: the tiles of size %d give another root than the checkpoint", ErrTilesMismatch, size)
	}
	return nil
}

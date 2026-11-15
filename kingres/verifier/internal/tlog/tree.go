// SPDX-License-Identifier: AGPL-3.0-only

package tlog

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/bits"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/merkle"
)

// Hash is a SHA-256 tree hash.
type Hash = merkle.Hash

// Errors that callers map to exit statuses: a tile that is not there means a
// check could not be made with the inputs given; a tile of the wrong size is
// a failure of the log.
var (
	ErrMissing = errors.New("tile is missing")
	ErrBadTile = errors.New("tile is malformed")
)

// TileSource returns the bytes of tile level, n at the width asked for
// (1 to 256). Implementations return exactly width*32 bytes or an error
// wrapping ErrMissing or ErrBadTile.
type TileSource interface {
	ReadTile(level int, n uint64, width int) ([]byte, error)
}

// FSSource reads tiles from a file system laid out as a log prefix (a
// directory of the publication, 4.6 of the design). Paths are built from
// numbers only, and fs.FS refuses ".." and rooted names, so nothing outside
// the directory can be read.
type FSSource struct{ FS fs.FS }

// ReadTile implements TileSource. For a partial width it reads the partial
// tile and, when that file is missing, the full tile and its first width
// hashes: tlog-tiles lets a log delete a partial tile once the full tile
// exists.
func (s FSSource) ReadTile(level int, n uint64, width int) ([]byte, error) {
	if width < 1 || width > TileWidth {
		return nil, fmt.Errorf("%w: width %d", ErrBadTile, width)
	}
	if width < TileWidth {
		data, err := s.read(TilePath(level, n, width), width*HashSize)
		if err == nil || !errors.Is(err, ErrMissing) {
			return data, err
		}
		full, ferr := s.read(TilePath(level, n, TileWidth), FullTile)
		if ferr != nil {
			if errors.Is(ferr, ErrMissing) {
				return nil, err // name the partial tile that was asked for
			}
			return nil, ferr
		}
		return full[:width*HashSize], nil
	}
	return s.read(TilePath(level, n, TileWidth), FullTile)
}

func (s FSSource) read(name string, want int) ([]byte, error) {
	f, err := s.FS.Open(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrMissing, name)
		}
		return nil, fmt.Errorf("opening %s: %w", name, err)
	}
	defer f.Close()
	buf := make([]byte, want+1)
	n := 0
	for n < len(buf) {
		m, rerr := f.Read(buf[n:])
		n += m
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return nil, fmt.Errorf("reading %s: %w", name, rerr)
		}
	}
	if n != want {
		return nil, fmt.Errorf("%w: %s has %d bytes, expected %d", ErrBadTile, name, n, want)
	}
	return buf[:want], nil
}

// Reader computes tree hashes of a tree of a fixed size from tiles.
type Reader struct {
	src   TileSource
	size  uint64
	cache map[tileKey][]Hash
}

type tileKey struct {
	level int
	n     uint64
}

// NewReader returns a Reader for the tree of the given size, which must be
// below MaxTreeSize.
func NewReader(src TileSource, size uint64) (*Reader, error) {
	if size >= MaxTreeSize {
		return nil, fmt.Errorf("tree size %d is too large, expected less than %d", size, MaxTreeSize)
	}
	return &Reader{src: src, size: size, cache: map[tileKey][]Hash{}}, nil
}

// tile returns the hashes of tile n at level for this tree size: the full
// tile when 256 hashes exist, otherwise the partial tile of the exact width.
func (r *Reader) tile(level int, n uint64) ([]Hash, error) {
	k := tileKey{level, n}
	if hs, ok := r.cache[k]; ok {
		return hs, nil
	}
	have := r.size >> (TileHeight * uint(level)) // hashes that exist at this level
	if n*TileWidth >= have {
		return nil, fmt.Errorf("tile %d at level %d is outside a tree of size %d", n, level, r.size)
	}
	width := TileWidth
	if left := have - n*TileWidth; left < TileWidth {
		width = int(left)
	}
	data, err := r.src.ReadTile(level, n, width)
	if err != nil {
		return nil, err
	}
	hs, err := ParseTile(data, width)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadTile, err)
	}
	r.cache[k] = hs
	return hs, nil
}

// node returns the hash of the aligned subtree of the given height whose
// leftmost leaf is index<<height. It must lie inside the tree. Tiles hold
// the hashes at heights that are multiples of 8; the levels in between are
// computed from the hashes of one tile, which always contains the whole
// subtree because 2^k divides 256.
func (r *Reader) node(height uint, index uint64) (Hash, error) {
	if height > 62 || index+1 > r.size>>height {
		return Hash{}, fmt.Errorf("subtree of height %d at index %d is outside a tree of size %d", height, index, r.size)
	}
	level, k := int(height/TileHeight), height%TileHeight
	first := index << k
	hs, err := r.tile(level, first/TileWidth)
	if err != nil {
		return Hash{}, err
	}
	off := int(first % TileWidth)
	return merkle.Root(hs[off : off+(1<<k)]), nil
}

// MTH returns the Merkle tree hash of leaves [lo, hi) of the tree, which must
// be a subtree in the sense of RFC 6962 (lo is a multiple of the largest
// power of two below hi-lo, the case in every proof). Aligned power of two
// ranges are read from tiles; the right edge of the tree is combined from
// them.
func (r *Reader) MTH(lo, hi uint64) (Hash, error) {
	n := hi - lo
	if hi > r.size || lo >= hi {
		if lo == hi && hi <= r.size {
			return merkle.EmptyRoot(), nil
		}
		return Hash{}, fmt.Errorf("range %d to %d is outside a tree of size %d", lo, hi, r.size)
	}
	if n&(n-1) == 0 {
		h := uint(bits.TrailingZeros64(n))
		if lo%n == 0 {
			return r.node(h, lo>>h)
		}
	}
	k := uint64(1) << (bits.Len64(n-1) - 1)
	left, err := r.MTH(lo, lo+k)
	if err != nil {
		return Hash{}, err
	}
	right, err := r.MTH(lo+k, hi)
	if err != nil {
		return Hash{}, err
	}
	return merkle.NodeHash(left, right), nil
}

// Root returns the root of the whole tree.
func (r *Reader) Root() (Hash, error) {
	if r.size == 0 {
		return merkle.EmptyRoot(), nil
	}
	return r.MTH(0, r.size)
}

// InclusionProof returns the audit path of RFC 9162 section 2.1.3.1 for the
// leaf at index in this tree: the missing nodes from the leaf upwards.
func (r *Reader) InclusionProof(index uint64) ([]Hash, error) {
	if index >= r.size {
		return nil, fmt.Errorf("leaf index %d is not smaller than tree size %d", index, r.size)
	}
	return r.path(index, 0, r.size)
}

// path is PATH(m, D[lo:hi]) with m relative to lo.
func (r *Reader) path(m, lo, hi uint64) ([]Hash, error) {
	n := hi - lo
	if n == 1 {
		return nil, nil
	}
	k := uint64(1) << (bits.Len64(n-1) - 1)
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
// this tree (n is its size): empty when first is 0 or equal to n.
func (r *Reader) ConsistencyProof(first uint64) ([]Hash, error) {
	if first > r.size {
		return nil, fmt.Errorf("old size %d is larger than tree size %d", first, r.size)
	}
	if first == 0 || first == r.size {
		return nil, nil
	}
	return r.subproof(first, 0, r.size, true)
}

// subproof is SUBPROOF(m, D[lo:hi], b) with m relative to lo.
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
	k := uint64(1) << (bits.Len64(n-1) - 1)
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

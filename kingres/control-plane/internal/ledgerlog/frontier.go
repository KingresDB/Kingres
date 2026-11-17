// SPDX-License-Identifier: AGPL-3.0-only

package ledgerlog

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
)

// Tile geometry (tlog-tiles): a tile is a subtree of height 8, so a full tile
// has 256 hashes of 32 bytes.
const (
	TileHeight = 8
	TileWidth  = 1 << TileHeight
	HashSize   = 32
	// MaxTreeSize is the first size that is refused: a size below 2^62 keeps
	// every index in uint64 without overflow (the verifier's limit).
	MaxTreeSize = uint64(1) << 62
	// maxLevel is the highest tile level of a tree below MaxTreeSize.
	maxLevel = 7
)

// frontierVersion is the version byte of the serialised frontier.
const frontierVersion byte = 1

// MaxFrontierSize is the longest serialised frontier: the version, the size
// and 62 hashes.
const MaxFrontierSize = 1 + 8 + 62*HashSize

// Frontier is the compact range of a tree (RFC 9162, the "compact range" of the
// design 4.5): the hashes of the perfect subtrees that make up the tree of
// Size leaves, one for each set bit of Size, the largest first (at most 62
// hashes). From it the root is computed and new leaves are appended without
// reading an old leaf or tile. A Frontier is a value: appending to a copy does
// not change the original.
type Frontier struct {
	size   uint64
	hashes []Hash
}

// Errors of frontiers.
var (
	// ErrFrontierMalformed marks a serialised frontier that is not valid.
	ErrFrontierMalformed = errors.New("ledgerlog: the frontier is malformed")
	// ErrFrontierRoot is returned when a frontier does not have the root it was
	// stored with.
	ErrFrontierRoot = errors.New("ledgerlog: the frontier does not have the stored root")
	// ErrTooLarge is returned when a tree would reach MaxTreeSize leaves.
	ErrTooLarge = errors.New("ledgerlog: the tree is too large")
)

// Size returns the number of leaves.
func (f Frontier) Size() uint64 { return f.size }

// Hashes returns a copy of the hashes, the largest subtree first.
func (f Frontier) Hashes() []Hash { return append([]Hash(nil), f.hashes...) }

// Clone returns an independent copy.
func (f Frontier) Clone() Frontier {
	return Frontier{size: f.size, hashes: append([]Hash(nil), f.hashes...)}
}

// Root returns the root of the tree: the hashes folded from the smallest
// subtree to the largest.
func (f Frontier) Root() Hash {
	if len(f.hashes) == 0 {
		return EmptyRoot()
	}
	r := f.hashes[len(f.hashes)-1]
	for i := len(f.hashes) - 2; i >= 0; i-- {
		r = NodeHash(f.hashes[i], r)
	}
	return r
}

// emitFunc receives each hash that completes a node at a tile level: the
// leaf itself (level 0) and each node whose height is a multiple of 8.
type emitFunc func(level int, index uint64, h Hash)

// appendLeaf adds one leaf hash. It reports an error when the tree would reach
// MaxTreeSize leaves.
func (f *Frontier) appendLeaf(leaf Hash, emit emitFunc) error {
	if f.size+1 >= MaxTreeSize {
		return ErrTooLarge
	}
	n := f.size
	h := leaf
	if emit != nil {
		emit(0, n, h)
	}
	for k := uint(0); (n>>k)&1 == 1; k++ {
		top := f.hashes[len(f.hashes)-1]
		f.hashes = f.hashes[:len(f.hashes)-1]
		h = NodeHash(top, h)
		if emit != nil && (k+1)%TileHeight == 0 {
			emit(int((k+1)/TileHeight), n>>(k+1), h)
		}
	}
	f.hashes = append(f.hashes, h)
	f.size++
	return nil
}

// Append adds one leaf hash (LeafHash of the entry).
func (f *Frontier) Append(leaf Hash) error { return f.appendLeaf(leaf, nil) }

// Marshal writes the frontier in its canonical form: the version byte 1, the
// size as a big endian u64, and the hashes, 9 + 32 * popcount(size) bytes.
func (f Frontier) Marshal() []byte {
	out := make([]byte, 0, 9+len(f.hashes)*HashSize)
	out = append(out, frontierVersion)
	out = binary.BigEndian.AppendUint64(out, f.size)
	for _, h := range f.hashes {
		out = append(out, h[:]...)
	}
	return out
}

// ParseFrontier parses a serialised frontier: the version must be 1, the
// size below MaxTreeSize, and the length exactly 9 + 32 * popcount(size).
// Marshal of the result gives the input back.
func ParseFrontier(b []byte) (Frontier, error) {
	if len(b) < 9 || len(b) > MaxFrontierSize {
		return Frontier{}, fmt.Errorf("%w: %d bytes", ErrFrontierMalformed, len(b))
	}
	if b[0] != frontierVersion {
		return Frontier{}, fmt.Errorf("%w: version %d, expected %d", ErrFrontierMalformed, b[0], frontierVersion)
	}
	size := binary.BigEndian.Uint64(b[1:9])
	if size >= MaxTreeSize {
		return Frontier{}, fmt.Errorf("%w: size %d is too large", ErrFrontierMalformed, size)
	}
	n := bits.OnesCount64(size)
	if len(b) != 9+n*HashSize {
		return Frontier{}, fmt.Errorf("%w: %d bytes for size %d, expected %d", ErrFrontierMalformed, len(b), size, 9+n*HashSize)
	}
	f := Frontier{size: size, hashes: make([]Hash, n)}
	for i := range f.hashes {
		copy(f.hashes[i][:], b[9+i*HashSize:])
	}
	return f, nil
}

// LoadFrontier parses a stored frontier and checks it against the size and
// root stored with it (the checkpoint or the log row): a frontier that does
// not give that root is refused, so a damaged or swapped frontier is never
// appended to.
func LoadFrontier(b []byte, wantSize uint64, wantRoot Hash) (Frontier, error) {
	f, err := ParseFrontier(b)
	if err != nil {
		return Frontier{}, err
	}
	if f.size != wantSize {
		return Frontier{}, fmt.Errorf("%w: it has size %d, the stored size is %d", ErrFrontierRoot, f.size, wantSize)
	}
	if f.Root() != wantRoot {
		return Frontier{}, fmt.Errorf("%w: size %d", ErrFrontierRoot, f.size)
	}
	return f, nil
}

// equal reports whether two frontiers are the same.
func (f Frontier) equal(o Frontier) bool { return bytes.Equal(f.Marshal(), o.Marshal()) }

// PartialWidth returns the width of the partial tile at level for a tree of
// size leaves: floor(size / 256^level) mod 256 (tlog-tiles). Zero means there
// is none (an empty tile is never served). For level -1 (entry bundles) it is
// the width at level 0.
func PartialWidth(size uint64, level int) int {
	if level < 0 {
		level = 0
	}
	if level > maxLevel+1 || TileHeight*level >= 64 {
		return 0
	}
	return int((size >> (TileHeight * uint(level))) % TileWidth)
}

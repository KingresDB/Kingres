// SPDX-License-Identifier: PostgreSQL

// Package merkle implements the RFC 6962 Merkle tree hashes and the audit
// path verification of RFC 9162 section 2.1.3.2 that the ledger format uses.
package merkle

import (
	"crypto/sha256"
	"errors"
	"fmt"
)

// Hash is a SHA-256 digest.
type Hash = [32]byte

// Prefixes of RFC 6962 section 2.1. README: "leaf_hash(data) =
// SHA256(0x00 || data); node_hash(l, r) = SHA256(0x01 || l || r)".
const (
	leafPrefix byte = 0x00
	nodePrefix byte = 0x01
)

// MaxPathLength is the longest audit path accepted. A path of 64 hashes
// covers trees of 2^64 leaves, more than any index can address.
const MaxPathLength = 64

// LeafHash returns SHA256(0x00 || data).
func LeafHash(data []byte) Hash {
	h := sha256.New()
	h.Write([]byte{leafPrefix})
	h.Write(data)
	var out Hash
	h.Sum(out[:0])
	return out
}

// NodeHash returns SHA256(0x01 || left || right).
func NodeHash(left, right Hash) Hash {
	h := sha256.New()
	h.Write([]byte{nodePrefix})
	h.Write(left[:])
	h.Write(right[:])
	var out Hash
	h.Sum(out[:0])
	return out
}

// EmptyRoot is MTH of zero leaves: SHA256 of the empty string.
func EmptyRoot() Hash { return sha256.Sum256(nil) }

// Root computes MTH over already hashed leaves: README "MTH([]) = SHA256("")
// ..., MTH([h0]) = h0, and for n > 1, with k the largest power of two smaller
// than n, MTH = node_hash(MTH(h[0..k-1]), MTH(h[k..n-1]))".
func Root(leaves []Hash) Hash {
	switch len(leaves) {
	case 0:
		return EmptyRoot()
	case 1:
		return leaves[0]
	}
	k := 1
	for k*2 < len(leaves) {
		k *= 2
	}
	return NodeHash(Root(leaves[:k]), Root(leaves[k:]))
}

// ErrPath is returned when an audit path does not fit the tree.
var ErrPath = errors.New("audit path does not fit the tree")

// RootFromPath computes the tree root implied by the leaf hash at index in a
// tree of size leaves and its audit path, following RFC 9162 section
// 2.1.3.2. The caller compares the result with the expected root.
func RootFromPath(leaf Hash, index, size uint64, path []Hash) (Hash, error) {
	if size == 0 {
		return Hash{}, fmt.Errorf("%w: tree size is 0, expected at least 1", ErrPath)
	}
	if index >= size {
		return Hash{}, fmt.Errorf("%w: leaf index %d is not smaller than tree size %d", ErrPath, index, size)
	}
	if len(path) > MaxPathLength {
		return Hash{}, fmt.Errorf("%w: %d hashes, expected at most %d", ErrPath, len(path), MaxPathLength)
	}
	fn, sn := index, size-1
	r := leaf
	for _, p := range path {
		if sn == 0 {
			return Hash{}, fmt.Errorf("%w: path has %d hashes, too many for index %d in a tree of %d", ErrPath, len(path), index, size)
		}
		if fn&1 == 1 || fn == sn {
			r = NodeHash(p, r)
			if fn&1 == 0 {
				for fn&1 == 0 && fn != 0 {
					fn >>= 1
					sn >>= 1
				}
			}
		} else {
			r = NodeHash(r, p)
		}
		fn >>= 1
		sn >>= 1
	}
	if sn != 0 {
		return Hash{}, fmt.Errorf("%w: path has %d hashes, too few for index %d in a tree of %d", ErrPath, len(path), index, size)
	}
	return r, nil
}

// VerifyPath checks that the audit path leads from the leaf hash at index in a
// tree of size leaves to root.
func VerifyPath(leaf Hash, index, size uint64, path []Hash, root Hash) error {
	got, err := RootFromPath(leaf, index, size, path)
	if err != nil {
		return err
	}
	if got != root {
		return fmt.Errorf("audit path leads to %x, expected root %x", got, root)
	}
	return nil
}

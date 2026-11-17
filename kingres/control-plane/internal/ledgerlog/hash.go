// SPDX-License-Identifier: AGPL-3.0-only

package ledgerlog

import (
	"crypto/sha256"
	"errors"
	"fmt"
)

// Hash is a SHA-256 tree hash.
type Hash = [32]byte

// Domain separation of RFC 6962 section 2.1 and RFC 9162 section 2.1.1, as the
// ledger of the extension uses it (design 4.1): a leaf hash is
// SHA-256(0x00 || data) and a node hash is SHA-256(0x01 || left || right).
const (
	leafPrefix byte = 0x00
	nodePrefix byte = 0x01
)

// Limits of proofs. A path of 64 hashes covers every tree below 2^62 leaves
// with room to spare; a consistency proof has at most ceil(log2 n) + 1 hashes
// (RFC 9162 section 2.1.4.1, design 14 correction 1) and 65 are accepted.
const (
	MaxInclusionPath   = 64
	MaxConsistencyPath = 65
)

// ErrProof marks a proof that does not fit the sizes it is given for or does
// not lead to the root.
var ErrProof = errors.New("ledgerlog: the proof does not verify")

// LeafHash returns SHA-256(0x00 || data).
func LeafHash(data []byte) Hash {
	h := sha256.New()
	h.Write([]byte{leafPrefix})
	h.Write(data)
	var out Hash
	h.Sum(out[:0])
	return out
}

// NodeHash returns SHA-256(0x01 || left || right).
func NodeHash(left, right Hash) Hash {
	h := sha256.New()
	h.Write([]byte{nodePrefix})
	h.Write(left[:])
	h.Write(right[:])
	var out Hash
	h.Sum(out[:0])
	return out
}

// EmptyRoot is the root of the tree of no leaves: SHA-256 of the empty string.
func EmptyRoot() Hash { return sha256.Sum256(nil) }

// largestPowerBelow returns the largest power of two smaller than n, for n >= 2.
func largestPowerBelow(n uint64) uint64 {
	k := uint64(1)
	for k<<1 < n {
		k <<= 1
	}
	return k
}

// RootOf returns the root over already hashed leaves (RFC 9162 2.1.1).
func RootOf(leaves []Hash) Hash {
	switch len(leaves) {
	case 0:
		return EmptyRoot()
	case 1:
		return leaves[0]
	}
	k := int(largestPowerBelow(uint64(len(leaves))))
	return NodeHash(RootOf(leaves[:k]), RootOf(leaves[k:]))
}

func proofErr(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrProof}, args...)...)
}

// RootFromInclusion computes the root implied by the leaf hash at index in a
// tree of size leaves and its audit path (RFC 9162 section 2.1.3.2).
func RootFromInclusion(leaf Hash, index, size uint64, path []Hash) (Hash, error) {
	if size == 0 {
		return Hash{}, proofErr("the tree is empty")
	}
	if index >= size {
		return Hash{}, proofErr("leaf index %d is not smaller than the tree size %d", index, size)
	}
	if len(path) > MaxInclusionPath {
		return Hash{}, proofErr("the path has %d hashes, expected at most %d", len(path), MaxInclusionPath)
	}
	fn, sn := index, size-1
	r := leaf
	for _, p := range path {
		if sn == 0 {
			return Hash{}, proofErr("the path is too long for index %d in a tree of %d", index, size)
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
		return Hash{}, proofErr("the path is too short for index %d in a tree of %d", index, size)
	}
	return r, nil
}

// VerifyInclusion checks that the audit path leads from the leaf hash at index
// in a tree of size leaves to root.
func VerifyInclusion(leaf Hash, index, size uint64, path []Hash, root Hash) error {
	got, err := RootFromInclusion(leaf, index, size, path)
	if err != nil {
		return err
	}
	if got != root {
		return proofErr("the path leads to %x, expected %x", got, root)
	}
	return nil
}

// VerifyConsistency checks that the tree of size first and root firstRoot is a
// prefix of the tree of size second and root secondRoot, given the consistency
// path (RFC 9162 section 2.1.4.2). An empty first tree is a prefix of every
// tree and needs an empty path; equal sizes need an empty path and equal
// roots.
func VerifyConsistency(first, second uint64, firstRoot, secondRoot Hash, path []Hash) error {
	switch {
	case first > second:
		return proofErr("the old size %d is larger than the new size %d", first, second)
	case len(path) > MaxConsistencyPath:
		return proofErr("the path has %d hashes, expected at most %d", len(path), MaxConsistencyPath)
	case first == 0:
		if len(path) != 0 {
			return proofErr("the path has %d hashes, expected none from an empty tree", len(path))
		}
		if firstRoot != EmptyRoot() {
			return proofErr("the root of the empty tree is not the hash of the empty string")
		}
		return nil
	case first == second:
		if len(path) != 0 {
			return proofErr("the path has %d hashes, expected none for equal sizes", len(path))
		}
		if firstRoot != secondRoot {
			return proofErr("two trees of size %d have different roots", first)
		}
		return nil
	}
	p := path
	if first&(first-1) == 0 {
		p = append([]Hash{firstRoot}, path...)
	}
	if len(p) == 0 {
		return proofErr("the path is empty")
	}
	fn, sn := first-1, second-1
	for fn&1 == 1 {
		fn >>= 1
		sn >>= 1
	}
	fr, sr := p[0], p[0]
	for _, c := range p[1:] {
		if sn == 0 {
			return proofErr("the path is too long for the sizes %d and %d", first, second)
		}
		if fn&1 == 1 || fn == sn {
			fr = NodeHash(c, fr)
			sr = NodeHash(c, sr)
			if fn&1 == 0 {
				for fn&1 == 0 && fn != 0 {
					fn >>= 1
					sn >>= 1
				}
			}
		} else {
			sr = NodeHash(sr, c)
		}
		fn >>= 1
		sn >>= 1
	}
	if sn != 0 {
		return proofErr("the path is too short for the sizes %d and %d", first, second)
	}
	if fr != firstRoot {
		return proofErr("the path does not lead to the old root")
	}
	if sr != secondRoot {
		return proofErr("the path does not lead to the new root")
	}
	return nil
}

// SPDX-License-Identifier: AGPL-3.0-only

package merkle

import (
	"fmt"
)

// MaxConsistencyPathLength bounds a consistency proof. RFC 9162 section
// 2.1.4.1 says a proof has at most ceil(log2(n)) + 1 nodes; for sizes below
// 2^63 that is 64, and the proof may carry the first root as one more.
const MaxConsistencyPathLength = 65

// VerifyConsistency checks that the tree of size first with root firstRoot is
// a prefix of the tree of size second with root secondRoot, given the
// consistency path, following RFC 9162 section 2.1.4.2.
//
// The RFC defines the algorithm for 0 < first < second. The two edges are
// handled here so that a caller never has to special-case them: an empty tree
// is a prefix of every tree and needs no path (its root must be the hash of
// the empty string), and equal sizes need an empty path and equal roots. A
// first size larger than second is never consistent.
func VerifyConsistency(first, second uint64, firstRoot, secondRoot Hash, path []Hash) error {
	switch {
	case first > second:
		return fmt.Errorf("%w: old size %d is larger than new size %d", ErrPath, first, second)
	case len(path) > MaxConsistencyPathLength:
		return fmt.Errorf("%w: consistency path has %d hashes, expected at most %d", ErrPath, len(path), MaxConsistencyPathLength)
	case first == 0:
		if len(path) != 0 {
			return fmt.Errorf("%w: consistency path has %d hashes, expected none from an empty tree", ErrPath, len(path))
		}
		if firstRoot != EmptyRoot() {
			return fmt.Errorf("root of the empty tree is %x, expected %x", firstRoot, EmptyRoot())
		}
		return nil
	case first == second:
		if len(path) != 0 {
			return fmt.Errorf("%w: consistency path has %d hashes, expected none for equal sizes", ErrPath, len(path))
		}
		if firstRoot != secondRoot {
			return fmt.Errorf("the trees have the same size %d but different roots %x and %x", first, firstRoot, secondRoot)
		}
		return nil
	}
	p := path
	if first&(first-1) == 0 { // step 2: first is an exact power of two
		p = append([]Hash{firstRoot}, path...)
	}
	if len(p) == 0 { // step 1
		return fmt.Errorf("%w: consistency path is empty", ErrPath)
	}
	fn, sn := first-1, second-1 // step 3
	for fn&1 == 1 {             // step 4
		fn >>= 1
		sn >>= 1
	}
	fr, sr := p[0], p[0] // step 5
	for _, c := range p[1:] {
		if sn == 0 {
			return fmt.Errorf("%w: consistency path has %d hashes, too many for sizes %d and %d", ErrPath, len(path), first, second)
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
		return fmt.Errorf("%w: consistency path has %d hashes, too few for sizes %d and %d", ErrPath, len(path), first, second)
	}
	if fr != firstRoot {
		return fmt.Errorf("%w: it leads to %x, expected %x", ErrOldRoot, fr, firstRoot)
	}
	if sr != secondRoot {
		return fmt.Errorf("%w: it leads to %x, expected %x", ErrNewRoot, sr, secondRoot)
	}
	return nil
}

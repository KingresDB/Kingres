// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package merkle

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
)

// The vectors below are the RFC 6962 / Certificate Transparency test tree
// roots for the leaf data of the reference implementation's tests. They are
// written down as constants here, independent of this package, and are also
// recomputed below with a separate straight-line implementation.
var rfcLeaves = []string{
	"",
	"00",
	"10",
	"2021",
	"3031",
	"40414243",
	"5051525354555657",
	"606162636465666768696a6b6c6d6e6f",
}

var rfcRoots = map[int]string{
	0: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
	1: "6e340b9cffb37a989ca544e6bb780a2c78901d3fb33738768511a30617afa01d",
	2: "fac54203e7cc696cf0dfcb42c92a1d9dbaf70ad9e621f4bd8d98662f00e3c125",
	3: "aeb6bcfe274b70a14fb067a5e5578264db0fa9b51af5e0ba159158f329e06e77",
	5: "4e3bbb1f7b478dcfe71fb631631519a3bca12c9aefca1612bfce4c13a86264d4",
	8: "5dc9da79a70659a9ad559cb701ded9a2ab9d823aad2f4960cfe370eff4604328",
}

// refHash is an independent SHA-256 helper for the reference tree.
func refHash(prefix byte, parts ...[]byte) [32]byte {
	h := sha256.New()
	h.Write([]byte{prefix})
	for _, p := range parts {
		h.Write(p)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// refMTH is the tree hash of RFC 6962 section 2.1 on raw leaf data, written
// directly from the RFC text.
func refMTH(data [][]byte) [32]byte {
	n := len(data)
	if n == 0 {
		return sha256.Sum256(nil)
	}
	if n == 1 {
		return refHash(0x00, data[0])
	}
	k := 1
	for k<<1 < n {
		k <<= 1
	}
	l, r := refMTH(data[:k]), refMTH(data[k:])
	return refHash(0x01, l[:], r[:])
}

// refPath is PATH(m, D[n]) of RFC 6962 section 2.1.1.
func refPath(m int, data [][]byte) [][32]byte {
	n := len(data)
	if n <= 1 {
		return nil
	}
	k := 1
	for k<<1 < n {
		k <<= 1
	}
	if m < k {
		return append(refPath(m, data[:k]), refMTH(data[k:]))
	}
	return append(refPath(m-k, data[k:]), refMTH(data[:k]))
}

func leafData(n int) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		out[i] = []byte(fmt.Sprintf("leaf-%d", i))
	}
	return out
}

func TestRootRFC6962Vectors(t *testing.T) {
	data := make([][]byte, len(rfcLeaves))
	for i, s := range rfcLeaves {
		b, err := hex.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		data[i] = b
	}
	for n, want := range rfcRoots {
		t.Run(fmt.Sprintf("%d leaves", n), func(t *testing.T) {
			leaves := make([]Hash, n)
			for i := 0; i < n; i++ {
				leaves[i] = LeafHash(data[i])
			}
			if got := hex.EncodeToString(func() []byte { r := Root(leaves); return r[:] }()); got != want {
				t.Errorf("Root = %s, want %s (RFC vector)", got, want)
			}
			ref := refMTH(data[:n])
			if got := hex.EncodeToString(ref[:]); got != want {
				t.Errorf("reference implementation in the test = %s, want %s", got, want)
			}
		})
	}
}

func TestRootMatchesReference(t *testing.T) {
	for n := 0; n <= 40; n++ {
		data := leafData(n)
		leaves := make([]Hash, n)
		for i := range data {
			leaves[i] = LeafHash(data[i])
		}
		if got, want := Root(leaves), refMTH(data); got != want {
			t.Errorf("n=%d: Root %x, want %x", n, got, want)
		}
	}
}

func TestVerifyPathAllTreesAndIndexes(t *testing.T) {
	for n := 1; n <= 33; n++ {
		data := leafData(n)
		root := refMTH(data)
		for i := 0; i < n; i++ {
			raw := refPath(i, data)
			path := make([]Hash, len(raw))
			copy(path, raw)
			leaf := LeafHash(data[i])
			if err := VerifyPath(leaf, uint64(i), uint64(n), path, root); err != nil {
				t.Fatalf("n=%d i=%d: %v", n, i, err)
			}
			// Wrong index (a path does not authenticate the tree size, so no size case: the
			// ledger authenticates sizes in the transaction leaf and the block hash), wrong leaf, wrong root.
			for _, bad := range []struct {
				name string
				idx  uint64
				size uint64
				leaf Hash
				root Hash
			}{
				{"index+1", uint64(i + 1), uint64(n), leaf, root},
				{"leaf", uint64(i), uint64(n), LeafHash([]byte("x")), root},
				{"root", uint64(i), uint64(n), leaf, LeafHash([]byte("y"))},
			} {
				if err := VerifyPath(bad.leaf, bad.idx, bad.size, path, bad.root); err == nil {
					t.Errorf("n=%d i=%d %s: verified, want an error", n, i, bad.name)
				}
			}
			if len(path) > 0 {
				if err := VerifyPath(leaf, uint64(i), uint64(n), path[1:], root); err == nil {
					t.Errorf("n=%d i=%d: short path verified", n, i)
				}
			}
			if err := VerifyPath(leaf, uint64(i), uint64(n), append(append([]Hash{}, path...), root), root); err == nil {
				t.Errorf("n=%d i=%d: long path verified", n, i)
			}
		}
	}
}

func TestRootFromPathRejects(t *testing.T) {
	var h Hash
	tests := []struct {
		name  string
		index uint64
		size  uint64
		path  []Hash
	}{
		{"size zero", 0, 0, nil},
		{"index equals size", 3, 3, nil},
		{"huge index", 1 << 63, 5, nil},
		{"path too long", 0, 1, make([]Hash, 1)},
		{"over the length cap", 0, 1 << 63, make([]Hash, MaxPathLength+1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := RootFromPath(h, tt.index, tt.size, tt.path); err == nil {
				t.Error("want an error")
			}
		})
	}
}

// SPDX-License-Identifier: AGPL-3.0-only

package tlog

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"slices"
	"testing"
	"testing/fstest"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/merkle"
)

// ---- reference implementation, written from the RFC definitions only ----
//
// These functions do not use the production tree code, only SHA-256, so a
// mistake in one is unlikely to be repeated in the other.

func sha(parts ...[]byte) Hash {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
	}
	var out Hash
	h.Sum(out[:0])
	return out
}

func leafData(i int) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(i))
	return append([]byte("leaf-"), b...)
}

func leafHashes(n int) []Hash {
	out := make([]Hash, n)
	for i := range out {
		out[i] = sha([]byte{0}, leafData(i))
	}
	return out
}

// refMTH is MTH of RFC 6962 section 2.1 over already hashed leaves.
func refMTH(h []Hash) Hash {
	switch len(h) {
	case 0:
		return sha()
	case 1:
		return h[0]
	}
	k := 1
	for k*2 < len(h) {
		k *= 2
	}
	l, r := refMTH(h[:k]), refMTH(h[k:])
	return sha([]byte{1}, l[:], r[:])
}

// refPath is PATH(m, D) of RFC 6962 section 2.1.1 / RFC 9162 2.1.3.1.
func refPath(m int, h []Hash) []Hash {
	if len(h) <= 1 {
		return nil
	}
	k := 1
	for k*2 < len(h) {
		k *= 2
	}
	if m < k {
		return append(refPath(m, h[:k]), refMTH(h[k:]))
	}
	return append(refPath(m-k, h[k:]), refMTH(h[:k]))
}

// refProof is PROOF(m, D) of RFC 9162 section 2.1.4.1.
func refProof(m int, h []Hash) []Hash { return refSub(m, h, true) }

func refSub(m int, h []Hash, b bool) []Hash {
	n := len(h)
	if m == n {
		if b {
			return nil
		}
		return []Hash{refMTH(h)}
	}
	k := 1
	for k*2 < n {
		k *= 2
	}
	if m <= k {
		return append(refSub(m, h[:k], b), refMTH(h[k:]))
	}
	return append(refSub(m-k, h[k:], false), refMTH(h[:k]))
}

// buildTiles writes every tile of the tree of the given size to a map file
// system: full tiles and the partial tile of each level, from the definition
// "tile n at level l holds MTH of the 256^l leaves under each hash".
func buildTiles(h []Hash, withPartial bool) fstest.MapFS {
	fsys := fstest.MapFS{}
	size := len(h)
	for level := 0; ; level++ {
		span := 1 << (8 * level)
		count := size / span
		if count == 0 || level > 7 {
			break
		}
		hs := make([]Hash, count)
		for j := range hs {
			hs[j] = refMTH(h[j*span : (j+1)*span])
		}
		for n := 0; n*256 < count; n++ {
			end := min((n+1)*256, count)
			var data []byte
			for _, x := range hs[n*256 : end] {
				data = append(data, x[:]...)
			}
			if end-n*256 == 256 {
				fsys[TilePath(level, uint64(n), 256)] = &fstest.MapFile{Data: data}
			} else if withPartial {
				fsys[TilePath(level, uint64(n), end-n*256)] = &fstest.MapFile{Data: data}
			}
		}
	}
	return fsys
}

func newReader(t testing.TB, fsys fstest.MapFS, size int) *Reader {
	t.Helper()
	r, err := NewReader(FSSource{FS: fsys}, uint64(size))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func hashesEqual(a, b []Hash) bool { return slices.Equal(a, b) }

func TestKnownLeafAndEmptyHashes(t *testing.T) {
	// Well known values of the SHA-256 tree: MTH({}) is the hash of the empty
	// string and the leaf hash of the empty entry is SHA-256(0x00).
	if got := hex.EncodeToString(func() []byte { h := merkle.EmptyRoot(); return h[:] }()); got != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatal(got)
	}
	h := merkle.LeafHash(nil)
	if hex.EncodeToString(h[:]) != "6e340b9cffb37a989ca544e6bb780a2c78901d3fb33738768511a30617afa01d" {
		t.Fatal(hex.EncodeToString(h[:]))
	}
}

func TestTreesAgainstReference(t *testing.T) {
	all := leafHashes(301)
	for size := 0; size <= 300; size++ {
		h := all[:size]
		fsys := buildTiles(h, true)
		r := newReader(t, fsys, size)
		root, err := r.Root()
		if err != nil || root != refMTH(h) {
			t.Fatalf("size %d: root %x err %v", size, root, err)
		}
		// Every index and every old size for small trees, a spread of them
		// for the others, to keep the run short.
		step := 1
		if size > 70 {
			step = 7
		}
		idxs := map[int]bool{}
		for i := 0; i < size; i += step {
			idxs[i] = true
		}
		for _, i := range []int{0, 1, size / 2, size - 2, size - 1} {
			if i >= 0 && i < size {
				idxs[i] = true
			}
		}
		for i := range idxs {
			got, err := r.InclusionProof(uint64(i))
			if err != nil {
				t.Fatalf("size %d index %d: %v", size, i, err)
			}
			want := refPath(i, h)
			if !hashesEqual(got, want) {
				t.Fatalf("size %d index %d: inclusion proof differs from the reference", size, i)
			}
			if err := merkle.VerifyPath(h[i], uint64(i), uint64(size), got, root); err != nil {
				t.Fatalf("size %d index %d: %v", size, i, err)
			}
		}
		for m := 0; m <= size; m += step {
			got, err := r.ConsistencyProof(uint64(m))
			if err != nil {
				t.Fatalf("sizes %d to %d: %v", m, size, err)
			}
			if m > 0 && m < size && !hashesEqual(got, refProof(m, h)) {
				t.Fatalf("sizes %d to %d: consistency proof differs from the reference", m, size)
			}
			if err := merkle.VerifyConsistency(uint64(m), uint64(size), refMTH(h[:m]), root, got); err != nil {
				t.Fatalf("sizes %d to %d: %v", m, size, err)
			}
		}
	}
}

func TestLargerTreeAcrossTileLevels(t *testing.T) {
	// 66000 leaves have two levels of full tiles and a level 2 partial tile
	// of width 1; only a few proofs are checked to keep the run short.
	size := 66000
	h := leafHashes(size)
	fsys := buildTiles(h, true)
	r := newReader(t, fsys, size)
	root, err := r.Root()
	if err != nil || root != refMTH(h) {
		t.Fatalf("root %x %v", root, err)
	}
	for _, i := range []int{0, 255, 256, 65535, 65536, 65999} {
		got, err := r.InclusionProof(uint64(i))
		if err != nil || !hashesEqual(got, refPath(i, h)) {
			t.Fatalf("index %d: %v", i, err)
		}
	}
	for _, m := range []int{1, 255, 256, 257, 65535, 65536, 65999} {
		got, err := r.ConsistencyProof(uint64(m))
		if err != nil || !hashesEqual(got, refProof(m, h)) {
			t.Fatalf("old size %d: %v", m, err)
		}
		if err := merkle.VerifyConsistency(uint64(m), uint64(size), refMTH(h[:m]), root, got); err != nil {
			t.Fatalf("old size %d: %v", m, err)
		}
	}
}

func TestOldSizeUsesTilesOfNewSize(t *testing.T) {
	// A proof between 300 and 700 is computed from the tiles of the tree of
	// 700 only: the old root is not read from its own partial tiles.
	h := leafHashes(700)
	r := newReader(t, buildTiles(h, true), 700)
	p, err := r.ConsistencyProof(300)
	if err != nil {
		t.Fatal(err)
	}
	if err := merkle.VerifyConsistency(300, 700, refMTH(h[:300]), refMTH(h), p); err != nil {
		t.Fatal(err)
	}
}

func TestPartialTileFallsBackToFullTile(t *testing.T) {
	h := leafHashes(300)
	// Tree of 300: level 0 tile 1 is partial with 44 hashes. Serve it only
	// as a full tile of a later tree (no partial file), as a log that
	// deleted the partial tile would, using a tree of 512 for the data.
	big := leafHashes(512)
	fsys := buildTiles(big, true)
	fsys[TilePath(1, 0, 1)] = buildTiles(h, true)[TilePath(1, 0, 1)] // the level 1 partial tile of the tree of 300
	r := newReader(t, fsys, 300)
	root, err := r.Root()
	if err != nil || root != refMTH(h) {
		t.Fatalf("root %x %v", root, err)
	}
}

func TestMissingAndMalformedTiles(t *testing.T) {
	h := leafHashes(300)
	good := buildTiles(h, true)
	clone := func() fstest.MapFS {
		c := fstest.MapFS{}
		for k, v := range good {
			c[k] = &fstest.MapFile{Data: append([]byte{}, v.Data...)}
		}
		return c
	}
	partial := TilePath(0, 1, 44)
	full := TilePath(1, 0, 1) // the only source of the hash of the first 256 leaves

	// Missing partial tile and no full tile: ErrMissing.
	fsys := clone()
	delete(fsys, partial)
	if _, err := newReader(t, fsys, 300).Root(); !errors.Is(err, ErrMissing) {
		t.Fatalf("missing partial: %v", err)
	}
	// Missing full tile.
	fsys = clone()
	delete(fsys, full)
	if _, err := newReader(t, fsys, 300).Root(); !errors.Is(err, ErrMissing) {
		t.Fatalf("missing full: %v", err)
	}
	// Tiles of a wrong size: every one of these is ErrBadTile.
	for name, mutate := range map[string]func([]byte) []byte{
		"one byte short":   func(b []byte) []byte { return b[:len(b)-1] },
		"one hash short":   func(b []byte) []byte { return b[:len(b)-32] },
		"one byte long":    func(b []byte) []byte { return append(b, 0) },
		"one hash long":    func(b []byte) []byte { return append(b, make([]byte, 32)...) },
		"empty":            func(b []byte) []byte { return nil },
		"twice the length": func(b []byte) []byte { return append(b, b...) },
	} {
		for _, p := range []string{partial, full} {
			fsys = clone()
			fsys[p].Data = mutate(fsys[p].Data)
			if _, err := newReader(t, fsys, 300).Root(); !errors.Is(err, ErrBadTile) {
				t.Errorf("%s of %s: %v", name, p, err)
			}
		}
	}
	// A flipped bit in a tile gives a root that is not the real one: the
	// caller's comparison with the signed root catches it.
	fsys = clone()
	fsys[full].Data[10] ^= 1
	root, err := newReader(t, fsys, 300).Root()
	if err != nil || root == refMTH(h) {
		t.Fatalf("flipped bit not visible: %v", err)
	}
	// A partial tile of the wrong width for the size (45 hashes for 300).
	fsys = clone()
	fsys[TilePath(0, 1, 45)] = fsys[partial]
	delete(fsys, partial)
	if _, err := newReader(t, fsys, 300).Root(); !errors.Is(err, ErrMissing) {
		t.Fatalf("wrong width file name: %v", err)
	}
}

func TestReaderBounds(t *testing.T) {
	if _, err := NewReader(FSSource{}, MaxTreeSize); err == nil {
		t.Error("tree size 2^62 accepted")
	}
	r := newReader(t, buildTiles(leafHashes(10), true), 10)
	if _, err := r.InclusionProof(10); err == nil {
		t.Error("index == size accepted")
	}
	if _, err := r.ConsistencyProof(11); err == nil {
		t.Error("old size above new size accepted")
	}
	if _, err := r.MTH(0, 11); err == nil {
		t.Error("range beyond the tree accepted")
	}
	if p, err := r.ConsistencyProof(0); err != nil || len(p) != 0 {
		t.Error("empty old tree needs no proof")
	}
	empty := newReader(t, fstest.MapFS{}, 0)
	if root, err := empty.Root(); err != nil || root != merkle.EmptyRoot() {
		t.Error("empty tree root")
	}
}

func TestFSSourceRefusesEscapes(t *testing.T) {
	// The source only builds names from numbers, and fs.FS rejects "..";
	// make sure an unusual index still stays inside.
	s := FSSource{FS: fstest.MapFS{}}
	if _, err := s.ReadTile(0, 1<<60, 256); !errors.Is(err, ErrMissing) {
		t.Fatalf("%v", err)
	}
	if _, err := s.ReadTile(0, 0, 0); !errors.Is(err, ErrBadTile) {
		t.Fatalf("%v", err)
	}
}

// ---- RFC 9162 verification: negative tests ----

func TestVerifyPathNegative(t *testing.T) {
	for _, size := range []int{1, 2, 3, 5, 7, 8, 9, 100} {
		h := leafHashes(size)
		root := refMTH(h)
		for i := 0; i < size; i++ {
			p := refPath(i, h)
			if err := merkle.VerifyPath(h[i], uint64(i), uint64(size), p, root); err != nil {
				t.Fatalf("size %d index %d: %v", size, i, err)
			}
			if len(p) > 0 {
				if merkle.VerifyPath(h[i], uint64(i), uint64(size), p[:len(p)-1], root) == nil {
					t.Errorf("size %d index %d: truncated path accepted", size, i)
				}
				if merkle.VerifyPath(h[i], uint64(i), uint64(size), p[1:], root) == nil {
					t.Errorf("size %d index %d: path without its first hash accepted", size, i)
				}
			}
			if merkle.VerifyPath(h[i], uint64(i), uint64(size), append(slices.Clone(p), Hash{}), root) == nil {
				t.Errorf("size %d index %d: extended path accepted", size, i)
			}
			if i+1 < size && size > 1 && merkle.VerifyPath(h[i], uint64(i+1), uint64(size), p, root) == nil && len(p) > 0 {
				// A different index may legitimately share the same path
				// only when the leaf hash is the same, which it is not.
				t.Errorf("size %d index %d: accepted at index %d", size, i, i+1)
			}
		}
	}
}

func TestVerifyConsistencyNegative(t *testing.T) {
	for _, size := range []int{2, 3, 4, 5, 7, 8, 9, 16, 33, 100} {
		h := leafHashes(size)
		root := refMTH(h)
		for m := 1; m < size; m++ {
			old := refMTH(h[:m])
			p := refProof(m, h)
			ok := func(m, n int, a, b Hash, p []Hash) error {
				return merkle.VerifyConsistency(uint64(m), uint64(n), a, b, p)
			}
			if err := ok(m, size, old, root, p); err != nil {
				t.Fatalf("%d to %d: %v", m, size, err)
			}
			if len(p) > 0 && ok(m, size, old, root, p[:len(p)-1]) == nil {
				t.Errorf("%d to %d: truncated proof accepted", m, size)
			}
			if ok(m, size, old, root, append(slices.Clone(p), Hash{})) == nil {
				t.Errorf("%d to %d: extended proof accepted", m, size)
			}
			if ok(m, size, old, root, nil) == nil {
				t.Errorf("%d to %d: empty proof accepted", m, size)
			}
			if ok(m, size, root, old, p) == nil {
				t.Errorf("%d to %d: swapped roots accepted", m, size)
			}
			if ok(m, size, Hash{1}, root, p) == nil || ok(m, size, old, Hash{1}, p) == nil {
				t.Errorf("%d to %d: wrong root accepted", m, size)
			}
			for j := range p {
				q := slices.Clone(p)
				q[j][0] ^= 1
				if ok(m, size, old, root, q) == nil {
					t.Errorf("%d to %d: flipped hash %d accepted", m, size, j)
				}
			}
			// The proof of another pair of sizes must not verify, unless it is
			// byte for byte the proof of this pair.
			for m2 := 1; m2 < size; m2++ {
				if m2 != m && !hashesEqual(refProof(m2, h), p) && ok(m, size, old, root, refProof(m2, h)) == nil {
					t.Errorf("%d to %d: proof of old size %d accepted", m, size, m2)
				}
			}
			// The sizes are not authenticated by the proof alone (the same
			// proof can verify for a neighbouring size); the signed checkpoint
			// binds size and root, so no assertion is made about that here.
		}
	}
	// Edges.
	h := leafHashes(5)
	root := refMTH(h)
	if err := merkle.VerifyConsistency(0, 5, merkle.EmptyRoot(), root, nil); err != nil {
		t.Errorf("empty old tree: %v", err)
	}
	if merkle.VerifyConsistency(0, 5, root, root, nil) == nil {
		t.Error("empty old tree with a non-empty root accepted")
	}
	if merkle.VerifyConsistency(0, 5, merkle.EmptyRoot(), root, []Hash{{}}) == nil {
		t.Error("empty old tree with a path accepted")
	}
	if err := merkle.VerifyConsistency(5, 5, root, root, nil); err != nil {
		t.Errorf("equal sizes: %v", err)
	}
	if merkle.VerifyConsistency(5, 5, root, Hash{1}, nil) == nil || merkle.VerifyConsistency(5, 5, root, root, []Hash{{}}) == nil {
		t.Error("equal sizes with other roots or a path accepted")
	}
	if merkle.VerifyConsistency(6, 5, root, root, nil) == nil {
		t.Error("old size above new size accepted")
	}
	if merkle.VerifyConsistency(1, 1<<40, root, root, make([]Hash, merkle.MaxConsistencyPathLength+1)) == nil {
		t.Error("very long path accepted")
	}
}

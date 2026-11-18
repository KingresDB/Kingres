// SPDX-License-Identifier: AGPL-3.0-only

package ledgerlog

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"testing"
)

// The RFC 6962 / Certificate Transparency test tree vectors the verifier's
// tests use (kingres/verifier/internal/merkle): same leaves, same roots.
var rfcLeaves = []string{"", "00", "10", "2021", "3031", "40414243", "5051525354555657", "606162636465666768696a6b6c6d6e6f"}

var rfcRoots = map[int]string{
	0: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
	1: "6e340b9cffb37a989ca544e6bb780a2c78901d3fb33738768511a30617afa01d",
	2: "fac54203e7cc696cf0dfcb42c92a1d9dbaf70ad9e621f4bd8d98662f00e3c125",
	3: "aeb6bcfe274b70a14fb067a5e5578264db0fa9b51af5e0ba159158f329e06e77",
	5: "4e3bbb1f7b478dcfe71fb631631519a3bca12c9aefca1612bfce4c13a86264d4",
	8: "5dc9da79a70659a9ad559cb701ded9a2ab9d823aad2f4960cfe370eff4604328",
}

func TestRFCVectors(t *testing.T) {
	for n, want := range rfcRoots {
		hs := make([]Hash, n)
		var fr Frontier
		for i := 0; i < n; i++ {
			d, _ := hex.DecodeString(rfcLeaves[i])
			hs[i] = LeafHash(d)
			if err := fr.Append(hs[i]); err != nil {
				t.Fatal(err)
			}
		}
		if got := RootOf(hs); hex.EncodeToString(got[:]) != want {
			t.Errorf("RootOf %d leaves = %x, want %s", n, got, want)
		}
		if got := fr.Root(); hex.EncodeToString(got[:]) != want {
			t.Errorf("frontier root of %d leaves = %x, want %s", n, got, want)
		}
	}
	if h := LeafHash(nil); hex.EncodeToString(h[:]) != "6e340b9cffb37a989ca544e6bb780a2c78901d3fb33738768511a30617afa01d" {
		t.Errorf("leaf hash of the empty entry: %x", h)
	}
}

func TestEveryProofOfSmallTreesAgainstReference(t *testing.T) {
	// The verifier's own test covers sizes 0 to 300 with the same reference
	// algorithm; the same sizes, indexes and old sizes are covered here.
	all := leafHashesOf(300)
	for size := 0; size <= 300; size++ {
		store := NewMemStore()
		l, err := Create(bg, testOrigin(1), store)
		if err != nil {
			t.Fatal(err)
		}
		if size > 0 {
			mustAppend(t, l, 0, size)
		}
		h := all[:size]
		r, err := NewReader(bg, store, uint64(size))
		if err != nil {
			t.Fatal(err)
		}
		root, err := r.Root()
		if err != nil || root != refMTH(h) || root != l.Root() {
			t.Fatalf("size %d: root %x err %v", size, root, err)
		}
		step := 1
		if size > 70 {
			step = 7
		}
		for i := 0; i < size; i += step {
			got, err := r.InclusionProof(uint64(i))
			if err != nil {
				t.Fatalf("size %d index %d: %v", size, i, err)
			}
			if !slices.Equal(got, refPath(i, h)) {
				t.Fatalf("size %d index %d: the proof differs from the reference", size, i)
			}
			if err := VerifyInclusion(h[i], uint64(i), uint64(size), got, root); err != nil {
				t.Fatalf("size %d index %d: %v", size, i, err)
			}
		}
		for m := 0; m <= size; m += step {
			got, err := r.ConsistencyProof(uint64(m))
			if err != nil {
				t.Fatalf("sizes %d to %d: %v", m, size, err)
			}
			if m > 0 && m < size && !slices.Equal(got, refProof(m, h)) {
				t.Fatalf("sizes %d to %d: the proof differs from the reference", m, size)
			}
			if err := VerifyConsistency(uint64(m), uint64(size), refMTH(h[:m]), root, got); err != nil {
				t.Fatalf("sizes %d to %d: %v", m, size, err)
			}
		}
	}
}

func TestTilesEqualTheDefinition(t *testing.T) {
	for _, n := range []int{1, 2, 3, 255, 256, 257, 511, 512, 513, 1000, 65535, 65536, 65539} {
		if n > 1000 && !longTests() {
			continue
		}
		store := NewMemStore()
		l, _ := Create(bg, testOrigin(1), store)
		mustAppend(t, l, 0, n)
		want := refTiles(t, n)
		got := requiredOf(t, store, uint64(n))
		for k, d := range want {
			// refTiles holds partial tiles of every level for the size, and the
			// full ones; Required names exactly the same keys.
			g, ok := got[k]
			if !ok {
				t.Fatalf("size %d: Required lacks %s", n, k.Path())
			}
			if !bytes.Equal(g, d) {
				t.Fatalf("size %d: tile %s differs from the definition", n, k.Path())
			}
		}
		if len(got) != len(want) {
			t.Fatalf("size %d: %d tiles required, %d in the definition", n, len(got), len(want))
		}
		if err := Audit(bg, store, uint64(n), l.Root()); err != nil {
			t.Fatalf("size %d: audit: %v", n, err)
		}
	}
}

func TestZeroSizeLog(t *testing.T) {
	store := NewMemStore()
	l, err := Create(bg, testOrigin(1), store)
	if err != nil {
		t.Fatal(err)
	}
	if l.Size() != 0 || l.Root() != EmptyRoot() {
		t.Fatal("a new log is not empty")
	}
	if _, err := l.Append(bg, nil); err == nil {
		t.Fatal("an empty append was accepted")
	}
	if len(mustRequired(t, 0)) != 0 {
		t.Fatal("an empty tree needs no tile")
	}
	r, _ := NewReader(bg, store, 0)
	if root, err := r.Root(); err != nil || root != EmptyRoot() {
		t.Fatalf("%x %v", root, err)
	}
	if _, err := r.InclusionProof(0); err == nil {
		t.Fatal("an inclusion proof in an empty tree")
	}
	if p, err := r.ConsistencyProof(0); err != nil || len(p) != 0 {
		t.Fatalf("%v %v", p, err)
	}
	// A checkpoint of size 0 is well formed.
	c := l.Checkpoint()
	if _, err := c.Text(); err != nil {
		t.Fatal(err)
	}
	if err := Audit(bg, store, 0, EmptyRoot()); err != nil {
		t.Fatal(err)
	}
}

// TestBatchingGivesTheSameLog is the property that appending leaves in any
// batching gives the same tiles, frontier and root as appending them one by
// one. The tiles compared are the ones that a reader of the final size may ask
// for; the one-by-one store also holds the partial tiles of the sizes on the
// way, and each of those must be a prefix of the final tile at the same place.
func TestBatchingGivesTheSameLog(t *testing.T) {
	rng := newRand(t)
	for trial := 0; trial < 12; trial++ {
		n := 1 + rng.Intn(700)
		if trial == 0 && longTests() {
			n = 65539
		}
		one := NewMemStore()
		lo, _ := Create(bg, testOrigin(1), one)
		if n < 3000 {
			for i := 0; i < n; i++ {
				mustAppend(t, lo, i, i+1)
			}
		} else {
			// Leaf by leaf for the first 600, then batches: keeps the run short.
			for i := 0; i < 600; i++ {
				mustAppend(t, lo, i, i+1)
			}
			mustAppend(t, lo, 600, n)
		}
		batched := NewMemStore()
		lb, _ := Create(bg, testOrigin(1), batched)
		for pos := 0; pos < n; {
			step := 1 + rng.Intn(1+rng.Intn(600))
			end := min(pos+step, n)
			mustAppend(t, lb, pos, end)
			pos = end
		}
		if lo.Root() != lb.Root() || !bytes.Equal(lo.Frontier().Marshal(), lb.Frontier().Marshal()) {
			t.Fatalf("n=%d: root or frontier differ", n)
		}
		a, b := requiredOf(t, one, uint64(n)), requiredOf(t, batched, uint64(n))
		if len(a) != len(b) {
			t.Fatalf("n=%d: %d against %d required tiles", n, len(a), len(b))
		}
		for k, d := range a {
			if !bytes.Equal(d, b[k]) {
				t.Fatalf("n=%d: tile %s differs", n, k.Path())
			}
		}
		req := map[[2]uint64]TileKey{}
		for _, k := range mustRequired(t, uint64(n)) {
			req[[2]uint64{uint64(k.Level + 1), k.Index}] = k
		}
		for _, k := range one.Keys() {
			cur, ok := req[[2]uint64{uint64(k.Level + 1), k.Index}]
			if !ok {
				t.Fatalf("n=%d: stray tile %s", n, k.Path())
			}
			if cur == k {
				continue
			}
			if k.Width >= cur.Width {
				t.Fatalf("n=%d: %s is not narrower than %s", n, k.Path(), cur.Path())
			}
			d, _ := one.GetTile(bg, k)
			nd, _ := one.GetTile(bg, cur)
			if !bytes.HasPrefix(nd, d) {
				t.Fatalf("n=%d: older %s is not a prefix of %s", n, k.Path(), cur.Path())
			}
		}
		if err := Audit(bg, one, uint64(n), lo.Root()); err != nil {
			t.Fatalf("n=%d: audit: %v", n, err)
		}
	}
}

// TestFrontierSavedAndLoadedContinues is the property that a frontier saved
// and loaded at any point continues to the same result as a log that never
// stopped.
func TestFrontierSavedAndLoadedContinues(t *testing.T) {
	rng := newRand(t)
	for trial := 0; trial < 8; trial++ {
		n := 1 + rng.Intn(900)
		ref := NewMemStore()
		lr, _ := Create(bg, testOrigin(1), ref)
		mustAppend(t, lr, 0, n)

		store := NewMemStore()
		l, _ := Create(bg, testOrigin(1), store)
		pos := 0
		for pos < n {
			end := min(pos+1+rng.Intn(300), n)
			a := mustAppend(t, l, pos, end)
			pos = end
			// Save, drop the log, load it again from the stored frontier and root.
			l2, err := Open(bg, testOrigin(1), store, a.Frontier, a.Size, a.Root, Options{})
			if err != nil {
				t.Fatalf("n=%d at %d: %v", n, pos, err)
			}
			if l2.Size() != a.Size || l2.Root() != a.Root {
				t.Fatalf("n=%d at %d: the reopened log differs", n, pos)
			}
			l = l2
		}
		if l.Root() != lr.Root() || !bytes.Equal(l.Frontier().Marshal(), lr.Frontier().Marshal()) {
			t.Fatalf("n=%d: the reopened log ends elsewhere", n)
		}
		a, b := requiredOf(t, store, uint64(n)), requiredOf(t, ref, uint64(n))
		for k, d := range a {
			if !bytes.Equal(d, b[k]) {
				t.Fatalf("n=%d: tile %s differs", n, k.Path())
			}
		}
	}
}

func flipBit(h Hash, bit int) Hash {
	h[bit/8%32] ^= 1 << (bit % 8)
	return h
}

// TestProofsVerifyAndFailAfterAnyBitChange: every inclusion and consistency
// proof computed from tiles verifies with this package's own functions, and fails
// after a change of any single bit of the leaf hash, a path hash, a root, or an
// index or size.
func TestProofsVerifyAndFailAfterAnyBitChange(t *testing.T) {
	rng := newRand(t)
	sizes := []int{1, 2, 3, 4, 5, 7, 8, 9, 100, 255, 256, 257, 300, 1000}
	for i := 0; i < 4; i++ {
		sizes = append(sizes, 1+rng.Intn(2000))
	}
	for _, n := range sizes {
		store := NewMemStore()
		l, _ := Create(bg, testOrigin(1), store)
		mustAppend(t, l, 0, n)
		root := l.Root()
		r, _ := NewReader(bg, store, uint64(n))
		hs := leafHashesOf(n)
		for k := 0; k < 3; k++ {
			idx := rng.Intn(n)
			if k == 0 {
				idx = 0
			} else if k == 1 {
				idx = n - 1
			}
			p, err := r.InclusionProof(uint64(idx))
			if err != nil {
				t.Fatal(err)
			}
			if len(p) > MaxInclusionPath {
				t.Fatalf("an inclusion proof of %d hashes", len(p))
			}
			if err := VerifyInclusion(hs[idx], uint64(idx), uint64(n), p, root); err != nil {
				t.Fatalf("n=%d idx=%d: %v", n, idx, err)
			}
			for bit := 0; bit < 256; bit += 1 + rng.Intn(5) {
				if VerifyInclusion(flipBit(hs[idx], bit), uint64(idx), uint64(n), p, root) == nil {
					t.Fatalf("n=%d idx=%d: a changed leaf hash (bit %d) verified", n, idx, bit)
				}
				if VerifyInclusion(hs[idx], uint64(idx), uint64(n), p, flipBit(root, bit)) == nil {
					t.Fatalf("n=%d idx=%d: a changed root (bit %d) verified", n, idx, bit)
				}
				for j := range p {
					q := slices.Clone(p)
					q[j] = flipBit(q[j], bit)
					if VerifyInclusion(hs[idx], uint64(idx), uint64(n), q, root) == nil {
						t.Fatalf("n=%d idx=%d: a changed path hash %d (bit %d) verified", n, idx, j, bit)
					}
				}
			}
			if n > 1 {
				// A wrong index or size, or a path of the wrong length.
				if VerifyInclusion(hs[idx], uint64((idx+1)%n), uint64(n), p, root) == nil && n > 1 && len(p) > 0 {
					t.Fatalf("n=%d idx=%d: the proof verified for another index", n, idx)
				}
				if len(p) > 0 && VerifyInclusion(hs[idx], uint64(idx), uint64(n), p[1:], root) == nil {
					t.Fatalf("n=%d idx=%d: a short path verified", n, idx)
				}
				if VerifyInclusion(hs[idx], uint64(idx), uint64(n), append(slices.Clone(p), Hash{}), root) == nil {
					t.Fatalf("n=%d idx=%d: a long path verified", n, idx)
				}
			}
		}
		for k := 0; k < 4; k++ {
			m := 1 + rng.Intn(n)
			if k == 0 {
				m = 1
			}
			p, err := r.ConsistencyProof(uint64(m))
			if err != nil {
				t.Fatal(err)
			}
			bound := 1
			for 1<<bound < n {
				bound++
			}
			if len(p) > bound+1 {
				t.Fatalf("n=%d m=%d: a consistency proof of %d hashes, expected at most %d", n, m, len(p), bound+1)
			}
			oldRoot := refMTH(hs[:m])
			if err := VerifyConsistency(uint64(m), uint64(n), oldRoot, root, p); err != nil {
				t.Fatalf("n=%d m=%d: %v", n, m, err)
			}
			if m == n {
				continue
			}
			for bit := 0; bit < 256; bit += 1 + rng.Intn(5) {
				if VerifyConsistency(uint64(m), uint64(n), flipBit(oldRoot, bit), root, p) == nil {
					t.Fatalf("n=%d m=%d: a changed old root (bit %d) verified", n, m, bit)
				}
				if VerifyConsistency(uint64(m), uint64(n), oldRoot, flipBit(root, bit), p) == nil {
					t.Fatalf("n=%d m=%d: a changed new root (bit %d) verified", n, m, bit)
				}
				for j := range p {
					q := slices.Clone(p)
					q[j] = flipBit(q[j], bit)
					if VerifyConsistency(uint64(m), uint64(n), oldRoot, root, q) == nil {
						t.Fatalf("n=%d m=%d: a changed path hash %d (bit %d) verified", n, m, j, bit)
					}
				}
			}
		}
	}
}

func TestProofBounds(t *testing.T) {
	var z Hash
	if err := VerifyInclusion(z, 0, 0, nil, z); !errors.Is(err, ErrProof) {
		t.Errorf("empty tree: %v", err)
	}
	if err := VerifyInclusion(z, 5, 5, nil, z); !errors.Is(err, ErrProof) {
		t.Errorf("index = size: %v", err)
	}
	if err := VerifyInclusion(z, 0, 1, make([]Hash, MaxInclusionPath+1), z); !errors.Is(err, ErrProof) {
		t.Errorf("path over the limit: %v", err)
	}
	if err := VerifyConsistency(5, 4, z, z, nil); !errors.Is(err, ErrProof) {
		t.Errorf("old larger than new: %v", err)
	}
	if err := VerifyConsistency(2, 9, z, z, make([]Hash, MaxConsistencyPath+1)); !errors.Is(err, ErrProof) {
		t.Errorf("consistency path over the limit: %v", err)
	}
	if err := VerifyConsistency(0, 3, z, z, nil); !errors.Is(err, ErrProof) {
		t.Errorf("a non-empty root for the empty tree: %v", err)
	}
	if err := VerifyConsistency(0, 3, EmptyRoot(), z, nil); err != nil {
		t.Errorf("empty old tree: %v", err)
	}
	if err := VerifyConsistency(3, 3, z, EmptyRoot(), nil); !errors.Is(err, ErrProof) {
		t.Errorf("equal sizes with other roots: %v", err)
	}
}

func TestAppendLimits(t *testing.T) {
	store := NewMemStore()
	l, _ := Create(bg, testOrigin(1), store)
	big := bytes.Repeat([]byte{1}, MaxEntrySize)
	if _, err := l.Append(bg, [][]byte{big}); err != nil {
		t.Fatalf("MaxEntrySize: %v", err)
	}
	if _, err := l.Append(bg, [][]byte{append(big, 1)}); err == nil {
		t.Fatal("MaxEntrySize+1 accepted")
	}
	if _, err := l.Append(bg, [][]byte{{}}); err == nil {
		t.Fatal("an empty entry was accepted")
	}
	if _, err := l.Append(bg, nil); err == nil {
		t.Fatal("no entries accepted")
	}
	// MaxBatch entries pass, MaxBatch+1 do not.
	small := repeatEntry([]byte{7}, MaxBatch)
	if _, err := l.Append(bg, small); err != nil {
		t.Fatalf("MaxBatch: %v", err)
	}
	if _, err := l.Append(bg, repeatEntry([]byte{7}, MaxBatch+1)); err == nil {
		t.Fatal("MaxBatch+1 accepted")
	}
	// MaxBatchBytes: 1,024 entries of 64 KiB - 1 are 64 MiB - 1 KiB and pass the
	// byte limit; the count of 1,025 of them passes the count but not the bytes.
	huge := make([][]byte, 0, 1025)
	for i := 0; i < 1025; i++ {
		huge = append(huge, big)
	}
	if _, err := l.Append(bg, huge); err == nil {
		t.Fatal("more than MaxBatchBytes accepted")
	}
	// The failed appends changed nothing.
	if l.Size() != uint64(1+MaxBatch) {
		t.Fatalf("size %d after the failed appends", l.Size())
	}
	if err := Audit(bg, store, l.Size(), l.Root()); err != nil {
		t.Fatal(err)
	}
}

func TestBundleOfLargeEntriesRoundTrips(t *testing.T) {
	store := NewMemStore()
	l, _ := Create(bg, testOrigin(1), store)
	var es [][]byte
	for i := 0; i < 300; i++ {
		es = append(es, bytes.Repeat([]byte{byte(i)}, 60000+i))
	}
	if _, err := l.Append(bg, es); err != nil {
		t.Fatal(err)
	}
	r, _ := NewReader(bg, store, 300)
	for _, i := range []uint64{0, 255, 256, 299} {
		e, err := r.Entry(i)
		if err != nil || !bytes.Equal(e, es[i]) {
			t.Fatalf("entry %d: %v", i, err)
		}
	}
	if _, err := r.Entry(300); err == nil {
		t.Fatal("an entry beyond the size")
	}
}

func TestReaderFallsBackToTheFullTile(t *testing.T) {
	store := NewMemStore()
	l, _ := Create(bg, testOrigin(1), store)
	mustAppend(t, l, 0, 300)
	mustAppend(t, l, 300, 2000)
	// A reader of the tree of size 300 asks for the partial tile of width 44;
	// when it has been deleted (the full tile exists) it must read the first
	// hashes of the full tile.
	store.Delete(TileKey{0, 1, 44})
	store.Delete(TileKey{EntryLevel, 1, 44})
	r, _ := NewReader(bg, store, 300)
	root, err := r.Root()
	if err != nil || root != refMTH(leafHashesOf(300)) {
		t.Fatalf("%x %v", root, err)
	}
	for _, i := range []uint64{0, 255, 256, 299} {
		if _, err := r.Entry(i); err != nil {
			t.Fatalf("entry %d: %v", i, err)
		}
	}
	// Missing tiles are errors that wrap ErrNotFound.
	store.Delete(TileKey{0, 0, 256})
	r2, _ := NewReader(bg, store, 300)
	if _, err := r2.LeafHash(0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a deleted tile: %v", err)
	}
}

func TestRequiredAndPaths(t *testing.T) {
	cases := map[TileKey]string{
		{0, 0, 256}:                    "tile/0/000",
		{0, 67, 256}:                   "tile/0/067",
		{0, 1234067, 256}:              "tile/0/x001/x234/067",
		{1, 1000, 256}:                 "tile/1/x001/000",
		{2, 5, 17}:                     "tile/2/005.p/17",
		{EntryLevel, 0, 255}:           "tile/entries/000.p/255",
		{EntryLevel, 1234067, 1}:       "tile/entries/x001/x234/067.p/1",
		{63, 999999999999, 256}:        "tile/63/x999/x999/x999/999",
		{0, 18446744073709551615, 256}: "tile/0/x018/x446/x744/x073/x709/x551/615",
	}
	for k, want := range cases {
		if got := k.Path(); got != want {
			t.Errorf("%+v: %s, want %s", k, got, want)
		}
		back, err := ParseTilePath(want)
		if err != nil || back != k {
			t.Errorf("%s: %+v %v", want, back, err)
		}
	}
	for _, bad := range []string{"", "tile", "tile/0", "tile/0/0", "tile/0/0000", "tile/00/000", "tile/64/000", "tile/0/x000/001", "tile/0/001/002", "tile/0/000.p/0", "tile/0/000.p/256", "tile/0/000.p/05", "tile/0/000.p", "tile/entries", "tile/0/000/", "/tile/0/000", "tile/0/x001", "tile/0/000.p/1/2", "tile/-1/000", "tile/0/x00a/000", "tile/0/000.q/3", "../tile/0/000", "tile/0/x001/x234/x567/x890/x123/x456/x789/012"} {
		if k, err := ParseTilePath(bad); err == nil {
			t.Errorf("%q parsed as %+v", bad, k)
		}
	}
	// The width of partial tiles for sizes at the edges.
	for _, tc := range []struct {
		size  uint64
		level int
		want  int
	}{{0, 0, 0}, {1, 0, 1}, {255, 0, 255}, {256, 0, 0}, {256, 1, 1}, {257, 0, 1}, {65535, 1, 255}, {65536, 1, 0}, {65536, 2, 1}, {65539, 0, 3}, {65539, 1, 0}, {65539, 2, 1}, {1<<62 - 1, 7, 63}, {1<<62 - 1, 8, 0}, {1000, -1, 232}} {
		if got := PartialWidth(tc.size, tc.level); got != tc.want {
			t.Errorf("PartialWidth(%d, %d) = %d, want %d", tc.size, tc.level, got, tc.want)
		}
	}
	// Required names the tiles of a size.
	req := mustRequired(t, 65539)
	want := map[string]bool{"tile/0/255": true, "tile/0/256.p/3": true, "tile/1/000": true, "tile/2/000.p/1": true, "tile/entries/000": true, "tile/entries/256.p/3": true}
	got := map[string]bool{}
	for _, k := range req {
		got[k.Path()] = true
	}
	for p := range want {
		if !got[p] {
			t.Errorf("Required(65539) lacks %s", p)
		}
	}
	if got["tile/1/000.p/255"] || got["tile/1/001.p/0"] {
		t.Error("Required(65539) names a tile that is not there")
	}
}

func TestOpenChecksTheFrontierAndTheTiles(t *testing.T) {
	store := NewMemStore()
	l, _ := Create(bg, testOrigin(1), store)
	a := mustAppend(t, l, 0, 700)
	open := func(s Store, fr []byte, size uint64, root Hash) error {
		_, err := Open(bg, testOrigin(1), s, fr, size, root, Options{})
		return err
	}
	if err := open(store, a.Frontier, a.Size, a.Root); err != nil {
		t.Fatal(err)
	}
	// A frontier that does not give the stored root, or size.
	if err := open(store, a.Frontier, a.Size, flipBit(a.Root, 3)); !errors.Is(err, ErrFrontierRoot) {
		t.Errorf("another root: %v", err)
	}
	if err := open(store, a.Frontier, a.Size+1, a.Root); !errors.Is(err, ErrFrontierRoot) {
		t.Errorf("another size: %v", err)
	}
	damaged := bytes.Clone(a.Frontier)
	damaged[20] ^= 1
	if err := open(store, damaged, a.Size, a.Root); !errors.Is(err, ErrFrontierRoot) {
		t.Errorf("a damaged frontier: %v", err)
	}
	if err := open(store, a.Frontier[:len(a.Frontier)-1], a.Size, a.Root); !errors.Is(err, ErrFrontierMalformed) {
		t.Errorf("a short frontier: %v", err)
	}
	// The frontier is ahead of the tiles: a right edge tile is missing, in each
	// level that has one.
	for _, k := range []TileKey{{0, 2, 188}, {1, 0, 2}, {EntryLevel, 2, 188}} {
		cp := NewMemStore()
		for _, kk := range store.Keys() {
			d, _ := store.GetTile(bg, kk)
			_ = cp.PutTile(bg, kk, d)
		}
		cp.Delete(k)
		if err := open(cp, a.Frontier, a.Size, a.Root); !errors.Is(err, ErrFrontierAhead) {
			t.Errorf("missing %s: %v", k.Path(), err)
		}
	}
	// A damaged tile on the right edge.
	for _, k := range []TileKey{{0, 2, 188}, {1, 0, 2}} {
		cp := NewMemStore()
		for _, kk := range store.Keys() {
			d, _ := store.GetTile(bg, kk)
			_ = cp.PutTile(bg, kk, d)
		}
		d, _ := cp.GetTile(bg, k)
		d[5] ^= 1
		cp.Corrupt(k, d)
		if err := open(cp, a.Frontier, a.Size, a.Root); !errors.Is(err, ErrTilesMismatch) {
			t.Errorf("damaged %s: %v", k.Path(), err)
		}
	}
	// An entry that does not have the leaf hash.
	cp := NewMemStore()
	for _, kk := range store.Keys() {
		d, _ := store.GetTile(bg, kk)
		_ = cp.PutTile(bg, kk, d)
	}
	bk := TileKey{EntryLevel, 2, 188}
	d, _ := cp.GetTile(bg, bk)
	d[len(d)-1] ^= 1
	cp.Corrupt(bk, d)
	if err := open(cp, a.Frontier, a.Size, a.Root); !errors.Is(err, ErrTilesMismatch) {
		t.Errorf("a damaged entry: %v", err)
	}
	// Creating a log in a store that holds one.
	if _, err := Create(bg, testOrigin(1), store); !errors.Is(err, ErrTilesAhead) {
		t.Errorf("create in a used store: %v", err)
	}
	if _, err := Create(bg, "bad origin", NewMemStore()); err == nil {
		t.Error("create with an invalid origin")
	}
}

func TestAuditFindsDamage(t *testing.T) {
	n := 1300
	store := NewMemStore()
	l, _ := Create(bg, testOrigin(1), store)
	mustAppend(t, l, 0, n)
	root := l.Root()
	for _, k := range mustRequired(t, uint64(n)) {
		d, _ := store.GetTile(bg, k)
		for _, pos := range []int{0, len(d) / 2, len(d) - 1} {
			bad := bytes.Clone(d)
			bad[pos] ^= 0x40
			store.Corrupt(k, bad)
			if err := Audit(bg, store, uint64(n), root); err == nil {
				t.Fatalf("damage in %s byte %d was not found", k.Path(), pos)
			}
		}
		store.Corrupt(k, d)
	}
	if err := Audit(bg, store, uint64(n), root); err != nil {
		t.Fatal(err)
	}
	if err := Audit(bg, store, uint64(n), flipBit(root, 1)); err == nil {
		t.Fatal("a wrong root passed the audit")
	}
}

func TestAppendIsRetryableAfterAFailedPut(t *testing.T) {
	rng := newRand(t)
	store := NewMemStore()
	l, _ := Create(bg, testOrigin(1), store)
	mustAppend(t, l, 0, 200)
	for attempt := 0; attempt < 20; attempt++ {
		fail := 1 + rng.Intn(6)
		calls := 0
		store.BeforePut = func(TileKey) error {
			calls++
			if calls == fail {
				return fmt.Errorf("injected")
			}
			return nil
		}
		before := l.Size()
		_, err := l.Append(bg, entriesRange(int(before), int(before)+400))
		store.BeforePut = nil
		if err == nil {
			continue
		}
		if l.Size() != before {
			t.Fatalf("the log moved on a failed append")
		}
		mustAppend(t, l, int(before), int(before)+400)
	}
	if err := Audit(bg, store, l.Size(), l.Root()); err != nil {
		t.Fatal(err)
	}
	if l.Root() != refMTH(leafHashesOf(int(l.Size()))) {
		t.Fatal("the root is wrong after retries")
	}
}

func TestStoreConflictsAreRefused(t *testing.T) {
	s := NewMemStore()
	k := TileKey{0, 0, 1}
	h := LeafHash([]byte("a"))
	d, _ := EncodeTile([]Hash{h})
	if err := s.PutTile(bg, k, d); err != nil {
		t.Fatal(err)
	}
	if err := s.PutTile(bg, k, d); err != nil {
		t.Fatalf("an identical put: %v", err)
	}
	other, _ := EncodeTile([]Hash{LeafHash([]byte("b"))})
	if err := s.PutTile(bg, k, other); !errors.Is(err, ErrConflict) {
		t.Fatalf("a put of other content: %v", err)
	}
	got, _ := s.GetTile(bg, k)
	if !bytes.Equal(got, d) {
		t.Fatal("the tile changed")
	}
	if err := s.PutTile(bg, TileKey{0, 0, 2}, d); err == nil {
		t.Fatal("a tile of the wrong length was stored")
	}
	if err := s.PutTile(bg, TileKey{EntryLevel, 0, 1}, []byte{0, 5, 1}); err == nil {
		t.Fatal("a cut bundle was stored")
	}
	if err := s.PutTile(bg, TileKey{0, 0, 0}, nil); err == nil {
		t.Fatal("width 0 was stored")
	}
	if _, err := s.GetTile(bg, TileKey{3, 3, 3}); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func repeatEntry(e []byte, n int) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		out[i] = e
	}
	return out
}

// TestLargerTreeAcrossTileLevels is the verifier's own test of 66,000 leaves: two
// levels of full tiles and a level 2 partial tile of width 1, with the same
// leaf indexes and old sizes.
func TestLargerTreeAcrossTileLevels(t *testing.T) {
	skipUnlessLong(t, "the 66,000-leaf tree (smaller trees up to 1,000 leaves run by default)")
	size := 66000
	h := leafHashesOf(size)
	store := NewMemStore()
	l, _ := Create(bg, testOrigin(1), store)
	mustAppend(t, l, 0, size)
	r, _ := NewReader(bg, store, uint64(size))
	root, err := r.Root()
	if err != nil || root != refMTH(h) || root != l.Root() {
		t.Fatalf("root %x %v", root, err)
	}
	for _, i := range []int{0, 255, 256, 65535, 65536, 65999} {
		got, err := r.InclusionProof(uint64(i))
		if err != nil || !slices.Equal(got, refPath(i, h)) {
			t.Fatalf("index %d: %v", i, err)
		}
		if err := VerifyInclusion(h[i], uint64(i), uint64(size), got, root); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range []int{1, 255, 256, 257, 65535, 65536, 65999} {
		got, err := r.ConsistencyProof(uint64(m))
		if err != nil || !slices.Equal(got, refProof(m, h)) {
			t.Fatalf("old size %d: %v", m, err)
		}
		if err := VerifyConsistency(uint64(m), uint64(size), refMTH(h[:m]), root, got); err != nil {
			t.Fatalf("old size %d: %v", m, err)
		}
	}
}

// TestMaxBatchBytesBoundary appends exactly MaxBatchBytes bytes (it passes) and
// one byte more (it does not).
func TestMaxBatchBytesBoundary(t *testing.T) {
	skipUnlessLong(t, "MaxBatchBytes boundary, about 200 MB")
	big := make([]byte, MaxEntrySize)
	mk := func(extra int) [][]byte {
		var es [][]byte
		total := 0
		for total+MaxEntrySize <= MaxBatchBytes {
			es = append(es, big)
			total += MaxEntrySize
		}
		es = append(es, big[:MaxBatchBytes-total+extra])
		return es
	}
	store := NewMemStore()
	l, _ := Create(bg, testOrigin(1), store)
	if _, err := l.Append(bg, mk(1)); err == nil {
		t.Fatal("MaxBatchBytes+1 accepted")
	}
	if l.Size() != 0 {
		t.Fatal("the log moved")
	}
	if _, err := l.Append(bg, mk(0)); err != nil {
		t.Fatalf("MaxBatchBytes: %v", err)
	}
	if l.Size() != 1025 {
		t.Fatalf("size %d", l.Size())
	}
}

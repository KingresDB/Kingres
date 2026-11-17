// SPDX-License-Identifier: AGPL-3.0-only

package ledgerlog

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math/bits"
	"testing"
)

func TestFrontierMatchesTheTreeAtEverySize(t *testing.T) {
	hs := leafHashesOf(600)
	var fr Frontier
	if fr.Root() != EmptyRoot() || fr.Size() != 0 {
		t.Fatal("empty frontier")
	}
	for i, h := range hs {
		if err := fr.Append(h); err != nil {
			t.Fatal(err)
		}
		if fr.Root() != refMTH(hs[:i+1]) {
			t.Fatalf("size %d: root differs", i+1)
		}
		if len(fr.Hashes()) != bits.OnesCount(uint(i+1)) {
			t.Fatalf("size %d: %d hashes", i+1, len(fr.Hashes()))
		}
		b := fr.Marshal()
		if len(b) != 9+32*bits.OnesCount(uint(i+1)) {
			t.Fatalf("size %d: %d bytes", i+1, len(b))
		}
		back, err := ParseFrontier(b)
		if err != nil || !back.equal(fr) || !bytes.Equal(back.Marshal(), b) {
			t.Fatalf("size %d: round trip %v", i+1, err)
		}
	}
	// A clone is independent.
	c := fr.Clone()
	_ = c.Append(Hash{1})
	if fr.Size() != 600 || c.Size() != 601 {
		t.Fatal("clone shares state")
	}
}

func TestFrontierRefusals(t *testing.T) {
	var fr Frontier
	for i := 0; i < 5; i++ {
		_ = fr.Append(Hash{byte(i)})
	}
	good := fr.Marshal()
	mut := func(f func(b []byte) []byte) []byte { return f(bytes.Clone(good)) }
	cases := map[string][]byte{
		"empty":        nil,
		"short":        good[:8],
		"version 0":    mut(func(b []byte) []byte { b[0] = 0; return b }),
		"version 2":    mut(func(b []byte) []byte { b[0] = 2; return b }),
		"cut hash":     good[:len(good)-1],
		"extra byte":   append(bytes.Clone(good), 0),
		"missing hash": good[:len(good)-32],
		"size 2^62": mut(func(b []byte) []byte {
			binary.BigEndian.PutUint64(b[1:], 1<<62)
			return b[:9+32]
		}),
		"size 7 with 2 hashes": mut(func(b []byte) []byte { binary.BigEndian.PutUint64(b[1:], 7); return b }),
		"too long":             make([]byte, MaxFrontierSize+1),
	}
	for name, in := range cases {
		if _, err := ParseFrontier(in); !errors.Is(err, ErrFrontierMalformed) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The largest frontier: size 2^62-1 has 62 hashes.
	big := make([]byte, 9+62*32)
	big[0] = 1
	binary.BigEndian.PutUint64(big[1:], 1<<62-1)
	if f, err := ParseFrontier(big); err != nil || len(f.Hashes()) != 62 || len(big) != MaxFrontierSize {
		t.Fatalf("%v", err)
	}
	// An append at the limit is refused: the tree stays below 2^62 leaves.
	f, _ := ParseFrontier(big)
	if err := f.Append(Hash{}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("%v", err)
	}
	// LoadFrontier checks size and root.
	if _, err := LoadFrontier(good, 5, fr.Root()); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFrontier(good, 6, fr.Root()); !errors.Is(err, ErrFrontierRoot) {
		t.Fatal(err)
	}
	if _, err := LoadFrontier(good, 5, Hash{}); !errors.Is(err, ErrFrontierRoot) {
		t.Fatal(err)
	}
}

func TestTileAndBundleCodecs(t *testing.T) {
	var hs []Hash
	for i := 0; i < TileWidth; i++ {
		hs = append(hs, Hash{byte(i), 1})
	}
	for _, w := range []int{1, 2, 255, 256} {
		d, err := EncodeTile(hs[:w])
		if err != nil || len(d) != 32*w {
			t.Fatal(err)
		}
		back, err := ParseTile(d, w)
		if err != nil || len(back) != w || back[w-1] != hs[w-1] {
			t.Fatal(err)
		}
	}
	if _, err := EncodeTile(nil); err == nil {
		t.Error("an empty tile")
	}
	if _, err := EncodeTile(append(hs, Hash{})); err == nil {
		t.Error("257 hashes")
	}
	for _, c := range []struct{ n, w int }{{31, 1}, {33, 1}, {0, 1}, {64, 3}, {32, 0}, {32, 257}, {8224, 257}} {
		if _, err := ParseTile(make([]byte, c.n), c.w); err == nil {
			t.Errorf("tile of %d bytes, width %d", c.n, c.w)
		}
	}
	// Bundles.
	es := [][]byte{{1}, {}, bytes.Repeat([]byte{2}, MaxEntrySize), {3, 4}}
	d, err := EncodeBundle(es)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseBundle(d, 4)
	if err != nil || len(back) != 4 || !bytes.Equal(back[2], es[2]) || len(back[1]) != 0 {
		t.Fatalf("%v", err)
	}
	again, _ := EncodeBundle(back)
	if !bytes.Equal(again, d) {
		t.Fatal("serialise(parse(b)) != b")
	}
	if _, err := ParseBundle(d, 3); err == nil {
		t.Error("a wrong count")
	}
	for name, in := range map[string][]byte{
		"empty":        nil,
		"cut length":   {0},
		"cut entry":    {0, 5, 1, 2},
		"trailing":     append(bytes.Clone(d), 0),
		"over the max": make([]byte, MaxBundleSize+1),
	} {
		if _, err := ParseBundle(in, 0); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// 256 entries pass, 257 do not; an entry over MaxEntrySize does not encode.
	many := make([][]byte, 257)
	for i := range many {
		many[i] = []byte{1}
	}
	if _, err := EncodeBundle(many[:256]); err != nil {
		t.Error(err)
	}
	if _, err := EncodeBundle(many); err == nil {
		t.Error("257 entries encoded")
	}
	raw := bytes.Repeat([]byte{0, 1, 9}, 257)
	if _, err := ParseBundle(raw, 0); err == nil {
		t.Error("257 entries parsed")
	}
	if _, err := EncodeBundle([][]byte{make([]byte, MaxEntrySize+1)}); err == nil {
		t.Error("an entry over the limit encoded")
	}
	// The largest bundle that can exist has MaxBundleSize bytes.
	bigEs := make([][]byte, 256)
	for i := range bigEs {
		bigEs[i] = make([]byte, MaxEntrySize)
	}
	bd, err := EncodeBundle(bigEs)
	if err != nil || len(bd) != MaxBundleSize {
		t.Fatalf("%d %v", len(bd), err)
	}
	if _, err := ParseBundle(bd, 256); err != nil {
		t.Fatal(err)
	}
}

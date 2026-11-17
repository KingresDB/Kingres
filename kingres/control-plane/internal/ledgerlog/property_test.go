// SPDX-License-Identifier: AGPL-3.0-only

package ledgerlog

import (
	"bytes"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func randomOrigin(r interface{ Intn(int) int }) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789./-_"
	n := 1 + r.Intn(MaxOriginSize)
	var sb strings.Builder
	for i := 0; i < n; i++ {
		sb.WriteByte(alphabet[r.Intn(len(alphabet))])
	}
	return sb.String()
}

// TestRoundTripProperties: parse(serialise(x)) = x and serialise(parse(b)) = b
// for the checkpoint, the signed note, the frontier, tiles, bundles, and tile
// paths, over random values. The seed is printed on failure.
func TestRoundTripProperties(t *testing.T) {
	rng := newRand(t)
	k1, k2 := testKey(t, 3), testKey(t, 9)
	for i := 0; i < 200; i++ {
		var c Checkpoint
		c.Origin = randomOrigin(rng)
		c.Size = uint64(rng.Int63()) % MaxTreeSize
		if rng.Intn(10) == 0 {
			c.Size = 0
			c.Root = EmptyRoot()
		} else {
			rng.Read(c.Root[:])
		}
		for j, n := 0, rng.Intn(MaxExtensionLines+1); j < n; j++ {
			w := randomOrigin(rng)
			c.Extensions = append(c.Extensions, w[:min(len(w), 1+rng.Intn(20))])
		}
		text, err := c.Text()
		if err != nil {
			t.Fatal(err)
		}
		back, err := ParseCheckpoint(text)
		if err != nil || !reflect.DeepEqual(back, c) {
			t.Fatalf("parse(serialise(x)) != x for %+v: %v", c, err)
		}
		again, _ := back.Text()
		if !bytes.Equal(again, text) {
			t.Fatal("serialise(parse(b)) != b")
		}
		signers := k1
		note, err := SignCheckpoint(bg, c, signers)
		if rng.Intn(2) == 0 {
			note, err = SignCheckpoint(bg, c, k1, k2)
		}
		if err != nil {
			t.Fatal(err)
		}
		n, err := ParseNote(note)
		if err != nil {
			t.Fatal(err)
		}
		nb, err := n.Marshal()
		if err != nil || !bytes.Equal(nb, note) {
			t.Fatal("serialise(parse(note)) != note")
		}
		if got, _, err := VerifyCheckpoint(note, c.Origin, k1.Public(), k2.Public()); err != nil || !reflect.DeepEqual(got, c) {
			t.Fatalf("verify: %v", err)
		}
		if pn, err := ParseNote(nb); err != nil || !reflect.DeepEqual(pn, n) {
			t.Fatal("parse(serialise(x)) != x for a note")
		}
	}
	for i := 0; i < 100; i++ {
		var fr Frontier
		n := rng.Intn(3000)
		for j := 0; j < n; j++ {
			var h Hash
			rng.Read(h[:])
			_ = fr.Append(h)
		}
		b := fr.Marshal()
		back, err := ParseFrontier(b)
		if err != nil || !back.equal(fr) || back.Root() != fr.Root() {
			t.Fatal("frontier round trip")
		}
	}
	for i := 0; i < 100; i++ {
		var hs []Hash
		for j, n := 0, 1+rng.Intn(TileWidth); j < n; j++ {
			var h Hash
			rng.Read(h[:])
			hs = append(hs, h)
		}
		d, _ := EncodeTile(hs)
		back, err := ParseTile(d, len(hs))
		if err != nil || !reflect.DeepEqual(back, hs) {
			t.Fatal("tile round trip")
		}
		var es [][]byte
		for j, n := 0, 1+rng.Intn(TileWidth); j < n; j++ {
			e := make([]byte, rng.Intn(300))
			rng.Read(e)
			es = append(es, e)
		}
		bd, err := EncodeBundle(es)
		if err != nil {
			t.Fatal(err)
		}
		bb, err := ParseBundle(bd, len(es))
		if err != nil || len(bb) != len(es) {
			t.Fatal("bundle round trip")
		}
		for j := range es {
			if !bytes.Equal(bb[j], es[j]) {
				t.Fatal("bundle entry")
			}
		}
		k := TileKey{Level: rng.Intn(MaxLevel+2) - 1, Index: uint64(rng.Int63()), Width: 1 + rng.Intn(TileWidth)}
		back2, err := ParseTilePath(k.Path())
		if err != nil || back2 != k {
			t.Fatalf("tile path %+v: %v", k, err)
		}
	}
}

// TestConcurrentReadsDuringAppends is for the race detector.
func TestConcurrentReadsDuringAppends(t *testing.T) {
	store := NewMemStore()
	l, _ := Create(bg, testOrigin(1), store)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = l.Size()
					_ = l.Root()
					_ = l.Frontier()
					_ = l.Checkpoint()
				}
			}
		}()
	}
	for i := 0; i < 50; i++ {
		mustAppend(t, l, i*10, i*10+10)
	}
	close(stop)
	wg.Wait()
	if err := Audit(bg, store, l.Size(), l.Root()); err != nil {
		t.Fatal(err)
	}
}

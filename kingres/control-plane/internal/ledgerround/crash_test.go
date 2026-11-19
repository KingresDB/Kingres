// SPDX-License-Identifier: AGPL-3.0-only

package ledgerround

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerlog"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerpub"
)

// snapshot is the whole published state of a store: every log with its frontier, every tile, every checkpoint note.
func snapshot(t *testing.T, e *env) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, l := range e.logs() {
		out[l.Origin+"|row"] = fmt.Sprintf("%s %d %x %x %x pending=%d closed=%v", l.State, l.Size, l.Root, l.Frontier, l.HeadBlockHash, len(l.PendingEntry), l.ClosedAt != nil)
		cps, err := e.store.Checkpoints(bg, l.ID, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cps {
			out[fmt.Sprintf("%s|cp|%d", l.Origin, c.Size)] = fmt.Sprintf("%x %x", c.Root, c.Note)
		}
		raw := e.store.RawTiles(l.ID)
		for _, k := range raw.Keys() {
			d, _ := raw.GetTile(bg, k)
			out[l.Origin+"|tile|"+k.Path()] = fmt.Sprintf("%x", d)
		}
	}
	return out
}

func diffSnapshots(t *testing.T, got, want map[string]string, what string) {
	t.Helper()
	for k, v := range want {
		if g, ok := got[k]; !ok {
			t.Errorf("%s: %s is missing", what, k)
		} else if g != v {
			t.Errorf("%s: %s differs", what, k)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("%s: %s is extra", what, k)
		}
	}
}

// A kill at any write of a round: the next process (a new collector over the same store) repairs exactly what the library prescribes,
// and the published state ends up byte for byte what an uninterrupted run gives. The writes are counted by the store and each one is cut
// in turn, before it is made, which is the state a kill -9 at that write leaves (the writes are atomic).
func TestARoundCutAtAnyWriteIsRepairedByTheNext(t *testing.T) {
	for _, phase := range []struct {
		name         string
		first, later int // blocks of the first round and added before the cut round
	}{{"first round", 0, 5}, {"incremental round across a tile", 3, 300}, {"incremental round, one block", 256, 1}} {
		t.Run(phase.name, func(t *testing.T) {
			build := func() (*env, *fakeChain, *fakeChain) {
				a, b := newChain(1, max(phase.first, 1)), newChain(40, 2)
				e := newEnv(t, a, b)
				if phase.first > 0 {
					e.round()
					e.advance(60e9)
				} else {
					a.blocks = nil
				}
				return e, a, b
			}
			grow := func(a, b *fakeChain) {
				if phase.first == 0 {
					a.add(phase.later)
				} else {
					a.add(phase.later)
				}
				b.add(1)
			}
			ref, ra, rb := build()
			grow(ra, rb)
			refBase := ref.store.Writes()
			ref.round()
			want := snapshot(t, ref)
			writesInRound := ref.store.Writes() - refBase
			if writesInRound < 4 {
				t.Fatalf("a round made only %d writes", writesInRound)
			}
			for cut := 1; cut <= writesInRound; cut++ {
				e, a, b := build()
				grow(a, b)
				base := e.store.Writes()
				e.store.BeforeWrite = func(kind string, n int) error {
					if n == base+cut {
						return fmt.Errorf("%w at the %s write number %d", errInjected, kind, cut)
					}
					return nil
				}
				if _, err := e.coll.Round(bg, "shop", OpHead); !errors.Is(err, errInjected) {
					t.Fatalf("cut %d: the round returned %v, not the injected failure", cut, err)
				}
				e.store.BeforeWrite = nil
				e.restart() // a new process
				for i := 0; i < 3; i++ {
					e.round()
				}
				diffSnapshots(t, snapshot(t, e), want, fmt.Sprintf("cut at write %d of %d", cut, writesInRound))
				for _, l := range e.logs() {
					if l.State != ledgerpub.StateActive {
						t.Errorf("cut %d: the log %s is %s (%s)", cut, l.Origin, l.State, l.Detail)
					}
					e.verifyLog(l, e.keys.signers...)
				}
			}
		})
	}
}

// A kill between the tiles and the frontier leaves tiles ahead; between the frontier and the checkpoint a tree without a checkpoint. The
// library names the first ErrTilesAhead and the collector repairs it by appending the same leaves again (never with other leaves).
func TestTilesAheadOfTheFrontierAreRepairedWithTheSameLeavesAndNeverWithOthers(t *testing.T) {
	a := newChain(1, 3)
	e := newEnv(t, a)
	a.add(2)
	// write the tiles of the next size by hand (the state a kill between the tiles and the frontier leaves)
	e.round()
	l := e.chainLog(0)
	lg, err := e.coll.openStored(bg, 5, l)
	if err != nil {
		t.Fatal(err)
	}
	var more [][]byte
	for i := 5; i < 8; i++ {
		a.add(1)
		more = append(more, a.entry(i))
	}
	if _, err := lg.Append(bg, more); err != nil {
		t.Fatal(err)
	}
	// the frontier still says 5: a normal round appends the same three leaves and finishes
	e.round()
	cp := e.verifyLog(e.chainLog(0), e.keys.signers...)
	if cp.Size != 8 || cp.Root != referenceRoot(a, 8) {
		t.Fatalf("size %d", cp.Size)
	}

	// other leaves for the same tiles: the log stops (ErrConflict is a divergence) and is not repaired
	b := newChain(90, 5)
	e2 := newEnv(t, b)
	e2.round()
	l2 := e2.chainLog(0)
	lg2, err := e2.coll.openStored(bg, 5, l2)
	if err != nil {
		t.Fatal(err)
	}
	other := newChain(200, 8) // a different history
	var rest [][]byte
	for i := 5; i < 8; i++ {
		rest = append(rest, other.entry(i))
	}
	if _, err := lg2.Append(bg, rest); err != nil {
		t.Fatal(err)
	}
	b.add(3)
	e2.round()
	if got := e2.chainLog(0); got.State != ledgerpub.StateDiverged || got.Size != 5 {
		t.Fatalf("the log after tiles of other leaves: %s size %d", got.State, got.Size)
	}
	if !e2.hasAlert(AlertDiverged + "/raise/critical/") {
		t.Error("no alert")
	}
	// the tiles that were there are untouched
	d, _ := e2.store.RawTiles(l2.ID).GetTile(bg, ledgerlog.TileKey{Level: 0, Index: 0, Width: 8})
	if len(d) != 8*32 {
		t.Errorf("tile of width 8: %d bytes", len(d))
	}
	_ = bytes.Equal
}

// The leaf of the aggregate holds the time of the round, so a kill between its tiles and its frontier must append the same bytes again, not a
// leaf made again with the time of the next round (that would be another leaf for the tiles that were written: a conflict, a stopped log).
func TestAnAggregateLeafCutBeforeItsFrontierIsAppendedAgainWithTheSameBytes(t *testing.T) {
	build := func() (*env, *fakeChain) {
		a := newChain(1, 3)
		return newEnv(t, a, newChain(40, 2)), a
	}
	ref, _ := build()
	frontiers := 0
	ref.store.BeforeWrite = func(kind string, _ int) error {
		if kind == "frontier" {
			frontiers++
		}
		return nil
	}
	ref.round()
	if frontiers != 3 { // two chains and the aggregate
		t.Fatalf("%d frontiers in a first round", frontiers)
	}
	e, _ := build()
	start := e.now
	n := 0
	e.store.BeforeWrite = func(kind string, _ int) error {
		if kind == "frontier" {
			if n++; n == 3 {
				return errInjected // the frontier of the aggregate, after its tiles were written
			}
		}
		return nil
	}
	if _, err := e.coll.Round(bg, "shop", OpHead); !errors.Is(err, errInjected) {
		t.Fatalf("%v", err)
	}
	e.store.BeforeWrite = nil
	e.restart()
	e.advance(5 * time.Second) // the next process runs later: a leaf made again would carry another time
	for i := 0; i < 2; i++ {
		e.round()
	}
	ar := e.aggregateLog()
	if ar.State != ledgerpub.StateActive || ar.Size != 1 {
		t.Fatalf("the aggregate log is %s with %d leaves (%s)", ar.State, ar.Size, ar.Detail)
	}
	e.verifyLog(ar, e.keys.signers...)
	if got, want := leafAt(t, e, 0).GeneratedAtUS, microsSince2000(start); got != want {
		t.Errorf("the leaf has the time %d, the round that made it had %d", got, want)
	}
}

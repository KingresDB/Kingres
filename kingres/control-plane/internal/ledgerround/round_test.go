// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ledgerround

import (
	"bytes"
	"errors"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerlog"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerpub"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
)

// The tree vectors of the verifier (same sizes, same roots): a chain log built by the collector from block headers has the root that the
// plain algorithm of RFC 9162 gives over the leaf hashes, at every size the verifier's own tests use around the edges of a tile, and its tiles
// are the ones ledgerlog.Required lists for that size, which Audit reads back against the checkpoint root.
func TestChainLogsMatchTheReferenceTreeAtTheSizesOfTheVerifier(t *testing.T) {
	sizes := []int{1, 2, 3, 5, 8, 255, 256, 257, 300}
	for _, n := range sizes {
		c := newChain(10, n)
		e := newEnv(t, c)
		e.round()
		l := e.chainLog(0)
		cp := e.verifyLog(l, e.keys.signers...)
		want := referenceRoot(c, n)
		if cp.Size != uint64(n) || cp.Root != want {
			t.Errorf("size %d: root %x, reference %x (size %d)", n, cp.Root, want, cp.Size)
		}
		if !bytes.Equal(l.HeadBlockHash, func() []byte { h := c.hashOf(n - 1); return h[:] }()) {
			t.Errorf("size %d: head block hash differs", n)
		}
	}
}

func TestRoundsExtendTheLogsAndTheAggregateOnlyWhenSomethingChanged(t *testing.T) {
	a, b := newChain(1, 3), newChain(40, 2)
	e := newEnv(t, a, b)
	res := e.round()
	if !res.Published || len(res.Uncovered()) != 0 || res.AggregateSize != 1 {
		t.Fatalf("first round: %+v", res)
	}
	e.verifyLog(e.chainLog(0), e.keys.signers...)
	e.verifyLog(e.chainLog(1), e.keys.signers...)
	agg := e.verifyLog(e.aggregateLog(), e.keys.signers...)
	if agg.Size != 1 {
		t.Fatalf("aggregate size %d", agg.Size)
	}
	// Nothing changed: no new tile, no new checkpoint, no new leaf.
	w := e.store.Writes()
	e.round()
	if e.store.Writes() != w {
		t.Errorf("an idle round wrote %d times", e.store.Writes()-w)
	}
	// One chain grows: its log gets a checkpoint and the aggregate a second leaf with both members.
	a.add(300)
	res = e.round()
	if res.AggregateSize != 2 {
		t.Fatalf("aggregate size after growth: %d", res.AggregateSize)
	}
	cp := e.verifyLog(e.chainLog(0), e.keys.signers...)
	if cp.Size != 303 || cp.Root != referenceRoot(a, 303) {
		t.Errorf("chain log after growth: size %d", cp.Size)
	}
	if got := e.chainLog(1).Size; got != 2 {
		t.Errorf("the other chain moved to %d", got)
	}
	agg = e.verifyLog(e.aggregateLog(), e.keys.signers...)
	r, err := ledgerlog.NewReader(bg, e.store.Tiles(e.aggregateLog().ID, 0), agg.Size)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := r.Entry(1)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ledgerlog.ParseAggregateEntry(raw)
	if err != nil || leaf.Seq != 1 || len(leaf.Members) != 2 {
		t.Fatalf("leaf %+v, %v", leaf, err)
	}
	for _, m := range leaf.Members {
		if m.Flags != 0 {
			t.Errorf("flags %x", m.Flags)
		}
		if m.Epoch != 1 {
			t.Errorf("epoch %d", m.Epoch)
		}
	}
	// the log of the first chain is consistent with its earlier size (a consistency proof exists between the two roots)
	rd, err := ledgerlog.NewReader(bg, e.store.Tiles(e.chainLog(0).ID, 0), 303)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := rd.ConsistencyProof(3)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledgerlog.VerifyConsistency(3, 303, referenceRoot(a, 3), referenceRoot(a, 303), proof); err != nil {
		t.Errorf("consistency: %v", err)
	}
}

func TestAGroupThatCannotBeReadMakesTheRoundIncompleteAndItsMemberStale(t *testing.T) {
	a, b := newChain(1, 3), newChain(40, 2)
	e := newEnv(t, a, b)
	e.round()
	e.src.err[1] = &ReadError{Transport: true, Message: "timeout"}
	a.add(1)
	res := e.round()
	if got := res.Uncovered(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("uncovered %v", got)
	}
	if !res.Published || res.AggregateSize != 2 {
		t.Fatalf("the other group must still be published: %+v", res)
	}
	r, _ := ledgerlog.NewReader(bg, e.store.Tiles(e.aggregateLog().ID, 0), 2)
	raw, _ := r.Entry(1)
	leaf, _ := ledgerlog.ParseAggregateEntry(raw)
	var stale, fresh int
	for _, m := range leaf.Members {
		switch {
		case m.Flags&ledgerlog.FlagStale != 0:
			stale++
			if m.Height != 1 {
				t.Errorf("a stale member repeats the last good head, got height %d", m.Height)
			}
		default:
			fresh++
			if m.Height != 3 {
				t.Errorf("the readable chain is at height 3 (4 blocks), got %d", m.Height)
			}
		}
	}
	if stale != 1 || fresh != 1 {
		t.Fatalf("stale %d fresh %d", stale, fresh)
	}
	// back: the flag is cleared in the next leaf
	delete(e.src.err, 1)
	e.advance(61e9)
	res = e.round()
	if len(res.Uncovered()) != 0 || res.AggregateSize != 3 {
		t.Fatalf("after the group came back: %+v", res)
	}
	r, _ = ledgerlog.NewReader(bg, e.store.Tiles(e.aggregateLog().ID, 0), 3)
	raw, _ = r.Entry(2)
	leaf, _ = ledgerlog.ParseAggregateEntry(raw)
	for _, m := range leaf.Members {
		if m.Flags != 0 {
			t.Errorf("flag %x after the group came back", m.Flags)
		}
	}
}

func TestAnUnreadGroupRaisesTheAlertAfterAMinuteAndItIsClearedWhenRead(t *testing.T) {
	a := newChain(1, 3)
	e := newEnv(t, a)
	e.round()
	e.src.err[0] = &ReadError{Transport: true, Message: "refused"}
	e.round()
	if e.hasAlert(AlertUnread + "/raise/") {
		t.Error("an alert before a minute")
	}
	e.advance(90e9)
	e.round()
	if !e.hasAlert(AlertUnread + "/raise/warning/database:shop|group:0") {
		t.Errorf("alerts %v", e.alerts)
	}
	e.advance(3600e9)
	e.round()
	if !e.hasAlert(AlertUnread + "/raise/critical/") {
		t.Errorf("alerts %v", e.alerts)
	}
	delete(e.src.err, 0)
	e.round()
	if !e.hasAlert(AlertUnread + "/clear/") {
		t.Errorf("alerts %v", e.alerts)
	}
}

func TestAFollowerNeverWritesAndADeposedLeaderWritesNothing(t *testing.T) {
	e := newEnv(t, newChain(1, 3))
	e.keys.term = 0 // a follower holds no term
	if _, err := e.coll.Round(bg, "shop", OpHead); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("a follower's round: %v", err)
	}
	if e.store.Writes() != 0 {
		t.Fatalf("a follower wrote %d times", e.store.Writes())
	}
	// a deposed leader: its term is not the stored one; the first write is refused and nothing is kept
	e.keys.term = 4
	_, err := e.coll.Round(bg, "shop", OpHead)
	if !errors.Is(err, scheduler.ErrDeposed) {
		t.Fatalf("a deposed leader's round: %v", err)
	}
	if len(e.logs()) != 0 {
		t.Errorf("a deposed leader created %d logs", len(e.logs()))
	}
	// a leader that is deposed in the middle of a round (the term changes after the first tiles): nothing more is written
	e.keys.term = 5
	n := 0
	e.store.BeforeWrite = func(kind string, _ int) error {
		n++
		if n == 3 {
			return scheduler.ErrDeposed
		}
		return nil
	}
	w := 0
	if _, err := e.coll.Round(bg, "shop", OpHead); !errors.Is(err, scheduler.ErrDeposed) {
		t.Fatalf("a round cut by a new term: %v", err)
	}
	w = e.store.Writes()
	if w != 3 {
		t.Errorf("writes after the term ended: %d", w)
	}
}

func TestPublicationOffWritesNothingAndAKeyThatCannotSignWritesNoCheckpoint(t *testing.T) {
	e := newEnv(t, newChain(1, 3))
	e.keys.enabled = false
	res := e.round()
	if res.Published || e.store.Writes() != 0 || !res.Answered() {
		t.Fatalf("publication off: %+v, %d writes", res, e.store.Writes())
	}
	e.keys.enabled = true
	e.keys.signErr = errors.New("no key is in force")
	res = e.round()
	if res.Published || res.SignErr == nil || e.store.Writes() != 0 {
		t.Fatalf("a signer that cannot sign: %+v, %d writes", res, e.store.Writes())
	}
	e.round()
	if e.coll.SignFailure() == "" {
		t.Error("no signing failure reported after two rounds")
	}
	e.keys.signErr = nil
	e.round()
	if got := e.coll.SignFailure(); got != "" {
		t.Errorf("not cleared: %q", got)
	}
}

// Signing a checkpoint and retiring a key exclude each other (corrections 128 and 129): when the key set changed after the signer was
// loaded, the checkpoint is not stored and the next round signs again.
func TestACheckpointIsNotStoredWhenTheKeySetChangedAfterTheSignerWasLoaded(t *testing.T) {
	e := newEnv(t, newChain(1, 3))
	e.keys.bumpAfter = true
	res := e.round()
	if !res.KeysChanged || res.Published {
		t.Fatalf("round: %+v", res)
	}
	if _, err := e.store.LatestCheckpoint(bg, e.chainLog(0).ID); !errors.Is(err, ledgerpub.ErrNotFound) {
		t.Fatalf("a checkpoint was stored: %v", err)
	}
	// the frontier and the tiles are there (the log is ahead of its checkpoint, which a crash leaves too); the next round signs
	res = e.round()
	if !res.Published {
		t.Fatalf("the next round: %+v", res)
	}
	e.verifyLog(e.chainLog(0), e.keys.signers...)
}

func TestTheFirstCheckpointLocksThePrefix(t *testing.T) {
	e := newEnv(t, newChain(1, 3))
	e.round()
	if ok, p := e.store.FirstCheckpointMarked(); !ok || p != testPrefix {
		t.Errorf("first checkpoint marked %v prefix %q", ok, p)
	}
}

func TestADatabaseWithMoreChainsThanAnAggregateHoldsIsRefused(t *testing.T) {
	var chains []*fakeChain
	for i := 0; i < 513; i++ {
		chains = append(chains, newChain(byte(i), 1))
		chains[i].id[1], chains[i].id[2] = byte(i>>8), byte(i)
	}
	e := newEnv(t, chains[:512]...)
	res := e.round()
	if res.AggregateSize != 1 {
		t.Fatalf("512 chains: %+v", res)
	}
	r, err := ledgerlog.NewReader(bg, e.store.Tiles(e.aggregateLog().ID, 0), 1)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := r.Entry(0)
	if err != nil || len(raw) != ledgerlog.MaxAggregateSize {
		t.Fatalf("the leaf of 512 members: %d bytes, %v (want %d)", len(raw), err, ledgerlog.MaxAggregateSize)
	}
	e.src.groups = append(e.src.groups, Group{ID: 512, Server: "srv512"})
	e.src.chains[512] = chains[512]
	if _, err := e.coll.Round(bg, "shop", OpHead); !errors.Is(err, ErrTooManyChains) {
		t.Fatalf("513 chains: %v", err)
	}
	if got := e.aggregateLog().Size; got != 1 {
		t.Errorf("the aggregate moved to %d", got)
	}
}

// A concurrent writer of the cluster row (a key task that commits at the same moment: a deadlock of share locks) is a try again; only a conflict that
// the store found in what is stored stops a log.
func TestABusyStoreIsRetriedAndOnlyAConflictStopsALog(t *testing.T) {
	e := newEnv(t, newChain(1, 3))
	e.store.BeforeWrite = func(kind string, _ int) error {
		if kind == "checkpoint" {
			return ledgerpub.ErrBusy
		}
		return nil
	}
	if _, err := e.coll.Round(bg, "shop", OpHead); !errors.Is(err, ledgerpub.ErrBusy) {
		t.Fatalf("%v", err)
	}
	if got := e.chainLog(0); got.State != ledgerpub.StateActive {
		t.Fatalf("a busy store stopped the log: %s", got.State)
	}
	e.store.BeforeWrite = nil
	e.round()
	e.verifyLog(e.chainLog(0), e.keys.signers...)

	e2 := newEnv(t, newChain(1, 3))
	e2.store.BeforeWrite = func(kind string, _ int) error {
		if kind == "checkpoint" {
			return ledgerpub.ErrConflict
		}
		return nil
	}
	_, _ = e2.coll.Round(bg, "shop", OpHead) // the aggregate log meets the same conflict and reports it too
	if got := e2.chainLog(0); got.State != ledgerpub.StateDiverged {
		t.Fatalf("a conflict of the store must stop the log: %s", got.State)
	}
}

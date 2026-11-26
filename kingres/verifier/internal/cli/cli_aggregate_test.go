// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/aggregate"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
)

func wantRun(t *testing.T, code int, args []string, contains ...string) string {
	t.Helper()
	got, out, errs := run(t, args...)
	if got != code {
		t.Fatalf("status %d, want %d\n%s%s", got, code, out, errs)
	}
	all := out + errs
	for _, c := range contains {
		if !strings.Contains(all, c) {
			t.Fatalf("the output lacks %q:\n%s", c, all)
		}
	}
	if code != 0 && strings.Contains(out, "VERIFIED:") {
		t.Fatalf("VERIFIED with status %d", code)
	}
	return all
}

func TestAggregateVerifiesAPublicationOfTwoChains(t *testing.T) {
	w, _, _ := stdWorld(t)
	out := wantRun(t, 0, w.aggArgs(), "VERIFIED:", "round 1, 2 member(s)", "not proved: ", "no global snapshot", "no --previous was given")
	if !strings.Contains(out, "  ok  member ") {
		t.Fatal(out)
	}
	wantRun(t, 0, w.aggArgs("--json"), `"verified": true`, `"members"`)
}

func TestAggregateContinuityWithAnEarlierRound(t *testing.T) {
	w, a, b := stdWorld(t)
	// the auditor holds the checkpoint of size 1
	note := filepath.Join(testdir.New(t), "agg1.note")
	w.publishAggregate(1)
	copyFile(t, w.logDir("a/"+w.aggID+"/checkpoint"), note)
	w.publishAggregate(2)
	wantRun(t, 0, w.aggArgs("--previous", note), "VERIFIED:", "ok  continuity", "aggregate-consistency")
	_ = a
	_ = b
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Every refusal has a test; each is the mutation of one thing.

func TestAggregateRefusesAMemberWhoseRootIsNotTheChainLogs(t *testing.T) {
	w := newWorld(t)
	a := makeChain(0xa1, 5, nil)
	w.addChain(a, 5)
	m := member(a, 5, 0)
	m.LogRoot[0] ^= 1
	w.appendRound(10_000_000, m)
	wantRun(t, 1, w.aggArgs(), "FAILED at step member-root")
}

func TestAggregateRefusesAMemberWhoseBlockHashIsNotTheLogs(t *testing.T) {
	w := newWorld(t)
	a := makeChain(0xa1, 5, nil)
	w.addChain(a, 5)
	m := member(a, 5, 0)
	m.BlockHash[0] ^= 1
	w.appendRound(10_000_000, m)
	wantRun(t, 1, w.aggArgs(), "FAILED at step member-block")
}

func TestAggregateRefusesAMemberAheadOfTheChainCheckpoint(t *testing.T) {
	w := newWorld(t)
	a := makeChain(0xa1, 5, nil)
	w.addChain(a, 3)
	w.appendRound(10_000_000, member(a, 5, 0))
	wantRun(t, 1, w.aggArgs(), "FAILED at step member", "smaller than the 5 blocks")
}

func TestAggregateRefusesADivergedMemberAndReportsAStaleOne(t *testing.T) {
	w := newWorld(t)
	a := makeChain(0xa1, 5, nil)
	w.addChain(a, 5)
	w.appendRound(10_000_000, member(a, 5, aggregate.FlagDiverged))
	wantRun(t, 1, w.aggArgs(), "FAILED at step diverged")
	w2 := newWorld(t)
	w2.addChain(a, 5)
	w2.appendRound(10_000_000, member(a, 5, aggregate.FlagStale))
	wantRun(t, 3, w2.aggArgs(), "INCOMPLETE", "flag stale")
}

func TestAggregateRefusesAWrongTrustedCheckpointAndAMissingChain(t *testing.T) {
	w, _, b := stdWorld(t)
	// the chain log of b is not there: could not check
	if err := os.RemoveAll(w.logDir("c/" + b.idHex)); err != nil {
		t.Fatal(err)
	}
	wantRun(t, 3, w.aggArgs(), "INCOMPLETE", "does not contain")
	// a trust file that pins another key
	w2, _, _ := stdWorld(t)
	w2.trust = w2.writeTrust(newSigner(9))
	wantRun(t, 1, w2.aggArgs(), "FAILED at step signature")
}

func TestAggregateRefusesALeafWhoseSequenceIsNotItsIndex(t *testing.T) {
	w := newWorld(t)
	a := makeChain(0xa1, 5, nil)
	w.addChain(a, 5)
	w.appendRound(10_000_000, member(a, 5, 0))
	// publish a log whose leaf says it is round 4
	w.leaves[0].Seq = 4
	w.aggEnc[0] = w.leaves[0].Encode()
	w.publishAggregate(1)
	wantRun(t, 1, w.aggArgs(), "FAILED at step aggregate-leaf", "round 4")
}

func TestAggregateRefusesALeafWithUnsortedMembers(t *testing.T) {
	w := newWorld(t)
	a, b := makeChain(0xa1, 5, nil), makeChain(0xb2, 3, nil)
	w.addChain(a, 5)
	w.addChain(b, 3)
	l := &aggregate.Leaf{Seq: 0, GeneratedAtUS: 1, Members: []aggregate.Member{member(b, 3, 0), member(a, 5, 0)}} // not sorted
	w.leaves = append(w.leaves, l)
	w.aggEnc = append(w.aggEnc, l.Encode())
	w.publishAggregate(1)
	wantRun(t, 1, w.aggArgs(), "FAILED at step aggregate-leaf", "out of order")
}

func TestAggregateRetiredMemberNeedsAnAuthorisedEvent(t *testing.T) {
	w := newWorld(t)
	a := makeChain(0xa1, 5, nil)
	w.addChain(a, 5)
	w.appendRound(10_000_000, member(a, 5, aggregate.FlagRetired))
	// no events at all: could not check
	wantRun(t, 3, w.aggArgs(), "INCOMPLETE", "no chain-retired event")
	// a valid event signed by the pinned key
	w.event(0, "key-added", []signer{w.s1}, keyAddedLines(w.s1)...)
	w.event(1, "chain-retired", []signer{w.s1}, chainRetiredLines(a, 4, "dropped")...)
	wantRun(t, 0, w.aggArgs(), "VERIFIED:", "ok  chain-retired", "(dropped)")
	// the same event signed by a key the trust chain does not authorise
	w.event(1, "chain-retired", []signer{w.s2}, chainRetiredLines(a, 4, "dropped")...)
	wantRun(t, 1, w.aggArgs(), "FAILED at step chain-retired", "signed by no key that the trust chain authorises")
	// an event that names another head
	w.event(1, "chain-retired", []signer{w.s1}, chainRetiredLines(a, 3, "dropped")...)
	wantRun(t, 1, w.aggArgs(), "FAILED at step chain-retired", "height 3")
	// a malformed event ends the walk
	w.put("events/000001", []byte("garbage"))
	wantRun(t, 1, w.aggArgs(), "FAILED at step events")
}

func TestAggregateContinuityRefusals(t *testing.T) {
	// a chain that vanished without a retired flag
	w := newWorld(t)
	a, b := makeChain(0xa1, 5, nil), makeChain(0xb2, 3, nil)
	w.addChain(a, 5)
	w.addChain(b, 3)
	w.appendRound(10_000_000, member(a, 3, 0), member(b, 2, 0))
	note := filepath.Join(testdir.New(t), "held.note")
	copyFile(t, w.logDir("a/"+w.aggID+"/checkpoint"), note)
	w.appendRound(20_000_000, member(a, 5, 0))
	wantRun(t, 1, w.aggArgs("--previous", note), "FAILED at step continuity", "removed without record")

	// a chain whose height went back
	w = newWorld(t)
	w.addChain(a, 5)
	w.appendRound(10_000_000, member(a, 5, 0))
	copyFile(t, w.logDir("a/"+w.aggID+"/checkpoint"), note)
	w.appendRound(20_000_000, member(a, 3, 0))
	wantRun(t, 1, w.aggArgs("--previous", note), "FAILED at step continuity", "the chain went back")

	// a retired chain that is in the next round again
	w = newWorld(t)
	w.addChain(a, 5)
	w.appendRound(10_000_000, member(a, 5, aggregate.FlagRetired))
	copyFile(t, w.logDir("a/"+w.aggID+"/checkpoint"), note)
	w.appendRound(20_000_000, member(a, 5, 0))
	w.event(0, "key-added", []signer{w.s1}, keyAddedLines(w.s1)...)
	w.event(1, "chain-retired", []signer{w.s1}, chainRetiredLines(a, 4, "dropped")...)
	wantRun(t, 1, w.aggArgs("--previous", note), "FAILED at step continuity", "retired")

	// the leader's time went back
	w = newWorld(t)
	w.addChain(a, 5)
	w.appendRound(20_000_000, member(a, 3, 0))
	copyFile(t, w.logDir("a/"+w.aggID+"/checkpoint"), note)
	w.appendRound(10_000_000, member(a, 5, 0))
	wantRun(t, 1, w.aggArgs("--previous", note), "FAILED at step continuity", "times went back")

	// a rollback of the aggregate itself: the held checkpoint is larger than the served one
	w, _, _ = stdWorld(t)
	copyFile(t, w.logDir("a/"+w.aggID+"/checkpoint"), note)
	w.publishAggregate(1)
	wantRun(t, 1, w.aggArgs("--previous", note), "FAILED at step rollback")
}

func TestAggregateChainLogThatForksBetweenRounds(t *testing.T) {
	w := newWorld(t)
	a := makeChain(0xa1, 5, nil)
	w.addChain(a, 5)
	w.appendRound(10_000_000, member(a, 3, 0))
	note := filepath.Join(testdir.New(t), "held.note")
	copyFile(t, w.logDir("a/"+w.aggID+"/checkpoint"), note)
	// round 2 names a root for size 5 that is a consistent extension of nothing the held round named: another history of the first 3 blocks
	other := makeChain(0xa1, 5, map[int][]extraEntry{1: {{table: 9, op: 0x49, row: rowHashOf(1, 1)}}})
	w.chains[0] = other
	w.publishChain(other, 5)
	w.appendRound(20_000_000, member(other, 5, 0))
	wantRun(t, 1, w.aggArgs("--previous", note), "FAILED at step continuity", "is not an extension")
}

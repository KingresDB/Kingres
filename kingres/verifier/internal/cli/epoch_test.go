// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
)

// Step 21 of phase 7 (design 9.4): the verifier's findings of a closed epoch. The world below is what the publisher makes when
// an operator acknowledges that a chain was restored (step 20, task ledger.epoch.start), written here from the documents:
//
//	round 0  the chain a at epoch 1 with 5 blocks, and chain b                (the auditor holds this checkpoint)
//	round 1  a flagged diverged (0x04): the publisher stopped the log
//	event 0  epoch-closed for a epoch 1: final size 5, the root of the log, common height unknown, restore
//	round 2  a flagged retired and diverged (0x06), the one leaf of the closing
//	round 3  a is epoch 2 (7 blocks of another history), epoch 1 is left out
//
// The log of epoch 1 stays served, frozen at 5 blocks; the log of epoch 2 is a different log.

type epochWorld struct {
	*world
	a1, a2, b *chainFix
	held      string // the checkpoint of the aggregate log of round 0, a note the auditor holds
	last      int    // rounds in the aggregate log
}

type epochOpts struct {
	closingLeafLast bool // stop after round 2: the last leaf is the closing leaf
	finalSize       int  // what the event says (default 5)
	eventSigners    []signer
	noEvent         bool
	forkedHeld      bool // the round the auditor holds names a history that the closed log does not have
	heldBlocks      int  // blocks of a that the round the auditor holds names (default 5)
}

func epochLines(c *chainFix, size int, common, kind string) []string {
	root := refMTH(c.leaves[:size])
	return []string{"chain: " + c.idHex, fmt.Sprintf("epoch: %d", c.epoch), fmt.Sprintf("final-size: %d", size), "final-root: " + hex.EncodeToString(root[:]),
		"common-height: " + common, "kind: " + kind, "reason: the nightly backup was restored", "acknowledged-by: admin"}
}

func newEpochWorld(t *testing.T, o epochOpts) *epochWorld {
	t.Helper()
	w := newWorld(t)
	a1, b := makeChain(0xa1, 5, nil), makeChain(0xb2, 3, nil)
	a2 := makeChainTx(0xa1, 7, nil, func(i int) uint64 { return uint64(500 + i) })
	a2.epoch = 2
	ew := &epochWorld{world: w, a1: a1, a2: a2, b: b}
	w.addChain(a1, 5)
	w.addChain(b, 3)
	heldA := a1
	if o.forkedHeld {
		heldA = makeChainTx(0xa1, 5, nil, func(i int) uint64 { return uint64(900 + i) })
	}
	hb := o.heldBlocks
	if hb == 0 {
		hb = 5
	}
	w.appendRound(10_000_000, member(heldA, hb, 0), member(b, 3, 0))
	ew.held = filepath.Join(testdir.New(t), "held.note")
	copyFile(t, w.logDir("a/"+w.aggID+"/checkpoint"), ew.held)
	w.appendRound(20_000_000, member(a1, 5, 0x04), member(b, 3, 0))
	if !o.noEvent {
		fin := o.finalSize
		if fin == 0 {
			fin = 5
		}
		ss := o.eventSigners
		if ss == nil {
			ss = []signer{w.s1}
		}
		lines := epochLines(a1, 5, "unknown", "restore")
		if fin != 5 {
			lines = epochLines(a1, fin, "unknown", "restore")
		}
		w.event(0, "epoch-closed", ss, lines...)
	}
	w.appendRound(30_000_000, member(a1, 5, 0x06), member(b, 3, 0))
	ew.last = 3
	if !o.closingLeafLast {
		w.publishChain(a2, 7)
		w.appendRound(40_000_000, member(a2, 7, 0), member(b, 3, 0))
		ew.last = 4
	}
	return ew
}

// bundleJSON writes bundle.json for the world: both epochs of a, b and the aggregate, and the proofs given.
func (ew *epochWorld) bundleJSON(proofs ...map[string]string) {
	ew.t.Helper()
	var logs []map[string]any
	for _, c := range []*chainFix{ew.a1, ew.a2, ew.b} {
		rel := fmt.Sprintf("c/%s/e%d", c.idHex, c.epoch)
		if _, err := os.Stat(ew.logDir(rel)); err != nil {
			continue
		}
		logs = append(logs, map[string]any{"kind": "chain", "origin": ew.prefix + "/" + rel, "chain_id": c.idHex, "epoch": c.epoch, "checkpoint": rel + "/checkpoint", "size": countTiles(ew.logDir(rel))})
	}
	logs = append(logs, map[string]any{"kind": "aggregate", "origin": ew.prefix + "/a/" + ew.aggID, "checkpoint": "a/" + ew.aggID + "/checkpoint", "size": len(ew.leaves)})
	doc := map[string]any{"format": 1, "created_at": "2026-11-19T10:00:00Z", "cluster_id": "00ff", "origin_prefix": ew.prefix, "logs": logs, "anchors": []string{}, "proofs": append([]map[string]string{}, proofs...)}
	b, _ := json.Marshal(doc)
	ew.put("bundle.json", b)
}

func (ew *epochWorld) aggArgs(more ...string) []string {
	return ew.world.aggArgs(more...)
}

func (ew *epochWorld) bundleArgs(more ...string) []string {
	return ew.world.bundleArgs(more...)
}

// The last leaf is the closing leaf: the old member is flagged retired and diverged, and a verified event closed its epoch.
func TestEpochClosedMemberIsAFindingThatTheAuditorMustAccept(t *testing.T) {
	ew := newEpochWorld(t, epochOpts{closingLeafLast: true})
	out := wantRun(t, 1, ew.aggArgs(), "FAILED at step epoch-closed", "epoch 1 of chain "+ew.a1.idHex[:8], "closed by event 000000", "restore", "common height not stated", "acknowledged by admin",
		"the nightly backup was restored", "5 block(s)", "--accept-epoch-closed 0")
	if strings.Contains(out, "VERIFIED:") {
		t.Fatal(out)
	}
	out = wantRun(t, 0, ew.aggArgs("--accept-epoch-closed", "0"), "VERIFIED:", "finding epoch-closed", "ACCEPTED by --accept-epoch-closed 0", "this is a finding and not a pass")
	if strings.Contains(out, "INCOMPLETE") {
		t.Fatal(out)
	}
	// the JSON carries the finding as a step and in the info
	code, js, errs := run(t, ew.aggArgs("--accept-epoch-closed", "0", "--json")...)
	if code != 0 {
		t.Fatalf("%d %s%s", code, js, errs)
	}
	var doc struct {
		Steps []struct{ Name string }
		Info  map[string]any
	}
	if err := json.Unmarshal([]byte(js), &doc); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range doc.Steps {
		found = found || strings.HasPrefix(s.Name, "finding epoch-closed ")
	}
	if !found || doc.Info["epochs_closed_accepted"] == nil {
		t.Fatalf("the finding is not in the JSON: %s", js)
	}
}

// A flag that names no finding changes nothing, and says so: a typo must not pass for an acceptance.
func TestAcceptingTheWrongEventAcceptsNothing(t *testing.T) {
	ew := newEpochWorld(t, epochOpts{closingLeafLast: true})
	wantRun(t, 1, ew.aggArgs("--accept-epoch-closed", "7"), "FAILED at step epoch-closed", "accepted nothing", "event 000007")
	for _, bad := range []string{"x", "-1", "007", "", "99999999999"} {
		code, out, errs := run(t, ew.aggArgs("--accept-epoch-closed", bad)...)
		if code != 2 {
			t.Fatalf("%q: status %d\n%s%s", bad, code, out, errs)
		}
	}
}

// A member flagged diverged with no event is still a failure at step diverged (V8-04), and an event that no authorised key signed
// is refused and said so.
func TestEpochClosedNeedsAVerifiedEvent(t *testing.T) {
	ew := newEpochWorld(t, epochOpts{closingLeafLast: true, noEvent: true})
	wantRun(t, 1, ew.aggArgs("--accept-epoch-closed", "0"), "FAILED at step diverged")
	ew = newEpochWorld(t, epochOpts{closingLeafLast: true, eventSigners: []signer{newSigner(9)}})
	wantRun(t, 1, ew.aggArgs("--accept-epoch-closed", "0"), "FAILED at step diverged", "signed by no key that the trust chain authorises")
}

// The event and the aggregate must describe the same last state of the closed log, and the flag does not cover a disagreement.
func TestEpochClosedEventThatDisagreesWithTheLogIsAFailureWhateverIsAccepted(t *testing.T) {
	ew := newEpochWorld(t, epochOpts{closingLeafLast: true, finalSize: 4})
	wantRun(t, 1, ew.aggArgs("--accept-epoch-closed", "0"), "FAILED at step epoch-closed", "two different last states")
}

// The new epoch verifies on its own (design 9.4), and the output says that nothing was compared with the old one.
func TestTheNewEpochVerifiesOnItsOwnAndSaysWhatItDidNotCompare(t *testing.T) {
	ew := newEpochWorld(t, epochOpts{})
	out := wantRun(t, 0, ew.aggArgs(), "VERIFIED:", "round 3, 2 member(s)", "closed by event 000000", "it compared nothing with epoch 1", "--previous")
	if strings.Contains(out, "FAILED") {
		t.Fatal(out)
	}
}

// Comparing the new epoch with a round you hold from before fails and says why; the flag turns it into a finding.
func TestComparingTheNewEpochWithAnOldRoundFailsAndSaysWhy(t *testing.T) {
	ew := newEpochWorld(t, epochOpts{})
	wantRun(t, 1, ew.aggArgs("--previous", ew.held), "FAILED at step epoch-closed", "closed by event 000000", "restore", "are not in epoch 2", "is a prefix of that closed log", "--accept-epoch-closed 0")
	out := wantRun(t, 0, ew.aggArgs("--previous", ew.held, "--accept-epoch-closed", "0"), "VERIFIED:", "ok  continuity", "finding epoch-closed", "ACCEPTED", "the head you hold is a prefix of the frozen log")
	if !strings.Contains(out, "new chain "+ew.a2.idHex+":e2") {
		t.Fatalf("the new epoch is not named as the new chain:\n%s", out)
	}
}

// A round you hold that names a history the closed log does not have is a fork, and the flag does not cover it.
func TestAForkedHeldRoundIsNotAnEpochFinding(t *testing.T) {
	ew := newEpochWorld(t, epochOpts{forkedHeld: true})
	wantRun(t, 1, ew.aggArgs("--previous", ew.held, "--accept-epoch-closed", "0"), "FAILED at step continuity", "is not in the closed log")
}

// With an unverified event the old way stays: a chain that left the aggregate without a record fails at continuity.
func TestAChainThatLeftWithoutAVerifiedEventStillFailsAtContinuity(t *testing.T) {
	ew := newEpochWorld(t, epochOpts{noEvent: true})
	wantRun(t, 1, ew.aggArgs("--previous", ew.held, "--accept-epoch-closed", "0"), "FAILED at step continuity", "removed without record")
}

// consistency between a checkpoint of epoch 1 and the log of epoch 2: two logs, no extension; the verified event says why.
func TestConsistencyAcrossEpochsFailsWithTheEvent(t *testing.T) {
	ew := newEpochWorld(t, epochOpts{})
	old := ew.logDir("c/" + ew.a1.idHex + "/e1/checkpoint")
	args := []string{"consistency", "--trust", ew.trust, "--log", "chain:" + ew.a2.idHex + ":e2", "--source", ew.dir, "--old", old, "--now", nowArg19}
	wantRun(t, 1, args, "FAILED at step epoch-closed", "closed by event 000000", "different logs", "is not a prefix")
}

// Without a verified event that closed the epoch, a checkpoint of another epoch is not a checkpoint of this log: an input error, as it always was.
func TestConsistencyAcrossEpochsWithoutAnEventSaysSo(t *testing.T) {
	ew := newEpochWorld(t, epochOpts{noEvent: true})
	old := ew.logDir("c/" + ew.a1.idHex + "/e1/checkpoint")
	args := []string{"consistency", "--trust", ew.trust, "--log", "chain:" + ew.a2.idHex + ":e2", "--source", ew.dir, "--old", old, "--now", nowArg19}
	wantRun(t, 2, args, "has no signature of a key of the trust file that verifies")
}

// bundle verify of a database with a closed epoch.
func TestBundleVerifyOfAClosedEpoch(t *testing.T) {
	ew := newEpochWorld(t, epochOpts{})
	ew.bundleJSON()
	wantRun(t, 1, ew.bundleArgs(), "FAILED at step epoch-closed", "closed by event 000000", "--accept-epoch-closed 0")
	out := wantRun(t, 0, ew.bundleArgs("--accept-epoch-closed", "0"), "VERIFIED:", "finding epoch-closed", "ACCEPTED", "the frozen log of the epoch in the bundle has the size and root the event states", "3 chain log(s)")
	if !strings.Contains(out, "log chain:a1a1a1a1:e1") || !strings.Contains(out, "log chain:a1a1a1a1:e2") {
		t.Fatalf("the logs of both epochs are not verified:\n%s", out)
	}
}

func TestBundleVerifyChecksTheEventAgainstTheClosedLog(t *testing.T) {
	ew := newEpochWorld(t, epochOpts{finalSize: 4})
	ew.bundleJSON()
	wantRun(t, 1, ew.bundleArgs("--accept-epoch-closed", "0"), "FAILED at step", "the event and the log it closes disagree")
}

func TestBundleVerifyNeedsTheEventForALogOfALaterEpoch(t *testing.T) {
	ew := newEpochWorld(t, epochOpts{noEvent: true})
	ew.bundleJSON()
	wantRun(t, 1, ew.bundleArgs(), "FAILED at step epoch-closed", "was not closed by a verified event", "the event was withheld")
	ew = newEpochWorld(t, epochOpts{eventSigners: []signer{newSigner(9)}})
	ew.bundleJSON()
	wantRun(t, 1, ew.bundleArgs(), "FAILED at step epoch-closed", "signed by no key that the trust chain authorises")
}

// A proof of a block of the closed epoch and one of the new epoch, in one bundle: each is shown in the epoch that holds it.
func TestBundleProofsAreShownInTheEpochThatHoldsTheBlock(t *testing.T) {
	ew := newEpochWorld(t, epochOpts{})
	ew.put("proofs/new.json", ew.a2.proofJSON(6, nil, 0))
	ew.bundleJSON(map[string]string{"file": "proofs/new.json"})
	out := wantRun(t, 0, ew.bundleArgs("--accept-epoch-closed", "0"), "VERIFIED:", "proof 1 log-inclusion", "proof 1 aggregate-leg", "shown in epoch 2 of the chain, of 2 epoch(s)")
	_ = out
	// a block of the closed epoch: found in epoch 1 after epoch 2 did not hold it, and a finding of its own
	ew.put("proofs/old.json", ew.a1.proofJSON(3, nil, 0))
	ew.bundleJSON(map[string]string{"file": "proofs/old.json"})
	wantRun(t, 1, ew.bundleArgs(), "FAILED at step epoch-closed", "proof 1 shows its block (height 3) in epoch 1")
	wantRun(t, 0, ew.bundleArgs("--accept-epoch-closed", "0"), "VERIFIED:", "proof 1 log-inclusion", "shown in epoch 1 of the chain", "the aggregate leg was not made", "finding: proof 1 shows its block (height 3) in epoch 1")
	// a proof that no epoch holds fails as it does today
	ew.put("proofs/none.json", makeChainTx(0xa1, 7, nil, func(i int) uint64 { return uint64(777 + i) }).proofJSON(4, nil, 0))
	ew.bundleJSON(map[string]string{"file": "proofs/none.json"})
	wantRun(t, 1, ew.bundleArgs("--accept-epoch-closed", "0"), "FAILED at step log-inclusion")
}

// checkpoint of a log of a closed epoch verifies, and says that the epoch was closed; the log of the new epoch says nothing of the kind.
func TestCheckpointOfAClosedEpochSaysSo(t *testing.T) {
	ew := newEpochWorld(t, epochOpts{})
	base := []string{"checkpoint", "--trust", ew.trust, "--source", ew.dir, "--now", nowArg19, "--min-size", "1"}
	wantRun(t, 0, append([]string{"checkpoint", "--log", "chain:" + ew.a1.idHex + ":e1"}, base[1:]...), "VERIFIED:", "this log is of an epoch that a verified event closed", "closed by event 000000")
	out := wantRun(t, 0, append([]string{"checkpoint", "--log", "chain:" + ew.a2.idHex + ":e2"}, base[1:]...), "VERIFIED:")
	if strings.Contains(out, "closed by event") {
		t.Fatalf("the open epoch says it was closed:\n%s", out)
	}
}

// The held checkpoint must agree with the event: a held size above the final size the event states cannot be right.
func TestConsistencyAcrossEpochsChecksTheHeldCheckpointAgainstTheEvent(t *testing.T) {
	ew := newEpochWorld(t, epochOpts{finalSize: 4})
	old := ew.logDir("c/" + ew.a1.idHex + "/e1/checkpoint")
	args := []string{"consistency", "--trust", ew.trust, "--log", "chain:" + ew.a2.idHex + ":e2", "--source", ew.dir, "--old", old, "--now", nowArg19}
	wantRun(t, 1, args, "FAILED at step epoch-closed", "larger than the final size 4")
}

// A held head that is shorter than the closed log is proved a prefix of it with a consistency proof from the tiles of the frozen log.
func TestAShorterHeldHeadIsProvedAPrefixOfTheFrozenLog(t *testing.T) {
	ew := newEpochWorld(t, epochOpts{heldBlocks: 3})
	wantRun(t, 1, ew.aggArgs("--previous", ew.held), "FAILED at step epoch-closed", "The head you hold (3 block(s)) is a prefix of that closed log")
	wantRun(t, 0, ew.aggArgs("--previous", ew.held, "--accept-epoch-closed", "0"), "VERIFIED:", "finding epoch-closed")
	// the same shorter head, of a history the closed log does not have
	ew = newEpochWorld(t, epochOpts{heldBlocks: 3, forkedHeld: true})
	wantRun(t, 1, ew.aggArgs("--previous", ew.held, "--accept-epoch-closed", "0"), "FAILED at step continuity", "is not an extension of the log at size 3")
}

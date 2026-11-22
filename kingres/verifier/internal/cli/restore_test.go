// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package cli

import (
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/events"
)

// P7R-09: the verifier reads the event restore-acknowledged (design 9.4), reports it in the commands that read the events, and names it in the
// failure of a comparison of the aggregate log with a checkpoint that you hold. The event is what the publisher writes after an operator
// accepted a restore of the system database (control plane, ledgerround/restore.go).

const sha1hex = "1111111111111111111111111111111111111111111111111111111111111111"
const sha2hex = "2222222222222222222222222222222222222222222222222222222222222222"

func restoreLines(gen, pubEvents, pubLogs, dbEvents, dbLogs int) []string {
	return []string{"generation: " + itoa(gen), "published-events: " + itoa(pubEvents), "published-logs: " + itoa(pubLogs), "published-state-sha256: " + sha1hex,
		"database-events: " + itoa(dbEvents), "database-logs: " + itoa(dbLogs), "database-state-sha256: " + sha2hex, "reason: the nightly backup was restored", "acknowledged-by: operator, file accept-restore"}
}

func TestACommandThatReadsTheEventsReportsARestoreAcknowledgedEvent(t *testing.T) {
	w, _, _ := stdWorld(t)
	w.event(0, "restore-acknowledged", []signer{w.s1}, restoreLines(1, 9, 2, 4, 1)...)
	out := wantRun(t, 0, w.aggArgs(), "VERIFIED:", "finding: the publication holds a verified event 000000 (restore-acknowledged, generation 1, acknowledged by operator, file accept-restore",
		"9 event(s) and 2 log(s) had been published, the restored database held 4 event(s) and 1 log(s)", "was cut there")
	if strings.Contains(out, "not interpreted") {
		t.Fatal(out)
	}
	// the single log commands say it for the aggregate log only (the chain logs are not cut by a restore)
	base := []string{"checkpoint", "--trust", w.trust, "--source", w.dir, "--now", "2026-11-19T12:00:00Z", "--min-size", "1"}
	wantRun(t, 0, append([]string{"checkpoint", "--log", "aggregate:" + w.aggID}, base[1:]...), "VERIFIED:", "restore-acknowledged")
	out = wantRun(t, 0, append([]string{"checkpoint", "--log", "chain:" + stdChainID(w) + ":e1"}, base[1:]...), "VERIFIED:")
	if strings.Contains(out, "restore-acknowledged") {
		t.Fatalf("a chain log is not cut by a restore:\n%s", out)
	}
	// bundle verify
	w.bundleJSON()
	wantRun(t, 0, w.bundleArgs(), "VERIFIED:", "restore-acknowledged, generation 1")
	// without the event nothing is said
	w2, _, _ := stdWorld(t)
	out = wantRun(t, 0, w2.aggArgs(), "VERIFIED:")
	if strings.Contains(out, "restore-acknowledged") {
		t.Fatalf("a note without an event:\n%s", out)
	}
}

// An event that no authorised key signed is not the publisher's statement: it is listed as skipped and explains nothing.
func TestAnUnauthorisedRestoreAcknowledgedEventExplainsNothing(t *testing.T) {
	w, _, _ := stdWorld(t)
	w.event(0, "restore-acknowledged", []signer{newSigner(9)}, restoreLines(1, 9, 2, 4, 1)...)
	out := wantRun(t, 0, w.aggArgs(), "VERIFIED:")
	if strings.Contains(out, "finding: the publication holds a verified") {
		t.Fatalf("an unauthorised event was reported as verified:\n%s", out)
	}
}

// A signed event that is not the strict format is not read, and says so; it does not end the run (P7F-06). The run goes on and reads the events
// after it, and nothing of the malformed one is used.
func TestAMalformedRestoreAcknowledgedEventIsNotReadAndTheRunGoesOn(t *testing.T) {
	for name, lines := range map[string][]string{
		"generation 0":    append([]string{"generation: 0"}, restoreLines(1, 9, 2, 4, 1)[1:]...),
		"a missing line":  restoreLines(1, 9, 2, 4, 1)[:8],
		"an unknown line": append(restoreLines(1, 9, 2, 4, 1), "extra: x"),
		"a short digest":  append(restoreLines(1, 9, 2, 4, 1)[:6], "database-state-sha256: 22", "reason: r", "acknowledged-by: a"),
		"a leading zero":  append([]string{"generation: 01"}, restoreLines(1, 9, 2, 4, 1)[1:]...),
	} {
		w, _, _ := stdWorld(t)
		w.event(0, "restore-acknowledged", []signer{w.s1}, lines...)
		// a well formed one after it is still read
		w.event(1, "restore-acknowledged", []signer{w.s1}, restoreLines(2, 9, 2, 4, 1)...)
		out := wantRun(t, 0, w.aggArgs(), "VERIFIED:", "finding: event 000000 is signed by an authorised key and has the type restore-acknowledged, but it is not in the form the publisher writes",
			"was not read", "finding: the publication holds a verified event 000001 (restore-acknowledged, generation 2")
		if strings.Contains(out, "verified event 000000") {
			t.Errorf("%s: the malformed event was read:\n%s", name, out)
		}
	}
	// the same generation twice: the first is read, the second is not
	w, _, _ := stdWorld(t)
	w.event(0, "restore-acknowledged", []signer{w.s1}, restoreLines(1, 9, 2, 4, 1)...)
	w.event(1, "restore-acknowledged", []signer{w.s1}, restoreLines(1, 9, 2, 4, 1)...)
	out := wantRun(t, 0, w.aggArgs(), "VERIFIED:", "verified event 000000 (restore-acknowledged, generation 1", "finding: event 000001 is signed by an authorised key", "acknowledged twice")
	if strings.Contains(out, "verified event 000001") {
		t.Errorf("the second event of one generation was read:\n%s", out)
	}
	// the commands that do not report restores are not touched by it: a key follow reads the events and goes on
	wf, _, _ := stdWorld(t)
	wf.event(0, "restore-acknowledged", []signer{wf.s1}, append(restoreLines(1, 9, 2, 4, 1), "extra: x")...)
	out = wantRun(t, 0, wf.followArgs(newOut(t)), "VERIFIED:")
	if strings.Contains(out, "restore-acknowledged") {
		t.Errorf("a key follow reports a restore event:\n%s", out)
	}
}

// A malformed restore event next to a fork: the failure says that an event was not read and cannot say whether it explains the fork.
func TestAForkNextToAnUnreadRestoreEventSaysSo(t *testing.T) {
	w, _, _ := stdWorld(t)
	held := t.TempDir() + "/held.note"
	copyFile(t, w.logDir("a/"+w.aggID+"/checkpoint"), held)
	w.publishAggregate(1)
	w.event(0, "restore-acknowledged", []signer{w.s1}, append(restoreLines(1, 9, 2, 4, 1), "extra: x")...)
	out := wantRun(t, 1, w.aggArgs("--previous", held), "FAILED at step rollback", "Also: event 000000 is typed restore-acknowledged and is not in the form the publisher writes", "was not read", "cannot say whether it explains the failure")
	if strings.Contains(out, "this explains the failure") {
		t.Errorf("an unread event is said to explain the failure:\n%s", out)
	}
}

// The failure of a comparison with a held checkpoint of the aggregate log names the event.
func TestAForkOfTheAggregateAgainstAHeldCheckpointNamesTheRestoreEvent(t *testing.T) {
	w, _, _ := stdWorld(t)
	held := t.TempDir() + "/held.note"
	copyFile(t, w.logDir("a/"+w.aggID+"/checkpoint"), held) // size 2
	w.publishAggregate(1)                                   // the log went back to size 1
	// without the event: a rollback, and no word about a restore
	out := wantRun(t, 1, w.aggArgs("--previous", held), "FAILED at step rollback")
	if strings.Contains(out, "restore-acknowledged") {
		t.Fatalf("a restore is named without an event:\n%s", out)
	}
	w.event(0, "restore-acknowledged", []signer{w.s1}, restoreLines(1, 9, 2, 4, 1)...)
	wantRun(t, 1, w.aggArgs("--previous", held), "FAILED at step rollback", "restore-acknowledged, generation 1", "this explains the failure", "does not show that the cut was honest")
	// the consistency command
	args := []string{"consistency", "--trust", w.trust, "--log", "aggregate:" + w.aggID, "--source", w.dir, "--old", held, "--now", "2026-11-19T12:00:00Z"}
	wantRun(t, 1, args, "FAILED at step sizes", "restore-acknowledged, generation 1", "this explains the failure")
}

func stdChainID(w *world) string { return w.chains[0].idHex }

// monitor reports the event as a note, and names it when the state it keeps for the aggregate log meets a fork.
func TestMonitorReportsARestoreAcknowledgedEventAndNamesItInAFork(t *testing.T) {
	w, _, st := monitorWorld(t)
	w.event(0, "restore-acknowledged", []signer{w.s1}, restoreLines(1, 9, 2, 4, 1)...)
	srv := serve(t, w)
	wantRun(t, 0, w.monitorArgs(srv, st, 1), "restore-acknowledged, generation 1")
	// the aggregate log goes back after the monitor saw it at size 1: a rollback, with the event next to it
	w.appendRound(20_000_000, member(w.chains[0], 5, 0))
	wantRun(t, 0, w.monitorArgs(srv, st, 1))
	w.publishAggregate(1)
	wantRun(t, 1, w.monitorArgs(srv, st, 1), "FAILED at step rollback", "restore-acknowledged, generation 1")
}

// A monitor that watches chain logs only still reports the event (the aggregate log is not among its logs, so no log command reports it).
func TestMonitorOfChainLogsOnlyReportsARestoreAcknowledgedEvent(t *testing.T) {
	w, _, st := monitorWorld(t)
	w.event(0, "restore-acknowledged", []signer{w.s1}, restoreLines(1, 9, 2, 4, 1)...)
	srv := serve(t, w)
	args := []string{"monitor", "--trust", w.trust, "--source", srv.URL, "--state", st, "--polls", "1", "--interval", "1ms", "--now", "2026-11-19T12:00:00Z",
		"--log", "chain:" + w.chains[0].idHex + ":e1", "--init"}
	wantRun(t, 0, args, "finding: the publication holds a verified event 000000 (restore-acknowledged, generation 1")
}

// P7F-03 (mutant P1): a held checkpoint of the same size as the served one with another root is a fork; with a restore event the failure names it,
// without one it does not.
func TestAForkOfTheSameSizeAgainstAHeldCheckpointNamesTheRestoreEvent(t *testing.T) {
	w, _, _ := stdWorld(t)
	held := t.TempDir() + "/held.note"
	copyFile(t, w.logDir("a/"+w.aggID+"/checkpoint"), held) // size 2
	w.leaves[1].GeneratedAtUS++                             // another second leaf: the same size, another root
	w.aggEnc[1] = w.leaves[1].Encode()
	w.publishAggregate(2)
	out := wantRun(t, 1, w.aggArgs("--previous", held), "FAILED at step fork", "two histories")
	if strings.Contains(out, "restore-acknowledged") {
		t.Fatalf("a restore is named without an event:\n%s", out)
	}
	w.event(0, "restore-acknowledged", []signer{w.s1}, restoreLines(1, 9, 2, 4, 1)...)
	wantRun(t, 1, w.aggArgs("--previous", held), "FAILED at step fork", "two histories", "restore-acknowledged, generation 1", "this explains the failure")
}

// P7F-03 (mutant P5): only a fork is what the event explains, a damaged tile is not.
func TestOnlyAForkIsExplainedByARestoreEventAndADamagedTileIsNot(t *testing.T) {
	pw := &pubInputs{walked: true, walk: &events.Walk{Restores: []events.RestoreAcknowledged{{Seq: 0, Generation: 1, AcknowledgedBy: "operator", Reason: "r"}}}}
	if got := pw.forkRemark("fork"); !strings.Contains(got, "restore-acknowledged, generation 1") {
		t.Errorf("a fork is not explained by the event: %q", got)
	}
	for _, step := range []string{"tiles", "tile", "sizes", ""} {
		if got := pw.forkRemark(step); got != "" {
			t.Errorf("a failure at step %q is explained by the event: %q", step, got)
		}
	}
	if got := (&pubInputs{walked: true, walk: &events.Walk{}}).forkRemark("fork"); got != "" {
		t.Errorf("a fork is explained without an event: %q", got)
	}
}

// P7G-01: a record that was lost and made again from the database reaches a generation number that an earlier restore had. The publisher announces
// the second cut under another subject, and the verifier reads both events: the same generation with another replaced record is a second cut, the
// same generation with the same replaced record is a repetition and is not read.
func TestTwoCutsOfOneGenerationWithOtherReplacedRecordsAreBothRead(t *testing.T) {
	second := restoreLines(1, 11, 3, 6, 2)
	second[3] = "published-state-sha256: " + sha2hex // another replaced record
	w, _, _ := stdWorld(t)
	w.event(0, "restore-acknowledged", []signer{w.s1}, restoreLines(1, 9, 2, 4, 1)...)
	w.event(1, "restore-acknowledged", []signer{w.s1}, second...)
	out := wantRun(t, 0, w.aggArgs(), "VERIFIED:", "verified event 000000 (restore-acknowledged, generation 1", "verified event 000001 (restore-acknowledged, generation 1")
	if strings.Contains(out, "not in the form the publisher writes") || strings.Contains(out, "acknowledged twice") {
		t.Fatalf("a second cut of one generation was refused:\n%s", out)
	}
	// a true repetition (the same generation and the same replaced record) is not read
	w2, _, _ := stdWorld(t)
	w2.event(0, "restore-acknowledged", []signer{w2.s1}, restoreLines(1, 9, 2, 4, 1)...)
	w2.event(1, "restore-acknowledged", []signer{w2.s1}, restoreLines(1, 11, 3, 6, 2)...)
	out = wantRun(t, 0, w2.aggArgs(), "VERIFIED:", "verified event 000000", "finding: event 000001 is signed by an authorised key", "acknowledged twice")
	if strings.Contains(out, "verified event 000001") {
		t.Fatalf("a repetition was read:\n%s", out)
	}
}

// The generation and the replaced record are both part of what makes an event a repetition: a later generation that replaced the same record
// is a new event.
func TestALaterGenerationWithTheSameReplacedRecordIsNotARepetition(t *testing.T) {
	w, _, _ := stdWorld(t)
	w.event(0, "restore-acknowledged", []signer{w.s1}, restoreLines(1, 9, 2, 4, 1)...)
	w.event(1, "restore-acknowledged", []signer{w.s1}, restoreLines(2, 9, 2, 4, 1)...)
	out := wantRun(t, 0, w.aggArgs(), "VERIFIED:", "verified event 000000 (restore-acknowledged, generation 1", "verified event 000001 (restore-acknowledged, generation 2")
	if strings.Contains(out, "acknowledged twice") {
		t.Fatalf("%s", out)
	}
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package cli

import (
	"strings"
	"testing"
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

// A signed event that is not the format ends the run, as every known event type does.
func TestAMalformedRestoreAcknowledgedEventIsRefused(t *testing.T) {
	for name, lines := range map[string][]string{
		"generation 0":    append([]string{"generation: 0"}, restoreLines(1, 9, 2, 4, 1)[1:]...),
		"a missing line":  restoreLines(1, 9, 2, 4, 1)[:8],
		"an unknown line": append(restoreLines(1, 9, 2, 4, 1), "extra: x"),
		"a short digest":  append(restoreLines(1, 9, 2, 4, 1)[:6], "database-state-sha256: 22", "reason: r", "acknowledged-by: a"),
		"a leading zero":  append([]string{"generation: 01"}, restoreLines(1, 9, 2, 4, 1)[1:]...),
	} {
		w, _, _ := stdWorld(t)
		w.event(0, "restore-acknowledged", []signer{w.s1}, lines...)
		code, out, errs := run(t, w.aggArgs()...)
		if code == 0 || strings.Contains(out, "VERIFIED:") {
			t.Errorf("%s: accepted (%d)\n%s%s", name, code, out, errs)
		}
	}
	// the same generation twice
	w, _, _ := stdWorld(t)
	w.event(0, "restore-acknowledged", []signer{w.s1}, restoreLines(1, 9, 2, 4, 1)...)
	w.event(1, "restore-acknowledged", []signer{w.s1}, restoreLines(1, 9, 2, 4, 1)...)
	if code, out, _ := run(t, w.aggArgs()...); code == 0 {
		t.Errorf("the same generation twice was accepted:\n%s", out)
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

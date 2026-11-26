// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
)

// L18-01: a state that the auditor named like a temporary file of the program is
// refused with status 2 before anything is read or removed. The reviewer's probe:
// the thief state of size 700 under such a name and the honest log served at size
// 300 (a rollback) was VERIFIED, because the cleanup of leftovers removed the held
// state before it was read.
func TestStateNamedLikeATemporaryFileIsRefusedAndNeverRead(t *testing.T) {
	th := newThief(t)
	dir := testdir.New(t)
	for _, name := range []string{".kingres-state-0123456789ab-7.tmp", ".kingres-evidence-0123456789ab-7.tmp"} {
		st := filepath.Join(dir, name)
		if err := os.WriteFile(st, []byte(th.stateText), 0o600); err != nil {
			t.Fatal(err)
		}
		th.serve(300)
		for _, extra := range [][]string{nil, {"--json"}} {
			code, out, errs := run(t, append(stateArgs(th.trustWeak, th.root, st), extra...)...)
			if code != 2 || strings.HasPrefix(out, "VERIFIED") {
				t.Fatalf("%s %v: status %d\n%s%s", name, extra, code, out, errs)
			}
			if !strings.Contains(out+errs, "temporary files") || !strings.Contains(out+errs, "another name") {
				t.Errorf("%s: the message does not say why and what to do:\n%s%s", name, out, errs)
			}
		}
		if fileText(t, st) != th.stateText {
			t.Fatalf("%s: the state was changed or removed", name)
		}
		if names := evidenceNames(dir); len(names) != 0 {
			t.Fatalf("evidence written for a refused state: %v", names)
		}
	}
}

// L18-04: the part of the handle tests that walks the weak path, the evidence merge
// and the removal of a leftover, so that a descriptor or handle left open there is
// found where the handle tests count them. th is a thief scenario made once; every
// call uses a fresh directory with a copy of the thief's state.
func walkWeakEvidenceRuns(t *testing.T, th *thief) {
	t.Helper()
	dir := testdir.New(t)
	st := filepath.Join(dir, "state")
	if err := os.WriteFile(st, []byte(th.stateText), 0o600); err != nil {
		t.Fatal(err)
	}
	want := func(step string, code int, args ...string) {
		t.Helper()
		if got, out, errs := run(t, append(stateArgs(th.trustWeak, th.root, st), args...)...); got != code {
			t.Fatalf("%s: status %d, want %d\n%s%s", step, got, code, out, errs)
		}
	}
	// a weak conflict that is new, and the same text served again
	th.serve(300)
	want("a new weak rollback", 1)
	roll := filepath.Join(dir, evidenceNames(dir)[0])
	was := fileText(t, roll)
	want("the same text again", 1)
	if fileText(t, roll) != was {
		t.Fatal("the same text again rewrote the evidence")
	}
	// the extension of an existing evidence file with a new line
	publishNote(t, th.root, th.honest, 300, signedBy(300, th.honest, []signer{th.b, th.a}))
	want("the same text with one more line", 1)
	if fileText(t, roll) == was {
		t.Fatal("the evidence file was not extended: the walk does not reach the merge")
	}
	// a file planted at the evidence name of the forks
	th.serve(900)
	want("a new weak fork", 1)
	var fork string
	for _, n := range evidenceNames(dir) {
		if strings.Contains(n, "conflict-weak-fork-") {
			fork = filepath.Join(dir, n)
		}
	}
	if fork == "" {
		t.Fatal("no file for the fork")
	}
	if err := os.WriteFile(fork, []byte("planted"), 0o600); err != nil {
		t.Fatal(err)
	}
	want("a planted file at the name", 1)
	// a run that removes a leftover of a crash
	left := filepath.Join(dir, ".kingres-state-0123456789ab-5.tmp")
	if err := os.WriteFile(left, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	want("a run with a leftover", 1)
	if _, err := os.Lstat(left); err == nil {
		t.Fatal("the leftover was not removed: the walk does not reach the removal")
	}
}

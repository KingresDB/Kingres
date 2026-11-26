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

// L14-03. The lines of one text only accumulate: a line that is stored (it was
// verified when it was stored and is bound to the text) is kept as it is when the
// file is rewritten, also when its key has left the trust file, and only the new
// lines are verified. A line of a key the trust file no longer holds cannot be
// verified now and makes nothing usable by itself.

// reversedLines rewrites a note with its signature lines in the opposite order of
// the canonical one.
func reversedLines(t *testing.T, note string) string {
	t.Helper()
	text, lines := splitNote(note)
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	return joinNote(text, lines)
}

func TestEvidenceKeepsTheLineOfAKeyThatLeftTheTrustFile(t *testing.T) {
	k0, k1, k2, k3 := newSigner(10), newSigner(11), newSigner(12), newSigner(13)
	all := leaves(700)
	root := testdir.New(t)
	full := trustOf(t, keySpec{"k0", k0, ""}, keySpec{"k1", k1, ""}, keySpec{"k2", k2, ""}, keySpec{"k3", k3, ""})
	state := filepath.Join(testdir.New(t), "state")
	publishNote(t, root, all, 700, signedBy(700, all, []signer{k0}))
	if code, out, errs := run(t, stateArgs(full, root, state)...); code != 0 {
		t.Fatalf("seed: %d\n%s%s", code, out, errs)
	}
	// a rollback served with the lines of k1 and k2: the evidence holds both
	publishNote(t, root, all, 300, signedBy(300, all, []signer{k1, k2}))
	if code, out, _ := run(t, stateArgs(full, root, state)...); code != 1 {
		t.Fatalf("first conflict: %d\n%s", code, out)
	}
	names := evidenceNames(filepath.Dir(state))
	if len(names) != 1 {
		t.Fatalf("evidence files %v", names)
	}
	ev := filepath.Join(filepath.Dir(state), names[0])
	if n := len(sigLines(fileText(t, ev))); n != 2 {
		t.Fatalf("the evidence holds %d lines, want 2", n)
	}
	// k1 leaves the trust file; the same rollback is served with the lines of k2 and k3
	without := trustOf(t, keySpec{"k0", k0, ""}, keySpec{"k2", k2, ""}, keySpec{"k3", k3, ""})
	publishNote(t, root, all, 300, signedBy(300, all, []signer{k2, k3}))
	code, out, _ := run(t, stateArgs(without, root, state)...)
	if code != 1 {
		t.Fatalf("second conflict: %d\n%s", code, out)
	}
	got := fileText(t, ev)
	_, k1Line := splitNote(signedBy(300, all, []signer{k1}))
	if !strings.Contains(got, strings.TrimSuffix(k1Line[0], "\n")) {
		t.Errorf("the line of the key that left the trust file was dropped from the evidence:\n%s", got)
	}
	if n := len(sigLines(got)); n != 3 {
		t.Errorf("the evidence holds %d lines, want 3 (k1 kept, k2, k3)", n)
	}
	for _, want := range []string{"the file now holds 3 signature line(s)", "2 verify under a key of the trust file", "1 are of keys the trust file does not hold", "not verified now", "this run added 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("the output does not say %q:\n%s", want, out)
		}
	}
	if len(evidenceNames(filepath.Dir(state))) != 1 {
		t.Errorf("another evidence file was made: %v", evidenceNames(filepath.Dir(state)))
	}
}

func TestStateKeepsTheLineOfAKeyThatLeftTheTrustFile(t *testing.T) {
	k0, k1, k2 := newSigner(10), newSigner(11), newSigner(12)
	all := leaves(700)
	root := testdir.New(t)
	full := trustOf(t, keySpec{"k0", k0, ""}, keySpec{"k1", k1, ""}, keySpec{"k2", k2, ""})
	state := filepath.Join(testdir.New(t), "state")
	publishNote(t, root, all, 300, signedBy(300, all, []signer{k0, k1}))
	if code, out, errs := run(t, stateArgs(full, root, state)...); code != 0 {
		t.Fatalf("seed: %d\n%s%s", code, out, errs)
	}
	// k1 leaves the file; the same checkpoint is served with the line of k2
	without := trustOf(t, keySpec{"k0", k0, ""}, keySpec{"k2", k2, ""})
	publishNote(t, root, all, 300, signedBy(300, all, []signer{k0, k2}))
	code, out, errs := run(t, stateArgs(without, root, state)...)
	if code != 0 {
		t.Fatalf("%d\n%s%s", code, out, errs)
	}
	_, k1Line := splitNote(signedBy(300, all, []signer{k1}))
	held := storedNote(t, state)
	if !strings.Contains(held, strings.TrimSuffix(k1Line[0], "\n")) || len(sigLines(held)) != 3 {
		t.Errorf("the state lost the line of the key that left the file:\n%s", held)
	}
	for _, want := range []string{"The state now holds 3 line(s)", "2 verify under a key of the trust file", "1 are of keys the trust file does not hold", "make nothing usable"} {
		if !strings.Contains(out, want) {
			t.Errorf("the output does not say %q:\n%s", want, out)
		}
	}
	// the state is read as before, and the extension still works
	publishNote(t, root, all, 700, signedBy(700, all, []signer{k0}))
	if code, out, errs := run(t, stateArgs(without, root, state)...); code != 0 || stateSize(t, state) != 700 {
		t.Fatalf("the extension: %d\n%s%s", code, out, errs)
	}
}

// What a stored line of a key that left the file means: nothing. A state whose only
// remaining lines are of such keys is not usable, as before, and the refusal names
// the key ids and says that removing a key did it (L14-08).
func TestLineOfAKeyThatLeftTheFileMakesNothingUsable(t *testing.T) {
	k0, k1, k2 := newSigner(10), newSigner(11), newSigner(12)
	all := leaves(700)
	root := testdir.New(t)
	full := trustOf(t, keySpec{"k0", k0, ""}, keySpec{"k1", k1, ""}, keySpec{"k2", k2, ""})
	state := filepath.Join(testdir.New(t), "state")
	publishNote(t, root, all, 300, signedBy(300, all, []signer{k0, k1}))
	if code, out, errs := run(t, stateArgs(full, root, state)...); code != 0 {
		t.Fatalf("seed: %d\n%s%s", code, out, errs)
	}
	kept := fileText(t, state)
	onlyK2 := trustOf(t, keySpec{"k2", k2, ""})
	publishNote(t, root, all, 700, signedBy(700, all, []signer{k2}))
	code, _, errs := run(t, stateArgs(onlyK2, root, state)...)
	if code != 2 || fileText(t, state) != kept {
		t.Fatalf("a state whose keys left the file: %d\n%s", code, errs)
	}
	for _, want := range []string{"the key id(s)", "the trust file holds none of those keys", "marked revoked in the file and never deleted", "--min-size"} {
		if !strings.Contains(errs, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, errs)
		}
	}
	// consistency --old STATE says it too
	code, _, errs = run(t, "consistency", "--trust", onlyK2, "--log", logSpecArg(), "--source", root, "--old", state)
	if code != 2 || !strings.Contains(errs, "never deleted") {
		t.Errorf("consistency: %d\n%s", code, errs)
	}
}

// L14-06 (M06b): an evidence file that has nothing to add is not rewritten: a file
// whose lines are in another order than the canonical one stays as it is.
func TestEvidenceIsNotRewrittenWhenNothingIsAdded(t *testing.T) {
	k0, k1, k2 := newSigner(10), newSigner(11), newSigner(12)
	all := leaves(700)
	root := testdir.New(t)
	tr := trustOf(t, keySpec{"k0", k0, ""}, keySpec{"k1", k1, ""}, keySpec{"k2", k2, ""})
	state := filepath.Join(testdir.New(t), "state")
	publishNote(t, root, all, 700, signedBy(700, all, []signer{k0}))
	if code, out, errs := run(t, stateArgs(tr, root, state)...); code != 0 {
		t.Fatalf("seed: %d\n%s%s", code, out, errs)
	}
	publishNote(t, root, all, 300, signedBy(300, all, []signer{k1, k2}))
	if code, out, _ := run(t, stateArgs(tr, root, state)...); code != 1 {
		t.Fatalf("first conflict: %d\n%s", code, out)
	}
	ev := filepath.Join(filepath.Dir(state), evidenceNames(filepath.Dir(state))[0])
	mine := reversedLines(t, fileText(t, ev))
	if mine == fileText(t, ev) {
		t.Fatal("the two orders are the same: the test cannot see a rewrite")
	}
	if err := os.WriteFile(ev, []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, served := range [][]signer{{k1, k2}, {k1}, {k2}} {
		publishNote(t, root, all, 300, signedBy(300, all, served))
		code, out, _ := run(t, stateArgs(tr, root, state)...)
		if code != 1 || !strings.Contains(out, "was already saved to") || strings.Contains(out, "the file now holds") {
			t.Fatalf("serving %d line(s): %d\n%s", len(served), code, out)
		}
		if fileText(t, ev) != mine {
			t.Fatalf("serving %d line(s) rewrote an evidence file that had nothing to add", len(served))
		}
	}
}

// L14-06 (M29): a served note of the same size and root with another text (an
// extension line) is not the same checkpoint: no merge, the state is left as it is.
func TestSameSizeAndRootWithAnotherTextMergesNothing(t *testing.T) {
	k0, k1 := newSigner(10), newSigner(11)
	all := leaves(300)
	root := testdir.New(t)
	tr := trustOf(t, keySpec{"k0", k0, ""}, keySpec{"k1", k1, ""})
	state := filepath.Join(testdir.New(t), "state")
	publishNote(t, root, all, 300, signedBy(300, all, []signer{k0}))
	if code, out, errs := run(t, stateArgs(tr, root, state)...); code != 0 {
		t.Fatalf("seed: %d\n%s%s", code, out, errs)
	}
	kept := fileText(t, state)
	text, _ := splitNote(signedBy(300, all, nil))
	text += "an extension line\n"
	publishNote(t, root, all, 300, joinNote(text, []string{sigLine(logOrigin(), text, k1)}))
	code, out, errs := run(t, stateArgs(tr, root, state)...)
	if code != 0 || fileText(t, state) != kept || strings.Contains(out, "signatures_added") || strings.Contains(out, "the state keeps the union") {
		t.Fatalf("another text of the same size and root: %d\n%s%s", code, out, errs)
	}
}

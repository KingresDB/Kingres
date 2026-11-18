// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/merkle"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
)

// These tests came from a mutation run of cli_state.go: what a run of
// checkpoint --state says it did (steps, notes, the lines of "not proved", the
// members of the JSON), for every way the state can go, and the messages that
// tell an auditor why a note they hold cannot serve.

func stepsOf(o map[string]any) map[string]string {
	out := map[string]string{}
	steps, _ := o["steps"].([]any)
	for _, s := range steps {
		m := s.(map[string]any)
		out[m["name"].(string)] = fmt.Sprint(m["detail"])
	}
	return out
}

func listOf(o map[string]any, key string) string {
	l, _ := o[key].([]any)
	var parts []string
	for _, x := range l {
		parts = append(parts, fmt.Sprint(x))
	}
	return strings.Join(parts, "\n")
}

// emptyTreeNote is a note of size 0, whose root is the hash of the empty string.
func emptyTreeNote(s signer) string {
	root := merkle.EmptyRoot()
	text := fmt.Sprintf("%s\n0\n%s\n", logOrigin(), base64.StdEncoding.EncodeToString(root[:]))
	return text + "\n" + sigLine(logOrigin(), text, s)
}

func TestStateRunsSayWhatTheyDid(t *testing.T) {
	k := newSigner(1)
	all := leaves(700)
	tr := trustOf(t, keySpec{"k", k, ""})
	root := testdir.New(t)
	state := filepath.Join(testdir.New(t), "state")

	// first use
	publishNote(t, root, all, 300, signedBy(300, all, []signer{k}))
	code, o := jsonOf(t, stateArgs(tr, root, state)...)
	st := stateMember(t, o)
	if code != 0 || st["action"] != "created" || st["extension_proved"] != false || st["old_size"] != nil || st["new_size"] != float64(300) {
		t.Fatalf("first use: %d %v", code, st)
	}
	if !strings.Contains(listOf(o, "notes"), "the state file did not exist and was created") ||
		!strings.Contains(listOf(o, "not_proved"), "that this checkpoint is not already a rollback") ||
		!strings.Contains(listOf(o, "not_proved"), "that the state file is the one you left") ||
		!strings.Contains(listOf(o, "notes"), "the state file was updated to size 300") ||
		!strings.Contains(stepsOf(o)["state"], "no earlier checkpoint was recorded") {
		t.Errorf("first use does not say what it did:\n%v", o)
	}

	// the same checkpoint again
	code, o = jsonOf(t, stateArgs(tr, root, state)...)
	st = stateMember(t, o)
	if code != 0 || st["action"] != "unchanged" || st["extension_proved"] != true || st["old_size"] != float64(300) {
		t.Fatalf("same: %d %v", code, st)
	}
	if strings.Contains(listOf(o, "notes"), "was updated") || !strings.Contains(stepsOf(o)["state"], "same size and root") ||
		!strings.Contains(listOf(o, "not_proved"), "that the state file is the one you left") {
		t.Errorf("same does not say what it did:\n%v", o)
	}

	// an advance
	publishNote(t, root, all, 700, signedBy(700, all, []signer{k}))
	code, o = jsonOf(t, stateArgs(tr, root, state)...)
	st = stateMember(t, o)
	steps := stepsOf(o)
	if code != 0 || st["action"] != "advanced" || st["extension_proved"] != true || st["old_size"] != float64(300) || st["new_size"] != float64(700) {
		t.Fatalf("advance: %d %v", code, st)
	}
	if !strings.Contains(steps["state"], "700 is larger than 300") || !strings.Contains(steps["tiles"], "proof hash(es)") || !strings.Contains(steps["consistency"], "RFC 9162") ||
		!strings.Contains(listOf(o, "notes"), "the state file was updated to size 700") ||
		!strings.Contains(listOf(o, "not_proved"), "the tile directory as a whole") {
		t.Errorf("an advance does not say what it did:\n%v", o)
	}

	// from the empty tree: a prefix of every tree, nothing to prove
	st0 := filepath.Join(testdir.New(t), "state0")
	z := testdir.New(t)
	publishNote(t, z, all, 300, emptyTreeNote(k))
	if code, out, errs := run(t, stateArgs(tr, z, st0)...); code != 0 || stateSize(t, st0) != 0 {
		t.Fatalf("a state of size 0: %d\n%s%s", code, out, errs)
	}
	publishNote(t, z, all, 300, signedBy(300, all, []signer{k}))
	code, o = jsonOf(t, stateArgs(tr, z, st0)...)
	st = stateMember(t, o)
	if code != 0 || st["action"] != "advanced" || st["extension_proved"] != true || st["old_size"] != float64(0) {
		t.Fatalf("from the empty tree: %d %v", code, st)
	}
	if !strings.Contains(stepsOf(o)["state"], "the empty tree, a prefix of every tree") {
		t.Errorf("no step says that the empty tree was the start: %v", stepsOf(o))
	}
	st1 := filepath.Join(testdir.New(t), "state1")
	if code, out, errs := run(t, stateArgs(tr, z, st1, "--note", writeNote(t, emptyTreeNote(k)))...); code != 0 {
		t.Fatalf("%d %s %s", code, out, errs)
	}
	code, out, _ := run(t, stateArgs(tr, z, st1)...)
	if code != 0 || !strings.Contains(out, "extends the empty tree of the state file") {
		t.Errorf("the headline of an advance from the empty tree: %d\n%s", code, out)
	}
	// and from a tree that is not the empty one, the headline names the size
	code, out, _ = run(t, stateArgs(tr, root, state)...)
	if code != 0 || !strings.Contains(out, "is the same as the checkpoint in the state file (size 700)") {
		t.Errorf("the headline of the same checkpoint: %d\n%s", code, out)
	}
}

// What a run says when a tile is missing or wrong, and what the evidence of a fork
// says in the JSON.
func TestStateRunsThatDoNotMoveSayWhere(t *testing.T) {
	k := newSigner(1)
	all := leaves(700)
	tr := trustOf(t, keySpec{"k", k, ""})
	root := testdir.New(t)
	state := filepath.Join(testdir.New(t), "state")
	publishNote(t, root, all, 300, signedBy(300, all, []signer{k}))
	if code, out, _ := run(t, stateArgs(tr, root, state)...); code != 0 {
		t.Fatal(out)
	}
	// no tile: status 3, the state stays at 300
	bare := testdir.New(t)
	logDir := filepath.Join(bare, "c", testChain, "e1")
	_ = os.MkdirAll(logDir, 0o755)
	_ = os.WriteFile(filepath.Join(logDir, "checkpoint"), []byte(signedBy(700, all, []signer{k})), 0o644)
	code, o := jsonOf(t, stateArgs(tr, bare, state)...)
	if code != 3 || !strings.Contains(listOf(o, "notes"), "the state file was not changed: it stays at size 300") {
		t.Errorf("no tiles: %d\n%v", code, o["notes"])
	}
	// damaged tiles: status 1, the state stays at 300
	damaged := testdir.New(t)
	publishNote(t, damaged, all, 700, signedBy(700, all, []signer{k}))
	_ = filepath.Walk(filepath.Join(damaged, "c"), func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() && strings.Contains(filepath.ToSlash(p), "/tile/") {
			b, _ := os.ReadFile(p)
			b[0] ^= 1
			_ = os.WriteFile(p, b, 0o644)
		}
		return nil
	})
	if code, _ = jsonOf(t, stateArgs(tr, damaged, state)...); code != 1 {
		t.Errorf("damaged tiles: %d", code)
	}

	// a rollback: the evidence is where the JSON says, and the note says that it may be genuine
	if code, out, _ := run(t, stateArgs(tr, root, state)...); code != 0 {
		t.Fatal(out)
	}
	publishNote(t, root, all, 700, signedBy(700, all, []signer{k}))
	if code, out, _ := run(t, stateArgs(tr, root, state)...); code != 0 {
		t.Fatal(out)
	}
	publishNote(t, root, all, 300, signedBy(300, all, []signer{k}))
	for i, want := range []string{"was saved to", "was already saved to"} {
		code, o = jsonOf(t, stateArgs(tr, root, state)...)
		s := stateMember(t, o)
		files, _ := filepath.Glob(state + ".conflict-300-*")
		if code != 1 || len(files) != 1 || filepath.Base(fmt.Sprint(s["conflict_file"])) != filepath.Base(files[0]) || s["evidence_saved"] != true {
			t.Fatalf("rollback %d: %d %v %v", i, code, s, files)
		}
		if !strings.Contains(listOf(o, "notes"), want) || !strings.Contains(listOf(o, "notes"), "a smaller size can be an old genuine checkpoint") {
			t.Errorf("rollback %d: the notes do not say %q:\n%s", i, want, listOf(o, "notes"))
		}
	}
	// a fork of the same size: the evidence is saved, and the rollback sentence is not there
	forked := leavesSalt(700, 400, "other")
	froot := testdir.New(t)
	publishNote(t, froot, forked, 700, signedBy(700, forked, []signer{k}))
	code, o = jsonOf(t, stateArgs(tr, froot, state)...)
	s := stateMember(t, o)
	if code != 1 || s["conflict_file"] == nil || strings.Contains(listOf(o, "notes"), "a smaller size can be") {
		t.Errorf("a fork: %d %v\n%s", code, s, listOf(o, "notes"))
	}
}

// The messages that say why a note you hold cannot serve, and what the notes
// about the signatures that were not counted name.
func TestHeldNoteMessagesNameTheCause(t *testing.T) {
	k1, k2, k3 := newSigner(1), newSigner(2), newSigner(3)
	all := leaves(700)
	root := testdir.New(t)
	state := filepath.Join(testdir.New(t), "state")
	good := trustOf(t, keySpec{"k1", k1, ""})
	publishNote(t, root, all, 300, signedBy(300, all, []signer{k1}))
	if code, out, _ := run(t, stateArgs(good, root, state)...); code != 0 {
		t.Fatal(out)
	}
	publishNote(t, root, all, 700, signedBy(700, all, []signer{k2}))
	fp16 := func(s signer) string { return s.fp()[:16] }

	// the only key that signed the state is revoked: the state is still the anchor
	// (L12-01), and the output names the cause, the fingerprint, and what it does
	// not show
	revoked := trustOf(t, keySpec{"k1", k1, `,"revoked":true`}, keySpec{"k2", k2, ""})
	state2 := filepath.Join(testdir.New(t), "state")
	if err := os.WriteFile(state2, []byte(fileText(t, state)), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errs := run(t, stateArgs(revoked, root, state2)...)
	for _, want := range []string{"is revoked in the trust file, so its signatures are not evidence", "(" + fp16(k1) + ")", "rests only on keys that the trust file no longer counts", "does not rest only on such keys any more"} {
		if code != 0 || !strings.Contains(out, want) {
			t.Errorf("a revoked signer, want %q: %d\n%s%s", want, code, out, errs)
		}
	}
	// no key of the file signed it at all: refused, with the advice that keeps the size
	other := trustOf(t, keySpec{"k2", k2, ""})
	code, _, errs = run(t, stateArgs(other, root, state)...)
	if code != 2 || !strings.Contains(errs, "has no signature of a key of the trust file that verifies") || strings.Contains(errs, "the note claims") || !strings.Contains(errs, "must not be deleted") || !strings.Contains(errs, "--min-size") {
		t.Errorf("no key signed it: %d\n%s", code, errs)
	}
	// a note that names a revoked key and a valid one: the revoked one is named, with its fingerprint, and a key of the file that is not on the note is not named
	three := trustOf(t, keySpec{"k1", k1, `,"revoked":true`}, keySpec{"k2", k2, ""}, keySpec{"k3", k3, `,"revoked":true`})
	publishNote(t, root, all, 700, signedBy(700, all, []signer{k1, k2}, junkLine(logOrigin(), newSigner(77))))
	code, o := jsonOf(t, "checkpoint", "--trust", three, "--log", logSpecArg(), "--source", root)
	notes := listOf(o, "notes")
	if code != 0 || !strings.Contains(notes, `carries a signature of key "k1" (`+fp16(k1)+`)`) || strings.Contains(notes, `carries a signature of key "k3"`) {
		t.Errorf("a revoked and a valid signer: %d\n%s", code, notes)
	}
	// the line of a key of the file that names it and does not verify says so, with the fingerprint
	third := junkLine(logOrigin(), k3)
	publishNote(t, root, all, 700, signedBy(700, all, []signer{k2}, third))
	code, o = jsonOf(t, "checkpoint", "--trust", three, "--log", logSpecArg(), "--source", root)
	notes = listOf(o, "notes")
	if code != 0 || !strings.Contains(notes, `a line that names key "k3" (`+fp16(k3)+`) and does not verify`) {
		t.Errorf("a line of a revoked key: %d\n%s", code, notes)
	}
	// what was dropped is counted: two lines that were not counted, one that verified
	publishNote(t, root, all, 700, signedBy(700, all, []signer{k2}, third, junkLine(logOrigin(), newSigner(78))))
	code, o = jsonOf(t, stateArgs(three, root, filepath.Join(testdir.New(t), "s"))...)
	if code != 0 || !strings.Contains(listOf(o, "notes"), "the note as served has 2 signature line(s) that were not counted (they verify under no key of the trust file): the state and any saved evidence keep only the 1 that verify (counted or not)") {
		t.Errorf("the count of lines that were not stored: %d\n%s", code, listOf(o, "notes"))
	}
}

// stateMember is the member info.state of a JSON result.
func stateMember(t *testing.T, o map[string]any) map[string]any {
	t.Helper()
	info, _ := o["info"].(map[string]any)
	st, _ := info["state"].(map[string]any)
	if st == nil {
		t.Fatalf("no info.state in %v", o)
	}
	return st
}

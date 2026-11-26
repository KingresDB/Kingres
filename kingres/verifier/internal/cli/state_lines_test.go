// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/checkpoint"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/signednote"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/trust"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
)

// L12-01 and L12-03. The invariant of the signature lines of a note: the identity
// of a checkpoint is its signed text (origin, size, root, extension lines) and
// never its signature lines. Lines of the same text only accumulate, so a state
// keeps the union of every line that verified for that text; two notes with the
// same text are the same checkpoint (no fork, no evidence file of their own, no
// status 4); and a state is usable as long as its text verified once under a key
// of the trust file, whatever the key has become since. Nobody without a key can
// therefore choose, by serving some of the genuine lines, what a later run does.

// firstLine is the verdict word and, for a failure, the step.
func firstLine(out string) string {
	l, _, _ := strings.Cut(out, "\n")
	head, _, _ := strings.Cut(l, ":")
	return head
}

// splitNote cuts a note into its text and its signature lines.
func splitNote(note string) (string, []string) {
	text, sigs, _ := strings.Cut(note, "\n\n")
	var lines []string
	for _, l := range strings.Split(strings.TrimSuffix(sigs, "\n"), "\n") {
		if l != "" {
			lines = append(lines, l+"\n")
		}
	}
	return text + "\n", lines
}

// joinNote makes a note from a text and signature lines in the order given.
func joinNote(text string, lines []string) string {
	return text + "\n" + strings.Join(lines, "")
}

func evidenceNames(dir string) []string {
	var out []string
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.Contains(e.Name(), ".conflict-") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func lineKeys(note string) map[string]bool {
	_, lines := splitNote(note)
	m := map[string]bool{}
	for _, l := range lines {
		m[l] = true
	}
	return m
}

// The reported case (L12-01). A note of two keys is served with the line of one
// of them; that key is revoked later. The state must be as usable as the one that
// was made from the whole note.
func TestPublisherCannotChooseWhichKeyTheStateRestsOn(t *testing.T) {
	k1, k2 := newSigner(1), newSigner(2)
	all := leaves(700)
	both := trustOf(t, keySpec{"k1", k1, ""}, keySpec{"k2", k2, ""})
	k2Revoked := trustOf(t, keySpec{"k1", k1, ""}, keySpec{"k2", k2, `,"revoked":true`})
	k2Limited := trustOf(t, keySpec{"k1", k1, ""}, keySpec{"k2", k2, `,"max_tree_size":299`})
	for _, c := range []struct {
		name string
		tr   string
	}{{"revoked", k2Revoked}, {"limited below the size", k2Limited}} {
		t.Run(c.name, func(t *testing.T) {
			var statuses [2]int
			for i, serveOnlyK2 := range []bool{false, true} {
				root := testdir.New(t)
				state := filepath.Join(testdir.New(t), "state")
				note := signedBy(300, all, []signer{k1, k2})
				if serveOnlyK2 {
					text, _ := splitNote(note)
					note = joinNote(text, []string{sigLine(logOrigin(), text, k2)})
				}
				publishNote(t, root, all, 300, note)
				if code, out, errs := run(t, stateArgs(both, root, state)...); code != 0 {
					t.Fatalf("seed: %d\n%s%s", code, out, errs)
				}
				publishNote(t, root, all, 700, signedBy(700, all, []signer{k1}))
				code, out, errs := run(t, stateArgs(c.tr, root, state)...)
				statuses[i] = code
				if code != 0 || stateSize(t, state) != 700 {
					t.Fatalf("serveOnlyK2=%v: %d\n%s%s", serveOnlyK2, code, out, errs)
				}
				if code, out, errs := run(t, "consistency", "--trust", c.tr, "--log", logSpecArg(), "--source", root, "--old", state); code != 0 {
					t.Fatalf("serveOnlyK2=%v consistency: %d\n%s%s", serveOnlyK2, code, out, errs)
				}
			}
			if statuses[0] != statuses[1] {
				t.Fatalf("the choice of lines changed the status: %v", statuses)
			}
		})
	}
}

// The state that rests only on a revoked key is the anchor, and says so; the
// next checkpoint that extends it and is signed by a key that counts replaces it.
func TestStateOnARevokedKeyOnlyIsUsedAndSaysSo(t *testing.T) {
	k1, k2 := newSigner(1), newSigner(2)
	all := leaves(700)
	root := testdir.New(t)
	both := trustOf(t, keySpec{"k1", k1, ""}, keySpec{"k2", k2, ""})
	revoked := trustOf(t, keySpec{"k1", k1, ""}, keySpec{"k2", k2, `,"revoked":true`})
	state := filepath.Join(testdir.New(t), "state")
	text, _ := splitNote(signedBy(300, all, nil))
	publishNote(t, root, all, 300, joinNote(text, []string{sigLine(logOrigin(), text, k2)}))
	if code, out, errs := run(t, stateArgs(both, root, state)...); code != 0 {
		t.Fatalf("seed: %d\n%s%s", code, out, errs)
	}
	// a smaller checkpoint is a rollback against the state, as ever
	publishNote(t, root, all, 255, signedBy(255, all, []signer{k1}))
	if code, out, _ := run(t, stateArgs(revoked, root, state)...); code != 1 || !strings.Contains(out, "FAILED at step rollback") || !strings.Contains(out, "rests only on keys that the trust file no longer counts") {
		t.Fatalf("a rollback against a state that rests on a revoked key: %d\n%s", code, out)
	}
	// a larger one that does not extend it is a fork, as ever
	forked := leavesSalt(700, 100, "other")
	froot := testdir.New(t)
	publishNote(t, froot, forked, 700, signedBy(700, forked, []signer{k1}))
	if code, out, _ := run(t, stateArgs(revoked, froot, state)...); code != 1 || !strings.Contains(out, "FAILED at step fork") {
		t.Fatalf("a fork against a state that rests on a revoked key: %d\n%s", code, out)
	}
	// the genuine one extends it, and the state now rests on a key that counts
	publishNote(t, root, all, 700, signedBy(700, all, []signer{k1}))
	code, out, _ := run(t, stateArgs(revoked, root, state)...)
	// the run proved that the held checkpoint is a prefix of one signed by a key that
	// counts, and the state it wrote rests on that key: nothing is left to list (L16-03)
	if code != 0 || strings.Contains(out, "that the checkpoint the state file held was genuine") || !strings.Contains(out, "does not rest only on such keys any more") {
		t.Fatalf("the genuine extension: %d\n%s", code, out)
	}
	code, out, _ = run(t, stateArgs(revoked, root, state)...)
	if code != 0 || strings.Contains(out, "rests only on keys") {
		t.Fatalf("the state after it advanced still says it rests on a revoked key: %d\n%s", code, out)
	}
}

// Lines of one text accumulate: the state keeps the union, and nothing else
// changes.
func TestLinesOfOneCheckpointAccumulate(t *testing.T) {
	k1, k2, k3 := newSigner(1), newSigner(2), newSigner(3)
	all := leaves(300)
	root := testdir.New(t)
	tr := trustOf(t, keySpec{"k1", k1, ""}, keySpec{"k2", k2, ""}, keySpec{"k3", k3, ""})
	state := filepath.Join(testdir.New(t), "state")
	text, _ := splitNote(signedBy(300, all, nil))
	serve := func(keys ...signer) {
		var ls []string
		for _, k := range keys {
			ls = append(ls, sigLine(logOrigin(), text, k))
		}
		publishNote(t, root, all, 300, joinNote(text, ls))
	}
	action := func(extra ...string) (string, string) {
		code, out, errs := run(t, append(stateArgs(tr, root, state), append(extra, "--json")...)...)
		if code != 0 {
			t.Fatalf("status %d\n%s%s", code, out, errs)
		}
		var got struct {
			Info struct {
				State struct {
					Action string `json:"action"`
				} `json:"state"`
			} `json:"info"`
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatal(err)
		}
		return got.Info.State.Action, fileText(t, state)
	}
	serve(k2)
	if a, _ := action(); a != "created" {
		t.Fatalf("first run: %s", a)
	}
	serve(k1) // another line of the same text
	a, s2 := action()
	if a != "signatures_added" || len(sigLines(storedNote(t, state))) != 2 {
		t.Fatalf("a second line: %s\n%s", a, s2)
	}
	serve(k3, k1, k2) // every line, in another order
	a, s3 := action()
	if a != "signatures_added" || len(sigLines(storedNote(t, state))) != 3 {
		t.Fatalf("the third line: %s\n%s", a, s3)
	}
	// nothing new: the file is not rewritten
	for _, ks := range [][]signer{{k1}, {k2}, {k3}, {k3, k2}, {k1, k2, k3}} {
		serve(ks...)
		before, _ := os.Stat(state)
		a, s := action()
		after, _ := os.Stat(state)
		if a != "unchanged" || s != s3 || !before.ModTime().Equal(after.ModTime()) {
			t.Fatalf("serving %d line(s) changed the state: %s", len(ks), a)
		}
	}
}

// L12-03: every subset of the genuine lines of a rolled-back checkpoint is one
// evidence file, never status 4.
func TestEveryChoiceOfLinesIsOneEvidenceFile(t *testing.T) {
	var ks []signer
	var specs []keySpec
	for i := 0; i < 4; i++ {
		ks = append(ks, newSigner(byte(20+i)))
		specs = append(specs, keySpec{fmt.Sprintf("k%d", i), ks[i], ""})
	}
	tr := trustOf(t, specs...)
	all := leaves(700)
	root := testdir.New(t)
	state := filepath.Join(testdir.New(t), "state")
	publishNote(t, root, all, 700, signedBy(700, all, ks))
	if code, out, errs := run(t, stateArgs(tr, root, state)...); code != 0 {
		t.Fatalf("seed: %d\n%s%s", code, out, errs)
	}
	text, _ := splitNote(signedBy(300, all, nil))
	kept := fileText(t, state)
	for mask := 1; mask < 16; mask++ {
		var ls []string
		for i := 0; i < 4; i++ {
			if mask>>uint(i)&1 == 1 {
				ls = append(ls, sigLine(logOrigin(), text, ks[i]))
			}
		}
		publishNote(t, root, all, 300, joinNote(text, ls))
		code, out, _ := run(t, stateArgs(tr, root, state)...)
		if code != 1 || strings.Contains(out, "NOT saved") {
			t.Fatalf("subset %04b: status %d, want 1\n%s", mask, code, out)
		}
	}
	if fileText(t, state) != kept {
		t.Fatal("a rollback moved the state")
	}
	names := evidenceNames(filepath.Dir(state))
	if len(names) != 1 {
		t.Fatalf("%d evidence files for one checkpoint, want 1: %v", len(names), names)
	}
	if got := sigLines(fileText(t, filepath.Join(filepath.Dir(state), names[0]))); len(got) != 4 {
		t.Fatalf("the evidence holds %d lines, want the union of all 4", len(got))
	}
	// the other order: fewest lines last
	root2, state2 := testdir.New(t), filepath.Join(testdir.New(t), "state")
	publishNote(t, root2, all, 700, signedBy(700, all, ks))
	if code, out, _ := run(t, stateArgs(tr, root2, state2)...); code != 0 {
		t.Fatal(out)
	}
	for mask := 15; mask >= 1; mask-- {
		var ls []string
		for i := 3; i >= 0; i-- {
			if mask>>uint(i)&1 == 1 {
				ls = append(ls, sigLine(logOrigin(), text, ks[i]))
			}
		}
		publishNote(t, root2, all, 300, joinNote(text, ls))
		if code, out, _ := run(t, stateArgs(tr, root2, state2)...); code != 1 {
			t.Fatalf("subset %04b descending: status %d\n%s", mask, code, out)
		}
	}
	if names := evidenceNames(filepath.Dir(state2)); len(names) != 1 {
		t.Fatalf("descending: %d evidence files: %v", len(names), names)
	}
}

// What a refusal can still say: when the trust file no longer holds a key that
// verifies the state, the run is refused (status 2) with the one command that
// restores the protection, and that command works.
func TestRefusedStateNamesTheRebaseAndTheRebaseWorks(t *testing.T) {
	k1, k2 := newSigner(1), newSigner(2)
	all := leaves(700)
	root := testdir.New(t)
	both := trustOf(t, keySpec{"k1", k1, ""}, keySpec{"k2", k2, ""})
	onlyK1 := trustOf(t, keySpec{"k1", k1, ""})
	state := filepath.Join(testdir.New(t), "state")
	text, _ := splitNote(signedBy(300, all, nil))
	publishNote(t, root, all, 300, joinNote(text, []string{sigLine(logOrigin(), text, k2)}))
	if code, out, errs := run(t, stateArgs(both, root, state)...); code != 0 {
		t.Fatalf("seed: %d\n%s%s", code, out, errs)
	}
	kept := fileText(t, state)
	publishNote(t, root, all, 700, signedBy(700, all, []signer{k1}))
	code, out, errs := run(t, stateArgs(onlyK1, root, state)...)
	if code != 2 || fileText(t, state) != kept {
		t.Fatalf("a state that no key of the file verifies: %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{"--min-size", "--state NEWFILE", "must not be deleted"} {
		if !strings.Contains(errs, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, errs)
		}
	}
	// the advice, followed: a new state at the size believed, then it works
	fresh := filepath.Join(testdir.New(t), "newstate")
	if code, out, errs := run(t, stateArgs(onlyK1, root, fresh, "--min-size", "300")...); code != 0 {
		t.Fatalf("the rebase: %d\n%s%s", code, out, errs)
	}
	if stateSize(t, fresh) != 700 {
		t.Fatal("the new state is not at the size of the checkpoint")
	}
	if code, out, errs := run(t, stateArgs(onlyK1, root, fresh)...); code != 0 {
		t.Fatalf("the new state is not usable: %d\n%s%s", code, out, errs)
	}
	// and a rollback is caught by it, which is the protection that was restored
	publishNote(t, root, all, 300, signedBy(300, all, []signer{k1}))
	if code, out, _ := run(t, stateArgs(onlyK1, root, fresh)...); code != 1 || !strings.Contains(out, "rollback") {
		t.Fatalf("the rebased state does not catch a rollback: %d\n%s", code, out)
	}
}

// The guard that stands behind the canonical form is reached through the seam:
// a file that a later run would not read is never stored.
func TestReadBackGuardStopsAStateThatWouldNotBeRead(t *testing.T) {
	s := newSigner(1)
	all := leaves(700)
	root := testdir.New(t)
	tr := trustOf(t, keySpec{"k", s, ""})
	state := filepath.Join(testdir.New(t), "state")
	publishNote(t, root, all, 300, signedBy(300, all, []signer{s}))
	if code, out, errs := run(t, stateArgs(tr, root, state)...); code != 0 {
		t.Fatalf("seed: %d\n%s%s", code, out, errs)
	}
	kept := fileText(t, state)
	publishNote(t, root, all, 700, signedBy(700, all, []signer{s}))
	t.Cleanup(func() { encodeStateFn = encodeState })
	encodeStateFn = func(cp *checkpoint.Checkpoint, note []byte, signers []signednote.Key, now time.Time) ([]byte, error) {
		return encodeState(cp, bytes.ReplaceAll(note, []byte("\n700\n"), []byte("\n701\n")), signers, now)
	}
	code, _, errs := run(t, stateArgs(tr, root, state)...)
	if code != 2 || !strings.Contains(errs, "cannot be recorded") || fileText(t, state) != kept {
		t.Fatalf("a state that would not be read was stored or not refused: %d\n%s", code, errs)
	}
}

// L12-02: a run that decided on an older state writes nothing over a newer one.
func TestLateWriterWritesNothingOverANewerState(t *testing.T) {
	s := newSigner(1)
	all := leaves(700)
	root := testdir.New(t)
	tr := trustOf(t, keySpec{"k", s, ""})
	state := filepath.Join(testdir.New(t), "state")
	publishNote(t, root, all, 300, signedBy(300, all, []signer{s}))
	if code, out, errs := run(t, stateArgs(tr, root, state)...); code != 0 {
		t.Fatalf("seed: %d\n%s%s", code, out, errs)
	}
	newer := fileText(t, state) + "\n" // a rival's write after this run read the state
	publishNote(t, root, all, 500, signedBy(500, all, []signer{s}))
	t.Cleanup(func() { encodeStateFn = encodeState })
	encodeStateFn = func(cp *checkpoint.Checkpoint, note []byte, signers []signednote.Key, now time.Time) ([]byte, error) {
		if err := os.WriteFile(state, []byte(newer), 0o600); err != nil {
			t.Fatal(err)
		}
		return encodeState(cp, note, signers, now)
	}
	code, _, errs := run(t, stateArgs(tr, root, state)...)
	if code != 2 || !strings.Contains(errs, "wrote nothing") || fileText(t, state) != newer {
		t.Fatalf("the late writer: %d\n%s", code, errs)
	}
}

// ---- the keyless adversary ----

// chooser gives the decisions of a world: a random source or the bytes of a
// fuzz input.
type chooser interface{ pick(n int) int }

type rngChooser struct{ r *rand.Rand }

func (c rngChooser) pick(n int) int { return c.r.Intn(n) }

type byteChooser struct {
	b []byte
	i int
}

func (c *byteChooser) pick(n int) int {
	if c.i >= len(c.b) {
		return 0
	}
	v := int(c.b[c.i]) % n
	c.i++
	return v
}

// runAdversaryWorld runs two worlds side by side over the same log and the same
// sequence of trust files. In the first the publisher serves the genuine note. In
// the second a party without a key sits between the publisher and the auditor
// and, at every run, may drop genuine signature lines, reorder them, add lines of
// keys the trust file holds or does not hold with junk signatures, and repeat a
// line. The property: whatever the adversary does that leaves a run with a counted
// genuine line and nothing malformed, the status, the size of the state, the name
// of every evidence file and the verdict of every later check are the ones of the
// first world; whatever else it does is a refusal that changes nothing, and the
// run after it, served the genuine note, is again the same as the first world.
// The one exception is not in the generator on purpose: the auditor deleting from
// the trust file every key whose line the adversary left (TestRefused...).
// advStats counts what the worlds did, for the log of the test.
var advStats = map[string]int{}

func runAdversaryWorld(t *testing.T, ch chooser, steps int, seedName string, nkeys int) {
	t.Helper()
	p := newPool(nkeys)
	all := leaves(700)
	sizes := []int{255, 300, 400, 513, 700}
	root := testdir.New(t)
	stA := filepath.Join(testdir.New(t), "state")
	stB := filepath.Join(testdir.New(t), "state")
	mk := func(size int, note string) string {
		publishNote(t, root, all, size, note)
		return writeNote(t, note)
	}
	weakSeen := false
	for step := 0; step < steps; step++ {
		st := map[int]standing{}
		for i := 0; i < nkeys; i++ {
			// usable more often than not, so that most runs advance the state
			st[i] = [...]standing{stUsable, stUsable, stUsable, stUsable, stRetired, stFuture, stRevoked, stLimited}[ch.pick(8)]
		}
		tr := trustWithStandings(t, p, st)
		size := sizes[ch.pick(len(sizes))]
		var signedIdx []int
		for i := 0; i < nkeys; i++ {
			if ch.pick(2) == 1 {
				signedIdx = append(signedIdx, i)
			}
		}
		if len(signedIdx) == 0 {
			signedIdx = []int{ch.pick(nkeys)}
		}
		var signers []signer
		for _, i := range signedIdx {
			signers = append(signers, p.keys[i])
		}
		honest := signedBy(size, all, signers)
		text, genuine := splitNote(honest)
		// what the adversary serves
		counts := func(i int) bool { // does the key count for this size now?
			s := st[i]
			return s == stUsable || (s == stLimited && size <= 400)
		}
		usableName := func(i int) bool { s := st[i]; return s == stUsable || s == stLimited }
		var kept []string
		keptIdx := []int{}
		for j, l := range genuine {
			if ch.pick(3) != 0 { // keep two of three
				kept = append(kept, l)
				keptIdx = append(keptIdx, signedIdx[j])
			}
		}
		if len(kept) == 0 {
			kept, keptIdx = []string{genuine[0]}, []int{signedIdx[0]}
		}
		refusal := false
		for i := ch.pick(3); i > 0; i-- { // junk lines
			switch ch.pick(3) {
			case 0: // a key that does not sign, named with a junk signature
				k := ch.pick(nkeys)
				isSigner := false
				for _, s := range signedIdx {
					if s == k {
						isSigner = true
					}
				}
				if line := junkLine(logOrigin(), p.keys[k]); !isSigner && !contains(kept, line) {
					kept = append(kept, line)
					if usableName(k) {
						refusal = true // a line that names a key the file can use and does not verify
					}
				}
			case 1: // a key the file does not know
				if line := junkLine(logOrigin(), newSigner(byte(150+ch.pick(40)))); !contains(kept, line) {
					kept = append(kept, line)
				}
			default: // a line repeated
				if ch.pick(2) == 0 {
					kept = append(kept, kept[ch.pick(len(kept))])
					refusal = true // two lines of one key: a malformed note
				}
			}
		}
		// the order
		for i := len(kept) - 1; i > 0; i-- {
			j := ch.pick(i + 1)
			kept[i], kept[j] = kept[j], kept[i]
		}
		served := joinNote(text, kept)
		counted := false
		for _, i := range keptIdx {
			if counts(i) {
				counted = true
			}
		}
		fileA, fileB := mk(size, honest), writeNote(t, served)
		// the same tiles serve both worlds; the note is given as a file
		argsFor := func(state, note string) []string {
			return stateArgs(tr, root, state, "--note", note)
		}
		beforeB, _ := os.ReadFile(stB)
		namesBefore := evidenceNames(filepath.Dir(stB))
		codeA, outA, _ := run(t, argsFor(stA, fileA)...)
		codeB, outB, errB := run(t, argsFor(stB, fileB)...)
		desc := fmt.Sprintf("%s step %d size %d signers %v kept %v standings %v", seedName, step, size, signedIdx, keptIdx, st)
		if strings.Contains(outB, "VERIFIED") && codeB != 0 {
			t.Fatalf("%s: VERIFIED with status %d", desc, codeB)
		}
		advStats[fmt.Sprintf("A status %d", codeA)]++
		if refusal || !counted {
			advStats["adversary refused"]++
			// a refusal: the adversary can always refuse a note by serving it wrong
			if codeB == 0 {
				t.Fatalf("%s: status 0 for a note that was made unusable (refusal=%v counted=%v)\n%s", desc, refusal, counted, outB)
			}
			afterB, _ := os.ReadFile(stB)
			if !bytes.Equal(beforeB, afterB) || fmt.Sprint(namesBefore) != fmt.Sprint(evidenceNames(filepath.Dir(stB))) {
				t.Fatalf("%s: a refused note changed the state or the evidence (status %d)\n%s%s", desc, codeB, outB, errB)
			}
			// the genuine note is served again: now the worlds agree
			codeB, outB, errB = run(t, argsFor(stB, fileA)...)
		}
		if !refusal && counted {
			advStats["adversary neutral"]++
		}
		weakA := strings.Contains(outA, "rests only on keys that the trust file no longer counts")
		weakB := strings.Contains(outB, "rests only on keys that the trust file no longer counts")
		if weakA != weakB {
			advStats["worlds differ in whether the state is weak"]++
		}
		if codeA != codeB || firstLine(outA) != firstLine(outB) {
			t.Fatalf("%s: the adversary changed the outcome: honest %d %q, adversary %d %q\n%s%s", desc, codeA, firstLine(outA), codeB, firstLine(outB), outB, errB)
		}
		if codeA == 4 || codeB == 4 {
			t.Fatalf("%s: status 4", desc)
		}
		if _, err := os.Stat(stA); err == nil {
			if stateSize(t, stA) != stateSize(t, stB) {
				t.Fatalf("%s: the state sizes differ: %d and %d", desc, stateSize(t, stA), stateSize(t, stB))
			}
			// the adversary's state holds genuine lines of the same text and no others
			linesA, linesB := lineKeys(storedNote(t, stA)), lineKeys(storedNote(t, stB))
			ta, _ := splitNote(storedNote(t, stA))
			tb, _ := splitNote(storedNote(t, stB))
			if ta != tb {
				t.Fatalf("%s: the held texts differ", desc)
			}
			for l := range linesB {
				if !linesA[l] && !contains(genuine, l) {
					t.Fatalf("%s: the adversary's state holds a line nobody signed: %q", desc, l)
				}
			}
			// every later check under the same trust file gives the same verdict
			for _, c := range [][]string{{"--old"}} {
				a, _, _ := run(t, "consistency", "--trust", tr, "--log", logSpecArg(), "--source", root, c[0], stA)
				b, _, _ := run(t, "consistency", "--trust", tr, "--log", logSpecArg(), "--source", root, c[0], stB)
				if a != b {
					t.Fatalf("%s: the state of the adversary world is used differently: consistency %d against %d", desc, a, b)
				}
			}
		}
		// A state that rests only on keys the trust file no longer counts keeps one
		// evidence file for its conflicts and not one per size (L14-02). Which lines a
		// state holds is what the publisher served on the run that made it, so a
		// keyless adversary decides whether the state of its world is such a state, and
		// with it the number of evidence files of the conflicts that follow; the names
		// are compared only while neither state has been weak. Everything else is
		// compared at every step: the status, the first line, the size, the held text.
		if strings.Contains(outA+outB, "rests only on keys that the trust file no longer counts") {
			weakSeen = true
		}
		if !weakSeen && fmt.Sprint(evidenceNames(filepath.Dir(stA))) != fmt.Sprint(evidenceNames(filepath.Dir(stB))) {
			t.Fatalf("%s: the evidence names differ: %v and %v", desc, evidenceNames(filepath.Dir(stA)), evidenceNames(filepath.Dir(stB)))
		}
	}
}

func contains(list []string, s string) bool {
	for _, l := range list {
		if l == s {
			return true
		}
	}
	return false
}

// TestKeylessAdversaryNeverChangesTheOutcome runs the worlds above for 150
// sequences of 8 runs: 1,200 runs in each world, each with a trust file of random
// standings (usable, retired, not yet valid, revoked, limited), a random size
// (a rollback among them), a random set of signers and a random adversary.
func TestKeylessAdversaryNeverChangesTheOutcome(t *testing.T) {
	seqs, wide := 150, 25
	if testing.Short() {
		seqs, wide = 20, 3
	}
	for seed := int64(0); seed < int64(seqs); seed++ {
		runAdversaryWorld(t, rngChooser{rand.New(rand.NewSource(1000 + seed))}, 8, fmt.Sprintf("seed %d", seed), 4)
	}
	// a trust file at the most keys it may hold (16), whose notes carry up to 16 lines
	for seed := int64(0); seed < int64(wide); seed++ {
		runAdversaryWorld(t, rngChooser{rand.New(rand.NewSource(5000 + seed))}, 8, fmt.Sprintf("wide seed %d", seed), trust.MaxKeys)
	}
	t.Logf("%d sequences of 4 keys and %d of %d keys, 8 runs each, in each world: %v", seqs, wide, trust.MaxKeys, advStats)
}

// FuzzKeylessAdversary is the same property driven by bytes.
func FuzzKeylessAdversary(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 0, 1, 1, 0, 2, 1, 1, 0, 0, 1, 2, 0, 1, 0, 0, 0})
	f.Add([]byte{3, 3, 3, 3, 0, 4, 1, 1, 0, 0, 1, 1, 1, 0, 2, 2, 0, 1, 1, 2, 2, 1, 1})
	f.Add([]byte{0, 0, 0, 0, 3, 2, 1, 0, 1, 1, 1, 1, 0, 0, 2, 1, 0, 0, 2, 0, 0, 1, 0, 1})
	f.Fuzz(func(t *testing.T, data []byte) {
		runAdversaryWorld(t, &byteChooser{b: data}, 5, "fuzz", 4)
	})
}

// Two epochs of one chain are two logs with their own origin: a state of one is
// not the anchor of the other, whatever lines it holds, and a second epoch makes a
// run without :e<N> a refusal that asks for it (a listing of the publication
// never decides which log is followed).
func TestTwoEpochsKeepTheirOwnState(t *testing.T) {
	k := newSigner(1)
	all := leaves(700)
	root := testdir.New(t)
	tr := trustOf(t, keySpec{"k", k, ""})
	e2origin := testPrefix + "/c/" + testChain + "/e2"
	e2 := func(size int) string {
		text := fmt.Sprintf("%s\n%d\n%s\n", e2origin, size, base64Root(all[:size]))
		return text + "\n" + sigLine(e2origin, text, k)
	}
	publishNote(t, root, all, 300, signedBy(300, all, []signer{k}))
	dir2 := filepath.Join(root, "c", testChain, "e2")
	writeTiles(t, dir2, all, 400)
	if err := os.WriteFile(filepath.Join(dir2, "checkpoint"), []byte(e2(400)), 0o644); err != nil {
		t.Fatal(err)
	}
	s1, s2 := filepath.Join(testdir.New(t), "s1"), filepath.Join(testdir.New(t), "s2")
	if code, out, errs := run(t, stateArgs(tr, root, s1)...); code != 0 {
		t.Fatalf("e1: %d\n%s%s", code, out, errs)
	}
	e2args := func(state string) []string {
		return []string{"checkpoint", "--trust", tr, "--log", "chain:" + testChain + ":e2", "--source", root, "--state", state}
	}
	if code, out, errs := run(t, e2args(s2)...); code != 0 {
		t.Fatalf("e2: %d\n%s%s", code, out, errs)
	}
	kept1, kept2 := fileText(t, s1), fileText(t, s2)
	if code, _, errs := run(t, e2args(s1)...); code != 2 || fileText(t, s1) != kept1 {
		t.Fatalf("the state of epoch 1 used for epoch 2: %d\n%s", code, errs)
	}
	if code, _, errs := run(t, stateArgs(tr, root, s2)...); code != 2 || fileText(t, s2) != kept2 {
		t.Fatalf("the state of epoch 2 used for epoch 1: %d\n%s", code, errs)
	}
	noEpoch := []string{"checkpoint", "--trust", tr, "--log", "chain:" + testChain, "--source", root, "--state", s1}
	if code, _, errs := run(t, noEpoch...); code != 2 || !strings.Contains(errs, "e<N>") || fileText(t, s1) != kept1 {
		t.Fatalf("two epochs and no :e<N>: %d\n%s", code, errs)
	}
}

func base64Root(h []h32) string {
	r := refMTH(h)
	return base64.StdEncoding.EncodeToString(r[:])
}

// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
)

// L16-07. A publisher without a key decides which genuine lines a state holds, and
// so whether a state rests only on a key that is revoked later. The random worlds
// of TestKeylessAdversaryNeverChangesTheOutcome meet that case rarely, so this
// test makes it every time: in world A the state holds the lines of two keys, in
// world B the adversary served the line of one, and then that one is revoked. In B
// the state is weak and in A it is not. What may differ is the wording, the name
// and the number of the evidence files (two at most in B, one per signed text in A)
// and the weak members of the JSON; the status, the step, the size and the held
// text must not, and a run that advances or adds a line leaves both worlds with a
// state that counts.

func TestKeylessAdversaryDecidesOnlyTheWordingWhenTheWorldsDifferInWhetherTheStateIsWeak(t *testing.T) {
	k1, k2 := newSigner(1), newSigner(2)
	all := leaves(900)
	forked300 := leavesSalt(300, 100, "fork300")
	forked700 := leavesSalt(700, 100, "fork700")
	both := trustOf(t, keySpec{"k1", k1, ""}, keySpec{"k2", k2, ""})
	revoked := trustOf(t, keySpec{"k1", k1, `,"revoked":true`}, keySpec{"k2", k2, ""})

	// the seed: the same checkpoint of size 300, served whole to A and with the line
	// of k1 only to B
	seedRoot := testdir.New(t)
	text, lines := splitNote(signedBy(300, all, []signer{k1, k2}))
	publishNote(t, seedRoot, all, 300, joinNote(text, lines))
	seedA, seedB := filepath.Join(testdir.New(t), "state"), filepath.Join(testdir.New(t), "state")
	for _, w := range []struct{ state, note string }{{seedA, joinNote(text, lines)}, {seedB, joinNote(text, lines[:1])}} {
		if code, out, errs := run(t, stateArgs(both, seedRoot, w.state, "--note", writeNote(t, w.note))...); code != 0 {
			t.Fatalf("seed: %d\n%s%s", code, out, errs)
		}
	}
	if n := len(lineKeys(storedNote(t, seedA))); n != 2 {
		t.Fatalf("world A holds %d lines, want 2", n)
	}
	if n := len(lineKeys(storedNote(t, seedB))); n != 1 {
		t.Fatalf("world B holds %d lines, want 1", n)
	}

	type kase struct {
		name     string
		step     string // the failed step, "" for a success
		code     int
		publish  func(root string)
		note     func() string // what is served, signed by k2 only
		conflict bool
	}
	cases := []kase{
		{"rollback", "rollback", 1, func(root string) { publishNote(t, root, all, 200, signedBy(200, all, []signer{k2})) },
			func() string { return signedBy(200, all, []signer{k2}) }, true},
		{"the same size, another root", "fork", 1, func(root string) { publishNote(t, root, forked300, 300, signedBy(300, forked300, []signer{k2})) },
			func() string { return signedBy(300, forked300, []signer{k2}) }, true},
		{"a larger size that does not extend it", "fork", 1, func(root string) { publishNote(t, root, forked700, 700, signedBy(700, forked700, []signer{k2})) },
			func() string { return signedBy(700, forked700, []signer{k2}) }, true},
		{"an extension", "", 0, func(root string) { publishNote(t, root, all, 700, signedBy(700, all, []signer{k2})) },
			func() string { return signedBy(700, all, []signer{k2}) }, false},
		{"the same checkpoint, k2's line", "", 0, func(root string) { publishNote(t, root, all, 300, signedBy(300, all, []signer{k2})) },
			func() string { return signedBy(300, all, []signer{k2}) }, false},
	}
	copyState := func(from string) string {
		b, err := os.ReadFile(from)
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(testdir.New(t), "state")
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := testdir.New(t)
			c.publish(root)
			note := writeNote(t, c.note())
			seeds := []string{seedA, seedB}
			states := []string{copyState(seedA), copyState(seedB)}
			var outs, texts [2]string
			var codes [2]int
			var weakJSON [2]*bool
			for i := range states {
				// the JSON from the same start, on a copy, and then the run itself
				_, jout, _ := run(t, append(stateArgs(revoked, root, copyState(seeds[i]), "--note", note), "--json")...)
				var a anchorJSON
				if err := json.Unmarshal([]byte(jout), &a); err != nil {
					t.Fatalf("%v\n%s", err, jout)
				}
				weakJSON[i] = a.Info.State.Weak
				code, out, errs := run(t, stateArgs(revoked, root, states[i], "--note", note)...)
				codes[i], outs[i] = code, out+errs
			}
			stA, stB := states[0], states[1]
			if codes[0] != c.code || codes[1] != c.code {
				t.Fatalf("status A %d, B %d, want %d\n%s\n---\n%s", codes[0], codes[1], c.code, outs[0], outs[1])
			}
			if firstLine(outs[0]) != firstLine(outs[1]) {
				t.Errorf("the step differs: %q and %q", firstLine(outs[0]), firstLine(outs[1]))
			}
			if c.step != "" && !strings.HasPrefix(outs[0], "FAILED at step "+c.step) {
				t.Errorf("A: %.200s", outs[0])
			}
			for i, st := range []string{stA, stB} {
				texts[i], _ = splitNote(storedNote(t, st))
			}
			if texts[0] != texts[1] || stateSize(t, stA) != stateSize(t, stB) {
				t.Errorf("the held checkpoints differ: %d and %d", stateSize(t, stA), stateSize(t, stB))
			}
			weakWords := "rests only on keys that the trust file no longer counts"
			if c.conflict {
				// the case the random worlds rarely meet: the worlds differ in whether the
				// state is weak, and only the wording and the evidence follow from it
				if strings.Contains(outs[0], weakWords) || !strings.Contains(outs[1], weakWords) {
					t.Errorf("A weak: %v, B weak: %v, want false and true", strings.Contains(outs[0], weakWords), strings.Contains(outs[1], weakWords))
				}
				if weakJSON[0] == nil || *weakJSON[0] || weakJSON[1] == nil || !*weakJSON[1] {
					t.Errorf("the JSON: A %v B %v, want false and true", weakJSON[0], weakJSON[1])
				}
				if n := evidenceNames(filepath.Dir(stA)); len(n) != 1 || strings.Contains(n[0], "conflict-weak") {
					t.Errorf("world A: %v, want one file for the signed text", n)
				}
				if n := evidenceNames(filepath.Dir(stB)); len(n) != 1 || !strings.Contains(n[0], "conflict-weak-"+c.step+"-300-") {
					t.Errorf("world B: %v, want the file of its kind", n)
				}
				// the note is saved in both or printed in both: nothing is lost in either
				for i, st := range []string{stA, stB} {
					if got := fileText(t, filepath.Join(filepath.Dir(st), evidenceNames(filepath.Dir(st))[0])); !strings.Contains(got, strings.Split(c.note(), "\n")[1]) {
						t.Errorf("world %d: the evidence file does not hold the served note:\n%s", i, got)
					}
				}
				return
			}
			// a success: both worlds end with a state that counts, and agree from then on
			for i, st := range []string{stA, stB} {
				code, out, _ := run(t, stateArgs(revoked, root, st, "--note", note)...)
				if code != 0 || strings.Contains(out, weakWords) {
					t.Errorf("world %d after the success: %d\n%s", i, code, out)
				}
			}
			if weakJSON[0] == nil || *weakJSON[0] || weakJSON[1] == nil || *weakJSON[1] {
				t.Errorf("a success leaves both states counting: A %v B %v", weakJSON[0], weakJSON[1])
			}
		})
	}
}

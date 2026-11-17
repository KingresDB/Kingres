// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// L14-02. A state that rests only on keys that the trust file no longer counts
// stays the anchor (the decision of the fourth round), and when it meets the
// honest log the run must not accuse the log: the output says that the state may
// be the wrong side, names the keys, gives the rebase, saves at most one evidence
// file for the state and not one per run, and puts the same facts in the JSON.

// thief is the reviewer's scenario: key a signs a forked log of 700 entries while
// it counts, the auditor stores it, a is revoked, and the honest log is signed by
// key b.
type thief struct {
	t         *testing.T
	a, b      signer
	trustWeak string // a revoked, b usable
	state     string
	stateText string
	honest    []h32
	root      string // the honest publication
}

func newThief(t *testing.T) *thief {
	t.Helper()
	th := &thief{t: t, a: newSigner(1), b: newSigner(2), honest: leaves(900), root: t.TempDir()}
	both := trustOf(t, keySpec{"a", th.a, ""}, keySpec{"b", th.b, ""})
	th.trustWeak = trustOf(t, keySpec{"a", th.a, `,"revoked":true`}, keySpec{"b", th.b, ""})
	forked := leavesSalt(700, 100, "thief")
	froot := t.TempDir()
	publishNote(t, froot, forked, 700, signedBy(700, forked, []signer{th.a}))
	th.state = filepath.Join(t.TempDir(), "state")
	if code, out, errs := run(t, stateArgs(both, froot, th.state)...); code != 0 {
		t.Fatalf("the thief's state: %d\n%s%s", code, out, errs)
	}
	th.stateText = fileText(t, th.state)
	return th
}

// serve publishes the honest log at size, signed by b.
func (th *thief) serve(size int) {
	publishNote(th.t, th.root, th.honest, size, signedBy(size, th.honest, []signer{th.b}))
}

// the three outcomes of the scenario: the honest log is smaller, the same size
// and larger than the state.
var thiefCases = []struct {
	size int
	step string
}{{300, "rollback"}, {700, "fork"}, {900, "fork"}}

func TestThiefStateMeetsTheHonestLogAndTheOutputSaysWhoMayBeWrong(t *testing.T) {
	th := newThief(t)
	for _, c := range thiefCases {
		th.serve(c.size)
		code, out, errs := run(t, stateArgs(th.trustWeak, th.root, th.state)...)
		if code != 1 || !strings.HasPrefix(out, "FAILED at step "+c.step+":") {
			t.Fatalf("size %d: status %d, want 1 at step %s\n%s%s", c.size, code, c.step, out, errs)
		}
		first, _, _ := strings.Cut(out, "\n")
		for _, want := range []string{
			`key "a"`, "revoked", "rests only on keys that the trust file no longer counts",
			"stolen key", "may come from your state and not from the log",
			"checkpoint --state NEWFILE --min-size N", "keep this state file",
		} {
			if !strings.Contains(first, want) {
				t.Errorf("size %d: the failure line does not say %q:\n%s", c.size, want, first)
			}
		}
		for _, bad := range []string{"both are signed by a key of the trust file", "the log has two histories", "two trusted checkpoints"} {
			if strings.Contains(out, bad) {
				t.Errorf("size %d: the output says %q of a state that no key counts:\n%s", c.size, bad, out)
			}
		}
		if fileText(t, th.state) != th.stateText {
			t.Fatalf("size %d: the state was changed by a conflict", c.size)
		}
	}
}

func TestThiefStateInTheJSON(t *testing.T) {
	th := newThief(t)
	for _, c := range thiefCases {
		th.serve(c.size)
		code, out, _ := run(t, append(stateArgs(th.trustWeak, th.root, th.state), "--json")...)
		var got struct {
			Verified bool   `json:"verified"`
			Failed   string `json:"failed_step"`
			Error    string `json:"error"`
			Info     struct {
				Extends bool `json:"extends_state"`
				State   struct {
					Action string `json:"action"`
					Weak   *bool  `json:"anchor_weak"`
					Keys   []struct {
						Name        string `json:"name"`
						Fingerprint string `json:"fingerprint"`
						Reason      string `json:"reason"`
					} `json:"anchor_weak_keys"`
					OldSize uint64 `json:"old_size"`
				} `json:"state"`
			} `json:"info"`
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("size %d: %v\n%s", c.size, err, out)
		}
		if code != 1 || got.Verified || got.Failed != c.step || got.Info.Extends {
			t.Fatalf("size %d: status %d verified %v step %q", c.size, code, got.Verified, got.Failed)
		}
		if got.Info.State.Weak == nil || !*got.Info.State.Weak || len(got.Info.State.Keys) != 1 ||
			got.Info.State.Keys[0].Name != "a" || got.Info.State.Keys[0].Fingerprint != th.a.fp() ||
			!strings.Contains(got.Info.State.Keys[0].Reason, "revoked") || got.Info.State.Action != "kept" || got.Info.State.OldSize != 700 {
			t.Errorf("size %d: the JSON does not carry the weak state: %+v", c.size, got.Info.State)
		}
		if !strings.Contains(got.Error, "stolen key") || !strings.Contains(got.Error, "--min-size") {
			t.Errorf("size %d: the error of the JSON does not say so: %s", c.size, got.Error)
		}
	}
}

// A weak state that is not in conflict says so in the JSON too (L14-07), and a
// state that counts says it does not.
func TestWeakAnchorIsInTheJSONOfASuccess(t *testing.T) {
	a, b := newSigner(1), newSigner(2)
	all := leaves(900)
	root := t.TempDir()
	both := trustOf(t, keySpec{"a", a, ""}, keySpec{"b", b, ""})
	weak := trustOf(t, keySpec{"a", a, `,"revoked":true`}, keySpec{"b", b, ""})
	state := filepath.Join(t.TempDir(), "state")
	publishNote(t, root, all, 300, signedBy(300, all, []signer{a}))
	if code, out, errs := run(t, stateArgs(both, root, state)...); code != 0 {
		t.Fatalf("seed: %d\n%s%s", code, out, errs)
	}
	type info struct {
		Info struct {
			State struct {
				Weak *bool `json:"anchor_weak"`
				Keys []any `json:"anchor_weak_keys"`
			} `json:"state"`
		} `json:"info"`
	}
	publishNote(t, root, all, 700, signedBy(700, all, []signer{b}))
	code, out, _ := run(t, append(stateArgs(weak, root, state), "--json")...)
	var got info
	if err := json.Unmarshal([]byte(out), &got); err != nil || code != 0 {
		t.Fatalf("%d %v\n%s", code, err, out)
	}
	if got.Info.State.Weak == nil || !*got.Info.State.Weak || len(got.Info.State.Keys) != 1 {
		t.Errorf("a run that extends a weak state does not say so: %s", out)
	}
	// the state now rests on b, which counts
	publishNote(t, root, all, 900, signedBy(900, all, []signer{b}))
	code, out, _ = run(t, append(stateArgs(weak, root, state), "--json")...)
	got = info{}
	if err := json.Unmarshal([]byte(out), &got); err != nil || code != 0 {
		t.Fatalf("%d %v\n%s", code, err, out)
	}
	if got.Info.State.Weak == nil || *got.Info.State.Weak || len(got.Info.State.Keys) != 0 {
		t.Errorf("a state that counts is called weak: %s", out)
	}
}

// Repeated runs, in text and JSON, at every size, keep one evidence file for the
// state and never add one; the file is the first conflicting note.
func TestThiefStateKeepsOneEvidenceFileWhateverTheNumberOfRuns(t *testing.T) {
	th := newThief(t)
	var first string
	for round := 0; round < 3; round++ {
		for _, c := range thiefCases {
			th.serve(c.size)
			run(t, stateArgs(th.trustWeak, th.root, th.state)...)
			run(t, append(stateArgs(th.trustWeak, th.root, th.state), "--json")...)
			names := evidenceNames(filepath.Dir(th.state))
			if len(names) != 1 || !strings.Contains(names[0], ".conflict-weak-700-") {
				t.Fatalf("round %d size %d: evidence files %v, want the one file of this state", round, c.size, names)
			}
			if first == "" {
				first = fileText(t, filepath.Join(filepath.Dir(th.state), names[0]))
				if !strings.Contains(first, "\n300\n") {
					t.Fatalf("the first conflict served was size 300, the file holds:\n%s", first)
				}
			} else if fileText(t, filepath.Join(filepath.Dir(th.state), names[0])) != first {
				t.Fatal("the evidence file was rewritten by a later run")
			}
		}
	}
	// the output says what happened to the evidence, and the status stays 1
	th.serve(900)
	code, out, _ := run(t, stateArgs(th.trustWeak, th.root, th.state)...)
	if code != 1 || !strings.Contains(out, "one file is kept for a state that rests only on keys the trust file no longer counts") {
		t.Errorf("a later conflict: status %d\n%s", code, out)
	}
	// a file that someone put at the name is not evidence: status 1 still, and it says NOT saved
	name := filepath.Join(filepath.Dir(th.state), evidenceNames(filepath.Dir(th.state))[0])
	if err := os.WriteFile(name, []byte("planted"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ = run(t, stateArgs(th.trustWeak, th.root, th.state)...)
	if code != 1 || !strings.Contains(out, "was NOT saved") || fileText(t, name) != "planted" {
		t.Errorf("a planted file at the name of the evidence: status %d\n%s", code, out)
	}
}

// The exit status of a conflict with a weak state is 1, and the rebase that the
// failure line names works and catches a rollback from then on.
func TestThiefStateRebaseWorks(t *testing.T) {
	th := newThief(t)
	th.serve(900)
	if code, out, _ := run(t, stateArgs(th.trustWeak, th.root, th.state)...); code != 1 {
		t.Fatalf("status %d\n%s", code, out)
	}
	fresh := filepath.Join(t.TempDir(), "newstate")
	if code, out, errs := run(t, stateArgs(th.trustWeak, th.root, fresh, "--min-size", "300")...); code != 0 || stateSize(t, fresh) != 900 {
		t.Fatalf("the rebase: %d\n%s%s", code, out, errs)
	}
	th.serve(300)
	if code, out, _ := run(t, stateArgs(th.trustWeak, th.root, fresh)...); code != 1 || !strings.Contains(out, "FAILED at step rollback") || strings.Contains(out, "stolen key") {
		t.Fatalf("the rebased state: %d\n%s", code, out)
	}
}

// A state that counts is not told that it may be the wrong side, and its
// evidence is one file for each conflicting checkpoint, as before.
func TestStateThatCountsKeepsTheOldWordsAndEvidence(t *testing.T) {
	th := newThief(t)
	both := trustOf(t, keySpec{"a", th.a, ""}, keySpec{"b", th.b, ""})
	for _, c := range thiefCases {
		th.serve(c.size)
		code, out, _ := run(t, stateArgs(both, th.root, th.state)...)
		if code != 1 || strings.Contains(out, "stolen key") || strings.Contains(out, "rests only on keys") {
			t.Fatalf("size %d: %d\n%s", c.size, code, out)
		}
		if c.step == "fork" && c.size == 900 && !strings.Contains(out, "both are signed by a key of the trust file") {
			t.Errorf("a state that counts lost the sentence that is true of it:\n%s", out)
		}
	}
	names := evidenceNames(filepath.Dir(th.state))
	if len(names) != 3 {
		t.Fatalf("a state that counts: %d evidence files, want one for each size: %v", len(names), names)
	}
	for _, n := range names {
		if strings.Contains(n, "conflict-weak") {
			t.Errorf("a weak name for a state that counts: %s", n)
		}
	}
}

// consistency --old STATE with such a state gives the same words and the same
// JSON, and its status is 1.
func TestConsistencyAgainstAThiefStateSaysWhoMayBeWrong(t *testing.T) {
	th := newThief(t)
	for _, c := range thiefCases {
		th.serve(c.size)
		code, out, errs := run(t, "consistency", "--trust", th.trustWeak, "--log", logSpecArg(), "--source", th.root, "--old", th.state)
		if code != 1 || !strings.HasPrefix(out, "FAILED at step ") {
			t.Fatalf("size %d: %d\n%s%s", c.size, code, out, errs)
		}
		first, _, _ := strings.Cut(out, "\n")
		for _, want := range []string{`key "a"`, "stolen key", "checkpoint --state NEWFILE --min-size N"} {
			if !strings.Contains(first, want) {
				t.Errorf("size %d: the failure line does not say %q:\n%s", c.size, want, first)
			}
		}
		_, jout, _ := run(t, "consistency", "--trust", th.trustWeak, "--log", logSpecArg(), "--source", th.root, "--old", th.state, "--json")
		var got struct {
			Info struct {
				Weak *bool `json:"old_anchor_weak"`
				Keys []struct {
					Name string `json:"name"`
				} `json:"old_anchor_weak_keys"`
			} `json:"info"`
		}
		if err := json.Unmarshal([]byte(jout), &got); err != nil || got.Info.Weak == nil || !*got.Info.Weak || len(got.Info.Keys) != 1 || got.Info.Keys[0].Name != "a" {
			t.Errorf("size %d: the JSON does not carry the weak state (%v): %s", c.size, err, jout)
		}
	}
	if names := evidenceNames(filepath.Dir(th.state)); len(names) != 0 {
		t.Errorf("consistency writes nothing, found %v", names)
	}
}

// L14-06 (M24b): consistency --old STATE with a weak state that the log does
// extend says that the old checkpoint was not shown to be genuine, in the text and
// in the JSON.
func TestConsistencyFromAWeakStateListsThatItWasNotShownGenuine(t *testing.T) {
	a, b := newSigner(1), newSigner(2)
	all := leaves(700)
	root := t.TempDir()
	both := trustOf(t, keySpec{"a", a, ""}, keySpec{"b", b, ""})
	weak := trustOf(t, keySpec{"a", a, `,"revoked":true`}, keySpec{"b", b, ""})
	state := filepath.Join(t.TempDir(), "state")
	publishNote(t, root, all, 300, signedBy(300, all, []signer{a}))
	if code, out, errs := run(t, stateArgs(both, root, state)...); code != 0 {
		t.Fatalf("seed: %d\n%s%s", code, out, errs)
	}
	publishNote(t, root, all, 700, signedBy(700, all, []signer{b}))
	code, out, errs := run(t, "consistency", "--trust", weak, "--log", logSpecArg(), "--source", root, "--old", state)
	if code != 0 || !strings.Contains(out, "not proved: that the old checkpoint was genuine: it rests only on keys the trust file no longer counts") {
		t.Fatalf("%d\n%s%s", code, out, errs)
	}
	_, jout, _ := run(t, "consistency", "--trust", weak, "--log", logSpecArg(), "--source", root, "--old", state, "--json")
	var got struct {
		NotProved []string `json:"not_proved"`
		Info      struct {
			Weak *bool `json:"old_anchor_weak"`
		} `json:"info"`
	}
	if err := json.Unmarshal([]byte(jout), &got); err != nil || got.Info.Weak == nil || !*got.Info.Weak {
		t.Fatalf("%v\n%s", err, jout)
	}
	found := false
	for _, l := range got.NotProved {
		found = found || strings.Contains(l, "that the old checkpoint was genuine")
	}
	if !found {
		t.Errorf("the JSON does not list it: %v", got.NotProved)
	}
	// and a state that counts does not list it
	if code, out, _ := run(t, "consistency", "--trust", both, "--log", logSpecArg(), "--source", root, "--old", state); code != 0 || strings.Contains(out, "that the old checkpoint was genuine") {
		t.Errorf("a state that counts: %d\n%s", code, out)
	}
}

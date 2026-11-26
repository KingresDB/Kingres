// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package cli

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/checkpoint"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/signednote"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/statefile"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
)

// The seventh review (L16): the evidence of a state that rests on a revoked key,
// the members of the JSON on every exit, the state a run repaired, the leftovers
// of a crash, and a failed flush of the directory.

var noteLineRE = regexp.MustCompile(`the note to keep, as base64 of its exact bytes \(decode it into a file\): ([A-Za-z0-9+/=]+)`)

// printedNote returns the note that a run printed in full, "" when it printed none.
func printedNote(t *testing.T, out string) string {
	t.Helper()
	m := noteLineRE.FindStringSubmatch(out)
	if m == nil {
		return ""
	}
	b, err := base64.StdEncoding.DecodeString(m[1])
	if err != nil {
		t.Fatalf("the printed note is not base64: %v", err)
	}
	return string(b)
}

// sizeOfNote is the second line of a note: the size it states.
func sizeOfNote(note string) string {
	l := strings.Split(note, "\n")
	if len(l) < 2 {
		return ""
	}
	return l[1]
}

func weakEvidence(th *thief) (rollback, fork []string) {
	for _, n := range evidenceNames(filepath.Dir(th.state)) {
		switch {
		case strings.Contains(n, ".conflict-weak-rollback-700-"):
			rollback = append(rollback, n)
		case strings.Contains(n, ".conflict-weak-fork-700-"):
			fork = append(fork, n)
		default:
			th.t.Errorf("an evidence name that is neither of the two kinds: %s", n)
		}
	}
	return
}

// L16-01 (a): a party without a key serves an old genuine checkpoint once (a
// rollback, which it can always do). It takes the file of the rollbacks and not
// the place of a later fork, which has a file of its own.
func TestWeakReplayOfAnOldCheckpointDoesNotTakeThePlaceOfAFork(t *testing.T) {
	th := newThief(t)
	th.serve(300)
	if code, out, _ := run(t, stateArgs(th.trustWeak, th.root, th.state)...); code != 1 || !strings.Contains(out, "FAILED at step rollback") {
		t.Fatalf("the replay: %d\n%s", code, out)
	}
	th.serve(900)
	code, out, _ := run(t, stateArgs(th.trustWeak, th.root, th.state)...)
	if code != 1 || !strings.Contains(out, "FAILED at step fork") || strings.Contains(out, "NOT saved") || printedNote(t, out) != "" {
		t.Fatalf("the fork after the replay: %d\n%s", code, out)
	}
	roll, fork := weakEvidence(th)
	if len(roll) != 1 || len(fork) != 1 {
		t.Fatalf("evidence files: rollback %v fork %v, want one of each", roll, fork)
	}
	dir := filepath.Dir(th.state)
	if r := fileText(t, filepath.Join(dir, roll[0])); sizeOfNote(r) != "300" {
		t.Errorf("the file of the rollbacks holds size %s:\n%s", sizeOfNote(r), r)
	}
	if f := fileText(t, filepath.Join(dir, fork[0])); sizeOfNote(f) != "900" {
		t.Errorf("the file of the forks holds size %s, want the fork of 900:\n%s", sizeOfNote(f), f)
	}
}

// L16-01 (a): a note that is not saved by this run is printed in full, in the text
// and in the JSON, whatever the reason.
func TestWeakConflictThatIsNotSavedIsPrintedInFull(t *testing.T) {
	th := newThief(t)
	dir := filepath.Dir(th.state)
	args := stateArgs(th.trustWeak, th.root, th.state)

	// 1. the file of the forks holds another checkpoint (the same size, another root)
	th.serve(700)
	if code, out, _ := run(t, args...); code != 1 || strings.Contains(out, "NOT saved") {
		t.Fatalf("the first fork: %d\n%s", code, out)
	}
	th.serve(900)
	code, out, _ := run(t, args...)
	if code != 1 || !strings.Contains(out, "was NOT saved by this run") {
		t.Fatalf("the second fork: %d\n%s", code, out)
	}
	served := fileText(t, filepath.Join(th.root, "c", testChain, "e1", "checkpoint"))
	if got := printedNote(t, out); got == "" || sizeOfNote(got) != "900" || !strings.Contains(served, strings.Split(got, "\n")[2]) {
		t.Fatalf("the note of the second fork is not in the output in full:\n%s", out)
	}
	_, jout, _ := run(t, append(args, "--json")...)
	var got struct {
		Info struct {
			State struct {
				Saved *bool   `json:"evidence_saved"`
				File  *string `json:"conflict_file"`
				Note  string  `json:"note_base64"`
				Weak  *bool   `json:"anchor_weak"`
			} `json:"state"`
		} `json:"info"`
	}
	if err := json.Unmarshal([]byte(jout), &got); err != nil {
		t.Fatalf("%v\n%s", err, jout)
	}
	if got.Info.State.Saved == nil || *got.Info.State.Saved || got.Info.State.File != nil || got.Info.State.Weak == nil || !*got.Info.State.Weak {
		t.Errorf("the JSON of a note that was not saved: %+v", got.Info.State)
	}
	if b, err := base64.StdEncoding.DecodeString(got.Info.State.Note); err != nil || sizeOfNote(string(b)) != "900" {
		t.Errorf("note_base64 does not hold the note of size 900: %v %q", err, got.Info.State.Note)
	}

	// 2. a file that someone put at the name of the forks
	name := filepath.Join(dir, evidenceNames(dir)[0])
	if err := os.WriteFile(name, []byte("planted"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ = run(t, args...)
	if code != 1 || !strings.Contains(out, "NOT saved") || sizeOfNote(printedNote(t, out)) != "900" || fileText(t, name) != "planted" {
		t.Errorf("a planted file at the name: %d\n%s", code, out)
	}

	// 3. the disk refuses
	old := saveWeakConfFn
	defer func() { saveWeakConfFn = old }()
	saveWeakConfFn = func(string, string, uint64, []byte, []byte, func([]byte) ([]byte, bool)) (statefile.Saved, error) {
		return statefile.Saved{}, errors.New("no space left on device")
	}
	code, out, _ = run(t, args...)
	if code != 1 || !strings.Contains(out, "no space left on device") || sizeOfNote(printedNote(t, out)) != "900" {
		t.Errorf("a failed save: %d\n%s", code, out)
	}
}

// The bound of the evidence of a weak state is two files whatever is served: any
// number of rollbacks and forks of any sizes, in text and in JSON.
func TestWeakEvidenceStaysAtTwoFilesWhateverIsServed(t *testing.T) {
	th := newThief(t)
	printed := 0
	for round := 0; round < 2; round++ {
		for _, size := range []int{100, 200, 300, 699, 700, 701, 800, 900} {
			th.serve(size)
			args := stateArgs(th.trustWeak, th.root, th.state)
			if round == 1 {
				args = append(args, "--json")
			}
			code, out, _ := run(t, args...)
			if code != 1 {
				t.Fatalf("size %d: status %d\n%s", size, code, out)
			}
			if strings.Contains(out, "NOT saved") {
				if round == 0 && printedNote(t, out) == "" {
					t.Fatalf("size %d: a note that was not saved is not printed:\n%s", size, out)
				}
				printed++
			}
			if names := evidenceNames(filepath.Dir(th.state)); len(names) > 2 {
				t.Fatalf("size %d: %d evidence files: %v", size, len(names), names)
			}
		}
	}
	roll, fork := weakEvidence(th)
	if len(roll) != 1 || len(fork) != 1 {
		t.Fatalf("rollback %v fork %v", roll, fork)
	}
	if printed == 0 {
		t.Error("no run printed a note that it could not save")
	}
}

// ---- L16-02: the member is on every exit after the held state was judged ----

type anchorJSON struct {
	Info struct {
		State struct {
			Weak *bool `json:"anchor_weak"`
		} `json:"state"`
		OldWeak *bool `json:"old_anchor_weak"`
	} `json:"info"`
}

func anchorOf(t *testing.T, out string) anchorJSON {
	t.Helper()
	var a anchorJSON
	if err := json.Unmarshal([]byte(out), &a); err != nil {
		t.Fatalf("not one JSON document: %v\n%s", err, out)
	}
	return a
}

// extEnv is a state of size 300 made while only key a signed, a publication that
// grows and is signed by b, and two trust files: one in which a still counts and
// one in which it is revoked.
type extEnv struct {
	t           *testing.T
	a, b        signer
	both, weak  string
	root, state string
	all         []h32
}

func newExtEnv(t *testing.T) *extEnv {
	t.Helper()
	x := &extEnv{t: t, a: newSigner(1), b: newSigner(2), all: leaves(900), root: testdir.New(t)}
	x.both = trustOf(t, keySpec{"a", x.a, ""}, keySpec{"b", x.b, ""})
	x.weak = trustOf(t, keySpec{"a", x.a, `,"revoked":true`}, keySpec{"b", x.b, ""})
	x.state = filepath.Join(testdir.New(t), "state")
	publishNote(t, x.root, x.all, 300, signedBy(300, x.all, []signer{x.a}))
	if code, out, errs := run(t, stateArgs(x.both, x.root, x.state)...); code != 0 {
		t.Fatalf("seed: %d\n%s%s", code, out, errs)
	}
	return x
}

func (x *extEnv) serve(size int) {
	publishNote(x.t, x.root, x.all, size, signedBy(size, x.all, []signer{x.b}))
}

func (x *extEnv) tiles() string { return filepath.Join(x.root, "c", testChain, "e1", "tile") }

func (x *extEnv) damage() {
	_ = filepath.Walk(x.tiles(), func(f string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			_ = os.WriteFile(f, []byte("short"), 0o644)
		}
		return nil
	})
}

func TestAnchorWeakIsOnEveryExitAfterTheStateWasJudged(t *testing.T) {
	for _, weak := range []bool{true, false} {
		weak := weak
		name := map[bool]string{true: "weak", false: "counts"}[weak]
		trustFor := func(x *extEnv) string {
			if weak {
				return x.weak
			}
			return x.both
		}
		// b must be able to sign in both trust files; the state is weak only in the first
		exits := []struct {
			name string
			code int
			set  func(x *extEnv) func()
		}{
			{"missing tile", 3, func(x *extEnv) func() { x.serve(700); _ = os.RemoveAll(x.tiles()); return nil }},
			{"bad tile", 1, func(x *extEnv) func() { x.serve(700); x.damage(); return nil }},
			{"read-back failure", 2, func(x *extEnv) func() {
				x.serve(700)
				old := encodeStateFn
				encodeStateFn = func(cp *checkpoint.Checkpoint, note []byte, signers []signednote.Key, now time.Time) ([]byte, error) {
					return encodeState(cp, append(append([]byte(nil), note...), 'x'), signers, now)
				}
				return func() { encodeStateFn = old }
			}},
			{"state changed under the run", 2, func(x *extEnv) func() {
				x.serve(700)
				old := encodeStateFn
				encodeStateFn = func(cp *checkpoint.Checkpoint, note []byte, signers []signednote.Key, now time.Time) ([]byte, error) {
					b, err := os.ReadFile(x.state)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(x.state, append(b, '\n'), 0o600); err != nil {
						t.Fatal(err)
					}
					return encodeState(cp, note, signers, now)
				}
				return func() { encodeStateFn = old }
			}},
			{"write failure", 2, func(x *extEnv) func() {
				x.serve(700)
				old := replaceStateFn
				replaceStateFn = func(string, []byte, []byte, bool, int64) error { return errors.New("no space left on device") }
				return func() { replaceStateFn = old }
			}},
			{"rollback", 1, func(x *extEnv) func() { x.serve(200); return nil }},
			{"success", 0, func(x *extEnv) func() { x.serve(700); return nil }},
		}
		for _, ex := range exits {
			t.Run(name+"/"+ex.name, func(t *testing.T) {
				x := newExtEnv(t)
				if restore := ex.set(x); restore != nil {
					defer restore()
				}
				code, out, _ := run(t, append(stateArgs(trustFor(x), x.root, x.state), "--json")...)
				if code != ex.code {
					t.Fatalf("status %d, want %d\n%s", code, ex.code, out)
				}
				a := anchorOf(t, out)
				if a.Info.State.Weak == nil {
					t.Fatalf("no info.state.anchor_weak on this exit:\n%s", out)
				}
				// a run that wrote a state with a key that counts reports the state as it is
				// after the write; every other exit reports the state that was held
				want := weak && ex.name != "success"
				if *a.Info.State.Weak != want {
					t.Errorf("anchor_weak is %v, want %v:\n%s", *a.Info.State.Weak, want, out)
				}
			})
		}
		t.Run(name+"/consistency", func(t *testing.T) {
			cons := []struct {
				name string
				code int
				set  func(x *extEnv, more *[]string)
			}{
				{"sizes", 1, func(x *extEnv, _ *[]string) { x.serve(200) }},
				{"missing tile", 3, func(x *extEnv, _ *[]string) { x.serve(700); _ = os.RemoveAll(x.tiles()) }},
				{"bad tile", 1, func(x *extEnv, _ *[]string) { x.serve(700); x.damage() }},
				{"new checkpoint is garbage", 1, func(x *extEnv, more *[]string) {
					*more = []string{"--new", writeNote(t, "garbage\n")}
				}},
				{"new checkpoint is missing", 2, func(x *extEnv, more *[]string) {
					*more = []string{"--new", filepath.Join(testdir.New(t), "none")}
				}},
				{"no checkpoint published", 3, func(x *extEnv, _ *[]string) {
					x.serve(700)
					_ = os.Remove(filepath.Join(x.root, "c", testChain, "e1", "checkpoint"))
				}},
				{"success", 0, func(x *extEnv, _ *[]string) { x.serve(700) }},
			}
			for _, c := range cons {
				x := newExtEnv(t)
				var more []string
				c.set(x, &more)
				args := append([]string{"consistency", "--trust", trustFor(x), "--log", logSpecArg(), "--source", x.root, "--old", x.state, "--json"}, more...)
				code, out, _ := run(t, args...)
				if code != c.code {
					t.Fatalf("%s: status %d, want %d\n%s", c.name, code, c.code, out)
				}
				a := anchorOf(t, out)
				if a.Info.OldWeak == nil || *a.Info.OldWeak != weak {
					t.Errorf("%s: old_anchor_weak is %v, want present and %v:\n%s", c.name, a.Info.OldWeak, weak, out)
				}
			}
		})
	}
}

// A run that stops before it has read the state (the lock is held by another run)
// has judged nothing and says nothing about it.
func TestLockTimeoutSaysNothingAboutTheAnchor(t *testing.T) {
	x := newExtEnv(t)
	x.serve(700)
	release := holdLock(t, x.state)
	defer release()
	code, out, _ := run(t, append(stateArgs(x.weak, x.root, x.state), "--json")...)
	if code != 2 {
		t.Fatalf("status %d\n%s", code, out)
	}
	if a := anchorOf(t, out); a.Info.State.Weak != nil {
		t.Errorf("a run that never read the state claims about it: %s", out)
	}
}

// ---- L16-03: a weak state that the run repaired ----

func TestWeakStateRepairedByTheRunIsReportedAsItIsAfterTheWrite(t *testing.T) {
	for _, tc := range []struct {
		name   string
		size   int
		action string
	}{{"the same checkpoint with a line that counts", 300, "signatures_added"}, {"an extension signed by a key that counts", 700, "advanced"}} {
		t.Run(tc.name, func(t *testing.T) {
			x := newExtEnv(t)
			x.serve(tc.size)
			code, out, _ := run(t, stateArgs(x.weak, x.root, x.state)...)
			if code != 0 || strings.Contains(out, "that the checkpoint the state file held was genuine") {
				t.Fatalf("text: %d\n%s", code, out)
			}
			if !strings.Contains(out, "does not rest only on such keys any more") {
				t.Errorf("the text does not say that the state was repaired:\n%s", out)
			}
			// the same again, from the same start, as JSON
			y := newExtEnv(t)
			y.serve(tc.size)
			code, jout, _ := run(t, append(stateArgs(y.weak, y.root, y.state), "--json")...)
			var got struct {
				NotProved []string `json:"not_proved"`
				Info      struct {
					State struct {
						Action   string `json:"action"`
						Weak     *bool  `json:"anchor_weak"`
						Repaired *bool  `json:"anchor_weak_repaired"`
						Keys     []any  `json:"anchor_weak_keys"`
					} `json:"state"`
				} `json:"info"`
			}
			if err := json.Unmarshal([]byte(jout), &got); err != nil || code != 0 {
				t.Fatalf("%d %v\n%s", code, err, jout)
			}
			s := got.Info.State
			if s.Action != tc.action || s.Weak == nil || *s.Weak || s.Repaired == nil || !*s.Repaired || len(s.Keys) != 0 {
				t.Errorf("the JSON of a repaired state: %+v\n%s", s, jout)
			}
			for _, l := range got.NotProved {
				if strings.Contains(l, "state file held was genuine") {
					t.Errorf("a non-proof that the run discharged is listed: %s", l)
				}
			}
			// and the next run agrees: the state does not rest only on a revoked key
			if code, out, _ := run(t, stateArgs(y.weak, y.root, y.state)...); code != 0 || strings.Contains(out, "rests only on keys") {
				t.Errorf("the next run: %d\n%s", code, out)
			}
		})
	}
}

// ---- L16-04: leftovers of a crash ----

func TestRunRemovesTheTemporaryFilesOfTheProgramAndNothingElse(t *testing.T) {
	x := newExtEnv(t)
	dir := filepath.Dir(x.state)
	mk := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// what a crash leaves: under the name of the state, and under the name of an evidence file
	gone := []string{
		mk(".kingres-state-0123456789ab-1234.tmp", "x"),
		mk(".kingres-evidence-0123456789ab-98765.tmp", "x"),
		mk(".kingres-state-ffffffffffff-1.tmp", "x"),
	}
	// what is not ours
	kept := []string{
		mk(".kingres-state-0123456789ab-12x4.tmp", "x"),
		mk(".kingres-state-0123456789a-1234.tmp", "x"),
		mk(".kingres-state-0123456789ab-.tmp", "x"),
		mk(".kingres-state-0123456789AB-1.tmp", "x"),
		mk(".kingres-evidence-0123456789ab-1.tmp.bak", "x"),
		mk("kingres-state-0123456789ab-1.tmp", "x"),
		mk("notes.tmp", "x"),
		mk(".kingres-state.lock.tmp", "x"),
	}
	if err := os.Mkdir(filepath.Join(dir, ".kingres-state-0123456789ab-777.tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	inside := mk(filepath.Join(".kingres-state-0123456789ab-777.tmp", "inner"), "x")
	kept = append(kept, inside)
	if err := os.Symlink(kept[0], filepath.Join(dir, ".kingres-evidence-0123456789ab-888.tmp")); err == nil {
		kept = append(kept, filepath.Join(dir, ".kingres-evidence-0123456789ab-888.tmp"))
	}
	// a run that has nothing to write: the same checkpoint again
	if code, out, errs := run(t, stateArgs(x.both, x.root, x.state)...); code != 0 {
		t.Fatalf("%d\n%s%s", code, out, errs)
	}
	for _, p := range gone {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s was left by a crash and is still there after a run", filepath.Base(p))
		}
	}
	for _, p := range kept {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s is not a temporary file of the program and was removed: %v", filepath.Base(p), err)
		}
	}
}

// ---- L16-05: a failed flush of the directory after a write that happened ----

func TestFailedDirectoryFlushAfterTheStateWasReplacedIsReportedAsThat(t *testing.T) {
	x := newExtEnv(t)
	x.serve(700)
	old := replaceStateFn
	defer func() { replaceStateFn = old }()
	replaceStateFn = func(p string, data, was []byte, existed bool, limit int64) error {
		if err := old(p, data, was, existed, limit); err != nil {
			return err
		}
		return &statefile.DirSyncError{Err: errors.New("injected flush failure")}
	}
	code, out, errs := run(t, append(stateArgs(x.both, x.root, x.state), "--json")...)
	if code != 2 {
		t.Fatalf("the status changed: %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{"was replaced", "the directory could not be flushed", "a power loss may bring the old file back", "injected flush failure"} {
		if !strings.Contains(out+errs, want) {
			t.Errorf("the message does not say %q:\n%s%s", want, out, errs)
		}
	}
	if strings.Contains(out+errs, "so later runs do not know it") {
		t.Errorf("the message says the state was not written, which is false:\n%s%s", out, errs)
	}
	if stateSize(t, x.state) != 700 {
		t.Errorf("the state is %d", stateSize(t, x.state))
	}
}

func TestFailedDirectoryFlushAfterTheEvidenceWasLinkedIsReportedAsThat(t *testing.T) {
	// a state that counts: the strong path, status 4 stays
	k, other := newSigner(1), newSigner(2)
	all := leaves(700)
	forked := leavesSalt(700, 300, "x")
	root, froot := testdir.New(t), testdir.New(t)
	tr := trustOf(t, keySpec{"k", k, ""}, keySpec{"o", other, ""})
	publishNote(t, root, all, 700, signedBy(700, all, []signer{k}))
	state := filepath.Join(testdir.New(t), "state")
	if code, out, _ := run(t, stateArgs(tr, root, state)...); code != 0 {
		t.Fatal(out)
	}
	publishNote(t, froot, forked, 700, signedBy(700, forked, []signer{k}))
	old := saveConflictFn
	defer func() { saveConflictFn = old }()
	saveConflictFn = func(p string, size uint64, r []byte, note []byte, m func([]byte) ([]byte, bool)) (statefile.Saved, error) {
		sv, err := old(p, size, r, note, m)
		if err != nil {
			return sv, err
		}
		return sv, &statefile.DirSyncError{Err: errors.New("injected flush failure")}
	}
	code, out, _ := run(t, append(stateArgs(tr, froot, state), "--json")...)
	if code != 4 {
		t.Fatalf("the status changed: %d\n%s", code, out)
	}
	var got struct {
		Notes []string `json:"notes"`
		Info  struct {
			State struct {
				Saved   *bool   `json:"evidence_saved"`
				Flushed *bool   `json:"evidence_flushed"`
				File    *string `json:"conflict_file"`
				Note    string  `json:"note_base64"`
			} `json:"state"`
		} `json:"info"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	s := got.Info.State
	if s.Saved == nil || !*s.Saved || s.Flushed == nil || *s.Flushed || s.File == nil || s.Note == "" {
		t.Errorf("the JSON: %+v", s)
	}
	text := strings.Join(got.Notes, "\n")
	if !strings.Contains(text, "was saved to") || !strings.Contains(text, "the directory could not be flushed") || strings.Contains(text, "NOT saved") {
		t.Errorf("the notes do not say what is true:\n%s", text)
	}
	if names := evidenceNames(filepath.Dir(state)); len(names) != 1 {
		t.Errorf("evidence files %v", names)
	}
}

func TestFailedDirectoryFlushAfterAWeakEvidenceFileIsReportedAsThat(t *testing.T) {
	th := newThief(t)
	th.serve(900)
	old := saveWeakConfFn
	defer func() { saveWeakConfFn = old }()
	saveWeakConfFn = func(p, kind string, size uint64, r []byte, note []byte, m func([]byte) ([]byte, bool)) (statefile.Saved, error) {
		sv, err := old(p, kind, size, r, note, m)
		if err != nil {
			return sv, err
		}
		return sv, &statefile.DirSyncError{Err: errors.New("injected flush failure")}
	}
	code, out, _ := run(t, stateArgs(th.trustWeak, th.root, th.state)...)
	if code != 1 || !strings.Contains(out, "was saved to") || !strings.Contains(out, "the directory could not be flushed") || strings.Contains(out, "NOT saved") || printedNote(t, out) == "" {
		t.Fatalf("%d\n%s", code, out)
	}
	if _, fork := weakEvidence(th); len(fork) != 1 {
		t.Errorf("the fork file is not there: %v", evidenceNames(filepath.Dir(th.state)))
	}
}

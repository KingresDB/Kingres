// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package cli

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/statefile"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
)

// ---- helpers: logs of our own, so that forks can be made ----

func logOrigin() string  { return testPrefix + "/c/" + testChain + "/e1" }
func logSpecArg() string { return "chain:" + testChain + ":e1" }

// leavesSalt is a tree whose entries from index from on differ by salt: a fork
// of leaves(n) at that index.
func leavesSalt(n, from int, salt string) []h32 {
	out := leaves(n)
	for i := from; i < n; i++ {
		out[i] = sha([]byte{0}, []byte(fmt.Sprintf("%s entry %d", salt, i)))
	}
	return out
}

// publishLog writes the tiles of the first size entries of all below root, and
// a checkpoint of that size signed by s.
func publishLog(t *testing.T, root string, s signer, all []h32, size int) string {
	t.Helper()
	logDir := filepath.Join(root, "c", testChain, "e1")
	writeTiles(t, logDir, all, size)
	note := s.note(logOrigin(), size, refMTH(all[:size]))
	if err := os.WriteFile(filepath.Join(logDir, "checkpoint"), []byte(note), 0o644); err != nil {
		t.Fatal(err)
	}
	return note
}

type keySpec struct {
	name  string
	s     signer
	extra string // members after the fingerprint, with a leading comma
}

func trustOf(t *testing.T, specs ...keySpec) string {
	t.Helper()
	var keys []string
	for _, k := range specs {
		keys = append(keys, fmt.Sprintf(`{"name":%q,"public_key":%q,"fingerprint":%q%s}`, k.name, base64.StdEncoding.EncodeToString(k.s.pub), k.s.fp(), k.extra))
	}
	f := filepath.Join(testdir.New(t), "trust.json")
	body := fmt.Sprintf(`{"format":1,"origin_prefix":%q,"keys":[%s]}`, testPrefix, strings.Join(keys, ","))
	if err := os.WriteFile(f, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

func writeNote(t *testing.T, note string) string {
	t.Helper()
	f := filepath.Join(testdir.New(t), "n")
	if err := os.WriteFile(f, []byte(note), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

func stateArgs(trust, source, state string, more ...string) []string {
	return append([]string{"checkpoint", "--trust", trust, "--log", logSpecArg(), "--source", source, "--state", state}, more...)
}

func stateSize(t *testing.T, path string) uint64 {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Size uint64 `json:"size"`
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatalf("the state file is not a record: %v\n%s", err, b)
	}
	return rec.Size
}

func fileText(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ---- L8-01 ----

func TestStateFirstUseGrowthAndBoundaries(t *testing.T) {
	s := newSigner(1)
	all := leaves(1100)
	tr := trustOf(t, keySpec{"k", s, ""})
	state := filepath.Join(testdir.New(t), "state")
	root := testdir.New(t)
	// First use: no state yet, status 0, nothing proved and the output says so.
	publishLog(t, root, s, all, 255)
	code, out, _ := run(t, stateArgs(tr, root, state)...)
	if code != 0 || !strings.Contains(out, "no earlier checkpoint was recorded") || stateSize(t, state) != 255 {
		t.Fatalf("first use: %d\n%s", code, out)
	}
	// Growth by one entry, to a full tile, across the tile boundary, and across
	// the second level: each is proved from the tiles in the same run.
	for _, size := range []int{256, 257, 300, 512, 513, 700, 1100} {
		publishLog(t, root, s, all, size)
		code, out, errs := run(t, stateArgs(tr, root, state)...)
		if code != 0 || !strings.Contains(out, "extends the checkpoint of size") || !strings.Contains(out, "consistency") || stateSize(t, state) != uint64(size) {
			t.Fatalf("growth to %d: %d\n%s%s", size, code, out, errs)
		}
	}
	// The same checkpoint again changes nothing.
	before := fileText(t, state)
	code, out, _ = run(t, stateArgs(tr, root, state)...)
	if code != 0 || fileText(t, state) != before || !strings.Contains(out, "nothing to prove") {
		t.Fatalf("same: %d\n%s", code, out)
	}
	// JSON says what was proved.
	publishLog(t, root, s, all, 1100)
	_, js, _ := run(t, append(stateArgs(tr, root, state), "--json")...)
	if !strings.Contains(js, `"extends_state": true`) {
		t.Errorf("json: %s", js)
	}
}

func TestStateRefusesAForkOfALargerSize(t *testing.T) {
	s := newSigner(1)
	tr := trustOf(t, keySpec{"k", s, ""})
	honest, fork := testdir.New(t), testdir.New(t)
	publishLog(t, honest, s, leaves(300), 300)
	publishLog(t, fork, s, leavesSalt(400, 5, "evil"), 400)
	state := filepath.Join(testdir.New(t), "state")
	if code, out, _ := run(t, stateArgs(tr, honest, state)...); code != 0 {
		t.Fatalf("honest: %d %s", code, out)
	}
	kept := fileText(t, state)
	code, out, _ := run(t, stateArgs(tr, fork, state)...)
	if code != 1 || strings.Contains(out, "VERIFIED") || !strings.Contains(out, "FAILED at step fork") {
		t.Fatalf("fork: %d\n%s", code, out)
	}
	if fileText(t, state) != kept {
		t.Fatal("the state was replaced by the fork")
	}
	// Both notes are kept, and the output says where.
	conflict := ""
	ents, _ := os.ReadDir(filepath.Dir(state))
	for _, e := range ents {
		if strings.Contains(e.Name(), ".conflict-400-") {
			conflict = filepath.Join(filepath.Dir(state), e.Name())
		}
	}
	if conflict == "" || !strings.Contains(out, filepath.Base(conflict)) {
		t.Fatalf("the conflicting checkpoint was not saved or not named (%q):\n%s", conflict, out)
	}
	if !strings.Contains(fileText(t, conflict), "\n400\n") {
		t.Error("the saved file is not the conflicting note")
	}
	// The saved note is a note a later run can use as --old.
	if code, out, _ := run(t, "consistency", "--trust", tr, "--log", logSpecArg(), "--source", fork, "--old", state); code != 1 {
		t.Errorf("consistency from the kept state: %d %s", code, out)
	}
}

// The publisher that shows an honest checkpoint to one command and the fork to
// the next: with --state there is one command, so the fork cannot be walked to.
func TestStateTwoFacedPublisher(t *testing.T) {
	s := newSigner(1)
	tr := trustOf(t, keySpec{"k", s, ""})
	h300, h500, fork := testdir.New(t), testdir.New(t), testdir.New(t)
	publishLog(t, h300, s, leaves(500), 300)
	publishLog(t, h500, s, leaves(500), 500)
	publishLog(t, fork, s, leavesSalt(400, 5, "evil"), 400)
	state := filepath.Join(testdir.New(t), "state")
	if code, out, _ := run(t, stateArgs(tr, h300, state)...); code != 0 {
		t.Fatal(out)
	}
	if code, out, _ := run(t, "consistency", "--trust", tr, "--log", logSpecArg(), "--source", h500, "--old", state); code != 0 {
		t.Fatal(out)
	}
	code, out, _ := run(t, stateArgs(tr, fork, state)...)
	if code != 1 || strings.Contains(out, "VERIFIED") {
		t.Fatalf("the second face was accepted: %d\n%s", code, out)
	}
	if stateSize(t, state) != 300 {
		t.Fatal("the state moved")
	}
}

func TestStateMissingAndDamagedTiles(t *testing.T) {
	s := newSigner(1)
	tr := trustOf(t, keySpec{"k", s, ""})
	all := leaves(700)
	state := filepath.Join(testdir.New(t), "state")
	root := testdir.New(t)
	publishLog(t, root, s, all, 300)
	if code, out, _ := run(t, stateArgs(tr, root, state)...); code != 0 {
		t.Fatal(out)
	}
	kept := fileText(t, state)

	// A source with the checkpoint and no tiles: status 3, state unchanged.
	bare := testdir.New(t)
	logDir := filepath.Join(bare, "c", testChain, "e1")
	_ = os.MkdirAll(logDir, 0o755)
	_ = os.WriteFile(filepath.Join(logDir, "checkpoint"), []byte(s.note(logOrigin(), 700, refMTH(all))), 0o644)
	code, out, _ := run(t, stateArgs(tr, bare, state)...)
	if code != 3 || strings.Contains(out, "VERIFIED") || !strings.Contains(out, "INCOMPLETE") || !strings.Contains(out, "state file was not changed") {
		t.Fatalf("no tiles: %d\n%s", code, out)
	}
	if fileText(t, state) != kept {
		t.Fatal("the state moved without a proof")
	}

	// A tile on the proof path changed: status 1, state unchanged.
	damaged := testdir.New(t)
	publishLog(t, damaged, s, all, 700)
	flipped := 0
	_ = filepath.Walk(filepath.Join(damaged, "c"), func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() && strings.Contains(filepath.ToSlash(p), "/tile/") {
			b, _ := os.ReadFile(p)
			b[0] ^= 1
			_ = os.WriteFile(p, b, 0o644)
			flipped++
		}
		return nil
	})
	if flipped == 0 {
		t.Fatal("no tile found")
	}
	code, out, _ = run(t, stateArgs(tr, damaged, state)...)
	if code != 1 || strings.Contains(out, "VERIFIED") {
		t.Fatalf("damaged tiles: %d\n%s", code, out)
	}
	if fileText(t, state) != kept {
		t.Fatal("the state moved over a damaged tile")
	}
}

func TestStateRollbackAndSameSizeFork(t *testing.T) {
	s := newSigner(1)
	tr := trustOf(t, keySpec{"k", s, ""})
	all := leaves(700)
	state := filepath.Join(testdir.New(t), "state")
	big := testdir.New(t)
	publishLog(t, big, s, all, 700)
	if code, out, _ := run(t, stateArgs(tr, big, state)...); code != 0 {
		t.Fatal(out)
	}
	kept := fileText(t, state)
	small := testdir.New(t)
	publishLog(t, small, s, all, 300)
	code, out, _ := run(t, stateArgs(tr, small, state)...)
	if code != 1 || !strings.Contains(out, "FAILED at step rollback") || !strings.Contains(out, ".conflict-300-") || fileText(t, state) != kept {
		t.Fatalf("rollback: %d\n%s", code, out)
	}
	same := testdir.New(t)
	publishLog(t, same, s, leavesSalt(700, 3, "other"), 700)
	code, out, _ = run(t, stateArgs(tr, same, state)...)
	if code != 1 || !strings.Contains(out, "FAILED at step fork") || !strings.Contains(out, ".conflict-700-") || fileText(t, state) != kept {
		t.Fatalf("same size fork: %d\n%s", code, out)
	}
}

func TestStateOfAnotherLogIsRefusedAndKept(t *testing.T) {
	s := newSigner(1)
	tr := trustOf(t, keySpec{"k", s, ""})
	root := testdir.New(t)
	publishLog(t, root, s, leaves(300), 300)
	state := filepath.Join(testdir.New(t), "state")
	other := s.note(testPrefix+"/c/"+testChain+"/e2", 5, refMTH(leaves(5)))
	if err := os.WriteFile(state, []byte(other), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errs := run(t, stateArgs(tr, root, state)...)
	if code != 2 || fileText(t, state) != other || strings.Contains(errs, "move the state file away") {
		t.Fatalf("another log: %d %s", code, errs)
	}
}

// ---- L8-03: a held note and the state of a key ----

func TestHeldNoteAndKeyStates(t *testing.T) {
	k1, k2 := newSigner(1), newSigner(2)
	all := leaves(700)
	oldSrc, newSrc := testdir.New(t), testdir.New(t)
	oldNote := publishLog(t, oldSrc, k1, all, 300)
	publishLog(t, newSrc, k2, all, 700)
	cases := []struct {
		name  string
		extra string
		// want is the status of the three uses: checkpoint --state, consistency
		// --old with a plain note, consistency --old with the state file.
		want [3]int
		say  string
	}{
		{"valid", "", [3]int{0, 0, 0}, ""},
		{"not yet valid", `,"not_before":"2999-01-01T00:00:00Z"`, [3]int{0, 0, 0}, "accepted for"},
		{"expired", `,"not_after":"2020-01-01T00:00:00Z"`, [3]int{0, 0, 0}, "accepted for"},
		{"revoked", `,"revoked":true`, [3]int{0, 2, 0}, "revoked"},
		{"limited below the size", `,"max_tree_size":299`, [3]int{0, 2, 0}, "counts only for tree sizes up to 299"},
		{"limited at the size", `,"max_tree_size":300`, [3]int{0, 0, 0}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// The state is written while the key is valid.
			trOK := trustOf(t, keySpec{"k1", k1, ""}, keySpec{"k2", k2, ""})
			state := filepath.Join(testdir.New(t), "state")
			if code, out, errs := run(t, stateArgs(trOK, oldSrc, state)...); code != 0 {
				t.Fatalf("seed: %d %s %s", code, out, errs)
			}
			kept := fileText(t, state)
			tr := trustOf(t, keySpec{"k1", k1, c.extra}, keySpec{"k2", k2, ""})
			// Two ways to use what the auditor holds: the state, and --old (the
			// state file, and the plain note they kept).
			cons := []string{"consistency", "--trust", tr, "--log", logSpecArg(), "--source", newSrc}
			code2, out2, err2 := run(t, append(cons, "--old", writeNote(t, oldNote))...)
			code3, out3, err3 := run(t, append(cons, "--old", state)...)
			code1, out1, err1 := run(t, stateArgs(tr, newSrc, state)...)
			for i, r := range []struct {
				code int
				text string
			}{{code1, out1 + err1}, {code2, out2 + err2}, {code3, out3 + err3}} {
				if r.code != c.want[i] {
					t.Errorf("%s, use %d: status %d, want %d\n%s", c.name, i, r.code, c.want[i], r.text)
				}
				if c.say != "" && !strings.Contains(r.text, c.say) {
					t.Errorf("%s, use %d: the output does not say %q:\n%s", c.name, i, c.say, r.text)
				}
				if c.want[i] == 2 {
					if strings.Contains(r.text, "move the state file away") || strings.Contains(r.text, "delete it") && !strings.Contains(r.text, "must not be deleted") && i == 0 {
						t.Errorf("%s: the advice gives up the protection:\n%s", c.name, r.text)
					}
				}
			}
			if c.want[0] == 2 && fileText(t, state) != kept {
				t.Errorf("%s: the state changed", c.name)
			}
			if c.want[0] == 0 && stateSize(t, state) != 700 {
				t.Errorf("%s: the state did not advance", c.name)
			}
		})
	}
}

func TestNoteSignedByRevokedAndValidKeySaysSo(t *testing.T) {
	k1, k2 := newSigner(1), newSigner(2)
	root := testdir.New(t)
	all := leaves(300)
	publishLog(t, root, k2, all, 300)
	both := k2.note(logOrigin(), 300, refMTH(all))
	extra := strings.Split(k1.note(logOrigin(), 300, refMTH(all)), "\n\n")[1]
	if err := os.WriteFile(filepath.Join(root, "c", testChain, "e1", "checkpoint"), []byte(both+extra), 0o644); err != nil {
		t.Fatal(err)
	}
	tr := trustOf(t, keySpec{"k1", k1, `,"revoked":true`}, keySpec{"k2", k2, ""})
	code, out, _ := run(t, "checkpoint", "--trust", tr, "--log", logSpecArg(), "--source", root)
	if code != 0 || !strings.Contains(out, "also carries a signature of key") || !strings.Contains(out, "revoked") {
		t.Fatalf("%d\n%s", code, out)
	}
}

// ---- L8-04: two runs, one state ----

// A run that waits the whole lock limit for another one and ends with status 2 and
// nothing changed is the designed answer of a loaded machine (the README says so);
// it is not a failure of this property, which is that the state never moves
// backwards: the state ends at least as high as the highest checkpoint a run was
// told about with status 0 or 1, and at 320 when no run gave up.
func TestStateTwoRunsNeverMoveBackwards(t *testing.T) {
	s := newSigner(1)
	tr := trustOf(t, keySpec{"k", s, ""})
	all := leaves(320)
	root := testdir.New(t)
	var notes []string
	for size := 301; size <= 320; size++ {
		notes = append(notes, publishLog(t, root, s, all, size))
	}
	state := filepath.Join(testdir.New(t), "state")
	publishLog(t, root, s, all, 300)
	if code, out, _ := run(t, stateArgs(tr, root, state)...); code != 0 {
		t.Fatal(out)
	}
	initial := fileText(t, state)
	// a long wait, so that a loaded machine seldom gives up; the runs that do are counted
	oldWait := statefile.LockWait
	statefile.LockWait = 2 * time.Minute
	defer func() { statefile.LockWait = oldWait }()
	gaveUp := 0
	for round := 0; round < 20; round++ {
		if err := os.WriteFile(state, []byte(initial), 0o644); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		files := make([]string, len(notes))
		for i, n := range notes {
			files[i] = writeNote(t, n)
		}
		var mu sync.Mutex
		told, roundGaveUp := 300, 0 // the highest size a run ended with status 0 or 1 for
		for i := range files {
			wg.Add(1)
			go func() {
				defer wg.Done()
				size := 320 - i // the note of the run is files[len-1-i], of size 301+len-1-i
				a := stateArgs(tr, root, state, "--note", files[len(files)-1-i])
				code, out, errs := run(t, a...)
				mu.Lock()
				defer mu.Unlock()
				switch {
				case code == 0 || code == 1:
					if size > told {
						told = size
					}
				case code == 2 && strings.Contains(errs, "has held the lock on the state file for"):
					roundGaveUp++ // the designed answer after a full wait: nothing was changed
				default:
					t.Errorf("status %d: %s %s", code, out, errs)
				}
			}()
		}
		wg.Wait()
		gaveUp += roundGaveUp
		got := stateSize(t, state)
		if got < uint64(told) {
			t.Fatalf("round %d: the state ended at %d, below %d, a size a run was told about: a higher state was lost", round, got, told)
		}
		if roundGaveUp == 0 && got != 320 {
			t.Fatalf("round %d: the state ended at %d, not 320: a higher state was lost", round, got)
		}
	}
	if gaveUp > 0 {
		t.Logf("%d runs waited the whole limit and ended with status 2, which is the designed answer", gaveUp)
	}
}

func TestStateLockWaitIsBounded(t *testing.T) {
	s := newSigner(1)
	tr := trustOf(t, keySpec{"k", s, ""})
	root := testdir.New(t)
	publishLog(t, root, s, leaves(300), 300)
	state := filepath.Join(testdir.New(t), "state")
	if code, out, _ := run(t, stateArgs(tr, root, state)...); code != 0 {
		t.Fatal(out)
	}
	release := holdLock(t, state)
	defer release()
	start := time.Now()
	code, _, errs := run(t, stateArgs(tr, root, state)...)
	if code != 2 || !strings.Contains(errs, "lock") {
		t.Fatalf("a locked state: %d %s", code, errs)
	}
	if time.Since(start) > 30*time.Second {
		t.Fatal("the wait was not bounded")
	}
}

// ---- L8-07 ----

func TestStateWithALongName(t *testing.T) {
	s := newSigner(1)
	tr := trustOf(t, keySpec{"k", s, ""})
	all := leaves(700)
	root := testdir.New(t)
	dir := testdir.New(t)
	publishLog(t, root, s, all, 300)
	long := filepath.Join(dir, strings.Repeat("n", 200))
	if code, out, errs := run(t, stateArgs(tr, root, long)...); code != 0 {
		t.Fatalf("a 200 byte name: %d %s %s", code, out, errs)
	}
}

// The link part is its own test, so that a system that cannot make a link
// reports a skip and not a pass.
func TestStateThroughALink(t *testing.T) {
	s := newSigner(1)
	tr := trustOf(t, keySpec{"k", s, ""})
	all := leaves(700)
	root := testdir.New(t)
	dir := testdir.New(t)
	publishLog(t, root, s, all, 300)
	long := filepath.Join(dir, "first")
	if code, out, errs := run(t, stateArgs(tr, root, long)...); code != 0 {
		t.Fatalf("a state: %d %s %s", code, out, errs)
	}
	target := filepath.Join(dir, "real")
	link := filepath.Join(dir, "link")
	if err := os.WriteFile(target, []byte(fileText(t, long)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("no link can be made here: %v", err)
	}
	publishLog(t, root, s, all, 700)
	if code, out, errs := run(t, stateArgs(tr, root, link)...); code != 0 {
		t.Fatalf("through a link: %d %s %s", code, out, errs)
	}
	if st, _ := os.Lstat(link); st.Mode()&os.ModeSymlink == 0 {
		t.Error("the link was replaced by a file")
	}
	if stateSize(t, target) != 700 {
		t.Error("the target was not updated")
	}
	if st, _ := os.Stat(target); os.PathSeparator == '/' && st.Mode().Perm() != 0o600 {
		t.Errorf("the mode was not kept: %v", st.Mode().Perm())
	}
}

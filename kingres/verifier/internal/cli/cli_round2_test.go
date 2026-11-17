// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/chain"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/statefile"
)

// jsonOf runs a command with --json and returns the object.
func jsonOf(t *testing.T, args ...string) (int, map[string]any) {
	t.Helper()
	code, out, errs := run(t, append(args, "--json")...)
	var o map[string]any
	if err := json.Unmarshal([]byte(out), &o); err != nil {
		t.Fatalf("not JSON (%v): %q %q", err, out, errs)
	}
	return code, o
}

func stateInfo(t *testing.T, o map[string]any) map[string]any {
	t.Helper()
	info, _ := o["info"].(map[string]any)
	st, _ := info["state"].(map[string]any)
	if info == nil || st == nil {
		t.Fatalf("no info.state in %v", o)
	}
	return info
}

// L9-11 (L9, L10): the JSON says what was proved: a first use does not claim
// a proof, an advance does, a fork does not.
func TestJSONSaysWhatWasProved(t *testing.T) {
	x := newExitEnv(t)
	st := filepath.Join(t.TempDir(), "state")
	// first use: nothing was proved, the state was created
	code, o := jsonOf(t, x.cp("--note", x.oldN, "--state", st)...)
	info := stateInfo(t, o)
	s := info["state"].(map[string]any)
	if code != 0 || info["extends_state"] != false || s["extension_proved"] != false || s["action"] != "created" {
		t.Errorf("first use: %d %v", code, info)
	}
	// advance: proved
	code, o = jsonOf(t, x.cp("--state", st)...)
	info = stateInfo(t, o)
	s = info["state"].(map[string]any)
	if code != 0 || info["extends_state"] != true || s["extension_proved"] != true || s["action"] != "advanced" {
		t.Errorf("advance: %d %v", code, info)
	}
	// the same again: nothing to prove, and that is what it says
	code, o = jsonOf(t, x.cp("--state", st)...)
	info = stateInfo(t, o)
	s = info["state"].(map[string]any)
	if code != 0 || info["extends_state"] != true || s["action"] != "unchanged" {
		t.Errorf("same: %d %v", code, info)
	}
	// a fork: not proved, kept
	dir, _ := x.forkSource()
	code, o = jsonOf(t, "checkpoint", "--trust", x.p.trust, "--log", x.p.log(), "--source", dir, "--state", st)
	info = stateInfo(t, o)
	s = info["state"].(map[string]any)
	if code != 1 || info["extends_state"] != false || s["extension_proved"] != false || s["action"] != "kept" || s["evidence_saved"] != true {
		t.Errorf("fork: %d %v", code, info)
	}
}

// L9-11 (S6): a stored tree of one leaf is not the empty tree: it moves only
// with a proof.
func TestStateOfSizeOneMovesOnlyWithAProof(t *testing.T) {
	s := newSigner(1)
	tr := trustOf(t, keySpec{"k", s, ""})
	honest := leaves(300)
	state := filepath.Join(t.TempDir(), "state")
	one := t.TempDir()
	publishLog(t, one, s, honest, 1)
	if code, out, errs := run(t, stateArgs(tr, one, state)...); code != 0 {
		t.Fatalf("size 1: %d %s %s", code, out, errs)
	}
	kept := fileText(t, state)
	// a history whose first leaf is another: the stored size 1 is not its prefix
	fork := t.TempDir()
	publishLog(t, fork, s, leavesSalt(300, 0, "other"), 300)
	code, out, _ := run(t, stateArgs(tr, fork, state)...)
	if code != 1 || strings.Contains(out, "VERIFIED") || !strings.Contains(out, "FAILED at step") || fileText(t, state) != kept {
		t.Fatalf("a fork of a tree of one leaf moved the state: %d\n%s", code, out)
	}
	// the honest growth reads tiles and proves it
	real := t.TempDir()
	publishLog(t, real, s, honest, 300)
	code, out, _ = run(t, stateArgs(tr, real, state)...)
	if code != 0 || !strings.Contains(out, "tile(s)") || strings.Contains(out, "the empty tree") || stateSize(t, state) != 300 {
		t.Fatalf("honest growth from one leaf: %d\n%s", code, out)
	}
}

// L9-11 (S23 to S25) and L9-15: what a state file must be. Each damage is
// refused by every command that holds it, the file stays as it was, and the
// advice never says to delete it.
func TestStateRecordStrictness(t *testing.T) {
	x := newExitEnv(t)
	good := x.stateAt300()
	record := fileText(t, good)
	mut := func(f func(string) string) string { return f(record) }
	cases := []struct{ name, content, say string }{
		{"text after the record", record + "x\n", "text after"},
		{"a second record", record + record, "text after"},
		{"an unknown member", mut(func(s string) string { return strings.Replace(s, `"log"`, `"extra": 1,`+"\n  "+`"log"`, 1) }), "unknown member"},
		{"another format", mut(func(s string) string { return strings.Replace(s, `"format": 1`, `"format": 2`, 1) }), "format 2"},
		{"format zero", mut(func(s string) string { return strings.Replace(s, `"format": 1`, `"format": 0`, 1) }), "format 0"},
		{"a duplicate member", mut(func(s string) string { return strings.Replace(s, `"size"`, `"format": 1,`+"\n  "+`"size"`, 1) }), "appears twice"},
		{"a missing member", mut(func(s string) string { return strings.Replace(s, `"verified_by"`, `"verified_by_x"`, 1) }), "unknown member"},
		{"a wrong type", mut(func(s string) string { return strings.Replace(s, `"size": 300`, `"size": "300"`, 1) }), "member \"size\""},
		{"a size that is not the note's", mut(func(s string) string { return strings.Replace(s, `"size": 300`, `"size": 301`, 1) }), "not those of the note"},
		{"a root that is not the note's", mut(func(s string) string {
			i := strings.Index(s, `"root": "`) + len(`"root": "`)
			return s[:i] + strings.Repeat("0", 64) + s[i+64:]
		}), "not those of the note"},
		{"a time that is not a time", mut(func(s string) string {
			i := strings.Index(s, `"verified_at": "`) + len(`"verified_at": "`)
			j := strings.Index(s[i:], `"`)
			return s[:i] + "yesterday" + s[i+j:]
		}), "verified_at"},
		{"an empty file", "", "is empty"},
		{"an object that is not a record", "{}", "missing"},
		{"a list", "[1]", "note"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := writeTemp(t, t.TempDir(), "state", c.content)
			for _, args := range [][]string{x.cp("--state", f), x.co("--old", f)} {
				code, out, errs := run(t, args...)
				if code != 2 || strings.Contains(out, "VERIFIED") || strings.Contains(out, "CONSISTENT") {
					t.Fatalf("%v: status %d\n%s%s", args[0], code, out, errs)
				}
				if !strings.Contains(errs, c.say) {
					t.Errorf("%v: the message does not say %q:\n%s", args[0], c.say, errs)
				}
				if strings.Contains(errs, "move the state file away") || strings.Contains(errs, "delete it") && !strings.Contains(errs, "must not be deleted") {
					t.Errorf("%v: the advice gives up the protection:\n%s", args[0], errs)
				}
			}
			if fileText(t, f) != c.content {
				t.Error("the file was changed")
			}
		})
	}
	// A record with only the layout changed is the same record.
	spaced := strings.TrimSuffix(record, "\n") + "\n\n  \n"
	f := writeTemp(t, t.TempDir(), "state", spaced)
	if code, out, errs := run(t, x.cp("--state", f)...); code != 0 {
		t.Errorf("white space after the record: %d %s %s", code, out, errs)
	}
}

// L9-15: an empty state file is refused, and the advice keeps the protection.
func TestEmptyStateFileIsNotNoState(t *testing.T) {
	x := newExitEnv(t)
	f := writeTemp(t, t.TempDir(), "state", "")
	code, out, errs := run(t, x.cp("--state", f)...)
	if code != 2 || strings.Contains(out, "VERIFIED") {
		t.Fatalf("%d %s %s", code, out, errs)
	}
	for _, want := range []string{"is empty", "no checkpoint", "restore your copy", "NEWFILE --min-size"} {
		if !strings.Contains(errs, want) {
			t.Errorf("the message does not say %q:\n%s", want, errs)
		}
	}
	if fileText(t, f) != "" {
		t.Error("the empty file was filled")
	}
}

// L9-11 (B2) and the question of what an export that ends early says.
func TestBlocksExportThatEndsEarly(t *testing.T) {
	all, err := os.ReadFile(fx("blocks.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(strings.TrimSuffix(string(all), "\n"), "\n")
	d, _ := os.ReadFile(fx("digests/d8.json"))
	dg, err := chain.ParseDigest(d)
	if err != nil {
		t.Fatal(err)
	}
	cid := fmt.Sprintf("%x", dg.ChainID)
	cut := writeTemp(t, t.TempDir(), "cut.jsonl", strings.Join(lines[:4], ""))
	// without a digest the prefix is a valid chain, and the output says exactly
	// that: how far it goes, and that nothing shows it is the whole chain
	code, out, _ := run(t, "blocks", "--blocks", cut, "--chain-id", cid)
	if code != 0 {
		t.Fatalf("a prefix: %d %s", code, out)
	}
	for _, want := range []string{"the 4 block headers read (heights 0 to 3)", "that the export is complete", "ends at block 3", "cut at any earlier block verifies the same way"} {
		if !strings.Contains(out, want) {
			t.Errorf("the output does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "the exported block headers form an unbroken chain") {
		t.Errorf("the headline still claims the whole export:\n%s", out)
	}
	code, o := jsonOf(t, "blocks", "--blocks", cut, "--chain-id", cid)
	info, _ := o["info"].(map[string]any)
	if code != 0 || info["head_height"] != float64(3) || info["complete_export_shown"] != false || info["blocks"] != float64(4) {
		t.Errorf("json: %d %v", code, info)
	}
	// with a digest above the cut, the cut is a failure
	if code, out, _ := run(t, "blocks", "--blocks", cut, "--digest", fx("digests/d8.json")); code != 1 || !strings.Contains(out, "FAILED at step digest") {
		t.Errorf("a cut before the digest: %d %s", code, out)
	}
	// with a digest inside the cut, the output says what lies beyond it
	code, out, _ = run(t, "blocks", "--blocks", cut, "--digest", fx("digests/d0.json"))
	if code != 0 || !strings.Contains(out, "the digest covers up to height 0") {
		t.Errorf("a digest inside the cut: %d %s", code, out)
	}
	// a valid prefix followed by a line too long to read is not verified up to the
	// cut: it is a failure at the step parse
	long := writeTemp(t, t.TempDir(), "long.jsonl", strings.Join(lines[:4], "")+strings.Repeat("x", 2<<20)+"\n")
	code, out, _ = run(t, "blocks", "--blocks", long, "--chain-id", cid)
	if code != 1 || strings.Contains(out, "VERIFIED") || !strings.Contains(out, "FAILED at step parse") {
		t.Errorf("a long line after a prefix: %d %.300s", code, out)
	}
	// ...and a prefix followed by a line that is cut in the middle
	half := writeTemp(t, t.TempDir(), "half.jsonl", strings.Join(lines[:4], "")+lines[4][:60])
	code, out, _ = run(t, "blocks", "--blocks", half, "--chain-id", cid)
	if code != 1 || strings.Contains(out, "VERIFIED") {
		t.Errorf("a half line after a prefix: %d %.300s", code, out)
	}
}

// L9-11 (E7): the time of the key windows is printed with its fraction.
func TestFixedTimeIsPrintedWithItsFraction(t *testing.T) {
	x := newExitEnv(t)
	for _, now := range []string{"2026-12-01T00:00:00.5Z", "2026-12-01T00:00:00.123456789Z", "2026-12-01T00:00:00Z"} {
		code, out, _ := run(t, x.cp("--now", now)...)
		if code != 0 || !strings.Contains(out, "time used for key validity windows: "+now+", fixed by --now") {
			t.Errorf("%s: %d\n%s", now, code, out)
		}
	}
}

// L9-11 (L3, L8): the alert sentence of the help, and the caveat of an advance.
func TestAlertSentenceAndTheCaveatOfAnAdvance(t *testing.T) {
	for _, cmd := range []string{"checkpoint", "consistency"} {
		if _, out, _ := run(t, cmd, "--help"); !strings.Contains(out, `Treat 3 as an alert, never as "probably fine"`) {
			t.Errorf("%s --help does not tell that 3 is not a pass:\n%s", cmd, out)
		}
	}
	x := newExitEnv(t)
	st := x.stateAt300()
	_, out, _ := run(t, x.cp("--state", st)...)
	if !strings.Contains(out, "a damaged tile elsewhere is not noticed") {
		t.Errorf("the advance does not say that only the proof path was read:\n%s", out)
	}
}

// L9-07: the evidence of a fork is claimed saved only when it is.
func TestConflictEvidenceClaims(t *testing.T) {
	x := newExitEnv(t)
	st := x.stateAt300()
	dir, root := x.forkSource()
	args := []string{"checkpoint", "--trust", x.p.trust, "--log", x.p.log(), "--source", dir, "--state", st}
	code, out, _ := run(t, args...)
	if code != 1 || !strings.Contains(out, "was saved to") {
		t.Fatalf("first fork: %d\n%s", code, out)
	}
	saved := statefile.ConflictPath(st, 400, root[:])
	want := fileText(t, filepath.Join(dir, "c", testChain, "e1", "checkpoint"))
	if fileText(t, saved) != want {
		t.Fatal("the evidence file is not the note")
	}
	// the same fork again: already saved, and the output says so
	code, out, _ = run(t, args...)
	if code != 1 || !strings.Contains(out, "was already saved to") {
		t.Fatalf("second run: %d\n%s", code, out)
	}
	// another file at that name: the claim moves to a name that holds the note
	if err := os.WriteFile(saved, []byte("PLANTED, not the note"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ = run(t, args...)
	if code != 1 || strings.Contains(out, "already saved to") || !strings.Contains(out, "-2") {
		t.Fatalf("a planted file: %d\n%s", code, out)
	}
	if fileText(t, saved) != "PLANTED, not the note" {
		t.Error("the planted file was overwritten")
	}
	if fileText(t, saved+"-2") != want {
		t.Error("the evidence is not in the second name")
	}
	// every name taken: not saved, a status of its own, and the output says NOT
	st2 := x.stateAt300()
	takeNames(t, st2, 400, root)
	args[len(args)-1] = st2
	code, out, _ = run(t, args...)
	if code != ExitEvidenceLost || !strings.Contains(out, "FAILED at step fork") || !strings.Contains(out, "was NOT saved") || strings.Contains(out, "was saved to") {
		t.Fatalf("names taken: %d\n%s", code, out)
	}
	code, o := jsonOf(t, args...)
	s, _ := o["info"].(map[string]any)["state"].(map[string]any)
	if code != ExitEvidenceLost || s["evidence_saved"] != false || s["conflict_file"] != nil {
		t.Errorf("json: %d %v", code, s)
	}
}

// L9-08: the state must be in a directory that only its owner can write.
func TestSharedStateDirectoryIsRefusedUnlessAccepted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the permissions of a Windows folder are not read")
	}
	x := newExitEnv(t)
	d := t.TempDir()
	if err := os.Chmod(d, 0o775); err != nil {
		t.Fatal(err)
	}
	st := filepath.Join(d, "state")
	code, out, errs := run(t, x.cp("--state", st)...)
	if code != 2 || strings.Contains(out, "VERIFIED") || !strings.Contains(errs, "group or others") || !strings.Contains(errs, "--allow-shared-state-dir") {
		t.Fatalf("a group writable directory: %d\n%s%s", code, out, errs)
	}
	if _, err := os.Stat(st); err == nil {
		t.Fatal("a state was written in a shared directory")
	}
	if _, err := os.Stat(filepath.Join(d, ".kingres-state.lock")); err == nil {
		t.Fatal("a lock was made in a shared directory")
	}
	code, out, errs = run(t, x.cp("--state", st, "--allow-shared-state-dir")...)
	if code != 0 || !strings.Contains(out, "accepted by --allow-shared-state-dir") {
		t.Fatalf("accepted: %d\n%s%s", code, out, errs)
	}
}

// L9-08: a link at the name of the lock is not followed.
func TestLockThroughALinkIsRefusedAndNothingIsCreated(t *testing.T) {
	x := newExitEnv(t)
	d := t.TempDir()
	target := filepath.Join(t.TempDir(), "created")
	if err := os.Symlink(target, filepath.Join(d, ".kingres-state.lock")); err != nil {
		t.Skipf("no link here: %v", err)
	}
	code, out, errs := run(t, x.cp("--state", filepath.Join(d, "state"))...)
	if code != 2 || strings.Contains(out, "VERIFIED") || !strings.Contains(errs, "link") {
		t.Fatalf("%d\n%s%s", code, out, errs)
	}
	if _, err := os.Lstat(target); err == nil {
		t.Fatal("a file was created where the link points")
	}
}

// L9-06: a digest directory is read by the names the listing gives, and the
// export command of the README makes names that every system accepts.
func TestDigestDirectoryNames(t *testing.T) {
	d := t.TempDir()
	names := []string{"digest-2026-11-14T17-21-53Z.json", "with space.json", "-leading-dash.json"}
	if runtime.GOOS != "windows" {
		names = append(names, "digest-2026-11-14T17:21:54Z.json", `back\slash.json`)
	}
	for i, n := range names {
		copyTo(t, fx(fmt.Sprintf("digests/d%d.json", i)), filepath.Join(d, n))
	}
	code, out, errs := run(t, "digest-chain", "--digests", d)
	if code != 0 || !strings.Contains(out, fmt.Sprintf("%d digests agree", len(names))) {
		t.Fatalf("%d\n%s%s", code, out, errs)
	}
}

func TestReadmeExportCommandMakesAReadableDirectory(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh here: the command of the README is a shell command")
	}
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	var cmdline string
	for _, l := range strings.Split(string(readme), "\n") {
		if strings.HasPrefix(l, "psql -X -At -c \"SELECT kingres.ledger_generate_digest()\" > ") {
			cmdline = l
		}
	}
	if cmdline == "" {
		t.Fatal("the export command is not in the README")
	}
	// The part before the redirect needs a database; what a digest looks like is
	// the fixture. The redirect and the name are the command's own text.
	redirect := cmdline[strings.Index(cmdline, " > ")+3:]
	d := t.TempDir()
	for i := 0; i < 2; i++ {
		abs, _ := filepath.Abs(fx(fmt.Sprintf("digests/d%d.json", i)))
		script := fmt.Sprintf("cat %q > %s", filepath.ToSlash(abs), redirect)
		cmd := exec.Command(sh, "-c", script)
		cmd.Dir = d
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("running %q: %v %s", script, err, out)
		}
		time.Sleep(1100 * time.Millisecond)
	}
	ents, _ := os.ReadDir(d)
	if len(ents) != 2 {
		t.Fatalf("the command made %d files, want 2", len(ents))
	}
	for _, e := range ents {
		if strings.ContainsAny(e.Name(), `:\/*?"<>|`) {
			t.Errorf("the command makes the name %q, which a Windows file system cannot hold", e.Name())
		}
	}
	code, out, errs := run(t, "digest-chain", "--digests", d)
	if code != 0 || !strings.Contains(out, "2 digests agree") {
		t.Fatalf("%d\n%s%s", code, out, errs)
	}
}

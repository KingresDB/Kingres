// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
)

// From a mutation run of cli_tlog.go: what checkpoint and consistency print for
// a success (steps, notes, lines of "not proved", the members of info), the
// epochs above 9, an aggregate log, --now, and the SIZE:ROOT form of --old.

func notesOf(o map[string]any) []string {
	l, _ := o["notes"].([]any)
	var out []string
	for _, x := range l {
		out = append(out, fmt.Sprint(x))
	}
	return out
}

func countContaining(list []string, sub string) int {
	n := 0
	for _, s := range list {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

func TestCheckpointOutputListsWhatItDid(t *testing.T) {
	p := newPublication(t, 300, 700)
	base := []string{"checkpoint", "--trust", p.trust, "--log", p.log(), "--source", p.dir}
	code, o := jsonOf(t, base...)
	steps := stepsOf(o)
	if code != 0 || len(steps) != 4 || steps["parse"] != "the note is well formed" ||
		steps["signature"] != "verified by key "+p.signer.fp()[:16]+" of the trust file" ||
		steps["origin"] != p.origin || !strings.Contains(steps["checkpoint"], "tree size 700, root ") {
		t.Fatalf("steps: %d %v", code, steps)
	}
	np := listOf(o, "not_proved")
	for _, want := range []string{"that this is the latest checkpoint", "the tiles and entries of the log", "when the checkpoint was made", "that the signer shows the same history", "that the trust file was obtained"} {
		if !strings.Contains(np, want) {
			t.Errorf("not proved does not say %q:\n%s", want, np)
		}
	}
	_, out, _ := run(t, base...)
	if !strings.HasPrefix(out, "VERIFIED: the checkpoint of "+p.origin+" at size 700 is signed by a trusted key.") {
		t.Errorf("the headline: %.200s", out)
	}
	// --min-size is a step when it is given and a bound that is met
	code, o = jsonOf(t, append(base, "--min-size", "700")...)
	if code != 0 || stepsOf(o)["min-size"] != "700 is at least 700" {
		t.Errorf("min-size 700: %d %v", code, stepsOf(o))
	}
	if _, has := stepsOf(jsonMust(t, base...))["min-size"]; has {
		t.Error("a min-size step without --min-size")
	}
	// the empty tree verifies with no --min-size: the default bound is 0
	empty := writeNote(t, emptyTreeNote(p.signer))
	if code, out, errs := run(t, "checkpoint", "--trust", p.trust, "--log", p.log(), "--note", empty); code != 0 {
		t.Errorf("a checkpoint of size 0: %d\n%s%s", code, out, errs)
	}
	// extension lines are signed text that has no meaning here, and are said to be there
	text := fmt.Sprintf("%s\n700\n%s\next one\next two\n", p.origin, rootOf(p.all, 700))
	ext := writeNote(t, p.signer.noteText(text, 0))
	code, o = jsonOf(t, "checkpoint", "--trust", p.trust, "--log", p.log(), "--note", ext)
	info, _ := o["info"].(map[string]any)
	if code != 0 || info["extension_lines"] != float64(2) || countContaining(notesOf(o), "the checkpoint has 2 extension line(s)") != 1 {
		t.Errorf("extension lines: %d %v %v", code, info, notesOf(o))
	}
	if code, o := jsonOf(t, base...); code != 0 || (o["info"].(map[string]any))["extension_lines"] != float64(0) || countContaining(notesOf(o), "extension line") != 0 {
		t.Errorf("no extension lines: %v", notesOf(o))
	}
}

func jsonMust(t *testing.T, args ...string) map[string]any {
	t.Helper()
	_, o := jsonOf(t, args...)
	return o
}

func TestConsistencyOutputListsWhatItDid(t *testing.T) {
	p := newPublication(t, 300, 700)
	base := []string{"consistency", "--trust", p.trust, "--log", p.log(), "--source", p.dir}
	oldNote := filepath.Join(p.logDir, "old.note")
	code, o := jsonOf(t, append(base, "--old", oldNote)...)
	steps := stepsOf(o)
	info, _ := o["info"].(map[string]any)
	if code != 0 || !strings.Contains(steps["new-checkpoint"], "size 700, signed by key "+p.signer.fp()[:16]) || steps["old-checkpoint"] != "size 300, signature verified" ||
		steps["sizes"] != "old 300, new 700" || !strings.Contains(steps["tiles"], "proof hash(es)") || !strings.Contains(steps["consistency"], "RFC 9162 2.1.4.2: both roots follow") {
		t.Fatalf("steps: %d %v", code, steps)
	}
	if info["old_size"] != float64(300) || info["new_size"] != float64(700) || info["old_root"] == nil || info["new_root"] == nil {
		t.Errorf("info: %v", info)
	}
	if countContaining(notesOf(o), "time used for key validity windows") != 1 {
		t.Errorf("notes: %v", notesOf(o))
	}
	np := listOf(o, "not_proved")
	if !strings.Contains(np, "that the entries behind the hashes") || !strings.Contains(np, "that nobody else was shown a different history") || strings.Contains(np, "the checkpoint you meant") {
		t.Errorf("not proved:\n%s", np)
	}
	// a state file as the old checkpoint is said to be one
	state := filepath.Join(testdir.New(t), "state")
	if code, out, errs := run(t, "checkpoint", "--trust", p.trust, "--log", p.log(), "--source", p.dir, "--note", oldNote, "--state", state); code != 0 {
		t.Fatalf("%d %s %s", code, out, errs)
	}
	if _, o := jsonOf(t, append(base, "--old", state)...); countContaining(notesOf(o), "--old is a state file, recorded at ") != 1 {
		t.Errorf("a state file: %v", notesOf(o))
	}
	// SIZE:ROOT: given without a signature, and said to be
	r300 := refMTH(p.all[:300])
	root300 := base64.StdEncoding.EncodeToString(r300[:])
	code, o = jsonOf(t, append(base, "--old", "300:"+root300)...)
	if code != 0 || !strings.Contains(stepsOf(o)["old-checkpoint"], "root given on the command line (not authenticated)") || !strings.Contains(listOf(o, "not_proved"), "that the old size and root are the checkpoint you meant") {
		t.Errorf("SIZE:ROOT: %d %v %s", code, stepsOf(o), listOf(o, "not_proved"))
	}
	// the same size: an empty proof, and the headline says the same
	r700 := refMTH(p.all[:700])
	root700 := base64.StdEncoding.EncodeToString(r700[:])
	code, o = jsonOf(t, append(base, "--old", "700:"+root700)...)
	if code != 0 || !strings.Contains(stepsOf(o)["consistency"], "both checkpoints have size 700 and the same root") || !strings.Contains(stepsOf(o)["tiles"], "no tile was needed") {
		t.Errorf("equal sizes: %d %v", code, stepsOf(o))
	}
	_, out, _ := run(t, append(base, "--old", "700:"+root700)...)
	if !strings.Contains(out, "the log at size 700 is the same as the log at size 700") {
		t.Errorf("the headline of equal sizes:\n%.300s", out)
	}
	// from the empty tree
	empty := base64.StdEncoding.EncodeToString(sha32(nil))
	code, o = jsonOf(t, append(base, "--old", "0:"+empty)...)
	if code != 0 || !strings.Contains(stepsOf(o)["consistency"], "the empty tree is a prefix of every tree") {
		t.Errorf("from the empty tree: %d %v", code, stepsOf(o))
	}
	// a SIZE:ROOT that is not one: too large a size, a root that is not canonical base64, a root of the wrong length
	for name, arg := range map[string]string{
		"size too large":               "4611686018427387904:" + root300,
		"short root":                   "300:" + base64.StdEncoding.EncodeToString(make([]byte, 31)),
		"long root":                    "300:" + base64.StdEncoding.EncodeToString(make([]byte, 33)),
		"bad base64":                   "300:" + strings.Repeat("A", 43) + "B=",
		"empty tree with another root": "0:" + root300,
	} {
		if code, _, errs := run(t, append(base, "--old", arg)...); code != 2 || errs == "" {
			t.Errorf("--old %s: status %d %s", name, code, errs)
		}
	}
	// when both notes carry a signature of a key that is not counted, the note about it is there once
	k2 := newSigner(2)
	two := trustOf(t, keySpec{"k1", p.signer, `,"revoked":true`}, keySpec{"k2", k2, ""})
	root := testdir.New(t)
	publishNote(t, root, p.all, 700, signedBy(700, p.all, []signer{p.signer, k2}))
	oldTwo := writeNote(t, signedBy(300, p.all, []signer{p.signer, k2}))
	code, o = jsonOf(t, "consistency", "--trust", two, "--log", logSpecArg(), "--source", root, "--old", oldTwo)
	if code != 0 || countContaining(notesOf(o), "also carries a signature of key") != 1 {
		t.Errorf("a revoked signer on both notes: %d %v", code, notesOf(o))
	}
}

func sha32(b []byte) []byte {
	h := sha(b)
	return h[:]
}

// An epoch above 9 is read, written and looked up as the number it is (a number
// read in base 11 or written in another base would look for another directory).
func TestEpochsAboveNine(t *testing.T) {
	k := newSigner(1)
	all := leaves(300)
	tr := trustOf(t, keySpec{"k", k, ""})
	for _, epoch := range []string{"10", "11", "99", "123456789"} {
		root := testdir.New(t)
		origin := testPrefix + "/c/" + testChain + "/e" + epoch
		logDir := filepath.Join(root, "c", testChain, "e"+epoch)
		writeTiles(t, logDir, all, 300)
		if err := os.WriteFile(filepath.Join(logDir, "checkpoint"), []byte(k.note(origin, 300, refMTH(all))), 0o644); err != nil {
			t.Fatal(err)
		}
		// named, and found because it is the only one
		for _, log := range []string{"chain:" + testChain + ":e" + epoch, "chain:" + testChain} {
			code, out, errs := run(t, "checkpoint", "--trust", tr, "--log", log, "--source", root)
			if code != 0 || !strings.Contains(out, "at size 300") {
				t.Errorf("epoch %s, --log %s: %d\n%s%s", epoch, log, code, out, errs)
			}
		}
	}
}

// An aggregate log has its own directory and no epoch.
func TestAggregateLogIsRead(t *testing.T) {
	k := newSigner(1)
	all := leaves(300)
	tr := trustOf(t, keySpec{"k", k, ""})
	id := strings.Repeat("cd", 32)
	root := testdir.New(t)
	origin := testPrefix + "/a/" + id
	logDir := filepath.Join(root, "a", id)
	writeTiles(t, logDir, all, 300)
	if err := os.WriteFile(filepath.Join(logDir, "checkpoint"), []byte(k.note(origin, 300, refMTH(all))), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errs := run(t, "checkpoint", "--trust", tr, "--log", "aggregate:"+id, "--source", root)
	if code != 0 || !strings.Contains(out, origin) {
		t.Errorf("an aggregate log: %d\n%s%s", code, out, errs)
	}
	if code, _, errs := run(t, "checkpoint", "--trust", tr, "--log", "aggregate:"+id+":e1", "--source", root); code != 2 || !strings.Contains(errs, "no epoch") {
		t.Errorf("an aggregate log with an epoch: %d %s", code, errs)
	}
	// and with a state: the same, through --note as well
	if code, out, errs := run(t, "checkpoint", "--trust", tr, "--log", "aggregate:"+id, "--note", filepath.Join(logDir, "checkpoint")); code != 0 {
		t.Errorf("--note of an aggregate log: %d\n%s%s", code, out, errs)
	}
}

func TestNowFlagMessages(t *testing.T) {
	p := newPublication(t, 300, 700)
	base := []string{"checkpoint", "--trust", p.trust, "--log", p.log(), "--source", p.dir}
	for _, c := range []struct{ now, want string }{
		{"", "--now is empty"},
		{"2026-01-01T00:00:00,5Z", "uses a point, not a comma"},
		{"yesterday", "is not an RFC 3339 time"},
		{"2026-01-01 00:00:00", "is not an RFC 3339 time"},
	} {
		code, _, errs := run(t, append(base, "--now", c.now)...)
		if code != 2 || !strings.Contains(errs, c.want) {
			t.Errorf("--now %q: %d %s", c.now, code, errs)
		}
	}
	// a point is fine, and the time is printed with its fraction
	code, out, _ := run(t, append(base, "--now", "2026-01-01T00:00:00.5Z")...)
	if code != 0 || !strings.Contains(out, "2026-01-01T00:00:00.5Z") {
		t.Errorf("a fraction: %d\n%.300s", code, out)
	}
}

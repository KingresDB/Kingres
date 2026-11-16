// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// runWithin runs a command and fails the test when it takes a second or more:
// a blocked open is a bug that must show as a failure and not as a hang.
func runWithin(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	type res struct {
		code     int
		out, err string
	}
	ch := make(chan res, 1)
	go func() {
		c, o, e := run(t, args...)
		ch <- res{c, o, e}
	}()
	select {
	case r := <-ch:
		return r.code, r.out, r.err
	case <-time.After(time.Second):
		t.Fatalf("%v did not return within a second", args)
		return 0, "", ""
	}
}

// trustWith writes a trust file for the publication's signer with extra
// members in the key object.
func trustWith(t *testing.T, p *publication, extra string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "trust.json")
	body := fmt.Sprintf(`{"format":1,"origin_prefix":%q,"keys":[{"name":"k","public_key":%q,"fingerprint":%q%s}]}`,
		testPrefix, base64.StdEncoding.EncodeToString(p.signer.pub), p.signer.fp(), extra)
	if err := os.WriteFile(f, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

// ---- L7-02: one structure for both formats ----

var (
	stepLine = regexp.MustCompile(`^  ok  (\S+)\s+(.*)$`)
)

// lines extracts what the text output says, in the groups the JSON has.
func textLines(out string) (steps, notes, notProved, missing, problems []string) {
	for _, l := range strings.Split(out, "\n") {
		switch {
		case stepLine.MatchString(l):
			m := stepLine.FindStringSubmatch(l)
			steps = append(steps, m[1]+"|"+strings.TrimSpace(m[2]))
		case strings.HasPrefix(l, "  note: "):
			notes = append(notes, strings.TrimPrefix(l, "  note: "))
		case strings.HasPrefix(l, "  not proved: "):
			notProved = append(notProved, strings.TrimPrefix(l, "  not proved: "))
		case strings.HasPrefix(l, "  missing: "):
			missing = append(missing, strings.TrimPrefix(l, "  missing: "))
		case strings.HasPrefix(l, "  - "):
			problems = append(problems, strings.TrimPrefix(l, "  - "))
		}
	}
	return
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// checkParity runs a command in both formats and fails when a line exists in
// one and not in the other (L7-02 (1)).
func checkParity(t *testing.T, name string, wantCode int, args ...string) {
	t.Helper()
	checkParityReset(t, name, wantCode, nil, args...)
}

// checkParityReset is checkParity with a function that puts the world back
// between the two runs (a state file that the first run created).
func checkParityReset(t *testing.T, name string, wantCode int, reset func(), args ...string) {
	t.Helper()
	code, text, _ := run(t, args...)
	if reset != nil {
		reset()
	}
	jcode, js, _ := run(t, append(append([]string{}, args...), "--json")...)
	if code != wantCode || jcode != wantCode {
		t.Fatalf("%s: exit %d (text) and %d (json), want %d\n%s\n%s", name, code, jcode, wantCode, text, js)
	}
	var o jsonOutput
	if err := json.Unmarshal([]byte(js), &o); err != nil {
		t.Fatalf("%s: %v\n%s", name, err, js)
	}
	steps, notes, notProved, missing, problems := textLines(text)
	var jsteps []string
	for _, s := range o.Steps {
		if s.OK {
			jsteps = append(jsteps, s.Name+"|"+strings.TrimSpace(s.Detail))
		}
	}
	for what, pair := range map[string][2][]string{
		"steps": {steps, jsteps}, "notes": {notes, o.Notes}, "not proved": {notProved, o.NotProved},
		"missing": {missing, o.Missing}, "problems": {problems, o.Problems},
	} {
		if !equal(pair[0], pair[1]) {
			t.Errorf("%s: %s differ between formats\ntext: %q\njson: %q", name, what, pair[0], pair[1])
		}
	}
	if len(notes)+len(notProved)+len(missing)+len(problems) == 0 && (wantCode == 0 || wantCode == 3) {
		// every result of these commands says something about its limits
		t.Errorf("%s: no note, no not-proved item, no missing item and no problem in the output", name)
	}
}

func TestTextAndJSONCarryTheSameLines(t *testing.T) {
	p := newPublication(t, 300, 700)
	old := filepath.Join(p.logDir, "old.note")
	limited := trustWith(t, p, `,"max_tree_size":700`)
	retiring := trustWith(t, p, `,"not_after":"2030-01-01T00:00:00Z"`)
	src := []string{"--trust", p.trust, "--log", p.log(), "--source", p.dir}

	checkParity(t, "checkpoint", 0, append([]string{"checkpoint"}, src...)...)
	checkParity(t, "checkpoint fixed time", 0, append([]string{"checkpoint", "--now", "2026-12-01T00:00:00Z"}, src...)...)
	checkParity(t, "checkpoint key notes", 0, "checkpoint", "--trust", limited, "--log", p.log(), "--source", p.dir, "--now", "2026-12-01T00:00:00Z")
	checkParity(t, "checkpoint window", 0, "checkpoint", "--trust", retiring, "--log", p.log(), "--source", p.dir, "--now", "2026-12-01T00:00:00Z")
	state := filepath.Join(t.TempDir(), "state")
	checkParityReset(t, "checkpoint state created", 0, func() { _ = os.Remove(state) }, append([]string{"checkpoint", "--state", state}, src...)...)
	_ = os.Remove(state)
	_, _, _ = run(t, append([]string{"checkpoint", "--state", state}, src...)...)
	checkParity(t, "checkpoint state same", 0, append([]string{"checkpoint", "--state", state}, src...)...)
	checkParity(t, "checkpoint failed", 1, append([]string{"checkpoint", "--min-size", "9999"}, src...)...)
	checkParity(t, "consistency", 0, append([]string{"consistency", "--old", old}, src...)...)
	checkParity(t, "consistency no tile", 0, append([]string{"consistency", "--old", filepath.Join(p.logDir, "checkpoint")}, src...)...)
	checkParity(t, "consistency failed", 1, append([]string{"consistency", "--old", "300:" + base64.StdEncoding.EncodeToString(make([]byte, 32))}, src...)...)
	checkParity(t, "trust show", 0, "trust", "show", "--trust", p.trust)
	checkParity(t, "trust show enforced members", 0, "trust", "show", "--trust", limited)
	checkParity(t, "trust show unusable", 2, "trust", "show", "--trust", trustWith(t, p, `,"revoked":true`))
	checkParity(t, "proof", 0, "proof", "--proof", fx("proofs/itm1_v1_d.json"), "--digest", fx("digests/d8.json"))
	checkParity(t, "proof failed", 1, "proof", "--proof", fx("proofs/itm1_v1_d.json"), "--digest", fx("digests/d8.json"), "--op", "I")
	checkParity(t, "digest-chain", 0, "digest-chain", "--digests", fx("digests/d0.json"), "--digests", fx("digests/d8.json"))
	checkParity(t, "digest-chain problems", 1, "digest-chain", "--digests", fx("digests/d8.json"), "--digests", fx("digests/other_chain.json"))
	checkParity(t, "blocks", 0, "blocks", "--blocks", fx("blocks.jsonl"), "--digest", fx("digests/d8.json"))

	// Status 3 carries its missing lines and notes in both formats.
	if err := os.Remove(filepath.Join(p.logDir, "tile", "0", "001")); err != nil {
		t.Fatal(err)
	}
	checkParity(t, "incomplete", 3, append([]string{"consistency", "--old", old}, src...)...)
}

// ---- L7-02: what is enforced ----

func TestRevokedKeyIsNeverUsed(t *testing.T) {
	p := newPublication(t, 300, 700)
	revoked := trustWith(t, p, `,"revoked":true`)
	for _, now := range []string{"2026-12-01T00:00:00Z", "2000-01-01T00:00:00Z"} {
		code, _, errs := run(t, "checkpoint", "--trust", revoked, "--log", p.log(), "--source", p.dir, "--now", now)
		if code != 2 || !strings.Contains(errs, "revoked") {
			t.Errorf("revoked key at %s: %d %s", now, code, errs)
		}
	}
	// A second, live key lets the note be checked; the note signed by the
	// revoked key does not count.
	other := newSigner(2)
	two := filepath.Join(t.TempDir(), "two.json")
	body := fmt.Sprintf(`{"format":1,"origin_prefix":%q,"keys":[{"name":"old","public_key":%q,"fingerprint":%q,"revoked":true},{"name":"new","public_key":%q,"fingerprint":%q}]}`,
		testPrefix, base64.StdEncoding.EncodeToString(p.signer.pub), p.signer.fp(), base64.StdEncoding.EncodeToString(other.pub), other.fp())
	_ = os.WriteFile(two, []byte(body), 0o644)
	code, out, _ := run(t, "checkpoint", "--trust", two, "--log", p.log(), "--source", p.dir)
	if code != 1 || !strings.Contains(out, "signature") || !strings.Contains(out, "revoked in the trust file") {
		t.Errorf("a note signed only by a revoked key: %d %s", code, out)
	}
	p.write(t, "checkpoint", other.note(p.origin, 700, refMTH(p.all)))
	code, out, _ = run(t, "checkpoint", "--trust", two, "--log", p.log(), "--source", p.dir)
	if code != 0 || !strings.Contains(out, "revoked in the trust file") {
		t.Errorf("a note signed by the live key: %d %s", code, out)
	}
}

func TestKeyLimitedToATreeSize(t *testing.T) {
	p := newPublication(t, 300, 700)
	lim := func(n int) string { return trustWith(t, p, fmt.Sprintf(`,"max_tree_size":%d`, n)) }
	log := []string{"--log", p.log(), "--source", p.dir}
	// 700 is within 700, beyond 699.
	if code, out, _ := run(t, append([]string{"checkpoint", "--trust", lim(700)}, log...)...); code != 0 || !strings.Contains(out, "counts only for checkpoints of tree size 700 or less") {
		t.Errorf("at the limit: %d %s", code, out)
	}
	code, out, _ := run(t, append([]string{"checkpoint", "--trust", lim(699)}, log...)...)
	if code != 1 || !strings.Contains(out, "FAILED at step key-size") || strings.Contains(out, "VERIFIED") {
		t.Errorf("beyond the limit: %d %s", code, out)
	}
	// The old checkpoint of consistency is checked with the limit too.
	old := filepath.Join(p.logDir, "old.note")
	if code, out, _ := run(t, append([]string{"consistency", "--trust", lim(299), "--old", old}, log...)...); code != 1 || !strings.Contains(out, "tree size 300") {
		t.Errorf("old note beyond the limit: %d %s", code, out)
	}
	if code, out, _ := run(t, append([]string{"consistency", "--trust", lim(700), "--old", old}, log...)...); code != 0 {
		t.Errorf("both within the limit: %d %s", code, out)
	}
}

func TestTrustShowSaysWhatIsEnforced(t *testing.T) {
	p := newPublication(t, 1, 2)
	f := trustWith(t, p, `,"not_before":"2026-01-01T00:00:00Z","not_after":"2030-01-01T00:00:00Z","max_tree_size":50`)
	code, out, _ := run(t, "trust", "show", "--trust", f, "--now", "2027-01-01T00:00:00Z")
	for _, want := range []string{"usable now", "not_before: enforced", "not_after: enforced the same way", "max_tree_size: enforced", "TRUST FILE:"} {
		if code != 0 || !strings.Contains(out, want) {
			t.Errorf("missing %q (exit %d):\n%s", want, code, out)
		}
	}
	code, _, errs := run(t, "trust", "show", "--trust", f, "--now", "2031-01-01T00:00:00Z")
	if code != 2 || !strings.Contains(errs, "no key can be used") {
		t.Errorf("expired: %d %s", code, errs)
	}
	// A file without keys is not called valid: every command refuses it.
	empty := filepath.Join(t.TempDir(), "e.json")
	_ = os.WriteFile(empty, []byte(`{"format":1,"origin_prefix":"a.com/x","keys":[]}`), 0o644)
	if code, out, _ := run(t, "trust", "show", "--trust", empty); code != 2 || strings.Contains(out, "valid") {
		t.Errorf("no keys: %d %s", code, out)
	}
}

// ---- L7-03: rollback ----

func TestCheckpointNamesRollback(t *testing.T) {
	p := newPublication(t, 300, 700)
	_, out, _ := run(t, "checkpoint", "--trust", p.trust, "--log", p.log(), "--source", p.dir)
	if !strings.Contains(out, "not proved: that this is the latest checkpoint") || !strings.Contains(out, "rollback") || !strings.Contains(out, "consistency --old") || !strings.Contains(out, "--min-size") {
		t.Errorf("the rollback limit is not named:\n%s", out)
	}
	_, js, _ := run(t, "checkpoint", "--trust", p.trust, "--log", p.log(), "--source", p.dir, "--json")
	if !strings.Contains(js, "rollback") {
		t.Errorf("json does not name rollback: %s", js)
	}
}

func TestCheckpointState(t *testing.T) {
	p := newPublication(t, 300, 700)
	state := filepath.Join(t.TempDir(), "state")
	args := func(more ...string) []string {
		return append([]string{"checkpoint", "--trust", p.trust, "--log", p.log(), "--source", p.dir, "--state", state}, more...)
	}
	old := filepath.Join(p.logDir, "old.note")
	cur, _ := os.ReadFile(filepath.Join(p.logDir, "checkpoint"))
	oldBytes, _ := os.ReadFile(old)

	// First use: the state is created.
	code, out, _ := run(t, args()...)
	if code != 0 || !strings.Contains(out, "state file did not exist") {
		t.Fatalf("first use: %d %s", code, out)
	}
	if got, _ := os.ReadFile(state); string(got) != string(cur) {
		t.Fatal("state was not written")
	}
	// Same checkpoint: fine, nothing rewritten.
	if code, out, _ := run(t, args()...); code != 0 || !strings.Contains(out, "same size and root") {
		t.Fatalf("same: %d %s", code, out)
	}
	// An older genuine checkpoint is refused, and the state keeps the larger one.
	p.write(t, "checkpoint", string(oldBytes))
	code, out, _ = run(t, args()...)
	if code != 1 || !strings.Contains(out, "FAILED at step rollback") || strings.Contains(out, "VERIFIED") {
		t.Fatalf("rollback: %d %s", code, out)
	}
	if got, _ := os.ReadFile(state); string(got) != string(cur) {
		t.Fatal("a failed run changed the state")
	}
	// Another history of the same size: a fork.
	forkRoot := refMTH(append([]h32{sha([]byte("other"))}, p.all[1:]...))
	p.write(t, "checkpoint", p.signer.note(p.origin, 700, forkRoot))
	if code, out, _ := run(t, args()...); code != 1 || !strings.Contains(out, "FAILED at step fork") {
		t.Fatalf("fork: %d %s", code, out)
	}
	// A larger size is accepted, the state follows, and the output says that
	// the extension was not proved.
	if err := os.WriteFile(state, oldBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	p.write(t, "checkpoint", string(cur))
	code, out, _ = run(t, args()...)
	if code != 0 || !strings.Contains(out, "not proved: that the log at size 700 extends the log at size 300") || !strings.Contains(out, "state file was updated to size 700") {
		t.Fatalf("growth: %d %s", code, out)
	}
	if got, _ := os.ReadFile(state); string(got) != string(cur) {
		t.Fatal("state was not advanced")
	}
	// No temporary file is left behind.
	ents, _ := os.ReadDir(filepath.Dir(state))
	if len(ents) != 1 {
		t.Errorf("the state directory holds %d entries, expected only the state", len(ents))
	}
	// A state of another log, or one that does not verify, is an input error.
	_ = os.WriteFile(state, []byte("garbage"), 0o644)
	if code, _, errs := run(t, args()...); code != 2 || !strings.Contains(errs, "--state") {
		t.Errorf("garbage state: %d %s", code, errs)
	}
	other := p.signer.note(testPrefix+"/c/"+testChain+"/e2", 5, refMTH(p.all[:5]))
	_ = os.WriteFile(state, []byte(other), 0o644)
	if code, _, _ := run(t, args()...); code != 2 {
		t.Errorf("state of another log: %d", code)
	}
}

// ---- L7-06: wording ----

func TestConsistencyOutputSaysWhatWasRead(t *testing.T) {
	p := newPublication(t, 300, 700)
	args := []string{"consistency", "--trust", p.trust, "--log", p.log(), "--source", p.dir}
	_, out, _ := run(t, append(args, "--old", filepath.Join(p.logDir, "old.note"))...)
	if !regexp.MustCompile(`read \d+ tile\(s\) on the proof path`).MatchString(out) || !strings.Contains(out, "only the tiles on the proof path were read") {
		t.Errorf("a normal run:\n%s", out)
	}
	// Equal sizes with the whole tile directory gone: verified, and the output
	// says that no tile was read instead of claiming a proof was read.
	if err := os.RemoveAll(filepath.Join(p.logDir, "tile")); err != nil {
		t.Fatal(err)
	}
	code, out, _ := run(t, append(args, "--old", filepath.Join(p.logDir, "checkpoint"))...)
	if code != 0 || !strings.Contains(out, "no tile was needed") || strings.Contains(out, "RFC 9162 2.1.4.2") || strings.Contains(out, "read from the tiles") {
		t.Errorf("equal sizes: %d\n%s", code, out)
	}
	if !strings.Contains(out, "is the same as the log at size 700") {
		t.Errorf("the headline of equal sizes claims growth:\n%s", out)
	}
	// A flipped hash in a tile that is not on the proof path is not noticed:
	// the help and README say so, and the output lists it as not proved.
	_, help, _ := run(t, "consistency", "--help")
	if !strings.Contains(help, "Only the\ntiles on the proof path are read") {
		t.Errorf("help does not say that only the proof path is read:\n%s", help)
	}
}

// ---- L7-07: one rule for what the publication does not hold ----

func TestAbsentPublicationObjectIsStatus3(t *testing.T) {
	p := newPublication(t, 300, 700)
	empty := t.TempDir()
	for name, args := range map[string][]string{
		"checkpoint, no checkpoint":    {"checkpoint", "--trust", p.trust, "--log", p.log(), "--source", empty},
		"checkpoint, no epoch dir":     {"checkpoint", "--trust", p.trust, "--log", "chain:" + testChain, "--source", empty},
		"checkpoint, another chain":    {"checkpoint", "--trust", p.trust, "--log", "chain:" + strings.Repeat("cd", 32) + ":e1", "--source", p.dir},
		"checkpoint, aggregate absent": {"checkpoint", "--trust", p.trust, "--log", "aggregate:" + testChain, "--source", p.dir},
		"consistency, no checkpoint":   {"consistency", "--trust", p.trust, "--log", p.log(), "--source", empty, "--old", filepath.Join(p.logDir, "old.note")},
	} {
		code, out, _ := run(t, args...)
		if code != 3 || !strings.HasPrefix(out, "INCOMPLETE:") || !strings.Contains(out, "missing: the publication does not contain") {
			t.Errorf("%s: exit %d\n%s", name, code, out)
		}
		if strings.Contains(out, "VERIFIED") || !strings.Contains(out, "alert") {
			t.Errorf("%s: status 3 must read as an alert:\n%s", name, out)
		}
	}
	// What the auditor named is status 2 when it cannot be used.
	for name, args := range map[string][]string{
		"source does not exist":  {"checkpoint", "--trust", p.trust, "--log", p.log(), "--source", filepath.Join(empty, "nope")},
		"source is a file":       {"checkpoint", "--trust", p.trust, "--log", p.log(), "--source", p.trust},
		"--note does not exist":  {"checkpoint", "--trust", p.trust, "--log", p.log(), "--note", filepath.Join(empty, "nope")},
		"--old does not exist":   {"consistency", "--trust", p.trust, "--log", p.log(), "--source", p.dir, "--old", filepath.Join(empty, "nope")},
		"--trust does not exist": {"checkpoint", "--trust", filepath.Join(empty, "nope"), "--log", p.log(), "--source", p.dir},
	} {
		if code, _, _ := run(t, args...); code != 2 {
			t.Errorf("%s: exit %d, want 2", name, code)
		}
	}
}

// ---- L7-01, L7-05: what the publication holds in a form it must not have ----

func TestSpecialFilesInASource(t *testing.T) {
	p := newPublication(t, 300, 700)
	args := []string{"checkpoint", "--trust", p.trust, "--log", p.log(), "--source", p.dir}
	// A directory in place of the checkpoint.
	cp := filepath.Join(p.logDir, "checkpoint")
	saved, _ := os.ReadFile(cp)
	if err := os.Remove(cp); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(cp, 0o755); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := runWithin(t, args...); code != 1 || !strings.Contains(out, "publication") || strings.Contains(out, "VERIFIED") {
		t.Errorf("directory as checkpoint: %d %s", code, out)
	}
	_ = os.Remove(cp)
	// A checkpoint larger than the limit.
	if err := os.WriteFile(cp, make([]byte, (1<<20)+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := runWithin(t, args...); code != 1 || !strings.Contains(out, "larger than the limit") {
		t.Errorf("huge checkpoint: %d %s", code, out)
	}
	_ = os.WriteFile(cp, saved, 0o644)
	// A directory in place of a tile.
	tile := filepath.Join(p.logDir, "tile", "0", "001")
	_ = os.Remove(tile)
	_ = os.Mkdir(tile, 0o755)
	if code, out, _ := runWithin(t, "consistency", "--trust", p.trust, "--log", p.log(), "--source", p.dir, "--old", filepath.Join(p.logDir, "old.note")); code != 1 || strings.Contains(out, "VERIFIED") {
		t.Errorf("directory as tile: %d %s", code, out)
	}
}

func TestLinksInASource(t *testing.T) {
	p := newPublication(t, 300, 700)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cp := filepath.Join(p.logDir, "checkpoint")
	saved, _ := os.ReadFile(cp)
	_ = os.Remove(cp)
	if err := os.Symlink(outside, cp); err != nil {
		t.Skipf("cannot make a symbolic link here: %v", err)
	}
	args := []string{"checkpoint", "--trust", p.trust, "--log", p.log(), "--source", p.dir}
	if code, out, _ := runWithin(t, args...); code != 1 || !strings.Contains(out, "symbolic link") {
		t.Errorf("link out of the root as checkpoint: %d %s", code, out)
	}
	// A link that stays inside the root is refused as well: the rule has no exception to explain.
	_ = os.Remove(cp)
	real := filepath.Join(p.logDir, "real-checkpoint")
	_ = os.WriteFile(real, saved, 0o644)
	if err := os.Symlink(real, cp); err != nil {
		t.Skip(err)
	}
	if code, out, _ := runWithin(t, args...); code != 1 || !strings.Contains(out, "symbolic link") {
		t.Errorf("link inside the root as checkpoint: %d %s", code, out)
	}
	// A link in the middle of the path: the epoch directory.
	_ = os.Remove(cp)
	_ = os.WriteFile(cp, saved, 0o644)
	moved := filepath.Join(p.dir, "c", testChain, "e1-real")
	if err := os.Rename(p.logDir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, p.logDir); err != nil {
		t.Skip(err)
	}
	if code, out, _ := runWithin(t, args...); code != 1 || !strings.Contains(out, "symbolic link") {
		t.Errorf("link as epoch directory: %d %s", code, out)
	}
	// Without the epoch the entry is not a directory and is reported, not skipped.
	if code, out, _ := runWithin(t, "checkpoint", "--trust", p.trust, "--log", "chain:"+testChain, "--source", p.dir); code != 1 || strings.Contains(out, "VERIFIED") {
		t.Errorf("link as epoch directory, epoch from the source: %d %s", code, out)
	}
	// A link given by the auditor on the command line is read.
	_ = os.Remove(p.logDir)
	_ = os.Rename(moved, p.logDir)
	lnk := filepath.Join(t.TempDir(), "note-link")
	if err := os.Symlink(cp, lnk); err != nil {
		t.Skip(err)
	}
	if code, out, _ := runWithin(t, "checkpoint", "--trust", p.trust, "--log", p.log(), "--note", lnk); code != 0 {
		t.Errorf("link to a note named by the auditor: %d %s", code, out)
	}
}

func TestDirectoryAsNamedFile(t *testing.T) {
	p := newPublication(t, 300, 700)
	dir := t.TempDir()
	for name, args := range map[string][]string{
		"--note":  {"checkpoint", "--trust", p.trust, "--log", p.log(), "--note", dir},
		"--trust": {"checkpoint", "--trust", dir, "--log", p.log(), "--source", p.dir},
		"--old":   {"consistency", "--trust", p.trust, "--log", p.log(), "--source", p.dir, "--old", dir},
		"--new":   {"consistency", "--trust", p.trust, "--log", p.log(), "--source", p.dir, "--old", "0:" + base64.StdEncoding.EncodeToString(emptyRoot()), "--new", dir},
		"--state": {"checkpoint", "--trust", p.trust, "--log", p.log(), "--source", p.dir, "--state", dir},
	} {
		if code, _, errs := runWithin(t, args...); code != 2 || !strings.Contains(errs, "regular file") {
			t.Errorf("%s: %d %s", name, code, errs)
		}
	}
}

func emptyRoot() []byte {
	r := sha()
	return r[:]
}

// A file that is too big is refused before it is read, wherever it is named.
func TestOversizedNamedFiles(t *testing.T) {
	p := newPublication(t, 300, 700)
	big := filepath.Join(t.TempDir(), "big")
	if err := os.WriteFile(big, make([]byte, (1<<20)+1), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"--note":  {"checkpoint", "--trust", p.trust, "--log", p.log(), "--note", big},
		"--trust": {"checkpoint", "--trust", big, "--log", p.log(), "--source", p.dir},
		"--old":   {"consistency", "--trust", p.trust, "--log", p.log(), "--source", p.dir, "--old", big},
	} {
		if code, _, errs := runWithin(t, args...); code != 2 || !strings.Contains(errs, "larger than the limit") {
			t.Errorf("%s: %d %s", name, code, errs)
		}
	}
}

// ---- the older commands ----

func TestDigestChainWithNothingToCompare(t *testing.T) {
	empty := t.TempDir()
	code, _, errs := run(t, "digest-chain", "--digests", empty)
	if code != 2 || !strings.Contains(errs, "no digest file") {
		t.Errorf("an empty directory: %d %s", code, errs)
	}
	one := fx("digests/d8.json")
	code, out, _ := run(t, "digest-chain", "--digests", one)
	if code != 1 || !strings.Contains(out, "at least 2") || strings.Contains(out, "agree") {
		t.Errorf("one digest cannot be compared: %d %s", code, out)
	}
}

func TestBlocksWithoutDigestSaysSo(t *testing.T) {
	_, out, _ := run(t, "blocks", "--blocks", fx("blocks.jsonl"), "--chain-id", chainIDOfFixtures(t))
	if !strings.Contains(out, "not proved: no --digest was given") || !strings.Contains(out, "rewritten from the genesis block verifies too") {
		t.Errorf("%s", out)
	}
}

func chainIDOfFixtures(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(fx("digests/d8.json"))
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		ChainID string `json:"chain_id"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	return d.ChainID
}

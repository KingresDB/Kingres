// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/release"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
)

// A release made in the test, with keys made in the test: no real key exists
// in this repository.
type testRelease struct {
	dir, sums, note, bin, key, vkey string
	binName                         string
	priv                            ed25519.PrivateKey
}

func newTestRelease(t *testing.T) *testRelease {
	t.Helper()
	seed := sha256.Sum256([]byte("release test key " + t.Name()))
	priv := ed25519.NewKeyFromSeed(seed[:])
	pub := priv.Public().(ed25519.PublicKey)
	dir := testdir.New(t)
	r := &testRelease{dir: dir, priv: priv, binName: "kingres-verify-linux-amd64",
		key: base64.StdEncoding.EncodeToString(pub), vkey: release.VKey(pub)}
	binary := []byte("pretend binary of the linux release")
	other := []byte("pretend binary of the windows release")
	sums, err := release.FormatSums([]release.Entry{
		{Hash: sha256.Sum256(binary), Name: r.binName},
		{Hash: sha256.Sum256(other), Name: "kingres-verify-windows-amd64.exe"},
	})
	if err != nil {
		t.Fatal(err)
	}
	note, err := release.Sign(priv, sums)
	if err != nil {
		t.Fatal(err)
	}
	r.sums = writeTemp(t, dir, "SHA256SUMS", string(sums))
	r.note = writeTemp(t, dir, "SHA256SUMS.note", string(note))
	r.bin = writeTemp(t, dir, r.binName, string(binary))
	return r
}

func (r *testRelease) args(more ...string) []string {
	return append([]string{"release", "verify", "--sums", r.sums, "--note", r.note, "--key", r.key}, more...)
}

func TestReleaseVerifyAccepts(t *testing.T) {
	r := newTestRelease(t)
	for _, key := range []string{r.key, r.vkey} {
		a := []string{"release", "verify", "--sums", r.sums, "--note", r.note, "--key", key, "--binary", r.bin}
		code, out, errs := run(t, a...)
		if code != 0 || !strings.HasPrefix(out, "VERIFIED:") || errs != "" {
			t.Fatalf("%d\n%s%s", code, out, errs)
		}
		if !strings.Contains(out, hex.EncodeToString(func() []byte { h := sha256.Sum256([]byte("pretend binary of the linux release")); return h[:] }())) {
			t.Errorf("the hash of the binary is not shown:\n%s", out)
		}
	}
	// the sums note alone: genuine, and it says no binary was checked
	code, out, _ := run(t, r.args()...)
	if code != 0 || !strings.Contains(out, "no binary was given") || !strings.Contains(out, "not proved: that any binary you hold") {
		t.Errorf("%d\n%s", code, out)
	}
	// a renamed copy, named
	renamed := writeTemp(t, r.dir, "kingres-verify", "pretend binary of the linux release")
	if code, out, _ := run(t, r.args("--binary", renamed, "--name", r.binName)...); code != 0 {
		t.Errorf("renamed: %d\n%s", code, out)
	}
	// JSON
	code, out, _ = run(t, r.args("--binary", r.bin, "--json")...)
	var v struct {
		Verified bool
		Steps    []struct{ Name string }
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil || code != 0 || !v.Verified || len(v.Steps) != 3 {
		t.Errorf("json: %d %v %s", code, err, out)
	}
}

func TestReleaseVerifyFailures(t *testing.T) {
	r := newTestRelease(t)
	otherSeed := sha256.Sum256([]byte("another key"))
	otherPriv := ed25519.NewKeyFromSeed(otherSeed[:])
	otherPub := base64.StdEncoding.EncodeToString(otherPriv.Public().(ed25519.PublicKey))

	cases := []struct {
		name string
		args []string
		step string
	}{
		{"binary changed", r.args("--binary", writeTemp(t, r.dir, r.binName+"-x", "tampered")), "binary"},
		{"binary not listed", r.args("--binary", writeTemp(t, r.dir, "kingres-verify-other", "x")), "binary"},
		{"another key", []string{"release", "verify", "--sums", r.sums, "--note", r.note, "--key", otherPub}, "note-signature"},
		{"sums changed after signing", []string{"release", "verify", "--sums", writeTemp(t, r.dir, "S2", strings.Replace(readFile(t, r.sums), "kingres-verify-linux", "kingres-verify-linuy", 1)), "--note", r.note, "--key", r.key}, "note-text"},
		{"sums as the note", []string{"release", "verify", "--sums", r.sums, "--note", r.sums, "--key", r.key}, "note-parse"},
	}
	for _, c := range cases {
		code, out, errs := run(t, c.args...)
		if code != 1 || !strings.HasPrefix(out, "FAILED at step "+c.step+":") || strings.Contains(out, "VERIFIED") {
			t.Errorf("%s: %d\n%s%s", c.name, code, out, errs)
		}
	}
	// a bad note when also the binary is wrong: the signature failure comes first
	code, out, _ := run(t, "release", "verify", "--sums", r.sums, "--note", r.note, "--key", otherPub, "--binary", writeTemp(t, r.dir, "kingres-verify-other", "x"))
	if code != 1 || !strings.Contains(out, "FAILED at step note-signature") {
		t.Errorf("%d\n%s", code, out)
	}
}

func TestReleaseVerifyInputErrors(t *testing.T) {
	r := newTestRelease(t)
	missing := filepath.Join(r.dir, "nope")
	for name, a := range map[string][]string{
		"no flags":          {"release", "verify"},
		"no key":            {"release", "verify", "--sums", r.sums, "--note", r.note},
		"bad key":           r.args("--key", "!!!"),
		"key of other name": {"release", "verify", "--sums", r.sums, "--note", r.note, "--key", strings.Replace(r.vkey, release.KeyName, "other", 1)},
		"no sums file":      {"release", "verify", "--sums", missing, "--note", r.note, "--key", r.key},
		"no note file":      {"release", "verify", "--sums", r.sums, "--note", missing, "--key", r.key},
		"no binary file":    r.args("--binary", missing),
		"name without one":  r.args("--name", r.binName),
		"stray argument":    r.args("extra"),
	} {
		code, out, errs := run(t, a...)
		if code != 2 || strings.Contains(out, "VERIFIED") || errs == "" {
			t.Errorf("%s: %d\n%s%s", name, code, out, errs)
		}
	}
	if code, _, _ := run(t, "release"); code != 2 {
		t.Error("release alone")
	}
	if code, _, _ := run(t, "release", "sign"); code != 2 {
		t.Error("release sign")
	}
	if code, out, _ := run(t, "release", "--help"); code != 0 || !strings.Contains(out, "release verify") {
		t.Error("release --help")
	}
}

// releaseRows are the rows of the exit sites of release verify for
// TestEveryExitSiteHasARowAndAScenario.
func releaseRows(_ *exitEnv) []exitRow {
	mk := func(site string, status int, f func(t *testing.T, r *testRelease) []string) exitRow {
		return exitRow{site: site, status: status, args: func(t *testing.T) []string { return f(t, newTestRelease(t)) }}
	}
	rows := flagCases("ReleaseVerify", "release", "verify")
	return append(rows,
		exitRow{site: "runRelease:exitUsage#1", status: 2, args: func(*testing.T) []string { return []string{"release"} }},
		exitRow{site: "runRelease:exitUsage#2", status: 2, args: func(*testing.T) []string { return []string{"release", "nonsense"} }},
		exitRow{site: "runReleaseVerify:inputError#1", status: 2, args: func(*testing.T) []string { return []string{"release", "verify"} }},
		mk("runReleaseVerify:finishFailed#1", 1, func(t *testing.T, r *testRelease) []string {
			return r.args("--binary", writeTemp(t, r.dir, r.binName+"-x", "tampered"))
		}),
	)
}

// P7R-01: a binary with the right name and other content is refused at the comparison of its SHA-256 with the signed sums, and
// the message names both hashes. Before this test no case reached that comparison: the "changed" file had a name that is not in
// the sums, so the run ended at the name lookup.
func TestReleaseVerifyRefusesARightNamedBinaryWithOtherBytes(t *testing.T) {
	r := newTestRelease(t)
	dir2 := testdir.New(t) // another directory, so that the file may carry the right name
	good := sha256.Sum256([]byte("pretend binary of the linux release"))
	for name, content := range map[string]string{
		"one byte changed": "pretend binary of the linux releasf",
		"appended":         "pretend binary of the linux release\n",
		"empty":            "",
	} {
		bin := writeTemp(t, dir2, r.binName, content)
		code, out, errs := run(t, r.args("--binary", bin)...)
		bad := sha256.Sum256([]byte(content))
		if code != 1 || !strings.HasPrefix(out, "FAILED at step binary:") || strings.Contains(out, "VERIFIED") ||
			!strings.Contains(out, hex.EncodeToString(bad[:])) || !strings.Contains(out, hex.EncodeToString(good[:])) {
			t.Errorf("%s: %d\n%s%s", name, code, out, errs)
		}
	}
	// the same with --name: a renamed file with other bytes
	renamed := writeTemp(t, dir2, "kingres-verify", "tampered")
	if code, out, _ := run(t, r.args("--binary", renamed, "--name", r.binName)...); code != 1 || !strings.HasPrefix(out, "FAILED at step binary:") {
		t.Errorf("renamed, other bytes: %d\n%s", code, out)
	}
	// the right content under a name that is not in the sums is refused without --name, and accepted with it
	other := writeTemp(t, dir2, "kingres-verify", "pretend binary of the linux release")
	if code, out, _ := run(t, r.args("--binary", other)...); code != 1 || !strings.HasPrefix(out, "FAILED at step binary:") || !strings.Contains(out, "is not in the signed sums") {
		t.Errorf("renamed without --name: %d\n%s", code, out)
	}
	// one genuine file and one changed file in one run: the run fails, and the genuine one does not hide the other
	wrong := writeTemp(t, testdir.New(t), "kingres-verify-windows-amd64.exe", "not the windows release")
	if code, out, _ := run(t, r.args("--binary", r.bin, "--binary", wrong)...); code != 1 || !strings.HasPrefix(out, "FAILED at step binary:") {
		t.Errorf("two binaries: %d\n%s", code, out)
	}
}

// P7R-07: the version, commit and date are signed with the release (the RELEASE file is listed in the sums), the output shows them
// when the file is given, and the "not proved" list says that an older genuine release verifies as well.
func TestReleaseVerifyShowsTheSignedVersionAndSaysAnOlderReleaseVerifiesToo(t *testing.T) {
	seed := sha256.Sum256([]byte("release info test key"))
	priv := ed25519.NewKeyFromSeed(seed[:])
	key := base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
	info, err := release.FormatInfo(release.Info{Version: "0.1.0", Commit: strings.Repeat("c0", 20), Date: "2026-11-21T10:00:00Z", Go: "go1.27.3"})
	if err != nil {
		t.Fatal(err)
	}
	binary := []byte("pretend binary")
	sums, err := release.FormatSums([]release.Entry{{Hash: sha256.Sum256(binary), Name: "kingres-verify-linux-amd64"}, {Hash: sha256.Sum256(info), Name: release.InfoName}})
	if err != nil {
		t.Fatal(err)
	}
	note, err := release.Sign(priv, sums)
	if err != nil {
		t.Fatal(err)
	}
	dir := testdir.New(t)
	sp, np := writeTemp(t, dir, "SHA256SUMS", string(sums)), writeTemp(t, dir, "SHA256SUMS.note", string(note))
	ip, bp := writeTemp(t, dir, release.InfoName, string(info)), writeTemp(t, dir, "kingres-verify-linux-amd64", string(binary))
	base := []string{"release", "verify", "--sums", sp, "--note", np, "--key", key}

	out := wantRun(t, 0, append(append([]string{}, base...), "--binary", bp, "--binary", ip),
		"VERIFIED:", "release-info", "version 0.1.0", "commit "+strings.Repeat("c0", 20), "commit date 2026-11-21T10:00:00Z", "built with go1.27.3",
		"not proved: that this is the newest release", "an older release", "rollback")
	if strings.Contains(out, "which release this is") {
		t.Errorf("the version was shown and the run still says it was not:\n%s", out)
	}
	// without the file: the run says the sums carry it
	wantRun(t, 0, append(append([]string{}, base...), "--binary", bp), "VERIFIED:", "not proved: which release this is: the signed sums list a RELEASE file", "not proved: that this is the newest release")
	// JSON carries the version
	wantRun(t, 0, append(append([]string{}, base...), "--binary", ip, "--json"), `"release_version": "0.1.0"`)
	// a RELEASE file that is not the signed one is refused at the hash, and one that matches but has the wrong format at release-info
	bad := writeTemp(t, testdir.New(t), release.InfoName, strings.Replace(string(info), "0.1.0", "9.9.9", 1))
	wantRun(t, 1, append(append([]string{}, base...), "--binary", bad), "FAILED at step binary:")
	junk := []byte("not a release file\n")
	sums2, _ := release.FormatSums([]release.Entry{{Hash: sha256.Sum256(junk), Name: release.InfoName}})
	note2, _ := release.Sign(priv, sums2)
	dir2 := testdir.New(t)
	wantRun(t, 1, []string{"release", "verify", "--sums", writeTemp(t, dir2, "SHA256SUMS", string(sums2)), "--note", writeTemp(t, dir2, "SHA256SUMS.note", string(note2)), "--key", key, "--binary", writeTemp(t, dir2, release.InfoName, string(junk))}, "FAILED at step release-info:")
}

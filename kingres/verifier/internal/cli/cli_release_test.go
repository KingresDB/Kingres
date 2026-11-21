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

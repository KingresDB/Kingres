// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func copyTo(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// ---- L8-05 ----

func TestDigestChainDirectoryCaps(t *testing.T) {
	d0 := fx("digests/d0.json")
	for _, c := range []struct {
		n    int
		want int
	}{{1024, 0}, {1025, 2}} {
		dir := t.TempDir()
		for i := 0; i < c.n; i++ {
			copyTo(t, d0, filepath.Join(dir, fmt.Sprintf("d%04d.json", i)))
		}
		code, out, errs := run(t, "digest-chain", "--digests", dir)
		if code != c.want {
			t.Errorf("%d files: status %d, want %d\n%.300s%.300s", c.n, code, c.want, out, errs)
		}
		if c.want == 2 && !strings.Contains(errs, "larger than the limit") && !strings.Contains(errs, "more than") {
			t.Errorf("%d files: %s", c.n, errs)
		}
	}
	// Explicit files are counted too.
	args := []string{"digest-chain"}
	for i := 0; i < 1025; i++ {
		args = append(args, d0)
	}
	if code, _, errs := run(t, args...); code != 2 || !strings.Contains(errs, "more than 1024") {
		t.Errorf("1025 named files: %d %s", code, errs)
	}
}

func TestDigestChainTotalSizeCap(t *testing.T) {
	d0, _ := os.ReadFile(fx("digests/d0.json"))
	dir := t.TempDir()
	// 257 files of 64 KiB each: every file is within its own limit, together they
	// are more than 16 MiB.
	pad := make([]byte, maxDigestFile-len(d0))
	for i := range pad {
		pad[i] = ' '
	}
	for i := 0; i < 257; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("d%d.json", i)), append(append([]byte{}, d0...), pad...), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	code, _, errs := run(t, "digest-chain", "--digests", dir)
	if code != 2 || !strings.Contains(errs, "together") {
		t.Errorf("%d %s", code, errs)
	}
}

func TestDigestChainGlobCharactersAreNames(t *testing.T) {
	root := t.TempDir()
	mk := func(name string, digests ...string) {
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, d := range digests {
			copyTo(t, fx("digests/"+d+".json"), filepath.Join(root, name, d+".json"))
		}
	}
	mk("x[ab]", "d0", "d1")
	mk("xa", "d2")
	mk("xb", "d3")
	code, out, errs := run(t, "digest-chain", "--digests", filepath.Join(root, "x[ab]"))
	if code != 0 || !strings.Contains(out, "2 digests agree") {
		t.Fatalf("the directory named x[ab] was not read as itself: %d %s %s", code, out, errs)
	}
	// A name with a star in a directory is a file or nothing.
	if code, _, _ := run(t, "digest-chain", "--digests", filepath.Join(root, "x*")); code != 2 {
		t.Errorf("a pattern was expanded: %d", code)
	}
}

// ---- L8-09 ----

func TestNowFlagDetails(t *testing.T) {
	p := newPublication(t, 300, 700)
	tr := trustWith(t, p, `,"not_after":"2020-01-01T00:00:00Z"`)
	base := []string{"checkpoint", "--trust", tr, "--log", p.log(), "--source", p.dir}
	// Boundary: the time itself is inside, one nanosecond later is not.
	if code, out, _ := run(t, append(base, "--now", "2020-01-01T00:00:00Z")...); code != 0 {
		t.Errorf("at not_after: %d %s", code, out)
	}
	code, _, errs := run(t, append(base, "--now", "2020-01-01T00:00:00.000000001Z")...)
	if code != 2 || !strings.Contains(errs, "2020-01-01T00:00:00.000000001Z") {
		t.Errorf("a nanosecond later: %d %s", code, errs)
	}
	// The printed time keeps its fraction, so two equal looking times never
	// explain a "passed".
	code, _, errs = run(t, append(base, "--now", "2020-01-01T00:00:00.9Z")...)
	if code != 2 || !strings.Contains(errs, "2020-01-01T00:00:00.9Z") {
		t.Errorf("fraction: %d %s", code, errs)
	}
	// An empty value is an error and never the clock.
	if code, _, errs := run(t, append(base, "--now", "")...); code != 2 || !strings.Contains(errs, "--now is empty") {
		t.Errorf("empty: %d %s", code, errs)
	}
	if code, _, _ := run(t, "trust", "show", "--trust", tr, "--now", ""); code != 2 {
		t.Errorf("trust show with an empty --now: %d", code)
	}
	if code, _, _ := run(t, append(base, "--now", "2020-01-01T00:00:00,5Z")...); code != 2 {
		t.Errorf("comma: %d", code)
	}
}

func TestTrustShowSaysWhatItCannotKnow(t *testing.T) {
	p := newPublication(t, 300, 700)
	tr := trustWith(t, p, `,"max_tree_size":10,"not_after":"2999-01-01T00:00:00Z"`)
	code, out, _ := run(t, "trust", "show", "--trust", tr)
	if code != 0 || !strings.Contains(out, "does not know the size of your log") || !strings.Contains(out, "clock was set back") || !strings.Contains(out, "revoked, which does not depend on the clock") {
		t.Errorf("%d\n%s", code, out)
	}
}

// ---- L8-11 ----

func TestSourceThatIsAFileSaysSo(t *testing.T) {
	p := newPublication(t, 300, 700)
	_, _, errs := run(t, "checkpoint", "--trust", p.trust, "--log", p.log(), "--source", p.trust)
	if !strings.Contains(errs, "it is a regular file") || strings.Contains(errs, "special") {
		t.Errorf("%s", errs)
	}
}

func TestRowHashJSONDoesNotClaimVerification(t *testing.T) {
	f := writeTemp(t, t.TempDir(), "c.json", `[{"name":"id","send_hex":"00000001"}]`)
	_, out, _ := run(t, "row-hash", "--table-id", "1", "--columns", f, "--json")
	if !strings.Contains(out, `"verified": false`) {
		t.Errorf("%s", out)
	}
}

// L9-05: a digest is a few hundred bytes; a file of more than 64 KiB is refused
// before it is parsed, by every command that reads a digest.
func TestDigestFileLimit(t *testing.T) {
	d0, _ := os.ReadFile(fx("digests/d0.json"))
	big := filepath.Join(t.TempDir(), "big.json")
	if err := os.WriteFile(big, append(d0, make([]byte, 64<<10)...), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"digest-chain", big},
		{"digest-chain", "--digests", filepath.Dir(big)},
		{"blocks", "--blocks", big, "--digest", big},
		{"proof", "--proof", big, "--digest", big},
	} {
		if code, _, errs := run(t, args...); code != 2 || !strings.Contains(errs, "larger than the limit") {
			t.Errorf("%v: %d %s", args[0], code, errs)
		}
	}
}

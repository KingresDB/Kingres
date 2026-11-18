// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"bytes"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
)

// shortWriter accepts half of every write and reports no error.
type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) / 2, nil }

// L10-08: status 0 says that the verdict was printed. A write that failed (a
// full disk, a closed descriptor) must not leave status 0, on any command, in
// text and in JSON, and a verdict that is not 0 keeps its status.
func TestFailedWriteOfTheVerdictIsNeverStatus0(t *testing.T) {
	x := newExitEnv(t)
	d := testdir.New(t)
	st := filepath.Join(d, "state")
	s := newSigner(9)
	pub := base64.StdEncoding.EncodeToString(s.pub)
	cases := map[string][]string{
		"version":      {"version"},
		"help":         {"--help"},
		"proof":        {"proof", "--proof", fx("proofs/acc5.json"), "--digest", fx("digests/d5.json")},
		"digest-chain": {"digest-chain", fx("digests/d5.json"), fx("digests/d8.json")},
		"checkpoint":   x.cp(),
		"state":        x.cp("--state", st),
		"consistency":  x.co("--old", x.oldN),
		"trust init":   {"trust", "init", "--origin-prefix", testPrefix, "--public-key", pub, "--fingerprint", s.fp(), "--out", "-"},
		"trust show":   {"trust", "show", "--trust", x.p.trust},
		"row-hash":     {"row-hash", "--table-id", "1", "--columns", fx("columns/acc1.json")},
	}
	for name, args := range cases {
		for _, asJSON := range []bool{false, true} {
			a := append([]string{}, args...)
			if asJSON && name != "version" && name != "help" {
				a = append(a, "--json")
			}
			// the command itself succeeds when the output works
			var ok bytes.Buffer
			want := Run(a, &ok, io.Discard, "test")
			if name == "state" {
				_ = os.Remove(st)
			}
			var errb bytes.Buffer
			got := Run(a, failingWriter{}, &errb, "test")
			if want == 0 {
				if got == 0 || !strings.Contains(errb.String(), "could not be written") {
					t.Errorf("%s (json=%v): status %d with a standard output that refuses every write\n%s", name, asJSON, got, errb.String())
				}
			} else if got != want {
				t.Errorf("%s (json=%v): status %d without a failing output, %d with", name, asJSON, want, got)
			}
			// half of the bytes written and no error: also a failed write
			if name == "state" {
				_ = os.Remove(st)
			}
			if want == 0 {
				if got := Run(a, shortWriter{}, io.Discard, "test"); got == 0 {
					t.Errorf("%s (json=%v): a short write ended with status 0", name, asJSON)
				}
			}
		}
	}
	// a failure keeps its own status: the verdict is not a pass either way
	if got := Run(x.cp("--min-size", "99999"), failingWriter{}, io.Discard, "test"); got != 1 {
		t.Errorf("a failed check with a failing output: status %d, want 1", got)
	}
}

// The same with a real device that refuses writes (Linux).
func TestVerdictToDevFull(t *testing.T) {
	f, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if err != nil {
		t.Skip("no /dev/full here")
	}
	defer f.Close()
	x := newExitEnv(t)
	for _, a := range [][]string{x.cp(), append(x.cp(), "--json"), {"trust", "show", "--trust", x.p.trust}} {
		if code := Run(a, f, io.Discard, "test"); code == 0 {
			t.Errorf("%v: status 0 on /dev/full", a[:2])
		}
	}
}

// L10-16: a trust file whose write failed (ulimit -f 0, a full disk) is not left
// behind as an empty file that makes the next run refuse the name.
func TestTrustInitRemovesAFileItCouldNotWrite(t *testing.T) {
	s := newSigner(9)
	out := filepath.Join(testdir.New(t), "trust.json")
	args := []string{"trust", "init", "--origin-prefix", testPrefix, "--public-key", base64.StdEncoding.EncodeToString(s.pub), "--fingerprint", s.fp(), "--out", out}
	failTrustWrite(t)
	if code, _, _ := run(t, args...); code != 2 {
		t.Fatalf("status %d, want 2", code)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("an empty trust file was left behind")
	}
	trustWrite = func(f *os.File, data []byte) (int, error) { return f.Write(data) }
	if code, _, errs := run(t, args...); code != 0 {
		t.Fatalf("the next run: %d %s", code, errs)
	}
}

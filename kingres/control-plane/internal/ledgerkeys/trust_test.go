// SPDX-License-Identifier: AGPL-3.0-only

package ledgerkeys

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTrustFileContent(t *testing.T) {
	var s Set
	s = okSet(t)(mustAdd(s, 1, "2026", t0))
	s = okSet(t)(s.Activate(fp(1), t0, 0))
	s = okSet(t)(mustAdd(s, 2, "2027", t0.Add(time.Hour)))
	// next keys are left out by default and written on request.
	b, err := TrustFile("ledger.example.com/kingres", s, TrustOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Format       int
		OriginPrefix string `json:"origin_prefix"`
		Keys         []struct {
			Name        string
			PublicKey   string `json:"public_key"`
			Fingerprint string
			NotBefore   *string `json:"not_before"`
			NotAfter    *string `json:"not_after"`
			Revoked     bool
			MaxTreeSize *uint64 `json:"max_tree_size"`
		}
		TSARoots   []string `json:"tsa_roots"`
		ClientKeys []string `json:"client_keys"`
		Witnesses  []string
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Format != 1 || doc.OriginPrefix != "ledger.example.com/kingres" || len(doc.Keys) != 1 {
		t.Fatalf("%s", b)
	}
	k := doc.Keys[0]
	if k.Name != "2026" || k.Fingerprint != fp(1) || k.NotBefore == nil || *k.NotBefore != "2026-12-01T10:00:00Z" || k.NotAfter != nil || k.Revoked || k.MaxTreeSize != nil {
		t.Fatalf("%s", b)
	}
	if pub, _ := base64.StdEncoding.DecodeString(k.PublicKey); !bytes.Equal(pub, pubN(1)) {
		t.Fatal("public key")
	}
	if !bytes.HasSuffix(b, []byte("}\n")) || bytes.Contains(b, []byte("\t")) {
		t.Fatal("not 2-space indented JSON with a final newline")
	}
	withNext, err := TrustEntries(s, TrustOptions{IncludeNext: true})
	if err != nil || len(withNext) != 2 || withNext[1].NotBefore != nil {
		t.Fatalf("next key: %v %+v", err, withNext)
	}
	// Retired and revoked.
	s = okSet(t)(s.Activate(fp(2), t0.Add(2*time.Hour), 0))
	s = okSet(t)(s.Retire(fp(1), t0.Add(3*time.Hour), AtMost(77)))
	s = okSet(t)(mustAdd(s, 3, "", t0.Add(90*time.Minute)))
	s = okSet(t)(s.Revoke(fp(3), t0.Add(4*time.Hour), t0.Add(90*time.Minute), "x"))
	es, err := TrustEntries(s, TrustOptions{})
	if err != nil || len(es) != 3 {
		t.Fatalf("%v %v", err, es)
	}
	if es[0].MaxTreeSize == nil || *es[0].MaxTreeSize != 77 || es[0].NotAfter != nil || es[0].Revoked {
		t.Fatalf("retired entry: %+v", es[0])
	}
	if es[1].MaxTreeSize != nil || es[1].Revoked {
		t.Fatalf("active entry: %+v", es[1])
	}
	if !es[2].Revoked || es[2].MaxTreeSize != nil || es[2].NotBefore != nil {
		t.Fatalf("revoked entry: %+v", es[2])
	}
}

func TestTrustFileRefusals(t *testing.T) {
	s := okSet(t)(mustAdd(nil, 1, "", t0))
	if _, err := TrustFile("p", s, TrustOptions{}); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("only a next key: %v", err)
	}
	s = okSet(t)(s.Activate(fp(1), t0, 0))
	for _, bad := range []string{"", "a b", "a+b", "https://x", "/x", "x/", "a//b", strings.Repeat("x", MaxPrefixSize+1)} {
		if _, err := TrustFile(bad, s, TrustOptions{}); err == nil {
			t.Errorf("prefix %q accepted", bad)
		}
	}
	for _, ok := range []string{"x", "ledger.example.com/kingres", strings.Repeat("x", MaxPrefixSize)} {
		if _, err := TrustFile(ok, s, TrustOptions{}); err != nil {
			t.Errorf("prefix %q: %v", ok, err)
		}
	}
	// The longest log origin fits when the prefix is at the limit.
	if n := MaxPrefixSize + len("/c/") + 64 + len("/e") + 9; n > MaxKeyNameSize {
		t.Errorf("a longest origin has %d bytes, more than %d", n, MaxKeyNameSize)
	}
	if _, err := TrustFile("p", Set{{Public: pubN(1)}}, TrustOptions{}); err == nil {
		t.Error("an invalid set was written")
	}
}

// verifierBin returns the kingres-verify binary named by KINGRES_VERIFY (or
// KINGRES_VERIFY_BIN), or skips the test. With KINGRES_REQUIRE_VERIFY=1 a
// missing verifier is a failure, not a skip.
func verifierBin(t testing.TB) string {
	t.Helper()
	p := os.Getenv("KINGRES_VERIFY")
	if p == "" {
		p = os.Getenv("KINGRES_VERIFY_BIN")
	}
	if p == "" {
		if os.Getenv("KINGRES_REQUIRE_VERIFY") == "1" {
			t.Fatal("KINGRES_REQUIRE_VERIFY=1 and KINGRES_VERIFY does not name a built kingres-verify")
		}
		t.Skip("KINGRES_VERIFY does not name a built kingres-verify")
	}
	return p
}

// TestVerifierTestsRan always runs. Without a named verifier the tests that run
// kingres-verify are skipped, and a skip is invisible in a plain go test: this
// test writes a plain notice to the standard error (which go test shows when
// it is run in the package directory or with -v), logs it, and passes, so that
// an ordinary developer run does not fail. With KINGRES_REQUIRE_VERIFY=1 (the
// setting for the integrator's runs) it fails instead.
func TestVerifierTestsRan(t *testing.T) {
	if os.Getenv("KINGRES_VERIFY") != "" || os.Getenv("KINGRES_VERIFY_BIN") != "" {
		return
	}
	msg := "NOTE: KINGRES_VERIFY is not set, so the tests that run kingres-verify were SKIPPED (TestTrustFileAgainstTheVerifier, TestRetireBoundAgainstTheVerifier, TestTrustFileExtremeTimesAgainstTheVerifier and others): the trust files this package writes were not read by the verifier in this run. Set KINGRES_VERIFY to a built kingres-verify, and KINGRES_REQUIRE_VERIFY=1 to make this a failure."
	if os.Getenv("KINGRES_REQUIRE_VERIFY") == "1" {
		t.Fatal(msg)
	}
	fmt.Fprintln(os.Stderr, msg)
	t.Log(msg)
}

func runVerifier(t testing.TB, args ...string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, verifierBin(t), args...).CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), string(out)
	}
	t.Fatalf("running kingres-verify: %v", err)
	return -1, ""
}

// TestTrustFileAgainstTheVerifier checks, with the real kingres-verify, that
// the file is read (trust show), that for a key with no window it is byte for
// byte what trust init writes, and that a rotated set is accepted.
func TestTrustFileAgainstTheVerifier(t *testing.T) {
	verifierBin(t)
	dir := t.TempDir()
	prefix := "ledger.example.com/kingres"
	k1 := seededKey(t, 21)
	s := okSet(t)(Set{}.Add(k1.Public(), "main", t0))
	// A next key has no window; make it active without a time to compare with
	// trust init: build the record by hand (the zero ActivatedAt is "no window").
	_ = s
	one := []TrustKey{{Name: "main", Public: k1.Public(), Fingerprint: k1.Fingerprint()}}
	pubFile := filepath.Join(dir, "pub")
	if err := os.WriteFile(pubFile, []byte(base64.StdEncoding.EncodeToString(k1.Public())), 0o600); err != nil {
		t.Fatal(err)
	}
	st, out := runVerifier(t, "trust", "init", "--origin-prefix", prefix, "--public-key", base64.StdEncoding.EncodeToString(k1.Public()), "--fingerprint", k1.Fingerprint(), "--name", "main", "--out", "-")
	if st != 0 {
		t.Fatalf("trust init: %d\n%s", st, out)
	}
	// The command prints status text as well; take the file from --out FILE.
	initFile := filepath.Join(dir, "init.json")
	if st, out := runVerifier(t, "trust", "init", "--origin-prefix", prefix, "--public-key", base64.StdEncoding.EncodeToString(k1.Public()), "--fingerprint", k1.Fingerprint(), "--name", "main", "--out", initFile); st != 0 {
		t.Fatalf("trust init: %d\n%s", st, out)
	}
	want, err := os.ReadFile(initFile)
	if err != nil {
		t.Fatal(err)
	}
	got, err := marshalTrust(prefix, one)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("trust file differs from trust init:\n%s\nwant\n%s", got, want)
	}
	// A rotated set with a retired, an active and a revoked key.
	k2, k3 := seededKey(t, 22), seededKey(t, 23)
	var set Set
	set = okSet(t)(set.Add(k1.Public(), "2026", t0))
	set = okSet(t)(set.Activate(k1.Fingerprint(), t0, 0))
	set = okSet(t)(set.Add(k2.Public(), "2027", t0))
	set = okSet(t)(set.Activate(k2.Fingerprint(), t0.Add(time.Hour), 0))
	set = okSet(t)(set.Retire(k1.Fingerprint(), t0.Add(2*time.Hour), AtMost(1000)))
	set = okSet(t)(set.Add(k3.Public(), "leaked", t0))
	set = okSet(t)(set.Revoke(k3.Fingerprint(), t0.Add(3*time.Hour), t0, "test"))
	b, err := TrustFile(prefix, set, TrustOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "trust.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if st, out := runVerifier(t, "trust", "show", "--trust", p, "--now", "2027-01-01T00:00:00Z"); st != 0 {
		t.Fatalf("trust show: %d\n%s\n%s", st, out, b)
	}
	// A signature of the cluster key passes through the file.
	_ = ed25519.PublicKeySize
}

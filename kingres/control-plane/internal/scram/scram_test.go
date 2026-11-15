// SPDX-License-Identifier: AGPL-3.0-only

package scram

import (
	"strings"
	"testing"
)

// The format is checked here; that PostgreSQL accepts the verifier and
// authenticates with the password is checked by the integration tests, which
// create roles from generated verifiers and log in as them.
func TestVerifierFormat(t *testing.T) {
	salt := []byte{0x5b, 0x6d, 0x99, 0x68, 0x9d, 0x12, 0x35, 0x8e, 0xec, 0xa0, 0x4b, 0x14, 0x12, 0x36, 0xfa, 0x81}
	got, err := VerifierWithSalt("pencil", salt, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "SCRAM-SHA-256$4096:W22ZaJ0SNY7soEsUEjb6gQ==$") || strings.Count(got, ":") != 2 {
		t.Errorf("unexpected format %s", got)
	}
	again, _ := VerifierWithSalt("pencil", salt, 4096)
	if again != got {
		t.Error("not deterministic")
	}
}

func TestVerifierRejects(t *testing.T) {
	for _, p := range []string{"", "a\x00b", strings.Repeat("x", MaxPasswordBytes+1)} {
		if _, err := Verifier(p); err == nil {
			t.Errorf("accepted %q", p[:min(len(p), 5)])
		}
	}
	a, _ := Verifier("secret")
	b, _ := Verifier("secret")
	if a == b {
		t.Error("salt is not random")
	}
	if strings.Contains(a, "secret") {
		t.Error("verifier contains the password")
	}
}

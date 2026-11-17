// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"path/filepath"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/setup"
)

// A member that deleted its certificates and has not yet recorded that it is uninitialized (ResetMember
// does the first, then the second) used to fail every handshake with "tls: internal error", which the
// leader's call took for a server that cannot be reached. During the reset it presents the bootstrap
// certificate instead; a member that merely lost its certificates still presents nothing.
func TestInternalCertificateDuringAReset(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pki")
	state, resetting := setup.Clustered, false
	var seen string
	get := internalCertificate(dir, func() pki.Names { return pki.Names{CommonName: "127.0.0.9"} },
		func() setup.State { return state }, func() bool { return resetting }, func(fp string) { seen = fp })

	if _, err := get(nil); err == nil {
		t.Fatal("a member without a certificate must not offer a bootstrap certificate")
	}
	resetting = true
	pair, err := get(nil)
	if err != nil || pair == nil {
		t.Fatalf("during a reset the handshake must work with the bootstrap certificate: %v", err)
	}
	if seen == "" || seen != pki.Fingerprint(pair.Certificate[0]) {
		t.Fatalf("the bootstrap certificate was not reported: %q", seen)
	}
	resetting, state = false, setup.Uninitialized
	if again, err := get(nil); err != nil || pki.Fingerprint(again.Certificate[0]) != seen {
		t.Fatalf("an uninitialized server keeps the same bootstrap certificate: %v", err)
	}
}

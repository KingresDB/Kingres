// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fwdproof"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
)

func memberCert(t *testing.T, ca *pki.CA, id string) tls.Certificate {
	t.Helper()
	n := pki.Names{CommonName: id}
	key, csr, err := pki.NewKeyAndCSR(n)
	if err != nil {
		t.Fatal(err)
	}
	crt, _, err := ca.Sign(csr, n, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(crt, key)
	if err != nil {
		t.Fatal(err)
	}
	return pair
}

// attestRig is the member s1 that received the operator's request (it signs) and the held-back old leader s2 (it verifies and holds no verifier at all: it has no
// key source, as an old leader whose system database is stopped).
func attestRig(t *testing.T) (m *mapFailover, d *haDeps, s3 *fwdproof.Signer) {
	t.Helper()
	ca, err := pki.CreateCA(filepath.Join(t.TempDir(), "pki"), "t", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	signer := func(id string) *fwdproof.Signer {
		pair := memberCert(t, ca, id)
		return &fwdproof.Signer{Load: func() (tls.Certificate, error) { return pair, nil }}
	}
	m, _ = abandonRig(t) // the old leader is s2
	m.self = "s1"
	m.attest = signer("s1").Attest
	d = &haDeps{ServerHost: "s2", VerifyAttestation: (&fwdproof.AssertionVerifier{Certs: ca}).CheckAttestation}
	return m, d, signer("s3")
}

// Phase 5 review H1 and L2: the declaration that a server is lost reaches the old leader of the map group, which is held back and has no verifier to read (the key
// source of its instance is down), with the attestation of the member that received the operator's request, and is accepted from nothing else: a member that only
// claims it, an attestation made for another server, a replay, a changed server name, one signed by another member and a role below kingres_admin are refused
// before the hold is looked at.
func TestTheDeclarationThatAServerIsLostIsAcceptedAtAnOldLeaderThatCannotReadAVerifier(t *testing.T) {
	m, d, other := attestRig(t)
	id := opID
	id.Username = "alice"
	ctx := context.Background()
	make1 := func(lost, to string) mapResumeBody {
		b := mapResumeBody{Strict: true, Lost: lost}
		if err := m.signLost(id, &b, to); err != nil {
			t.Fatal(err)
		}
		return b
	}

	// attested by the member that received the call of an administrator, with no key source at the old leader
	if err := openLost(ctx, d, "s1", make1("s9", "s2")); err != nil {
		t.Fatalf("an attested declaration: %v", err)
	}
	// a member that claims it without an attestation
	callErr(t, openLost(ctx, d, "s3", mapResumeBody{Strict: true, Lost: "s9"}), 401, "identity_unproven")
	// made for another server of the cluster, or sent by another member than the one that signed
	callErr(t, openLost(ctx, d, "s1", make1("s9", "s5")), 401, "identity_unproven")
	callErr(t, openLost(ctx, d, "s3", make1("s9", "s2")), 401, "identity_unproven")
	// signed by a member that did not send the call
	b := mapResumeBody{Strict: true, Lost: "s9", Username: "alice", Roles: id.ClusterRoles, RequestID: "r"}
	a, err := other.Attest(lostMessage("s1", "s2", b))
	if err != nil {
		t.Fatal(err)
	}
	b.Attestation = &a
	callErr(t, openLost(ctx, d, "s1", b), 401, "identity_unproven")
	// the declared server, or the user, was changed after it was signed
	c := make1("s9", "s2")
	c.Lost = "s8"
	callErr(t, openLost(ctx, d, "s1", c), 401, "identity_unproven")
	c = make1("s9", "s2")
	c.Username = "mallory"
	callErr(t, openLost(ctx, d, "s1", c), 401, "identity_unproven")
	// a replay
	r := make1("s9", "s2")
	if err := openLost(ctx, d, "s1", r); err != nil {
		t.Fatal(err)
	}
	callErr(t, openLost(ctx, d, "s1", r), 401, "identity_unproven")
	// an operator below kingres_admin: the member does not sign for it, and a forged body that names a lower role carries no valid attestation
	low := id
	low.ClusterRoles = []string{"kingres_ops"}
	if err := m.signLost(low, &mapResumeBody{Lost: "s9"}, "s2"); err == nil {
		t.Fatal("a member attested the declaration of an operator below kingres_admin")
	}
	// a server that cannot verify refuses
	callErr(t, openLost(ctx, &haDeps{ServerHost: "s2"}, "s1", make1("s9", "s2")), 401, "identity_unproven")
	// and the handler asks for it
	d.MapFailover = m
	_, err = handleMapResume(ctx, d, "s3", []byte(`{"strict":true,"lost":"s9"}`))
	callErr(t, err, 401, "identity_unproven")
}

// The body that carries the declaration never holds a proof of the session (phase 5 review M-D): the operator's ClientKey stays with the member that
// authenticated the session.
func TestTheDeclarationCarriesNoProofOfTheSession(t *testing.T) {
	m, _, _ := attestRig(t)
	id := opID
	id.Username = "alice"
	b := mapResumeBody{Strict: true, Lost: "s9"}
	if err := m.signLost(id, &b, "s2"); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(b)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for k := range fields {
		if k == "proof" {
			t.Fatalf("the declaration carries a proof of the session: %s", raw)
		}
	}
}

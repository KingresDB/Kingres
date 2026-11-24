// SPDX-License-Identifier: AGPL-3.0-only

package fwdproof

import (
	"context"
	"crypto/tls"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
)

func lostMsg() Message {
	return Message{Kind: KindMapLost, From: "db2", To: "db1", Username: "alice", Roles: []string{"kingres_admin"},
		Operation: "abandonMapGroupBootstrap", ContentType: "application/json", Body: []byte(`{"lost":"db3"}`), RequestID: "r1"}
}

// An attestation is accepted from the server that sends the call, for exactly this request and this receiver, once, and not from another member or after a change.
func TestAMemberAttestationIsAcceptedOnlyFromTheSenderForThisRequest(t *testing.T) {
	ca, err := pki.CreateCA(filepath.Join(t.TempDir(), "pki"), "t", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	sender, other := serverPair(t, ca, "db2"), serverPair(t, ca, "db4")
	now := time.Now()
	v := &AssertionVerifier{Certs: ca, Now: func() time.Time { return now }} // no leader: the old leader of the map group has none
	ctx := context.Background()
	attest := func(pair tls.Certificate, m Message, at time.Time) Assertion {
		a, err := (&Signer{Load: func() (tls.Certificate, error) { return pair, nil }, Now: func() time.Time { return at }}).Attest(m)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	a := attest(sender, lostMsg(), now)
	if err := v.CheckAttestation(ctx, lostMsg(), a); err != nil {
		t.Fatalf("a valid attestation: %v", err)
	}
	if err := v.CheckAttestation(ctx, lostMsg(), a); !errors.Is(err, ErrReplay) {
		t.Errorf("a replay: %v", err)
	}
	for name, mutate := range map[string]func(*Message){
		"lost server": func(m *Message) { m.Body = []byte(`{"lost":"db5"}`) },
		"user":        func(m *Message) { m.Username = "root_admin" },
		"roles":       func(m *Message) { m.Roles = []string{"kingres_ops"} },
		"receiver":    func(m *Message) { m.To = "db9" },
		"request":     func(m *Message) { m.RequestID = "r2" },
		"kind":        func(m *Message) { m.Kind = KindForward },
	} {
		m := lostMsg()
		mutate(&m)
		if err := v.CheckAttestation(ctx, m, attest(sender, lostMsg(), now)); err == nil {
			t.Errorf("an attestation was accepted for another %s", name)
		}
	}
	// signed by a member that is not the sender of the call
	if err := v.CheckAttestation(ctx, lostMsg(), attest(other, lostMsg(), now)); !errors.Is(err, ErrInvalid) {
		t.Errorf("an attestation signed by another member: %v", err)
	}
	if err := v.CheckAttestation(ctx, lostMsg(), attest(sender, lostMsg(), now.Add(-3*time.Minute))); !errors.Is(err, ErrExpired) {
		t.Errorf("an old attestation: %v", err)
	}
	// an attestation is not an assertion of a gather call, and the other way round
	g := gatherMsg()
	if err := v.CheckAttestation(ctx, g, attest(sender, g, now)); err == nil {
		t.Error("an attestation of a gather message was accepted")
	}
	bad := attest(sender, lostMsg(), now)
	bad.Signature[len(bad.Signature)-1] ^= 1
	if err := v.CheckAttestation(ctx, lostMsg(), bad); !errors.Is(err, ErrInvalid) {
		t.Errorf("a damaged signature: %v", err)
	}
	lv := &AssertionVerifier{Certs: ca, Leader: func() string { return "db2" }, Now: func() time.Time { return now }}
	if err := lv.Check(ctx, lostMsg(), attest(sender, lostMsg(), now)); err == nil {
		t.Error("an attestation was accepted as the assertion of a gather call")
	}
}

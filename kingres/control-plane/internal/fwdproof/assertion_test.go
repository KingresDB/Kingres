// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package fwdproof

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
)

func serverPair(t *testing.T, ca *pki.CA, id string) tls.Certificate {
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

func gatherMsg() Message {
	return Message{Kind: KindGather, From: "db2", To: "db3", Username: "alice", Roles: []string{"kingres_ops"},
		Operation: "listLogs", Query: "limit=5", RequestID: "r1", Source: "panel", SourceAddress: "203.0.113.5", Ordinal: 3}
}

// A member accepts a gather call only with an assertion signed by the leader
// it knows, for exactly this request, from exactly this asking server.
func TestAssertionIsAcceptedOnlyFromTheLeaderForThisRequest(t *testing.T) {
	ca, err := pki.CreateCA(filepath.Join(t.TempDir(), "pki"), "t", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	leaderPair, memberPair := serverPair(t, ca, "db1"), serverPair(t, ca, "db4")
	now := time.Now()
	leader := "db1"
	v := &AssertionVerifier{Certs: ca, Leader: func() string { return leader }, Now: func() time.Time { return now }}
	ctx := context.Background()
	sign := func(pair tls.Certificate, m Message, at time.Time) Assertion {
		a, err := (&Signer{Load: func() (tls.Certificate, error) { return pair, nil }, Now: func() time.Time { return at }}).Sign(m)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}

	a := sign(leaderPair, gatherMsg(), now)
	// Every receiver gets the same assertion: the receiver and the ordinal are not part of it.
	other := gatherMsg()
	other.To, other.Ordinal = "db5", 9
	if err := v.Check(ctx, other, a); err != nil {
		t.Fatalf("a valid assertion: %v", err)
	}
	if err := v.Check(ctx, other, a); !errors.Is(err, ErrReplay) {
		t.Errorf("a replayed assertion: %v", err)
	}
	for name, mutate := range map[string]func(*Message){
		"user":      func(m *Message) { m.Username = "root_admin" },
		"roles":     func(m *Message) { m.Roles = []string{"kingres_admin"} },
		"operation": func(m *Message) { m.Operation = "listAlerts" },
		"query":     func(m *Message) { m.Query = "limit=500" },
		"asker":     func(m *Message) { m.From = "db4" }, // a compromised member that received the assertion
		"request":   func(m *Message) { m.RequestID = "r2" },
	} {
		m := gatherMsg()
		mutate(&m)
		if err := v.Check(ctx, m, sign(leaderPair, gatherMsg(), now)); err == nil {
			t.Errorf("an assertion was accepted for another %s", name)
		}
	}
	// A member's own certificate and key make no assertion: it is not the leader.
	forged := sign(memberPair, gatherMsg(), now)
	if err := v.Check(ctx, gatherMsg(), forged); !errors.Is(err, ErrInvalid) {
		t.Errorf("an assertion signed by a member: %v", err)
	}
	// The leader's identity is this server's own view: after a change of leader the old one's assertions stop.
	leader = "db4"
	if err := v.Check(ctx, gatherMsg(), sign(leaderPair, gatherMsg(), now)); !errors.Is(err, ErrInvalid) {
		t.Errorf("an assertion of a server that is no longer the leader: %v", err)
	}
	leader = "db1"
	if err := v.Check(ctx, gatherMsg(), sign(leaderPair, gatherMsg(), now.Add(-3*time.Minute))); !errors.Is(err, ErrExpired) {
		t.Errorf("an old assertion: %v", err)
	}
	// A signature that was altered.
	bad := sign(leaderPair, gatherMsg(), now)
	bad.Signature[len(bad.Signature)-1] ^= 1
	if err := v.Check(ctx, gatherMsg(), bad); !errors.Is(err, ErrInvalid) {
		t.Errorf("a damaged signature: %v", err)
	}
	// A revoked leader certificate is refused like any other.
	crt, err := x509.ParseCertificate(leaderPair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := ca.Revoke(pki.SerialHex(crt.SerialNumber), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := v.Check(ctx, gatherMsg(), sign(leaderPair, gatherMsg(), now)); !errors.Is(err, ErrInvalid) {
		t.Errorf("a revoked leader certificate: %v", err)
	}
}

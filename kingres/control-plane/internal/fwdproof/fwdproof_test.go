// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package fwdproof

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scram"
)

type keys map[string]string // role -> verifier

func (k keys) StoredKey(_ context.Context, user string) ([]byte, error) {
	v, ok := k[user]
	if !ok {
		return nil, errors.New("no such role")
	}
	p, err := scram.Parse(v)
	if err != nil {
		return nil, err
	}
	return p.StoredKey, nil
}

func setup(t *testing.T, password string) (clientKey []byte, verifier string) {
	t.Helper()
	verifier, err := scram.Verifier(password)
	if err != nil {
		t.Fatal(err)
	}
	p, err := scram.Parse(verifier)
	if err != nil {
		t.Fatal(err)
	}
	clientKey, err = p.ClientKey(password)
	if err != nil {
		t.Fatal(err)
	}
	return clientKey, verifier
}

func msg() Message {
	return Message{Kind: KindForward, From: "node2", To: "node1", Username: "alice", Roles: []string{"kingres_admin"},
		Operation: "createRole", PathParams: map[string]string{"a": "b"}, Query: "x=1", Body: []byte(`{"n":1}`), RequestID: "r1"}
}

func TestProofAcceptedOnceAndOnlyForItsRequest(t *testing.T) {
	ck, v := setup(t, "s3cret")
	now := time.Unix(1_800_000_000, 0)
	c := NewChecker(keys{"alice": v}, 0, func() time.Time { return now })
	p, err := Sign(ck, msg(), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Check(context.Background(), msg(), p); err != nil {
		t.Fatalf("a valid proof: %v", err)
	}
	if err := c.Check(context.Background(), msg(), p); !errors.Is(err, ErrReplay) {
		t.Fatalf("a replayed proof: %v", err)
	}
	// Every bound field: a proof for one request is worthless for another.
	for name, mutate := range map[string]func(*Message){
		"operation": func(m *Message) { m.Operation = "deleteRole" },
		"body":      func(m *Message) { m.Body = []byte(`{"n":2}`) },
		"query":     func(m *Message) { m.Query = "x=2" },
		"param":     func(m *Message) { m.PathParams = map[string]string{"a": "c"} },
		"user":      func(m *Message) { m.Username = "bob" },
		"roles":     func(m *Message) { m.Roles = nil },
		"sender":    func(m *Message) { m.From = "node3" },
		"receiver":  func(m *Message) { m.To = "node4" },
		"kind":      func(m *Message) { m.Kind = KindGather },
		"pin":       func(m *Message) { m.Pin = "aa" },
	} {
		p, _ := Sign(ck, msg(), now)
		m := msg()
		mutate(&m)
		if err := c.Check(context.Background(), m, p); err == nil {
			t.Errorf("a proof was accepted for a changed %s", name)
		}
	}
}

func TestProofNeedsThePassword(t *testing.T) {
	_, v := setup(t, "s3cret")
	other, _ := setup(t, "another") // somebody who only has a certificate and a guess
	now := time.Unix(1_800_000_000, 0)
	c := NewChecker(keys{"alice": v}, 0, func() time.Time { return now })
	p, _ := Sign(other, msg(), now)
	if err := c.Check(context.Background(), msg(), p); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a proof made without the password: %v", err)
	}
	// The verifier itself (StoredKey, ServerKey) does not make a proof either.
	parsed, _ := scram.Parse(v)
	p, _ = Sign(parsed.StoredKey, msg(), now)
	if err := c.Check(context.Background(), msg(), p); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a proof made from the verifier: %v", err)
	}
}

func TestProofWindowAndPasswordChange(t *testing.T) {
	ck, v := setup(t, "s3cret")
	now := time.Unix(1_800_000_000, 0)
	k := keys{"alice": v}
	c := NewChecker(k, time.Minute, func() time.Time { return now })
	old, _ := Sign(ck, msg(), now.Add(-2*time.Minute))
	if err := c.Check(context.Background(), msg(), old); !errors.Is(err, ErrExpired) {
		t.Fatalf("an old proof: %v", err)
	}
	future, _ := Sign(ck, msg(), now.Add(2*time.Minute))
	if err := c.Check(context.Background(), msg(), future); !errors.Is(err, ErrExpired) {
		t.Fatalf("a proof from the future: %v", err)
	}
	_, k["alice"] = setup(t, "changed") // the password was changed on the leader
	p, _ := Sign(ck, msg(), now)
	if err := c.Check(context.Background(), msg(), p); !errors.Is(err, ErrInvalid) {
		t.Fatalf("the old session after a password change: %v", err)
	}
	k["alice"] = "md5deadbeef"
	p, _ = Sign(ck, msg(), now)
	if err := c.Check(context.Background(), msg(), p); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a role without a SCRAM verifier: %v", err)
	}
	delete(k, "alice")
	if err := c.Check(context.Background(), msg(), p); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a role that does not exist: %v", err)
	}
}

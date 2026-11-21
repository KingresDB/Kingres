// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover/journal"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scram"
)

type storedKeys map[string][]byte

func (k storedKeys) StoredKey(_ context.Context, username string) ([]byte, error) {
	if v, ok := k[username]; ok {
		return v, nil
	}
	return nil, errors.New("no such role")
}

func proofRig(t *testing.T, key []byte) (*mapFailover, *haDeps) {
	t.Helper()
	m, _ := abandonRig(t) // self "s2"
	m.self = "s1"         // the member that received the request
	d := &haDeps{IdentityCheck: newIdentityChecker(storedKeys{"alice": scram.Stored(key)}, "s2")}
	return m, d
}

func callErr(t *testing.T, err error, status int, code string) {
	t.Helper()
	var ce *internalapi.CallError
	if !errors.As(err, &ce) || ce.Status != status || ce.Code != code {
		t.Fatalf("want %d %s, got %v", status, code, err)
	}
}

// F4-05. A request of an operator that a member sends to the candidate carries the proof of the operator's session over its bytes. The candidate takes the
// decision allowDataLoss and the author from the proven request, and from nothing else.
func TestAMapStartOfAnOperatorIsProvenAndAnythingElseIsRefused(t *testing.T) {
	key := make([]byte, sha256.Size)
	for i := range key {
		key[i] = byte(i + 1)
	}
	m, d := proofRig(t, key)
	id := internalapi.ForwardIdentity{Username: "alice", ClusterRoles: []string{"kingres_ops"}, ProofKey: key}
	send := func(body mapStartBody, id internalapi.ForwardIdentity) []byte {
		s, err := m.sign(id, body, "s2")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(s)
		return b
	}
	ctx := context.Background()

	// proven: the decision and the author are the operator's
	b, who, err := openMapStart(ctx, d, "s1", send(mapStartBody{Server: "s0", AllowDataLoss: true, RequestedBy: "mallory"}, id))
	if err != nil || who != "alice" || !b.AllowDataLoss || b.Server != "s0" {
		t.Fatalf("a proven request: %+v %q %v", b, who, err)
	}

	// the decision changed after it was signed
	raw := send(mapStartBody{Server: "s0", AllowDataLoss: false}, id)
	var s mapStartSigned
	_ = json.Unmarshal(raw, &s)
	s.Request = json.RawMessage(`{"server":"s0","requestedBy":"","allowDataLoss":true,"comment":""}`)
	tampered, _ := json.Marshal(s)
	_, _, err = openMapStart(ctx, d, "s1", tampered)
	callErr(t, err, 401, "identity_unproven")

	// the same proof again
	_, _, err = openMapStart(ctx, d, "s1", send(mapStartBody{Server: "s0"}, id))
	if err != nil {
		t.Fatal(err)
	}
	replayed := send(mapStartBody{Server: "s0"}, id)
	if _, _, err = openMapStart(ctx, d, "s1", replayed); err != nil {
		t.Fatal(err)
	}
	_, _, err = openMapStart(ctx, d, "s1", replayed)
	callErr(t, err, 401, "identity_unproven")

	// another server's name on the call: the proof was made for the sender
	_, _, err = openMapStart(ctx, d, "s3", send(mapStartBody{Server: "s0"}, id))
	callErr(t, err, 401, "identity_unproven")

	// a key that is not the user's
	wrong := id
	wrong.ProofKey = make([]byte, sha256.Size)
	_, _, err = openMapStart(ctx, d, "s1", send(mapStartBody{Server: "s0"}, wrong))
	callErr(t, err, 401, "identity_unproven")

	// a role that does not allow the failover
	ro := id
	ro.ClusterRoles = []string{"kingres_readonly"}
	_, _, err = openMapStart(ctx, d, "s1", send(mapStartBody{Server: "s0"}, ro))
	callErr(t, err, 403, "forbidden")

	// the plain form without a planned switchover, which is what a dishonest member would send
	plain, _ := json.Marshal(mapStartBody{Server: "s0", AllowDataLoss: true, RequestedBy: "alice"})
	_, _, err = openMapStart(ctx, d, "s1", plain)
	callErr(t, err, 401, "identity_unproven")

	// the plain form of the old leader's own task
	task, _ := json.Marshal(mapStartBody{Server: "s0", Planned: &journal.Planned{TaskID: 7, StandbyID: 3}})
	if b, who, err = openMapStart(ctx, d, "s0", task); err != nil || who != "" || b.Planned == nil {
		t.Fatalf("a planned request: %+v %q %v", b, who, err)
	}

	// no way to check a proof: refused
	_, _, err = openMapStart(ctx, &haDeps{}, "s1", send(mapStartBody{Server: "s0"}, id))
	callErr(t, err, 401, "identity_unproven")
}

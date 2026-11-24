// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"crypto/sha256"
	"testing"
)

// Phase 5 review L2: the declaration that a server is lost reaches the old leader of the map group with the proof of the operator's session and is
// accepted from nothing else: a member that only claims it, a proof made for another server, a replay, a changed server name and a role below
// kingres_admin are refused before the hold is looked at.
func TestTheDeclarationThatAServerIsLostIsProvenAtTheOldLeader(t *testing.T) {
	key := make([]byte, sha256.Size)
	for i := range key {
		key[i] = byte(i + 7)
	}
	m, d := proofRig(t, key) // the member that received the call is s1; the old leader is s2
	id := opID
	id.Username, id.ProofKey = "alice", key
	ctx := context.Background()
	make1 := func(lost, to string) mapResumeBody {
		b := mapResumeBody{Strict: true, Lost: lost}
		if err := m.signLost(id, &b, to); err != nil {
			t.Fatal(err)
		}
		return b
	}

	// proven by an administrator
	if err := openLost(ctx, d, "s1", make1("s9", "s2")); err != nil {
		t.Fatalf("a proven declaration: %v", err)
	}
	// a member that claims it without a proof
	callErr(t, openLost(ctx, d, "s3", mapResumeBody{Strict: true, Lost: "s9"}), 401, "identity_unproven")
	// the proof was made for another server of the cluster, or the call comes from another member
	callErr(t, openLost(ctx, d, "s1", make1("s9", "s5")), 401, "identity_unproven")
	callErr(t, openLost(ctx, d, "s3", make1("s9", "s2")), 401, "identity_unproven")
	// the declared server was changed after it was signed
	b := make1("s9", "s2")
	b.Lost = "s8"
	callErr(t, openLost(ctx, d, "s1", b), 401, "identity_unproven")
	// a replay
	r := make1("s9", "s2")
	if err := openLost(ctx, d, "s1", r); err != nil {
		t.Fatal(err)
	}
	callErr(t, openLost(ctx, d, "s1", r), 401, "identity_unproven")
	// an operator below kingres_admin
	low := id
	low.ClusterRoles = []string{"kingres_ops"}
	lb := mapResumeBody{Strict: true, Lost: "s9"}
	if err := m.signLost(low, &lb, "s2"); err != nil {
		t.Fatal(err)
	}
	callErr(t, openLost(ctx, d, "s1", lb), 403, "forbidden")
	// and the handler asks for it
	d.MapFailover = m
	_, err := handleMapResume(ctx, d, "s3", []byte(`{"strict":true,"lost":"s9"}`))
	callErr(t, err, 401, "identity_unproven")
}

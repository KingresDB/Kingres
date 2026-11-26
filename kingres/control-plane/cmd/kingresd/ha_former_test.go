// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clock"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/lease"
)

type oneServer struct{}

func (oneServer) Topology(context.Context) (lease.Topology, error) {
	return lease.Topology{Groups: []int32{1}, Members: []string{"a"}}, nil
}

// What the lease manager says before the directory of a former primary is rebuilt: nothing on disk holds it back (the
// primary was not fenced, handed over or kept waiting by the product): refused; held back: accepted; a lease that this
// server holds, or adopts, refuses it again.
func TestTheProofOfAFormerPrimaryNeedsAMarkerOnDiskAndNoLease(t *testing.T) {
	clk := clock.NewReal()
	dir := filepath.Join(tempDir(t), "lease")
	grantor, err := lease.NewGrantor(clk, 3*time.Second, dir)
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := lease.New(lease.Config{Self: "a", Clock: clk, Duration: 10 * time.Second, Renew: 2 * time.Second, Margin: 3 * time.Second,
		Grantor: grantor, Source: oneServer{}, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	proof := formerPrimaryProof(mgr)
	if err := proof(1); err == nil {
		t.Fatal("a primary that nothing held back was accepted as a former primary")
	}
	if err := mgr.Relinquish(2); err != nil {
		t.Fatal(err)
	}
	if err := proof(2); err != nil {
		t.Fatalf("a primary that the product held back: %v", err)
	}
	if err := proof(3); err == nil {
		t.Fatal("the marker of group 2 holds group 3 as well")
	}
	// This server holds the lease of group 1 (a one-server cluster grants itself): the proof is refused although a marker is there.
	mgr.Renew(context.Background())
	if !mgr.Holds(1) {
		t.Fatal("the one-server cluster does not hold the lease")
	}
	if err := mgr.Relinquish(1); err != nil {
		t.Fatal(err)
	}
	if err := proof(1); err == nil {
		t.Fatal("a group whose lease this server holds was accepted")
	}
	if err := mgr.Adopt(2, lease.AdoptFence); err != nil {
		t.Fatal(err)
	}
	if err := proof(2); err == nil {
		t.Fatal("a group whose lease this server adopts was accepted")
	}
}

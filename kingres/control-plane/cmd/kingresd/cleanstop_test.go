// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/lease"
)

type fakeStopper struct {
	stays   bool // the instance still runs after the stop (the stop failed)
	running bool
	cdErr   error
	state   string
}

func (f *fakeStopper) Stop(context.Context, int) error {
	if !f.stays {
		f.running = false
	}
	return nil
}
func (f *fakeStopper) Running(context.Context, int) (bool, error) { return f.running, nil }
func (f *fakeStopper) ControlData(context.Context, int) (map[string]string, error) {
	if f.cdErr != nil {
		return nil, f.cdErr
	}
	return map[string]string{"Database cluster state": f.state, "Latest checkpoint location": "0/6000028"}, nil
}

type fakeLeases struct{ holdBack, relinquish, release int }

func (f *fakeLeases) HoldBack(int32) error   { f.holdBack++; return nil }
func (f *fakeLeases) Relinquish(int32) error { f.relinquish++; return nil }
func (f *fakeLeases) ReleaseGroup(context.Context, int32) (lease.TransferResult, error) {
	f.release++
	return lease.TransferResult{Granted: 2, Need: 2}, nil
}

// F5-07. The clean stop holds the primary back and renews the lease until the stop is over; the renewal ends on every end: the lease is released after a clean stop,
// and Relinquish ends the renewal when the instance was not stopped, was not left clean, or its control data cannot be read (it is stopped and no answer can be given).
func TestTheRenewalEndsOnEveryEndOfTheCleanStop(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cases := []struct {
		name                string
		st                  *fakeStopper
		relinquish, release int
		wantErr             bool
	}{
		{"clean", &fakeStopper{running: true, state: "shut down"}, 0, 1, false},
		{"not stopped", &fakeStopper{running: true, stays: true}, 1, 0, false},
		{"not left clean", &fakeStopper{running: true, state: "in production"}, 1, 0, false},
		{"control data unreadable", &fakeStopper{running: true, cdErr: errors.New("no pg_controldata")}, 1, 0, true},
	}
	for _, c := range cases {
		lm := &fakeLeases{}
		ans, err := cleanStop(context.Background(), c.st, lm, nil, log, 1, 0)
		if lm.holdBack != 1 || lm.relinquish != c.relinquish || lm.release != c.release || (err != nil) != c.wantErr {
			t.Errorf("%s: holdBack %d relinquish %d release %d err %v (want relinquish %d release %d err %v)", c.name, lm.holdBack, lm.relinquish, lm.release, err, c.relinquish, c.release, c.wantErr)
		}
		if a, ok := ans.(failover.DemoteAnswer); c.name == "clean" && (!ok || !a.Stopped || !a.Released || a.Checkpoint == "") {
			t.Errorf("clean: %+v", ans)
		}
	}
}

// F5-12. The checkpoint of the stopped primary of the map group is given only while a marker holds it back, and never while it runs.
func TestTheCheckpointIsGivenOnlyForAHeldBackStoppedPrimary(t *testing.T) {
	ctx := context.Background()
	stopped := &fakeStopper{running: false, state: "shut down"}
	ans, err := mapCheckpoint(ctx, stopped, 1, true, true)
	if cp, ok := ans.(failover.MapCheckpoint); err != nil || !ok || cp.Checkpoint != "0/6000028" || cp.ClusterState != "shut down" {
		t.Fatalf("a held back stopped primary: %v %+v", err, ans)
	}
	_, err = mapCheckpoint(ctx, stopped, 1, false, true)
	callErr(t, err, 409, "not_primary")
	_, err = mapCheckpoint(ctx, stopped, 1, true, false)
	callErr(t, err, 409, "not_held_back")
	_, err = mapCheckpoint(ctx, &fakeStopper{running: true, state: "in production"}, 1, true, true)
	callErr(t, err, 409, "not_stopped")
	_, err = mapCheckpoint(ctx, &fakeStopper{running: false, cdErr: errors.New("no pg_controldata")}, 1, true, true)
	callErr(t, err, 503, "unavailable")
}

// The proof of a stop that was made is read without a stop and without any call on the lease: it is asked for again after the lease step (a resumed or
// replayed task), when this server holds the lease again as the fence holder and a release would give it away.
func TestTheProofOfAStopTouchesNoLeaseAndStopsNothing(t *testing.T) {
	ctx := context.Background()
	st := &fakeStopper{running: false, state: "shut down"}
	ans, err := stopProof(ctx, st, nil, 1)
	if a, ok := ans.(failover.DemoteAnswer); err != nil || !ok || !a.Stopped || a.ClusterState != "shut down" || a.Checkpoint != "0/6000028" || a.Released {
		t.Fatalf("the proof of a stopped instance: %v %+v", err, ans)
	}
	// An instance that runs is not stopped by a request for the proof: the answer says it is not stopped.
	run := &fakeStopper{running: true, state: "shut down"}
	ans, err = stopProof(ctx, run, nil, 1)
	if a, ok := ans.(failover.DemoteAnswer); err != nil || !ok || a.Stopped || !run.running {
		t.Fatalf("the proof of an instance that runs: %v %+v running=%v", err, ans, run.running)
	}
	// The control data that cannot be read is an error, not a proof.
	if _, err = stopProof(ctx, &fakeStopper{running: false, cdErr: errors.New("no pg_controldata")}, nil, 1); err == nil {
		t.Fatal("an unreadable control data was answered as a proof")
	}
}

// The request of the repeated stop reaches the proof and not the stop: the flag is read from the body of the call under its name, and the dispatch of the
// callee honours it. A request without the flag stops and releases as before.
func TestAProofOnlyRequestReachesTheProofAndNotTheCleanStop(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var proof, full failover.DemoteRequest
	if err := json.Unmarshal([]byte(`{"group":1,"mode":"fast","expectedTerm":3,"proofOnly":true}`), &proof); err != nil || !proof.ProofOnly {
		t.Fatalf("the flag is not read from the body of the call: %v %+v", err, proof)
	}
	if err := json.Unmarshal([]byte(`{"group":1,"mode":"fast","expectedTerm":3}`), &full); err != nil || full.ProofOnly {
		t.Fatalf("a request without the flag is a proof only: %v %+v", err, full)
	}
	lm := &fakeLeases{}
	ans, err := demoteOf(context.Background(), &fakeStopper{running: false, state: "shut down"}, lm, nil, log, 1, proof)
	if a, ok := ans.(failover.DemoteAnswer); err != nil || !ok || !a.Stopped || !a.ProofOnly || lm.holdBack+lm.relinquish+lm.release != 0 {
		t.Fatalf("a proof only request touched the lease or was not answered as one: %v %+v holdBack %d relinquish %d release %d", err, ans, lm.holdBack, lm.relinquish, lm.release)
	}
	lm = &fakeLeases{}
	ans, err = demoteOf(context.Background(), &fakeStopper{running: true, state: "shut down"}, lm, nil, log, 1, full)
	if a, ok := ans.(failover.DemoteAnswer); err != nil || !ok || !a.Stopped || !a.Released || a.ProofOnly || lm.holdBack != 1 || lm.release != 1 {
		t.Fatalf("a request without the flag did not stop and release: %v %+v holdBack %d release %d", err, ans, lm.holdBack, lm.release)
	}
}

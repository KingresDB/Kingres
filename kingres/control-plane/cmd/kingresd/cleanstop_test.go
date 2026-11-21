// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
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

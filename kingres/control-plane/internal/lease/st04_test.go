// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package lease

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// blockingInstance is a fake instance whose Start waits until it is released.
type blockingInstance struct {
	*fakeInstance
	entered chan struct{}
	release chan struct{}
}

func (b *blockingInstance) Start(ctx context.Context) error {
	close(b.entered)
	<-b.release
	return b.fakeInstance.Start(ctx)
}

// ST-04: a fence never waits behind a start of the primary that is under way, and a primary that comes up after the
// fence was written is stopped again: a start must not bring an instance back after a fence stopped it.
func TestAFenceDoesNotWaitForAStartAndTheStartDoesNotSurviveIt(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a"}, true)
	bi := &blockingInstance{fakeInstance: &fakeInstance{running: false}, entered: make(chan struct{}), release: make(chan struct{})}
	r.m.c.Instance = bi
	dir := filepath.Join(r.dir, "lease")
	if err := os.WriteFile(FencedPath(dir, 1), []byte("fenced\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.m.Renew(ctx) // wins the lease and starts the primary (blocked inside Start)
	select {
	case <-bi.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the primary was not started after the lease was won")
	}
	done := make(chan error, 1)
	go func() { done <- r.m.Fence(ctx, 1) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Fence: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the fence waited behind the start")
	}
	if !markerExists(dir, FencedMarker, 1) {
		t.Fatal("the fence wrote no marker")
	}
	close(bi.release)
	r.m.wg.Wait()
	if up, _, _ := bi.state(); up {
		t.Fatal("the instance that came up after the fence is still running")
	}
}

// slowStop is a fake instance whose immediate stop takes as long as the test wants.
type slowStop struct {
	*fakeInstance
	entered chan struct{}
	release chan struct{}
}

func (b *slowStop) StopImmediate(ctx context.Context) error {
	close(b.entered)
	<-b.release
	return b.fakeInstance.StopImmediate(ctx)
}

// A round that wins the lease while a fence is stopping the primary must not start the instance (it would find it still
// running and start nothing, the stop would end it, and the markers it removed would leave nothing to start it again:
// a primary that holds its lease and stays down). The next round, after the stop, starts it. This was the cause of a
// server whose instance stayed stopped after a lease that was lost and won again in the same moment.
func TestARoundThatWinsTheLeaseDuringAFenceStartsTheInstanceAfterTheStop(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a"}, true)
	si := &slowStop{fakeInstance: &fakeInstance{running: true}, entered: make(chan struct{}), release: make(chan struct{})}
	r.m.c.Instance = si
	dir := filepath.Join(r.dir, "lease")
	done := make(chan error, 1)
	go func() { done <- r.m.Fence(ctx, 1) }()
	select {
	case <-si.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the fence did not reach the stop")
	}
	r.m.Renew(ctx) // wins the lease while the stop is under way
	r.m.wg.Wait()
	if _, _, starts := si.state(); starts != 0 {
		t.Fatal("the instance was started while a fence was stopping it")
	}
	if !markerExists(dir, FencedMarker, 1) {
		t.Fatal("the fence marker was removed while the stop was under way")
	}
	close(si.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	r.m.Renew(ctx)
	r.m.wg.Wait()
	if up, _, _ := si.state(); !up {
		t.Fatal("the primary stayed down although the lease is held")
	}
	if markerExists(dir, FencedMarker, 1) {
		t.Fatal("the fence marker is still there after the start")
	}
}

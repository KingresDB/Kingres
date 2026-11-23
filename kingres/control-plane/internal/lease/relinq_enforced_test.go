// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package lease

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// P1 of the review of the hard failover of the map group: a primary that was relinquished and does not stop is stopped by its lapsed lease although the plan of the
// server no longer lists a standby of its group (a plan push during the failover), and the idle refresh never keeps its watchdog file alive.
func TestARelinquishedPrimaryIsFencedWhenThePlanListsNoStandby(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, true)
	r.m.Renew(ctx)
	if err := r.m.Relinquish(1); err != nil {
		t.Fatal(err)
	}
	*r.enf = false // the plan push emptied the local standby list
	r.clk.Advance(dur - margin + time.Millisecond)
	r.m.Check(ctx)
	if up, stops, _ := r.inst.state(); up || stops != 1 {
		t.Fatalf("a relinquished primary was not stopped by the lapsed lease: running=%v stops=%d", up, stops)
	}
}

func TestTheIdleRefreshNeverKeepsTheWatchdogFileOfARelinquishedGroupAlive(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, true)
	wf := filepath.Join(tempDir(t), "lease.json")
	r.m.c.WatchdogFileFor = func(int32) string { return wf }
	r.m.Renew(ctx)
	if err := r.m.Relinquish(1); err != nil {
		t.Fatal(err)
	}
	*r.enf = false
	_ = os.Remove(wf)
	r.m.touchIdle()
	if _, err := os.Stat(wf); err == nil {
		t.Fatal("the idle refresh wrote the watchdog file of a relinquished group")
	}
	// after a restart of kingresd: the memory is gone, the marker on disk is what remains
	r.m.mu.Lock()
	delete(r.m.relinq, 1)
	delete(r.m.groups, 1)
	r.m.mu.Unlock()
	if !r.m.enforced(1) {
		t.Fatal("a group with the hand-over marker on disk is not enforced after a restart")
	}
	r.m.touchIdle()
	if _, err := os.Stat(wf); err == nil {
		t.Fatal("the idle refresh wrote the file after a restart")
	}
	if err := r.m.Unrelinquish(1); err != nil {
		t.Fatal(err)
	}
	if r.m.enforced(1) {
		t.Fatal("the end of the hold must give the group back to the plan")
	}
}

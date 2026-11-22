// SPDX-License-Identifier: AGPL-3.0-only

package standby

import (
	"strings"
	"testing"
)

// After a failover of group 1 (primary was on b, standby on c is the primary now), the old primary's record names b
// as a standby that waits to be rebuilt. The reconcile loop rebuilds it (with pg_rewind); it does not make a new
// standby, and it does not remove it, whatever the ring says.
func TestDiffRoutesAnOldPrimaryToARebuild(t *testing.T) {
	l := NewLeader(&Leader{})
	top := topology{
		ring:   []Server{{ID: "a"}, {ID: "b"}, {ID: "c"}},
		groups: []groupInfo{{ID: 0, Primary: "a", Port: 5000}, {ID: 1, Primary: "c", Port: 5102}, {ID: 2, Primary: "c", Port: 5002}},
	}
	recs := []Record{
		{ID: 1, Group: 0, Server: "b", PrimaryServer: "a", State: stateReady},
		{ID: 7, Group: 1, Server: "b", PrimaryServer: "c", State: stateRebuilding, Diverged: true, NeedsRebuild: true},
		{ID: 3, Group: 2, Server: "a", PrimaryServer: "c", State: stateReady},
	}
	w := l.diff(top, recs, 1)
	if len(w.Rebuild) != 1 || w.Rebuild[0].ID != 7 {
		t.Errorf("rebuild: %+v", w.Rebuild)
	}
	if len(w.Resume) != 0 || len(w.Remove) != 0 || len(w.Add) != 0 {
		t.Errorf("resume %+v remove %+v add %+v", w.Resume, w.Remove, w.Add)
	}
}

// A server that is dead is no place for a new standby: the replacement of an old primary that did not return goes to
// the other server; a standby that is on a dead server already stays (it may only be rebooting).
func TestDiffPlacesNoNewStandbyOnADeadServer(t *testing.T) {
	dead := map[string]bool{"b": true}
	l := NewLeader(&Leader{Dead: func(s string) bool { return dead[s] }})
	top := topology{ring: []Server{{ID: "a"}, {ID: "b"}, {ID: "c"}}, groups: []groupInfo{{ID: 1, Primary: "a", Port: 5001}}}
	// group 1's primary is on a: the next server in the ring is b, which is dead, so the standby goes to c
	w := l.diff(top, nil, 1)
	if got := servers(w.Add); len(got) != 1 || got[0] != "c" {
		t.Errorf("replacement placed on %v, want c", got)
	}
	// a standby that exists on the dead server stays
	recs := []Record{{ID: 5, Group: 1, Server: "b", PrimaryServer: "a", State: stateReady}}
	if w := l.diff(top, recs, 1); !w.empty() {
		t.Errorf("a standby on a dead server was touched: %+v", w)
	}
}

// What the rewind itself printed completes a report whose dry run was refused (a primary that needed crash recovery).
func TestTheReportIsCompletedFromTheRewindItself(t *testing.T) {
	var info OldPrimaryInfo
	completeFromRewind(&info, "pg_rewind: servers diverged at WAL location 0/3000060 on timeline 1\npg_rewind: need to copy 3 MB (total source directory size is 40 MB)\n", "0/3100000", false)
	if info.DivergenceLsn != "0/3000060" || info.RewindBytes == nil || *info.RewindBytes != 3<<20 || info.DivergenceBytes == nil || *info.DivergenceBytes != 0x3100000-0x3000060 {
		t.Fatalf("%+v", info)
	}
	// a checkpoint before the divergence says nothing about the bytes
	var early OldPrimaryInfo
	completeFromRewind(&early, "pg_rewind: servers diverged at WAL location 0/3000060 on timeline 1\n", "0/2000000", false)
	if early.DivergenceBytes != nil || early.DivergenceLsn == "" || early.Message == "" {
		t.Fatalf("%+v", early)
	}
	// what the dry run measured is kept
	kept := OldPrimaryInfo{DivergenceLsn: "0/1"}
	completeFromRewind(&kept, "pg_rewind: servers diverged at WAL location 0/9 on timeline 1\n", "", false)
	if kept.DivergenceLsn != "0/1" {
		t.Fatalf("%+v", kept)
	}
}

// A directory that is shut down has finished its crash recovery, so its last checkpoint is the end of the old WAL: the bytes after the divergence are measured, and
// they are zero when that end is before the point where the histories parted (the new primary held more than the old one wrote). A directory that is not shut down
// gives a lower bound, or nothing when the checkpoint is before the divergence; the message says which.
func TestTheDivergenceIsMeasuredFromAShutDownDirectory(t *testing.T) {
	const rewind = "pg_rewind: servers diverged at WAL location 0/3000060 on timeline 1\n"
	var measured OldPrimaryInfo
	completeFromRewind(&measured, rewind, "0/3100000", true)
	if measured.DivergenceBytes == nil || *measured.DivergenceBytes != 0x3100000-0x3000060 || !strings.Contains(measured.Message, "measured from the shutdown checkpoint") {
		t.Fatalf("%+v", measured)
	}
	var none OldPrimaryInfo
	completeFromRewind(&none, rewind, "0/2000000", true)
	if none.DivergenceBytes == nil || *none.DivergenceBytes != 0 || !strings.Contains(none.Message, "wrote nothing after the divergence") {
		t.Fatalf("an old WAL that ends before the divergence: %+v", none)
	}
	var unknown OldPrimaryInfo
	completeFromRewind(&unknown, rewind, "0/2000000", false)
	if unknown.DivergenceBytes != nil || !strings.Contains(unknown.Message, "could not measure the old WAL") {
		t.Fatalf("a checkpoint before the divergence of a directory that was not shut down says nothing: %+v", unknown)
	}
	var lower OldPrimaryInfo
	completeFromRewind(&lower, rewind, "0/3100000", false)
	if lower.DivergenceBytes == nil || !strings.Contains(lower.Message, "at least") {
		t.Fatalf("%+v", lower)
	}
}

func TestOnlyAShutDownDirectoryHasItsCheckpointAsTheEndOfTheWal(t *testing.T) {
	for state, want := range map[string]bool{"shut down": true, "in production": false, "in crash recovery": false, "shut down in recovery": false, "": false} {
		if cp, shut := endOfOldWal(map[string]string{"Latest checkpoint location": "0/3100000", "Database cluster state": state}); cp != "0/3100000" || shut != want {
			t.Errorf("state %q: %q %v, want shut down %v", state, cp, shut, want)
		}
	}
}

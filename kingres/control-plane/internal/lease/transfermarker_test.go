// SPDX-License-Identifier: AGPL-3.0-only

package lease

import (
	"os"
	"testing"
)

// Phase 5 review L-4: kingresd ends between the marker "transferring to X" and the answer of the grantors. At the restart the
// marker is read again: when this server's own grantor still names this server as the holder (it never moved the lease), the
// transfer did not take effect here and the marker is dropped, so that the group is renewed again. When the grantor already names
// the target, the lease may be the target's and the group stays given up. Markers of a release or a hold-back are not touched.
func TestATransferringMarkerIsDroppedAtTheRestartWhenNoGrantorMovedTheLease(t *testing.T) {
	r := newRig(t, []string{"a", "b", "c"}, true)
	dir := r.m.c.Dir
	put := func(text string) {
		t.Helper()
		if err := os.WriteFile(MarkerPath(dir, RelinquishedMarker, 1), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reload := func() bool {
		r.m.mu.Lock()
		defer r.m.mu.Unlock()
		r.m.relinq = map[int32]bool{}
		r.m.loadRelinquished()
		return r.m.relinq[1]
	}
	// The lease is this server's at its own grantor: the transfer never reached it.
	if _, err := r.m.c.Grantor.Grant(1, "a", dur, false); err != nil {
		t.Fatal(err)
	}
	put("transferring to b\n")
	if reload() {
		t.Fatal("a transfer that moved nothing here kept the group given up after the restart")
	}
	if markerExists(dir, RelinquishedMarker, 1) {
		t.Fatal("the stale marker was left on disk (the start gate would still refuse the primary)")
	}
	// The same marker for a release stays: only a transfer is undone by what the grantor says.
	put("released\n")
	if !reload() || !markerExists(dir, RelinquishedMarker, 1) {
		t.Fatal("the marker of a release was dropped")
	}
	// The grantor already moved the lease to the target: the group stays given up.
	if _, err := r.m.c.Grantor.Transfer(1, "a", "b", dur); err != nil {
		t.Fatal(err)
	}
	put("transferring to b\n")
	if !reload() || !markerExists(dir, RelinquishedMarker, 1) {
		t.Fatal("the transfer reached this grantor, the group must stay given up")
	}
}

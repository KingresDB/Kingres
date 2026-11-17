// SPDX-License-Identifier: AGPL-3.0-only

//go:build !windows

package main

import (
	"os"
	"testing"
	"time"
)

// ST-01: kingresd tells the watchdog that it stopped enforcing the lease on purpose (the group has no
// standby any more): the watchdog must not fence on a file that is not written any more, and it fences
// again when a later file arms it.
func TestWatchdogDoesNotFenceAfterTheLeaseWasSwitchedOff(t *testing.T) {
	r := newWDRig(t) // its lease file runs out after 300 ms
	if err := os.WriteFile(r.lease, []byte(`{"seq": 2, "validMs": 0, "off": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r.start(t)
	// The negative observation: the old file would have been fenced after 300 ms; look for ten times that.
	if waitFor(3*time.Second, func() bool { return exists(r.marker) || exists(r.calls) }) {
		t.Fatal("the watchdog fenced although kingresd switched the lease off")
	}
	// Armed again: a normal file that then runs out is fenced.
	if err := os.WriteFile(r.lease, []byte(`{"seq": 3, "validMs": 300}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !waitFor(5*time.Second, func() bool { return exists(r.marker) && exists(r.calls) }) {
		t.Fatal("a file that arms the watchdog again must be fenced when it runs out")
	}
}

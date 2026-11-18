// SPDX-License-Identifier: AGPL-3.0-only

package lease

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// F2-07: the file of a group that is not enforced is refreshed by its own loop, not behind a round: while the rounds
// get no answer for a long time the file keeps changing, so the watchdog does not stop a healthy single-copy primary.
func TestTheIdleWatchdogFileIsRefreshedWithoutARound(t *testing.T) {
	r := newRig(t, []string{"a", "b", "c"}, false)
	wf := filepath.Join(t.TempDir(), "lease.json")
	if err := os.WriteFile(wf, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.m.c.WatchdogFileFor = func(int32) string { return wf }
	r.m.topo, r.m.haveT = Topology{Groups: []int32{1}, Members: []string{"a", "b", "c"}}, true
	read := func() WatchdogFile {
		b, err := os.ReadFile(wf)
		if err != nil {
			t.Fatal(err)
		}
		var f WatchdogFile
		if err := json.Unmarshal(b, &f); err != nil {
			t.Fatal(err)
		}
		return f
	}
	r.m.touchIdle() // no round has run
	first := read()
	if first.ValidMs <= 0 || first.Off {
		t.Fatalf("the idle file: %+v", first)
	}
	r.m.touchIdle()
	if second := read(); second.Seq == first.Seq {
		t.Fatal("the idle file did not change between two refreshes")
	}
	*r.enf = true
	before := read()
	r.m.touchIdle()
	if after := read(); after.Seq != before.Seq {
		t.Fatal("an enforced group's file must be written by its rounds only")
	}
}

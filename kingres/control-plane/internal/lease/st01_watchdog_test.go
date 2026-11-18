// SPDX-License-Identifier: AGPL-3.0-only

package lease

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func readWD(t *testing.T, p string) WatchdogFile {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var f WatchdogFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// ST-01: every change of whether a group is enforced is written to the watchdog's file at the next
// renewal, in both directions, before the last file can go stale; a restart of kingresd writes it again;
// and a group that never had a watchdog file does not get one.
func TestTheWatchdogFileFollowsEnforcementBothWays(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a"}, true)
	wf := filepath.Join(r.dir, "instance", "lease.json")
	if err := os.MkdirAll(filepath.Dir(wf), 0o700); err != nil {
		t.Fatal(err)
	}
	r.m.c.WatchdogFile = wf
	r.m.Renew(ctx)
	f1 := readWD(t, wf)
	if f1.Off || f1.ValidMs <= 0 {
		t.Fatalf("an enforced group must write a lease the watchdog counts down: %+v", f1)
	}
	*r.enf = false // the last standby is gone
	r.clk.Advance(2 * time.Second)
	r.m.Renew(ctx)
	f2 := readWD(t, wf)
	if f2.Off || f2.Seq == f1.Seq || f2.ValidMs != (dur-margin).Milliseconds() {
		t.Fatalf("the file of a group that is not enforced must be refreshed with a validity of duration minus margin (FO-01): %+v after %+v", f2, f1)
	}
	// Still not enforced: told again at every renewal (a restarted kingresd does not know what it wrote).
	r.clk.Advance(2 * time.Second)
	r.m.Renew(ctx)
	if f3 := readWD(t, wf); f3.Off || f3.Seq == f2.Seq || f3.ValidMs <= 0 {
		t.Fatalf("not told again: %+v", f3)
	}
	*r.enf = true
	r.clk.Advance(2 * time.Second)
	r.m.Renew(ctx)
	if f4 := readWD(t, wf); f4.Off || f4.ValidMs <= 0 {
		t.Fatalf("enforcement came back: the watchdog must be armed with a lease again: %+v", f4)
	}
	// A demotion disarms it explicitly (and the validity stays long for a watchdog that does not know the field, FO-05).
	r.m.DisarmWatchdog(1)
	if f5 := readWD(t, wf); !f5.Off || f5.ValidMs < time.Hour.Milliseconds() {
		t.Fatalf("DisarmWatchdog: %+v", f5)
	}
	// A group that never had a file gets none while it is not enforced.
	other := filepath.Join(r.dir, "never", "lease.json")
	if err := os.MkdirAll(filepath.Dir(other), 0o700); err != nil {
		t.Fatal(err)
	}
	r.m.c.WatchdogFile = other
	*r.enf = false
	r.clk.Advance(2 * time.Second)
	r.m.Renew(ctx)
	if _, err := os.Stat(other); err == nil {
		t.Fatal("a file was made for a group that is not enforced and never had a watchdog")
	}
}

// FO-01: a group that is not enforced keeps its file fresh at every pass, also when no majority renewed the lease
// (such a group does not lose its primary for that); a kingresd that stops writing leaves a file that runs out, so the
// watchdog fences (no "off" without expiry). Every file has a validity above zero, so that a watchdog of an earlier
// version, which ignores Off, never reads zero (FO-05).
func TestTheFileOfAGroupThatIsNotEnforcedIsRefreshedAtEveryPass(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, true)
	wf := filepath.Join(r.dir, "instance", "lease.json")
	if err := os.MkdirAll(filepath.Dir(wf), 0o700); err != nil {
		t.Fatal(err)
	}
	r.m.c.WatchdogFile = wf
	r.m.Renew(ctx)
	*r.enf = false
	r.voter.down["b"], r.voter.down["c"] = true, true // no majority: the round fails
	prev := readWD(t, wf)
	for i := 0; i < 3; i++ {
		r.clk.Advance(2 * time.Second)
		r.m.Renew(ctx)
		f := readWD(t, wf)
		if f.Seq == prev.Seq || f.Off || f.ValidMs <= 0 {
			t.Fatalf("pass %d: the file of a group that is not enforced was not refreshed: %+v after %+v", i, f, prev)
		}
		prev = f
	}
}

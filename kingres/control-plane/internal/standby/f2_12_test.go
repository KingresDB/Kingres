// SPDX-License-Identifier: AGPL-3.0-only

package standby

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// F2-12: the freeze is bound to the run that made it. A deposed leader's late unfreeze (the same task, an older epoch)
// is refused and the freeze of the run that took the task over stays.
func TestAFreezeIsBoundToTheEpochOfItsRun(t *testing.T) {
	a, _ := freezeAgent()
	if err := a.FreezeBy(1, time.Minute, 7, 5); err != nil {
		t.Fatal(err)
	}
	if err := a.UnfreezeBy(1, 7, 4); !errors.Is(err, ErrStaleFreeze) {
		t.Fatalf("an unfreeze of an older epoch: %v", err)
	}
	if !a.isFrozen(1) {
		t.Fatal("the late unfreeze lifted the freeze")
	}
	if err := a.FreezeBy(1, time.Minute, 7, 3); !errors.Is(err, ErrStaleFreeze) {
		t.Fatalf("a freeze of an older epoch: %v", err)
	}
	if err := a.FreezeBy(1, time.Minute, 7, 6); err != nil {
		t.Fatalf("a renewal of a newer epoch: %v", err)
	}
	if err := a.UnfreezeBy(1, 7, 6); err != nil || a.isFrozen(1) {
		t.Fatalf("the run that holds the freeze lifts it: %v", err)
	}
	// Another task replaces a freeze (a new failover of the group).
	_ = a.FreezeBy(2, time.Minute, 7, 9)
	if err := a.FreezeBy(2, time.Minute, 8, 1); err != nil {
		t.Fatalf("a freeze of another task: %v", err)
	}
}

// A freeze that cannot be written is reported to the caller, and the owner survives a restart.
func TestAFreezeThatCannotBeWrittenIsAnError(t *testing.T) {
	a, _ := freezeAgent()
	dir := t.TempDir()
	a.DataDir = filepath.Join(dir, "missing", "deeper") // the directory does not exist: the write fails
	if err := a.FreezeBy(1, time.Minute, 7, 5); err == nil {
		t.Fatal("the caller was told the freeze was kept although it could not be written")
	}
	if !a.isFrozen(1) {
		t.Fatal("a freeze that could not be written must still hold in memory (the safe direction)")
	}
	b, _ := freezeAgent()
	b.DataDir = dir
	if err := b.FreezeBy(3, time.Hour, 7, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, freezeFileName)); err != nil {
		t.Fatal("the freeze was not written")
	}
	c, _ := freezeAgent()
	c.DataDir = dir
	c.loadFreezes()
	if !c.isFrozen(3) {
		t.Fatal("the freeze did not survive a restart")
	}
	if err := c.UnfreezeBy(3, 7, 4); !errors.Is(err, ErrStaleFreeze) {
		t.Fatalf("the owner of a freeze did not survive a restart: %v", err)
	}
}

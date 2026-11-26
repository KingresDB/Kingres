// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package standby

import (
	"context"
	"errors"
	"testing"
	"time"
)

// F3-14: an unfreeze lifts only a freeze of its own task; a call of task 0 and a late call of another task do not.
func TestUnfreezeLiftsOnlyTheFreezeOfItsOwnTask(t *testing.T) {
	a, _ := freezeAgent()
	if err := a.FreezeBy(4, time.Minute, 7, 3); err != nil {
		t.Fatal(err)
	}
	if err := a.UnfreezeBy(4, 0, 0); !errors.Is(err, ErrStaleFreeze) || !a.isFrozen(4) {
		t.Fatalf("an unfreeze with task 0 lifted a freeze or was not refused: %v", err)
	}
	if err := a.UnfreezeBy(4, 8, 99); !errors.Is(err, ErrStaleFreeze) || !a.isFrozen(4) {
		t.Fatalf("an unfreeze of another task lifted the freeze or was not refused: %v", err)
	}
	if _, err := a.handleFreezeCall(context.Background(), CallUnfreeze, []byte(`{"group":4}`)); err == nil || !a.isFrozen(4) {
		t.Fatalf("an unfreeze call without a task was accepted: %v", err)
	}
	if err := a.UnfreezeBy(4, 7, 3); err != nil || a.isFrozen(4) {
		t.Fatalf("the owner could not lift its freeze: %v", err)
	}
}

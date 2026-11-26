// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package standby

import (
	"testing"
	"time"
)

// FO-02: a frozen group's list is never changed, also when no name is planned (the last standby is being rebuilt,
// its slot is lost, or a plan dropped it); unfrozen it is emptied at once.
func TestAFrozenListIsNotEmptiedWhenNoNameIsPlanned(t *testing.T) {
	in := SyncInput{Now: time.Second, Streaming: map[string]bool{}, Applied: "ANY 1 (kingres_standby_7)", Frozen: true, Grace: 30 * time.Second}
	var s SyncState
	if d := s.Decide(in); d.Set != nil {
		t.Errorf("a frozen list was changed to %q", *d.Set)
	}
	in.Frozen = false
	if d := s.Decide(in); d.Set == nil || *d.Set != "" {
		t.Errorf("an unfrozen group with no planned name must be emptied, got %v", d.Set)
	}
}

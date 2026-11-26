// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package standby

import (
	"testing"
	"time"
)

// F2-01: nothing changes synchronous_standby_names of a frozen group, whatever the plan and the streaming say.
func TestAFrozenListNeverChanges(t *testing.T) {
	for _, tc := range []struct {
		name      string
		planned   []string
		streaming map[string]bool
		applied   string
	}{
		{"a standby that streams replaces the name that was listed", []string{"kingres_standby_8"}, map[string]bool{"kingres_standby_8": true}, "ANY 1 (kingres_standby_7)"},
		{"a new standby widens the list", []string{"kingres_standby_7", "kingres_standby_8"}, map[string]bool{"kingres_standby_7": true, "kingres_standby_8": true}, "ANY 1 (kingres_standby_7)"},
		{"an empty list is given a name", []string{"kingres_standby_8"}, map[string]bool{"kingres_standby_8": true}, ""},
		{"a list is emptied after the grace", []string{"kingres_standby_7"}, map[string]bool{}, "ANY 1 (kingres_standby_7)"},
		{"a list is emptied when no standby is planned", nil, nil, "ANY 1 (kingres_standby_7)"},
	} {
		var st SyncState
		d := st.Decide(SyncInput{Now: time.Hour, Planned: tc.planned, Streaming: tc.streaming, Applied: tc.applied, Frozen: true, Grace: time.Second})
		if d.Set != nil {
			t.Errorf("%s: the frozen list was set to %q", tc.name, *d.Set)
		}
		// The same input unfrozen does change it (the freeze is what holds it).
		var st2 SyncState
		if d2 := st2.Decide(SyncInput{Now: time.Hour, Planned: tc.planned, Streaming: tc.streaming, Applied: tc.applied, Grace: time.Second}); d2.Set == nil && tc.name != "a list is emptied after the grace" {
			t.Errorf("%s: the unfrozen list does not change either; the case proves nothing", tc.name)
		}
	}
}

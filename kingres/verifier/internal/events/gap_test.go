// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package events

import (
	"strings"
	"testing"
)

// V8-11: a file with a number the walk did not reach is a gap.
func TestCheckGap(t *testing.T) {
	cases := []struct {
		count int
		names []string
		gap   bool
	}{
		{0, nil, false},
		{3, []string{"000000", "000001", "000002"}, false},
		{3, []string{"000000", "000001", "000002", "000004"}, true},
		{3, []string{"000005", "000004"}, true},
		{0, []string{"000001"}, true},
		{3, []string{"000000", "readme", "00001", "0000010", "00000a", ".000009"}, false},
		{3, []string{"000003"}, true}, // the walk stopped at 3 and 3 is there: it was not read (an error of the walk, not an absence)
	}
	for i, c := range cases {
		w := &Walk{Count: c.count}
		err := w.CheckGap(c.names)
		if (err != nil) != c.gap {
			t.Errorf("case %d: %v", i, err)
		}
		if err != nil && !strings.Contains(err.Error(), "without a gap") {
			t.Errorf("case %d: %v", i, err)
		}
	}
	w := &Walk{Count: 2}
	err := w.CheckGap([]string{"000006", "000004", "000000"})
	if err == nil || !strings.Contains(err.Error(), "event 000002 is absent and event 000004 is present") {
		t.Fatalf("%v", err)
	}
}

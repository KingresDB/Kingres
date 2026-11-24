// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package events

import (
	"strings"
	"testing"
)

func TestRestoreAcknowledgedText(t *testing.T) {
	r := RestoreAcknowledged{Seq: 3, Generation: 2, PublishedEvents: 9, PublishedLogs: 4, DatabaseEvents: 5, DatabaseLogs: 3, AcknowledgedBy: "operator", Reason: "restore"}
	got := r.Text()
	for _, want := range []string{"event 000003", "restore-acknowledged", "generation 2", "9 event(s) and 4 log(s) had been published", "held 5 event(s) and 3 log(s)"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q lacks %q", got, want)
		}
	}
}

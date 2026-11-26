// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// A lease with state none has a null holder, not an empty string (contract 0.9.0).
func TestGroupLeaseHolderIsNullWhenNone(t *testing.T) {
	b, _ := json.Marshal(GroupLease{State: "none"})
	if !strings.Contains(string(b), `"holder":null`) {
		t.Errorf("%s", b)
	}
	h := "db1"
	b, _ = json.Marshal(GroupLease{Holder: &h, State: "held"})
	if !strings.Contains(string(b), `"holder":"db1"`) {
		t.Errorf("%s", b)
	}
}

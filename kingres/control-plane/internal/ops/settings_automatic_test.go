// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package ops

import (
	"encoding/json"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
)

func autoUpdate(t *testing.T, body string) (map[string]any, int) {
	t.Helper()
	var a api.AutomaticSettingsUpdate
	if err := json.Unmarshal([]byte(body), &a); err != nil {
		t.Fatal(err)
	}
	set := map[string]any{}
	return set, len(automaticSettings(&a, set))
}

func TestAutomaticSettingsValidation(t *testing.T) {
	set, n := autoUpdate(t, `{"enabled": true, "threshold": 0.2, "settleSeconds": 0, "window": {"from": "22:00", "to": "06:00", "timeZone": "Europe/Berlin"}}`)
	if n != 0 || set["rebalance.automatic.enabled"] != true || set["rebalance.automatic.threshold"] != 0.2 || set["rebalance.automatic.settleSeconds"] != 0 {
		t.Fatalf("valid update: %d errors, %v", n, set)
	}
	if _, ok := set["rebalance.automatic.window"]; !ok {
		t.Fatal("the window is stored")
	}
	if set, n := autoUpdate(t, `{"window": null}`); n != 0 || set["rebalance.automatic.window"] != nil {
		t.Fatalf("a null window removes it: %d %v", n, set)
	} else if _, ok := set["rebalance.automatic.window"]; !ok {
		t.Fatal("null window must be written")
	}
	for _, bad := range []string{
		`{}`,
		`{"threshold": 0}`,
		`{"threshold": 1.5}`,
		`{"strategy": "x"}`,
		`{"checkIntervalSeconds": 1}`,
		`{"minIntervalSeconds": -1}`,
		`{"window": {"from": "25:00", "to": "06:00", "timeZone": "UTC"}}`,
		`{"window": {"from": "22:00", "to": "6:00", "timeZone": "UTC"}}`,
		`{"window": {"from": "22:00", "to": "06:00", "timeZone": "Mars/Base"}}`,
		`{"window": {"from": "22:00", "to": "06:00", "timeZone": ""}}`,
	} {
		if _, n := autoUpdate(t, bad); n == 0 {
			t.Errorf("%s must be refused", bad)
		}
	}
	var a api.AutomaticSettingsUpdate
	if err := json.Unmarshal([]byte(`{"unknown": 1}`), &a); err == nil {
		t.Error("an unknown member is refused")
	}
}

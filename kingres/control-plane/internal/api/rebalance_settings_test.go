// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package api

import (
	"encoding/json"
	"testing"
)

func TestRebalanceSettingsUpdateThreshold(t *testing.T) {
	var r RebalanceSettingsUpdate
	if err := json.Unmarshal([]byte(`{"threshold": null}`), &r); err != nil || !r.ThresholdSet || r.Threshold != nil {
		t.Fatalf("null: %v %+v", err, r)
	}
	r = RebalanceSettingsUpdate{}
	if err := json.Unmarshal([]byte(`{"threshold": 0.2, "strategy": "byDiskSize"}`), &r); err != nil || !r.ThresholdSet || *r.Threshold != 0.2 || *r.Strategy != "byDiskSize" {
		t.Fatalf("value: %v %+v", err, r)
	}
	r = RebalanceSettingsUpdate{}
	if err := json.Unmarshal([]byte(`{"maxConcurrentMoves": 3}`), &r); err != nil || r.ThresholdSet {
		t.Fatalf("absent: %v %+v", err, r)
	}
	if err := json.Unmarshal([]byte(`{"nonsense": 1}`), &RebalanceSettingsUpdate{}); err == nil {
		t.Fatal("an unknown member is refused")
	}
}

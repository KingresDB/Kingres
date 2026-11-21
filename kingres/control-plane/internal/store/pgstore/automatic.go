// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package pgstore

import (
	"encoding/json"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
)

// defaultAutomatic are the settings of the automatic rebalance of a cluster that never changed them (design 8.2, 8.3).
func defaultAutomatic() *api.AutomaticSettings {
	return &api.AutomaticSettings{Strategy: "byBucketCount", Threshold: 0.15, CheckIntervalSeconds: 60, SettleSeconds: 300,
		SettleAfterTopologySeconds: 600, MinIntervalSeconds: 3600, FailoverSettleSeconds: 600}
}

// applyAutomatic sets one stored member of the automatic settings.
func applyAutomatic(a *api.AutomaticSettings, key string, v []byte) {
	var n int
	var f float64
	var s string
	var b bool
	switch key {
	case "enabled":
		if json.Unmarshal(v, &b) == nil {
			a.Enabled = b
		}
	case "strategy":
		if json.Unmarshal(v, &s) == nil {
			a.Strategy = s
		}
	case "threshold":
		if json.Unmarshal(v, &f) == nil {
			a.Threshold = f
		}
	case "checkIntervalSeconds":
		if json.Unmarshal(v, &n) == nil {
			a.CheckIntervalSeconds = n
		}
	case "settleSeconds":
		if json.Unmarshal(v, &n) == nil {
			a.SettleSeconds = n
		}
	case "settleAfterTopologySeconds":
		if json.Unmarshal(v, &n) == nil {
			a.SettleAfterTopologySeconds = n
		}
	case "minIntervalSeconds":
		if json.Unmarshal(v, &n) == nil {
			a.MinIntervalSeconds = n
		}
	case "failoverSettleSeconds":
		if json.Unmarshal(v, &n) == nil {
			a.FailoverSettleSeconds = n
		}
	case "window":
		var w api.AutomaticWindow
		if string(v) != "null" && json.Unmarshal(v, &w) == nil {
			a.Window = &w
		} else {
			a.Window = nil
		}
	}
}

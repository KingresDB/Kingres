// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package api

import (
	"bytes"
	"encoding/json"
)

// AutomaticWindow is the time of day in which an automatic run may start (contract 0.15.0).
type AutomaticWindow struct {
	From     string `json:"from"`
	To       string `json:"to"`
	TimeZone string `json:"timeZone"`
}

// AutomaticSettings is RebalanceSettings.automatic.
type AutomaticSettings struct {
	Enabled                    bool             `json:"enabled"`
	Strategy                   string           `json:"strategy"`
	Threshold                  float64          `json:"threshold"`
	CheckIntervalSeconds       int              `json:"checkIntervalSeconds"`
	SettleSeconds              int              `json:"settleSeconds"`
	SettleAfterTopologySeconds int              `json:"settleAfterTopologySeconds"`
	MinIntervalSeconds         int              `json:"minIntervalSeconds"`
	FailoverSettleSeconds      int              `json:"failoverSettleSeconds"`
	Window                     *AutomaticWindow `json:"window"`
}

// AutomaticSettingsUpdate is the "automatic" member of a settings update; every member is optional.
type AutomaticSettingsUpdate struct {
	Enabled                    *bool            `json:"enabled,omitempty"`
	Strategy                   *string          `json:"strategy,omitempty"`
	Threshold                  *float64         `json:"threshold,omitempty"`
	CheckIntervalSeconds       *int             `json:"checkIntervalSeconds,omitempty"`
	SettleSeconds              *int             `json:"settleSeconds,omitempty"`
	SettleAfterTopologySeconds *int             `json:"settleAfterTopologySeconds,omitempty"`
	MinIntervalSeconds         *int             `json:"minIntervalSeconds,omitempty"`
	FailoverSettleSeconds      *int             `json:"failoverSettleSeconds,omitempty"`
	Window                     *AutomaticWindow `json:"window,omitempty"`
	// WindowSet tells that "window" was sent, also as null (no window: any time).
	WindowSet bool `json:"-"`
}

// UnmarshalJSON decodes strictly and records whether "window" was sent.
func (a *AutomaticSettingsUpdate) UnmarshalJSON(b []byte) error {
	type plain AutomaticSettingsUpdate
	var p plain
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	_, p.WindowSet = m["window"]
	*a = AutomaticSettingsUpdate(p)
	return nil
}

// UnmarshalJSON decodes the member strictly and records whether "threshold" was sent, because
// "threshold": null (take the strategy's default again) differs from no threshold.
func (r *RebalanceSettingsUpdate) UnmarshalJSON(b []byte) error {
	type plain RebalanceSettingsUpdate
	var p plain
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	_, p.ThresholdSet = m["threshold"]
	*r = RebalanceSettingsUpdate(p)
	return nil
}

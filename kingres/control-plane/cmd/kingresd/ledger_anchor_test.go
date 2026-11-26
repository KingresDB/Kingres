// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package main

import (
	"log/slog"
	"testing"
	"time"
)

func TestAnchorLimitsComeFromTheEnvironmentAndBadValuesAreIgnored(t *testing.T) {
	env := map[string]string{}
	get := func(k string) string { return env[k] }
	log := slog.New(slog.DiscardHandler)
	if l := anchorLimits(get, log); l.MaxAttempts != 0 || l.MaxAge != 0 {
		t.Errorf("defaults: %+v", l)
	}
	env["KINGRES_ANCHOR_MAX_RETRY_ATTEMPTS"], env["KINGRES_ANCHOR_MAX_RETRY_AGE_HOURS"] = "12", "5"
	if l := anchorLimits(get, log); l.MaxAttempts != 12 || l.MaxAge != 5*time.Hour {
		t.Errorf("set: %+v", l)
	}
	env["KINGRES_ANCHOR_MAX_RETRY_ATTEMPTS"], env["KINGRES_ANCHOR_MAX_RETRY_AGE_HOURS"] = "-1", "99999"
	if l := anchorLimits(get, log); l.MaxAttempts != 0 || l.MaxAge != 0 {
		t.Errorf("bad: %+v", l)
	}
}

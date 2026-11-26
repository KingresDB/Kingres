// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package standby

import (
	"testing"
	"time"
)

// A group that runs without a synchronous standby raises groupWithoutSynchronousStandby
// at the severity the REST contract gives it, critical: commits are acknowledged by one
// server only. A group that has its standby raises nothing and the kind is still
// reported as evaluated, so that an open alert can clear.
func TestProbeRaisesGroupWithoutSynchronousStandbyAsCritical(t *testing.T) {
	a := &Agent{Self: "s1", primaries: map[int32]*PrimaryStatus{
		1: {Without: true},
		2: {Without: false},
	}, slotWarn: map[int32]map[int64]int64{}}
	conds, evaluated := a.Probe(t.Context(), time.Time{})
	if len(conds) != 1 {
		t.Fatalf("want one condition (group 1), got %+v", conds)
	}
	c := conds[0]
	if c.Kind != "groupWithoutSynchronousStandby" || c.Severity != "critical" || c.Subject != "server:s1|group:1" {
		t.Fatalf("unexpected condition %+v", c)
	}
	if !evaluated["groupWithoutSynchronousStandby"] {
		t.Fatal("the kind must be reported as evaluated")
	}
}

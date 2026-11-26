// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package httpapi

import (
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// The reason a step was skipped is shown on that step (the contract's step state skipped).
func TestTaskToAPIShowsTheSkipReasonOnTheSkippedStep(t *testing.T) {
	out := taskToAPI(task.Task{ID: 3, Kind: "standby.reconcile", State: task.Done, Detail: "skipped: needs kingres_shard 0.5",
		Steps: []task.StepState{{Name: "a", State: task.StepDone}, {Name: "standby.register_in_catalog", State: task.StepSkipped}, {Name: "c", State: task.StepDone}}})
	steps := out.Progress.Steps
	if steps[1].State != "skipped" || steps[1].Detail == nil || *steps[1].Detail != "skipped: needs kingres_shard 0.5" {
		t.Errorf("steps %+v", steps)
	}
	if steps[0].Detail != nil || steps[2].Detail != nil {
		t.Errorf("the detail was put on other steps: %+v", steps)
	}
}

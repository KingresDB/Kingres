// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package rebalance

import (
	"context"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

func TestSplitChildren(t *testing.T) {
	// design 4.7, example 4: bucket 17 of a group of 128, two parts: 17 and 145, modulus 256
	c := SplitChildren(17, 128, 2)
	if len(c) != 2 || c[0].Bucket != 17 || c[1].Bucket != 145 || c[0].Modulus != 256 || c[1].Remainder != 145 {
		t.Fatalf("%+v", c)
	}
	// a second split of a child: bucket 145 (modulus 256) in four parts
	c = SplitChildren(145, 256, 4)
	want := []int{145, 401, 657, 913}
	for i, ch := range c {
		if ch.Bucket != want[i] || ch.Modulus != 1024 || ch.Remainder != want[i] {
			t.Errorf("child %d: %+v", i, ch)
		}
	}
}

func TestSplitRequestNormalize(t *testing.T) {
	r := SplitRequest{}
	if errs := r.normalize(); len(errs) != 0 || r.Parts != 2 || r.Verify != "count" {
		t.Fatalf("defaults: %v %+v", errs, r)
	}
	for _, bad := range []SplitRequest{{Parts: 3}, {Parts: 32}, {Parts: -2}, {Parts: 2, Verify: "full"}} {
		if len(bad.normalize()) != 1 {
			t.Errorf("%+v must be refused", bad)
		}
	}
	if SplitSubjectID("db", 3, 17) != "db/3/17" {
		t.Error("subject id")
	}
}

func TestSplitDone(t *testing.T) {
	for phase, want := range map[string]bool{"cleaning": true, "done": true, "switching": false, "copying": false, "failed": false} {
		if splitDone(phase) != want {
			t.Errorf("%s", phase)
		}
	}
}

// The request is normalized before the preview and before the task input is made: a start without verify must not record an empty one
// (the first cluster run of the split failed with "verify must be count, full or none, not ").
func TestSplitRefusesABadRequestBeforeTouchingTheCluster(t *testing.T) {
	s := &Service{}
	if _, err := s.Split(context.Background(), "u", "db", 1, 3, SplitRequest{Parts: 5}); err == nil {
		t.Fatal("parts 5 must be refused")
	}
}

func TestSplitCancelGuard(t *testing.T) {
	mk := func(state string) task.Task {
		return task.Task{Steps: []task.StepState{{Name: StepSplitCheck, State: task.StepDone}, {Name: StepSplitRun, State: state}}}
	}
	if !splitCancellable(mk(task.StepRunning)) || !splitCancellable(mk(task.StepPending)) {
		t.Error("a cancel is accepted while the split runs")
	}
	if splitCancellable(mk(task.StepDone)) {
		t.Error("nothing is left to cancel when the split is done")
	}
}

func TestSplitCancelTooLateAndCancelledAreTruthful(t *testing.T) {
	late := task.Task{Detail: splitTooLateText + " (switching)", Steps: []task.StepState{{Name: StepSplitRun, State: task.StepRunning}}}
	if splitCancellable(late) {
		t.Error("once the cancel came too late the engine must not end the task as cancelled")
	}
	var c task.Coded = splitCancelled{}
	if c.TaskCode() != task.CodeCanceled || c.TaskRetryable() || !strings.Contains(c.Error(), "unchanged") {
		t.Errorf("a split that was undone says so: %v", c)
	}
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package httpapi

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/contracttest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// The answers of the exits of a paused import are the Task of the contract: the 202 of a repeat of the import request (the task that was paused, pending again, with
// the detail that it had) and the 200 of the cancel of a paused committed import (failed, code cancelled, the message that names the servers).
func TestTheTasksOfThePausedImportExitsMatchTheContract(t *testing.T) {
	ck := contracttest.NewChecker(t)
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	steps := []task.StepState{{Name: "Verify", State: task.StepDone}, {Name: "Take the final name", State: task.StepPending}}
	resumed := task.Task{ID: 42, Kind: "database.import", State: task.Pending, SubjectType: "database", SubjectID: "shop", Steps: steps, Percent: 90,
		Detail:    "Repeat the same import request to finish it, or cancel the task to give it up. The import of shop is committed and not finished; nothing is undone.",
		CreatedAt: now, UpdatedAt: now, CreatedBy: "imp"}
	body, err := json.Marshal(taskToAPI(resumed))
	if err != nil {
		t.Fatal(err)
	}
	if err := ck.Schema("Task", body); err != nil {
		t.Errorf("the 202 of a repeat of the import request: %v\n%s", err, body)
	}
	ended := resumed
	ended.State = task.Failed
	ended.Error = &task.Error{Code: task.CodeCanceled, Message: "Ended by a cancel after the commit point; nothing was undone. Open on: s1. Not open on: s2."}
	body, err = json.Marshal(taskToAPI(ended))
	if err != nil {
		t.Fatal(err)
	}
	if err := ck.Schema("Task", body); err != nil {
		t.Errorf("the 200 of the cancel of a paused committed import: %v\n%s", err, body)
	}
}

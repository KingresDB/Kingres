// SPDX-License-Identifier: AGPL-3.0-only

package internalapi

import (
	"encoding/json"
	"time"
)

// The calls that let a server record the task of one of its own instance
// operations (start and stop) in the system database, which only the manager
// leader holds. They are answered only by the leader (policy ToLeader); the
// caller is any server of the cluster, and what it may record is limited by the
// leader to the kinds instance.start and instance.stop with an instance subject.

// TaskStepRecord is one step of a recorded task.
type TaskStepRecord struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

// TaskErrorRecord is the failure of a recorded task.
type TaskErrorRecord struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Step      string `json:"step,omitempty"`
	Retryable bool   `json:"retryable"`
}

// TaskRecord is the body of POST /task/record and POST /task/settle.
type TaskRecord struct {
	// ID is zero for a new task; with an id the task is written under it (an
	// announced or reserved id).
	ID          int64            `json:"id,omitempty"`
	Kind        string           `json:"kind"`
	State       string           `json:"state"`
	SubjectType string           `json:"subjectType"`
	SubjectID   string           `json:"subjectId"`
	CreatedBy   string           `json:"createdBy"`
	Input       json.RawMessage  `json:"input,omitempty"`
	Steps       []TaskStepRecord `json:"steps"`
	Percent     float64          `json:"percent"`
	Error       *TaskErrorRecord `json:"error,omitempty"`
	CreatedAt   time.Time        `json:"createdAt"`
	UpdatedAt   time.Time        `json:"updatedAt"`
}

// TaskRecordResult is the answer of POST /task/record and POST /task/reserve.
type TaskRecordResult struct {
	ID int64 `json:"id"`
}

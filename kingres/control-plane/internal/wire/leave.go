// SPDX-License-Identifier: AGPL-3.0-only

package wire

import "time"

// LeaveRequest is the call POST /leave-cluster from a server that leaves the
// cluster to the manager leader. It names no server: the server that leaves
// is the one whose certificate the call was made with, so a server cannot ask
// for the removal of another. Actor is the administrator who asked.
type LeaveRequest struct {
	Actor string `json:"actor"`
	Force bool   `json:"force,omitempty"`
}

// LeaveResponse is the answer of the leader: the task it started. The task
// lives on the leader; the leaving server turns this into its own answer.
type LeaveResponse struct {
	TaskID    int64     `json:"taskId"`
	Kind      string    `json:"kind"`
	State     string    `json:"state"`
	Steps     []string  `json:"steps"`
	CreatedAt time.Time `json:"createdAt"`
	CreatedBy string    `json:"createdBy"`
	// Leader is the id of the manager leader that answered.
	Leader string `json:"leader"`
}

// LeaveStatusRequest is the call POST /leave-status: the leaving server asks
// the leader how its own leave task stands, so that it resets itself only
// when the task has ended. The leader answers only for the task whose subject
// is the calling server.
type LeaveStatusRequest struct {
	TaskID int64 `json:"taskId"`
}

// LeaveStatusResponse is the state of the leave task.
type LeaveStatusResponse struct {
	State string `json:"state"`
}

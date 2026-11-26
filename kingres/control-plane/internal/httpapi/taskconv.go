// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package httpapi

import (
	"strconv"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// taskToAPI converts an engine task to the contract's Task.
func taskToAPI(t task.Task) api.Task {
	steps := make([]api.TaskStep, len(t.Steps))
	var current *string
	for i, st := range t.Steps {
		steps[i] = api.TaskStep{Name: st.Name, State: st.State}
		if st.State == task.StepRunning {
			n := st.Name
			current = &n
		}
	}
	out := api.Task{
		ID: t.ID, Kind: t.Kind, State: string(t.State),
		Progress:    api.TaskProgress{Percent: t.Percent, CurrentStep: current, Steps: steps},
		Subject:     &api.TaskSubject{Type: t.SubjectType, ID: t.SubjectID},
		Cancellable: task.Cancellable(t),
		CreatedAt:   t.CreatedAt.UTC(), UpdatedAt: t.UpdatedAt.UTC(), CreatedBy: t.CreatedBy,
	}
	if t.Detail != "" {
		d := t.Detail
		out.Detail = &d
		// The detail belongs to the step that runs; a task that is not running one
		// shows it on the step that was skipped last (the reason it was skipped).
		target := -1
		for i := range out.Progress.Steps {
			switch out.Progress.Steps[i].State {
			case task.StepRunning:
				target = i
			case task.StepSkipped:
				if target < 0 || out.Progress.Steps[target].State != task.StepRunning {
					target = i
				}
			}
		}
		if target >= 0 {
			out.Progress.Steps[target].Detail = &d
		}
	}
	if t.Error != nil {
		te := &api.TaskError{Code: t.Error.Code, Message: t.Error.Message}
		if t.Error.Step != "" {
			st := t.Error.Step
			te.Step = &st
		}
		r := t.Error.Retryable
		te.Retryable = &r
		out.Error = te
	}
	return out
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

// withCancelGuard narrows the cancellable flag of a task that the store read to what the cancel guard of its kind says now: the store
// knows only the state, and a guard looks at the steps and the detail (a schema move that is in its switch, a failover past its
// promotion). A kind without a guard keeps the flag.
func withCancelGuard(t api.Task) api.Task {
	if !t.Cancellable {
		return t
	}
	g := task.Task{Kind: t.Kind, State: task.State(t.State)}
	if t.Detail != nil {
		g.Detail = *t.Detail
	}
	for _, s := range t.Progress.Steps {
		g.Steps = append(g.Steps, task.StepState{Name: s.Name, State: s.State})
	}
	t.Cancellable = task.Cancellable(g)
	return t
}

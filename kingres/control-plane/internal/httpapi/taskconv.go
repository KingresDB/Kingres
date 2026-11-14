// SPDX-License-Identifier: PostgreSQL

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
		Cancellable: t.State == task.Pending || t.State == task.Running,
		CreatedAt:   t.CreatedAt.UTC(), UpdatedAt: t.UpdatedAt.UTC(), CreatedBy: t.CreatedBy,
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

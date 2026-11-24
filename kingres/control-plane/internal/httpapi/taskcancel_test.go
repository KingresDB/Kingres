// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package httpapi

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// The read of a task says what the cancel guard of its kind says now, not only that the task is unfinished: the console shows its
// Cancel button from this flag (a schema move that is in its switch answers task_not_cancellable to the request).
func TestTheReadOfATaskFollowsTheCancelGuard(t *testing.T) {
	guard := func(tk task.Task) bool { return !strings.HasPrefix(tk.Detail, "switching") }
	def := task.Definition{Kind: "test.guarded", Cancellable: guard, Steps: []task.Step{{Name: "x", Run: func(context.Context, *task.Env) error { return nil }}}}
	if _, err := task.NewEngine(task.NewMemStore(), []task.Definition{def}, task.Options{Holder: "A", Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	detail := func(s string) *string { return &s }
	cases := []struct {
		name string
		in   api.Task
		want bool
	}{
		{"before the point", api.Task{Kind: "test.guarded", State: "running", Cancellable: true, Detail: detail("copying: rows")}, true},
		{"past the point", api.Task{Kind: "test.guarded", State: "running", Cancellable: true, Detail: detail("switching: waiting")}, false},
		{"no detail", api.Task{Kind: "test.guarded", State: "running", Cancellable: true}, true},
		{"the store said no", api.Task{Kind: "test.guarded", State: "done", Cancellable: false}, false},
		{"a kind without a guard keeps the flag", api.Task{Kind: "test.unguarded", State: "running", Cancellable: true, Detail: detail("switching")}, true},
	}
	for _, c := range cases {
		if got := withCancelGuard(c.in).Cancellable; got != c.want {
			t.Errorf("%s: cancellable %v, want %v", c.name, got, c.want)
		}
	}
}

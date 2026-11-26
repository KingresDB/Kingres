// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package task_test

import (
	"context"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fence"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// The context of a step carries the run's fencing token, so that every remote call
// the step makes names the task and the lease epoch of the run.
func TestStepContextCarriesTheFence(t *testing.T) {
	clk := newClock()
	st := task.NewMemStoreClock(clk.Now)
	var got fence.Token
	var ok bool
	var epoch int64
	d := task.Definition{Kind: "test.kind", Steps: []task.Step{{Name: "work", Run: func(ctx context.Context, env *task.Env) error {
		got, ok = fence.From(ctx)
		epoch = env.Fence()
		return nil
	}}}}
	e := engine(t, st, "A", clk, d)
	tk, err := e.Submit(context.Background(), "test.kind", task.Subject{Type: "x", ID: "1"}, nil, "u")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !ok || got.Task != tk.ID || got.Epoch != epoch || epoch < 1 {
		t.Fatalf("token %+v (present %v), task %d, env.Fence() %d", got, ok, tk.ID, epoch)
	}
	_ = time.Second
}

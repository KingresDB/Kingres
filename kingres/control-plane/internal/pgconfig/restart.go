// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package pgconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

type restartInput struct {
	Actor string         `json:"actor"`
	Order []restartEntry `json:"order"`
}

type restartEntry struct {
	Server   string `json:"server"`
	Instance int    `json:"instance"`
	Role     string `json:"role"`
}

// StartRestart starts the task config.restart: it restarts the instances that wait for a restart,
// in an order that never leaves a standby below its primary, and stops at the first failure.
func (l *Leader) StartRestart(ctx context.Context, actor string, req api.RestartPendingRequest) (task.Task, error) {
	if !req.AcknowledgeDowntime {
		return task.Task{}, problem.Validation("The downtime must be acknowledged.", problem.FieldError{Field: "/acknowledgeDowntime", Message: "must be true: restarting a primary is downtime for its shard group", Code: "required"})
	}
	sn, err := l.snapshot(ctx)
	if err != nil {
		return task.Task{}, serviceDown(err)
	}
	only := map[string]bool{}
	for _, s := range req.ServerIDs {
		only[s] = true
	}
	pending := map[string]bool{}
	for _, s := range sn.servers {
		st := sn.states[s.ID]
		if st == nil || len(only) > 0 && !only[s.ID] {
			continue
		}
		for _, in := range st.Instances {
			if len(in.Pending) > 0 {
				pending[s.ID] = true
			}
		}
	}
	if len(pending) == 0 {
		return task.Task{}, problem.New(problem.NothingPending, "No instance waits for a restart.")
	}
	order, conflict := restartOrder(sn, pending, pendingValues(sn))
	if conflict != "" {
		return task.Task{}, problem.New(problem.ConfigRestartOrder, conflict)
	}
	var in restartInput
	in.Actor = actor
	for _, st := range order {
		if !instancePending(sn, st.Server, st.Instance) {
			continue
		}
		in.Order = append(in.Order, restartEntry{Server: st.Server, Instance: st.Instance, Role: st.Role})
	}
	return l.startConfigTask(ctx, KindRestart, SubjectType, SubjectRestart, in, actor)
}

func instancePending(sn *snapshot, server string, inst int) bool {
	st := sn.states[server]
	if st == nil {
		return false
	}
	for _, in := range st.Instances {
		if in.ID == inst {
			return len(in.Pending) > 0
		}
	}
	return false
}

// pendingValues returns what a restart will bring for the five standby settings: the pending value
// of a server's primary instance where there is one, else its running value.
func pendingValues(sn *snapshot) map[string]map[string]*big.Rat {
	out := map[string]map[string]*big.Rat{}
	for _, name := range fiveSettings {
		out[name] = map[string]*big.Rat{}
		for _, s := range sn.servers {
			st := sn.states[s.ID]
			if st == nil {
				continue
			}
			for _, in := range st.Instances {
				if in.ID != 1 {
					continue
				}
				v, ok := in.Values[name+"/pending"]
				if !ok {
					v, ok = in.Values[name]
				}
				if ok {
					if r, ok := new(big.Rat).SetString(v); ok {
						out[name][s.ID] = r
					}
				}
			}
		}
	}
	return out
}

func (l *Leader) restartSteps(input json.RawMessage) ([]task.Step, error) {
	var in restartInput
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, fmt.Errorf("pgconfig: the task input is not valid: %w", err)
	}
	var steps []task.Step
	for _, e := range in.Order {
		steps = append(steps, task.Step{Name: fmt.Sprintf("Restart instance %d (%s) on server %s", e.Instance, e.Role, e.Server),
			Run: func(ctx context.Context, env *task.Env) error {
				// Not idempotent in effect but safe to repeat: an instance that restarted already has nothing pending.
				if !l.pendingNow(ctx, e.Server, e.Instance) {
					return nil
				}
				if err := l.callServer(ctx, e.Server, CallRestart, RestartRequest{Instance: e.Instance, IfPending: true}, nil); err != nil {
					return fmt.Errorf("instance %d on server %s: %w", e.Instance, e.Server, err)
				}
				return nil
			}})
	}
	if len(steps) == 0 {
		steps = append(steps, task.Step{Name: "Nothing to restart", Run: func(context.Context, *task.Env) error { return nil }})
	}
	return steps, nil
}

func (l *Leader) pendingNow(ctx context.Context, server string, inst int) bool {
	var st StateResponse
	c, cancel := context.WithTimeout(ctx, l.GatherTimeout)
	defer cancel()
	if err := l.callServer(c, server, CallState, StateRequest{}, &st); err != nil {
		return true // cannot tell: restart
	}
	for _, in := range st.Instances {
		if in.ID == inst {
			return len(in.Pending) > 0 || !in.Running
		}
	}
	return false
}

var _ = sort.Strings

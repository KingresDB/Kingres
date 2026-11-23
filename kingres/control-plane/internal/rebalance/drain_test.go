// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package rebalance

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

func codeOf(t *testing.T, err error) problem.Code {
	t.Helper()
	var p *problem.Problem
	if !errors.As(err, &p) {
		t.Fatalf("not a problem: %v", err)
	}
	return p.Code
}

func TestDrainRefusals(t *testing.T) {
	three := []string{"a", "b", "c"}
	cases := []struct {
		name   string
		req    DrainRequest
		f      DrainFacts
		status string
		want   problem.Code
	}{
		{"ok", DrainRequest{}, DrainFacts{Servers: three, Receivers: []string{"b", "c"}}, "live", ""},
		{"last server", DrainRequest{}, DrainFacts{Servers: []string{"a"}}, "live", problem.LastServer},
		{"dead server", DrainRequest{}, DrainFacts{Servers: three, Receivers: []string{"b"}}, "dead", problem.NodeStateInvalid},
		{"nobody receives", DrainRequest{}, DrainFacts{Servers: three}, "live", problem.NoReceivingServer},
		{"map group", DrainRequest{}, DrainFacts{Servers: three, Receivers: []string{"b"}, HostsMapGroup: true}, "live", problem.ServerHostsMapGroup},
		{"replicas", DrainRequest{}, DrainFacts{Servers: three, Receivers: []string{"b", "c"}, ReplicaCount: 2}, "live", problem.ReplicasCannotBeKept},
		{"replicas accepted", DrainRequest{AcceptFewerReplicas: true}, DrainFacts{Servers: three, Receivers: []string{"b", "c"}, ReplicaCount: 2}, "live", ""},
		{"replicas fit", DrainRequest{}, DrainFacts{Servers: []string{"a", "b", "c", "d"}, Receivers: []string{"b"}, ReplicaCount: 2}, "live", ""},
		{"degraded", DrainRequest{}, DrainFacts{Servers: three, Receivers: []string{"b"}, ReplicaCount: 1, Degraded: []int32{2}}, "live", problem.GroupDegraded},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &Service{ServerStatus: func(context.Context, string) string { return c.status }}
			err := s.drainRefusal(context.Background(), "a", c.req, c.f)
			if c.want == "" {
				if err != nil {
					t.Fatalf("want no refusal, got %v", err)
				}
				return
			}
			if err == nil || codeOf(t, err) != c.want {
				t.Fatalf("want %s, got %v", c.want, err)
			}
		})
	}
}

func TestDrainStepsAreSkippedByAPlainRebalance(t *testing.T) {
	s := &Service{}
	for name, f := range map[string]func(context.Context, *PGRows, Options) error{
		"flag":  func(ctx context.Context, r *PGRows, o Options) error { return s.stepDrainFlag(ctx, nil, r, o) },
		"empty": func(ctx context.Context, r *PGRows, o Options) error { return s.stepDrainEmpty(ctx, nil, r, o) },
		"end":   func(ctx context.Context, r *PGRows, o Options) error { return s.stepDrainEnd(ctx, nil, r, o) },
	} {
		err := f(context.Background(), nil, Options{})
		if err == nil {
			t.Errorf("%s: a rebalance must skip the drain step", name)
		}
	}
	// A drain that keeps the server (no removeWhenDone) skips the last step too.
	if err := s.stepDrainEnd(context.Background(), nil, nil, Options{DrainServer: "a"}); err == nil {
		t.Error("the removal is skipped without removeWhenDone")
	}
}

func TestPlanAddsTheDrainedServerToTheExclusion(t *testing.T) {
	o := Options{DrainServer: "db1", ExcludeServers: []string{"db2"}}
	// the same rule as PG.Plan applies, tested on the options (the SQL is covered by the cluster tests)
	if needsExtension08(o) != true {
		t.Fatal("a drain needs plan_rebalance")
	}
}

func TestDrainStandbyStep(t *testing.T) {
	ctx := context.Background()
	s := &Service{}
	// not a drain, or no standby management: skipped
	if err := s.stepDrainStandbys(ctx, nil, nil, Options{}); err == nil {
		t.Error("a rebalance skips the step")
	}
	if err := s.stepDrainStandbys(ctx, nil, nil, Options{DrainServer: "a"}); err == nil {
		t.Error("a build without the hook skips the step")
	}
	// the standbys leave in two rounds
	left := []int{2, 1, 0}
	calls := 0
	s = &Service{DrainWait: time.Minute, DrainPoll: time.Millisecond, RelocateStandbys: func(context.Context, string, bool) (int, error) {
		n := left[min(calls, len(left)-1)]
		calls++
		return n, nil
	}}
	if err := s.stepDrainStandbys(ctx, nil, nil, Options{DrainServer: "a"}); err != nil || calls != 3 {
		t.Fatalf("relocation in rounds: %v after %d calls", err, calls)
	}
	// nobody can take them: without acceptFewerReplicas the step fails with its code, with it the step ends
	forced := 0
	stuck := &Service{DrainWait: 5 * time.Millisecond, DrainPoll: time.Millisecond, RelocateStandbys: func(_ context.Context, _ string, force bool) (int, error) {
		if force {
			forced++
		}
		return 1, nil
	}}
	err := stuck.stepDrainStandbys(ctx, nil, nil, Options{DrainServer: "a"})
	if err == nil || codeOf(t, err) != problem.ReplicasCannotBeKept {
		t.Fatalf("stuck standbys: %v", err)
	}
	if forced != 0 {
		t.Fatal("the server is not forced out of the ring when fewer standbys are not accepted")
	}
	if err := stuck.stepDrainStandbys(ctx, nil, nil, Options{DrainServer: "a", AcceptFewerReplicas: true}); err != nil {
		t.Fatalf("accepted: %v", err)
	}
	if forced != 1 {
		t.Fatalf("with acceptFewerReplicas the server is forced out of the ring once at the bound: %d", forced)
	}
	// a failure of the hook is the failure of the step
	boom := &Service{RelocateStandbys: func(context.Context, string, bool) (int, error) { return 0, errors.New("boom") }}
	if err := boom.stepDrainStandbys(ctx, nil, nil, Options{DrainServer: "a"}); err == nil {
		t.Fatal("the error of the hook")
	}
}

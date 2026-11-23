// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package rebalance

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

// lagEnv is a system database with the catalog table of the extension (three groups, a standby each) and a service whose lag source
// is a map.
func lagEnv(t *testing.T, lags map[int32]int64, fail map[int32]bool) (*Service, *guardEnv) {
	t.Helper()
	e, _ := newGuardEnv(t)
	ctx := context.Background()
	for _, q := range []string{
		`CREATE SCHEMA IF NOT EXISTS kingres`,
		`CREATE TABLE kingres.node (node_id int PRIMARY KEY, group_id int, host text, role text, is_active boolean, should_have_shards boolean)`,
		`INSERT INTO kingres.node VALUES (1, 0, 'a', 'primary', true, true), (2, 0, 'b', 'standby', true, true),
			(3, 1, 'a', 'primary', true, true), (4, 1, 'b', 'standby', true, true),
			(5, 2, 'b', 'primary', true, true), (6, 2, 'a', 'standby', true, true)`,
	} {
		if _, err := e.sys.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	s := &Service{Conns: e, Backend: &PG{Conns: e}, StandbyLag: func(_ context.Context, g int32) (int64, error) {
		if fail[g] {
			return 0, errors.New("the primary does not answer")
		}
		return lags[g], nil
	}}
	return s, e
}

func facts(t *testing.T, s *Service) ClusterFacts {
	t.Helper()
	f, err := s.clusterFacts(context.Background(), nil, DefaultAutoSettings(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// The lag rule reads the lag of every group with a primary and the limit of the cluster settings (default 64 MiB).
func TestClusterFactsNameTheGroupsThatLagBeyondTheLimit(t *testing.T) {
	s, e := lagEnv(t, map[int32]int64{0: 10, 1: 64<<20 + 1, 2: 64 << 20}, nil)
	if f := facts(t, s); !reflect.DeepEqual(f.GroupsLagging, []int32{1}) || f.ReplicaCount != 1 {
		t.Fatalf("default limit: lagging %v, replicas %d", f.GroupsLagging, f.ReplicaCount)
	}
	// the limit of the cluster settings applies
	if _, err := e.sys.Exec(context.Background(), `INSERT INTO kingres_cp.cluster_setting (key, value) VALUES ('rebalance.maxStandbyLagBytes', '5'::jsonb)`); err != nil {
		t.Fatal(err)
	}
	if f := facts(t, s); !reflect.DeepEqual(f.GroupsLagging, []int32{0, 1, 2}) {
		t.Fatalf("limit 5: %v", f.GroupsLagging)
	}
	if _, err := e.sys.Exec(context.Background(), `UPDATE kingres_cp.cluster_setting SET value = '1000000000'::jsonb WHERE key = 'rebalance.maxStandbyLagBytes'`); err != nil {
		t.Fatal(err)
	}
	if f := facts(t, s); len(f.GroupsLagging) != 0 {
		t.Fatalf("a high limit: %v", f.GroupsLagging)
	}
}

// A primary that cannot be asked cannot show that its standby is in step: the group counts as lagging.
func TestAGroupWhosePrimaryCannotBeAskedCountsAsLagging(t *testing.T) {
	s, _ := lagEnv(t, nil, map[int32]bool{2: true})
	if f := facts(t, s); !reflect.DeepEqual(f.GroupsLagging, []int32{2}) {
		t.Fatalf("lagging %v", f.GroupsLagging)
	}
}

// Nobody is asked when no standby is wanted.
func TestNoLagIsReadWithoutStandbys(t *testing.T) {
	s, e := lagEnv(t, map[int32]int64{1: 1 << 40}, nil)
	if _, err := e.sys.Exec(context.Background(), `DELETE FROM kingres.node WHERE role = 'standby'`); err != nil {
		t.Fatal(err)
	}
	asked := 0
	inner := s.StandbyLag
	s.StandbyLag = func(ctx context.Context, g int32) (int64, error) { asked++; return inner(ctx, g) }
	if f := facts(t, s); len(f.GroupsLagging) != 0 || asked != 0 {
		t.Fatalf("lagging %v, asked %d", f.GroupsLagging, asked)
	}
}

// The guard that runs before each move of an automatic run stops it on a lagging standby, with the rule in the text.
func TestTheGuardStopsAnAutomaticRunOnALaggingStandby(t *testing.T) {
	s, _ := lagEnv(t, map[int32]int64{1: 200 << 20}, nil)
	err := s.autoGuard(context.Background(), Options{})
	var p *problem.Problem
	if !errors.As(err, &p) || !strings.Contains(p.Detail, RuleDegraded) {
		t.Fatalf("the guard: %v", err)
	}
	s.StandbyLag = func(context.Context, int32) (int64, error) { return 0, nil }
	if err := s.autoGuard(context.Background(), Options{}); err != nil {
		t.Fatalf("in step: %v", err)
	}
}

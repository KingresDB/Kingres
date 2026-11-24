// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package rebalance

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
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

// M3. Inside a run the limit is twice the setting and the guard pauses on the third lagging evaluation in a row, not on the first.
func TestTheGuardPausesAnAutomaticRunOnlyOnThreeEvaluationsOverTwiceTheLimit(t *testing.T) {
	ctx := context.Background()
	const limit = int64(64 << 20)
	lag := int64(100 << 20) // over the limit of a start, under twice it
	s, _ := lagEnv(t, nil, nil)
	s.StandbyLag = func(_ context.Context, g int32) (int64, error) {
		if g == 1 {
			return lag, nil
		}
		return 0, nil
	}
	for i := 0; i < 5; i++ {
		if err := s.autoGuard(ctx, Options{}); err != nil {
			t.Fatalf("a lag of %d under twice the limit %d paused the run at evaluation %d: %v", lag, limit, i, err)
		}
	}
	// a start (facts at the plain limit) does see it
	if f := facts(t, s); len(f.GroupsLagging) != 1 {
		t.Fatalf("at the start the limit is the setting: %v", f.GroupsLagging)
	}
	lag = 3 * limit
	for i := 0; i < 2; i++ {
		if err := s.autoGuard(ctx, Options{}); err != nil {
			t.Fatalf("paused at evaluation %d of 3: %v", i, err)
		}
	}
	err := s.autoGuard(ctx, Options{})
	var p *problem.Problem
	if !errors.As(err, &p) || !strings.Contains(p.Detail, RuleDegraded) {
		t.Fatalf("the third evaluation: %v", err)
	}
	// the row starts again after the pause, and a clean evaluation ends a row
	lag = 0
	for i := 0; i < 5; i++ {
		if err := s.autoGuard(ctx, Options{}); err != nil {
			t.Fatalf("in step: %v", err)
		}
	}
	lag = 3 * limit
	_ = s.autoGuard(ctx, Options{})
	_ = s.autoGuard(ctx, Options{})
	lag = 0
	_ = s.autoGuard(ctx, Options{})
	lag = 3 * limit
	if err := s.autoGuard(ctx, Options{}); err != nil {
		t.Fatalf("a clean evaluation did not end the row: %v", err)
	}
}

// M3. The groups are asked at the same time: a source that waits for the others to be asked too would wait for ever one group at a time.
func TestTheLagOfTheGroupsIsReadInParallel(t *testing.T) {
	s, _ := lagEnv(t, nil, nil)
	var arrived atomic.Int32
	s.StandbyLag = func(ctx context.Context, g int32) (int64, error) {
		arrived.Add(1)
		deadline := time.Now().Add(5 * time.Second)
		for arrived.Load() < 3 {
			if time.Now().After(deadline) {
				return 0, errors.New("the other groups were not asked at the same time")
			}
			time.Sleep(5 * time.Millisecond)
		}
		return 0, nil
	}
	if f := facts(t, s); len(f.GroupsLagging) != 0 {
		t.Fatalf("lagging %v: the groups were asked one after the other", f.GroupsLagging)
	}
}

// M2. The query reads the physical standbys only: a base backup, a logical walsender and a sender without a slot are ignored; a standby that
// has no replayed position yet is unreadable (-1, lagging); no sender is no lag. The views are given as tables of the same columns.
func TestTheLagQueryReadsOnlyPhysicalStandbys(t *testing.T) {
	e, _ := newGuardEnv(t)
	ctx := context.Background()
	for _, q := range []string{
		`CREATE TABLE fake_stat (pid int, state text, replay_lsn pg_lsn)`,
		`CREATE TABLE fake_slots (active_pid int, slot_type text)`,
	} {
		if _, err := e.sys.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	read := func() int64 {
		t.Helper()
		var out string
		if err := e.sys.QueryRow(ctx, lagSQL("fake_stat", "fake_slots")).Scan(&out); err != nil {
			t.Fatal(err)
		}
		n, err := strconv.ParseInt(out, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	add := func(pid int, state, replay, slotType string) {
		t.Helper()
		r := "NULL"
		if replay != "" {
			r = replay
		}
		if _, err := e.sys.Exec(ctx, fmt.Sprintf(`INSERT INTO fake_stat VALUES (%d, '%s', %s)`, pid, state, r)); err != nil {
			t.Fatal(err)
		}
		if slotType != "" {
			if _, err := e.sys.Exec(ctx, fmt.Sprintf(`INSERT INTO fake_slots VALUES (%d, '%s')`, pid, slotType)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if n := read(); n != 0 {
		t.Fatalf("no sender: %d", n)
	}
	behind := `(pg_current_wal_lsn() - 5000000)`
	add(1, "streaming", behind, "physical")
	if n := read(); n < 5000000 || n > 6000000 {
		t.Fatalf("a standby 5 MB behind: %d", n)
	}
	// ignored: a base backup, a logical walsender, a sender with no slot, a state that is not catching up or streaming
	add(2, "backup", "NULL::pg_lsn", "physical")
	add(3, "streaming", "'0/0'::pg_lsn", "logical")
	add(4, "streaming", "'0/0'::pg_lsn", "")
	add(5, "startup", "NULL::pg_lsn", "physical")
	if n := read(); n < 5000000 || n > 6000000 {
		t.Fatalf("senders that are not standbys were read: %d", n)
	}
	// the largest lag of the physical standbys
	add(6, "catchup", `(pg_current_wal_lsn() - 20000000)`, "physical")
	if n := read(); n < 20000000 || n > 21000000 {
		t.Fatalf("two standbys: %d", n)
	}
	// a standby that streams and has no replayed position: unreadable
	add(7, "streaming", "NULL::pg_lsn", "physical")
	if n := read(); n != -1 {
		t.Fatalf("a standby without a replayed position: %d, want -1", n)
	}
}

// A manual start is refused with group_degraded while a standby lags by more than the limit, unless the request says force; a drain, an
// automatic run and a cluster that wants no standbys are not asked.
func TestAManualStartIsRefusedWhileAStandbyLagsUnlessForced(t *testing.T) {
	ctx := context.Background()
	lag := int64(65 << 20)
	s, e := lagEnv(t, nil, nil)
	s.StandbyLag = func(_ context.Context, g int32) (int64, error) {
		if g == 2 {
			return lag, nil
		}
		return 0, nil
	}
	manual := Options{Trigger: TriggerManual}
	err := s.manualLagGate(ctx, manual, false)
	var p *problem.Problem
	if !errors.As(err, &p) || p.Code != problem.GroupDegraded || !strings.Contains(p.Detail, "[2]") {
		t.Fatalf("a lagging group: %v", err)
	}
	for name, c := range map[string]struct {
		o     Options
		drain bool
	}{
		"force":     {Options{Trigger: TriggerManual, Force: true}, false},
		"a drain":   {manual, true},
		"automatic": {Options{Trigger: TriggerAutomatic}, false},
	} {
		if err := s.manualLagGate(ctx, c.o, c.drain); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	lag = 64 << 20 // exactly the limit is allowed
	if err := s.manualLagGate(ctx, manual, false); err != nil {
		t.Errorf("at the limit: %v", err)
	}
	// a primary that cannot be asked cannot show that its standby is in step
	s.StandbyLag = func(context.Context, int32) (int64, error) { return 0, errors.New("no answer") }
	if err := s.manualLagGate(ctx, manual, false); err == nil {
		t.Error("unreadable lag was accepted")
	}
	// no standby wanted: nobody is asked
	if _, err := e.sys.Exec(ctx, `DELETE FROM kingres.node WHERE role = 'standby'`); err != nil {
		t.Fatal(err)
	}
	if err := s.manualLagGate(ctx, manual, false); err != nil {
		t.Errorf("no standbys: %v", err)
	}
}

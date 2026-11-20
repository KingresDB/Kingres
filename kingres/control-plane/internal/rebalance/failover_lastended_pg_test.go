// SPDX-License-Identifier: AGPL-3.0-only

package rebalance

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover"
)

type envConns struct {
	t *testing.T
	e *guardEnv
}

func (c envConns) ServiceConn(_ context.Context, db string) (*pgx.Conn, error) {
	return c.e.in.Connect(c.t, db), nil
}

// F4-04, F4-15. The cool-down of the automatic failover sees a run that ended without finishing a group (it failed), counts the age by the
// database's clock, and does not see a run that is still going.
func TestTheCooldownSeesARunThatFailedAndIgnoresOneThatIsRunning(t *testing.T) {
	e, _ := newGuardEnv(t)
	ctx := context.Background()
	for _, q := range []string{
		`UPDATE kingres_cp.task SET state = 'done', lease_holder = NULL, lease_expires_at = NULL`,
		`INSERT INTO kingres_cp.task (id, kind, state, subject_type, subject_id, created_by) OVERRIDING SYSTEM VALUE VALUES
			(101, 'server.failover', 'failed', 'server', 's1', 'x'),
			(102, 'server.failover', 'done', 'server', 's2', 'x')`,
		`INSERT INTO kingres_cp.failover_run (task_id, kind, trigger, mode, server_id, finished_at, outcome) VALUES
			(101, 'failover', 'automatic', 'hard', 's1', clock_timestamp() - interval '60 seconds', 'failed'),
			(102, 'failover', 'manual', 'hard', 's2', clock_timestamp() - interval '10 minutes', 'completed')`,
		`INSERT INTO kingres_cp.failover_group (task_id, group_id, seq, state, finished_at) VALUES
			(101, 3, 0, 'frozen', NULL),
			(102, 4, 0, 'done', clock_timestamp() - interval '10 minutes')`,
	} {
		if _, err := e.sys.Exec(ctx, q); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	s := &failover.Service{Conns: envConns{t, e}}
	got, err := s.LastEnded(ctx)
	if err != nil {
		t.Fatal(err)
	}
	age := func(g int32) time.Duration { return time.Since(got[g]) }
	if _, ok := got[3]; !ok || age(3) < 55*time.Second || age(3) > 120*time.Second {
		t.Errorf("group 3 (a failed run): %v", got)
	}
	if _, ok := got[4]; !ok || age(4) < 9*time.Minute || age(4) > 11*time.Minute {
		t.Errorf("group 4 (a finished group): %v", got)
	}
}

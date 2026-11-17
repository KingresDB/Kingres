// SPDX-License-Identifier: AGPL-3.0-only

package clustertest_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// rbDump describes what the system database and the extension hold about the run of task id, for the
// failure message of a test that waits for a run that does not end: the task row, the moves of the run,
// the extension's own move rows and what check_cluster reports. It never fails the test itself.
func rbDump(n *clustertest.Node, id any) string {
	var out strings.Builder
	query := func(db, title, sql string, args ...any) {
		fmt.Fprintf(&out, "-- %s\n", title)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		conn, err := dialService(n, db)
		if err != nil {
			fmt.Fprintf(&out, "   (%v)\n", err)
			return
		}
		defer func() { _ = conn.Close(context.Background()) }()
		rows, err := conn.Query(ctx, sql, args...)
		if err != nil {
			fmt.Fprintf(&out, "   (%v)\n", err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			vals, err := rows.Values()
			if err != nil {
				fmt.Fprintf(&out, "   (%v)\n", err)
				return
			}
			fmt.Fprintf(&out, "   %v\n", vals)
		}
	}
	query("kingres_system", "task", `SELECT id, state, lease_holder, lease_epoch, lease_expires_at, cancel_requested FROM kingres_cp.task WHERE id = $1`, id)
	query("kingres_system", "run", `SELECT task_id, outcome, stop_requested, stopped_by, planned_moves, round, error FROM kingres_cp.rebalance_run WHERE task_id = $1`, id)
	query("kingres_system", "moves of the run", `SELECT seq, colocation_id, bucket, from_server, to_server, state, attempts, move_id, phase, started_at, finished_at, error
		FROM kingres_cp.rebalance_move WHERE task_id = $1 ORDER BY seq`, id)
	query("rb", "moves of the extension", `SELECT move_id, colocation_id, bucket, source_group, target_group, phase, started_at, detail FROM kingres.bucket_move ORDER BY move_id`)
	query("rb", "check_cluster", `SELECT check_name, node_id, object, detail FROM kingres.check_cluster()`)
	return out.String()
}

// dialService opens a service connection to a database of n without failing the test.
func dialService(n *clustertest.Node, db string) (*pgx.Conn, error) {
	pw, err := os.ReadFile(filepath.Join(n.DataDir, "service-credentials"))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return n.PG(ctx, "kingresd", strings.TrimSpace(string(pw)), db)
}

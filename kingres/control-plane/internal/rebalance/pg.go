// SPDX-License-Identifier: AGPL-3.0-only

package rebalance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fence"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// Conns opens service connections on this server (the manager leader, which runs
// the primary of the map group: moves are started there).
type Conns interface {
	ServiceConn(ctx context.Context, db string) (*pgx.Conn, error)
}

// forbidding are the findings of kingres.check_cluster() that make a move unsafe.
// The others (orphan tables of finished moves, leftovers that a worker will
// remove, old prepared transactions, reference table differences) do not.
var forbidding = map[string]bool{
	"catalog_differs": true, "partition_missing": true, "partition_misplaced": true, "stray_bucket_table": true,
	"bucket_map_incomplete": true, "group_without_primary": true, "bucket_moving_without_move": true,
	"move_without_moving_bucket": true, "node_unreachable": true, "extension_version_differs": true,
	"server_major_differs": true, "secret_missing": true, "stalled_move": true,
}

// PG is the cluster side of the loop, on the extension's functions.
type PG struct {
	Conns Conns
	// Leases tells the lease state of a shard group as this server knows it
	// ("held", "lost", "granted", "none"; "" when it knows nothing).
	Leases func(group int32) string
	// Disk reports the used and total bytes of the data file system of a server,
	// nil when the server does not report it.
	Disk func(ctx context.Context, server string) (used, total *int64)
	// MinFreeFraction is the free part of the target's disk that a move must leave (default 0.1).
	MinFreeFraction float64
	// RecoverWait bounds how long the recovery of a move left behind may take (default 10 min).
	RecoverWait time.Duration
	Log         *slog.Logger
}

func (b *PG) log() *slog.Logger {
	if b.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return b.Log
}

func (b *PG) with(ctx context.Context, db string, f func(c *pgx.Conn) error) error {
	c, err := b.Conns.ServiceConn(ctx, db)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	return f(c)
}

// Databases implements Backend.
func (b *PG) Databases(ctx context.Context, o Options) ([]string, error) {
	var names []string
	err := b.with(ctx, "postgres", func(c *pgx.Conn) error {
		rows, err := c.Query(ctx, `SELECT datname FROM pg_catalog.pg_database WHERE NOT datistemplate AND datallowconn ORDER BY datname`)
		if err != nil {
			return err
		}
		names, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, d := range o.Databases {
		want[d] = true
	}
	var out []string
	for _, n := range names {
		if len(want) > 0 && !want[n] {
			continue
		}
		var has bool
		err := b.with(ctx, n, func(c *pgx.Conn) error {
			return c.QueryRow(ctx, `SELECT to_regclass('kingres.colocation') IS NOT NULL AND EXISTS (SELECT 1 FROM kingres.colocation)`).Scan(&has)
		})
		if err != nil {
			// A database that cannot be read has no distributed tables for this purpose
			// only when it does not exist any more; anything else is an error.
			var pe *pgconn.PgError
			if errors.As(err, &pe) && pe.Code == "3D000" {
				continue
			}
			var pe2 *pgconn.PgError
			if errors.As(err, &pe2) && pe2.Code == "42P01" {
				continue
			}
			return nil, err
		}
		if has {
			out = append(out, n)
		}
	}
	return out, nil
}

// Check implements Backend. The refusals are problems with their own codes.
func (b *PG) Check(ctx context.Context, dbs []string) error {
	var tasks []string
	err := b.with(ctx, sysdb.Database, func(c *pgx.Conn) error {
		rows, err := c.Query(ctx, `SELECT kind || ' ' || subject_id FROM kingres_cp.task
			WHERE state IN ('pending', 'running', 'paused') AND kind IN ('server.failover', 'server.add', 'server.remove', 'server.leave')`)
		if err != nil {
			return err
		}
		tasks, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		return err
	}
	for _, t := range tasks {
		if strings.HasPrefix(t, "server.failover") {
			return problem.New(problem.FailoverInProgress, "A failover is in progress ("+t+"); the buckets stay where they are until it has finished.")
		}
	}
	if len(tasks) > 0 {
		return problem.New(problem.OperationInProgress, "Another operation changes the servers ("+tasks[0]+"); wait until it has finished.")
	}
	for _, db := range dbs {
		if err := b.checkDatabase(ctx, db); err != nil {
			return err
		}
	}
	return nil
}

func (b *PG) checkDatabase(ctx context.Context, db string) error {
	return b.with(ctx, db, func(c *pgx.Conn) error {
		var receivers int
		if err := c.QueryRow(ctx, `SELECT count(DISTINCT group_id) FROM kingres.node WHERE role = 'primary' AND is_active AND should_have_shards`).Scan(&receivers); err != nil {
			return err
		}
		if receivers == 0 {
			return problem.New(problem.NoReceivingServer, "No server should hold shards in database "+db+"; set shouldHaveShards on at least one server first.")
		}
		// Every node that takes part needs wal_level = logical; check_cluster does not look at it.
		nodes, err := c.Query(ctx, `SELECT node_id, host FROM kingres.node WHERE role = 'primary' AND is_active ORDER BY node_id`)
		if err != nil {
			return err
		}
		type nodeRef struct {
			id   int32
			host string
		}
		var refs []nodeRef
		for nodes.Next() {
			var r nodeRef
			if err := nodes.Scan(&r.id, &r.host); err != nil {
				nodes.Close()
				return err
			}
			refs = append(refs, r)
		}
		nodes.Close()
		if err := nodes.Err(); err != nil {
			return err
		}
		for _, r := range refs {
			var level *string
			var err error
			level, err = onNode(ctx, c, r.id, `SELECT current_setting('wal_level')`)
			if err != nil {
				return fmt.Errorf("asking node %d (%s) for wal_level: %w", r.id, r.host, err)
			}
			if level == nil || *level != "logical" {
				got := "unknown"
				if level != nil {
					got = *level
				}
				return problem.New(problem.ClusterInconsistent, fmt.Sprintf("Database %s: node %d on server %s runs with wal_level = %s; a bucket move needs wal_level = logical on every node. Set it in the configuration and restart the instance.", db, r.id, r.host, got))
			}
		}
		rows, err := c.Query(ctx, `SELECT check_name, coalesce(node_id::text, ''), coalesce(object, ''), coalesce(detail, '') FROM kingres.check_cluster()`)
		if err != nil {
			return err
		}
		defer rows.Close()
		var versions, other []string
		for rows.Next() {
			var name, node, object, detail string
			if err := rows.Scan(&name, &node, &object, &detail); err != nil {
				return err
			}
			if !forbidding[name] {
				continue
			}
			line := name
			if node != "" {
				line += " (node " + node + ")"
			}
			if detail != "" {
				line += ": " + clipText(detail, 120)
			}
			if name == "extension_version_differs" || name == "server_major_differs" {
				versions = append(versions, line)
			} else {
				other = append(other, line)
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(versions) > 0 {
			return problem.New(problem.VersionMismatch, "Database "+db+": the servers do not run the same extension version and PostgreSQL major: "+strings.Join(versions, "; ")+".")
		}
		if len(other) > 0 {
			return problem.New(problem.ClusterInconsistent, "Database "+db+": kingres.check_cluster() reports "+strings.Join(other, "; ")+". Resolve it before moving buckets.")
		}
		return nil
	})
}

func clipText(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// Plan implements Backend.
func (b *PG) Plan(ctx context.Context, dbs []string) ([]Move, error) {
	var out []Move
	for _, db := range dbs {
		err := b.with(ctx, db, func(c *pgx.Conn) error {
			rows, err := c.Query(ctx, `SELECT colocation_id, bucket, source_group, target_group, size_bytes FROM kingres.rebalance_plan()`)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				m := Move{Database: db}
				if err := rows.Scan(&m.Colocation, &m.Bucket, &m.From, &m.To, &m.Size); err != nil {
					return err
				}
				out = append(out, m)
			}
			return rows.Err()
		})
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "55000" && strings.Contains(pe.Message, "no shard group accepts") {
			return nil, problem.New(problem.NoReceivingServer, "No server should hold shards in database "+db+".")
		}
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Placement implements Backend.
func (b *PG) Placement(ctx context.Context, dbs []string) ([]Placement, error) {
	var out []Placement
	for _, db := range dbs {
		err := b.with(ctx, db, func(c *pgx.Conn) error {
			rows, err := c.Query(ctx, `SELECT n.host, count(*) FROM kingres.bucket_map b
				JOIN kingres.node n ON n.group_id = b.group_id AND n.role = 'primary' AND n.is_active
				GROUP BY n.host ORDER BY n.host`)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				p := Placement{Database: db}
				if err := rows.Scan(&p.Server, &p.Buckets); err != nil {
					return err
				}
				out = append(out, p)
			}
			return rows.Err()
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ServerOf implements Backend. An unknown group gives the empty string.
func (b *PG) ServerOf(ctx context.Context, db string, group int32) string {
	var host string
	_ = b.with(ctx, db, func(c *pgx.Conn) error {
		return c.QueryRow(ctx, `SELECT host FROM kingres.node WHERE group_id = $1 AND role = 'primary' AND is_active`, group).Scan(&host)
	})
	return host
}

// CheckMove implements Backend: the lease state of both groups, a synchronous
// standby that is missing, the disk of the target.
func (b *PG) CheckMove(ctx context.Context, m Move) error {
	if b.Leases != nil {
		for _, g := range []int32{m.From, m.To} {
			if b.Leases(g) == "lost" {
				return &SkipError{Reason: SkipGroupLeaseLost, Detail: fmt.Sprintf("this server lost the lease of shard group %d", g)}
			}
		}
	}
	for _, g := range []int32{m.From, m.To} {
		degraded, err := b.syncStandbyMissing(ctx, m.Database, g)
		if err != nil {
			return err
		}
		if degraded {
			return &SkipError{Reason: SkipGroupDegraded, Detail: fmt.Sprintf("shard group %d requires a synchronous standby and has none that is in sync; commits on it would wait", g)}
		}
	}
	if b.Disk != nil && m.Size != nil {
		target := b.ServerOf(ctx, m.Database, m.To)
		used, total := b.Disk(ctx, target)
		if used != nil && total != nil && *total > 0 {
			free := *total - *used
			minFree := b.MinFreeFraction
			if minFree == 0 {
				minFree = 0.1
			}
			if free < *m.Size+*m.Size/5 || float64(free) < minFree*float64(*total) {
				return &SkipError{Reason: SkipTargetDiskLow, Detail: fmt.Sprintf("%s has %d bytes free of %d", target, free, *total)}
			}
		}
	}
	return nil
}

// syncStandbyMissing reports whether the primary of the group has a
// synchronous_standby_names list and no standby that is in sync with it. A move
// copies through logical replication and writes markers on the source and the
// target: with no synchronous standby every one of those commits would wait.
func (b *PG) syncStandbyMissing(ctx context.Context, db string, group int32) (bool, error) {
	var res *string
	err := b.with(ctx, db, func(c *pgx.Conn) error {
		var node int32
		if err := c.QueryRow(ctx, `SELECT kingres.internal_group_node($1)`, group).Scan(&node); err != nil {
			return err
		}
		var err error
		res, err = onNode(ctx, c, node, `SELECT (current_setting('synchronous_standby_names') <> '' AND NOT EXISTS (SELECT 1 FROM pg_stat_replication WHERE sync_state IN ('sync', 'quorum')))::text`)
		return err
	})
	if err != nil {
		// A node that cannot be asked is a reason to wait, not to move.
		return false, fmt.Errorf("asking the primary of shard group %d for its synchronous standbys: %w", group, err)
	}
	return res != nil && *res == "true", nil
}

// RunMove implements Backend. The statement runs on a connection of its own.
func (b *PG) RunMove(ctx context.Context, m Move) error {
	return b.with(ctx, m.Database, func(c *pgx.Conn) error {
		var id int64
		return c.QueryRow(ctx, `SELECT kingres.move_bucket($1, $2, $3)`, m.Colocation, m.Bucket, m.To).Scan(&id)
	})
}

type extMove struct {
	id    int64
	phase string
	err   string
}

func (b *PG) findMove(ctx context.Context, c *pgx.Conn, colocation, bucket int32, since time.Time, id int64) (extMove, bool, error) {
	var e extMove
	var err error
	if id != 0 {
		err = c.QueryRow(ctx, `SELECT move_id, phase, coalesce(detail, '') FROM kingres.bucket_move WHERE move_id = $1`, id).Scan(&e.id, &e.phase, &e.err)
	} else {
		err = c.QueryRow(ctx, `SELECT move_id, phase, coalesce(detail, '') FROM kingres.bucket_move
			WHERE colocation_id = $1 AND bucket = $2 AND started_at >= $3 ORDER BY move_id DESC LIMIT 1`,
			colocation, bucket, since.Add(-10*time.Second)).Scan(&e.id, &e.phase, &e.err)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return extMove{}, false, nil
	}
	return e, err == nil, err
}

// phasePercent is an estimate by phase: the extension does not report how much
// of the copy is done, so the bar moves by phase only.
func phasePercent(phase string) float64 {
	switch phase {
	case "preparing":
		return 5
	case "copying":
		return 30
	case "catching_up":
		return 85
	case "switching":
		return 95
	case "cleaning", "done":
		return 100
	}
	return 0
}

// Progress implements Backend.
func (b *PG) Progress(ctx context.Context, m Move, since time.Time) (Progress, error) {
	var p Progress
	err := b.with(ctx, m.Database, func(c *pgx.Conn) error {
		e, ok, err := b.findMove(ctx, c, m.Colocation, m.Bucket, since, 0)
		if err != nil || !ok {
			return err
		}
		p = Progress{ExtMoveID: e.id, Phase: e.phase, Percent: phasePercent(e.phase), Found: true}
		return nil
	})
	return p, err
}

// CancelMove implements Backend.
func (b *PG) CancelMove(ctx context.Context, m Move, since time.Time) (bool, error) {
	var stopped bool
	err := b.with(ctx, m.Database, func(c *pgx.Conn) error {
		e, ok, err := b.findMove(ctx, c, m.Colocation, m.Bucket, since, 0)
		if err != nil || !ok {
			return err
		}
		if _, err := c.Exec(ctx, `SELECT kingres.cancel_bucket_move($1)`, e.id); err != nil {
			var pe *pgconn.PgError
			if errors.As(err, &pe) && pe.Code == "55000" {
				return nil // switching, or past it: it finishes by itself
			}
			return err
		}
		stopped = true
		return nil
	})
	return stopped, err
}

// Recover implements Backend: the extension's recovery decides. resume_bucket_moves
// undoes a move that did not reach its switch and completes one that did; it
// never touches a move whose session is alive, so a move that a leader still
// runs is waited for, not taken.
func (b *PG) Recover(ctx context.Context, r MoveRow) (string, string, error) {
	wait := b.RecoverWait
	if wait == 0 {
		wait = 10 * time.Minute
	}
	deadline := time.Now().Add(wait)
	for {
		var state, text string
		var settled bool
		err := b.with(ctx, r.Move.Database, func(c *pgx.Conn) error {
			e, ok, err := b.findMove(ctx, c, r.Move.Colocation, r.Move.Bucket, r.StartedAt, r.ExtMoveID)
			if err != nil {
				return err
			}
			if !ok {
				state, text, settled = MoveFailed, "the move had not been recorded by the extension when the run was interrupted", true
				return nil
			}
			switch e.phase {
			case "done", "cleaning":
				state, settled = MoveDone, true
			case "failed":
				state, text, settled = MoveFailed, e.err, true
			case "cancelled":
				state, text, settled = MoveCancelled, e.err, true
			default:
				_, err := c.Exec(ctx, `SELECT kingres.resume_bucket_moves()`)
				return err
			}
			return nil
		})
		if err != nil {
			return "", "", err
		}
		if settled {
			return state, clipText(text, 500), nil
		}
		if time.Now().After(deadline) {
			return "", "", fmt.Errorf("the move is still not settled after %s; run kingres.resume_bucket_moves() on the map group primary and check kingres.check_cluster()", wait)
		}
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// Cleanup implements Backend.
func (b *PG) Cleanup(ctx context.Context) {
	dbs, err := b.Databases(ctx, Options{})
	if err != nil {
		return
	}
	for _, db := range dbs {
		if err := b.with(ctx, db, func(c *pgx.Conn) error {
			_, err := c.Exec(ctx, `SELECT kingres.cleanup_moved_buckets()`)
			return err
		}); err != nil {
			b.log().Warn("cleaning up the orphan tables of moved buckets", "database", db, "error", err)
		}
	}
}

// ---------------------------------------------------------------------------

// PGRows is Rows on the tables of the system database, for one task.
type PGRows struct {
	Conns  Conns
	TaskID int64
}

func (r *PGRows) sys(ctx context.Context, f func(c *pgx.Conn) error) error {
	c, err := r.Conns.ServiceConn(ctx, sysdb.Database)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	return f(c)
}

// leased runs f in a transaction that first proves the run still holds the lease
// of the task in the epoch of its context. FOR SHARE orders it against a claim of
// the task by another server, which updates the row.
func (r *PGRows) leased(ctx context.Context, f func(tx pgx.Tx) error) error {
	tok, ok := fence.From(ctx)
	if !ok || tok.Task != r.TaskID {
		return errors.New("rebalance: a write without the fence of the task run")
	}
	return r.sys(ctx, func(c *pgx.Conn) error {
		tx, err := c.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
		var one int
		err = tx.QueryRow(ctx, `SELECT 1 FROM kingres_cp.task WHERE id = $1 AND lease_epoch = $2 AND state = 'running'
			AND lease_expires_at > clock_timestamp() FOR SHARE`, r.TaskID, tok.Epoch).Scan(&one)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLeaseLost
		}
		if err != nil {
			return err
		}
		if err := f(tx); err != nil {
			return err
		}
		return tx.Commit(ctx)
	})
}

// CreateRun implements Rows. It needs no lease: the request that starts the run
// writes it before the task runs, and the first step repeats it.
func (r *PGRows) CreateRun(ctx context.Context, opts Options, before []Placement) (Run, error) {
	ob, _ := json.Marshal(opts)
	pb, _ := json.Marshal(before)
	err := r.sys(ctx, func(c *pgx.Conn) error {
		_, err := c.Exec(ctx, `INSERT INTO kingres_cp.rebalance_run (task_id, options, started_at, placement_before)
			VALUES ($1, $2, clock_timestamp(), $3) ON CONFLICT (task_id) DO NOTHING`, r.TaskID, ob, pb)
		return err
	})
	if err != nil {
		return Run{}, err
	}
	return r.Run(ctx)
}

// runColsOf is the column list of a run, with the table alias a joined query needs.
func runColsOf(a string) string {
	return a + `task_id, ` + a + `options, ` + a + `started_at, ` + a + `finished_at, coalesce(` + a + `outcome, ''), ` + a + `stop_requested, coalesce(` + a + `stopped_by, ''), ` + a + `planned_moves,
	coalesce(` + a + `placement_before, 'null'::jsonb), coalesce(` + a + `placement_after, 'null'::jsonb), coalesce(` + a + `error, '')`
}

var runCols = runColsOf("")

// ScanRun reads a run from a row of runCols.
func ScanRun(row pgx.Row) (Run, error) {
	var run Run
	var opts, before, after []byte
	if err := row.Scan(&run.TaskID, &opts, &run.StartedAt, &run.FinishedAt, &run.Outcome, &run.StopRequested, &run.StoppedBy,
		&run.Planned, &before, &after, &run.Error); err != nil {
		return Run{}, err
	}
	_ = json.Unmarshal(opts, &run.Options)
	_ = json.Unmarshal(before, &run.Before)
	_ = json.Unmarshal(after, &run.After)
	return run, nil
}

// Run implements Rows.
func (r *PGRows) Run(ctx context.Context) (Run, error) {
	var run Run
	err := r.sys(ctx, func(c *pgx.Conn) error {
		var err error
		run, err = ScanRun(c.QueryRow(ctx, `SELECT `+runCols+` FROM kingres_cp.rebalance_run WHERE task_id = $1`, r.TaskID))
		return err
	})
	return run, err
}

// SetPlanned implements Rows.
func (r *PGRows) SetPlanned(ctx context.Context, n int) error {
	return r.leased(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE kingres_cp.rebalance_run SET planned_moves = $2 WHERE task_id = $1`, r.TaskID, n)
		return err
	})
}

// StartMove implements Rows.
func (r *PGRows) StartMove(ctx context.Context, m Move, fromServer, toServer string, attempts int) (int, error) {
	var seq int
	err := r.leased(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `UPDATE kingres_cp.rebalance_move SET state = 'running', attempts = $5, phase = NULL, percent = 0, move_id = NULL,
			started_at = clock_timestamp(), finished_at = NULL, error = NULL, from_group = $6, to_group = $7, from_server = $8, to_server = $9
			WHERE task_id = $1 AND database_name = $2 AND colocation_id = $3 AND bucket = $4 AND state IN ('failed', 'cancelled', 'skipped')
			RETURNING seq`, r.TaskID, m.Database, m.Colocation, m.Bucket, attempts, m.From, m.To, fromServer, toServer).Scan(&seq)
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO kingres_cp.rebalance_move (task_id, seq, database_name, colocation_id, bucket, from_group, to_group,
			from_server, to_server, state, attempts, size_bytes, started_at)
			SELECT $1, coalesce(max(seq), 0) + 1, $2, $3, $4, $5, $6, $7, $8, 'running', $9, $10, clock_timestamp()
			FROM kingres_cp.rebalance_move WHERE task_id = $1 RETURNING seq`,
			r.TaskID, m.Database, m.Colocation, m.Bucket, m.From, m.To, fromServer, toServer, attempts, m.Size).Scan(&seq)
	})
	return seq, err
}

// UpdateMove implements Rows.
func (r *PGRows) UpdateMove(ctx context.Context, seq int, extID int64, phase string, percent float64) error {
	return r.leased(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE kingres_cp.rebalance_move SET move_id = CASE WHEN $3 > 0 THEN $3 ELSE move_id END,
			phase = $4, percent = $5 WHERE task_id = $1 AND seq = $2`, r.TaskID, seq, extID, phase, float32(percent))
		return err
	})
}

// FinishMove implements Rows.
func (r *PGRows) FinishMove(ctx context.Context, seq int, state, errText, skipReason string, size *int64) error {
	return r.leased(ctx, func(tx pgx.Tx) error {
		if skipReason != "" {
			errText = skipReason + ": " + errText
		}
		_, err := tx.Exec(ctx, `UPDATE kingres_cp.rebalance_move SET state = $3, finished_at = clock_timestamp(),
			error = nullif($4, ''), size_bytes = coalesce($5, size_bytes), percent = CASE WHEN $3 = 'done' THEN 100 ELSE percent END
			WHERE task_id = $1 AND seq = $2`, r.TaskID, seq, state, clipText(errText, 500), size)
		return err
	})
}

// AddSkipped implements Rows.
func (r *PGRows) AddSkipped(ctx context.Context, m Move, fromServer, toServer, reason, detail string) error {
	return r.leased(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO kingres_cp.rebalance_move (task_id, seq, database_name, colocation_id, bucket, from_group, to_group,
			from_server, to_server, state, attempts, size_bytes, started_at, finished_at, error)
			SELECT $1, coalesce(max(seq), 0) + 1, $2, $3, $4, $5, $6, $7, $8, 'skipped', 0, $9, clock_timestamp(), clock_timestamp(), $10
			FROM kingres_cp.rebalance_move WHERE task_id = $1`,
			r.TaskID, m.Database, m.Colocation, m.Bucket, m.From, m.To, fromServer, toServer, m.Size, clipText(reason+": "+detail, 500))
		return err
	})
}

const moveCols = `seq, database_name, colocation_id, bucket, from_group, to_group, coalesce(from_server, ''), coalesce(to_server, ''), state, attempts,
	coalesce(move_id, 0), coalesce(phase, ''), percent, size_bytes, started_at, finished_at, coalesce(error, '')`

// ScanMove reads a move from a row of moveCols.
func ScanMove(row pgx.Row) (MoveRow, error) {
	var m MoveRow
	var pct float32
	err := row.Scan(&m.Seq, &m.Move.Database, &m.Move.Colocation, &m.Move.Bucket, &m.Move.From, &m.Move.To, &m.FromServer, &m.ToServer,
		&m.State, &m.Attempts, &m.ExtMoveID, &m.Phase, &pct, &m.Size, &m.StartedAt, &m.FinishedAt, &m.Error)
	m.Percent = float64(pct)
	m.Move.Size = m.Size
	if m.State == MoveSkipped {
		if reason, _, ok := strings.Cut(m.Error, ": "); ok {
			m.SkipReason = reason
		}
	}
	return m, err
}

// Moves implements Rows.
func (r *PGRows) Moves(ctx context.Context) ([]MoveRow, error) {
	var out []MoveRow
	err := r.sys(ctx, func(c *pgx.Conn) error {
		rows, err := c.Query(ctx, `SELECT `+moveCols+` FROM kingres_cp.rebalance_move WHERE task_id = $1 ORDER BY seq`, r.TaskID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			m, err := ScanMove(rows)
			if err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}

// Settle implements Rows. A run that is only stopped keeps no finish time: it
// goes on when it is started again.
func (r *PGRows) Settle(ctx context.Context, outcome, stoppedBy, errText string, after []Placement) error {
	pa, _ := json.Marshal(after)
	return r.leased(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE kingres_cp.rebalance_run SET outcome = $2, stopped_by = nullif($3, ''), error = nullif($4, ''),
			finished_at = CASE WHEN $2 = 'stopped' THEN NULL ELSE clock_timestamp() END,
			placement_after = CASE WHEN $5::jsonb = 'null'::jsonb THEN placement_after ELSE $5::jsonb END
			WHERE task_id = $1`, r.TaskID, outcome, stoppedBy, clipText(errText, 500), pa)
		return err
	})
}

// RequestStop is the operator's stop. It is not a write of the run that holds
// the lease, so it has no fence; the run polls the flag.
func RequestStop(ctx context.Context, c *pgx.Conn, taskID int64, by string) (bool, error) {
	tag, err := c.Exec(ctx, `UPDATE kingres_cp.rebalance_run SET stop_requested = true, stopped_by = $2
		WHERE task_id = $1 AND (outcome IS NULL)`, taskID, by)
	return tag.RowsAffected() == 1, err
}

// ResumeRun clears a stop so that the run goes on (the operator started again).
func ResumeRun(ctx context.Context, c *pgx.Conn, taskID int64, opts *Options) error {
	if opts != nil {
		ob, _ := json.Marshal(*opts)
		_, err := c.Exec(ctx, `UPDATE kingres_cp.rebalance_run SET options = $2 WHERE task_id = $1`, taskID, ob)
		if err != nil {
			return err
		}
	}
	_, err := c.Exec(ctx, `UPDATE kingres_cp.rebalance_run SET outcome = NULL, stop_requested = false, stopped_by = NULL, finished_at = NULL, error = NULL
		WHERE task_id = $1 AND outcome = 'stopped'`, taskID)
	return err
}

// SortPlacements orders placements by database and server (stable output).
func SortPlacements(p []Placement) {
	sort.Slice(p, func(i, j int) bool {
		if p[i].Database != p[j].Database {
			return p[i].Database < p[j].Database
		}
		return p[i].Server < p[j].Server
	})
}

// onNode runs a statement that returns one value on a node of the cluster and returns
// it as text. The node this connection is on answers directly: the extension's own call
// to a node connects with the cluster's node options, which a node need not accept from
// itself (a one-server cluster has no TLS between nodes).
func onNode(ctx context.Context, c *pgx.Conn, node int32, sql string) (*string, error) {
	var local *int32
	if err := c.QueryRow(ctx, `SELECT node_id FROM kingres.local_node`).Scan(&local); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	var out *string
	if local != nil && *local == node {
		return out, c.QueryRow(ctx, sql).Scan(&out)
	}
	return out, c.QueryRow(ctx, `SELECT kingres.internal_exec_scalar($1, $2)`, node, sql).Scan(&out)
}

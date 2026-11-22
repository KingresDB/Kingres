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
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
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
	"move_without_moving_bucket": true, "node_unreachable": true, "bucket_layout": true, "split_stalled": true, "extension_version_differs": true,
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
	// GuardHold is how long a call may be unrecorded before it is asked what it is doing (default 30 s); a call that
	// waits for a lock is waited for beyond it, up to MaxCallWait (default 5 min).
	GuardHold   time.Duration
	MaxCallWait time.Duration
	// afterGuard is called with the guard held, before the call (tests).
	afterGuard func(ctx context.Context)
	Log        *slog.Logger
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

// planLegacy is the plan of the extension before 0.8: rebalance_plan(), by bucket count.
func (b *PG) planLegacy(ctx context.Context, dbs []string) ([]Move, error) {
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

// Mark implements Backend: the highest id the extension has given to a move, on the map group primary of the
// database the move is in.
func (b *PG) Mark(ctx context.Context, m Move) (int64, error) {
	var after int64
	err := b.with(ctx, m.Database, func(c *pgx.Conn) error {
		return c.QueryRow(ctx, `SELECT coalesce(max(move_id), 0) FROM kingres.bucket_move`).Scan(&after)
	})
	return after, err
}

// guardKeepAlive is how often the guard's session does something while the call waits: the server ends a
// session that is idle in its transaction (guardIdleTimeout), and a call that waits for a lock must not lose
// its guard to that.
const guardKeepAlive = 5 * time.Second

// guardMaxWait bounds, from the start of the call, how long it may stay unrecorded while it is running.
const guardMaxWait = 5 * time.Minute

// RunMove implements Backend. The statement runs on a connection of its own, after the lease of the
// run was proved and while the proof is kept (see guard.go): a run that lost its lease starts nothing.
// It returns ErrLeaseLost, without having called move_bucket, when the guard cannot be had.
//
// Until the extension has recorded the move the call is the run's: the guard is kept (and kept alive), and the call is
// cancelled when the run ends or when it has not been recorded after guardHold and is not running any more or after
// guardMaxWait; that is a *NotStartedError, not an attempt. A call that waits for a lock (the cluster lock of the
// extension, which a long operation of another session can hold) is running: it is waited for, as long as the guard
// holds, which means as long as the run still holds the task (RBR-01). From the record on the move is the extension's:
// the call is not cancelled when ctx ends, so a holder that lost the task or was stopped does not cut a move in the
// middle of its switch (which leaves prepared transactions for the recovery); the one that follows finds the live
// move by its record and waits for it.
func (b *PG) RunMove(ctx context.Context, m Move, after int64) error {
	tok, ok := fence.From(ctx)
	if !ok {
		return errors.New("rebalance: a call that starts a move without the fence of the task run")
	}
	g, err := acquireGuard(ctx, b.Conns, tok.Task)
	if err != nil {
		return notStartedUnless(err)
	}
	defer func() { g.release() }()
	c, err := b.Conns.ServiceConn(ctx, m.Database)
	if err != nil {
		return notStartedUnless(err)
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	watch, err := b.Conns.ServiceConn(ctx, m.Database)
	if err != nil {
		return notStartedUnless(err)
	}
	defer func() { _ = watch.Close(context.WithoutCancel(ctx)) }()
	if b.afterGuard != nil {
		b.afterGuard(ctx)
	}
	// The guard's session may have been ended by the server while this process was frozen, or for another
	// reason than a lost lease (a restart of the system database, an idle timeout): the lease is then proved
	// again, and only a lease that is really gone is a lost lease (RBR-04).
	if err := g.alive(ctx); err != nil {
		g.release()
		if g, err = acquireGuard(ctx, b.Conns, tok.Task); err != nil {
			return notStartedUnless(err)
		}
	}
	// The call has a context of its own: ctx ends it only while it is not recorded.
	callCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	pid := c.PgConn().PID()
	errc := make(chan error, 1)
	go func() {
		var id int64
		errc <- c.QueryRow(callCtx, `SELECT kingres.move_bucket($1, $2, $3)`, m.Colocation, m.Bucket, m.To).Scan(&id)
	}()
	// reopen replaces the observation session after it failed; if that fails too the old one stays and the next
	// pass tries again.
	reopen := func() {
		if nw, err := b.Conns.ServiceConn(ctx, m.Database); err == nil {
			_ = watch.Close(context.WithoutCancel(ctx))
			watch = nw
		}
	}
	withdraw := func(why error) error {
		cancel()
		<-errc
		return &NotStartedError{Err: why}
	}
	start := time.Now()
	hold := start.Add(b.guardHold())
	nextAlive := start.Add(guardKeepAlive)
	ctxDone := ctx.Done()
	// observed is false while the last look at the extension's record failed: the loop then knows nothing about
	// the call (P4D-02).
	observed := true
	for {
		select {
		case err := <-errc:
			return asMoveError(err)
		case <-ctxDone:
			// The run ended before the loop saw a record. One last look (P4D-08): the extension may have recorded the move
			// in the last poll, and a recorded move is the extension's and is not cancelled. An unrecorded call cannot be
			// found by anybody, so it goes.
			lctx, lcancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			e, found, ferr := b.findMove(lctx, watch, m.Colocation, m.Bucket, Ref{After: after}, 0)
			lcancel()
			if ferr == nil && found && e.id > after {
				g.release()
				return asMoveError(<-errc)
			}
			cancel()
			<-errc
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		e, found, ferr := b.findMove(ctx, watch, m.Colocation, m.Bucket, Ref{After: after}, 0)
		observed = ferr == nil
		if ferr != nil {
			// ST-02: a failed observation says nothing about the call. The session is made again; the loop goes on.
			reopen()
		} else if found && e.id > after {
			g.release() // idempotent
			break
		}
		now := time.Now()
		if now.After(nextAlive) {
			nextAlive = now.Add(guardKeepAlive)
			if err := g.alive(ctx); err != nil {
				// The proof is void while the call has no record: prove the lease again, or take the call back.
				g.release()
				if g, err = acquireGuard(ctx, b.Conns, tok.Task); err != nil {
					if errors.Is(err, ErrLeaseLost) {
						cancel()
						<-errc
						return err
					}
					return withdraw(err)
				}
			}
		}
		if now.After(hold) {
			// The bound applies to a call that was looked at. When the last look failed the record may exist (possibly in
			// "switching"), and cancelling then is the cancel that ST-02 removed (P4D-02). The wait then ends with the call,
			// with the lease (the guard) or with the run (ctx).
			if now.Sub(start) >= b.maxCallWait() && observed {
				return withdraw(fmt.Errorf("the extension did not record the move within %s", b.maxCallWait()))
			}
			running, rerr := b.callRunning(ctx, watch, pid)
			if rerr != nil {
				// ST-02: "unknown" is not "not running": a call whose state is unknown is never cancelled. The wait is
				// bounded by maxCallWait above while the observation works; the observation goes on a new session.
				reopen()
				running = true
			}
			if !running {
				return withdraw(fmt.Errorf("the extension did not record the move within %s and the call is not running", b.guardHold()))
			}
			hold = now.Add(b.guardHold()) // waiting for a lock or a node: still the call of this run
		}
		continue
	}
	// Recorded: the call runs to its end on its own.
	return asMoveError(<-errc)
}

// notStartedUnless marks an error before the call was made as "nothing was started" unless it says that the lease
// is gone, which the loop treats as its own case.
func notStartedUnless(err error) error {
	if errors.Is(err, ErrLeaseLost) {
		return err
	}
	return &NotStartedError{Err: err}
}

// callRunning asks the database what the backend of the call is doing: it is running when it is active (a lock
// wait counts: it is the statement waiting), and not when it has ended or is idle.
func (b *PG) callRunning(ctx context.Context, c *pgx.Conn, pid uint32) (bool, error) {
	var state *string
	err := c.QueryRow(ctx, `SELECT state FROM pg_catalog.pg_stat_activity WHERE pid = $1`, int32(pid)).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil && state != nil && *state == "active", err
}

// asMoveError marks an error that PostgreSQL raised for the statement (the extension said no and the move
// is over) as a MoveError. Anything else (EOF, reset, a closed connection) is not an answer about the move.
// A FATAL error (the backend was terminated, a shutdown) ends the session, not necessarily the move.
// Only the message is kept: the extension's detail names nodes and remote errors (RB-09).
func asMoveError(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Severity != "FATAL" && pe.Severity != "PANIC" {
		return &MoveError{Err: errors.New(pe.Message)}
	}
	return err
}

func (b *PG) guardHold() time.Duration {
	if b.GuardHold > 0 {
		return b.GuardHold
	}
	return guardHold
}

type extMove struct {
	id    int64
	phase string
	err   string
}

// findMove finds the extension's record of a move. By its id when that is known (ref.ID, or id). Else by the
// identity the call carried: the record of the bucket with an id above the mark taken before the call (the mark
// is kept with the row, so that a holder that takes over has it). Only for a row made before the mark was kept
// (ref.Since is set), by the time the row was started with a tolerance for the difference of the two clocks
// (ST-05): the row's time is the clock of the system database, the record's the clock of the map group primary.
func (b *PG) findMove(ctx context.Context, c *pgx.Conn, colocation, bucket int32, ref Ref, id int64) (extMove, bool, error) {
	var e extMove
	var err error
	if id == 0 {
		id = ref.ID
	}
	switch {
	case id != 0:
		err = c.QueryRow(ctx, `SELECT move_id, phase, coalesce(detail, '') FROM kingres.bucket_move WHERE move_id = $1`, id).Scan(&e.id, &e.phase, &e.err)
	case ref.Since.IsZero():
		err = c.QueryRow(ctx, `SELECT move_id, phase, coalesce(detail, '') FROM kingres.bucket_move
			WHERE colocation_id = $1 AND bucket = $2 AND move_id > $3 ORDER BY move_id DESC LIMIT 1`,
			colocation, bucket, ref.After).Scan(&e.id, &e.phase, &e.err)
	default:
		err = c.QueryRow(ctx, `SELECT move_id, phase, coalesce(detail, '') FROM kingres.bucket_move
			WHERE colocation_id = $1 AND bucket = $2 AND started_at >= $3 ORDER BY move_id DESC LIMIT 1`,
			colocation, bucket, ref.Since.Add(-sinceTolerance)).Scan(&e.id, &e.phase, &e.err)
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
func (b *PG) Progress(ctx context.Context, m Move, ref Ref) (Progress, error) {
	var p Progress
	err := b.with(ctx, m.Database, func(c *pgx.Conn) error {
		e, ok, err := b.findMove(ctx, c, m.Colocation, m.Bucket, ref, 0)
		if err != nil || !ok {
			return err
		}
		p = Progress{ExtMoveID: e.id, Phase: e.phase, Percent: phasePercent(e.phase), Found: true}
		return nil
	})
	return p, err
}

// CancelMove implements Backend.
func (b *PG) CancelMove(ctx context.Context, m Move, ref Ref) (bool, error) {
	var stopped bool
	err := b.with(ctx, m.Database, func(c *pgx.Conn) error {
		e, ok, err := b.findMove(ctx, c, m.Colocation, m.Bucket, ref, 0)
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

// sinceTolerance is how far the record of the extension may be before the row of the loop when the two are
// matched by time (only rows made before the mark was kept): the clocks of two servers differ.
const sinceTolerance = 10 * time.Second

// Recover implements Backend: the extension's recovery decides. resume_bucket_moves
// undoes a move that did not reach its switch and completes one that did; it
// never touches a move whose session is alive, so a move that a leader still
// runs is waited for, not taken. The wait is bounded by RecoverWait only while the move is not alive: a move
// whose session holds the extension's mark of a live move is waited for as long as it lives (ST-07; a copy is
// unbounded by design), and the run can be stopped meanwhile (the loop watches it).
func (b *PG) Recover(ctx context.Context, r MoveRow) (string, string, error) {
	warned := false
	wait := b.RecoverWait
	if wait == 0 {
		wait = 10 * time.Minute
	}
	deadline := time.Now().Add(wait)
	for {
		var state, text string
		var settled, alive bool
		err := b.with(ctx, r.Move.Database, func(c *pgx.Conn) error {
			ref := Ref{After: r.After}
			if !r.Marked {
				ref.Since = r.StartedAt
			}
			e, ok, err := b.findMove(ctx, c, r.Move.Colocation, r.Move.Bucket, ref, r.ExtMoveID)
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
				if _, err := c.Exec(ctx, `SELECT kingres.resume_bucket_moves()`); err != nil {
					return err
				}
				// A live session is waited for as long as it lives; an answer that cannot be had counts as
				// not alive (the bound applies).
				var live bool
				if err := c.QueryRow(ctx, `SELECT kingres.internal_move_alive($1)`, e.id).Scan(&live); err != nil {
					// Said once per call of Recover: a function that does not answer (another signature, not installed) must
					// not turn the wait for a live move into the old fixed bound without a trace (F2-16).
					if !warned {
						warned = true
						b.log().Warn("the extension cannot say whether a bucket move is alive; the bound of the wait applies", "move", e.id, "error", err)
					}
				} else if live {
					alive = true
				}
			}
			return nil
		})
		if err != nil {
			return "", "", err
		}
		if settled {
			return state, clipText(text, 500), nil
		}
		if alive {
			deadline = time.Now().Add(wait)
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

var runCols = runColsOf("") + `, round, coalesce((SELECT t.cancel_requested FROM kingres_cp.task t WHERE t.id = task_id), false)`

// ScanRun reads a run from a row of runCols.
func ScanRun(row pgx.Row) (Run, error) {
	var run Run
	var opts, before, after []byte
	if err := row.Scan(&run.TaskID, &opts, &run.StartedAt, &run.FinishedAt, &run.Outcome, &run.StopRequested, &run.StoppedBy,
		&run.Planned, &before, &after, &run.Error, &run.Round, &run.CancelRequested); err != nil {
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
func (r *PGRows) StartMove(ctx context.Context, m Move, fromServer, toServer string, attempts int, mark int64) (int, error) {
	var seq int
	err := r.leased(ctx, func(tx pgx.Tx) error {
		// ST-06: the write is repeated when its answer was lost. The row of this attempt that the first execution
		// made is the answer; a second row for it would count one move twice.
		if e := tx.QueryRow(ctx, `SELECT seq FROM kingres_cp.rebalance_move WHERE task_id = $1 AND database_name = $2 AND colocation_id = $3
			AND bucket = $4 AND state = 'running' AND attempts = $5 AND move_id IS NULL
			AND round = (SELECT round FROM kingres_cp.rebalance_run WHERE task_id = $1)`,
			r.TaskID, m.Database, m.Colocation, m.Bucket, attempts).Scan(&seq); e == nil {
			return nil
		}
		err := tx.QueryRow(ctx, `UPDATE kingres_cp.rebalance_move SET state = 'running', attempts = $5, phase = NULL, percent = 0, move_id = NULL,
			started_at = clock_timestamp(), finished_at = NULL, error = NULL, ext_mark = $10, from_group = $6, to_group = $7, from_server = $8, to_server = $9
			WHERE task_id = $1 AND database_name = $2 AND colocation_id = $3 AND bucket = $4 AND state IN ('failed', 'cancelled', 'skipped')
			AND round = (SELECT round FROM kingres_cp.rebalance_run WHERE task_id = $1)
			RETURNING seq`, r.TaskID, m.Database, m.Colocation, m.Bucket, attempts, m.From, m.To, fromServer, toServer, mark).Scan(&seq)
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO kingres_cp.rebalance_move (task_id, seq, database_name, colocation_id, bucket, from_group, to_group,
			from_server, to_server, state, attempts, size_bytes, started_at, round, ext_mark, reason, cost)
			SELECT $1, coalesce(max(seq), 0) + 1, $2, $3, $4, $5, $6, $7, $8, 'running', $9, $10, clock_timestamp(), (SELECT round FROM kingres_cp.rebalance_run WHERE task_id = $1), $11::bigint, $12, $13
			FROM kingres_cp.rebalance_move WHERE task_id = $1 RETURNING seq`,
			r.TaskID, m.Database, m.Colocation, m.Bucket, m.From, m.To, fromServer, toServer, attempts, m.Size, mark, nullIfEmpty(m.Reason), m.Cost).Scan(&seq)
	})
	return seq, err
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
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
			from_server, to_server, state, attempts, size_bytes, started_at, finished_at, error, round)
			SELECT $1, coalesce(max(seq), 0) + 1, $2, $3, $4, $5, $6, $7, $8, 'skipped', 0, $9, clock_timestamp(), clock_timestamp(), $10, (SELECT round FROM kingres_cp.rebalance_run WHERE task_id = $1)
			FROM kingres_cp.rebalance_move WHERE task_id = $1
			HAVING NOT EXISTS (SELECT 1 FROM kingres_cp.rebalance_move WHERE task_id = $1 AND database_name = $2 AND colocation_id = $3
				AND bucket = $4 AND state = 'skipped' AND error = $10
				AND round = (SELECT round FROM kingres_cp.rebalance_run WHERE task_id = $1))`,
			r.TaskID, m.Database, m.Colocation, m.Bucket, m.From, m.To, fromServer, toServer, m.Size, clipText(reason+": "+detail, 500))
		return err
	})
}

const moveCols = `seq, database_name, colocation_id, bucket, from_group, to_group, coalesce(from_server, ''), coalesce(to_server, ''), state, attempts,
	coalesce(move_id, 0), coalesce(phase, ''), percent, size_bytes, started_at, finished_at, coalesce(error, ''), round, ext_mark, coalesce(reason, ''), coalesce(cost, 0)`

// ScanMove reads a move from a row of moveCols.
func ScanMove(row pgx.Row) (MoveRow, error) {
	var m MoveRow
	var pct float32
	var mark *int64
	err := row.Scan(&m.Seq, &m.Move.Database, &m.Move.Colocation, &m.Move.Bucket, &m.Move.From, &m.Move.To, &m.FromServer, &m.ToServer,
		&m.State, &m.Attempts, &m.ExtMoveID, &m.Phase, &pct, &m.Size, &m.StartedAt, &m.FinishedAt, &m.Error, &m.Round, &mark, &m.Move.Reason, &m.Move.Cost)
	if mark != nil {
		m.After, m.Marked = *mark, true
	}
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
		if err != nil || outcome != OutcomeCompleted {
			return err
		}
		// A rebalance that completes resets the counter of automatic failovers (design 4.4, the contract says so).
		_, err = tx.Exec(ctx, `INSERT INTO kingres_cp.cluster_setting (key, value) VALUES ('autoFailover.eventsSinceReset', '0'::jsonb)
			ON CONFLICT (key) DO UPDATE SET value = '0'::jsonb, updated_at = now()`)
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

// ResumeRun starts a stopped run again, in one transaction with the task going from paused to pending:
// the stop is cleared, the options are the ones of the request (an empty request means all databases:
// a person who starts again decides again, RB-11) and a new round begins, so that the attempts and the
// skips of the stopped round do not count (RB-07; their rows stay in the report). The task is resumed
// here and not by the engine so that the exclusion of the database (migration 0009) and the change of the run
// are one atomic step. It returns task.ErrNotPaused when the task is not paused and *task.ExcludedError when a
// change of the servers is unfinished.
func ResumeRun(ctx context.Context, c *pgx.Conn, taskID int64, opts Options) error {
	tx, err := c.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	tag, err := tx.Exec(ctx, `UPDATE kingres_cp.task SET state = 'pending', cancel_requested = false, updated_at = clock_timestamp()
		WHERE id = $1 AND state = 'paused'`, taskID)
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && (pe.Code == "KX001" || pe.Code == "KX002") {
			return &task.ExcludedError{Kind: Kind, Other: pe.Detail}
		}
		return err
	}
	if tag.RowsAffected() != 1 {
		return task.ErrNotPaused
	}
	ob, _ := json.Marshal(opts)
	if _, err := tx.Exec(ctx, `UPDATE kingres_cp.rebalance_run SET options = $2, round = round + 1, outcome = NULL, stop_requested = false,
		stopped_by = NULL, finished_at = NULL, error = NULL WHERE task_id = $1 AND outcome = 'stopped'`, taskID, ob); err != nil {
		return err
	}
	return tx.Commit(ctx)
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

// UndoStart implements Rows: the try that started nothing is taken back. A first try leaves no row; a later
// one leaves the row as failed with the attempts of the tries before it, so that it counts for nothing and
// the back-off of the earlier failure applies.
func (r *PGRows) UndoStart(ctx context.Context, seq, attempts int, why string) error {
	return r.leased(ctx, func(tx pgx.Tx) error {
		if attempts <= 1 {
			_, err := tx.Exec(ctx, `DELETE FROM kingres_cp.rebalance_move WHERE task_id = $1 AND seq = $2 AND state = 'running' AND move_id IS NULL`, r.TaskID, seq)
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE kingres_cp.rebalance_move SET state = 'failed', attempts = $3, phase = NULL, finished_at = clock_timestamp(), error = $4
			WHERE task_id = $1 AND seq = $2 AND state = 'running' AND move_id IS NULL`, r.TaskID, seq, attempts-1, clipText(why, 500))
		return err
	})
}

func (b *PG) maxCallWait() time.Duration {
	if b.MaxCallWait > 0 {
		return b.MaxCallWait
	}
	return guardMaxWait
}

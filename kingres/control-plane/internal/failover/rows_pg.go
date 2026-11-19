// SPDX-License-Identifier: AGPL-3.0-only

package failover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fence"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// Conns opens service connections on this server (the manager leader, which runs the
// primary of the map group).
type Conns interface {
	ServiceConn(ctx context.Context, db string) (*pgx.Conn, error)
}

// PGRows is Rows on the tables of the system database (migration 0011), for one task.
// Every write is one transaction that first proves that the run in the context still
// holds the lease of the task in its epoch (the same proof as internal/rebalance), so
// a run that lost the task writes nothing.
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

func (r *PGRows) leased(ctx context.Context, f func(tx pgx.Tx) error) error {
	tok, ok := fence.From(ctx)
	if !ok || tok.Task != r.TaskID {
		return errors.New("failover: a write without the fence of the task run")
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

// Begin implements Rows.
func (r *PGRows) Begin(ctx context.Context, in Input, term int64, groups []GroupRow) error {
	opts, _ := json.Marshal(in)
	return r.leased(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO kingres_cp.failover_run (task_id, kind, trigger, mode, server_id, requested_by, options, expected_term)
			VALUES ($1, 'failover', $2, $3, $4, $5, $6, $7) ON CONFLICT (task_id) DO NOTHING`,
			r.TaskID, orStr(in.Trigger, "manual"), in.Mode, in.ServerID, in.RequestedBy, opts, term); err != nil {
			return err
		}
		for _, g := range groups {
			if _, err := tx.Exec(ctx, `INSERT INTO kingres_cp.failover_group (task_id, group_id, seq, old_server, old_node_id, term_before)
				VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (task_id, group_id) DO NOTHING`, r.TaskID, g.Group, g.Seq, g.OldServer, g.OldNode, term); err != nil {
				return err
			}
		}
		return nil
	})
}

func orStr(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// Run implements Rows.
func (r *PGRows) Run(ctx context.Context) (RunRow, bool, error) {
	var out RunRow
	found := true
	err := r.sys(ctx, func(c *pgx.Conn) error {
		var term *int64
		var opts []byte
		err := c.QueryRow(ctx, `SELECT task_id, expected_term, coalesce(outcome, ''), counted, options FROM kingres_cp.failover_run WHERE task_id = $1`, r.TaskID).
			Scan(&out.TaskID, &term, &out.Outcome, &out.Counted, &opts)
		if errors.Is(err, pgx.ErrNoRows) {
			found = false
			return nil
		}
		if term != nil {
			out.ExpectedTerm = *term
		}
		var o Input
		if err == nil && json.Unmarshal(opts, &o) == nil && o.Mode != "" {
			out.Options = &o
		}
		return err
	})
	return out, found, err
}

const groupCols = `group_id, seq, state, coalesce(old_server, ''), coalesce(old_node_id, 0), coalesce(new_server, ''), coalesce(new_node_id, 0),
	coalesce(chosen_standby_id, 0), term_before, term_after, timeline, lease_wait_ms, decision, data_loss, unreached, incidents,
	coalesce(old_primary, ''), coalesce(error, ''), started_at, promoted_at, finished_at, old_primary_report`

func scanGroup(row pgx.Row) (GroupRow, error) {
	var g GroupRow
	err := row.Scan(&g.Group, &g.Seq, &g.State, &g.OldServer, &g.OldNode, &g.NewServer, &g.NewNode, &g.ChosenStandby, &g.TermBefore,
		&g.TermAfter, &g.Timeline, &g.LeaseWaitMs, &g.Decision, &g.DataLoss, &g.Unreached, &g.Incidents, &g.OldPrimary, &g.Error,
		&g.StartedAt, &g.PromotedAt, &g.FinishedAt, &g.OldPrimaryReport)
	return g, err
}

// Group implements Rows.
func (r *PGRows) Group(ctx context.Context, group int32) (GroupRow, bool, error) {
	var out GroupRow
	found := true
	err := r.sys(ctx, func(c *pgx.Conn) error {
		var err error
		out, err = scanGroup(c.QueryRow(ctx, `SELECT `+groupCols+` FROM kingres_cp.failover_group WHERE task_id = $1 AND group_id = $2`, r.TaskID, group))
		if errors.Is(err, pgx.ErrNoRows) {
			found = false
			return nil
		}
		return err
	})
	return out, found, err
}

// Set implements Rows: only the members that are set are written.
func (r *PGRows) Set(ctx context.Context, group int32, u GroupSet) error {
	var sets []string
	args := []any{r.TaskID, group}
	add := func(col string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if u.State != nil {
		add("state", *u.State)
	}
	if u.NewServer != nil {
		add("new_server", *u.NewServer)
	}
	if u.NewNode != nil {
		add("new_node_id", *u.NewNode)
	}
	if u.ChosenStandby != nil {
		add("chosen_standby_id", *u.ChosenStandby)
	}
	if u.TermAfter != nil {
		add("term_after", *u.TermAfter)
	}
	if u.Timeline != nil {
		add("timeline", *u.Timeline)
	}
	if u.LeaseWaitMs != nil {
		add("lease_wait_ms", *u.LeaseWaitMs)
	}
	if u.Decision != nil {
		add("decision", u.Decision)
	}
	if u.DataLoss != nil {
		add("data_loss", u.DataLoss)
	}
	if u.Unreached != nil {
		add("unreached", u.Unreached)
	}
	if u.Incidents != nil {
		add("incidents", u.Incidents)
	}
	if u.OldPrimary != nil {
		add("old_primary", *u.OldPrimary)
	}
	if u.Error != nil {
		add("error", *u.Error)
	}
	if u.ClearPin {
		sets = append(sets, "chosen_standby_id = NULL, new_server = NULL, new_node_id = NULL")
	}
	if u.Started {
		sets = append(sets, "started_at = coalesce(started_at, clock_timestamp())")
	}
	if u.Promoted {
		sets = append(sets, "promoted_at = coalesce(promoted_at, clock_timestamp())")
	}
	if u.Finished {
		sets = append(sets, "finished_at = coalesce(finished_at, clock_timestamp())")
	}
	if len(sets) == 0 {
		return nil
	}
	return r.leased(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE kingres_cp.failover_group SET `+strings.Join(sets, ", ")+` WHERE task_id = $1 AND group_id = $2`, args...)
		return err
	})
}

// Pin implements Rows: one conditional transaction, the choice of the first writer stays.
func (r *PGRows) Pin(ctx context.Context, group int32, s StandbyRec, decision, dataLoss []byte) (StandbyRec, bool, error) {
	out := s
	pinnedNow := false
	err := r.leased(ctx, func(tx pgx.Tx) error {
		var chosen int64
		var server string
		var node int32
		err := tx.QueryRow(ctx, `SELECT coalesce(chosen_standby_id, 0), coalesce(new_server, ''), coalesce(new_node_id, 0) FROM kingres_cp.failover_group
			WHERE task_id = $1 AND group_id = $2 FOR UPDATE`, r.TaskID, group).Scan(&chosen, &server, &node)
		if err != nil {
			return err
		}
		if chosen != 0 {
			out = StandbyRec{ID: chosen, Group: group, Server: server, CatalogNode: node, InstanceID: s.InstanceID}
			if s.ID != chosen {
				out.InstanceID = 0
				_ = tx.QueryRow(ctx, `SELECT instance_id, coalesce(port, 0) FROM kingres_cp.standby WHERE standby_id = $1`, chosen).Scan(&out.InstanceID, &out.Port)
			}
			return nil
		}
		pinnedNow = true
		_, err = tx.Exec(ctx, `UPDATE kingres_cp.failover_group SET chosen_standby_id = $3, new_server = $4, new_node_id = $5, state = 'pinned',
			decision = coalesce($6, decision), data_loss = coalesce($7, data_loss) WHERE task_id = $1 AND group_id = $2`,
			r.TaskID, group, s.ID, s.Server, s.CatalogNode, decision, dataLoss)
		return err
	})
	return out, pinnedNow, err
}

// Event implements Rows.
func (r *PGRows) Event(ctx context.Context, group *int32, step, kind string, detail map[string]any) error {
	b, _ := json.Marshal(detail)
	return r.leased(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO kingres_cp.failover_event (task_id, seq, group_id, step, kind, detail)
			SELECT $1, coalesce(max(seq), 0) + 1, $2, $3, $4, $5 FROM kingres_cp.failover_event WHERE task_id = $1`, r.TaskID, group, step, kind, b)
		return err
	})
}

// Finish implements Rows.
func (r *PGRows) Finish(ctx context.Context, outcome, errText string) error {
	return r.leased(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE kingres_cp.failover_run SET outcome = $2, error = nullif($3, ''), finished_at = coalesce(finished_at, clock_timestamp())
			WHERE task_id = $1`, r.TaskID, outcome, errText)
		return err
	})
}

// Abandon implements Rows. It needs no lease: the engine calls it after it recorded
// the task as failed or cancelled, and the run is closed only when it is still open.
func (r *PGRows) Abandon(ctx context.Context, outcome, errText string) error {
	return r.sys(ctx, func(c *pgx.Conn) error {
		_, err := c.Exec(ctx, `UPDATE kingres_cp.failover_run SET outcome = $2, error = nullif($3, ''), finished_at = clock_timestamp()
			WHERE task_id = $1 AND finished_at IS NULL`, r.TaskID, outcome, truncate(errText, 500))
		return err
	})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ProveLease proves, inside tx, that the task run named by the fence token of ctx still
// holds the lease of its task (the same proof as every write of PGRows), for the writes
// of a failover that are not in its own tables (the records of the standbys).
func ProveLease(ctx context.Context, tx pgx.Tx) error {
	tok, ok := fence.From(ctx)
	if !ok {
		return errors.New("failover: a write without the fence of the task run")
	}
	var one int
	err := tx.QueryRow(ctx, `SELECT 1 FROM kingres_cp.task WHERE id = $1 AND lease_epoch = $2 AND state = 'running'
		AND lease_expires_at > clock_timestamp() FOR SHARE`, tok.Task, tok.Epoch).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrLeaseLost
	}
	return err
}

// Warned implements Rows.
func (r *PGRows) Warned(ctx context.Context) (bool, error) {
	var n int
	err := r.sys(ctx, func(c *pgx.Conn) error {
		return c.QueryRow(ctx, `SELECT (SELECT count(*) FROM kingres_cp.failover_event WHERE task_id = $1 AND kind = ANY($2))
			+ (SELECT count(*) FROM kingres_cp.failover_group WHERE task_id = $1 AND incidents IS NOT NULL AND incidents <> '[]'::jsonb)`, r.TaskID, WarningKinds).Scan(&n)
	})
	return n > 0, err
}

// SPDX-License-Identifier: AGPL-3.0-only

package failover

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// MapRecord is what the bootstrap of the map group knows when the system database is writable again (design 3.4, B7): the
// task of the failover is recorded with the steps of group 0 up to F7 done and the later ones pending, and the report rows are
// written from the journal.
type MapRecord struct {
	// JournalID names the bootstrap; a second call with the same id changes nothing and returns the same task.
	JournalID     string
	Input         Input // Mode hard, ServerID the old leader's server, Groups from the new catalog (group 0 first)
	OldNode       int32
	NewNode       int32
	NewServer     string
	ChosenStandby int64
	TermBefore    int64
	TermAfter     int64
	Timeline      int64
	Decision      []byte
	DataLoss      []byte
	LeaseWaitMs   int
	StartedAt     time.Time
	Events        []MapEvent
}

// MapEvent is a line of the journal that becomes an event of the report.
type MapEvent struct {
	At     time.Time
	Step   string
	Kind   string
	Detail map[string]any
}

// StepNames lists the names of the steps of a task for the input, in order.
func (r *Runner) StepNames(in Input) ([]string, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	steps, err := r.steps(raw)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Name
	}
	return out, nil
}

// RecordMapGroupRun inserts the task and its run rows in one transaction on conn, a connection to the system database of the
// new map primary. The task is pending: the engine claims it once its gate is open and runs F8 to F13 for group 0 and every
// step for the ordinary groups of the old server.
func (r *Runner) RecordMapGroupRun(ctx context.Context, conn *pgx.Conn, rec MapRecord) (int64, error) {
	var existing int64
	err := conn.QueryRow(ctx, `SELECT task_id FROM kingres_cp.failover_run WHERE options->>'journalId' = $1`, rec.JournalID).Scan(&existing)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	names, err := r.StepNames(rec.Input)
	if err != nil {
		return 0, err
	}
	done := map[string]bool{StepCheck: true}
	for _, s := range []string{StepFreeze, StepStop, StepLease, StepChoose, StepHandOver, StepPromote, StepCatalog} {
		done[StepName(0, s)] = true
	}
	states := make([]task.StepState, len(names))
	for i, n := range names {
		st := task.StepPending
		if done[n] {
			st = task.StepDone
		}
		states[i] = task.StepState{Name: n, State: st}
	}
	raw, _ := json.Marshal(rec.Input)
	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	now := time.Now()
	t, err := task.NewPGStore(tx).Create(ctx, task.Task{Kind: KindFailover, SubjectType: SubjectType, SubjectID: rec.Input.ServerID, Input: raw,
		Steps: states, CreatedAt: now, UpdatedAt: now, CreatedBy: orStr(rec.Input.RequestedBy, "system")})
	if err != nil {
		return 0, err
	}
	opts := map[string]any{}
	_ = json.Unmarshal(raw, &opts)
	opts["journalId"] = rec.JournalID
	ob, _ := json.Marshal(opts)
	if _, err := tx.Exec(ctx, `INSERT INTO kingres_cp.failover_run (task_id, kind, trigger, mode, server_id, requested_by, options, expected_term, started_at)
		VALUES ($1, 'failover', $2, $3, $4, $5, $6, $7, $8)`,
		t.ID, orStr(rec.Input.Trigger, "manual"), rec.Input.Mode, rec.Input.ServerID, rec.Input.RequestedBy, ob, rec.TermBefore, startOr(rec.StartedAt, now)); err != nil {
		return 0, err
	}
	for i, g := range rec.Input.Groups {
		if g != 0 {
			if _, err := tx.Exec(ctx, `INSERT INTO kingres_cp.failover_group (task_id, group_id, seq, old_server, term_before) VALUES ($1, $2, $3, $4, $5)`,
				t.ID, g, i, rec.Input.ServerID, rec.TermBefore); err != nil {
				return 0, err
			}
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO kingres_cp.failover_group (task_id, group_id, seq, state, old_server, old_node_id, new_server, new_node_id,
				chosen_standby_id, decision, data_loss, term_before, term_after, timeline, lease_wait_ms, started_at, promoted_at)
			VALUES ($1, 0, $2, 'promoted', $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, now())`,
			t.ID, i, rec.Input.ServerID, rec.OldNode, rec.NewServer, rec.NewNode, rec.ChosenStandby, nullJSON(rec.Decision), nullJSON(rec.DataLoss),
			rec.TermBefore, rec.TermAfter, rec.Timeline, rec.LeaseWaitMs, startOr(rec.StartedAt, now)); err != nil {
			return 0, err
		}
	}
	for i, e := range rec.Events {
		b, _ := json.Marshal(e.Detail)
		zero := int32(0)
		if _, err := tx.Exec(ctx, `INSERT INTO kingres_cp.failover_event (task_id, seq, at, group_id, step, kind, detail) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			t.ID, i+1, startOr(e.At, now), zero, e.Step, e.Kind, b); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return t.ID, nil
}

func startOr(t, d time.Time) time.Time {
	if t.IsZero() {
		return d
	}
	return t
}

func nullJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

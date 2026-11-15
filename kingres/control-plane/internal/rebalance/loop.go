// SPDX-License-Identifier: AGPL-3.0-only

// Package rebalance runs the rebalance of a cluster as one task of the task
// engine (design shard-phase10.md, section 6; first increment: the strategy "by
// bucket count", one move at a time).
//
// The package has two halves. The loop (this file) decides what to move next,
// retries, stops and recovers; it knows nothing of PostgreSQL and works on the
// Backend and Rows interfaces, so that its rules are tested with fakes. pg.go
// implements the interfaces with the extension's functions (rebalance_plan,
// move_bucket, cancel_bucket_move, resume_bucket_moves, check_cluster) and the
// tables of the system database; service.go is what the HTTP layer and the task
// engine use.
package rebalance

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// Task kind and subject of a rebalance: one at a time, cluster-wide.
const (
	Kind        = "rebalance"
	SubjectType = "cluster"
	SubjectID   = "cluster"
)

// Options of a rebalance (contract RebalanceOptions).
type Options struct {
	// Databases restricts the run to these databases; empty means all.
	Databases []string `json:"databases,omitempty"`
	// MaxConcurrentMoves is accepted and recorded; this increment runs one move at a time.
	MaxConcurrentMoves int `json:"maxConcurrentMoves,omitempty"`
}

// Move is one bucket move: the bucket of a colocation group of a database from a
// shard group to another.
type Move struct {
	Database   string
	Colocation int32
	Bucket     int32
	From, To   int32
	// Size is the estimated bytes to copy, nil when unknown.
	Size *int64
}

// Key identifies the bucket, whatever the groups.
func (m Move) Key() string { return fmt.Sprintf("%s/%d/%d", m.Database, m.Colocation, m.Bucket) }

// Placement is the number of primary buckets a server holds in a database.
type Placement struct {
	Database string `json:"databaseName"`
	Server   string `json:"serverId"`
	Buckets  int    `json:"primaryBuckets"`
}

// Move states in a run.
const (
	MoveRunning   = "running"
	MoveDone      = "done"
	MoveFailed    = "failed"
	MoveCancelled = "cancelled"
	MoveSkipped   = "skipped"
)

// Reasons a move is skipped (contract skipReason).
const (
	SkipTargetDiskLow  = "targetDiskLow"
	SkipGroupDegraded  = "groupDegraded"
	SkipGroupLeaseLost = "groupLeaseLost"
)

// Run outcomes (contract RebalanceReportSummary.outcome).
const (
	OutcomeCompleted = "completed"
	OutcomeStopped   = "stopped"
	OutcomeFailed    = "failed"
)

// MoveRow is the record of one move of a run.
type MoveRow struct {
	Seq        int
	Move       Move
	FromServer string
	ToServer   string
	State      string
	Attempts   int
	// ExtMoveID is the id of the extension's own record, 0 while unknown.
	ExtMoveID  int64
	Phase      string
	Percent    float64
	Size       *int64
	StartedAt  time.Time
	FinishedAt *time.Time
	Error      string
	// SkipReason is set for a skipped move.
	SkipReason string
}

// Run is the record of a run (the report of the contract is read from it).
type Run struct {
	TaskID        int64
	Options       Options
	StartedAt     time.Time
	FinishedAt    *time.Time
	Outcome       string
	StopRequested bool
	StoppedBy     string
	Planned       int
	Before, After []Placement
	Error         string
}

// ErrLeaseLost is returned by Rows when the run that writes is no longer the
// holder of the task's lease.
var ErrLeaseLost = task.ErrLeaseLost

// Rows is the persistence of a run. Every method that writes checks that the
// caller still holds the task's lease in the epoch of its context, so a leader
// that lost its lease cannot record or start anything.
type Rows interface {
	// CreateRun records the run if it does not exist (a run that exists is kept).
	CreateRun(ctx context.Context, opts Options, before []Placement) (Run, error)
	Run(ctx context.Context) (Run, error)
	SetPlanned(ctx context.Context, n int) error
	// StartMove records a move as running; attempts is the number of this try.
	// A move of the same bucket that failed or was skipped before keeps its row.
	StartMove(ctx context.Context, m Move, fromServer, toServer string, attempts int) (seq int, err error)
	UpdateMove(ctx context.Context, seq int, extID int64, phase string, percent float64) error
	FinishMove(ctx context.Context, seq int, state, errText, skipReason string, size *int64) error
	// AddSkipped records a move that was not started, with the reason.
	AddSkipped(ctx context.Context, m Move, fromServer, toServer, reason, detail string) error
	Moves(ctx context.Context) ([]MoveRow, error)
	// Settle ends the run: outcome, who stopped it, the placement after, an error text.
	Settle(ctx context.Context, outcome, stoppedBy, errText string, after []Placement) error
}

// SkipError makes the loop skip a move (it is recorded and not tried again in
// this run) instead of ending the run.
type SkipError struct{ Reason, Detail string }

func (e *SkipError) Error() string { return "move skipped: " + e.Reason + ": " + e.Detail }

// Progress is what the extension says about a running move.
type Progress struct {
	ExtMoveID int64
	Phase     string
	Percent   float64
	// Found is false while the extension has no record of the move yet.
	Found bool
}

// Backend is what the loop needs from the cluster.
type Backend interface {
	// Databases lists the databases with distributed tables, filtered by the options.
	Databases(ctx context.Context, o Options) ([]string, error)
	// Check verifies the conditions of the cluster for moving (a *problem.Problem
	// when it refuses). It is called before the run starts and before each move.
	Check(ctx context.Context, dbs []string) error
	// Plan is the moves the extension would do now, databases in the order given,
	// each database's moves in the extension's order.
	Plan(ctx context.Context, dbs []string) ([]Move, error)
	Placement(ctx context.Context, dbs []string) ([]Placement, error)
	// ServerOf names the server that runs the primary of a group.
	ServerOf(ctx context.Context, db string, group int32) string
	// CheckMove verifies the conditions of one move; a *SkipError skips it.
	CheckMove(ctx context.Context, m Move) error
	// RunMove executes the move and returns when the bucket is on the target (or
	// the move failed). It blocks; the loop watches it with Progress.
	RunMove(ctx context.Context, m Move) error
	// Progress reads the extension's record of the move.
	Progress(ctx context.Context, m Move, since time.Time) (Progress, error)
	// CancelMove asks the extension to stop the move; stopped is false when it has
	// passed the point where it can be stopped (it then runs to its end).
	CancelMove(ctx context.Context, m Move, since time.Time) (stopped bool, err error)
	// Recover settles a move that a previous run left running, through the
	// extension's own functions (resume_bucket_moves). It returns the state the
	// move ended in: MoveDone, MoveFailed or MoveCancelled.
	Recover(ctx context.Context, m MoveRow) (state, errText string, err error)
	// Cleanup drops the orphan tables whose delay is over (best effort).
	Cleanup(ctx context.Context)
}

// Loop is the rebalance algorithm of step "Move buckets".
type Loop struct {
	Backend Backend
	Rows    Rows
	// MaxAttempts is how often a bucket is tried before it is given up (default 3).
	MaxAttempts int
	// Backoff times the number of failures is the wait before a retry (default 30 s).
	Backoff time.Duration
	// Poll is the period of the progress and stop check of a running move (default 500 ms).
	Poll time.Duration
	Now  func() time.Time
	// Sleep waits d or until ctx ends; time-based by default.
	Sleep func(ctx context.Context, d time.Duration)
	// OnProgress is called after each recorded change of a move (the audit and tests).
	OnProgress func(MoveRow)
}

func (l *Loop) defaults() {
	if l.MaxAttempts == 0 {
		l.MaxAttempts = 3
	}
	if l.Backoff == 0 {
		l.Backoff = 30 * time.Second
	}
	if l.Poll == 0 {
		l.Poll = 500 * time.Millisecond
	}
	if l.Now == nil {
		l.Now = time.Now
	}
	if l.Sleep == nil {
		l.Sleep = func(ctx context.Context, d time.Duration) {
			select {
			case <-ctx.Done():
			case <-time.After(d):
			}
		}
	}
}

// Moves runs the moves until nothing is left to do, the run is stopped or ends
// with failures. It returns task.Pause when an operator or a safety rule stopped
// the run. A nil return means the loop is over: the report step reads the rows to
// decide the outcome.
//
// One move runs at a time. Before each move the plan is computed again, so that a
// move that changed the placement, a server flag changed meanwhile or a bucket
// that moved by itself is taken into account; the first move of the new plan that
// is neither exhausted, backing off nor skipped is the next one.
func (l *Loop) Moves(ctx context.Context, opts Options) error {
	l.defaults()
	dbs, err := l.Backend.Databases(ctx, opts)
	if err != nil {
		return err
	}
	skipped := map[string]bool{}
	// Skips of an earlier part of this run (before a restart or a stop) stay skipped.
	rows, err := l.Rows.Moves(ctx)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.State == MoveSkipped {
			skipped[r.Move.Key()] = true
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := l.recoverLeftovers(ctx); err != nil {
			return err
		}
		run, err := l.Rows.Run(ctx)
		if err != nil {
			return err
		}
		if run.StopRequested {
			return l.stopped(ctx, run)
		}
		if err := l.Backend.Check(ctx, dbs); err != nil {
			// A condition that does not hold any more stops the run so that an
			// operator can look at it; starting again checks it again.
			if serr := l.Rows.Settle(ctx, OutcomeStopped, "safetyRule", err.Error(), nil); serr != nil {
				return serr
			}
			return task.Pause("A safety condition does not hold: " + err.Error())
		}
		plan, err := l.Backend.Plan(ctx, dbs)
		if err != nil {
			return err
		}
		rows, err = l.Rows.Moves(ctx)
		if err != nil {
			return err
		}
		if n := countDone(rows) + len(plan); n > run.Planned {
			if err := l.Rows.SetPlanned(ctx, n); err != nil {
				return err
			}
		}
		next, wait, found := l.pick(plan, rows, skipped)
		if !found {
			if wait > 0 {
				l.Sleep(ctx, min(wait, l.Poll*4))
				continue
			}
			l.Backend.Cleanup(ctx)
			return nil
		}
		if err := l.Backend.CheckMove(ctx, next.move); err != nil {
			var se *SkipError
			if !errors.As(err, &se) {
				return err
			}
			skipped[next.move.Key()] = true
			if err := l.Rows.AddSkipped(ctx, next.move, l.Backend.ServerOf(ctx, next.move.Database, next.move.From),
				l.Backend.ServerOf(ctx, next.move.Database, next.move.To), se.Reason, se.Detail); err != nil {
				return err
			}
			continue
		}
		if err := l.execute(ctx, next.move, next.attempts+1); err != nil {
			return err
		}
	}
}

type candidate struct {
	move     Move
	attempts int
}

// pick chooses the next move of the plan. wait is how long the earliest move
// that is only backing off still has to wait; found is false when nothing can be
// started now.
func (l *Loop) pick(plan []Move, rows []MoveRow, skipped map[string]bool) (c candidate, wait time.Duration, found bool) {
	attempts := map[string]int{}
	last := map[string]time.Time{}
	for _, r := range rows {
		k := r.Move.Key()
		if r.State == MoveFailed || r.State == MoveRunning {
			attempts[k] = max(attempts[k], r.Attempts)
			if r.FinishedAt != nil && r.FinishedAt.After(last[k]) {
				last[k] = *r.FinishedAt
			}
		}
	}
	now := l.Now()
	for _, m := range plan {
		k := m.Key()
		if skipped[k] || attempts[k] >= l.MaxAttempts {
			continue
		}
		if a := attempts[k]; a > 0 {
			if until := last[k].Add(time.Duration(a) * l.Backoff); now.Before(until) {
				if d := until.Sub(now); wait == 0 || d < wait {
					wait = d
				}
				continue
			}
		}
		return candidate{move: m, attempts: attempts[k]}, 0, true
	}
	return candidate{}, wait, false
}

func countDone(rows []MoveRow) int {
	n := 0
	for _, r := range rows {
		if r.State == MoveDone {
			n++
		}
	}
	return n
}

// recoverLeftovers settles the moves that an earlier run of the task left
// running (the leader died, lost its lease or was restarted): the extension's own
// recovery decides, never this code (resume_bucket_moves undoes what did not
// reach the switch and completes what did).
func (l *Loop) recoverLeftovers(ctx context.Context) error {
	rows, err := l.Rows.Moves(ctx)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.State != MoveRunning {
			continue
		}
		state, text, err := l.Backend.Recover(ctx, r)
		if err != nil {
			return fmt.Errorf("recovering the move of bucket %s: %w", r.Move.Key(), err)
		}
		if err := l.Rows.FinishMove(ctx, r.Seq, state, text, "", r.Size); err != nil {
			return err
		}
	}
	return nil
}

// execute runs one move and watches it. The stop request is polled with the
// progress: a move that has not reached its switch is cancelled through the
// extension (it undoes itself and the bucket stays where it was); a move that has
// is allowed to finish, because the switch is atomic and cannot be interrupted.
func (l *Loop) execute(ctx context.Context, m Move, attempt int) error {
	started := l.Now()
	seq, err := l.Rows.StartMove(ctx, m, l.Backend.ServerOf(ctx, m.Database, m.From), l.Backend.ServerOf(ctx, m.Database, m.To), attempt)
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	// The move runs on a connection of its own; cancelling ctx (a lost lease, a
	// shutdown) cancels the statement and leaves the move to the recovery.
	go func() { done <- l.Backend.RunMove(ctx, m) }()
	cancelled := false
	for {
		select {
		case err := <-done:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return l.settleMove(ctx, seq, m, started, err, cancelled)
		case <-time.After(l.Poll):
		case <-ctx.Done():
			// Leave the row running: the next run settles it with Recover.
			return ctx.Err()
		}
		if p, err := l.Backend.Progress(ctx, m, started); err == nil && p.Found {
			if err := l.Rows.UpdateMove(ctx, seq, p.ExtMoveID, p.Phase, p.Percent); err != nil {
				return err
			}
			if l.OnProgress != nil {
				l.OnProgress(MoveRow{Seq: seq, Move: m, State: MoveRunning, ExtMoveID: p.ExtMoveID, Phase: p.Phase, Percent: p.Percent})
			}
		}
		// A stop is asked again at every poll until the extension has cancelled
		// the move: a move that is switching answers that it cannot be cancelled
		// now and may fall back to catching up, so the next poll can succeed.
		if !cancelled {
			run, err := l.Rows.Run(ctx)
			if err != nil {
				return err
			}
			if run.StopRequested {
				if stopped, err := l.Backend.CancelMove(ctx, m, started); err == nil && stopped {
					cancelled = true
				}
			}
		}
	}
}

func (l *Loop) settleMove(ctx context.Context, seq int, m Move, started time.Time, err error, cancelled bool) error {
	c := context.WithoutCancel(ctx)
	switch {
	case err == nil:
		var size *int64
		if m.Size != nil {
			size = m.Size
		}
		if err := l.Rows.UpdateMove(c, seq, 0, "done", 100); err != nil {
			return err
		}
		return l.Rows.FinishMove(c, seq, MoveDone, "", "", size)
	case cancelled:
		return l.Rows.FinishMove(c, seq, MoveCancelled, err.Error(), "", nil)
	default:
		return l.Rows.FinishMove(c, seq, MoveFailed, err.Error(), "", nil)
	}
}

// stopped records the stop and pauses the task. No move is in flight here.
func (l *Loop) stopped(ctx context.Context, run Run) error {
	by := run.StoppedBy
	if by == "" {
		by = "operator"
	}
	if err := l.Rows.Settle(ctx, OutcomeStopped, by, "", nil); err != nil {
		return err
	}
	return task.Pause("Stopped by " + by + "; start the rebalance again to continue.")
}

// Verdict is the outcome of a run whose loop ended, computed from its rows.
type Verdict struct {
	Outcome string
	Failed  int
	Skipped int
	Done    int
	Text    string
}

// Judge decides how a run that has no more moves to do ended: failed when a
// bucket was given up after its attempts, completed otherwise (skipped moves are
// in the report; they are not failures).
func Judge(rows []MoveRow, maxAttempts int) Verdict {
	if maxAttempts == 0 {
		maxAttempts = 3
	}
	byKey := map[string]MoveRow{}
	var keys []string
	for _, r := range rows {
		k := r.Move.Key()
		prev, ok := byKey[k]
		if !ok {
			keys = append(keys, k)
		}
		if !ok || r.Seq > prev.Seq {
			byKey[k] = r
		}
	}
	sort.Strings(keys)
	var v Verdict
	var bad []string
	for _, r := range rows {
		switch r.State {
		case MoveDone:
			v.Done++
		case MoveSkipped:
			v.Skipped++
		}
	}
	for _, k := range keys {
		if r := byKey[k]; r.State == MoveFailed && r.Attempts >= maxAttempts {
			v.Failed++
			bad = append(bad, k)
		}
	}
	v.Outcome = OutcomeCompleted
	if v.Failed > 0 {
		v.Outcome = OutcomeFailed
		v.Text = fmt.Sprintf("%d bucket moves failed and were given up after %d attempts: %s", v.Failed, maxAttempts, join(bad, 5))
	}
	return v
}

func join(s []string, limit int) string {
	out := ""
	for i, x := range s {
		if i == limit {
			return out + fmt.Sprintf(" and %d more", len(s)-limit)
		}
		if i > 0 {
			out += ", "
		}
		out += x
	}
	return out
}

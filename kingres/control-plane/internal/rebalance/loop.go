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
	"log/slog"
	"sort"
	"sync/atomic"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
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
	// Strategy is byBucketCount or byDiskSize (contract 0.16.0); empty takes the cluster setting.
	Strategy string `json:"strategy,omitempty"`
	// Threshold is the largest deviation from the ideal that is accepted, 0 to 1; nil takes the strategy's default.
	Threshold *float64 `json:"threshold,omitempty"`
	// DrainOnly plans only the moves off servers that should not hold shards.
	DrainOnly bool `json:"drainOnly,omitempty"`
	// MaxMovesPerServer limits the moves that involve one server as source or target.
	MaxMovesPerServer int `json:"maxMovesPerServer,omitempty"`
	// AcknowledgeFailures clears a suspension by failed moves (a manual start).
	AcknowledgeFailures bool `json:"acknowledgeFailures,omitempty"`
	// CleanupDelaySeconds overrides the delay before the source orphan is dropped, for this run.
	CleanupDelaySeconds int `json:"cleanupDelaySeconds,omitempty"`
	// ExcludeServers plans as if these servers should not hold shards (a preview of a drain); never stored.
	ExcludeServers []string `json:"-"`
	// Trigger is manual, automatic or drain.
	Trigger string `json:"trigger,omitempty"`
	// DrainServer, RemoveWhenDone and AcceptFewerReplicas make the run a drain of that server (design 11).
	DrainServer         string `json:"drainServer,omitempty"`
	RemoveWhenDone      bool   `json:"removeWhenDone,omitempty"`
	AcceptFewerReplicas bool   `json:"acceptFewerReplicas,omitempty"`
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
	// Cost is what the plan counted for the bucket (1, or its bytes by disk size); 0 when unknown.
	Cost int64
	// Reason is balance or drain; empty when unknown.
	Reason string
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
	// OutcomeCancelled is a run that a person ended for good by cancelling its task (contract 0.9.0).
	OutcomeCancelled = "cancelled"
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
	ExtMoveID int64
	// After is the mark taken before the call, kept with the row (ST-05); Marked says that it is known (0 is a mark).
	After      int64
	Marked     bool
	Phase      string
	Percent    float64
	Size       *int64
	StartedAt  time.Time
	FinishedAt *time.Time
	Error      string
	// SkipReason is set for a skipped move.
	SkipReason string
	// Round is the round of the run the move belongs to.
	Round int
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
	// Round counts the starts again by a person: attempts and skips are those of the current round.
	Round int
	// CancelRequested is the cancel request of the task (the run ends as cancelled).
	CancelRequested bool
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
	// StartMove records a move as running; attempts is the number of this try. mark is the mark taken before the call
	// (Backend.Mark), written in the same statement as the row, so that a row never exists without it (P4D-03).
	StartMove(ctx context.Context, m Move, fromServer, toServer string, attempts int, mark int64) (seq int, err error)
	UpdateMove(ctx context.Context, seq int, extID int64, phase string, percent float64) error
	FinishMove(ctx context.Context, seq int, state, errText, skipReason string, size *int64) error
	// UndoStart takes back the record of a start that did not happen (the call was not made, or was
	// withdrawn before the extension recorded the move): attempts is the number the start used, and the
	// bucket is as if that try had not been made. why is kept as the reason.
	UndoStart(ctx context.Context, seq, attempts int, why string) error
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
	Plan(ctx context.Context, dbs []string, o Options) ([]Move, error)
	Placement(ctx context.Context, dbs []string) ([]Placement, error)
	// ServerOf names the server that runs the primary of a group.
	ServerOf(ctx context.Context, db string, group int32) string
	// CheckMove verifies the conditions of one move; a *SkipError skips it.
	CheckMove(ctx context.Context, m Move) error
	// Mark returns the identity after which the record of the next call for m is: the highest id the
	// extension has given to a move. The move that the call makes is the one of the bucket with an id above
	// it, which is told without comparing two clocks (RBR-05).
	Mark(ctx context.Context, m Move) (after int64, err error)
	// RunMove executes the move and returns when the bucket is on the target (or
	// the move failed). It blocks; the loop watches it with Progress. after is the Mark taken before.
	// It returns a *NotStartedError when it is certain that no move was started, and it does not
	// cancel a call that the extension has recorded when ctx ends: the extension owns that move from then on.
	RunMove(ctx context.Context, m Move, after int64) error
	// Progress reads the extension's record of the move.
	Progress(ctx context.Context, m Move, ref Ref) (Progress, error)
	// CancelMove asks the extension to stop the move; stopped is false when it has
	// passed the point where it can be stopped (it then runs to its end).
	CancelMove(ctx context.Context, m Move, ref Ref) (stopped bool, err error)
	// Recover settles a move that a previous run left running, through the
	// extension's own functions (resume_bucket_moves). It returns the state the
	// move ended in: MoveDone, MoveFailed or MoveCancelled.
	Recover(ctx context.Context, m MoveRow) (state, errText string, err error)
	// Cleanup drops the orphan tables whose delay is over (best effort).
	Cleanup(ctx context.Context)
}

// Ref says which record of the extension is the move of a call: the one with this id when it is known, else
// the one of the bucket with an id above After, else (a row left by an earlier holder, whose mark is gone) the
// one that began at or after Since, both times read from the system database, which is the extension's server.
type Ref struct {
	ID    int64
	After int64
	Since time.Time
}

// NotStartedError says that no move was started: the guard or a connection could not be had, the call was
// cancelled before the extension recorded the move, or the cluster is too busy to accept it. It is not an
// attempt at the move; the loop takes the try back and tries again later, a bounded number of times.
type NotStartedError struct{ Err error }

func (e *NotStartedError) Error() string { return "the move was not started: " + e.Err.Error() }
func (e *NotStartedError) Unwrap() error { return e.Err }

// notStartedTries is how many tries in a row may end without a move being started before the run is
// stopped (a safety stop): the cluster is busy or the extension does not accept moves, which a person should see.
const notStartedTries = 3

// MoveError is an error that the extension itself raised for the statement of a move: the statement
// ended and so did the move (a failure the extension recorded). Any other error of RunMove, a lost
// connection above all, says nothing about the move: the loop asks the extension what became of it
// (RB-04) before it records anything or starts another move.
type MoveError struct{ Err error }

func (e *MoveError) Error() string { return e.Err.Error() }
func (e *MoveError) Unwrap() error { return e.Err }

// transientTries is how often an inspection of the cluster that failed for a reason that is not a
// refusal (a refused connection, a timeout) is tried before the run is stopped (RB-05).
const transientTries = 4

// definiteRecoverWait bounds how long the loop waits for the extension to settle a move that failed with an
// error of its own (the undo may need a server that is down).
const definiteRecoverWait = 30 * time.Second

// MaxMovesFactor bounds the moves of a run (RB-06): a run stops, as a safety rule, when the moves it
// made (done or failed) exceed this multiple of the moves of its first plan plus MovesSlack.
const (
	MaxMovesFactor = 3
	MovesSlack     = 10
)

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
	Log        *slog.Logger
	// Guard is asked, before each move of a run that was started by the automatic mode, whether a safety rule of
	// design 8.3 stops it; a *problem.Problem is the refusal. nil: no rule beyond the checks of Backend.Check.
	Guard func(ctx context.Context, o Options) error

	// notStartedRun counts the tries in a row that started no move (see notStarted).
	notStartedRun int
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
	// Every write of the loop is tried again when the system database cannot be reached for a moment; when it
	// stays unreachable the run is paused, after the move in flight has been waited for (RBR-02).
	if _, wrapped := l.Rows.(*retryRows); !wrapped {
		l.Rows = &retryRows{Rows: l.Rows, l: l}
	}
	err := l.moves(ctx, opts)
	var ie *inspectError
	if errors.As(err, &ie) {
		return l.safetyStop(ctx, err)
	}
	return err
}

func (l *Loop) moves(ctx context.Context, opts Options) error {
	var dbs []string
	if err := l.inspect(ctx, func() (err error) { dbs, err = l.Backend.Databases(ctx, opts); return }); err != nil {
		return l.safetyStop(ctx, err)
	}
	skipped := map[string]bool{}
	// Skips of an earlier part of this round (before a restart) stay skipped; the skips of a round that a
	// person ended by starting again do not (RB-07).
	run0, err := l.Rows.Run(ctx)
	if err != nil {
		return err
	}
	rows, err := l.Rows.Moves(ctx)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.State == MoveSkipped && r.Round == run0.Round {
			skipped[r.Move.Key()] = true
		}
	}
	// The moves of this part of the run are counted from here (RB-06).
	madeBefore, firstPlan, tries := movesMade(rows), -1, 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := l.recoverLeftovers(ctx); err != nil {
			if errors.Is(err, errStopWhileWaiting) {
				return l.stoppedWhileWaiting(ctx)
			}
			return err
		}
		run, err := l.Rows.Run(ctx)
		if err != nil {
			return err
		}
		if run.StopRequested || run.CancelRequested {
			return l.stopped(ctx, run)
		}
		// A condition that does not hold any more, or a cluster that cannot be inspected, stops
		// the run so that an operator can look at it; starting again checks it again.
		if err := l.inspect(ctx, func() error { return l.Backend.Check(ctx, dbs) }); err != nil {
			return l.safetyStop(ctx, err)
		}
		if opts.Trigger == TriggerAutomatic && l.Guard != nil {
			if err := l.Guard(ctx, opts); err != nil {
				return l.safetyStop(ctx, err)
			}
		}
		var plan []Move
		if err := l.inspect(ctx, func() (err error) { plan, err = l.Backend.Plan(ctx, dbs, opts); return }); err != nil {
			return l.safetyStop(ctx, err)
		}
		rows, err = l.Rows.Moves(ctx)
		if err != nil {
			return err
		}
		if firstPlan < 0 {
			firstPlan = len(plan)
		}
		if made := movesMade(rows) - madeBefore; made > MaxMovesFactor*firstPlan+MovesSlack {
			return l.safetyStop(ctx, &problem.Problem{Detail: fmt.Sprintf(
				"The run made %d moves for a plan of %d and the plan is not empty: something else keeps changing the placement (for example shouldHaveShards). Look at the report, fix the cause and start again.", made, firstPlan)})
		}
		if n := countDone(rows) + len(plan); n > run.Planned {
			if err := l.Rows.SetPlanned(ctx, n); err != nil {
				return err
			}
		}
		next, wait, found := l.pick(plan, roundRows(rows, run.Round), skipped)
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
				if isRefusal(err) {
					return l.safetyStop(ctx, err)
				}
				// A node that cannot be asked now is a reason to wait and look again, not to fail.
				l.log().Warn("rebalance: checking a move failed; trying again", "move", next.move.Key(), "error", err)
				l.Sleep(ctx, l.Backoff/6)
				if tries++; tries >= transientTries {
					return l.safetyStop(ctx, &inspectError{err})
				}
				continue
			}
			skipped[next.move.Key()] = true
			if err := l.Rows.AddSkipped(ctx, next.move, l.Backend.ServerOf(ctx, next.move.Database, next.move.From),
				l.Backend.ServerOf(ctx, next.move.Database, next.move.To), se.Reason, se.Detail); err != nil {
				return err
			}
			continue
		}
		tries = 0
		if err := l.execute(ctx, next.move, next.attempts+1); err != nil {
			if errors.Is(err, errStopWhileWaiting) {
				return l.stoppedWhileWaiting(ctx)
			}
			return err
		}
	}
}

// roundRows are the rows of one round of the run: attempts and exhaustion count within a round.
func roundRows(rows []MoveRow, round int) []MoveRow {
	var out []MoveRow
	for _, r := range rows {
		if r.Round == round {
			out = append(out, r)
		}
	}
	return out
}

func movesMade(rows []MoveRow) int {
	n := 0
	for _, r := range rows {
		if r.State != MoveSkipped {
			n += max(r.Attempts, 1)
		}
	}
	return n
}

func (l *Loop) log() *slog.Logger {
	if l.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return l.Log
}

// isRefusal says whether err is a decision of the control plane (a *problem.Problem: a condition that does
// not hold) and not a failure to ask.
func isRefusal(err error) bool {
	var p *problem.Problem
	return errors.As(err, &p)
}

// inspect runs one look at the cluster. A refusal is returned at once. Any other error (a refused
// connection, a timeout) is tried again a few times with a growing wait; when it persists it is
// returned as an *inspectError (RB-05: the same rule wherever the loop looks at the cluster).
func (l *Loop) inspect(ctx context.Context, f func() error) error {
	var err error
	for i := 0; i < transientTries; i++ {
		if err = f(); err == nil || isRefusal(err) || ctx.Err() != nil {
			return err
		}
		l.log().Warn("rebalance: the cluster could not be inspected", "try", i+1, "error", err)
		l.Sleep(ctx, time.Duration(i+1)*l.Backoff/6)
	}
	return &inspectError{err}
}

type inspectError struct{ err error }

func (e *inspectError) Error() string { return e.err.Error() }
func (e *inspectError) Unwrap() error { return e.err }

// safetyStop records the run as stopped by a safety rule and pauses the task. The text kept in the
// run is a refusal's own sentence, or, for an error that is not a refusal, a fixed sentence: the
// text of such an error names roles, databases and sockets and stays in the log (RB-09).
func (l *Loop) safetyStop(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, ErrLeaseLost) {
		return err
	}
	text := "The cluster could not be inspected; the log of kingresd has the reason."
	var p *problem.Problem
	var ns *NotStartedError
	if errors.As(err, &p) {
		text = p.Detail
	} else if errors.As(err, &ns) {
		text = "The extension did not accept a move several times in a row; the cluster may be busy with another operation. The log of kingresd has the reason."
		l.log().Warn("rebalance: the run is stopped because moves cannot be started", "error", err)
	} else {
		l.log().Warn("rebalance: the run is stopped because the cluster could not be inspected", "error", err)
	}
	if serr := l.Rows.Settle(ctx, OutcomeStopped, "safetyRule", text, nil); serr != nil {
		if errors.Is(serr, ErrLeaseLost) || ctx.Err() != nil {
			return serr
		}
		// The run cannot be recorded as stopped (the system database is the reason, or one of them): pausing
		// the task is still right, and a start again begins a new round from the rows as they are.
		l.log().Warn("rebalance: the stop of the run could not be recorded", "error", serr)
	}
	return task.Pause("A safety condition does not hold: " + text)
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

// errStopWhileWaiting is the end of a wait for the extension (the settling of a move that an earlier run
// left, or the one of a session that ended) because the operator asked to stop: the move stays as it is, in the
// extension's hands, and the run pauses. A stop must not wait for a stuck move: a failover has to stop
// the rebalance first and cannot wait for the longest bound of a move (README, "Rebalancing and failover").
var errStopWhileWaiting = errors.New("rebalance: stopped while waiting for a move")

// recoverWatched is Backend.Recover with the stop of the run watched: while the extension is waited for,
// the run's stop and cancel flags are read every poll, and a request ends the wait with errStopWhileWaiting.
func (l *Loop) recoverWatched(ctx context.Context, r MoveRow) (state, text string, err error) {
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var stopped atomic.Bool
	go func() {
		t := time.NewTicker(l.Poll)
		defer t.Stop()
		for {
			select {
			case <-wctx.Done():
				return
			case <-t.C:
				if run, rerr := l.rawRows().Run(wctx); rerr == nil && (run.StopRequested || run.CancelRequested) {
					stopped.Store(true)
					cancel()
					return
				}
			}
		}
	}()
	state, text, err = l.Backend.Recover(wctx, r)
	if err != nil && stopped.Load() && ctx.Err() == nil {
		return "", "", errStopWhileWaiting
	}
	return state, text, err
}

// rawRows are the rows without the retries: for the polls that run while a move is in flight, which must not
// be held up by a wait of their own.
func (l *Loop) rawRows() Rows {
	if r, ok := l.Rows.(*retryRows); ok {
		return r.Rows
	}
	return l.Rows
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
		state, text, err := l.recoverWatched(ctx, r)
		if errors.Is(err, errStopWhileWaiting) {
			return err
		}
		if err != nil {
			return fmt.Errorf("recovering the move of bucket %s: %w", r.Move.Key(), err)
		}
		if err := l.Rows.FinishMove(ctx, r.Seq, state, text, "", r.Size); err != nil {
			return err
		}
	}
	return nil
}

// stoppedWhileWaiting ends the run as stopped after a wait that the stop of the operator cut short.
func (l *Loop) stoppedWhileWaiting(ctx context.Context) error {
	run, err := l.Rows.Run(ctx)
	if err != nil {
		return err
	}
	return l.stopped(ctx, run)
}

// execute runs one move and watches it. The stop request is polled with the
// progress: a move that has not reached its switch is cancelled through the
// extension (it undoes itself and the bucket stays where it was); a move that has
// is allowed to finish, because the switch is atomic and cannot be interrupted.
//
// Once the extension has recorded the move it is the extension's: a run that loses its lease or stops does not
// cancel the call (see PG.RunMove), it stops watching, and the holder that follows settles the move through the
// extension. A failure of the system database while the call is in flight is not a reason to leave it
// either: the call goes on and finishes, the polls are skipped, and what became of the move is recorded when
// it is over, with the same retries as every other write (RBR-02).
func (l *Loop) execute(ctx context.Context, m Move, attempt int) error {
	// The mark comes first and is written with the row (P4D-03): there is no row without a mark, so the match by time is
	// only for rows made before the mark was kept. A mark that cannot be had starts nothing and leaves no row.
	after, err := l.Backend.Mark(ctx, m)
	if err != nil {
		return l.notStarted(ctx, 0, attempt, &NotStartedError{err})
	}
	seq, err := l.Rows.StartMove(ctx, m, l.Backend.ServerOf(ctx, m.Database, m.From), l.Backend.ServerOf(ctx, m.Database, m.To), attempt, after)
	if err != nil {
		return err
	}
	ref := Ref{After: after}
	done := make(chan error, 1)
	go func() { done <- l.Backend.RunMove(ctx, m, after) }()
	cancelled := false
	for {
		select {
		case err := <-done:
			return l.finishCall(ctx, m, seq, attempt, ref, err, cancelled)
		case <-time.After(l.Poll):
		case <-ctx.Done():
			// Leave the row running: the next run settles it with Recover.
			return ctx.Err()
		}
		if err := l.watch(ctx, m, seq, &ref, &cancelled); err != nil {
			return err
		}
	}
}

// watch is one poll of a move in flight: the progress is recorded and a stop is passed on. It fails only with
// ErrLeaseLost; every other failure to reach the system database is skipped, the next poll tries again.
func (l *Loop) watch(ctx context.Context, m Move, seq int, ref *Ref, cancelled *bool) error {
	rows := l.rawRows()
	if p, err := l.Backend.Progress(ctx, m, *ref); err == nil && p.Found {
		ref.ID = p.ExtMoveID
		if err := rows.UpdateMove(ctx, seq, p.ExtMoveID, p.Phase, p.Percent); err != nil {
			if errors.Is(err, ErrLeaseLost) {
				return err
			}
			l.log().Warn("rebalance: the progress of a move could not be recorded; the move goes on", "move", m.Key(), "error", err)
			return nil
		}
		if l.OnProgress != nil {
			l.OnProgress(MoveRow{Seq: seq, Move: m, State: MoveRunning, ExtMoveID: p.ExtMoveID, Phase: p.Phase, Percent: p.Percent})
		}
	}
	// A stop is asked again at every poll until the extension has cancelled
	// the move: a move that is switching answers that it cannot be cancelled
	// now and may fall back to catching up, so the next poll can succeed.
	if !*cancelled {
		run, err := rows.Run(ctx)
		if err != nil {
			if errors.Is(err, ErrLeaseLost) {
				return err
			}
			return nil
		}
		if run.StopRequested || run.CancelRequested {
			if stopped, err := l.Backend.CancelMove(ctx, m, *ref); err == nil && stopped {
				*cancelled = true
			}
		}
	}
	return nil
}

// finishCall settles the move after its call returned.
func (l *Loop) finishCall(ctx context.Context, m Move, seq, attempt int, ref Ref, err error, cancelled bool) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// The lease was gone when the call was to be made: nothing was started, and the
	// holder does nothing more in the name of the task (the row stays for the new holder).
	if errors.Is(err, ErrLeaseLost) {
		return err
	}
	var ns *NotStartedError
	if errors.As(err, &ns) {
		return l.notStarted(ctx, seq, attempt, ns)
	}
	l.notStartedRun = 0
	if err != nil {
		var me *MoveError
		definite := errors.As(err, &me)
		// The extension decides what became of the move before anything is recorded or another move
		// starts. A lost connection says nothing about the move (its backend may be alive and finish it:
		// it is waited for). An error the extension raised ended the statement, but when the undo could
		// not reach a node (a server is down) the move's row stays in its phase until a recovery settles
		// it: ask for that now, for a bounded time, and record the failure either way.
		rctx := ctx
		if definite {
			var cancel context.CancelFunc
			rctx, cancel = context.WithTimeout(ctx, definiteRecoverWait)
			defer cancel()
		} else {
			l.log().Warn("rebalance: the session of a move ended with an error; asking the extension what became of it", "move", m.Key(), "error", err)
		}
		state, text, rerr := l.recoverWatched(rctx, MoveRow{Seq: seq, Move: m, State: MoveRunning, ExtMoveID: ref.ID, After: ref.After, Marked: ref.Since.IsZero(), StartedAt: ref.Since})
		switch {
		case errors.Is(rerr, errStopWhileWaiting):
			return rerr
		case rerr != nil && !definite:
			return fmt.Errorf("settling the move of bucket %s after its session ended: %w", m.Key(), rerr)
		case rerr != nil:
			l.log().Warn("rebalance: the extension has not settled a failed move; a recovery will", "move", m.Key(), "error", rerr)
		case state == MoveDone:
			return l.settleRecovered(ctx, seq, m, state, text)
		case !definite:
			return l.settleRecovered(ctx, seq, m, state, text)
		}
	}
	return l.settleMove(ctx, seq, m, err, cancelled)
}

// notStarted takes back a try that started nothing and, when that keeps happening, stops the run: the
// cluster is busy or the extension does not accept moves, which a person should look at. It is not an
// attempt at the move (RBR-01).
func (l *Loop) notStarted(ctx context.Context, seq, attempt int, ns *NotStartedError) error {
	l.log().Warn("rebalance: a move was not started; it will be tried again", "error", ns.Err)
	if seq > 0 { // 0: the mark could not be had and no row was made
		if err := l.Rows.UndoStart(ctx, seq, attempt, ns.Error()); err != nil {
			return err
		}
	}
	if l.notStartedRun++; l.notStartedRun >= notStartedTries {
		l.notStartedRun = 0
		return l.safetyStop(ctx, ns)
	}
	l.Sleep(ctx, l.Backoff/6)
	return nil
}

// settleRecovered records the state the extension reports for a move whose session ended without
// an answer: done, failed (with the extension's reason) or cancelled.
func (l *Loop) settleRecovered(ctx context.Context, seq int, m Move, state, text string) error {
	c := context.WithoutCancel(ctx)
	switch state {
	case MoveDone:
		if err := l.Rows.UpdateMove(c, seq, 0, "done", 100); err != nil {
			return err
		}
		return l.Rows.FinishMove(c, seq, MoveDone, "", "", m.Size)
	case MoveCancelled:
		return l.Rows.FinishMove(c, seq, MoveCancelled, text, "", nil)
	default:
		return l.Rows.FinishMove(c, seq, MoveFailed, text, "", nil)
	}
}

func (l *Loop) settleMove(ctx context.Context, seq int, m Move, err error, cancelled bool) error {
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
	// Only the last round counts for what was given up: an earlier round was ended by a person who started again.
	last := 0
	for _, r := range rows {
		last = max(last, r.Round)
	}
	byKey := map[string]MoveRow{}
	var keys []string
	for _, r := range rows {
		if r.Round != last {
			continue
		}
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
	for _, r := range Current(rows) {
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

// Current are the rows that count in the totals of a run: every row of a move that was made, and the last row of
// each bucket that was not. A failed, skipped or cancelled row of a bucket that has a later row (a round that a person
// started again tried it once more, or moved it) is history: it stays in the report and is not counted again
// (RBR-03).
func Current(rows []MoveRow) []MoveRow {
	last := map[string]int{}
	for _, r := range rows {
		if k := r.Move.Key(); r.Seq > last[k] {
			last[k] = r.Seq
		}
	}
	var out []MoveRow
	for _, r := range rows {
		if (r.State == MoveFailed || r.State == MoveSkipped || r.State == MoveCancelled) && r.Seq < last[r.Move.Key()] {
			continue
		}
		out = append(out, r)
	}
	return out
}

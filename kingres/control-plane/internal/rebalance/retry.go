// SPDX-License-Identifier: AGPL-3.0-only

package rebalance

import (
	"context"
	"errors"
	"time"
)

// retryRows is Rows with every call tried again when the system database cannot be reached for a moment
// (a refused connection, a restart, a connection limit): the writes of the loop were returned at once
// before, which ended a step while the move it had started went on and finished (RBR-02). What is not
// retried is an answer: ErrLeaseLost (the run no longer holds the task), the end of the run's context and a
// refusal. After transientTries tries the error is an *inspectError, which Moves turns into a pause of the
// run (a safety stop) once nothing is in flight any more.
type retryRows struct {
	Rows
	l *Loop
}

// do runs f until it succeeds, fails for a reason that waiting does not change, or the tries are used.
func (r *retryRows) do(ctx context.Context, f func() error) error {
	var err error
	for i := 0; i < transientTries; i++ {
		if err = f(); err == nil || !r.l.transientSystemError(ctx, err) {
			return err
		}
		r.l.log().Warn("rebalance: the system database could not be reached", "try", i+1, "error", err)
		if i+1 < transientTries {
			r.l.Sleep(ctx, time.Duration(i+1)*r.l.Backoff/6)
		}
	}
	return &inspectError{err}
}

// transientSystemError says whether err is a failure to reach the system database that another try may meet
// differently: not the loss of the lease, not the end of the context, not a decision of the control plane.
func (l *Loop) transientSystemError(ctx context.Context, err error) bool {
	return ctx.Err() == nil && !errors.Is(err, ErrLeaseLost) && !isRefusal(err) && !errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded)
}

func (r *retryRows) CreateRun(ctx context.Context, opts Options, before []Placement) (run Run, err error) {
	err = r.do(ctx, func() (e error) { run, e = r.Rows.CreateRun(ctx, opts, before); return })
	return
}

func (r *retryRows) Run(ctx context.Context) (run Run, err error) {
	err = r.do(ctx, func() (e error) { run, e = r.Rows.Run(ctx); return })
	return
}

func (r *retryRows) SetPlanned(ctx context.Context, n int) error {
	return r.do(ctx, func() error { return r.Rows.SetPlanned(ctx, n) })
}

func (r *retryRows) StartMove(ctx context.Context, m Move, fromServer, toServer string, attempts int) (seq int, err error) {
	err = r.do(ctx, func() (e error) { seq, e = r.Rows.StartMove(ctx, m, fromServer, toServer, attempts); return })
	return
}

func (r *retryRows) UpdateMove(ctx context.Context, seq int, extID int64, phase string, percent float64) error {
	return r.do(ctx, func() error { return r.Rows.UpdateMove(ctx, seq, extID, phase, percent) })
}

func (r *retryRows) FinishMove(ctx context.Context, seq int, state, errText, skipReason string, size *int64) error {
	return r.do(ctx, func() error { return r.Rows.FinishMove(ctx, seq, state, errText, skipReason, size) })
}

func (r *retryRows) UndoStart(ctx context.Context, seq, attempts int, why string) error {
	return r.do(ctx, func() error { return r.Rows.UndoStart(ctx, seq, attempts, why) })
}

func (r *retryRows) AddSkipped(ctx context.Context, m Move, fromServer, toServer, reason, detail string) error {
	return r.do(ctx, func() error { return r.Rows.AddSkipped(ctx, m, fromServer, toServer, reason, detail) })
}

func (r *retryRows) Moves(ctx context.Context) (rows []MoveRow, err error) {
	err = r.do(ctx, func() (e error) { rows, e = r.Rows.Moves(ctx); return })
	return
}

func (r *retryRows) Settle(ctx context.Context, outcome, stoppedBy, errText string, after []Placement) error {
	return r.do(ctx, func() error { return r.Rows.Settle(ctx, outcome, stoppedBy, errText, after) })
}

// SPDX-License-Identifier: AGPL-3.0-only

package rebalance

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// lossCluster loses the connection of the session of one move: move_bucket returns EOF while the
// backend finishes the move on its own. inflight counts the moves that are on their way at once.
type lossCluster struct {
	*fakeCluster
	lose     int32 // bucket whose session is lost
	loseDone atomic.Bool
	finishOK bool // the backend finishes the move (else it undoes it)
	inflight atomic.Int32
	maxIn    atomic.Int32
	mu2      sync.Mutex
	settled  chan struct{}
}

func (c *lossCluster) enter() {
	n := c.inflight.Add(1)
	for {
		m := c.maxIn.Load()
		if n <= m || c.maxIn.CompareAndSwap(m, n) {
			return
		}
	}
}

func (c *lossCluster) RunMove(ctx context.Context, m Move, after int64) error {
	c.enter()
	if m.Bucket == c.lose && !c.loseDone.Swap(true) {
		c.settled = make(chan struct{})
		go func() { // the backend goes on after the client is gone
			time.Sleep(150 * time.Millisecond)
			if c.finishOK {
				c.fakeCluster.mu.Lock()
				c.fakeCluster.owner[m.Bucket] = m.To
				c.fakeCluster.mu.Unlock()
			}
			c.inflight.Add(-1)
			close(c.settled)
		}()
		return io.ErrUnexpectedEOF
	}
	defer c.inflight.Add(-1)
	return c.fakeCluster.RunMove(ctx, m, after)
}

// Recover waits for the backend, as the extension does for a move that is alive, and reports what it found.
func (c *lossCluster) Recover(_ context.Context, r MoveRow) (string, string, error) {
	if r.Move.Bucket == c.lose && c.settled != nil {
		<-c.settled
	}
	c.fakeCluster.mu.Lock()
	defer c.fakeCluster.mu.Unlock()
	if c.fakeCluster.owner[r.Move.Bucket] == r.Move.To {
		return MoveDone, "", nil
	}
	return MoveFailed, "the move was undone", nil
}

// RB-04: a lost connection is not a failed move. The move finished on the server, so it is recorded done
// with one attempt, and no other move was in flight beside it.
func TestLostConnectionOfAMoveThatFinishesIsRecordedDone(t *testing.T) {
	c := &lossCluster{fakeCluster: newCluster(map[int32]int32{0: 1, 1: 1}, map[int32]int32{0: 2, 1: 2}), lose: 0, finishOK: true}
	r := &memRows{}
	_, _ = r.CreateRun(context.Background(), Options{}, nil)
	if err := testLoop(c, r).Moves(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	if m, _ := r.state("shop/1/0"); m.State != MoveDone || m.Attempts != 1 {
		t.Errorf("bucket 0: %+v", m)
	}
	if v := Judge(mustMoves(t, r), 3); v.Outcome != OutcomeCompleted || v.Failed != 0 {
		t.Errorf("verdict %+v", v)
	}
	if n := c.maxIn.Load(); n != 1 {
		t.Errorf("%d moves were in flight at once", n)
	}
}

// RB-04: when the backend undid the move the extension says so, and the move is a failed attempt that is tried again.
func TestLostConnectionOfAMoveThatWasUndoneIsRetried(t *testing.T) {
	c := &lossCluster{fakeCluster: newCluster(map[int32]int32{0: 1}, map[int32]int32{0: 2}), lose: 0}
	r := &memRows{}
	_, _ = r.CreateRun(context.Background(), Options{}, nil)
	if err := testLoop(c, r).Moves(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	if m, _ := r.state("shop/1/0"); m.State != MoveDone || m.Attempts != 2 {
		t.Errorf("bucket 0: %+v", m)
	}
	if n := c.maxIn.Load(); n != 1 {
		t.Errorf("%d moves in flight at once", n)
	}
}

func mustMoves(t *testing.T, r *memRows) []MoveRow {
	t.Helper()
	m, err := r.Moves(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// flaky is a cluster that cannot be inspected for a while.
type flaky struct {
	*fakeCluster
	checkFails, planFails int
	checks, plans         int
}

func (f *flaky) Check(ctx context.Context, dbs []string) error {
	f.checks++
	if f.checks <= f.checkFails {
		return errors.New("dial error: connection refused")
	}
	return f.fakeCluster.Check(ctx, dbs)
}

func (f *flaky) Plan(ctx context.Context, dbs []string, o Options) ([]Move, error) {
	f.plans++
	if f.plans <= f.planFails {
		return nil, errors.New("dial error: connection refused")
	}
	return f.fakeCluster.Plan(ctx, dbs, o)
}

// RB-05: a refused connection is retried in Check and in Plan alike; a short outage does not stop the run.
func TestTransientErrorsAreRetriedInCheckAndPlan(t *testing.T) {
	for name, f := range map[string]*flaky{
		"check": {fakeCluster: newCluster(map[int32]int32{0: 1}, map[int32]int32{0: 2}), checkFails: 2},
		"plan":  {fakeCluster: newCluster(map[int32]int32{0: 1}, map[int32]int32{0: 2}), planFails: 2},
	} {
		t.Run(name, func(t *testing.T) {
			r := &memRows{}
			_, _ = r.CreateRun(context.Background(), Options{}, nil)
			if err := testLoop(f, r).Moves(context.Background(), Options{}); err != nil {
				t.Fatal(err)
			}
			if m, _ := r.state("shop/1/0"); m.State != MoveDone {
				t.Errorf("bucket 0: %+v", m)
			}
		})
	}
}

// RB-05: an outage that lasts stops the run the same way wherever it hits, as a safety stop with a fixed
// text (no connection string), never as a failed task.
func TestLastingOutagePausesTheRunWhereverItHits(t *testing.T) {
	for name, f := range map[string]*flaky{
		"check": {fakeCluster: newCluster(map[int32]int32{0: 1}, map[int32]int32{0: 2}), checkFails: 100},
		"plan":  {fakeCluster: newCluster(map[int32]int32{0: 1}, map[int32]int32{0: 2}), planFails: 100},
	} {
		t.Run(name, func(t *testing.T) {
			r := &memRows{}
			_, _ = r.CreateRun(context.Background(), Options{}, nil)
			err := testLoop(f, r).Moves(context.Background(), Options{})
			var pe *task.PauseError
			if !errors.As(err, &pe) {
				t.Fatalf("want a pause, got %v", err)
			}
			run, _ := r.Run(context.Background())
			if run.Outcome != OutcomeStopped || run.StoppedBy != "safetyRule" || run.Error != "The cluster could not be inspected; the log of kingresd has the reason." {
				t.Errorf("run %+v", run)
			}
		})
	}
}

// RB-06: a plan that never empties (something undoes every move) ends the run as a safety stop with a
// number, not after hours.
func TestNonConvergingPlanStopsTheRun(t *testing.T) {
	c := &undoing{fakeCluster: newCluster(map[int32]int32{0: 1}, map[int32]int32{0: 2})}
	r := &memRows{}
	_, _ = r.CreateRun(context.Background(), Options{}, nil)
	err := testLoop(c, r).Moves(context.Background(), Options{})
	var pe *task.PauseError
	if !errors.As(err, &pe) {
		t.Fatalf("want a pause, got %v", err)
	}
	run, _ := r.Run(context.Background())
	if run.Outcome != OutcomeStopped || run.StoppedBy != "safetyRule" || run.Error == "" {
		t.Errorf("run %+v", run)
	}
	if n := len(c.runs); n > MaxMovesFactor*1+MovesSlack+1 {
		t.Errorf("%d moves ran", n)
	}
}

// undoing flips the wish of the plan after every move, like two automations toggling shouldHaveShards.
type undoing struct{ *fakeCluster }

func (u *undoing) RunMove(ctx context.Context, m Move, after int64) error {
	err := u.fakeCluster.RunMove(ctx, m, after)
	u.fakeCluster.mu.Lock()
	u.fakeCluster.target[m.Bucket] = m.From
	u.fakeCluster.mu.Unlock()
	return err
}

// RB-07: a bucket that used its attempts in a round that a person ended by starting again is tried again in the
// new round (the old rows stay as history), and the skip of an earlier round does not carry over.
func TestStartAgainBeginsANewRound(t *testing.T) {
	c := newCluster(map[int32]int32{0: 1, 1: 1}, map[int32]int32{0: 2, 1: 2})
	c.failFor[0] = 100
	c.skip[1] = &SkipError{Reason: SkipTargetDiskLow, Detail: "full"}
	r := &memRows{}
	_, _ = r.CreateRun(context.Background(), Options{}, nil)
	if err := testLoop(c, r).Moves(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	if m, _ := r.state("shop/1/0"); m.State != MoveFailed || m.Attempts != 3 {
		t.Fatalf("first round: %+v", m)
	}
	// The operator repaired the cause and starts again: a new round.
	c.mu.Lock()
	c.failFor[0], c.skip[1] = 0, nil
	c.mu.Unlock()
	r.run.Round++
	if err := testLoop(c, r).Moves(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	rows := mustMoves(t, r)
	if v := Judge(rows, 3); v.Outcome != OutcomeCompleted || v.Failed != 0 || v.Done != 2 {
		t.Errorf("verdict %+v rows %+v", v, rows)
	}
	if c.owner[0] != 2 || c.owner[1] != 2 {
		t.Errorf("owners %v", c.owner)
	}
	hist := 0
	for _, m := range rows {
		if m.Round == 0 {
			hist++
		}
	}
	if hist != 2 { // the failed row of bucket 0 and the skipped row of bucket 1 are kept
		t.Errorf("%d rows of the first round kept, want 2: %+v", hist, rows)
	}
}

// RB-08: a cancel request of the task stops the loop like a stop, so that the engine can end the task as cancelled.
func TestCancelRequestStopsTheLoop(t *testing.T) {
	c := newCluster(map[int32]int32{0: 1, 1: 1}, map[int32]int32{0: 2, 1: 2})
	r := &memRows{}
	_, _ = r.CreateRun(context.Background(), Options{}, nil)
	r.run.CancelRequested = true
	err := testLoop(c, r).Moves(context.Background(), Options{})
	var pe *task.PauseError
	if !errors.As(err, &pe) || len(c.runs) != 0 {
		t.Fatalf("err %v runs %v", err, c.runs)
	}
}

// A FATAL error of PostgreSQL (the backend was terminated) ends the session and says nothing about the move;
// an ERROR the extension raised ends the move.
func TestOnlyAnErrorOfTheStatementIsAnAnswerAboutTheMove(t *testing.T) {
	var me *MoveError
	if !errors.As(asMoveError(&pgconn.PgError{Severity: "ERROR", Message: "bucket is already being moved"}), &me) {
		t.Error("an ERROR of the statement is the extension's answer")
	}
	for _, sev := range []string{"FATAL", "PANIC"} {
		if errors.As(asMoveError(&pgconn.PgError{Severity: sev, Code: "57P01", Message: "terminating connection"}), &me) {
			t.Errorf("a %s error was taken as an answer about the move", sev)
		}
	}
	if errors.As(asMoveError(io.ErrUnexpectedEOF), &me) {
		t.Error("an EOF was taken as an answer about the move")
	}
}

// SPDX-License-Identifier: AGPL-3.0-only

package rebalance

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// notStartedCluster answers the first n calls of a bucket with "nothing was started" (the cluster lock was busy
// for longer than the call could wait), then lets the move run.
type notStartedCluster struct {
	*fakeCluster
	bucket int32
	n      int32
	calls  atomic.Int32
}

func (c *notStartedCluster) RunMove(ctx context.Context, m Move, after int64) error {
	if m.Bucket == c.bucket && c.calls.Add(1) <= c.n {
		return &NotStartedError{Err: errors.New("the extension did not record the move within 30s")}
	}
	return c.fakeCluster.RunMove(ctx, m, after)
}

// RBR-01: a call that was withdrawn before the extension recorded the move is not an attempt at the move. Two such
// tries in a row, then the move: it is done with one attempt, and a bucket never goes near the limit of three.
func TestAWithdrawnCallIsNotAnAttempt(t *testing.T) {
	c := &notStartedCluster{fakeCluster: newCluster(map[int32]int32{0: 1}, map[int32]int32{0: 2}), bucket: 0, n: notStartedTries - 1}
	r := &memRows{}
	_, _ = r.CreateRun(context.Background(), Options{}, nil)
	if err := testLoop(c, r).Moves(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	m, _ := r.state("shop/1/0")
	if m.State != MoveDone || m.Attempts != 1 {
		t.Fatalf("the move: %+v, want done with one attempt", m)
	}
	if v := Judge(mustMoves(t, r), 3); v.Outcome != OutcomeCompleted || v.Failed != 0 {
		t.Errorf("verdict %+v", v)
	}
}

// When the cluster never accepts the move the run does not burn the attempts of the bucket: after notStartedTries
// withdrawn calls in a row it stops (a safety stop) and says why, and no attempt was recorded.
func TestWithdrawnCallsInARowStopTheRunWithoutFailingTheBucket(t *testing.T) {
	c := &notStartedCluster{fakeCluster: newCluster(map[int32]int32{0: 1}, map[int32]int32{0: 2}), bucket: 0, n: 100}
	r := &memRows{}
	_, _ = r.CreateRun(context.Background(), Options{}, nil)
	err := testLoop(c, r).Moves(context.Background(), Options{})
	var pe *task.PauseError
	if !errors.As(err, &pe) {
		t.Fatalf("want a pause, got %v", err)
	}
	if run, _ := r.Run(context.Background()); run.Outcome != OutcomeStopped || run.StoppedBy != "safetyRule" {
		t.Errorf("run %+v", run)
	}
	for _, m := range mustMoves(t, r) {
		if m.State == MoveFailed || m.Attempts > 1 {
			t.Errorf("a withdrawn call was recorded as an attempt: %+v", m)
		}
	}
	if n := c.calls.Load(); n != notStartedTries {
		t.Errorf("%d calls, want %d", n, notStartedTries)
	}
}

// flakyRows is memRows whose system database cannot be reached for a while: every call fails with a refused
// connection until `until` returns false for the number of calls made.
type flakyRows struct {
	*memRows
	down atomic.Bool
	hits atomic.Int32
	upAt atomic.Int32
}

func (f *flakyRows) fail() error {
	if f.down.Load() {
		if n := f.hits.Add(1); f.upAt.Load() > 0 && n >= f.upAt.Load() {
			f.down.Store(false) // the database is back after this many refused calls
			return nil
		}
		return errors.New("failed to connect to the system database: connection refused")
	}
	return nil
}
func (f *flakyRows) Run(ctx context.Context) (Run, error) {
	if err := f.fail(); err != nil {
		return Run{}, err
	}
	return f.memRows.Run(ctx)
}
func (f *flakyRows) UpdateMove(ctx context.Context, seq int, ext int64, phase string, pct float64) error {
	if err := f.fail(); err != nil {
		return err
	}
	return f.memRows.UpdateMove(ctx, seq, ext, phase, pct)
}
func (f *flakyRows) FinishMove(ctx context.Context, seq int, state, text, reason string, size *int64) error {
	if err := f.fail(); err != nil {
		return err
	}
	return f.memRows.FinishMove(ctx, seq, state, text, reason, size)
}
func (f *flakyRows) Moves(ctx context.Context) ([]MoveRow, error) {
	if err := f.fail(); err != nil {
		return nil, err
	}
	return f.memRows.Moves(ctx)
}
func (f *flakyRows) StartMove(ctx context.Context, m Move, from, to string, attempts int) (int, error) {
	if err := f.fail(); err != nil {
		return 0, err
	}
	return f.memRows.StartMove(ctx, m, from, to, attempts)
}
func (f *flakyRows) SetPlanned(ctx context.Context, n int) error {
	if err := f.fail(); err != nil {
		return err
	}
	return f.memRows.SetPlanned(ctx, n)
}
func (f *flakyRows) Settle(ctx context.Context, outcome, by, text string, after []Placement) error {
	if err := f.fail(); err != nil {
		return err
	}
	return f.memRows.Settle(ctx, outcome, by, text, after)
}

// RBR-02: the system database cannot be reached while a move is in flight (the polls fail, then the record of the
// end of the move fails for a moment too). The move is not left: the call goes on and finishes, the move is
// recorded done once the database is back, with one attempt, and no failure is reported for a move that
// succeeded. Before, the first failed write ended the step while the move went on.
func TestABlipOfTheSystemDatabaseMidMoveDoesNotFailTheMove(t *testing.T) {
	c := newCluster(map[int32]int32{0: 1}, map[int32]int32{0: 2})
	c.block[0] = make(chan struct{})
	rows := &flakyRows{memRows: &memRows{}}
	_, _ = rows.CreateRun(context.Background(), Options{}, nil)
	l := testLoop(c, rows)
	done := make(chan error, 1)
	go func() { done <- l.Moves(context.Background(), Options{}) }()
	for { // the call is in flight
		c.mu.Lock()
		n := len(c.runs)
		c.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	rows.down.Store(true)
	for rows.hits.Load() < 3 { // the polls meet the outage
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-done:
		t.Fatalf("the loop returned while the move was in flight: %v", err)
	default:
	}
	// The move ends while the database is still away; the database comes back a moment later.
	rows.upAt.Store(rows.hits.Load() + 2) // the record of the end meets the outage twice more, then the database is back
	close(c.block[0])
	if err := <-done; err != nil {
		t.Fatalf("the run: %v", err)
	}
	if m, _ := rows.state("shop/1/0"); m.State != MoveDone || m.Attempts != 1 {
		t.Fatalf("the move: %+v, want done with one attempt", m)
	}
	if c.owner[0] != 2 {
		t.Error("the bucket was not moved")
	}
}

// When the system database stays away for good the run pauses (a safety stop) and nothing is recorded as a failure.
func TestASystemDatabaseThatStaysAwayPausesTheRun(t *testing.T) {
	c := newCluster(map[int32]int32{0: 1}, map[int32]int32{0: 2})
	rows := &flakyRows{memRows: &memRows{}}
	_, _ = rows.CreateRun(context.Background(), Options{}, nil)
	rows.down.Store(true)
	err := testLoop(c, rows).Moves(context.Background(), Options{})
	var pe *task.PauseError
	if !errors.As(err, &pe) {
		t.Fatalf("want a pause, got %v", err)
	}
	if len(c.runs) != 0 {
		t.Errorf("a move was started with the system database away: %v", c.runs)
	}
}

// stuckRecover waits for a move that never settles (a dead target, the recovery of the extension waiting for a
// session that is alive) until its context ends.
type stuckRecover struct{ *fakeCluster }

func (s *stuckRecover) Recover(ctx context.Context, _ MoveRow) (string, string, error) {
	<-ctx.Done()
	return "", "", ctx.Err()
}

// RBR-06: a stop does not wait for a move that is stuck: the failover that has to stop the rebalance cannot
// wait for the bound of the recovery. The row stays running, for the extension's recovery and the next start.
func TestAStopEndsTheWaitForAStuckMove(t *testing.T) {
	c := &stuckRecover{newCluster(map[int32]int32{0: 1}, map[int32]int32{0: 2})}
	r := &memRows{}
	_, _ = r.CreateRun(context.Background(), Options{}, nil)
	now := time.Unix(1000, 0)
	r.moves = []MoveRow{{Seq: 1, Move: Move{Database: "shop", Colocation: 1, Bucket: 0, From: 1, To: 2}, State: MoveRunning, Attempts: 1, StartedAt: now}}
	done := make(chan error, 1)
	go func() { done <- testLoop(c, r).Moves(context.Background(), Options{}) }()
	time.Sleep(50 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("the loop did not wait for the move: %v", err)
	default:
	}
	r.stop()
	select {
	case err := <-done:
		var pe *task.PauseError
		if !errors.As(err, &pe) {
			t.Fatalf("want a pause, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stop did not end the wait for a stuck move")
	}
	if m, _ := r.state("shop/1/0"); m.State != MoveRunning {
		t.Errorf("the row of the stuck move is %s, it stays running for the recovery", m.State)
	}
	if run, _ := r.Run(context.Background()); run.Outcome != OutcomeStopped {
		t.Errorf("run %+v", run)
	}
}

// RBR-03: the totals of a run count each bucket once: a failure or a skip of an earlier round that a later round
// repaired is history, not a count.
func TestTotalsDoNotCountTheRowsOfAnEarlierRoundThatWereRepaired(t *testing.T) {
	mv := func(seq int, bucket int32, state string, round, attempts int) MoveRow {
		return MoveRow{Seq: seq, Move: Move{Database: "shop", Colocation: 1, Bucket: bucket, From: 1, To: 2}, State: state, Round: round, Attempts: attempts}
	}
	rows := []MoveRow{
		mv(1, 0, MoveDone, 0, 1),
		mv(2, 1, MoveFailed, 0, 3), // given up in round 0
		mv(3, 2, MoveSkipped, 0, 0),
		mv(4, 1, MoveDone, 1, 1), // moved in round 1
		mv(5, 2, MoveDone, 1, 1),
	}
	v := Judge(rows, 3)
	if v.Outcome != OutcomeCompleted || v.Done != 3 || v.Failed != 0 || v.Skipped != 0 {
		t.Errorf("verdict %+v, want completed with 3 done, nothing failed or skipped", v)
	}
	if got := len(Current(rows)); got != 3 {
		t.Errorf("%d rows count, want 3", got)
	}
	p := progressOf(Run{Planned: 3}, rows)
	if p.MovesDone != 3 || p.MovesFailed != 0 || p.MovesSkipped != 0 || p.MovesTotal != 3 || p.Percent != 100 {
		t.Errorf("progress %+v", p)
	}
	// A bucket that is still failed or skipped in the last round does count.
	rows = append(rows, mv(6, 3, MoveFailed, 1, 3), mv(7, 4, MoveSkipped, 1, 0))
	if v := Judge(rows, 3); v.Outcome != OutcomeFailed || v.Failed != 1 || v.Skipped != 1 || v.Done != 3 {
		t.Errorf("verdict %+v", v)
	}
}

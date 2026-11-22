// SPDX-License-Identifier: AGPL-3.0-only

package rebalance

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// memRows is Rows in memory. epoch is the epoch the "lease" is held in: a call
// whose context carries another one is refused, like the system database does.
type memRows struct {
	mu       sync.Mutex
	run      Run
	moves    []MoveRow
	lost     bool
	startErr error
}

func (r *memRows) CreateRun(_ context.Context, o Options, before []Placement) (Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.run.TaskID == 0 {
		r.run = Run{TaskID: 1, Options: o, Before: before, StartedAt: time.Unix(1000, 0)}
	}
	return r.run, nil
}
func (r *memRows) Run(context.Context) (Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.run, nil
}
func (r *memRows) guard() error {
	if r.lost {
		return ErrLeaseLost
	}
	return nil
}
func (r *memRows) SetPlanned(_ context.Context, n int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.guard(); err != nil {
		return err
	}
	r.run.Planned = n
	return nil
}
func (r *memRows) StartMove(_ context.Context, m Move, from, to string, attempts int, mark int64) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.guard(); err != nil {
		return 0, err
	}
	if r.startErr != nil {
		return 0, r.startErr
	}
	for i := range r.moves {
		x := &r.moves[i]
		if x.Move.Key() == m.Key() && x.Round == r.run.Round && (x.State == MoveFailed || x.State == MoveCancelled) {
			x.State, x.Attempts, x.FinishedAt, x.Error, x.After, x.Marked = MoveRunning, attempts, nil, "", mark, true
			return x.Seq, nil
		}
	}
	seq := len(r.moves) + 1
	r.moves = append(r.moves, MoveRow{Seq: seq, Move: m, FromServer: from, ToServer: to, State: MoveRunning, Attempts: attempts, Size: m.Size, StartedAt: time.Unix(1000, 0), Round: r.run.Round, After: mark, Marked: true})
	return seq, nil
}
func (r *memRows) UpdateMove(_ context.Context, seq int, ext int64, phase string, pct float64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.guard(); err != nil {
		return err
	}
	x := &r.moves[seq-1]
	if ext > 0 {
		x.ExtMoveID = ext
	}
	x.Phase, x.Percent = phase, pct
	return nil
}
func (r *memRows) FinishMove(_ context.Context, seq int, state, text, _ string, size *int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.guard(); err != nil {
		return err
	}
	x := &r.moves[seq-1]
	now := time.Unix(5000, 0)
	x.State, x.Error, x.FinishedAt = state, text, &now
	if size != nil {
		x.Size = size
	}
	return nil
}
func (r *memRows) AddSkipped(_ context.Context, m Move, from, to, reason, detail string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.guard(); err != nil {
		return err
	}
	now := time.Unix(2000, 0)
	r.moves = append(r.moves, MoveRow{Seq: len(r.moves) + 1, Move: m, FromServer: from, ToServer: to, State: MoveSkipped, SkipReason: reason, Error: detail, FinishedAt: &now, Round: r.run.Round})
	return nil
}
func (r *memRows) Moves(context.Context) ([]MoveRow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]MoveRow(nil), r.moves...), nil
}
func (r *memRows) Settle(_ context.Context, outcome, by, text string, after []Placement) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.guard(); err != nil {
		return err
	}
	r.run.Outcome, r.run.StoppedBy, r.run.Error = outcome, by, text
	if after != nil {
		r.run.After = after
	}
	return nil
}
func (r *memRows) stop() {
	r.mu.Lock()
	r.run.StopRequested = true
	r.mu.Unlock()
}
func (r *memRows) unstop() {
	r.mu.Lock()
	r.run.StopRequested, r.run.Outcome = false, ""
	r.mu.Unlock()
}
func (r *memRows) state(key string) (MoveRow, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.moves {
		if m.Move.Key() == key {
			return m, true
		}
	}
	return MoveRow{}, false
}

// fakeCluster is Backend: a bucket map that a move changes, and hooks.
type fakeCluster struct {
	mu      sync.Mutex
	owner   map[int32]int32 // bucket -> group
	target  map[int32]int32 // where the plan wants each bucket
	runs    []string
	failFor map[int32]int // bucket -> number of failures still to give
	block   map[int32]chan struct{}
	// canCancel says whether CancelMove succeeds.
	canCancel bool
	cancelled chan struct{}
	checkErr  error
	skip      map[int32]*SkipError
	recovered string
	cleaned   int
}

func newCluster(owner, target map[int32]int32) *fakeCluster {
	return &fakeCluster{owner: owner, target: target, failFor: map[int32]int{}, block: map[int32]chan struct{}{}, skip: map[int32]*SkipError{}, canCancel: true, cancelled: make(chan struct{}, 8)}
}

func (f *fakeCluster) Databases(context.Context, Options) ([]string, error) {
	return []string{"shop"}, nil
}
func (f *fakeCluster) Check(context.Context, []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checkErr
}
func (f *fakeCluster) Plan(context.Context, []string) ([]Move, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Move
	for b := int32(0); b < 16; b++ {
		if f.owner[b] != f.target[b] {
			sz := int64(1000)
			out = append(out, Move{Database: "shop", Colocation: 1, Bucket: b, From: f.owner[b], To: f.target[b], Size: &sz})
		}
	}
	return out, nil
}
func (f *fakeCluster) Placement(context.Context, []string) ([]Placement, error) {
	return []Placement{{Database: "shop", Server: "a", Buckets: 1}}, nil
}
func (f *fakeCluster) ServerOf(_ context.Context, _ string, g int32) string {
	return fmt.Sprintf("s%d", g)
}
func (f *fakeCluster) CheckMove(_ context.Context, m Move) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if se := f.skip[m.Bucket]; se != nil {
		return se
	}
	return nil
}
func (f *fakeCluster) Mark(context.Context, Move) (int64, error) { return 0, nil }
func (f *fakeCluster) RunMove(ctx context.Context, m Move, _ int64) error {
	f.mu.Lock()
	f.runs = append(f.runs, fmt.Sprintf("%d", m.Bucket))
	ch := f.block[m.Bucket]
	fail := f.failFor[m.Bucket] > 0
	if fail {
		f.failFor[m.Bucket]--
	}
	f.mu.Unlock()
	if ch != nil {
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
		f.mu.Lock()
		if f.failFor[m.Bucket] > 0 {
			f.failFor[m.Bucket]--
			fail = true
		}
		f.mu.Unlock()
	}
	if fail {
		return &MoveError{Err: errors.New("simulated failure of the copy")}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.owner[m.Bucket] = m.To
	return nil
}
func (f *fakeCluster) Progress(context.Context, Move, Ref) (Progress, error) {
	return Progress{Found: true, ExtMoveID: 7, Phase: "copying", Percent: 30}, nil
}
func (f *fakeCluster) CancelMove(_ context.Context, m Move, _ Ref) (bool, error) {
	f.mu.Lock()
	can := f.canCancel
	ch := f.block[m.Bucket]
	f.mu.Unlock()
	if !can {
		return false, nil
	}
	f.cancelled <- struct{}{}
	// The extension undoes the move: the running session returns an error.
	if ch != nil {
		f.mu.Lock()
		f.failFor[m.Bucket]++
		f.mu.Unlock()
		close(ch)
		f.mu.Lock()
		delete(f.block, m.Bucket)
		f.mu.Unlock()
	}
	return true, nil
}
func (f *fakeCluster) Recover(_ context.Context, r MoveRow) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recovered = r.Move.Key()
	if f.owner[r.Move.Bucket] != r.Move.To {
		return MoveFailed, "the move did not reach the target", nil
	}
	return MoveDone, "", nil
}
func (f *fakeCluster) Cleanup(context.Context) { f.cleaned++ }

func testLoop(c Backend, r Rows) *Loop {
	now := time.Unix(5000, 0)
	var mu sync.Mutex
	return &Loop{Backend: c, Rows: r, MaxAttempts: 3, Backoff: time.Second, Poll: 5 * time.Millisecond,
		Now: func() time.Time { mu.Lock(); defer mu.Unlock(); return now },
		Sleep: func(_ context.Context, d time.Duration) {
			mu.Lock()
			now = now.Add(d)
			mu.Unlock()
		}}
}

func ctxWithFence() context.Context { return context.Background() }

// The loop plans again before every move and runs the plan's moves in order.
func TestMovesFollowThePlanAndPlanAgain(t *testing.T) {
	c := newCluster(map[int32]int32{0: 1, 1: 1, 2: 1, 3: 2}, map[int32]int32{0: 3, 1: 1, 2: 3, 3: 2})
	r := &memRows{}
	_, _ = r.CreateRun(context.Background(), Options{}, nil)
	l := testLoop(c, r)
	// A move that changes the target while the first runs: the plan adapts.
	c.mu.Lock()
	c.target[1] = 3
	c.mu.Unlock()
	if err := l.Moves(ctxWithFence(), Options{}); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(c.runs); got != "[0 1 2]" {
		t.Errorf("moves run: %s", got)
	}
	for b, g := range c.owner {
		if g != c.target[b] {
			t.Errorf("bucket %d is in %d, wanted %d", b, g, c.target[b])
		}
	}
	run, _ := r.Run(context.Background())
	if run.Planned != 3 {
		t.Errorf("planned = %d", run.Planned)
	}
	if c.cleaned != 1 {
		t.Errorf("cleanup called %d times", c.cleaned)
	}
	moves, _ := r.Moves(context.Background())
	if v := Judge(moves, 3); v.Outcome != OutcomeCompleted || v.Done != 3 {
		t.Errorf("verdict %+v", v)
	}
}

// A move that fails is tried again after a backoff; the other buckets are not held up.
func TestFailedMoveIsRetriedThenSucceeds(t *testing.T) {
	c := newCluster(map[int32]int32{0: 1, 1: 1}, map[int32]int32{0: 2, 1: 2})
	c.failFor[0] = 2
	r := &memRows{}
	_, _ = r.CreateRun(context.Background(), Options{}, nil)
	if err := testLoop(c, r).Moves(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	m, _ := r.state("shop/1/0")
	if m.State != MoveDone || m.Attempts != 3 {
		t.Errorf("bucket 0: %+v", m)
	}
	// Bucket 1 ran while bucket 0 was backing off.
	if got := fmt.Sprint(c.runs); got != "[0 1 0 0]" {
		t.Errorf("order: %s", got)
	}
	if o := c.owner[1]; o != 2 {
		t.Errorf("bucket 1 not moved")
	}
}

// A bucket that fails every time is given up after the attempts; the run is judged failed, the rest is done.
func TestMoveGivenUpAfterTheAttempts(t *testing.T) {
	c := newCluster(map[int32]int32{0: 1, 1: 1}, map[int32]int32{0: 2, 1: 2})
	c.failFor[0] = 100
	r := &memRows{}
	_, _ = r.CreateRun(context.Background(), Options{}, nil)
	if err := testLoop(c, r).Moves(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	moves, _ := r.Moves(context.Background())
	v := Judge(moves, 3)
	if v.Outcome != OutcomeFailed || v.Failed != 1 || v.Done != 1 {
		t.Errorf("verdict %+v", v)
	}
	if m, _ := r.state("shop/1/0"); m.Attempts != 3 || m.State != MoveFailed {
		t.Errorf("bucket 0: %+v", m)
	}
	if n := len(c.runs); n != 4 { // 3 tries of bucket 0, 1 of bucket 1
		t.Errorf("runs: %v", c.runs)
	}
}

// A move that cannot be started under the conditions of the move is skipped, recorded, and not tried again.
func TestSkippedMoveIsRecordedOnce(t *testing.T) {
	c := newCluster(map[int32]int32{0: 1, 1: 1}, map[int32]int32{0: 2, 1: 2})
	c.skip[0] = &SkipError{Reason: SkipTargetDiskLow, Detail: "s2 has 10 bytes free"}
	r := &memRows{}
	_, _ = r.CreateRun(context.Background(), Options{}, nil)
	if err := testLoop(c, r).Moves(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	moves, _ := r.Moves(context.Background())
	skips := 0
	for _, m := range moves {
		if m.State == MoveSkipped {
			skips++
			if m.SkipReason != SkipTargetDiskLow {
				t.Errorf("reason %q", m.SkipReason)
			}
		}
	}
	if skips != 1 || fmt.Sprint(c.runs) != "[1]" {
		t.Errorf("skips %d, runs %v", skips, c.runs)
	}
	if v := Judge(moves, 3); v.Outcome != OutcomeCompleted || v.Skipped != 1 {
		t.Errorf("verdict %+v", v)
	}
}

// A stop cancels the move that has not reached its switch, pauses the task, and starting again goes on.
func TestStopCancelsTheRunningMoveAndStartContinues(t *testing.T) {
	c := newCluster(map[int32]int32{0: 1, 1: 1}, map[int32]int32{0: 2, 1: 2})
	c.block[0] = make(chan struct{})
	r := &memRows{}
	_, _ = r.CreateRun(context.Background(), Options{}, nil)
	l := testLoop(c, r)
	done := make(chan error, 1)
	go func() { done <- l.Moves(context.Background(), Options{}) }()
	for { // wait until the move runs
		c.mu.Lock()
		n := len(c.runs)
		c.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	r.stop()
	err := <-done
	var pe *task.PauseError
	if !errors.As(err, &pe) {
		t.Fatalf("want a pause, got %v", err)
	}
	if m, _ := r.state("shop/1/0"); m.State != MoveCancelled {
		t.Errorf("the move is %s, not cancelled", m.State)
	}
	if run, _ := r.Run(context.Background()); run.Outcome != OutcomeStopped || run.StoppedBy != "operator" {
		t.Errorf("run %+v", run)
	}
	// Start again: the cancelled bucket is moved, not counted as a failed attempt beyond its one.
	r.unstop()
	if err := l.Moves(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	if c.owner[0] != 2 || c.owner[1] != 2 {
		t.Errorf("owners %v", c.owner)
	}
}

// A move past the point where it can be cancelled is allowed to finish; the task pauses after it.
func TestStopLetsAMoveInTheSwitchFinish(t *testing.T) {
	c := newCluster(map[int32]int32{0: 1, 1: 1}, map[int32]int32{0: 2, 1: 2})
	c.canCancel = false
	c.block[0] = make(chan struct{})
	r := &memRows{}
	_, _ = r.CreateRun(context.Background(), Options{}, nil)
	l := testLoop(c, r)
	done := make(chan error, 1)
	go func() { done <- l.Moves(context.Background(), Options{}) }()
	for {
		c.mu.Lock()
		n := len(c.runs)
		c.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	r.stop()
	time.Sleep(30 * time.Millisecond) // several polls: the stop is asked again each time, in vain
	close(c.block[0])
	err := <-done
	var pe *task.PauseError
	if !errors.As(err, &pe) {
		t.Fatalf("want a pause, got %v", err)
	}
	if m, _ := r.state("shop/1/0"); m.State != MoveDone {
		t.Errorf("the move is %s, want done", m.State)
	}
	if c.owner[1] != 1 {
		t.Errorf("bucket 1 moved after the stop")
	}
}

// A move left running by a run that died is settled through the extension before anything else is planned.
func TestLeftoverMoveIsRecoveredFirst(t *testing.T) {
	c := newCluster(map[int32]int32{0: 1, 1: 1}, map[int32]int32{0: 2, 1: 2})
	r := &memRows{}
	_, _ = r.CreateRun(context.Background(), Options{}, nil)
	sz := int64(1000)
	r.moves = []MoveRow{{Seq: 1, Move: Move{Database: "shop", Colocation: 1, Bucket: 0, From: 1, To: 2, Size: &sz}, State: MoveRunning, Attempts: 1, ExtMoveID: 9}}
	c.owner[0] = 2 // the extension finished it
	if err := testLoop(c, r).Moves(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	if c.recovered != "shop/1/0" {
		t.Errorf("recovered %q", c.recovered)
	}
	if m, _ := r.state("shop/1/0"); m.State != MoveDone {
		t.Errorf("leftover: %+v", m)
	}
	if fmt.Sprint(c.runs) != "[1]" {
		t.Errorf("runs %v", c.runs)
	}
}

// A condition of the cluster that stops holding between moves stops the run, with the reason.
func TestSafetyRuleStopsTheRun(t *testing.T) {
	c := newCluster(map[int32]int32{0: 1}, map[int32]int32{0: 2})
	c.checkErr = problem.New(problem.ClusterInconsistent, "check_cluster reports stalled_move")
	r := &memRows{}
	_, _ = r.CreateRun(context.Background(), Options{}, nil)
	err := testLoop(c, r).Moves(context.Background(), Options{})
	var pe *task.PauseError
	if !errors.As(err, &pe) {
		t.Fatalf("want a pause, got %v", err)
	}
	if run, _ := r.Run(context.Background()); run.Outcome != OutcomeStopped || run.StoppedBy != "safetyRule" || run.Error == "" {
		t.Errorf("run %+v", run)
	}
	if len(c.runs) != 0 {
		t.Errorf("a move ran: %v", c.runs)
	}
}

// A run that lost its lease starts nothing: the store refuses the record of the move, and the move is never run.
func TestLostLeaseStartsNothing(t *testing.T) {
	c := newCluster(map[int32]int32{0: 1}, map[int32]int32{0: 2})
	r := &memRows{}
	_, _ = r.CreateRun(context.Background(), Options{}, nil)
	r.lost = true
	err := testLoop(c, r).Moves(context.Background(), Options{})
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("got %v", err)
	}
	if len(c.runs) != 0 {
		t.Errorf("a move ran: %v", c.runs)
	}
}

// A cancelled context (shutdown, lost lease) leaves the move row running for the next run to recover.
func TestShutdownLeavesTheMoveForTheRecovery(t *testing.T) {
	c := newCluster(map[int32]int32{0: 1}, map[int32]int32{0: 2})
	c.block[0] = make(chan struct{})
	r := &memRows{}
	_, _ = r.CreateRun(context.Background(), Options{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- testLoop(c, r).Moves(ctx, Options{}) }()
	for {
		c.mu.Lock()
		n := len(c.runs)
		c.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if m, _ := r.state("shop/1/0"); m.State != MoveRunning {
		t.Errorf("the move is %s; it must stay running for the recovery", m.State)
	}
}

func TestJudge(t *testing.T) {
	rows := []MoveRow{
		{Seq: 1, Move: Move{Database: "d", Bucket: 1}, State: MoveDone, Attempts: 1},
		{Seq: 2, Move: Move{Database: "d", Bucket: 2}, State: MoveFailed, Attempts: 3},
		{Seq: 3, Move: Move{Database: "d", Bucket: 3}, State: MoveFailed, Attempts: 1}, // plan changed, not given up
		{Seq: 4, Move: Move{Database: "d", Bucket: 4}, State: MoveSkipped},
	}
	v := Judge(rows, 3)
	if v.Outcome != OutcomeFailed || v.Failed != 1 || v.Done != 1 || v.Skipped != 1 {
		t.Errorf("%+v", v)
	}
	if v := Judge(rows[:1], 3); v.Outcome != OutcomeCompleted {
		t.Errorf("%+v", v)
	}
}

// UndoStart takes back a try that started nothing: no row for a first try, a failed row with the attempts of
// the tries before it for a later one.
func (r *memRows) UndoStart(_ context.Context, seq, attempts int, why string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.guard(); err != nil {
		return err
	}
	x := &r.moves[seq-1]
	if attempts <= 1 {
		r.moves = r.moves[:seq-1] // the real rows delete it; it is the newest here
		return nil
	}
	now := time.Unix(5000, 0)
	x.State, x.Attempts, x.FinishedAt, x.Error = MoveFailed, attempts-1, &now, why
	return nil
}

// ST-05: the loop keeps the mark of every move with the row before it makes the call, so that a holder that
// takes over has it.
func TestEveryMoveKeepsItsMarkWithItsRow(t *testing.T) {
	c := newCluster(map[int32]int32{0: 1, 1: 1}, map[int32]int32{0: 2, 1: 2})
	r := &memRows{}
	_, _ = r.CreateRun(context.Background(), Options{}, nil)
	l := testLoop(c, r)
	if err := l.Moves(ctxWithFence(), Options{}); err != nil {
		t.Fatal(err)
	}
	moves, _ := r.Moves(context.Background())
	if len(moves) != 2 {
		t.Fatalf("%d moves", len(moves))
	}
	for _, m := range moves {
		if !m.Marked {
			t.Errorf("the row of %s has no mark", m.Move.Key())
		}
	}
}

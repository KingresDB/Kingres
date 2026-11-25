// SPDX-License-Identifier: AGPL-3.0-only

package task_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// blockingDef is a one-step task whose step reports that it started and then
// waits for release or for the cancellation of its context.
func blockingDef(c *counter, started chan<- struct{}, release <-chan struct{}, sawCancel chan<- struct{}) task.Definition {
	return task.Definition{Kind: "test.kind", Steps: []task.Step{{Name: "work", Run: func(ctx context.Context, _ *task.Env) error {
		c.inc("work")
		started <- struct{}{}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			sawCancel <- struct{}{}
			return ctx.Err()
		}
	}}}}
}

// TestSkewedClockCannotTakeALiveLease is CP2-06: engine B's clock is five
// minutes ahead of A's. Both run on one store; the lease arithmetic uses the
// store's clock, so B finds A's lease alive and does not start the step a
// second time.
func TestSkewedClockCannotTakeALiveLease(t *testing.T) {
	dbClock := newClock()
	skewed := newClock()
	skewed.Add(5 * time.Minute)
	st := task.NewMemStoreClock(dbClock.Now)
	c := &counter{n: map[string]int{}}
	started, release, saw := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
	d := blockingDef(c, started, release, saw)
	a := engine(t, st, "A", dbClock, d)
	b := engine(t, st, "B", skewed, d)
	ctx := context.Background()
	if _, err := a.Submit(ctx, "test.kind", task.Subject{Type: "x", ID: "1"}, nil, "u"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := a.RunOnce(ctx); done <- err }()
	<-started // A is inside the step and holds the lease
	if ran, err := b.RunOnce(ctx); ran || err != nil {
		t.Fatalf("B ran=%v err=%v: it must not take a lease that is alive on the database's clock", ran, err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := c.get("work"); n != 1 {
		t.Errorf("the step ran %d times, want 1", n)
	}
}

// renewFixture runs a one-step task whose step waits; renewals are driven by tick
// and all fail once failRenew is switched on.
type renewFixture struct {
	clk                 *clock
	st                  *task.MemStore
	counter             *counter
	tick                chan time.Time
	started, release    chan struct{}
	sawCancel, finished chan struct{}
	done                chan error
	id                  int64
}

func newRenewFixture(t *testing.T) *renewFixture {
	t.Helper()
	f := &renewFixture{clk: newClock(), counter: &counter{n: map[string]int{}}, tick: make(chan time.Time),
		started: make(chan struct{}), release: make(chan struct{}), sawCancel: make(chan struct{}, 1), done: make(chan error, 1)}
	f.st = task.NewMemStoreClock(f.clk.Now)
	d := blockingDef(f.counter, f.started, f.release, f.sawCancel)
	a, err := task.NewEngine(f.st, []task.Definition{d}, task.Options{Holder: "A", Lease: 30 * time.Second, Now: f.clk.Now, StoreTries: 1, RenewEvery: f.tick})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tk, _ := a.Submit(ctx, "test.kind", task.Subject{Type: "x", ID: "1"}, nil, "u")
	f.id = tk.ID
	go func() { _, err := a.RunOnce(ctx); f.done <- err }()
	<-f.started
	f.st.FailOn(func(op string) error {
		if op == "renew" {
			return errors.New("the store is unreachable")
		}
		return nil
	})
	return f
}

// TestRenewalFailureWithTimeLeftDoesNotStopTheStep: a renewal that fails while the lease the
// engine last confirmed still has time left only logs.
func TestRenewalFailureWithTimeLeftDoesNotStopTheStep(t *testing.T) {
	f := newRenewFixture(t)
	f.clk.Add(5 * time.Second)
	// The second send returns only after the loop finished the first renewal, and the clock
	// has not moved since: the first failure was judged at +5 s.
	f.tick <- f.clk.Now()
	f.tick <- f.clk.Now()
	close(f.release)
	if err := <-f.done; err != nil {
		t.Fatalf("run: %v", err)
	}
	select {
	case <-f.sawCancel:
		t.Fatal("the step was stopped while the lease was still confirmed")
	default:
	}
}

// TestStepStopsWhenTheLeaseCannotBeRenewed is the second half of CP2-06: a leader that
// cannot renew stops its step once the lease it last confirmed may have expired, so that the
// server that takes the task over is the only one working on it.
func TestStepStopsWhenTheLeaseCannotBeRenewed(t *testing.T) {
	f := newRenewFixture(t)
	f.clk.Add(25 * time.Second) // lease 30 s minus a margin of 10 s has passed
	f.tick <- f.clk.Now()
	<-f.sawCancel
	if err := <-f.done; !errors.Is(err, task.ErrLeaseLost) {
		t.Fatalf("A ended with %v, want ErrLeaseLost", err)
	}
	// The lease runs out on the database's clock and B takes the task over.
	f.st.FailOn(nil)
	f.clk.Add(time.Minute)
	b := engine(t, f.st, "B", f.clk, task.Definition{Kind: "test.kind", Steps: []task.Step{{Name: "work", Run: func(context.Context, *task.Env) error { f.counter.inc("work"); return nil }}}})
	if ran, err := b.RunOnce(context.Background()); !ran || err != nil {
		t.Fatalf("B: %v %v", ran, err)
	}
	if got, _ := b.Get(context.Background(), f.id); got.State != task.Done {
		t.Errorf("state %s", got.State)
	}
}

// TestFencingEpochSeparatesRunsOfTheSameHolder: a run that lost its lease is
// refused even when the same holder id claimed the task again.
func TestFencingEpochSeparatesRunsOfTheSameHolder(t *testing.T) {
	clk := newClock()
	st := task.NewMemStoreClock(clk.Now)
	ctx := context.Background()
	e := engine(t, st, "A", clk, defn(&counter{n: map[string]int{}}, "only"))
	tk, _ := e.Submit(ctx, "test.kind", task.Subject{Type: "x", ID: "1"}, nil, "u")
	first, _ := st.Claim(ctx, "A", 30*time.Second)
	clk.Add(time.Minute)
	second, _ := st.Claim(ctx, "A", 30*time.Second)
	if first == nil || second == nil || second.Epoch != first.Epoch+1 {
		t.Fatalf("epochs: %+v %+v", first, second)
	}
	old := task.Lease{Holder: "A", Epoch: first.Epoch}
	if _, err := st.SetStep(ctx, tk.ID, old, 0, task.StepDone, 100); !errors.Is(err, task.ErrLeaseLost) {
		t.Errorf("SetStep of the old run: %v", err)
	}
	if err := st.Finish(ctx, tk.ID, old, task.Done, nil); !errors.Is(err, task.ErrLeaseLost) {
		t.Errorf("Finish of the old run: %v", err)
	}
	if _, err := st.SetStep(ctx, tk.ID, task.Lease{Holder: "A", Epoch: second.Epoch}, 0, task.StepDone, 100); err != nil {
		t.Errorf("SetStep of the current run: %v", err)
	}
}

// TestNoStepStartsOnAnExpiredLease: marking a step running is refused once the
// lease has expired on the store's clock, even if nobody has taken the task over
// yet (a holder that was paused must not start a non-idempotent step).
func TestNoStepStartsOnAnExpiredLease(t *testing.T) {
	clk := newClock()
	st := task.NewMemStoreClock(clk.Now)
	ctx := context.Background()
	e := engine(t, st, "A", clk, defn(&counter{n: map[string]int{}}, "only"))
	tk, _ := e.Submit(ctx, "test.kind", task.Subject{Type: "x", ID: "1"}, nil, "u")
	l, _ := st.Claim(ctx, "A", 30*time.Second)
	lease := task.Lease{Holder: "A", Epoch: l.Epoch}
	clk.Add(time.Minute)
	if _, err := st.SetStep(ctx, tk.ID, lease, 0, task.StepRunning, 0); !errors.Is(err, task.ErrLeaseLost) {
		t.Errorf("running mark on an expired lease: %v", err)
	}
}

// extendFixture is the renew fixture with a step that lengthens its lease to two minutes and then waits, as the stop of the map group does; the store
// becomes unreachable right after (it is the instance that is stopped).
func newExtendFixture(t *testing.T, d time.Duration, storeStops bool) *renewFixture {
	t.Helper()
	f := &renewFixture{clk: newClock(), counter: &counter{n: map[string]int{}}, tick: make(chan time.Time),
		started: make(chan struct{}), release: make(chan struct{}), sawCancel: make(chan struct{}, 1), done: make(chan error, 1)}
	f.st = task.NewMemStoreClock(f.clk.Now)
	def := task.Definition{Kind: "test.kind", Steps: []task.Step{{Name: "work", Run: func(ctx context.Context, env *task.Env) error {
		restore, err := env.ExtendLease(ctx, d)
		if err != nil {
			return err
		}
		defer restore()
		if storeStops {
			f.st.FailOn(func(op string) error {
				if op == "renew" {
					return errors.New("the store is stopped")
				}
				return nil
			})
		}
		f.started <- struct{}{}
		select {
		case <-f.release:
			return nil
		case <-ctx.Done():
			f.sawCancel <- struct{}{}
			return ctx.Err()
		}
	}}}}
	a, err := task.NewEngine(f.st, []task.Definition{def}, task.Options{Holder: "A", Lease: 30 * time.Second, Now: f.clk.Now, StoreTries: 1, RenewEvery: f.tick})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tk, _ := a.Submit(ctx, "test.kind", task.Subject{Type: "x", ID: "1"}, nil, "u")
	f.id = tk.ID
	go func() { _, err := a.RunOnce(ctx); f.done <- err }()
	<-f.started
	return f
}

// A step that lengthened its lease is not stopped when the store cannot be reached for longer than the lease of the engine (the clean stop of the map group
// outlasts it), and the store holds the task for that long, so no other server can take it over meanwhile.
func TestAStepWithALengthenedLeaseSurvivesAStoreThatCannotBeReached(t *testing.T) {
	f := newExtendFixture(t, 2*time.Minute, true)
	f.clk.Add(40 * time.Second) // beyond lease 30 s: it would have been stopped at 20 s
	f.tick <- f.clk.Now()
	f.tick <- f.clk.Now()
	b := engine(t, f.st, "B", f.clk, task.Definition{Kind: "test.kind", Steps: []task.Step{{Name: "work", Run: func(context.Context, *task.Env) error { f.counter.inc("work"); return nil }}}})
	if ran, err := b.RunOnce(context.Background()); ran || err != nil {
		t.Fatalf("B took over a task whose lease was lengthened: %v %v", ran, err)
	}
	select {
	case <-f.sawCancel:
		t.Fatal("the step was stopped inside its lengthened lease")
	default:
	}
	close(f.release)
	if err := <-f.done; err != nil {
		t.Fatalf("run: %v", err)
	}
}

// The step is stopped once the lengthened lease, less its margin, has passed without a renewal: the extension moves the end of the trust, it does not remove it.
func TestAStepWithALengthenedLeaseStopsWhenTheLengthenedLeaseRunsOut(t *testing.T) {
	f := newExtendFixture(t, 2*time.Minute, true)
	f.clk.Add(111 * time.Second) // 120 s minus a margin of 10 s has passed
	f.tick <- f.clk.Now()
	<-f.sawCancel
	if err := <-f.done; !errors.Is(err, task.ErrLeaseLost) {
		t.Fatalf("ended with %v, want ErrLeaseLost", err)
	}
}

// A lease that cannot be lengthened is an error of the step (the store did not take it), and a length that is not longer than the engine's changes nothing.
func TestAStepThatCannotLengthenItsLeaseIsToldSo(t *testing.T) {
	clk := newClock()
	st := task.NewMemStoreClock(clk.Now)
	var got error
	var short error
	var left time.Duration
	def := task.Definition{Kind: "test.kind", Steps: []task.Step{{Name: "work", Run: func(ctx context.Context, env *task.Env) error {
		_, short = env.ExtendLease(ctx, 10*time.Second)
		cur, _ := st.Get(ctx, env.Task.ID)
		left = cur.LeaseExpires.Sub(clk.Now())
		st.FailOn(func(op string) error { return errors.New("the store is stopped") })
		_, got = env.ExtendLease(ctx, time.Minute)
		return nil
	}}}}
	e := engine(t, st, "A", clk, def)
	e.Submit(context.Background(), "test.kind", task.Subject{Type: "x", ID: "1"}, nil, "u")
	_, _ = e.RunOnce(context.Background())
	if left < 25*time.Second {
		t.Errorf("a shorter length cut the lease to %v", left)
	}
	if short != nil {
		t.Errorf("a shorter length: %v", short)
	}
	if got == nil {
		t.Error("a lengthening that the store refused was reported as done")
	}
}

// A renewal that comes while the lease is lengthened keeps it as long as it was made (a renewal with the length of the engine would cut it back before the
// stop).
func TestARenewalDuringALengthenedLeaseKeepsItLengthened(t *testing.T) {
	f := newExtendFixture(t, 2*time.Minute, false)
	f.clk.Add(5 * time.Second)
	f.tick <- f.clk.Now()
	f.tick <- f.clk.Now() // returns once the first renewal is done
	got, _ := f.st.Get(context.Background(), f.id)
	if left := got.LeaseExpires.Sub(f.clk.Now()); left < 100*time.Second {
		t.Errorf("the lease has %v left after a renewal during the extension, want about two minutes", left)
	}
	close(f.release)
	if err := <-f.done; err != nil {
		t.Fatalf("run: %v", err)
	}
}

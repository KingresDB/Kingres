// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package scheduler_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
)

// view is a leader view the test changes.
type view struct {
	mu sync.Mutex
	id string
}

func (v *view) set(id string) { v.mu.Lock(); v.id = id; v.mu.Unlock() }
func (v *view) get(context.Context) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.id, nil
}

type fixture struct {
	v      *view
	s      *scheduler.Scheduler
	bumps  atomic.Int64
	bumpAt atomic.Int64 // unix nanoseconds of the last bump
	cancel context.CancelFunc
	done   chan struct{}
	t0     time.Time
}

func newFixture(t *testing.T, margin time.Duration) *fixture {
	t.Helper()
	f := &fixture{v: &view{}, done: make(chan struct{}), t0: time.Now()}
	s, err := scheduler.New(scheduler.Config{
		Self: "a", Leader: f.v.get, Margin: margin, Poll: 10 * time.Millisecond,
		Bump: func(context.Context) (scheduler.Term, error) {
			f.bumpAt.Store(time.Now().UnixNano())
			return scheduler.Term(f.bumps.Add(1)), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.s = s
	return f
}

func (f *fixture) start() {
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	go func() { f.s.Run(ctx); close(f.done) }()
}

func (f *fixture) stop(t *testing.T) {
	t.Helper()
	f.cancel()
	select {
	case <-f.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the scheduler did not stop")
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func TestAFollowerRunsNothingAndHoldsNoTerm(t *testing.T) {
	f := newFixture(t, 0)
	var runs atomic.Int64
	if err := f.s.Add(scheduler.Job{Name: "j", Every: 5 * time.Millisecond, Run: func(context.Context, scheduler.Term) error { runs.Add(1); return nil }}); err != nil {
		t.Fatal(err)
	}
	f.v.set("b")
	f.start()
	time.Sleep(150 * time.Millisecond)
	f.stop(t)
	if runs.Load() != 0 || f.bumps.Load() != 0 {
		t.Errorf("a follower ran %d passes and took %d terms", runs.Load(), f.bumps.Load())
	}
	if _, ok := f.s.Term(); ok {
		t.Error("a follower holds a term")
	}
}

func TestANewLeaderWaitsTheMarginThenTakesATermAndRunsTheJobsWithIt(t *testing.T) {
	const margin = 120 * time.Millisecond
	f := newFixture(t, margin)
	var got atomic.Int64
	if err := f.s.Add(scheduler.Job{Name: "j", Every: 5 * time.Millisecond, Run: func(_ context.Context, term scheduler.Term) error { got.Store(int64(term)); return nil }}); err != nil {
		t.Fatal(err)
	}
	f.v.set("a")
	start := time.Now()
	f.start()
	eventually(t, "the job runs", func() bool { return got.Load() != 0 })
	f.stop(t)
	if at := time.Unix(0, f.bumpAt.Load()); at.Sub(start) < margin {
		t.Errorf("the term was taken %v after the start, the margin is %v", at.Sub(start), margin)
	}
	if got.Load() != 1 {
		t.Errorf("the job ran with term %d, want 1", got.Load())
	}
}

func TestLosingLeadershipCancelsTheJobsAndTheNextLeadershipTakesAHigherTerm(t *testing.T) {
	f := newFixture(t, 0)
	var running atomic.Int32
	var lastTerm atomic.Int64
	var cancelled atomic.Int32
	if err := f.s.Add(scheduler.Job{Name: "long", Every: time.Hour, Run: func(ctx context.Context, term scheduler.Term) error {
		lastTerm.Store(int64(term))
		running.Add(1)
		<-ctx.Done()
		cancelled.Add(1)
		return ctx.Err()
	}}); err != nil {
		t.Fatal(err)
	}
	f.v.set("a")
	f.start()
	defer f.stop(t)
	eventually(t, "the first pass", func() bool { return running.Load() == 1 })
	if term, ok := f.s.Term(); !ok || term != 1 {
		t.Fatalf("Term() = %d, %v", term, ok)
	}
	f.v.set("b")
	eventually(t, "the job is cancelled", func() bool { return cancelled.Load() == 1 })
	eventually(t, "the term is dropped", func() bool { _, ok := f.s.Term(); return !ok })
	f.v.set("a")
	eventually(t, "the second leadership", func() bool { return running.Load() == 2 })
	if lastTerm.Load() != 2 {
		t.Errorf("the second leadership ran with term %d, want 2", lastTerm.Load())
	}
}

func TestAJobThatFindsTheTermOverEndsTheLeadership(t *testing.T) {
	f := newFixture(t, 0)
	var runs atomic.Int32
	if err := f.s.Add(scheduler.Job{Name: "deposed", Every: time.Hour, Run: func(context.Context, scheduler.Term) error {
		if runs.Add(1) == 1 {
			return scheduler.ErrDeposed
		}
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	f.v.set("a")
	f.start()
	defer f.stop(t)
	// The first period ends at once; the loop starts another because this server still seems to be the
	// leader, which takes a new term (the stored term would have been the other leader's in a real cluster).
	eventually(t, "a second term", func() bool { return f.bumps.Load() >= 2 })
}

func TestAPanickingJobDoesNotEndTheProcessOrTheOtherJobs(t *testing.T) {
	f := newFixture(t, 0)
	var ok atomic.Int32
	if err := f.s.Add(scheduler.Job{Name: "bad", Every: 5 * time.Millisecond, Run: func(context.Context, scheduler.Term) error { panic("boom") }}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Add(scheduler.Job{Name: "good", Every: 5 * time.Millisecond, Run: func(context.Context, scheduler.Term) error { ok.Add(1); return nil }}); err != nil {
		t.Fatal(err)
	}
	f.v.set("a")
	f.start()
	defer f.stop(t)
	eventually(t, "the good job runs several times", func() bool { return ok.Load() >= 3 })
}

func TestAFailingBumpMeansNoJobsAreRun(t *testing.T) {
	var runs atomic.Int32
	v := &view{id: "a"}
	s, err := scheduler.New(scheduler.Config{Self: "a", Leader: v.get, Poll: 5 * time.Millisecond,
		Bump: func(context.Context) (scheduler.Term, error) { return 0, errors.New("no database") }})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add(scheduler.Job{Name: "j", Every: time.Millisecond, Run: func(context.Context, scheduler.Term) error { runs.Add(1); return nil }}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	s.Run(ctx)
	if runs.Load() != 0 {
		t.Errorf("%d passes ran without a term", runs.Load())
	}
}

func TestAddRefusesBadJobs(t *testing.T) {
	f := newFixture(t, 0)
	ok := func(context.Context, scheduler.Term) error { return nil }
	for name, j := range map[string]scheduler.Job{
		"no name":      {Every: time.Second, Run: ok},
		"no function":  {Name: "x", Every: time.Second},
		"no interval":  {Name: "x", Run: ok},
		"jitter of 1":  {Name: "x", Every: time.Second, Jitter: 1, Run: ok},
		"negative one": {Name: "x", Every: time.Second, Jitter: -0.1, Run: ok},
	} {
		if err := f.s.Add(j); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if err := f.s.Add(scheduler.Job{Name: "x", Every: time.Second, Run: ok}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Add(scheduler.Job{Name: "x", Every: time.Second, Run: ok}); err == nil {
		t.Error("a second job of the same name was accepted")
	}
}

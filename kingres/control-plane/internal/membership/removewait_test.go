// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package membership

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// fakeRemoval answers the calls of the removal of one node: remove gives the errors of refusals in turn (nil after the last),
// cleaning the number of moves of the group that are cleaning before each try, force the number of moves each forced cleanup finished.
type fakeRemoval struct {
	removeErrs []error
	cleaning   []int
	cleanErr   error
	forced     []int
	forceErr   error
	removes    int
	cleanings  int
	forces     int
	said       []string
}

func (f *fakeRemoval) calls() removeCalls {
	return removeCalls{
		remove: func() error {
			f.removes++
			if f.removes <= len(f.removeErrs) {
				return f.removeErrs[f.removes-1]
			}
			return nil
		},
		cleaning: func() (int, error) {
			f.cleanings++
			if f.cleanErr != nil {
				return 0, f.cleanErr
			}
			if f.cleanings <= len(f.cleaning) {
				return f.cleaning[f.cleanings-1], nil
			}
			return f.cleaning[len(f.cleaning)-1], nil
		},
		force: func() (int, error) {
			f.forces++
			if f.forceErr != nil {
				return 0, f.forceErr
			}
			if f.forces <= len(f.forced) {
				return f.forced[f.forces-1], nil
			}
			return 0, nil
		},
		say: func(s string) { f.said = append(f.said, s) },
	}
}

func inUse() error {
	return &pgconn.PgError{Code: "55006", Message: "cannot remove node 3: shard group 2 takes part in bucket move 40"}
}

func isInUse(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "55006"
}

// TestTheRemovalOfANodeWaitsForTheMovesThatAnotherCleanupHolds: the first forced cleanup finishes 23 of the 24 moves of the group
// (another cleanup holds the last one); the removal is refused again, waits, forces the cleanup again and is done. It says in the
// task what it waits for and clears that at the end. Found by the final checks of 0.1.0: the removal tried once more and failed.
func TestTheRemovalOfANodeWaitsForTheMovesThatAnotherCleanupHolds(t *testing.T) {
	f := &fakeRemoval{removeErrs: []error{inUse(), inUse()}, cleaning: []int{24, 1}, forced: []int{23, 1}}
	forced, ran, err := removeWaitingForCleaning(context.Background(), f.calls(), time.Minute, time.Millisecond)
	if err != nil {
		t.Fatalf("the removal failed: %v", err)
	}
	if !ran || forced != 24 {
		t.Errorf("forced %d moves (ran %v), want 24", forced, ran)
	}
	if f.removes != 3 || f.forces != 2 {
		t.Errorf("%d removals and %d forced cleanups, want 3 and 2", f.removes, f.forces)
	}
	if len(f.said) != 2 || !strings.Contains(f.said[0], "1 bucket moves of its group in the cleaning phase") || f.said[1] != "" {
		t.Errorf("the task was told %q", f.said)
	}
}

// TestTheRemovalOfANodeIsNotRetriedForAnotherRefusal: a refusal while no move of the group is cleaning (the group owns buckets, has a
// standby, or only has moves in earlier phases; the read of the moves counts 0 then) is returned at once, and no cleanup is forced; so is
// any other error.
func TestTheRemovalOfANodeIsNotRetriedForAnotherRefusal(t *testing.T) {
	f := &fakeRemoval{removeErrs: []error{inUse()}, cleaning: []int{0}}
	_, ran, err := removeWaitingForCleaning(context.Background(), f.calls(), time.Minute, time.Millisecond)
	if !isInUse(err) || ran || f.forces != 0 || f.removes != 1 {
		t.Errorf("err %v, ran %v, %d forced cleanups, %d removals; want the refusal at once", err, ran, f.forces, f.removes)
	}

	other := &pgconn.PgError{Code: "57P01"}
	f = &fakeRemoval{removeErrs: []error{other}, cleaning: []int{3}}
	if _, _, err := removeWaitingForCleaning(context.Background(), f.calls(), time.Minute, time.Millisecond); !errors.Is(err, other) || f.cleanings != 0 || f.forces != 0 {
		t.Errorf("another error: %v, %d reads, %d cleanups", err, f.cleanings, f.forces)
	}

	f = &fakeRemoval{removeErrs: []error{inUse()}, cleanErr: errors.New("read failed")}
	if _, _, err := removeWaitingForCleaning(context.Background(), f.calls(), time.Minute, time.Millisecond); !isInUse(err) || f.forces != 0 {
		t.Errorf("a failed read of the moves: %v, %d cleanups", err, f.forces)
	}

	f = &fakeRemoval{removeErrs: []error{inUse()}, cleaning: []int{2}, forceErr: errors.New("cleanup failed")}
	if _, ran, err := removeWaitingForCleaning(context.Background(), f.calls(), time.Minute, time.Millisecond); !isInUse(err) || ran || f.removes != 1 {
		t.Errorf("a failed cleanup: %v, ran %v, %d removals", err, ran, f.removes)
	}
}

// TestTheRemovalOfANodeGivesUpWhenTheWaitIsOver: a move that stays in the cleaning phase ends the wait with the refusal.
func TestTheRemovalOfANodeGivesUpWhenTheWaitIsOver(t *testing.T) {
	errs := make([]error, 1000)
	for i := range errs {
		errs[i] = inUse()
	}
	f := &fakeRemoval{removeErrs: errs, cleaning: []int{1}}
	start := time.Now()
	_, ran, err := removeWaitingForCleaning(context.Background(), f.calls(), 50*time.Millisecond, 5*time.Millisecond)
	if !isInUse(err) || !ran {
		t.Fatalf("err %v, ran %v; want the refusal after the wait", err, ran)
	}
	if d := time.Since(start); d < 50*time.Millisecond || d > 5*time.Second {
		t.Errorf("gave up after %v", d)
	}
	if f.removes < 3 || f.removes > 100 {
		t.Errorf("%d tries", f.removes)
	}
	if len(f.said) == 0 || f.said[len(f.said)-1] != "" {
		t.Errorf("the detail was not cleared: %q", f.said)
	}
}

// TestTheRemovalOfANodeStopsWithItsContext: the wait ends when the task is stopped.
func TestTheRemovalOfANodeStopsWithItsContext(t *testing.T) {
	errs := []error{inUse(), inUse(), inUse()}
	f := &fakeRemoval{removeErrs: errs, cleaning: []int{1}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := removeWaitingForCleaning(ctx, f.calls(), time.Minute, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("err %v, want the context's", err)
	}
}

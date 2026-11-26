// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package failover

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func startingUp() error {
	return fmt.Errorf("failed to connect to `user=kingresd database=fo`: %w", &pgconn.PgError{Severity: "FATAL", Code: "57P03", Message: "the database system is shutting down"})
}

func TestCatalogUnavailableClassification(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{startingUp(), true},
		{&pgconn.PgError{Code: "57P01"}, true},
		{&pgconn.PgError{Code: "08006"}, true},
		{&pgconn.PgError{Code: "53300"}, true},
		{&pgconn.PgError{Code: "42P01"}, false},
		{&pgconn.PgError{Code: "42501"}, false},
		{&SQLError{Code: "", Text: "timeout"}, true},
		{&SQLError{Code: "57P03"}, true},
		{&SQLError{Code: "KS009"}, false},
		{fail(CodeBadPromoteTarget, false, "no node"), false},
		{errors.New("dial unix: connect: no such file or directory"), true},
		{nil, false},
	} {
		if got := catalogUnavailable(c.err); got != c.want {
			t.Errorf("%v: got %v want %v", c.err, got, c.want)
		}
	}
}

// The promoted instance of the map group is stopped by the fence of its lease and started again after the promotion: the steps that read the
// catalog wait for it and the task goes on (it ended failed at "Check the catalog" with SQLSTATE 57P03 before).
func TestACatalogThatIsStartingAfterThePromotionIsWaitedFor(t *testing.T) {
	w := newWorld()
	w.readErrs = []error{startingUp(), startingUp(), startingUp(), startingUp(), startingUp(), startingUp(), startingUp(), startingUp()}
	r := w.runner()
	w.fakeClock(r)
	if name, err := runAll(t, r, hardInput()); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if w.readCalls < 9 {
		t.Errorf("the catalog was read %d times", w.readCalls)
	}
}

// A catalog that stays unreadable pauses the step (nothing is failed after the promotion), and a failure that the server answered on purpose pauses
// at once without a wait.
func TestACatalogThatStaysUnreadableAfterThePromotionPauses(t *testing.T) {
	w := newWorld()
	w.readForever = startingUp()
	r := w.runner()
	r.CatalogReadWait = 20 * r.LeasePoll
	w.fakeClock(r)
	name, err := runAll(t, r, hardInput())
	var pe *PauseError
	if !errors.As(err, &pe) {
		t.Fatalf("%s: %v (a failed task is not what the operator meets after the promotion)", name, err)
	}
	if w.readCalls < 10 {
		t.Errorf("the catalog was read %d times, the wait did not happen", w.readCalls)
	}
}

func TestACatalogErrorThatTheServerAnsweredDoesNotWait(t *testing.T) {
	w := newWorld()
	w.readForever = &pgconn.PgError{Code: "42501", Message: "permission denied"}
	r := w.runner()
	w.fakeClock(r)
	_, err := runAll(t, r, hardInput())
	var pe *PauseError
	if !errors.As(err, &pe) {
		t.Fatalf("got %v", err)
	}
	if w.readCalls > 3 {
		t.Errorf("the catalog was read %d times for an error that does not end by itself", w.readCalls)
	}
}

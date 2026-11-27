// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package failover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
	"github.com/jackc/pgx/v5/pgconn"
)

func startingUp() error {
	return fmt.Errorf("failed to connect to `user=kingresd database=fo`: %w", &pgconn.PgError{Severity: "FATAL", Code: "57P03", Message: "the database system is shutting down"})
}

func errs(n int) []error {
	out := make([]error, n)
	for i := range out {
		out[i] = startingUp()
	}
	return out
}

func TestCatalogUnavailableClassification(t *testing.T) {
	dial := &net.OpError{Op: "dial", Net: "unix", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ENOENT}}
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"starting up", startingUp(), true},
		{"admin shutdown", &pgconn.PgError{Code: "57P01"}, true},
		{"crash shutdown", &pgconn.PgError{Code: "57P02"}, true},
		{"connection failure class", &pgconn.PgError{Code: "08006"}, true},
		{"too many connections", &pgconn.PgError{Code: "53300"}, true},
		{"undefined table", &pgconn.PgError{Code: "42P01"}, false},
		{"permission denied", &pgconn.PgError{Code: "42501"}, false},
		{"query canceled", &pgconn.PgError{Code: "57014"}, false},
		{"internal error", &pgconn.PgError{Code: "XX000"}, false},
		{"call that did not finish", &SQLError{Code: "", Text: "timeout"}, true},
		{"sql error 57P03", &SQLError{Code: "57P03"}, true},
		{"sql error KS009", &SQLError{Code: "KS009"}, false},
		{"coded error", fail(CodeBadPromoteTarget, false, "no node"), false},
		{"socket gone", fmt.Errorf("failed to connect: %w", dial), true},
		{"connection refused", fmt.Errorf("dial: %w", syscall.ECONNREFUSED), true},
		{"connection reset", fmt.Errorf("read: %w", syscall.ECONNRESET), true},
		{"eof", fmt.Errorf("read: %w", io.EOF), true},
		{"unexpected eof", fmt.Errorf("read: %w", io.ErrUnexpectedEOF), true},
		{"deadline", fmt.Errorf("call: %w", context.DeadlineExceeded), true},
		{"instance management disabled", errors.New("instance management is not configured (pg-bin-dir)"), false},
		{"no service credentials", errors.New("cluster: no service credentials"), false},
		{"untrusted schema refusal", errors.New("the schema kingres of the database is not the extension's"), false},
		{"scan error", errors.New("can't scan into dest[0]: cannot scan NULL into *int32"), false},
		{"nil", nil, false},
	} {
		if got := catalogUnavailable(c.err); got != c.want {
			t.Errorf("%s: %v: got %v want %v", c.name, c.err, got, c.want)
		}
	}
}

// stepCase is a step after the promotion that reads a catalog: the step, and the number of reads of the step that answer before the fault begins (the reads that
// belong to an earlier part of the step).
type stepCase struct {
	name string
	step string
	skip int
}

var catalogSteps = []stepCase{
	{"catalogFollows", StepCatalog, 0},
	{"checkCatalog targets", StepCheckCat, 0},
	// the check reads the nodes of the system database twice for its targets (the system catalog and the one of the database); the loop reads after them
	{"checkCatalog loop", StepCheckCat, 2},
	{"repoint", StepRepoint, 0},
	{"records", StepRecords, 0},
}

// The promoted instance of the map group is stopped by the fence of its lease and started again after the promotion: each step that reads the catalog waits
// for it and the task goes on (it ended failed at "Check the catalog" with SQLSTATE 57P03 before).
func TestEachStepAfterThePromotionWaitsForACatalogThatIsStarting(t *testing.T) {
	for _, c := range catalogSteps {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld()
			w.readStep, w.readSkip, w.readErrs = c.step, c.skip, errs(8)
			r := w.runner()
			w.fakeClock(r)
			if name, err := runAll(t, r, hardInput()); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if w.readCalls < 9 {
				t.Errorf("the faulted step read the catalog %d times", w.readCalls)
			}
		})
	}
}

// A catalog that stays unreadable pauses the step that reads it, with a message that names the step and the read (nothing is failed after the promotion).
func TestEachStepAfterThePromotionPausesWhenTheCatalogStaysUnreadable(t *testing.T) {
	for _, c := range catalogSteps {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld()
			w.readStep, w.readSkip, w.readForever = c.step, c.skip, startingUp()
			r := w.runner()
			r.CatalogReadWait = 20 * r.LeasePoll
			w.fakeClock(r)
			name, err := runAll(t, r, hardInput())
			var pe *PauseError
			if !errors.As(err, &pe) || name != StepName(1, c.step) {
				t.Fatalf("%s: %v (a failed task is not what the operator meets after the promotion)", name, err)
			}
			if !strings.Contains(pe.Reason, `"`+c.step+`"`) || !strings.Contains(pe.Reason, "cannot read") || strings.Contains(pe.Reason, "moved to the promoted standby") {
				t.Errorf("the pause does not name the step and the read: %s", pe.Reason)
			}
			if w.readCalls < 10 {
				t.Errorf("the catalog was read %d times, the wait did not happen", w.readCalls)
			}
		})
	}
}

// An error that does not end by waiting (a refused privilege, a service without credentials) pauses at once without a wait.
func TestACatalogErrorThatDoesNotEndByWaitingPausesAtOnce(t *testing.T) {
	for _, e := range []error{&pgconn.PgError{Code: "42501", Message: "permission denied"}, errors.New("cluster: no service credentials")} {
		w := newWorld()
		w.readStep, w.readForever = StepRepoint, e
		r := w.runner()
		w.fakeClock(r)
		name, err := runAll(t, r, hardInput())
		var pe *PauseError
		if !errors.As(err, &pe) || name != StepName(1, StepRepoint) {
			t.Fatalf("%s: %v", name, err)
		}
		if w.readCalls != 1 {
			t.Errorf("%v: the catalog was read %d times for an error that does not end by itself", e, w.readCalls)
		}
	}
}

// The wait of a step is one for the whole step: the check of the catalog reads it for its targets and again for each database, and the reads do not get a
// full wait each.
func TestTheWaitForTheCatalogIsOneForTheWholeStep(t *testing.T) {
	w := newWorld()
	// 15 unavailable reads end the first read of the check (its targets: two reads that answer), 15 more would end the read of its loop: 30 polls of 10 ms against a wait
	// of 20 polls. A nil entry of readErrs is a read that answers.
	w.readStep = StepCheckCat
	w.readErrs = append(append(errs(15), nil, nil), errs(15)...)
	r := w.runner()
	r.CatalogReadWait = 20 * r.LeasePoll
	w.fakeClock(r)
	name, err := runAll(t, r, hardInput())
	var pe *PauseError
	if !errors.As(err, &pe) || name != StepName(1, StepCheckCat) {
		t.Fatalf("%s: %v (the two reads of the step each got a full wait)", name, err)
	}
	if w.readCalls > 25 {
		t.Errorf("the step read the catalog %d times, more than one wait allows", w.readCalls)
	}
}

// runResuming runs the steps in order as the engine does; a pause ends the run of the step, the fault is lifted (the catalog answers again) and the same
// step runs again, as the repeated request resumes it. It returns the name of the step that paused ("" when none did).
func runResuming(t *testing.T, w *world, r *Runner, in Input, lift func()) (paused string) {
	t.Helper()
	raw, _ := json.Marshal(in)
	steps, err := r.steps(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		for try := 0; ; try++ {
			env := &task.Env{Task: task.Task{ID: 1, Input: raw}}
			err := s.Run(context.Background(), env)
			var sk *task.SkipError
			if errors.As(err, &sk) || err == nil {
				if try > 0 && w.alertOn["failoverBlocked|"+subjectOf(1, in.ServerID)] {
					t.Errorf("the alert failoverBlocked of the group is still raised when the resumed step %s ended", s.Name)
				}
				break
			}
			var pe *PauseError
			if !errors.As(err, &pe) || try > 0 {
				t.Fatalf("%s: %v", s.Name, err)
			}
			if paused != "" {
				t.Fatalf("%s paused after %s", s.Name, paused)
			}
			paused = s.Name
			if !w.alertOn["failoverBlocked|"+subjectOf(1, in.ServerID)] {
				t.Fatalf("%s paused without the alert failoverBlocked of the group", s.Name)
			}
			lift()
		}
	}
	return paused
}

// A step after the promotion that paused because the catalog could not be read ends its alert when the repeated request runs it to the end: the alert is the
// one of the subject of the group, the same that "Make the catalog follow" ends, and nothing else ends it.
func TestAResumedStepAfterThePromotionEndsTheAlertOfItsPause(t *testing.T) {
	for _, c := range []stepCase{catalogSteps[2], catalogSteps[3], catalogSteps[4]} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld()
			w.readStep, w.readSkip, w.readForever = c.step, c.skip, startingUp()
			r := w.runner()
			r.CatalogReadWait = 5 * r.LeasePoll
			w.fakeClock(r)
			paused := runResuming(t, w, r, hardInput(), func() {
				w.mu.Lock()
				w.readForever = nil
				w.mu.Unlock()
			})
			if paused != StepName(1, c.step) {
				t.Fatalf("the task paused at %q, want %q", paused, StepName(1, c.step))
			}
			if n := w.stepRuns[StepName(1, c.step)]; n != 2 {
				t.Errorf("the step ran %d times, want 2 (the pause and the repeated request)", n)
			}
			if w.alertOn["failoverBlocked|"+subjectOf(1, "s1")] {
				t.Errorf("the alert failoverBlocked of the group is still raised after the step ended: %v", w.alertOn)
			}
		})
	}
}

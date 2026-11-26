// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// A server that answers with an error that is not "unreachable" is tried again until
// the change is applied: a server whose set of primaries has just changed refuses
// for a moment and then accepts (the replication to a promoted primary).
func TestOnServerRetriesARefusalUntilItIsApplied(t *testing.T) {
	oldPause := otherErrorPause
	otherErrorPause = time.Millisecond
	defer func() { otherErrorPause = oldPause }()
	o := &Ops{Log: slog.New(slog.DiscardHandler)}
	calls := 0
	err := o.onServer(context.Background(), &task.Env{}, "s2", "bring up to date", func() error {
		calls++
		switch calls {
		case 1:
			return fmt.Errorf("the role could not be created: %w", errors.New("instance is in recovery"))
		case 2:
			return fmt.Errorf("wrapped: %w", peerclient.ErrUnreachable)
		case 3:
			return errors.New("connection refused by the instance")
		}
		return nil
	})
	if err != nil || calls != 4 {
		t.Fatalf("err %v after %d calls", err, calls)
	}
}

// After the budget the error is returned, with the server named: never an endless
// silent loop.
func TestOnServerGivesTheLastErrorAfterTheBudget(t *testing.T) {
	oldPause, oldBudget := otherErrorPause, otherErrorBudget
	otherErrorPause, otherErrorBudget = time.Millisecond, 20*time.Millisecond
	defer func() { otherErrorPause, otherErrorBudget = oldPause, oldBudget }()
	o := &Ops{Log: slog.New(slog.DiscardHandler)}
	err := o.onServer(context.Background(), &task.Env{}, "s2", "bring up to date", func() error { return errors.New("always refused") })
	if err == nil || err.Error() != "on server s2: always refused" {
		t.Fatalf("got %v", err)
	}
}

// An error that waiting does not change fails at once, with the server named: a role that cannot exist, a
// bad request, a refusal of the callee. Errors that heal are retried (the instance is in recovery, starting,
// the connection was lost).
func TestOnServerFailsAtOnceOnAnErrorThatCannotHeal(t *testing.T) {
	oldPause := otherErrorPause
	otherErrorPause = time.Millisecond
	defer func() { otherErrorPause = oldPause }()
	o := &Ops{Log: slog.New(slog.DiscardHandler)}
	apply := func(msg string) error {
		return &peerclient.APIError{Status: 422, Code: "apply_failed", Message: msg}
	}
	for _, tc := range []struct {
		name    string
		err     error
		healing bool
	}{
		{"syntax error", apply("SQLSTATE 42601: syntax error"), false},
		{"a role that exists", apply("SQLSTATE 42710: role already exists"), false},
		{"a value that is not valid", apply("SQLSTATE 22023: invalid parameter"), false},
		{"the contract refuses", &peerclient.APIError{Status: 400, Code: "bad_request", Message: "x"}, false},
		{"a lock", apply("SQLSTATE 55P03: lock not available"), true},
		{"the instance is in recovery", apply("SQLSTATE 25006: cannot execute in a read-only transaction"), true},
		{"the instance is starting", apply("SQLSTATE 57P03: the database system is starting up"), true},
		{"the database is about to arrive", apply("SQLSTATE 3D000: database does not exist"), true},
		{"a lost connection", apply("the statement failed"), true},
		{"the server is not ready", &peerclient.APIError{Status: 503, Code: "service_unavailable", Message: "x"}, true},
		{"not the leader", &peerclient.APIError{Status: 403, Code: "not_leader", Message: "x"}, true},
	} {
		calls := 0
		err := o.onServer(context.Background(), &task.Env{}, "s2", "apply", func() error {
			calls++
			if calls == 1 {
				return tc.err
			}
			return nil
		})
		if tc.healing && (err != nil || calls != 2) {
			t.Errorf("%s: err %v after %d calls, want a second try that succeeds", tc.name, err, calls)
		}
		if !tc.healing && (err == nil || calls != 1) {
			t.Errorf("%s: err %v after %d calls, want a failure at once", tc.name, err, calls)
		}
	}
}

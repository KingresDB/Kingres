// SPDX-License-Identifier: AGPL-3.0-only

package ledgerpub

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// A deadlock between two writers of the cluster row is a try again, never a conflict: the collector stops a log on ErrConflict, and a log must not
// be stopped because a key task committed at the same moment.
func TestADeadlockIsBusyAndNeverAConflict(t *testing.T) {
	for code, want := range map[string]error{"40P01": ErrBusy, "40001": ErrBusy, "23505": ErrConflict, "KC001": ErrConflict} {
		err := conflictCode(&pgconn.PgError{Code: code})
		if !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", code, err, want)
		}
		if want == ErrBusy && errors.Is(err, ErrConflict) {
			t.Errorf("%s is a conflict", code)
		}
	}
	other := errors.New("connection lost")
	if got := conflictCode(other); got != other {
		t.Errorf("another error was changed: %v", got)
	}
	if conflictCode(nil) != nil {
		t.Error("nil")
	}
}

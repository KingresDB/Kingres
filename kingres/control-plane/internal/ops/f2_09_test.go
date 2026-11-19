// SPDX-License-Identifier: AGPL-3.0-only

package ops

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// unwritable is a recorder whose leader cannot be reached.
type unwritable struct{}

func (unwritable) Record(context.Context, task.Task) (task.Task, error) {
	return task.Task{}, errors.New("the leader cannot be reached")
}
func (unwritable) Settle(context.Context, task.Task) error {
	return errors.New("the leader cannot be reached")
}
func (unwritable) Reserve(context.Context) int64 { return 0 }

// F2-09, the other direction: a stop or restart of an enforced primary does not run when its record cannot be
// written (the record is where a failover excludes it). A start, and any operation on a group that is not enforced,
// is kept in memory as before.
func TestAStopOfAnEnforcedPrimaryDoesNotRunOnARecordThatCannotBeWritten(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		kind     string
		enforced bool
		refused  bool
	}{
		{KindInstanceStop, true, true},
		{KindInstanceRestart, true, true},
		{KindInstanceStart, true, false},
		{KindInstanceStop, false, false},
		{KindInstanceRestart, false, false},
	} {
		enforced := tc.enforced
		o := &Ops{Log: slog.New(slog.DiscardHandler), EnforcedPrimary: func() bool { return enforced }}
		_, recorded, err := o.openTask(ctx, unwritable{}, newInstanceTask(tc.kind, stopStep, "3", "alice"))
		var p *problem.Problem
		switch {
		case tc.refused && (!errors.As(err, &p) || p.Code != problem.ServiceUnavailable):
			t.Errorf("%s enforced=%v: got %v, want a refusal (service_unavailable)", tc.kind, tc.enforced, err)
		case !tc.refused && err != nil:
			t.Errorf("%s enforced=%v: refused: %v", tc.kind, tc.enforced, err)
		case !tc.refused && recorded:
			t.Errorf("%s enforced=%v: said recorded", tc.kind, tc.enforced)
		}
	}
}

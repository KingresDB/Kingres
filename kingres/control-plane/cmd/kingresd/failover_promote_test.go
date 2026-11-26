// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package main

import (
	"errors"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
)

// F3-05: a standby whose role file already says primary, and that is still in recovery, can be promoted by a later call.
func TestPromoteActionFinishesAnUnfinishedPromotion(t *testing.T) {
	standby := instance.Role{Group: 1, Role: instance.RoleStandby}
	primary := instance.Role{Group: 1, Role: instance.RolePrimary}
	unknown := instance.Role{Group: 1, Role: instance.RoleUnknown}
	for _, c := range []struct {
		name       string
		inRecovery bool
		role       instance.Role
		want       string
	}{
		{"a standby", true, standby, promoteDo},
		{"the role file written, pg_promote not answered", true, primary, promoteDo},
		{"promoted", false, primary, promoteAlready},
		{"out of recovery as a standby", false, standby, promoteRefuse},
		{"an instance of no role in recovery", true, unknown, promoteRefuse},
		{"an instance of no role, writable", false, unknown, promoteRefuse},
	} {
		if got := promoteAction(c.inRecovery, c.role); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

type fakeAttacher struct{ err error }

func (f fakeAttacher) Attach(int32) error { return f.err }

// F3-08: a lease that was lost between the check and the attach is not an answer of success.
func TestAttachPromotedAnswersLeaseNotHeldWhenTheLeaseWasLost(t *testing.T) {
	if err := attachPromoted(fakeAttacher{}, 1); err != nil {
		t.Fatalf("a held lease: %v", err)
	}
	err := attachPromoted(fakeAttacher{errors.New("lease: the lease of shard group 1 is not held")}, 1)
	var ce *internalapi.CallError
	if !errors.As(err, &ce) || ce.Code != "lease_not_held" || ce.Status != 409 {
		t.Fatalf("got %v", err)
	}
}

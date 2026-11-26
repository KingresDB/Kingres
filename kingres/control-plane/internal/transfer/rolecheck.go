// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package transfer

import (
	"context"
	"errors"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
)

// RoleChecker re-checks the role of a transfer at the start of every step (DXR-23, F23). The map of connectable roles keeps a role
// connectable until kingresd restarts, so a transfer of many hours would run on with a role that signed out, was demoted from
// kingres_admin, changed its password, expired, or was dropped and made again under the same name. A step that fails the check ends the task
// with the code forbidden and stores nothing.
type RoleChecker struct {
	// Current gives the cluster roles of the role now: auth.ErrInvalidCredentials when it does not exist, cannot log in or has expired
	// (auth.Authenticator.CurrentRoles).
	Current func(ctx context.Context, name string) ([]string, error)
	// OID gives the OID of the role of that name; found is false when there is none.
	OID func(ctx context.Context, name string) (oid int64, found bool, err error)
}

// Check returns nil when the role of the record is still the same role, may still sign in and still holds kingres_admin. Otherwise it
// returns an *Error with the code forbidden (the role lost its right), or a retryable one when the check could not be made.
func (c RoleChecker) Check(ctx context.Context, id Identity) error {
	roles, err := c.Current(ctx, id.Name)
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		return Errorf(CodeForbidden, false, "the role can no longer sign in")
	case err != nil:
		return Errorf(CodeSessionLost, true, "the role cannot be checked right now")
	}
	if !auth.Allows(roles, "kingres_admin") {
		return Errorf(CodeForbidden, false, "the role is no longer an administrator")
	}
	oid, found, err := c.OID(ctx, id.Name)
	switch {
	case err != nil:
		return Errorf(CodeSessionLost, true, "the role cannot be checked right now")
	case !found || oid != id.OID:
		return Errorf(CodeForbidden, false, "the role was replaced by another role of the same name")
	}
	return nil
}

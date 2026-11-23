// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

// Package extgate puts the answer of extguard in the words of the operations of the control plane. The service role is a superuser; before it reads or calls
// anything in the schema kingres of a user database, the caller asks extgate (Require, Usable or RefuseUntrusted) and gets either permission, the old
// "not installed" behaviour, or a refusal that is a *problem.Problem (code extension_unavailable) which names the database and the check that failed.
//
// The refusal never names the object that does not belong: its name is chosen by the owner of the database.
package extgate

import (
	"context"
	"fmt"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extguard"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

// Problem is the refusal for a schema that cannot be trusted. db may be empty.
func Problem(u *extguard.UntrustedError, db string) *problem.Problem {
	where := "this database"
	if db != "" {
		where = fmt.Sprintf("database %q", db)
	}
	return problem.New(problem.ExtensionUnavailable, fmt.Sprintf("The schema %s of %s in %s cannot be trusted (%s), so nothing in it is read or called. "+
		"A superuser has to remove what does not belong to the extension from the schema, or drop and create the extension again.", u.Schema, u.Extension, where, u.Reason))
}

// Verify is extguard.Verify with the refusal as a problem: Info.Present is false for an absent extension (no error), the error is a problem for an untrusted one,
// and the error of the query comes back as it is.
func Verify(ctx context.Context, q extguard.Querier, ext, db string) (extguard.Info, error) {
	info, err := extguard.Verify(ctx, q, ext)
	if u, ok := extguard.IsUntrusted(err); ok {
		return info, Problem(u, db)
	}
	return info, err
}

// Usable tells whether ext is installed with a trusted schema. An untrusted schema is (false, the refusal); an absent extension is (false, nil).
func Usable(ctx context.Context, q extguard.Querier, ext, db string) (bool, error) {
	info, err := Verify(ctx, q, ext, db)
	return err == nil && info.Present, err
}

// Require is the gate of an operation that cannot do without ext: absent is problem extension_missing, untrusted is the refusal.
func Require(ctx context.Context, q extguard.Querier, ext, db string) error {
	ok, err := Usable(ctx, q, ext, db)
	if err != nil {
		return err
	}
	if !ok {
		return problem.New(problem.ExtensionMissing, fmt.Sprintf("%s is not installed in %s.", ext, dbText(db)))
	}
	return nil
}

// RefuseUntrusted is the gate of an operation that works without the extensions but must not read their schema when it cannot be trusted: only an untrusted
// schema is an error, absent is fine.
func RefuseUntrusted(ctx context.Context, q extguard.Querier, db string, exts ...string) error {
	rs, err := extguard.VerifyAll(ctx, q, exts...)
	if err != nil {
		return err
	}
	for _, r := range rs {
		if u, ok := extguard.IsUntrusted(r.Err); ok {
			return Problem(u, db)
		}
	}
	return nil
}

func dbText(db string) string {
	if db == "" {
		return "this database"
	}
	return fmt.Sprintf("database %q", db)
}

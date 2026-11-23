// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"errors"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extguard"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

// untrustedProblem turns the *extguard.UntrustedError of a check into the refusal that the caller returns; any other error is returned as it is.
func untrustedProblem(err error) error {
	if u, ok := extguard.IsUntrusted(err); ok {
		return problem.New(problem.Conflict, "The schema "+u.Schema+" of the extension "+u.Extension+" in this database cannot be trusted ("+string(u.Reason)+
			"): it is not owned by a superuser, a role that is not a superuser can create objects in it, or it holds an object that is not a member of an extension. Nothing in it is read or called; an operator must restore the schema (see the alert extensionSchemaUntrusted).")
	}
	return err
}

// extPresent is extguard.Verify for the code that can do without the extension: it reports whether the extension is installed and trusted. An extension whose
// schema cannot be trusted is the refusal of untrustedProblem, never "absent", so that nothing of its schema is read or called after it.
func extPresent(ctx context.Context, x execer, ext string) (extguard.Info, error) {
	info, err := extguard.Verify(ctx, x, ext)
	if err != nil {
		return info, untrustedProblem(err)
	}
	return info, nil
}

// needExt is extPresent for the code that must have the extension: an absent one is a conflict that says so.
func needExt(ctx context.Context, x execer, ext, what string) (extguard.Info, error) {
	info, err := extPresent(ctx, x, ext)
	if err != nil {
		return info, err
	}
	if !info.Present {
		return info, problem.New(problem.Conflict, what)
	}
	return info, nil
}

// errExtAbsent is for the code that maps the absence of an extension itself.
var errExtAbsent = errors.New("the extension is not installed")

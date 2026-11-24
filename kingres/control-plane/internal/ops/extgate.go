// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extgate"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extguard"
)

// The service role is a superuser, and it reads kingres.* in databases whose owners it does not trust. Every read and call below the schema kingres on a service
// session in a user database is preceded by extguard (see its package comment, and internal/extgate for the refusal): the extension must be installed and nothing
// but members of extensions owned by a superuser may be in its schema. The helpers here give the operations of this package the three answers they need.

// verifyExt is extguard.Verify with the refusal as a problem: Info.Present false for an absent extension (no error), a problem for an untrusted one.
func verifyExt(ctx context.Context, q extguard.Querier, ext string) (extguard.Info, error) {
	return extgate.Verify(ctx, q, ext, "")
}

// extUsable tells whether ext is installed with a trusted schema. An untrusted schema is (false, the refusal).
func extUsable(ctx context.Context, q extguard.Querier, ext string) (bool, error) {
	return extgate.Usable(ctx, q, ext, "")
}

// requireExt is the gate of an operation that cannot do without ext: absent is ExtensionMissing, untrusted is the refusal.
func requireExt(ctx context.Context, q extguard.Querier, ext string) error {
	return extgate.Require(ctx, q, ext, "")
}

// refuseUntrusted is the gate of an operation that works without ext but must not read its schema when it cannot be trusted: only an untrusted schema is an error.
func refuseUntrusted(ctx context.Context, q extguard.Querier, exts ...string) error {
	return extgate.RefuseUntrusted(ctx, q, "", exts...)
}

// asServiceExt is asService that first requires the extensions: the session of the service role reads or calls their schema.
func (o *Ops) asServiceExt(ctx context.Context, db string, need []string, f func(tx pgx.Tx) error) error {
	return o.asService(ctx, db, func(tx pgx.Tx) error {
		for _, e := range need {
			if err := extgate.Require(ctx, tx, e, db); err != nil {
				return err
			}
		}
		return f(tx)
	})
}

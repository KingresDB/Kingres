// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// The owners of the tables of the analytics operations (review P12C-04, P12D-04).
//
// The conversion of a table and the runs of a policy drive the DDL of kingres_timepart in the session of the service role with the setting trust_table_owners on: the
// code of the owner of the table (an index expression, a check, a default) runs as that owner inside that session, and what it leaves behind would run later with the
// rights of the service role. A scheduled policy repeats this without a person in the loop. So the operations are open for a table of a superuser, and for the table
// of any other role only when the administrator allows it with the cluster setting analytics.allowNonSuperuserOwners (default off).

// settingNonSuperuserOwners is the key of the cluster setting in kingres_cp.cluster_setting.
const settingNonSuperuserOwners = "analytics.allowNonSuperuserOwners"

// ownerInfo is the owner of a table.
type ownerInfo struct {
	Name  string
	Super bool
}

// tableOwnerOf reads the owner of a table or partition of the database the transaction is in.
func tableOwnerOf(ctx context.Context, tx pgx.Tx, schema, name string) (ownerInfo, error) {
	var o ownerInfo
	err := tx.QueryRow(ctx, `SELECT r.rolname::text, r.rolsuper FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace JOIN pg_roles r ON r.oid = c.relowner
		WHERE n.nspname = $1 AND c.relname = $2`, schema, name).Scan(&o.Name, &o.Super)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, notOnThisInstance(schema, name)
	}
	return o, err
}

// nonSuperuserOwnersAllowed reads the cluster setting; a system database without the table or the key means off.
func (o *Ops) nonSuperuserOwnersAllowed(ctx context.Context) (bool, error) {
	if o.Service == nil {
		return false, nil
	}
	var allowed bool
	err := o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
		var have bool
		if err := tx.QueryRow(ctx, `SELECT to_regclass('kingres_cp.cluster_setting') IS NOT NULL`).Scan(&have); err != nil || !have {
			return err
		}
		var v *bool
		err := tx.QueryRow(ctx, `SELECT (value #>> '{}')::boolean FROM kingres_cp.cluster_setting WHERE key = $1`, settingNonSuperuserOwners).Scan(&v)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err == nil && v != nil {
			allowed = *v
		}
		return err
	})
	return allowed, wrap(err)
}

// untrustedOwner returns the first owner that the analytics operations are not open for now, "" when all are.
func untrustedOwner(allowed bool, owners ...ownerInfo) string {
	if allowed {
		return ""
	}
	for _, w := range owners {
		if !w.Super {
			return w.Name
		}
	}
	return ""
}

// ownerNotTrustedText is the reason that the owner of a table is refused.
func ownerNotTrustedText(owner string) string {
	return fmt.Sprintf("the table is owned by role %q, which is not a superuser: the code of its owner would run in the session of the service role while the table is converted or maintained; "+
		"turn on the cluster setting analytics.allowNonSuperuserOwners to allow it", owner)
}

// fillOwner sets TableOwner and OwnerTrusted of a policy.
func (o *Ops) fillOwner(ctx context.Context, tx pgx.Tx, p *api.PartitionPolicy, schema, table string) error {
	w, err := tableOwnerOf(ctx, tx, schema, table)
	if err != nil {
		return err
	}
	allowed, err := o.nonSuperuserOwnersAllowed(ctx)
	if err != nil {
		return err
	}
	trusted := untrustedOwner(allowed, w) == ""
	p.TableOwner, p.OwnerTrusted = w.Name, &trusted
	return nil
}

// ownerRefusal is the problem of a policy or a run for a table whose owner is not trusted.
func ownerRefusal(owner string) error {
	return problem.New(problem.PartitionPolicyConflict, "The policy cannot run: "+ownerNotTrustedText(owner)+".")
}

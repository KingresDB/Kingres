// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package extguard

import (
	"context"
	"fmt"
)

// SystemProblem is one way in which the system database of the control plane is not what it must be.
type SystemProblem struct {
	// Object is "database" or the schema (kingres, kingres_cp, public).
	Object string
	Reason Reason
}

// The reasons of SystemProblems.
const (
	ReasonDatabaseOwner  Reason = "databaseOwner"
	ReasonDatabaseCreate Reason = "databaseCreate"
)

func (p SystemProblem) String() string { return fmt.Sprintf("%s: %s", p.Object, p.Reason) }

// systemSQL looks at the database it is connected to (the system database): its owner, who may create schemas in it, and, for each of the schemas kingres,
// kingres_cp and public that exist, the owner and who may create objects in it. A superuser owner and a grant to a superuser are fine, and so is the grant to
// pg_database_owner that PostgreSQL gives the schema public (the owner of the database is checked on its own).
const systemSQL = `
SELECT 'database', 'databaseOwner'
  FROM pg_catalog.pg_database d
 WHERE d.datname = pg_catalog.current_database()
   AND NOT COALESCE((SELECT r.rolsuper FROM pg_catalog.pg_roles r WHERE r.oid = d.datdba), false)
UNION ALL
SELECT 'database', 'databaseCreate'
  FROM pg_catalog.pg_database d
 WHERE d.datname = pg_catalog.current_database()
   AND EXISTS (SELECT 1 FROM pg_catalog.aclexplode(COALESCE(d.datacl, pg_catalog.acldefault('d', d.datdba))) a
                WHERE a.privilege_type = 'CREATE' AND a.grantee <> 6171
                  AND (a.grantee = 0 OR NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles g WHERE g.oid = a.grantee AND g.rolsuper)))
UNION ALL
SELECT n.nspname::pg_catalog.text, 'schemaOwner'
  FROM pg_catalog.pg_namespace n
 WHERE n.nspname IN ('kingres', 'kingres_cp', 'public')
   AND NOT COALESCE((SELECT r.rolsuper FROM pg_catalog.pg_roles r WHERE r.oid = n.nspowner), false)
UNION ALL
SELECT n.nspname::pg_catalog.text, 'schemaCreate'
  FROM pg_catalog.pg_namespace n
 WHERE n.nspname IN ('kingres', 'kingres_cp', 'public')
   AND EXISTS (SELECT 1 FROM pg_catalog.aclexplode(COALESCE(n.nspacl, pg_catalog.acldefault('n', n.nspowner))) a
                WHERE a.privilege_type = 'CREATE' AND a.grantee <> 6171
                  AND (a.grantee = 0 OR NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles g WHERE g.oid = a.grantee AND g.rolsuper)))
 ORDER BY 1, 2`

// SystemDatabaseProblems checks the system database through q, which must be a connection to it. An empty list is a database that can be trusted: a superuser
// owns it and its schemas kingres, kingres_cp and public, and no role that is not a superuser (PUBLIC included) can create schemas or objects there.
func SystemDatabaseProblems(ctx context.Context, q Querier) ([]SystemProblem, error) {
	rows, err := q.Query(ctx, systemSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SystemProblem
	for rows.Next() {
		var p SystemProblem
		if err := rows.Scan(&p.Object, &p.Reason); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

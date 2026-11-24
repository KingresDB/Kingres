// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package rebalance

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// trustStub makes the stand-in of the extension's catalog that a test wrote in the schema kingres what the control plane requires of a real one: the extension
// kingres_shard is recorded, owned by the superuser that made it, and every table, view and function of the schema is a member of it, as CREATE EXTENSION would
// have made them. The gate (extgate, extguard) is not weakened: it reads the same catalog it reads on a real database, and only this fixture says that the stub is the
// extension's. Call it after the last object of the stub was made; a test that makes more objects in the schema afterwards calls it again.
func trustStub(t testing.TB, c *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO pg_catalog.pg_extension (oid, extname, extowner, extnamespace, extrelocatable, extversion)
		 SELECT pg_catalog.pg_nextoid('pg_catalog.pg_extension'::pg_catalog.regclass, 'oid', 'pg_catalog.pg_extension_oid_index'::pg_catalog.regclass),
		        'kingres_shard', (SELECT r.oid FROM pg_catalog.pg_roles r WHERE r.rolname = current_user), 'kingres'::pg_catalog.regnamespace, false, '0.9'
		  WHERE NOT EXISTS (SELECT 1 FROM pg_catalog.pg_extension WHERE extname = 'kingres_shard')`,
		`INSERT INTO pg_catalog.pg_depend (classid, objid, objsubid, refclassid, refobjid, refobjsubid, deptype)
		 SELECT 'pg_catalog.pg_class'::pg_catalog.regclass, c.oid, 0, 'pg_catalog.pg_extension'::pg_catalog.regclass, e.oid, 0, 'e'
		   FROM pg_catalog.pg_class c, pg_catalog.pg_extension e
		  WHERE e.extname = 'kingres_shard' AND c.relnamespace = 'kingres'::pg_catalog.regnamespace AND c.relkind IN ('r', 'v', 'm', 'p', 'f')
		    AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend d WHERE d.classid = 'pg_catalog.pg_class'::pg_catalog.regclass AND d.objid = c.oid AND d.deptype = 'e')`,
		`INSERT INTO pg_catalog.pg_depend (classid, objid, objsubid, refclassid, refobjid, refobjsubid, deptype)
		 SELECT 'pg_catalog.pg_proc'::pg_catalog.regclass, p.oid, 0, 'pg_catalog.pg_extension'::pg_catalog.regclass, e.oid, 0, 'e'
		   FROM pg_catalog.pg_proc p, pg_catalog.pg_extension e
		  WHERE e.extname = 'kingres_shard' AND p.pronamespace = 'kingres'::pg_catalog.regnamespace
		    AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend d WHERE d.classid = 'pg_catalog.pg_proc'::pg_catalog.regclass AND d.objid = p.oid AND d.deptype = 'e')`,
	} {
		if _, err := c.Exec(ctx, q); err != nil {
			t.Fatalf("recording the stub as the extension: %v", err)
		}
	}
}

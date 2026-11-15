// SPDX-License-Identifier: AGPL-3.0-only

package pgstore

import (
	"context"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
)

// roleSQL lists roles with their direct ordinary memberships and the cluster
// roles they belong to, directly or through other roles. The four built-in
// cluster roles and the predefined pg_* roles are not listed as accounts.
const roleSQL = `
SELECT r.rolname, r.rolcanlogin, r.rolsuper, r.rolcreatedb, r.rolcreaterole, r.rolconnlimit, NULLIF(r.rolvaliduntil, 'infinity'::timestamptz),
  coalesce((SELECT array_agg(g.rolname ORDER BY g.rolname) FROM pg_auth_members m
            JOIN pg_roles g ON g.oid = m.roleid
            WHERE m.member = r.oid AND m.inherit_option
              AND g.rolname NOT IN ('kingres_admin', 'kingres_ops', 'kingres_readonly', 'kingres_stats')),
           '{}'::name[])::text[],
  coalesce((SELECT array_agg(c.rolname ORDER BY c.rolname) FROM pg_roles c
            WHERE c.rolname IN ('kingres_admin', 'kingres_ops', 'kingres_readonly', 'kingres_stats')
              AND EXISTS (
                WITH RECURSIVE x(roleid) AS (
                    SELECT a.roleid FROM pg_auth_members a WHERE a.member = r.oid AND a.inherit_option
                  UNION
                    SELECT a.roleid FROM pg_auth_members a JOIN x ON a.member = x.roleid WHERE a.inherit_option)
                SELECT 1 FROM x WHERE x.roleid = c.oid)),
           '{}'::name[])::text[]
FROM pg_roles r
WHERE r.rolname !~ '^pg_'
  AND r.rolname NOT IN ('kingres_admin', 'kingres_ops', 'kingres_readonly', 'kingres_stats')
  AND ($1::bool IS NULL OR r.rolcanlogin = $1)
  AND ($2 = '' OR position(lower($2) IN lower(r.rolname)) > 0)
  AND ($3 = '' OR r.rolname = $3)
  AND ($4 = '' OR r.rolname > $4)
ORDER BY r.rolname
LIMIT $5::int`

// readRoles reads the roles after the name `after` in the order of the names, at
// most limit of them (0: no limit; only for one role by name).
func (s *Store) readRoles(ctx context.Context, f store.RoleFilter, name, after string, limit int) ([]api.Role, error) {
	p, err := s.pool(ctx, "")
	if err != nil {
		return nil, err
	}
	var login any
	if f.CanLogin != nil {
		login = *f.CanLogin
	}
	var lim any
	if limit > 0 {
		lim = limit
	}
	rows, err := p.Query(ctx, roleSQL, login, f.Search, name, after, lim)
	if err != nil {
		return nil, s.wrap("reading roles", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (api.Role, error) {
		var role api.Role
		var until *time.Time
		err := r.Scan(&role.Name, &role.CanLogin, &role.IsSuperuser, &role.CanCreateDatabase,
			&role.CanCreateRole, &role.ConnectionLimit, &until, &role.MemberOf, &role.ClusterRoles)
		if until != nil {
			u := until.UTC()
			// PostgreSQL reports "infinity" as a far-future timestamp.
			if u.Year() < 9999 {
				role.ValidUntil = &u
			}
		}
		if role.MemberOf == nil {
			role.MemberOf = []string{}
		}
		if role.ClusterRoles == nil {
			role.ClusterRoles = []string{}
		}
		return role, err
	})
	return out, s.wrap("reading roles", err)
}

// Roles implements store.Store.
func (s *Store) Roles(ctx context.Context, f store.RoleFilter, p store.Page) (api.RolePage, error) {
	after, err := store.DecodeTextKey(p.Cursor, 1)
	if err != nil {
		return api.RolePage{Items: []api.Role{}}, err
	}
	key := ""
	if after != nil {
		key = after[0]
	}
	// One more than the page, so that the next page is known to exist; the
	// database never builds more than that (keyset paging on the role name).
	all, err := s.readRoles(ctx, f, "", key, p.Limit+1)
	if err != nil {
		return api.RolePage{Items: []api.Role{}}, err
	}
	items, next := store.KeysetPage(all, p, func(r api.Role) string { return store.EncodeTextKey(r.Name) })
	if items == nil {
		items = []api.Role{}
	}
	return api.RolePage{Items: items, NextCursor: next}, nil
}

// Role implements store.Store.
func (s *Store) Role(ctx context.Context, name string) (api.Role, error) {
	all, err := s.readRoles(ctx, store.RoleFilter{}, name, "", 1)
	if err != nil {
		return api.Role{}, err
	}
	if len(all) == 0 {
		return api.Role{}, store.ErrNotFound
	}
	return all[0], nil
}

// privileges the contract knows (schema Privilege), by PostgreSQL name.
var knownPrivileges = map[string]bool{
	"connect": true, "create": true, "temporary": true, "usage": true, "select": true,
	"insert": true, "update": true, "delete": true, "truncate": true, "references": true, "trigger": true,
}

// The explicit grants of one level of one database, grouped by object and
// grantee (the privileges of a group in one row), after a position and at most a
// limit of groups: keyset paging in SQL, so that a database with a million grants
// is never read as a whole. $1 is the list of known privileges, $2 the grantee
// filter ("" none).
const grantDatabaseSQL = `
SELECT r.rolname, array_agg(DISTINCT lower(a.privilege_type) ORDER BY lower(a.privilege_type)) FROM pg_database d
CROSS JOIN LATERAL aclexplode(d.datacl) a JOIN pg_roles r ON r.oid = a.grantee
WHERE d.datname = current_database() AND a.grantee <> 0 AND lower(a.privilege_type) = ANY($1)
  AND ($2 = '' OR r.rolname = $2)
  AND ($3::text IS NULL OR r.rolname > $3)
GROUP BY r.rolname ORDER BY r.rolname LIMIT $4`

// $3 filters the schema ("" none); $4 and $5 are the position (schema, grantee).
const grantSchemaSQL = `
SELECT n.nspname, r.rolname, array_agg(DISTINCT lower(a.privilege_type) ORDER BY lower(a.privilege_type)) FROM pg_namespace n
CROSS JOIN LATERAL aclexplode(n.nspacl) a JOIN pg_roles r ON r.oid = a.grantee
WHERE a.grantee <> 0 AND n.nspname !~ '^pg_' AND n.nspname <> 'information_schema' AND lower(a.privilege_type) = ANY($1)
  AND ($2 = '' OR r.rolname = $2) AND ($3 = '' OR n.nspname = $3)
  AND ($4::text IS NULL OR (n.nspname, r.rolname) > ($4, $5))
GROUP BY n.nspname, r.rolname ORDER BY n.nspname, r.rolname LIMIT $6`

// $3 and $4 filter the schema and the table ("" none); $5, $6, $7 are the
// position (schema, table, grantee).
const grantTableSQL = `
SELECT n.nspname, c.relname, r.rolname, array_agg(DISTINCT lower(a.privilege_type) ORDER BY lower(a.privilege_type)) FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
CROSS JOIN LATERAL aclexplode(c.relacl) a JOIN pg_roles r ON r.oid = a.grantee
WHERE c.relkind IN ('r', 'p') AND NOT c.relispartition AND a.grantee <> 0
  AND n.nspname !~ '^pg_' AND n.nspname <> 'information_schema' AND lower(a.privilege_type) = ANY($1)
  AND ($2 = '' OR r.rolname = $2) AND ($3 = '' OR n.nspname = $3) AND ($4 = '' OR c.relname = $4)
  AND ($5::text IS NULL OR (n.nspname, c.relname, r.rolname) > ($5, $6, $7))
GROUP BY n.nspname, c.relname, r.rolname ORDER BY n.nspname, c.relname, r.rolname LIMIT $8`

func privilegeList() []string {
	out := make([]string, 0, len(knownPrivileges))
	for p := range knownPrivileges {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// levelGrants reads at most limit grants of one level of one database that
// come after pos (schema, table, grantee parts; nil: from the start).
func (s *Store) levelGrants(ctx context.Context, h *dbHandle, db, level string, f store.GrantFilter, pos []string, limit int) ([]api.Grant, error) {
	privs := privilegeList()
	var rows pgx.Rows
	var err error
	// text is nil for "no position" (a typed NULL for the comparison above).
	at := func(i int) any {
		if pos == nil {
			return nil
		}
		return pos[i]
	}
	switch level {
	case "database":
		rows, err = h.pool.Query(ctx, grantDatabaseSQL, privs, f.Grantee, at(2), limit)
	case "schema":
		rows, err = h.pool.Query(ctx, grantSchemaSQL, privs, f.Grantee, f.Schema, at(0), at(2), limit)
	default:
		rows, err = h.pool.Query(ctx, grantTableSQL, privs, f.Grantee, f.Schema, f.Table, at(0), at(1), at(2), limit)
	}
	if err != nil {
		return nil, s.wrap("reading grants", err)
	}
	defer rows.Close()
	var out []api.Grant
	for rows.Next() {
		g := api.Grant{Level: level, Database: db}
		var schema, table string
		switch level {
		case "database":
			err = rows.Scan(&g.Grantee, &g.Privileges)
		case "schema":
			err = rows.Scan(&schema, &g.Grantee, &g.Privileges)
		default:
			err = rows.Scan(&schema, &table, &g.Grantee, &g.Privileges)
		}
		if err != nil {
			return nil, s.wrap("scanning grants", err)
		}
		if level != "database" {
			g.Schema = &schema
		}
		if level == "table" {
			g.Table = &table
		}
		out = append(out, g)
	}
	return out, s.wrap("reading grants", rows.Err())
}

// grantCursor makes the cursor that continues after g.
func grantCursor(g api.Grant) string {
	return store.EncodeTextKey(g.Database, g.Level, deref(g.Schema), deref(g.Table), g.Grantee)
}

// Grants implements store.Store. The grants are read from the ACLs of the
// databases, schemas and tables (explicit privileges only; PUBLIC and
// implicit owner privileges are not listed), in the order database, level,
// schema, table, grantee, with keyset paging in SQL: a page reads at most one
// more grant than it returns, whatever the number of grants.
func (s *Store) Grants(ctx context.Context, f store.GrantFilter, p store.Page) (api.GrantPage, error) {
	empty := api.GrantPage{Items: []api.Grant{}}
	after, err := store.DecodeTextKey(p.Cursor, 5)
	if err != nil {
		return empty, err
	}
	names, err := s.databaseNames(ctx)
	if err != nil {
		return empty, err
	}
	var all []api.Grant
	want := p.Limit + 1
collect:
	for _, db := range names {
		if (f.Database != "" && db != f.Database) || (after != nil && db < after[0]) {
			continue
		}
		h, err := s.requireDatabase(ctx, db)
		if err != nil {
			return empty, err
		}
		for _, level := range []string{"database", "schema", "table"} {
			if (f.Level != "" && level != f.Level) || (after != nil && db == after[0] && level < after[1]) {
				continue
			}
			if level == "database" && (f.Schema != "" || f.Table != "") || level == "schema" && f.Table != "" {
				continue // the filter names an object this level does not have
			}
			var pos []string
			if after != nil && db == after[0] && level == after[1] {
				pos = after[2:]
			}
			got, err := s.levelGrants(ctx, h, db, level, f, pos, want-len(all))
			if err != nil {
				s.opts.Logger.Debug("skipping database in grant listing", "database", db, "error", err)
				continue collect
			}
			all = append(all, got...)
			if len(all) >= want {
				break collect
			}
		}
	}
	items, next := store.KeysetPage(all, p, grantCursor)
	if items == nil {
		items = []api.Grant{}
	}
	return api.GrantPage{Items: items, NextCursor: next}, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

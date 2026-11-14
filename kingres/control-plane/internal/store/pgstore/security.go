// SPDX-License-Identifier: PostgreSQL

package pgstore

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
)

// roleSQL lists roles with their direct ordinary memberships and the cluster
// roles they belong to, directly or through other roles. The four built-in
// cluster roles and the predefined pg_* roles are not listed as accounts.
const roleSQL = `
SELECT r.rolname, r.rolcanlogin, r.rolsuper, r.rolcreatedb, r.rolcreaterole, r.rolconnlimit, r.rolvaliduntil,
  coalesce((SELECT array_agg(g.rolname ORDER BY g.rolname) FROM pg_auth_members m
            JOIN pg_roles g ON g.oid = m.roleid
            WHERE m.member = r.oid
              AND g.rolname NOT IN ('kingres_admin', 'kingres_ops', 'kingres_readonly', 'kingres_stats')),
           '{}'::name[])::text[],
  coalesce((SELECT array_agg(c.rolname ORDER BY c.rolname) FROM pg_roles c
            WHERE c.rolname IN ('kingres_admin', 'kingres_ops', 'kingres_readonly', 'kingres_stats')
              AND EXISTS (
                WITH RECURSIVE x(roleid) AS (
                    SELECT a.roleid FROM pg_auth_members a WHERE a.member = r.oid
                  UNION
                    SELECT a.roleid FROM pg_auth_members a JOIN x ON a.member = x.roleid)
                SELECT 1 FROM x WHERE x.roleid = c.oid)),
           '{}'::name[])::text[]
FROM pg_roles r
WHERE r.rolname !~ '^pg_'
  AND r.rolname NOT IN ('kingres_admin', 'kingres_ops', 'kingres_readonly', 'kingres_stats')
  AND ($1::bool IS NULL OR r.rolcanlogin = $1)
  AND ($2 = '' OR position(lower($2) IN lower(r.rolname)) > 0)
  AND ($3 = '' OR r.rolname = $3)
ORDER BY r.rolname`

func (s *Store) readRoles(ctx context.Context, f store.RoleFilter, name string) ([]api.Role, error) {
	p, err := s.pool(ctx, "")
	if err != nil {
		return nil, err
	}
	var login any
	if f.CanLogin != nil {
		login = *f.CanLogin
	}
	rows, err := p.Query(ctx, roleSQL, login, f.Search, name)
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
	all, err := s.readRoles(ctx, f, "")
	if err != nil {
		return api.RolePage{Items: []api.Role{}}, err
	}
	items, next, err := store.Paginate(all, p)
	return api.RolePage{Items: items, NextCursor: next}, err
}

// Role implements store.Store.
func (s *Store) Role(ctx context.Context, name string) (api.Role, error) {
	all, err := s.readRoles(ctx, store.RoleFilter{}, name)
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

type grantKey struct{ grantee, level, schema, table string }

const grantDatabaseSQL = `
SELECT r.rolname, a.privilege_type FROM pg_database d
CROSS JOIN LATERAL aclexplode(d.datacl) a JOIN pg_roles r ON r.oid = a.grantee
WHERE d.datname = current_database() AND a.grantee <> 0`

const grantSchemaSQL = `
SELECT n.nspname, r.rolname, a.privilege_type FROM pg_namespace n
CROSS JOIN LATERAL aclexplode(n.nspacl) a JOIN pg_roles r ON r.oid = a.grantee
WHERE a.grantee <> 0 AND n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'`

const grantTableSQL = `
SELECT n.nspname, c.relname, r.rolname, a.privilege_type FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
CROSS JOIN LATERAL aclexplode(c.relacl) a JOIN pg_roles r ON r.oid = a.grantee
WHERE c.relkind IN ('r', 'p') AND NOT c.relispartition AND a.grantee <> 0
  AND n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'`

// collectGrants runs a grant query and feeds each row to add. names is the
// number of leading text columns before the privilege column.
func (s *Store) collectGrants(ctx context.Context, h *dbHandle, sql string, names int, add func(names []string, priv string)) error {
	rows, err := h.pool.Query(ctx, sql)
	if err != nil {
		return s.wrap("reading grants", err)
	}
	defer rows.Close()
	for rows.Next() {
		vals := make([]string, names+1)
		dest := make([]any, len(vals))
		for i := range vals {
			dest[i] = &vals[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return s.wrap("scanning grants", err)
		}
		add(vals[:names], vals[names])
	}
	return s.wrap("reading grants", rows.Err())
}

// databaseGrants reads the explicit grants of one database into the map.
func (s *Store) databaseGrants(ctx context.Context, h *dbHandle, into map[grantKey]map[string]bool) error {
	put := func(k grantKey, priv string) {
		priv = strings.ToLower(priv)
		if !knownPrivileges[priv] {
			return
		}
		if into[k] == nil {
			into[k] = map[string]bool{}
		}
		into[k][priv] = true
	}
	// Result columns of the three queries: grantee, then privilege; schema
	// and table names come first for the narrower levels.
	if err := s.collectGrants(ctx, h, grantDatabaseSQL, 1, func(n []string, priv string) {
		put(grantKey{grantee: n[0], level: "database"}, priv)
	}); err != nil {
		return err
	}
	if err := s.collectGrants(ctx, h, grantSchemaSQL, 2, func(n []string, priv string) {
		put(grantKey{grantee: n[1], level: "schema", schema: n[0]}, priv)
	}); err != nil {
		return err
	}
	return s.collectGrants(ctx, h, grantTableSQL, 3, func(n []string, priv string) {
		put(grantKey{grantee: n[2], level: "table", schema: n[0], table: n[1]}, priv)
	})
}

// Grants implements store.Store. The grants are read from the ACLs of the
// databases, schemas and tables (explicit privileges only; PUBLIC and
// implicit owner privileges are not listed).
func (s *Store) Grants(ctx context.Context, f store.GrantFilter, p store.Page) (api.GrantPage, error) {
	names, err := s.databaseNames(ctx)
	if err != nil {
		return api.GrantPage{Items: []api.Grant{}}, err
	}
	var all []api.Grant
	for _, db := range names {
		if f.Database != "" && db != f.Database {
			continue
		}
		h, err := s.requireDatabase(ctx, db)
		if err != nil {
			return api.GrantPage{Items: []api.Grant{}}, err
		}
		found := map[grantKey]map[string]bool{}
		if err := s.databaseGrants(ctx, h, found); err != nil {
			s.opts.Logger.Debug("skipping database in grant listing", "database", db, "error", err)
			continue
		}
		for k, privs := range found {
			g := api.Grant{Grantee: k.grantee, Level: k.level, Database: db, Privileges: make([]string, 0, len(privs))}
			if k.schema != "" {
				sc := k.schema
				g.Schema = &sc
			}
			if k.table != "" {
				t := k.table
				g.Table = &t
			}
			for pr := range privs {
				g.Privileges = append(g.Privileges, pr)
			}
			sort.Strings(g.Privileges)
			if store.GrantMatches(g, f) {
				all = append(all, g)
			}
		}
	}
	sort.Slice(all, func(i, j int) bool { return grantLess(all[i], all[j]) })
	items, next, err := store.Paginate(all, p)
	return api.GrantPage{Items: items, NextCursor: next}, err
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func grantLess(a, b api.Grant) bool {
	if a.Database != b.Database {
		return a.Database < b.Database
	}
	if a.Level != b.Level {
		return a.Level < b.Level
	}
	if x, y := deref(a.Schema), deref(b.Schema); x != y {
		return x < y
	}
	if x, y := deref(a.Table), deref(b.Table); x != y {
		return x < y
	}
	return a.Grantee < b.Grantee
}

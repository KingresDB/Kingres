// SPDX-License-Identifier: AGPL-3.0-only

package cluster

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extguard"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sqlx"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/wire"
)

// Global objects (design control-plane-3.md, D4 step 3): roles with their
// password verifiers and memberships, and databases with the extensions they
// use. Passwords are never read or written, only verifiers, and neither is
// logged or stored by kingresd.

const roleSelect = `SELECT r.rolname, r.rolsuper, r.rolinherit, r.rolcreaterole, r.rolcreatedb, r.rolcanlogin,
	r.rolreplication, r.rolbypassrls, r.rolconnlimit,
	CASE WHEN r.rolvaliduntil = 'infinity' THEN NULL ELSE r.rolvaliduntil END, a.rolpassword
FROM pg_catalog.pg_roles r JOIN pg_catalog.pg_authid a ON a.oid = r.oid
WHERE r.rolname !~ '^pg_' AND r.rolname <> $1`

const membershipSelect = `SELECT m.rolname, r.rolname, am.admin_option, am.inherit_option, am.set_option
FROM pg_catalog.pg_auth_members am
JOIN pg_catalog.pg_roles r ON r.oid = am.roleid JOIN pg_catalog.pg_roles m ON m.oid = am.member
WHERE m.rolname !~ '^pg_' AND m.rolname <> $1 AND r.rolname <> $1`

// CollectRoles reads the named roles (all roles when names is empty) from the
// local instance, with their memberships.
func (s *Service) CollectRoles(ctx context.Context, names ...string) ([]wire.RoleSpec, error) {
	c, err := s.InstanceConn(ctx, s.ConnectID(), "postgres")
	if err != nil {
		return nil, fmt.Errorf("connecting to the instance: %w", err)
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	return collectRoles(ctx, c, names)
}

func collectRoles(ctx context.Context, c *pgx.Conn, names []string) ([]wire.RoleSpec, error) {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	rows, err := c.Query(ctx, roleSelect+` ORDER BY r.rolname`, instance.ServiceRole)
	if err != nil {
		return nil, err
	}
	var out []wire.RoleSpec
	idx := map[string]int{}
	for rows.Next() {
		var r wire.RoleSpec
		if err := rows.Scan(&r.Name, &r.Super, &r.Inherit, &r.CreateRole, &r.CreateDB, &r.Login, &r.Replication,
			&r.BypassRLS, &r.ConnectionLimit, &r.ValidUntil, &r.Verifier); err != nil {
			rows.Close()
			return nil, err
		}
		if len(want) > 0 && !want[r.Name] {
			continue
		}
		r.MemberOf = []wire.Membership{}
		idx[r.Name] = len(out)
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	mrows, err := c.Query(ctx, membershipSelect, instance.ServiceRole)
	if err != nil {
		return nil, err
	}
	defer mrows.Close()
	for mrows.Next() {
		var member string
		var m wire.Membership
		if err := mrows.Scan(&member, &m.Role, &m.Admin, &m.Inherit, &m.Set); err != nil {
			return nil, err
		}
		if i, ok := idx[member]; ok {
			out[i].MemberOf = append(out[i].MemberOf, m)
		}
	}
	return out, mrows.Err()
}

// CollectGlobals reads every role and every connectable user database (and the
// system database, which exists on every server as the home of the catalog
// copy) of the local instance.
func (s *Service) CollectGlobals(ctx context.Context) (wire.Globals, error) {
	var g wire.Globals
	roles, err := s.CollectRoles(ctx)
	if err != nil {
		return g, err
	}
	g.Roles = roles
	c, err := s.InstanceConn(ctx, s.ConnectID(), "postgres")
	if err != nil {
		return g, err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	rows, err := c.Query(ctx, `SELECT d.datname, o.rolname, pg_encoding_to_char(d.encoding), d.datcollate, d.datctype, d.datconnlimit
		FROM pg_catalog.pg_database d JOIN pg_catalog.pg_roles o ON o.oid = d.datdba
		WHERE NOT d.datistemplate AND d.datallowconn ORDER BY d.datname`)
	if err != nil {
		return g, err
	}
	for rows.Next() {
		var d wire.DatabaseSpec
		if err := rows.Scan(&d.Name, &d.Owner, &d.Encoding, &d.LcCollate, &d.LcCtype, &d.ConnectionLimit); err != nil {
			rows.Close()
			return g, err
		}
		g.Databases = append(g.Databases, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return g, err
	}
	for i := range g.Databases {
		d := &g.Databases[i]
		d.Settings = []string{}
		srows, err := c.Query(ctx, `SELECT unnest(s.setconfig) FROM pg_catalog.pg_db_role_setting s
			JOIN pg_catalog.pg_database d ON d.oid = s.setdatabase WHERE d.datname = $1 AND s.setrole = 0`, d.Name)
		if err != nil {
			return g, err
		}
		for srows.Next() {
			var kv string
			if err := srows.Scan(&kv); err != nil {
				srows.Close()
				return g, err
			}
			d.Settings = append(d.Settings, kv)
		}
		srows.Close()
		exts, err := s.databaseExtensions(ctx, d.Name)
		if err != nil {
			return g, err
		}
		for _, e := range exts {
			if e == "kingres_shard" {
				d.Sharded = true
			}
		}
		d.Extensions = exts
	}
	return g, nil
}

func (s *Service) databaseExtensions(ctx context.Context, db string) ([]string, error) {
	c, err := s.InstanceConn(ctx, s.ConnectID(), db)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	rows, err := c.Query(ctx, `SELECT extname FROM pg_catalog.pg_extension WHERE extname <> 'plpgsql' ORDER BY extname`)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if out == nil {
		out = []string{}
	}
	return out, err
}

func roleAttrs(b *sqlx.Builder, r wire.RoleSpec) {
	pick := func(on bool, yes, no sqlx.Fixed) {
		if on {
			b.Text(yes)
		} else {
			b.Text(no)
		}
	}
	pick(r.Super, "SUPERUSER", "NOSUPERUSER")
	pick(r.Inherit, "INHERIT", "NOINHERIT")
	pick(r.CreateRole, "CREATEROLE", "NOCREATEROLE")
	pick(r.CreateDB, "CREATEDB", "NOCREATEDB")
	pick(r.Login, "LOGIN", "NOLOGIN")
	pick(r.Replication, "REPLICATION", "NOREPLICATION")
	pick(r.BypassRLS, "BYPASSRLS", "NOBYPASSRLS")
	b.Text("CONNECTION LIMIT").Text(sqlx.Fixed(strconv.Itoa(r.ConnectionLimit)))
	if r.ValidUntil != nil {
		b.Text("VALID UNTIL").Lit(r.ValidUntil.UTC().Format(time.RFC3339Nano))
	} else {
		b.Text("VALID UNTIL").Lit("infinity")
	}
	if r.Verifier != nil {
		b.Text("PASSWORD").Lit(*r.Verifier)
	} else {
		b.Text("PASSWORD NULL")
	}
}

var verifierRE = regexp.MustCompile(`^SCRAM-SHA-256\$[0-9]+:[A-Za-z0-9+/=]+\$[A-Za-z0-9+/=]+:[A-Za-z0-9+/=]+$`)

// validateRoleSpec refuses what must never be applied: reserved names, and a
// password field that is not a SCRAM verifier (PostgreSQL would take any other
// text for a clear text password and hash it).
func validateRoleSpec(r wire.RoleSpec) error {
	if _, err := sqlx.NewIdent(r.Name); err != nil || strings.HasPrefix(r.Name, "pg_") || r.Name == instance.ServiceRole {
		return fmt.Errorf("role name %q cannot be copied", truncName(r.Name))
	}
	if r.Verifier != nil && !verifierRE.MatchString(*r.Verifier) {
		return errors.New("a password field is not a SCRAM-SHA-256 verifier")
	}
	for _, m := range r.MemberOf {
		if _, err := sqlx.NewIdent(m.Role); err != nil {
			return fmt.Errorf("membership role %q is not valid", truncName(m.Role))
		}
	}
	return nil
}

func truncName(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}

// ApplyRoles creates or updates roles on the local instance and makes the
// memberships of each of them exactly the given ones. Roles that are parents
// must exist (copy them in the same call or earlier).
func (s *Service) ApplyRoles(ctx context.Context, roles []wire.RoleSpec) error {
	for _, r := range roles {
		if err := validateRoleSpec(r); err != nil {
			return err
		}
	}
	for _, id := range s.WritableIDs() {
		if err := s.applyRolesAt(ctx, id, roles); err != nil {
			return err
		}
	}
	return nil
}

// applyRolesAt applies the roles to one instance; a server that runs the primaries of several
// groups has several PostgreSQL clusters, each with its own roles.
func (s *Service) applyRolesAt(ctx context.Context, id int, roles []wire.RoleSpec) error {
	c, err := s.InstanceConn(ctx, id, "postgres")
	if err != nil {
		return fmt.Errorf("connecting to the instance: %w", err)
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	for _, r := range roles {
		ok, err := roleExists(ctx, c, r.Name)
		if err != nil {
			return err
		}
		b := new(sqlx.Builder)
		if ok {
			b.Text("ALTER ROLE").ID(sqlx.MustIdent(r.Name)).Text("WITH")
		} else {
			b.Text("CREATE ROLE").ID(sqlx.MustIdent(r.Name)).Text("WITH")
		}
		roleAttrs(b, r)
		if err := execB(ctx, c, b); err != nil {
			return fmt.Errorf("copying role %s: %w", truncName(r.Name), pgReason(err))
		}
	}
	for _, r := range roles {
		if err := applyMemberships(ctx, c, r); err != nil {
			return fmt.Errorf("copying the memberships of role %s: %w", truncName(r.Name), err)
		}
	}
	return nil
}

func applyMemberships(ctx context.Context, c *pgx.Conn, r wire.RoleSpec) error {
	rows, err := c.Query(ctx, `SELECT p.rolname FROM pg_catalog.pg_auth_members am
		JOIN pg_catalog.pg_roles p ON p.oid = am.roleid JOIN pg_catalog.pg_roles m ON m.oid = am.member
		WHERE m.rolname = $1 AND p.rolname <> $2`, r.Name, instance.ServiceRole)
	if err != nil {
		return err
	}
	have, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	want := map[string]wire.Membership{}
	for _, m := range r.MemberOf {
		want[m.Role] = m
	}
	for _, p := range have {
		if _, keep := want[p]; !keep {
			if err := execB(ctx, c, new(sqlx.Builder).Text("REVOKE").ID(sqlx.MustIdent(p)).Text("FROM").ID(sqlx.MustIdent(r.Name))); err != nil {
				return pgReason(err)
			}
		}
	}
	tf := func(v bool) sqlx.Fixed {
		if v {
			return "TRUE"
		}
		return "FALSE"
	}
	for _, m := range r.MemberOf {
		b := new(sqlx.Builder).Text("GRANT").ID(sqlx.MustIdent(m.Role)).Text("TO").ID(sqlx.MustIdent(r.Name))
		b.Text("WITH ADMIN").Text(tf(m.Admin)).Text(", INHERIT").Text(tf(m.Inherit)).Text(", SET").Text(tf(m.Set))
		if err := execB(ctx, c, b); err != nil {
			return pgReason(err)
		}
	}
	return nil
}

// DropRole drops a role on the local instance (not an error when it is gone).
func (s *Service) DropRole(ctx context.Context, name string) error {
	if _, err := sqlx.NewIdent(name); err != nil || strings.HasPrefix(name, "pg_") || name == instance.ServiceRole {
		return fmt.Errorf("role name %q cannot be dropped", truncName(name))
	}
	return s.EachWritable(ctx, "postgres", func(c *pgx.Conn) error {
		if err := execB(ctx, c, new(sqlx.Builder).Text("DROP ROLE IF EXISTS").ID(sqlx.MustIdent(name))); err != nil {
			return errors.New("the role could not be dropped")
		}
		return nil
	})
}

var (
	gucRE    = regexp.MustCompile(`^[a-z_][a-z0-9_.]{0,62}=.{0,200}$`)
	localeOK = regexp.MustCompile(`^[A-Za-z0-9_.@-]{1,64}$`)
)

// ApplyDatabases creates the databases that do not exist and installs the
// extensions each uses (kingres_shard excepted: kingres.add_node installs it).
func (s *Service) ApplyDatabases(ctx context.Context, dbs []wire.DatabaseSpec) error {
	for _, id := range s.WritableIDs() {
		if err := s.applyDatabasesAt(ctx, id, dbs); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) applyDatabasesAt(ctx context.Context, id int, dbs []wire.DatabaseSpec) error {
	c, err := s.InstanceConn(ctx, id, "postgres")
	if err != nil {
		return fmt.Errorf("connecting to the instance: %w", err)
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	for _, d := range dbs {
		if err := s.applyDatabase(ctx, id, c, d); err != nil {
			return fmt.Errorf("copying database %s: %w", truncName(d.Name), err)
		}
	}
	return nil
}

func (s *Service) applyDatabase(ctx context.Context, id int, c *pgx.Conn, d wire.DatabaseSpec) error {
	name, err := sqlx.NewIdent(d.Name)
	if err != nil {
		return errors.New("invalid database name")
	}
	owner, err := sqlx.NewIdent(d.Owner)
	if err != nil {
		return errors.New("invalid owner name")
	}
	var exists bool
	if err := c.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_database WHERE datname = $1)", d.Name).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		for _, v := range []string{d.Encoding, d.LcCollate, d.LcCtype} {
			if !localeOK.MatchString(v) {
				return errors.New("invalid encoding or locale")
			}
		}
		b := new(sqlx.Builder).Text("CREATE DATABASE").ID(name).Text("OWNER").ID(owner).
			Text("TEMPLATE template0 ENCODING").Lit(d.Encoding).Text("LC_COLLATE").Lit(d.LcCollate).Text("LC_CTYPE").Lit(d.LcCtype).
			Text("CONNECTION LIMIT").Text(sqlx.Fixed(strconv.Itoa(d.ConnectionLimit)))
		if err := execB(ctx, c, b); err != nil {
			return pgReason(err)
		}
	}
	for _, kv := range d.Settings {
		if !gucRE.MatchString(kv) {
			return errors.New("invalid database setting")
		}
		k, v, _ := strings.Cut(kv, "=")
		if err := execB(ctx, c, new(sqlx.Builder).Text("ALTER DATABASE").ID(name).Text("SET").Text(sqlx.Fixed(k)).Text("=").Lit(v)); err != nil {
			return pgReason(err)
		}
	}
	if len(d.Extensions) == 0 {
		return nil
	}
	dc, err := s.InstanceConn(ctx, id, d.Name)
	if err != nil {
		return err
	}
	defer func() { _ = dc.Close(context.WithoutCancel(ctx)) }()
	if d.Sharded {
		// kingres_shard creates the schema kingres itself and fails when another
		// extension made it first, so the others wait until kingres.add_node
		// installed it (the leader applies the databases again after that).
		// Found by extguard, not by name alone: a schema of the tenant is not the extension's (the check is the owner, the grants and the members).
		info, err := extguard.Verify(ctx, dc, extguard.Shard)
		if err != nil {
			return fmt.Errorf("the extension kingres_shard of database %s is not used: %w", truncName(d.Name), err)
		}
		if !info.Present {
			return nil
		}
	}
	for _, e := range d.Extensions {
		if e == "kingres_shard" {
			continue
		}
		id, err := sqlx.NewIdent(e)
		if err != nil {
			return errors.New("invalid extension name")
		}
		// Before CREATE EXTENSION: an extension that is there must be trusted, and the schema it would be created in must not be one that a tenant made (the
		// control file of an extension with "schema = ..." uses an existing schema, and the tenant would own what the service role runs).
		if extguard.IsKingres(e) {
			if info, err := extguard.Verify(ctx, dc, e); err != nil {
				return fmt.Errorf("extension %s of database %s is not used: %w", truncName(e), truncName(d.Name), err)
			} else if !info.Present {
				if err := extguard.PreInstallExtension(ctx, dc, e); err != nil {
					return fmt.Errorf("extension %s is not installed in database %s: %w", truncName(e), truncName(d.Name), err)
				}
			}
		}
		if err := execB(ctx, dc, new(sqlx.Builder).Text("CREATE EXTENSION IF NOT EXISTS").ID(id)); err != nil {
			return fmt.Errorf("installing extension %s failed", truncName(e))
		}
	}
	return nil
}

// ApplyGlobals applies roles, then databases.
func (s *Service) ApplyGlobals(ctx context.Context, g wire.Globals) error {
	if err := s.ApplyRoles(ctx, g.Roles); err != nil {
		return err
	}
	return s.ApplyDatabases(ctx, g.Databases)
}

// pgReason describes a PostgreSQL error for the operator: SQLSTATE and
// PostgreSQL's own message (which never repeats a password or verifier).
func pgReason(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return fmt.Errorf("SQLSTATE %s: %s", pe.Code, pe.Message)
	}
	if errors.Is(err, errNotQuoted) {
		return errors.New("the statement could not be built")
	}
	return errors.New("the statement failed")
}

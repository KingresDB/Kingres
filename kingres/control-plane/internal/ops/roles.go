// SPDX-License-Identifier: PostgreSQL

package ops

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scram"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sqlx"
)

// roleDB is the database role statements run in; roles are cluster-wide.
const roleDB = "postgres"

var roleNameRE = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

func isClusterRole(n string) bool {
	for _, r := range cluster.ClusterRoles {
		if r == n {
			return true
		}
	}
	return false
}

func field(f, m, c string) problem.FieldError {
	return problem.FieldError{Field: f, Message: m, Code: c}
}

// otherAdmins counts the roles that can log in, are not expired, are members
// of kingres_admin (directly or through other roles) and are not except.
const otherAdminsSQL = `
WITH RECURSIVE g(member) AS (
    SELECT a.member FROM pg_catalog.pg_auth_members a
    WHERE a.roleid = (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = 'kingres_admin') AND a.inherit_option
  UNION
    SELECT a.member FROM pg_catalog.pg_auth_members a JOIN g ON a.roleid = g.member WHERE a.inherit_option)
SELECT count(*) FROM pg_catalog.pg_roles r
WHERE r.oid IN (SELECT member FROM g) AND r.rolcanlogin AND r.rolname <> $1
  AND (r.rolvaliduntil IS NULL OR r.rolvaliduntil > now())`

func lastAdminCheck(ctx context.Context, x execer, role string) error {
	var n int
	if err := x.QueryRow(ctx, otherAdminsSQL, role).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return problem.New(problem.LastAdmin, "This would leave the cluster without an administrator that can sign in.")
	}
	return nil
}

func isAdminMember(ctx context.Context, x execer, role string) (bool, error) {
	var ok bool
	err := x.QueryRow(ctx, `
WITH RECURSIVE g(member) AS (
    SELECT a.member FROM pg_catalog.pg_auth_members a
    WHERE a.roleid = (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = 'kingres_admin') AND a.inherit_option
  UNION
    SELECT a.member FROM pg_catalog.pg_auth_members a JOIN g ON a.roleid = g.member WHERE a.inherit_option)
SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_roles r WHERE r.oid IN (SELECT member FROM g) AND r.rolname = $1)`, role).Scan(&ok)
	return ok, err
}

func roleExistsTx(ctx context.Context, x execer, name string) (bool, error) {
	var ok bool
	err := x.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = $1)", name).Scan(&ok)
	return ok, err
}

func verifier(pw *string, f string) (string, error) {
	if utf8.RuneCountInString(*pw) < 8 || len(*pw) > 1024 {
		return "", problem.Validation("The role is invalid.", field(f, "must be 8 to 1024 characters", "out_of_range"))
	}
	v, err := scram.Verifier(*pw)
	if err != nil {
		return "", problem.Validation("The role is invalid.", field(f, "is not acceptable", "pattern"))
	}
	return v, nil
}

func validUntilLit(t *time.Time) string {
	if t == nil {
		return "infinity"
	}
	return t.UTC().Format("2006-01-02 15:04:05.999999") + "+00"
}

// CreateRole creates a role as the signed-in actor and applies the optional
// memberships and cluster roles in the same transaction.
func (o *Ops) createRole(ctx context.Context, actor string, req api.RoleCreateRequest) error {
	var errs []problem.FieldError
	if !roleNameRE.MatchString(req.Name) {
		errs = append(errs, field("/name", "must match ^[a-z_][a-z0-9_]{0,62}$", "pattern"))
	} else if len(req.Name) > 3 && req.Name[:3] == "pg_" {
		errs = append(errs, field("/name", "names starting with pg_ are reserved", "reserved"))
	}
	var vfy string
	if req.Password != nil {
		v, err := verifier(req.Password, "/password")
		if err != nil {
			errs = append(errs, problemFields(err)...)
		}
		vfy = v
	}
	if req.ConnectionLimit != nil && *req.ConnectionLimit < -1 {
		errs = append(errs, field("/connectionLimit", "must be -1 or more", "out_of_range"))
	}
	for i, m := range req.MemberOf {
		if _, err := sqlx.NewIdent(m); err != nil {
			errs = append(errs, field("/memberOf/"+strconv.Itoa(i), "is not a valid role name", "pattern"))
		} else if isClusterRole(m) {
			errs = append(errs, field("/memberOf/"+strconv.Itoa(i), "cluster roles are set with clusterRoles", "invalid"))
		} else if why := memberOfRefusal(m); why != "" {
			errs = append(errs, field("/memberOf/"+strconv.Itoa(i), why, "invalid"))
		}
	}
	for i, c := range req.ClusterRoles {
		if !isClusterRole(c) {
			errs = append(errs, field("/clusterRoles/"+strconv.Itoa(i), "must be one of the four cluster roles", "enum"))
		}
	}
	if len(errs) > 0 {
		return problem.Validation("The role is invalid.", errs...)
	}
	name := sqlx.MustIdent(req.Name)
	login := req.CanLogin == nil || *req.CanLogin
	err := o.asRole(ctx, roleDB, actor, func(tx pgx.Tx) error {
		if ok, err := roleExistsTx(ctx, tx, req.Name); err != nil {
			return err
		} else if ok {
			return problem.New(problem.AlreadyExists, fmt.Sprintf("Role %q already exists.", req.Name))
		}
		for i, m := range req.MemberOf {
			if ok, err := roleExistsTx(ctx, tx, m); err != nil {
				return err
			} else if !ok {
				return problem.Validation("The role is invalid.", field("/memberOf/"+strconv.Itoa(i), "role does not exist", "not_found"))
			}
			if err := refuseDangerousTarget(ctx, tx, m); err != nil {
				return err
			}
		}
		b := stmt().Text("CREATE ROLE").ID(name)
		attr := func(on bool, yes, no sqlx.Fixed) { b.Text(map[bool]sqlx.Fixed{true: yes, false: no}[on]) }
		attr(login, "LOGIN", "NOLOGIN")
		attr(req.CanCreateDatabase != nil && *req.CanCreateDatabase, "CREATEDB", "NOCREATEDB")
		attr(req.CanCreateRole != nil && *req.CanCreateRole, "CREATEROLE", "NOCREATEROLE")
		if req.ConnectionLimit != nil {
			b.Text("CONNECTION LIMIT").Text(sqlx.Fixed(strconv.Itoa(*req.ConnectionLimit)))
		}
		if req.ValidUntil != nil {
			b.Text("VALID UNTIL").Lit(validUntilLit(req.ValidUntil))
		}
		if vfy != "" {
			b.Text("PASSWORD").Lit(vfy)
		}
		if err := run(ctx, tx, b); err != nil {
			return err
		}
		// Every kingres_admin may manage this role later (ADMIN OPTION without
		// inheriting or being able to become it). Administrators themselves are
		// the exception: that grant would make a membership cycle, and PostgreSQL
		// lets only a superuser manage them (see roleExecutor).
		for _, c := range req.ClusterRoles {
			if c == "kingres_admin" {
				return nil
			}
		}
		return run(ctx, tx, stmt().Text("GRANT").ID(name).Text("TO").ID(sqlx.MustIdent("kingres_admin")).
			Text("WITH ADMIN OPTION, INHERIT FALSE, SET FALSE"))
	})
	if err != nil {
		return wrap(err)
	}
	if len(req.MemberOf) == 0 && len(req.ClusterRoles) == 0 {
		return nil
	}
	// Memberships are granted with the service role: PostgreSQL records the
	// grantor of a grant and only lets the grantor revoke it, which would
	// make roles administered by one administrator unmanageable for the next.
	err = o.asService(ctx, roleDB, func(tx pgx.Tx) error {
		for _, m := range req.MemberOf {
			if err := run(ctx, tx, stmt().Text("GRANT").ID(sqlx.MustIdent(m)).Text("TO").ID(name)); err != nil {
				return err
			}
		}
		for _, c := range req.ClusterRoles {
			if err := grantClusterRole(ctx, tx, name, c); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		// Do not leave a half-made role behind.
		if derr := o.asRole(ctx, roleDB, actor, func(tx pgx.Tx) error {
			return run(ctx, tx, stmt().Text("DROP ROLE").ID(name))
		}); derr != nil {
			o.Log.Error("removing a role whose memberships failed", "role", req.Name)
		}
	}
	return wrap(err)
}

func wrap(err error) error {
	if err == nil {
		return nil
	}
	return PGProblem(err)
}

// UpdateRole changes attributes of a role.
func (o *Ops) updateRole(ctx context.Context, actor, role string, req api.RoleUpdateRequest) error {
	var errs []problem.FieldError
	if _, err := sqlx.NewIdent(role); err != nil {
		return problem.New(problem.NotFound, "No such role.")
	}
	var vfy string
	if req.Password != nil {
		v, err := verifier(req.Password, "/password")
		if err != nil {
			errs = append(errs, problemFields(err)...)
		}
		vfy = v
	}
	if req.ConnectionLimit != nil && *req.ConnectionLimit < -1 {
		errs = append(errs, field("/connectionLimit", "must be -1 or more", "out_of_range"))
	}
	if len(errs) > 0 {
		return problem.Validation("The role update is invalid.", errs...)
	}
	disabling := req.CanLogin != nil && !*req.CanLogin
	err := o.roleExecutor(ctx, actor, role, func(tx pgx.Tx) error {
		if ok, err := roleExistsTx(ctx, tx, role); err != nil {
			return err
		} else if !ok {
			return problem.New(problem.NotFound, fmt.Sprintf("Role %q does not exist.", role))
		}
		if disabling {
			if role == actor {
				return problem.New(problem.Conflict, "A role cannot disable itself.")
			}
			if isAdm, err := isAdminMember(ctx, tx, role); err != nil {
				return err
			} else if isAdm {
				if err := lastAdminCheck(ctx, tx, role); err != nil {
					return err
				}
			}
		}
		b := stmt().Text("ALTER ROLE").ID(sqlx.MustIdent(role))
		if req.CanLogin != nil {
			b.Text(map[bool]sqlx.Fixed{true: "LOGIN", false: "NOLOGIN"}[*req.CanLogin])
		}
		if req.CanCreateDatabase != nil {
			b.Text(map[bool]sqlx.Fixed{true: "CREATEDB", false: "NOCREATEDB"}[*req.CanCreateDatabase])
		}
		if req.CanCreateRole != nil {
			b.Text(map[bool]sqlx.Fixed{true: "CREATEROLE", false: "NOCREATEROLE"}[*req.CanCreateRole])
		}
		if req.ConnectionLimit != nil {
			b.Text("CONNECTION LIMIT").Text(sqlx.Fixed(strconv.Itoa(*req.ConnectionLimit)))
		}
		if req.ValidUntil.Set {
			b.Text("VALID UNTIL").Lit(validUntilLit(req.ValidUntil.Value))
		}
		if vfy != "" {
			b.Text("PASSWORD").Lit(vfy)
		}
		return run(ctx, tx, b)
	})
	if err == nil && disabling {
		o.Roles.Revoke(ctx, role)
	}
	return wrap(err)
}

// DeleteRole drops a role.
func (o *Ops) deleteRole(ctx context.Context, actor, role string) error {
	if _, err := sqlx.NewIdent(role); err != nil {
		return problem.New(problem.NotFound, "No such role.")
	}
	err := o.roleExecutor(ctx, actor, role, func(tx pgx.Tx) error {
		if ok, err := roleExistsTx(ctx, tx, role); err != nil {
			return err
		} else if !ok {
			return problem.New(problem.NotFound, fmt.Sprintf("Role %q does not exist.", role))
		}
		if isClusterRole(role) {
			return problem.New(problem.Conflict, "The built-in cluster roles cannot be dropped.")
		}
		if role == actor {
			return problem.New(problem.Conflict, "A role cannot drop itself.")
		}
		if isAdm, err := isAdminMember(ctx, tx, role); err != nil {
			return err
		} else if isAdm {
			if err := lastAdminCheck(ctx, tx, role); err != nil {
				return err
			}
		}
		return run(ctx, tx, stmt().Text("DROP ROLE").ID(sqlx.MustIdent(role)))
	})
	if err == nil {
		o.Roles.Revoke(ctx, role)
	}
	return wrap(err)
}

// currentMemberships returns the roles that role is a direct member of.
func currentMemberships(ctx context.Context, x execer, role string) ([]string, error) {
	rows, err := x.Query(ctx, `SELECT g.rolname FROM pg_catalog.pg_auth_members m
		JOIN pg_catalog.pg_roles g ON g.oid = m.roleid
		JOIN pg_catalog.pg_roles r ON r.oid = m.member WHERE r.rolname = $1 AND m.inherit_option ORDER BY 1`, role)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func diff(want, have []string) (add, del []string) {
	h, w := map[string]bool{}, map[string]bool{}
	for _, x := range have {
		h[x] = true
	}
	for _, x := range want {
		w[x] = true
		if !h[x] {
			add = append(add, x)
		}
	}
	for _, x := range have {
		if !w[x] {
			del = append(del, x)
		}
	}
	sort.Strings(add)
	sort.Strings(del)
	return add, del
}

// SetMemberships replaces the ordinary roles that role is a member of.
func (o *Ops) setMemberships(ctx context.Context, actor, role string, want []string) error {
	var errs []problem.FieldError
	for i, m := range want {
		if _, err := sqlx.NewIdent(m); err != nil {
			errs = append(errs, field("/memberOf/"+strconv.Itoa(i), "is not a valid role name", "pattern"))
		} else if isClusterRole(m) {
			errs = append(errs, field("/memberOf/"+strconv.Itoa(i), "cluster roles are set with the cluster-roles resource", "invalid"))
		} else if why := memberOfRefusal(m); why != "" {
			errs = append(errs, field("/memberOf/"+strconv.Itoa(i), why, "invalid"))
		} else if m == role {
			errs = append(errs, field("/memberOf/"+strconv.Itoa(i), "a role cannot be a member of itself", "invalid"))
		}
	}
	if len(errs) > 0 {
		return problem.Validation("The memberships are invalid.", errs...)
	}
	if _, err := sqlx.NewIdent(role); err != nil {
		return problem.New(problem.NotFound, "No such role.")
	}
	return wrap(o.asService(ctx, roleDB, func(tx pgx.Tx) error {
		if ok, err := roleExistsTx(ctx, tx, role); err != nil {
			return err
		} else if !ok {
			return problem.New(problem.NotFound, fmt.Sprintf("Role %q does not exist.", role))
		}
		have, err := currentMemberships(ctx, tx, role)
		if err != nil {
			return err
		}
		var ordinary []string
		for _, h := range have {
			if !isClusterRole(h) {
				ordinary = append(ordinary, h)
			}
		}
		add, del := diff(want, ordinary)
		for i, m := range add {
			if ok, err := roleExistsTx(ctx, tx, m); err != nil {
				return err
			} else if !ok {
				return problem.Validation("The memberships are invalid.", field("/memberOf/"+strconv.Itoa(i), "role "+strconv.Quote(m)+" does not exist", "not_found"))
			}
			if err := refuseDangerousTarget(ctx, tx, m); err != nil {
				return err
			}
		}
		r := sqlx.MustIdent(role)
		for _, m := range add {
			if err := run(ctx, tx, stmt().Text("GRANT").ID(sqlx.MustIdent(m)).Text("TO").ID(r)); err != nil {
				return err
			}
		}
		for _, m := range del {
			if err := revokeAll(ctx, tx, m, role); err != nil {
				return err
			}
		}
		return nil
	}))
}

// SetClusterRoles replaces the cluster roles of a role.
func (o *Ops) setClusterRoles(ctx context.Context, actor, role string, want []string) error {
	for i, c := range want {
		if !isClusterRole(c) {
			return problem.Validation("The cluster roles are invalid.", field("/clusterRoles/"+strconv.Itoa(i), "must be one of the four cluster roles", "enum"))
		}
	}
	if _, err := sqlx.NewIdent(role); err != nil {
		return problem.New(problem.NotFound, "No such role.")
	}
	return wrap(o.asService(ctx, roleDB, func(tx pgx.Tx) error {
		if ok, err := roleExistsTx(ctx, tx, role); err != nil {
			return err
		} else if !ok {
			return problem.New(problem.NotFound, fmt.Sprintf("Role %q does not exist.", role))
		}
		have, err := currentMemberships(ctx, tx, role)
		if err != nil {
			return err
		}
		var cur []string
		for _, h := range have {
			if isClusterRole(h) {
				cur = append(cur, h)
			}
		}
		add, del := diff(want, cur)
		r := sqlx.MustIdent(role)
		for _, c := range del {
			if c == "kingres_admin" {
				if err := lastAdminCheck(ctx, tx, role); err != nil {
					return err
				}
			}
			if err := revokeAll(ctx, tx, c, role); err != nil {
				return err
			}
		}
		for _, c := range add {
			if err := grantClusterRole(ctx, tx, r, c); err != nil {
				return err
			}
		}
		return nil
	}))
}

// grantClusterRole makes role a member of a cluster role. A member of
// kingres_admin also needs CREATEROLE and CREATEDB to create roles and
// databases with its own connection.
func grantClusterRole(ctx context.Context, x execer, role sqlx.Ident, clusterRole string) error {
	if err := run(ctx, x, stmt().Text("GRANT").ID(sqlx.MustIdent(clusterRole)).Text("TO").ID(role)); err != nil {
		return err
	}
	if clusterRole == "kingres_admin" {
		return run(ctx, x, stmt().Text("ALTER ROLE").ID(role).Text("CREATEROLE CREATEDB"))
	}
	return nil
}

// revokeAll removes the membership of member in parent whoever granted it:
// PostgreSQL lets a role revoke only the grants it made itself, and a
// superuser can name the grantor.
func revokeAll(ctx context.Context, x execer, parent, member string) error {
	rows, err := x.Query(ctx, `SELECT DISTINCT g.rolname FROM pg_catalog.pg_auth_members am
		JOIN pg_catalog.pg_roles p ON p.oid = am.roleid JOIN pg_catalog.pg_roles m ON m.oid = am.member
		JOIN pg_catalog.pg_roles g ON g.oid = am.grantor WHERE p.rolname = $1 AND m.rolname = $2`, parent, member)
	if err != nil {
		return err
	}
	grantors, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, g := range grantors {
		gi, err := sqlx.NewIdent(g)
		if err != nil {
			return err
		}
		if err := run(ctx, x, stmt().Text("REVOKE").ID(sqlx.MustIdent(parent)).Text("FROM").ID(sqlx.MustIdent(member)).Text("GRANTED BY").ID(gi)); err != nil {
			return err
		}
	}
	return nil
}

// roleExecutor runs f as the signed-in actor, so PostgreSQL's own checks
// apply. The exception is a target that is itself an administrator (a member
// of kingres_admin): PostgreSQL has no way to let administrators manage each
// other (the ADMIN OPTION grant that would allow it is a membership cycle), so
// for those the change runs with the service role. The caller has already
// been authorized as kingres_admin by the cluster-role check.
func (o *Ops) roleExecutor(ctx context.Context, actor, target string, f func(tx pgx.Tx) error) error {
	isAdmin := false
	if err := o.asService(ctx, roleDB, func(tx pgx.Tx) error {
		var err error
		isAdmin, err = isAdminMember(ctx, tx, target)
		return err
	}); err != nil {
		return err
	}
	if isAdmin {
		return o.asService(ctx, roleDB, f)
	}
	return o.asRole(ctx, roleDB, actor, f)
}

func problemFields(err error) []problem.FieldError {
	var p *problem.Problem
	if errors.As(err, &p) {
		return p.Errors
	}
	return nil
}

// The role operations run on this server as the signed-in role, then on every
// other server (design D4); see fanout.go.

// CreateRole creates a role as the signed-in actor and applies the optional
// memberships and cluster roles in the same transaction.
func (o *Ops) CreateRole(ctx context.Context, actor string, req api.RoleCreateRequest) error {
	if err := o.createRole(ctx, actor, req); err != nil {
		return err
	}
	o.replicateRoles(ctx, req.Name)
	o.roleChanged(ctx, req.Name)
	return nil
}

// UpdateRole changes attributes of a role.
func (o *Ops) UpdateRole(ctx context.Context, actor, role string, req api.RoleUpdateRequest) error {
	if err := o.updateRole(ctx, actor, role, req); err != nil {
		return err
	}
	o.replicateRoles(ctx, role)
	o.roleChanged(ctx, role)
	return nil
}

// DeleteRole drops a role.
func (o *Ops) DeleteRole(ctx context.Context, actor, role string) error {
	if err := o.deleteRole(ctx, actor, role); err != nil {
		return err
	}
	o.replicateRoleDrop(ctx, role)
	o.roleChanged(ctx, role)
	return nil
}

// SetMemberships replaces the ordinary memberships of a role.
func (o *Ops) SetMemberships(ctx context.Context, actor, role string, want []string) error {
	if err := o.setMemberships(ctx, actor, role, want); err != nil {
		return err
	}
	o.replicateRoles(ctx, role)
	o.roleChanged(ctx, role)
	return nil
}

// SetClusterRoles replaces the cluster roles of a role.
func (o *Ops) SetClusterRoles(ctx context.Context, actor, role string, want []string) error {
	if err := o.setClusterRoles(ctx, actor, role, want); err != nil {
		return err
	}
	o.replicateRoles(ctx, role)
	o.roleChanged(ctx, role)
	return nil
}

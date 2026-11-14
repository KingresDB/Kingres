// SPDX-License-Identifier: PostgreSQL

package ops

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sqlx"
)

// Closed enumerations of the privileges valid per object level.
var (
	dbPrivs     = sqlx.NewKeywordSet("CONNECT", "CREATE", "TEMPORARY")
	schemaPrivs = sqlx.NewKeywordSet("USAGE", "CREATE")
	tablePrivs  = sqlx.NewKeywordSet("SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER")
)

type grantTarget struct {
	privs []sqlx.Keyword
	on    func(b *sqlx.Builder)
}

// parseGrant validates a grant request completely before any connection is
// opened: level, privilege names (lower case, from the set of the level) and
// every identifier.
func parseGrant(req api.GrantRequest) (grantTarget, sqlx.Ident, error) {
	var errs []problem.FieldError
	var set sqlx.KeywordSet
	switch req.Level {
	case "database":
		set = dbPrivs
		if req.Schema != "" || req.Table != "" {
			errs = append(errs, field("/schema", "must be omitted for level database", "invalid"))
		}
	case "schema":
		set = schemaPrivs
		if req.Schema == "" {
			errs = append(errs, field("/schema", "is required for level schema", "required"))
		}
		if req.Table != "" {
			errs = append(errs, field("/table", "must be omitted for level schema", "invalid"))
		}
	case "table":
		set = tablePrivs
		if req.Schema == "" {
			errs = append(errs, field("/schema", "is required for level table", "required"))
		}
		if req.Table == "" {
			errs = append(errs, field("/table", "is required for level table", "required"))
		}
	default:
		errs = append(errs, field("/level", "must be database, schema or table", "enum"))
	}
	var t grantTarget
	seen := map[string]bool{}
	if set.Words() != nil {
		for i, p := range req.Privileges {
			kw, err := set.Parse(strings.ToUpper(p))
			if err != nil || p != strings.ToLower(p) || seen[p] {
				errs = append(errs, field("/privileges/"+strconv.Itoa(i), "is not valid at this level", "enum"))
				continue
			}
			seen[p] = true
			t.privs = append(t.privs, kw)
		}
	}
	if len(req.Privileges) == 0 {
		errs = append(errs, field("/privileges", "at least one privilege is required", "required"))
	}
	grantee, gerr := sqlx.NewIdent(req.Grantee)
	if gerr != nil {
		errs = append(errs, field("/grantee", "is not a valid role name", "pattern"))
	}
	db, derr := sqlx.NewIdent(req.Database)
	if derr != nil {
		errs = append(errs, field("/database", "is not a valid database name", "pattern"))
	}
	var obj sqlx.Ident
	switch {
	case len(errs) > 0:
	case req.Level == "database":
		obj = db
		t.on = func(b *sqlx.Builder) { b.Text("ON DATABASE").ID(obj) }
	case req.Level == "schema":
		o, err := sqlx.NewIdent(req.Schema)
		if err != nil {
			errs = append(errs, field("/schema", "is not a valid schema name", "pattern"))
		}
		obj = o
		t.on = func(b *sqlx.Builder) { b.Text("ON SCHEMA").ID(obj) }
	default:
		o, err := sqlx.QualifiedIdent(req.Schema, req.Table)
		if err != nil {
			errs = append(errs, field("/table", "is not a valid table name", "pattern"))
		}
		obj = o
		t.on = func(b *sqlx.Builder) { b.Text("ON TABLE").ID(obj) }
	}
	if len(errs) > 0 {
		return grantTarget{}, sqlx.Ident{}, problem.Validation("The grant is invalid.", errs...)
	}
	return t, grantee, nil
}

func (t grantTarget) build(verb, prep sqlx.Fixed, grantee sqlx.Ident) *sqlx.Builder {
	b := stmt().Text(verb)
	for i, p := range t.privs {
		if i > 0 {
			b.Text(",")
		}
		b.Kw(p)
	}
	t.on(b)
	return b.Text(prep).ID(grantee)
}

// checkLedgerRule refuses update, delete and truncate on append-only ledger
// tables (the ledger rejects them anyway; this answers with a clear 422).
func checkLedgerRule(ctx context.Context, x execer, req api.GrantRequest) error {
	if req.Level != "table" {
		return nil
	}
	risky := false
	for _, p := range req.Privileges {
		if p == "update" || p == "delete" || p == "truncate" {
			risky = true
		}
	}
	if !risky {
		return nil
	}
	var has bool
	if err := x.QueryRow(ctx, "SELECT to_regclass('kingres.ledger_table') IS NOT NULL").Scan(&has); err != nil || !has {
		return err
	}
	qn, err := sqlx.QualifiedIdent(req.Schema, req.Table)
	if err != nil {
		return err
	}
	var appendOnly bool
	err = x.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM kingres.ledger_table lt
		WHERE lt.relid = to_regclass($1) AND lt.mode = 'append_only')`, qn.String()).Scan(&appendOnly)
	if err != nil {
		return err
	}
	if appendOnly {
		return problem.New(problem.Unprocessable, "UPDATE, DELETE and TRUNCATE cannot be granted on an append-only ledger table.")
	}
	return nil
}

// Grant issues GRANT as the actor.
func (o *Ops) Grant(ctx context.Context, actor string, req api.GrantRequest) error {
	t, grantee, err := parseGrant(req)
	if err != nil {
		return err
	}
	return wrap(o.asRole(ctx, req.Database, actor, func(tx pgx.Tx) error {
		if err := checkLedgerRule(ctx, tx, req); err != nil {
			return err
		}
		return run(ctx, tx, t.build("GRANT", "TO", grantee))
	}))
}

// Revoke issues REVOKE as the actor.
func (o *Ops) Revoke(ctx context.Context, actor string, req api.GrantRequest) error {
	t, grantee, err := parseGrant(req)
	if err != nil {
		return err
	}
	return wrap(o.asRole(ctx, req.Database, actor, func(tx pgx.Tx) error {
		return run(ctx, tx, t.build("REVOKE", "FROM", grantee))
	}))
}

// ApplyTemplate applies readOnly, readWrite or owner to a role on a database.
func (o *Ops) ApplyTemplate(ctx context.Context, actor string, req api.GrantTemplateRequest) error {
	var errs []problem.FieldError
	switch req.Template {
	case "readOnly", "readWrite", "owner":
	default:
		errs = append(errs, field("/template", "must be readOnly, readWrite or owner", "enum"))
	}
	grantee, gerr := sqlx.NewIdent(req.Grantee)
	if gerr != nil {
		errs = append(errs, field("/grantee", "is not a valid role name", "pattern"))
	}
	db, derr := sqlx.NewIdent(req.Database)
	if derr != nil {
		errs = append(errs, field("/database", "is not a valid database name", "pattern"))
	}
	if len(errs) > 0 {
		return problem.Validation("The template request is invalid.", errs...)
	}
	return wrap(o.asRole(ctx, req.Database, actor, func(tx pgx.Tx) error {
		if ok, err := roleExistsTx(ctx, tx, req.Grantee); err != nil {
			return err
		} else if !ok {
			return problem.New(problem.NotFound, fmt.Sprintf("Role %q does not exist.", req.Grantee))
		}
		if req.Template == "owner" {
			return run(ctx, tx, stmt().Text("ALTER DATABASE").ID(db).Text("OWNER TO").ID(grantee))
		}
		if err := run(ctx, tx, stmt().Text("GRANT CONNECT ON DATABASE").ID(db).Text("TO").ID(grantee)); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT nspname FROM pg_catalog.pg_namespace
			WHERE nspname !~ '^pg_' AND nspname NOT IN ('information_schema', 'kingres') ORDER BY 1`)
		if err != nil {
			return err
		}
		schemas, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		write := req.Template == "readWrite"
		var hasLedger bool
		if err := tx.QueryRow(ctx, "SELECT to_regclass('kingres.ledger_table') IS NOT NULL").Scan(&hasLedger); err != nil {
			return err
		}
		for _, sn := range schemas {
			s, err := sqlx.NewIdent(sn)
			if err != nil {
				continue
			}
			if err := run(ctx, tx, stmt().Text("GRANT USAGE ON SCHEMA").ID(s).Text("TO").ID(grantee)); err != nil {
				return err
			}
			if err := run(ctx, tx, stmt().Text("GRANT SELECT ON ALL TABLES IN SCHEMA").ID(s).Text("TO").ID(grantee)); err != nil {
				return err
			}
			if err := run(ctx, tx, stmt().Text("ALTER DEFAULT PRIVILEGES IN SCHEMA").ID(s).Text("GRANT SELECT ON TABLES TO").ID(grantee)); err != nil {
				return err
			}
			if !write {
				continue
			}
			// Write privileges go table by table so that append-only ledger
			// tables only get INSERT.
			appendOnly := "false"
			if hasLedger {
				appendOnly = "EXISTS (SELECT 1 FROM kingres.ledger_table lt WHERE lt.relid = c.oid AND lt.mode = 'append_only')"
			}
			// appendOnly is one of two fixed texts, never request text.
			trows, err := tx.Query(ctx, `SELECT c.relname, `+appendOnly+`
				FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
				WHERE n.nspname = $1 AND c.relkind IN ('r', 'p') ORDER BY 1`, sn)
			if err != nil {
				return err
			}
			type tbl struct {
				name       string
				appendOnly bool
			}
			tables, err := pgx.CollectRows(trows, func(r pgx.CollectableRow) (tbl, error) {
				var t tbl
				return t, r.Scan(&t.name, &t.appendOnly)
			})
			if err != nil {
				return err
			}
			for _, t := range tables {
				qn, err := sqlx.QualifiedIdent(sn, t.name)
				if err != nil {
					continue
				}
				privs := "INSERT, UPDATE, DELETE"
				if t.appendOnly {
					privs = "INSERT"
				}
				// The privilege list is one of two fixed strings.
				if err := run(ctx, tx, stmt().Text(sqlx.Fixed("GRANT "+privs+" ON TABLE")).ID(qn).Text("TO").ID(grantee)); err != nil {
					return err
				}
			}
			if err := run(ctx, tx, stmt().Text("ALTER DEFAULT PRIVILEGES IN SCHEMA").ID(s).Text("GRANT INSERT, UPDATE, DELETE ON TABLES TO").ID(grantee)); err != nil {
				return err
			}
		}
		return nil
	}))
}

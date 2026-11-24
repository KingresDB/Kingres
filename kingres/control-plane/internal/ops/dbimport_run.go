// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/archive"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sqlshape"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sqlx"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/transfer"
)

// This file is the part of the import that runs as the signed-in role: its connection, the statements of the three SQL files and the load of the data.

// importSession are the settings of the connection of the role. They are set by the importer (a SET in the archive is refused by the grammar, not honoured), and
// search_path is pg_catalog: every name in an archive is qualified, and an unqualified one finds nothing that the role could have placed in front of the system.
var importSession = []string{
	"SET search_path = pg_catalog",
	"SET statement_timeout = '10min'",
	"SET lock_timeout = '1min'",
	"SET idle_in_transaction_session_timeout = '15min'",
	"SET standard_conforming_strings = on",
	"SET client_min_messages = warning",
	"SET default_transaction_read_only = off",
	"SET check_function_bodies = on",
}

// importRoleConn opens the connection of the importing role to the private database, refuses a role that is dangerous (again, at every use) and sets the session.
func (o *Ops) importRoleConn(ctx context.Context, priv, role string) (*pgx.Conn, error) {
	c, err := o.dbRoleConn(ctx, priv, role)
	if err != nil {
		return nil, transfer.Errorf(transfer.CodeSessionLost, true, "the role cannot connect to the private database from this server now; repeat the operation")
	}
	bad, derr := dangerousRole(ctx, c)
	if derr != nil {
		_ = c.Close(context.WithoutCancel(ctx))
		return nil, transfer.Errorf(transfer.CodeSessionLost, true, "the check of the role could not be made now; repeat the operation")
	}
	if bad {
		_ = c.Close(context.WithoutCancel(ctx))
		return nil, transfer.Errorf(transfer.CodeForbidden, false, "the role has the SUPERUSER, REPLICATION or BYPASSRLS attribute or access to server files, programs or logs; the statements of an archive run as that role")
	}
	if why, ierr := importerRefusal(ctx, c, role); ierr != nil {
		_ = c.Close(context.WithoutCancel(ctx))
		return nil, transfer.Errorf(transfer.CodeSessionLost, true, "the check of the role could not be made now; repeat the operation")
	} else if why != "" {
		_ = c.Close(context.WithoutCancel(ctx))
		return nil, transfer.Errorf(transfer.CodeForbidden, false, "%s", importerText)
	}
	for _, s := range importSession {
		if _, err := c.Exec(ctx, s); err != nil {
			_ = c.Close(context.WithoutCancel(ctx))
			return nil, transfer.SafeError(err)
		}
	}
	return c, nil
}

// execOne runs the text of one statement through the extended protocol: the server refuses text that holds several statements (42601), the second line behind
// the grammar. The result is read to its end.
func execOne(ctx context.Context, conn *pgx.Conn, text string) error {
	rr := conn.PgConn().ExecParams(ctx, text, nil, nil, nil, nil)
	_, err := rr.Close()
	return err
}

// sqlEntry finds the fact and the checksum of an SQL entry of the archive.
func (a *archiveFile) entry(name string) (archive.EntryFact, string, bool) {
	for _, f := range a.st.res.Entries {
		if f.Name == name {
			for _, e := range a.st.res.Manifest.Entries {
				if e.Name == name {
					return f, e.SHA256, true
				}
			}
		}
	}
	return archive.EntryFact{}, "", false
}

// statementError makes the error of a task from the failure of a statement of the archive: its position and kind (never its text) and what SafeError says.
func statementError(st sqlshape.Statement, err error) error {
	te := transfer.SafeError(err)
	cp := *te
	cp.Sentence = fmt.Sprintf("the statement %s (%s) failed: %s", st.Position, st.Kind, te.Sentence)
	return &cp
}

// sqlFileOpts are what differs between the three files.
type sqlFileOpts struct {
	// skipSchemas are the schemas that the control plane made for the extensions: their CREATE SCHEMA and COMMENT ON SCHEMA are not run.
	skipSchemas map[string]bool
	// onError may handle a failed statement; it returns nil when it did, or the error to end with.
	onError func(ctx context.Context, conn *pgx.Conn, st sqlshape.Statement, text string, err error) error
	// note is called with the number of statements run so far, now and then.
	note func(n int)
}

// runSQLFile runs one SQL file of the archive as the role: every statement that the shape grammar accepts completely, one per round trip. The first refusal
// of the grammar or failure of the server ends it. It returns the number of statements run.
func (o *Ops) runSQLFile(ctx context.Context, conn *pgx.Conn, a *archiveFile, name string, opts sqlFileOpts) (int, error) {
	f, sum, ok := a.entry(name)
	if !ok {
		return 0, transfer.Errorf(transfer.CodeArchiveInvalid, false, "the archive has no %s", name)
	}
	er := archive.OpenEntry(a.r, f, sum)
	defer func() { _ = er.Close() }()
	hints := sqlshape.Hints{InheritanceChildren: catalogInheritance(&a.st.res.Catalog)}
	n := 0
	err := sqlshape.Each(er, name, hints, func(st sqlshape.Statement, text string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if (st.Kind == "CREATE SCHEMA" || (st.Kind == "COMMENT" && st.Comment == "SCHEMA")) && opts.skipSchemas[st.Name] {
			return nil
		}
		if err := execOne(ctx, conn, text); err != nil {
			if opts.onError != nil {
				return opts.onError(ctx, conn, st, text, err)
			}
			return statementError(st, err)
		}
		n++
		if opts.note != nil && n%200 == 0 {
			opts.note(n)
		}
		return nil
	})
	if err != nil {
		var re *sqlshape.RefusedError
		var te *transfer.Error
		switch {
		case errors.As(err, &te):
			return n, err
		case errors.As(err, &re):
			return n, refusedError([]sqlshape.Refusal{re.Refusal})
		case errors.Is(err, archive.ErrEntryChanged):
			return n, transfer.Errorf(transfer.CodeArchiveInvalid, false, "%s is not the file that was validated", name)
		case ctx.Err() != nil:
			return n, ctx.Err()
		}
		return n, transfer.Errorf(transfer.CodeArchiveInvalid, false, "%s could not be read as statements", name)
	}
	return n, nil
}

// extSkipSchemas are the schemas of the extensions that the control plane made itself (not public, not pg_catalog).
func extSkipSchemas(m archive.Manifest) map[string]bool {
	out := map[string]bool{}
	for _, e := range m.Extensions {
		if e.Schema != "public" && e.Schema != "pg_catalog" && e.Name != "plpgsql" {
			out[e.Schema] = true
		}
	}
	return out
}

// ------------------------------------------------------------------ access of the importing role to the private database

// importAccessOp is the parameter of ApplyImportAccess.
type importAccessOp struct {
	Name  string `json:"name"`
	Role  string `json:"role"`
	Allow bool   `json:"allow"`
	// Full is set on the server that holds the coordinator: the role also gets CREATE on the database and on the schema public there.
	Full bool `json:"full,omitempty"`
}

// importAccessLocal gives the importing role access to the private database of this server or takes it away. Taking it away withdraws CONNECT and ends the
// sessions of the role in that database: the role cannot change an object between a check of the control plane and the call of the service role that uses it.
func (o *Ops) importAccessLocal(ctx context.Context, op importAccessOp) error {
	if !strings.HasPrefix(op.Name, privatePrefix) || !dbNameRE.MatchString(op.Name) {
		return errors.New("this database cannot be changed")
	}
	db, err := sqlx.NewIdent(op.Name)
	if err != nil {
		return err
	}
	role, err := sqlx.NewIdent(op.Role)
	if err != nil {
		return err
	}
	c, err := o.Service.ServiceConn(ctx, "postgres")
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	if op.Allow {
		priv := "CONNECT"
		if op.Full {
			priv = "CONNECT, CREATE, TEMPORARY"
		}
		if err := run(ctx, c, stmt().Text("GRANT").Text(sqlx.Fixed(priv)).Text("ON DATABASE").ID(db).Text("TO").ID(role)); err != nil {
			return err
		}
		// The role needs CREATE on the schema public on every server: the distribution makes the tables there with the owner of the table, and the extension
		// refuses an owner who may create in the schema on one server and not on another.
		d, err := o.Service.ServiceConn(ctx, op.Name)
		if err != nil {
			return err
		}
		defer func() { _ = d.Close(context.WithoutCancel(ctx)) }()
		return run(ctx, d, stmt().Text("GRANT USAGE, CREATE ON SCHEMA public TO").ID(role))
	}
	return o.withdrawLocal(ctx, c, op.Name, db, role)
}

// importAccess gives the importing role access to the private database of this server and of every other, or takes it away.
func (o *Ops) importAccess(ctx context.Context, env *task.Env, p dbImportParams, priv string, allow bool) error {
	op := importAccessOp{Name: priv, Role: p.Owner.Name, Allow: allow, Full: true}
	if err := o.importAccessLocal(ctx, op); err != nil {
		return transfer.SafeError(err)
	}
	op.Full = false
	for _, s := range p.Servers {
		s := s
		if o.Fanout == nil {
			break
		}
		if err := o.onServer(ctx, env, s, "change the access of the role", func() error { return o.Fanout.Apply(ctx, s, ApplyImportAccess, op) }); err != nil {
			return err
		}
	}
	return nil
}

// ------------------------------------------------------------------ the tables, the views, the data and the constraints

// openForRole is the common start of the steps that run as the role: the record, the archive, the access of the role, its connection.
func (o *Ops) openForRole(ctx context.Context, env *task.Env) (p dbImportParams, a *archiveFile, conn *pgx.Conn, priv string, err error) {
	var rec transfer.Record
	var x *xferState
	p, rec, x, err = o.loadImport(ctx, env)
	if err != nil {
		return
	}
	if err = o.refuseDangerousImporter(ctx, p.Owner.Name); err != nil {
		return
	}
	if a, err = o.openImport(ctx, x, rec, p, env.Task.ID); err != nil {
		return
	}
	priv = p.private(env.Task.ID)
	if err = o.importAccess(ctx, env, p, priv, true); err != nil {
		a.close()
		return
	}
	if conn, err = o.importRoleConn(ctx, priv, p.Owner.Name); err != nil {
		a.close()
		return
	}
	return
}

func (o *Ops) stepIMTables(ctx context.Context, env *task.Env) error {
	_, a, conn, _, err := o.openForRole(ctx, env)
	if err != nil {
		return err
	}
	defer a.close()
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	n, err := o.runSQLFile(ctx, conn, a, archive.PreDataName, sqlFileOpts{skipSchemas: extSkipSchemas(a.st.res.Manifest),
		note: func(n int) { _ = env.SetDetail(ctx, fmt.Sprintf("%d statements", n)) }})
	if err != nil {
		return err
	}
	return env.SetDetail(ctx, fmt.Sprintf("%d statements", n))
}

func (o *Ops) stepIMViews(ctx context.Context, env *task.Env) error {
	_, a, conn, _, err := o.openForRole(ctx, env)
	if err != nil {
		return err
	}
	defer a.close()
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	n, err := o.runSQLFile(ctx, conn, a, archive.ViewsName, sqlFileOpts{skipSchemas: extSkipSchemas(a.st.res.Manifest)})
	if err != nil {
		return err
	}
	return env.SetDetail(ctx, fmt.Sprintf("%d statements", n))
}

// progressNote reports the bytes read through the detail of the task.
type countReader struct {
	r    io.Reader
	n    *int64
	note func()
	next int64
}

func (c *countReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	*c.n += int64(n)
	if *c.n >= c.next {
		c.next = *c.n + 16<<20
		c.note()
	}
	return n, err
}

// dataEntriesOf are the data entries of the archive in the order of the manifest, without the histories of the ledger tables (they are exported for the
// record; the history table of a ledger table is created empty by ledger_enable and nothing may write to it).
func dataEntriesOf(m archive.Manifest) []archive.ManifestEntry {
	var out []archive.ManifestEntry
	for _, e := range m.Entries {
		if _, ok := archive.IsData(e.Name); ok && !e.LedgerHistory {
			out = append(out, e)
		}
	}
	return out
}

func splitQualified(q string) (schema, table string, ok bool) {
	i := strings.Index(q, ".")
	if i <= 0 || i == len(q)-1 || strings.Contains(q[i+1:], ".") {
		return "", "", false
	}
	return q[:i], q[i+1:], true
}

func (o *Ops) stepIMData(ctx context.Context, env *task.Env) error {
	p, a, conn, _, err := o.openForRole(ctx, env)
	if err != nil {
		return err
	}
	defer a.close()
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	res := a.st.res
	byTable := map[string]archive.CatalogTable{}
	for _, ct := range res.Catalog.Tables {
		byTable[ct.Qualified()] = ct
	}
	entries := dataEntriesOf(res.Manifest)
	facts := map[string]archive.EntryFact{}
	for _, f := range res.Entries {
		facts[f.Name] = f
	}
	var total int64
	for i, me := range entries {
		ct, ok := byTable[me.Table]
		if !ok {
			return transfer.Errorf(transfer.CodeArchiveInvalid, false, "a data entry is for a table that the catalog does not list")
		}
		left, terr := o.timeLeft(env.Task)
		if terr != nil {
			return terr
		}
		if err := o.roleChecker().Check(ctx, p.Owner); err != nil {
			return err
		}
		fact := facts[me.Name]
		var read int64
		note := func() {
			_ = env.SetDetail(ctx, fmt.Sprintf("%s (%d of %d), %d bytes so far", me.Table, i+1, len(entries), total+read))
		}
		note()
		rows, err := o.loadTable(ctx, conn, env, a, p, ct, me, fact, left, &read, note)
		if err != nil {
			return err
		}
		total += read
		if rows != me.Rows {
			return transfer.Errorf(transfer.CodeRowCountMismatch, false, "the database loaded %d rows into %s and the manifest says %d", rows, safeName(me.Table), me.Rows)
		}
	}
	return env.SetDetail(ctx, fmt.Sprintf("%d tables, %d bytes", len(entries), total))
}

// loadTable loads one data entry into its table in one transaction of the role: the session formats of the archive, COPY (or the insert of a reference table),
// the count of the records of the scanner against the rows of the server and against the manifest, and then the commit.
func (o *Ops) loadTable(ctx context.Context, conn *pgx.Conn, env *task.Env, a *archiveFile, p dbImportParams, ct archive.CatalogTable, me archive.ManifestEntry,
	fact archive.EntryFact, left time.Duration, read *int64, note func()) (int64, error) {
	if ct.Kind == "reference" && me.Size > MaxAtomicReferenceBytes {
		return 0, transfer.Errorf(transfer.CodeImportTooLarge, false, "the reference table %s is larger than %d bytes, which is the most that one transaction takes", safeName(me.Table), MaxAtomicReferenceBytes)
	}
	er := archive.OpenEntry(a.r, fact, me.SHA256)
	defer func() { _ = er.Close() }()
	csv := transfer.CSV{Delimiter: ",", Quote: `"`, NullString: `\N`, Encoding: "UTF8"}.Fill()
	sc := transfer.NewScanner(&countReader{r: er, n: read, note: note, next: 16 << 20}, csv.QuoteByte())
	schema, table, ok := splitQualified(me.Table)
	if !ok {
		return 0, transfer.Errorf(transfer.CodeArchiveInvalid, false, "a table name is not valid")
	}
	// A reference table is written from typed arrays: the check of its columns is made before the transaction (it has a transaction of its own).
	var refCols, refTypes []string
	if ct.Kind == "reference" {
		var terr error
		if refCols, refTypes, terr = o.importTarget(ctx, conn, schema, table, me.Columns, "reference"); terr != nil {
			return 0, transfer.Errorf(transfer.CodeTableNotEligible, false, "the reference table %s cannot take the load", safeName(me.Table))
		}
	}
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, transfer.SafeError(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	formats, err := transfer.SessionFormats("UTF8")
	if err != nil {
		return 0, err
	}
	ms := left.Milliseconds()
	for _, s := range append(formats, fmt.Sprintf("SET LOCAL statement_timeout = %d", ms), fmt.Sprintf("SET LOCAL idle_in_transaction_session_timeout = %d", ms)) {
		if _, err := tx.Exec(ctx, s); err != nil {
			return 0, transfer.SafeError(err)
		}
	}
	var rows int64
	if ct.Kind == "reference" {
		ip := importParams{Schema: schema, Table: table, Columns: refCols, ColumnTypes: refTypes, CSV: csv, Header: transfer.HeaderSkip, SkipBOM: false, Reference: true}
		rows, err = o.loadReference(ctx, tx, ip, sc, sc)
	} else {
		sql, serr := transfer.ArchiveCopyFromSQL(schema, table, me.Columns)
		if serr != nil {
			return 0, transfer.Errorf(transfer.CodeArchiveInvalid, false, "a table or a column name is not valid")
		}
		var tag pgconn.CommandTag
		tag, err = tx.Conn().PgConn().CopyFrom(ctx, sc, sql)
		rows = tag.RowsAffected()
	}
	if err != nil {
		return 0, o.dataError(ctx, env.Task, sc, err, schema, table)
	}
	// The archive's CSV has the header record and one record for each row.
	want := sc.Records() - 1
	if sc.Records() == 0 {
		want = 0
	}
	if rows != want {
		return 0, transfer.Errorf(transfer.CodeRowCountMismatch, false, "the database loaded %d rows into %s and the file has %d records", rows, safeName(me.Table), want)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, transfer.SafeError(err)
	}
	return rows, nil
}

// dataError maps what the load of one entry ended with: the scanner's refusals, a changed entry, and anything else through SafeError.
func (o *Ops) dataError(ctx context.Context, t task.Task, sc *transfer.Scanner, err error, schema, table string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var em *transfer.ErrEndMarker
	var rl *transfer.ErrRecordTooLarge
	switch {
	case errors.As(err, &em), errors.As(sc.Err(), &em):
		return &transfer.Error{Code: transfer.CodeImportEndMarker, Sentence: "a data entry has a line that is only backslash and dot outside quotes", Line: int(em.Line)}
	case errors.As(err, &rl), errors.As(sc.Err(), &rl):
		return &transfer.Error{Code: transfer.CodeRecordTooLarge, Sentence: "a record of a data entry is larger than 16 MiB", Line: int(rl.Line)}
	case errors.Is(err, archive.ErrEntryChanged), errors.Is(sc.Err(), archive.ErrEntryChanged):
		return transfer.Errorf(transfer.CodeArchiveInvalid, false, "a data entry is not the file that was validated")
	}
	te := transfer.SafeError(err)
	if te.SQLState == "57014" {
		if _, terr := o.timeLeft(t); terr != nil {
			return terr
		}
	}
	return te.ForTarget(schema, table)
}

// foreign keys ------------------------------------------------------------

// fkViolation is a foreign key that the loaded rows do not satisfy.
type fkViolation struct {
	Constraint string
	Table      string
	Count      int64
}

func (v fkViolation) String() string {
	return fmt.Sprintf("%s on %s: %d rows violate it", v.Constraint, v.Table, v.Count)
}

// fkViolations counts the rows that violate a foreign key (match simple) from the catalog, as the role. The names come from the catalog and are quoted by it.
func fkViolations(ctx context.Context, conn *pgx.Conn, constraint, table string) (int64, error) {
	var child, parent string
	var ccols, pcols []string
	err := conn.QueryRow(ctx, `SELECT c.conrelid::pg_catalog.regclass::pg_catalog.text, c.confrelid::pg_catalog.regclass::pg_catalog.text,
			(SELECT pg_catalog.array_agg(pg_catalog.quote_ident(a.attname::pg_catalog.text) ORDER BY k.ord) FROM pg_catalog.unnest(c.conkey) WITH ORDINALITY k(attnum, ord) JOIN pg_catalog.pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum),
			(SELECT pg_catalog.array_agg(pg_catalog.quote_ident(a.attname::pg_catalog.text) ORDER BY k.ord) FROM pg_catalog.unnest(c.confkey) WITH ORDINALITY k(attnum, ord) JOIN pg_catalog.pg_attribute a ON a.attrelid = c.confrelid AND a.attnum = k.attnum)
		FROM pg_catalog.pg_constraint c WHERE c.contype = 'f' AND c.conname = $1::pg_catalog.text::pg_catalog.name AND c.conrelid = $2::pg_catalog.text::pg_catalog.regclass`, constraint, table).Scan(&child, &parent, &ccols, &pcols)
	if err != nil {
		return 0, err
	}
	if len(ccols) == 0 || len(ccols) != len(pcols) {
		return 0, errors.New("the foreign key could not be read")
	}
	var notNull, eq []string
	for i := range ccols {
		notNull = append(notNull, "c."+ccols[i]+" IS NOT NULL")
		eq = append(eq, "p."+pcols[i]+" OPERATOR(pg_catalog.=) c."+ccols[i])
	}
	var n int64
	q := "SELECT pg_catalog.count(*) FROM " + child + " c WHERE " + strings.Join(notNull, " AND ") + " AND NOT EXISTS (SELECT 1 FROM " + parent + " p WHERE " + strings.Join(eq, " AND ") + ")"
	err = conn.QueryRow(ctx, q).Scan(&n)
	return n, err
}

func (o *Ops) stepIMPost(ctx context.Context, env *task.Env) error {
	p, a, conn, _, err := o.openForRole(ctx, env)
	if err != nil {
		return err
	}
	defer a.close()
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	var violations []fkViolation
	opts := sqlFileOpts{skipSchemas: extSkipSchemas(a.st.res.Manifest)}
	opts.onError = func(ctx context.Context, c *pgx.Conn, st sqlshape.Statement, text string, err error) error {
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "23503" || st.Kind != "ALTER TABLE ADD CONSTRAINT" {
			return statementError(st, err)
		}
		// A foreign key that the loaded rows violate (the archive was read at different instants, there is no global snapshot). The statement is run again
		// with NOT VALID, a suffix that the control plane adds, to count the violations; with fail the work is rolled back and the task fails.
		if !p.keepNotValid() {
			cnt, cerr := o.countByRollback(ctx, c, text, pe.ConstraintName, pe.TableName, pe.SchemaName)
			if cerr != nil {
				return statementError(st, err)
			}
			return &transfer.Error{Code: transfer.CodeForeignKey, Sentence: fmt.Sprintf("%d rows violate a foreign key", cnt), Constraint: safeNameText(pe.ConstraintName),
				Schema: safeNameText(pe.SchemaName), Table: safeNameText(pe.TableName)}
		}
		if err2 := execOne(ctx, c, notValidText(text)); err2 != nil {
			return statementError(st, err2)
		}
		cnt, cerr := fkViolations(ctx, c, pe.ConstraintName, qualifiedText(pe.SchemaName, pe.TableName))
		if cerr != nil {
			return statementError(st, cerr)
		}
		violations = append(violations, fkViolation{Constraint: safeNameText(pe.ConstraintName), Table: safeNameText(qualifiedText(pe.SchemaName, pe.TableName)), Count: cnt})
		return nil
	}
	opts.note = func(n int) { _ = env.SetDetail(ctx, fmt.Sprintf("%d statements", n)) }
	n, err := o.runSQLFile(ctx, conn, a, archive.PostDataName, opts)
	if err != nil {
		return err
	}
	// The materialized views were made WITH NO DATA; they are filled last, as the role, when everything they read is there.
	for _, mv := range a.st.plan.MatViews {
		schema, name, ok := splitQualified(mv)
		if !ok {
			return transfer.Errorf(transfer.CodeArchiveInvalid, false, "a materialized view has a name that is not understood")
		}
		id, ierr := sqlx.QualifiedIdent(schema, name)
		if ierr != nil {
			return transfer.Errorf(transfer.CodeArchiveInvalid, false, "a materialized view has a name that is not valid")
		}
		if err := execOne(ctx, conn, "REFRESH MATERIALIZED VIEW "+id.String()); err != nil {
			return transfer.SafeError(err).ForTarget(schema, name)
		}
	}
	detail := fmt.Sprintf("%d statements", n)
	if len(violations) > 0 {
		var parts []string
		for _, v := range violations {
			parts = append(parts, v.String())
		}
		detail += "\nNOT VALID: " + strings.Join(parts, "; ")
	}
	return env.SetDetail(ctx, detail)
}

// notValidText is the statement with NOT VALID at its end: the control plane's suffix, on a line of its own so that a comment that ends the text of the archive
// cannot swallow it. A statement that is NOT VALID already is returned as it is.
func notValidText(text string) string {
	if sqlshape.EndsWithNotValid(text) {
		return text
	}
	return text + "\n NOT VALID"
}

func qualifiedText(schema, table string) string {
	id, err := sqlx.QualifiedIdent(schema, table)
	if err != nil {
		return ""
	}
	return id.String()
}

func safeNameText(s string) string { return safeName(s) }

// countByRollback runs the statement with NOT VALID in a transaction, counts the violations and rolls the transaction back.
func (o *Ops) countByRollback(ctx context.Context, c *pgx.Conn, text, constraint, table, schema string) (int64, error) {
	tx, err := c.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := execOne(ctx, c, notValidText(text)); err != nil {
		return 0, err
	}
	return fkViolations(ctx, c, constraint, qualifiedText(schema, table))
}

// stringsOf is a helper for messages.
func itoa(n int) string { return strconv.Itoa(n) }

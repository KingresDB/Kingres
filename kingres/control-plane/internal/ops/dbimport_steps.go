// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/archive"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extguard"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sqlx"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/transfer"
)

// ------------------------------------------------------------------ step 1: check the archive

// extensionSchemas maps the extensions of the manifest to the schema they were installed in.
func extensionSchemas(m archive.Manifest) map[string]string {
	out := map[string]string{}
	for _, e := range m.Extensions {
		out[e.Name] = e.Schema
	}
	return out
}

func hasExt(m archive.Manifest, name string) bool {
	for _, e := range m.Extensions {
		if e.Name == name {
			return true
		}
	}
	return false
}

func (o *Ops) stepIMCheck(ctx context.Context, env *task.Env) error {
	p, rec, x, err := o.loadImport(ctx, env)
	if err != nil {
		return err
	}
	if err := o.stillEnabled(ctx); err != nil {
		return err
	}
	if err := o.refuseDangerousImporter(ctx, p.Owner.Name); err != nil {
		return err
	}
	// The first step always reads the archive again: a state left in this process by an earlier run of the same task is dropped.
	o.setImportState(env.Task.ID, nil)
	a, err := o.openImport(ctx, x, rec, p, env.Task.ID)
	if err != nil {
		return err
	}
	defer a.close()
	res, plan := a.st.res, a.st.plan
	// Versions, locale and extensions: the importer creates the database with the archive's encoding and locale (libc) and installs the extensions from the closed list.
	tgt, err := o.target(ctx)
	if err != nil {
		return transfer.Errorf(transfer.CodeSessionLost, true, "the service role cannot connect to this cluster now; repeat the operation")
	}
	tgt.Provider = "libc"
	tgt.SupportsDistributedSchemas = true
	reasons := append(archive.CheckCompatibility(res.Manifest, tgt), archive.CheckCatalogCompatibility(res.Catalog, tgt)...)
	if len(reasons) > 0 {
		var codes []string
		for i, r := range reasons {
			if i >= maxImportReasons {
				break
			}
			codes = append(codes, r.Code)
		}
		return transfer.Errorf(transfer.CodeArchiveIncompatible, false, "the archive does not fit this cluster (%s)", strings.Join(codes, ", "))
	}
	if res.Manifest.Totals.UncompressedBytes > o.maxImport() {
		return transfer.Errorf(transfer.CodeImportTooLarge, false, "the archive holds %d bytes and an import takes at most %d", res.Manifest.Totals.UncompressedBytes, o.maxImport())
	}
	for _, ct := range res.Catalog.Tables {
		if ct.Ledger != nil && !p.AckLedger {
			return transfer.Errorf(transfer.CodeLedgerRestart, false, "the archive has ledger tables; an import starts a new chain for them: set acknowledgeLedgerRestart to accept it")
		}
	}
	// The statements and the catalog must agree on which tables exist.
	cat := map[string]bool{}
	for _, ct := range res.Catalog.Tables {
		cat[ct.Qualified()] = true
	}
	// The user's partitions of a distributed table are written by the exporter as tables of the parent's schema; the catalog names them in the parent.
	userPart := map[string]bool{}
	for _, ct := range res.Catalog.Tables {
		if ct.UserPartitioning != nil {
			for _, up := range ct.UserPartitioning.Partitions {
				userPart[ct.Schema+"."+up.Name] = true
			}
		}
	}
	for n := range plan.Tables {
		if !cat[n] && !userPart[n] {
			return transfer.Errorf(transfer.CodeArchiveInvalid, false, "the SQL files create a table that the catalog does not list (%s)", safeName(n))
		}
	}
	for n := range cat {
		if !plan.Tables[n] {
			return transfer.Errorf(transfer.CodeArchiveInvalid, false, "the catalog lists a table that the SQL files do not create (%s)", safeName(n))
		}
	}
	if plan.Statements > sqlshapeMaxTotal {
		return transfer.Errorf(transfer.CodeImportTooLarge, false, "the SQL files hold more statements than an import takes")
	}
	// The final name must still be free (it is checked again when it is taken).
	var exists bool
	if err := o.asService(ctx, "postgres", func(tx pgx.Tx) error {
		var e error
		exists, e = dbExists(ctx, tx, p.Database)
		return e
	}); err != nil {
		return transfer.Errorf(transfer.CodeSessionLost, true, "the service role cannot connect to this cluster now; repeat the operation")
	}
	if exists {
		return transfer.Errorf(transfer.CodeForbidden, false, "the database %s exists", p.Database)
	}
	return env.SetDetail(ctx, fmt.Sprintf("%d tables, %d statements, %d bytes", len(res.Catalog.Tables), plan.Statements, res.Manifest.Totals.UncompressedBytes))
}

// sqlshapeMaxTotal bounds the statements of the three files together (each file is bounded by the grammar).
const sqlshapeMaxTotal = 3 * 200000

// ------------------------------------------------------------------ step 2: the private database

// importCreateOp is what a server needs to make the private database (the kind ApplyImportCreate, parameters that are structured values).
type importCreateOp struct {
	Name string `json:"name"`
	// Marker is written as the comment of the database when it is made (kingres-import:<task id>): the compensation of the task drops a database only when it
	// is owned by the service role and carries this marker, so a task never drops a database that it did not make.
	Marker    string `json:"marker,omitempty"`
	Encoding  string `json:"encoding"`
	LcCollate string `json:"lcCollate,omitempty"`
	LcCtype   string `json:"lcCtype,omitempty"`
	// Role is the importing role, which is granted USAGE on the schemas of the extensions.
	Role       string          `json:"role,omitempty"`
	Schemas    []string        `json:"schemas,omitempty"`
	Extensions []importExtSpec `json:"extensions,omitempty"`
}

type importExtSpec struct {
	Name   string `json:"name"`
	Schema string `json:"schema"`
}

var errDatabaseExists = errors.New("the database exists")

// encodingRE is the encodings the archive may name; every one is a name PostgreSQL knows, so the pattern is only a bound.
var encodingRE = regexp.MustCompile(`^[A-Za-z0-9_]{1,32}$`)

// createPrivateLocal makes the private database on this server. A database that exists is a FAILURE (the name is the task's own, so nobody else made it, and a
// success would be the start of a database that this task did not make). The database is owned by the service role, and PUBLIC loses CONNECT at once: nobody
// but the service role can connect until the step that needs the importing role gives it access.
func (o *Ops) createPrivateLocal(ctx context.Context, op importCreateOp) error {
	if !strings.HasPrefix(op.Name, privatePrefix) || !dbNameRE.MatchString(op.Name) || protectedDB[op.Name] {
		return errors.New("this database name cannot be created")
	}
	if !encodingRE.MatchString(op.Encoding) || (op.LcCollate != "" && !localeRE.MatchString(op.LcCollate)) || (op.LcCtype != "" && !localeRE.MatchString(op.LcCtype)) {
		return errors.New("the encoding or the locale is not valid")
	}
	c, err := o.Service.ServiceConn(ctx, "postgres")
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	if ok, err := dbExists(ctx, c, op.Name); err != nil {
		return err
	} else if ok {
		return errDatabaseExists
	}
	b := stmt().Text("CREATE DATABASE").ID(sqlx.MustIdent(op.Name)).Text("TEMPLATE template0 ENCODING").Lit(op.Encoding)
	if op.LcCollate != "" {
		b.Text("LC_COLLATE").Lit(op.LcCollate)
	}
	if op.LcCtype != "" {
		b.Text("LC_CTYPE").Lit(op.LcCtype)
	}
	b.Text("CONNECTION LIMIT -1")
	if err := run(ctx, c, b); err != nil {
		return err
	}
	if err := run(ctx, c, stmt().Text("REVOKE ALL ON DATABASE").ID(sqlx.MustIdent(op.Name)).Text("FROM PUBLIC")); err != nil {
		return err
	}
	if op.Marker != "" {
		return run(ctx, c, stmt().Text("COMMENT ON DATABASE").ID(sqlx.MustIdent(op.Name)).Text("IS").Lit(op.Marker))
	}
	return nil
}

// setupPrivateLocal installs, in the private database of this server, the schemas and the extensions of the closed list (never kingres_shard: it is installed
// by the step that registers the nodes), as the service role, and gives the importing role USAGE on their schemas.
func (o *Ops) setupPrivateLocal(ctx context.Context, op importCreateOp) error {
	if !strings.HasPrefix(op.Name, privatePrefix) || !dbNameRE.MatchString(op.Name) {
		return errors.New("this database cannot be set up")
	}
	if len(op.Extensions) == 0 && len(op.Schemas) == 0 {
		return nil
	}
	// The whole request is checked before a connection is used: a schema or an extension that is not allowed is refused whatever else is in it.
	for _, s := range op.Schemas {
		if _, err := sqlx.NewIdent(s); err != nil || s == "public" || strings.HasPrefix(s, "pg_") || s == "information_schema" {
			return errors.New("a schema of an extension is not valid")
		}
	}
	for _, e := range op.Extensions {
		if !archive.AllowedExtension(e.Name) || e.Name == extguard.Shard || e.Name == "plpgsql" {
			return errors.New("this extension cannot be installed by an import")
		}
		if _, err := sqlx.NewIdent(e.Name); err != nil {
			return errors.New("this extension cannot be installed by an import")
		}
		if _, err := sqlx.NewIdent(e.Schema); err != nil {
			return errors.New("a schema of an extension is not valid")
		}
	}
	c, err := o.Service.ServiceConn(ctx, op.Name)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	if _, err := c.Exec(ctx, "SET search_path = pg_catalog"); err != nil {
		return err
	}
	var role sqlx.Ident
	if op.Role != "" {
		if role, err = sqlx.NewIdent(op.Role); err != nil {
			return err
		}
	}
	grant := func(schema string, create bool) error {
		if op.Role == "" {
			return nil
		}
		what := sqlx.Fixed("USAGE")
		if create {
			what = "USAGE, CREATE"
		}
		return run(ctx, c, stmt().Text("GRANT").Text(what).Text("ON SCHEMA").ID(sqlx.MustIdent(schema)).Text("TO").ID(role))
	}
	for _, s := range op.Schemas {
		id, err := sqlx.NewIdent(s)
		if err != nil || s == "public" || strings.HasPrefix(s, "pg_") || s == "information_schema" {
			return errors.New("a schema of an extension is not valid")
		}
		if err := run(ctx, c, stmt().Text("CREATE SCHEMA IF NOT EXISTS").ID(id)); err != nil {
			return err
		}
		// A schema that the archive's extension lives in may hold tables of the database too.
		if err := grant(s, true); err != nil {
			return err
		}
	}
	for _, e := range op.Extensions {
		if !archive.AllowedExtension(e.Name) || e.Name == extguard.Shard || e.Name == "plpgsql" {
			return errors.New("this extension cannot be installed by an import")
		}
		id, err := sqlx.NewIdent(e.Name)
		if err != nil {
			return err
		}
		b := stmt().Text("CREATE EXTENSION IF NOT EXISTS").ID(id)
		if !strings.HasPrefix(e.Name, "kingres_") {
			sid, err := sqlx.NewIdent(e.Schema)
			if err != nil {
				return err
			}
			b.Text("WITH SCHEMA").ID(sid)
		}
		if err := run(ctx, c, b); err != nil {
			return fmt.Errorf("installing %s: %w", e.Name, err)
		}
	}
	// The schema that a KingresDB extension makes (kingres) is used by the role's own statements only through names that the grammar refuses, but the role must
	// see it to use the types and the access method of the extensions.
	if hasInstalled(op.Extensions, "kingres_") {
		if err := grant("kingres", false); err != nil {
			return err
		}
	}
	return nil
}

func hasInstalled(l []importExtSpec, prefix string) bool {
	for _, e := range l {
		if strings.HasPrefix(e.Name, prefix) {
			return true
		}
	}
	return false
}

// importMarker is the comment that marks a private database as the one that a task made: its name already says which task it is.
func importMarker(priv string) string {
	return "kingres-import:" + strings.TrimPrefix(priv, privatePrefix)
}

// createOp builds the parameters of the creation from the manifest: the locale of the source, the schemas and the extensions other than the sharding one.
func importCreateOpOf(priv, role string, m archive.Manifest) importCreateOp {
	op := importCreateOp{Name: priv, Marker: importMarker(priv), Encoding: m.Source.Encoding, LcCollate: m.Source.LcCollate, LcCtype: m.Source.LcCtype, Role: role}
	seen := map[string]bool{}
	for _, e := range m.Extensions {
		if e.Name == extguard.Shard || e.Name == "plpgsql" {
			continue
		}
		op.Extensions = append(op.Extensions, importExtSpec{Name: e.Name, Schema: e.Schema})
		if e.Schema != "public" && e.Schema != "pg_catalog" && !strings.HasPrefix(e.Name, "kingres_") && !seen[e.Schema] {
			seen[e.Schema] = true
			op.Schemas = append(op.Schemas, e.Schema)
		}
	}
	// A KingresDB extension is installed in the order the manifest lists them (the ledger needs nothing of the others).
	sort.SliceStable(op.Extensions, func(i, j int) bool { return false })
	return op
}

func (o *Ops) stepIMCreate(ctx context.Context, env *task.Env) error {
	p, rec, x, err := o.loadImport(ctx, env)
	if err != nil {
		return err
	}
	if err := o.refuseDangerousImporter(ctx, p.Owner.Name); err != nil {
		return err
	}
	a, err := o.openImport(ctx, x, rec, p, env.Task.ID)
	if err != nil {
		return err
	}
	defer a.close()
	priv := privateName(env.Task.ID)
	op := importCreateOpOf(priv, p.Owner.Name, a.st.res.Manifest)
	if err := o.createPrivateLocal(ctx, op); err != nil {
		if errors.Is(err, errDatabaseExists) {
			return transfer.Errorf(transfer.CodeForbidden, false, "the private database %s exists already; a task cannot use a database it did not make", priv)
		}
		return transfer.Errorf(transfer.CodeArchiveIncompatible, false, "this cluster cannot create a database with the encoding and the locale of the archive")
	}
	return env.SetDetail(ctx, priv)
}

// stepIMCreateOn makes the private database on another server: the same strict creation and the same extensions, run there by the service role of that server.
func (o *Ops) stepIMCreateOn(ctx context.Context, env *task.Env, server string) error {
	p, rec, x, err := o.loadImport(ctx, env)
	if err != nil {
		return err
	}
	a, err := o.openImport(ctx, x, rec, p, env.Task.ID)
	if err != nil {
		return err
	}
	defer a.close()
	op := importCreateOpOf(privateName(env.Task.ID), "", a.st.res.Manifest)
	if o.Fanout == nil {
		return nil
	}
	return o.Fanout.Apply(ctx, server, ApplyImportCreate, op)
}

// ------------------------------------------------------------------ step 3: the extensions

func (o *Ops) stepIMInstall(ctx context.Context, env *task.Env) error {
	p, rec, x, err := o.loadImport(ctx, env)
	if err != nil {
		return err
	}
	a, err := o.openImport(ctx, x, rec, p, env.Task.ID)
	if err != nil {
		return err
	}
	defer a.close()
	m := a.st.res.Manifest
	priv := privateName(env.Task.ID)
	sharded := hasExt(m, extguard.Shard)
	if sharded {
		if err := o.installShardLocal(ctx, priv); err != nil {
			return transfer.Errorf(transfer.CodeSQLError, false, "the sharding extension could not be installed in the private database")
		}
	}
	if err := o.setupPrivateLocal(ctx, importCreateOpOf(priv, p.Owner.Name, m)); err != nil {
		return transfer.SafeError(err)
	}
	if sharded {
		for _, s := range p.Servers {
			s := s
			if err := o.onServer(ctx, env, s, "register the server", func() error {
				if o.Fanout == nil {
					return nil
				}
				return o.Fanout.Register(ctx, priv, OtherServer{ID: s, PGPort: o.pgPortOf(ctx, s)})
			}); err != nil {
				return err
			}
		}
		if len(m.Extensions) > 0 && len(a.st.res.Catalog.DistributedSchemas) > 0 {
			if err := o.setShardingMode(ctx, priv, "both", nil); err != nil {
				return transfer.Errorf(transfer.CodeTableNotEligible, false, "the sharding mode of the private database could not be set for distributed schemas")
			}
		}
	}
	return env.SetDetail(ctx, fmt.Sprintf("%d extensions", len(m.Extensions)))
}

// installShardLocal installs kingres_shard in the private database and registers this server as its first node. The database is the task's own and nobody
// but the service role can connect to it yet.
func (o *Ops) installShardLocal(ctx context.Context, db string) error {
	c, err := o.Service.ServiceConn(ctx, db)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	if _, err := c.Exec(ctx, "SET search_path = pg_catalog"); err != nil {
		return err
	}
	if _, err := c.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS kingres_shard"); err != nil {
		return err
	}
	var trusted bool
	err = inTx(ctx, c, func(tx pgx.Tx) error {
		var e error
		trusted, e = shardTrusted(ctx, tx)
		return e
	})
	if err != nil || !trusted {
		return errors.New("the sharding extension is not trusted")
	}
	var nodes int
	if err := c.QueryRow(ctx, "SELECT count(*) FROM kingres.node").Scan(&nodes); err != nil {
		return err
	}
	if nodes == 0 {
		if _, err := c.Exec(ctx, "SELECT kingres.init_cluster($1, $2)", o.ServerHost, o.PGPort); err != nil {
			return fmt.Errorf("registering the node in the database: %w", err)
		}
	}
	return nil
}

// shardTrusted is shardUsable for a caller that has no facts: the sharding extension of the database is installed, owned by a superuser, and the relations of
// the schema kingres that are read are members of it (extguard cannot decide about it: the extension is created in pg_catalog).
func shardTrusted(ctx context.Context, tx pgx.Tx) (bool, error) {
	return (*dbFacts)(nil).shardUsable(ctx, tx)
}

// ------------------------------------------------------------------ step 4: the functions of the expressions

// denied are the builtin functions that an expression of an archive may not call even though the role may be allowed to: they read or write files, run
// programs or other statements, sleep, send signals, change settings or the state of the server, or take locks for a long time. The check is a list of
// prefixes and names; PostgreSQL's privileges still decide for the role.
var deniedPrefixes = []string{"pg_sleep", "pg_read_", "pg_ls_", "pg_stat_file", "pg_file_", "lo_", "pg_terminate_backend", "pg_cancel_backend", "pg_reload_conf",
	"pg_rotate_logfile", "pg_switch_wal", "pg_create_", "pg_drop_", "pg_logical_", "pg_replication_", "pg_promote", "pg_backup_", "pg_start_backup", "pg_stop_backup",
	"pg_wal_", "pg_advisory_", "pg_try_advisory_", "pg_notify", "pg_import_system_collations", "pg_stat_reset", "pg_log_backend_memory_contexts", "pg_export_snapshot",
	"pg_current_logfile", "pg_ident_file", "pg_hba_file", "pg_get_wal_", "set_config", "query_to_xml", "cursor_to_xml", "table_to_xml", "schema_to_xml", "database_to_xml",
	"ts_stat", "dblink", "pg_ls", "setval", "pg_catalog_", "pg_copy_", "pg_reload", "pg_cancel", "pg_terminate", "pg_set_", "pg_xact_", "pg_prewarm", "pg_database_size",
	"pg_conf_load_time", "pg_postmaster_start_time", "pg_stat_get_", "pg_stat_clear", "pg_replication_origin", "pg_sync_replication_slots", "pg_truncate_visibility_map",
	"pg_extension_config_dump", "pg_event_trigger", "binary_upgrade_", "pg_set_relation_stats", "pg_set_attribute_stats", "pg_clear_", "xmlexport"}

func deniedFunction(name string) bool {
	n := strings.ToLower(name)
	for _, p := range deniedPrefixes {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

// exprExtensions are the extensions whose functions an expression may call; the other extensions (the sharding, the ledger, the time partitioning and the
// columnar ones) are management interfaces, and their functions run with the right of the caller where the importing role has none.
var exprExtensions = map[string]bool{"pgcrypto": true, "uuid-ossp": true, "citext": true, "hstore": true, "pg_trgm": true, "btree_gin": true, "btree_gist": true,
	"ltree": true, "unaccent": true, "fuzzystrmatch": true, "kingres_sketch": true}

// functionVerdict decides about one function name of the plan from the candidates that pg_proc has. schema is "" for an unqualified name (resolved in
// pg_catalog, the search path of the role). extSchema maps an extension of the manifest to its schema. It returns "" when the call is allowed, else a reason.
func functionVerdict(schema, name string, cands []procCand, extSchema map[string]string) string {
	var in []procCand
	for _, c := range cands {
		if schema == "" && c.Schema == "pg_catalog" || schema != "" && c.Schema == schema {
			in = append(in, c)
		}
	}
	if len(in) == 0 {
		return "the function does not exist where the statements look for it"
	}
	for _, c := range in {
		switch {
		case c.Schema == "pg_catalog":
			if c.Ext != "" {
				return "the function belongs to an extension of KingresDB that an expression may not call"
			}
			if deniedFunction(name) {
				return "the function is not allowed in an expression"
			}
		default:
			if c.Ext == "" || !exprExtensions[c.Ext] || extSchema[c.Ext] != c.Schema {
				return "the function does not belong to an extension of the archive that an expression may call"
			}
			if deniedFunction(name) {
				return "the function is not allowed in an expression"
			}
		}
		if c.SecDef {
			return "the function runs with the rights of its owner"
		}
	}
	return ""
}

type procCand struct {
	Schema, Ext string
	SecDef      bool
}

// safeName bounds and cleans a name that came from an archive before it is put in a message.
func safeName(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n >= 48 {
			b.WriteString("...")
			break
		}
		if !(r == '_' || r == '.' || r == '$' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			r = '?'
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// checkFunctions looks every function name of the plan up in pg_proc of the private database, which holds the builtin functions and those of the installed
// extensions and nothing else (the importing role has not been able to connect yet), and returns the names that are not allowed with a fixed reason.
func checkFunctions(ctx context.Context, tx pgx.Tx, names []string, extSchema map[string]string) ([]string, error) {
	var refused []string
	for _, full := range names {
		parts := strings.Split(full, ".")
		if len(parts) > 2 || parts[len(parts)-1] == "" {
			refused = append(refused, safeName(full)+": a function name that is not understood")
			continue
		}
		schema, name := "", parts[len(parts)-1]
		if len(parts) == 2 {
			schema = parts[0]
		}
		rows, err := tx.Query(ctx, `SELECT n.nspname::text, coalesce((SELECT e.extname::text FROM pg_catalog.pg_depend d JOIN pg_catalog.pg_extension e ON e.oid = d.refobjid
				WHERE d.classid = 'pg_catalog.pg_proc'::pg_catalog.regclass AND d.objid = p.oid AND d.refclassid = 'pg_catalog.pg_extension'::pg_catalog.regclass AND d.deptype = 'e' LIMIT 1), ''),
				p.prosecdef FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace WHERE p.proname = $1::pg_catalog.text::pg_catalog.name`, name)
		if err != nil {
			return nil, err
		}
		var cands []procCand
		for rows.Next() {
			var c procCand
			if err := rows.Scan(&c.Schema, &c.Ext, &c.SecDef); err != nil {
				rows.Close()
				return nil, err
			}
			cands = append(cands, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if why := functionVerdict(schema, name, cands, extSchema); why != "" {
			refused = append(refused, safeName(full)+": "+why)
		}
	}
	return refused, nil
}

func (o *Ops) stepIMFunctions(ctx context.Context, env *task.Env) error {
	p, rec, x, err := o.loadImport(ctx, env)
	if err != nil {
		return err
	}
	a, err := o.openImport(ctx, x, rec, p, env.Task.ID)
	if err != nil {
		return err
	}
	defer a.close()
	priv := privateName(env.Task.ID)
	var refused []string
	if err := o.asService(ctx, priv, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL search_path = pg_catalog"); err != nil {
			return err
		}
		var e error
		refused, e = checkFunctions(ctx, tx, a.st.plan.Functions, extensionSchemas(a.st.res.Manifest))
		return e
	}); err != nil {
		return transfer.SafeError(err)
	}
	if len(refused) > 0 {
		if len(refused) > 5 {
			refused = append(refused[:5], fmt.Sprintf("and %d more", len(refused)-5))
		}
		return transfer.Errorf(transfer.CodeStatementRefused, false, "the archive calls functions that are not allowed in an expression: %s; nothing was run", strings.Join(refused, "; "))
	}
	return env.SetDetail(ctx, fmt.Sprintf("%d functions", len(a.st.plan.Functions)))
}

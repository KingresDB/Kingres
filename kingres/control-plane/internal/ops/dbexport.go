// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

// The task database.export (contract 0.40.0, design "Database export [14b]"): one zip archive of a database, staged on the leader, sealed and kept for the
// retention time. The structure is the schema-only dump of pg_dump, taken by the service role in the snapshot of one read-only transaction and turned into
// the logical schema by package dumpnorm; the catalog document says how the tables are distributed; the data of every table is read as the signed-in role
// through COPY (SELECT) and written to its own entry by the streaming writer of package archive. Before the file is sealed the exporter reads it back through
// the validator of the importer and compares the fingerprint of the structure with the one it started with.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/archive"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/dumpnorm"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sqlshape"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/staging"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/transfer"
)

// KindDatabaseExport and KindArchiveValidate are the task kinds of the database archive.
const (
	KindDatabaseExport  = "database.export"
	KindArchiveValidate = "archive.validate"
)

// Step names of database.export.
const (
	stepDCheck     = "Check the request"
	stepDStructure = "Take the structure"
	stepDNormalise = "Normalise the schema"
	stepDData      = "Write the data"
	stepDVerify    = "Check the archive"
	stepDSeal      = "Seal"
)

// tableKinds are the task kinds that change a table of a database and are refused while a database export or import runs, and that make a database export
// wait (design: the catalog, the schema and the data must not be taken at different structural states by the control plane's own operations).
var tableKinds = []string{"table.distribute", "table.undistribute", "table.alter", "table.partition", "table.import", "table.setStorage", "partition.maintain"}

type dbExportParams struct {
	TransferID int64             `json:"transferId"`
	Database   string            `json:"database"`
	Owner      transfer.Identity `json:"owner"`
	Retention  int               `json:"retentionHours"`
	MaxFile    int64             `json:"maxFileBytes"`
	Exclude    []string          `json:"excludeTables"`
	DataMode   string            `json:"dataMode"`
	AckLO      bool              `json:"acknowledgeLargeObjectsLost"`
}

func (p dbExportParams) excluded() map[string]bool {
	m := map[string]bool{}
	for _, q := range p.Exclude {
		m[q] = true
	}
	return m
}

// dbWork is what the steps of one export pass to each other in memory: the structure that was read and the schema files that were made. A step that does
// not find it (the process restarted) makes it again, which is safe: it is all read-only.
type dbWork struct {
	facts       *dbFacts
	fingerprint string
	rawPre      string
	rawPost     string
	norm        dumpnorm.Output
	cat         archive.Catalog
	manifest    archive.Manifest
	single      bool
	rows        map[string]int64
	entries     []dataEntry
}

type dbWorks struct {
	mu sync.Mutex
	m  map[int64]*dbWork
}

func (o *Ops) work(id int64) *dbWork {
	o.dbw.mu.Lock()
	defer o.dbw.mu.Unlock()
	return o.dbw.m[id]
}

func (o *Ops) setWork(id int64, w *dbWork) {
	o.dbw.mu.Lock()
	defer o.dbw.mu.Unlock()
	if o.dbw.m == nil {
		o.dbw.m = map[int64]*dbWork{}
	}
	if w == nil {
		delete(o.dbw.m, id)
		return
	}
	o.dbw.m[id] = w
}

// databaseTaskBusy refuses to start a database export while a table task of the database is unfinished (and the table tasks refuse while an export runs, in
// StartTask): one query on the task table.
func (o *Ops) databaseTaskBusy(ctx context.Context, db string) error {
	var kind string
	err := o.asService(ctx, "kingres_system", func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT kind FROM kingres_cp.task WHERE state IN ('pending', 'running', 'paused') AND kind = ANY($1) AND subject_type = 'table' AND subject_id LIKE $2 ESCAPE '\' LIMIT 1`,
			tableKinds, likeEscape(db)+"/%").Scan(&kind)
		if errors.Is(err, pgx.ErrNoRows) {
			kind = ""
			return nil
		}
		return err
	})
	if err != nil {
		return err
	}
	if kind != "" {
		return problem.New(problem.OperationInProgress, "A task of kind "+kind+" is unfinished in this database; wait for it, or cancel it.").WithRetryAfter(30)
	}
	return nil
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func tableTaskKind(kind string) bool {
	for _, k := range tableKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// exportRunning refuses a table task of a database in which a database export is unfinished.
func (o *Ops) exportRunning(ctx context.Context, subjType, subjID string) error {
	if subjType != "table" {
		return nil
	}
	db, _, ok := strings.Cut(subjID, "/")
	if !ok {
		return nil
	}
	found := false
	err := o.asService(ctx, "kingres_system", func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM kingres_cp.task WHERE kind = 'database.export' AND state IN ('pending', 'running', 'paused') AND subject_type = 'database' AND subject_id = $1)`, db).Scan(&found)
	})
	if err != nil {
		return err
	}
	if found {
		return problem.New(problem.OperationInProgress, "A database export is running for this database; table operations wait until it ends.").WithRetryAfter(30)
	}
	return nil
}

// roleReadable checks, as the signed-in role, that it can use every schema and read every table of the database (design step 1); the first 20 objects that it
// cannot are named. The list is read from the catalog through the connection of the role.
func (o *Ops) roleReadable(ctx context.Context, conn *pgx.Conn) ([]api.TableBlocker, int, error) {
	var out []api.TableBlocker
	total := 0
	err := inTx(ctx, conn, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT 'no_usage', n.nspname::text, 0 FROM pg_namespace n WHERE `+userSchemaFilter+` AND NOT has_schema_privilege(n.oid, 'USAGE')
			UNION ALL
			SELECT 'no_privilege', n.nspname::text || '.' || c.relname::text, 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE c.relkind IN ('r', 'p') AND `+userSchemaFilter+` AND NOT c.relispartition
			  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e')
			  AND NOT has_table_privilege(c.oid, 'SELECT')
			  AND (NOT EXISTS (SELECT 1 FROM pg_attribute a WHERE a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped)
			       OR EXISTS (SELECT 1 FROM pg_attribute a WHERE a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped AND a.attgenerated::text = '' AND NOT has_column_privilege(c.oid, a.attnum, 'SELECT')))
			ORDER BY 3, 2`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var code, obj string
			var ord int
			if err := rows.Scan(&code, &obj, &ord); err != nil {
				return err
			}
			total++
			if len(out) < 20 {
				out = append(out, blocker(code, obj, "The role cannot read this object."))
			}
		}
		return rows.Err()
	})
	return out, total, err
}

// StartDatabaseExport answers exportDatabase: it checks what the design lists for the first step, reserves the room, makes the record and starts the task.
func (o *Ops) StartDatabaseExport(ctx context.Context, actor, db string, req api.ExportDatabaseRequest) (task.Task, error) {
	x, err := o.xfer(ctx)
	if err != nil {
		return task.Task{}, err
	}
	if err := checkNames(db, "public", "t"); err != nil {
		return task.Task{}, problem.New(problem.NotFound, "No such database.")
	}
	st, err := o.settings(ctx)
	if err != nil {
		return task.Task{}, err
	}
	dataMode := req.DataMode
	var errs []problem.FieldError
	switch dataMode {
	case "":
		dataMode = "all"
	case "all", "none":
	default:
		errs = append(errs, field("/dataMode", "must be all or none", "unknown_value"))
	}
	retention := st.RetentionHours
	if req.RetentionHours != nil {
		if *req.RetentionHours < 1 || *req.RetentionHours > transfer.MaxRetentionHours {
			errs = append(errs, field("/retentionHours", "must be 1 to 168", "out_of_range"))
		} else {
			retention = *req.RetentionHours
		}
	}
	if len(req.ExcludeTables) > 10000 {
		errs = append(errs, field("/excludeTables", "has too many tables", "max_items"))
	}
	if len(errs) > 0 {
		return task.Task{}, problem.Validation("The export request is invalid.", errs...)
	}
	who, err := o.identity(ctx, actor)
	if err != nil {
		return task.Task{}, err
	}
	if err := o.roleChecker().Check(ctx, who); err != nil {
		return task.Task{}, transfer.SafeError(err).Problem()
	}
	o.startMu.Lock()
	defer o.startMu.Unlock()
	if err := o.concurrency(ctx, st, actor); err != nil {
		return task.Task{}, err
	}
	if err := o.databaseTaskBusy(ctx, db); err != nil {
		return task.Task{}, err
	}
	// The structure, read by the service role (404 when the database does not exist).
	var facts *dbFacts
	if err := o.asServiceRO(ctx, db, func(tx pgx.Tx) error {
		var e error
		facts, e = readDBFacts(ctx, tx)
		return e
	}); err != nil {
		return task.Task{}, err
	}
	known := map[string]bool{}
	for _, t := range facts.dataTables() {
		known[t.qualified()] = true
	}
	var exclude []string
	for i, tr := range req.ExcludeTables {
		q := tr.Schema + "." + tr.Table
		if !known[q] {
			errs = append(errs, field(fmt.Sprintf("/excludeTables/%d", i), "the database has no such table", "not_found"))
			continue
		}
		exclude = append(exclude, q)
	}
	if len(errs) > 0 {
		return task.Task{}, problem.Validation("The export request is invalid.", errs...)
	}
	sort.Strings(exclude)
	if err := o.checkExportable(ctx, db, actor, facts, req.AcknowledgeLargeObjectsLost); err != nil {
		return task.Task{}, err
	}
	p := dbExportParams{Database: db, Owner: who, Retention: retention, MaxFile: st.MaxFileBytes, Exclude: exclude, DataMode: dataMode, AckLO: req.AcknowledgeLargeObjectsLost}
	// The record and the reservation: the per-group sizes times a factor of the stored form, capped by the largest file.
	x.store.SetQuota(st.StagingQuotaBytes)
	est := dbEstimate(facts, st.MaxFileBytes)
	rep, _ := json.Marshal(exportReport{Encoding: "UTF8", Consistency: []api.TableConsistency{}})
	rs := o.records()
	now := o.transferNow()
	id, err := rs.Create(ctx, transfer.Record{Kind: transfer.KindDatabaseExport, State: transfer.StateBuilding, ServerID: o.ServerHost, Owner: who, Database: db,
		ReservedBytes: est, Format: "archive/1", ExpiresAt: now.Add(time.Duration(retention) * time.Hour)})
	if err != nil {
		return task.Task{}, wrap(err)
	}
	failRecord := func(e error) (task.Task, error) {
		x.forget(id)
		_ = rs.Remove(context.WithoutCancel(ctx), id)
		return task.Task{}, e
	}
	res, err := x.store.Reserve(id, who.Name, est)
	if err != nil {
		if q := quotaProblem(err); q != nil {
			return failRecord(q)
		}
		return failRecord(wrap(err))
	}
	x.mu.Lock()
	x.res[id] = res
	x.mu.Unlock()
	if err := rs.Apply(ctx, id, transfer.Update{Report: rep}); err != nil {
		return failRecord(wrap(err))
	}
	p.TransferID = id
	t, err := o.StartTask(ctx, KindDatabaseExport, "database", db, p, actor)
	if err != nil {
		return failRecord(err)
	}
	tid := t.ID
	_ = rs.Apply(ctx, id, transfer.Update{TaskID: &tid})
	return t, nil
}

// asServiceRO runs f in a read-only transaction of the service role.
func (o *Ops) asServiceRO(ctx context.Context, db string, f func(tx pgx.Tx) error) error {
	if o.Service == nil {
		return problem.New(problem.UnsupportedOp, "This server has no managed instance.")
	}
	c, err := o.Service.ServiceConn(ctx, db)
	if err != nil {
		return connectProblem(err)
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	tx, err := c.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return connectProblem(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	return f(tx)
}

// dbEstimate is the reservation of a database export (design "Where files live" 2): the sizes of what this server holds, for every group that holds buckets,
// times a factor of the stored form (columnar 10, others 1.5), at least 64 MiB and at most the largest file.
func dbEstimate(f *dbFacts, maxFile int64) int64 {
	var est float64
	for _, t := range f.dataTables() {
		factor := 1.5
		if t.AM == "columnar" {
			factor = 10
		}
		_, groups := f.label(t)
		est += float64(t.Bytes) * float64(groups) * factor
	}
	e := int64(est)
	if e < 64<<20 {
		e = 64 << 20
	}
	if e > maxFile {
		e = maxFile
	}
	return e
}

// checkExportable is the check of the first step, made at the request and again in the step: what the archive does not carry, the privileges of the role
// on every table, and the code of other roles that a read would run.
func (o *Ops) checkExportable(ctx context.Context, db, role string, f *dbFacts, ackLO bool) error {
	if len(f.Unsupported) > 0 {
		kinds := map[string]bool{}
		var names []string
		for _, u := range f.Unsupported {
			k, _, _ := strings.Cut(u, ": ")
			if !kinds[k] {
				kinds[k] = true
				names = append(names, k)
			}
		}
		sort.Strings(names)
		return problem.New(problem.SchemaUnrecognised, "The database has objects that an archive does not carry: "+strings.Join(names, ", ")+". Such a database is a job for pg_dump and pg_restore.")
	}
	if (f.LargeObjects > 0 || len(f.OIDColumns) > 0) && !ackLO {
		return problem.New(problem.LargeObjectsPresent, fmt.Sprintf("The database has %d large objects and %d columns of type oid or reg* whose values do not carry over to another database; acknowledgeLargeObjectsLost accepts that they are lost.", f.LargeObjects, len(f.OIDColumns)))
	}
	conn, err := o.roleConn(ctx, db, role)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	objs, total, err := o.roleReadable(ctx, conn)
	if err != nil {
		return wrapProblem(err)
	}
	if total > 0 {
		names := make([]string, 0, len(objs))
		for _, b := range objs {
			names = append(names, b.Object)
		}
		return problem.New(problem.ExportNotReadable, fmt.Sprintf("The role cannot read %d objects of the database. The first %d are: %s", total, len(objs), strings.Join(names, ", "))).WithReasons(objs)
	}
	// The code of other roles that a read would run (a virtual generated column of a table that another role owns).
	var foreign []string
	if err := inTx(ctx, conn, func(tx pgx.Tx) error {
		var owner string
		if e := tx.QueryRow(ctx, `SELECT current_user::text`).Scan(&owner); e != nil {
			return e
		}
		for _, t := range f.dataTables() {
			if len(t.Gen) == 0 || t.Owner == owner {
				continue
			}
			fn, e := untrustedFunctions(ctx, tx, t.Schema, t.Name, t.Gen)
			if e != nil {
				return e
			}
			foreign = append(foreign, fn...)
		}
		return nil
	}); err != nil {
		return wrapProblem(err)
	}
	if len(foreign) > 0 {
		return problem.New(problem.ExportUntrustedCode, "A read of the database would run a function owned by another role: "+strings.Join(foreign, ", ")+".")
	}
	return nil
}

// DBExportHook is called by a test at the start of each step of database.export (the name of the step); it may block, and an error ends the step.
var DBExportHook func(ctx context.Context, step string) error

func dbStep(name string, run func(ctx context.Context, env *task.Env) error) func(ctx context.Context, env *task.Env) error {
	return xferStep(func(ctx context.Context, env *task.Env) error {
		if h := DBExportHook; h != nil {
			if err := h(ctx, name); err != nil {
				return err
			}
		}
		return run(ctx, env)
	})
}

func (o *Ops) dbExportDefinition() task.Definition {
	return task.Definition{
		Kind: KindDatabaseExport,
		Steps: []task.Step{
			{Name: stepDCheck, Run: dbStep(stepDCheck, o.stepDBCheck)},
			{Name: stepDStructure, Run: dbStep(stepDStructure, o.stepDBStructure)},
			{Name: stepDNormalise, Run: dbStep(stepDNormalise, o.stepDBNormalise)},
			{Name: stepDData, Interruptible: true, Run: dbStep(stepDData, o.stepDBData)},
			{Name: stepDVerify, Run: dbStep(stepDVerify, o.stepDBVerify)},
			{Name: stepDSeal, Run: dbStep(stepDSeal, o.stepDBSeal)},
		},
		OnFail: o.dbExportOnFail,
	}
}

func (o *Ops) loadDBExport(ctx context.Context, env *task.Env) (dbExportParams, transfer.Record, *xferState, error) {
	p, err := inputOf[dbExportParams](env.Task)
	if err != nil {
		return p, transfer.Record{}, nil, err
	}
	x, err := o.xfer(ctx)
	if err != nil {
		return p, transfer.Record{}, nil, transfer.Errorf(transfer.CodeLeaderChanged, true, "the staging store of this server is not available; repeat the operation")
	}
	rec, err := o.records().Get(ctx, p.TransferID)
	if err != nil {
		return p, rec, x, transfer.Errorf(transfer.CodeLeaderChanged, true, "the file of the export is not known to this server; repeat the operation")
	}
	if rec.ServerID != o.ServerHost || rec.State != transfer.StateBuilding {
		return p, rec, x, transfer.Errorf(transfer.CodeLeaderChanged, true, "the leader changed while the export was running; repeat the operation")
	}
	if err := o.roleChecker().Check(ctx, rec.Owner); err != nil {
		return p, rec, x, err
	}
	return p, rec, x, nil
}

func (o *Ops) stepDBCheck(ctx context.Context, env *task.Env) error {
	p, rec, _, err := o.loadDBExport(ctx, env)
	if err != nil {
		return err
	}
	if err := o.stillEnabled(ctx); err != nil {
		return err
	}
	var facts *dbFacts
	if err := o.asServiceRO(ctx, p.Database, func(tx pgx.Tx) error {
		var e error
		facts, e = readDBFacts(ctx, tx)
		return e
	}); err != nil {
		return transfer.Errorf(transfer.CodeForbidden, false, "the database can no longer be exported as it was requested")
	}
	if err := o.checkExportable(ctx, p.Database, rec.Owner.Name, facts, p.AckLO); err != nil {
		return transfer.Errorf(transfer.CodeForbidden, false, "the database can no longer be exported as it was requested: %s", firstSentence(err))
	}
	return env.SetDetail(ctx, "transfer "+idText(p.TransferID))
}

// firstSentence is the text of a problem without anything the problem got from outside (a problem of the checks is built from fixed sentences and names).
func firstSentence(err error) string {
	var p *problem.Problem
	if errors.As(err, &p) {
		return p.Title
	}
	return "the check failed"
}

// stepDBStructure reads the catalog and dumps the schema, in one transaction of the service role.
func (o *Ops) stepDBStructure(ctx context.Context, env *task.Env) error {
	p, rec, _, err := o.loadDBExport(ctx, env)
	if err != nil {
		return err
	}
	w, err := o.takeStructure(ctx, p)
	if err != nil {
		return err
	}
	o.setWork(rec.ID, w)
	return nil
}

func (o *Ops) takeStructure(ctx context.Context, p dbExportParams) (*dbWork, error) {
	if o.Service == nil {
		return nil, transfer.Errorf(transfer.CodeSessionLost, true, "this server has no managed instance")
	}
	c, err := o.Service.ServiceConn(ctx, p.Database)
	if err != nil {
		return nil, transfer.Errorf(transfer.CodeSessionLost, true, "the service role cannot connect to the database now; repeat the operation")
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	tx, err := c.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, transfer.Errorf(transfer.CodeSessionLost, true, "the service role cannot connect to the database now; repeat the operation")
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	facts, err := readDBFacts(ctx, tx)
	if err != nil {
		return nil, err
	}
	var snap string
	if err := tx.QueryRow(ctx, `SELECT pg_export_snapshot()`).Scan(&snap); err != nil {
		return nil, err
	}
	w := &dbWork{facts: facts, fingerprint: facts.fingerprint()}
	cfg := c.Config()
	if w.rawPre, err = o.runDump(ctx, cfg, "pre-data", snap); err != nil {
		return nil, err
	}
	if w.rawPost, err = o.runDump(ctx, cfg, "post-data", snap); err != nil {
		return nil, err
	}
	return w, nil
}

// sqlChecker is the checker of the SQL files of an archive: the shape grammar in its validate-only mode.
func sqlChecker(hints map[string]bool) archive.SQLChecker {
	return func(name string, r io.Reader) (archive.SQLResult, error) {
		plan, err := sqlshape.ParseFile(r, name, sqlshape.Hints{InheritanceChildren: hints})
		if err != nil {
			return archive.SQLResult{}, archive.NewInvalid("sql", name, "an SQL file could not be read as statements")
		}
		out := archive.SQLResult{ByKind: plan.ByKind}
		for _, rf := range plan.Refused {
			out.Refused = append(out.Refused, archive.RefusedStatement{Kind: rf.Kind, Position: rf.Position, Fingerprint: rf.Fingerprint, Reason: rf.Reason})
		}
		return out, nil
	}
}

// stepDBNormalise makes the three SQL files and runs them through the shape grammar: an archive that the importer would refuse fails here.
func (o *Ops) stepDBNormalise(ctx context.Context, env *task.Env) error {
	p, rec, _, err := o.loadDBExport(ctx, env)
	if err != nil {
		return err
	}
	w := o.work(rec.ID)
	if w == nil {
		if w, err = o.takeStructure(ctx, p); err != nil {
			return err
		}
	}
	norm, err := dumpnorm.Normalise(w.rawPre, w.rawPost, w.facts.normFacts())
	if err != nil {
		if r, ok := dumpnorm.IsRefusal(err); ok {
			return transfer.Errorf(transfer.CodeSchemaUnrecognised, false, "the dump holds a construct that an archive does not carry: %s", r.Kind)
		}
		return transfer.Errorf(transfer.CodeSchemaUnrecognised, false, "the dump could not be turned into a schema")
	}
	hints := w.facts.inheritanceChildren()
	for name, text := range map[string]string{archive.PreDataName: norm.Pre, archive.ViewsName: norm.Views, archive.PostDataName: norm.Post} {
		res, serr := sqlChecker(hints)(name, strings.NewReader(text))
		if serr != nil {
			return transfer.Errorf(transfer.CodeSchemaUnrecognised, false, "the schema of the database could not be read as statements")
		}
		if len(res.Refused) > 0 {
			kinds := map[string]bool{}
			var ks []string
			for _, r := range res.Refused {
				if !kinds[r.Kind] {
					kinds[r.Kind] = true
					ks = append(ks, r.Kind)
				}
			}
			sort.Strings(ks)
			return transfer.Errorf(transfer.CodeSchemaUnrecognised, false, "the schema has statements that an archive does not carry: %s", strings.Join(ks, ", "))
		}
	}
	w.norm = norm
	w.cat = w.facts.catalog()
	o.setWork(rec.ID, w)
	return nil
}

// dataWriter is what the data step writes into: the archive writer over the sealing writer.
type archiveSink struct {
	sw *sealWriter
	aw *archive.Writer
}

// stepDBData writes the whole archive (the fixed entries, the data and the manifest) to the staged file. It is one step and one process: a restart writes the
// file again from the beginning.
func (o *Ops) stepDBData(ctx context.Context, env *task.Env) error {
	p, rec, x, err := o.loadDBExport(ctx, env)
	if err != nil {
		return err
	}
	res, err := x.reservation(rec)
	if err != nil {
		return err
	}
	var last error
	for attempt := 0; attempt < exportAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(exportRetryGap):
			}
		}
		_ = x.store.Remove(rec.ID)
		left, terr := o.timeLeft(env.Task)
		if terr != nil {
			return terr
		}
		w := o.work(rec.ID)
		if w == nil || w.norm.Pre == "" {
			// A restart between the steps: the structure is read again, and the files are made again.
			if w, err = o.takeStructure(ctx, p); err != nil {
				return err
			}
			norm, nerr := dumpnorm.Normalise(w.rawPre, w.rawPost, w.facts.normFacts())
			if nerr != nil {
				return transfer.Errorf(transfer.CodeSchemaUnrecognised, false, "the dump holds a construct that an archive does not carry")
			}
			w.norm, w.cat = norm, w.facts.catalog()
			o.setWork(rec.ID, w)
		}
		meta, sealed, sum, err := o.writeArchive(ctx, env, x, rec, res, p, w, left)
		if err == nil {
			var rep exportReport
			_ = json.Unmarshal(rec.Report, &rep)
			rep.Rows = w.manifest.Totals.Rows
			rep.Warnings = w.manifest.Warnings
			rep.Consistency = archiveConsistency(w)
			b, _ := json.Marshal(rep)
			size := sealed
			return o.records().Apply(ctx, rec.ID, transfer.Update{SizeBytes: &size, SHA256: &sum, Report: b, Touch: true, KeyID: &meta.KeyID, WrappedKey: meta.WrappedKey})
		}
		last = err
		_ = x.store.Remove(rec.ID)
		if te := transfer.SafeError(err); !te.Retryable || ctx.Err() != nil {
			return o.timedOut(env.Task, stagingError(err))
		}
	}
	return last
}

// archiveConsistency is the label of every table of the archive for the record.
func archiveConsistency(w *dbWork) []api.TableConsistency {
	out := []api.TableConsistency{}
	for _, e := range w.manifest.Entries {
		if e.Table == "" || e.LedgerHistory {
			continue
		}
		out = append(out, api.TableConsistency{Table: e.Table, Level: e.Consistency, Groups: e.Groups})
	}
	return out
}

// writeArchive runs one attempt: the roles connection, the entries one after another, the manifest, the seal.
func (o *Ops) writeArchive(ctx context.Context, env *task.Env, x *xferState, rec transfer.Record, res *staging.Reservation, p dbExportParams, w *dbWork, timeout time.Duration) (meta staging.FileMeta, size int64, sum string, err error) {
	excluded := p.excluded()
	entries := w.facts.dataEntries(excluded, p.DataMode)
	single := w.facts.singleSnapshot() && len(entries) > 0
	conn, err := o.roleConn(ctx, p.Database, rec.Owner.Name)
	if err != nil {
		return meta, 0, "", transfer.Errorf(transfer.CodeSessionLost, true, "the role cannot connect to the database from this server now; repeat the operation")
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	formats, err := transfer.SessionFormats("UTF8")
	if err != nil {
		return meta, 0, "", err
	}
	setup := append(formats, "SET LOCAL row_security = off", fmt.Sprintf("SET LOCAL statement_timeout = %d", timeout.Milliseconds()))
	begin := func(iso pgx.TxIsoLevel) (pgx.Tx, error) {
		tx, e := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: iso, AccessMode: pgx.ReadOnly})
		if e != nil {
			return nil, e
		}
		for _, s := range setup {
			if _, e := tx.Exec(ctx, s); e != nil {
				_ = tx.Rollback(context.WithoutCancel(ctx))
				return nil, e
			}
		}
		return tx, nil
	}
	fw, meta, err := x.store.Create(rec.ID, res, x.keys)
	if err != nil {
		return meta, 0, "", err
	}
	sw := &sealWriter{w: fw, h: sha256.New(), max: p.MaxFile}
	aw := archive.NewWriter(sw)
	aw.SetLimit(p.MaxFile)
	fail := func(e error) (staging.FileMeta, int64, string, error) {
		_ = fw.Discard()
		return meta, 0, "", e
	}
	catJSON, err := json.Marshal(w.cat)
	if err != nil {
		return fail(err)
	}
	for _, f := range []struct {
		name string
		data []byte
	}{{archive.CatalogName, catJSON}, {archive.PreDataName, []byte(w.norm.Pre)}, {archive.ViewsName, []byte(w.norm.Views)}, {archive.PostDataName, []byte(w.norm.Post)}} {
		ew, e := aw.Create(f.name)
		if e != nil {
			return fail(e)
		}
		if _, e := ew.Write(f.data); e != nil {
			return fail(e)
		}
	}
	// The data. One transaction for every table when the archive can be one snapshot, one statement per table otherwise.
	var tx pgx.Tx
	if single {
		if tx, err = begin(pgx.RepeatableRead); err != nil {
			return fail(err)
		}
		defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	}
	metaEntries := map[string]archive.ManifestEntry{}
	rows := map[string]int64{}
	for i, e := range entries {
		name := archive.DataName(i + 1)
		sqlText, e2 := transfer.ArchiveCopySQL(e.Table.Schema, e.Table.Name, e.Cols, e.Only)
		if e2 != nil {
			return fail(e2)
		}
		ew, e2 := aw.Create(name)
		if e2 != nil {
			return fail(e2)
		}
		cur := tx
		if !single {
			if cur, e2 = begin(pgx.ReadCommitted); e2 != nil {
				return fail(e2)
			}
		}
		_ = env.SetDetail(ctx, fmt.Sprintf("%s (%d of %d), %d bytes so far", e.Table.qualified(), i+1, len(entries), sw.n))
		tag, e2 := cur.Conn().PgConn().CopyTo(ctx, ew, sqlText)
		if !single {
			_ = cur.Rollback(context.WithoutCancel(ctx))
		}
		if e2 != nil {
			return fail(e2)
		}
		level, groups := w.facts.label(e.Table)
		if single {
			level, groups = "archive-single-snapshot", 1
		}
		named := e.Table.qualified()
		if e.History {
			named = e.Base
		}
		me := archive.ManifestEntry{Table: named, Columns: e.Cols, Rows: tag.RowsAffected(), Consistency: level, Groups: groups, LedgerHistory: e.History}
		metaEntries[name] = me
		if !e.History {
			rows[e.Table.qualified()] = me.Rows
		}
	}
	// The independent count of every table, in the same transaction when there is one (design step 5).
	var warns []string
	for _, e := range entries {
		if e.History {
			continue
		}
		var n int64
		q := "SELECT count(*) FROM " + qualified(e.Table.Schema, e.Table.Name)
		if e.Only {
			q = "SELECT count(*) FROM ONLY " + qualified(e.Table.Schema, e.Table.Name)
		}
		cur := tx
		var own pgx.Tx
		if !single {
			var e3 error
			if own, e3 = begin(pgx.ReadCommitted); e3 != nil {
				return fail(e3)
			}
			cur = own
		}
		cerr := cur.QueryRow(ctx, q).Scan(&n)
		if own != nil {
			_ = own.Rollback(context.WithoutCancel(ctx))
		}
		if cerr != nil {
			return fail(cerr)
		}
		if n != rows[e.Table.qualified()] {
			if single {
				return fail(transfer.Errorf(transfer.CodeRowCountMismatch, false, "the rows written for table %s are not the rows that the table has", e.Table.qualified()))
			}
			warns = append(warns, fmt.Sprintf("table %s changed while it was read: %d rows were written and %d counted at the end", e.Table.qualified(), rows[e.Table.qualified()], n))
		}
	}
	if single {
		if e := tx.Commit(ctx); e != nil {
			return fail(e)
		}
	}
	w.single = single
	w.entries = entries
	w.rows = rows
	manifest := w.facts.manifest(p.Database, o.kingresVersion(), w.fingerprint, o.transferNow(), p.DataMode, excluded, append(w.facts.warnings(entries, single, p.AckLO, excluded), warns...))
	m, e2 := aw.Finish(manifest, metaEntries)
	if e2 != nil {
		return fail(e2)
	}
	w.manifest = m
	sealed, e2 := fw.Finish()
	if e2 != nil {
		return meta, 0, "", e2
	}
	return meta, sealed.Plaintext, hex.EncodeToString(sw.h.Sum(nil)), nil
}

func (o *Ops) kingresVersion() string {
	if o.Transfer != nil && o.Transfer.KingresVersion != "" {
		return o.Transfer.KingresVersion
	}
	return "dev"
}

// stepDBVerify reads the sealed file back through the validator of the importer and compares the fingerprint of the structure with the one the export
// started with: a difference fails the task with schema_changed and no file is kept.
func (o *Ops) stepDBVerify(ctx context.Context, env *task.Env) error {
	p, rec, x, err := o.loadDBExport(ctx, env)
	if err != nil {
		return err
	}
	w := o.work(rec.ID)
	if w == nil {
		return transfer.Errorf(transfer.CodeLeaderChanged, true, "the export lost its state; repeat the operation")
	}
	cur, err := o.records().Get(ctx, rec.ID)
	if err != nil || cur.SizeBytes == nil || cur.SHA256 == "" {
		return transfer.Errorf(transfer.CodeCopyInterrupted, true, "the export did not finish; repeat the operation")
	}
	dk, err := x.keys.Unwrap(rec.ID, rec.KeyID, rec.WrappedKey)
	if err != nil {
		return transfer.Errorf(transfer.CodeKeyLost, false, "the key of the file is gone")
	}
	r, err := x.store.OpenReader(rec.ID, dk)
	if err != nil {
		return transfer.Errorf(transfer.CodeLeaderChanged, true, "the file of the export is not there; repeat the operation")
	}
	defer func() { _ = r.Close() }()
	res, verr := archive.Validate(r, r.Size(), archive.Limits{MaxImportBytes: p.MaxFile * 4, SQLFor: func(cat *archive.Catalog) archive.SQLChecker { return sqlChecker(catalogInheritance(cat)) }})
	if verr != nil {
		// The reasons of the validator are fixed codes and bounded names of the archive that this export made: no value of the data.
		var ae *archive.Error
		if errors.As(verr, &ae) && len(ae.Reasons) > 0 {
			return transfer.Errorf(transfer.CodeArchiveInvalid, false, "the archive that was written is not accepted by its own validator (%s: %s)", ae.Reasons[0].Code, ae.Reasons[0].Object)
		}
		return transfer.Errorf(transfer.CodeArchiveInvalid, false, "the archive that was written is not accepted by its own validator")
	}
	for _, sr := range res.SQL {
		if len(sr.Refused) > 0 {
			return transfer.Errorf(transfer.CodeSchemaUnrecognised, false, "the archive that was written has statements that its own validator refuses")
		}
	}
	// The structure at the end.
	var facts *dbFacts
	if err := o.asServiceRO(ctx, p.Database, func(tx pgx.Tx) error {
		var e error
		facts, e = readDBFacts(ctx, tx)
		return e
	}); err != nil {
		return transfer.Errorf(transfer.CodeSessionLost, true, "the service role cannot connect to the database now; repeat the operation")
	}
	if facts.fingerprint() != w.fingerprint {
		return transfer.Errorf(transfer.CodeSchemaChanged, false, "a table or a column of the database changed while it was exported; repeat the export when the structure is quiet")
	}
	return nil
}

// catalogInheritance are the tables of a catalog document that inherit from another table.
func catalogInheritance(cat *archive.Catalog) map[string]bool {
	out := map[string]bool{}
	for _, t := range cat.Tables {
		if t.Parent != "" && (t.TreeRole == "inheritanceChild" || t.TreeRole == "inheritanceParent") {
			out[t.Qualified()] = true
		}
	}
	return out
}

func (o *Ops) stepDBSeal(ctx context.Context, env *task.Env) error {
	p, rec, x, err := o.loadDBExport(ctx, env)
	if err != nil {
		return err
	}
	st, _, serr := x.store.State(rec.ID)
	if serr != nil || st != staging.StateFinal {
		return transfer.Errorf(transfer.CodeLeaderChanged, true, "the file of the export is not there; repeat the operation")
	}
	cur, err := o.records().Get(ctx, rec.ID)
	if err != nil {
		return err
	}
	if cur.SHA256 == "" || cur.SizeBytes == nil {
		return transfer.Errorf(transfer.CodeCopyInterrupted, true, "the export did not finish; repeat the operation")
	}
	if res, rerr := x.reservation(rec); rerr == nil {
		res.Settle()
	}
	ready := transfer.StateReady
	exp := o.transferNow().Add(time.Duration(p.Retention) * time.Hour)
	if err := o.records().Apply(ctx, rec.ID, transfer.Update{State: &ready, ExpiresAt: &exp, Touch: true}); err != nil {
		return err
	}
	o.setWork(rec.ID, nil)
	// The console reads the transfer from the detail of the task that is done.
	_ = env.SetDetail(ctx, "transfer "+idText(p.TransferID))
	if o.Transfer != nil && o.Transfer.Audit != nil {
		var rep exportReport
		_ = json.Unmarshal(cur.Report, &rep)
		o.Transfer.Audit(ctx, "dataTransfer.completed", map[string]any{"kind": "databaseExport", "transferId": idText(rec.ID), "rows": rep.Rows, "bytes": *cur.SizeBytes, "sha256": cur.SHA256,
			"tables": len(rep.Consistency), "consistency": rep.Consistency, "outcome": "success"})
	}
	return nil
}

// dbExportOnFail removes the partial file and the reservation of a failed or cancelled export at once.
func (o *Ops) dbExportOnFail(ctx context.Context, t task.Task) {
	p, err := inputOf[dbExportParams](t)
	if err != nil {
		return
	}
	o.setWork(p.TransferID, nil)
	x, err := o.xfer(ctx)
	if err != nil {
		return
	}
	rec, err := o.records().Get(ctx, p.TransferID)
	if err != nil || rec.ServerID != o.ServerHost {
		return
	}
	_ = x.store.Remove(rec.ID)
	x.forget(rec.ID)
	failed, zero := transfer.StateFailed, int64(0)
	_ = o.records().Apply(context.WithoutCancel(ctx), rec.ID, transfer.Update{State: &failed, ReservedBytes: &zero})
	if o.Transfer != nil && o.Transfer.Audit != nil {
		code := ""
		if t.Error != nil {
			code = t.Error.Code
		}
		o.Transfer.Audit(ctx, "dataTransfer.completed", map[string]any{"kind": "databaseExport", "transferId": idText(rec.ID), "outcome": "failure", "code": code})
	}
}

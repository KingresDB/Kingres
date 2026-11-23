// SPDX-License-Identifier: AGPL-3.0-only

package ops

// The task table.export (contract 0.19.0, design "Table export"): one COPY (SELECT ...) TO STDOUT of a table, as the signed-in role, written to a
// sealed file on the leader. The statement is built from names that passed sqlx and from option values that passed the checks of package transfer; no
// client text is SQL. The read is one statement, so the guarantee is the one of a statement (README of kingres_shard, "Atomicity is not a global
// snapshot"): every row was committed, and a table over several groups is one snapshot per group.

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/archive"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fence"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/staging"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/transfer"
)

// KindTableExport and KindTableImport are the task kinds of the data transfer.
const (
	KindTableExport = "table.export"
	KindTableImport = "table.import"
)

// Step names of table.export.
const (
	stepXCheck = "Check the request"
	stepXClass = "Read the table class"
	stepXWrite = "Write the file"
	stepXSeal  = "Seal"
)

// exportParams is the input of the task: the request after validation, and what the request found out. Nothing in it is a value of the data.
type exportParams struct {
	TransferID int64             `json:"transferId"`
	Database   string            `json:"database"`
	Schema     string            `json:"schema"`
	Table      string            `json:"table"`
	Columns    []string          `json:"columns"`
	Stringy    []string          `json:"stringyColumns"`
	CSV        transfer.CSV      `json:"csv"`
	Header     bool              `json:"header"`
	BOM        bool              `json:"bom"`
	Gzip       bool              `json:"gzip"`
	Safe       bool              `json:"spreadsheetSafe"`
	Only       bool              `json:"only"`
	Owner      transfer.Identity `json:"owner"`
	Retention  int               `json:"retentionHours"`
	MaxFile    int64             `json:"maxFileBytes"`
	RLSOff     bool              `json:"rlsOff"`
	AckForeign bool              `json:"acknowledgeOtherRolesCode"`
}

// colInfo is a column of the table as the catalog says.
type colInfo struct {
	Name      string
	Generated bool
	Virtual   bool
	Stringy   bool
	CanRead   bool
}

// tableFacts is what a request to export or to import learns of the table, through a connection of the role.
type tableFacts struct {
	Exists         bool
	Kind           string // r, p, v, m, f ...
	IsPartition    bool
	HasInheritance bool // the table has children that are not partitions
	Columns        []colInfo
	RLS            bool
	RLSBypass      bool
	OwnerIsRole    bool
	Columnar       bool
	LocalBytes     int64
	LedgerPolicy   string
	HasTriggers    bool
	ForeignCode    []string
}

// timeLeft is what remains of the longest time of a transfer task (the flag transfer-max-duration); the COPY carries it as its statement_timeout.
func (o *Ops) timeLeft(t task.Task) (time.Duration, error) {
	max := 24 * time.Hour
	if o.Transfer != nil && o.Transfer.MaxDuration > 0 {
		max = o.Transfer.MaxDuration
	}
	left := max - o.transferNow().Sub(t.CreatedAt)
	if left < time.Second {
		return 0, transfer.Errorf(transfer.CodeTimeout, false, "the transfer ran out of time")
	}
	return left, nil
}

// timedOut turns the cancel that the statement timeout makes (57014) into the code of the time limit when the time is up.
func (o *Ops) timedOut(t task.Task, err error) error {
	if te := transfer.SafeError(err); te.SQLState == "57014" {
		if _, terr := o.timeLeft(t); terr != nil {
			return terr
		}
	}
	return err
}

var bucketNameRE = regexp.MustCompile(`_b[0-9]{4}$`)

// readFacts asks the catalog, as the role, about the table.
func readFacts(ctx context.Context, tx pgx.Tx, schema, table string) (tableFacts, error) {
	var f tableFacts
	rel := qualified(schema, table)
	err := tx.QueryRow(ctx, `SELECT c.relkind::text, c.relispartition, c.relrowsecurity,
			c.relowner = (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = current_user),
			(SELECT rolbypassrls OR rolsuper FROM pg_catalog.pg_roles WHERE rolname = current_user),
			coalesce((SELECT am.amname = 'columnar' FROM pg_catalog.pg_am am WHERE am.oid = c.relam), false),
			CASE WHEN c.relkind IN ('r', 'p', 'm') THEN pg_catalog.pg_total_relation_size(c.oid) ELSE 0 END,
			EXISTS (SELECT 1 FROM pg_catalog.pg_inherits i WHERE i.inhparent = c.oid) AND c.relkind = 'r'
		FROM pg_catalog.pg_class c WHERE c.oid = to_regclass($1)`, rel).Scan(&f.Kind, &f.IsPartition, &f.RLS, &f.OwnerIsRole, &f.RLSBypass, &f.Columnar, &f.LocalBytes, &f.HasInheritance)
	if errors.Is(err, pgx.ErrNoRows) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	f.Exists = true
	rows, err := tx.Query(ctx, `SELECT a.attname::text, a.attgenerated::text <> '', a.attgenerated::text = 'v',
			(t.typtype = 'e' OR t.typcategory = 'S' OR t.typname = 'xml' OR (t.typtype = 'd' AND (bt.typcategory = 'S' OR bt.typname = 'xml'))),
			pg_catalog.has_column_privilege(a.attrelid, a.attnum, 'SELECT')
		FROM pg_catalog.pg_attribute a
		JOIN pg_catalog.pg_type t ON t.oid = a.atttypid
		LEFT JOIN pg_catalog.pg_type bt ON bt.oid = t.typbasetype
		WHERE a.attrelid = to_regclass($1) AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum`, rel)
	if err != nil {
		return f, err
	}
	f.Columns, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (colInfo, error) {
		var c colInfo
		return c, r.Scan(&c.Name, &c.Generated, &c.Virtual, &c.Stringy, &c.CanRead)
	})
	return f, err
}

// ineligible makes the table_not_eligible problem with its blockers.
func ineligible(msg string, blockers ...api.TableBlocker) *problem.Problem {
	return problem.New(problem.TableNotEligible, msg).WithBlockers(blockers)
}

func blocker(code, object, msg string) api.TableBlocker {
	return api.TableBlocker{Code: code, Object: object, Message: msg}
}

// eligibleKind checks what every transfer needs of the table: it exists, is an ordinary or partitioned table, is not in a schema of the system or the
// extensions and is not a bucket partition of a distributed table.
func eligibleKind(schema, table string, f tableFacts) error {
	switch schema {
	case "kingres", "pg_catalog", "information_schema", "pg_toast":
		return ineligible("The table is in a schema of the system.", blocker("system_schema", schema, "Tables of this schema are not exported or imported."))
	}
	if !f.Exists {
		return problem.New(problem.NotFound, fmt.Sprintf("Table %s.%s does not exist.", schema, table))
	}
	switch f.Kind {
	case "r", "p":
	default:
		return ineligible("Only tables can be exported or imported.", blocker("not_a_table", schema+"."+table, "A view, a materialized view or a foreign table is not transferred."))
	}
	if f.IsPartition && bucketNameRE.MatchString(table) {
		return ineligible("A bucket partition cannot be named directly.", blocker("bucket_partition", schema+"."+table, "Name the distributed table, not its bucket partitions."))
	}
	return nil
}

// roleConn opens a connection of the signed-in role to a database.
func (o *Ops) roleConn(ctx context.Context, db, role string) (*pgx.Conn, error) {
	if o.Roles == nil {
		return nil, problem.New(problem.UnsupportedOp, "This server cannot act as the signed-in role (no managed instance).")
	}
	c, err := o.Roles.Connect(ctx, db, role)
	if err != nil {
		o.logConnect(role, err)
		return nil, connectProblem(err)
	}
	return c, nil
}

// distribution reads, with the service role, how the table is distributed: its class and the number of groups that hold buckets. A database
// without the extension, or a table that is not in its catalog, is local.
func (o *Ops) distribution(ctx context.Context, db, schema, table string) (class string, groups int, err error) {
	class, groups = "local", 1
	err = o.asService(ctx, db, func(tx pgx.Tx) error {
		var have bool
		if e := tx.QueryRow(ctx, `SELECT to_regclass('kingres.dist_table') IS NOT NULL`).Scan(&have); e != nil || !have {
			return e
		}
		var method string
		var coloc *int
		e := tx.QueryRow(ctx, `SELECT method, colocation_id FROM kingres.dist_table WHERE relid = to_regclass($1)`, qualified(schema, table)).Scan(&method, &coloc)
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		class = method
		if method == "hash" && coloc != nil {
			return tx.QueryRow(ctx, `SELECT greatest(count(DISTINCT group_id), 1) FROM kingres.bucket_map WHERE colocation_id = $1 AND state IN ('active', 'moving')`, *coloc).Scan(&groups)
		}
		return nil
	})
	return class, groups, wrap(err)
}

// consistencyOf is the label of the guarantee of one export (design "What is consistent"). The statement of an export is one statement: a table on one group
// is one snapshot, a table on several groups is one snapshot per group, and the label never says more than that.
func consistencyOf(schema, table, class string, groups int) api.TableConsistency {
	level := "single-snapshot"
	if class == "hash" && groups > 1 {
		level = "per-group"
	}
	if groups < 1 {
		groups = 1
	}
	return api.TableConsistency{Table: schema + "." + table, Level: level, Groups: groups}
}

// untrustedFunctions lists, as the role, the functions that a read of the exported columns of a table that another role owns would run and that are
// owned by a role that is neither the reader nor a superuser: the expressions of virtual generated columns and of row-level-security policies.
func untrustedFunctions(ctx context.Context, tx pgx.Tx, schema, table string, columns []string) ([]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT p.proname::text
		FROM pg_catalog.pg_depend d
		JOIN pg_catalog.pg_proc p ON d.refclassid = 'pg_catalog.pg_proc'::regclass AND p.oid = d.refobjid
		WHERE p.proowner <> (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = current_user)
		  AND NOT (SELECT rolsuper FROM pg_catalog.pg_roles WHERE oid = p.proowner)
		  AND ((d.classid = 'pg_catalog.pg_attrdef'::regclass AND d.objid IN (
		          SELECT ad.oid FROM pg_catalog.pg_attrdef ad JOIN pg_catalog.pg_attribute a ON a.attrelid = ad.adrelid AND a.attnum = ad.adnum
		          WHERE ad.adrelid = to_regclass($1) AND a.attgenerated::text = 'v' AND a.attname::text = ANY($2)))
		    OR (d.classid = 'pg_catalog.pg_policy'::regclass AND d.objid IN (SELECT oid FROM pg_catalog.pg_policy WHERE polrelid = to_regclass($1))))
		ORDER BY 1 LIMIT 20`, qualified(schema, table), columns)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// concurrency refuses a new transfer when the cluster or the role already runs as many as the setting allows.
func (o *Ops) concurrency(ctx context.Context, st transfer.Settings, role string) error {
	var total, own int
	err := wrap(o.asService(ctx, "kingres_system", func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*)::int, count(*) FILTER (WHERE created_by = $1)::int FROM kingres_cp.task
			WHERE kind IN ('table.export', 'table.import', 'database.export', 'archive.validate', 'database.import') AND state IN ('pending', 'running', 'paused')`, role).Scan(&total, &own)
	}))
	if err != nil {
		return err
	}
	if total >= st.MaxConcurrent || own >= 1 {
		return problem.New(problem.OperationInProgress, "The data transfers that may run at once are running: "+fmt.Sprint(st.MaxConcurrent)+" in the cluster, one per role.").WithRetryAfter(30)
	}
	return nil
}

// estimateBytes is the reservation of an export: the size of what this server holds of the table, for every group that holds buckets, times a factor of
// the stored form (columnar tables 10, others 1.5), at least 64 MiB and at most the largest file. The reservation grows as bytes are written, so a bad
// estimate stops the exporter that made it and not its neighbours.
func estimateBytes(f tableFacts, groups int, maxFile int64) int64 {
	factor := 1.5
	if f.Columnar {
		factor = 10
	}
	est := int64(float64(f.LocalBytes) * float64(groups) * factor)
	if est < 64<<20 {
		est = 64 << 20
	}
	if est > maxFile {
		est = maxFile
	}
	return est
}

// StartExport answers exportTable: it checks everything the design lists for the first step, reserves the room, makes the record and starts the task.
func (o *Ops) StartExport(ctx context.Context, actor, db, schema, table string, req api.ExportTableRequest) (task.Task, error) {
	x, err := o.xfer(ctx)
	if err != nil {
		return task.Task{}, err
	}
	if err := checkNames(db, schema, table); err != nil {
		return task.Task{}, err
	}
	st, err := o.settings(ctx)
	if err != nil {
		return task.Task{}, err
	}
	csv := transfer.CSV{Delimiter: req.Delimiter, Quote: req.Quote, Encoding: req.Encoding}
	if req.NullString != nil {
		csv.NullString = *req.NullString
	}
	csv = csv.Fill()
	errs := csv.Check()
	if req.UTF8BOM && csv.Encoding != "UTF8" {
		errs = append(errs, field("/utf8Bom", "needs the encoding UTF8", "conflict"))
	}
	switch req.Compression {
	case "", "none", "gzip":
	default:
		errs = append(errs, field("/compression", "must be none or gzip", "unknown_value"))
	}
	retention := st.RetentionHours
	if req.RetentionHours != nil {
		if *req.RetentionHours < 1 || *req.RetentionHours > transfer.MaxRetentionHours {
			errs = append(errs, field("/retentionHours", "must be 1 to 168", "out_of_range"))
		} else {
			retention = *req.RetentionHours
		}
	}
	if len(req.Columns) > transfer.MaxColumns {
		errs = append(errs, field("/columns", "has too many columns", "max_items"))
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
	if StartGap != nil {
		StartGap()
	}
	conn, err := o.roleConn(ctx, db, actor)
	if err != nil {
		return task.Task{}, err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	var facts tableFacts
	var foreign []string
	if err := inTx(ctx, conn, func(tx pgx.Tx) error {
		var e error
		if facts, e = readFacts(ctx, tx, schema, table); e != nil {
			return e
		}
		if e = eligibleKind(schema, table, facts); e != nil {
			return e
		}
		cols := req.Columns
		if len(cols) == 0 {
			for _, c := range facts.Columns {
				cols = append(cols, c.Name)
			}
		}
		if foreign, e = o.exportColumnsCheck(ctx, tx, schema, table, facts, cols); e != nil {
			return e
		}
		return nil
	}); err != nil {
		return task.Task{}, wrapProblem(err)
	}
	if len(foreign) > 0 && !req.AcknowledgeOtherRolesCode && !facts.OwnerIsRole {
		return task.Task{}, problem.New(problem.ExportUntrustedCode, "A read of this table would run a function owned by another role: "+strings.Join(foreign, ", ")+". acknowledgeOtherRolesCode accepts the risk.")
	}
	cols := req.Columns
	if len(cols) == 0 {
		for _, c := range facts.Columns {
			cols = append(cols, c.Name)
		}
	}
	byName := map[string]colInfo{}
	for _, c := range facts.Columns {
		byName[c.Name] = c
	}
	var stringy []string
	for _, c := range cols {
		if byName[c].Stringy {
			stringy = append(stringy, c)
		}
	}
	class, groups, err := o.distribution(ctx, db, schema, table)
	if err != nil {
		return task.Task{}, err
	}
	safe := req.SpreadsheetSafe == nil || *req.SpreadsheetSafe
	header := req.Header == nil || *req.Header
	p := exportParams{Database: db, Schema: schema, Table: table, Columns: cols, Stringy: stringy, CSV: csv, Header: header, BOM: req.UTF8BOM,
		Gzip: req.Compression == "gzip", Safe: safe, Only: facts.HasInheritance, Owner: who, Retention: retention, MaxFile: st.MaxFileBytes,
		RLSOff: true, AckForeign: req.AcknowledgeOtherRolesCode}
	if _, err := transfer.CopyToSQL(schema, table, exportColumns(p), csv, header, p.Only, safe); err != nil {
		return task.Task{}, problem.Validation("The export request is invalid.", field("/columns", "a column name is not valid", "pattern"))
	}
	// The record and the reservation.
	x.store.SetQuota(st.StagingQuotaBytes)
	est := estimateBytes(facts, groups, st.MaxFileBytes)
	format := "csv"
	if p.Gzip {
		format = "csv.gz"
	}
	rep, _ := json.Marshal(exportReport{Encoding: csv.Encoding, Compression: req.Compression, Consistency: []api.TableConsistency{consistencyOf(schema, table, class, groups)}, Safe: safe})
	rs := o.records()
	now := o.transferNow()
	id, err := rs.Create(ctx, transfer.Record{Kind: transfer.KindTableExport, State: transfer.StateBuilding, ServerID: o.ServerHost, Owner: who, Database: db, Schema: schema, Table: table,
		ReservedBytes: est, Format: format, ExpiresAt: now.Add(time.Duration(retention) * time.Hour)})
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
	t, err := o.StartTask(ctx, KindTableExport, "table", db+"/"+schema+"."+table, p, actor)
	if err != nil {
		return failRecord(err)
	}
	tid := t.ID
	_ = rs.Apply(ctx, id, transfer.Update{TaskID: &tid})
	return t, nil
}

func exportColumns(p exportParams) []transfer.ExportColumn {
	st := map[string]bool{}
	for _, s := range p.Stringy {
		st[s] = true
	}
	out := make([]transfer.ExportColumn, 0, len(p.Columns))
	for _, c := range p.Columns {
		out = append(out, transfer.ExportColumn{Name: c, Stringy: st[c]})
	}
	return out
}

// exportColumnsCheck validates the columns of the request against the catalog and the privileges of the role, and returns the functions of other roles that a
// read of them would run (only asked for a table another role owns).
func (o *Ops) exportColumnsCheck(ctx context.Context, tx pgx.Tx, schema, table string, f tableFacts, cols []string) ([]string, error) {
	known := map[string]colInfo{}
	for _, c := range f.Columns {
		known[c.Name] = c
	}
	seen := map[string]bool{}
	var errs []problem.FieldError
	var cannot []api.TableBlocker
	for i, c := range cols {
		ci, ok := known[c]
		switch {
		case seen[c]:
			errs = append(errs, field(fmt.Sprintf("/columns/%d", i), "is listed twice", "duplicate"))
		case !ok:
			errs = append(errs, field(fmt.Sprintf("/columns/%d", i), "the table has no such column", "not_found"))
		case !ci.CanRead:
			cannot = append(cannot, blocker("no_privilege", c, "The role has no SELECT privilege on this column."))
		}
		seen[c] = true
	}
	if len(errs) > 0 {
		return nil, problem.Validation("The export request is invalid.", errs...)
	}
	if len(cols) == 0 {
		return nil, problem.Validation("The export request is invalid.", field("/columns", "the table has no columns", "required"))
	}
	if len(cannot) > 0 {
		return nil, ineligible("The role cannot read every column.", cannot...)
	}
	if f.RLS && !(f.RLSBypass || f.OwnerIsRole) {
		return nil, ineligible("The table has row-level security.", blocker("row_level_security", schema+"."+table, "A role that does not own the table and has no BYPASSRLS would read only the rows a policy lets through, so the export is refused."))
	}
	if f.OwnerIsRole {
		return nil, nil
	}
	return untrustedFunctions(ctx, tx, schema, table, cols)
}

// wrapProblem returns a problem as it is and wraps any other error.
func wrapProblem(err error) error {
	var p *problem.Problem
	if errors.As(err, &p) {
		return p
	}
	return wrap(err)
}

// xferStep makes the error of a step safe: a step of a transfer never records the text of PostgreSQL or of the system. Errors that are about the
// task itself (a lease that was lost, a fence, a pause, a skip, the context) are left to the engine.
func xferStep(run func(ctx context.Context, env *task.Env) error) func(ctx context.Context, env *task.Env) error {
	return func(ctx context.Context, env *task.Env) error {
		err := run(ctx, env)
		if err == nil {
			return nil
		}
		var pe *task.PauseError
		var se *task.SkipError
		switch {
		case errors.Is(err, task.ErrLeaseLost), errors.Is(err, fence.ErrFenced), errors.As(err, &pe), errors.As(err, &se):
			return err
		case ctx.Err() != nil:
			return err
		}
		return transfer.SafeError(err)
	}
}

// exportOf finds the parameters of the export a task runs.
func exportOf(t task.Task) (exportParams, error) { return inputOf[exportParams](t) }

func (o *Ops) exportDefinition() task.Definition {
	return task.Definition{
		Kind: KindTableExport,
		Steps: []task.Step{
			{Name: stepXCheck, Run: xferStep(o.stepExportCheck)},
			{Name: stepXClass, Run: xferStep(o.stepExportClass)},
			{Name: stepXWrite, Interruptible: true, Run: xferStep(o.stepExportWrite)},
			{Name: stepXSeal, Run: xferStep(o.stepExportSeal)},
		},
		OnFail: o.exportOnFail,
	}
}

// loadExport reads the record of the export and checks that it is still ours to work on: the leader that holds the file is this server, the role is the
// one of the record and still has its right. A step that finds otherwise ends the task (design "Leader change and failover").
func (o *Ops) loadExport(ctx context.Context, env *task.Env) (exportParams, transfer.Record, *xferState, error) {
	p, err := exportOf(env.Task)
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

func (o *Ops) stepExportCheck(ctx context.Context, env *task.Env) error {
	p, rec, _, err := o.loadExport(ctx, env)
	if err != nil {
		return err
	}
	if err := o.stillEnabled(ctx); err != nil {
		return err
	}
	// The checks of the request are made again: a function or a type may have been changed in the time the task waited (DX14-05).
	conn, err := o.roleConn(ctx, p.Database, rec.Owner.Name)
	if err != nil {
		return transfer.Errorf(transfer.CodeSessionLost, true, "the role cannot connect to the database from this server now; repeat the operation")
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	var foreign []string
	var facts tableFacts
	if err := inTx(ctx, conn, func(tx pgx.Tx) error {
		var e error
		if facts, e = readFacts(ctx, tx, p.Schema, p.Table); e != nil {
			return e
		}
		if e = eligibleKind(p.Schema, p.Table, facts); e != nil {
			return e
		}
		foreign, e = o.exportColumnsCheck(ctx, tx, p.Schema, p.Table, facts, p.Columns)
		return e
	}); err != nil {
		return transfer.Errorf(transfer.CodeForbidden, false, "the table can no longer be exported as it was requested: %s", transfer.SafeError(err).Error())
	}
	if len(foreign) > 0 && !p.AckForeign && !facts.OwnerIsRole {
		return transfer.Errorf(transfer.CodeForbidden, false, "a column of the table now runs a function owned by another role; request the export again")
	}
	return env.SetDetail(ctx, "transfer "+idText(p.TransferID))
}

func (o *Ops) stepExportClass(ctx context.Context, env *task.Env) error {
	p, rec, _, err := o.loadExport(ctx, env)
	if err != nil {
		return err
	}
	class, groups, err := o.distribution(ctx, p.Database, p.Schema, p.Table)
	if err != nil {
		return err
	}
	var rep exportReport
	_ = json.Unmarshal(rec.Report, &rep)
	rep.Consistency = []api.TableConsistency{consistencyOf(p.Schema, p.Table, class, groups)}
	b, _ := json.Marshal(rep)
	return o.records().Apply(ctx, rec.ID, transfer.Update{Report: b})
}

// sealWriter is the writer under the compression: it counts and hashes what goes into the file and stops an export that passes the largest file.
type sealWriter struct {
	w   *staging.Writer
	h   hash.Hash
	n   int64
	max int64
}

func (s *sealWriter) Write(p []byte) (int, error) {
	if s.max > 0 && s.n+int64(len(p)) > s.max {
		return 0, staging.ErrFileTooLarge
	}
	n, err := s.w.Write(p)
	s.h.Write(p[:n])
	s.n += int64(n)
	return n, err
}

// stagingError maps the errors of the staging store of a running export to the codes of the transfer.
func stagingError(err error) error {
	switch {
	case errors.Is(err, staging.ErrFileTooLarge), errors.Is(err, archive.ErrLimit):
		return transfer.Errorf(transfer.CodeExportTooLarge, false, "the export is larger than the largest staged file")
	case errors.Is(err, staging.ErrQuotaExceeded):
		return transfer.Errorf(transfer.CodeQuotaExceeded, false, "the staging quota is full")
	case errors.Is(err, staging.ErrInsufficientDisk):
		return transfer.Errorf(transfer.CodeInsufficientDisk, false, "the staging area has too little free space")
	}
	return err
}

// exportAttempts is how often a read that a moved bucket or a lost connection interrupted starts again, and the wait between two.
var (
	exportAttempts = 3
	exportRetryGap = 5 * time.Second
)

func (o *Ops) stepExportWrite(ctx context.Context, env *task.Env) error {
	p, rec, x, err := o.loadExport(ctx, env)
	if err != nil {
		return err
	}
	res, err := x.reservation(rec)
	if err != nil {
		return err
	}
	sql, err := transfer.CopyToSQL(p.Schema, p.Table, exportColumns(p), p.CSV, p.Header, p.Only, p.Safe)
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
		// The file is rewritten from the beginning: a file is never part of two attempts.
		_ = x.store.Remove(rec.ID)
		left, terr := o.timeLeft(env.Task)
		if terr != nil {
			return terr
		}
		meta, sealed, sum, rows, err := o.writeExport(ctx, x, rec, res, p, sql, left)
		if err == nil {
			rep := exportReport{}
			_ = json.Unmarshal(rec.Report, &rep)
			cur, gerr := o.records().Get(ctx, rec.ID)
			if gerr == nil {
				_ = json.Unmarshal(cur.Report, &rep)
			}
			rep.Rows = rows
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

// writeExport runs one attempt: the statement in a read-only transaction of the role, the bytes through the compression and the seal.
func (o *Ops) writeExport(ctx context.Context, x *xferState, rec transfer.Record, res *staging.Reservation, p exportParams, sql string, timeout time.Duration) (meta staging.FileMeta, size int64, sum string, rows int64, err error) {
	conn, err := o.roleConn(ctx, p.Database, rec.Owner.Name)
	if err != nil {
		return meta, 0, "", 0, transfer.Errorf(transfer.CodeSessionLost, true, "the role cannot connect to the database from this server now; repeat the operation")
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
	if err != nil {
		return meta, 0, "", 0, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	formats, err := transfer.SessionFormats(p.CSV.Encoding)
	if err != nil {
		return meta, 0, "", 0, err
	}
	for _, s := range append(formats, "SET LOCAL row_security = off", fmt.Sprintf("SET LOCAL statement_timeout = %d", timeout.Milliseconds())) {
		if _, err := tx.Exec(ctx, s); err != nil {
			return meta, 0, "", 0, err
		}
	}
	w, meta, err := x.store.Create(rec.ID, res, x.keys)
	if err != nil {
		return meta, 0, "", 0, err
	}
	sw := &sealWriter{w: w, h: sha256.New(), max: p.MaxFile}
	var out io.Writer = sw
	var zw *gzip.Writer
	if p.Gzip {
		zw = gzip.NewWriter(sw)
		out = zw
	}
	if p.BOM {
		if _, err := out.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil {
			_ = w.Discard()
			return meta, 0, "", 0, err
		}
	}
	tag, err := tx.Conn().PgConn().CopyTo(ctx, out, sql)
	if err != nil {
		_ = w.Discard()
		return meta, 0, "", 0, err
	}
	if zw != nil {
		if err := zw.Close(); err != nil {
			_ = w.Discard()
			return meta, 0, "", 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		_ = w.Discard()
		return meta, 0, "", 0, err
	}
	sealed, err := w.Finish()
	if err != nil {
		return meta, 0, "", 0, err
	}
	return meta, sealed.Plaintext, hex.EncodeToString(sw.h.Sum(nil)), tag.RowsAffected(), nil
}

func (o *Ops) stepExportSeal(ctx context.Context, env *task.Env) error {
	p, rec, x, err := o.loadExport(ctx, env)
	if err != nil {
		return err
	}
	st, size, serr := x.store.State(rec.ID)
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
	_ = size
	if res, rerr := x.reservation(rec); rerr == nil {
		res.Settle()
	}
	ready := transfer.StateReady
	exp := o.transferNow().Add(time.Duration(p.Retention) * time.Hour)
	if err := o.records().Apply(ctx, rec.ID, transfer.Update{State: &ready, ExpiresAt: &exp, Touch: true}); err != nil {
		return err
	}
	if o.Transfer != nil && o.Transfer.Audit != nil {
		var rep exportReport
		_ = json.Unmarshal(cur.Report, &rep)
		o.Transfer.Audit(ctx, "dataTransfer.completed", map[string]any{"kind": "export", "transferId": idText(rec.ID), "rows": rep.Rows, "bytes": *cur.SizeBytes, "sha256": cur.SHA256,
			"consistency": rep.Consistency, "outcome": "success"})
	}
	return nil
}

// exportOnFail removes the partial file and the reservation of a failed or cancelled export at once (the hook of the kind), and says in the audit
// log how it ended.
func (o *Ops) exportOnFail(ctx context.Context, t task.Task) {
	p, err := exportOf(t)
	if err != nil {
		return
	}
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
		o.Transfer.Audit(ctx, "dataTransfer.completed", map[string]any{"kind": "export", "transferId": idText(rec.ID), "outcome": "failure", "code": code})
	}
}

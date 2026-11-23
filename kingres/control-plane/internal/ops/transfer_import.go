// SPDX-License-Identifier: AGPL-3.0-only

package ops

// The task table.import (contract 0.19.0, design "Table import"): a complete upload is loaded into an existing table with COPY ... FROM STDIN, as the
// signed-in role, in ONE explicit transaction: BEGIN, COPY, the count the server reports compared with the number of records the control plane
// counted, the commit intent written under the lease, COMMIT. Nothing is stored unless the two counts are equal. After the intent the outcome is
// unknown until it is observed: a task found with the intent and the commit not recorded fails with import_outcome_unknown and never retries.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sqlx"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/staging"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/transfer"
)

// Step names of table.import.
const (
	stepICheckUpload = "Check the upload"
	stepICheckTable  = "Check the table"
	stepILoad        = "Load rows"
	stepICommit      = "Confirm the commit"
	stepIClean       = "Clean up"
)

// intentMarker is what the detail of the task says once the commit was sent: "commit sent for N rows". The write of it is checked against the lease
// and the epoch by the store, so a leader that lost its lease cannot send the commit.
const intentMarker = "commit sent for "

// MaxAtomicReferenceBytes is the cap of the import into a reference table: it is written on every node in one transaction (design "Reference tables").
const MaxAtomicReferenceBytes = int64(256) << 20

// referenceChunk is the number of rows of one insert into a reference table.
const referenceChunk = 10000

type importParams struct {
	UploadID     int64               `json:"uploadId"`
	Database     string              `json:"database"`
	Schema       string              `json:"schema"`
	Table        string              `json:"table"`
	Columns      []string            `json:"columns"`
	ColumnTypes  []string            `json:"columnTypes,omitempty"`
	CSV          transfer.CSV        `json:"csv"`
	Header       transfer.HeaderMode `json:"header"`
	ForceNotNull []string            `json:"forceNotNull,omitempty"`
	ForceNull    []string            `json:"forceNull,omitempty"`
	Gzip         *bool               `json:"gzip,omitempty"`
	SkipBOM      bool                `json:"skipBom"`
	KeepUpload   bool                `json:"keepUpload"`
	Reference    bool                `json:"reference,omitempty"`
	MaxBytes     int64               `json:"maxBytes"`
	Owner        transfer.Identity   `json:"owner"`
	// MarkerNonce is the random key of the marker of this import (see transfer_marker.go).
	MarkerNonce string `json:"markerNonce"`
}

// ImportFault is the fault point hook of the tests (nil in production): it is called with the name of a point of the commit protocol and the process id of
// the backend of the import, so that a test can end the connection at that point. The points are "beforeStart" (the first step of the task, no connection), "beforeCheck" and "afterCheck" (before and after the table check of the task, no connection), "beforeIntent", "intentWrite", "afterIntent" and "afterCommit".
var ImportFault func(point string, backendPID uint32) error

// StartGap is called by a test between the check of the limits of the transfers and the start of the task, to widen the window that startMu closes.
var StartGap func()

func importFault(point string, conn *pgx.Conn) error {
	if ImportFault != nil {
		var pid uint32
		if conn != nil {
			pid = conn.PgConn().PID()
		}
		return ImportFault(point, pid)
	}
	return nil
}

// ledgerPolicy reads, with the service role, the signature policy of a ledger table ("" when the table is not one).
func (o *Ops) ledgerPolicy(ctx context.Context, db, schema, table string) (string, error) {
	var policy string
	err := o.asService(ctx, db, func(tx pgx.Tx) error {
		var have bool
		if e := tx.QueryRow(ctx, `SELECT to_regclass('kingres.ledger_table') IS NOT NULL`).Scan(&have); e != nil || !have {
			return e
		}
		e := tx.QueryRow(ctx, `SELECT signature_policy FROM kingres.ledger_table WHERE relid = to_regclass($1)`, qualified(schema, table)).Scan(&policy)
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		return e
	})
	return policy, wrap(err)
}

// importFacts checks, through a connection of the role, that the table can take the import and returns the columns the file has and their types.
func (o *Ops) importTarget(ctx context.Context, conn *pgx.Conn, schema, table string, want []string, class string) (cols, types []string, err error) {
	var f tableFacts
	err = inTx(ctx, conn, func(tx pgx.Tx) error {
		var e error
		if f, e = readFacts(ctx, tx, schema, table); e != nil {
			return e
		}
		if e = eligibleKind(schema, table, f); e != nil {
			return e
		}
		if f.RLS {
			return ineligible("The table has row-level security.", blocker("row_level_security", schema+"."+table, "PostgreSQL refuses COPY into a table with row-level security, and the router does too."))
		}
		known := map[string]colInfo{}
		for _, c := range f.Columns {
			known[c.Name] = c
		}
		cols = want
		if len(cols) == 0 {
			for _, c := range f.Columns {
				if !c.Generated {
					cols = append(cols, c.Name)
				}
			}
		}
		var errs []problem.FieldError
		seen := map[string]bool{}
		for i, c := range cols {
			ci, ok := known[c]
			switch {
			case seen[c]:
				errs = append(errs, field(fmt.Sprintf("/columns/%d", i), "is listed twice", "duplicate"))
			case !ok:
				errs = append(errs, field(fmt.Sprintf("/columns/%d", i), "the table has no such column", "not_found"))
			case ci.Generated:
				errs = append(errs, field(fmt.Sprintf("/columns/%d", i), "is a generated column; COPY cannot set it", "forbidden"))
			}
			seen[c] = true
		}
		if len(errs) > 0 {
			return problem.Validation("The import request is invalid.", errs...)
		}
		if len(cols) == 0 {
			return problem.Validation("The import request is invalid.", field("/columns", "the table has no column to load", "required"))
		}
		var cannot []api.TableBlocker
		for _, c := range cols {
			var ok bool
			if e := tx.QueryRow(ctx, `SELECT pg_catalog.has_column_privilege(to_regclass($1), $2, 'INSERT')`, qualified(schema, table), c).Scan(&ok); e != nil {
				return e
			}
			if !ok {
				cannot = append(cannot, blocker("no_privilege", c, "The role has no INSERT privilege on this column."))
			}
		}
		if len(cannot) > 0 {
			return ineligible("The role cannot write every column.", cannot...)
		}
		if class == "reference" {
			// A reference table is written on every node from typed arrays (spike S2, V3); a column that is an array would be flattened by unnest.
			rows, qe := tx.Query(ctx, `SELECT a.attname::text, pg_catalog.format_type(a.atttypid, a.atttypmod), t.typcategory::text = 'A'
				FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_type t ON t.oid = a.atttypid WHERE a.attrelid = to_regclass($1) AND a.attname::text = ANY($2) AND a.attnum > 0 AND NOT a.attisdropped`, qualified(schema, table), cols)
			if qe != nil {
				return qe
			}
			byName := map[string]string{}
			var arrays []api.TableBlocker
			for rows.Next() {
				var n, ft string
				var arr bool
				if e := rows.Scan(&n, &ft, &arr); e != nil {
					rows.Close()
					return e
				}
				byName[n] = ft
				if arr {
					arrays = append(arrays, blocker("array_column", n, "A column of an array type cannot be loaded into a reference table in this version."))
				}
			}
			rows.Close()
			if len(arrays) > 0 {
				return ineligible("The reference table has a column of an array type.", arrays...)
			}
			for _, c := range cols {
				types = append(types, byName[c])
			}
		}
		return nil
	})
	return cols, types, wrapProblem(err)
}

// StartImport answers importTable.
func (o *Ops) StartImport(ctx context.Context, actor, db, schema, table string, req api.ImportTableRequest) (task.Task, error) {
	if _, err := o.xfer(ctx); err != nil {
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
	header := transfer.HeaderMode(req.Header)
	switch header {
	case "":
		header = transfer.HeaderSkip
	case transfer.HeaderNone, transfer.HeaderSkip, transfer.HeaderMatch:
	default:
		errs = append(errs, field("/header", "must be none, skip or match", "unknown_value"))
	}
	if req.SkipBOM != nil && *req.SkipBOM && csv.Encoding != "UTF8" {
		errs = append(errs, field("/skipBom", "removes a UTF-8 byte order mark and needs the encoding UTF8", "conflict"))
	}
	var gz *bool
	switch req.Compression {
	case "":
	case "none", "gzip":
		v := req.Compression == "gzip"
		gz = &v
	default:
		errs = append(errs, field("/compression", "must be none or gzip", "unknown_value"))
	}
	inList := func(name string, list []string) {
		for _, n := range list {
			ok := false
			for _, c := range req.Columns {
				ok = ok || c == n
			}
			if len(req.Columns) > 0 && !ok {
				errs = append(errs, field(name, "names a column that is not in columns", "not_found"))
				return
			}
		}
	}
	inList("/forceNotNull", req.ForceNotNull)
	inList("/forceNull", req.ForceNull)
	if len(req.Columns) > transfer.MaxColumns || len(req.ForceNotNull) > transfer.MaxColumns || len(req.ForceNull) > transfer.MaxColumns {
		errs = append(errs, field("/columns", "has too many columns", "max_items"))
	}
	if len(errs) > 0 {
		return task.Task{}, problem.Validation("The import request is invalid.", errs...)
	}
	who, err := o.identity(ctx, actor)
	if err != nil {
		return task.Task{}, err
	}
	if err := o.roleChecker().Check(ctx, who); err != nil {
		return task.Task{}, transfer.SafeError(err).Problem()
	}
	up, _, err := o.owned(ctx, actor, req.UploadID, transfer.KindUpload)
	if err != nil {
		return task.Task{}, err
	}
	if up.Purpose == "databaseArchive" {
		return task.Task{}, problem.Validation("The upload is a database archive, not a CSV file.", field("/uploadId", "the purpose of the upload is databaseArchive", "conflict"))
	}
	switch up.State {
	case transfer.StateReceiving:
		return task.Task{}, problem.New(problem.UploadIncomplete, "The upload has not received all its bytes.")
	case transfer.StateReady:
	default:
		return task.Task{}, problem.New(problem.NotFound, "There is no such transfer.")
	}
	if o.taskActive(ctx, up) {
		return task.Task{}, problem.New(problem.OperationInProgress, "Another task is using this upload.")
	}
	class, _, err := o.distribution(ctx, db, schema, table)
	if err != nil {
		return task.Task{}, err
	}
	cap := o.maxAtomic()
	if class == "reference" && cap > MaxAtomicReferenceBytes {
		cap = MaxAtomicReferenceBytes
	}
	size := int64(0)
	if up.SizeBytes != nil {
		size = *up.SizeBytes
	}
	plain := gz == nil || !*gz
	if gz == nil {
		if x, xerr := o.xfer(ctx); xerr == nil {
			plain = !o.isGzip(x, up)
		}
	}
	if size > cap && plain {
		return task.Task{}, problem.New(problem.ImportTooLarge, "The file is larger than "+strconv.FormatInt(cap, 10)+" bytes; split the file into parts of at most that size and import them one after the other.")
	}
	policy, err := o.ledgerPolicy(ctx, db, schema, table)
	if err != nil {
		return task.Task{}, err
	}
	if policy == "required" {
		return task.Task{}, problem.New(problem.ImportLedgerPolicy, "The table is a ledger table whose signature policy is required: an unsigned write is refused (KL010), so the first row would fail.")
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
	bad, derr := dangerousRole(ctx, conn)
	if derr != nil {
		return task.Task{}, derr
	}
	if bad {
		return task.Task{}, problem.New(problem.Forbidden, "An import runs the triggers, defaults and checks of the table as the signed-in role, so it is refused for a role with the SUPERUSER, REPLICATION or BYPASSRLS attribute or with access to server files, programs or logs; sign in as another role.")
	}
	cols, types, err := o.importTarget(ctx, conn, schema, table, req.Columns, class)
	if err != nil {
		return task.Task{}, err
	}
	for _, n := range append(append([]string{}, req.ForceNotNull...), req.ForceNull...) {
		found := false
		for _, c := range cols {
			found = found || c == n
		}
		if !found {
			return task.Task{}, problem.Validation("The import request is invalid.", field("/forceNotNull", "names a column that is not loaded", "not_found"))
		}
	}
	skipBOM := (req.SkipBOM == nil || *req.SkipBOM) && csv.Encoding == "UTF8"
	if _, err := transfer.CopyFromSQL(schema, table, cols, csv, header, req.ForceNotNull, req.ForceNull); err != nil {
		return task.Task{}, problem.Validation("The import request is invalid.", field("/columns", "a name is not valid", "pattern"))
	}
	nonce, nerr := newMarkerNonce()
	if nerr != nil {
		return task.Task{}, nerr
	}
	p := importParams{MarkerNonce: nonce, UploadID: up.ID, Database: db, Schema: schema, Table: table, Columns: cols, ColumnTypes: types, CSV: csv, Header: header, ForceNotNull: req.ForceNotNull,
		ForceNull: req.ForceNull, Gzip: gz, SkipBOM: skipBOM, KeepUpload: req.KeepUpload, Reference: class == "reference", MaxBytes: cap, Owner: who}
	t, err := o.StartTask(ctx, KindTableImport, "table", db+"/"+schema+"."+table, p, actor)
	if err != nil {
		return task.Task{}, err
	}
	tid := t.ID
	_ = o.records().Apply(ctx, up.ID, transfer.Update{TaskID: &tid})
	return t, nil
}

// isGzip looks at the first two bytes of a staged file.
func (o *Ops) isGzip(x *xferState, rec transfer.Record) bool {
	dk, err := x.keys.Unwrap(rec.ID, rec.KeyID, rec.WrappedKey)
	if err != nil {
		return false
	}
	r, err := x.store.OpenReader(rec.ID, dk)
	if err != nil {
		return false
	}
	defer func() { _ = r.Close() }()
	var magic [2]byte
	n, _ := r.ReadAt(magic[:], 0)
	return n == 2 && magic[0] == 0x1f && magic[1] == 0x8b
}

// MaxAtomicBytes is the effective cap of one import (the flag of the server), for the settings.
func (o *Ops) MaxAtomicBytes() int64 { return o.maxAtomic() }

func (o *Ops) maxAtomic() int64 {
	if o.Transfer != nil && o.Transfer.MaxAtomicBytes > 0 {
		return o.Transfer.MaxAtomicBytes
	}
	return transfer.AtomicDefault
}

func importOf(t task.Task) (importParams, error) { return inputOf[importParams](t) }

func (o *Ops) importDefinition() task.Definition {
	return task.Definition{
		Kind: KindTableImport,
		Steps: []task.Step{
			{Name: stepICheckUpload, Run: xferStep(o.stepImportCheckUpload)},
			{Name: stepICheckTable, Run: xferStep(o.stepImportCheckTable)},
			{Name: stepILoad, Interruptible: true, Run: xferStep(o.stepImportLoad)},
			{Name: stepICommit, Run: xferStep(o.stepImportCommit)},
			{Name: stepIClean, Run: xferStep(o.stepImportClean)},
		},
		OnFail: o.importOnFail,
		// A cancel is accepted until the commit intent is written (and not at all once the commit step started).
		Cancellable: func(t task.Task) bool {
			if strings.Contains(t.Detail, intentMarker) {
				return false
			}
			switch stepState(t, stepICommit) {
			case task.StepRunning, task.StepDone, task.StepSkipped:
				return false
			}
			return true
		},
	}
}

// loadUpload finds the record of the upload of the task and checks that it is still the caller's, on this server, and complete.
func (o *Ops) loadUpload(ctx context.Context, p importParams) (transfer.Record, *xferState, error) {
	x, err := o.xfer(ctx)
	if err != nil {
		return transfer.Record{}, nil, transfer.Errorf(transfer.CodeLeaderChanged, true, "the staging store of this server is not available; repeat the operation")
	}
	rec, err := o.records().Get(ctx, p.UploadID)
	if err != nil || rec.Owner != p.Owner {
		return rec, x, transfer.Errorf(transfer.CodeLeaderChanged, true, "the upload is not known to this server; repeat the operation")
	}
	if rec.ServerID != o.ServerHost || rec.State != transfer.StateReady {
		return rec, x, transfer.Errorf(transfer.CodeLeaderChanged, true, "the leader changed or the upload is gone; send the file again and repeat the operation")
	}
	if err := o.roleChecker().Check(ctx, rec.Owner); err != nil {
		return rec, x, err
	}
	return rec, x, nil
}

func (o *Ops) stepImportCheckUpload(ctx context.Context, env *task.Env) error {
	p, err := importOf(env.Task)
	if err != nil {
		return err
	}
	_ = importFault("beforeStart", nil)
	if err := o.stillEnabled(ctx); err != nil {
		return err
	}
	rec, x, err := o.loadUpload(ctx, p)
	if err != nil {
		return err
	}
	st, _, serr := x.store.State(rec.ID)
	if serr != nil || st != staging.StateFinal {
		o.setLost(ctx, rec.ID)
		return transfer.Errorf(transfer.CodeLeaderChanged, true, "the file of the upload is gone; send it again and repeat the operation")
	}
	sum, err := o.sumOf(x, rec)
	if err != nil {
		return err
	}
	if sum != rec.SHA256 {
		return transfer.Errorf(transfer.CodeKeyLost, false, "the staged file does not have the checksum it had when it was received")
	}
	return env.SetDetail(ctx, "upload "+idText(rec.ID))
}

func (o *Ops) stepImportCheckTable(ctx context.Context, env *task.Env) error {
	p, err := importOf(env.Task)
	if err != nil {
		return err
	}
	if err := importFault("beforeCheck", nil); err != nil {
		return err
	}
	if _, _, err := o.loadUpload(ctx, p); err != nil {
		return err
	}
	class, _, err := o.distribution(ctx, p.Database, p.Schema, p.Table)
	if err != nil {
		return err
	}
	if (class == "reference") != p.Reference {
		return transfer.Errorf(transfer.CodeLeaderChanged, true, "the table changed its kind since the request; repeat the operation")
	}
	policy, err := o.ledgerPolicy(ctx, p.Database, p.Schema, p.Table)
	if err != nil {
		return err
	}
	if policy == "required" {
		return transfer.Errorf(transfer.CodeForbidden, false, "the ledger policy of the table became required")
	}
	if err := o.ensureMarker(ctx, p.Database, p.Owner.Name); err != nil {
		return err
	}
	conn, err := o.roleConn(ctx, p.Database, p.Owner.Name)
	if err != nil {
		return transfer.Errorf(transfer.CodeSessionLost, true, "the role cannot connect to the database from this server now; repeat the operation")
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	cols, types, err := o.importTarget(ctx, conn, p.Schema, p.Table, p.Columns, class)
	if err != nil {
		var pr *problem.Problem
		if errors.As(err, &pr) {
			return transfer.Errorf(transfer.CodeForbidden, false, "the table no longer accepts this import (%s)", pr.Title)
		}
		return err
	}
	if len(cols) != len(p.Columns) {
		return transfer.Errorf(transfer.CodeForbidden, false, "the columns of the table changed since the request")
	}
	for i := range types {
		if i >= len(p.ColumnTypes) || types[i] != p.ColumnTypes[i] {
			return transfer.Errorf(transfer.CodeForbidden, false, "the type of a column of the table changed since the request")
		}
	}
	_ = importFault("afterCheck", nil)
	return nil
}

// reader builds the pipeline of the file: the sealed segments, the decompression (counted against the limit), the BOM and the scanner.
func (o *Ops) importReader(x *xferState, rec transfer.Record, p importParams) (io.Reader, *transfer.Scanner, *errRecorder, func(), error) {
	dk, err := x.keys.Unwrap(rec.ID, rec.KeyID, rec.WrappedKey)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	r, err := x.store.OpenReader(rec.ID, dk)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	var src io.Reader = r
	gz := p.Gzip != nil && *p.Gzip
	if p.Gzip == nil {
		var magic [2]byte
		n, _ := r.ReadAt(magic[:], 0)
		gz = n == 2 && magic[0] == 0x1f && magic[1] == 0x8b
	}
	if gz {
		src, err = transfer.Gunzip(r)
		if err != nil {
			_ = r.Close()
			return nil, nil, nil, nil, transfer.Errorf(transfer.CodeSQLError, false, "the file is not a valid gzip stream")
		}
	}
	src = transfer.LimitedExpansion(src, p.MaxBytes)
	if p.SkipBOM {
		src = transfer.StripBOM(src)
	}
	// The first failure of the stream under the scanner (a gzip bomb, a bad gzip stream) is kept: the server answers a stream that failed with its own
	// text, and the cause is what the control plane knows.
	rec2 := &errRecorder{r: src}
	sc := transfer.NewScanner(rec2, p.CSV.QuoteByte())
	return sc, sc, rec2, func() { _ = r.Close() }, nil
}

// errRecorder keeps the first error of a reader that is not the end of the stream.
type errRecorder struct {
	r   io.Reader
	err error
}

func (e *errRecorder) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err != nil && err != io.EOF && e.err == nil {
		e.err = err
	}
	return n, err
}

// progressReader reports the bytes read, every 16 MiB, through the detail of the task.
type progressReader struct {
	r     io.Reader
	n     int64
	next  int64
	every int64
	note  func(n int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.n += int64(n)
	if p.n >= p.next {
		p.next = p.n + p.every
		p.note(p.n)
	}
	return n, err
}

// resolveAfterIntent turns the question "did it commit" into the answer of the task: done when the marker is there, a retryable failure with the proof when it
// is not, an unknown outcome when it cannot be asked.
func (o *Ops) resolveAfterIntent(ctx context.Context, env *task.Env, p importParams) error {
	// The marker is in the database that this server loaded into. After a change of leader the answer is not in reach: say so, and never claim that nothing was stored.
	if up, gerr := o.records().Get(ctx, p.UploadID); gerr != nil || up.ServerID != o.ServerHost {
		return o.unknownOutcome(env.Task)
	}
	committed, rows, err := o.decideOutcome(ctx, p, env.Task.ID)
	switch {
	case err != nil:
		return o.unknownOutcome(env.Task)
	case committed:
		return env.SetDetail(ctx, intentMarker+strconv.FormatInt(rows, 10)+" rows; committed")
	}
	return transfer.Errorf(transfer.CodeCopyInterrupted, true, "the commit did not happen: the marker of the task in the database proves that nothing was stored; repeat the operation")
}

func (o *Ops) stepImportLoad(ctx context.Context, env *task.Env) error {
	p, err := importOf(env.Task)
	if err != nil {
		return err
	}
	// A run that finds the intent never loads again: the marker in the database says whether the rows are in.
	if strings.Contains(env.Task.Detail, intentMarker) {
		return o.resolveAfterIntent(ctx, env, p)
	}
	rec, x, err := o.loadUpload(ctx, p)
	if err != nil {
		return err
	}
	left, err := o.timeLeft(env.Task)
	if err != nil {
		return err
	}
	src, sc, under, closeSrc, err := o.importReader(x, rec, p)
	if err != nil {
		return err
	}
	defer closeSrc()
	conn, err := o.roleConn(ctx, p.Database, p.Owner.Name)
	if err != nil {
		return transfer.Errorf(transfer.CodeSessionLost, true, "the role cannot connect to the database from this server now; repeat the operation")
	}
	// The connection and its transaction belong to this run and end with it, whatever happens (a lease that is lost stands the run down without a hook).
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	bad, derr := dangerousRole(ctx, conn)
	if derr != nil {
		return transfer.Errorf(transfer.CodeSessionLost, true, "the check of the role could not be made now; repeat the operation")
	}
	secret, serr := o.markerSecret(ctx, p.Database)
	if errors.Is(serr, errMarkerNotVerified) {
		return transfer.Errorf(transfer.CodeForbidden, false, "the schema kingres_transfer of the database is not what the control plane makes; an operator must remove it")
	}
	if serr != nil {
		return transfer.Errorf(transfer.CodeSessionLost, true, "the marker of the database could not be read now; repeat the operation")
	}
	if bad {
		return transfer.Errorf(transfer.CodeForbidden, false, "the role has the SUPERUSER, REPLICATION or BYPASSRLS attribute or access to server files, programs or logs; an import runs the triggers and defaults of the table as that role")
	}
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	formats, err := transfer.SessionFormats(p.CSV.Encoding)
	if err != nil {
		return err
	}
	// A transaction that is left open (a process that stalls) is ended by the server after the time that is left of the task.
	for _, s := range append(formats, fmt.Sprintf("SET LOCAL statement_timeout = %d", left.Milliseconds()), fmt.Sprintf("SET LOCAL idle_in_transaction_session_timeout = %d", left.Milliseconds())) {
		if _, err := tx.Exec(ctx, s); err != nil {
			return err
		}
	}
	total := rec.SizeBytes
	pr := &progressReader{r: src, every: 16 << 20, next: 16 << 20, note: func(n int64) {
		msg := "loaded " + strconv.FormatInt(n, 10) + " bytes"
		if total != nil {
			msg += " of " + strconv.FormatInt(*total, 10)
		}
		_ = env.SetDetail(ctx, msg)
	}}
	var tag int64
	if p.Reference {
		tag, err = o.loadReference(ctx, tx, p, pr, sc)
	} else {
		sql, serr := transfer.CopyFromSQL(p.Schema, p.Table, p.Columns, p.CSV, p.Header, p.ForceNotNull, p.ForceNull)
		if serr != nil {
			return serr
		}
		ct, cerr := tx.Conn().PgConn().CopyFrom(ctx, pr, sql)
		err = cerr
		if err == nil {
			tag = ct.RowsAffected()
		}
	}
	if err != nil {
		return o.importError(ctx, env.Task, p, sc, under, err)
	}
	want := sc.Records()
	if p.Header != transfer.HeaderNone && want > 0 {
		want--
	}
	if tag != want {
		return transfer.Errorf(transfer.CodeRowCountMismatch, false, "the database loaded %d rows and the file has %d records; nothing was stored", tag, want)
	}
	// The role is checked again before the commit: a role that lost its right ends the task with nothing stored.
	if err := o.roleChecker().Check(ctx, p.Owner); err != nil {
		return err
	}
	_ = importFault("beforeIntent", conn)
	// The marker commits with the rows or not at all. A second marker of the same task (a run that raced this one) fails here, and nothing commits.
	if _, err := tx.Exec(ctx, "SELECT kingres_transfer.mark($1, $2, $3, $4)", p.MarkerNonce, env.Task.ID, tag, markerTag(secret, p.MarkerNonce, env.Task.ID, tag)); err != nil {
		return transfer.Errorf(transfer.CodeCopyInterrupted, true, "the marker of the import could not be written; nothing was stored, repeat the operation")
	}
	// A connection that broke since the load: nothing was committed, the transaction died with it, and the import may be repeated.
	if _, err := tx.Exec(ctx, "SELECT 1"); err != nil {
		return transfer.Errorf(transfer.CodeCopyInterrupted, true, "the connection to the database broke before the commit; nothing was stored, repeat the operation")
	}
	// The intent is written under the lease before COMMIT is sent: a leader that lost its lease cannot record it, so it cannot commit.
	werr := importFault("intentWrite", conn)
	if werr == nil {
		werr = env.SetDetail(ctx, intentMarker+strconv.FormatInt(tag, 10)+" rows")
	}
	if werr != nil {
		return werr
	}
	_ = importFault("afterIntent", conn)
	// After the intent there is no cancel: the commit is not tied to the context of the request.
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
	defer cancel()
	if err := tx.Commit(bg); err != nil {
		// The answer of the server is lost or the commit failed: the marker is the proof, asked on another connection.
		_ = conn.Close(context.WithoutCancel(ctx))
		return o.resolveAfterIntent(bg, env, p)
	}
	// A fault point for the crash between the commit and the record of the step (the lease lost, the process ended): the run that comes next asks the marker.
	if err := importFault("afterCommit", conn); err != nil {
		return err
	}
	return env.SetDetail(ctx, intentMarker+strconv.FormatInt(tag, 10)+" rows; committed")
}

// importError maps what the load ended with: the scanner's own refusals, the header, the limit, and anything else through SafeError.
func (o *Ops) importError(ctx context.Context, t task.Task, p importParams, sc *transfer.Scanner, under *errRecorder, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var em *transfer.ErrEndMarker
	var rl *transfer.ErrRecordTooLarge
	switch {
	case errors.As(err, &em), errors.As(sc.Err(), &em):
		return &transfer.Error{Code: transfer.CodeImportEndMarker, Sentence: "the file has a line that is only backslash and dot outside quotes", Line: int(em.Line)}
	case errors.As(err, &rl), errors.As(sc.Err(), &rl):
		return &transfer.Error{Code: transfer.CodeRecordTooLarge, Sentence: "a record is larger than 16 MiB", Line: int(rl.Line)}
	case errors.Is(err, transfer.ErrExpansion), errors.Is(sc.Err(), transfer.ErrExpansion), errors.Is(under.err, transfer.ErrExpansion):
		return transfer.Errorf(transfer.CodeImportTooLarge, false, "the file is larger than %d bytes once decompressed; split it", p.MaxBytes)
	}
	te := transfer.SafeError(err)
	if p.Header == transfer.HeaderMatch && te.SQLState == "22P04" && te.Line <= 1 {
		return transfer.Errorf(transfer.CodeImportHeader, false, "the header of the file does not match the columns")
	}
	if te.SQLState == "57014" {
		if _, terr := o.timeLeft(t); terr != nil {
			return terr
		}
	}
	return te.ForTarget(p.Schema, p.Table)
}

func (o *Ops) unknownOutcome(t task.Task) error {
	n := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(t.Detail[strings.Index(t.Detail, intentMarker):], intentMarker), "rows"))
	if i := strings.IndexAny(n, " ,"); i > 0 {
		n = n[:i]
	}
	if _, err := strconv.ParseInt(n, 10, 64); err != nil {
		n = "some"
	}
	return transfer.Errorf(transfer.CodeOutcomeUnknown, false, "the commit was sent for %s rows and its outcome is not known: the rows may have been loaded; check the table before you repeat the import", n)
}

// loadReference loads the file into a reference table: COPY into a temporary table of the same columns (so PostgreSQL parses the CSV with the same
// options), then inserts from typed arrays in chunks of 10 000 rows, all in the open transaction. The router refuses a cast of text in the select list of
// an insert into a reference table, and accepts typed parameters (spike S2, V3). It returns the number of rows inserted.
func (o *Ops) loadReference(ctx context.Context, tx pgx.Tx, p importParams, src io.Reader, sc *transfer.Scanner) (int64, error) {
	rel, err := sqlx.QualifiedIdent(p.Schema, p.Table)
	if err != nil {
		return 0, err
	}
	var colList []string
	for _, c := range p.Columns {
		id, ierr := sqlx.NewIdent(c)
		if ierr != nil {
			return 0, ierr
		}
		colList = append(colList, id.String())
	}
	cl := strings.Join(colList, ", ")
	if _, err := tx.Exec(ctx, "CREATE TEMPORARY TABLE kx_import ON COMMIT DROP AS SELECT "+cl+" FROM "+rel.String()+" WITH NO DATA"); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, "ALTER TABLE kx_import ADD COLUMN kx_n bigserial"); err != nil {
		return 0, err
	}
	// The temporary table takes the file with the options of the request (a header of a file is read as the request says).
	sql, err := transfer.CopyFromSQL("pg_temp", "kx_import", p.Columns, p.CSV, p.Header, p.ForceNotNull, p.ForceNull)
	if err != nil {
		return 0, err
	}
	ct, err := tx.Conn().PgConn().CopyFrom(ctx, src, sql)
	if err != nil {
		return 0, err
	}
	n := ct.RowsAffected()
	var sel, params, aliases []string
	for i, c := range colList {
		sel = append(sel, "array_agg("+c+" ORDER BY kx_n)::text")
		params = append(params, "$"+strconv.Itoa(i+1)+"::"+p.ColumnTypes[i]+"[]")
		aliases = append(aliases, "c"+strconv.Itoa(i))
	}
	read := "SELECT " + strings.Join(sel, ", ") + " FROM kx_import WHERE kx_n > $1 AND kx_n <= $2"
	ins := "INSERT INTO " + rel.String() + " (" + cl + ") SELECT u." + strings.Join(aliases, ", u.") + " FROM unnest(" + strings.Join(params, ", ") + ") AS u(" + strings.Join(aliases, ", ") + ")"
	var done int64
	for from := int64(0); from < n; from += referenceChunk {
		vals := make([]*string, len(colList))
		args := make([]any, len(colList))
		dst := make([]any, len(colList))
		for i := range vals {
			dst[i] = &vals[i]
		}
		if err := tx.QueryRow(ctx, read, from, from+referenceChunk).Scan(dst...); err != nil {
			return 0, err
		}
		for i := range vals {
			args[i] = vals[i]
		}
		tag, err := tx.Exec(ctx, ins, args...)
		if err != nil {
			return 0, err
		}
		done += tag.RowsAffected()
	}
	if done != n {
		return 0, transfer.Errorf(transfer.CodeRowCountMismatch, false, "the insert into the reference table wrote %d of %d rows; nothing was stored", done, n)
	}
	_ = sc
	return n, nil
}

// stepImportCommit confirms the commit from the marker in the database, which is the proof (it committed with the rows). A task that gets here has done the
// load step, which ends only with the commit done or with an error; the marker says how many rows. Nothing is claimed that the marker does not say.
func (o *Ops) stepImportCommit(ctx context.Context, env *task.Env) error {
	p, err := importOf(env.Task)
	if err != nil {
		return err
	}
	var state string
	var rows int64
	if err := o.asService(ctx, p.Database, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT state, rows FROM "+markerTable+" WHERE nonce = $1", p.MarkerNonce).Scan(&state, &rows)
	}); err != nil || state != "committed" {
		return o.unknownOutcome(env.Task)
	}
	if o.Transfer != nil && o.Transfer.Audit != nil {
		o.Transfer.Audit(ctx, "dataTransfer.completed", map[string]any{"kind": "import", "uploadId": idText(p.UploadID), "rows": rows, "commitIntentWritten": true, "outcome": "success"})
	}
	return env.SetDetail(ctx, "committed "+strconv.FormatInt(rows, 10)+" rows")
}

func (o *Ops) stepImportClean(ctx context.Context, env *task.Env) error {
	p, err := importOf(env.Task)
	if err != nil {
		return err
	}
	if p.KeepUpload {
		return nil
	}
	x, err := o.xfer(ctx)
	if err != nil {
		return nil
	}
	if rec, gerr := o.records().Get(ctx, p.UploadID); gerr == nil && rec.State == transfer.StateReady {
		o.removeRecord(ctx, x, rec)
	}
	return nil
}

// importOnFail rolls back an open transaction of a task that failed or was cancelled and keeps the upload (until its retention time) so that the import
// can be tried again; it writes how the task ended to the audit log.
func (o *Ops) importOnFail(ctx context.Context, t task.Task) {
	p, err := importOf(t)
	if err != nil {
		return
	}
	if o.Transfer != nil && o.Transfer.Audit != nil {
		code := ""
		if t.Error != nil {
			code = t.Error.Code
		}
		o.Transfer.Audit(ctx, "dataTransfer.completed", map[string]any{"kind": "import", "uploadId": idText(p.UploadID), "outcome": "failure", "code": code,
			"commitIntentWritten": strings.Contains(t.Detail, intentMarker)})
	}
}

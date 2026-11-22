// SPDX-License-Identifier: AGPL-3.0-only

package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sqlx"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// The task table.setStorage (phase 12, analytics-phase12-storage.md section 13.1): a table or one partition of a range-partitioned table is converted
// between the heap and the columnar format.
//
// A plain table is converted with ALTER TABLE ... SET ACCESS METHOD, which rewrites it under an ACCESS EXCLUSIVE lock (the step says so). The step asks for
// the lock with a short lock_timeout and tries again while it says what it waits for, so that a long transaction of another session does not make every
// reader of the table queue behind a conversion that waits for it. A partition is converted with kingres.convert_partition of kingres_timepart: it builds
// the new partition aside and swaps it in, so writers wait only for the swap. A table that is columnar already gets the new options for the stripes written
// from then on (columnar_set_options).
//
// Both run with kingres_timepart.trust_table_owners on, as partition.maintain does (trustedConn): one rule for what drives the DDL of the extension.
//
// The task runs on the leader and converts the table in the instance of this server: a table of another group's instance is not found here.

// KindSetStorage is the task kind.
const KindSetStorage = "table.setStorage"

// The names of the steps (shown to users).
const (
	stepStorageCheckTable = "Check the table"
	stepStorageCheckPart  = "Check the partition"
	stepStorageConvert    = "Convert the storage"
	stepStorageConvertP   = "Convert the partition"
	stepStorageAnalyze    = "Analyze the table"
	stepStorageAnalyzeP   = "Analyze the partition"
)

// Timings that tests shorten: the lock_timeout of one try, the pause between tries, and how long a step tries before it fails with operation_in_progress.
var (
	storageLockTimeout = 3 * time.Second
	storageLockPause   = 2 * time.Second
	storageLockLimit   = 10 * time.Minute
)

// storageParams is the input of the task.
type storageParams struct {
	Database string `json:"database"`
	Schema   string `json:"schema"`
	Table    string `json:"table"`
	// Partition is the name of the partition (in the schema of the table); empty for a table.
	Partition        string   `json:"partition,omitempty"`
	AccessMethod     string   `json:"accessMethod"`
	Compression      string   `json:"compression,omitempty"`
	CompressionLevel int      `json:"compressionLevel,omitempty"`
	StripeRowCount   int      `json:"stripeRowCount,omitempty"`
	ChunkRowCount    int      `json:"chunkRowCount,omitempty"`
	SortBy           []string `json:"sortBy,omitempty"`
	Actor            string   `json:"actor"`
}

// rel is the table or partition that is converted, as text that PostgreSQL reads as a regclass.
func (p storageParams) rel() string {
	if p.Partition != "" {
		return qualified(p.Schema, p.Partition)
	}
	return qualified(p.Schema, p.Table)
}

// optionsJSON is the options argument of convert_partition (its keys are the arguments of columnar_set_options).
func (p storageParams) optionsJSON() any {
	m := map[string]any{}
	if p.Compression != "" {
		m["compression"] = p.Compression
	}
	if p.CompressionLevel != 0 {
		m["compression_level"] = p.CompressionLevel
	}
	if p.StripeRowCount != 0 {
		m["stripe_row_count"] = p.StripeRowCount
	}
	if p.ChunkRowCount != 0 {
		m["chunk_row_count"] = p.ChunkRowCount
	}
	if len(m) == 0 {
		return nil
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func (o *Ops) storageDefinition() task.Definition {
	return task.Definition{Kind: KindSetStorage, Dynamic: func(in json.RawMessage) ([]task.Step, error) {
		p, err := rawInput[storageParams](in)
		if err != nil {
			return nil, err
		}
		if p.Partition != "" {
			return []task.Step{
				{Name: stepStorageCheckPart, Run: o.stepStorageCheck},
				{Name: stepStorageConvertP, Interruptible: true, Run: o.stepStorageConvert},
				{Name: stepStorageAnalyzeP, Run: o.stepStorageAnalyze},
			}, nil
		}
		return []task.Step{
			{Name: stepStorageCheckTable, Run: o.stepStorageCheck},
			{Name: stepStorageConvert, Interruptible: true, Run: o.stepStorageConvert},
			{Name: stepStorageAnalyze, Run: o.stepStorageAnalyze},
		}, nil
	}}
}

var identRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]{0,62}$`)

// validateStorage checks the request and fills the parameters; partition says whether it is the request of setPartitionStorage.
func validateStorage(req api.TableStorageRequest, partition bool) (storageParams, error) {
	var p storageParams
	var errs []problem.FieldError
	switch req.AccessMethod {
	case "heap", "columnar":
		p.AccessMethod = req.AccessMethod
	case "":
		errs = append(errs, field("/accessMethod", "is required", "required"))
	default:
		errs = append(errs, field("/accessMethod", "must be heap or columnar", "enum"))
	}
	hasOptions := req.Compression != "" || req.CompressionLevel != nil || req.StripeRowCount != nil || req.ChunkRowCount != nil
	if hasOptions && req.AccessMethod == "heap" {
		errs = append(errs, field("/accessMethod", "the columnar options need the columnar format", "conflict"))
	}
	switch req.Compression {
	case "", "none", "pglz", "lz4", "zstd":
		p.Compression = req.Compression
	default:
		errs = append(errs, field("/compression", "must be none, pglz, lz4 or zstd", "enum"))
	}
	ranged := func(f string, v *int, lo, hi int) int {
		if v == nil {
			return 0
		}
		if *v < lo || *v > hi {
			errs = append(errs, field(f, fmt.Sprintf("must be between %d and %d", lo, hi), "out_of_range"))
		}
		return *v
	}
	p.CompressionLevel = ranged("/compressionLevel", req.CompressionLevel, 1, 19)
	p.StripeRowCount = ranged("/stripeRowCount", req.StripeRowCount, 1000, 10000000)
	p.ChunkRowCount = ranged("/chunkRowCount", req.ChunkRowCount, 100, 100000)
	if len(req.SortBy) > 0 {
		if !partition {
			errs = append(errs, field("/sortBy", "is only valid for a partition", "conflict"))
		} else if req.AccessMethod == "heap" {
			errs = append(errs, field("/sortBy", "needs the columnar format", "conflict"))
		}
	}
	if len(req.SortBy) > 16 {
		errs = append(errs, field("/sortBy", "at most 16 columns", "out_of_range"))
	}
	for i, c := range req.SortBy {
		if !identRE.MatchString(c) {
			errs = append(errs, field(fmt.Sprintf("/sortBy/%d", i), "is not a column name", "pattern"))
		}
	}
	p.SortBy = req.SortBy
	if len(errs) > 0 {
		return p, problem.Validation("The storage request is invalid.", errs...)
	}
	return p, nil
}

// PrepareSetStorage validates a storage request for a table (partition empty) or one partition and returns the task input. It refuses what the
// task would refuse as its first step, so that no task exists for it: a table that does not exist, a library that is not preloaded, a conversion
// that is not possible (409 conversion_refused with the reasons as Problem.blockers).
func (o *Ops) PrepareSetStorage(ctx context.Context, actor, db, schema, table, partition string, req api.TableStorageRequest) (any, error) {
	p, _, err := o.checkSetStorage(ctx, actor, db, schema, table, partition, req, false)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// PreviewSetStorage is the answer of a storage request with dryRun=true: what the request would do and why it would be refused, as typed reasons. It starts
// nothing and installs nothing. A request that is not valid, a table that is not there and a library that is not preloaded are the same problems as for the
// real request; the reasons of a conversion that cannot be done are in the answer and not a 409.
func (o *Ops) PreviewSetStorage(ctx context.Context, actor, db, schema, table, partition string, req api.TableStorageRequest) (api.TableStoragePreview, error) {
	prev := api.TableStoragePreview{Blockers: []api.TableBlocker{}, Notes: []string{}}
	p, info, err := o.checkSetStorage(ctx, actor, db, schema, table, partition, req, true)
	var pr *problem.Problem
	switch {
	case errors.As(err, &pr) && pr.Code == problem.ConversionRefused:
		if bl, ok := pr.Blockers.([]api.TableBlocker); ok {
			prev.Blockers = bl
		}
		p = storageParams{AccessMethod: req.AccessMethod}
	case err != nil:
		return prev, err
	}
	prev.Target = p.AccessMethod
	prev.Notes = append(prev.Notes, info.notes...)
	if info.known {
		cur := info.am
		if info.kind == "p" {
			prev.Current = nil
		} else {
			prev.Current = &cur
		}
	}
	prev.WillConvert = len(prev.Blockers) == 0 && !info.same
	switch {
	case info.same:
		prev.Notes = append(prev.Notes, p.what()+" is stored as "+p.AccessMethod+" already.")
	case info.known && len(prev.Blockers) == 0 && p.Partition == "":
		prev.Notes = append(prev.Notes, "ALTER TABLE ... SET ACCESS METHOD rewrites the table under an ACCESS EXCLUSIVE lock: readers and writers wait until it ends.")
	case info.known && len(prev.Blockers) == 0:
		prev.Notes = append(prev.Notes, "The new partition is built aside and swapped in: writers of the partition wait only while it is swapped.")
	}
	if info.sameWithOptions {
		prev.Notes = append(prev.Notes, "The options are set for the stripes that are written from now on; the stripes that exist keep theirs.")
		prev.WillConvert = true
	}
	return prev, nil
}

// checkSetStorage is what PrepareSetStorage and PreviewSetStorage share.
func (o *Ops) checkSetStorage(ctx context.Context, actor, db, schema, table, partition string, req api.TableStorageRequest, dry bool) (storageParams, storageInfo, error) {
	if err := checkNames(db, schema, table); err != nil {
		return storageParams{}, storageInfo{}, err
	}
	if partition != "" {
		if _, err := sqlx.QualifiedIdent(schema, partition); err != nil {
			return storageParams{}, storageInfo{}, problem.New(problem.NotFound, "No such partition.")
		}
	}
	p, err := validateStorage(req, partition != "")
	if err != nil {
		return p, storageInfo{}, err
	}
	p.Database, p.Schema, p.Table, p.Partition, p.Actor = db, schema, table, partition, actor
	if o.maintainRunning(ctx, db, schema, table) {
		return p, storageInfo{}, problem.New(problem.OperationInProgress, "A partition.maintain task is running for the table; convert when it has ended.")
	}
	if err := o.ensureStorageExtensions(ctx, p, !dry); err != nil {
		return p, storageInfo{}, err
	}
	if dry && partition != "" {
		// the refusals of a partition come from a function of kingres_timepart: without the extension a preview can only say that it will be installed
		if v, err := o.ext().Installed(ctx, db, ExtTimepart); err == nil && v == "" {
			return p, storageInfo{notes: []string{"kingres_columnar and kingres_timepart are installed in this database when the conversion is requested; the reasons of a refusal are known after that."}}, nil
		}
	}
	blockers, info, err := o.storageRefusals(ctx, p)
	if err != nil {
		return p, info, err
	}
	if len(blockers) > 0 {
		return p, info, conversionRefused(blockers)
	}
	return p, info, nil
}

// conversionRefused is the 409 with the reasons as text and as Problem.blockers.
func conversionRefused(b []api.TableBlocker) error {
	parts := make([]string, len(b))
	for i, x := range b {
		parts[i] = x.Message
	}
	return problem.New(problem.ConversionRefused, "The storage format cannot be changed: "+strings.Join(parts, "; ")+".").WithBlockers(b)
}

// ensureStorageExtensions makes sure that this server's database has what a conversion needs: kingres_columnar and, for a partition,
// kingres_timepart are installed (they are installed when the database is made; an older database gets them here, at the first use), and the library
// of kingres_columnar is preloaded, on this server and on every other server of the cluster (a conversion to columnar writes WAL of the resource manager that every
// standby must be able to replay; the remedy for a server that has not loaded it is to restart its instance, standbys first).
func (o *Ops) ensureStorageExtensions(ctx context.Context, p storageParams, install bool) error {
	need := []string{ExtColumnar}
	if p.Partition != "" {
		need = append(need, ExtTimepart)
	}
	if err := o.ensureExtensionsOpt(ctx, p.Database, need, true, install); err != nil {
		return err
	}
	if p.AccessMethod != "columnar" {
		return nil
	}
	for _, s := range o.others(ctx) {
		if o.Fanout == nil {
			break
		}
		err := o.Fanout.Apply(ctx, s.ID, ApplyExtensionCheck, extensionOp{Database: p.Database, Extension: ExtColumnar, Preload: true})
		if err == nil {
			continue
		}
		if e, ok := asExtError(s.ID, err).(*extError); ok {
			return problem.New(e.code, e.msg+" The library must be preloaded on every server before a table is converted; restart the instance of that server (standbys first).")
		}
		return wrap(err)
	}
	return nil
}

// ensureExtensions installs the extensions in the database of this server when they are not (an older database), and checks the preload of
// kingres_columnar when preload is true. The problem is 409 extension_missing and names the extension and this server.
func (o *Ops) ensureExtensions(ctx context.Context, db string, exts []string, preload bool) error {
	return o.ensureExtensionsOpt(ctx, db, exts, preload, true)
}

// ensureExtensionsOpt is ensureExtensions that installs only when install is true: a dry run looks and changes nothing.
func (o *Ops) ensureExtensionsOpt(ctx context.Context, db string, exts []string, preload, install bool) error {
	b := o.ext()
	for _, e := range exts {
		v, err := b.Available(ctx, db, e)
		if err != nil {
			return connectProblem(err)
		}
		if v == "" {
			return problem.New(problem.ExtensionMissing, fmt.Sprintf("The package of server %s does not contain %s.", o.ServerHost, e))
		}
		if install {
			if err := b.Install(ctx, db, e); err != nil {
				return wrap(err)
			}
		}
		if e == ExtColumnar && preload {
			ok, err := b.Preloaded(ctx, e)
			if err != nil {
				return wrap(err)
			}
			if !ok {
				return problem.New(problem.ExtensionMissing, fmt.Sprintf("The library %s is not loaded by server %s: its instance was started before the library was added to shared_preload_libraries. "+
					"Restart the instance of that server (the setting is managed by kingresd and cannot be edited), standbys first, then try again.", e, o.ServerHost))
			}
		}
	}
	return nil
}

// storageInfo is what the checks learned about the table or partition.
type storageInfo struct {
	known bool
	kind  string // r or p
	am    string
	// same: the format is the requested one and there is nothing to do; sameWithOptions: the format is the requested one (columnar) and options were given.
	same, sameWithOptions bool
	notes                 []string
}

// storageRefusals lists the reasons why the conversion cannot be done now, as typed blockers; none means that it can. A table or partition that already
// has the format is not refused.
func (o *Ops) storageRefusals(ctx context.Context, p storageParams) ([]api.TableBlocker, storageInfo, error) {
	var blockers []api.TableBlocker
	var info storageInfo
	err := o.policyTx(ctx, p.Database, func(tx pgx.Tx) error {
		var kind, am string
		var isPart bool
		var parentOK bool
		err := tx.QueryRow(ctx, `SELECT c.relkind::text, coalesce(a.amname, ''), c.relispartition,
			CASE WHEN $2::text = '' THEN true ELSE EXISTS (SELECT 1 FROM pg_inherits i WHERE i.inhrelid = c.oid AND i.inhparent = $2::text::regclass) END
			FROM pg_class c LEFT JOIN pg_am a ON a.oid = c.relam WHERE c.oid = $1::regclass`, p.rel(), parentOf(p)).Scan(&kind, &am, &isPart, &parentOK)
		if err != nil {
			var pe *pgconn.PgError
			if errors.Is(err, pgx.ErrNoRows) || (errors.As(err, &pe) && (pe.Code == "42P01" || pe.Code == "3F000")) {
				return notOnThisInstance(p.Schema, firstNonEmpty(p.Partition, p.Table))
			}
			return err
		}
		info = storageInfo{known: true, kind: kind, am: am}
		if p.Partition != "" && !parentOK {
			return problem.New(problem.NotFound, fmt.Sprintf("%s.%s is not a partition of %s.%s (a partition is addressed in the schema of its table).", p.Schema, p.Partition, p.Schema, p.Table))
		}
		if am == p.AccessMethod && (p.AccessMethod == "heap" || !p.hasOptions()) {
			info.same = true
			return nil // nothing to do
		}
		info.sameWithOptions = am == p.AccessMethod
		add := func(code, object, msg string) {
			blockers = append(blockers, api.TableBlocker{Code: code, Object: object, Message: msg})
		}
		obj := p.Schema + "." + firstNonEmpty(p.Partition, p.Table)
		switch {
		case kind == "p":
			add("partitioned_table", obj, "a partitioned table has no storage of its own, convert its partitions")
		case p.Partition == "" && isPart:
			add("partition", obj, "this is a partition, use the operation for a partition")
		}
		if len(blockers) > 0 {
			return nil
		}
		if p.Partition == "" {
			blockers = append(blockers, tableConversionReasons(ctx, tx, p)...)
			return nil
		}
		var r *string
		if err := tx.QueryRow(ctx, `SELECT kingres.timepart__convert_refusal($1::text::regclass::oid, $2::text::regclass::oid, $3::name, $4::text[], $5::jsonb)`,
			qualified(p.Schema, p.Table), p.rel(), p.AccessMethod, nullableList(p.SortBy), p.optionsJSON()).Scan(&r); err != nil {
			return err
		}
		if r != nil && *r != "" {
			add("other", obj, *r)
		}
		return nil
	})
	return blockers, info, wrap(err)
}

func nullableList(l []string) any {
	if len(l) == 0 {
		return nil
	}
	return l
}

func (p storageParams) hasOptions() bool {
	return p.Compression != "" || p.CompressionLevel != 0 || p.StripeRowCount != 0 || p.ChunkRowCount != 0 || len(p.SortBy) > 0
}

func (p storageParams) what() string {
	if p.Partition != "" {
		return "Partition " + p.Schema + "." + p.Partition
	}
	return "Table " + p.Schema + "." + p.Table
}

func parentOf(p storageParams) string {
	if p.Partition == "" {
		return ""
	}
	return qualified(p.Schema, p.Table)
}

// tableConversionReasons are the reasons that stop a plain table from being converted that the catalog shows: a ledger table (its history is
// append-only and hash-chained), a distributed or reference table (they are converted through their partitions, and the sharding extension owns
// their storage), a table of a publication (the columnar format has no logical decoding of updates), and foreign keys.
func tableConversionReasons(ctx context.Context, tx pgx.Tx, p storageParams) []api.TableBlocker {
	var out []api.TableBlocker
	one := func(q string, args ...any) bool {
		var yes bool
		if err := tx.QueryRow(ctx, q, args...).Scan(&yes); err != nil {
			return false
		}
		return yes
	}
	rel := p.rel()
	obj := p.Schema + "." + p.Table
	// A statement that names a table the extension has not made fails when it is parsed and would abort the transaction: ask for the table first.
	registered := func(table string) bool {
		if !one(`SELECT to_regclass($1) IS NOT NULL`, table) {
			return false
		}
		return one(`SELECT EXISTS (SELECT 1 FROM `+table+` WHERE relid = $1::regclass)`, rel)
	}
	if registered("kingres.ledger_table") {
		out = append(out, api.TableBlocker{Code: "ledger_table", Object: obj, Message: "it is a ledger table"})
	}
	if registered("kingres.dist_table") {
		out = append(out, api.TableBlocker{Code: "distributed_table", Object: obj, Message: "it is a distributed or reference table"})
	}
	if p.AccessMethod == "columnar" {
		if one(`SELECT EXISTS (SELECT 1 FROM pg_publication_rel WHERE prrelid = $1::regclass)`, rel) {
			out = append(out, api.TableBlocker{Code: "publication", Object: obj, Message: "it belongs to a publication"})
		}
		if one(`SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE contype = 'f' AND (conrelid = $1::regclass OR confrelid = $1::regclass))`, rel) {
			out = append(out, api.TableBlocker{Code: "foreign_keys", Object: obj, Message: "it has foreign keys"})
		}
	}
	return out
}

// stepStorageCheck repeats the checks of the request: the state may have changed before the task runs.
func (o *Ops) stepStorageCheck(ctx context.Context, env *task.Env) error {
	p, err := inputOf[storageParams](env.Task)
	if err != nil {
		return err
	}
	if err := o.ensureStorageExtensions(ctx, p, true); err != nil {
		return storageTaskError(err)
	}
	blockers, _, err := o.storageRefusals(ctx, p)
	if err != nil {
		return storageTaskError(err)
	}
	if len(blockers) > 0 {
		return storageTaskError(conversionRefused(blockers))
	}
	return nil
}

// storageTaskError gives a problem of the checks the code of the problem when the task records it.
func storageTaskError(err error) error {
	var pr *problem.Problem
	if errors.As(err, &pr) {
		switch pr.Code {
		case problem.ExtensionMissing, problem.ConversionRefused, problem.NotFound, problem.ExtensionUnavailable, problem.ExtensionVersionMismatch:
			return &extError{code: pr.Code, msg: pr.Detail}
		}
	}
	return err
}

// withLockRetry runs f (which sets its own lock_timeout) until it does not fail because a lock is held (55P03). Between the tries it says what it waits
// for. After storageLockLimit it fails with operation_in_progress, retryable: the same request can be made again.
func (o *Ops) withLockRetry(ctx context.Context, env *task.Env, doing string, f func() error) error {
	deadline := time.Now().Add(storageLockLimit)
	for {
		err := f()
		var pe *pgconn.PgError
		if err == nil || !errors.As(err, &pe) || pe.Code != "55P03" {
			return err
		}
		if time.Now().After(deadline) {
			return &extError{code: problem.OperationInProgress, retry: true, msg: fmt.Sprintf("Gave up waiting for a lock (%s): %s. Another session holds it; ask again when it has ended.", doing, pe.Message)}
		}
		_ = env.SetDetail(ctx, fmt.Sprintf("Waiting for a lock held by another session (%s): %s", doing, pe.Message))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(storageLockPause):
		}
	}
}

// stepStorageConvert converts the table or the partition. It says that a plain table is locked exclusively for the whole rewrite.
func (o *Ops) stepStorageConvert(ctx context.Context, env *task.Env) error {
	p, err := inputOf[storageParams](env.Task)
	if err != nil {
		return err
	}
	c, err := o.trustedConn(ctx, p.Database)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	var am string
	if err := c.QueryRow(ctx, `SELECT coalesce(a.amname, '') FROM pg_class c LEFT JOIN pg_am a ON a.oid = c.relam WHERE c.oid = $1::regclass`, p.rel()).Scan(&am); err != nil {
		return err
	}
	if am == p.AccessMethod {
		if p.AccessMethod == "heap" || !p.hasOptions() {
			return env.SetDetail(ctx, p.what()+" is stored as "+am+" already.")
		}
		// columnar to columnar with options: ALTER ... SET ACCESS METHOD to the same method does nothing, the options are set for the stripes to come
		_ = env.SetDetail(ctx, "The options are set for the stripes that are written from now on; the stripes that exist keep theirs.")
		return storageSQLError(o.withLockRetry(ctx, env, "set the options", func() error {
			tx, err := c.Begin(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
			if _, err := tx.Exec(ctx, `SELECT pg_catalog.set_config('lock_timeout', $1, true)`, strconv.FormatInt(storageLockTimeout.Milliseconds(), 10)+"ms"); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `SELECT kingres.columnar_set_options($1::regclass, $2, $3, $4, $5)`, p.rel(), nullStr(p.Compression), nullInt(p.CompressionLevel), nullInt(p.StripeRowCount), nullInt(p.ChunkRowCount)); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}))
	}
	if p.Partition != "" {
		_ = env.SetDetail(ctx, "Writers of the partition wait only while the new partition is swapped in.")
		_, err := c.Exec(ctx, `CALL kingres.convert_partition($1::text::regclass, $2::text::regclass, $3::name, $4::text[], $5::jsonb)`,
			qualified(p.Schema, p.Table), p.rel(), p.AccessMethod, nullableList(p.SortBy), p.optionsJSON())
		return storageSQLError(err)
	}
	_ = env.SetDetail(ctx, "ALTER TABLE ... SET ACCESS METHOD rewrites the table under an ACCESS EXCLUSIVE lock: readers and writers wait until it ends.")
	qi, err := sqlx.QualifiedIdent(p.Schema, p.Table)
	if err != nil {
		return err
	}
	return storageSQLError(o.withLockRetry(ctx, env, "convert the table", func() error {
		tx, err := c.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
		// The lock is asked for with a short lock_timeout: a long transaction of another session must not make every reader of the table queue behind this
		// statement (it waits for ACCESS EXCLUSIVE, and the sessions that come after it wait for it). The step asks again.
		if _, err := tx.Exec(ctx, `SELECT pg_catalog.set_config('lock_timeout', $1, true)`, strconv.FormatInt(storageLockTimeout.Milliseconds(), 10)+"ms"); err != nil {
			return err
		}
		for name, v := range map[string]string{"kingres_columnar.compression": p.Compression, "kingres_columnar.compression_level": itoa0(p.CompressionLevel),
			"kingres_columnar.stripe_row_count": itoa0(p.StripeRowCount), "kingres_columnar.chunk_row_count": itoa0(p.ChunkRowCount)} {
			if v == "" || p.AccessMethod == "heap" {
				continue
			}
			if _, err := tx.Exec(ctx, `SELECT pg_catalog.set_config($1, $2, true)`, name, v); err != nil {
				return err
			}
		}
		if err := run(ctx, tx, stmt().Text("ALTER TABLE").ID(qi).Text("SET ACCESS METHOD").ID(sqlx.MustIdent(p.AccessMethod))); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}))
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

func itoa0(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

// storageSQLError gives a refusal of PostgreSQL or of the extensions a code: a thing the format cannot take, or code of an owner that is not trusted, is
// conversion_refused (final); a lock that stays held, or the claim of another run of the table (55P03 from the extension), is operation_in_progress
// (retryable). Other errors stay as they are (a lost connection fails the step and the same request can be made again).
func storageSQLError(err error) error {
	if err == nil {
		return nil
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case "0A000", "42809", "22023", "23514", "42501":
			return &extError{code: problem.ConversionRefused, msg: pe.Message}
		case "55P03":
			return &extError{code: problem.OperationInProgress, retry: true, msg: pe.Message + " (another run or session holds the table; ask again when it has ended)"}
		}
	}
	return err
}

// stepStorageAnalyze refreshes the statistics of the table (the columnar format has its own sampling: a new format is of no use to the planner until
// the table is analyzed).
func (o *Ops) stepStorageAnalyze(ctx context.Context, env *task.Env) error {
	p, err := inputOf[storageParams](env.Task)
	if err != nil {
		return err
	}
	c, err := o.Service.ServiceConn(ctx, p.Database)
	if err != nil {
		return connectProblem(err)
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	qi, err := sqlx.QualifiedIdent(p.Schema, firstNonEmpty(p.Partition, p.Table))
	if err != nil {
		return err
	}
	return storageSQLError(o.withLockRetry(ctx, env, "analyze", func() error {
		if _, err := c.Exec(ctx, `SELECT pg_catalog.set_config('lock_timeout', $1, false)`, strconv.FormatInt(storageLockTimeout.Milliseconds(), 10)+"ms"); err != nil {
			return err
		}
		return run(ctx, c, stmt().Text("ANALYZE").ID(qi))
	}))
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

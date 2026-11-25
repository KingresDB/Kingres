// SPDX-License-Identifier: AGPL-3.0-only

package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extguard"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sqlx"
)

// The time-partitioning policy of a table (phase 12, analytics-phase12-storage.md section 13): get, put, delete and plan. The policy is a row of
// kingres_timepart in the database of the table, written through kingres.timepart_set_policy, which does the checks of the grid, the time zone and
// the owner. Like the storage operations, these act on the tables of this server's instance, where the leader runs. A user-partitioned distributed table
// is one of them: its policy and its runs are on the map group primary, which the leader runs, and a run creates, converts and retires its partitions on every
// node (kingres_timepart 0.3, phase 12 section 11).

// isoDurationRE is an ISO 8601 duration: P, then years, months, weeks and days, and after T hours, minutes and seconds. PostgreSQL reads it as an interval.
var isoDurationRE = regexp.MustCompile(`^P(?:\d+Y)?(?:\d+M)?(?:\d+W)?(?:\d+D)?(?:T(?:\d+H)?(?:\d+M)?(?:\d+(?:\.\d+)?S)?)?$`)

func isoDuration(s string) bool {
	return len(s) <= 64 && len(s) > 1 && !strings.HasSuffix(s, "T") && isoDurationRE.MatchString(s)
}

// policyArgs are the arguments of timepart_set_policy.
type policyArgs struct {
	every, ahead, runEvery string
	retention, columnar    *string
	action, timezone       string
	sortBy                 []string
	options                any
	enabled                bool
}

// validatePolicy checks the request (the shape; the grid and the time zone are checked by the extension) and applies the defaults.
func validatePolicy(req api.PartitionPolicyRequest) (policyArgs, error) {
	a := policyArgs{ahead: "P7D", runEvery: "PT1H", action: "drop", timezone: "UTC", enabled: true, sortBy: req.ColumnarSortBy}
	var errs []problem.FieldError
	dur := func(f, v string, required bool) {
		switch {
		case v == "" && required:
			errs = append(errs, field(f, "is required", "required"))
		case v != "" && !isoDuration(v):
			errs = append(errs, field(f, "must be an ISO 8601 duration such as P1D, P1M or PT6H", "pattern"))
		}
	}
	dur("/partitionEvery", req.PartitionEvery, true)
	a.every = req.PartitionEvery
	if req.CreateAhead != "" {
		dur("/createAhead", req.CreateAhead, false)
		a.ahead = req.CreateAhead
	}
	if req.RunEvery != "" {
		dur("/runEvery", req.RunEvery, false)
		a.runEvery = req.RunEvery
	}
	if req.Retention != nil {
		dur("/retention", *req.Retention, true)
		a.retention = req.Retention
	}
	if req.ColumnarAfter != nil {
		dur("/columnarAfter", *req.ColumnarAfter, true)
		a.columnar = req.ColumnarAfter
	}
	switch req.RetentionAction {
	case "":
	case "drop", "detach":
		a.action = req.RetentionAction
	default:
		errs = append(errs, field("/retentionAction", "must be drop or detach", "enum"))
	}
	if req.Timezone != "" {
		if len(req.Timezone) > 64 || strings.ContainsAny(req.Timezone, "\x00\r\n'\"") {
			errs = append(errs, field("/timezone", "is not a time zone name", "pattern"))
		}
		a.timezone = req.Timezone
	}
	if req.Enabled != nil {
		a.enabled = *req.Enabled
	}
	if len(req.ColumnarSortBy) > 16 {
		errs = append(errs, field("/columnarSortBy", "at most 16 columns", "out_of_range"))
	}
	for i, c := range req.ColumnarSortBy {
		if !identRE.MatchString(c) {
			errs = append(errs, field(fmt.Sprintf("/columnarSortBy/%d", i), "is not a column name", "pattern"))
		}
	}
	opts := map[string]any{}
	if req.Compression != nil {
		switch *req.Compression {
		case "none", "pglz", "lz4", "zstd":
			opts["compression"] = *req.Compression
		default:
			errs = append(errs, field("/compression", "must be none, pglz, lz4 or zstd", "enum"))
		}
	}
	if req.CompressionLvl != nil {
		if *req.CompressionLvl < 1 || *req.CompressionLvl > 19 {
			errs = append(errs, field("/compressionLevel", "must be between 1 and 19", "out_of_range"))
		}
		opts["compression_level"] = *req.CompressionLvl
	}
	if len(opts) > 0 {
		b, _ := json.Marshal(opts)
		a.options = string(b)
	}
	if len(errs) > 0 {
		return a, problem.Validation("The policy is invalid.", errs...)
	}
	return a, nil
}

// policyFields maps the name of an argument in a message of the extension to the field of the request.
var policyFields = []struct{ name, pointer string }{
	{"partition_every", "/partitionEvery"}, {"create_ahead", "/createAhead"}, {"retention_action", "/retentionAction"}, {"retention", "/retention"},
	{"columnar_after", "/columnarAfter"}, {"columnar_sort", "/columnarSortBy"}, {"columnar_options", "/compression"}, {"timezone", "/timezone"},
	{"time zone", "/timezone"}, {"run_every", "/runEvery"}, {"compression", "/compression"},
}

// policySQLError maps a refusal of the extension: a value that is not valid is a 400 with the field it names, a table that cannot have a policy
// (not partitioned by range on a time column, a default partition) is partition_policy_conflict.
func policySQLError(err error) error {
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return wrap(err)
	}
	switch pe.Code {
	case "0A000", "54000":
		// 54000: a distributed table that the policy would give more leaves (buckets times partitions) than kingres_shard.max_partition_leaves
		return problem.New(problem.PartitionPolicyConflict, pe.Message+".")
	case "22023", "22007", "22008", "22P02", "23514":
		// The argument the message names first; of two names at the same place the longer one (retention_action, not retention).
		f, at, long := "/partitionEvery", len(pe.Message), 0
		for _, x := range policyFields {
			if i := strings.Index(pe.Message, x.name); i >= 0 && (i < at || i == at && len(x.name) > long) {
				f, at, long = x.pointer, i, len(x.name)
			}
		}
		return problem.Validation("The policy is invalid.", field(f, pe.Message, "invalid"))
	case "42704":
		return problem.New(problem.NotFound, pe.Message+".")
	}
	return wrap(err)
}

// policyTx runs f in a transaction on the service connection with intervals written as ISO 8601. f reads and calls kingres_timepart, so the extension must be
// installed and its schema trusted (extgate.go).
func (o *Ops) policyTx(ctx context.Context, db string, f func(tx pgx.Tx) error) error {
	return o.isoTx(ctx, db, []string{extguard.Timepart}, f)
}

// isoTx is policyTx with the extensions the caller needs named: a caller that reads only the catalog of PostgreSQL names none and checks what it reads itself.
func (o *Ops) isoTx(ctx context.Context, db string, need []string, f func(tx pgx.Tx) error) error {
	return o.asServiceExt(ctx, db, need, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_catalog.set_config('intervalstyle', 'iso_8601', true)`); err != nil {
			return err
		}
		return f(tx)
	})
}

// notOnThisInstance is the 404 of a table that the instance of the manager leader does not have: it does not exist, or it lives on the instance of another
// shard group, which the storage and policy operations of this version do not reach. The detail says both.
func notOnThisInstance(schema, table string) error {
	return problem.New(problem.NotFound, fmt.Sprintf("Table %s.%s does not exist on the instance of the manager leader. The operations on storage and partitioning act on the tables of that instance; "+
		"a table that lives on another shard group's instance is not reached in this version.", schema, table))
}

// timepartInstalled tells whether kingres_timepart is installed in the database of this server. The read operations ask this and never install: a read that
// ran CREATE EXTENSION, or an ALTER EXTENSION UPDATE while a run goes, would be a write by a read-only role (and on a follower a refused write).
func (o *Ops) timepartInstalled(ctx context.Context, db string) (bool, error) {
	v, err := o.ext().Installed(ctx, db, ExtTimepart)
	if err != nil {
		return false, connectProblem(err)
	}
	return v != "", nil
}

func noPolicy(schema, table string, installed bool) error {
	why := "has no partitioning policy"
	if !installed {
		why = "has no partitioning policy (kingres_timepart is not installed in this database)"
	}
	return problem.New(problem.NotFound, fmt.Sprintf("Table %s.%s %s.", schema, table, why))
}

// requirePartitioned returns the key type of a table that a policy can work on; the problem is 404 for a table that is not there and
// partition_policy_conflict for one that cannot have a policy.
func requirePartitioned(ctx context.Context, tx pgx.Tx, schema, table string) error {
	var kind string
	err := tx.QueryRow(ctx, `SELECT c.relkind::text FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1 AND c.relname = $2 AND c.relkind IN ('r', 'p')`, schema, table).Scan(&kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return notOnThisInstance(schema, table)
	}
	if err != nil {
		return err
	}
	if kind != "p" {
		return problem.New(problem.PartitionPolicyConflict, fmt.Sprintf("Table %s.%s is not a partitioned table.", schema, table))
	}
	return nil
}

const policyColumns = `p.partition_every::text, p.create_ahead::text, p.retention::text, p.retention_action, p.columnar_after::text, p.columnar_sort,
	p.columnar_options->>'compression', (p.columnar_options->>'compression_level')::int, p.timezone, p.run_every::text, p.enabled, p.next_run_at,
	p.created_by::text, p.updated_at`

// GetPartitionPolicy reads the policy of a table with its next run, its last run and the health of the partitions ahead.
func (o *Ops) GetPartitionPolicy(ctx context.Context, db, schema, table string) (api.PartitionPolicy, error) {
	var out api.PartitionPolicy
	if err := checkNames(db, schema, table); err != nil {
		return out, err
	}
	if ok, err := o.timepartInstalled(ctx, db); err != nil {
		return out, err
	} else if !ok {
		return out, noPolicy(schema, table, false)
	}
	err := o.policyTx(ctx, db, func(tx pgx.Tx) (err error) {
		if err := requirePartitioned(ctx, tx, schema, table); err != nil {
			return err
		}
		out, err = readPolicy(ctx, tx, schema, table)
		if err != nil {
			return err
		}
		return o.fillOwner(ctx, tx, &out, schema, table)
	})
	return out, wrap(err)
}

func readPolicy(ctx context.Context, tx pgx.Tx, schema, table string) (api.PartitionPolicy, error) {
	var out api.PartitionPolicy
	var next time.Time
	var nextp *time.Time
	err := tx.QueryRow(ctx, `SELECT `+policyColumns+` FROM kingres.timepart_policy p WHERE p.schema_name = $1 AND p.table_name = $2`, schema, table).Scan(
		&out.PartitionEvery, &out.CreateAhead, &out.Retention, &out.RetentionAction, &out.ColumnarAfter, &out.ColumnarSortBy, &out.Compression,
		&out.CompressionLevel, &out.Timezone, &out.RunEvery, &out.Enabled, &nextp, &out.CreatedBy, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, noPolicy(schema, table, true)
	}
	if err != nil {
		return out, err
	}
	out.UpdatedAt = out.UpdatedAt.UTC()
	if nextp != nil {
		next = nextp.UTC()
		out.NextRunAt = &next
	}
	if out.ColumnarSortBy == nil {
		out.ColumnarSortBy = nil
	}
	// The key of the time partitioning: the column of the range partitions, which for a distributed table is the key of the partitions below the hash
	// level and not the distribution column that pg_partitioned_table has for the table itself (kingres_timepart reads it from the right place).
	err = tx.QueryRow(ctx, `SELECT k.r_keycol::text, k.r_keytype FROM kingres.timepart__key_info($1::regclass::oid) k`, qualified(schema, table)).Scan(&out.PartitionColumn, &out.PartitionColumnType)
	if err != nil {
		return out, err
	}
	switch out.PartitionColumnType {
	case "timestamp without time zone":
		out.PartitionColumnType = "timestamp"
	case "timestamp with time zone":
		out.PartitionColumnType = "timestamptz"
	}
	// the last run
	var run api.PartitionRunSummary
	err = tx.QueryRow(ctx, `SELECT r.run_id, r.state, r.started_at, r.finished_at FROM kingres.timepart_run r
		WHERE r.schema_name = $1 AND r.table_name = $2 ORDER BY r.run_id DESC LIMIT 1`, schema, table).Scan(&run.ID, &run.State, &run.StartedAt, &run.FinishedAt)
	switch {
	case err == nil:
		run.StartedAt = run.StartedAt.UTC()
		if run.FinishedAt != nil {
			f := run.FinishedAt.UTC()
			run.FinishedAt = &f
		}
		out.LastRun = &run
	case !errors.Is(err, pgx.ErrNoRows):
		return out, err
	}
	// health
	out.Health = api.PartitionPolicyHealth{Problems: []string{}}
	// the partitions of a distributed table are rows of the catalog of the sharding extension and not tables here, so they are read the way the policy reads them
	if err := tx.QueryRow(ctx, `SELECT (SELECT greatest(max(c.hi) - now(), interval '0') FROM kingres.timepart__children($1::regclass::oid) c WHERE NOT c.is_default)::text`, qualified(schema, table)).Scan(&out.Health.PartitionsAhead); err != nil {
		return out, err
	}
	rows, err := tx.Query(ctx, `SELECT c.problem FROM kingres.timepart_check() c WHERE c.schema_name = $1 AND c.table_name = $2`, schema, table)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return out, err
		}
		found = true
		if name, ok := healthProblems[p]; ok {
			out.Health.Problems = append(out.Health.Problems, name)
		}
	}
	out.Health.OK = !found
	return out, rows.Err()
}

// healthProblems maps the problems of kingres.timepart_check to the names of the contract; the others make the policy not ok and are not named.
var healthProblems = map[string]string{"ahead_low": "aheadLow", "last_run_failed": "lastRunFailed", "default_partition_present": "defaultPartitionPresent", "misaligned": "misaligned"}

// PutPartitionPolicy creates or replaces the policy. It writes the policy and nothing else: the scheduler takes the table at its next tick (the
// policy is due now).
func (o *Ops) PutPartitionPolicy(ctx context.Context, actor, db, schema, table string, req api.PartitionPolicyRequest) (api.PartitionPolicy, error) {
	var out api.PartitionPolicy
	if err := checkNames(db, schema, table); err != nil {
		return out, err
	}
	a, err := validatePolicy(req)
	if err != nil {
		return out, err
	}
	need := []string{ExtTimepart}
	if a.columnar != nil {
		need = append(need, ExtColumnar)
	}
	if err := o.ensureExtensions(ctx, db, need, a.columnar != nil); err != nil {
		return out, err
	}
	if running, err := o.maintainRunning(ctx, db, schema, table); err != nil {
		return out, err
	} else if running {
		return out, problem.New(problem.OperationInProgress, "A partition.maintain task is running for this table; its end moves the schedule. Set the policy when it has ended.")
	}
	allowed, err := o.nonSuperuserOwnersAllowed(ctx)
	if err != nil {
		return out, err
	}
	err = o.policyTx(ctx, db, func(tx pgx.Tx) error {
		if err := requirePartitioned(ctx, tx, schema, table); err != nil {
			return err
		}
		w, err := tableOwnerOf(ctx, tx, schema, table)
		if err != nil {
			return err
		}
		if who := untrustedOwner(allowed, w); who != "" {
			return ownerRefusal(who)
		}
		if _, err := tx.Exec(ctx, `SELECT kingres.timepart_set_policy($1::regclass, $2::interval, $3::interval, $4::interval, $5, $6::interval, $7::text[], $8::jsonb, $9, $10::interval, $11)`,
			qualified(schema, table), a.every, a.ahead, a.retention, a.action, a.columnar, nullableList(a.sortBy), a.options, a.timezone, a.runEvery, a.enabled); err != nil {
			return policySQLError(err)
		}
		// A new policy is due now; a policy that is replaced is due now too, so that the new settings act at the next tick.
		if _, err := tx.Exec(ctx, `UPDATE kingres.timepart_policy SET next_run_at = now() WHERE schema_name = $1 AND table_name = $2`, schema, table); err != nil {
			return err
		}
		out, err = readPolicy(ctx, tx, schema, table)
		if err != nil {
			return err
		}
		return o.fillOwner(ctx, tx, &out, schema, table)
	})
	return out, err
}

// DeletePartitionPolicy removes the policy; the partitions stay. A run that is going makes it operation_in_progress (the task holds the subject).
func (o *Ops) DeletePartitionPolicy(ctx context.Context, db, schema, table string) error {
	if err := checkNames(db, schema, table); err != nil {
		return err
	}
	if ok, err := o.timepartInstalled(ctx, db); err != nil {
		return err
	} else if !ok {
		return noPolicy(schema, table, false)
	}
	if running, err := o.maintainRunning(ctx, db, schema, table); err != nil {
		return err
	} else if running {
		return problem.New(problem.OperationInProgress, "A partition.maintain task is running for this table.")
	}
	err := o.policyTx(ctx, db, func(tx pgx.Tx) error {
		if err := requirePartitioned(ctx, tx, schema, table); err != nil {
			return err
		}
		if _, err := readPolicy(ctx, tx, schema, table); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT kingres.timepart_unset_policy($1::regclass)`, qualified(schema, table)); err != nil {
			return policySQLError(err)
		}
		return nil
	})
	return err
}

// PlanPartitionPolicy returns the actions a run would take at asOf (now when nil), with the stored policy or with the policy of the request. Nothing
// is changed.
func (o *Ops) PlanPartitionPolicy(ctx context.Context, db, schema, table string, req api.PlanPartitionPolicyRequest) (api.PartitionPlan, error) {
	out := api.PartitionPlan{Actions: []api.PartitionAction{}}
	if err := checkNames(db, schema, table); err != nil {
		return out, err
	}
	asOf := time.Now().UTC()
	if req.AsOf != nil {
		asOf = req.AsOf.UTC()
	}
	out.AsOf = asOf
	var hyp *policyArgs
	if req.Policy != nil {
		a, err := validatePolicy(*req.Policy)
		if err != nil {
			return out, err
		}
		hyp = &a
	}
	if ok, err := o.timepartInstalled(ctx, db); err != nil {
		return out, err
	} else if !ok {
		if hyp != nil {
			return out, problem.New(problem.ExtensionMissing, "kingres_timepart is not installed in this database; set a policy first (the first write installs it).")
		}
		return out, noPolicy(schema, table, false)
	}
	err := o.policyTx(ctx, db, func(tx pgx.Tx) error {
		if err := requirePartitioned(ctx, tx, schema, table); err != nil {
			return err
		}
		var rows pgx.Rows
		var err error
		if hyp == nil {
			if _, err := readPolicy(ctx, tx, schema, table); err != nil {
				return err
			}
			rows, err = tx.Query(ctx, `SELECT seq, kind, partition_name::text, range_from, range_to, reason FROM kingres.timepart_plan($1::regclass, $2)`, qualified(schema, table), asOf)
		} else {
			rows, err = tx.Query(ctx, `SELECT pr.seq, pr.kind, pr.part_name::text, pr.range_from, pr.range_to, pr.reason
				FROM kingres.timepart__plan_rows($1::regclass::oid, $2::interval, $3, $4::interval, $5::interval, $6, $7::interval, $8) pr ORDER BY pr.seq`,
				qualified(schema, table), hyp.every, hyp.timezone, hyp.ahead, hyp.retention, hyp.action, hyp.columnar, asOf)
		}
		if err != nil {
			return policySQLError(err)
		}
		defer rows.Close()
		for rows.Next() {
			var x api.PartitionAction
			var from, to *time.Time
			if err := rows.Scan(&x.Seq, &x.Kind, &x.Partition, &from, &to, &x.Detail); err != nil {
				return err
			}
			if x.Kind == "misaligned" {
				continue
			}
			x.State = "planned"
			x.RangeFrom, x.RangeTo = utc(from), utc(to)
			out.Actions = append(out.Actions, x)
		}
		return rows.Err()
	})
	return out, err
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// subjectOf is the subject of a task of a table.
func subjectOf(db, schema, table string) string { return db + "/" + schema + "." + table }

// maintainRunning tells whether a task partition.maintain is unfinished for the table (asked of the task store by kind and subject, not in a window of the newest tasks).
// It fails closed: when the task store cannot answer, the caller is told so and does not go ahead (a delete or a put during a run, a second run).
func (o *Ops) maintainRunning(ctx context.Context, db, schema, table string) (bool, error) {
	eng := o.Engine()
	if eng == nil {
		return false, nil
	}
	yes, err := eng.ActiveFor(ctx, KindMaintain, "table", subjectOf(db, schema, table))
	if err != nil {
		return false, problem.New(problem.ServiceUnavailable, "The task store cannot say whether a run of this policy is going; try again.").WithRetryAfter(5)
	}
	return yes, nil
}

var _ = sqlx.Fixed("")

// SPDX-License-Identifier: AGPL-3.0-only

package ops

// The table lifecycle (contract 0.14.0, increment 9.6 of kingres/docs/design/shard-phase9.md): distribute (with rows), undistribute, alter and
// the partition operations of a distributed table are tasks with named steps. The steps call the functions of the sharding extension
// (kingres_shard 0.7); the task never holds a database transaction across steps, and every step checks what exists, so a task killed with
// the process goes on at the first step that is not done.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sqlx"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// Task kinds of the table lifecycle (contract 0.14.0).
const (
	KindTableUndistribute = "table.undistribute"
	KindTableAlter        = "table.alter"
	KindTablePartition    = "table.partition"
)

// Codes of a failed table task (TaskError.code).
const (
	CodeTableNotEligible   = "table_not_eligible"
	CodeInsufficientDisk   = "insufficient_disk"
	CodeVerificationFail   = "verification_failed"
	CodeTableOpNotSwitched = "switch_not_possible"
)

// onlineRowsThreshold is the number of estimated rows from which mode auto chooses the online copy.
const onlineRowsThreshold = 100000

// switchWait is how long the switch step waits before it tries again when the attempts of the extension were used up (design: 60 s).
// A variable so that tests can shorten it.
var switchWait = 60 * time.Second

// switchRounds bounds how often the switch step goes on after the blockers did not clear (a day at 60 s).
var switchRounds = 1440

// lockWait is how long a step waits before it asks again when the extension could not get a lock on the table in time (55P03: a long
// transaction holds it). The extension refuses after rebuild_lock_timeout and says "the operation tries again"; the caller is the one that tries.
// A variable so that tests can shorten it.
var lockWait = 5 * time.Second

// Step names (shown to users).
const (
	stepCheckTable = "Check the table"
	stepFindLinked = "Find linked tables"
	stepPlan       = "Plan the change"
	stepFreeSpace  = "Check free space"
	stepPrepare    = "Prepare"
	stepLayout     = "Create the new layout"
	stepCopy       = "Copy rows"
	stepCatchUp    = "Catch up with writers"
	stepSwitch     = "Switch"
	stepFinish     = "Verify and clean up"
	stepBlocking   = "Copy rows and switch"
	stepCheckReq   = "Check the request"
	stepPartitions = "Change the partitions on every server"
)

// tableParams is the input of the table tasks. V is 2 for the tasks of this version; a task recorded before has the older
// distributeParams (one step), which the definition of table.distribute still runs.
type tableParams struct {
	V        int    `json:"v"`
	Op       string `json:"op"` // distribute, undistribute, alter, partition
	Method   string `json:"method,omitempty"`
	Database string `json:"database"`
	Schema   string `json:"schema"`
	Table    string `json:"table"`
	Column   string `json:"column,omitempty"`
	// distribute
	ColocSchema string `json:"colocateSchema,omitempty"`
	ColocTable  string `json:"colocateTable,omitempty"`
	BucketCount int    `json:"bucketCount,omitempty"`
	// Online is the mode resolved at the request: the rows are copied while writers go on.
	Online         bool   `json:"online"`
	DependentViews string `json:"dependentViews,omitempty"`
	// undistribute
	Cascade     bool `json:"cascadeViaForeignKeys,omitempty"`
	TargetGroup int  `json:"targetGroupId,omitempty"`
	// alter
	AlterColumn  string `json:"alterColumn,omitempty"`
	AlterBuckets int    `json:"alterBuckets,omitempty"`
	AlterColoc   string `json:"alterColocateWith,omitempty"` // default, none, or schema.table
	CascadeColoc bool   `json:"cascadeToColocated,omitempty"`
	// partition
	PartOp       string `json:"partOp,omitempty"` // create, attach, detach, drop
	PartName     string `json:"partName,omitempty"`
	PartBound    string `json:"partBound,omitempty"`
	PartSchema   string `json:"partSchema,omitempty"`
	PartTable    string `json:"partTable,omitempty"`
	IfNotExists  bool   `json:"ifNotExists,omitempty"`
	EstRows      int64  `json:"estimatedRows,omitempty"`
	EstBytes     int64  `json:"estimatedBytes,omitempty"`
	LinkedTables int    `json:"linkedTables,omitempty"`
}

func (p tableParams) subject() string { return p.Database + "/" + p.Schema + "." + p.Table }

func (p tableParams) rel() string { return qualified(p.Schema, p.Table) }

// tableTaskError is a failure of a step that carries the code and the retry hint the task is recorded with (task.Coded).
type tableTaskError struct {
	code  string
	retry bool
	msg   string
}

func (e *tableTaskError) Error() string       { return e.msg }
func (e *tableTaskError) TaskCode() string    { return e.code }
func (e *tableTaskError) TaskRetryable() bool { return e.retry }

// tableBackend is what the steps need of the database; the production one runs the functions of the extension on a service connection,
// a test supplies its own.
type tableBackend interface {
	// Report returns the lines of table_change_report for the change.
	Report(ctx context.Context, p tableParams) (blockers, warnings, notes []api.TableBlocker, err error)
	// Sizes returns the estimated rows and bytes of the table (with the tables it takes along), and the servers that receive data.
	Sizes(ctx context.Context, p tableParams) (rows, bytes int64, servers []string, err error)
	// Begin starts the online operation, or returns the id of the one that is unfinished for the table.
	Begin(ctx context.Context, p tableParams) (int64, error)
	// Advance moves the operation to a phase and returns the phase it reached and its detail.
	Advance(ctx context.Context, p tableParams, op int64, to string) (reached, detail string, err error)
	// Progress returns the rows copied and expected.
	Progress(ctx context.Context, p tableParams, op int64) (done, total int64, err error)
	// Verified returns the verification result of a finished operation ("ok", "skipped", "failed" or "").
	Verified(ctx context.Context, p tableParams, op int64) (string, error)
	// Blocking runs the blocking function of the change.
	Blocking(ctx context.Context, p tableParams) error
	// Partition runs the partition function.
	Partition(ctx context.Context, p tableParams) error
	// UnfinishedOp returns the unfinished operation of the table, 0 when there is none, and whether it has switched.
	UnfinishedOp(ctx context.Context, p tableParams) (op int64, switched bool, err error)
	// Cancel undoes an operation that has not switched.
	Cancel(ctx context.Context, p tableParams, op int64) error
}

// DiskFree reports the free bytes of the disk that holds the data of a server (known false when this server cannot tell).
type DiskFree func(ctx context.Context, server string) (free int64, known bool)

func (o *Ops) backend() tableBackend {
	if o.tableBackendOverride != nil {
		return o.tableBackendOverride
	}
	return pgBackend{o: o}
}

// tableDefinitions are the task definitions of the table lifecycle.
func (o *Ops) tableDefinitions() []task.Definition {
	mk := func(kind string) task.Definition {
		return task.Definition{
			Kind: kind,
			Dynamic: func(in json.RawMessage) ([]task.Step, error) {
				p, err := rawInput[tableParams](in)
				if err != nil {
					return nil, err
				}
				if p.V < 2 && kind == KindTableDistribute {
					return []task.Step{{Name: "Distribute the table", Irreversible: true, Run: o.stepDistribute}}, nil
				}
				return o.tableSteps(p), nil
			},
			OnFail:      o.tableOnFail,
			Cancellable: tableCancellable,
		}
	}
	return []task.Definition{mk(KindTableDistribute), mk(KindTableUndistribute), mk(KindTableAlter), mk(KindTablePartition)}
}

func stepState(t task.Task, name string) string {
	for _, s := range t.Steps {
		if s.Name == name {
			return s.State
		}
	}
	return ""
}

// tableCancellable: a table task is cancellable until its switch has started (cancelling it afterwards would not undo the swap).
func tableCancellable(t task.Task) bool {
	for _, name := range []string{stepSwitch, stepFinish, stepBlocking, stepPartitions} {
		switch stepState(t, name) {
		case task.StepDone, task.StepRunning, task.StepSkipped:
			if name == stepSwitch && stepState(t, name) == task.StepRunning {
				continue // the wait of a switch that has not happened is cancellable (see stepSwitchOp)
			}
			return false
		}
	}
	return true
}

// tableOnFail is the compensation: an online operation that did not switch is cancelled, so that nothing of it is left. A node that
// cannot be reached keeps its leftovers: the extension names them and the recovery worker removes them when the node returns.
func (o *Ops) tableOnFail(ctx context.Context, t task.Task) {
	p, err := rawInput[tableParams](t.Input)
	if err != nil || p.V < 2 || !p.Online {
		return
	}
	b := o.backend()
	op, switched, err := b.UnfinishedOp(ctx, p)
	if err != nil || op == 0 || switched {
		return
	}
	if err := b.Cancel(ctx, p, op); err != nil && o.Log != nil {
		o.Log.Warn("table task: the operation could not be cancelled", "task", t.ID, "op", op, "error", err)
	}
}

func (o *Ops) tableSteps(p tableParams) []task.Step {
	switch p.Op {
	case "partition":
		return []task.Step{
			{Name: stepCheckReq, Run: o.stepCheck},
			{Name: stepPartitions, Run: func(ctx context.Context, env *task.Env) error { return o.stepPartition(ctx, env, p) }},
			{Name: stepFinish, Run: func(ctx context.Context, env *task.Env) error { return nil }},
		}
	}
	var steps []task.Step
	steps = append(steps, task.Step{Name: stepCheckTable, Run: o.stepCheck})
	if p.Op == "undistribute" {
		steps = append(steps, task.Step{Name: stepFindLinked, Run: o.stepLinked})
	}
	if p.Op == "alter" {
		steps = append(steps, task.Step{Name: stepPlan, Run: o.stepPlan})
	}
	steps = append(steps, task.Step{Name: stepFreeSpace, Run: o.stepFree})
	if !p.Online {
		return append(steps,
			task.Step{Name: stepBlocking, Run: func(ctx context.Context, env *task.Env) error {
				if err := env.SetDetail(ctx, "Writers of the table wait during this step."); err != nil {
					return err
				}
				return o.backend().Blocking(ctx, p)
			}},
			task.Step{Name: stepFinish, Run: func(ctx context.Context, env *task.Env) error { return nil }})
	}
	adv := func(to string, detail func(ctx context.Context, env *task.Env, op int64) string) func(ctx context.Context, env *task.Env) error {
		return func(ctx context.Context, env *task.Env) error {
			op, _, err := o.backend().UnfinishedOp(ctx, p)
			if err != nil {
				return err
			}
			if op == 0 {
				return &tableTaskError{code: task.CodeStepFailed, msg: "The operation of the extension does not exist (it was cancelled or finished)."}
			}
			var d string
			if err := o.whileLocked(ctx, env, func() (e error) { _, d, e = o.backend().Advance(ctx, p, op, to); return e }); err != nil {
				return err
			}
			_ = env.SetDetail(ctx, d) // also clears what the wait for a lock showed
			if detail != nil {
				if s := detail(ctx, env, op); s != "" {
					_ = env.SetDetail(ctx, s)
				}
			}
			return nil
		}
	}
	progress := func(ctx context.Context, env *task.Env, op int64) string {
		if done, total, err := o.backend().Progress(ctx, p, op); err == nil && total > 0 {
			return fmt.Sprintf("%d of about %d rows copied", done, total)
		}
		return ""
	}
	return append(steps,
		task.Step{Name: stepPrepare, Run: func(ctx context.Context, env *task.Env) error {
			return o.whileLocked(ctx, env, func() error { _, err := o.backend().Begin(ctx, p); return err })
		}},
		task.Step{Name: stepLayout, Run: adv("shadow", nil)},
		task.Step{Name: stepCopy, Run: adv("copying", progress)},
		task.Step{Name: stepCatchUp, Run: adv("catching_up", nil)},
		task.Step{Name: stepSwitch, Run: func(ctx context.Context, env *task.Env) error { return o.stepSwitchOp(ctx, env, p) }},
		task.Step{Name: stepFinish, Run: func(ctx context.Context, env *task.Env) error { return o.stepFinishOp(ctx, env, p) }},
	)
}

// stepCheck repeats the check of the request: the table may have changed since.
func (o *Ops) stepCheck(ctx context.Context, env *task.Env) error {
	p, err := inputOf[tableParams](env.Task)
	if err != nil {
		return err
	}
	blockers, _, _, err := o.backend().Report(ctx, p)
	if err != nil {
		return err
	}
	if len(blockers) > 0 {
		return &tableTaskError{code: CodeTableNotEligible, msg: "The table cannot take the change: " + describeBlockers(blockers)}
	}
	return nil
}

func describeBlockers(b []api.TableBlocker) string {
	var parts []string
	for i, x := range b {
		if i == 3 {
			parts = append(parts, fmt.Sprintf("and %d more", len(b)-3))
			break
		}
		parts = append(parts, x.Code+": "+x.Message)
	}
	return strings.Join(parts, "; ")
}

// stepLinked fails with the list when the foreign key component has more tables than the request names and cascade is off.
func (o *Ops) stepLinked(ctx context.Context, env *task.Env) error {
	p, err := inputOf[tableParams](env.Task)
	if err != nil {
		return err
	}
	if p.Cascade || p.LinkedTables == 0 {
		return nil
	}
	return &tableTaskError{code: CodeTableNotEligible, msg: fmt.Sprintf("The table is linked to %d other tables by foreign keys; undistribute them together with cascadeViaForeignKeys.", p.LinkedTables)}
}

// stepPlan says what is rebuilt (the extension decides; a change of the catalog only is one call without copy).
func (o *Ops) stepPlan(ctx context.Context, env *task.Env) error {
	p, err := inputOf[tableParams](env.Task)
	if err != nil {
		return err
	}
	rows, bytes, _, err := o.backend().Sizes(ctx, p)
	if err != nil {
		return err
	}
	return env.SetDetail(ctx, fmt.Sprintf("About %d rows, %d bytes.", rows, bytes))
}

// stepFree compares the estimate with the free space of every destination server; a server that cannot say is not checked.
func (o *Ops) stepFree(ctx context.Context, env *task.Env) error {
	p, err := inputOf[tableParams](env.Task)
	if err != nil {
		return err
	}
	if o.DiskFree == nil {
		return task.Skip("this server cannot tell the free space of the servers")
	}
	_, bytes, servers, err := o.backend().Sizes(ctx, p)
	if err != nil {
		return err
	}
	if bad := freeSpaceShortfall(p, bytes, servers, func(s string) (int64, bool) { return o.DiskFree(ctx, s) }); bad != "" {
		return &tableTaskError{code: CodeInsufficientDisk, msg: bad}
	}
	return nil
}

// needFree is the free space a destination server needs: the share of the data it receives plus a margin (indexes, the capture log, the old copy
// that stays until the cleanup). An undistribute puts everything on one server.
func needFree(p tableParams, bytes int64, servers int) int64 {
	if servers < 1 {
		servers = 1
	}
	share := bytes
	if p.Op != "undistribute" {
		share = bytes / int64(servers)
	}
	return share + share/2 + 64<<20
}

// freeSpaceShortfall names the first server that lacks the space, or returns "".
func freeSpaceShortfall(p tableParams, bytes int64, servers []string, free func(string) (int64, bool)) string {
	need := needFree(p, bytes, len(servers))
	for _, s := range servers {
		f, ok := free(s)
		if !ok {
			continue
		}
		if f < need {
			return fmt.Sprintf("Server %s has %d bytes free and the change needs about %d.", s, f, need)
		}
	}
	return ""
}

// stepSwitchOp calls the switch; when the attempts of the extension are used up (writers hold the table, a long transaction delays it) the task shows the
// blockers in its detail and tries again after switchWait, for as long as they hold; a request to cancel ends the wait.
func (o *Ops) stepSwitchOp(ctx context.Context, env *task.Env, p tableParams) error {
	b := o.backend()
	op, _, err := b.UnfinishedOp(ctx, p)
	if err != nil {
		return err
	}
	if op == 0 {
		return &tableTaskError{code: task.CodeStepFailed, msg: "The operation of the extension does not exist (it was cancelled or finished)."}
	}
	for round := 0; ; round++ {
		reached, detail, err := b.Advance(ctx, p, op, "switching")
		if err != nil {
			return err
		}
		if reached == "cleaning" || reached == "done" {
			_ = env.SetDetail(ctx, "")
			return nil
		}
		if round >= switchRounds {
			return &tableTaskError{code: CodeTableOpNotSwitched, retry: true, msg: "The switch did not become possible: " + detail}
		}
		if err := env.SetDetail(ctx, "Waiting to switch: "+detail); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(switchWait):
		}
		if eng := o.Engine(); eng != nil {
			if t, gerr := eng.Get(ctx, env.Task.ID); gerr == nil && t.CancelRequested {
				return task.Pause("A cancel was requested while the switch was waiting.")
			}
		}
	}
}

func (o *Ops) stepFinishOp(ctx context.Context, env *task.Env, p tableParams) error {
	b := o.backend()
	op, _, err := b.UnfinishedOp(ctx, p)
	if err != nil {
		return err
	}
	if op == 0 {
		return nil // finished by an earlier run of this step
	}
	if _, d, err := b.Advance(ctx, p, op, "cleaning"); err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && strings.Contains(pe.Message, "verification") {
			return &tableTaskError{code: CodeVerificationFail, msg: pe.Message}
		}
		return err
	} else if d != "" {
		_ = env.SetDetail(ctx, d)
	}
	v, err := b.Verified(ctx, p, op)
	if err != nil {
		return err
	}
	if v == "failed" {
		return &tableTaskError{code: CodeVerificationFail, msg: "The new table differs from the old copy, which is kept for inspection (kingres.table_operations names it)."}
	}
	if v != "" {
		_ = env.SetDetail(ctx, "Verification: "+v)
	}
	return nil
}

func (o *Ops) stepPartition(ctx context.Context, env *task.Env, p tableParams) error {
	_ = env.SetDetail(ctx, fmt.Sprintf("%s the partition on every server", strings.ToUpper(p.PartOp[:1])+p.PartOp[1:]))
	return o.whileLocked(ctx, env, func() error { return o.backend().Partition(ctx, p) })
}

// whileLocked runs f and, when it fails because a lock on the table was not granted in time (55P03), shows the holders in the detail of the task,
// waits lockWait and runs it again, for as long as the holders stay (at most switchRounds times); a request to cancel ends the wait.
func (o *Ops) whileLocked(ctx context.Context, env *task.Env, f func() error) error {
	for round := 0; ; round++ {
		err := f()
		var pe *pgconn.PgError
		if err == nil && round > 0 {
			_ = env.SetDetail(ctx, "")
		}
		if err == nil || !errors.As(err, &pe) || pe.Code != "55P03" {
			return err
		}
		if round >= switchRounds {
			return &tableTaskError{code: CodeTableOpNotSwitched, retry: true, msg: pe.Message}
		}
		why := pe.Message
		if pe.Detail != "" {
			why += ". " + pe.Detail
		}
		if err := env.SetDetail(ctx, "Waiting for a lock: "+why); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(lockWait):
		}
		if eng := o.Engine(); eng != nil {
			if t, gerr := eng.Get(ctx, env.Task.ID); gerr == nil && t.CancelRequested {
				return task.Pause("A cancel was requested while the task waited for a lock.")
			}
		}
	}
}

// ---------------------------------------------------------------------------------------------------------------------------
// The production backend
// ---------------------------------------------------------------------------------------------------------------------------

type pgBackend struct{ o *Ops }

func (b pgBackend) conn(ctx context.Context, db string) (*pgx.Conn, error) {
	if b.o.Service == nil {
		return nil, problem.New(problem.UnsupportedOp, "This server has no managed instance.")
	}
	return b.o.Service.ServiceConn(ctx, db)
}

func (b pgBackend) with(ctx context.Context, db string, f func(c *pgx.Conn) error) error {
	c, err := b.conn(ctx, db)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	return f(c)
}

// reportKind maps the change to the kind of table_change_report.
func reportKind(p tableParams) string {
	switch p.Op {
	case "distribute":
		if p.Method == "reference" {
			return "distribute_reference"
		}
		return "distribute"
	case "undistribute":
		return "undistribute"
	case "alter":
		return "alter"
	}
	switch p.PartOp {
	case "create":
		return "create_partition"
	case "attach":
		return "attach_partition"
	case "detach":
		return "detach_partition"
	}
	return "drop_partition"
}

// reportOptions are the options of table_change_report for the change (the names of the extension, snake case).
func reportOptions(p tableParams) map[string]any {
	opts := map[string]any{}
	if p.Online {
		opts["mode"] = "online"
	}
	set := func(k string, v any) { opts[k] = v }
	if p.Column != "" {
		set("column", p.Column)
	}
	if p.ColocTable != "" {
		set("colocate_with", qualified(p.ColocSchema, p.ColocTable))
	}
	if p.BucketCount > 0 {
		set("bucket_count", p.BucketCount)
	}
	if p.Op == "undistribute" {
		set("cascade_via_foreign_keys", p.Cascade)
		set("target_group", p.TargetGroup)
	}
	if p.Op == "alter" {
		if p.AlterColumn != "" {
			set("distribution_column", p.AlterColumn)
		}
		if p.AlterBuckets > 0 {
			set("bucket_count", p.AlterBuckets)
		}
		if p.AlterColoc != "" {
			set("colocate_with", p.AlterColoc)
		}
		set("cascade_to_colocated", p.CascadeColoc)
	}
	if p.Op == "partition" {
		set("partition", p.PartName)
		if p.PartTable != "" {
			set("table", qualified(p.PartSchema, p.PartTable))
		}
	}
	if p.DependentViews == "recreate" {
		set("dependent_views", "recreate")
	}
	return opts
}

func (b pgBackend) Report(ctx context.Context, p tableParams) (blockers, warnings, notes []api.TableBlocker, err error) {
	err = b.with(ctx, p.Database, func(c *pgx.Conn) error {
		if ok, err := b.hasExtension(ctx, c); err != nil {
			return err
		} else if !ok {
			return problem.New(problem.UnsupportedOp, "The database has no kingres_shard 0.7 or later, which the table operations need.")
		}
		opts, _ := json.Marshal(reportOptions(p))
		rows, err := c.Query(ctx, "SELECT severity, code, object, detail FROM kingres.table_change_report($1, $2::regclass, $3::jsonb)", reportKind(p), p.rel(), string(opts))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var sev string
			var l api.TableBlocker
			if err := rows.Scan(&sev, &l.Code, &l.Object, &l.Message); err != nil {
				return err
			}
			switch sev {
			case "blocker":
				blockers = append(blockers, l)
			case "warning":
				warnings = append(warnings, l)
			default:
				notes = append(notes, l)
			}
		}
		return rows.Err()
	})
	return blockers, warnings, notes, err
}

func (b pgBackend) hasExtension(ctx context.Context, c *pgx.Conn) (bool, error) {
	var ok bool
	err := c.QueryRow(ctx, `SELECT to_regprocedure('kingres.table_change_report(text,regclass,jsonb)') IS NOT NULL`).Scan(&ok)
	return ok, err
}

func (b pgBackend) Sizes(ctx context.Context, p tableParams) (rows, bytes int64, servers []string, err error) {
	err = b.with(ctx, p.Database, func(c *pgx.Conn) error {
		var groups int
		if err := c.QueryRow(ctx, `SELECT count(DISTINCT group_id) FROM kingres.node WHERE role = 'primary' AND is_active`).Scan(&groups); err != nil {
			return err
		}
		if groups < 1 {
			groups = 1
		}
		var dist bool
		if err := c.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM kingres.dist_table WHERE relid = $1::regclass)`, p.rel()).Scan(&dist); err != nil {
			return err
		}
		// A plain table is read as it is; a distributed table is read on this node (its partitions here) and scaled by the groups.
		err := c.QueryRow(ctx, `SELECT coalesce(sum(greatest(c.reltuples, 0))::bigint, 0), coalesce(sum(pg_catalog.pg_total_relation_size(c.oid))::bigint, 0)
			FROM pg_catalog.pg_class c WHERE c.oid = $1::regclass OR c.oid IN (SELECT inhrelid FROM pg_catalog.pg_inherits WHERE inhparent = $1::regclass)`, p.rel()).Scan(&rows, &bytes)
		if err != nil {
			return err
		}
		if dist {
			rows *= int64(groups)
			bytes *= int64(groups)
		}
		hosts, err := c.Query(ctx, `SELECT DISTINCT host FROM kingres.node WHERE is_active AND ($1 OR group_id = $2) ORDER BY host`, p.Op != "undistribute", p.TargetGroup)
		if err != nil {
			return err
		}
		defer hosts.Close()
		for hosts.Next() {
			var h string
			if err := hosts.Scan(&h); err != nil {
				return err
			}
			servers = append(servers, h)
		}
		return hosts.Err()
	})
	return rows, bytes, servers, err
}

func (b pgBackend) setViews(ctx context.Context, c *pgx.Conn, p tableParams) error {
	v := "refuse"
	if p.DependentViews == "recreate" {
		v = "recreate"
	}
	_, err := c.Exec(ctx, "SELECT pg_catalog.set_config('kingres_shard.dependent_views', $1, false)", v)
	return err
}

var phaseKinds = map[string]string{"distribute": "distribute", "undistribute": "undistribute", "alter": "alter"}

func (b pgBackend) UnfinishedOp(ctx context.Context, p tableParams) (op int64, switched bool, err error) {
	err = b.with(ctx, p.Database, func(c *pgx.Conn) error {
		var phase string
		kind := phaseKinds[p.Op]
		if p.Method == "reference" {
			kind = "distribute_reference"
		}
		e := c.QueryRow(ctx, `SELECT o.op_id, o.phase FROM kingres.table_operation o
			WHERE o.kind = $1 AND NOT (o.spec ? 'mirror') AND o.phase NOT IN ('done', 'failed', 'cancelled')
			  AND (o.spec->>'relid')::oid = $2::regclass::oid ORDER BY o.op_id DESC LIMIT 1`, kind, p.rel()).Scan(&op, &phase)
		if errors.Is(e, pgx.ErrNoRows) {
			// the table was replaced: its operation is found by the name
			e = c.QueryRow(ctx, `SELECT o.op_id, o.phase FROM kingres.table_operation o
				WHERE o.kind = $1 AND NOT (o.spec ? 'mirror') AND o.phase NOT IN ('done', 'failed', 'cancelled')
				  AND o.spec->>'schema' = $2 AND o.spec->>'table' = $3 ORDER BY o.op_id DESC LIMIT 1`, kind, p.Schema, p.Table).Scan(&op, &phase)
			if errors.Is(e, pgx.ErrNoRows) {
				op = 0
				return nil
			}
		}
		if e != nil {
			return e
		}
		switched = phase == "cleaning"
		return nil
	})
	return op, switched, err
}

func (b pgBackend) Begin(ctx context.Context, p tableParams) (int64, error) {
	if op, _, err := b.UnfinishedOp(ctx, p); err != nil || op != 0 {
		return op, err
	}
	var op int64
	err := b.with(ctx, p.Database, func(c *pgx.Conn) error {
		if err := b.setViews(ctx, c, p); err != nil {
			return err
		}
		opts, _ := json.Marshal(reportOptions(p))
		kind := phaseKinds[p.Op]
		if p.Method == "reference" {
			kind = "distribute_reference"
		}
		return c.QueryRow(ctx, "SELECT kingres.begin_table_operation($1, $2::regclass, $3::jsonb)", kind, p.rel(), string(opts)).Scan(&op)
	})
	return op, tableSQLProblem(err)
}

func (b pgBackend) Advance(ctx context.Context, p tableParams, op int64, to string) (reached, detail string, err error) {
	err = b.with(ctx, p.Database, func(c *pgx.Conn) error {
		if err := c.QueryRow(ctx, "SELECT kingres.table_operation_advance($1, $2)", op, to).Scan(&reached); err != nil {
			return err
		}
		var d *string
		if err := c.QueryRow(ctx, "SELECT detail FROM kingres.table_operation WHERE op_id = $1", op).Scan(&d); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if d != nil {
			detail = *d
		}
		return nil
	})
	return reached, detail, tableSQLProblem(err)
}

func (b pgBackend) Progress(ctx context.Context, p tableParams, op int64) (done, total int64, err error) {
	err = b.with(ctx, p.Database, func(c *pgx.Conn) error {
		return c.QueryRow(ctx, "SELECT coalesce(rows_copied, 0), coalesce(rows_total, 0) FROM kingres.table_operations WHERE op_id = $1", op).Scan(&done, &total)
	})
	return done, total, err
}

func (b pgBackend) Verified(ctx context.Context, p tableParams, op int64) (v string, err error) {
	err = b.with(ctx, p.Database, func(c *pgx.Conn) error {
		var s *string
		if err := c.QueryRow(ctx, "SELECT verified FROM kingres.table_operation WHERE op_id = $1", op).Scan(&s); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if s != nil {
			v = *s
		}
		return nil
	})
	return v, err
}

func (b pgBackend) Blocking(ctx context.Context, p tableParams) error {
	return tableSQLProblem(b.with(ctx, p.Database, func(c *pgx.Conn) error {
		if err := b.setViews(ctx, c, p); err != nil {
			return err
		}
		switch p.Op {
		case "distribute":
			if p.Method == "reference" {
				var done bool
				if err := c.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM kingres.dist_table WHERE relid = $1::regclass)`, p.rel()).Scan(&done); err != nil || done {
					return err
				}
				_, err := c.Exec(ctx, "SELECT kingres.create_reference_table($1::regclass)", p.rel())
				return err
			}
			var done bool
			if err := c.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM kingres.dist_table WHERE relid = $1::regclass)`, p.rel()).Scan(&done); err != nil || done {
				return err
			}
			var coloc *string
			if p.ColocTable != "" {
				q := qualified(p.ColocSchema, p.ColocTable)
				coloc = &q
			}
			var buckets *int
			if p.BucketCount > 0 {
				buckets = &p.BucketCount
			}
			_, err := c.Exec(ctx, "SELECT kingres.create_distributed_table($1::regclass, $2::name, $3::regclass, $4::int)", p.rel(), p.Column, coloc, buckets)
			return err
		case "undistribute":
			var dist bool
			if err := c.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM kingres.dist_table WHERE relid = $1::regclass)`, p.rel()).Scan(&dist); err != nil || !dist {
				return err // a repeated step: the table is not distributed any more
			}
			_, err := c.Exec(ctx, "SELECT kingres.undistribute_table($1::regclass, $2, $3)", p.rel(), p.Cascade, p.TargetGroup)
			return err
		case "alter":
			var colocate *string
			if p.AlterColoc != "" {
				colocate = &p.AlterColoc
			}
			var col *string
			if p.AlterColumn != "" {
				col = &p.AlterColumn
			}
			var buckets *int
			if p.AlterBuckets > 0 {
				buckets = &p.AlterBuckets
			}
			// a repeated step finds the change done: the extension sees nothing to do
			_, err := c.Exec(ctx, "SELECT kingres.alter_distributed_table($1::regclass, $2::name, $3::int, $4::text, $5)", p.rel(), col, buckets, colocate, p.CascadeColoc)
			return err
		}
		return fmt.Errorf("unknown table operation %q", p.Op)
	}))
}

func (b pgBackend) Partition(ctx context.Context, p tableParams) error {
	return tableSQLProblem(b.with(ctx, p.Database, func(c *pgx.Conn) error {
		switch p.PartOp {
		case "create":
			_, err := c.Exec(ctx, "SELECT kingres.create_partition($1::regclass, $2, $3, $4)", p.rel(), p.PartName, p.PartBound, true)
			return err
		case "drop":
			_, err := c.Exec(ctx, "SELECT kingres.drop_partition($1::regclass, $2, $3)", p.rel(), p.PartName, true)
			return err
		case "detach":
			var exists bool
			if err := c.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM kingres.dist_partition WHERE parent = $1::regclass AND partition_name = $2)`, p.rel(), p.PartName).Scan(&exists); err != nil || !exists {
				return err // a repeated step: it is detached already
			}
			_, err := c.Exec(ctx, "SELECT kingres.detach_partition($1::regclass, $2)", p.rel(), p.PartName)
			return err
		case "attach":
			var exists bool
			if err := c.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM kingres.dist_partition WHERE parent = $1::regclass AND partition_name = $2)`, p.rel(), p.PartTable).Scan(&exists); err != nil || exists {
				return err // already attached
			}
			_, err := c.Exec(ctx, "SELECT kingres.attach_partition($1::regclass, $2::regclass, $3)", p.rel(), qualified(p.PartSchema, p.PartTable), p.PartBound)
			return err
		}
		return fmt.Errorf("unknown partition operation %q", p.PartOp)
	}))
}

func (b pgBackend) Cancel(ctx context.Context, p tableParams, op int64) error {
	return b.with(ctx, p.Database, func(c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SELECT kingres.cancel_table_operation($1)", op)
		return err
	})
}

// tableSQLProblem turns the refusals of the extension that a person can act on into tasks errors with a code: an operation that the extension
// refuses because a group is busy (55006) is retryable.
func tableSQLProblem(err error) error {
	if err == nil {
		return nil
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case "55006":
			return &tableTaskError{code: "operation_in_progress", retry: true, msg: pe.Message}
		case "0A000", "42P01", "42704":
			return &tableTaskError{code: CodeTableNotEligible, msg: pe.Message}
		}
	}
	return err
}

var _ = sqlx.NewIdent

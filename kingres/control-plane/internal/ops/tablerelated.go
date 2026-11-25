// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

// Distribute with related tables (contract 0.32.0, increment 9.7 of kingres/docs/design/shard-phase9.md, "A set of ordinary tables linked by
// foreign keys"). The sharding extension refuses a foreign key between a table that is distributed and one that is not, so a set of ordinary tables that
// are linked by keys cannot be distributed one by one. This file is the control plane's answer: one task that takes the keys between the tables out
// of the way, distributes the table and then each related table in its colocation group (one step each; every step looks at what exists, so a task
// that is run again goes on with the tables that are not distributed yet), and adds the keys back `NOT VALID` and validates them.
//
// What the extension offers and is used here: kingres.dist_table (is a table distributed, which tables the closure may not go through),
// kingres.table_change_report (the blockers of each table, per table), kingres.create_distributed_table with colocate_with (the related tables
// join the group of the table). Every read of kingres.* goes through the extension guard (pgBackend.with).
//
// The keys are captured when the request is made and kept in the input of the task, so that a crash after the drop loses nothing: the definitions
// are in the task, and a failed step tries to put them back before it reports.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sqlx"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// Step names of the task with related tables.
const (
	stepRelCheck    = "Check the tables"
	stepRelDrop     = "Take the foreign keys between the tables out"
	stepRelAdd      = "Add the foreign keys again"
	stepRelValidate = "Validate the foreign keys"
)

// relatedParam is a related table in the input of the task.
type relatedParam struct {
	Schema string `json:"schema"`
	Table  string `json:"table"`
	Column string `json:"column"`
}

// relatedKey is a foreign key between two tables of the set, as captured when the request was made.
type relatedKey struct {
	Name      string `json:"name"`
	Schema    string `json:"schema"`
	Table     string `json:"table"`
	RefSchema string `json:"refSchema"`
	RefTable  string `json:"refTable"`
	Def       string `json:"def"`
	Validated bool   `json:"validated"`
}

// relatedBackend is the database side that the task with related tables needs besides tableBackend.
type relatedBackend interface {
	// Graph returns the foreign key closure of the table over the tables that are not distributed.
	Graph(ctx context.Context, p tableParams) (relGraph, error)
	// Distributed says whether the table is in kingres.dist_table.
	Distributed(ctx context.Context, p tableParams) (bool, error)
	// DropKeys drops the keys that exist.
	DropKeys(ctx context.Context, p tableParams, keys []relatedKey) error
	// AddKeys adds the keys that do not exist (NOT VALID when the original was valid) and returns the ones that could not be added.
	AddKeys(ctx context.Context, p tableParams, keys []relatedKey) (failed []string, err error)
	// ValidateKeys validates the keys that were valid before.
	ValidateKeys(ctx context.Context, p tableParams, keys []relatedKey) error
}

func (o *Ops) relBackend() (relatedBackend, error) {
	if rb, ok := o.backend().(relatedBackend); ok {
		return rb, nil
	}
	return nil, problem.New(problem.UnsupportedOp, "This server cannot distribute related tables.")
}

// ---------------------------------------------------------------------------------------------------------------------------
// The plan: pure functions over the graph
// ---------------------------------------------------------------------------------------------------------------------------

type tableKey struct{ Schema, Table string }

func (k tableKey) String() string { return k.Schema + "." + k.Table }

type relNode struct {
	Key         tableKey
	Rows, Bytes *int64
	Distributed bool
}

type relEdge struct {
	Name      string
	Child     tableKey // the table that has the key
	Parent    tableKey // the table the key refers to
	Cols      []string
	RefCols   []string
	Def       string
	Validated bool
}

// relGraph is the closure of a table: its nodes and the keys between them.
type relGraph struct {
	Nodes []relNode
	Edges []relEdge
}

// relPlan is what the planner decides.
type relPlan struct {
	Tables   []relTable // the related tables in the order they are distributed
	Keys     []relEdge  // the keys between the tables of the set
	Blockers []api.TableBlocker
	Warnings []api.TableBlocker
}

type relTable struct {
	Key    tableKey
	Column string // empty when it follows from nothing
	Rows   *int64
	Bytes  *int64
}

// derive returns the column of the other end of an edge for a column of one end.
func derive(e relEdge, from tableKey, col string) string {
	cols, other := e.Cols, e.RefCols // from is the child: the column of the parent
	if from == e.Parent {
		cols, other = e.RefCols, e.Cols
	}
	for i, c := range cols {
		if c == col && i < len(other) {
			return other[i]
		}
	}
	return ""
}

// planRelated orders the closure, gives each related table its column and lists what stops the change. choice holds the columns the caller named;
// rootCol is the distribution column of the table itself ("" when it is not known yet: the columns are then left empty and not a blocker).
func planRelated(g relGraph, root tableKey, rootCol string, choice map[tableKey]string) relPlan {
	var plan relPlan
	node := map[tableKey]relNode{}
	for _, n := range g.Nodes {
		node[n.Key] = n
	}
	inSet := map[tableKey]bool{root: true}
	order := []tableKey{}
	queue := []tableKey{root}
	warned := map[tableKey]bool{}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, e := range g.Edges {
			var other tableKey
			switch cur {
			case e.Child:
				other = e.Parent
			case e.Parent:
				other = e.Child
			default:
				continue
			}
			if other == cur || inSet[other] {
				continue
			}
			if n, ok := node[other]; ok && n.Distributed {
				if !warned[other] {
					warned[other] = true
					plan.Warnings = append(plan.Warnings, api.TableBlocker{Code: "related_table_distributed", Object: other.String(),
						Message: fmt.Sprintf("table %s is linked by foreign key %q and is already distributed; the key stays, and the report of the extension decides whether it can", other, e.Name)})
				}
				continue
			}
			inSet[other] = true
			order = append(order, other)
			queue = append(queue, other)
		}
	}
	col := map[tableKey]string{root: rootCol}
	for k, c := range choice {
		if inSet[k] && k != root && c != "" {
			col[k] = c
		}
	}
	var keys []relEdge
	for _, e := range g.Edges {
		if inSet[e.Child] && inSet[e.Parent] {
			keys = append(keys, e)
		}
	}
	sort.SliceStable(keys, func(i, j int) bool {
		if keys[i].Child != keys[j].Child {
			return keys[i].Child.String() < keys[j].Child.String()
		}
		return keys[i].Name < keys[j].Name
	})
	for changed := true; changed; {
		changed = false
		for _, e := range keys {
			if c := col[e.Child]; c != "" && col[e.Parent] == "" {
				if d := derive(e, e.Child, c); d != "" {
					col[e.Parent] = d
					changed = true
				}
			}
			if c := col[e.Parent]; c != "" && col[e.Child] == "" {
				if d := derive(e, e.Parent, c); d != "" {
					col[e.Child] = d
					changed = true
				}
			}
		}
	}
	for _, k := range order {
		t := relTable{Key: k, Column: col[k], Rows: node[k].Rows, Bytes: node[k].Bytes}
		if t.Column == "" && rootCol != "" {
			plan.Blockers = append(plan.Blockers, api.TableBlocker{Code: "related_column_unknown", Object: k.String(),
				Message: fmt.Sprintf("the distribution column of %s does not follow from the foreign keys; name it", k)})
		}
		plan.Tables = append(plan.Tables, t)
	}
	plan.Keys = keys
	return plan
}

// checkChoice compares the tables a request names with the closure: a table of the closure that is missing and a table that is not in it are blockers;
// the columns named in the request win over the derived ones.
func checkChoice(plan relPlan, choice []api.RelatedTableChoice) (relPlan, error) {
	have := map[tableKey]bool{}
	for _, c := range choice {
		k := tableKey{c.Schema, c.Table}
		if have[k] {
			return plan, problem.Validation("The distribution request is invalid.", field("/relatedTables", fmt.Sprintf("%s is named twice", k), "duplicate"))
		}
		have[k] = true
	}
	inPlan := map[tableKey]bool{}
	for i := range plan.Tables {
		k := plan.Tables[i].Key
		inPlan[k] = true
		if !have[k] {
			plan.Blockers = append(plan.Blockers, api.TableBlocker{Code: "related_table_missing", Object: k.String(),
				Message: fmt.Sprintf("table %s is linked to the table by foreign keys and must be distributed with it", k)})
		}
	}
	for _, c := range choice {
		k := tableKey{c.Schema, c.Table}
		if !inPlan[k] {
			plan.Blockers = append(plan.Blockers, api.TableBlocker{Code: "related_table_unrelated", Object: k.String(),
				Message: fmt.Sprintf("table %s is not linked to the table by foreign keys", k)})
		}
	}
	return plan, nil
}

// filterSetBlockers drops the lines of the extension's report that the removal of the keys of the set clears: a key to or from an ordinary table
// of the set.
func filterSetBlockers(bl []api.TableBlocker, keys []relEdge) []api.TableBlocker {
	names := map[string]bool{}
	for _, k := range keys {
		names[k.Name] = true
	}
	var out []api.TableBlocker
	for _, b := range bl {
		if (b.Code == "foreign_key_plain_table" || b.Code == "referenced_by_plain_table") && names[b.Object] {
			continue
		}
		out = append(out, b)
	}
	return out
}

func keysOf(edges []relEdge) []relatedKey {
	out := make([]relatedKey, 0, len(edges))
	for _, e := range edges {
		out = append(out, relatedKey{Name: e.Name, Schema: e.Child.Schema, Table: e.Child.Table, RefSchema: e.Parent.Schema, RefTable: e.Parent.Table, Def: e.Def, Validated: e.Validated})
	}
	return out
}

// relTables is the root and the related tables of the task, in the order of distribution.
func (p tableParams) relTable(i int) tableParams {
	q := p
	q.Related, q.Keys, q.Online = nil, nil, false
	if i == 0 {
		return q
	}
	r := p.Related[i-1]
	q.Schema, q.Table, q.Column = r.Schema, r.Table, r.Column
	q.ColocSchema, q.ColocTable, q.BucketCount = p.Schema, p.Table, 0
	return q
}

// ---------------------------------------------------------------------------------------------------------------------------
// The request
// ---------------------------------------------------------------------------------------------------------------------------

// relatedPlan reads the closure of the table and plans it. choice is nil for the plan that listRelatedTables answers.
func (o *Ops) relatedPlan(ctx context.Context, p tableParams, rootCol string, choice []api.RelatedTableChoice, check bool) (relPlan, error) {
	rb, err := o.relBackend()
	if err != nil {
		return relPlan{}, err
	}
	g, err := rb.Graph(ctx, p)
	if err != nil {
		return relPlan{}, wrap(err)
	}
	cm := map[tableKey]string{}
	for _, c := range choice {
		cm[tableKey{c.Schema, c.Table}] = c.DistributionColumn
	}
	plan := planRelated(g, tableKey{p.Schema, p.Table}, rootCol, cm)
	if check {
		if plan, err = checkChoice(plan, choice); err != nil {
			return plan, err
		}
	}
	// a table that another table task or an operation of the extension holds
	busy := o.TableOperations(ctx, p.Database)
	for _, k := range append([]tableKey{{p.Schema, p.Table}}, tableKeys(plan.Tables)...) {
		if op, ok := busy[tableSubject(p.Database, k.Schema, k.Table)]; ok {
			plan.Blockers = append(plan.Blockers, api.TableBlocker{Code: "related_table_busy", Object: k.String(),
				Message: fmt.Sprintf("table %s has an unfinished operation (%s)", k, op.Kind)})
		}
	}
	return plan, nil
}

func tableKeys(ts []relTable) []tableKey {
	out := make([]tableKey, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Key)
	}
	return out
}

// ListRelatedTables answers listRelatedTables.
func (o *Ops) ListRelatedTables(ctx context.Context, db, schema, table, column string) (api.RelatedTablesPlan, error) {
	if err := checkNames(db, schema, table); err != nil {
		return api.RelatedTablesPlan{}, err
	}
	if column != "" {
		if _, err := sqlx.NewIdent(column); err != nil {
			return api.RelatedTablesPlan{}, problem.Validation("The request is invalid.", field("distributionColumn", "is not a valid column name", "pattern"))
		}
	}
	p := tableParams{V: 2, Op: "distribute", Method: "hash", Database: db, Schema: schema, Table: table, Column: column}
	if err := o.asService(ctx, db, func(tx pgx.Tx) error {
		if ok, err := tableExists(ctx, tx, schema, table); err != nil {
			return err
		} else if !ok {
			return problem.New(problem.NotFound, fmt.Sprintf("Table %s.%s does not exist.", schema, table))
		}
		return nil
	}); err != nil {
		return api.RelatedTablesPlan{}, wrap(err)
	}
	rb, err := o.relBackend()
	if err != nil {
		return api.RelatedTablesPlan{}, err
	}
	if done, err := rb.Distributed(ctx, p); err != nil {
		return api.RelatedTablesPlan{}, wrap(err)
	} else if done {
		return api.RelatedTablesPlan{}, notEligible([]api.TableBlocker{{Code: "not_plain", Object: schema + "." + table, Message: "the table is already distributed"}})
	}
	plan, err := o.relatedPlan(ctx, p, column, nil, false)
	if err != nil {
		return api.RelatedTablesPlan{}, err
	}
	out := api.RelatedTablesPlan{Tables: []api.RelatedTable{}, Keys: []api.RelatedKey{}, Blockers: nz(plan.Blockers), Warnings: nz(plan.Warnings),
		ColocateWith: api.TableRef{Schema: schema, Table: table}}
	for _, t := range plan.Tables {
		rt := api.RelatedTable{Schema: t.Key.Schema, Table: t.Key.Table, EstimatedRows: t.Rows, SizeBytes: t.Bytes}
		if t.Column != "" {
			c := t.Column
			rt.DistributionColumn = &c
		}
		out.Tables = append(out.Tables, rt)
	}
	for _, k := range plan.Keys {
		out.Keys = append(out.Keys, api.RelatedKey{Name: k.Name, Schema: k.Child.Schema, Table: k.Child.Table, ReferencesSchema: k.Parent.Schema,
			ReferencesTable: k.Parent.Table, Definition: k.Def})
	}
	return out, nil
}

// prepareRelated adds the related tables to the input of a distribute task: the closure must be named whole, every table needs a column, the keys
// between the tables are captured, and only the blocking copy is used. The extension's report of each table is read here too.
func (o *Ops) prepareRelated(ctx context.Context, p tableParams, req api.DistributeTableRequest) (tableParams, error) {
	if req.Method != "hash" {
		return p, problem.Validation("The distribution request is invalid.", field("/relatedTables", "is allowed for method hash only", "forbidden"))
	}
	if req.Mode == "online" {
		return p, problem.Validation("The distribution request is invalid.", field("/mode", "tables distributed together are copied in the blocking mode only", "enum"))
	}
	plan, err := o.relatedPlan(ctx, p, p.Column, req.RelatedTables, true)
	if err != nil {
		return p, err
	}
	if len(plan.Blockers) > 0 {
		return p, notEligible(plan.Blockers)
	}
	p.Online = false
	p.Related = nil
	for _, t := range plan.Tables {
		p.Related = append(p.Related, relatedParam{Schema: t.Key.Schema, Table: t.Key.Table, Column: t.Column})
	}
	p.Keys = keysOf(plan.Keys)
	p.LinkedTables = len(plan.Tables)
	if bl, err := o.checkSet(ctx, p, plan.Keys); err != nil {
		return p, wrap(err)
	} else if len(bl) > 0 {
		return p, notEligible(bl)
	}
	b := o.backend()
	rows, bytes, _, err := b.Sizes(ctx, p)
	if err != nil {
		return p, wrap(err)
	}
	p.EstRows, p.EstBytes = rows, bytes
	for _, t := range plan.Tables {
		if t.Rows != nil {
			p.EstRows += *t.Rows
		}
		if t.Bytes != nil {
			p.EstBytes += *t.Bytes
		}
	}
	return p, nil
}

// checkSet runs the report of the extension for each table of the set (blocking mode) and returns the blockers that the removal of the keys of the
// set does not clear. A table that is distributed already (a task that is run again) is not checked.
func (o *Ops) checkSet(ctx context.Context, p tableParams, keys []relEdge) ([]api.TableBlocker, error) {
	rb, err := o.relBackend()
	if err != nil {
		return nil, err
	}
	var all []api.TableBlocker
	for i := 0; i <= len(p.Related); i++ {
		q := p.relTable(i)
		if done, err := rb.Distributed(ctx, q); err != nil {
			return nil, err
		} else if done {
			continue
		}
		if i > 0 {
			q.ColocSchema, q.ColocTable = "", "" // the table it joins is not distributed yet
		}
		bl, _, _, err := o.backend().Report(ctx, q)
		if err != nil {
			return nil, err
		}
		all = append(all, filterSetBlockers(bl, keys)...)
	}
	return all, nil
}

// keysAsEdges rebuilds the edges of the keys of the input (the report filter needs the names only).
func keysAsEdges(keys []relatedKey) []relEdge {
	out := make([]relEdge, 0, len(keys))
	for _, k := range keys {
		out = append(out, relEdge{Name: k.Name})
	}
	return out
}

// previewRelated adds the related tables to the preview of a distribute.
func (o *Ops) previewRelated(ctx context.Context, p tableParams, d *api.DistributeTableRequest, out *api.TableChangePreview) error {
	plan, err := o.relatedPlan(ctx, p, p.Column, d.RelatedTables, true)
	if err != nil {
		return err
	}
	if d.Mode == "online" {
		plan.Blockers = append(plan.Blockers, api.TableBlocker{Code: "related_online", Object: p.Schema + "." + p.Table,
			Message: "tables distributed together are copied in the blocking mode only"})
	}
	out.Blockers = append(filterSetBlockers(out.Blockers, plan.Keys), plan.Blockers...)
	out.Warnings = append(out.Warnings, plan.Warnings...)
	out.Warnings = append(out.Warnings, api.TableBlocker{Code: "related_keys_not_enforced", Object: p.Schema + "." + p.Table,
		Message: "no foreign key between these tables is enforced from the moment the keys are taken out until they are added again"})
	for _, k := range plan.Keys {
		out.Notes = append(out.Notes, api.TableBlocker{Code: "related_key", Object: k.Name,
			Message: fmt.Sprintf("key %q of %s is dropped and added again (%s)", k.Name, k.Child, k.Def)})
	}
	rows, bytes := int64(0), int64(0)
	if out.Estimate.Rows != nil {
		rows = *out.Estimate.Rows
	}
	if out.Estimate.Bytes != nil {
		bytes = *out.Estimate.Bytes
	}
	for _, t := range plan.Tables {
		out.AffectedTables = append(out.AffectedTables, api.TableChangeTable{Schema: t.Key.Schema, Name: t.Key.Table, Role: "related", SizeBytes: t.Bytes, EstimatedRows: t.Rows})
		if t.Rows != nil {
			rows += *t.Rows
		}
		if t.Bytes != nil {
			bytes += *t.Bytes
		}
	}
	out.Estimate.Rows, out.Estimate.Bytes = &rows, &bytes
	out.Estimate.OnlineAvailable, out.Estimate.ModeRecommended, out.Estimate.WritersBlocked = false, "blocking", "wholeCopy"
	reason := "tables distributed together are copied in the blocking mode only"
	out.Estimate.OnlineUnavailableReason = &reason
	return nil
}

// ---------------------------------------------------------------------------------------------------------------------------
// The steps
// ---------------------------------------------------------------------------------------------------------------------------

func (o *Ops) relatedSteps(p tableParams) []task.Step {
	steps := []task.Step{
		{Name: stepRelCheck, Run: o.stepRelCheck},
		{Name: stepFreeSpace, Run: o.stepRelFree},
		{Name: stepRelDrop, Run: o.stepRelDrop},
	}
	for i := 0; i <= len(p.Related); i++ {
		i := i
		name := "Distribute " + p.Schema + "." + p.Table
		if i > 0 {
			name = "Distribute " + p.Related[i-1].Schema + "." + p.Related[i-1].Table
		}
		steps = append(steps, task.Step{Name: name, Run: func(ctx context.Context, env *task.Env) error { return o.stepRelDistribute(ctx, env, i) }})
	}
	return append(steps,
		task.Step{Name: stepRelAdd, Run: func(ctx context.Context, env *task.Env) error { return o.stepRelAdd(ctx, env) }},
		task.Step{Name: stepRelValidate, Run: o.stepRelValidate})
}

func (o *Ops) stepRelCheck(ctx context.Context, env *task.Env) error {
	p, err := inputOf[tableParams](env.Task)
	if err != nil {
		return err
	}
	bl, err := o.checkSet(ctx, p, keysAsEdges(p.Keys))
	if err != nil {
		return err
	}
	if len(bl) > 0 {
		return &tableTaskError{code: CodeTableNotEligible, msg: "The tables cannot take the change: " + describeBlockers(bl)}
	}
	return nil
}

func (o *Ops) stepRelFree(ctx context.Context, env *task.Env) error {
	p, err := inputOf[tableParams](env.Task)
	if err != nil {
		return err
	}
	if o.DiskFree == nil {
		return task.Skip("this server cannot tell the free space of the servers")
	}
	var total int64
	var servers []string
	for i := 0; i <= len(p.Related); i++ {
		_, bytes, s, err := o.backend().Sizes(ctx, p.relTable(i))
		if err != nil {
			return err
		}
		total += bytes
		servers = s
	}
	if bad := freeSpaceShortfall(p, total, servers, func(s string) (int64, bool) { return o.DiskFree(ctx, s) }); bad != "" {
		return &tableTaskError{code: CodeInsufficientDisk, msg: bad}
	}
	return nil
}

// failRelated puts the keys back where it can and returns the error of the step with the keys that could not be put back named in it.
func (o *Ops) failRelated(ctx context.Context, p tableParams, cause error) error {
	code, retry := task.CodeStepFailed, false
	var c task.Coded
	if errors.As(cause, &c) {
		code, retry = c.TaskCode(), c.TaskRetryable()
	}
	msg := cause.Error()
	if rb, err := o.relBackend(); err == nil {
		failed, aerr := rb.AddKeys(context.WithoutCancel(ctx), p, p.Keys)
		switch {
		case aerr != nil:
			msg += "; the foreign keys could not be put back: " + aerr.Error()
		case len(failed) > 0:
			msg += "; these foreign keys were not put back: " + strings.Join(failed, "; ")
		}
	}
	return &tableTaskError{code: code, retry: retry, msg: msg}
}

func (o *Ops) stepRelDrop(ctx context.Context, env *task.Env) error {
	p, err := inputOf[tableParams](env.Task)
	if err != nil {
		return err
	}
	rb, err := o.relBackend()
	if err != nil {
		return err
	}
	if len(p.Keys) > 0 {
		_ = env.SetDetail(ctx, "No foreign key between these tables is enforced until they are added again.")
	}
	return o.whileLocked(ctx, env, func() error { return rb.DropKeys(ctx, p, p.Keys) })
}

func (o *Ops) stepRelDistribute(ctx context.Context, env *task.Env, i int) error {
	p, err := inputOf[tableParams](env.Task)
	if err != nil {
		return err
	}
	rb, err := o.relBackend()
	if err != nil {
		return err
	}
	q := p.relTable(i)
	if done, err := rb.Distributed(ctx, q); err != nil {
		return o.failRelated(ctx, p, err)
	} else if done {
		return nil
	}
	_ = env.SetDetail(ctx, "Writers of "+q.Schema+"."+q.Table+" wait during this step. No foreign key between the tables is enforced.")
	if err := o.whileLocked(ctx, env, func() error { return o.backend().Blocking(ctx, q) }); err != nil {
		return o.failRelated(ctx, p, err)
	}
	return nil
}

func (o *Ops) stepRelAdd(ctx context.Context, env *task.Env) error {
	p, err := inputOf[tableParams](env.Task)
	if err != nil {
		return err
	}
	rb, err := o.relBackend()
	if err != nil {
		return err
	}
	failed, err := rb.AddKeys(ctx, p, p.Keys)
	if err != nil {
		return o.failRelated(ctx, p, err)
	}
	if len(failed) > 0 {
		return &tableTaskError{code: task.CodeStepFailed, msg: "These foreign keys could not be added again: " + strings.Join(failed, "; ")}
	}
	_ = env.SetDetail(ctx, "")
	return nil
}

func (o *Ops) stepRelValidate(ctx context.Context, env *task.Env) error {
	p, err := inputOf[tableParams](env.Task)
	if err != nil {
		return err
	}
	rb, err := o.relBackend()
	if err != nil {
		return err
	}
	return o.whileLocked(ctx, env, func() error { return rb.ValidateKeys(ctx, p, p.Keys) })
}

// ---------------------------------------------------------------------------------------------------------------------------
// The production backend
// ---------------------------------------------------------------------------------------------------------------------------

// The closure does not go through a table that is distributed: such a table is a node of the graph and not expanded.
const relatedGraphCTE = `WITH RECURSIVE e AS (
	SELECT k.oid, k.conrelid, k.confrelid, k.conname, k.conkey, k.confkey, pg_catalog.pg_get_constraintdef(k.oid) AS def, k.convalidated
	  FROM pg_catalog.pg_constraint k
	 WHERE k.contype = 'f' AND k.conparentid = 0 AND k.conrelid <> k.confrelid
), cl(relid, dist) AS (
	SELECT r.oid, EXISTS (SELECT 1 FROM kingres.dist_table d WHERE d.relid = r.oid::pg_catalog.regclass)
	  FROM (SELECT $1::pg_catalog.regclass::pg_catalog.oid AS oid) r
	UNION
	SELECT x.relid, EXISTS (SELECT 1 FROM kingres.dist_table d WHERE d.relid = x.relid::pg_catalog.regclass)
	  FROM cl, e,
	       LATERAL (SELECT CASE WHEN e.conrelid = cl.relid THEN e.confrelid ELSE e.conrelid END AS relid) x
	 WHERE NOT cl.dist AND (e.conrelid = cl.relid OR e.confrelid = cl.relid)
)`

func (b pgBackend) Graph(ctx context.Context, p tableParams) (g relGraph, err error) {
	err = b.with(ctx, p.Database, func(c *pgx.Conn) error {
		rows, err := c.Query(ctx, relatedGraphCTE+`
			SELECT n.nspname, r.relname, greatest(r.reltuples, 0)::pg_catalog.int8, pg_catalog.pg_total_relation_size(r.oid), cl.dist
			  FROM cl JOIN pg_catalog.pg_class r ON r.oid = cl.relid JOIN pg_catalog.pg_namespace n ON n.oid = r.relnamespace
			 ORDER BY n.nspname, r.relname`, p.rel())
		if err != nil {
			return err
		}
		for rows.Next() {
			var n relNode
			var tr, tb int64
			if err := rows.Scan(&n.Key.Schema, &n.Key.Table, &tr, &tb, &n.Distributed); err != nil {
				rows.Close()
				return err
			}
			n.Rows, n.Bytes = &tr, &tb
			g.Nodes = append(g.Nodes, n)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		rows, err = c.Query(ctx, relatedGraphCTE+`
			SELECT e.conname, n1.nspname, c1.relname, n2.nspname, c2.relname,
			       (SELECT pg_catalog.array_agg(a.attname::pg_catalog.text ORDER BY u.ord) FROM pg_catalog.unnest(e.conkey) WITH ORDINALITY u(attnum, ord)
			         JOIN pg_catalog.pg_attribute a ON a.attrelid = e.conrelid AND a.attnum = u.attnum),
			       (SELECT pg_catalog.array_agg(a.attname::pg_catalog.text ORDER BY u.ord) FROM pg_catalog.unnest(e.confkey) WITH ORDINALITY u(attnum, ord)
			         JOIN pg_catalog.pg_attribute a ON a.attrelid = e.confrelid AND a.attnum = u.attnum),
			       e.def, e.convalidated
			  FROM e
			  JOIN pg_catalog.pg_class c1 ON c1.oid = e.conrelid JOIN pg_catalog.pg_namespace n1 ON n1.oid = c1.relnamespace
			  JOIN pg_catalog.pg_class c2 ON c2.oid = e.confrelid JOIN pg_catalog.pg_namespace n2 ON n2.oid = c2.relnamespace
			 WHERE e.conrelid IN (SELECT relid FROM cl) AND e.confrelid IN (SELECT relid FROM cl)
			 ORDER BY n1.nspname, c1.relname, e.conname`, p.rel())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e relEdge
			if err := rows.Scan(&e.Name, &e.Child.Schema, &e.Child.Table, &e.Parent.Schema, &e.Parent.Table, &e.Cols, &e.RefCols, &e.Def, &e.Validated); err != nil {
				return err
			}
			g.Edges = append(g.Edges, e)
		}
		return rows.Err()
	})
	return g, err
}

func (b pgBackend) Distributed(ctx context.Context, p tableParams) (done bool, err error) {
	err = b.with(ctx, p.Database, func(c *pgx.Conn) error {
		return c.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM kingres.dist_table WHERE relid = $1::regclass)`, p.rel()).Scan(&done)
	})
	return done, err
}

func keyExists(ctx context.Context, c *pgx.Conn, k relatedKey) (bool, error) {
	var ok bool
	err := c.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint WHERE conrelid = to_regclass($1) AND conname = $2 AND contype = 'f')`,
		qualified(k.Schema, k.Table), k.Name).Scan(&ok)
	return ok, err
}

func (b pgBackend) DropKeys(ctx context.Context, p tableParams, keys []relatedKey) error {
	return tableSQLProblem(b.with(ctx, p.Database, func(c *pgx.Conn) error {
		for _, k := range keys {
			name, err := sqlx.NewIdent(k.Name)
			if err != nil {
				return err
			}
			if _, err := c.Exec(ctx, "ALTER TABLE IF EXISTS "+qualified(k.Schema, k.Table)+" DROP CONSTRAINT IF EXISTS "+name.String()); err != nil {
				return err
			}
		}
		return nil
	}))
}

func (b pgBackend) AddKeys(ctx context.Context, p tableParams, keys []relatedKey) (failed []string, err error) {
	err = b.with(ctx, p.Database, func(c *pgx.Conn) error {
		for _, k := range keys {
			if ok, err := keyExists(ctx, c, k); err != nil {
				return err
			} else if ok {
				continue
			}
			name, err := sqlx.NewIdent(k.Name)
			if err != nil {
				return err
			}
			stmt := "ALTER TABLE " + qualified(k.Schema, k.Table) + " ADD CONSTRAINT " + name.String() + " " + k.Def
			// NOT VALID first, so that the rows are checked once, by VALIDATE, outside the lock of the add; a key that the extension does not take
			// NOT VALID is added valid.
			_, err = c.Exec(ctx, stmt+" NOT VALID")
			var pe *pgconn.PgError
			if err != nil && errors.As(err, &pe) && (pe.Code == "0A000" || pe.Code == "42601") {
				_, err = c.Exec(ctx, stmt)
			}
			if err != nil {
				failed = append(failed, fmt.Sprintf("%s on %s.%s (%s): %s", k.Name, k.Schema, k.Table, k.Def, err.Error()))
			}
		}
		return nil
	})
	return failed, err
}

func (b pgBackend) ValidateKeys(ctx context.Context, p tableParams, keys []relatedKey) error {
	return tableSQLProblem(b.with(ctx, p.Database, func(c *pgx.Conn) error {
		for _, k := range keys {
			if !k.Validated {
				continue
			}
			name, err := sqlx.NewIdent(k.Name)
			if err != nil {
				return err
			}
			var valid bool
			if err := c.QueryRow(ctx, `SELECT convalidated FROM pg_catalog.pg_constraint WHERE conrelid = to_regclass($1) AND conname = $2 AND contype = 'f'`,
				qualified(k.Schema, k.Table), k.Name).Scan(&valid); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return fmt.Errorf("the foreign key %s on %s.%s does not exist", k.Name, k.Schema, k.Table)
				}
				return err
			}
			if valid {
				continue
			}
			if _, err := c.Exec(ctx, "ALTER TABLE "+qualified(k.Schema, k.Table)+" VALIDATE CONSTRAINT "+name.String()); err != nil {
				return err
			}
		}
		return nil
	}))
}

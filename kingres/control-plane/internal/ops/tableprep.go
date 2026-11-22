// SPDX-License-Identifier: AGPL-3.0-only

package ops

// The requests of the table lifecycle: what is checked before a task exists (the shape of the request, the table, the report of the extension,
// the mode), so that a table that cannot take the change is refused with 422 table_not_eligible and its blockers, and the task starts only for
// a change that can be made.

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sqlx"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

func checkModes(mode, views string, errs []problem.FieldError) []problem.FieldError {
	switch mode {
	case "", "auto", "online", "blocking":
	default:
		errs = append(errs, field("/mode", "must be auto, online or blocking", "enum"))
	}
	switch views {
	case "", "refuse", "recreate":
	default:
		errs = append(errs, field("/dependentViews", "must be refuse or recreate", "enum"))
	}
	return errs
}

func checkNames(db, schema, table string) error {
	if _, err := sqlx.QualifiedIdent(schema, table); err != nil {
		return problem.New(problem.NotFound, "No such table.")
	}
	if _, err := sqlx.NewIdent(db); err != nil {
		return problem.New(problem.NotFound, "No such database.")
	}
	return nil
}

// notEligible is the problem of a table that cannot take the change.
func notEligible(b []api.TableBlocker) error {
	return problem.New(problem.TableNotEligible, "The table cannot take the requested change: "+describeBlockers(b)).WithBlockers(b)
}

// rebalanceRunning refuses a table change while a rebalance is unfinished (a rebalance moves the buckets of the groups the change rebuilds).
func (o *Ops) rebalanceRunning(ctx context.Context) error {
	eng := o.Engine()
	if eng == nil {
		return nil
	}
	ts, err := eng.List(ctx, 200)
	if err != nil {
		return nil // the engine that starts the task finds out the rest
	}
	for _, t := range ts {
		if t.Kind == "rebalance" && t.State.Active() {
			return problem.New(problem.RebalanceInProgress, "A rebalance is unfinished; a table operation waits until it ends.")
		}
	}
	return nil
}

// resolveMode decides online or blocking. online asks for the online copy (refused with the blockers of the online report when the table cannot
// take it), blocking for the blocking one, auto picks online for a table with a usable key and more than onlineRowsThreshold estimated rows.
func (o *Ops) resolveMode(ctx context.Context, mode string, p tableParams) (tableParams, error) {
	b := o.backend()
	rows, bytes, _, err := b.Sizes(ctx, p)
	if err != nil {
		return p, wrap(err)
	}
	p.EstRows, p.EstBytes = rows, bytes
	try := func(online bool) ([]api.TableBlocker, error) {
		q := p
		q.Online = online
		bl, _, _, err := b.Report(ctx, q)
		return bl, err
	}
	switch mode {
	case "online":
		bl, err := try(true)
		if err != nil {
			return p, wrap(err)
		}
		if len(bl) > 0 {
			return p, notEligible(bl)
		}
		p.Online = true
	case "blocking":
		bl, err := try(false)
		if err != nil {
			return p, wrap(err)
		}
		if len(bl) > 0 {
			return p, notEligible(bl)
		}
	default:
		onl, err := try(true)
		if err != nil {
			return p, wrap(err)
		}
		if len(onl) == 0 && rows > onlineRowsThreshold {
			p.Online = true
			return p, nil
		}
		bl, err := try(false)
		if err != nil {
			return p, wrap(err)
		}
		if len(bl) > 0 {
			return p, notEligible(bl)
		}
	}
	return p, nil
}

// PrepareDistribute validates a distribute request against the catalog of the database and returns the task input.
func (o *Ops) PrepareDistribute(ctx context.Context, db, schema, table string, req api.DistributeTableRequest) (any, error) {
	var errs []problem.FieldError
	switch req.Method {
	case "hash":
		if req.DistributionColumn == "" {
			errs = append(errs, field("/distributionColumn", "is required for method hash", "required"))
		}
	case "reference":
		if req.DistributionColumn != "" {
			errs = append(errs, field("/distributionColumn", "is not allowed for method reference", "forbidden"))
		}
		if req.ColocateWith != nil {
			errs = append(errs, field("/colocateWith", "is not allowed for method reference", "forbidden"))
		}
	default:
		errs = append(errs, field("/method", "must be hash or reference", "enum"))
	}
	errs = checkModes(req.Mode, req.DependentViews, errs)
	p := tableParams{V: 2, Op: "distribute", Method: req.Method, Database: db, Schema: schema, Table: table, Column: req.DistributionColumn, DependentViews: req.DependentViews}
	if err := checkNames(db, schema, table); err != nil {
		return nil, err
	}
	if req.DistributionColumn != "" {
		if _, err := sqlx.NewIdent(req.DistributionColumn); err != nil {
			errs = append(errs, field("/distributionColumn", "is not a valid column name", "pattern"))
		}
	}
	if req.BucketCount != nil {
		if *req.BucketCount < 1 || *req.BucketCount > 4096 {
			errs = append(errs, field("/bucketCount", "must be between 1 and 4096", "range"))
		}
		p.BucketCount = *req.BucketCount
	}
	if c := req.ColocateWith; c != nil {
		if _, err := sqlx.QualifiedIdent(c.Schema, c.Table); err != nil {
			errs = append(errs, field("/colocateWith", "is not a valid table reference", "pattern"))
		}
		p.ColocSchema, p.ColocTable = c.Schema, c.Table
	}
	if len(errs) > 0 {
		return nil, problem.Validation("The distribution request is invalid.", errs...)
	}
	if p.BucketCount == 0 && req.Method == "hash" {
		// the bucket count of the database; a database the control plane did not create has the extension's default
		var n *int
		if err := o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SELECT bucket_count FROM kingres_cp.database_setting WHERE database_name = $1", db).Scan(&n)
		}); err == nil && n != nil {
			p.BucketCount = *n
		}
	}
	err := o.asService(ctx, db, func(tx pgx.Tx) error {
		if ok, err := tableExists(ctx, tx, schema, table); err != nil {
			return err
		} else if !ok {
			return problem.New(problem.NotFound, fmt.Sprintf("Table %s.%s does not exist.", schema, table))
		}
		if req.Method == "hash" {
			var has bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_attribute a WHERE a.attrelid = to_regclass($1)
				AND a.attname = $2 AND a.attnum > 0 AND NOT a.attisdropped)`, qualified(schema, table), req.DistributionColumn).Scan(&has); err != nil {
				return err
			}
			if !has {
				return problem.Validation("The distribution request is invalid.", field("/distributionColumn", "the table has no such column", "not_found"))
			}
		}
		var ledgerTable, haveLedger bool
		if err := tx.QueryRow(ctx, `SELECT to_regclass('kingres.ledger_table') IS NOT NULL`).Scan(&haveLedger); err != nil {
			return err
		}
		if haveLedger {
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM kingres.ledger_table WHERE relid = to_regclass($1))`, qualified(schema, table)).Scan(&ledgerTable); err != nil {
				return err
			}
		}
		if ledgerTable {
			return problem.New(problem.Unprocessable, fmt.Sprintf("Table %s.%s is a ledger table; a ledger table cannot be distributed or made a reference table.", schema, table))
		}
		return nil
	})
	if err != nil {
		return nil, wrap(err)
	}
	if err := o.rebalanceRunning(ctx); err != nil {
		return nil, err
	}
	p, err = o.resolveMode(ctx, req.Mode, p)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// PrepareUndistribute validates an undistribute request and returns the task input.
func (o *Ops) PrepareUndistribute(ctx context.Context, db, schema, table string, opt api.UndistributeOptions) (any, error) {
	errs := checkModes(opt.Mode, opt.DependentViews, nil)
	if opt.TargetGroupID < 0 {
		errs = append(errs, field("targetGroupId", "must not be negative", "range"))
	}
	if err := checkNames(db, schema, table); err != nil {
		return nil, err
	}
	if len(errs) > 0 {
		return nil, problem.Validation("The undistribute request is invalid.", errs...)
	}
	p := tableParams{V: 2, Op: "undistribute", Database: db, Schema: schema, Table: table, Cascade: opt.CascadeViaForeignKeys,
		TargetGroup: opt.TargetGroupID, DependentViews: opt.DependentViews}
	if err := o.mustBeDistributed(ctx, &p); err != nil {
		return nil, err
	}
	if err := o.rebalanceRunning(ctx); err != nil {
		return nil, err
	}
	return o.resolveMode(ctx, opt.Mode, p)
}

// mustBeDistributed reads the method of the table (hash or reference) and counts the tables its foreign keys link to it.
func (o *Ops) mustBeDistributed(ctx context.Context, p *tableParams) error {
	return wrap(o.asService(ctx, p.Database, func(tx pgx.Tx) error {
		if ok, err := tableExists(ctx, tx, p.Schema, p.Table); err != nil {
			return err
		} else if !ok {
			return problem.New(problem.NotFound, fmt.Sprintf("Table %s.%s does not exist.", p.Schema, p.Table))
		}
		var method *string
		if err := tx.QueryRow(ctx, `SELECT (SELECT method FROM kingres.dist_table WHERE relid = $1::regclass)`, p.rel()).Scan(&method); err != nil {
			return err
		}
		if method == nil {
			return notEligible([]api.TableBlocker{{Code: "not_distributed", Object: p.Schema + "." + p.Table, Message: "the table is not distributed"}})
		}
		p.Method = *method
		return tx.QueryRow(ctx, `SELECT count(*) - 1 FROM kingres.internal_fk_component($1::regclass, true)`, p.rel()).Scan(&p.LinkedTables)
	}))
}

// PrepareAlter validates an alter request and returns the task input.
func (o *Ops) PrepareAlter(ctx context.Context, db, schema, table string, req api.AlterTableDistributionRequest) (any, error) {
	errs := checkModes(req.Mode, req.DependentViews, nil)
	if req.DistributionColumn == "" && req.BucketCount == nil && req.ColocateWith == nil {
		errs = append(errs, field("", "at least one of distributionColumn, bucketCount and colocateWith is required", "required"))
	}
	p := tableParams{V: 2, Op: "alter", Database: db, Schema: schema, Table: table, DependentViews: req.DependentViews, CascadeColoc: req.CascadeToColocated}
	if err := checkNames(db, schema, table); err != nil {
		return nil, err
	}
	if req.DistributionColumn != "" {
		if _, err := sqlx.NewIdent(req.DistributionColumn); err != nil {
			errs = append(errs, field("/distributionColumn", "is not a valid column name", "pattern"))
		}
		p.AlterColumn = req.DistributionColumn
	}
	if req.BucketCount != nil {
		if *req.BucketCount < 1 || *req.BucketCount > 4096 {
			errs = append(errs, field("/bucketCount", "must be between 1 and 4096", "range"))
		}
		p.AlterBuckets = *req.BucketCount
	}
	if c := req.ColocateWith; c != nil {
		if c.Table != nil {
			if _, err := sqlx.QualifiedIdent(c.Table.Schema, c.Table.Table); err != nil {
				errs = append(errs, field("/colocateWith", "is not a valid table reference", "pattern"))
			}
			p.AlterColoc = qualified(c.Table.Schema, c.Table.Table)
		} else {
			p.AlterColoc = c.Keyword
		}
	}
	if req.CascadeToColocated && req.Mode == "online" {
		errs = append(errs, field("/cascadeToColocated", "needs mode blocking", "conflict"))
	}
	if len(errs) > 0 {
		return nil, problem.Validation("The alter request is invalid.", errs...)
	}
	if err := o.mustBeDistributed(ctx, &p); err != nil {
		return nil, err
	}
	if p.Method != "hash" {
		return nil, notEligible([]api.TableBlocker{{Code: "not_distributed", Object: p.Schema + "." + p.Table, Message: "only a hash-distributed table can be altered"}})
	}
	if err := o.rebalanceRunning(ctx); err != nil {
		return nil, err
	}
	mode := req.Mode
	if req.CascadeToColocated {
		mode = "blocking"
	}
	return o.resolveMode(ctx, mode, p)
}

// PreparePartition validates a partition request (create, attach, detach, drop) and returns the task input.
func (o *Ops) PreparePartition(ctx context.Context, db, schema, table, op, name string, create *api.CreateTablePartitionRequest, attach *api.AttachTablePartitionRequest) (any, error) {
	if err := checkNames(db, schema, table); err != nil {
		return nil, err
	}
	p := tableParams{V: 2, Op: "partition", PartOp: op, Database: db, Schema: schema, Table: table, PartName: name}
	var errs []problem.FieldError
	switch op {
	case "create":
		p.PartName, p.PartBound, p.IfNotExists = create.Name, create.Bound, create.IfNotExists
		errs = append(errs, checkPartitionName(create.Name)...)
		errs = append(errs, checkBound(create.Bound)...)
	case "attach":
		p.PartSchema, p.PartTable, p.PartBound = attach.Table.Schema, attach.Table.Table, attach.Bound
		p.PartName = attach.Table.Table
		if _, err := sqlx.QualifiedIdent(attach.Table.Schema, attach.Table.Table); err != nil {
			errs = append(errs, field("/table", "is not a valid table reference", "pattern"))
		}
		errs = append(errs, checkBound(attach.Bound)...)
	case "detach", "drop":
		errs = append(errs, checkPartitionName(name)...)
	}
	if len(errs) > 0 {
		return nil, problem.Validation("The partition request is invalid.", errs...)
	}
	var exists, partitioned bool
	err := o.asService(ctx, db, func(tx pgx.Tx) error {
		if ok, err := tableExists(ctx, tx, schema, table); err != nil {
			return err
		} else if !ok {
			return problem.New(problem.NotFound, fmt.Sprintf("Table %s.%s does not exist.", schema, table))
		}
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM kingres.dist_table WHERE relid = $1::regclass AND partition_method IS NOT NULL)`, p.rel()).Scan(&partitioned); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM kingres.dist_partition WHERE parent = $1::regclass AND partition_name = $2)`, p.rel(), p.PartName).Scan(&exists)
	})
	if err != nil {
		return nil, wrap(err)
	}
	if !partitioned {
		return nil, notEligible([]api.TableBlocker{{Code: "not_partitioned", Object: schema + "." + table, Message: "the table is not a user-partitioned distributed table"}})
	}
	switch op {
	case "create":
		if exists && !p.IfNotExists {
			return nil, problem.New(problem.AlreadyExists, fmt.Sprintf("Partition %s of %s.%s exists.", p.PartName, schema, table))
		}
	case "detach", "drop":
		if !exists {
			return nil, problem.New(problem.NotFound, fmt.Sprintf("Partition %s of %s.%s does not exist.", name, schema, table))
		}
	}
	if err := o.rebalanceRunning(ctx); err != nil {
		return nil, err
	}
	bl, _, _, err := o.backend().Report(ctx, p)
	if err != nil {
		return nil, wrap(err)
	}
	if len(bl) > 0 {
		return nil, notEligible(bl)
	}
	return p, nil
}

func checkPartitionName(n string) []problem.FieldError {
	if n == "" || len(n) > 57 {
		return []problem.FieldError{field("/name", "must be 1 to 57 bytes (leaf names append _bNNNN)", "length")}
	}
	if _, err := sqlx.NewIdent(n); err != nil {
		return []problem.FieldError{field("/name", "is not a valid name", "pattern")}
	}
	return nil
}

func checkBound(b string) []problem.FieldError {
	if strings.TrimSpace(b) == "" {
		return []problem.FieldError{field("/bound", "is required", "required")}
	}
	if strings.ContainsAny(b, ";") || strings.Contains(b, "--") || strings.Contains(b, "/*") {
		return []problem.FieldError{field("/bound", "must be the text that follows FOR VALUES, or DEFAULT", "pattern")}
	}
	return nil
}

// PreviewTableChange answers previewTableChange: the report of the extension and an estimate, without changing anything.
func (o *Ops) PreviewTableChange(ctx context.Context, db, schema, table string, req api.TableChangeRequest) (api.TableChangePreview, error) {
	if err := checkNames(db, schema, table); err != nil {
		return api.TableChangePreview{}, err
	}
	p := tableParams{V: 2, Database: db, Schema: schema, Table: table}
	mode := ""
	switch req.Kind {
	case "distribute":
		d := req.Distribute
		if d == nil {
			return api.TableChangePreview{}, problem.Validation("The request is invalid.", field("/distribute", "is required for kind distribute", "required"))
		}
		p.Op, p.Method, p.Column, p.DependentViews, mode = "distribute", d.Method, d.DistributionColumn, d.DependentViews, d.Mode
		if d.BucketCount != nil {
			p.BucketCount = *d.BucketCount
		}
		if d.ColocateWith != nil {
			p.ColocSchema, p.ColocTable = d.ColocateWith.Schema, d.ColocateWith.Table
		}
	case "undistribute":
		u := req.Undistribute
		if u == nil {
			u = &api.UndistributeChange{}
		}
		p.Op, p.Cascade, p.TargetGroup, p.DependentViews, mode = "undistribute", u.CascadeViaForeignKeys, u.TargetGroupID, u.DependentViews, u.Mode
	case "alter":
		a := req.Alter
		if a == nil {
			return api.TableChangePreview{}, problem.Validation("The request is invalid.", field("/alter", "is required for kind alter", "required"))
		}
		p.Op, p.AlterColumn, p.CascadeColoc, p.DependentViews, mode = "alter", a.DistributionColumn, a.CascadeToColocated, a.DependentViews, a.Mode
		if a.BucketCount != nil {
			p.AlterBuckets = *a.BucketCount
		}
		if a.ColocateWith != nil {
			if a.ColocateWith.Table != nil {
				p.AlterColoc = qualified(a.ColocateWith.Table.Schema, a.ColocateWith.Table.Table)
			} else {
				p.AlterColoc = a.ColocateWith.Keyword
			}
		}
	case "createPartition", "attachPartition", "detachPartition", "dropPartition":
		p.Op = "partition"
		p.PartOp = strings.ToLower(strings.TrimSuffix(req.Kind, "Partition"))
		if c := req.Partition; c != nil {
			p.PartName, p.PartBound = c.Name, c.Bound
			if c.Table != nil {
				p.PartSchema, p.PartTable = c.Table.Schema, c.Table.Table
				p.PartName = c.Table.Table
			}
		}
	default:
		return api.TableChangePreview{}, problem.Validation("The request is invalid.", field("/kind", "is not a kind of change", "enum"))
	}
	b := o.backend()
	rows, bytes, _, err := b.Sizes(ctx, p)
	if err != nil {
		return api.TableChangePreview{}, wrap(err)
	}
	// the online report says whether the online copy is possible; the one the change would run decides the blockers
	onl := p
	onl.Online = true
	onlBlockers, _, _, err := b.Report(ctx, onl)
	if err != nil {
		return api.TableChangePreview{}, wrap(err)
	}
	onlineOK := len(onlBlockers) == 0 && p.Op != "partition"
	run := p
	run.Online = onlineOK && (mode == "online" || (mode != "blocking" && rows > onlineRowsThreshold))
	blockers, warnings, notes, err := b.Report(ctx, run)
	if err != nil {
		return api.TableChangePreview{}, wrap(err)
	}
	out := api.TableChangePreview{Blockers: nz(blockers), Warnings: nz(warnings), Notes: nz(notes),
		AffectedTables: []api.TableChangeTable{{Schema: schema, Name: table, Role: "source", SizeBytes: &bytes, EstimatedRows: &rows}}}
	out.Estimate = api.TableChangeEstimate{Rows: &rows, Bytes: &bytes, OnlineAvailable: onlineOK, ModeRecommended: "blocking", WritersBlocked: "wholeCopy"}
	if onlineOK && rows > onlineRowsThreshold {
		out.Estimate.ModeRecommended = "online"
	}
	if run.Online {
		out.Estimate.WritersBlocked = "switchOnly"
	}
	if p.Op == "partition" {
		out.Estimate.WritersBlocked = "none"
		out.Estimate.Rows, out.Estimate.Bytes = nil, nil
	}
	if !onlineOK && p.Op != "partition" {
		r := describeBlockers(onlBlockers)
		out.Estimate.OnlineUnavailableReason = &r
	}
	return out, nil
}

func nz(b []api.TableBlocker) []api.TableBlocker {
	if b == nil {
		return []api.TableBlocker{}
	}
	return b
}

// ListTablePartitions lists the user partitions of a distributed table (the size is that of the leaves this server holds).
func (o *Ops) ListTablePartitions(ctx context.Context, db, schema, table string, limit int, cursor string) (api.TablePartitionPage, error) {
	page := api.TablePartitionPage{Items: []api.TablePartition{}}
	if err := checkNames(db, schema, table); err != nil {
		return page, err
	}
	if limit < 1 {
		limit = 50
	}
	err := o.asService(ctx, db, func(tx pgx.Tx) error {
		if ok, err := tableExists(ctx, tx, schema, table); err != nil {
			return err
		} else if !ok {
			return problem.New(problem.NotFound, fmt.Sprintf("Table %s.%s does not exist.", schema, table))
		}
		var have bool
		if err := tx.QueryRow(ctx, `SELECT to_regclass('kingres.partitions') IS NOT NULL`).Scan(&have); err != nil || !have {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT partition_name::text, method, bound, is_default, leaf_count, size_here FROM kingres.partitions
			WHERE table_name = $1::regclass AND partition_name::text > $2 ORDER BY partition_name LIMIT $3`, qualified(schema, table), cursor, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var x api.TablePartition
			if err := rows.Scan(&x.Name, &x.Method, &x.Bound, &x.IsDefault, &x.LeafCount, &x.SizeBytes); err != nil {
				return err
			}
			page.Items = append(page.Items, x)
		}
		return rows.Err()
	})
	if err != nil {
		return page, wrap(err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		c := page.Items[limit-1].Name
		page.NextCursor = &c
	}
	return page, nil
}

var _ = task.CodeStepFailed

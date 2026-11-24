// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

// The operation indicator of a table (contract 0.32.0, Table.operation): which long operation runs on a table now. It is read from two places.
// The tasks of the control plane that have a table as their subject (a distribute with related tables also names the related tables in its input)
// give the kind, the task and the progress. The sharding extension records the operations that rebuild a table (kingres.table_operation), also
// those that were started from SQL (create_distributed_table_concurrently and the others); a record of a table that already has a task is left
// out, because the task drives it. Every read of kingres.* is behind the extension guard: an absent or untrusted extension means no records.

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extguard"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// maxOperationTasks bounds how many recent tasks are looked at for the indicator.
const maxOperationTasks = 500

// tableSubject is the subject id of the tasks of a table.
func tableSubject(db, schema, table string) string { return db + "/" + schema + "." + table }

// operationKind maps a task kind with a table subject to the kind of Table.operation.
func operationKind(taskKind string) string {
	switch taskKind {
	case KindTableDistribute:
		return "distribute"
	case KindTableUndistribute:
		return "undistribute"
	case KindTableAlter:
		return "alter"
	case KindTablePartition:
		return "partition"
	case KindSetStorage:
		return "convertStorage"
	case KindMaintain:
		return "maintain"
	case KindTableExport:
		return "export"
	case KindTableImport:
		return "import"
	}
	return ""
}

// extensionOperationKind maps the kind of a record of the extension.
func extensionOperationKind(kind string) string {
	switch kind {
	case "distribute", "distribute_reference":
		return "distribute"
	case "undistribute", "alter":
		return kind
	}
	return "other"
}

// taskOperation builds the indicator of one active task for a table.
func taskOperation(t task.Task, role string) api.TableOperation {
	id := t.ID
	started := t.CreatedAt
	op := api.TableOperation{Kind: operationKind(t.Kind), Role: role, Source: "task", TaskID: &id, State: string(t.State)}
	if !started.IsZero() {
		op.StartedAt = &started
	}
	if t.State == task.Running || t.State == task.Paused {
		p := t.Percent
		op.Percent = &p
	}
	for _, s := range t.Steps {
		if s.State == task.StepRunning {
			name := s.Name
			op.Step = &name
			break
		}
	}
	return op
}

// TableOperations returns the operation that runs on each table of a database, by the subject text "database/schema.table". A table without an
// operation is not in the map. An error of the extension side is not an error of the caller: the indicator is then made from the tasks alone.
func (o *Ops) TableOperations(ctx context.Context, db string) map[string]api.TableOperation {
	out := map[string]api.TableOperation{}
	if eng := o.Engine(); eng != nil {
		if ts, err := eng.List(ctx, maxOperationTasks); err == nil {
			prefix := db + "/"
			// one operation is shown per table: a task that has the table as its subject wins over one that only takes it along
			for _, t := range ts {
				if !t.State.Active() || t.SubjectType != "table" || operationKind(t.Kind) == "" {
					continue
				}
				if strings.HasPrefix(t.SubjectID, prefix) {
					if cur, ok := out[t.SubjectID]; !ok || cur.Role == "related" {
						out[t.SubjectID] = taskOperation(t, "subject")
					}
				}
				if t.Kind == KindTableDistribute {
					if p, err := rawInput[tableParams](t.Input); err == nil && p.Database == db {
						for _, r := range p.Related {
							k := tableSubject(db, r.Schema, r.Table)
							if _, ok := out[k]; !ok {
								out[k] = taskOperation(t, "related")
							}
						}
					}
				}
			}
		}
	}
	for k, v := range o.extensionOperations(ctx, db) {
		if _, ok := out[k]; !ok {
			out[k] = v
		}
	}
	return out
}

// extensionOperations reads the unfinished operations of the sharding extension on this server's instance.
func (o *Ops) extensionOperations(ctx context.Context, db string) map[string]api.TableOperation {
	out := map[string]api.TableOperation{}
	if o.Service == nil {
		return out
	}
	_ = o.asService(ctx, db, func(tx pgx.Tx) error {
		if ok, err := extUsable(ctx, tx, extguard.Shard); err != nil || !ok {
			return nil
		}
		var has bool
		if err := tx.QueryRow(ctx, `SELECT to_regclass('kingres.table_operation_item') IS NOT NULL`).Scan(&has); err != nil || !has {
			return nil
		}
		rows, err := tx.Query(ctx, `SELECT o.kind, o.phase, o.started_at, n.nspname, c.relname, i.rows_copied, i.rows_total
			FROM kingres.table_operation o
			JOIN kingres.table_operation_item i ON i.op_id = o.op_id
			JOIN pg_catalog.pg_class c ON c.oid = i.relid::oid
			JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
			WHERE o.phase NOT IN ('done', 'failed', 'cancelled') AND NOT (o.spec ? 'mirror')
			ORDER BY o.op_id DESC`)
		if err != nil {
			return nil
		}
		defer rows.Close()
		for rows.Next() {
			var kind, phase, schema, table string
			var started time.Time
			var copied int64
			var total *int64
			if err := rows.Scan(&kind, &phase, &started, &schema, &table, &copied, &total); err != nil {
				return nil
			}
			k := tableSubject(db, schema, table)
			if _, ok := out[k]; ok {
				continue
			}
			op := api.TableOperation{Kind: extensionOperationKind(kind), Role: "subject", Source: "extension", State: phase, StartedAt: &started}
			if total != nil && *total > 0 && phase == "copying" {
				p := float64(copied) * 100 / float64(*total)
				if p > 100 {
					p = 100
				}
				op.Percent = &p
			}
			out[k] = op
		}
		return nil
	})
	return out
}

// AnnotateTables sets Table.operation of the tables of one database.
func (o *Ops) AnnotateTables(ctx context.Context, db string, items []api.Table) {
	ops := o.TableOperations(ctx, db)
	for i := range items {
		if op, ok := ops[tableSubject(db, items[i].Schema, items[i].Name)]; ok {
			op := op
			items[i].Operation = &op
		} else {
			items[i].Operation = nil
		}
	}
}

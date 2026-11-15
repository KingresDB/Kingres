// SPDX-License-Identifier: AGPL-3.0-only

package ops

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sqlx"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

type distributeParams struct {
	Database    string `json:"database"`
	Schema      string `json:"schema"`
	Table       string `json:"table"`
	Column      string `json:"column"`
	ColocSchema string `json:"colocateSchema,omitempty"`
	ColocTable  string `json:"colocateTable,omitempty"`
}

func tableExists(ctx context.Context, x execer, schema, table string) (bool, error) {
	var ok bool
	err := x.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2 AND c.relkind IN ('r', 'p'))`, schema, table).Scan(&ok)
	return ok, err
}

// PrepareDistribute validates a distribute request against the catalog of
// the database and returns the task input.
func (o *Ops) PrepareDistribute(ctx context.Context, db, schema, table string, req api.DistributeTableRequest) (any, error) {
	if req.Method == "reference" {
		return nil, problem.New(problem.UnsupportedOp, "Reference tables are not supported by the installed kingres_shard version.")
	}
	var errs []problem.FieldError
	if req.Method != "hash" {
		errs = append(errs, field("/method", "must be hash or reference", "enum"))
	}
	if req.DistributionColumn == "" {
		errs = append(errs, field("/distributionColumn", "is required for method hash", "required"))
	}
	p := distributeParams{Database: db, Schema: schema, Table: table, Column: req.DistributionColumn}
	if _, err := sqlx.QualifiedIdent(schema, table); err != nil {
		return nil, problem.New(problem.NotFound, "No such table.")
	}
	if _, err := sqlx.NewIdent(db); err != nil {
		return nil, problem.New(problem.NotFound, "No such database.")
	}
	if req.DistributionColumn != "" {
		if _, err := sqlx.NewIdent(req.DistributionColumn); err != nil {
			errs = append(errs, field("/distributionColumn", "is not a valid column name", "pattern"))
		}
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
	err := o.asService(ctx, db, func(tx pgx.Tx) error {
		if ok, err := tableExists(ctx, tx, schema, table); err != nil {
			return err
		} else if !ok {
			return problem.New(problem.NotFound, fmt.Sprintf("Table %s.%s does not exist.", schema, table))
		}
		var has bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_attribute a WHERE a.attrelid = to_regclass($1)
			AND a.attname = $2 AND a.attnum > 0 AND NOT a.attisdropped)`, qualified(schema, table), req.DistributionColumn).Scan(&has); err != nil {
			return err
		}
		if !has {
			return problem.Validation("The distribution request is invalid.", field("/distributionColumn", "the table has no such column", "not_found"))
		}
		return nil
	})
	if err != nil {
		return nil, wrap(err)
	}
	return p, nil
}

// qualified returns the quoted schema.table text for use as a bind value
// that PostgreSQL parses as a regclass.
func qualified(schema, table string) string {
	q, err := sqlx.QualifiedIdent(schema, table)
	if err != nil {
		return ""
	}
	return q.String()
}

func (o *Ops) stepDistribute(ctx context.Context, env *task.Env) error {
	p, err := inputOf[distributeParams](env.Task)
	if err != nil {
		return err
	}
	var buckets *int
	if err := o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT bucket_count FROM kingres_cp.database_setting WHERE database_name = $1", p.Database).Scan(&buckets)
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		buckets = nil // a database the control plane did not create: extension default
	}
	c, err := o.Service.ServiceConn(ctx, p.Database)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	var coloc *string
	if p.ColocTable != "" {
		q := qualified(p.ColocSchema, p.ColocTable)
		coloc = &q
	}
	_, err = c.Exec(ctx, "SELECT kingres.create_distributed_table($1::regclass, $2::name, $3::regclass, $4::int)",
		qualified(p.Schema, p.Table), p.Column, coloc, buckets)
	return err
}

// EnableLedger puts a table under ledger control (kingres.ledger_enable),
// run with the service role: the functions of the ledger are not executable
// by ordinary roles.
func (o *Ops) EnableLedger(ctx context.Context, db, schema, table string, req api.EnableTableLedgerRequest) error {
	mode := ""
	switch req.Mode {
	case "appendOnly":
		mode = "append_only"
	case "updatable":
		mode = "updatable"
	default:
		return problem.Validation("The request is invalid.", field("/mode", "must be appendOnly or updatable", "enum"))
	}
	if _, err := sqlx.NewIdent(db); err != nil {
		return problem.New(problem.NotFound, "No such database.")
	}
	if _, err := sqlx.QualifiedIdent(schema, table); err != nil {
		return problem.New(problem.NotFound, "No such table.")
	}
	return wrap(o.asService(ctx, db, func(tx pgx.Tx) error {
		if ok, err := tableExists(ctx, tx, schema, table); err != nil {
			return err
		} else if !ok {
			return problem.New(problem.NotFound, fmt.Sprintf("Table %s.%s does not exist.", schema, table))
		}
		var hasLedger bool
		var version string
		if err := tx.QueryRow(ctx, `SELECT coalesce((SELECT extversion FROM pg_catalog.pg_extension WHERE extname = 'kingres_ledger'), '')`).Scan(&version); err != nil {
			return err
		}
		hasLedger = version != ""
		if !hasLedger {
			return problem.New(problem.Conflict, "The ledger is not enabled on this database.")
		}
		if mode == "updatable" && !ledgerSupportsUpdatable(version) {
			return problem.New(problem.UnsupportedOp, "Updatable ledger tables need kingres_ledger 0.2; version "+version+" is installed.")
		}
		var already bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM kingres.ledger_table WHERE relid = to_regclass($1))", qualified(schema, table)).Scan(&already); err != nil {
			return err
		}
		if already {
			return problem.New(problem.Conflict, "The table is already under ledger control; this cannot be changed.")
		}
		_, err := tx.Exec(ctx, "SELECT kingres.ledger_enable($1::regclass, $2)", qualified(schema, table), mode)
		return err
	}))
}

// ledgerSupportsUpdatable reports whether an extension version has updatable
// tables (0.2 and later).
func ledgerSupportsUpdatable(version string) bool {
	return version != "" && version != "0.1"
}

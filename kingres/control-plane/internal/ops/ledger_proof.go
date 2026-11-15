// SPDX-License-Identifier: AGPL-3.0-only
// Portions Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sqlx"
)

// Inclusion proofs (createInclusionProof).
//
// The server reads the row as the signed-in PostgreSQL role, so table
// privileges and row security decide whether the caller may prove it, and
// computes its row hash with kingres.ledger_row_hash (executable by PUBLIC).
// The proof itself is made on a service connection, because
// kingres.ledger_inclusion_proof is executable only by granted roles and
// would let any reader learn the row hashes of the whole table; the cluster
// role (kingres_readonly) and the row read are what authorize it.

// proofRequest is a validated request.
type proofRequest struct {
	schema, table sqlx.Ident
	pk            map[string]json.RawMessage
	txid          *int64
	op            string
}

// validateProofRequest checks the body against the contract.
func validateProofRequest(req api.InclusionProofRequest) (proofRequest, error) {
	var errs []problem.FieldError
	pr := proofRequest{pk: req.PrimaryKey, txid: req.LedgerTxID, op: req.Op}
	var err error
	if pr.schema, err = sqlx.NewIdent(req.Schema); err != nil {
		errs = append(errs, field("/schema", "must be 1 to 63 bytes without control characters", "out_of_range"))
	}
	if pr.table, err = sqlx.NewIdent(req.Table); err != nil {
		errs = append(errs, field("/table", "must be 1 to 63 bytes without control characters", "out_of_range"))
	}
	switch pr.op {
	case "":
		pr.op = "I"
	case "I", "D":
	default:
		errs = append(errs, field("/op", "must be I or D", "enum"))
	}
	if req.LedgerTxID != nil && *req.LedgerTxID < 0 {
		errs = append(errs, field("/ledgerTxId", "must not be negative", "out_of_range"))
	}
	if pr.op == "D" && req.LedgerTxID == nil {
		errs = append(errs, field("/ledgerTxId", "is required when op is D", "required"))
	}
	if len(req.PrimaryKey) == 0 {
		errs = append(errs, field("/primaryKey", "must name every column of the primary key", "required"))
	}
	cols := make([]string, 0, len(req.PrimaryKey))
	for c := range req.PrimaryKey {
		cols = append(cols, c)
	}
	sort.Strings(cols)
	for _, c := range cols {
		if _, err := sqlx.NewIdent(c); err != nil {
			errs = append(errs, field("/primaryKey", "a column name is not valid", "invalid"))
			continue
		}
		if !scalarJSON(req.PrimaryKey[c]) {
			errs = append(errs, field("/primaryKey/"+c, "must be a string, a number or a boolean", "type"))
		}
	}
	if len(errs) > 0 {
		return pr, problem.Validation("The inclusion proof request is invalid.", errs...)
	}
	return pr, nil
}

// scalarJSON reports whether raw is a JSON string, number or boolean.
func scalarJSON(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return false
	}
	switch c := raw[0]; {
	case c == '"', c == '-', c >= '0' && c <= '9':
		return true
	}
	return bytes.Equal(raw, []byte("true")) || bytes.Equal(raw, []byte("false"))
}

// rowHashSQL builds the statement that reads the row (or its removed
// version) and returns its row hash. The row is matched by the primary key
// that jsonb_populate_record converts to the column types, so PostgreSQL does
// every conversion and no value is ever part of the text.
func rowHashSQL(rel sqlx.Ident, pkCols []string, txid *int64, history bool, doc []byte) (sqlx.Stmt, error) {
	b := stmt()
	fn := "kingres.ledger_row_hash"
	if history {
		fn = "kingres.ledger_history_row_hash"
	}
	b.Kw("SELECT").Text(sqlx.Fixed(fn + "(t.*)")).Kw("FROM").ID(rel).Text("t,")
	b.Text("jsonb_populate_record(CAST(NULL AS").ID(rel).Text("), CAST(").Arg(doc).Text("AS jsonb)) k")
	b.Kw("WHERE")
	for i, c := range pkCols {
		id, err := sqlx.NewIdent(c)
		if err != nil {
			return sqlx.Stmt{}, err
		}
		if i > 0 {
			b.Kw("AND")
		}
		b.Text("t.").ID(id).Text("= k.").ID(id)
	}
	if history {
		b.Kw("AND").Text("t.ledger_txid =").Arg(*txid).Kw("ORDER").Kw("BY").Text("t.ledger_seq").Kw("LIMIT").Text("1")
	}
	return b.Build()
}

// proofTable is what the catalog says about the table.
type proofTable struct {
	relid     int64
	tableID   int32
	mode      string
	histSch   string
	histName  string
	pkCols    []string
	distCol   string
	hasDist   bool
	extVerion string
}

func (o *Ops) lookupProofTable(ctx context.Context, tx pgx.Tx, pr proofRequest) (proofTable, error) {
	var t proofTable
	err := tx.QueryRow(ctx, `SELECT coalesce((SELECT extversion FROM pg_catalog.pg_extension WHERE extname = 'kingres_ledger'), '')`).Scan(&t.extVerion)
	if err != nil {
		return t, err
	}
	if t.extVerion == "" {
		return t, problem.New(problem.Conflict, "The ledger is not enabled on this database.")
	}
	var kind string
	err = tx.QueryRow(ctx, `SELECT c.oid::bigint, c.relkind::text FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2 AND c.relkind IN ('r', 'p')`, pr.schema.Name(), pr.table.Name()).Scan(&t.relid, &kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, problem.New(problem.NotFound, "The table does not exist.")
	}
	if err != nil {
		return t, err
	}
	// kingres.ledger_table is readable by everybody.
	var hist *int64
	q := `SELECT table_id, mode, NULL::bigint FROM kingres.ledger_table WHERE relid = $1::bigint::oid::regclass`
	if ledgerSupportsUpdatable(t.extVerion) {
		q = `SELECT table_id, mode, history_relid::oid::bigint FROM kingres.ledger_table WHERE relid = $1::bigint::oid::regclass`
	}
	err = tx.QueryRow(ctx, q, t.relid).Scan(&t.tableID, &t.mode, &hist)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, problem.New(problem.Unprocessable, "The table is not a ledger table.")
	}
	if err != nil {
		return t, err
	}
	if hist != nil {
		err = tx.QueryRow(ctx, `SELECT n.nspname, c.relname FROM pg_catalog.pg_class c
			JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace WHERE c.oid = $1::bigint::oid`, *hist).Scan(&t.histSch, &t.histName)
		if err != nil {
			return t, err
		}
	}
	rows, err := tx.Query(ctx, `SELECT a.attname::text FROM pg_catalog.pg_index i
		JOIN pg_catalog.pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY (i.indkey)
		WHERE i.indrelid = $1::bigint::oid AND i.indisprimary ORDER BY a.attnum`, t.relid)
	if err != nil {
		return t, err
	}
	if t.pkCols, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return t, err
	}
	var dist bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('kingres.dist_table') IS NOT NULL`).Scan(&dist); err != nil {
		return t, err
	}
	if dist {
		var col *string
		err := tx.QueryRow(ctx, `SELECT dist_column::text FROM kingres.dist_table WHERE relid = $1::bigint::oid::regclass AND method = 'hash'`, t.relid).Scan(&col)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return t, err
		}
		if col != nil {
			t.hasDist, t.distCol = true, *col
		}
	}
	return t, nil
}

// checkProofTarget applies the rules that depend on the table.
func checkProofTarget(t proofTable, pr proofRequest) error {
	if len(t.pkCols) == 0 {
		return problem.New(problem.Unprocessable, "The table has no primary key, so a row of it cannot be addressed.")
	}
	if t.mode == "append_only" && (pr.txid != nil || pr.op == "D") {
		if !ledgerSupportsUpdatable(t.extVerion) {
			return problem.New(problem.UnsupportedOp, "Removed versions need kingres_ledger 0.2; version "+t.extVerion+" is installed.")
		}
		return problem.New(problem.Unprocessable, "An append-only table has no removed versions: ledgerTxId and op D are for updatable tables.")
	}
	var errs []problem.FieldError
	want := map[string]bool{}
	for _, c := range t.pkCols {
		want[c] = true
		if _, ok := pr.pk[c]; !ok {
			errs = append(errs, field("/primaryKey/"+c, "this primary key column is missing", "required"))
		}
	}
	extra := make([]string, 0)
	for c := range pr.pk {
		if !want[c] {
			extra = append(extra, c)
		}
	}
	sort.Strings(extra)
	for _, c := range extra {
		errs = append(errs, field("/primaryKey/"+c, "is not a column of the primary key", "unknown"))
	}
	if t.hasDist && !want[t.distCol] {
		errs = append(errs, field("/primaryKey", "the distribution column "+t.distCol+" must be part of the primary key to find the shard group of the row", "invalid"))
	}
	if len(errs) > 0 {
		return problem.Validation("The primary key does not match the table.", errs...)
	}
	return nil
}

// CreateInclusionProof proves one version of a row of a ledger table.
func (o *Ops) CreateInclusionProof(ctx context.Context, actor, db string, req api.InclusionProofRequest) (api.InclusionProof, error) {
	pr, err := validateProofRequest(req)
	if err != nil {
		return nil, err
	}
	if _, err := ledgerDB(db); err != nil {
		return nil, err
	}
	doc, err := json.Marshal(pr.pk)
	if err != nil {
		return nil, problem.New(problem.InternalError, "An unexpected error occurred.")
	}
	var (
		tbl  proofTable
		hash []byte
	)
	err = o.asRole(ctx, db, actor, func(tx pgx.Tx) error {
		var err error
		if tbl, err = o.lookupProofTable(ctx, tx, pr); err != nil {
			return err
		}
		if err := checkProofTarget(tbl, pr); err != nil {
			return err
		}
		hist := false
		target, err := sqlx.QualifiedIdent(pr.schema.Name(), pr.table.Name())
		if err != nil {
			return err
		}
		if pr.txid != nil {
			if target, err = sqlx.QualifiedIdent(tbl.histSch, tbl.histName); err != nil {
				return err
			}
			hist = true
		}
		st, err := rowHashSQL(target, tbl.pkCols, pr.txid, hist, doc)
		if err != nil {
			return errBuild
		}
		err = tx.QueryRow(ctx, st.SQL, st.Args...).Scan(&hash)
		if errors.Is(err, pgx.ErrNoRows) {
			if hist {
				return problem.New(problem.NotFound, "The table has no removed version of this row with that ledgerTxId.")
			}
			return problem.New(problem.NotFound, "The table has no row with this primary key.")
		}
		return err
	})
	if err != nil {
		return nil, proofProblem(err)
	}
	var out api.InclusionProof
	err = o.asService(ctx, db, func(tx pgx.Tx) error {
		var raw []byte
		var err error
		if ledgerSupportsUpdatable(tbl.extVerion) {
			err = tx.QueryRow(ctx, `SELECT kingres.ledger_inclusion_proof($1::bigint::oid::regclass, $2::bytea, $3::text::"char")`, tbl.relid, hash, pr.op).Scan(&raw)
		} else {
			err = tx.QueryRow(ctx, `SELECT kingres.ledger_inclusion_proof($1::bigint::oid::regclass, $2::bytea)`, tbl.relid, hash).Scan(&raw)
		}
		if err != nil {
			return err
		}
		out = map[string]json.RawMessage{}
		if err := json.Unmarshal(raw, &out); err != nil {
			return fmt.Errorf("reading the proof of the extension: %w", err)
		}
		out["shard_group_id"] = json.RawMessage(strconv.Itoa(int(localGroup(ctx, tx))))
		return nil
	})
	if err != nil {
		var pe *pgconn.PgError
		switch {
		case errors.As(err, &pe) && pe.Code == "55000":
			return nil, problem.New(problem.LedgerEntryPending, "The row is not in a block yet: generate a digest (generateLedgerDigest closes the open block) and ask again.")
		case errors.As(err, &pe) && pe.Code == "02000":
			if tbl.hasDist {
				return nil, problem.New(problem.UnsupportedOp, "The entry is not on the chain of this server's shard group; proofs of rows on other shard groups are not supported yet.")
			}
			return nil, problem.New(problem.NotFound, "The ledger has no entry for this row version.")
		}
		return nil, wrap(err)
	}
	return out, nil
}

// proofProblem maps what the signed-in role's statements can raise: a value
// that does not fit its column is the caller's mistake (400), everything
// else goes through PGProblem.
func proofProblem(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && len(pe.Code) >= 2 && pe.Code[:2] == "22" {
		return problem.Validation("The primary key does not match the column types.", field("/primaryKey", "a value cannot be converted to the type of its column: "+pe.Message, "invalid"))
	}
	return wrap(err)
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ledgerround

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extguard"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sqlx"
)

// Reader answers CallRead on the server that runs the primary of a shard group: a closed set of four operations, each one fixed function of
// the extension run as the service role on that primary. It takes no SQL text; the database name is checked as an identifier.
type Reader struct {
	// Conn opens a service connection to database db on the primary instance of group, which must run on this server.
	Conn func(ctx context.Context, group int32, db string) (*pgx.Conn, error)
	// HasPrimary tells whether this server runs the primary of group.
	HasPrimary func(group int32) bool
	// Timeout bounds one call (default 20 seconds).
	Timeout time.Duration
}

func callErr(status int, code, msg string) error {
	return &internalapi.CallError{Status: status, Code: code, Message: msg}
}

// Codes of the errors of CallRead.
const (
	CodeNotPrimary   = "not_primary"
	CodeNoDatabase   = "no_database"
	CodeNoLedger     = "no_ledger"
	CodeLedgerOld    = "ledger_too_old"
	CodeLedgerBroken = "ledger_damaged"
	CodeBadRequest   = "bad_request"
	CodeUnavailable  = "unavailable"
)

// Handle runs one CallRead; the body is raw JSON.
func (r *Reader) Handle(ctx context.Context, body []byte) (any, error) {
	var req ReadRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, callErr(http.StatusBadRequest, CodeBadRequest, "The body is not valid.")
	}
	switch req.Op {
	case OpHead, OpDigest, OpStatus:
	case OpEntries:
		if req.From < 0 || req.Max < 0 || req.Max > MaxEntries {
			return nil, callErr(http.StatusBadRequest, CodeBadRequest, "from and max are out of range.")
		}
	default:
		return nil, callErr(http.StatusBadRequest, CodeBadRequest, "No such operation.")
	}
	if _, err := sqlx.NewIdent(req.Database); err != nil || req.Group < 0 {
		return nil, callErr(http.StatusBadRequest, CodeBadRequest, "The database name is not valid.")
	}
	if r.HasPrimary != nil && !r.HasPrimary(req.Group) {
		return nil, callErr(http.StatusConflict, CodeNotPrimary, "This server does not run the primary of that shard group.")
	}
	to := r.Timeout
	if to <= 0 {
		to = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	c, err := r.Conn(ctx, req.Group, req.Database)
	if err != nil {
		return nil, mapPG(err)
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	out, err := run(ctx, c, req)
	if err != nil {
		return nil, mapPG(err)
	}
	return out, nil
}

// mapPG maps an error of the connection or of a function to the error the call answers. The text of a PostgreSQL error is the one the
// extension wrote for the publisher (a block number, a transaction id), never row data.
func mapPG(err error) error {
	var ce *internalapi.CallError
	if errors.As(err, &ce) {
		return ce
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch {
		case pe.Code == "3D000":
			return callErr(http.StatusNotFound, CodeNoDatabase, "The database does not exist on this group.")
		case pe.Code == "XX001":
			return callErr(http.StatusUnprocessableEntity, CodeLedgerBroken, pe.Message)
		case len(pe.Code) >= 2 && (pe.Code[:2] == "28" || pe.Code[:2] == "08" || pe.Code[:2] == "53" || pe.Code[:2] == "57"):
			return callErr(http.StatusServiceUnavailable, CodeUnavailable, "The instance cannot be reached now.")
		}
		return callErr(http.StatusUnprocessableEntity, CodeLedgerBroken, "PostgreSQL refused the read: "+pe.Message)
	}
	return callErr(http.StatusServiceUnavailable, CodeUnavailable, "The instance cannot be reached now.")
}

func run(ctx context.Context, c *pgx.Conn, req ReadRequest) (ReadResponse, error) {
	if err := requireLedger(ctx, c); err != nil {
		return ReadResponse{}, err
	}
	var hasBlocks, hasRecords, hasHead, hasEntries bool
	if err := c.QueryRow(ctx, `SELECT pg_catalog.to_regclass('kingres.ledger_blocks') IS NOT NULL,
		pg_catalog.to_regclass('kingres.ledger_digest') IS NOT NULL AND pg_catalog.to_regclass('kingres.ledger_verification') IS NOT NULL,
		pg_catalog.to_regprocedure('kingres.ledger_head()') IS NOT NULL,
		pg_catalog.to_regprocedure('kingres.ledger_blocks_entries(bigint,integer)') IS NOT NULL`).Scan(&hasBlocks, &hasRecords, &hasHead, &hasEntries); err != nil {
		return ReadResponse{}, err
	}
	if !hasBlocks {
		return ReadResponse{}, callErr(http.StatusConflict, CodeNoLedger, "The ledger is not enabled on this database.")
	}
	if !hasHead || !hasEntries || !hasRecords {
		return ReadResponse{}, callErr(http.StatusUnprocessableEntity, CodeLedgerOld, "This database has an older kingres_ledger (ALTER EXTENSION kingres_ledger UPDATE).")
	}
	switch req.Op {
	case OpHead:
		return head(ctx, c)
	case OpEntries:
		rows, err := c.Query(ctx, `SELECT height, entry FROM kingres.ledger_blocks_entries($1, $2)`, req.From, req.Max)
		if err != nil {
			return ReadResponse{}, err
		}
		defer rows.Close()
		var out ReadResponse
		for rows.Next() {
			var e EntryRow
			if err := rows.Scan(&e.Height, &e.Entry); err != nil {
				return ReadResponse{}, err
			}
			out.Entries = append(out.Entries, e)
		}
		return out, rows.Err()
	case OpStatus:
		var doc []byte
		if err := c.QueryRow(ctx, `SELECT coalesce((SELECT to_jsonb(s) FROM kingres.ledger_status s LIMIT 1), 'null'::jsonb)`).Scan(&doc); err != nil {
			return ReadResponse{}, err
		}
		return ReadResponse{Status: doc}, nil
	}
	return digest(ctx, c)
}

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func head(ctx context.Context, x queryRower) (ReadResponse, error) {
	var out ReadResponse
	err := x.QueryRow(ctx, `SELECT encode(chain_id, 'hex'), height, encode(block_hash, 'hex'), closed_at_us FROM kingres.ledger_head()`).
		Scan(&out.ChainID, &out.Height, &out.BlockHash, &out.ClosedAtUS)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReadResponse{Empty: true}, nil
	}
	return out, err
}

// digest records a digest (which closes the open block) and returns it with the head it names. The extension attributes the digest to the
// service role; the caller of the REST operation is remembered by the leader in the digest view.
func digest(ctx context.Context, c *pgx.Conn) (ReadResponse, error) {
	var have, want string
	if err := c.QueryRow(ctx, `SELECT coalesce((SELECT extversion FROM pg_catalog.pg_extension WHERE extname = 'kingres_ledger'), ''),
		coalesce((SELECT default_version FROM pg_catalog.pg_available_extensions WHERE name = 'kingres_ledger'), '')`).Scan(&have, &want); err != nil {
		return ReadResponse{}, err
	}
	if have != "" && want != "" && have != want {
		return ReadResponse{}, callErr(http.StatusUnprocessableEntity, CodeLedgerOld,
			"This database has kingres_ledger "+have+" and the installed library is "+want+"; run ALTER EXTENSION kingres_ledger UPDATE.")
	}
	tx, err := c.Begin(ctx)
	if err != nil {
		return ReadResponse{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var doc []byte
	if err := tx.QueryRow(ctx, `SELECT kingres.ledger_generate_digest()`).Scan(&doc); err != nil {
		return ReadResponse{}, err
	}
	if doc == nil {
		return ReadResponse{Empty: true}, nil
	}
	var d DigestRecord
	if err := tx.QueryRow(ctx, `SELECT digest_id, generated_at, generated_by::text, height, encode(block_hash, 'hex'), coalesce((digest->>'hash_version')::int, 1), digest
		FROM kingres.ledger_digest ORDER BY digest_id DESC LIMIT 1`).Scan(&d.ID, &d.At, &d.By, &d.Height, &d.BlockHash, &d.HashVersion, &d.Document); err != nil {
		return ReadResponse{}, err
	}
	d.At = d.At.UTC()
	out, err := head(ctx, tx)
	if err != nil {
		return ReadResponse{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ReadResponse{}, err
	}
	out.Digest = &d
	return out, nil
}

// requireLedger checks the extension before anything of the schema kingres is read or called (extguard): the session is the service role's, a superuser, and a
// schema that a database owner made would run its functions as that role. An extension that is not installed is the answer "no ledger"; one whose schema cannot be
// trusted is refused, and nothing in it is touched.
func requireLedger(ctx context.Context, c *pgx.Conn) error {
	info, err := extguard.Verify(ctx, c, extguard.Ledger)
	if u, bad := extguard.IsUntrusted(err); bad {
		return callErr(http.StatusUnprocessableEntity, CodeLedgerBroken,
			"The schema "+u.Schema+" of kingres_ledger cannot be trusted ("+string(u.Reason)+"); the ledger of this database is not read.")
	}
	if err != nil {
		return err
	}
	if !info.Present {
		return callErr(http.StatusConflict, CodeNoLedger, "The ledger is not enabled on this database.")
	}
	return nil
}

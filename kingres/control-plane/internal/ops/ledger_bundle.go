// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// Bundles for offline verification (phase 7, step 20; design 7.3): the task ledger.bundle.create builds a .tar.gz of the publication of one
// database under the data directory of the server that runs it (the manager leader) and keeps it for 24 hours. The number of the bundle is
// written in the detail of the task as "bundle N", which is what the console reads; the contract has no member for the result of a task.

// KindLedgerBundle is the task that builds a bundle.
const KindLedgerBundle = "ledger.bundle.create"

// BundleKeep is how long a bundle is kept.
const BundleKeep = 24 * time.Hour

// ErrBundleGone is returned for a bundle whose time has passed.
var ErrBundleGone = errors.New("ops: the bundle has expired")

type bundleParams struct {
	Database string                  `json:"database"`
	BundleID int64                   `json:"bundleId"`
	Request  api.LedgerBundleRequest `json:"request"`
}

// bundlePath is the file of a bundle; the id is a number, so the name holds nothing from the caller.
func (o *Ops) bundlePath(id int64) string {
	return filepath.Join(o.BundleDir, "bundle-"+strconv.FormatInt(id, 10)+".tar.gz")
}

// PrepareBundle validates a bundle request, checks that the database is published and gives the bundle its number.
func (o *Ops) PrepareBundle(ctx context.Context, db, actor string, req api.LedgerBundleRequest) (any, error) {
	if _, err := ledgerDB(db); err != nil {
		return nil, err
	}
	var errs []problem.FieldError
	if req.IncludeAnchors == nil {
		errs = append(errs, field("/includeAnchors", "is required", "required"))
	}
	if req.SinceSize != nil && *req.SinceSize < 0 {
		errs = append(errs, field("/sinceSize", "must not be negative", "range"))
	}
	if len(req.Logs) > 100 {
		errs = append(errs, field("/logs", "at most 100 origins", "length"))
	}
	for _, l := range req.Logs {
		if l == "" || len(l) > 300 {
			errs = append(errs, field("/logs", "an origin has 1 to 300 characters", "length"))
			break
		}
	}
	if len(req.Proofs) > 20 {
		errs = append(errs, field("/proofs", "at most 20 proofs", "length"))
	}
	if len(errs) > 0 {
		return nil, problem.Validation("The request is invalid.", errs...)
	}
	if o.LedgerBundleBuild == nil || o.BundleDir == "" {
		return nil, problem.New(problem.UnsupportedOp, "This server does not publish the ledger.")
	}
	if o.LedgerPublished != nil {
		ok, err := o.LedgerPublished(ctx, db)
		if err != nil {
			return nil, wrap(err)
		}
		if !ok {
			return nil, problem.New(problem.LedgerNotPublished, "The database has no published ledger yet.")
		}
	}
	var id int64
	err := o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM kingres_cp.ledger_bundle WHERE expires_at < now() - interval '1 hour' AND server_id = $1`, o.ServerHost); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO kingres_cp.ledger_bundle (database_name, server_id, created_by) VALUES ($1, $2, $3) RETURNING id`, db, o.ServerHost, actor).Scan(&id)
	})
	if err != nil {
		return nil, wrap(err)
	}
	return bundleParams{Database: db, BundleID: id, Request: req}, nil
}

func (o *Ops) ledgerBundleDefinition() task.Definition {
	return task.Definition{Kind: KindLedgerBundle, Steps: []task.Step{{Name: "Build the bundle", Run: o.stepLedgerBundle}}}
}

func (o *Ops) stepLedgerBundle(ctx context.Context, env *task.Env) error {
	p, err := inputOf[bundleParams](env.Task)
	if err != nil {
		return err
	}
	if err := env.SetDetail(ctx, fmt.Sprintf("bundle %d", p.BundleID)); err != nil {
		return err
	}
	if err := os.MkdirAll(o.BundleDir, 0o700); err != nil {
		return o.failBundle(ctx, p.BundleID, err)
	}
	// the proofs the caller asked for are made now, with the service role, and go into the bundle as files
	files := map[string][]byte{}
	for i, pr := range p.Request.Proofs {
		proof, err := o.CreateInclusionProof(ctx, "system", p.Database, pr)
		if err != nil {
			return o.failBundle(ctx, p.BundleID, fmt.Errorf("proof %d: %w", i+1, err))
		}
		raw, err := json.Marshal(proof)
		if err != nil {
			return o.failBundle(ctx, p.BundleID, err)
		}
		files["proofs/proof-"+strconv.Itoa(i+1)+".json"] = append(raw, '\n')
	}
	tmp := o.bundlePath(p.BundleID) + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return o.failBundle(ctx, p.BundleID, err)
	}
	err = o.LedgerBundleBuild(ctx, p.Database, p.Request, files, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return o.failBundle(ctx, p.BundleID, err)
	}
	if err := os.Rename(tmp, o.bundlePath(p.BundleID)); err != nil {
		return o.failBundle(ctx, p.BundleID, err)
	}
	st, err := os.Stat(o.bundlePath(p.BundleID))
	if err != nil {
		return o.failBundle(ctx, p.BundleID, err)
	}
	return o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE kingres_cp.ledger_bundle SET state = 'ready', size_bytes = $2, expires_at = now() + interval '24 hours' WHERE id = $1`, p.BundleID, st.Size())
		return err
	})
}

func (o *Ops) failBundle(ctx context.Context, id int64, cause error) error {
	_ = o.asService(context.WithoutCancel(ctx), sysdb.Database, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE kingres_cp.ledger_bundle SET state = 'failed' WHERE id = $1`, id)
		return err
	})
	return cause
}

// OpenBundle opens the file of a bundle of the database. A bundle that does not exist (or is another database's) is NotFound; one whose
// 24 hours have passed is ErrBundleGone and its file is removed.
func (o *Ops) OpenBundle(ctx context.Context, db string, id int64) (io.ReadCloser, int64, error) {
	if _, err := ledgerDB(db); err != nil {
		return nil, 0, err
	}
	var state string
	var expired bool
	err := o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT state, expires_at < now() FROM kingres_cp.ledger_bundle WHERE id = $1 AND database_name = $2 AND server_id = $3`,
			id, db, o.ServerHost).Scan(&state, &expired)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, problem.New(problem.NotFound, "The bundle does not exist.")
	}
	if err != nil {
		return nil, 0, wrap(err)
	}
	if expired {
		_ = os.Remove(o.bundlePath(id))
		return nil, 0, ErrBundleGone
	}
	if state != "ready" {
		return nil, 0, problem.New(problem.NotFound, "The bundle is not ready.")
	}
	f, err := os.Open(o.bundlePath(id))
	if err != nil {
		return nil, 0, problem.New(problem.NotFound, "The bundle does not exist.")
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, wrap(err)
	}
	return f, st.Size(), nil
}

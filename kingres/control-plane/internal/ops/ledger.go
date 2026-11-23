// SPDX-License-Identifier: AGPL-3.0-only

package ops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extguard"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sqlx"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// Which role kingresd uses for the ledger: the service role (a superuser) for
// every ledger function and for reading the records. The ledger functions
// and the digest and verification tables are not executable or readable by
// PUBLIC (kingres_ledger README, privilege model), and granting them to the
// administrators of a database would let the panel's roles write the chain.
// So nothing is granted at database creation; the cluster-role check
// (kingres_ops for digests and verifications) decides who may ask.

func ledgerDB(db string) (string, error) {
	if _, err := sqlx.NewIdent(db); err != nil {
		return "", problem.New(problem.NotFound, "No such database.")
	}
	return db, nil
}

// ledgerRecords reports whether the database has the 0.2 records.
func ledgerRecords(ctx context.Context, x execer) (installed, records bool, err error) {
	// The extension first (extguard): nothing of the schema kingres is read before it is known to be the extension's own.
	info, err := extPresent(ctx, x, extguard.Ledger)
	if err != nil || !info.Present {
		return false, false, err
	}
	err = x.QueryRow(ctx, `SELECT pg_catalog.to_regclass('kingres.ledger_blocks') IS NOT NULL,
		pg_catalog.to_regclass('kingres.ledger_digest') IS NOT NULL AND pg_catalog.to_regclass('kingres.ledger_verification') IS NOT NULL`).Scan(&installed, &records)
	return
}

func needRecords(ctx context.Context, x execer) error {
	inst, rec, err := ledgerRecords(ctx, x)
	if err != nil {
		return err
	}
	if !inst {
		return problem.New(problem.Conflict, "The ledger is not enabled on this database.")
	}
	if !rec {
		return problem.New(problem.UnsupportedOp, "Digest and verification records need kingres_ledger 0.2; this database has an older version (ALTER EXTENSION kingres_ledger UPDATE).")
	}
	return nil
}

// needCurrentLedger refuses a call that writes the ledger when the SQL of the
// extension in this database is not the version of the installed library.
// The library writes the ledger tables only through the functions of its own
// version (kingres_ledger README, "Upgrades"): with older SQL PostgreSQL
// answers "insufficient privilege", which would reach the caller as
// "forbidden" and say nothing of what to do. The capture triggers keep
// working in such a database; what waits for the update is everything that
// this file and EnableLedger call.
func needCurrentLedger(ctx context.Context, x execer) error {
	have, want, err := ledgerVersions(ctx, x)
	if err != nil {
		return err
	}
	if have == "" || want == "" || have == want {
		return nil
	}
	return problem.New(problem.UnsupportedOp, "This database has kingres_ledger "+have+" and the installed library is "+want+
		"; the ledger is written again after ALTER EXTENSION kingres_ledger UPDATE in this database.")
}

type digestRow struct {
	id          int64
	at          time.Time
	by          string
	height      int64
	hash        string
	hashVersion int
	doc         []byte
}

const digestCols = `digest_id, generated_at, generated_by::text, height, encode(block_hash, 'hex'),
	coalesce((digest->>'hash_version')::int, 1), digest`

// toDigest maps a digest row. A digest is made by the server that runs the
// request and holds one chain, that of its own shard group; uncovered lists
// the other shard groups of the database, whose chains it does not contain
// (CP2-08). Without all chains there is no aggregate hash either.
func toDigest(db string, r digestRow, group int32, uncovered []int32, actor string) api.LedgerDigest {
	by := actor
	switch {
	case by != "":
	case r.by == instance.ServiceRole:
		by = "system" // made by kingresd without a caller: the scheduled digest
	default:
		by = r.by
	}
	if uncovered == nil {
		uncovered = []int32{}
	}
	return api.LedgerDigest{ID: r.id, DatabaseName: db, GeneratedAt: r.at.UTC(), GeneratedBy: &by, HashVersion: r.hashVersion,
		Chains: []api.LedgerDigestChain{{ShardGroupID: group, BlockHeight: r.height, BlockHash: r.hash, Document: r.doc}}, Anchors: []api.DigestAnchor{},
		Complete: len(uncovered) == 0, UncoveredShardGroupIDs: uncovered}
}

// GenerateDigest builds a block if needed, records and returns the digest.
// actor is the role that asked for it: the extension attributes the digest to the
// role that ran the function, the service role, so the control plane remembers
// the caller itself (kingres_cp.ledger_digest_actor).
func (o *Ops) GenerateDigest(ctx context.Context, db, actor string) (api.LedgerDigest, error) {
	if _, err := ledgerDB(db); err != nil {
		return api.LedgerDigest{}, err
	}
	if o.LedgerDigest != nil {
		return o.LedgerDigest(ctx, db, actor)
	}
	var out api.LedgerDigest
	var chain string
	err := o.asService(ctx, db, func(tx pgx.Tx) error {
		if err := needRecords(ctx, tx); err != nil {
			return err
		}
		if err := needCurrentLedger(ctx, tx); err != nil {
			return err
		}
		var dig []byte
		if err := tx.QueryRow(ctx, "SELECT kingres.ledger_generate_digest()").Scan(&dig); err != nil {
			return err
		}
		if dig == nil {
			return problem.New(problem.Conflict, "The ledger is empty: there is nothing to build a digest of.")
		}
		var r digestRow
		if err := tx.QueryRow(ctx, "SELECT "+digestCols+" FROM kingres.ledger_digest ORDER BY digest_id DESC LIMIT 1").
			Scan(&r.id, &r.at, &r.by, &r.height, &r.hash, &r.hashVersion, &r.doc); err != nil {
			return err
		}
		ch, err := chainOf(ctx, tx)
		if err != nil {
			return err
		}
		chain = ch.id
		out = toDigest(db, r, ch.group, otherGroups(ctx, tx, ch.group), actor)
		return nil
	})
	if err != nil {
		return out, wrap(err)
	}
	if err := o.rememberDigestActor(ctx, db, chain, out.ID, actor); err != nil {
		return api.LedgerDigest{}, wrap(fmt.Errorf("recording who asked for digest %d: %w", out.ID, err))
	}
	return out, nil
}

// rememberDigestActor stores the caller of a digest.
func (o *Ops) rememberDigestActor(ctx context.Context, db, chain string, id int64, actor string) error {
	return o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO kingres_cp.ledger_digest_actor (database_name, chain_id, digest_id, actor)
			VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`, db, chain, id, actor)
		return err
	})
}

// digestActors returns the callers remembered for the digests ids of a chain.
func (o *Ops) digestActors(ctx context.Context, db, chain string, ids []int64) (map[int64]string, error) {
	out := map[int64]string{}
	if len(ids) == 0 {
		return out, nil
	}
	err := o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT digest_id, actor FROM kingres_cp.ledger_digest_actor
			WHERE database_name = $1 AND chain_id = $2 AND digest_id = ANY ($3)`, db, chain, ids)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var actor string
			if err := rows.Scan(&id, &actor); err != nil {
				return err
			}
			out[id] = actor
		}
		return rows.Err()
	})
	return out, err
}

// ListDigests lists recorded digests, newest first.
func (o *Ops) ListDigests(ctx context.Context, db string, p store.Page) (api.LedgerDigestPage, error) {
	page := api.LedgerDigestPage{Items: []api.LedgerDigest{}}
	if _, err := ledgerDB(db); err != nil {
		return page, err
	}
	if o.LedgerDigests != nil {
		return o.LedgerDigests(ctx, db, p)
	}
	before, bounded, err := store.DecodeKey(p.Cursor)
	if err != nil {
		return page, problem.Validation("The cursor is not valid.", field("cursor", "not a cursor issued by this server", "invalid_cursor"))
	}
	var (
		listed    []digestRow
		chain     string
		group     int32
		uncovered []int32
	)
	err = o.asService(ctx, db, func(tx pgx.Tx) error {
		inst, rec, err := ledgerRecords(ctx, tx)
		if err != nil || !inst || !rec {
			return err // without records the list is empty
		}
		var bound any
		if bounded {
			bound = before
		}
		rows, err := tx.Query(ctx, "SELECT "+digestCols+" FROM kingres.ledger_digest WHERE ($1::bigint IS NULL OR digest_id < $1) ORDER BY digest_id DESC LIMIT $2", bound, p.Limit+1)
		if err != nil {
			return err
		}
		list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (digestRow, error) {
			var d digestRow
			return d, r.Scan(&d.id, &d.at, &d.by, &d.height, &d.hash, &d.hashVersion, &d.doc)
		})
		if err != nil {
			return err
		}
		if len(list) > p.Limit {
			next := store.EncodeKey(list[p.Limit-1].id)
			page.NextCursor = &next
			list = list[:p.Limit]
		}
		ch, err := chainOf(ctx, tx)
		if err != nil {
			return err
		}
		chain, group, listed = ch.id, ch.group, list
		uncovered = otherGroups(ctx, tx, group)
		return nil
	})
	if err != nil || len(listed) == 0 {
		return page, wrap(err)
	}
	ids := make([]int64, len(listed))
	for i, d := range listed {
		ids[i] = d.id
	}
	actors, err := o.digestActors(ctx, db, chain, ids)
	if err != nil {
		return page, wrap(err)
	}
	for _, d := range listed {
		page.Items = append(page.Items, toDigest(db, d, group, uncovered, actors[d.id]))
	}
	return page, nil
}

type verifyParams struct {
	Database string   `json:"database"`
	Scope    string   `json:"scope"`
	Checks   []string `json:"checks,omitempty"`
}

// PrepareVerification validates a verification request.
func (o *Ops) PrepareVerification(ctx context.Context, db string, req api.LedgerVerifyRequest) (any, error) {
	if _, err := ledgerDB(db); err != nil {
		return nil, err
	}
	scope := req.Scope
	switch scope {
	case "":
		scope = "sinceLastVerified"
	case "full", "sinceLastVerified":
	default:
		return nil, problem.Validation("The request is invalid.", field("/scope", "must be full or sinceLastVerified", "enum"))
	}
	if req.DigestID != nil {
		return nil, problem.New(problem.UnsupportedOp, "Verifying against a recorded digest is not supported yet.")
	}
	seen := map[string]bool{}
	for _, ck := range req.Checks {
		switch {
		case ck != "chain" && ck != "publication" && ck != "anchors":
			return nil, problem.Validation("The request is invalid.", field("/checks", "each check is chain, publication or anchors", "enum"))
		case seen[ck]:
			return nil, problem.Validation("The request is invalid.", field("/checks", "a check is named once", "unique"))
		}
		seen[ck] = true
	}
	if seen["anchors"] {
		return nil, problem.New(problem.UnsupportedOp, "The check of the anchors is not supported by this server version yet.")
	}
	if seen["publication"] && o.LedgerAudit == nil {
		return nil, problem.New(problem.UnsupportedOp, "This server does not publish the ledger, so there is no publication to audit.")
	}
	err := o.asService(ctx, db, func(tx pgx.Tx) error {
		if err := needRecords(ctx, tx); err != nil {
			return err
		}
		return needCurrentLedger(ctx, tx)
	})
	if err != nil {
		return nil, wrap(err)
	}
	return verifyParams{Database: db, Scope: scope, Checks: req.Checks}, nil
}

func (o *Ops) stepVerifyChain(ctx context.Context, env *task.Env) error {
	p, err := inputOf[verifyParams](env.Task)
	if err != nil {
		return err
	}
	c, err := o.Service.ServiceConn(ctx, p.Database)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	// The extension is checked before anything of its schema is read or called (extguard).
	if err := needRecords(ctx, c); err != nil {
		return err
	}
	from := int64(0)
	if p.Scope == "sinceLastVerified" {
		// Continue after the last clean verification.
		var to *int64
		if err := c.QueryRow(ctx, "SELECT max(to_height) FROM kingres.ledger_verification WHERE state = 'clean'").Scan(&to); err != nil {
			return err
		}
		if to != nil {
			from = *to + 1
		}
	}
	var top *int64
	if err := c.QueryRow(ctx, "SELECT max(height) FROM kingres.ledger_blocks").Scan(&top); err != nil {
		return err
	}
	if top == nil {
		// Nothing to verify. A task that ends successfully must have made a record
		// (CP2-08), so an empty ledger is a failure the operator can read.
		return errors.New("the ledger is empty: there is nothing to verify")
	}
	if from > *top {
		// Nothing is new since the last clean run: check the head block again, so
		// that the task that is reported done has a record of what it checked.
		from = *top
	}
	// The record of a verification is a write: say what to run, not
	// "insufficient privilege", when the SQL of the extension is older than
	// the library.
	if err := needCurrentLedger(ctx, c); err != nil {
		return err
	}
	var id int64
	if err := c.QueryRow(ctx, "SELECT kingres.ledger_verify_recorded($1, NULL)", from).Scan(&id); err != nil {
		return err
	}
	if err := o.recordVerificationTask(ctx, p.Database, c, id, env.Task.ID); err != nil {
		return err
	}
	var state string
	var problems int
	if err := c.QueryRow(ctx, "SELECT state, coalesce(problem_count, 0) FROM kingres.ledger_verification WHERE verification_id = $1", id).Scan(&state, &problems); err != nil {
		return err
	}
	if state != "clean" {
		return fmt.Errorf("the verification found %d problem(s); see kingres.ledger_verification %d", problems, id)
	}
	return nil
}

// stepLedgerRetire is the first step of dropDatabase: the publication of the chains of the database ends with a final round, the
// chain-retired events and one aggregate leaf in which every member is retired (design 5.5). It is idempotent, and does nothing when the
// database has no published chain.
func (o *Ops) stepLedgerRetire(ctx context.Context, env *task.Env) error {
	if o.LedgerRetireDatabase == nil {
		return task.Skip("this server does not publish the ledger")
	}
	p, err := inputOf[dropParams](env.Task)
	if err != nil {
		return err
	}
	return o.LedgerRetireDatabase(ctx, p.Name)
}

// stepVerifyLedger runs the checks of a verification: the chain (always: the task that is done leaves the extension's record), then the
// audit of the stored publication when it was asked for. Both are run, so the operator reads every finding at once; the task fails when
// either found something.
func (o *Ops) stepVerifyLedger(ctx context.Context, env *task.Env) error {
	p, err := inputOf[verifyParams](env.Task)
	if err != nil {
		return err
	}
	chainErr := o.stepVerifyChain(ctx, env)
	var findings []string
	for _, ck := range p.Checks {
		if ck != "publication" {
			continue
		}
		f, err := o.LedgerAudit(ctx, p.Database)
		if err != nil {
			return fmt.Errorf("the audit of the publication could not run: %w", err)
		}
		findings = append(findings, f...)
	}
	switch {
	case chainErr != nil && len(findings) > 0:
		return fmt.Errorf("%w; the audit of the publication found %d problem(s): %s", chainErr, len(findings), strings.Join(findings, "; "))
	case len(findings) > 0:
		return fmt.Errorf("the audit of the publication found %d problem(s): %s", len(findings), strings.Join(findings, "; "))
	}
	return chainErr
}

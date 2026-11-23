// SPDX-License-Identifier: AGPL-3.0-only
// Portions Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extguard"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// Verification records (listLedgerVerifications, getLedgerVerification).
//
// The records are the extension's (kingres.ledger_verification of the user
// database); the control plane adds two things in kingres_cp.ledger_verification
// of the system database: its own identifier of each record (allocated in the
// order of the extension's ids, the first time a record is seen, so ids grow
// with time and stay unique across a dropped and re-created database) and the
// task that produced it. Records the extension made without a task (started
// in SQL or by the ledger worker) get an identifier the first time they are
// listed and no task. Reads that allocate ids need the system database to be
// writable, which is why both operations are answered by the manager leader.

const verifyTable = "kingres_cp.ledger_verification"

// encodeVerifyCursor makes the cursor of a verification list: "v<id>", so a
// cursor of another list ("k<id>", "o<n>") is refused.
func encodeVerifyCursor(id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte("v" + strconv.FormatInt(id, 10)))
}

// decodeVerifyCursor parses a cursor; "" means no bound.
func decodeVerifyCursor(c string) (id int64, present bool, err error) {
	if c == "" {
		return 0, false, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil || len(b) < 2 || b[0] != 'v' {
		return 0, false, store.ErrInvalidCursor
	}
	n, err := strconv.ParseInt(string(b[1:]), 10, 64)
	if err != nil || n < 1 {
		return 0, false, store.ErrInvalidCursor
	}
	return n, true, nil
}

// ledgerChain identifies the chain a record belongs to.
type ledgerChain struct {
	id    string
	group int32
}

// localGroup is the shard group of this node's chain (0, the map group, when
// the database is not sharded).
func localGroup(ctx context.Context, x execer) int32 {
	var ok bool
	if info, err := extguard.Verify(ctx, x, extguard.Shard); err != nil || !info.Present {
		return 0 // absent, or a schema that cannot be trusted: nothing of it is read
	}
	if x.QueryRow(ctx, `SELECT pg_catalog.to_regclass('kingres.local_node') IS NOT NULL AND pg_catalog.to_regclass('kingres.node') IS NOT NULL`).Scan(&ok) != nil || !ok {
		return 0
	}
	var g int32
	if x.QueryRow(ctx, `SELECT n.group_id FROM kingres.local_node l JOIN kingres.node n ON n.node_id = l.node_id LIMIT 1`).Scan(&g) != nil {
		return 0
	}
	return g
}

// otherGroups lists the shard groups of the database other than the one whose
// chain this server holds: the chains that a digest or a verification made here
// does not cover. Never nil, so that the answer says "none" with [].
func otherGroups(ctx context.Context, x execer, local int32) []int32 {
	out := []int32{}
	var ok bool
	if info, err := extguard.Verify(ctx, x, extguard.Shard); err != nil || !info.Present {
		return out
	}
	if x.QueryRow(ctx, `SELECT pg_catalog.to_regclass('kingres.node') IS NOT NULL`).Scan(&ok) != nil || !ok {
		return out
	}
	rows, err := x.Query(ctx, `SELECT DISTINCT group_id FROM kingres.node WHERE group_id <> $1 ORDER BY group_id`, local)
	if err != nil {
		return out
	}
	groups, err := pgx.CollectRows(rows, pgx.RowTo[int32])
	if err != nil {
		return out
	}
	return append(out, groups...)
}

func chainOf(ctx context.Context, x execer) (ledgerChain, error) {
	// Nothing of the schema kingres is read before the extension is known to be the extension's own (extguard).
	if _, err := needExt(ctx, x, extguard.Ledger, "The ledger is not enabled on this database."); err != nil {
		return ledgerChain{}, err
	}
	c := ledgerChain{group: localGroup(ctx, x)}
	err := x.QueryRow(ctx, `SELECT encode(chain_id, 'hex') FROM kingres.ledger_chain LIMIT 1`).Scan(&c.id)
	return c, err
}

// extVerification is a row of kingres.ledger_verification.
type extVerification struct {
	id         int64
	startedAt  time.Time
	finishedAt *time.Time
	startedBy  string
	from       int64
	state      string
	blocks     *int64
	rows       *int64
	problems   []byte
}

const extVerificationCols = `verification_id, started_at, finished_at, started_by::text, from_height, state, blocks_checked, rows_checked, problems`

func scanExtVerification(r pgx.CollectableRow) (extVerification, error) {
	var e extVerification
	err := r.Scan(&e.id, &e.startedAt, &e.finishedAt, &e.startedBy, &e.from, &e.state, &e.blocks, &e.rows, &e.problems)
	return e, err
}

// verificationLink is what the system database remembers of a record.
type verificationLink struct {
	id      int64
	extID   int64
	group   int32
	taskID  *int64
	creator *string // created_by of the task
}

// kindOf maps a check of the extension to the kind of the contract.
func kindOf(check string) string {
	switch check {
	case "block_hash":
		return "blockHashMismatch"
	case "block_prev_hash":
		return "prevHashMismatch"
	case "block_tx_root", "tx_root":
		return "merkleRootMismatch"
	case "table_rows", "history_rows":
		return "rowHashMismatch"
	case "block_height", "tx_block":
		return "missingBlock"
	}
	return "other"
}

var (
	blockInObject = regexp.MustCompile(`block (\d+)`)
	tableObject   = regexp.MustCompile(`^[^ ]+\.[^ ]+$`)
)

type extProblem struct {
	Check  string  `json:"check"`
	Object *string `json:"object"`
	Detail string  `json:"detail"`
}

// inconsistencies converts the problems of a record.
func inconsistencies(raw []byte, group int32) []api.LedgerInconsistency {
	out := []api.LedgerInconsistency{}
	if len(raw) == 0 {
		return out
	}
	var ps []extProblem
	if json.Unmarshal(raw, &ps) != nil {
		return out
	}
	for _, p := range ps {
		in := api.LedgerInconsistency{ShardGroupID: group, Kind: kindOf(p.Check)}
		obj := ""
		if p.Object != nil {
			obj = *p.Object
			if m := blockInObject.FindStringSubmatch(obj); m != nil {
				if h, err := strconv.ParseInt(m[1], 10, 64); err == nil {
					in.BlockHeight = &h
				}
			}
			if tableObject.MatchString(obj) {
				t := obj
				in.Table = &t
			}
		}
		detail := p.Detail
		if obj != "" {
			detail = obj + ": " + detail
		}
		if in.Kind == "other" {
			detail = p.Check + ": " + detail
		}
		in.Detail = detail
		out = append(out, in)
	}
	return out
}

// toVerification maps the record of the extension and its link to the
// contract's schema.
func toVerification(db string, l verificationLink, e extVerification, uncovered []int32) api.LedgerVerification {
	if uncovered == nil {
		uncovered = []int32{}
	}
	v := api.LedgerVerification{
		ID: l.id, DatabaseName: db, Scope: "sinceLastVerified", TaskID: l.taskID,
		StartedAt: e.startedAt.UTC(), BlocksChecked: e.blocks, RowsChecked: e.rows,
		Inconsistencies: []api.LedgerInconsistency{},
		Complete:        len(uncovered) == 0, UncoveredShardGroupIDs: uncovered,
	}
	if e.from == 0 {
		v.Scope = "full"
	}
	if e.finishedAt != nil {
		f := e.finishedAt.UTC()
		v.FinishedAt = &f
	}
	switch e.state {
	case "clean":
		v.State = "passed"
	case "problems":
		v.State = "failed"
		v.Inconsistencies = inconsistencies(e.problems, l.group)
	case "failed":
		// The run itself aborted: its cause is in the error of the task, and
		// the counts are not known.
		v.State = "failed"
		v.BlocksChecked, v.RowsChecked = nil, nil
	default:
		v.State = "running"
	}
	by := e.startedBy
	switch {
	case l.creator != nil:
		by = *l.creator
	case by == instance.ServiceRole:
		by = "system"
	}
	if by != "" {
		v.StartedBy = &by
	}
	return v
}

// syncVerificationIDs gives every record in ext (ascending) that has no
// identifier one, in that order. Callers hold the advisory lock of the table.
func syncVerificationIDs(ctx context.Context, tx pgx.Tx, db string, ch ledgerChain, ext []int64) error {
	if len(ext) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO `+verifyTable+` (database_name, chain_id, shard_group_id, extension_id)
		SELECT $1, $2, $3, u.e FROM unnest($4::bigint[]) WITH ORDINALITY AS u(e, o) ORDER BY u.o
		ON CONFLICT (database_name, chain_id, shard_group_id, extension_id) DO NOTHING`, db, ch.id, ch.group, ext)
	return err
}

func lockVerifications(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('kingres_cp.ledger_verification'))`)
	return err
}

// ListVerifications lists the finished verification runs, newest first.
func (o *Ops) ListVerifications(ctx context.Context, db string, p store.Page) (api.LedgerVerificationPage, error) {
	page := api.LedgerVerificationPage{Items: []api.LedgerVerification{}}
	if _, err := ledgerDB(db); err != nil {
		return page, err
	}
	before, bounded, err := decodeVerifyCursor(p.Cursor)
	if err != nil {
		return page, problem.Validation("The cursor is not valid.", field("cursor", "not a cursor issued by this server for this list", "invalid_cursor"))
	}
	var (
		ch        ledgerChain
		ext       []int64
		uncovered []int32
		found     bool
	)
	err = o.asService(ctx, db, func(tx pgx.Tx) error {
		inst, rec, err := ledgerRecords(ctx, tx)
		if err != nil || !inst || !rec {
			return err // without records the list is empty
		}
		if ch, err = chainOf(ctx, tx); err != nil {
			return err
		}
		uncovered = otherGroups(ctx, tx, ch.group)
		rows, err := tx.Query(ctx, `SELECT verification_id FROM kingres.ledger_verification WHERE state <> 'running' ORDER BY verification_id`)
		if err != nil {
			return err
		}
		if ext, err = pgx.CollectRows(rows, pgx.RowTo[int64]); err != nil {
			return err
		}
		found = true
		return nil
	})
	if err != nil || !found {
		return page, wrap(err)
	}
	var links []verificationLink
	err = o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
		if err := lockVerifications(ctx, tx); err != nil {
			return err
		}
		if err := syncVerificationIDs(ctx, tx, db, ch, ext); err != nil {
			return err
		}
		var bound any
		if bounded {
			bound = before
		}
		rows, err := tx.Query(ctx, `SELECT v.id, v.extension_id, v.shard_group_id, v.task_id, t.created_by
			FROM `+verifyTable+` v LEFT JOIN kingres_cp.task t ON t.id = v.task_id
			WHERE v.database_name = $1 AND v.chain_id = $2 AND v.shard_group_id = $3 AND ($4::bigint IS NULL OR v.id < $4)
			ORDER BY v.id DESC LIMIT $5`, db, ch.id, ch.group, bound, p.Limit+1)
		if err != nil {
			return err
		}
		links, err = pgx.CollectRows(rows, scanLink)
		return err
	})
	if err != nil {
		return page, wrap(err)
	}
	if len(links) > p.Limit {
		next := encodeVerifyCursor(links[p.Limit-1].id)
		page.NextCursor = &next
		links = links[:p.Limit]
	}
	if len(links) == 0 {
		return page, nil
	}
	ids := make([]int64, len(links))
	for i, l := range links {
		ids[i] = l.extID
	}
	byExt := map[int64]extVerification{}
	err = o.asService(ctx, db, func(tx pgx.Tx) error {
		if _, err := needExt(ctx, tx, extguard.Ledger, "The ledger is not enabled on this database."); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+extVerificationCols+` FROM kingres.ledger_verification WHERE verification_id = ANY ($1)`, ids)
		if err != nil {
			return err
		}
		list, err := pgx.CollectRows(rows, scanExtVerification)
		for _, e := range list {
			byExt[e.id] = e
		}
		return err
	})
	if err != nil {
		return page, wrap(err)
	}
	for _, l := range links {
		if e, ok := byExt[l.extID]; ok {
			page.Items = append(page.Items, toVerification(db, l, e, uncovered))
		}
	}
	return page, nil
}

func scanLink(r pgx.CollectableRow) (verificationLink, error) {
	var l verificationLink
	err := r.Scan(&l.id, &l.extID, &l.group, &l.taskID, &l.creator)
	return l, err
}

// GetVerification returns one verification run.
func (o *Ops) GetVerification(ctx context.Context, db string, id int64) (api.LedgerVerification, error) {
	none := problem.New(problem.NotFound, "No such verification run.")
	if _, err := ledgerDB(db); err != nil {
		return api.LedgerVerification{}, err
	}
	var (
		l      verificationLink
		dbName string
		chain  string
	)
	err := o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT v.id, v.extension_id, v.shard_group_id, v.task_id, t.created_by, v.database_name, v.chain_id
			FROM `+verifyTable+` v LEFT JOIN kingres_cp.task t ON t.id = v.task_id WHERE v.id = $1`, id).
			Scan(&l.id, &l.extID, &l.group, &l.taskID, &l.creator, &dbName, &chain)
		if errors.Is(err, pgx.ErrNoRows) {
			return none
		}
		return err
	})
	if err != nil {
		return api.LedgerVerification{}, wrap(err)
	}
	if dbName != db {
		return api.LedgerVerification{}, none
	}
	var out api.LedgerVerification
	err = o.asService(ctx, db, func(tx pgx.Tx) error {
		inst, rec, err := ledgerRecords(ctx, tx)
		if err != nil {
			return err
		}
		if !inst || !rec {
			return none
		}
		if ch, err := chainOf(ctx, tx); err != nil || ch.id != chain {
			if err != nil {
				return err
			}
			return none // the database was dropped and created again
		}
		rows, err := tx.Query(ctx, `SELECT `+extVerificationCols+` FROM kingres.ledger_verification WHERE verification_id = $1 AND state <> 'running'`, l.extID)
		if err != nil {
			return err
		}
		list, err := pgx.CollectRows(rows, scanExtVerification)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			return none
		}
		out = toVerification(db, l, list[0], otherGroups(ctx, tx, l.group))
		return nil
	})
	return out, wrap(err)
}

// recordVerificationTask stores that the record extID of database db was made
// by task taskID (called by the verification task when the run ends). The
// older records of the chain get their identifiers first, so identifiers keep
// the order of the extension's.
func (o *Ops) recordVerificationTask(ctx context.Context, db string, c execer, extID, taskID int64) error {
	ch, err := chainOf(ctx, c)
	if err != nil {
		return err
	}
	rows, err := c.Query(ctx, `SELECT verification_id FROM kingres.ledger_verification WHERE verification_id <= $1 AND state <> 'running' ORDER BY verification_id`, extID)
	if err != nil {
		return err
	}
	ext, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return err
	}
	return o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
		if err := lockVerifications(ctx, tx); err != nil {
			return err
		}
		if err := syncVerificationIDs(ctx, tx, db, ch, ext); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE `+verifyTable+` SET task_id = $1
			WHERE database_name = $2 AND chain_id = $3 AND shard_group_id = $4 AND extension_id = $5`, taskID, db, ch.id, ch.group, extID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errors.New("the verification record was not found to link it to its task")
		}
		return nil
	})
}

// SPDX-License-Identifier: AGPL-3.0-only

package ledgeranchor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
)

// SysFunc runs f with a service connection to the system database (the leader's).
type SysFunc func(ctx context.Context, f func(c *pgx.Conn) error) error

// PGStore is the Store in kingres_cp (migration 0032, with the logs and checkpoints of 0030).
type PGStore struct {
	Sys SysFunc
	// OnWrite, when set, is called after the guard of every write transaction and before its statements, with the
	// kind of the write ("target", "enqueue", "begin", "apply"). A test uses it to end the process at a write.
	OnWrite func(kind string)
}

func (s PGStore) write(ctx context.Context, term scheduler.Term, kind string, f func(tx pgx.Tx) error) error {
	return s.Sys(ctx, func(c *pgx.Conn) error {
		tx, err := c.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
		if err := term.Guard(ctx, tx); err != nil {
			return err
		}
		if s.OnWrite != nil {
			s.OnWrite(kind)
		}
		if err := f(tx); err != nil {
			return err
		}
		return tx.Commit(ctx)
	})
}

func (s PGStore) read(ctx context.Context, f func(c *pgx.Conn) error) error { return s.Sys(ctx, f) }

func isUnique(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

const targetCols = `id, kind, name, enabled, enabled_since, config::text, credentials_set, last_test::text, revision, created_by, created_at, updated_at`

func scanTarget(row pgx.Row) (TargetRow, error) {
	var t TargetRow
	var cfg string
	var lt *string
	if err := row.Scan(&t.ID, &t.Kind, &t.Name, &t.Enabled, &t.EnabledSince, &cfg, &t.CredentialsSet, &lt, &t.Revision, &t.CreatedBy, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return TargetRow{}, err
	}
	t.Config = json.RawMessage(cfg)
	if lt != nil {
		t.LastTest = json.RawMessage(*lt)
	}
	t.CreatedAt, t.UpdatedAt = t.CreatedAt.UTC(), t.UpdatedAt.UTC()
	return t, nil
}

// Targets implements Store.
func (s PGStore) Targets(ctx context.Context) ([]TargetRow, error) {
	var out []TargetRow
	err := s.read(ctx, func(c *pgx.Conn) error {
		rows, err := c.Query(ctx, `SELECT `+targetCols+` FROM kingres_cp.ledger_anchor_target ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			t, err := scanTarget(rows)
			if err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

// Target implements Store.
func (s PGStore) Target(ctx context.Context, id int64) (TargetRow, error) {
	var t TargetRow
	err := s.read(ctx, func(c *pgx.Conn) error {
		var err error
		t, err = scanTarget(c.QueryRow(ctx, `SELECT `+targetCols+` FROM kingres_cp.ledger_anchor_target WHERE id = $1`, id))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return TargetRow{}, ErrNotFound
	}
	return t, err
}

// CreateTarget implements Store.
func (s PGStore) CreateTarget(ctx context.Context, term scheduler.Term, t TargetRow) (TargetRow, error) {
	var out TargetRow
	err := s.write(ctx, term, "target", func(tx pgx.Tx) error {
		var err error
		out, err = scanTarget(tx.QueryRow(ctx, `INSERT INTO kingres_cp.ledger_anchor_target
			(kind, name, enabled, enabled_since, config, credentials_set, created_by) VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7)
			RETURNING `+targetCols, t.Kind, t.Name, t.Enabled, t.EnabledSince, string(t.Config), t.CredentialsSet, t.CreatedBy))
		return err
	})
	if isUnique(err) {
		return TargetRow{}, ErrExists
	}
	return out, err
}

// UpdateTarget implements Store.
func (s PGStore) UpdateTarget(ctx context.Context, term scheduler.Term, t TargetRow, expect int64) (TargetRow, error) {
	var out TargetRow
	err := s.write(ctx, term, "target", func(tx pgx.Tx) error {
		var err error
		out, err = scanTarget(tx.QueryRow(ctx, `UPDATE kingres_cp.ledger_anchor_target SET name = $2, enabled = $3, enabled_since = $4, config = $5::jsonb,
			credentials_set = $6, revision = revision + 1, updated_at = now() WHERE id = $1 AND revision = $7 RETURNING `+targetCols,
			t.ID, t.Name, t.Enabled, t.EnabledSince, string(t.Config), t.CredentialsSet, expect))
		if errors.Is(err, pgx.ErrNoRows) {
			var one int
			if e := tx.QueryRow(ctx, `SELECT 1 FROM kingres_cp.ledger_anchor_target WHERE id = $1`, t.ID).Scan(&one); e != nil {
				return ErrNotFound
			}
			return ErrConflict
		}
		return err
	})
	if isUnique(err) {
		return TargetRow{}, ErrExists
	}
	return out, err
}

// DeleteTarget implements Store.
func (s PGStore) DeleteTarget(ctx context.Context, term scheduler.Term, id int64) error {
	return s.write(ctx, term, "target", func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM kingres_cp.ledger_anchor_target WHERE id = $1`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// SetLastTest implements Store.
func (s PGStore) SetLastTest(ctx context.Context, term scheduler.Term, id int64, test json.RawMessage) error {
	return s.write(ctx, term, "target", func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE kingres_cp.ledger_anchor_target SET last_test = $2::jsonb WHERE id = $1`, id, string(test))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// Settings implements Store.
func (s PGStore) Settings(ctx context.Context) (Settings, error) {
	var set Settings
	err := s.read(ctx, func(c *pgx.Conn) error {
		var secs int
		if err := c.QueryRow(ctx, `SELECT anchor_interval_seconds, anchor_minimum, anchor_required FROM kingres_cp.ledger_cluster`).Scan(&secs, &set.Minimum, &set.Required); err != nil {
			return err
		}
		set.Interval = time.Duration(secs) * time.Second
		return nil
	})
	return set, err
}

const aggQuery = `SELECT l.log_id, l.database_name, l.origin, c.size, c.note, c.signed_at
	FROM kingres_cp.ledger_log l
	JOIN LATERAL (SELECT size, note, signed_at FROM kingres_cp.ledger_checkpoint WHERE log_id = l.log_id ORDER BY size DESC LIMIT 1) c ON true
	WHERE l.kind = 'aggregate' AND l.state <> 'closed' AND l.database_name IS NOT NULL`

func scanAgg(row pgx.Row) (Aggregate, error) {
	var a Aggregate
	if err := row.Scan(&a.LogID, &a.Database, &a.Origin, &a.Size, &a.Note, &a.SignedAt); err != nil {
		return Aggregate{}, err
	}
	a.AggregateID = path.Base(a.Origin)
	a.SignedAt = a.SignedAt.UTC()
	return a, nil
}

// Aggregates implements Store.
func (s PGStore) Aggregates(ctx context.Context) ([]Aggregate, error) {
	var out []Aggregate
	err := s.read(ctx, func(c *pgx.Conn) error {
		rows, err := c.Query(ctx, aggQuery+` ORDER BY l.log_id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			a, err := scanAgg(rows)
			if err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	return out, err
}

// AggregateOf implements Store.
func (s PGStore) AggregateOf(ctx context.Context, db string) (Aggregate, error) {
	var a Aggregate
	err := s.read(ctx, func(c *pgx.Conn) error {
		var err error
		a, err = scanAgg(c.QueryRow(ctx, aggQuery+` AND l.database_name = $1`, db))
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var one int
		if e := c.QueryRow(ctx, `SELECT 1 FROM kingres_cp.ledger_log WHERE kind = 'aggregate' AND state <> 'closed' AND database_name = $1`, db).Scan(&one); e != nil {
			return ErrNotFound
		}
		return ErrNoAggregate
	})
	return a, err
}

// FirstAfter implements Store.
func (s PGStore) FirstAfter(ctx context.Context, logID, size int64) (time.Time, bool, error) {
	var t *time.Time
	err := s.read(ctx, func(c *pgx.Conn) error {
		return c.QueryRow(ctx, `SELECT min(signed_at) FROM kingres_cp.ledger_checkpoint WHERE log_id = $1 AND size > $2`, logID, size).Scan(&t)
	})
	if err != nil || t == nil {
		return time.Time{}, false, err
	}
	return t.UTC(), true, nil
}

const rowCols = `a.id, a.target_id, a.target_name, a.kind, a.log_id, a.database_name, a.aggregate_id, a.origin, a.size, a.payload_sha256, a.state,
	a.attempts, a.refusals, a.retryables, a.failing_since, a.next_attempt_at, a.evidence, a.evidence_time, a.anchored_at, a.last_error, a.error_code,
	a.alert, a.created_at, a.submit_start, a.generation, a.version, a.updated_at`

// scanRow reads rowCols; withNote adds the note as the last column.
func scanRow(row pgx.Row, withNote bool) (Row, error) {
	var r Row
	var tid *int64
	var logID, size int64
	var digest []byte
	var state string
	var evidence *string
	var start *time.Time
	var gen int32
	dest := []any{&r.ID, &tid, &r.TargetName, &r.Kind, &logID, &r.Database, &r.AggregateID, &r.Origin, &size, &digest, &state,
		&r.Attempts, &r.Refusals, &r.Retryables, &r.FailingSince, &r.NextAttemptAt, &evidence, &r.EvidenceTime, &r.AnchoredAt, &r.LastError, &r.ErrorCode,
		&r.Alert, &r.CreatedAt, &start, &gen, &r.Version, &r.UpdatedAt}
	if withNote {
		dest = append(dest, &r.Note)
	}
	if err := row.Scan(dest...); err != nil {
		return Row{}, err
	}
	if tid == nil {
		r.TargetGone = true
	} else {
		r.TargetID = *tid
	}
	r.Record.LogID = logKey(logID)
	r.Size = uint64(size) //nolint:gosec // the column is at least 1
	copy(r.PayloadSHA256[:], digest)
	r.State = anchor.State(state)
	if evidence != nil {
		r.Evidence = json.RawMessage(*evidence)
	}
	if start != nil {
		r.SubmitStart = start.UTC().Truncate(time.Second)
	}
	r.Generation = uint32(gen) //nolint:gosec // 0 to 1000 by the constraint
	r.CreatedAt, r.UpdatedAt = r.CreatedAt.UTC(), r.UpdatedAt.UTC()
	for _, p := range []**time.Time{&r.FailingSince, &r.NextAttemptAt, &r.EvidenceTime, &r.AnchoredAt} {
		if *p != nil {
			t := (*p).UTC()
			*p = &t
		}
	}
	return r, nil
}

func apiStateSQL(state string) (string, bool) {
	if state == "submitted" {
		return `a.state IN ('submitted', 'claimed')`, true
	}
	return ``, false
}

// Records implements Store.
func (s PGStore) Records(ctx context.Context, f Filter) ([]Row, error) {
	var out []Row
	err := s.read(ctx, func(c *pgx.Conn) error {
		var where []string
		var args []any
		arg := func(v any) string { args = append(args, v); return "$" + strconv.Itoa(len(args)) }
		if f.Database != "" {
			where = append(where, "a.database_name = "+arg(f.Database))
		}
		if f.TargetID != 0 {
			where = append(where, "a.target_id = "+arg(f.TargetID))
		}
		if f.State != "" {
			if cond, ok := apiStateSQL(f.State); ok {
				where = append(where, cond)
			} else {
				where = append(where, "a.state = "+arg(f.State))
			}
		}
		if f.Before != 0 {
			where = append(where, "a.id < "+arg(f.Before))
		}
		q := `SELECT ` + rowCols + ` FROM kingres_cp.ledger_anchor a`
		if len(where) > 0 {
			q += ` WHERE ` + strings.Join(where, " AND ")
		}
		q += ` ORDER BY a.id DESC`
		if f.Limit > 0 {
			q += ` LIMIT ` + arg(f.Limit)
		}
		rows, err := c.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanRow(rows, false)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

const noteSel = `, (SELECT note FROM kingres_cp.ledger_anchor n WHERE n.id = a.id)`

// Record implements Store.
func (s PGStore) Record(ctx context.Context, id int64) (Row, error) {
	var r Row
	err := s.read(ctx, func(c *pgx.Conn) error {
		var err error
		r, err = scanRow(c.QueryRow(ctx, `SELECT `+rowCols+noteSel+` FROM kingres_cp.ledger_anchor a WHERE a.id = $1`, id), true)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Row{}, ErrNotFound
	}
	return r, err
}

// Enqueue implements Store.
func (s PGStore) Enqueue(ctx context.Context, term scheduler.Term, e Enqueue) (Row, bool, error) {
	pl, err := anchor.ParsePayload(e.Agg.AggregateID, e.Agg.Note)
	if err != nil {
		return Row{}, false, fmt.Errorf("the checkpoint of the aggregate log is not a payload: %w", err)
	}
	if pl.Origin != e.Agg.Origin || pl.Size != uint64(e.Agg.Size) { //nolint:gosec // at least 1
		return Row{}, false, errors.New("the note of the checkpoint is not the one of its log and size")
	}
	var out Row
	created := false
	err = s.write(ctx, term, "enqueue", func(tx pgx.Tx) error {
		// a record for this size, or a newer one, already exists: the checkpoint is covered
		r, err := scanRow(tx.QueryRow(ctx, `SELECT `+rowCols+` FROM kingres_cp.ledger_anchor a
			WHERE a.target_id = $1 AND a.log_id = $2 AND a.size >= $3 ORDER BY a.size LIMIT 1`, e.Target.ID, e.Agg.LogID, e.Agg.Size), false)
		switch {
		case err == nil:
			out = r
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		// the older records of this (target, log) that are still pending are superseded by the new one; the state
		// machine of the library is the only thing that decides what "superseded" is, so the rows are read and
		// written back through it
		rows, err := tx.Query(ctx, `SELECT `+rowCols+` FROM kingres_cp.ledger_anchor a WHERE a.target_id = $1 AND a.log_id = $2 AND a.state = 'pending' FOR UPDATE OF a`, e.Target.ID, e.Agg.LogID)
		if err != nil {
			return err
		}
		var old []Row
		for rows.Next() {
			o, err := scanRow(rows, false)
			if err != nil {
				rows.Close()
				return err
			}
			old = append(old, o)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, o := range old {
			sup, err := o.Record.Supersede()
			if err != nil {
				continue
			}
			if _, err := tx.Exec(ctx, `UPDATE kingres_cp.ledger_anchor SET state = $2, next_attempt_at = NULL, version = version + 1, updated_at = now() WHERE id = $1`,
				o.ID, string(sup.State)); err != nil {
				return err
			}
		}
		out, err = scanRow(tx.QueryRow(ctx, `INSERT INTO kingres_cp.ledger_anchor AS a (target_id, target_name, kind, log_id, database_name, aggregate_id, origin, size,
			payload_sha256, note, state, next_attempt_at, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'pending', $11, $11)
			RETURNING `+rowCols, e.Target.ID, e.Target.Name, e.Target.Kind, e.Agg.LogID, e.Agg.Database, e.Agg.AggregateID, e.Agg.Origin, e.Agg.Size,
			pl.Digest[:], e.Agg.Note, e.Now.UTC()), false)
		created = err == nil
		return err
	})
	return out, created, err
}

// Begin implements Store.
func (s PGStore) Begin(ctx context.Context, term scheduler.Term, id int64, now time.Time) (Row, error) {
	var out Row
	err := s.write(ctx, term, "begin", func(tx pgx.Tx) error {
		r, err := scanRow(tx.QueryRow(ctx, `SELECT `+rowCols+noteSel+` FROM kingres_cp.ledger_anchor a WHERE a.id = $1 FOR UPDATE OF a`, id), true)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if r.State != anchor.StatePending || r.TargetGone {
			return ErrConflict
		}
		if r.SubmitStart.IsZero() {
			rec, err := r.Record.WithSubmitStart(now)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE kingres_cp.ledger_anchor SET submit_start = $2, version = version + 1, updated_at = now() WHERE id = $1 AND submit_start IS NULL`,
				id, rec.SubmitStart); err != nil {
				return err
			}
			r.Record, r.Version = rec, r.Version+1
		}
		out = r
		return nil
	})
	return out, err
}

// Apply implements Store.
func (s PGStore) Apply(ctx context.Context, term scheduler.Term, w Row, expect int64) error {
	var ev *string
	if len(w.Evidence) > 0 {
		x := string(w.Evidence)
		ev = &x
	}
	var start *time.Time
	if !w.SubmitStart.IsZero() {
		t := w.SubmitStart
		start = &t
	}
	return s.write(ctx, term, "apply", func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE kingres_cp.ledger_anchor SET state = $2, attempts = $3, refusals = $4, retryables = $5, failing_since = $6,
			next_attempt_at = $7, evidence = $8, evidence_time = $9, anchored_at = $10, last_error = $11, error_code = $12, alert = $13, created_at = $14,
			submit_start = $15, generation = $16, version = version + 1, updated_at = now() WHERE id = $1 AND version = $17`,
			w.ID, string(w.State), w.Attempts, w.Refusals, w.Retryables, w.FailingSince, w.NextAttemptAt, ev, w.EvidenceTime, w.AnchoredAt,
			w.LastError, w.ErrorCode, w.Alert, w.CreatedAt, start, int32(w.Generation), expect) //nolint:gosec // at most 1000
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			var one int
			if e := tx.QueryRow(ctx, `SELECT 1 FROM kingres_cp.ledger_anchor WHERE id = $1`, w.ID).Scan(&one); e != nil {
				return ErrNotFound
			}
			return ErrConflict
		}
		if w.State == anchor.StateAnchored {
			_, err = tx.Exec(ctx, `UPDATE kingres_cp.ledger_checkpoint SET anchored = true WHERE log_id = $1 AND size = $2
				AND (SELECT count(DISTINCT target_id) FROM kingres_cp.ledger_anchor WHERE log_id = $1 AND size = $2 AND state = 'anchored')
				    >= (SELECT anchor_minimum FROM kingres_cp.ledger_cluster)`, logID(w.Record.LogID), int64(w.Size)) //nolint:gosec // below 2^62
			return err
		}
		return nil
	})
}

// Due implements Store.
func (s PGStore) Due(ctx context.Context, now time.Time, limit int) ([]Row, error) {
	var out []Row
	err := s.read(ctx, func(c *pgx.Conn) error {
		rows, err := c.Query(ctx, `SELECT `+rowCols+noteSel+` FROM kingres_cp.ledger_anchor a
			JOIN kingres_cp.ledger_anchor_target t ON t.id = a.target_id
			WHERE t.enabled AND a.state IN ('pending', 'submitted', 'claimed')
			  AND (a.next_attempt_at IS NULL OR a.next_attempt_at <= $1 OR a.state = 'claimed')
			ORDER BY a.id LIMIT $2`, now.UTC(), limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanRow(rows, true)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// NewestAnchored implements Store.
func (s PGStore) NewestAnchored(ctx context.Context) ([]Anchored, error) {
	var out []Anchored
	err := s.read(ctx, func(c *pgx.Conn) error {
		rows, err := c.Query(ctx, `SELECT DISTINCT ON (target_id, log_id) target_id, log_id, size, anchored_at, evidence_time
			FROM kingres_cp.ledger_anchor WHERE state = 'anchored' AND target_id IS NOT NULL AND anchored_at IS NOT NULL
			ORDER BY target_id, log_id, size DESC`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a Anchored
			if err := rows.Scan(&a.TargetID, &a.LogID, &a.Size, &a.AnchoredAt, &a.EvidenceTime); err != nil {
				return err
			}
			a.AnchoredAt = a.AnchoredAt.UTC()
			if a.EvidenceTime != nil {
				t := a.EvidenceTime.UTC()
				a.EvidenceTime = &t
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	return out, err
}

// Failures implements Store.
func (s PGStore) Failures(ctx context.Context) ([]Row, error) {
	var out []Row
	err := s.read(ctx, func(c *pgx.Conn) error {
		rows, err := c.Query(ctx, `SELECT `+rowCols+` FROM kingres_cp.ledger_anchor a
			WHERE a.state IN ('failed', 'expired') AND a.alert <> '' AND a.target_id IS NOT NULL ORDER BY a.id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanRow(rows, false)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// DBTime implements Store.
func (s PGStore) DBTime(ctx context.Context) (time.Time, error) {
	var t time.Time
	err := s.read(ctx, func(c *pgx.Conn) error { return c.QueryRow(ctx, `SELECT now()`).Scan(&t) })
	return t.UTC(), err
}

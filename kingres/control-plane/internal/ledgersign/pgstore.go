// SPDX-License-Identifier: AGPL-3.0-only

package ledgersign

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerkeys"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerlog"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
)

// SysFunc runs f with a service connection to the system database (the leader's).
type SysFunc func(ctx context.Context, f func(c *pgx.Conn) error) error

// PGStore is the Store in kingres_cp (migration 0030).
type PGStore struct{ Sys SysFunc }

// BumpTerm is the scheduler's Bump: it increments the stored term.
func (s PGStore) BumpTerm(ctx context.Context) (scheduler.Term, error) {
	var t scheduler.Term
	err := s.Sys(ctx, func(c *pgx.Conn) error {
		var err error
		t, err = scheduler.BumpTerm(ctx, c)
		return err
	})
	return t, err
}

func ts(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func tv(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.UTC().Truncate(time.Second)
}

// Load implements Store.
func (s PGStore) Load(ctx context.Context) (Snapshot, error) {
	var out Snapshot
	err := s.Sys(ctx, func(c *pgx.Conn) error {
		tx, err := c.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
		var trust []byte
		cl := &out.Cluster
		if err := tx.QueryRow(ctx, `SELECT cluster_id, term, keys_revision, coalesce(origin_prefix, ''), public_url, publication_enabled,
			publication_interval_seconds, anchor_interval_seconds, anchor_minimum, anchor_required, first_checkpoint_at,
			convert_to(trust_file, 'UTF8'), holders, backup_advised, discontinuity FROM kingres_cp.ledger_cluster`).Scan(
			&cl.ClusterID, &out.Term, &out.Revision, &cl.OriginPrefix, &cl.PublicURL, &cl.PublicationEnabled,
			&cl.PublicationInterval, &cl.AnchorInterval, &cl.AnchorMinimum, &cl.AnchorRequired, &cl.FirstCheckpointAt,
			&trust, &cl.Holders, &cl.BackupAdvised, &cl.Discontinuity); err != nil {
			return fmt.Errorf("reading kingres_cp.ledger_cluster: %w", err)
		}
		cl.TrustFile = trust
		rows, err := tx.Query(ctx, `SELECT public_key, state, mode, label, created_at, activated_at, overlap_ends, retired_at, revoked_at,
			compromised_since, max_tree_size, reason, coalesce(task_id, 0) FROM kingres_cp.ledger_signing_key ORDER BY created_at, fingerprint`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var k KeyRow
			var pub []byte
			var state string
			var created time.Time
			var act, ov, ret, rev, comp *time.Time
			var max *int64
			if err := rows.Scan(&pub, &state, &k.Mode, &k.Label, &created, &act, &ov, &ret, &rev, &comp, &max, &k.Reason, &k.TaskID); err != nil {
				rows.Close()
				return err
			}
			st, err := ledgerkeys.ParseState(state)
			if err != nil {
				rows.Close()
				return err
			}
			k.Public, k.State, k.CreatedAt = ed25519.PublicKey(pub), st, created.UTC().Truncate(time.Second)
			k.ActivatedAt, k.OverlapEnds, k.RetiredAt, k.RevokedAt, k.CompromisedSince = tv(act), tv(ov), tv(ret), tv(rev), tv(comp)
			if max != nil {
				k.MaxTreeSize = uint64(*max) //nolint:gosec // the column is checked to be at least 1
			}
			out.Keys = append(out.Keys, k)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		ev, err := tx.Query(ctx, `SELECT seq, type, coalesce(subject, ''), note, created_at FROM kingres_cp.ledger_event ORDER BY seq`)
		if err != nil {
			return err
		}
		defer ev.Close()
		for ev.Next() {
			var e EventRow
			if err := ev.Scan(&e.Seq, &e.Type, &e.Subject, &e.Note, &e.CreatedAt); err != nil {
				return err
			}
			e.CreatedAt = e.CreatedAt.UTC()
			out.Events = append(out.Events, e)
		}
		return ev.Err()
	})
	return out, err
}

// Commit implements Store. The first statement is the term guard; the revision is then raised by a
// compare and swap, so a second change that read the same state fails.
func (s PGStore) Commit(ctx context.Context, term scheduler.Term, expect int64, ch Change) error {
	return s.Sys(ctx, func(c *pgx.Conn) error {
		tx, err := c.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
		if err := term.Guard(ctx, tx); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE kingres_cp.ledger_cluster SET keys_revision = keys_revision + 1 WHERE keys_revision = $1`, expect)
		if err != nil {
			return mapConflict(err)
		}
		if tag.RowsAffected() != 1 {
			return ErrConflict
		}
		if ch.CheckpointBound != nil && ch.BoundKey != nil {
			max, err := signedByMax(ctx, tx, ch.BoundKey, int64(*ch.CheckpointBound)) //nolint:gosec // a bound is below 2^62
			if err != nil {
				return err
			}
			if max > *ch.CheckpointBound {
				return fmt.Errorf("%w: a checkpoint signed by the retired key above its bound was stored meanwhile", ErrConflict)
			}
		} else if ch.CheckpointBound != nil {
			var max int64
			if err := tx.QueryRow(ctx, `SELECT coalesce(max(size), 0) FROM kingres_cp.ledger_checkpoint`).Scan(&max); err != nil {
				return err
			}
			if max < 0 || uint64(max) > *ch.CheckpointBound { //nolint:gosec // not negative after the first test
				return fmt.Errorf("%w: a checkpoint above the bound of the retired key was stored meanwhile", ErrConflict)
			}
		}
		if cl := ch.Cluster; cl != nil {
			var prefix *string
			if cl.OriginPrefix != "" {
				prefix = &cl.OriginPrefix
			}
			var trust *string
			if cl.TrustFile != nil {
				t := string(cl.TrustFile)
				trust = &t
			}
			holders := cl.Holders
			if holders == nil {
				holders = []string{}
			}
			if _, err := tx.Exec(ctx, `UPDATE kingres_cp.ledger_cluster SET cluster_id = $1,
				-- The first checkpoint belongs to the collector (ledgerpub.PutCheckpoint), which can store it after this change read the row: the time is
				-- never taken back and the prefix is never changed once it is set, whatever stale row this change started from.
				origin_prefix = CASE WHEN first_checkpoint_at IS NULL THEN $2::text ELSE origin_prefix END, public_url = $3, publication_enabled = $4,
				publication_interval_seconds = $5, anchor_interval_seconds = $6, anchor_minimum = $7, anchor_required = $8,
				first_checkpoint_at = coalesce(first_checkpoint_at, $9::timestamptz),
				trust_file = $10, holders = $11, backup_advised = $12, discontinuity = $13`,
				cl.ClusterID, prefix, cl.PublicURL, cl.PublicationEnabled, cl.PublicationInterval, cl.AnchorInterval, cl.AnchorMinimum,
				cl.AnchorRequired, cl.FirstCheckpointAt, trust, holders, cl.BackupAdvised, cl.Discontinuity); err != nil {
				return mapConflict(err)
			}
		}
		for _, k := range ch.Keys {
			var max *int64
			if k.MaxTreeSize != 0 {
				m := int64(k.MaxTreeSize) //nolint:gosec // at most 2^62-1, checked by the key set
				max = &m
			}
			var task *int64
			if k.TaskID != 0 {
				task = &k.TaskID
			}
			if _, err := tx.Exec(ctx, `INSERT INTO kingres_cp.ledger_signing_key (fingerprint, public_key, state, mode, label, created_at, activated_at,
				overlap_ends, retired_at, revoked_at, compromised_since, max_tree_size, reason, task_id)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
				ON CONFLICT (fingerprint) DO UPDATE SET state = $3, mode = $4, label = $5, activated_at = $7, overlap_ends = $8, retired_at = $9,
				revoked_at = $10, compromised_since = $11, max_tree_size = $12, reason = $13, task_id = $14`,
				k.Fingerprint(), []byte(k.Public), k.State.String(), k.Mode, k.Label, k.CreatedAt.UTC(), ts(k.ActivatedAt), ts(k.OverlapEnds),
				ts(k.RetiredAt), ts(k.RevokedAt), ts(k.CompromisedSince), max, k.Reason, task); err != nil {
				return mapConflict(err)
			}
		}
		for _, e := range ch.Events {
			var subject *string
			if e.Subject != "" {
				subject = &e.Subject
			}
			if _, err := tx.Exec(ctx, `INSERT INTO kingres_cp.ledger_event (seq, type, subject, note, created_at) VALUES ($1, $2, $3, $4, $5)`,
				e.Seq, e.Type, subject, e.Note, e.CreatedAt.UTC()); err != nil {
				return mapConflict(err)
			}
		}
		return tx.Commit(ctx)
	})
}

// mapConflict turns the errors two concurrent changes cause (a deadlock between two share locks that
// both want the row, a serialization failure, a repeated event) into ErrConflict.
func mapConflict(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && (pe.Code == "40P01" || pe.Code == "40001" || pe.Code == "23505") {
		return fmt.Errorf("%w (%s)", ErrConflict, pe.Code)
	}
	return err
}

// DBTime implements Store.
func (s PGStore) DBTime(ctx context.Context) (time.Time, error) {
	var t time.Time
	err := s.Sys(ctx, func(c *pgx.Conn) error { return c.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&t) })
	return t, err
}

// LogOrigins implements Store.
func (s PGStore) LogOrigins(ctx context.Context) ([]string, error) {
	var out []string
	err := s.Sys(ctx, func(c *pgx.Conn) error {
		rows, err := c.Query(ctx, `SELECT origin FROM kingres_cp.ledger_log ORDER BY log_id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var o string
			if err := rows.Scan(&o); err != nil {
				return err
			}
			out = append(out, o)
		}
		return rows.Err()
	})
	return out, err
}

// MaxCheckpointSize implements Store.
func (s PGStore) MaxCheckpointSize(ctx context.Context) (uint64, error) {
	var n int64
	err := s.Sys(ctx, func(c *pgx.Conn) error {
		return c.QueryRow(ctx, `SELECT coalesce(max(size), 0) FROM kingres_cp.ledger_checkpoint`).Scan(&n)
	})
	if n < 0 {
		n = 0
	}
	return uint64(n), err //nolint:gosec // not negative
}

// signedByMax is the largest size above floor of a stored checkpoint that carries a signature of pub (the line is the one of the key id
// for the origin of the log it belongs to). It reads the notes of the checkpoints above the floor only: after the key-retired event that
// is what the other key has signed since, a few rows.
func signedByMax(ctx context.Context, q interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}, pub ed25519.PublicKey, floor int64) (uint64, error) {
	rows, err := q.Query(ctx, `SELECT l.origin, c.size, c.note FROM kingres_cp.ledger_checkpoint c JOIN kingres_cp.ledger_log l USING (log_id) WHERE c.size > $1`, floor)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var max uint64
	for rows.Next() {
		var origin string
		var size int64
		var note []byte
		if err := rows.Scan(&origin, &size, &note); err != nil {
			return 0, err
		}
		n, err := ledgerlog.ParseNote(note)
		if err != nil {
			// a note that does not parse cannot be shown to carry the key; the checkpoint was written by this daemon, so this is damage: refuse
			return 0, fmt.Errorf("a stored checkpoint of %s does not parse: %w", origin, err)
		}
		id := ledgerkeys.KeyID(origin, pub)
		for _, s := range n.Signatures {
			if s.Name == origin && s.ID == id && uint64(size) > max { //nolint:gosec // sizes are positive
				max = uint64(size) //nolint:gosec // as above
			}
		}
	}
	return max, rows.Err()
}

// MaxCheckpointSizeSignedBy implements Store.
func (s PGStore) MaxCheckpointSizeSignedBy(ctx context.Context, pub ed25519.PublicKey) (uint64, error) {
	var n uint64
	err := s.Sys(ctx, func(c *pgx.Conn) error {
		var err error
		n, err = signedByMax(ctx, c, pub, 0)
		return err
	})
	return n, err
}

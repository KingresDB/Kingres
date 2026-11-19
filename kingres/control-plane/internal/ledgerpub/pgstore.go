// SPDX-License-Identifier: AGPL-3.0-only

package ledgerpub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerlog"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
)

// SysFunc runs f with a service connection to the system database (the leader's).
type SysFunc func(ctx context.Context, f func(c *pgx.Conn) error) error

// PGStore is the Store in kingres_cp (migrations 0030 and 0031).
type PGStore struct {
	Sys SysFunc
	// OnWrite, when set, is called after the guard of every write transaction and before its statements, with the kind of the
	// write ("tile", "frontier", "pending", "checkpoint", "note", "state", ...). A test uses it to end the process at a write.
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

// conflictCode maps the errors that two writers that met cause: a unique violation or the refusal to change a tile is ErrConflict; a
// deadlock between two share locks that both want the row, or a failed serialization, is ErrBusy (a try again, never a divergence).
func conflictCode(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case "23505", "KC001":
			return fmt.Errorf("%w (%s)", ErrConflict, pe.Code)
		case "40P01", "40001":
			return fmt.Errorf("%w (%s)", ErrBusy, pe.Code)
		}
	}
	return err
}

const logCols = `log_id, kind, coalesce(database_name, ''), shard_group_id, chain_id, epoch, origin, state, detail, size, root, frontier,
	head_block_hash, updated_at, closed_at, retired_in_seq, pending_entry, unread_since, signed_at`

func scanLog(r pgx.Row) (LogRow, error) {
	var l LogRow
	var size int64
	err := r.Scan(&l.ID, &l.Kind, &l.Database, &l.ShardGroupID, &l.ChainID, &l.Epoch, &l.Origin, &l.State, &l.Detail, &size, &l.Root, &l.Frontier,
		&l.HeadBlockHash, &l.UpdatedAt, &l.ClosedAt, &l.RetiredInSeq, &l.PendingEntry, &l.UnreadSince, &l.SignedAt)
	l.Size = size
	l.UpdatedAt = l.UpdatedAt.UTC()
	return l, err
}

// Logs implements Store.
func (s PGStore) Logs(ctx context.Context) ([]LogRow, error) {
	var out []LogRow
	err := s.Sys(ctx, func(c *pgx.Conn) error {
		rows, err := c.Query(ctx, `SELECT `+logCols+` FROM kingres_cp.ledger_log ORDER BY log_id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			l, err := scanLog(rows)
			if err != nil {
				return err
			}
			out = append(out, l)
		}
		return rows.Err()
	})
	return out, err
}

// LogByOrigin implements Store.
func (s PGStore) LogByOrigin(ctx context.Context, origin string) (LogRow, error) {
	var l LogRow
	err := s.Sys(ctx, func(c *pgx.Conn) error {
		var err error
		l, err = scanLog(c.QueryRow(ctx, `SELECT `+logCols+` FROM kingres_cp.ledger_log WHERE origin = $1`, origin))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return l, ErrNotFound
	}
	return l, err
}

// CreateLog implements Store.
func (s PGStore) CreateLog(ctx context.Context, term scheduler.Term, row LogRow) (LogRow, error) {
	var out LogRow
	err := s.write(ctx, term, "log", func(tx pgx.Tx) error {
		var db *string
		if row.Database != "" {
			db = &row.Database
		}
		_, err := tx.Exec(ctx, `INSERT INTO kingres_cp.ledger_log (kind, database_name, shard_group_id, chain_id, epoch, origin, state, size, root, frontier)
			VALUES ($1, $2, $3, $4, $5, $6, 'active', 0, $7, $8) ON CONFLICT (origin) DO NOTHING`,
			row.Kind, db, row.ShardGroupID, row.ChainID, row.Epoch, row.Origin, emptyRoot(), ledgerlog.Frontier{}.Marshal())
		if err != nil {
			return conflictCode(err)
		}
		out, err = scanLog(tx.QueryRow(ctx, `SELECT `+logCols+` FROM kingres_cp.ledger_log WHERE origin = $1`, row.Origin))
		return err
	})
	return out, err
}

// ---- tiles ----

type pgTiles struct {
	s    PGStore
	id   int64
	term scheduler.Term
}

// Tiles implements Store.
func (s PGStore) Tiles(logID int64, term scheduler.Term) ledgerlog.Store {
	return pgTiles{s: s, id: logID, term: term}
}

func checkTile(k ledgerlog.TileKey, data []byte) error {
	if !k.Valid() {
		return fmt.Errorf("ledgerpub: %v is not a tile", k)
	}
	if k.Level >= 0 {
		_, err := ledgerlog.ParseTile(data, k.Width)
		return err
	}
	_, err := ledgerlog.ParseBundle(data, k.Width)
	return err
}

func (t pgTiles) GetTile(ctx context.Context, k ledgerlog.TileKey) ([]byte, error) {
	var data []byte
	err := t.s.Sys(ctx, func(c *pgx.Conn) error {
		return c.QueryRow(ctx, `SELECT data FROM kingres_cp.ledger_tile WHERE log_id = $1 AND level = $2 AND idx = $3 AND width = $4`,
			t.id, k.Level, int64(k.Index), k.Width).Scan(&data) //nolint:gosec // an index below 2^56
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ledgerlog.ErrNotFound, k.Path())
	}
	return data, err
}

func (t pgTiles) PutTile(ctx context.Context, k ledgerlog.TileKey, data []byte) error {
	if err := checkTile(k, data); err != nil {
		return err
	}
	return t.s.write(ctx, t.term, "tile", func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO kingres_cp.ledger_tile (log_id, level, idx, width, data) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (log_id, level, idx, width) DO NOTHING`, t.id, k.Level, int64(k.Index), k.Width, data) //nolint:gosec // an index below 2^56
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			return nil
		}
		var old []byte
		if err := tx.QueryRow(ctx, `SELECT data FROM kingres_cp.ledger_tile WHERE log_id = $1 AND level = $2 AND idx = $3 AND width = $4`,
			t.id, k.Level, int64(k.Index), k.Width).Scan(&old); err != nil { //nolint:gosec // an index below 2^56
			return err
		}
		if !bytes.Equal(old, data) {
			return fmt.Errorf("%w: %s", ledgerlog.ErrConflict, k.Path())
		}
		return nil
	})
}

func (t pgTiles) Widths(ctx context.Context, level int, index uint64) ([]int, error) {
	var out []int
	err := t.s.Sys(ctx, func(c *pgx.Conn) error {
		rows, err := c.Query(ctx, `SELECT width FROM kingres_cp.ledger_tile WHERE log_id = $1 AND level = $2 AND idx = $3 ORDER BY width`, t.id, level, int64(index)) //nolint:gosec // below 2^56
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var w int
			if err := rows.Scan(&w); err != nil {
				return err
			}
			out = append(out, w)
		}
		return rows.Err()
	})
	return out, err
}

// ---- frontier, checkpoints, state ----

// SaveFrontier implements Store.
func (s PGStore) SaveFrontier(ctx context.Context, term scheduler.Term, logID, size int64, root, frontier, head []byte) error {
	return s.write(ctx, term, "frontier", func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE kingres_cp.ledger_log SET size = $2, root = $3, frontier = $4, head_block_hash = coalesce($5, head_block_hash),
			pending_entry = NULL, updated_at = now()
			WHERE log_id = $1 AND state = 'active' AND (size < $2 OR (size = $2 AND root = $3))`, logID, size, root, frontier, head)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("%w: the frontier is not an extension of the stored one, or the log is not active", ErrConflict)
		}
		return nil
	})
}

// PutPending implements Store.
func (s PGStore) PutPending(ctx context.Context, term scheduler.Term, logID int64, entry []byte) error {
	return s.write(ctx, term, "pending", func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE kingres_cp.ledger_log SET pending_entry = $2 WHERE log_id = $1 AND state = 'active'`, logID, entry)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrNotFound
		}
		return nil
	})
}

// PutCheckpoint implements Store.
func (s PGStore) PutCheckpoint(ctx context.Context, term scheduler.Term, c Checkpoint, rev int64, markFirst bool, prefix string) error {
	return s.write(ctx, term, "checkpoint", func(tx pgx.Tx) (err error) {
		defer func() { err = conflictCode(err) }()
		var have int64
		if err := tx.QueryRow(ctx, `SELECT keys_revision FROM kingres_cp.ledger_cluster`).Scan(&have); err != nil {
			return err
		}
		if have != rev {
			return ErrKeysChanged
		}
		var size int64
		var root []byte
		var state string
		if err := tx.QueryRow(ctx, `SELECT size, root, state FROM kingres_cp.ledger_log WHERE log_id = $1 FOR UPDATE`, c.LogID).Scan(&size, &root, &state); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if state != StateActive || size != c.Size || !bytes.Equal(root, c.Root) {
			return fmt.Errorf("%w: the note is not for the stored tree of the log", ErrConflict)
		}
		var oldRoot []byte
		switch err := tx.QueryRow(ctx, `SELECT root FROM kingres_cp.ledger_checkpoint WHERE log_id = $1 AND size = $2`, c.LogID, c.Size).Scan(&oldRoot); {
		case err == nil:
			if !bytes.Equal(oldRoot, c.Root) {
				return fmt.Errorf("%w: another root for size %d", ErrConflict, c.Size)
			}
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		var max int64
		if err := tx.QueryRow(ctx, `SELECT coalesce(max(size), 0) FROM kingres_cp.ledger_checkpoint WHERE log_id = $1`, c.LogID).Scan(&max); err != nil {
			return err
		}
		if max > c.Size {
			return fmt.Errorf("%w: a smaller tree than one signed", ErrConflict)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO kingres_cp.ledger_checkpoint (log_id, size, root, note, signed_at) VALUES ($1, $2, $3, $4, $5)`,
			c.LogID, c.Size, c.Root, c.Note, c.SignedAt.UTC()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE kingres_cp.ledger_log SET signed_at = $2 WHERE log_id = $1`, c.LogID, c.SignedAt.UTC()); err != nil {
			return err
		}
		if markFirst {
			if _, err := tx.Exec(ctx, `UPDATE kingres_cp.ledger_cluster SET first_checkpoint_at = now(), origin_prefix = coalesce(origin_prefix, $1)
				WHERE first_checkpoint_at IS NULL`, prefix); err != nil {
				return err
			}
		}
		return nil
	})
}

// ReplaceNote implements Store.
func (s PGStore) ReplaceNote(ctx context.Context, term scheduler.Term, logID, size int64, note []byte, keysRevision int64) error {
	return s.write(ctx, term, "note", func(tx pgx.Tx) error {
		var have int64
		if err := tx.QueryRow(ctx, `SELECT keys_revision FROM kingres_cp.ledger_cluster`).Scan(&have); err != nil {
			return err
		}
		if have != keysRevision {
			return ErrKeysChanged
		}
		var old []byte
		if err := tx.QueryRow(ctx, `SELECT note FROM kingres_cp.ledger_checkpoint WHERE log_id = $1 AND size = $2 FOR UPDATE`, logID, size).Scan(&old); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if err := sameText(old, note); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE kingres_cp.ledger_checkpoint SET note = $3 WHERE log_id = $1 AND size = $2`, logID, size, note)
		return err
	})
}

func scanCheckpoint(r pgx.Row) (Checkpoint, error) {
	var c Checkpoint
	err := r.Scan(&c.LogID, &c.Size, &c.Root, &c.Note, &c.SignedAt)
	c.SignedAt = c.SignedAt.UTC()
	return c, err
}

// Checkpoint implements Store.
func (s PGStore) Checkpoint(ctx context.Context, logID, size int64) (Checkpoint, error) {
	var c Checkpoint
	err := s.Sys(ctx, func(conn *pgx.Conn) error {
		var err error
		c, err = scanCheckpoint(conn.QueryRow(ctx, `SELECT log_id, size, root, note, signed_at FROM kingres_cp.ledger_checkpoint WHERE log_id = $1 AND size = $2`, logID, size))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// LatestCheckpoint implements Store.
func (s PGStore) LatestCheckpoint(ctx context.Context, logID int64) (Checkpoint, error) {
	var c Checkpoint
	err := s.Sys(ctx, func(conn *pgx.Conn) error {
		var err error
		c, err = scanCheckpoint(conn.QueryRow(ctx, `SELECT log_id, size, root, note, signed_at FROM kingres_cp.ledger_checkpoint WHERE log_id = $1 ORDER BY size DESC LIMIT 1`, logID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// Checkpoints implements Store.
func (s PGStore) Checkpoints(ctx context.Context, logID, before int64, limit int) ([]Checkpoint, error) {
	var out []Checkpoint
	err := s.Sys(ctx, func(conn *pgx.Conn) error {
		rows, err := conn.Query(ctx, `SELECT log_id, size, root, note, signed_at FROM kingres_cp.ledger_checkpoint
			WHERE log_id = $1 AND ($2 = 0 OR size < $2) ORDER BY size DESC LIMIT $3`, logID, before, limitOrAll(limit))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanCheckpoint(rows)
			if err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

func limitOrAll(n int) int {
	if n <= 0 {
		return 1 << 30
	}
	return n
}

// SetState implements Store.
func (s PGStore) SetState(ctx context.Context, term scheduler.Term, logID int64, state, detail string) error {
	return s.write(ctx, term, "state", func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE kingres_cp.ledger_log SET state = $2, detail = $3, updated_at = now(),
			closed_at = CASE WHEN $2 = 'closed' AND closed_at IS NULL THEN now() ELSE closed_at END
			WHERE log_id = $1 AND (state <> 'closed' OR $2 = 'closed')`, logID, state, detail)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("%w: no such log, or it is closed", ErrConflict)
		}
		return nil
	})
}

// SetRetired implements Store.
func (s PGStore) SetRetired(ctx context.Context, term scheduler.Term, logID, seq int64) error {
	return s.write(ctx, term, "retired", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE kingres_cp.ledger_log SET retired_in_seq = $2 WHERE log_id = $1`, logID, seq)
		return err
	})
}

// SetUnread implements Store.
func (s PGStore) SetUnread(ctx context.Context, term scheduler.Term, logID int64, since *time.Time) error {
	return s.write(ctx, term, "unread", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE kingres_cp.ledger_log SET unread_since = $2 WHERE log_id = $1`, logID, since)
		return err
	})
}

// ---- digests ----

// PutDigest implements Store.
func (s PGStore) PutDigest(ctx context.Context, term scheduler.Term, v DigestView) (int64, error) {
	var id int64
	err := s.write(ctx, term, "digest", func(tx pgx.Tx) error {
		unc := v.Uncovered
		if unc == nil {
			unc = []int32{}
		}
		return tx.QueryRow(ctx, `INSERT INTO kingres_cp.ledger_digest_view (database_name, generated_at, generated_by, hash_version, chains, complete,
			uncovered, aggregate_size, aggregate_hash) VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8, $9) RETURNING digest_id`,
			v.Database, v.GeneratedAt.UTC(), v.GeneratedBy, v.HashVersion, json.RawMessage(v.Chains), v.Complete, unc, v.AggregateSize, v.AggregateHash).Scan(&id)
	})
	return id, err
}

const digestCols = `digest_id, database_name, generated_at, generated_by, hash_version, chains, complete, uncovered, aggregate_size, aggregate_hash`

func scanDigest(r pgx.Row) (DigestView, error) {
	var v DigestView
	var chains []byte
	err := r.Scan(&v.ID, &v.Database, &v.GeneratedAt, &v.GeneratedBy, &v.HashVersion, &chains, &v.Complete, &v.Uncovered, &v.AggregateSize, &v.AggregateHash)
	v.Chains = chains
	v.GeneratedAt = v.GeneratedAt.UTC()
	return v, err
}

// Digest implements Store.
func (s PGStore) Digest(ctx context.Context, database string, id int64) (DigestView, error) {
	var v DigestView
	err := s.Sys(ctx, func(c *pgx.Conn) error {
		var err error
		v, err = scanDigest(c.QueryRow(ctx, `SELECT `+digestCols+` FROM kingres_cp.ledger_digest_view WHERE database_name = $1 AND digest_id = $2`, database, id))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return v, ErrNotFound
	}
	return v, err
}

// Digests implements Store.
func (s PGStore) Digests(ctx context.Context, database string, before int64, limit int) ([]DigestView, error) {
	var out []DigestView
	err := s.Sys(ctx, func(c *pgx.Conn) error {
		rows, err := c.Query(ctx, `SELECT `+digestCols+` FROM kingres_cp.ledger_digest_view WHERE database_name = $1 AND ($2 = 0 OR digest_id < $2)
			ORDER BY digest_id DESC LIMIT $3`, database, before, limitOrAll(limit))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			v, err := scanDigest(rows)
			if err != nil {
				return err
			}
			out = append(out, v)
		}
		return rows.Err()
	})
	return out, err
}

// LastDigestAt implements Store.
func (s PGStore) LastDigestAt(ctx context.Context, database string) (time.Time, error) {
	var t *time.Time
	err := s.Sys(ctx, func(c *pgx.Conn) error {
		return c.QueryRow(ctx, `SELECT max(generated_at) FROM kingres_cp.ledger_digest_view WHERE database_name = $1`, database).Scan(&t)
	})
	if t == nil {
		return time.Time{}, err
	}
	return t.UTC(), err
}

// Object implements Store.
func (s PGStore) Object(ctx context.Context, origin, rest string) (Object, error) {
	if origin == "" {
		switch {
		case rest == "logs.json":
			logs, err := s.Logs(ctx)
			if err != nil {
				return Object{}, err
			}
			return Object{Data: LogsJSON(logs), Mutable: true, ContentType: "application/json"}, nil
		case rest == "keyring.json":
			var d []byte
			err := s.Sys(ctx, func(c *pgx.Conn) error {
				return c.QueryRow(ctx, `SELECT convert_to(trust_file, 'UTF8') FROM kingres_cp.ledger_cluster`).Scan(&d)
			})
			if err != nil {
				return Object{}, err
			}
			if d == nil {
				return Object{}, ErrNotFound
			}
			return Object{Data: d, Mutable: true, ContentType: "application/json"}, nil
		case strings.HasPrefix(rest, "events/"):
			n, err := strconv.ParseInt(strings.TrimPrefix(rest, "events/"), 10, 64)
			if err != nil {
				return Object{}, ErrNotFound
			}
			var d []byte
			err = s.Sys(ctx, func(c *pgx.Conn) error {
				return c.QueryRow(ctx, `SELECT note FROM kingres_cp.ledger_event WHERE seq = $1`, n).Scan(&d)
			})
			if errors.Is(err, pgx.ErrNoRows) {
				return Object{}, ErrNotFound
			}
			if err != nil {
				return Object{}, err
			}
			return Object{Data: d, ContentType: "text/plain; charset=utf-8"}, nil
		}
		return Object{}, ErrNotFound
	}
	row, err := s.LogByOrigin(ctx, origin)
	if err != nil {
		return Object{}, err
	}
	r, err := ParseRest(rest)
	if err != nil {
		return Object{}, ErrNotFound
	}
	if r.Checkpoint {
		c, err := s.LatestCheckpoint(ctx, row.ID)
		if err != nil {
			return Object{}, err
		}
		return Object{Data: c.Note, Mutable: true, ContentType: "text/plain; charset=utf-8"}, nil
	}
	d, err := pgTiles{s: s, id: row.ID}.GetTile(ctx, r.Tile)
	if err != nil {
		if errors.Is(err, ledgerlog.ErrNotFound) {
			return Object{}, ErrNotFound
		}
		return Object{}, err
	}
	return Object{Data: d, ContentType: "application/octet-stream"}, nil
}

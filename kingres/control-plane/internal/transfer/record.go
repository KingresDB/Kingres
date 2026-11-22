// SPDX-License-Identifier: AGPL-3.0-only

package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// Kinds and states of a staged file (kingres_cp.data_transfer, migration 0035).
const (
	KindTableExport    = "tableExport"
	KindDatabaseExport = "databaseExport"
	KindUpload         = "upload"

	StateReceiving = "receiving"
	StateBuilding  = "building"
	StateReady     = "ready"
	StateFailed    = "failed"
	StateDeleted   = "deleted"
	StateExpired   = "expired"
	StateLost      = "lost"
)

// ErrNotFound is returned for a record that does not exist or that another role owns: the two are not told apart, so that the existence
// of a file is not leaked.
var ErrNotFound = errors.New("transfer: no such transfer")

// Identity names the role that owns a staged file: by name and by OID, so that a role that was dropped and made again under the same
// name does not inherit the files of the old one (DXR-23).
type Identity struct {
	Name string
	OID  int64
}

// Record is one row of kingres_cp.data_transfer.
type Record struct {
	ID            int64
	Kind          string
	State         string
	ServerID      string
	Owner         Identity
	Database      string
	Schema        string
	Table         string
	SizeBytes     *int64
	DeclaredBytes *int64
	ReservedBytes int64
	SHA256        string
	Format        string
	Report        json.RawMessage
	KeyID         string
	WrappedKey    []byte
	CreatedAt     time.Time
	UpdatedAt     time.Time
	ExpiresAt     time.Time
	DownloadedAt  *time.Time
	TaskID        *int64
}

// IDString is the identifier as the contract shows it, a decimal string.
func (r Record) IDString() string { return strconv.FormatInt(r.ID, 10) }

// Tx runs f in a transaction of the service role on the system database. The operations of the control plane have it (Ops.asService).
type Tx func(ctx context.Context, f func(tx pgx.Tx) error) error

// Records are the accessors of the table of staged files.
type Records struct{ Run Tx }

const recordColumns = `transfer_id, kind, state, server_id, created_by, created_by_oid,
	coalesce(database_name, ''), coalesce(schema_name, ''), coalesce(table_name, ''),
	size_bytes, declared_bytes, reserved_bytes, coalesce(sha256, ''), coalesce(format, ''), report,
	coalesce(key_id, ''), wrapped_key, created_at, updated_at, expires_at, downloaded_at, task_id`

func scanRecord(row pgx.Row) (Record, error) {
	var r Record
	err := row.Scan(&r.ID, &r.Kind, &r.State, &r.ServerID, &r.Owner.Name, &r.Owner.OID,
		&r.Database, &r.Schema, &r.Table,
		&r.SizeBytes, &r.DeclaredBytes, &r.ReservedBytes, &r.SHA256, &r.Format, &r.Report,
		&r.KeyID, &r.WrappedKey, &r.CreatedAt, &r.UpdatedAt, &r.ExpiresAt, &r.DownloadedAt, &r.TaskID)
	r.CreatedAt, r.UpdatedAt, r.ExpiresAt = r.CreatedAt.UTC(), r.UpdatedAt.UTC(), r.ExpiresAt.UTC()
	if r.DownloadedAt != nil {
		t := r.DownloadedAt.UTC()
		r.DownloadedAt = &t
	}
	return r, err
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Create inserts a record and returns its id. The owner must have a name and an OID: a file without an owner could be read by anyone.
func (rs Records) Create(ctx context.Context, r Record) (int64, error) {
	if r.Owner.Name == "" || r.Owner.OID == 0 {
		return 0, errors.New("transfer: a record needs an owner")
	}
	if r.ExpiresAt.IsZero() {
		return 0, errors.New("transfer: a record needs an expiry")
	}
	var id int64
	err := rs.Run(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO kingres_cp.data_transfer
			(kind, state, server_id, created_by, created_by_oid, database_name, schema_name, table_name, declared_bytes, reserved_bytes, format, expires_at, task_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13) RETURNING transfer_id`,
			r.Kind, r.State, r.ServerID, r.Owner.Name, r.Owner.OID, nullable(r.Database), nullable(r.Schema), nullable(r.Table),
			r.DeclaredBytes, r.ReservedBytes, nullable(r.Format), r.ExpiresAt, r.TaskID).Scan(&id)
	})
	return id, err
}

// Get reads a record whoever owns it (the janitor, the leader's own steps).
func (rs Records) Get(ctx context.Context, id int64) (Record, error) {
	var out Record
	err := rs.Run(ctx, func(tx pgx.Tx) error {
		var e error
		out, e = scanRecord(tx.QueryRow(ctx, `SELECT `+recordColumns+` FROM kingres_cp.data_transfer WHERE transfer_id = $1`, id))
		return e
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	return out, err
}

// GetOwned reads a record of the given owner; another owner's record is ErrNotFound. A record that was deleted is not found either.
func (rs Records) GetOwned(ctx context.Context, id int64, who Identity) (Record, error) {
	r, err := rs.Get(ctx, id)
	if err != nil {
		return Record{}, err
	}
	if r.Owner != who || r.State == StateDeleted {
		return Record{}, ErrNotFound
	}
	return r, nil
}

// List returns the files of one owner and kind, newest first, with ids below before (0: from the newest). It never returns a deleted
// record. limit is at most 500.
func (rs Records) List(ctx context.Context, who Identity, kinds []string, before int64, limit int) ([]Record, error) {
	if limit < 1 || limit > 500 {
		limit = 50
	}
	if before <= 0 {
		before = 1<<62 - 1
	}
	var out []Record
	err := rs.Run(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+recordColumns+` FROM kingres_cp.data_transfer
			WHERE created_by = $1 AND created_by_oid = $2 AND kind = ANY($3) AND state <> 'deleted' AND transfer_id < $4
			ORDER BY transfer_id DESC LIMIT $5`, who.Name, who.OID, kinds, before, limit)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Record, error) { return scanRecord(r) })
		return err
	})
	return out, err
}

// Update is a change of a record. A nil member is left as it is.
type Update struct {
	State         *string
	SizeBytes     *int64
	ReservedBytes *int64
	SHA256        *string
	Format        *string
	Report        json.RawMessage
	KeyID         *string
	WrappedKey    []byte
	ExpiresAt     *time.Time
	TaskID        *int64
	// Touch sets updated_at (bytes arrived).
	Touch bool
	// Downloaded sets downloaded_at (the last byte of a download was served).
	Downloaded bool
}

// Apply changes the record. A record in the state deleted is never changed back: the file is gone.
func (rs Records) Apply(ctx context.Context, id int64, u Update) error {
	var n int64
	err := rs.Run(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE kingres_cp.data_transfer SET
			state = coalesce($2, state), size_bytes = coalesce($3, size_bytes), reserved_bytes = coalesce($4, reserved_bytes),
			sha256 = coalesce($5, sha256), format = coalesce($6, format), report = coalesce($7, report),
			key_id = coalesce($8, key_id), wrapped_key = coalesce($9, wrapped_key), expires_at = coalesce($10, expires_at),
			task_id = coalesce($11, task_id),
			updated_at = CASE WHEN $12 THEN now() ELSE updated_at END,
			downloaded_at = CASE WHEN $13 THEN now() ELSE downloaded_at END
			WHERE transfer_id = $1 AND state <> 'deleted'`,
			id, u.State, u.SizeBytes, u.ReservedBytes, u.SHA256, u.Format, nilIfEmpty(u.Report), u.KeyID, nilIfEmptyBytes(u.WrappedKey), u.ExpiresAt, u.TaskID, u.Touch, u.Downloaded)
		n = tag.RowsAffected()
		return err
	})
	if err == nil && n == 0 {
		return ErrNotFound
	}
	return err
}

func nilIfEmpty(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return []byte(b)
}

func nilIfEmptyBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// MaxID is the highest transfer id the system database knows; the janitor refuses to delete files when the files show a higher one (a sign
// of a restored system database). 0 when there is no record.
func (rs Records) MaxID(ctx context.Context) (int64, error) {
	var id int64
	err := rs.Run(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT coalesce(max(transfer_id), 0) FROM kingres_cp.data_transfer`).Scan(&id)
	})
	return id, err
}

// OnServer lists the records of this server that hold a file or may do: the states receiving, building and ready.
func (rs Records) OnServer(ctx context.Context, server string) ([]Record, error) {
	var out []Record
	err := rs.Run(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+recordColumns+` FROM kingres_cp.data_transfer
			WHERE server_id = $1 AND state IN ('receiving', 'building', 'ready') ORDER BY transfer_id`, server)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Record, error) { return scanRecord(r) })
		return err
	})
	return out, err
}

// Reserved is the sum of the reservations of the live files of the cluster, and the number of the live files of one owner (the quota and
// the limits per role).
func (rs Records) Reserved(ctx context.Context) (total int64, err error) {
	err = rs.Run(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT coalesce(sum(reserved_bytes), 0)::bigint FROM kingres_cp.data_transfer WHERE state IN ('receiving', 'building', 'ready')`).Scan(&total)
	})
	return total, err
}

// SPDX-License-Identifier: PostgreSQL

package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DB is the subset of pgx that PGStore needs; *pgxpool.Pool and *pgx.Conn
// satisfy it.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// PGStore keeps tasks in kingres_cp.task of the system database.
type PGStore struct{ db DB }

// NewPGStore returns a store on db, a connection to kingres_system.
func NewPGStore(db DB) *PGStore { return &PGStore{db: db} }

const cols = `id, kind, state, subject_type, subject_id, input, progress, error, cancel_requested,
	created_at, updated_at, created_by, coalesce(lease_holder, ''), coalesce(lease_expires_at, 'epoch')`

type progressJSON struct {
	Detail  string      `json:"detail,omitempty"`
	Percent float64     `json:"percent"`
	Steps   []StepState `json:"steps"`
}

func scan(row pgx.Row) (Task, error) {
	var t Task
	var state string
	var prog, errj []byte
	err := row.Scan(&t.ID, &t.Kind, &state, &t.SubjectType, &t.SubjectID, &t.Input, &prog, &errj,
		&t.CancelRequested, &t.CreatedAt, &t.UpdatedAt, &t.CreatedBy, &t.Holder, &t.LeaseExpires)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Task{}, ErrNotFound
		}
		return Task{}, err
	}
	t.State = State(state)
	var p progressJSON
	if err := json.Unmarshal(prog, &p); err != nil {
		return Task{}, fmt.Errorf("task %d: progress: %w", t.ID, err)
	}
	t.Percent, t.Steps, t.Detail = p.Percent, p.Steps, p.Detail
	if len(errj) > 0 {
		var e Error
		if err := json.Unmarshal(errj, &e); err == nil {
			t.Error = &e
		}
	}
	return t, nil
}

// Create implements Store.
func (s *PGStore) Create(ctx context.Context, t Task) (Task, error) {
	prog, _ := json.Marshal(progressJSON{Steps: t.Steps})
	row := s.db.QueryRow(ctx, `INSERT INTO kingres_cp.task
		(kind, state, subject_type, subject_id, input, progress, created_at, updated_at, created_by)
		VALUES ($1, 'pending', $2, $3, $4, $5, $6, $6, $7) RETURNING `+cols,
		t.Kind, t.SubjectType, t.SubjectID, []byte(t.Input), prog, t.CreatedAt, t.CreatedBy)
	out, err := scan(row)
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return Task{}, ErrSubjectBusy
	}
	return out, err
}

// Get implements Store.
func (s *PGStore) Get(ctx context.Context, id int64) (Task, error) {
	return scan(s.db.QueryRow(ctx, `SELECT `+cols+` FROM kingres_cp.task WHERE id = $1`, id))
}

const claimSQL = `UPDATE kingres_cp.task SET state = 'running', lease_holder = $1, lease_expires_at = $2, updated_at = $3
	WHERE id = (SELECT id FROM kingres_cp.task
	            WHERE %s AND (state = 'pending' OR (state = 'running' AND lease_expires_at <= $3))
	            ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED)
	RETURNING ` + cols

// Claim implements Store.
func (s *PGStore) Claim(ctx context.Context, holder string, now time.Time, lease time.Duration) (*Task, error) {
	t, err := scan(s.db.QueryRow(ctx, fmt.Sprintf(claimSQL, "true"), holder, now.Add(lease), now))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return &t, err
}

// ClaimID implements Store.
func (s *PGStore) ClaimID(ctx context.Context, id int64, holder string, now time.Time, lease time.Duration) (*Task, error) {
	t, err := scan(s.db.QueryRow(ctx, fmt.Sprintf(claimSQL, "id = $4"), holder, now.Add(lease), now, id))
	if errors.Is(err, ErrNotFound) {
		if _, gerr := s.Get(ctx, id); gerr != nil {
			return nil, gerr
		}
		return nil, nil
	}
	return &t, err
}

func (s *PGStore) heldOrErr(ctx context.Context, tag pgconn.CommandTag, id int64) error {
	if tag.RowsAffected() == 1 {
		return nil
	}
	if _, err := s.Get(ctx, id); err != nil {
		return err
	}
	return ErrLeaseLost
}

// Renew implements Store.
func (s *PGStore) Renew(ctx context.Context, id int64, holder string, now time.Time, lease time.Duration) error {
	tag, err := s.db.Exec(ctx, `UPDATE kingres_cp.task SET lease_expires_at = $3
		WHERE id = $1 AND lease_holder = $2 AND state = 'running'`, id, holder, now.Add(lease))
	if err != nil {
		return err
	}
	return s.heldOrErr(ctx, tag, id)
}

// SetStep implements Store.
func (s *PGStore) SetStep(ctx context.Context, id int64, holder string, i int, state string, pct float64, now time.Time) (bool, error) {
	var cancel bool
	err := s.db.QueryRow(ctx, `UPDATE kingres_cp.task SET
		progress = jsonb_set(jsonb_set(progress, ARRAY['steps', $3::text, 'state'], to_jsonb($4::text)), '{percent}', to_jsonb($5::float8)),
		updated_at = $6
		WHERE id = $1 AND lease_holder = $2 AND state = 'running' RETURNING cancel_requested`,
		id, holder, strconv.Itoa(i), state, pct, now).Scan(&cancel)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, gerr := s.Get(ctx, id); gerr != nil {
			return false, gerr
		}
		return false, ErrLeaseLost
	}
	return cancel, err
}

// Finish implements Store.
func (s *PGStore) Finish(ctx context.Context, id int64, holder string, state State, e *Error, now time.Time) error {
	var ej []byte
	if e != nil {
		ej, _ = json.Marshal(e)
	}
	tag, err := s.db.Exec(ctx, `UPDATE kingres_cp.task SET state = $3, error = $4, updated_at = $5,
		progress = CASE WHEN $3 = 'done' THEN jsonb_set(progress, '{percent}', '100') ELSE progress END,
		lease_holder = NULL, lease_expires_at = NULL
		WHERE id = $1 AND lease_holder = $2 AND state = 'running'`, id, holder, string(state), ej, now)
	if err != nil {
		return err
	}
	return s.heldOrErr(ctx, tag, id)
}

// RequestCancel implements Store.
func (s *PGStore) RequestCancel(ctx context.Context, id int64, now time.Time) error {
	t, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	switch t.State {
	case Done, Failed:
		return ErrNotCancellable
	case Pending:
		ej, _ := json.Marshal(Error{Code: CodeCanceled, Message: "The task was canceled before it started."})
		tag, err := s.db.Exec(ctx, `UPDATE kingres_cp.task SET state = 'failed', error = $2, updated_at = $3
			WHERE id = $1 AND state = 'pending'`, id, ej, now)
		if err == nil && tag.RowsAffected() == 0 {
			// It started meanwhile: ask it to stop instead.
			_, err = s.db.Exec(ctx, `UPDATE kingres_cp.task SET cancel_requested = true WHERE id = $1`, id)
		}
		return err
	default:
		_, err := s.db.Exec(ctx, `UPDATE kingres_cp.task SET cancel_requested = true WHERE id = $1`, id)
		return err
	}
}

// List implements Store.
func (s *PGStore) List(ctx context.Context, limit int) ([]Task, error) {
	rows, err := s.db.Query(ctx, `SELECT `+cols+` FROM kingres_cp.task ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SetDetail implements DetailStore.
func (s *PGStore) SetDetail(ctx context.Context, id int64, holder, detail string) error {
	tag, err := s.db.Exec(ctx, `UPDATE kingres_cp.task SET progress = jsonb_set(progress, '{detail}', to_jsonb($3::text))
		WHERE id = $1 AND lease_holder = $2 AND state = 'running'`, id, holder, detail)
	if err != nil {
		return err
	}
	return s.heldOrErr(ctx, tag, id)
}

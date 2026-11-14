// SPDX-License-Identifier: PostgreSQL

package pgstore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
)

// The task table is not specified yet by the design notes; the contract says
// the id is "task_id in kingres.task". The columns read here are the
// ones the contract's Task schema needs, and are documented in the README as
// an assumption for the task engine to honor:
//
//	kingres.task(task_id bigint PK, kind text, state text, progress jsonb,
//	              error jsonb, subject jsonb, cancellable bool,
//	              created_at timestamptz, updated_at timestamptz, created_by text)
const taskColumns = `task_id, kind, state, progress, error, subject, cancellable, created_at, updated_at, created_by`

type taskRow struct {
	id          int64
	kind, state string
	progress    []byte
	taskErr     []byte
	subject     []byte
	cancellable bool
	createdAt   time.Time
	updatedAt   time.Time
	createdBy   string
}

func scanTask(r pgx.CollectableRow) (taskRow, error) {
	var t taskRow
	err := r.Scan(&t.id, &t.kind, &t.state, &t.progress, &t.taskErr, &t.subject, &t.cancellable, &t.createdAt, &t.updatedAt, &t.createdBy)
	return t, err
}

func (t taskRow) toAPI() (api.Task, error) {
	out := api.Task{
		ID: t.id, Kind: t.kind, State: t.state, Cancellable: t.cancellable,
		CreatedAt: t.createdAt.UTC(), UpdatedAt: t.updatedAt.UTC(), CreatedBy: t.createdBy,
		Progress: api.TaskProgress{Steps: []api.TaskStep{}},
	}
	if len(t.progress) > 0 {
		if err := json.Unmarshal(t.progress, &out.Progress); err != nil {
			return api.Task{}, fmt.Errorf("decoding progress of task %d: %w", t.id, err)
		}
		if out.Progress.Steps == nil {
			out.Progress.Steps = []api.TaskStep{}
		}
	}
	if len(t.taskErr) > 0 && string(t.taskErr) != "null" {
		out.Error = new(api.TaskError)
		if err := json.Unmarshal(t.taskErr, out.Error); err != nil {
			return api.Task{}, fmt.Errorf("decoding error of task %d: %w", t.id, err)
		}
	}
	if len(t.subject) > 0 && string(t.subject) != "null" {
		out.Subject = new(api.TaskSubject)
		if err := json.Unmarshal(t.subject, out.Subject); err != nil {
			return api.Task{}, fmt.Errorf("decoding subject of task %d: %w", t.id, err)
		}
	}
	return out, nil
}

// Tasks implements store.Store. Tasks are listed newest first with a keyset
// cursor on task_id; without kingres.task the list is empty.
func (s *Store) Tasks(ctx context.Context, f store.TaskFilter, p store.Page) (api.TaskPage, error) {
	empty := api.TaskPage{Items: []api.Task{}}
	before, bounded, err := store.DecodeKey(p.Cursor)
	if err != nil {
		return empty, err
	}
	pool, err := s.pool(ctx, "")
	if err != nil {
		return empty, err
	}
	ok, err := s.regclassExists(ctx, pool, "kingres.task")
	if err != nil || !ok {
		return empty, err
	}
	var bound any
	if bounded {
		bound = before
	}
	rows, err := pool.Query(ctx, `SELECT `+taskColumns+` FROM kingres.task
		WHERE ($1::bigint IS NULL OR task_id < $1) AND ($2 = '' OR state = $2) AND ($3 = '' OR kind = $3)
		ORDER BY task_id DESC LIMIT $4`, bound, f.State, f.Kind, p.Limit+1)
	if err != nil {
		return empty, s.wrap("reading kingres.task", err)
	}
	raw, err := pgx.CollectRows(rows, scanTask)
	if err != nil {
		return empty, s.wrap("reading kingres.task", err)
	}
	page := api.TaskPage{Items: make([]api.Task, 0, len(raw))}
	if len(raw) > p.Limit {
		next := store.EncodeKey(raw[p.Limit-1].id)
		page.NextCursor = &next
		raw = raw[:p.Limit]
	}
	for _, r := range raw {
		t, err := r.toAPI()
		if err != nil {
			return empty, err
		}
		page.Items = append(page.Items, t)
	}
	return page, nil
}

// Task implements store.Store.
func (s *Store) Task(ctx context.Context, id int64) (api.Task, error) {
	pool, err := s.pool(ctx, "")
	if err != nil {
		return api.Task{}, err
	}
	ok, err := s.regclassExists(ctx, pool, "kingres.task")
	if err != nil {
		return api.Task{}, err
	}
	if !ok {
		return api.Task{}, store.ErrNotFound
	}
	rows, err := pool.Query(ctx, `SELECT `+taskColumns+` FROM kingres.task WHERE task_id = $1`, id)
	if err != nil {
		return api.Task{}, s.wrap("reading kingres.task", err)
	}
	raw, err := pgx.CollectRows(rows, scanTask)
	if err != nil {
		return api.Task{}, s.wrap("reading kingres.task", err)
	}
	if len(raw) == 0 {
		return api.Task{}, store.ErrNotFound
	}
	return raw[0].toAPI()
}

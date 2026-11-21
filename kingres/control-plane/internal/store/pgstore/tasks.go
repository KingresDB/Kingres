// SPDX-License-Identifier: AGPL-3.0-only

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

// Tasks live in kingres_cp.task of the system database (migration 1, see
// package sysdb); the database of the service DSN is the system database.
const taskColumns = `id, kind, state, subject_type, subject_id, progress, error, (state IN ('pending', 'running') AND kind NOT IN ('instance.start', 'instance.stop', 'instance.restart')
	-- the move of a schema (and the move first of its undistribution) can be stopped until the phase switching (internal/ops, schemaMoveCancellable)
	AND NOT (kind IN ('schema.move', 'schema.undistribute') AND (coalesce(progress->>'detail', '') ~ '^(switching|cleaning|done)'
		OR EXISTS (SELECT 1 FROM jsonb_array_elements(coalesce(progress->'steps', '[]'::jsonb)) s
			WHERE s->>'name' IN ('Switch', 'Clean up the old copy', 'Make the schema local') AND s->>'state' <> 'pending')))), created_at, updated_at, created_by`

type taskRow struct {
	id          int64
	kind, state string
	progress    []byte
	taskErr     []byte
	subjType    string
	subjID      string
	cancellable bool
	createdAt   time.Time
	updatedAt   time.Time
	createdBy   string
}

func scanTask(r pgx.CollectableRow) (taskRow, error) {
	var t taskRow
	err := r.Scan(&t.id, &t.kind, &t.state, &t.subjType, &t.subjID, &t.progress, &t.taskErr, &t.cancellable, &t.createdAt, &t.updatedAt, &t.createdBy)
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
	var extra struct {
		Detail string `json:"detail"`
	}
	if len(t.progress) > 0 {
		_ = json.Unmarshal(t.progress, &extra)
	}
	if extra.Detail != "" {
		d := extra.Detail
		out.Detail = &d
	}
	out.Subject = &api.TaskSubject{Type: t.subjType, ID: t.subjID}
	for i, st := range out.Progress.Steps {
		if st.State == "running" {
			n := st.Name
			out.Progress.CurrentStep = &n
			if out.Detail != nil {
				out.Progress.Steps[i].Detail = out.Detail
			}
		}
	}
	return out, nil
}

// Tasks implements store.Store. Tasks are listed newest first with a keyset
// cursor on task_id; without kingres_cp.task the list is empty.
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
	ok, err := s.regclassExists(ctx, pool, "kingres_cp.task")
	if err != nil || !ok {
		return empty, err
	}
	var bound any
	if bounded {
		bound = before
	}
	rows, err := pool.Query(ctx, `SELECT `+taskColumns+` FROM kingres_cp.task
		WHERE ($1::bigint IS NULL OR id < $1) AND ($2 = '' OR state = $2) AND ($3 = '' OR kind = $3)
		ORDER BY id DESC LIMIT $4`, bound, f.State, f.Kind, p.Limit+1)
	if err != nil {
		return empty, s.wrap("reading kingres_cp.task", err)
	}
	raw, err := pgx.CollectRows(rows, scanTask)
	if err != nil {
		return empty, s.wrap("reading kingres_cp.task", err)
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
	ok, err := s.regclassExists(ctx, pool, "kingres_cp.task")
	if err != nil {
		return api.Task{}, err
	}
	if !ok {
		return api.Task{}, store.ErrNotFound
	}
	rows, err := pool.Query(ctx, `SELECT `+taskColumns+` FROM kingres_cp.task WHERE id = $1 `, id)
	if err != nil {
		return api.Task{}, s.wrap("reading kingres_cp.task", err)
	}
	raw, err := pgx.CollectRows(rows, scanTask)
	if err != nil {
		return api.Task{}, s.wrap("reading kingres_cp.task", err)
	}
	if len(raw) == 0 {
		return api.Task{}, store.ErrNotFound
	}
	return raw[0].toAPI()
}

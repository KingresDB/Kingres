// SPDX-License-Identifier: PostgreSQL

package task

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Recording tasks that the engine does not run. The tasks of instance start and
// stop cannot be run by the engine's usual order (run a step, then record it):
// the system database that holds the task is hosted by the instance the task
// stops or starts, and on a follower there is no task table at all (the leader
// holds it). Their owner writes the task itself (Record), settles it when it is
// over (Settle) and, when the database cannot be written at the time, keeps the
// task in memory and writes it finished later under the id it announced
// (Record with an id).

// RecordLease is how long a recorded task that was not settled stays unclaimed.
// After it the engine claims it like any task of an interrupted leader; the
// definition of the kind then fails it (the server that recorded it died).
const RecordLease = 15 * time.Minute

// RecordStore is implemented by stores that can record and settle tasks.
type RecordStore interface {
	// Record inserts t as it is. A t.ID above zero is taken as the id (a task
	// announced earlier, or reserved with ReserveID); recording the same id again
	// changes nothing. ErrSubjectBusy when t is active and its subject already has
	// an active task.
	Record(ctx context.Context, t Task) (Task, error)
	// Settle sets state, steps, percent and error of the recorded task t.ID.
	Settle(ctx context.Context, t Task) error
	// ReserveID takes an id for a task recorded later; 0 when none is available.
	ReserveID(ctx context.Context) int64
}

// ErrUnsupportedStore means the store lacks an optional capability.
var ErrUnsupportedStore = errors.New("task: the store does not support recording tasks")

// Record writes a task that this engine does not run; see RecordStore.
func (e *Engine) Record(ctx context.Context, t Task) (Task, error) {
	rs, ok := e.store.(RecordStore)
	if !ok {
		return Task{}, ErrUnsupportedStore
	}
	now := e.now()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	if t.UpdatedAt.IsZero() {
		t.UpdatedAt = now
	}
	return rs.Record(ctx, t)
}

// Settle ends a recorded task, retrying while the store is briefly unavailable.
func (e *Engine) Settle(ctx context.Context, t Task) error {
	rs, ok := e.store.(RecordStore)
	if !ok {
		return ErrUnsupportedStore
	}
	t.UpdatedAt = e.now()
	return e.retry(ctx, func() error { return rs.Settle(ctx, t) })
}

// ReserveID takes an id for a task recorded later (0: none).
func (e *Engine) ReserveID(ctx context.Context) int64 {
	rs, ok := e.store.(RecordStore)
	if !ok {
		return 0
	}
	return rs.ReserveID(ctx)
}

func progressOf(t Task) ([]byte, error) {
	return json.Marshal(progressJSON{Detail: t.Detail, Percent: t.Percent, Steps: t.Steps})
}

func (t Task) active() bool { return t.State.Active() }

// Record implements RecordStore.
func (s *PGStore) Record(ctx context.Context, t Task) (Task, error) {
	if len(t.Input) == 0 {
		t.Input = json.RawMessage(`{}`)
	}
	prog, _ := progressOf(t)
	var ej []byte
	if t.Error != nil {
		ej, _ = json.Marshal(t.Error)
	}
	var holder any
	var leaseSecs any // the lease runs on the database's clock (see claimSQL)
	if t.active() {
		holder, leaseSecs = "recorder", RecordLease.Seconds()
	}
	var err error
	if t.ID > 0 {
		_, err = s.db.Exec(ctx, `INSERT INTO kingres_cp.task
			(id, kind, state, subject_type, subject_id, input, progress, error, created_at, updated_at, created_by, lease_holder, lease_expires_at)
			OVERRIDING SYSTEM VALUE
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, clock_timestamp() + make_interval(secs => $13::float8))
			ON CONFLICT (id) DO NOTHING`,
			t.ID, t.Kind, string(t.State), t.SubjectType, t.SubjectID, []byte(t.Input), prog, ej, t.CreatedAt, t.UpdatedAt, t.CreatedBy, holder, leaseSecs)
	} else {
		err = s.db.QueryRow(ctx, `INSERT INTO kingres_cp.task
			(kind, state, subject_type, subject_id, input, progress, error, created_at, updated_at, created_by, lease_holder, lease_expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, clock_timestamp() + make_interval(secs => $12::float8)) RETURNING id`,
			t.Kind, string(t.State), t.SubjectType, t.SubjectID, []byte(t.Input), prog, ej, t.CreatedAt, t.UpdatedAt, t.CreatedBy, holder, leaseSecs).Scan(&t.ID)
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return Task{}, ErrSubjectBusy
	}
	if err != nil {
		return Task{}, err
	}
	return s.Get(ctx, t.ID)
}

// Settle implements RecordStore.
func (s *PGStore) Settle(ctx context.Context, t Task) error {
	prog, _ := progressOf(t)
	var ej []byte
	if t.Error != nil {
		ej, _ = json.Marshal(t.Error)
	}
	leaseSecs := any(nil)
	holder := any(nil)
	if t.active() {
		holder, leaseSecs = "recorder", RecordLease.Seconds()
	}
	tag, err := s.db.Exec(ctx, `UPDATE kingres_cp.task SET state = $2, progress = $3, error = $4, updated_at = $5,
		lease_holder = $6, lease_expires_at = clock_timestamp() + make_interval(secs => $7::float8)
		WHERE id = $1 AND state IN ('running', 'done', 'failed')`, t.ID, string(t.State), prog, ej, t.UpdatedAt, holder, leaseSecs)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		if _, gerr := s.Get(ctx, t.ID); gerr != nil {
			return gerr
		}
	}
	return nil
}

// ReserveID implements RecordStore.
func (s *PGStore) ReserveID(ctx context.Context) int64 {
	var id int64
	if err := s.db.QueryRow(ctx, `SELECT nextval(pg_get_serial_sequence('kingres_cp.task', 'id'))`).Scan(&id); err != nil {
		return 0
	}
	return id
}

// Record implements RecordStore.
func (m *MemStore) Record(_ context.Context, t Task) (Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t.ID > 0 {
		if cur, ok := m.tasks[t.ID]; ok {
			return cp(cur), nil
		}
	}
	if t.active() {
		for _, o := range m.tasks {
			if o.State.Active() && o.SubjectType == t.SubjectType && o.SubjectID == t.SubjectID {
				return Task{}, ErrSubjectBusy
			}
		}
	}
	if t.ID == 0 {
		m.next++
		t.ID = m.next
	}
	c := cp(&t)
	m.tasks[t.ID] = &c
	return cp(&c), nil
}

// Settle implements RecordStore.
func (m *MemStore) Settle(_ context.Context, t Task) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.tasks[t.ID]
	if !ok {
		return ErrNotFound
	}
	cur.State, cur.Steps, cur.Percent, cur.Error, cur.UpdatedAt = t.State, append([]StepState(nil), t.Steps...), t.Percent, t.Error, t.UpdatedAt
	return nil
}

// ReserveID implements RecordStore.
func (m *MemStore) ReserveID(context.Context) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	return m.next
}

// recordedKinds are the kinds written by their owner and never run by the
// engine (see RecordStore); cancelling them is meaningless.
var recordedKinds = map[string]bool{"instance.start": true, "instance.stop": true, "instance.restart": true}

// Cancellable reports whether a cancel request is accepted for the task: it
// is not finished and the engine runs it.
func Cancellable(t Task) bool {
	return (t.State == Pending || t.State == Running) && !recordedKinds[t.Kind]
}

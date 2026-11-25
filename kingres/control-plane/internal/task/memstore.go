// SPDX-License-Identifier: AGPL-3.0-only

package task

import (
	"context"
	"sort"
	"sync"
	"time"
)

// MemStore is an in-memory Store with the same rules as the PostgreSQL one;
// the unit tests of the engine use it.
type MemStore struct {
	mu     sync.Mutex
	next   int64
	tasks  map[int64]*Task
	failOn func(op string) error // test hook
	// clock stands for the database's clock: every engine that shares the
	// store agrees on it, whatever its own clock says.
	clock func() time.Time
}

// NewMemStore returns an empty store.
func NewMemStore() *MemStore { return &MemStore{tasks: map[int64]*Task{}, clock: time.Now} }

// NewMemStoreClock returns an empty store whose lease arithmetic uses clock
// (the stand-in for the database clock in tests).
func NewMemStoreClock(clock func() time.Time) *MemStore {
	return &MemStore{tasks: map[int64]*Task{}, clock: clock}
}

// FailOn installs a hook that can make an operation fail (tests).
func (m *MemStore) FailOn(f func(op string) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failOn = f
}

func (m *MemStore) hook(op string) error {
	if m.failOn != nil {
		return m.failOn(op)
	}
	return nil
}

func cp(t *Task) Task {
	c := *t
	c.Steps = append([]StepState(nil), t.Steps...)
	if t.Error != nil {
		e := *t.Error
		c.Error = &e
	}
	return c
}

// ActiveFor implements ActiveStore.
func (m *MemStore) ActiveFor(_ context.Context, kind, subjType, subjID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.tasks {
		if t.Kind == kind && t.SubjectType == subjType && t.SubjectID == subjID && t.State.Active() {
			return true, nil
		}
	}
	return false, nil
}

// Create implements Store.
func (m *MemStore) Create(_ context.Context, t Task) (Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, o := range m.tasks {
		if o.State.Active() && o.SubjectType == t.SubjectType && o.SubjectID == t.SubjectID {
			return Task{}, ErrSubjectBusy
		}
	}
	if x := m.excludedLocked(t.Kind); x != nil {
		return Task{}, x
	}
	m.next++
	t.ID = m.next
	m.tasks[t.ID] = &t
	return cp(&t), nil
}

// Get implements Store.
func (m *MemStore) Get(_ context.Context, id int64) (Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return Task{}, ErrNotFound
	}
	return cp(t), nil
}

func claimable(t *Task, now time.Time) bool {
	return t.State == Pending || (t.State == Running && !t.LeaseExpires.After(now))
}

func (m *MemStore) take(t *Task, holder string, now time.Time, lease time.Duration) *Task {
	t.State, t.Holder, t.LeaseExpires, t.UpdatedAt = Running, holder, now.Add(lease), now
	t.Epoch++
	c := cp(t)
	return &c
}

// Claim implements Store.
func (m *MemStore) Claim(_ context.Context, holder string, lease time.Duration) (*Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.clock()
	if err := m.hook("claim"); err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(m.tasks))
	for id := range m.tasks {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		if t := m.tasks[id]; claimable(t, now) {
			return m.take(t, holder, now, lease), nil
		}
	}
	return nil, nil
}

// ClaimID implements Store.
func (m *MemStore) ClaimID(_ context.Context, id int64, holder string, lease time.Duration) (*Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.clock()
	t, ok := m.tasks[id]
	if !ok {
		return nil, ErrNotFound
	}
	if !claimable(t, now) {
		return nil, nil
	}
	return m.take(t, holder, now, lease), nil
}

func (m *MemStore) held(id int64, l Lease) (*Task, error) {
	t, ok := m.tasks[id]
	if !ok {
		return nil, ErrNotFound
	}
	if t.Holder != l.Holder || t.Epoch != l.Epoch || t.State != Running {
		return nil, ErrLeaseLost
	}
	return t, nil
}

// Renew implements Store.
func (m *MemStore) Renew(_ context.Context, id int64, l Lease, lease time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.hook("renew"); err != nil {
		return err
	}
	t, err := m.held(id, l)
	if err != nil {
		return err
	}
	t.LeaseExpires = m.clock().Add(lease)
	return nil
}

// SetStep implements Store.
func (m *MemStore) SetStep(_ context.Context, id int64, l Lease, i int, state string, pct float64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.hook("setstep:" + state); err != nil {
		return false, err
	}
	t, err := m.held(id, l)
	if err != nil {
		return false, err
	}
	now := m.clock()
	if state == StepRunning && !t.LeaseExpires.After(now) {
		return false, ErrLeaseLost // see Store.SetStep
	}
	t.Steps[i].State, t.Percent, t.UpdatedAt = state, pct, now
	return t.CancelRequested, nil
}

// Finish implements Store.
func (m *MemStore) Finish(_ context.Context, id int64, l Lease, state State, e *Error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, err := m.held(id, l)
	if err != nil {
		return err
	}
	t.State, t.Error, t.UpdatedAt, t.Holder, t.LeaseExpires = state, e, m.clock(), "", time.Time{}
	if state == Done {
		t.Percent = 100
	}
	return nil
}

// RequestCancel implements Store.
func (m *MemStore) RequestCancel(_ context.Context, id int64, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return ErrNotFound
	}
	switch t.State {
	case Done, Failed:
		return ErrNotCancellable
	case Pending, Paused:
		t.State, t.UpdatedAt = Failed, now
		t.Error = &Error{Code: CodeCanceled, Message: "The task was canceled before it started."}
	default:
		t.CancelRequested = true
	}
	return nil
}

// RequestCancelIf implements CondCancelStore.
func (m *MemStore) RequestCancelIf(_ context.Context, id int64, now time.Time, seen State, fail *Error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return ErrNotFound
	}
	if t.State == Done || t.State == Failed || seen == Done || seen == Failed {
		return ErrNotCancellable
	}
	if t.State != seen {
		return ErrStateChanged
	}
	switch seen {
	case Pending, Paused:
		e := *fail
		t.State, t.UpdatedAt, t.Error = Failed, now, &e
	default:
		t.CancelRequested = true
	}
	return nil
}

// List implements Store.
func (m *MemStore) List(_ context.Context, limit int) ([]Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Task, 0, len(m.tasks))
	for _, t := range m.tasks {
		out = append(out, cp(t))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Export returns copies of all tasks (for persistence).
func (m *MemStore) Export() []Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Task, 0, len(m.tasks))
	for _, t := range m.tasks {
		out = append(out, cp(t))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Import replaces the content with tasks (for loading persisted state).
func (m *MemStore) Import(ts []Task) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tasks, m.next = map[int64]*Task{}, 0
	for i := range ts {
		t := ts[i]
		m.tasks[t.ID] = &t
		if t.ID > m.next {
			m.next = t.ID
		}
	}
}

// SetMark implements MarkStore.
func (m *MemStore) SetMark(_ context.Context, id int64, l Lease, mark string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, err := m.held(id, l)
	if err != nil {
		return err
	}
	if t.CancelRequested {
		return ErrCancelRequested
	}
	t.Mark = mark
	return nil
}

// SetDetail implements DetailStore.
func (m *MemStore) SetDetail(_ context.Context, id int64, l Lease, detail string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, err := m.held(id, l)
	if err != nil {
		return err
	}
	t.Detail = detail
	return nil
}

// Pause implements PauseStore.
func (m *MemStore) Pause(_ context.Context, id int64, l Lease, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, err := m.held(id, l)
	if err != nil {
		return err
	}
	t.State, t.Detail, t.UpdatedAt, t.Holder, t.LeaseExpires = Paused, reason, m.clock(), "", time.Time{}
	return nil
}

// Resume implements PauseStore.
func (m *MemStore) Resume(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return ErrNotFound
	}
	if t.State != Paused {
		return ErrNotPaused
	}
	t.State, t.CancelRequested, t.UpdatedAt = Pending, false, m.clock()
	return nil
}

// serverOps are the kinds that exclude a rebalance (and are excluded by it), see migration 0009.
var serverOps = map[string]bool{"server.add": true, "server.remove": true, "server.leave": true, "server.failover": true, "replication.restorePreferredPrimaries": true}

// failoverClass are the kinds of which at most one is unfinished at any time (migration 0011, KX003).
var failoverClass = map[string]bool{"server.failover": true, "replication.restorePreferredPrimaries": true}

// databaseOps are the kinds that change which databases and tables have a catalog (migration 0016): a failover is
// excluded by them and excludes them.
var databaseOps = map[string]bool{"database.create": true, "database.drop": true, "table.distribute": true,
	// migration 0041: the tasks of the distributed schemas exclude a failover too
	"schema.distribute": true, "schema.move": true, "schema.undistribute": true}

// schemaMoves are the schema tasks that move a bucket (a schema is one bucket of a colocation group of its own; make local may
// move it first): a rebalance and they exclude each other (migration 0041, KX001 and KX002).
var schemaMoves = map[string]bool{"schema.move": true, "schema.undistribute": true}

// membershipOps change which servers hold shards: a schema task that moves a bucket and they exclude each other (migration 0041, KX006 and KX007).
var membershipOps = map[string]bool{"server.add": true, "server.remove": true, "server.leave": true}

// excludedLocked is the rule of the database trigger for a task of kind about to be created.
func (m *MemStore) excludedLocked(kind string) error {
	for _, o := range m.tasks {
		switch {
		case kind == "rebalance" && serverOps[o.Kind] && o.State.Active():
			return &ExcludedError{Kind: kind, Other: o.Kind}
		case serverOps[kind] && o.Kind == "rebalance" && (o.State == Pending || o.State == Running):
			return &ExcludedError{Kind: kind, Other: "rebalance"}
		case failoverClass[kind] && failoverClass[o.Kind] && o.State.Active():
			return &ExcludedError{Kind: kind, Other: o.Kind}
		// Migration 0016 (F3-04): a failover and a creation, a drop or a distribution exclude each other.
		case failoverClass[kind] && databaseOps[o.Kind] && o.State.Active():
			return &ExcludedError{Kind: kind, Other: o.Kind}
		case databaseOps[kind] && failoverClass[o.Kind] && o.State.Active():
			return &ExcludedError{Kind: kind, Other: o.Kind}
		// Migration 0041: a rebalance and a schema task that moves a bucket exclude each other.
		case kind == "rebalance" && schemaMoves[o.Kind] && o.State.Active():
			return &ExcludedError{Kind: kind, Other: o.Kind}
		case schemaMoves[kind] && o.Kind == "rebalance" && (o.State == Pending || o.State == Running):
			return &ExcludedError{Kind: kind, Other: "rebalance"}
		case schemaMoves[kind] && membershipOps[o.Kind] && o.State.Active():
			return &ExcludedError{Kind: kind, Other: o.Kind}
		case membershipOps[kind] && schemaMoves[o.Kind] && o.State.Active():
			return &ExcludedError{Kind: kind, Other: o.Kind}
		// Migration 0045: a database import and a rebalance (a drain is one), or the addition or the removal of a server, exclude each other.
		case kind == "database.import" && o.Kind == "rebalance" && (o.State == Pending || o.State == Running):
			return &ExcludedError{Kind: kind, Other: o.Kind}
		case kind == "database.import" && (o.Kind == "server.add" || o.Kind == "server.remove" || o.Kind == "server.leave") && o.State.Active():
			return &ExcludedError{Kind: kind, Other: o.Kind}
		case (kind == "rebalance" || kind == "server.add" || kind == "server.remove" || kind == "server.leave") && o.Kind == "database.import" && o.State.Active():
			return &ExcludedError{Kind: kind, Other: o.Kind}
		}
	}
	return nil
}

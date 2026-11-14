// SPDX-License-Identifier: PostgreSQL

// Package task is the task engine of kingresd (design control-plane-2.md,
// D4). A task is a row in kingres_cp.task executed by the manager leader as
// an ordered list of idempotent steps. A step records its completion before
// the next one starts, so an interrupted task resumes at the first step that
// is not done. The leader holds a lease on the row and renews it; a task
// whose lease expired is taken over. There is at most one unfinished task per
// subject. Tasks never store secrets: a secret reaches a step only through
// the in-memory Env of the run that was started with it.
package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// State of a task.
type State string

// Task states.
const (
	Pending State = "pending"
	Running State = "running"
	Paused  State = "paused"
	Failed  State = "failed"
	Done    State = "done"
)

// Active reports whether the state counts for the one-task-per-subject rule.
func (s State) Active() bool { return s == Pending || s == Running || s == Paused }

// Step states.
const (
	StepPending = "pending"
	StepRunning = "running"
	StepDone    = "done"
	StepFailed  = "failed"
)

// StepState is the recorded state of one step.
type StepState struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

// Error is the recorded failure of a task.
type Error struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Step      string `json:"step,omitempty"`
	Retryable bool   `json:"retryable"`
}

// Error codes of a failed task.
const (
	CodeStepFailed        = "step_failed"
	CodeCanceled          = "cancelled"
	CodeSecretUnavailable = "secret_unavailable"
	CodeUnknownKind       = "unknown_kind"
)

// Task is a persisted task.
type Task struct {
	ID          int64
	Kind        string
	State       State
	SubjectType string
	SubjectID   string
	// Input is task input without secrets.
	Input json.RawMessage
	// Steps has one entry per step of the definition.
	Steps []StepState
	// Percent is the overall progress, 0 to 100.
	Percent         float64
	Error           *Error
	CancelRequested bool
	CreatedAt       time.Time
	UpdatedAt       time.Time
	CreatedBy       string
	Holder          string
	LeaseExpires    time.Time
}

// DoneSteps is the number of leading steps that are done.
func (t Task) DoneSteps() int {
	n := 0
	for _, s := range t.Steps {
		if s.State != StepDone {
			break
		}
		n++
	}
	return n
}

// Subject identifies what a task works on.
type Subject struct{ Type, ID string }

// Errors of the engine and the stores.
var (
	// ErrSubjectBusy means the subject already has an unfinished task.
	ErrSubjectBusy = errors.New("subject already has an unfinished task")
	// ErrNotFound means the task does not exist.
	ErrNotFound = errors.New("task not found")
	// ErrLeaseLost means the caller no longer holds the lease of the task.
	ErrLeaseLost = errors.New("task lease lost")
	// ErrNotCancellable means the task is finished.
	ErrNotCancellable = errors.New("task is finished")
	// ErrUnknownKind means no definition is registered for the kind.
	ErrUnknownKind = errors.New("unknown task kind")
)

// Env is what a step can use besides its context.
type Env struct {
	// Task is the task being run (a copy).
	Task    Task
	secrets map[string]string
}

// Secret returns a secret handed to this run, or false (after a restart the
// secrets are gone).
func (e *Env) Secret(name string) (string, bool) {
	v, ok := e.secrets[name]
	return v, ok
}

// Step is one idempotent unit of work. Run may be called again after an
// interruption, even when it had finished its effect, so it must check what
// exists before it creates.
type Step struct {
	// Name is shown to users (English).
	Name string
	// Irreversible steps say so; cancellation never undoes a finished step.
	Irreversible bool
	// NeedsSecret names a secret the step needs; when it is missing the task
	// fails with CodeSecretUnavailable instead of running the step.
	NeedsSecret string
	// Run does the work.
	Run func(ctx context.Context, env *Env) error
}

// Definition is the ordered step list of a task kind.
type Definition struct {
	Kind  string
	Steps []Step
}

// Store persists tasks. Every method that changes a task named by a holder
// checks the holder, so a leader that lost its lease cannot write.
type Store interface {
	// Create inserts a pending task or returns ErrSubjectBusy.
	Create(ctx context.Context, t Task) (Task, error)
	// Get returns a task.
	Get(ctx context.Context, id int64) (Task, error)
	// Claim takes the oldest pending task, or a running task whose lease has
	// expired at now, for holder until now+lease. nil when none is available.
	Claim(ctx context.Context, holder string, now time.Time, lease time.Duration) (*Task, error)
	// ClaimID claims one specific task the same way.
	ClaimID(ctx context.Context, id int64, holder string, now time.Time, lease time.Duration) (*Task, error)
	// Renew extends the lease.
	Renew(ctx context.Context, id int64, holder string, now time.Time, lease time.Duration) error
	// SetStep records the state of step i and the progress; it returns
	// whether cancellation was requested.
	SetStep(ctx context.Context, id int64, holder string, i int, state string, percent float64, now time.Time) (cancel bool, err error)
	// Finish sets the final state (Done or Failed), the error, and releases
	// the lease.
	Finish(ctx context.Context, id int64, holder string, state State, e *Error, now time.Time) error
	// RequestCancel marks a task for cancellation; a pending task is failed
	// directly with CodeCanceled.
	RequestCancel(ctx context.Context, id int64, now time.Time) error
	// List returns tasks, newest first.
	List(ctx context.Context, limit int) ([]Task, error)
}

// Engine runs tasks.
type Engine struct {
	store  Store
	defs   map[string]Definition
	holder string
	lease  time.Duration
	poll   time.Duration
	now    func() time.Time
	log    *slog.Logger
	tries  int
}

// Options configure an Engine.
type Options struct {
	// Holder identifies this process in lease rows.
	Holder string
	// Lease is the lease length (default 30s); Poll the idle polling
	// interval of Run (default 2s).
	Lease, Poll time.Duration
	// Now is the clock (default time.Now).
	Now    func() time.Time
	Logger *slog.Logger
	// StoreTries is how many times a store write is tried, one second apart
	// (default 30).
	StoreTries int
}

// NewEngine builds an engine. Definitions must have at least one step each.
func NewEngine(s Store, defs []Definition, o Options) (*Engine, error) {
	e := &Engine{store: s, defs: map[string]Definition{}, holder: o.Holder, lease: o.Lease, poll: o.Poll,
		now: o.Now, log: o.Logger, tries: o.StoreTries}
	if e.holder == "" {
		return nil, errors.New("task: a holder id is required")
	}
	if e.tries == 0 {
		e.tries = 30
	}
	if e.lease == 0 {
		e.lease = 30 * time.Second
	}
	if e.poll == 0 {
		e.poll = 2 * time.Second
	}
	if e.now == nil {
		e.now = time.Now
	}
	if e.log == nil {
		e.log = slog.New(slog.DiscardHandler)
	}
	for _, d := range defs {
		if d.Kind == "" || len(d.Steps) == 0 {
			return nil, fmt.Errorf("task: definition %q needs a kind and steps", d.Kind)
		}
		if _, dup := e.defs[d.Kind]; dup {
			return nil, fmt.Errorf("task: duplicate definition %q", d.Kind)
		}
		e.defs[d.Kind] = d
	}
	return e, nil
}

// Submit creates a pending task. input must not contain secrets. It returns
// ErrSubjectBusy when the subject has an unfinished task.
func (e *Engine) Submit(ctx context.Context, kind string, subj Subject, input any, createdBy string) (Task, error) {
	def, ok := e.defs[kind]
	if !ok {
		return Task{}, fmt.Errorf("%w: %s", ErrUnknownKind, kind)
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return Task{}, fmt.Errorf("task: encoding input: %w", err)
	}
	steps := make([]StepState, len(def.Steps))
	for i, s := range def.Steps {
		steps[i] = StepState{Name: s.Name, State: StepPending}
	}
	now := e.now()
	return e.store.Create(ctx, Task{Kind: kind, State: Pending, SubjectType: subj.Type, SubjectID: subj.ID,
		Input: raw, Steps: steps, CreatedAt: now, UpdatedAt: now, CreatedBy: createdBy})
}

// RunNow claims and runs the task id to its end in the calling goroutine
// with the given in-memory secrets (they are dropped when the run ends). It
// is how a request that carries a password starts its task.
func (e *Engine) RunNow(ctx context.Context, id int64, secrets map[string]string) error {
	t, err := e.store.ClaimID(ctx, id, e.holder, e.now(), e.lease)
	if err != nil {
		return err
	}
	if t == nil {
		return nil // somebody else runs it
	}
	return e.run(ctx, *t, secrets)
}

// RunOnce claims and runs one available task. It reports whether it ran one.
func (e *Engine) RunOnce(ctx context.Context) (bool, error) {
	t, err := e.store.Claim(ctx, e.holder, e.now(), e.lease)
	if err != nil || t == nil {
		return false, err
	}
	return true, e.run(ctx, *t, nil)
}

// Run polls for tasks until ctx ends.
func (e *Engine) Run(ctx context.Context) {
	for ctx.Err() == nil {
		ran, err := e.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			e.log.Error("task engine", "error", err)
		}
		if ran {
			continue
		}
		select {
		case <-ctx.Done():
		case <-time.After(e.poll):
		}
	}
}

// Cancel asks for the cancellation of a task (between steps).
func (e *Engine) Cancel(ctx context.Context, id int64) error {
	return e.store.RequestCancel(ctx, id, e.now())
}

// Get returns a task.
func (e *Engine) Get(ctx context.Context, id int64) (Task, error) { return e.store.Get(ctx, id) }

func (e *Engine) run(ctx context.Context, t Task, secrets map[string]string) error {
	def, ok := e.defs[t.Kind]
	if !ok {
		return e.finish(ctx, t, Failed, &Error{Code: CodeUnknownKind, Message: "No definition for task kind " + t.Kind + "."})
	}
	env := &Env{Task: t, secrets: secrets}
	// The lease is renewed in the background while a step runs. A lost lease
	// cancels the step's context.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		e.renewLoop(runCtx, cancel, t.ID)
	}()
	defer wg.Wait()
	defer cancel()

	for i := t.DoneSteps(); i < len(def.Steps); i++ {
		if runCtx.Err() != nil {
			return runCtx.Err() // lease lost or shutdown: the next leader resumes here
		}
		st := def.Steps[i]
		if st.NeedsSecret != "" {
			if _, have := secrets[st.NeedsSecret]; !have {
				return e.fail(ctx, t, i, st, &Error{Code: CodeSecretUnavailable, Step: st.Name, Retryable: false,
					Message: "The step needs a secret that is only available to the request that started the task."})
			}
		}
		if cancelReq, err := e.setStep(ctx, t.ID, e.holder, i, StepRunning, percent(i, len(def.Steps)), e.now()); err != nil {
			return err
		} else if cancelReq {
			return e.cancelTask(ctx, t, def, i)
		}
		if err := st.Run(runCtx, env); err != nil {
			if runCtx.Err() != nil && ctx.Err() == nil {
				return ErrLeaseLost
			}
			if ctx.Err() != nil {
				return ctx.Err() // interrupted, not failed: resumes later
			}
			return e.fail(ctx, t, i, st, &Error{Code: CodeStepFailed, Step: st.Name, Retryable: true, Message: clip(err.Error())})
		}
		cancelReq, err := e.setStep(ctx, t.ID, e.holder, i, StepDone, percent(i+1, len(def.Steps)), e.now())
		if err != nil {
			return err
		}
		if cancelReq && i+1 < len(def.Steps) {
			return e.cancelTask(ctx, t, def, i+1)
		}
	}
	return e.finish(ctx, t, Done, nil)
}

func (e *Engine) renewLoop(ctx context.Context, cancel context.CancelFunc, id int64) {
	tick := time.NewTicker(e.lease / 3)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := e.store.Renew(ctx, id, e.holder, e.now(), e.lease); err != nil {
				if errors.Is(err, ErrLeaseLost) || errors.Is(err, ErrNotFound) {
					e.log.Warn("task lease lost", "task", id)
					cancel()
					return
				}
				e.log.Error("renewing task lease", "task", id, "error", err)
			}
		}
	}
}

func (e *Engine) cancelTask(ctx context.Context, t Task, def Definition, next int) error {
	var irrev []string
	for _, s := range def.Steps[:next] {
		if s.Irreversible {
			irrev = append(irrev, s.Name)
		}
	}
	msg := "The task was canceled between steps."
	if len(irrev) > 0 {
		msg += " Steps already done are not undone, including irreversible ones."
	}
	return e.finish(ctx, t, Failed, &Error{Code: CodeCanceled, Message: msg, Retryable: false})
}

func (e *Engine) fail(ctx context.Context, t Task, i int, st Step, te *Error) error {
	if i < len(t.Steps) {
		if _, err := e.setStep(ctx, t.ID, e.holder, i, StepFailed, percent(i, len(t.Steps)), e.now()); err != nil {
			e.log.Error("recording failed step", "task", t.ID, "error", err)
		}
	}
	e.log.Warn("task step failed", "task", t.ID, "kind", t.Kind, "step", st.Name)
	return e.finish(ctx, t, Failed, te)
}

func (e *Engine) finish(ctx context.Context, t Task, s State, te *Error) error {
	c := context.WithoutCancel(ctx)
	return e.retry(c, func() error { return e.store.Finish(c, t.ID, e.holder, s, te, e.now()) })
}

// setStep records a step state, retrying while the store is briefly
// unavailable (a task may restart the very instance that holds its row).
func (e *Engine) setStep(ctx context.Context, id int64, holder string, i int, state string, pct float64, now time.Time) (bool, error) {
	var cancel bool
	err := e.retry(ctx, func() error {
		var err error
		cancel, err = e.store.SetStep(ctx, id, holder, i, state, pct, now)
		return err
	})
	return cancel, err
}

// retry runs f again for a few seconds unless the error says the lease or the
// task is gone.
func (e *Engine) retry(ctx context.Context, f func() error) error {
	var err error
	for attempt := 0; attempt < e.tries; attempt++ {
		if err = f(); err == nil || errors.Is(err, ErrLeaseLost) || errors.Is(err, ErrNotFound) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Second):
		}
	}
	return err
}

func percent(done, total int) float64 { return float64(done) * 100 / float64(total) }

// clip bounds a message stored in a task row.
func clip(s string) string {
	if len(s) > 500 {
		return s[:500] + "..."
	}
	return s
}

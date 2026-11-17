// SPDX-License-Identifier: AGPL-3.0-only

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

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fence"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
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
	// StepSkipped is a step that decided, on purpose, that it has nothing to do
	// (contract TaskStep.state, 0.9.0); the task goes on with the next step.
	StepSkipped = "skipped"
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
	// CodeStepInterrupted means a NonIdempotent step had started when the
	// previous run ended, so whether its effect happened is unknown.
	CodeStepInterrupted = "step_interrupted"
	// CodeFenced means a server kept refusing the calls of the task because it had seen a newer run of it than
	// this server's lease can have (see MaxFencedRuns).
	CodeFenced = "fenced"
)

// MaxFencedRuns bounds the runs of one task, in a row, that end because a callee refused their fence while
// the store still lets them hold the lease. Each such run stands down, its lease runs out and the next claim has a
// higher epoch, so a callee that has seen a newer run than this server can reach (for instance after a restore of
// the system database, which makes task ids and epochs repeat) is caught up with after some passes; it is never
// caught up with when its mark is far ahead. Then the task fails, with the alert of its kind, instead of looping with a
// lease period between passes and nothing to see.
const MaxFencedRuns = 5

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
	Percent float64
	// Detail is free text a step records for the operator (for example the
	// certificate fingerprint it saw); never a secret. Not part of the contract.
	Detail          string
	Error           *Error
	CancelRequested bool
	CreatedAt       time.Time
	UpdatedAt       time.Time
	CreatedBy       string
	Holder          string
	// Epoch is the fencing token of the lease: it grows with every claim, so a
	// run that lost its lease is told apart from the run that holds it now.
	Epoch        int64
	LeaseExpires time.Time
}

// Lease names who holds a task and in which epoch. A store accepts a write of
// a task only from the holder in the current epoch.
type Lease struct {
	Holder string
	Epoch  int64
}

// DoneSteps is the number of leading steps that are over: done or skipped.
func (t Task) DoneSteps() int {
	n := 0
	for _, s := range t.Steps {
		if s.State != StepDone && s.State != StepSkipped {
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

// ExcludedError means the database refused to start a task because an operation that excludes it is unfinished
// (migration 0009): a rebalance against a change of the servers. Other is the kind of the other task, or
// "rebalance" when the refused task is a server operation.
type ExcludedError struct{ Kind, Other string }

func (e *ExcludedError) Error() string {
	return "task of kind " + e.Kind + " excluded by " + e.Other
}

// excluded maps the database error of the exclusion trigger, nil for any other error.
func excluded(kind string, err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && (pe.Code == "KX001" || pe.Code == "KX002" || pe.Code == "KX003" || pe.Code == "KX004") {
		return &ExcludedError{Kind: kind, Other: pe.Detail}
	}
	return nil
}

// PauseError is what a step returns to stop its task without failing it: the
// engine records the task as paused, releases the lease and keeps the steps that
// are done; the step that paused runs again from its start when the task is
// resumed (Engine.Resume), so it must be able to pick up where it left off.
type PauseError struct{ Reason string }

func (e *PauseError) Error() string { return "task paused: " + e.Reason }

// SkipError is what a step returns when it decided on purpose to do nothing: the
// engine records the step as skipped (not done), keeps the reason as the task's
// detail and goes on with the next step.
type SkipError struct{ Reason string }

func (e *SkipError) Error() string { return "step skipped: " + e.Reason }

// Skip returns the error that makes the engine record the step as skipped.
func Skip(reason string) error { return &SkipError{Reason: reason} }

// Pause returns the error that pauses the task (see PauseError).
func Pause(reason string) error { return &PauseError{Reason: reason} }

// PauseStore is implemented by stores that can pause and resume a task.
type PauseStore interface {
	// Pause changes a running task of the lease holder to Paused and releases
	// the lease; reason is kept as the task's detail.
	Pause(ctx context.Context, id int64, l Lease, reason string) error
	// Resume changes a paused task to Pending, so that the next claim takes it.
	// ErrNotPaused when the task is in another state.
	Resume(ctx context.Context, id int64) error
}

// ErrNotPaused means Resume was asked for a task that is not paused.
var ErrNotPaused = errors.New("task is not paused")

// Env is what a step can use besides its context.
type Env struct {
	// Task is the task being run (a copy).
	Task    Task
	secrets map[string]string
	eng     *Engine
	lease   Lease
}

// DetailStore is implemented by stores that can keep a task's Detail.
type DetailStore interface {
	SetDetail(ctx context.Context, id int64, l Lease, detail string) error
}

// SetDetail records free text on the task (see Task.Detail); a store without
// support ignores it.
func (e *Env) SetDetail(ctx context.Context, detail string) error {
	if e.eng == nil {
		return nil
	}
	ds, ok := e.eng.store.(DetailStore)
	if !ok {
		return nil
	}
	if len(detail) > 2000 {
		detail = detail[:2000]
	}
	e.Task.Detail = detail
	return ds.SetDetail(ctx, e.Task.ID, e.lease, detail)
}

// Fence returns the fencing token of this run (the lease epoch). A step that
// calls another server for a non-idempotent effect can send it along, so that
// the other side refuses a call of a run that lost its lease meanwhile.
func (e *Env) Fence() int64 { return e.lease.Epoch }

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
	// NonIdempotent marks a step that must never run twice. The engine
	// records the step as running before it calls Run; when a later run finds
	// that record (the leader died or lost its lease after the step started,
	// so the outcome is unknown) it does not run the step again but fails the
	// task with CodeStepInterrupted. Steps without the mark are at-least-once:
	// they run again.
	NonIdempotent bool
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
	// Dynamic, when set, builds the steps of one task from its input (a task
	// with one step per server); Steps is then ignored. It must give the same
	// answer every time for the same input.
	Dynamic func(input json.RawMessage) ([]Step, error)
	// OnFail, when set, runs after the task has been recorded as failed or
	// canceled (not when it was only interrupted): it cleans up what the steps
	// that ran created. Its error is only logged.
	OnFail func(ctx context.Context, t Task)
	// Cancellable, when set, says whether a cancel request is accepted for a task of
	// this kind in its present state (the cancel guard). A failover is not
	// cancellable once its irreversible step is done: it rolls forward only. Nil: every
	// unfinished task is cancellable.
	Cancellable func(t Task) bool
}

func (d Definition) steps(input json.RawMessage) ([]Step, error) {
	if d.Dynamic != nil {
		return d.Dynamic(input)
	}
	return d.Steps, nil
}

// Store persists tasks. Every method that changes a task named by a lease
// checks the holder and the epoch, so a leader that lost its lease cannot write.
//
// All lease arithmetic (the expiry written by a claim or a renewal, the test
// whether a lease has expired) is done with the clock of the store (the
// database's), never with the clock of the calling server: servers whose
// clocks differ must agree on whether a lease is still valid.
type Store interface {
	// Create inserts a pending task or returns ErrSubjectBusy.
	Create(ctx context.Context, t Task) (Task, error)
	// Get returns a task.
	Get(ctx context.Context, id int64) (Task, error)
	// Claim takes the oldest pending task, or a running task whose lease has
	// expired, for holder for the length of lease, and increments the epoch.
	// nil when none is available.
	Claim(ctx context.Context, holder string, lease time.Duration) (*Task, error)
	// ClaimID claims one specific task the same way.
	ClaimID(ctx context.Context, id int64, holder string, lease time.Duration) (*Task, error)
	// Renew extends the lease to lease from now.
	Renew(ctx context.Context, id int64, l Lease, lease time.Duration) error
	// SetStep records the state of step i and the progress; it returns
	// whether cancellation was requested. Marking a step StepRunning also
	// requires that the lease has not expired: that record and the check are
	// one atomic statement, so a holder that was paused past its lease cannot
	// start a step another server may already have taken over.
	SetStep(ctx context.Context, id int64, l Lease, i int, state string, percent float64) (cancel bool, err error)
	// Finish sets the final state (Done or Failed), the error, and releases
	// the lease.
	Finish(ctx context.Context, id int64, l Lease, state State, e *Error) error
	// RequestCancel marks a task for cancellation; a pending task is failed
	// directly with CodeCanceled.
	RequestCancel(ctx context.Context, id int64, now time.Time) error
	// List returns tasks, newest first.
	List(ctx context.Context, limit int) ([]Task, error)
}

// Engine runs tasks.
type Engine struct {
	store Store
	defs  map[string]Definition
	defMu sync.RWMutex
	// claimMu keeps pollers from claiming a task that SubmitHeld is creating.
	claimMu sync.RWMutex
	holder  string
	lease   time.Duration
	poll    time.Duration
	// now is the clock of this server. It is used only to measure how long ago
	// a lease was last confirmed by the store, never to compute an expiry.
	now func() time.Time
	// renewEvery replaces the renewal ticker (tests).
	renewEvery <-chan time.Time
	// gate, when set and false, keeps the engine from claiming or running (Options.Gate).
	gate    func() bool
	log     *slog.Logger
	tries   int
	faultFn FaultFunc
	// fenced counts, per task, the runs in a row that a callee refused (see MaxFencedRuns).
	fencedMu sync.Mutex
	fenced   map[int64]int
}

// Options configure an Engine.
type Options struct {
	// Holder identifies this process in lease rows.
	Holder string
	// Lease is the lease length (default 30s); Poll the idle polling
	// interval of Run (default 2s).
	Lease, Poll time.Duration
	// Now is the clock of this server (default time.Now); see Engine.now.
	Now func() time.Time
	// RenewEvery, when set, replaces the renewal ticker: one renewal per
	// received value. Only tests use it, to drive the renewals without waiting.
	RenewEvery <-chan time.Time
	Logger     *slog.Logger
	// StoreTries is how many times a store write is tried, one second apart
	// (default 30).
	StoreTries int
	// Gate, when set, is asked before every claim and between the steps of a run: while it
	// returns false the engine neither claims nor runs anything (a run stands down quietly at
	// the next step boundary, records nothing and runs no hook; its lease lapses and the task
	// is claimed again once the gate is open). A server that was promoted in PostgreSQL as the
	// map group's standby but is not yet the recognised primary of the map group keeps it
	// closed (design ha-phase5-part2.md, 3.4).
	Gate func() bool
	// Fault, when set, is called at the named crash points (see FaultFunc).
	// Production code leaves it nil; the crash-injection tests set it.
	Fault FaultFunc
}

// NewEngine builds an engine. Definitions must have at least one step each.
func NewEngine(s Store, defs []Definition, o Options) (*Engine, error) {
	e := &Engine{store: s, defs: map[string]Definition{}, holder: o.Holder, lease: o.Lease, poll: o.Poll,
		now: o.Now, log: o.Logger, tries: o.StoreTries, faultFn: o.Fault, renewEvery: o.RenewEvery, gate: o.Gate}
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
		if d.Kind == "" || (len(d.Steps) == 0 && d.Dynamic == nil) {
			return nil, fmt.Errorf("task: definition %q needs a kind and steps", d.Kind)
		}
		if _, dup := e.defs[d.Kind]; dup {
			return nil, fmt.Errorf("task: duplicate definition %q", d.Kind)
		}
		e.defs[d.Kind] = d
		if d.Cancellable != nil {
			guards.Store(d.Kind, d.Cancellable)
		}
	}
	return e, nil
}

// Submit creates a pending task. input must not contain secrets. It returns
// ErrSubjectBusy when the subject has an unfinished task.
func (e *Engine) Submit(ctx context.Context, kind string, subj Subject, input any, createdBy string) (Task, error) {
	def, ok := e.def(kind)
	if !ok {
		return Task{}, fmt.Errorf("%w: %s", ErrUnknownKind, kind)
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return Task{}, fmt.Errorf("task: encoding input: %w", err)
	}
	defSteps, err := def.steps(raw)
	if err != nil {
		return Task{}, fmt.Errorf("task: building the steps: %w", err)
	}
	steps := make([]StepState, len(defSteps))
	for i, s := range defSteps {
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
	if e.gateClosed() {
		return nil // the task stays pending; the poll claims it once the gate is open
	}
	started := e.now()
	t, err := e.store.ClaimID(ctx, id, e.holder, e.lease)
	if err != nil {
		return err
	}
	if t == nil {
		return nil // somebody else runs it
	}
	return e.run(ctx, *t, started, secrets)
}

// RunOnce claims and runs one available task. It reports whether it ran one.
func (e *Engine) RunOnce(ctx context.Context) (bool, error) {
	if e.gateClosed() {
		return false, nil
	}
	e.claimMu.RLock()
	started := e.now()
	t, err := e.store.Claim(ctx, e.holder, e.lease)
	e.claimMu.RUnlock()
	if err != nil || t == nil {
		return false, err
	}
	return true, e.run(ctx, *t, started, nil)
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

// Cancel asks for the cancellation of a task. A running task stops at its next
// step boundary, or at the next pause point of a step that can pause. A paused task
// is not run by anybody, so it is failed at once (the store's condition is "still
// paused") and its cleanup hook runs here, with the task as recorded.
func (e *Engine) Cancel(ctx context.Context, id int64) error {
	t, gerr := e.store.Get(ctx, id)
	if gerr == nil && recordedKinds[t.Kind] {
		return ErrNotCancellable // written by its owner, not run by the engine
	}
	if gerr == nil {
		if def, ok := e.def(t.Kind); ok && def.Cancellable != nil && !def.Cancellable(t) {
			return ErrNotCancellable // the guard of the kind: past the point of no return
		}
	}
	if err := e.store.RequestCancel(ctx, id, e.now()); err != nil {
		return err
	}
	if gerr == nil && t.State == Paused {
		// Read again: the hook runs only when this request ended the task (it may have
		// been resumed meanwhile, and then a run owns it).
		if now, err := e.store.Get(ctx, id); err == nil && now.State == Failed && now.Error != nil && now.Error.Code == CodeCanceled {
			e.onFail(ctx, now, now.Error)
		}
	}
	return nil
}

// Resume makes a paused task pending; the next claim (or RunNow) runs it.
func (e *Engine) Resume(ctx context.Context, id int64) error {
	ps, ok := e.store.(PauseStore)
	if !ok {
		return errors.New("task: the store cannot resume tasks")
	}
	return ps.Resume(ctx, id)
}

// RunNowID claims and runs the task id like RunNow, for a caller that wants to
// run it in the background right after Resume.
func (e *Engine) RunNowID(ctx context.Context, id int64) error { return e.RunNow(ctx, id, nil) }

// Get returns a task.
func (e *Engine) Get(ctx context.Context, id int64) (Task, error) { return e.store.Get(ctx, id) }

// leaseOf is the lease this engine holds on t, as the store handed it out.
func (e *Engine) leaseOf(t Task) Lease { return Lease{Holder: e.holder, Epoch: t.Epoch} }

// leaseMargin is the part of the lease that is kept in reserve: a run stops its
// step this long before the lease it last confirmed would expire.
func (e *Engine) leaseMargin() time.Duration { return e.lease / 3 }

// run executes the task. started is the time on this server's clock before the
// claim was sent to the store: the lease is valid at most until started+lease
// by this clock (the store's expiry is measured from a later moment), so
// counting from it is the safe side.
func (e *Engine) run(ctx context.Context, t Task, started time.Time, secrets map[string]string) error {
	def, ok := e.def(t.Kind)
	if !ok {
		return e.finish(ctx, t, Failed, &Error{Code: CodeUnknownKind, Message: "No definition for task kind " + t.Kind + "."})
	}
	steps, err := def.steps(t.Input)
	if err != nil {
		return e.finish(ctx, t, Failed, &Error{Code: CodeStepFailed, Message: "The steps of the task cannot be built."})
	}
	def.Steps = steps
	env := &Env{Task: t, secrets: secrets, eng: e, lease: e.leaseOf(t)}
	// The lease is renewed in the background while a step runs. A lost lease,
	// and a lease that could not be renewed for so long that it may have
	// expired for the store, cancel the step's context: a leader that cannot
	// reach the store stops working instead of running on while another server
	// takes the task over.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	guard := newLeaseGuard(e, cancel, started.Add(e.lease-e.leaseMargin()))
	defer guard.stop()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		e.renewLoop(runCtx, guard, env.lease, t.ID)
	}()
	defer wg.Wait()
	defer cancel()

	for i := t.DoneSteps(); i < len(def.Steps); i++ {
		if runCtx.Err() != nil || guard.expired() {
			return runCtx.Err() // lease lost or shutdown: the next leader resumes here
		}
		if e.gateClosed() {
			e.log.Info("task engine: the gate is closed; the run stands down at a step boundary", "task", t.ID, "kind", t.Kind, "step", i)
			return nil
		}
		st := def.Steps[i]
		if st.NonIdempotent && i < len(t.Steps) && t.Steps[i].State == StepRunning {
			return e.fail(ctx, t, i, st, &Error{Code: CodeStepInterrupted, Step: st.Name, Retryable: false,
				Message: "The step was interrupted after it started and must not run twice, so it is unknown whether it took effect. Check the result and submit the task again if needed."})
		}
		e.fault(FaultBeforeStep, t.ID, i)
		if st.NeedsSecret != "" {
			if _, have := secrets[st.NeedsSecret]; !have {
				return e.fail(ctx, t, i, st, &Error{Code: CodeSecretUnavailable, Step: st.Name, Retryable: false,
					Message: "The step needs a secret that is only available to the request that started the task."})
			}
		}
		if cancelReq, err := e.setStep(runCtx, t, i, StepRunning, percent(i, len(def.Steps))); err != nil {
			return err
		} else if cancelReq {
			return e.cancelTask(ctx, t, def, i)
		}
		e.fault(FaultStepStarted, t.ID, i)
		if err := st.Run(fence.With(runCtx, fence.Token{Task: t.ID, Epoch: env.lease.Epoch}), env); err != nil {
			// A step that was refused for its lease or its fence did not fail: the run was
			// superseded, or its lease ran out. It stands down and does nothing more in the
			// name of the task (no record, no hook, no alert); see leaseLost.
			if leaseLost(err) {
				return e.standDown(ctx, t, i, st, err)
			}
			var pe *PauseError
			if errors.As(err, &pe) && runCtx.Err() == nil {
				cancelReq, perr := e.pause(ctx, t, i, pe.Reason)
				if perr != nil {
					return perr
				}
				if cancelReq {
					return e.cancelTask(ctx, t, def, i)
				}
				return nil
			}
			var se *SkipError
			if errors.As(err, &se) && runCtx.Err() == nil {
				if ds, ok := e.store.(DetailStore); ok {
					if derr := ds.SetDetail(ctx, t.ID, env.lease, clip(se.Reason)); derr != nil {
						return derr
					}
				}
				cancelReq, serr := e.setStep(ctx, t, i, StepSkipped, percent(i+1, len(def.Steps)))
				if serr != nil {
					return serr
				}
				if cancelReq && i+1 < len(def.Steps) {
					return e.cancelTask(ctx, t, def, i+1)
				}
				continue
			}
			if runCtx.Err() != nil && ctx.Err() == nil {
				return ErrLeaseLost
			}
			if ctx.Err() != nil {
				return ctx.Err() // interrupted, not failed: resumes later
			}
			return e.fail(ctx, t, i, st, &Error{Code: CodeStepFailed, Step: st.Name, Retryable: true, Message: clip(err.Error())})
		}
		e.fault(FaultStepRan, t.ID, i)
		e.clearFenced(t.ID)
		cancelReq, err := e.setStep(ctx, t, i, StepDone, percent(i+1, len(def.Steps)))
		if err != nil {
			return err
		}
		e.fault(FaultStepRecorded, t.ID, i)
		if cancelReq && i+1 < len(def.Steps) {
			return e.cancelTask(ctx, t, def, i+1)
		}
	}
	e.fault(FaultBeforeFinish, t.ID, len(def.Steps))
	err = e.finish(ctx, t, Done, nil)
	e.fault(FaultAfterFinish, t.ID, len(def.Steps))
	return err
}

// leaseGuard cancels a run whose lease may have expired. validUntil is the
// last moment, by the engine's clock, at which the lease is known to hold; a
// successful renewal moves it forward. The timer covers a store call that
// does not return.
type leaseGuard struct {
	e      *Engine
	cancel context.CancelFunc
	mu     sync.Mutex
	until  time.Time
	timer  *time.Timer
}

func newLeaseGuard(e *Engine, cancel context.CancelFunc, until time.Time) *leaseGuard {
	g := &leaseGuard{e: e, cancel: cancel, until: until}
	g.timer = time.AfterFunc(until.Sub(e.now()), g.fire)
	return g
}

func (g *leaseGuard) fire() {
	g.e.log.Warn("task lease not confirmed in time; stopping the step")
	g.cancel()
}

// renewed records a renewal that the store accepted; started is the time on
// the engine's clock before the renewal was sent.
func (g *leaseGuard) renewed(started time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.until = started.Add(g.e.lease - g.e.leaseMargin())
	g.timer.Reset(g.until.Sub(g.e.now()))
}

func (g *leaseGuard) expired() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return !g.e.now().Before(g.until)
}

func (g *leaseGuard) stop() { g.timer.Stop() }

func (e *Engine) renewLoop(ctx context.Context, g *leaseGuard, l Lease, id int64) {
	tick := e.renewEvery
	if tick == nil {
		t := time.NewTicker(e.lease / 3)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			e.fault(FaultLeaseRenew, id, -1)
			started := e.now()
			if err := e.store.Renew(ctx, id, l, e.lease); err != nil {
				if errors.Is(err, ErrLeaseLost) || errors.Is(err, ErrNotFound) {
					e.log.Warn("task lease lost", "task", id)
					g.cancel()
					return
				}
				e.log.Error("renewing task lease", "task", id, "error", err)
				if g.expired() {
					g.fire()
					return
				}
				continue
			}
			g.renewed(started)
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
	te := &Error{Code: CodeCanceled, Message: msg, Retryable: false}
	if err := e.finish(ctx, t, Failed, te); err != nil {
		return err // not recorded under this lease: no hook
	}
	e.onFail(ctx, t, te)
	return nil
}

// pause records the task as paused. The step that paused goes back to pending,
// so the task shows where it will go on. It reports a cancel request that arrived
// meanwhile: the caller then cancels the task instead of leaving it paused.
func (e *Engine) pause(ctx context.Context, t Task, i int, reason string) (bool, error) {
	ps, ok := e.store.(PauseStore)
	if !ok {
		return false, e.fail(ctx, t, i, Step{Name: t.Steps[i].Name}, &Error{Code: CodeStepFailed, Step: t.Steps[i].Name, Message: "The store cannot pause tasks."})
	}
	cancelReq, err := e.setStep(ctx, t, i, StepPending, percent(i, len(t.Steps)))
	if err != nil {
		return false, err
	}
	if cancelReq {
		return true, nil
	}
	c := context.WithoutCancel(ctx)
	return false, e.retry(c, func() error { return ps.Pause(c, t.ID, e.leaseOf(t), clip(reason)) })
}

func (e *Engine) fail(ctx context.Context, t Task, i int, st Step, te *Error) error {
	if i < len(t.Steps) {
		if _, err := e.setStep(ctx, t, i, StepFailed, percent(i, len(t.Steps))); err != nil {
			if leaseLost(err) {
				return ErrLeaseLost
			}
			e.log.Error("recording failed step", "task", t.ID, "error", err)
		}
	}
	e.log.Warn("task step failed", "task", t.ID, "kind", t.Kind, "step", st.Name, "error", te.Message)
	if err := e.finish(ctx, t, Failed, te); err != nil {
		return err // not recorded under this lease: no hook, no alert
	}
	e.onFail(ctx, t, te)
	return nil
}

// onFail runs the definition's cleanup hook for a task that this call has just
// recorded as failed (the record is conditional on the holder and the epoch, so a
// superseded run never gets here). The hook sees the task as recorded: state
// failed and the error.
func (e *Engine) onFail(ctx context.Context, t Task, te *Error) {
	if d, ok := e.def(t.Kind); ok && d.OnFail != nil {
		t.State, t.Error = Failed, te
		d.OnFail(context.WithoutCancel(ctx), t)
	}
}

// leaseLost reports whether err says that the run no longer holds its task: the
// store refused a write named by its lease (ErrLeaseLost, also when the lease only
// ran out and nobody took the task over) or a callee refused its fencing token
// (fence.ErrFenced). That is not a failure of the task. The rule of the engine
// (README of the control plane, "The fencing rule"): a holder that lost the lease
// does nothing more in its name. It writes nothing, starts nothing and runs no
// hook; the task stays as the store has it. When another server holds it now, that
// server decides what happens next. When nobody took it over, the lease is only
// expired and the task is claimed again by the next poll of the leader (the epoch
// grows, the interrupted step runs again from its start, as after a restart).
func leaseLost(err error) bool {
	return errors.Is(err, ErrLeaseLost) || errors.Is(err, fence.ErrFenced)
}

// standDown ends a run that was refused for its lease or its fence: it records nothing and says, in the log, which
// task, step, holder and epoch stood down and why (a quiet end hid a task that looped). A callee that refuses the
// fence while the store still holds this lease for the run is counted, and after MaxFencedRuns runs in a
// row the task fails (the engine's own record, under its lease, then the hook of its kind).
func (e *Engine) standDown(ctx context.Context, t Task, i int, st Step, err error) error {
	fenced := errors.Is(err, fence.ErrFenced) && !errors.Is(err, ErrLeaseLost)
	e.log.Warn("task run stands down", "task", t.ID, "kind", t.Kind, "step", st.Name, "holder", e.holder, "epoch", t.Epoch, "fencedByCallee", fenced, "error", err)
	if !fenced {
		return ErrLeaseLost
	}
	e.fencedMu.Lock()
	if e.fenced == nil {
		e.fenced = map[int64]int{}
	}
	e.fenced[t.ID]++
	n := e.fenced[t.ID]
	e.fencedMu.Unlock()
	if n < MaxFencedRuns {
		return ErrLeaseLost
	}
	e.clearFenced(t.ID)
	return e.fail(ctx, t, i, st, &Error{Code: CodeFenced, Step: st.Name, Retryable: true,
		Message: fmt.Sprintf("A server refused the calls of this task %d times in a row because it has seen a newer run of it than this server's (epoch %d): %s", MaxFencedRuns, t.Epoch, clip(err.Error()))})
}

func (e *Engine) clearFenced(id int64) {
	e.fencedMu.Lock()
	delete(e.fenced, id)
	e.fencedMu.Unlock()
}

func (e *Engine) finish(ctx context.Context, t Task, s State, te *Error) error {
	c := context.WithoutCancel(ctx)
	return e.retry(c, func() error { return e.store.Finish(c, t.ID, e.leaseOf(t), s, te) })
}

// setStep records a step state, retrying while the store is briefly
// unavailable (a task may restart the very instance that holds its row).
func (e *Engine) setStep(ctx context.Context, t Task, i int, state string, pct float64) (bool, error) {
	var cancel bool
	err := e.retry(ctx, func() error {
		var err error
		cancel, err = e.store.SetStep(ctx, t.ID, e.leaseOf(t), i, state, pct)
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

// Register adds a definition after the engine was built (a definition that
// needs the engine's owner, which does not exist yet when the engine is made).
func (e *Engine) Register(d Definition) error {
	if d.Kind == "" || (len(d.Steps) == 0 && d.Dynamic == nil) {
		return fmt.Errorf("task: definition %q needs a kind and steps", d.Kind)
	}
	e.defMu.Lock()
	defer e.defMu.Unlock()
	if _, dup := e.defs[d.Kind]; dup {
		return fmt.Errorf("task: duplicate definition %q", d.Kind)
	}
	e.defs[d.Kind] = d
	return nil
}

func (e *Engine) def(kind string) (Definition, bool) {
	e.defMu.RLock()
	defer e.defMu.RUnlock()
	d, ok := e.defs[kind]
	return d, ok
}

// List returns the newest tasks.
func (e *Engine) List(ctx context.Context, limit int) ([]Task, error) {
	return e.store.List(ctx, limit)
}

// SubmitHeld creates a task and claims it for this engine in one step that no
// poller of Run or RunOnce can interleave with, so that a task whose steps need
// secrets is never started without them. The returned function runs the task to
// its end with the secrets (they are dropped when the run ends).
func (e *Engine) SubmitHeld(ctx context.Context, kind string, subj Subject, input any, createdBy string) (Task, func(ctx context.Context, secrets map[string]string) error, error) {
	e.claimMu.Lock()
	defer e.claimMu.Unlock()
	t, err := e.Submit(ctx, kind, subj, input, createdBy)
	if err != nil {
		return Task{}, nil, err
	}
	started := e.now()
	claimed, err := e.store.ClaimID(ctx, t.ID, e.holder, e.lease)
	if err != nil {
		return Task{}, nil, err
	}
	if claimed == nil {
		return Task{}, nil, errors.New("task: the new task was claimed by someone else")
	}
	run := func(ctx context.Context, secrets map[string]string) error {
		return e.run(ctx, *claimed, started, secrets)
	}
	return *claimed, run, nil
}

// Problem is the answer for the person who asked for the refused task: the contract's 409 codes
// rebalance_in_progress (a server operation refused by a rebalance), failover_in_progress and
// operation_in_progress (a rebalance refused by a change of the servers).
func (e *ExcludedError) Problem() *problem.Problem {
	switch {
	case e.Other == "rebalance":
		return problem.New(problem.RebalanceInProgress, "A rebalance is running; wait until it has finished.")
	case e.Other == "server.failover" || e.Other == "replication.restorePreferredPrimaries":
		return problem.New(problem.FailoverInProgress, "A failover is in progress; the buckets stay where they are until it has finished.")
	default:
		return problem.New(problem.OperationInProgress, "Another operation changes the servers ("+e.Other+"); wait until it has finished.")
	}
}

// guards holds the cancel guards of the registered definitions, by kind, so that the
// answer of the REST layer (Cancellable) and the engine's own check (Cancel) are the same.
var guards sync.Map // kind -> func(Task) bool

func (e *Engine) gateClosed() bool { return e.gate != nil && !e.gate() }

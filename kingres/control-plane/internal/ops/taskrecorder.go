// SPDX-License-Identifier: AGPL-3.0-only

package ops

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// TaskRecorder writes the tasks of instance start and stop. The leader writes
// them to its system database; any other server asks the leader over the
// internal API (PeerRecorder), because only the leader has the task table.
type TaskRecorder interface {
	// Record writes t (see task.RecordStore); ErrSubjectBusy when the instance
	// already has an active task.
	Record(ctx context.Context, t task.Task) (task.Task, error)
	// Settle writes the end of a recorded task.
	Settle(ctx context.Context, t task.Task) error
	// Reserve takes a task id for a task recorded later (0: none).
	Reserve(ctx context.Context) int64
}

// errNoEngine means the task engine does not exist yet.
var errNoEngine = errors.New("ops: the task engine is not running yet")

// engineRecorder records through this server's own task engine.
type engineRecorder struct{ o *Ops }

func (r engineRecorder) engine() (*task.Engine, error) {
	if e := r.o.Engine(); e != nil {
		return e, nil
	}
	return nil, errNoEngine
}

func (r engineRecorder) Record(ctx context.Context, t task.Task) (task.Task, error) {
	e, err := r.engine()
	if err != nil {
		return task.Task{}, err
	}
	return e.Record(ctx, t)
}

func (r engineRecorder) Settle(ctx context.Context, t task.Task) error {
	e, err := r.engine()
	if err != nil {
		return err
	}
	return e.Settle(ctx, t)
}

func (r engineRecorder) Reserve(ctx context.Context) int64 {
	e, err := r.engine()
	if err != nil {
		return 0
	}
	return e.ReserveID(ctx)
}

func (o *Ops) recorder() TaskRecorder {
	if o.Recorder != nil {
		return o.Recorder
	}
	return engineRecorder{o}
}

// PeerCaller makes an internal API call to another server (*peerclient.Client).
type PeerCaller interface {
	Do(ctx context.Context, serverID, method, path string, in, out any) error
}

// PeerRecorder records locally on the leader and through the leader's internal
// API on every other server.
type PeerRecorder struct {
	// Local records on this server (it must be the leader to succeed).
	Local TaskRecorder
	// Leader returns the leader's id and whether it is this server. While this
	// server's own instance is down it may answer from the last known leader.
	Leader func(ctx context.Context) (id string, self bool, err error)
	// Peers calls the leader.
	Peers PeerCaller
}

func (p *PeerRecorder) call(ctx context.Context, path string, in, out any) error {
	id, _, err := p.Leader(ctx)
	if err != nil {
		return err
	}
	if id == "" {
		return errors.New("ops: the manager leader is not known")
	}
	return p.Peers.Do(ctx, id, http.MethodPost, path, in, out)
}

// Record implements TaskRecorder.
func (p *PeerRecorder) Record(ctx context.Context, t task.Task) (task.Task, error) {
	if _, self, err := p.Leader(ctx); err == nil && self {
		return p.Local.Record(ctx, t)
	}
	var res internalapi.TaskRecordResult
	if err := p.call(ctx, "/task/record", toWire(t), &res); err != nil {
		return task.Task{}, mapRemote(err)
	}
	t.ID = res.ID
	return t, nil
}

// Settle implements TaskRecorder.
func (p *PeerRecorder) Settle(ctx context.Context, t task.Task) error {
	if _, self, err := p.Leader(ctx); err == nil && self {
		return p.Local.Settle(ctx, t)
	}
	return mapRemote(p.call(ctx, "/task/settle", toWire(t), nil))
}

// Reserve implements TaskRecorder.
func (p *PeerRecorder) Reserve(ctx context.Context) int64 {
	if _, self, err := p.Leader(ctx); err == nil && self {
		return p.Local.Reserve(ctx)
	}
	var res internalapi.TaskRecordResult
	if err := p.call(ctx, "/task/reserve", struct{}{}, &res); err != nil {
		return 0
	}
	return res.ID
}

// remoteBusy is the code the leader answers when the instance already has an
// active task.
const remoteBusy = "subject_busy"

// remoteFailover is the code the leader answers when the instance belongs to a group that is being
// failed over (migration 0011, KX004): its stop or restart is refused until the failover has ended.
const remoteFailover = "failover_in_progress"

// mapRemote turns the leader's answer into the error the callers know.
func mapRemote(err error) error {
	if err == nil {
		return nil
	}
	var ae *peerclient.APIError
	if errors.As(err, &ae) && ae.Code == remoteBusy {
		return task.ErrSubjectBusy
	}
	if errors.As(err, &ae) && ae.Code == remoteFailover {
		return &task.ExcludedError{Kind: "instance", Other: "server.failover"}
	}
	return err
}

func toWire(t task.Task) internalapi.TaskRecord {
	w := internalapi.TaskRecord{ID: t.ID, Kind: t.Kind, State: string(t.State), SubjectType: t.SubjectType, SubjectID: t.SubjectID,
		CreatedBy: t.CreatedBy, Input: t.Input, Percent: t.Percent, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt}
	for _, s := range t.Steps {
		w.Steps = append(w.Steps, internalapi.TaskStepRecord{Name: s.Name, State: s.State})
	}
	if t.Error != nil {
		w.Error = &internalapi.TaskErrorRecord{Code: t.Error.Code, Message: t.Error.Message, Step: t.Error.Step, Retryable: t.Error.Retryable}
	}
	return w
}

// recordableKinds are the kinds a server may record on the leader.
var recordableKinds = map[string]bool{KindInstanceStart: true, KindInstanceStop: true, KindInstanceRestart: true}

// fromWire validates a task recorded by a peer and converts it.
func fromWire(w internalapi.TaskRecord) (task.Task, *internalapi.CallError) {
	bad := func(msg string) (task.Task, *internalapi.CallError) {
		return task.Task{}, &internalapi.CallError{Status: http.StatusBadRequest, Code: "bad_request", Message: msg}
	}
	if !recordableKinds[w.Kind] || w.SubjectType != "instance" {
		return bad("only the tasks of instance start and stop can be recorded")
	}
	if n, err := strconv.ParseInt(w.SubjectID, 10, 32); err != nil || n < 1 {
		return bad("the subject must be an instance id")
	}
	switch task.State(w.State) {
	case task.Running, task.Done, task.Failed:
	default:
		return bad("the state must be running, done or failed")
	}
	if len(w.Steps) == 0 || len(w.Steps) > 4 || len(w.CreatedBy) > 63 || w.ID < 0 || len(w.Input) > 1024 {
		return bad("the task is not valid")
	}
	t := task.Task{ID: w.ID, Kind: w.Kind, State: task.State(w.State), SubjectType: w.SubjectType, SubjectID: w.SubjectID,
		CreatedBy: w.CreatedBy, Input: w.Input, Percent: w.Percent, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt}
	if t.Percent < 0 || t.Percent > 100 {
		return bad("the percent is not valid")
	}
	if t.CreatedAt.IsZero() || t.UpdatedAt.IsZero() {
		now := time.Now().UTC()
		t.CreatedAt, t.UpdatedAt = now, now
	}
	if len(t.Input) == 0 {
		t.Input = json.RawMessage(`{}`)
	}
	for _, s := range w.Steps {
		switch s.State {
		case task.StepPending, task.StepRunning, task.StepDone, task.StepFailed:
		default:
			return bad("a step state is not valid")
		}
		if len(s.Name) == 0 || len(s.Name) > 100 {
			return bad("a step name is not valid")
		}
		t.Steps = append(t.Steps, task.StepState{Name: s.Name, State: s.State})
	}
	if w.Error != nil {
		t.Error = &task.Error{Code: clipTo(w.Error.Code, 64), Message: clip(w.Error.Message), Step: clipTo(w.Error.Step, 100), Retryable: w.Error.Retryable}
	}
	return t, nil
}

func clipTo(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// HandleTaskCall answers the task calls of other servers; it runs on the
// leader (the internal API only dispatches them there). key is the call, for
// example "POST /task/record"; peer is the verified server id of the caller,
// which may record and settle the tasks of its own instances only.
func (o *Ops) HandleTaskCall(ctx context.Context, key, peer string, body []byte) (any, error) {
	rec := engineRecorder{o}
	busy := &internalapi.CallError{Status: http.StatusConflict, Code: remoteBusy, Message: "The instance already has an unfinished task."}
	unavailable := &internalapi.CallError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "The task cannot be recorded now."}
	switch key {
	case "POST /task/reserve":
		id := rec.Reserve(ctx)
		if id == 0 {
			return nil, unavailable
		}
		return internalapi.TaskRecordResult{ID: id}, nil
	case "POST /task/record", "POST /task/settle":
		var w internalapi.TaskRecord
		if err := json.Unmarshal(body, &w); err != nil {
			return nil, &internalapi.CallError{Status: http.StatusBadRequest, Code: "bad_request", Message: "The body is not a task."}
		}
		t, ce := fromWire(w)
		if ce != nil {
			return nil, ce
		}
		if key == "POST /task/record" {
			if ce := o.ownedBy(ctx, peer, t.SubjectID); ce != nil {
				return nil, ce
			}
		}
		if key == "POST /task/settle" {
			if t.ID == 0 {
				return nil, &internalapi.CallError{Status: http.StatusBadRequest, Code: "bad_request", Message: "A task id is required."}
			}
			cur, err := o.engineGet(ctx, t.ID)
			if err != nil {
				return nil, unavailable
			}
			if !recordableKinds[cur.Kind] {
				return nil, &internalapi.CallError{Status: http.StatusBadRequest, Code: "bad_request", Message: "This task cannot be settled by a peer."}
			}
			// The instance of the stored task counts, not the one the caller names.
			if ce := o.ownedBy(ctx, peer, cur.SubjectID); ce != nil {
				return nil, ce
			}
			if err := rec.Settle(ctx, t); err != nil {
				return nil, unavailable
			}
			return struct{}{}, nil
		}
		got, err := rec.Record(ctx, t)
		switch {
		case errors.Is(err, task.ErrSubjectBusy):
			return nil, busy
		case errors.As(err, new(*task.ExcludedError)):
			return nil, &internalapi.CallError{Status: http.StatusConflict, Code: remoteFailover, Message: "The instance belongs to a shard group that is being failed over."}
		case err != nil:
			return nil, unavailable
		}
		return internalapi.TaskRecordResult{ID: got.ID}, nil
	}
	return nil, &internalapi.CallError{Status: http.StatusNotFound, Code: "not_found", Message: "No such call."}
}

// ownedBy refuses a call about an instance that is not one of the peer's.
func (o *Ops) ownedBy(ctx context.Context, peer, instanceID string) *internalapi.CallError {
	if o.InstanceOwner == nil {
		return &internalapi.CallError{Status: http.StatusForbidden, Code: "forbidden", Message: "Tasks of instances cannot be recorded by a peer here."}
	}
	own, err := o.InstanceOwner(ctx, instanceID, peer)
	switch {
	case err != nil:
		return &internalapi.CallError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "The task cannot be recorded now."}
	case !own:
		return &internalapi.CallError{Status: http.StatusForbidden, Code: "forbidden", Message: "The instance is not one of the calling server's."}
	}
	return nil
}

func (o *Ops) engineGet(ctx context.Context, id int64) (task.Task, error) {
	e := o.Engine()
	if e == nil {
		return task.Task{}, errNoEngine
	}
	return e.Get(ctx, id)
}

// NewPeerRecorder returns the recorder of a server that may or may not be the
// leader: leader says who it is and peers calls it.
func (o *Ops) NewPeerRecorder(leader func(ctx context.Context) (id string, self bool, err error), peers PeerCaller) TaskRecorder {
	return &PeerRecorder{Local: engineRecorder{o}, Leader: leader, Peers: peers}
}

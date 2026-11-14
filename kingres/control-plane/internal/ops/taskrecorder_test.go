// SPDX-License-Identifier: PostgreSQL

package ops

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

func engineOps(t *testing.T) *Ops {
	t.Helper()
	o := &Ops{Log: slog.New(slog.DiscardHandler)}
	eng, err := task.NewEngine(task.NewMemStore(), o.InstanceDefinitions(), task.Options{Holder: "t", Logger: slog.New(slog.DiscardHandler), Poll: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	o.SetEngine(eng)
	return o
}

type recordingPeers struct {
	calls []string
	in    []any
	err   error
	id    int64
}

func (p *recordingPeers) Do(_ context.Context, serverID, method, path string, in, out any) error {
	p.calls = append(p.calls, method+" "+serverID+" "+path)
	p.in = append(p.in, in)
	if p.err != nil {
		return p.err
	}
	if r, ok := out.(*internalapi.TaskRecordResult); ok {
		r.ID = p.id
	}
	return nil
}

// A follower records through the leader; the leader (or a standalone server)
// records in its own engine.
func TestPeerRecorderGoesToTheLeader(t *testing.T) {
	o := engineOps(t)
	peers := &recordingPeers{id: 77}
	self := false
	rec := o.NewPeerRecorder(func(context.Context) (string, bool, error) { return "leader.example", self, nil }, peers)
	in := newInstanceTask(KindInstanceStop, stopStep, "3", "alice")
	got, err := rec.Record(context.Background(), in)
	if err != nil || got.ID != 77 || len(peers.calls) != 1 || peers.calls[0] != "POST leader.example /task/record" {
		t.Fatalf("remote record: %+v %v %v", got, err, peers.calls)
	}
	wire := peers.in[0].(internalapi.TaskRecord)
	if wire.Kind != KindInstanceStop || wire.SubjectID != "3" || wire.State != "running" || len(wire.Steps) != 1 || wire.CreatedBy != "alice" {
		t.Errorf("wire task %+v", wire)
	}
	if err := rec.Settle(context.Background(), ended(got, nil)); err != nil || peers.calls[1] != "POST leader.example /task/settle" {
		t.Errorf("remote settle: %v %v", err, peers.calls)
	}
	if id := rec.Reserve(context.Background()); id != 77 || peers.calls[2] != "POST leader.example /task/reserve" {
		t.Errorf("remote reserve: %d %v", id, peers.calls)
	}
	// The leader's answer "busy" is the engine's error.
	peers.err = &peerclient.APIError{Status: 409, Code: "subject_busy", Message: "x"}
	if _, err := rec.Record(context.Background(), in); !errors.Is(err, task.ErrSubjectBusy) {
		t.Errorf("busy: %v", err)
	}
	// Any other failure is an error the caller keeps the task in memory for.
	peers.err = errors.New("unreachable")
	if _, err := rec.Record(context.Background(), in); err == nil || errors.Is(err, task.ErrSubjectBusy) {
		t.Errorf("unreachable leader: %v", err)
	}
	// On the leader nothing goes over the network.
	self = true
	peers.calls = nil
	got, err = rec.Record(context.Background(), in)
	if err != nil || got.ID == 0 || len(peers.calls) != 0 {
		t.Errorf("local record: %+v %v %v", got, err, peers.calls)
	}
}

func wireOf(t *testing.T, mut func(*internalapi.TaskRecord)) []byte {
	t.Helper()
	w := toWire(newInstanceTask(KindInstanceStart, startStep, "2", "bob"))
	if mut != nil {
		mut(&w)
	}
	b, _ := json.Marshal(w)
	return b
}

// The leader records what a peer asks only for the instance kinds, and refuses
// everything else.
func TestHandleTaskCallValidatesWhatAPeerMayRecord(t *testing.T) {
	o := engineOps(t)
	ctx := context.Background()
	res, err := o.HandleTaskCall(ctx, "POST /task/record", wireOf(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	id := res.(internalapi.TaskRecordResult).ID
	got, err := o.Engine().Get(ctx, id)
	if err != nil || got.Kind != KindInstanceStart || got.SubjectID != "2" || got.State != task.Running {
		t.Fatalf("recorded: %+v %v", got, err)
	}
	// The same subject is busy.
	var ce *internalapi.CallError
	if _, err := o.HandleTaskCall(ctx, "POST /task/record", wireOf(t, nil)); !errors.As(err, &ce) || ce.Status != 409 || ce.Code != "subject_busy" {
		t.Errorf("busy: %v", err)
	}
	// Settle it.
	fin := ended(got, nil)
	if _, err := o.HandleTaskCall(ctx, "POST /task/settle", wireOf(t, func(w *internalapi.TaskRecord) { *w = toWire(fin) })); err != nil {
		t.Fatal(err)
	}
	if back, _ := o.Engine().Get(ctx, id); back.State != task.Done {
		t.Errorf("settled: %+v", back)
	}
	for name, mut := range map[string]func(*internalapi.TaskRecord){
		"a foreign kind":        func(w *internalapi.TaskRecord) { w.Kind = "database.drop" },
		"a foreign subject":     func(w *internalapi.TaskRecord) { w.SubjectType = "database"; w.SubjectID = "shop" },
		"a non-numeric subject": func(w *internalapi.TaskRecord) { w.SubjectID = "../x" },
		"a pending state":       func(w *internalapi.TaskRecord) { w.State = "pending" },
		"no steps":              func(w *internalapi.TaskRecord) { w.Steps = nil },
		"a bad step state":      func(w *internalapi.TaskRecord) { w.Steps[0].State = "skipped" },
		"a bad percent":         func(w *internalapi.TaskRecord) { w.Percent = 500 },
		"a negative id":         func(w *internalapi.TaskRecord) { w.ID = -4 },
	} {
		if _, err := o.HandleTaskCall(ctx, "POST /task/record", wireOf(t, mut)); !errors.As(err, &ce) || ce.Status != 400 {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A task of another kind cannot be settled by a peer, nor one that does not exist.
	foreign := newInstanceTask("database.drop", "drop", "9", "me")
	foreign.SubjectType = "database"
	foreign, err = o.Engine().Record(ctx, foreign)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.HandleTaskCall(ctx, "POST /task/settle", wireOf(t, func(w *internalapi.TaskRecord) { w.ID = foreign.ID })); !errors.As(err, &ce) || ce.Status != 400 {
		t.Errorf("settle of a task of another kind: %v", err)
	}
	if _, err := o.HandleTaskCall(ctx, "POST /task/settle", wireOf(t, func(w *internalapi.TaskRecord) { w.ID = 4242 })); !errors.As(err, &ce) || ce.Status != 503 {
		t.Errorf("settle of an unknown task: %v", err)
	}
	if _, err := o.HandleTaskCall(ctx, "POST /task/nothing", nil); !errors.As(err, &ce) || ce.Status != 404 {
		t.Errorf("unknown call: %v", err)
	}
	if res, err := o.HandleTaskCall(ctx, "POST /task/reserve", nil); err != nil || res.(internalapi.TaskRecordResult).ID == 0 {
		t.Errorf("reserve: %v %v", res, err)
	}
}

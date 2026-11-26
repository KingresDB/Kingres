// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

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
	// The tests' instances belong to the server "db2" (and to nobody else).
	o := &Ops{Log: slog.New(slog.DiscardHandler), InstanceOwner: func(_ context.Context, _, server string) (bool, error) { return server == "db2", nil }}
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
	res, err := o.HandleTaskCall(ctx, "POST /task/record", "db2", wireOf(t, nil))
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
	if _, err := o.HandleTaskCall(ctx, "POST /task/record", "db2", wireOf(t, nil)); !errors.As(err, &ce) || ce.Status != 409 || ce.Code != "subject_busy" {
		t.Errorf("busy: %v", err)
	}
	// Settle it.
	fin := ended(got, nil)
	if _, err := o.HandleTaskCall(ctx, "POST /task/settle", "db2", wireOf(t, func(w *internalapi.TaskRecord) { *w = toWire(fin) })); err != nil {
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
		if _, err := o.HandleTaskCall(ctx, "POST /task/record", "db2", wireOf(t, mut)); !errors.As(err, &ce) || ce.Status != 400 {
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
	if _, err := o.HandleTaskCall(ctx, "POST /task/settle", "db2", wireOf(t, func(w *internalapi.TaskRecord) { w.ID = foreign.ID })); !errors.As(err, &ce) || ce.Status != 400 {
		t.Errorf("settle of a task of another kind: %v", err)
	}
	if _, err := o.HandleTaskCall(ctx, "POST /task/settle", "db2", wireOf(t, func(w *internalapi.TaskRecord) { w.ID = 4242 })); !errors.As(err, &ce) || ce.Status != 503 {
		t.Errorf("settle of an unknown task: %v", err)
	}
	if _, err := o.HandleTaskCall(ctx, "POST /task/nothing", "db2", nil); !errors.As(err, &ce) || ce.Status != 404 {
		t.Errorf("unknown call: %v", err)
	}
	if res, err := o.HandleTaskCall(ctx, "POST /task/reserve", "db2", nil); err != nil || res.(internalapi.TaskRecordResult).ID == 0 {
		t.Errorf("reserve: %v %v", res, err)
	}
}

// CP2-20: a peer records and settles the tasks of its own instances only, and a
// server that has no way to tell refuses.
func TestHandleTaskCallBindsTheCallToTheCallersInstances(t *testing.T) {
	o := engineOps(t)
	ctx := context.Background()
	var ce *internalapi.CallError
	if _, err := o.HandleTaskCall(ctx, "POST /task/record", "db3", wireOf(t, nil)); !errors.As(err, &ce) || ce.Status != 403 {
		t.Errorf("recording the task of another server's instance: %v", err)
	}
	res, err := o.HandleTaskCall(ctx, "POST /task/record", "db2", wireOf(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	id := res.(internalapi.TaskRecordResult).ID
	got, _ := o.Engine().Get(ctx, id)
	// Settling goes by the stored task's instance, whatever subject the caller writes.
	settle := func(peer string) error {
		_, err := o.HandleTaskCall(ctx, "POST /task/settle", peer, wireOf(t, func(w *internalapi.TaskRecord) { *w = toWire(ended(got, nil)); w.SubjectID = "9" }))
		return err
	}
	if err := settle("db3"); !errors.As(err, &ce) || ce.Status != 403 {
		t.Errorf("settling the task of another server's instance: %v", err)
	}
	if back, _ := o.Engine().Get(ctx, id); back.State != task.Running {
		t.Errorf("a refused settle changed the task: %+v", back)
	}
	if err := settle("db2"); err != nil {
		t.Errorf("the owner settles its task: %v", err)
	}
	o.InstanceOwner = nil
	if _, err := o.HandleTaskCall(ctx, "POST /task/record", "db2", wireOf(t, nil)); !errors.As(err, &ce) || ce.Status != 403 {
		t.Errorf("no way to tell who owns an instance: %v", err)
	}
}

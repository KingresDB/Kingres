// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ledgersign_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerkeys"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgersign"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// deadStore stops every write of the task engine once the simulated process has crashed, so that nothing the
// engine does after the crash point is recorded (the same device as the crash test of internal/task).
type deadStore struct {
	task.Store
	dead atomic.Bool
}

var errDead = errors.New("process is dead")

func (d *deadStore) Renew(ctx context.Context, id int64, l task.Lease, x time.Duration) error {
	if d.dead.Load() {
		return errDead
	}
	return d.Store.Renew(ctx, id, l, x)
}

func (d *deadStore) SetStep(ctx context.Context, id int64, l task.Lease, i int, s string, p float64) (bool, error) {
	if d.dead.Load() {
		return false, errDead
	}
	return d.Store.SetStep(ctx, id, l, i, s, p)
}

func (d *deadStore) Finish(ctx context.Context, id int64, l task.Lease, s task.State, e *task.Error) error {
	if d.dead.Load() {
		return errDead
	}
	return d.Store.Finish(ctx, id, l, s, e)
}

type crashed struct{}

// A scenario prepares a cluster, submits one task through the operation of the contract and says what must be
// true once the task is done, however many times it was killed.
type scenario struct {
	name  string
	steps int
	// prepare brings the cluster to the state the task starts from and returns the task to run.
	prepare func(t *testing.T, e *env) task.Task
	// check asserts the final state.
	check func(t *testing.T, e *env, tk task.Task)
}

func scenarios() []scenario {
	return []scenario{
		{name: "create", steps: 4,
			prepare: func(t *testing.T, e *env) task.Task {
				tk, err := e.svc.CreateKey(context.Background(), "a", api.CreateLedgerSigningKeyRequest{})
				if err != nil {
					t.Fatal(err)
				}
				return tk
			},
			check: func(t *testing.T, e *env, _ task.Task) { checkSet(t, e, 1, 0, 0, 1) }},
		{name: "rotate", steps: 5,
			prepare: func(t *testing.T, e *env) task.Task {
				e.create(0)
				e.clk.Add(time.Hour)
				tk, err := e.svc.CreateKey(context.Background(), "a", api.CreateLedgerSigningKeyRequest{})
				if err != nil {
					t.Fatal(err)
				}
				return tk
			},
			check: func(t *testing.T, e *env, _ task.Task) { checkSet(t, e, 2, 0, 0, 2) }},
		{name: "retire", steps: 4,
			prepare: func(t *testing.T, e *env) task.Task {
				e.create(0)
				e.clk.Add(time.Hour)
				e.create(1)
				e.clk.Add(25 * time.Hour)
				tk, err := e.svc.RetireKey(context.Background(), "a", load(t, e).Keys[0].Fingerprint())
				if err != nil {
					t.Fatal(err)
				}
				return tk
			},
			check: func(t *testing.T, e *env, _ task.Task) { checkSet(t, e, 1, 1, 0, 3) }},
		{name: "revoke", steps: 4,
			prepare: func(t *testing.T, e *env) task.Task {
				e.create(0)
				e.clk.Add(time.Hour)
				e.create(0)
				fp := load(t, e).Keys[0].Fingerprint()
				tk, err := e.svc.RevokeKey(context.Background(), "a", fp, api.RevokeLedgerSigningKeyRequest{CompromisedSince: e.clk.Now(), Reason: "crash test", Confirm: fp})
				if err != nil {
					t.Fatal(err)
				}
				return tk
			},
			check: func(t *testing.T, e *env, _ task.Task) { checkSet(t, e, 1, 0, 1, 3) }},
		{name: "distribute", steps: 2,
			prepare: func(t *testing.T, e *env) task.Task {
				e.create(0)
				e.hold = []string{"s2"} // s3 stops being a holder, s2 stays
				tk, err := e.eng.Submit(context.Background(), "ledger.key.distribute", task.Subject{Type: "ledgerKey", ID: "signing"}, nil, "a")
				if err != nil {
					t.Fatal(err)
				}
				return tk
			},
			check: func(t *testing.T, e *env, _ task.Task) {
				checkSet(t, e, 1, 0, 0, 1)
				if e.peers["s3"].kr.Has(load(t, e).Keys[0].Fingerprint()) {
					t.Error("s3 still has the key file")
				}
			}},
	}
}

// checkSet asserts the invariants of a finished (or recovered) key set: the counts, a valid set, the trust file the
// library builds from it, one event of each type per key in order, and the key files exactly where they belong.
func checkSet(t *testing.T, e *env, active, retired, revoked, events int) {
	t.Helper()
	s := load(t, e)
	var a, r, v, n int
	for _, k := range s.Keys {
		switch k.State {
		case ledgerkeys.StateActive:
			a++
		case ledgerkeys.StateRetired:
			r++
		case ledgerkeys.StateRevoked:
			v++
		case ledgerkeys.StateNext:
			n++
		}
	}
	if a != active || r != retired || v != revoked || n != 0 {
		t.Errorf("keys: %d active %d retired %d revoked %d next, want %d %d %d 0", a, r, v, n, active, retired, revoked)
	}
	if len(s.Events) != events {
		t.Errorf("%d events, want %d", len(s.Events), events)
	}
	for i, ev := range s.Events {
		if ev.Seq != int64(i) {
			t.Errorf("event %d has the number %d", i, ev.Seq)
		}
	}
	if err := s.Set().Validate(); err != nil {
		t.Errorf("the set is not valid: %v", err)
	}
	want, err := ledgerkeys.TrustFile(s.Cluster.OriginPrefix, s.Set(), ledgerkeys.TrustOptions{})
	if err != nil || string(want) != string(s.Cluster.TrustFile) {
		t.Errorf("the stored trust file is not the one built from the set: %v", err)
	}
	// Files: the leader and every current holder have exactly the next and active keys; a server that is
	// no holder has none.
	wantFiles := map[string]bool{}
	for _, k := range s.Keys {
		if k.State == ledgerkeys.StateActive || k.State == ledgerkeys.StateNext {
			wantFiles[k.Fingerprint()] = true
		}
	}
	isHolder := map[string]bool{"s1": true}
	for _, h := range e.hold {
		isHolder[h] = true
	}
	check := func(id string, fps []string) {
		got := map[string]bool{}
		for _, fp := range fps {
			got[fp] = true
		}
		if isHolder[id] {
			if len(got) != len(wantFiles) {
				t.Errorf("%s has %d key files, want %d", id, len(got), len(wantFiles))
			}
			for fp := range wantFiles {
				if !got[fp] {
					t.Errorf("%s lacks the file of %s", id, fp[:16])
				}
			}
		} else if len(got) != 0 {
			t.Errorf("%s is no holder and has %d key files", id, len(got))
		}
	}
	fps, _ := e.kr.List()
	check("s1", fps)
	for id, p := range e.peers {
		fps, _ := p.kr.List()
		check(id, fps)
	}
}

// crashRun prepares the scenario on e, kills the engine at the crash point of a step, restarts the daemon over the same
// store, directories and task rows, lets the lease run out and runs the task again: it must end done in the state the
// scenario says.
func crashRun(t *testing.T, e *env, sc scenario, point string, step int) {
	t.Helper()
	tk := sc.prepare(t, e)
	ds := &deadStore{Store: e.tasks}
	e.store2 = ds
	e.rebuild() // the engine that will be killed
	e.fault = func(p string, _ int64, st int) {
		if p == point && st == step {
			ds.dead.Store(true)
			panic(crashed{})
		}
	}
	reached := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(crashed); !ok {
					panic(r)
				}
				reached = true
			}
		}()
		_ = e.eng.RunNow(context.Background(), tk.ID, nil)
	}()
	if !reached {
		t.Fatal("the crash point was never reached")
	}
	// The daemon restarts: a new service and engine over the same store, directories and task rows, and the lease of
	// the dead run runs out.
	e.store2, e.fault = nil, nil
	e.rebuild()
	e.clk.Add(31 * time.Second)
	if ran, err := e.eng.RunOnce(context.Background()); !ran || err != nil {
		t.Fatalf("the next run: %v %v", ran, err)
	}
	got, _ := e.eng.Get(context.Background(), tk.ID)
	if got.State != task.Done {
		t.Fatalf("the task is %s: %+v", got.State, got.Error)
	}
	sc.check(t, e, got)
	// Nothing of a key leaked while the process died: the logs hold no secret.
	e.noSecret("the log", []byte(e.logs.String()))
}

func TestKillingKeyTasksAtEveryCrashPointOfTheEngineLeavesAConsistentSetAndTheNextRunFinishes(t *testing.T) {
	for _, sc := range scenarios() {
		for _, point := range []string{task.FaultBeforeStep, task.FaultStepStarted, task.FaultStepRan, task.FaultStepRecorded, task.FaultBeforeFinish} {
			for k := 0; k < sc.steps; k++ {
				if point == task.FaultBeforeFinish && k > 0 {
					continue // one crash before the finish
				}
				step := k
				if point == task.FaultBeforeFinish {
					step = sc.steps
				}
				t.Run(fmt.Sprintf("%s/%s#%d", sc.name, point, k), func(t *testing.T) {
					crashRun(t, newEnv(t, memFactory, withPeers("s2", "s3")), sc, point, step)
				})
			}
		}
	}
}

// A crash inside a step, between the file and the row, and between the signature and the commit, is repaired by
// the step that runs again: the key file is adopted, the event is made once.
func TestAKillInsideAStepDoesNotMakeASecondKeyOrASecondEvent(t *testing.T) {
	for _, killAt := range []string{"generate", "event"} {
		t.Run(killAt, func(t *testing.T) {
			e := newEnv(t, memFactory, withPeers("s2"))
			tk, err := e.svc.CreateKey(context.Background(), "a", api.CreateLedgerSigningKeyRequest{})
			if err != nil {
				t.Fatal(err)
			}
			killed := false
			e.mem.Hook = func(ch ledgersign.Change) error {
				if killed {
					return nil
				}
				if (killAt == "generate" && len(ch.Keys) > 0 && len(ch.Events) == 0) || (killAt == "event" && len(ch.Events) > 0) {
					killed = true
					return errors.New("killed")
				}
				return nil
			}
			_ = e.eng.RunNow(context.Background(), tk.ID, nil)
			got, _ := e.eng.Get(context.Background(), tk.ID)
			if got.State != task.Failed {
				t.Fatalf("the killed run: %s", got.State)
			}
			e.mem.Hook = nil
			e.mustDone(e.run("ledger.key.create", nil))
			checkSet(t, e, 1, 0, 0, 1)
		})
	}
}

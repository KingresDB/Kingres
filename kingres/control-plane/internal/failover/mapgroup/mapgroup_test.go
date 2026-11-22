// SPDX-License-Identifier: AGPL-3.0-only

package mapgroup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover/journal"
)

// world models three servers: s1 ran the primary of the map group and the leader (it is dead), s2 (this server) and s3 host the
// standbys 10 and 11 of group 0 (nodes 4 and 5).
type world struct {
	mu                        sync.Mutex
	pulls                     int
	pullCheckpoint, pullState string
	pullErr                   error
	refusedErr                error  // telling the old leader fails
	behind                    int    // the callee answers standby_behind this many times before it promotes
	onPromote                 func() // runs inside the call that promotes (a test gives the bootstrap up there)
	resumeOldErr              error  // the answer of the call that asks the old leader to end its hold
	onResumeOld               func() // runs inside the call that asks the old leader to end its hold
	promoteErr                error  // the callee refuses a promotion that carries a proof
	promoteReqs               []failover.PromoteRequest
	freezeErr                 error // FreezeLocal fails from the second call on (the agent was lost with a restart)
	freezes                   int
	log                       []string
	self                      string
	term                      int64
	primary                   string
	standbys                  []failover.StandbyRec
	pos                       map[string]failover.Position
	posErr                    map[string]error
	live                      map[string]string
	oldAlive                  bool
	promoted                  map[string]bool
	leaseHeld                 bool
	rebalance                 int
	taskID                    int64
	recorded                  int
	alerts                    []string
	facts                     failover.GroupFacts
	nodes                     []failover.Node
	noCA                      bool
}

func newWorld() *world {
	return &world{
		self: "s2", term: 5, primary: "s1",
		standbys: []failover.StandbyRec{
			{ID: 10, Group: 0, Server: "s2", InstanceID: 100, Port: 5500, State: "ready", CatalogNode: 4},
			{ID: 11, Group: 0, Server: "s3", InstanceID: 100, Port: 5501, State: "ready", CatalogNode: 5},
		},
		pos: map[string]failover.Position{
			"s2": {Server: "s2", InRecovery: true, Timeline: 1, ReceiveLsn: "0/5000000", ReplayLsn: "0/5000000", SystemIdentifier: "9"},
			"s3": {Server: "s3", InRecovery: true, Timeline: 1, ReceiveLsn: "0/4000000", ReplayLsn: "0/4000000", SystemIdentifier: "9"},
		},
		posErr:   map[string]error{},
		live:     map[string]string{"s1": "dead", "s2": "live", "s3": "live"},
		promoted: map[string]bool{},
		facts:    failover.GroupFacts{SyncStreaming: true, LastKnownAge: time.Second, PrimaryPlanned: 1},
		nodes: []failover.Node{
			{ID: 1, Group: 0, Host: "s1", Port: 5400, Role: "primary"},
			{ID: 4, Group: 0, Host: "s2", Port: 5500, Role: "standby"},
			{ID: 5, Group: 0, Host: "s3", Port: 5501, Role: "standby"},
		},
	}
}

func (w *world) rec(f string, a ...any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.log = append(w.log, fmt.Sprintf(f, a...))
}

func (w *world) index(prefix string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i, l := range w.log {
		if strings.HasPrefix(l, prefix) {
			return i
		}
	}
	return -1
}

func (w *world) count(prefix string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, l := range w.log {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

type ports struct{ w *world }

func (p ports) Self() string { return p.w.self }
func (p ports) CatalogTerm(context.Context) (int64, string, error) {
	return p.w.term, p.w.primary, nil
}
func (p ports) Members(context.Context) ([]string, error) { return []string{"s1", "s2", "s3"}, nil }
func (p ports) Live(_ context.Context, s string) string   { return p.w.live[s] }
func (p ports) Standbys(context.Context) ([]failover.StandbyRec, error) {
	return append([]failover.StandbyRec(nil), p.w.standbys...), nil
}
func (p ports) Facts(context.Context) (failover.GroupFacts, error) { return p.w.facts, nil }
func (p ports) Position(_ context.Context, s string) (failover.Position, error) {
	if err := p.w.posErr[s]; err != nil {
		return failover.Position{}, err
	}
	pos := p.w.pos[s]
	if p.w.promoted[s] {
		pos.InRecovery = false
	}
	return pos, nil
}
func (p ports) State(_ context.Context, s string) (failover.State, error) {
	if s == "s1" && !p.w.oldAlive {
		return failover.State{}, failover.ErrUnreachable
	}
	f := false
	return failover.State{Server: s, Instances: []failover.StateInstance{{ID: 1, Group: 0, Running: true, InRecovery: &f, Role: "primary"}}}, nil
}
func (p ports) Demote(_ context.Context, s string, r failover.DemoteRequest) (failover.DemoteAnswer, error) {
	p.w.rec("demote %s to=%s", s, r.TransferTo)
	return failover.DemoteAnswer{Stopped: true, Transferred: true}, nil
}
func (p ports) Lease(_ context.Context, s string, r failover.LeaseRequest) (failover.LeaseAnswer, error) {
	switch {
	case r.TransferTo != "":
		p.w.rec("lease-transfer %s to %s", s, r.TransferTo)
		return failover.LeaseAnswer{Transferred: true, Holder: r.TransferTo}, nil
	default:
		p.w.rec("lease-%s %s", r.Role, s)
		return failover.LeaseAnswer{Held: true}, nil
	}
}
func (p ports) Promote(_ context.Context, s string, r failover.PromoteRequest) (failover.PromoteAnswer, error) {
	p.w.mu.Lock()
	p.w.mu.Unlock()
	if p.w.onPromote != nil {
		p.w.onPromote()
	}
	p.w.mu.Lock()
	p.w.promoteReqs = append(p.w.promoteReqs, r)
	if p.w.promoteErr != nil && r.Checkpoint != "" {
		p.w.mu.Unlock()
		return failover.PromoteAnswer{}, p.w.promoteErr
	}
	if p.w.behind > 0 && r.Checkpoint != "" {
		p.w.behind--
		p.w.mu.Unlock()
		return failover.PromoteAnswer{}, &failover.CallError{Status: 409, Code: "standby_behind", Message: "the standby is behind"}
	}
	already := p.w.promoted[s]
	p.w.promoted[s] = true
	p.w.mu.Unlock()
	if already {
		p.w.rec("promote-already %s", s)
		return failover.PromoteAnswer{Status: "already", Timeline: 2}, nil
	}
	p.w.rec("promote %s", s)
	return failover.PromoteAnswer{Status: "promoted", Timeline: 2, Lsn: "0/5000100"}, nil
}
func (p ports) FreezeLocal(context.Context, time.Duration) error {
	p.w.rec("freeze")
	p.w.mu.Lock()
	defer p.w.mu.Unlock()
	p.w.freezes++
	if p.w.freezes > 1 {
		return p.w.freezeErr
	}
	return nil
}
func (p ports) Continue(_ context.Context, s string, j journal.Journal) error {
	p.w.rec("continue %s id=%s", s, j.ID)
	return nil
}
func (p ports) Catalog() failover.Catalog { return fcat{p.w} }
func (p ports) PauseRebalance(context.Context) (int, error) {
	p.w.rec("pause-rebalance")
	return p.w.rebalance, nil
}
func (p ports) RecordTask(_ context.Context, j journal.Journal) (int64, error) {
	p.w.mu.Lock()
	defer p.w.mu.Unlock()
	p.w.recorded++
	p.w.taskID = 77
	p.w.log = append(p.w.log, "record-task")
	return p.w.taskID, nil
}
func (p ports) MoveSystem(context.Context) error             { p.w.rec("move-system"); return nil }
func (p ports) CheckAuthority(context.Context) (bool, error) { return !p.w.noCA, nil }
func (p ports) Alert(_ context.Context, kind, _, _, _ string, raise bool) {
	if raise {
		p.w.mu.Lock()
		p.w.alerts = append(p.w.alerts, kind)
		p.w.mu.Unlock()
	}
}

type fcat struct{ w *world }

func (c fcat) Databases(context.Context) ([]string, error) { return []string{failover.SystemDB}, nil }
func (c fcat) Nodes(context.Context, string) ([]failover.Node, error) {
	return append([]failover.Node(nil), c.w.nodes...), nil
}
func (c fcat) Status(context.Context, string) (int64, bool, error) { return c.w.term, false, nil }
func (c fcat) PromoteNode(_ context.Context, db string, id int32) (failover.PromoteResult, error) {
	c.w.rec("promote_node %s %d", db, id)
	c.w.mu.Lock()
	defer c.w.mu.Unlock()
	if c.w.primary != c.w.self {
		c.w.primary = c.w.self
		c.w.term++
	}
	return failover.PromoteResult{Status: "already", Term: c.w.term, Final: true}, nil
}
func (c fcat) RecoverPrepared(context.Context, string) error { return nil }
func (c fcat) CheckCluster(context.Context, string) ([]failover.Incident, error) {
	return nil, nil
}

func (w *world) runner(t *testing.T) (*Runner, *journal.Store) {
	t.Helper()
	js, err := journal.Open(tempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_000_000, 0)
	return &Runner{P: ports{w}, Journal: js,
		Now:           func() time.Time { return now },
		Sleep:         func(ctx context.Context, d time.Duration) error { now = now.Add(d); return ctx.Err() },
		PromoteNodeOp: failover.PromoteOptions{Pause: time.Millisecond}}, js
}

func req() Request { return Request{OldServer: "s1", Trigger: "manual", RequestedBy: "admin"} }

func TestTheMapGroupIsPromotedInTheOrderOfTheDesign(t *testing.T) {
	w := newWorld()
	r, js := w.runner(t)
	res, err := r.Run(context.Background(), req())
	if err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(w.log, "\n"))
	}
	order := []string{"freeze", "lease-fence s2", "lease-holder s2", "promote s2", "promote_node kingres_system 4", "move-system", "pause-rebalance", "record-task"}
	last := -1
	for _, o := range order {
		i := w.index(o)
		if i < 0 || i < last {
			t.Fatalf("%q out of order (at %d, after %d):\n%s", o, i, last, strings.Join(w.log, "\n"))
		}
		last = i
	}
	if res.TaskID != 77 || js.Active() || !js.Gate() {
		t.Fatalf("result %+v active=%v", res, js.Active())
	}
	for _, s := range journal.Order {
		if !js.IsDone(s) {
			t.Errorf("step %q is not done", s)
		}
	}
}

// Every step is idempotent and survives the end of kingresd at any boundary: after a crash the journal is read and the work goes
// on without a second promotion and without a second task.
func TestTheBootstrapSurvivesACrashAtEveryStepBoundary(t *testing.T) {
	for _, step := range journal.Order {
		for _, where := range []string{"before", "after"} {
			point := step + ":" + where
			t.Run(point, func(t *testing.T) {
				w := newWorld()
				r, js := w.runner(t)
				crashed := false
				r.Fault = func(p string) error {
					if p == point && !crashed {
						crashed = true
						return errors.New("kingresd ended")
					}
					return nil
				}
				_, err := r.Run(context.Background(), req())
				if step == journal.StepDecide {
					// B1 has no function of its own: the journal is begun by Decide and nothing is done.
					if err != nil && !crashed {
						t.Fatal(err)
					}
				} else if err == nil {
					t.Fatalf("the crash at %s did not end the run", point)
				}
				if !js.Active() && !js.IsDone(journal.StepGateOpen) {
					t.Fatal("the journal is lost")
				}
				// a restart: the journal is read again from disk by a new store
				res, ok, err := r.Resume(context.Background())
				if crashed && !ok && !js.IsDone(journal.StepGateOpen) {
					t.Fatal("nothing to resume")
				}
				if err != nil {
					t.Fatalf("resume: %v\n%s", err, strings.Join(w.log, "\n"))
				}
				if w.count("promote ")+w.count("promote-already") < 1 {
					t.Fatalf("the standby was never promoted:\n%s", strings.Join(w.log, "\n"))
				}
				if w.count("promote s2") != 1 {
					t.Fatalf("promoted %d times:\n%s", w.count("promote s2"), strings.Join(w.log, "\n"))
				}
				if w.recorded != 1 {
					t.Fatalf("the task was recorded %d times", w.recorded)
				}
				_ = res
				if js.Active() {
					t.Fatalf("the gate is still closed:\n%s", strings.Join(w.log, "\n"))
				}
			})
		}
	}
}

func TestAnotherCandidateThatIsBetterGetsTheLeaseAndTheJournal(t *testing.T) {
	w := newWorld()
	w.pos["s3"] = failover.Position{Server: "s3", InRecovery: true, Timeline: 1, ReceiveLsn: "0/9000000", ReplayLsn: "0/9000000", SystemIdentifier: "9"}
	r, js := w.runner(t)
	res, err := r.Run(context.Background(), req())
	if err != nil {
		t.Fatal(err)
	}
	if res.HandedTo != "s3" {
		t.Fatalf("result %+v\n%s", res, strings.Join(w.log, "\n"))
	}
	if w.index("promote s2") >= 0 || w.index("lease-transfer s2 to s3") < 0 || w.index("continue s3") < 0 {
		t.Fatalf("hand-over:\n%s", strings.Join(w.log, "\n"))
	}
	if w.index("lease-transfer s2 to s3") > w.index("continue s3") {
		t.Fatal("the other candidate was told before it held the lease")
	}
	if p := func() *journal.Pin { j, _ := js.Current(); return j.Pin }(); p == nil || p.Server != "s3" {
		t.Fatalf("the pin of the journal: %+v", p)
	}
}

func TestAnInstanceThatIsOutOfRecoveryIsNotAdopted(t *testing.T) {
	w := newWorld()
	w.promoted["s3"] = true
	r, _ := w.runner(t)
	_, err := r.Run(context.Background(), req())
	var pe *failover.PauseError
	if !errors.As(err, &pe) {
		t.Fatalf("got %v", err)
	}
	if w.index("promote ") >= 0 || len(w.alerts) == 0 || w.alerts[0] != "failoverNeedsDecision" {
		t.Fatalf("log:\n%s\nalerts %v", strings.Join(w.log, "\n"), w.alerts)
	}
}

func TestAnUnreadableStandbyNeedsTheAcceptanceOfALoss(t *testing.T) {
	w := newWorld()
	w.posErr["s3"] = failover.ErrUnreachable
	r, _ := w.runner(t)
	_, err := r.Run(context.Background(), req())
	var pe *failover.PauseError
	if !errors.As(err, &pe) || w.index("promote ") >= 0 {
		t.Fatalf("got %v\n%s", err, strings.Join(w.log, "\n"))
	}
	// F4-01. The same request with the acceptance, repeated on the same runner and the same journal (the pause did not close it), goes
	// on: the decision of the operator is taken by the bootstrap in progress.
	rq := req()
	rq.AllowDataLoss = true
	if _, err := r.Run(context.Background(), rq); err != nil || w.index("promote s2") < 0 {
		t.Fatalf("with allowDataLoss: %v\n%s", err, strings.Join(w.log, "\n"))
	}
}

// F4-01 (the other direction). An automatic request that finds a journal begun by an operator does not change it, and an operator can take
// the acceptance back before anything is pinned.
func TestARepeatedRequestChangesTheAcceptanceOnlyWhenItIsManual(t *testing.T) {
	st, err := journal.Open(tempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	j, err := st.Begin("a", "s1", 4, "manual", "op", true)
	if err != nil || !j.AllowDataLoss {
		t.Fatalf("%+v %v", j, err)
	}
	if j, _ = st.Begin("b", "s1", 4, "automatic", "system", false); !j.AllowDataLoss || j.Trigger != "manual" || j.RequestedBy != "op" {
		t.Fatalf("an automatic request changed the journal of an operator: %+v", j)
	}
	if j, _ = st.Begin("c", "s1", 4, "manual", "op2", false); j.AllowDataLoss || j.RequestedBy != "op2" {
		t.Fatalf("the operator could not take the acceptance back: %+v", j)
	}
	if j, _ = st.Begin("d", "s1", 4, "manual", "op2", true); !j.AllowDataLoss || len(j.Events) != 2 {
		t.Fatalf("%+v", j)
	}
	if _, err := st.SetPin(journal.Pin{StandbyID: 1, Node: 1, Server: "s2"}); err != nil {
		t.Fatal(err)
	}
	if j, _ = st.Begin("e", "s1", 4, "manual", "op2", false); !j.AllowDataLoss {
		t.Fatalf("a pinned journal took a new acceptance: %+v", j)
	}
}

func TestDecideRefusals(t *testing.T) {
	cases := []struct {
		name string
		mod  func(w *world)
		req  Request
		code string
	}{
		{"the catalog names another primary", func(w *world) { w.primary = "s3" }, req(), "unprocessable"},
		{"this server is the old one", func(w *world) {}, Request{OldServer: "s2"}, "unprocessable"},
		{"no ready standby here", func(w *world) { w.standbys[0].State = "creating" }, req(), "no_replica_available"},
		{"no majority", func(w *world) { w.live["s3"] = "dead" }, req(), "quorum_unavailable"},
	}
	for _, c := range cases {
		w := newWorld()
		c.mod(w)
		if c.name == "this server is the old one" {
			w.primary = "s2"
		}
		r, js := w.runner(t)
		_, err := r.Run(context.Background(), c.req)
		var ref *Refusal
		if !errors.As(err, &ref) || ref.Code != c.code {
			t.Errorf("%s: %v", c.name, err)
		}
		if js.Active() {
			t.Errorf("%s: a refused request began a journal", c.name)
		}
	}
}

func TestAnOldServerThatAnswersIsNotAskedToDemoteAndTheRequestIsRefused(t *testing.T) {
	w := newWorld()
	w.oldAlive = true
	w.live["s1"] = "live"
	r, js := w.runner(t)
	_, err := r.Run(context.Background(), req())
	var ref *Refusal
	if !errors.As(err, &ref) || ref.Code != "unsupported_operation" || ref.Status != 422 {
		t.Fatalf("a manual request for a live old server: %v", err)
	}
	if js.Active() {
		t.Fatal("a refused request began a journal")
	}
	if w.count("demote") != 0 || w.count("lease-") != 0 {
		t.Fatalf("a call was made to the live old server or a lease was taken:\n%s", strings.Join(w.log, "\n"))
	}
}

// Without the key of the certificate authority on the new leader the product says so (the escrow is an open point of the design).
func TestANewLeaderWithoutTheKeyOfTheCertificateAuthorityRaisesAnAlert(t *testing.T) {
	w := newWorld()
	w.noCA = true
	r, _ := w.runner(t)
	if _, err := r.Run(context.Background(), req()); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range w.alerts {
		if a == AlertAuthority {
			found = true
		}
	}
	if !found {
		t.Fatalf("alerts %v", w.alerts)
	}
	w2 := newWorld()
	r2, _ := w2.runner(t)
	if _, err := r2.Run(context.Background(), req()); err != nil {
		t.Fatal(err)
	}
	for _, a := range w2.alerts {
		if a == AlertAuthority {
			t.Fatalf("an alert although the key is there: %v", w2.alerts)
		}
	}
}

// A step that finished is not run again after a crash, and a task that was recorded is not recorded twice.
func TestAFinishedStepIsNotRepeatedAfterACrash(t *testing.T) {
	for _, c := range []struct {
		point, marker string
		want          int
	}{
		// B2 asks once; each of the six steps after it asks again (the freeze is renewed, and redone after a restart)
		{journal.StepFreeze + ":after", "freeze", 7},
		{journal.StepCatalog + ":after", "promote_node", 1},
	} {
		w := newWorld()
		r, _ := w.runner(t)
		crashed := false
		r.Fault = func(p string) error {
			if p == c.point && !crashed {
				crashed = true
				return errors.New("kingresd ended")
			}
			return nil
		}
		if _, err := r.Run(context.Background(), req()); err == nil {
			t.Fatalf("%s: no crash", c.point)
		}
		if _, _, err := r.Resume(context.Background()); err != nil {
			t.Fatal(err)
		}
		if n := w.count(c.marker); n != c.want {
			t.Errorf("after a crash at %s the step with %q ran %d times, want %d", c.point, c.marker, n, c.want)
		}
	}
}

func TestARecordedTaskIsNotRecordedAgainAfterACrash(t *testing.T) {
	w := newWorld()
	r, _ := w.runner(t)
	crashed := false
	r.Fault = func(p string) error {
		if p == "B7 recorded" && !crashed {
			crashed = true
			return errors.New("kingresd ended")
		}
		return nil
	}
	if _, err := r.Run(context.Background(), req()); err == nil {
		t.Fatal("no crash")
	}
	if _, _, err := r.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.recorded != 1 {
		t.Fatalf("recorded %d times", w.recorded)
	}
}

// tempDir is a directory that is removed without failing the test when the machine still holds a file in it (an indexer on Windows).
func tempDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "kg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// F4-05. An automatic start for an old server that this server hears is refused (conflict), and so is an operator's (unsupported_operation) until the live old leader is built.
func TestAnAutomaticStartForALiveOldServerIsRefused(t *testing.T) {
	w := newWorld()
	w.live["s1"] = "live"
	r, _ := w.runner(t)
	rq := req()
	rq.Trigger = "automatic"
	_, err := r.Decide(context.Background(), rq)
	var ref *Refusal
	if !errors.As(err, &ref) || ref.Code != "conflict" {
		t.Fatalf("an automatic start for a live server: %v", err)
	}
	_, err = r.Decide(context.Background(), req())
	if !errors.As(err, &ref) || ref.Code != "unsupported_operation" {
		t.Fatalf("a manual request for a live server is refused until it is built: %v", err)
	}
}

// The freeze is asked for again before every step after B2: when it cannot be had (the agent is gone), the bootstrap does not go on to
// the promotion without it.
func TestTheBootstrapDoesNotPromoteWithoutItsFreeze(t *testing.T) {
	w := newWorld()
	w.freezeErr = errors.New("the standby agent is not running")
	r, _ := w.runner(t)
	if _, err := r.Run(context.Background(), req()); err == nil {
		t.Fatal("the bootstrap went on without a freeze")
	}
	if w.count("promote ") != 0 {
		t.Fatalf("promoted without the freeze: %v", w.log)
	}
}

// The planned switchover of the map group, bootstrap side: the old leader is alive (its own task asked), the lease is won once it stops, the standby
// it chose is pinned, the promotion waits for the proof it delivers and carries it, and the bootstrap records no task (the task is the leader's).
func TestAPlannedSwitchoverOfTheMapGroupWaitsForTheProofAndRecordsNoTask(t *testing.T) {
	w := newWorld()
	w.live["s1"] = "live"
	r, js := w.runner(t)
	sleeps := 0
	base := r.Sleep
	r.Sleep = func(ctx context.Context, d time.Duration) error {
		sleeps++
		if sleeps == 3 { // the old leader finished its clean stop and sends the proof
			if err := js.SetProof("0/6000028", "shut down"); err != nil {
				t.Errorf("the proof: %v", err)
			}
		}
		return base(ctx, d)
	}
	rq := req()
	rq.Planned = &journal.Planned{TaskID: 77, StandbyID: 10}
	if _, err := r.Run(context.Background(), rq); err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(w.log, "\n"))
	}
	if len(w.promoteReqs) != 1 || w.promoteReqs[0].Checkpoint != "0/6000028" || w.promoteReqs[0].ClusterState != "shut down" || w.promoteReqs[0].StandbyID != 10 {
		t.Fatalf("the promotion did not carry the proof: %+v", w.promoteReqs)
	}
	if w.count("record-planned 77") != 1 {
		t.Fatalf("the group 0 row of the leader's task was not marked promoted:\n%s", strings.Join(w.log, "\n"))
	}
	if w.count("record-task") != 0 {
		t.Fatalf("a task was recorded for a planned switchover:\n%s", strings.Join(w.log, "\n"))
	}
	if cur, _ := js.Current(); !cur.Recorded || cur.TaskID != 77 {
		t.Fatalf("the journal does not name the task of the old leader: %+v", cur)
	}
}

// No proof, no promotion: the bootstrap pauses and nothing is promoted.
func TestAPlannedSwitchoverWithoutTheProofPausesBeforeThePromotion(t *testing.T) {
	w := newWorld()
	w.live["s1"] = "live"
	r, _ := w.runner(t)
	rq := req()
	rq.Planned = &journal.Planned{TaskID: 77, StandbyID: 10}
	_, err := r.Run(context.Background(), rq)
	var pe *failover.PauseError
	if !errors.As(err, &pe) {
		t.Fatalf("want a pause: %v", err)
	}
	if w.count("promote ") != 0 {
		t.Fatalf("promoted without the proof:\n%s", strings.Join(w.log, "\n"))
	}
}

// The standby that the old leader chose must be this server's: another is not a choice.
func TestAPlannedSwitchoverPinsOnlyTheStandbyOfTheOldLeadersChoice(t *testing.T) {
	w := newWorld()
	w.live["s1"] = "live"
	r, js := w.runner(t)
	rq := req()
	rq.Planned = &journal.Planned{TaskID: 77, StandbyID: 11} // the standby on s3, not on this server
	_, err := r.Run(context.Background(), rq)
	var fe *failover.Error
	if !errors.As(err, &fe) || fe.Code != failover.CodeNoCandidate {
		t.Fatalf("want no_candidate: %v", err)
	}
	if cur, _ := js.Current(); cur.Pin != nil {
		t.Fatalf("a standby was pinned: %+v", cur.Pin)
	}
}

// A planned request for a server that this server does not hear is a conflict, and an ordinary one for a live server stays refused.
func TestAPlannedRequestNeedsALiveOldServer(t *testing.T) {
	w := newWorld() // s1 is dead
	r, js := w.runner(t)
	rq := req()
	rq.Planned = &journal.Planned{TaskID: 77, StandbyID: 10}
	_, err := r.Decide(context.Background(), rq)
	var ref *Refusal
	if !errors.As(err, &ref) || ref.Code != "conflict" || js.Active() {
		t.Fatalf("a planned request for a dead server: %v", err)
	}
}

func (p ports) RecordPlanned(_ context.Context, taskID, termAfter, timeline int64) error {
	p.w.rec("record-planned %d term=%d timeline=%d", taskID, termAfter, timeline)
	return nil
}

func (p ports) PlannedRefused(_ context.Context, oldServer string, taskID int64, reason string) error {
	p.w.rec("planned-refused %s task=%d", oldServer, taskID)
	return p.w.refusedErr
}

func (p ports) ResumeOldLeader(_ context.Context, oldServer string) error {
	p.w.rec("resume-old-leader %s", oldServer)
	if p.w.onResumeOld != nil {
		p.w.onResumeOld()
	}
	return p.w.resumeOldErr
}

// The standby refuses the proof after the old leader stopped: the candidate lets go of the lease and tells the old leader (planned-refused), gives the
// bootstrap up and promotes nothing; the old leader's task rolls back and starts its primary again.
func TestARefusedProofGivesTheBootstrapUpAndTellsTheOldLeader(t *testing.T) {
	w := newWorld()
	w.live["s1"] = "live"
	w.promoteErr = &failover.CallError{Status: 409, Code: "unprovable", Message: "the standby is behind"}
	r, js := w.runner(t)
	rq := req()
	rq.Planned = &journal.Planned{TaskID: 77, StandbyID: 10}
	if _, err := r.Decide(context.Background(), rq); err != nil {
		t.Fatal(err)
	}
	if err := js.SetProof("0/6000028", "shut down"); err != nil {
		t.Fatal(err)
	}
	_, err := r.Run(context.Background(), rq)
	var fe *failover.Error
	if !errors.As(err, &fe) || fe.Code != failover.CodeSwitchoverUnproven {
		t.Fatalf("want switchover_unproven: %v\n%s", err, strings.Join(w.log, "\n"))
	}
	if w.count("planned-refused s1 task=77") != 1 || w.count("promote s2") != 0 {
		t.Fatalf("the refusal:\n%s", strings.Join(w.log, "\n"))
	}
	if js.Active() {
		t.Fatal("the bootstrap is still in progress after the refusal")
	}
}

func (p ports) PullProof(_ context.Context, oldServer string) (string, string, error) {
	p.w.mu.Lock()
	defer p.w.mu.Unlock()
	p.w.pulls++
	if p.w.pullErr != nil {
		return "", "", p.w.pullErr
	}
	return p.w.pullCheckpoint, p.w.pullState, nil
}

// The old leader's kingresd ended after the clean stop and did not send the proof: the candidate reads it from the old leader's server once the wait
// has lasted ProofPullAfter, and promotes on it. A state other than "shut down" is not a proof, and is not taken.
func TestTheCandidateReadsTheProofItselfWhenTheOldLeaderCannotSendIt(t *testing.T) {
	w := newWorld()
	w.live["s1"] = "live"
	w.pullCheckpoint, w.pullState = "0/6000028", "shut down"
	r, js := w.runner(t)
	rq := req()
	rq.Planned = &journal.Planned{TaskID: 77, StandbyID: 10}
	if _, err := r.Run(context.Background(), rq); err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(w.log, "\n"))
	}
	if len(w.promoteReqs) != 1 || w.promoteReqs[0].Checkpoint != "0/6000028" || w.pulls < 1 {
		t.Fatalf("the promotion did not carry the proof that was read: %+v (pulls %d)", w.promoteReqs, w.pulls)
	}
	if cur, _ := js.Current(); cur.Planned == nil || cur.Planned.Checkpoint != "0/6000028" {
		t.Fatalf("%+v", cur.Planned)
	}
}

func TestAStateThatIsNotShutDownIsNotAProof(t *testing.T) {
	w := newWorld()
	w.live["s1"] = "live"
	w.pullCheckpoint, w.pullState = "0/6000028", "in production"
	r, _ := w.runner(t)
	r.ProofWait = 3 * time.Minute
	rq := req()
	rq.Planned = &journal.Planned{TaskID: 77, StandbyID: 10}
	_, err := r.Run(context.Background(), rq)
	var pe *failover.PauseError
	if !errors.As(err, &pe) || len(w.promoteReqs) != 0 {
		t.Fatalf("want a pause and no promotion: %v %+v", err, w.promoteReqs)
	}
}

// F5-03. An abandon that arrives while the call that promotes is in flight is refused: the intent was written before the call.
func TestAnAbandonWhilePromoteIsInFlightIsRefused(t *testing.T) {
	w := newWorld()
	r, js := w.runner(t)
	var abandonErr error
	w.onPromote = func() { abandonErr = js.Abandon("an operator") }
	if _, err := r.Run(context.Background(), req()); err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(w.log, "\n"))
	}
	if !errors.Is(abandonErr, journal.ErrPromoted) {
		t.Fatalf("the abandon during the promotion: %v", abandonErr)
	}
}

// F5-04. The standby has not replayed the shutdown checkpoint yet: the bootstrap asks again for VerifyWait, as the ordinary groups do, and promotes when it has;
// the standby that stays behind is refused at the end of the wait, and a proof that can never be made at once.
func TestAStandbyThatIsBehindForAWhileIsWaitedFor(t *testing.T) {
	w := newWorld()
	w.live["s1"] = "live"
	w.behind = 3
	r, js := w.runner(t)
	rq := req()
	rq.Planned = &journal.Planned{TaskID: 77, StandbyID: 10}
	_, _ = r.Decide(context.Background(), rq)
	_ = js.SetProof("0/6000028", "shut down")
	if _, err := r.Run(context.Background(), rq); err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(w.log, "\n"))
	}
	if w.count("planned-refused") != 0 || w.count("promote s2") != 1 {
		t.Fatalf("the standby that caught up was refused:\n%s", strings.Join(w.log, "\n"))
	}
}

func TestAStandbyThatStaysBehindIsRefusedAtTheEndOfTheWait(t *testing.T) {
	w := newWorld()
	w.live["s1"] = "live"
	w.behind = 1000
	r, js := w.runner(t)
	r.VerifyWait = 10 * time.Second
	rq := req()
	rq.Planned = &journal.Planned{TaskID: 77, StandbyID: 10}
	_, _ = r.Decide(context.Background(), rq)
	_ = js.SetProof("0/6000028", "shut down")
	_, err := r.Run(context.Background(), rq)
	var fe *failover.Error
	if !errors.As(err, &fe) || fe.Code != failover.CodeSwitchoverUnproven || w.count("planned-refused s1 task=77") != 1 {
		t.Fatalf("%v\n%s", err, strings.Join(w.log, "\n"))
	}
}

// F5-09. The journal is given up although the old leader could not be told: the lease is let go of already, and an active journal would keep the gate shut.
func TestAGivenUpJournalDoesNotDependOnTellingTheOldLeader(t *testing.T) {
	w := newWorld()
	w.live["s1"] = "live"
	w.promoteErr = &failover.CallError{Status: 409, Code: "unprovable", Message: "no"}
	w.refusedErr = errors.New("the old leader does not answer")
	r, js := w.runner(t)
	rq := req()
	rq.Planned = &journal.Planned{TaskID: 77, StandbyID: 10}
	_, _ = r.Decide(context.Background(), rq)
	_ = js.SetProof("0/6000028", "shut down")
	_, err := r.Run(context.Background(), rq)
	var fe *failover.Error
	if !errors.As(err, &fe) || js.Active() {
		t.Fatalf("a journal that could not tell the old leader stays active: %v active=%v", err, js.Active())
	}
}

// F6-02. The bootstrap that is given up while the promoting call is repeated (the standby answers standby_behind) does not call again: another call would promote
// a standby whose lease was let go of, behind the back of the journal.
func TestAGivenUpBootstrapDoesNotRepeatThePromotingCall(t *testing.T) {
	w := newWorld()
	w.live["s1"] = "live"
	w.behind = 1000
	r, js := w.runner(t)
	r.VerifyWait = time.Hour
	rq := req()
	rq.Planned = &journal.Planned{TaskID: 77, StandbyID: 10}
	_, _ = r.Decide(context.Background(), rq)
	_ = js.SetProof("0/6000028", "shut down")
	calls := 0
	w.onPromote = func() {
		calls++
		if calls == 2 {
			if err := js.AbandonUnpromoted("an operator"); err != nil {
				t.Errorf("the abandon between two calls: %v", err)
			}
		}
	}
	_, err := r.Run(context.Background(), rq)
	if err == nil {
		t.Fatalf("the bootstrap that was given up went on:\n%s", strings.Join(w.log, "\n"))
	}
	if calls != 2 {
		t.Fatalf("the promoting call was made %d times; it must stop after the abandon (2)", calls)
	}
}

// F6-01. A refused proof gives the bootstrap up and then asks the old leader's server to end the hold of its primary: the old leader waits for as long as a
// bootstrap that names it is active, so the call comes after the journal is given up, and a failure of the call does not undo the refusal.
func TestARefusedProofAsksTheOldLeaderToEndItsHoldAfterTheJournalIsGivenUp(t *testing.T) {
	w := newWorld()
	w.live["s1"] = "live"
	w.promoteErr = &failover.CallError{Status: 409, Code: "unprovable", Message: "the standby is behind"}
	w.resumeOldErr = errors.New("the old leader cannot be reached")
	r, js := w.runner(t)
	activeAtTheCall := true
	w.onResumeOld = func() { activeAtTheCall = js.Active() }
	rq := req()
	rq.Planned = &journal.Planned{TaskID: 77, StandbyID: 10}
	_, _ = r.Decide(context.Background(), rq)
	_ = js.SetProof("0/6000028", "shut down")
	_, err := r.Run(context.Background(), rq)
	var fe *failover.Error
	if !errors.As(err, &fe) || fe.Code != failover.CodeSwitchoverUnproven {
		t.Fatalf("want switchover_unproven: %v", err)
	}
	if w.count("resume-old-leader s1") != 1 {
		t.Fatalf("the old leader was not asked to end its hold:\n%s", strings.Join(w.log, "\n"))
	}
	if activeAtTheCall {
		t.Fatal("the old leader was asked while the bootstrap that names it was still active")
	}
}

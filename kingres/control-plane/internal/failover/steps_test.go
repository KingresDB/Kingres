// SPDX-License-Identifier: AGPL-3.0-only

package failover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

func mustOrder(t *testing.T, w *world, first, second string) {
	t.Helper()
	a, b := w.index(first), w.index(second)
	if a < 0 || b < 0 || a > b {
		t.Fatalf("%q (at %d) must come before %q (at %d); log:\n%s", first, a, second, b, strings.Join(w.log, "\n"))
	}
}

// The old primary's server is dead: the failover takes the fence lease, chooses the
// standby with the most WAL, hands the lease to its server, promotes it only then, and
// makes the catalog follow.
func TestFailoverOfADeadPrimary(t *testing.T) {
	w := newWorld()
	r := w.runner()
	w.fakeClock(r)
	if name, err := runAll(t, r, hardInput()); err != nil {
		t.Fatalf("%s: %v\n%s", name, err, strings.Join(w.log, "\n"))
	}
	// standby 10 on s2 holds more WAL than 11 on s3: s2 is promoted, and it is the leader's own server.
	if w.index("promote s2") < 0 || w.index("promote s3") >= 0 {
		t.Fatalf("wrong standby promoted:\n%s", strings.Join(w.log, "\n"))
	}
	mustOrder(t, w, "gone s2 s1", "lease-fence s2")
	mustOrder(t, w, "lease-fence s2", "promote s2")
	mustOrder(t, w, "promote s2", "promote_node kingres_system 4")
	mustOrder(t, w, "promote_node kingres_system 4", "repoint s3")
	mustOrder(t, w, "repoint s3", "after-promotion 10")
	if w.index("recover-prepared-on s2") < 0 {
		t.Error("recover_prepared_transactions() was not run on the promoted instance")
	}
	if w.index("demote") >= 0 {
		t.Error("a dead server was asked to stop its instance")
	}
	if w.index("gone s2 s1") < 0 || w.index("gone s3 s1") < 0 {
		t.Errorf("the other primaries were not told to stop waiting for the standbys of the dead server:\n%s", strings.Join(w.log, "\n"))
	}
	if g := w.rows.groups[1]; g.State != "done" || g.ChosenStandby != 10 || g.TermAfter == nil || *g.TermAfter != 6 {
		t.Errorf("group row %+v", g)
	}
	if w.rows.outcome != "completed" {
		t.Errorf("outcome %q", w.rows.outcome)
	}
	if w.frozen {
		t.Error("the group is still frozen")
	}
	if w.index("audit failoverGroupPromoted") < 0 || w.index("audit failoverGroupStarted") < 0 {
		t.Error("the events were not audited")
	}
}

// A reachable old primary is stopped first, and its server hands the lease over, so
// that taking the fence lease is immediate.
func TestFailoverStopsAReachableOldPrimaryFirst(t *testing.T) {
	w := newWorld()
	w.oldAlive, w.oldWritable = true, true
	w.live["s1"] = "live"
	r := w.runner()
	w.fakeClock(r)
	if name, err := runAll(t, r, hardInput()); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	mustOrder(t, w, "demote s1 to=s2", "lease-fence s2")
	mustOrder(t, w, "lease-fence s2", "promote s2")
	if w.index("gone") >= 0 {
		t.Error("the standbys of a live server were declared gone")
	}
}

// Nothing is promoted while the old holder still renews the lease: the step ends with
// lease_still_held and no instance was promoted (invariant I1).
func TestNothingIsPromotedWhileTheOldLeaseIsHeld(t *testing.T) {
	w := newWorld()
	w.leaseHeldByOld = true
	r := w.runner()
	w.fakeClock(r)
	name, err := runAll(t, r, hardInput())
	var fe *Error
	if !errors.As(err, &fe) || fe.Code != CodeLeaseStillHeld {
		t.Fatalf("%s: %v", name, err)
	}
	if name != StepName(1, StepLease) {
		t.Errorf("failed in %q", name)
	}
	if w.index("promote") >= 0 && w.index("promote_node") < 0 && w.index("promote s") >= 0 {
		t.Fatal("promoted")
	}
	if w.index("position") >= 0 {
		t.Error("positions were read before the old primary was fenced")
	}
}

// The step "Promote the standby" refuses to promote when the server does not hold the
// lease, and goes back to hand it over once.
func TestPromoteWithoutTheLeaseGoesBackOnceThenFails(t *testing.T) {
	w := newWorld()
	r := w.runner()
	w.fakeClock(r)
	// Run up to the hand-over.
	raw, _ := json.Marshal(hardInput())
	steps, _ := r.steps(raw)
	env := &task.Env{Task: task.Task{ID: 1, Input: raw}}
	for _, s := range steps[:6] { // check, freeze, stop, lease, choose, hand over
		if err := s.Run(context.Background(), env); err != nil && !isSkip(err) {
			t.Fatalf("%s: %v", s.Name, err)
		}
	}
	w.lease["s2"] = false // the lease is lost between the hand-over and the promotion
	w.leaseHeldByOld = true
	err := steps[6].Run(context.Background(), env)
	var fe *Error
	if !errors.As(err, &fe) || fe.Code != CodeLeaseLost {
		t.Fatalf("got %v", err)
	}
	if w.index("promote s2") >= 0 {
		t.Fatal("promoted without the lease")
	}
}

func isSkip(err error) bool {
	var sk *task.SkipError
	return errors.As(err, &sk)
}

// Every step is a no-op on its second execution: running the whole task again after it
// finished changes nothing and promotes nothing twice.
func TestEveryStepRepeatsHarmlessly(t *testing.T) {
	w := newWorld()
	r := w.runner()
	w.fakeClock(r)
	if name, err := runAll(t, r, hardInput()); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	promotes := w.count("promote s")
	records := w.records
	if name, err := runAll(t, r, hardInput()); err != nil {
		t.Fatalf("second run, %s: %v\n%s", name, err, strings.Join(w.log, "\n"))
	}
	if w.count("promote s") != promotes {
		t.Errorf("a second promotion was made")
	}
	if w.rows.groups[1].ChosenStandby != 10 || w.rows.groups[1].State != "done" {
		t.Errorf("row after the repeat %+v", w.rows.groups[1])
	}
	_ = records
}

// A crash between pg_promote and the record of the step: the repeat finds the standby
// promoted (out of recovery) and pins it; it never chooses another one.
func TestARepeatAfterPromotionKeepsTheSameStandby(t *testing.T) {
	w := newWorld()
	r := w.runner()
	w.fakeClock(r)
	raw, _ := json.Marshal(hardInput())
	steps, _ := r.steps(raw)
	env := &task.Env{Task: task.Task{ID: 1, Input: raw}}
	for _, s := range steps[:7] { // up to and including the promotion
		if err := s.Run(context.Background(), env); err != nil && !isSkip(err) {
			t.Fatalf("%s: %v", s.Name, err)
		}
	}
	// The leader dies and the pin is lost with it (a second run starts from a fresh record): the instance is out of recovery.
	w.rows.groups[1].ChosenStandby = 0
	w.rows.groups[1].NewServer = ""
	if err := steps[4].Run(context.Background(), env); err != nil { // choose
		t.Fatal(err)
	}
	if w.rows.groups[1].ChosenStandby != 10 {
		t.Fatalf("pinned %d", w.rows.groups[1].ChosenStandby)
	}
	if w.count("promote s") != 1 {
		t.Fatalf("promoted %d times", w.count("promote s"))
	}
}

// Two instances of the group out of recovery: the task stops (invariant I5).
func TestSplitPromotionStopsTheTask(t *testing.T) {
	w := newWorld()
	w.promoted["s2"], w.promoted["s3"] = true, true
	w.lease["s2"] = true
	r := w.runner()
	w.fakeClock(r)
	_, err := runAll(t, r, hardInput())
	var fe *Error
	if !errors.As(err, &fe) || fe.Code != CodeSplitPromotion {
		t.Fatalf("got %v", err)
	}
	if len(w.alerts) == 0 || w.alerts[len(w.alerts)-1] != "splitPromotion" {
		t.Errorf("alerts %v", w.alerts)
	}
}

// A standby that cannot be read: the verdict is unknown and nothing is promoted
// unless the operator accepted the loss.
func TestAnUnreadableStandbyNeedsADecision(t *testing.T) {
	w := newWorld()
	w.posErr["s2"] = ErrUnreachable // the better standby cannot be read
	r := w.runner()
	w.fakeClock(r)
	name, err := runAll(t, r, hardInput())
	var pe *PauseError
	if !errors.As(err, &pe) || name != StepName(1, StepChoose) {
		t.Fatalf("%s: %v", name, err)
	}
	if w.index("promote s") >= 0 {
		t.Fatal("promoted")
	}
	if len(w.alerts) == 0 || w.alerts[len(w.alerts)-1] != "failoverNeedsDecision" {
		t.Errorf("alerts %v", w.alerts)
	}
	// The same with allowDataLoss: the readable standby is promoted.
	w2 := newWorld()
	w2.posErr["s2"] = ErrUnreachable
	r2 := w2.runner()
	w2.fakeClock(r2)
	in := hardInput()
	in.AllowDataLoss = true
	if name, err := runAll(t, r2, in); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if w2.index("promote s3") < 0 {
		t.Fatal("s3 was not promoted")
	}
}

func TestAnAsyncGroupNeedsAcceptanceOfTheLoss(t *testing.T) {
	w := newWorld()
	w.facts.AsyncDatabases = []string{"shop"}
	r := w.runner()
	w.fakeClock(r)
	_, err := runAll(t, r, hardInput())
	var pe *PauseError
	if !errors.As(err, &pe) {
		t.Fatalf("got %v", err)
	}
}

// The requested target is promoted although it is behind: possible loss, so the
// request must have accepted it.
func TestTargetBehindTheBestNeedsAcceptance(t *testing.T) {
	w := newWorld()
	r := w.runner()
	w.fakeClock(r)
	in := hardInput()
	in.Targets = map[int32]string{1: "s3"}
	if _, err := runAll(t, r, in); err == nil {
		t.Fatal("promoted a standby that is behind without the acceptance")
	}
	in.AllowDataLoss = true
	w2 := newWorld()
	r2 := w2.runner()
	w2.fakeClock(r2)
	if name, err := runAll(t, r2, in); err != nil || w2.index("promote s3") < 0 {
		t.Fatalf("%s: %v", name, err)
	}
}

// The catalog says success but a node did not get its part: the check step repeats the
// call, and asks when the catalog still does not show the promotion.
func TestACatalogThatDoesNotFollowIsNotAccepted(t *testing.T) {
	w := newWorld()
	w.catalogFollows = false
	r := w.runner()
	w.fakeClock(r)
	name, err := runAll(t, r, hardInput())
	var pe *PauseError
	if !errors.As(err, &pe) || name != StepName(1, StepCheckCat) {
		t.Fatalf("%s: %v", name, err)
	}
	if w.count("promote_node") != 2 {
		t.Errorf("promote_node was called %d times", w.count("promote_node"))
	}
}

// A promote_node that is slow to answer KS009 for the new primary's own list makes the
// task apply the empty list again and repeat.
func TestPromoteNodeCannotCommitReappliesTheList(t *testing.T) {
	w := newWorld()
	w.catScript = []any{&SQLError{Code: "KS009", Text: "node 4 (s2:5500) cannot commit: its synchronous standbys do not confirm"}}
	r := w.runner()
	w.fakeClock(r)
	if name, err := runAll(t, r, hardInput()); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if w.index("promote-reapply s2") < 0 {
		t.Error("the list was not applied again")
	}
}

// A lost answer of the promotion: the callee did the work, the answer never came; the
// step asks for the state and goes on instead of promoting again blindly.
func TestALostAnswerOfThePromotionIsRepeated(t *testing.T) {
	w := newWorld()
	w.promoteErr["s2#0:after"] = ErrUnreachable // the instance was promoted and the answer never came
	r := w.runner()
	r.PromoteTries = 1 // one try: only the state of the callee can tell that the promotion happened
	w.fakeClock(r)
	if name, err := runAll(t, r, hardInput()); err != nil {
		t.Fatalf("%s: %v\n%s", name, err, strings.Join(w.log, "\n"))
	}
	if w.count("promote s2") != 1 || w.count("promote-already") != 1 {
		t.Errorf("promoted %d times, asked again %d times (the state of the callee must decide)", w.count("promote s2"), w.count("promote-already"))
	}
}

func TestSupersededRunRecordsNothing(t *testing.T) {
	w := newWorld()
	r := w.runner()
	w.fakeClock(r)
	w.rows.lost = true
	_, err := runAll(t, r, hardInput())
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("got %v", err)
	}
	if w.index("promote s") >= 0 || w.index("freeze") >= 0 {
		t.Fatalf("a superseded run acted:\n%s", strings.Join(w.log, "\n"))
	}
}

// The refusals of the request, each with the code the API answers.
func TestPreflightRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(w *world, in *Input)
		code string
	}{
		{"no standby that is ready", func(w *world, in *Input) { w.standbys = nil }, "no_replica_available"},
		{"a standby not registered in the catalog", func(w *world, in *Input) { w.standbys[0].CatalogNode = 0 }, "cluster_inconsistent"},
		{"a standby whose node is not a standby of the group", func(w *world, in *Input) { w.standbys[0].CatalogNode = 2 }, "cluster_inconsistent"},
		{"the primary names a standby that is not in the catalog (FO-15)", func(w *world, in *Input) {
			w.oldAlive = true
			w.primaryNames = []int64{10, 99}
		}, "cluster_inconsistent"},
		{"no majority", func(w *world, in *Input) { w.live["s3"] = "dead" }, "quorum_unavailable"},
		{"lease settings differ", func(w *world, in *Input) { w.settings["s3"] = LeaseSettings{10000, 2000} }, "lease_settings_differ"},
		{"a member of an older version", func(w *world, in *Input) { w.settingsErr["s3"] = &CallError{Status: 404, Code: "not_found"} }, "version_mismatch"},
		{"a rebalance runs", func(w *world, in *Input) { w.rebalance = true }, "rebalance_in_progress"},
		{"the map group", func(w *world, in *Input) { in.ServerID = "s2"; in.Groups = []int32{0} }, "unsupported_operation"},
		{"a group the server is not primary of", func(w *world, in *Input) { in.Groups = []int32{2} }, "unprocessable"},
		{"a target that is not a standby of the group", func(w *world, in *Input) { in.Targets = map[int32]string{1: "s1"} }, "unprocessable"},
		{"graceful", func(w *world, in *Input) { in.Mode = ModeGraceful }, "unsupported_operation"},
		{"an old primary that runs with no watchdog (F2-13)", func(w *world, in *Input) { w.oldAlive, w.oldWritable, w.noWatchdog = true, true, true }, "cluster_inconsistent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld()
			in := hardInput()
			tc.mod(w, &in)
			ref := w.runner().Preflight(context.Background(), in)
			if ref == nil || ref.Code != tc.code {
				t.Fatalf("got %v, want %s", ref, tc.code)
			}
		})
	}
	w := newWorld()
	if ref := w.runner().Preflight(context.Background(), hardInput()); ref != nil {
		t.Fatalf("a good request was refused: %v", ref)
	}
}

func TestCancellableOnlyBeforeTheOldPrimaryIsStopped(t *testing.T) {
	mk := func(states ...string) task.Task {
		names := []string{StepCheck, StepName(1, StepFreeze), StepName(1, StepStop), StepName(1, StepLease)}
		var ss []task.StepState
		for i, n := range names {
			st := task.StepPending
			if i < len(states) {
				st = states[i]
			}
			ss = append(ss, task.StepState{Name: n, State: st})
		}
		return task.Task{State: task.Running, Steps: ss}
	}
	if !Cancellable(mk(task.StepDone, task.StepDone)) {
		t.Error("not cancellable before the stop")
	}
	if Cancellable(mk(task.StepDone, task.StepDone, task.StepRunning)) {
		t.Error("cancellable while the old primary is being stopped")
	}
	if Cancellable(mk(task.StepDone, task.StepDone, task.StepDone, task.StepDone)) {
		t.Error("cancellable after the stop")
	}
	if Cancellable(task.Task{State: task.Done}) {
		t.Error("a finished task is cancellable")
	}
}

// The standby on another server is promoted: the leader hands the lease over to it with a
// transfer, the new server holds it, and only then is the instance promoted.
func TestFailoverToAnotherServerHandsTheLeaseOverFirst(t *testing.T) {
	w := newWorld()
	w.pos["s3"] = Position{Server: "s3", Group: 1, InRecovery: true, Timeline: 1, ReceiveLsn: "0/9000000", ReplayLsn: "0/9000000", SystemIdentifier: "9"}
	r := w.runner()
	w.fakeClock(r)
	if name, err := runAll(t, r, hardInput()); err != nil {
		t.Fatalf("%s: %v\n%s", name, err, strings.Join(w.log, "\n"))
	}
	mustOrder(t, w, "lease-fence s2", "lease-transfer s2 to s3")
	mustOrder(t, w, "lease-transfer s2 to s3", "lease-holder s3")
	mustOrder(t, w, "lease-holder s3", "promote s3")
	mustOrder(t, w, "promote s3", "promote_node kingres_system 5")
	mustOrder(t, w, "position s3", "lease-transfer")
}

// shopNodes is the catalog of a second sharded database: the same hosts and ports, other node ids.
func shopNodes() []Node {
	return []Node{
		{ID: 10, Group: 0, Host: "s2", Port: 5400, Role: "primary"},
		{ID: 11, Group: 1, Host: "s1", Port: 5401, Role: "primary"},
		{ID: 14, Group: 1, Host: "s2", Port: 5500, Role: "standby"},
		{ID: 15, Group: 1, Host: "s3", Port: 5501, Role: "standby"},
		{ID: 12, Group: 2, Host: "s3", Port: 5402, Role: "primary"},
	}
}

// Every sharded database has a catalog of its own with its own node ids: the standby is promoted in each of them,
// by its host and port, and a database where it is not registered refuses the failover before anything is done.
func TestEveryDatabaseCatalogIsMoved(t *testing.T) {
	w := newWorld()
	w.shop = shopNodes()
	r := w.runner()
	w.fakeClock(r)
	if name, err := runAll(t, r, hardInput()); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	mustOrder(t, w, "promote s2", "promote_node kingres_system 4")
	if w.index("promote_node shop 14") < 0 {
		t.Fatalf("the catalog of the second database was not moved with its own node id:\n%s", strings.Join(w.log, "\n"))
	}
	for _, n := range w.shop {
		if n.ID == 14 && n.Role != "primary" {
			t.Errorf("node 14 of the second database is %s", n.Role)
		}
	}
}

func TestADatabaseWithoutTheStandbyRefusesTheFailover(t *testing.T) {
	w := newWorld()
	w.shop = shopNodes()[:3] // the standby on s3 is not registered there
	ref := w.runner().Preflight(context.Background(), hardInput())
	if ref == nil || ref.Code != "cluster_inconsistent" || !strings.Contains(ref.Message, "shop") {
		t.Fatalf("got %v", ref)
	}
}

func TestADatabaseWhoseCatalogDoesNotFollowIsNotAccepted(t *testing.T) {
	w := newWorld()
	w.shop = shopNodes()
	w.noFollow = "shop" // the extension answers for that database and changes nothing
	r := w.runner()
	w.fakeClock(r)
	name, err := runAll(t, r, hardInput())
	var pe *PauseError
	if !errors.As(err, &pe) || name != StepName(1, StepCheckCat) {
		t.Fatalf("%s: %v", name, err)
	}
}

// An instance that is still starting answers 503 unavailable for a while: the promotion waits for it much longer than for
// a call that did not answer, and goes on when it answers.
func TestAnInstanceThatIsStartingIsWaitedFor(t *testing.T) {
	w := newWorld()
	for i := 0; i < 6; i++ {
		w.promoteErr[fmt.Sprintf("s2#%d", i)] = &CallError{Status: 503, Code: "unavailable", Message: "The instance cannot be reached."}
	}
	r := w.runner()
	w.fakeClock(r)
	if name, err := runAll(t, r, hardInput()); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if w.count("promote s2") != 1 {
		t.Fatalf("promoted %d times", w.count("promote s2"))
	}
}

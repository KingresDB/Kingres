// SPDX-License-Identifier: AGPL-3.0-only

package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// fakeBackend is the database side of the table tasks, in memory. It keeps the contract of the real one: Begin returns the unfinished operation
// when there is one, Advance moves forward and never back, and a repeated call of a finished phase is harmless.
type fakeBackend struct {
	mu        sync.Mutex
	calls     []string
	op        int64
	phase     string // "" when no operation
	cancelled bool
	blockers  []api.TableBlocker
	// stuck is the number of Advance(..., "switching") calls that answer "still switching" before the switch happens (-1: never).
	stuck       int
	verified    string
	partitioned int
	blocking    int
	rows        int64
	bytes       int64
	servers     []string
	// failOn is a phase whose Advance fails (a node is down).
	failOn string
	// lockFails is the number of Advance(..., "shadow") calls that fail because a transaction holds the table (55P03).
	lockFails int
}

var phaseOrder = map[string]int{"preparing": 0, "shadow": 1, "copying": 2, "catching_up": 3, "switching": 4, "cleaning": 5, "done": 6}

func (f *fakeBackend) log(s string) { f.calls = append(f.calls, s) }

func (f *fakeBackend) Report(context.Context, tableParams) ([]api.TableBlocker, []api.TableBlocker, []api.TableBlocker, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("report")
	return f.blockers, nil, nil, nil
}

func (f *fakeBackend) Sizes(context.Context, tableParams) (int64, int64, []string, error) {
	return f.rows, f.bytes, f.servers, nil
}

func (f *fakeBackend) Begin(context.Context, tableParams) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("begin")
	if f.op != 0 && f.phase != "done" && !f.cancelled {
		return f.op, nil
	}
	f.op, f.phase, f.cancelled = 7, "preparing", false
	return f.op, nil
}

func (f *fakeBackend) UnfinishedOp(context.Context, tableParams) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.op == 0 || f.phase == "done" || f.cancelled {
		return 0, false, nil
	}
	return f.op, phaseOrder[f.phase] >= phaseOrder["cleaning"], nil
}

func (f *fakeBackend) Advance(_ context.Context, _ tableParams, _ int64, to string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("advance " + to)
	if to == "shadow" && f.lockFails > 0 {
		f.lockFails--
		return f.phase, "", &pgconn.PgError{Code: "55P03", Message: "could not lock table public.orders in SHARE ROW EXCLUSIVE mode", Detail: "Held by: pid 7"}
	}
	if f.failOn == to {
		return f.phase, "", errors.New("node 3 is down")
	}
	switch to {
	case "switching":
		if phaseOrder[f.phase] >= phaseOrder["cleaning"] {
			return f.phase, "", nil // switched already
		}
		if f.stuck != 0 {
			if f.stuck > 0 {
				f.stuck--
			}
			f.phase = "switching"
			return "switching", "attempt failed: a transaction holds the table", nil
		}
		f.phase = "cleaning" // the switch happened
		return "cleaning", "", nil
	case "cleaning":
		f.phase = "done"
		return "done", "", nil
	}
	if phaseOrder[to] > phaseOrder[f.phase] {
		f.phase = to
	}
	return f.phase, "", nil
}

func (f *fakeBackend) Progress(context.Context, tableParams, int64) (int64, int64, error) {
	return 5, 10, nil
}

func (f *fakeBackend) Verified(context.Context, tableParams, int64) (string, error) {
	return f.verified, nil
}

func (f *fakeBackend) Blocking(context.Context, tableParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("blocking")
	f.blocking++
	return nil
}

func (f *fakeBackend) Partition(context.Context, tableParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("partition")
	f.partitioned++
	return nil
}

func (f *fakeBackend) Cancel(context.Context, tableParams, int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("cancel")
	f.cancelled = true
	return nil
}

func (f *fakeBackend) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTableOps(t *testing.T, b *fakeBackend, opts task.Options) (*Ops, *task.Engine, *task.MemStore, *testClock) {
	t.Helper()
	clk := &testClock{t: time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)}
	mem := task.NewMemStoreClock(clk.Now)
	o := &Ops{tableBackendOverride: b}
	if opts.Holder == "" {
		opts.Holder = "A"
	}
	opts.Now, opts.Lease, opts.StoreTries = clk.Now, 30*time.Second, 1
	e, err := task.NewEngine(mem, o.tableDefinitions(), opts)
	if err != nil {
		t.Fatal(err)
	}
	o.SetEngine(e)
	return o, e, mem, clk
}

func onlineParams(op string) tableParams {
	return tableParams{V: 2, Op: op, Method: "hash", Database: "shop", Schema: "public", Table: "orders", Column: "customer_id", Online: true, Cascade: true}
}

func tableKindOf(op string) string {
	return map[string]string{"distribute": KindTableDistribute, "undistribute": KindTableUndistribute, "alter": KindTableAlter, "partition": KindTablePartition}[op]
}

func stepNames(t task.Task) []string {
	var n []string
	for _, s := range t.Steps {
		n = append(n, s.Name)
	}
	return n
}

func TestOnlineTasksHaveTheNamedSteps(t *testing.T) {
	want := map[string][]string{
		"distribute":   {stepCheckTable, stepFreeSpace, stepPrepare, stepLayout, stepCopy, stepCatchUp, stepSwitch, stepFinish},
		"undistribute": {stepCheckTable, stepFindLinked, stepFreeSpace, stepPrepare, stepLayout, stepCopy, stepCatchUp, stepSwitch, stepFinish},
		"alter":        {stepCheckTable, stepPlan, stepFreeSpace, stepPrepare, stepLayout, stepCopy, stepCatchUp, stepSwitch, stepFinish},
	}
	for op, names := range want {
		b := &fakeBackend{}
		o, e, _, _ := newTableOps(t, b, task.Options{})
		tk, err := e.Submit(context.Background(), tableKindOf(op), task.Subject{Type: "table", ID: "shop/public.orders"}, onlineParams(op), "u")
		if err != nil {
			t.Fatal(err)
		}
		if got := stepNames(tk); strings.Join(got, "|") != strings.Join(names, "|") {
			t.Errorf("%s: steps %v, want %v", op, got, names)
		}
		if ran, err := e.RunOnce(context.Background()); !ran || err != nil {
			t.Fatalf("%s: %v %v", op, ran, err)
		}
		got, _ := e.Get(context.Background(), tk.ID)
		if got.State != task.Done {
			t.Fatalf("%s: %s %+v", op, got.State, got.Error)
		}
		_ = o
		if b.count("begin") != 1 {
			t.Errorf("%s: begin ran %d times", op, b.count("begin"))
		}
		var adv []string
		for _, c := range b.calls {
			if strings.HasPrefix(c, "advance ") {
				adv = append(adv, strings.TrimPrefix(c, "advance "))
			}
		}
		if strings.Join(adv, ",") != "shadow,copying,catching_up,switching,cleaning" {
			t.Errorf("%s: phases %v", op, adv)
		}
	}
}

func TestBlockingTasksCallOnce(t *testing.T) {
	for _, op := range []string{"distribute", "undistribute", "alter"} {
		b := &fakeBackend{}
		_, e, _, _ := newTableOps(t, b, task.Options{})
		p := onlineParams(op)
		p.Online = false
		tk, _ := e.Submit(context.Background(), tableKindOf(op), task.Subject{Type: "table", ID: "x"}, p, "u")
		if _, err := e.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		got, _ := e.Get(context.Background(), tk.ID)
		if got.State != task.Done || b.blocking != 1 {
			t.Errorf("%s: %s, blocking calls %d", op, got.State, b.blocking)
		}
		if b.count("begin") != 0 {
			t.Errorf("%s: a blocking task began an online operation", op)
		}
		if !strings.Contains(strings.Join(stepNames(got), "|"), stepBlocking) {
			t.Errorf("%s: no step %q: %v", op, stepBlocking, stepNames(got))
		}
	}
}

func TestPartitionTask(t *testing.T) {
	b := &fakeBackend{}
	_, e, _, _ := newTableOps(t, b, task.Options{})
	p := tableParams{V: 2, Op: "partition", PartOp: "create", Database: "shop", Schema: "public", Table: "orders", PartName: "o_2026_11", PartBound: "FROM ('2026-11-01') TO ('2026-12-01')"}
	tk, _ := e.Submit(context.Background(), KindTablePartition, task.Subject{Type: "table", ID: "x"}, p, "u")
	if got := stepNames(tk); strings.Join(got, "|") != strings.Join([]string{stepCheckReq, stepPartitions, stepFinish}, "|") {
		t.Fatalf("steps %v", got)
	}
	if _, err := e.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := e.Get(context.Background(), tk.ID)
	if got.State != task.Done || b.partitioned != 1 {
		t.Fatalf("%s, %d partition calls", got.State, b.partitioned)
	}
}

func TestBlockersFailTheTaskWithTheirCode(t *testing.T) {
	b := &fakeBackend{blockers: []api.TableBlocker{{Code: "trigger", Object: "t1", Message: "trigger t1 is not supported"}}}
	_, e, _, _ := newTableOps(t, b, task.Options{})
	tk, _ := e.Submit(context.Background(), KindTableDistribute, task.Subject{Type: "table", ID: "x"}, onlineParams("distribute"), "u")
	_, _ = e.RunOnce(context.Background())
	got, _ := e.Get(context.Background(), tk.ID)
	if got.State != task.Failed || got.Error == nil || got.Error.Code != CodeTableNotEligible || got.Error.Retryable {
		t.Fatalf("%s %+v", got.State, got.Error)
	}
	if !strings.Contains(got.Error.Message, "trigger") {
		t.Errorf("the message does not name the blocker: %q", got.Error.Message)
	}
	if b.count("begin") != 0 {
		t.Error("an operation began for a table that is not eligible")
	}
}

func TestTheSwitchWaitsWhileBlockersHold(t *testing.T) {
	old := switchWait
	switchWait = time.Millisecond
	defer func() { switchWait = old }()
	b := &fakeBackend{stuck: 3}
	_, e, _, _ := newTableOps(t, b, task.Options{})
	tk, _ := e.Submit(context.Background(), KindTableDistribute, task.Subject{Type: "table", ID: "x"}, onlineParams("distribute"), "u")
	if _, err := e.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := e.Get(context.Background(), tk.ID)
	if got.State != task.Done {
		t.Fatalf("%s %+v", got.State, got.Error)
	}
	if n := b.count("advance switching"); n != 4 {
		t.Errorf("the switch was tried %d times, want 4 (three blocked, one switched)", n)
	}
}

func TestCancelWhileTheSwitchWaitsUndoesTheOperation(t *testing.T) {
	old := switchWait
	switchWait = 5 * time.Millisecond
	defer func() { switchWait = old }()
	b := &fakeBackend{stuck: -1}
	_, e, _, _ := newTableOps(t, b, task.Options{})
	ctx := context.Background()
	tk, _ := e.Submit(ctx, KindTableDistribute, task.Subject{Type: "table", ID: "x"}, onlineParams("distribute"), "u")
	done := make(chan struct{})
	go func() { _, _ = e.RunOnce(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for b.count("advance switching") < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := e.Cancel(ctx, tk.ID); err != nil {
		t.Fatalf("a task that waits to switch is cancellable: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not end after the cancel")
	}
	got, _ := e.Get(ctx, tk.ID)
	if got.State != task.Failed || got.Error == nil || got.Error.Code != task.CodeCanceled {
		t.Fatalf("%s %+v", got.State, got.Error)
	}
	if b.count("cancel") != 1 || !b.cancelled {
		t.Errorf("the operation of the extension was not cancelled (calls %v)", b.calls)
	}
}

func TestCancellableGuard(t *testing.T) {
	mk := func(states map[string]string) task.Task {
		var ss []task.StepState
		for _, n := range []string{stepCheckTable, stepFreeSpace, stepPrepare, stepLayout, stepCopy, stepCatchUp, stepSwitch, stepFinish} {
			s := states[n]
			if s == "" {
				s = task.StepPending
			}
			ss = append(ss, task.StepState{Name: n, State: s})
		}
		return task.Task{Steps: ss}
	}
	if !tableCancellable(mk(map[string]string{stepCheckTable: task.StepDone, stepCopy: task.StepRunning})) {
		t.Error("a task that copies is cancellable")
	}
	if !tableCancellable(mk(map[string]string{stepCopy: task.StepDone, stepSwitch: task.StepRunning})) {
		t.Error("a task that waits to switch is cancellable")
	}
	if tableCancellable(mk(map[string]string{stepSwitch: task.StepDone})) {
		t.Error("a task whose switch is done is not cancellable")
	}
	if tableCancellable(mk(map[string]string{stepSwitch: task.StepDone, stepFinish: task.StepRunning})) {
		t.Error("a task that cleans up is not cancellable")
	}
	blocking := task.Task{Steps: []task.StepState{{Name: stepCheckTable, State: task.StepDone}, {Name: stepBlocking, State: task.StepRunning}}}
	if tableCancellable(blocking) {
		t.Error("a blocking copy that runs is not cancellable")
	}
}

func TestAFailureBeforeTheSwitchCancelsTheOperation(t *testing.T) {
	b := &fakeBackend{failOn: "copying"}
	_, e, _, _ := newTableOps(t, b, task.Options{})
	tk, _ := e.Submit(context.Background(), KindTableDistribute, task.Subject{Type: "table", ID: "x"}, onlineParams("distribute"), "u")
	_, _ = e.RunOnce(context.Background())
	got, _ := e.Get(context.Background(), tk.ID)
	if got.State != task.Failed || got.Error == nil || got.Error.Step != stepCopy {
		t.Fatalf("%s %+v", got.State, got.Error)
	}
	if !b.cancelled || b.count("cancel") != 1 {
		t.Errorf("nothing was left behind: the operation must be cancelled (calls %v)", b.calls)
	}
}

func TestAFailureAfterTheSwitchLeavesTheOperation(t *testing.T) {
	b := &fakeBackend{failOn: "cleaning"}
	_, e, _, _ := newTableOps(t, b, task.Options{})
	tk, _ := e.Submit(context.Background(), KindTableDistribute, task.Subject{Type: "table", ID: "x"}, onlineParams("distribute"), "u")
	_, _ = e.RunOnce(context.Background())
	got, _ := e.Get(context.Background(), tk.ID)
	if got.State != task.Failed || got.Error == nil || got.Error.Step != stepFinish {
		t.Fatalf("%s %+v", got.State, got.Error)
	}
	if b.cancelled {
		t.Error("an operation that switched was cancelled")
	}
}

func TestVerificationFailureFailsTheTaskNotRetryable(t *testing.T) {
	b := &fakeBackend{verified: "failed"}
	_, e, _, _ := newTableOps(t, b, task.Options{})
	tk, _ := e.Submit(context.Background(), KindTableAlter, task.Subject{Type: "table", ID: "x"}, onlineParams("alter"), "u")
	_, _ = e.RunOnce(context.Background())
	got, _ := e.Get(context.Background(), tk.ID)
	if got.State != task.Failed || got.Error == nil || got.Error.Code != CodeVerificationFail || got.Error.Retryable {
		t.Fatalf("%s %+v", got.State, got.Error)
	}
}

// TestCrashAtEveryStepResumes kills the first run at each step (after the step ran, before it was recorded) and checks that the second run
// finishes the task: the operation of the extension is begun once, the phases are advanced to the end, and nothing is done twice that must not be.
func TestCrashAtEveryStepResumes(t *testing.T) {
	type crashed struct{}
	steps := 8
	for k := 0; k < steps; k++ {
		t.Run(fmt.Sprintf("step%d", k), func(t *testing.T) {
			b := &fakeBackend{}
			clk := &testClock{t: time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)}
			mem := task.NewMemStoreClock(clk.Now)
			o := &Ops{tableBackendOverride: b}
			hook := func(p string, _ int64, step int) {
				if p == task.FaultStepRan && step == k {
					panic(crashed{})
				}
			}
			a, err := task.NewEngine(mem, o.tableDefinitions(), task.Options{Holder: "A", Lease: 30 * time.Second, Now: clk.Now, StoreTries: 1, Fault: hook})
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			tk, _ := a.Submit(ctx, KindTableDistribute, task.Subject{Type: "table", ID: "x"}, onlineParams("distribute"), "u")
			func() {
				defer func() {
					if r := recover(); r != nil {
						if _, ok := r.(crashed); !ok {
							panic(r)
						}
					}
				}()
				_, _ = a.RunOnce(ctx)
			}()
			if mid, _ := mem.Get(ctx, tk.ID); mid.State == task.Done {
				return // the step has no crash point (it was skipped): the first run finished
			}
			clk.Add(31 * time.Second)
			bEng, err := task.NewEngine(mem, o.tableDefinitions(), task.Options{Holder: "B", Lease: 30 * time.Second, Now: clk.Now, StoreTries: 1})
			if err != nil {
				t.Fatal(err)
			}
			if ran, err := bEng.RunOnce(ctx); !ran || err != nil {
				t.Fatalf("B: %v %v", ran, err)
			}
			got, _ := bEng.Get(ctx, tk.ID)
			if got.State != task.Done {
				t.Fatalf("%s %+v", got.State, got.Error)
			}
			if b.op != 7 || b.phase != "done" {
				t.Errorf("the operation ends in %q (op %d)", b.phase, b.op)
			}
			// the operation was begun once (a repeated Prepare found it)
			if n := b.count("begin"); n < 1 || n > 2 {
				t.Errorf("begin ran %d times", n)
			}
			if b.cancelled {
				t.Error("a crash cancelled the operation")
			}
		})
	}
}

func TestAnOldDistributeTaskStillRunsItsOneStep(t *testing.T) {
	o := &Ops{}
	defs := o.tableDefinitions()
	var d task.Definition
	for _, x := range defs {
		if x.Kind == KindTableDistribute {
			d = x
		}
	}
	in, _ := json.Marshal(distributeParams{Method: "hash", Database: "shop", Schema: "public", Table: "orders", Column: "id"})
	steps, err := d.Dynamic(in)
	if err != nil || len(steps) != 1 || steps[0].Name != "Distribute the table" {
		t.Fatalf("%v %v", steps, err)
	}
}

func TestFreeSpaceNamesTheServer(t *testing.T) {
	p := tableParams{Op: "distribute"}
	free := map[string]int64{"a": 10 << 30, "b": 1 << 20, "c": 8 << 30}
	got := freeSpaceShortfall(p, 9<<30, []string{"a", "b", "c"}, func(s string) (int64, bool) { v, ok := free[s]; return v, ok })
	if !strings.Contains(got, "Server b") {
		t.Fatalf("want server b named, got %q", got)
	}
	// a server that cannot say is not checked; an undistribute needs the whole table on its one target
	if s := freeSpaceShortfall(p, 9<<30, []string{"x"}, func(string) (int64, bool) { return 0, false }); s != "" {
		t.Errorf("an unknown server was refused: %q", s)
	}
	if need := needFree(tableParams{Op: "undistribute"}, 9<<30, 4); need < 9<<30 {
		t.Errorf("an undistribute needs the whole table: %d", need)
	}
	if need := needFree(p, 8<<30, 4); need >= 8<<30 {
		t.Errorf("a distribute needs a share: %d", need)
	}
}

func TestFreeSpaceStepIsSkippedWithoutAProbe(t *testing.T) {
	b := &fakeBackend{rows: 1, bytes: 1 << 30, servers: []string{"a"}}
	o, e, _, _ := newTableOps(t, b, task.Options{})
	_ = o
	tk, _ := e.Submit(context.Background(), KindTableDistribute, task.Subject{Type: "table", ID: "x"}, onlineParams("distribute"), "u")
	_, _ = e.RunOnce(context.Background())
	got, _ := e.Get(context.Background(), tk.ID)
	if got.State != task.Done || stepState(got, stepFreeSpace) != task.StepSkipped {
		t.Fatalf("%s, free space step %s", got.State, stepState(got, stepFreeSpace))
	}
	// with a probe that says no space the task fails with insufficient_disk, before the operation begins
	b2 := &fakeBackend{rows: 1, bytes: 100 << 30, servers: []string{"a", "b"}}
	o2, e2, _, _ := newTableOps(t, b2, task.Options{})
	o2.DiskFree = func(_ context.Context, s string) (int64, bool) { return 1 << 30, true }
	tk2, _ := e2.Submit(context.Background(), KindTableDistribute, task.Subject{Type: "table", ID: "x"}, onlineParams("distribute"), "u")
	_, _ = e2.RunOnce(context.Background())
	got2, _ := e2.Get(context.Background(), tk2.ID)
	if got2.State != task.Failed || got2.Error == nil || got2.Error.Code != CodeInsufficientDisk {
		t.Fatalf("%s %+v", got2.State, got2.Error)
	}
	if b2.count("begin") != 0 {
		t.Error("an operation began on a server without space")
	}
}

func TestUndistributeNamesLinkedTablesWithoutCascade(t *testing.T) {
	b := &fakeBackend{}
	_, e, _, _ := newTableOps(t, b, task.Options{})
	p := onlineParams("undistribute")
	p.Cascade, p.LinkedTables = false, 2
	tk, _ := e.Submit(context.Background(), KindTableUndistribute, task.Subject{Type: "table", ID: "x"}, p, "u")
	_, _ = e.RunOnce(context.Background())
	got, _ := e.Get(context.Background(), tk.ID)
	if got.State != task.Failed || got.Error == nil || got.Error.Code != CodeTableNotEligible || got.Error.Step != stepFindLinked {
		t.Fatalf("%s %+v", got.State, got.Error)
	}
}

func TestRequestValidationNeedsNoDatabase(t *testing.T) {
	o := &Ops{}
	ctx := context.Background()
	three := 5000
	cases := map[string]error{}
	_, cases["buckets"] = o.PrepareAlter(ctx, "shop", "public", "orders", api.AlterTableDistributionRequest{BucketCount: &three})
	_, cases["nothing"] = o.PrepareAlter(ctx, "shop", "public", "orders", api.AlterTableDistributionRequest{})
	_, cases["mode"] = o.PrepareUndistribute(ctx, "shop", "public", "orders", api.UndistributeOptions{Mode: "fast"})
	_, cases["group"] = o.PrepareUndistribute(ctx, "shop", "public", "orders", api.UndistributeOptions{TargetGroupID: -1})
	_, cases["bound"] = o.PreparePartition(ctx, "shop", "public", "orders", "create", "", &api.CreateTablePartitionRequest{Name: "p1", Bound: "DEFAULT; DROP TABLE x"}, nil)
	_, cases["longname"] = o.PreparePartition(ctx, "shop", "public", "orders", "create", "", &api.CreateTablePartitionRequest{Name: strings.Repeat("a", 58), Bound: "DEFAULT"}, nil)
	_, cases["views"] = o.PrepareDistribute(ctx, "shop", "public", "orders", api.DistributeTableRequest{Method: "hash", DistributionColumn: "id", DependentViews: "drop"})
	_, cases["method"] = o.PrepareDistribute(ctx, "shop", "public", "orders", api.DistributeTableRequest{Method: "range"})
	_, cases["cascadeonline"] = o.PrepareAlter(ctx, "shop", "public", "orders", api.AlterTableDistributionRequest{DistributionColumn: "id", CascadeToColocated: true, Mode: "online"})
	for name, err := range cases {
		var pe *problem.Problem
		if !errors.As(err, &pe) || pe.Code != problem.ValidationFailed {
			t.Errorf("%s: want validation_failed, got %v", name, err)
		}
	}
}

func TestTableNotEligibleCarriesTheBlockers(t *testing.T) {
	err := notEligible([]api.TableBlocker{{Code: "dependent_object", Object: "public.v1", Message: "view public.v1 depends on the table"}})
	var pe *problem.Problem
	if !errors.As(err, &pe) || pe.Code != problem.TableNotEligible || pe.Status != 422 {
		t.Fatalf("%v", err)
	}
	b, _ := json.Marshal(pe)
	if !strings.Contains(string(b), `"blockers":[{"code":"dependent_object"`) {
		t.Errorf("the blockers are not in the body: %s", b)
	}
}

func TestColocateChoiceJSON(t *testing.T) {
	var r api.AlterTableDistributionRequest
	if err := json.Unmarshal([]byte(`{"colocateWith": {"schema": "public", "table": "t"}, "bucketCount": 12}`), &r); err != nil || r.ColocateWith.Table == nil || r.ColocateWith.Table.Table != "t" {
		t.Fatalf("%+v %v", r, err)
	}
	if err := json.Unmarshal([]byte(`{"colocateWith": "none"}`), &r); err != nil || r.ColocateWith.Keyword != "none" {
		t.Fatalf("%+v %v", r, err)
	}
	if err := json.Unmarshal([]byte(`{"colocateWith": "maybe"}`), &r); err == nil {
		t.Error("an unknown word was accepted")
	}
}

func TestReportOptionsCarryTheDependentViewsSetting(t *testing.T) {
	p := tableParams{Op: "distribute", Online: true, Column: "id", BucketCount: 8, DependentViews: "recreate", ColocSchema: "public", ColocTable: "t0"}
	o := reportOptions(p)
	if o["dependent_views"] != "recreate" || o["mode"] != "online" || o["bucket_count"] != 8 || o["column"] != "id" || o["colocate_with"] == "" {
		t.Errorf("%v", o)
	}
	if _, ok := reportOptions(tableParams{Op: "distribute"})["dependent_views"]; ok {
		t.Error("the default mode sets nothing")
	}
	if k := reportKind(tableParams{Op: "distribute", Method: "reference"}); k != "distribute_reference" {
		t.Errorf("kind %s", k)
	}
	if k := reportKind(tableParams{Op: "partition", PartOp: "detach"}); k != "detach_partition" {
		t.Errorf("kind %s", k)
	}
}

func TestAStepWaitsForALongTransactionThatHoldsTheTable(t *testing.T) {
	old := lockWait
	lockWait = time.Millisecond
	defer func() { lockWait = old }()
	b := &fakeBackend{lockFails: 3}
	_, e, _, _ := newTableOps(t, b, task.Options{})
	tk, _ := e.Submit(context.Background(), KindTableDistribute, task.Subject{Type: "table", ID: "x"}, onlineParams("distribute"), "u")
	if _, err := e.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := e.Get(context.Background(), tk.ID)
	if got.State != task.Done {
		t.Fatalf("%s %+v", got.State, got.Error)
	}
	if n := b.count("advance shadow"); n != 4 {
		t.Errorf("the layout was asked for %d times, want 4 (three refused for the lock, one made)", n)
	}
}

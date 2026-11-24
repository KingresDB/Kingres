// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// Tests of distribute with related tables (contract 0.32.0): the plan over a graph of foreign keys, the check of the request, and the task run table by
// table against an in-memory backend.

func tk(s, t string) tableKey { return tableKey{s, t} }

// shopGraph is customers <- orders <- items, and orders -> products, with a table that has nothing to do with them.
func shopGraph() relGraph {
	return relGraph{
		Nodes: []relNode{
			{Key: tk("public", "customers"), Rows: i64(10)}, {Key: tk("public", "orders"), Rows: i64(100)},
			{Key: tk("public", "items"), Rows: i64(1000)}, {Key: tk("public", "products"), Rows: i64(5)},
		},
		Edges: []relEdge{
			{Name: "orders_customer_fk", Child: tk("public", "orders"), Parent: tk("public", "customers"), Cols: []string{"customer_id"}, RefCols: []string{"id"},
				Def: "FOREIGN KEY (customer_id) REFERENCES customers(id)", Validated: true},
			{Name: "items_order_fk", Child: tk("public", "items"), Parent: tk("public", "orders"), Cols: []string{"order_id"}, RefCols: []string{"id"},
				Def: "FOREIGN KEY (order_id) REFERENCES orders(id)", Validated: true},
			{Name: "orders_product_fk", Child: tk("public", "orders"), Parent: tk("public", "products"), Cols: []string{"product_id"}, RefCols: []string{"id"},
				Def: "FOREIGN KEY (product_id) REFERENCES products(id)", Validated: false},
		},
	}
}

func TestPlanRelatedOrdersTheClosureAndDerivesColumns(t *testing.T) {
	plan := planRelated(shopGraph(), tk("public", "customers"), "id", nil)
	var names []string
	cols := map[string]string{}
	for _, r := range plan.Tables {
		names = append(names, r.Key.Table)
		cols[r.Key.Table] = r.Column
	}
	// customers is the root: orders is next to it, items and products are one key further
	if got := strings.Join(names, ","); got != "orders,items,products" && got != "orders,products,items" {
		t.Fatalf("order %v", names)
	}
	// customers(id) <- orders(customer_id): orders by customer_id; items(order_id) -> orders(id) needs orders.id, which is not customer_id
	if cols["orders"] != "customer_id" {
		t.Errorf("orders column %q", cols["orders"])
	}
	if len(plan.Keys) != 3 {
		t.Errorf("keys %d", len(plan.Keys))
	}
	// the key of items goes to orders(id), the column of orders is customer_id: the column of items does not follow, and the plan says so
	var unknown []string
	for _, b := range plan.Blockers {
		if b.Code == "related_column_unknown" {
			unknown = append(unknown, b.Object)
		}
	}
	if cols["items"] != "" || !hasStr(unknown, "public.items") {
		t.Errorf("items column %q, unknown %v", cols["items"], unknown)
	}
	// products: orders(product_id) -> products(id); the column of orders is customer_id, so it does not follow either
	if cols["products"] != "" {
		t.Errorf("products column %q", cols["products"])
	}
}

func hasStr(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func TestPlanRelatedFollowsASharedColumnAndTakesTheCallersChoice(t *testing.T) {
	g := relGraph{
		Nodes: []relNode{{Key: tk("s", "a")}, {Key: tk("s", "b")}, {Key: tk("s", "c")}},
		Edges: []relEdge{
			{Name: "b_a", Child: tk("s", "b"), Parent: tk("s", "a"), Cols: []string{"tenant_id", "a_id"}, RefCols: []string{"tenant_id", "id"}},
			{Name: "c_b", Child: tk("s", "c"), Parent: tk("s", "b"), Cols: []string{"tenant_id", "b_id"}, RefCols: []string{"tenant_id", "id"}},
		},
	}
	plan := planRelated(g, tk("s", "a"), "tenant_id", nil)
	if len(plan.Blockers) != 0 || len(plan.Tables) != 2 || plan.Tables[0].Column != "tenant_id" || plan.Tables[1].Column != "tenant_id" {
		t.Fatalf("%+v", plan)
	}
	// a choice for c wins, and b follows from the root
	plan = planRelated(g, tk("s", "a"), "tenant_id", map[tableKey]string{tk("s", "c"): "b_id"})
	byName := map[string]string{}
	for _, r := range plan.Tables {
		byName[r.Key.Table] = r.Column
	}
	if byName["c"] != "b_id" || byName["b"] != "tenant_id" {
		t.Errorf("%v", byName)
	}
	// without a root column nothing is a blocker: the console asks again when the column is chosen
	plan = planRelated(g, tk("s", "a"), "", nil)
	if len(plan.Blockers) != 0 {
		t.Errorf("blockers without a root column: %+v", plan.Blockers)
	}
}

func TestPlanRelatedDoesNotGoThroughADistributedTable(t *testing.T) {
	g := relGraph{
		Nodes: []relNode{{Key: tk("s", "a")}, {Key: tk("s", "ref"), Distributed: true}},
		Edges: []relEdge{{Name: "a_ref", Child: tk("s", "a"), Parent: tk("s", "ref"), Cols: []string{"r"}, RefCols: []string{"id"}}},
	}
	plan := planRelated(g, tk("s", "a"), "r", nil)
	if len(plan.Tables) != 0 || len(plan.Keys) != 0 {
		t.Fatalf("a distributed neighbour is not in the set: %+v", plan)
	}
	if len(plan.Warnings) != 1 || plan.Warnings[0].Code != "related_table_distributed" {
		t.Errorf("warnings %+v", plan.Warnings)
	}
}

func TestCheckChoiceNamesTheMissingAndTheUnrelatedTables(t *testing.T) {
	plan := planRelated(shopGraph(), tk("public", "customers"), "", nil)
	got, err := checkChoice(plan, []api.RelatedTableChoice{{Schema: "public", Table: "orders"}, {Schema: "public", Table: "elsewhere"}})
	if err != nil {
		t.Fatal(err)
	}
	var missing, unrelated []string
	for _, b := range got.Blockers {
		switch b.Code {
		case "related_table_missing":
			missing = append(missing, b.Object)
		case "related_table_unrelated":
			unrelated = append(unrelated, b.Object)
		}
	}
	if !reflect.DeepEqual(unrelated, []string{"public.elsewhere"}) || len(missing) != 2 {
		t.Errorf("missing %v unrelated %v", missing, unrelated)
	}
	if _, err := checkChoice(plan, []api.RelatedTableChoice{{Schema: "public", Table: "orders"}, {Schema: "public", Table: "orders"}}); err == nil {
		t.Error("a table named twice is accepted")
	}
}

func TestFilterSetBlockersKeepsWhatTheKeysDoNotClear(t *testing.T) {
	keys := []relEdge{{Name: "orders_customer_fk"}}
	in := []api.TableBlocker{
		{Code: "foreign_key_plain_table", Object: "orders_customer_fk"},
		{Code: "foreign_key_plain_table", Object: "orders_other_fk"},
		{Code: "trigger", Object: "orders_customer_fk"},
		{Code: "referenced_by_plain_table", Object: "orders_customer_fk"},
	}
	got := filterSetBlockers(in, keys)
	if len(got) != 2 || got[0].Object != "orders_other_fk" || got[1].Code != "trigger" {
		t.Errorf("%+v", got)
	}
}

// ---------------------------------------------------------------------------------------------------------------------------
// The task
// ---------------------------------------------------------------------------------------------------------------------------

// relFake is the database side of a distribute with related tables, in memory: tables that are distributed, keys that exist.
type relFake struct {
	*fakeBackend
	rmu     sync.Mutex
	graph   relGraph
	dist    map[string]bool
	keys    map[string]bool
	events  []string
	failOn  string // Blocking fails once for this table
	addFail map[string]bool
}

func newRelFake() *relFake {
	f := &relFake{fakeBackend: &fakeBackend{}, graph: shopGraph(), dist: map[string]bool{}, keys: map[string]bool{}, addFail: map[string]bool{}}
	for _, e := range f.graph.Edges {
		f.keys[e.Name] = true
	}
	return f
}

func (f *relFake) ev(s string) { f.rmu.Lock(); f.events = append(f.events, s); f.rmu.Unlock() }

func (f *relFake) eventsText() string {
	f.rmu.Lock()
	defer f.rmu.Unlock()
	return strings.Join(f.events, "|")
}

func (f *relFake) Graph(context.Context, tableParams) (relGraph, error) { return f.graph, nil }

func (f *relFake) Distributed(_ context.Context, p tableParams) (bool, error) {
	f.rmu.Lock()
	defer f.rmu.Unlock()
	return f.dist[p.Table], nil
}

func (f *relFake) Blocking(_ context.Context, p tableParams) error {
	f.rmu.Lock()
	defer f.rmu.Unlock()
	if f.failOn == p.Table {
		f.failOn = ""
		f.events = append(f.events, "fail "+p.Table)
		return errors.New("node 3 is down")
	}
	f.dist[p.Table] = true
	f.events = append(f.events, "distribute "+p.Table+" col="+p.Column+" with="+p.ColocTable)
	return nil
}

func (f *relFake) DropKeys(_ context.Context, _ tableParams, keys []relatedKey) error {
	f.rmu.Lock()
	defer f.rmu.Unlock()
	for _, k := range keys {
		delete(f.keys, k.Name)
	}
	f.events = append(f.events, "drop")
	return nil
}

func (f *relFake) AddKeys(_ context.Context, _ tableParams, keys []relatedKey) ([]string, error) {
	f.rmu.Lock()
	defer f.rmu.Unlock()
	var failed []string
	for _, k := range keys {
		if f.keys[k.Name] {
			continue
		}
		if f.addFail[k.Name] {
			failed = append(failed, k.Name+" on "+k.Schema+"."+k.Table)
			continue
		}
		f.keys[k.Name] = true
	}
	f.events = append(f.events, "add")
	return failed, nil
}

func (f *relFake) ValidateKeys(_ context.Context, _ tableParams, keys []relatedKey) error {
	f.ev("validate")
	return nil
}

func relatedInput(t *testing.T, f *relFake, root string, rootCol string, choice []api.RelatedTableChoice) tableParams {
	t.Helper()
	o := &Ops{tableBackendOverride: f}
	p := tableParams{V: 2, Op: "distribute", Method: "hash", Database: "shop", Schema: "public", Table: root, Column: rootCol}
	out, err := o.prepareRelated(context.Background(), p, api.DistributeTableRequest{Method: "hash", DistributionColumn: rootCol, RelatedTables: choice})
	if err != nil {
		t.Fatalf("prepareRelated: %v", err)
	}
	return out
}

// tenantGraph is a set whose keys all carry tenant_id, so every column follows from the root.
func tenantGraph() relGraph {
	return relGraph{
		Nodes: []relNode{{Key: tk("public", "customers"), Rows: i64(10), Bytes: i64(8192)}, {Key: tk("public", "orders"), Rows: i64(100), Bytes: i64(16384)}, {Key: tk("public", "items"), Rows: i64(1000), Bytes: i64(32768)}},
		Edges: []relEdge{
			{Name: "orders_customer_fk", Child: tk("public", "orders"), Parent: tk("public", "customers"), Cols: []string{"tenant_id", "customer_id"}, RefCols: []string{"tenant_id", "id"},
				Def: "FOREIGN KEY (tenant_id, customer_id) REFERENCES customers(tenant_id, id)", Validated: true},
			{Name: "items_order_fk", Child: tk("public", "items"), Parent: tk("public", "orders"), Cols: []string{"tenant_id", "order_id"}, RefCols: []string{"tenant_id", "id"},
				Def: "FOREIGN KEY (tenant_id, order_id) REFERENCES orders(tenant_id, id)", Validated: false},
		},
	}
}

func tenantFake() *relFake {
	f := newRelFake()
	f.graph = tenantGraph()
	f.keys = map[string]bool{"orders_customer_fk": true, "items_order_fk": true}
	return f
}

var tenantChoice = []api.RelatedTableChoice{{Schema: "public", Table: "orders"}, {Schema: "public", Table: "items"}}

func TestPrepareRelatedCapturesTheKeysAndRefusesWhatIsNotWhole(t *testing.T) {
	f := tenantFake()
	f.rows, f.bytes = 10, 8192
	p := relatedInput(t, f, "customers", "tenant_id", tenantChoice)
	if len(p.Related) != 2 || p.Related[0].Column != "tenant_id" || p.Related[1].Column != "tenant_id" || len(p.Keys) != 2 || p.Online {
		t.Fatalf("%+v", p)
	}
	if p.EstRows != 1110 || p.EstBytes < 8192+16384+32768 {
		t.Errorf("estimate %d rows %d bytes", p.EstRows, p.EstBytes)
	}
	var validated []bool
	for _, k := range p.Keys {
		validated = append(validated, k.Validated)
	}
	if !reflect.DeepEqual(validated, []bool{true, false}) && !reflect.DeepEqual(validated, []bool{false, true}) {
		t.Errorf("validated flags %v", validated)
	}
	o := &Ops{tableBackendOverride: f}
	base := tableParams{V: 2, Op: "distribute", Method: "hash", Database: "shop", Schema: "public", Table: "customers", Column: "tenant_id"}
	_, err := o.prepareRelated(context.Background(), base, api.DistributeTableRequest{Method: "hash", DistributionColumn: "tenant_id",
		RelatedTables: []api.RelatedTableChoice{{Schema: "public", Table: "orders"}}})
	var pe *problem.Problem
	if !errors.As(err, &pe) || pe.Code != problem.TableNotEligible || !strings.Contains(err.Error(), "related_table_missing") {
		t.Errorf("a partial list: %v", err)
	}
	if _, err := o.prepareRelated(context.Background(), base, api.DistributeTableRequest{Method: "hash", DistributionColumn: "tenant_id", Mode: "online", RelatedTables: tenantChoice}); err == nil ||
		!strings.Contains(err.Error(), "invalid") {
		t.Errorf("online with related tables: %v", err)
	}
	if _, err := o.prepareRelated(context.Background(), base, api.DistributeTableRequest{Method: "reference", RelatedTables: tenantChoice}); err == nil {
		t.Error("related tables of a reference table")
	}
}

func TestPrepareRelatedRefusesABusyTable(t *testing.T) {
	f := tenantFake()
	o, e, _, _ := newRelOps(t, f)
	if _, err := e.Submit(context.Background(), KindTableAlter, task.Subject{Type: "table", ID: "shop/public.orders"}, tableParams{V: 2, Op: "alter", Database: "shop", Schema: "public", Table: "orders"}, "u"); err != nil {
		t.Fatal(err)
	}
	base := tableParams{V: 2, Op: "distribute", Method: "hash", Database: "shop", Schema: "public", Table: "customers", Column: "tenant_id"}
	_, err := o.prepareRelated(context.Background(), base, api.DistributeTableRequest{Method: "hash", DistributionColumn: "tenant_id", RelatedTables: tenantChoice})
	if err == nil || !strings.Contains(err.Error(), "related_table_busy") {
		t.Errorf("%v", err)
	}
}

func newRelOps(t *testing.T, b tableBackend) (*Ops, *task.Engine, *task.MemStore, *testClock) {
	t.Helper()
	clk := &testClock{t: time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)}
	mem := task.NewMemStoreClock(clk.Now)
	o := &Ops{tableBackendOverride: b}
	opts := task.Options{Holder: "A", Now: clk.Now, Lease: 30 * time.Second, StoreTries: 1}
	e, err := task.NewEngine(mem, o.tableDefinitions(), opts)
	if err != nil {
		t.Fatal(err)
	}
	o.SetEngine(e)
	return o, e, mem, clk
}

func runRelated(t *testing.T, f *relFake) (task.Task, *Ops) {
	t.Helper()
	o, e, _, _ := newRelOps(t, f)
	in := relatedInput(t, f, "customers", "tenant_id", tenantChoice)
	sub, err := e.Submit(context.Background(), KindTableDistribute, task.Subject{Type: "table", ID: "shop/public.customers"}, in, "u")
	if err != nil {
		t.Fatal(err)
	}
	wantSteps := []string{stepRelCheck, stepFreeSpace, stepRelDrop, "Distribute public.customers", "Distribute public.orders", "Distribute public.items", stepRelAdd, stepRelValidate}
	if got := stepNames(sub); strings.Join(got, "|") != strings.Join(wantSteps, "|") {
		t.Fatalf("steps %v", got)
	}
	if ran, err := e.RunOnce(context.Background()); !ran || err != nil {
		t.Fatalf("%v %v", ran, err)
	}
	got, _ := e.Get(context.Background(), sub.ID)
	return got, o
}

func TestRelatedTaskDistributesTableByTableAndPutsTheKeysBack(t *testing.T) {
	f := tenantFake()
	got, _ := runRelated(t, f)
	if got.State != task.Done {
		t.Fatalf("%s %+v", got.State, got.Error)
	}
	want := "drop|distribute customers col=tenant_id with=|distribute orders col=tenant_id with=customers|distribute items col=tenant_id with=customers|add|validate"
	if f.eventsText() != want {
		t.Errorf("events\n got %s\nwant %s", f.eventsText(), want)
	}
	if !f.keys["orders_customer_fk"] || !f.keys["items_order_fk"] {
		t.Errorf("keys %v", f.keys)
	}
}

func TestRelatedTaskFailureNamesTheKeysThatCouldNotBePutBack(t *testing.T) {
	f := tenantFake()
	f.failOn = "orders"
	f.addFail["items_order_fk"] = true
	got, _ := runRelated(t, f)
	if got.State != task.Failed || got.Error == nil {
		t.Fatalf("%s", got.State)
	}
	if !strings.Contains(got.Error.Message, "node 3 is down") || !strings.Contains(got.Error.Message, "were not put back") || !strings.Contains(got.Error.Message, "items_order_fk") {
		t.Errorf("message %q", got.Error.Message)
	}
	// the key that could be put back is back; the table that was distributed stays distributed
	if !f.keys["orders_customer_fk"] || f.keys["items_order_fk"] || !f.dist["customers"] || f.dist["orders"] {
		t.Errorf("keys %v dist %v", f.keys, f.dist)
	}
}

func TestRelatedTaskGoesOnWithTheTablesThatAreNotDistributedYet(t *testing.T) {
	f := tenantFake()
	f.dist["customers"] = true // an earlier run distributed the table and was stopped
	delete(f.keys, "orders_customer_fk")
	delete(f.keys, "items_order_fk")
	got, _ := runRelated(t, f)
	if got.State != task.Done {
		t.Fatalf("%s %+v", got.State, got.Error)
	}
	if strings.Contains(f.eventsText(), "distribute customers") || !strings.Contains(f.eventsText(), "distribute orders") {
		t.Errorf("events %s", f.eventsText())
	}
	if !f.keys["orders_customer_fk"] || !f.keys["items_order_fk"] {
		t.Errorf("keys %v", f.keys)
	}
}

func TestRelatedTaskIsCancellableOnlyBeforeTheKeysAreTaken(t *testing.T) {
	steps := func(states ...string) task.Task {
		names := []string{stepRelCheck, stepFreeSpace, stepRelDrop, "Distribute public.customers"}
		var s []task.StepState
		for i, n := range names {
			st := task.StepPending
			if i < len(states) {
				st = states[i]
			}
			s = append(s, task.StepState{Name: n, State: st})
		}
		return task.Task{Steps: s}
	}
	if !tableCancellable(steps(task.StepDone, task.StepDone)) {
		t.Error("not cancellable before the keys are taken out")
	}
	if tableCancellable(steps(task.StepDone, task.StepDone, task.StepRunning)) {
		t.Error("cancellable while the keys are taken out")
	}
}

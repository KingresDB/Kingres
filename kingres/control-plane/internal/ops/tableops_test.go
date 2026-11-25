// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// Tests of the operation indicator of a table (contract 0.32.0, Table.operation) made from the tasks. The records of the sharding extension are read
// through the guard and need a database; they are tested in tableops_pg_test.go.

func TestTableOperationsAreMadeFromTheActiveTasksOfTheDatabase(t *testing.T) {
	f := tenantFake()
	o, e, _, _ := newRelOps(t, f)
	ctx := context.Background()
	submit := func(kind, db, schema, table string, in tableParams) task.Task {
		tk, err := e.Submit(ctx, kind, task.Subject{Type: "table", ID: db + "/" + schema + "." + table}, in, "u")
		if err != nil {
			t.Fatal(err)
		}
		return tk
	}
	alter := submit(KindTableAlter, "shop", "public", "orders", tableParams{V: 2, Op: "alter", Database: "shop", Schema: "public", Table: "orders"})
	in := relatedInput(t, f, "customers", "tenant_id", tenantChoice)
	rel := submit(KindTableDistribute, "shop", "public", "customers", in)
	submit(KindTablePartition, "other", "public", "orders", tableParams{V: 2, Op: "partition", Database: "other", Schema: "public", Table: "orders"})

	ops := o.TableOperations(ctx, "shop")
	if len(ops) != 3 {
		t.Fatalf("%d operations: %+v", len(ops), ops)
	}
	orders := ops["shop/public.orders"]
	if orders.Kind != "alter" || orders.Role != "subject" || orders.Source != "task" || orders.TaskID == nil || *orders.TaskID != alter.ID || orders.State != "pending" || orders.Percent != nil {
		t.Errorf("orders %+v", orders)
	}
	if c := ops["shop/public.customers"]; c.Kind != "distribute" || c.Role != "subject" || *c.TaskID != rel.ID {
		t.Errorf("customers %+v", c)
	}
	if it := ops["shop/public.items"]; it.Kind != "distribute" || it.Role != "related" || *it.TaskID != rel.ID {
		t.Errorf("items %+v", it)
	}
	if _, ok := ops["other/public.orders"]; ok {
		t.Error("a task of another database is shown")
	}

	items := []api.Table{{Schema: "public", Name: "customers"}, {Schema: "public", Name: "elsewhere"}, {Schema: "public", Name: "items"}}
	o.AnnotateTables(ctx, "shop", items)
	if items[0].Operation == nil || items[1].Operation != nil || items[2].Operation == nil || items[2].Operation.Role != "related" {
		t.Errorf("annotated %+v", items)
	}
}

func TestFinishedTasksAreNotAnOperation(t *testing.T) {
	f := tenantFake()
	o, e, _, _ := newRelOps(t, f)
	ctx := context.Background()
	if _, err := e.Submit(ctx, KindTablePartition, task.Subject{Type: "table", ID: "shop/public.orders"}, tableParams{V: 2, Op: "partition", PartOp: "create", Database: "shop", Schema: "public", Table: "orders"}, "u"); err != nil {
		t.Fatal(err)
	}
	if ran, err := e.RunOnce(ctx); !ran || err != nil {
		t.Fatalf("%v %v", ran, err)
	}
	if ops := o.TableOperations(ctx, "shop"); len(ops) != 0 {
		t.Errorf("a finished task is an operation: %+v", ops)
	}
}

func TestOperationKindsOfTheTableTasks(t *testing.T) {
	for kind, want := range map[string]string{KindTableDistribute: "distribute", KindTableUndistribute: "undistribute", KindTableAlter: "alter", KindTablePartition: "partition",
		KindSetStorage: "convertStorage", KindMaintain: "maintain", KindTableExport: "export", KindTableImport: "import", "rebalance": ""} {
		if got := operationKind(kind); got != want {
			t.Errorf("%s: %q, want %q", kind, got, want)
		}
	}
	for kind, want := range map[string]string{"distribute": "distribute", "distribute_reference": "distribute", "undistribute": "undistribute", "alter": "alter", "weird": "other"} {
		if got := extensionOperationKind(kind); got != want {
			t.Errorf("extension %s: %q, want %q", kind, got, want)
		}
	}
}

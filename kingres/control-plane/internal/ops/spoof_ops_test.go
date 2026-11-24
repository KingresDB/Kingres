// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extguard"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/spooftest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
)

func codeOf(err error) problem.Code {
	var p *problem.Problem
	if errors.As(err, &p) {
		return p.Code
	}
	return ""
}

// TestSpoofHarnessRunsMarkers is the control of the test below: a superuser that reads the schema of the tenant does run its code. Without it a clean result of the
// other test would prove nothing.
func TestSpoofHarnessRunsMarkers(t *testing.T) {
	env := spooftest.New(t)
	ctx := context.Background()
	c, err := env.ServiceConn(ctx, spooftest.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close(ctx) }()
	var n int
	if err := c.QueryRow(ctx, `SELECT count(*) FROM kingres.node`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if err := c.QueryRow(ctx, `SELECT to_regprocedure('kingres.set_sharding_mode(text,boolean)') IS NOT NULL`).Scan(new(bool)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, `SELECT kingres.set_sharding_mode('schema', NULL)`); err != nil {
		t.Fatal(err)
	}
	m := env.Markers(t)
	if len(m) != 2 {
		t.Fatalf("a superuser session that reads a view and calls a function of the tenant runs both: %v", m)
	}
}

// TestSchemaSpoofIsNeverExecutedByOps: a database that a tenant owns, with no KingresDB extension and a schema kingres of the tenant whose views and functions
// write a marker when they run. The operations of the control plane that read or call kingres.* as the service role (a superuser) in a user database run against
// it. None of the objects of the tenant may run, and the operations refuse or answer as for a database without the extension.
func TestSchemaSpoofIsNeverExecutedByOps(t *testing.T) {
	env := spooftest.New(t)
	ctx := context.Background()
	db := spooftest.Database
	o := &Ops{Service: env, ServerHost: "10.0.0.1", PGPort: 5432}
	check := func(t *testing.T, what string) { t.Helper(); env.NoMarkers(t, what) }

	t.Run("tables: prepare distribute, undistribute, alter, partition, list partitions", func(t *testing.T) {
		_, err := o.PrepareDistribute(ctx, db, "public", "t", api.DistributeTableRequest{Method: "hash", DistributionColumn: "id"})
		_ = err // the request may be refused for other reasons; what matters is that the schema of the tenant did not run
		check(t, "PrepareDistribute")
		_, err = o.PrepareUndistribute(ctx, db, "public", "t", api.UndistributeOptions{})
		if codeOf(err) != problem.ExtensionMissing {
			t.Fatalf("PrepareUndistribute on a database without kingres_shard: %v", err)
		}
		check(t, "PrepareUndistribute")
		_, err = o.PrepareAlter(ctx, db, "public", "t", api.AlterTableDistributionRequest{DistributionColumn: "id"})
		check(t, "PrepareAlter")
		_ = err
		name := "p1"
		_, err = o.PreparePartition(ctx, db, "public", "t", "create", name, &api.CreateTablePartitionRequest{}, nil)
		check(t, "PreparePartition")
		_ = err
		page, err := o.ListTablePartitions(ctx, db, "public", "t", 10, "")
		if err != nil || len(page.Items) != 0 {
			t.Fatalf("ListTablePartitions: %+v %v", page, err)
		}
		check(t, "ListTablePartitions")
	})

	t.Run("tables: enable the ledger", func(t *testing.T) {
		err := o.EnableLedger(ctx, db, "public", "t", api.EnableTableLedgerRequest{Mode: "appendOnly"})
		if codeOf(err) != problem.Conflict {
			t.Fatalf("EnableLedger without kingres_ledger: %v", err)
		}
		check(t, "EnableLedger")
	})

	t.Run("tables: the table tasks (report, sizes, unfinished operation)", func(t *testing.T) {
		b := pgBackend{o}
		p := tableParams{V: 2, Op: "distribute", Method: "hash", Database: db, Schema: "public", Table: "t", Column: "id"}
		if _, _, _, err := b.Report(ctx, p); codeOf(err) != problem.UnsupportedOp {
			t.Fatalf("Report: %v", err)
		}
		if _, _, _, err := b.Sizes(ctx, p); codeOf(err) != problem.UnsupportedOp {
			t.Fatalf("Sizes: %v", err)
		}
		if _, _, err := b.UnfinishedOp(ctx, p); codeOf(err) != problem.UnsupportedOp {
			t.Fatalf("UnfinishedOp: %v", err)
		}
		check(t, "the table tasks")
	})

	t.Run("tables: the distribution step", func(t *testing.T) {
		// stepDistribute opens its own connection and gates it before it reads kingres.dist_table
		c, err := env.ServiceConn(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close(ctx) }()
		if err := requireExt(ctx, c, extguard.Shard); codeOf(err) != problem.ExtensionMissing {
			t.Fatalf("requireExt: %v", err)
		}
		check(t, "requireExt")
	})

	t.Run("partition policy and runs, maintain, the scheduler", func(t *testing.T) {
		if _, err := o.GetPartitionPolicy(ctx, db, "public", "t"); codeOf(err) != problem.NotFound {
			t.Fatalf("GetPartitionPolicy: %v", err)
		}
		err := o.policyTx(ctx, db, func(tx pgx.Tx) error { t.Fatal("the transaction body ran without kingres_timepart"); return nil })
		if codeOf(err) != problem.ExtensionMissing {
			t.Fatalf("policyTx: %v", err)
		}
		if _, err := o.trustedConn(ctx, db, extguard.Timepart); codeOf(err) != problem.ExtensionMissing {
			t.Fatalf("trustedConn: %v", err)
		}
		pb := PartitionBackend{O: o}
		if _, err := pb.Due(ctx, time.Now()); err != nil {
			t.Fatalf("Due: %v", err)
		}
		if _, _, err := pb.Checks(ctx); err != nil {
			t.Fatalf("Checks: %v", err)
		}
		check(t, "partition policy")
	})

	t.Run("storage conversion preview", func(t *testing.T) {
		bl, _, err := o.storageRefusals(ctx, storageParams{Database: db, Schema: "public", Table: "t", AccessMethod: "columnar"})
		_, _ = bl, err
		check(t, "storageRefusals")
	})

	t.Run("schemas: list, get, prepare", func(t *testing.T) {
		page, err := o.ListSchemas(ctx, db, SchemaFilter{}, store.Page{Limit: 50})
		if err != nil {
			t.Fatalf("ListSchemas: %v", err)
		}
		for _, s := range page.Items {
			if s.Distribution != nil {
				t.Fatalf("schema %s reports a distribution that only the schema of the tenant could have made", s.Name)
			}
			if s.Name == "kingres" {
				t.Fatalf("the schema kingres is listed as a schema of the database")
			}
		}
		check(t, "ListSchemas")
		if _, err := o.GetSchema(ctx, db, "public"); err != nil {
			t.Fatalf("GetSchema: %v", err)
		}
		check(t, "GetSchema")
		_, err = o.PrepareDistributeSchema(ctx, db, "public", api.DistributeSchemaRequest{})
		if codeOf(err) != problem.UnsupportedOp {
			t.Fatalf("PrepareDistributeSchema: %v", err)
		}
		_, err = o.PrepareMoveSchema(ctx, db, "public", api.MoveSchemaRequest{TargetShardGroupID: 1})
		if codeOf(err) != problem.UnsupportedOp {
			t.Fatalf("PrepareMoveSchema: %v", err)
		}
		_, err = o.PrepareUndistributeSchema(ctx, db, "public", nil)
		if codeOf(err) != problem.UnsupportedOp {
			t.Fatalf("PrepareUndistributeSchema: %v", err)
		}
		check(t, "the schema requests")
	})

	t.Run("server groups: shouldHaveShards and weight in every database", func(t *testing.T) {
		if err := o.SetShouldHaveShards(ctx, "10.0.0.1", false); err != nil {
			t.Fatalf("SetShouldHaveShards: %v", err)
		}
		if err := o.setWeight(ctx, "10.0.0.1", 2); err != nil {
			t.Fatalf("setWeight: %v", err)
		}
		check(t, "server groups")
	})

	t.Run("databases: sharding mode", func(t *testing.T) {
		if err := o.setShardingMode(ctx, db, "schema", nil); codeOf(err) != problem.UnsupportedOp {
			t.Fatalf("setShardingMode: %v", err)
		}
		check(t, "setShardingMode")
	})

	t.Run("export and import: the distribution and the ledger policy", func(t *testing.T) {
		class, groups, err := o.distribution(ctx, db, "public", "t")
		if err != nil || class != "local" || groups != 1 {
			t.Fatalf("distribution: %q %d %v", class, groups, err)
		}
		pol, err := o.ledgerPolicy(ctx, db, "public", "t")
		if err != nil || pol != "" {
			t.Fatalf("ledgerPolicy: %q %v", pol, err)
		}
		check(t, "export and import")
	})
}

// TestUntrustedSchemaIsRefusedByOps is the other attack: kingres_shard is installed (by the service role), but into a schema kingres that the tenant made and owns.
// The operations that read or call the sharding catalog refuse with extension_unavailable, say that the schema cannot be trusted, and none of the code of the tenant
// runs.
func TestUntrustedSchemaIsRefusedByOps(t *testing.T) {
	env := spooftest.NewUntrusted(t)
	ctx := context.Background()
	db := spooftest.Database
	o := &Ops{Service: env, ServerHost: "10.0.0.1", PGPort: 5432}
	refused := func(what string, err error) {
		t.Helper()
		var p *problem.Problem
		if !errors.As(err, &p) || p.Code != problem.ExtensionUnavailable || !strings.Contains(p.Detail, "cannot be trusted") {
			t.Fatalf("%s: %v, want problem extension_unavailable that says the schema cannot be trusted", what, err)
		}
		env.NoMarkers(t, what)
	}
	_, err := o.PrepareUndistribute(ctx, db, "public", "t", api.UndistributeOptions{})
	refused("PrepareUndistribute", err)
	_, err = o.ListSchemas(ctx, db, SchemaFilter{}, store.Page{Limit: 10})
	refused("ListSchemas", err)
	_, err = o.GetSchema(ctx, db, "public")
	refused("GetSchema", err)
	_, err = o.PrepareDistributeSchema(ctx, db, "public", api.DistributeSchemaRequest{})
	refused("PrepareDistributeSchema", err)
	_, err = o.ListTablePartitions(ctx, db, "public", "t", 10, "")
	refused("ListTablePartitions", err)
	_, _, _, err = pgBackend{o}.Report(ctx, tableParams{V: 2, Op: "distribute", Method: "hash", Database: db, Schema: "public", Table: "t", Column: "id"})
	refused("Report", err)
	refused("SetShouldHaveShards", o.SetShouldHaveShards(ctx, "10.0.0.1", true))
	refused("setShardingMode", o.setShardingMode(ctx, db, "schema", nil))
	_, _, err = o.distribution(ctx, db, "public", "t")
	refused("distribution", err)
	// the extension is trusted where it is alone in its schema: the answer for the other databases is unchanged
	c, err := env.ServiceConn(ctx, "postgres")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close(ctx) }()
	if ok, err := extUsable(ctx, c, extguard.Shard); err != nil || ok {
		t.Fatalf("extUsable in a database without the extension: %v %v", ok, err)
	}
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package rebalance

import (
	"context"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

// The distributed schemas of phase 11 are items of the plan (design 9.9): the loop runs them like buckets, with the same retries, the
// same records and the same report, and the contract names them (kind, schemaName).

func TestMoveKindNamesADistributedSchema(t *testing.T) {
	if k, n := (Move{}).Kind(); k != "move" || n != nil {
		t.Errorf("a bucket: %q %v", k, n)
	}
	if k, n := (Move{Schema: "tenant_7"}).Kind(); k != "schema" || n == nil || *n != "tenant_7" {
		t.Errorf("a schema: %q %v", k, n)
	}
	// the contract members are left out for a bucket, so that its output is what it always was
	if k, n := schemaOf(Move{}); k != "" || n != nil {
		t.Errorf("schemaOf a bucket: %q %v", k, n)
	}
	if k, n := schemaOf(Move{Schema: `a "b"`}); k != "schema" || n == nil || *n != `a "b"` {
		t.Errorf("schemaOf a schema: %q %v", k, n)
	}
}

// A schema item is run with its name (the call is move_schema), retried after a failure, and recorded with its name in the report.
func TestASchemaItemIsRunRetriedAndReported(t *testing.T) {
	c := newCluster(map[int32]int32{0: 1, 1: 1}, map[int32]int32{0: 2, 1: 2})
	c.schema = map[int32]string{1: "tenant_7"}
	c.failFor[1] = 1
	r := &memRows{}
	_, _ = r.CreateRun(context.Background(), Options{}, nil)
	if err := testLoop(c, r).Moves(context.Background(), Options{}); err != nil {
		t.Fatal(err)
	}
	if c.owner[0] != 2 || c.owner[1] != 2 {
		t.Fatalf("owners %v", c.owner)
	}
	var tries, bucketRuns int
	for _, m := range c.ran {
		switch {
		case m.Schema == "tenant_7" && m.Bucket == 1:
			tries++
		case m.Schema == "" && m.Bucket == 0:
			bucketRuns++
		default:
			t.Errorf("a move the backend got with the wrong identity: %+v", m)
		}
	}
	if tries != 2 || bucketRuns != 1 {
		t.Fatalf("the schema was tried %d times, the bucket %d", tries, bucketRuns)
	}
	moves, _ := r.Moves(context.Background())
	if v := Judge(moves, 2); v.Outcome != OutcomeCompleted || v.Done != 2 {
		t.Fatalf("verdict %+v", v)
	}
	rep := reportMoves(moves)
	var seen int
	for _, m := range rep {
		if m.Bucket == 1 {
			seen++
			if m.Kind != "schema" || m.SchemaName == nil || *m.SchemaName != "tenant_7" || m.Attempts != 2 || m.Outcome != "done" {
				t.Errorf("the report entry of the schema: %+v", m)
			}
		} else if m.Kind != "" || m.SchemaName != nil {
			t.Errorf("the report entry of a bucket carries a schema: %+v", m)
		}
	}
	if seen != 1 {
		t.Fatalf("report: %+v", rep)
	}
}

// A schema that cannot be moved is given up after the attempts and the run is failed, with the schema named in its entry, like a bucket.
func TestASchemaThatFailsEveryTimeFailsTheRun(t *testing.T) {
	c := newCluster(map[int32]int32{0: 1}, map[int32]int32{0: 2})
	c.schema = map[int32]string{0: "tenant_9"}
	c.failFor[0] = 100
	r := &memRows{}
	_, _ = r.CreateRun(context.Background(), Options{DrainOnly: true}, nil)
	if err := testLoop(c, r).Moves(context.Background(), Options{DrainOnly: true}); err != nil {
		t.Fatal(err)
	}
	moves, _ := r.Moves(context.Background())
	if v := Judge(moves, 1); v.Outcome != OutcomeFailed || v.Failed != 1 {
		t.Fatalf("verdict %+v", v)
	}
	rep := reportMoves(moves)
	if len(rep) != 1 || rep[0].Outcome != "failed" || rep[0].SchemaName == nil || *rep[0].SchemaName != "tenant_9" {
		t.Fatalf("report %+v", rep)
	}
}

// The blockers of a drain: a pinned schema is schema_pinned, every other reason is unprocessable, a schema that can move is no blocker.
func TestSchemaBlockersOfADrain(t *testing.T) {
	got := schemaBlockers([]SchemaOnServer{
		{Database: "shop", Schema: "free", Group: 1},
		{Database: "shop", Schema: "ledgered", Group: 1, Blocker: `schema_pinned: schema "ledgered" holds the ledger table(s) t on shard group 1`},
		{Database: "crm", Schema: "nokey", Group: 2, Blocker: "table nokey.t has no primary key and no full replica identity"},
		{Database: "crm", Schema: "silent", Group: 2, Blocker: "shard group 2 did not answer: timeout"},
	})
	if len(got) != 3 {
		t.Fatalf("blockers: %+v", got)
	}
	if got[0].Code != string(problem.SchemaPinned) || !strings.Contains(got[0].Detail, `"ledgered"`) || !strings.Contains(got[0].Detail, "shop") {
		t.Errorf("pinned: %+v", got[0])
	}
	for _, b := range got[1:] {
		if b.Code != string(problem.Unprocessable) {
			t.Errorf("other: %+v", b)
		}
	}
	if len(schemaBlockers(nil)) != 0 || len(schemaBlockers([]SchemaOnServer{{Schema: "free"}})) != 0 {
		t.Error("a schema that can move is not a blocker")
	}
	var _ []api.DrainBlocker = got
}

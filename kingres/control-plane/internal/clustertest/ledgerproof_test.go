// SPDX-License-Identifier: PostgreSQL
// Portions Copyright (c) 2026, RooyeKhat Media Co

package clustertest_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// The verification records are read from the manager leader whichever server
// answers: a run started on a follower (forwarded, as every mutating cluster
// operation) is listed with its task by the leader and by the follower, and
// the proof is made on the chain of the server that holds the table.
func TestLedgerVerificationRecordsAndProofsInACluster(t *testing.T) {
	c := sharedCluster(t)
	leader, follower := c.Leader(), c.Nodes[1]
	ctx := context.Background()
	admin := leader.MustPG(clustertest.AdminUser, clustertest.AdminPass, "before_db")
	if _, err := admin.Exec(ctx, "CREATE TABLE lpt (id int PRIMARY KEY, v text)"); err != nil {
		t.Fatalf("creating the table: %v", err)
	}
	leader.Expect(200, "PUT", "/databases/before_db/schemas/public/tables/lpt/ledger", map[string]any{"mode": "appendOnly"})
	if _, err := admin.Exec(ctx, "INSERT INTO lpt VALUES (1, 'a')"); err != nil {
		t.Fatal(err)
	}
	d := leader.Expect(201, "POST", "/databases/before_db/ledger/digests", nil)
	chains, _ := d["chains"].([]any)
	if len(chains) != 1 || chains[0].(map[string]any)["document"] == nil {
		t.Fatalf("digest without a document: %v", d)
	}
	var group int32
	if err := leader.ServicePG("before_db").QueryRow(ctx, `SELECT n.group_id FROM kingres.local_node l JOIN kingres.node n ON n.node_id = l.node_id`).Scan(&group); err != nil {
		t.Fatal(err)
	}
	if got := int32(chains[0].(map[string]any)["shardGroupId"].(float64)); got != group {
		t.Errorf("the digest's chain is group %d, the local node's group is %d", got, group)
	}
	proof := leader.Expect(200, "POST", "/databases/before_db/ledger/inclusion-proofs",
		map[string]any{"schema": "public", "table": "lpt", "primaryKey": map[string]any{"id": 1}})
	if proof["shard_group_id"] != float64(group) || proof["relation"] != "public.lpt" {
		t.Errorf("proof %v", proof)
	}

	// A verification started on the follower runs on the leader as a task.
	task := follower.Expect(202, "POST", "/databases/before_db/ledger/verifications", map[string]any{"scope": "full"})
	done := follower.WaitTaskDone(clustertest.TaskID(task), 4*time.Minute)
	if done["state"] != "done" {
		t.Fatalf("verification task: %v", done)
	}
	for _, n := range []*clustertest.Node{leader, follower} {
		list := n.Expect(200, "GET", "/databases/before_db/ledger/verifications", nil)
		items, _ := list["items"].([]any)
		if len(items) == 0 {
			t.Fatalf("node %d lists no verification: %v", n.Index, list)
		}
		first := items[0].(map[string]any)
		if first["taskId"] != task["id"] || first["state"] != "passed" || first["scope"] != "full" {
			t.Errorf("node %d: newest run %v (task %v)", n.Index, first, task["id"])
		}
		got := n.Expect(200, "GET", "/databases/before_db/ledger/verifications/"+itoa(first["id"]), nil)
		if got["taskId"] != task["id"] || got["id"] != first["id"] {
			t.Errorf("node %d: get %v", n.Index, got)
		}
	}
}

func itoa(v any) string {
	f, _ := v.(float64)
	return fmt.Sprint(int64(f))
}

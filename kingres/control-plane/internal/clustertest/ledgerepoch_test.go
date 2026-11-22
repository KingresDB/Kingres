// SPDX-License-Identifier: AGPL-3.0-only

package clustertest_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// Step 20, design 9.4: a group that lost the tail of its chain diverges; the acknowledgement closes its epoch with a signed event, keeps the
// old log, opens the log of epoch 2 built from block 0 of the chain as it is, clears the alert, and the standalone verifier accepts the new log.
func TestAcknowledgingADivergenceStartsANewEpoch(t *testing.T) {
	p := formPublishing(t, "ep_db")
	leader, follower := p.c.Nodes[0], p.c.Nodes[1]
	until(t, 90*time.Second, "both servers to be listed", func() bool { return len(leader.Servers()) == 2 })
	ledgerTable(t, leader, "ep_db", "t0", 4)
	ledgerTable(t, follower, "ep_db", "t1", 4)
	p.startPublishing(t)
	insertRows(t, follower, "ep_db", "t1", 10, 12)
	if d := leader.Expect(201, "POST", "/databases/ep_db/ledger/digests", nil); d["complete"] != true {
		t.Fatalf("%v", d)
	}
	var old pubLog
	for _, l := range publicationLogs(t, leader) {
		if l.Kind == "chain" && l.Size > 12 {
			old = l
		}
	}
	if old.Origin == "" {
		t.Fatal("no chain with more than 12 blocks")
	}
	ctx := context.Background()
	sc := follower.ServicePG("ep_db")
	for _, q := range []string{
		"SET kingres_ledger.allow_manual_write = on",
		"UPDATE pg_trigger SET tgenabled = 'D' WHERE tgname IN ('kingres_ledger_capture', 'kingres_ledger_block_modify', 'kingres_ledger_block_truncate')",
		"DELETE FROM kingres.ledger_digest WHERE height > 5",
		"DELETE FROM kingres.ledger_tx WHERE block_height > 5",
		"DELETE FROM kingres.ledger_blocks WHERE height > 5",
		"UPDATE pg_trigger SET tgenabled = 'A' WHERE tgname IN ('kingres_ledger_capture', 'kingres_ledger_block_modify', 'kingres_ledger_block_truncate')",
	} {
		if _, err := sc.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	_ = sc.Close(ctx)
	// the group that lost its tail goes on with other blocks
	insertRows(t, follower, "ep_db", "t1", 100, 9)
	until(t, 3*time.Minute, "the log to be diverged", func() bool {
		for _, l := range publicationLogs(t, leader) {
			if l.Origin == old.Origin {
				return l.State == "diverged"
			}
		}
		return false
	})
	// which shard group is it: the member of the aggregate that is flagged diverged
	group, other := -1, -1
	until(t, 2*time.Minute, "the aggregate to flag the member diverged", func() bool {
		group, other = -1, -1
		a := leader.Expect(200, "GET", "/databases/ep_db/ledger/aggregate", nil)
		for _, m := range anyList(a["members"]) {
			mm := asMap(m)
			g := int(mm["shardGroupId"].(float64))
			diverged := false
			for _, f := range anyList(mm["flags"]) {
				diverged = diverged || f == "diverged"
			}
			if diverged {
				group = g
			} else {
				other = g
			}
		}
		return group >= 0 && other >= 0
	})
	url := func(g int) string { return fmt.Sprintf("/databases/ep_db/ledger/chains/%d/acknowledge", g) }
	good := map[string]any{"kind": "restore", "reason": "the nightly backup was restored", "confirm": "ep_db"}
	// refusals
	leader.Expect(409, "POST", url(other), good)
	for _, bad := range []map[string]any{
		{"kind": "restore", "reason": "r", "confirm": "not_the_database"},
		{"kind": "unknown", "reason": "r", "confirm": "ep_db"},
		{"kind": "restore", "reason": "", "confirm": "ep_db"},
		{"kind": "restore", "reason": "r", "confirm": "ep_db", "extra": 1},
	} {
		if st, _ := leader.Do("POST", url(group), bad, nil); st != 400 {
			t.Errorf("%v: %d", bad, st)
		}
	}
	// the acknowledgement, through the follower
	task := follower.Expect(202, "POST", url(group), good)
	if task["kind"] != "ledger.epoch.start" {
		t.Fatalf("the task: %v", task)
	}
	if done := leader.WaitTaskDone(clustertest.TaskID(task), 4*time.Minute); done["state"] != "done" {
		t.Fatalf("the epoch task: %v", done)
	}
	// the event, signed and published
	closed := 0
	for _, e := range ledgerEvents(t, leader) {
		if strings.Contains(e, "epoch-closed") {
			closed++
			for _, want := range []string{"kind: restore", "acknowledged-by: " + clustertest.AdminUser, fmt.Sprintf("final-size: %d", old.Size)} {
				if !strings.Contains(e, want) {
					t.Errorf("the event lacks %q: %s", want, e)
				}
			}
		}
	}
	if closed != 1 {
		t.Errorf("%d epoch-closed events", closed)
	}
	// the old log is closed and still served; the log of epoch 2 follows the chain and the verifier accepts it
	var next pubLog
	until(t, 3*time.Minute, "the log of epoch 2 to be built", func() bool {
		for _, l := range publicationLogs(t, leader) {
			if strings.HasSuffix(l.Origin, "/e2") && l.State == "active" && l.Size > 12 {
				next = l
				return true
			}
		}
		return false
	})
	for _, l := range publicationLogs(t, leader) {
		if l.Origin == old.Origin && (l.State != "closed" || l.Size != old.Size) {
			t.Errorf("the old log: %+v", l)
		}
	}
	if st, _ := httpGet(t, p.base(0)+"/"+logPath(old.Origin)+"/checkpoint"); st != 200 {
		t.Errorf("the checkpoint of the old epoch is not served: %d", st)
	}
	until(t, 60*time.Second, "the alert to be cleared", func() bool {
		al := leader.Expect(200, "GET", "/alerts", nil)
		for _, it := range anyList(al["items"]) {
			if m := asMap(it); m["kind"] == "ledgerChainDiverged" && m["state"] != "resolved" && m["resolvedAt"] == nil {
				return false
			}
		}
		return true
	})
	auditStored(t, leader)
	verifyPublication(t, p, 0, logPath(next.Origin), uint64(next.Size))
	// the same acknowledgement again: the chain is not diverged any more
	leader.Expect(409, "POST", url(group), good)
}

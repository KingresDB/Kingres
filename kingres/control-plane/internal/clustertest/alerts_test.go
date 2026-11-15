// SPDX-License-Identifier: PostgreSQL

package clustertest_test

import (
	"context"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// ledgerPendingTooOld: raised when the oldest pending ledger transaction of a
// database has waited longer than the limit (here three seconds), for that
// database only; cleared when the queue has been built into a block. Checked
// against a real instance with the block builder switched off and on again.
func TestLedgerPendingTooOldAlert(t *testing.T) {
	c := clustertest.New(t, 1)
	n := c.Leader()
	n.Extra = []string{"-monitor-interval", "500ms", "-ledger-pending-max-age", "3s"}
	n.Start()
	n.InitCluster("ledger-alerts")
	ctx := context.Background()
	body := n.Expect(202, "POST", "/databases", map[string]any{"name": "ldb", "ledger": map[string]any{"mode": "optIn"}})
	n.WaitTaskDone(clustertest.TaskID(body), 4*time.Minute)
	body = n.Expect(202, "POST", "/databases", map[string]any{"name": "quiet", "ledger": map[string]any{"mode": "optIn"}})
	n.WaitTaskDone(clustertest.TaskID(body), 4*time.Minute)
	admin := n.MustPG(clustertest.AdminUser, clustertest.AdminPass, "ldb")
	if _, err := admin.Exec(ctx, "CREATE TABLE acc (id bigint PRIMARY KEY, v int)"); err != nil {
		t.Fatal(err)
	}
	n.Expect(200, "PUT", "/databases/ldb/schemas/public/tables/acc/ledger", map[string]any{"mode": "appendOnly"})

	svc := n.ServicePG("postgres")
	setBuilder := func(sql string) {
		t.Helper()
		if _, err := svc.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Exec(ctx, "SELECT pg_reload_conf()"); err != nil {
			t.Fatal(err)
		}
	}
	// Switch the block builder off, and wait until it has stopped.
	setBuilder("ALTER SYSTEM SET kingres_ledger.block_interval = 0")
	time.Sleep(12 * time.Second) // the launcher looks every 10 s by default
	if _, err := admin.Exec(ctx, "INSERT INTO acc VALUES (1, 1), (2, 2)"); err != nil {
		t.Fatal(err)
	}
	var pending int
	if err := n.ServicePG("ldb").QueryRow(ctx, "SELECT count(*) FROM kingres.ledger_pending").Scan(&pending); err != nil || pending == 0 {
		t.Fatalf("nothing is pending: %d %v (the block builder was not switched off)", pending, err)
	}

	waitFor := func(state string, want bool, d time.Duration) map[string]any {
		t.Helper()
		deadline := time.Now().Add(d)
		for {
			var found map[string]any
			for _, a := range items(t, n.Expect(200, "GET", "/alerts?state="+state+"&limit=200", nil)) {
				if a["kind"] == "ledgerPendingTooOld" {
					found = a
				}
			}
			if (found != nil) == want {
				return found
			}
			if time.Now().After(deadline) {
				t.Fatalf("ledgerPendingTooOld in state %s: found %v, want %v", state, found != nil, want)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	a := waitFor("open", true, 60*time.Second)
	if a["databaseName"] != "ldb" || a["serverId"] != n.Host || a["severity"] != "warning" || a["title"] != "Ledger entries pending for too long" {
		t.Errorf("the alert: %v", a)
	}
	// One alert for the database, not one per check, and none for the database without a queue.
	open := 0
	for _, x := range items(t, n.Expect(200, "GET", "/alerts?state=open&limit=200", nil)) {
		if x["kind"] == "ledgerPendingTooOld" {
			open++
		}
	}
	if open != 1 {
		t.Errorf("%d open alerts of the kind", open)
	}
	// The builder is back on: the queue is built into a block and the alert resolves.
	setBuilder("ALTER SYSTEM RESET kingres_ledger.block_interval")
	waitFor("open", false, 90*time.Second)
	if r := waitFor("resolved", true, 10*time.Second); r["databaseName"] != "ldb" || r["resolvedAt"] == nil {
		t.Errorf("the resolved alert: %v", r)
	}
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

//go:build linux

package clustertest_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// watchdogPids are the processes of the lease watchdog of the primary instance 1 of node n: found by the lease file
// named in their command line, never by the name of the program.
func watchdogPids(n *clustertest.Node) []int {
	want := filepath.Join(n.DataDir, "instances", "1", "lease.json")
	entries, _ := os.ReadDir("/proc")
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		args := strings.Split(string(b), "\x00")
		if len(args) > 0 && strings.Contains(args[0], "kingres-watchdog") && strings.Contains(string(b), want) {
			out = append(out, pid)
		}
	}
	return out
}

// T11c: the old primary runs although it should not: its kingresd and its watchdog are gone, its instance runs on, the
// failover proceeds (the lease of its group runs out at the grantors), and the old instance goes on accepting writes
// that nobody else has. When its kingresd is back it fences the instance (the catalog calls the node a standby and the
// server holds no lease), the alert oldPrimaryRunning was raised, the leader rebuilds it, the writes it took are gone
// and the report states the window.
func TestOldPrimaryRunningUnfenced(t *testing.T) {
	c, _, _ := foCluster(t)
	leader := c.Leader()
	g, old, nw := foVictim(t, c)
	waitFacts(t, leader, g)
	ctx := context.Background()
	defer func() {
		if t.Failed() {
			t.Logf("catalog of the leader: %s\nleader log tail:\n%s\nold instances:\n%s", foCatalogDump(leader), leader.LogTail(2500), instLog(old, 1, 6000))
		}
	}()
	until(t, 30*time.Second, "the watchdog of the old primary", func() bool { return len(watchdogPids(old)) > 0 })
	for _, pid := range watchdogPids(old) {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			t.Fatalf("killing the watchdog %d: %v", pid, err)
		}
	}
	old.Kill()
	data := filepath.Join(old.DataDir, "instances", "1", "pgdata")
	if _, err := os.Stat(filepath.Join(data, "postmaster.pid")); err != nil {
		t.Fatalf("the old primary's instance does not run: %v", err)
	}
	// The instance keeps running with neither kingresd nor watchdog: nothing stops it. The failover waits for the
	// lease at the grantors and proceeds.
	probe := old.ServicePG("postgres")
	opKeepWal(t, probe)
	if _, err := probe.Exec(ctx, `CREATE TABLE div_probe (id int PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	old.StopStandbys()
	st, resp := leader.DoThroughRestart("POST", "/servers/"+old.Host+"/failover", map[string]any{"mode": "hard", "allowDataLoss": true, "comment": "T11c"}, nil)
	if st != 202 {
		t.Fatalf("failover request: %d %v", st, resp)
	}
	if task := leader.WaitTaskDone(clustertest.TaskID(resp), 8*time.Minute); task["state"] != "done" {
		t.Fatalf("the failover: %v", task)
	}
	// The old primary, unfenced, takes writes nobody else has.
	if _, err := probe.Exec(ctx, `SET synchronous_commit = local`); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 5; i++ {
		if _, err := probe.Exec(ctx, `INSERT INTO div_probe VALUES ($1)`, i); err != nil {
			t.Fatalf("a write on the unfenced old primary: %v", err)
		}
	}
	_ = probe.Close(ctx)
	// The check of the failover saw it: check_cluster() reports a standby that is not in recovery.
	sys := leader.ServicePG("kingres_system")
	defer func() { _ = sys.Close(ctx) }()
	var raised int
	if err := sys.QueryRow(ctx, `SELECT count(*) FROM kingres_cp.alert WHERE kind = 'oldPrimaryRunning' AND subject LIKE '%group:'||$1`, strconv.Itoa(g)).Scan(&raised); err != nil || raised == 0 {
		t.Errorf("the alert oldPrimaryRunning was not raised (%v)", err)
	}

	// kingresd is back: the instance is fenced, and the old primary is rebuilt.
	old.Start()
	until(t, 3*time.Minute, "the instance of the old primary to be stopped by its kingresd", func() bool {
		_, err := os.Stat(filepath.Join(data, "postmaster.pid"))
		if err == nil {
			return false
		}
		return true
	})
	until(t, 10*time.Minute, "the old primary to be rebuilt as a standby", func() bool {
		r := opRecordOf(t, leader, g, old.Host)
		return r.ok && r.state == "ready" && !r.diverged
	})
	rec := opRecordOf(t, leader, g, old.Host)
	var oc *pgx.Conn
	until(t, 2*time.Minute, "a connection to the rebuilt standby", func() bool {
		cc, err := opConn(old, rec.instance, rec.port, "postgres")
		if err != nil {
			return false
		}
		oc = cc
		var n int
		var inRec bool
		if err := cc.QueryRow(ctx, `SELECT pg_is_in_recovery(), (SELECT count(*) FROM div_probe)`).Scan(&inRec, &n); err != nil {
			return false
		}
		if !inRec || n != 0 {
			t.Errorf("the rebuilt old primary: in recovery %v, %d of the 5 rows it took alone (want 0)", inRec, n)
		}
		return true
	})
	defer func() { _ = oc.Close(ctx) }()
	rep := leader.Expect(200, "GET", fmt.Sprintf("/failover/reports/%v", clustertest.TaskID(resp)), nil)
	gs, _ := rep["groups"].([]any)
	if len(gs) != 1 {
		t.Fatalf("report: %v", rep)
	}
	op, _ := gs[0].(map[string]any)["oldPrimary"].(map[string]any)
	if op["state"] != "standby" {
		t.Errorf("old primary in the report: %v", op)
	}
	if msg, _ := op["message"].(string); !strings.Contains(msg, "parted at") && func() bool { db, _ := op["divergenceBytes"].(float64); return db <= 0 }() {
		t.Errorf("the report does not state the window of the writes the old primary took: %v", op)
	}
	var open int
	if err := sys.QueryRow(ctx, `SELECT count(*) FROM kingres_cp.alert WHERE kind = 'oldPrimaryRunning' AND subject LIKE '%group:'||$1 AND cleared_at IS NULL`, strconv.Itoa(g)).Scan(&open); err != nil || open != 0 {
		t.Errorf("%d alerts oldPrimaryRunning are still open after the old primary was rebuilt (%v)", open, err)
	}
	_ = nw
}

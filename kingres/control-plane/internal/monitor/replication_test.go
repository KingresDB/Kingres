// SPDX-License-Identifier: AGPL-3.0-only

package monitor_test

import (
	"context"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/monitor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// CP2-07: a server that has not applied a change is reported, from a minute
// on as a warning and from ten minutes on as critical, and the alert goes away
// when the server is up to date.
func TestReplicationPendingConditions(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	rd := monitor.Readings{Host: "leader", InstanceUp: true, ReplicationChecked: true, Now: now, Replication: []monitor.ReplicationReading{
		{Server: "fresh", Tasks: 1, Since: now.Add(-10 * time.Second)},
		{Server: "behind", Tasks: 2, Since: now.Add(-3 * time.Minute)},
		{Server: "gone", Tasks: 1, Since: now.Add(-time.Hour)},
	}}
	conds, ev := monitor.Evaluate(rd)
	got := map[string]string{}
	for _, c := range conds {
		if c.Kind == "replicationPending" {
			got[c.Subject] = c.Severity
		}
	}
	want := map[string]string{"server:leader|behind:behind": "warning", "server:leader|behind:gone": "critical"}
	if len(got) != len(want) || got["server:leader|behind:behind"] != "warning" || got["server:leader|behind:gone"] != "critical" {
		t.Errorf("conditions %v, want %v", got, want)
	}
	if !ev["replicationPending"] {
		t.Error("replicationPending was not evaluated")
	}
	rd.ReplicationChecked = false
	if _, ev := monitor.Evaluate(rd); ev["replicationPending"] {
		t.Error("an unread table must neither raise nor clear")
	}
}

// ReadReplication finds the servers whose step of an unfinished replication task
// is not done, and nothing for finished tasks or finished steps.
func TestReadReplicationAgainstPostgreSQL(t *testing.T) {
	in := pgtest.Start(t, nil)
	ctx := context.Background()
	if _, err := in.Connect(t, "postgres").Exec(ctx, "CREATE DATABASE kingres_system"); err != nil {
		t.Fatal(err)
	}
	c := in.Connect(t, "kingres_system")
	if _, err := sysdb.Migrate(ctx, c); err != nil {
		t.Fatal(err)
	}
	insert := func(subject, state, steps string) {
		t.Helper()
		if _, err := c.Exec(ctx, `INSERT INTO kingres_cp.task (kind, state, subject_type, subject_id, progress, created_by)
			VALUES ('replicate.apply', $1, 'replication', $2, $3::jsonb, 'system')`, state, subject, steps); err != nil {
			t.Fatal(err)
		}
	}
	insert("a", "running", `{"percent":50,"steps":[{"name":"Bring server n2 up to date","state":"done"},{"name":"Bring server n3 up to date","state":"running"}]}`)
	insert("b", "failed", `{"percent":0,"steps":[{"name":"Bring server n3 up to date","state":"failed"}]}`)
	insert("c", "done", `{"percent":100,"steps":[{"name":"Bring server n4 up to date","state":"done"}]}`)
	got, err := monitor.ReadReplication(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Server != "n3" || got[0].Tasks != 2 {
		t.Errorf("servers behind: %+v, want n3 with 2 tasks", got)
	}
}

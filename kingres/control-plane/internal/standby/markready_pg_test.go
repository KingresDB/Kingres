// SPDX-License-Identifier: AGPL-3.0-only

package standby

import (
	"context"
	"errors"
	"testing"
)

// The leader makes a record ready only after the primary's server took a list with the entry Ready (M1 of the phase 5
// review): at the moment that list is taken the record is still not ready, and when it is not taken the record stays as
// it was. That order is what makes the lease fence of the primary armed before a failover can choose the standby.
func TestARecordBecomesReadyOnlyAfterThePrimaryTookTheReadyList(t *testing.T) {
	ctx := context.Background()
	c, l := systemDB(t)
	for _, q := range []string{
		`CREATE SCHEMA IF NOT EXISTS kingres`,
		`CREATE TABLE kingres.node (node_id serial PRIMARY KEY, group_id int, host text, port int, role text, is_active boolean DEFAULT true, server_group text)`,
		`INSERT INTO kingres.node (group_id, host, port, role) VALUES (1, 'p', 7001, 'primary'), (2, 's', 7002, 'primary')`,
	} {
		if _, err := c.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	insertOld(t, c, 11, 1, "s", stateCreating, false, 0)
	l.Self = "self"
	own, err := NewAgent(&Agent{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	l.Agent = own
	primary, err := NewAgent(&Agent{DataDir: t.TempDir()}) // the agent of the server of the primary
	if err != nil {
		t.Fatal(err)
	}
	state := func() string {
		var s string
		if err := c.QueryRow(ctx, `SELECT state FROM kingres_cp.standby WHERE standby_id = 11`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	var atPrimary []string // "ready entry / record state" at each list the primary took
	fail := false
	l.Call = func(_ context.Context, server, _, _ string, in, _ any) error {
		p, ok := in.(PlanRequest)
		if !ok || server != "p" {
			return nil
		}
		if fail {
			return errors.New("the server does not answer")
		}
		for _, e := range p.Standbys {
			if e.ID == 11 {
				atPrimary = append(atPrimary, map[bool]string{true: "ready", false: "notready"}[e.Ready]+"/"+state())
			}
		}
		return primary.Plan(context.Background(), p)
	}
	rec, _, err := l.st.get(ctx, 11)
	if primary.HasStandby(1) {
		t.Fatal("no fence before a ready list")
	}
	if err != nil {
		t.Fatal(err)
	}
	// The primary's server does not take the list: the record stays creating.
	fail = true
	if err := l.markReady(ctx, rec, "p", 7100); err == nil {
		t.Fatal("a list that was not taken must fail the step")
	}
	if s := state(); s != stateCreating {
		t.Fatalf("the record is %q after a list that was not taken", s)
	}
	fail = false
	atPrimary = nil
	if err := l.markReady(ctx, rec, "p", 7100); err != nil {
		t.Fatal(err)
	}
	if len(atPrimary) != 1 || atPrimary[0] != "ready/creating" {
		t.Fatalf("the primary must take the Ready entry while the record is still creating: %v", atPrimary)
	}
	if s := state(); s != stateReady {
		t.Fatalf("the record is %q", s)
	}
	if !primary.HasStandby(1) {
		t.Fatal("the primary's server arms the lease fence once the record is ready")
	}
	// No entry stays forced afterwards: a list built later follows the record.
	l.mu.Lock()
	n := len(l.early)
	l.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d forced entries left", n)
	}
}

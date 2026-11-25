// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package clustertest_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// A member server goes away for good while a database import is still before its commit point. The import waits for the server (its task says so), the server
// cannot be removed while the import is unfinished (migration 0045), and the operator cancels the import: it is undone on the servers that answer, the exclusion is
// released and the removal of the dead server is accepted.
func TestDatabaseImportCancelWhileAMemberServerIsGone(t *testing.T) {
	c := clustertest.FormTLS(t, 3, nil, func(leader *clustertest.Node) {
		body := leader.Expect(202, "POST", "/databases", map[string]any{"name": "gsrc", "sharding": map[string]any{"enabled": false}})
		leader.WaitTaskDone(clustertest.TaskID(body), 4*time.Minute)
	})
	leader := c.Leader()
	var victim, other *clustertest.Node
	for _, n := range c.Nodes {
		switch {
		case n == leader:
		case victim == nil:
			victim = n
		default:
			other = n
		}
	}
	for _, n := range c.Nodes {
		n.Login(clustertest.AdminUser, clustertest.AdminPass)
	}
	leader.Expect(200, "PATCH", "/settings/cluster", map[string]any{"dataTransfer": map[string]any{"enabled": true, "maxConcurrent": 4}})
	q := func(sql string) { leader.Expect(200, "POST", "/query", map[string]any{"database": "gsrc", "sql": sql}) }
	q("create table public.t (id serial primary key, name text)")
	q("insert into public.t (name) select 'row' || g from generate_series(1, 200) g")
	leader.Expect(201, "POST", "/roles", map[string]any{"name": dimImporter, "password": dimImporterPass, "clusterRoles": []string{"kingres_admin"}})
	e := &dimEnv{c: c, adm: map[*clustertest.Node]string{}, imp: map[*clustertest.Node]string{}}
	for _, n := range c.Nodes {
		e.login(t, n)
	}
	archive := e.export(t, "gsrc")
	up := e.upload(t, leader, archive)

	// The server is lost as soon as the import has begun (its kingresd is killed and stays down).
	st, out := e.startImport(leader, up, "gdst")
	if st != 202 {
		t.Fatalf("import: %d %v", st, out)
	}
	id := clustertest.TaskID(out)
	victim.Kill()

	// The import waits for the lost server, it is not paused and not failed.
	until(t, 5*time.Minute, "the import to wait for the lost server", func() bool {
		_, tk := e.asAdmin(leader).Do("GET", fmt.Sprintf("/tasks/%v", id), nil, nil)
		if tk["state"] == "failed" || tk["state"] == "done" {
			t.Fatalf("the import ended while a server is gone: %v", tk)
		}
		d, _ := tk["detail"].(string)
		return tk["state"] == "running" && strings.Contains(d, "Waiting for server "+victim.Host)
	})
	// While it waits, the removal of the server is refused (migration 0045).
	if st, resp := e.asAdmin(leader).Do("DELETE", "/servers/"+victim.Host, nil, nil); st != 409 || resp["code"] != "operation_in_progress" {
		t.Fatalf("the removal of the lost server while the import waits: %d %v", st, resp)
	}
	// The cancel is accepted while the import is not committed.
	_, tk := e.asAdmin(leader).Do("GET", fmt.Sprintf("/tasks/%v", id), nil, nil)
	if tk["cancellable"] != true {
		t.Errorf("the task of the waiting import is not shown as cancellable: %v", tk["cancellable"])
	}
	e.asAdmin(leader).Expect(200, "POST", fmt.Sprintf("/tasks/%v/cancel", id), nil)
	res := e.asAdmin(leader).WaitTask(id, 4*time.Minute)
	if res["state"] != "failed" || taskErrCode(res) != "cancelled" {
		t.Fatalf("after the cancel: %v %v", res["state"], res["error"])
	}
	// Undone on the servers that answer: no final database and no private one.
	for _, n := range []*clustertest.Node{leader, other} {
		until(t, 4*time.Minute, fmt.Sprintf("node %d to hold nothing of the import", n.Index), func() bool {
			got := e.databases(t, n, "gdst")
			return !strings.Contains(got, "gdst") && !strings.Contains(got, "kingres_import_")
		})
	}
	// The exclusion is released: the removal of the lost server is accepted now (it is no longer refused for the import).
	until(t, 2*time.Minute, "the removal of the lost server to be accepted", func() bool {
		st, resp := e.asAdmin(leader).Do("DELETE", "/servers/"+victim.Host, nil, nil)
		if st == 409 && resp["code"] == "operation_in_progress" {
			return false
		}
		if st != 202 {
			t.Fatalf("the removal of the lost server after the cancel: %d %v", st, resp)
		}
		return true
	})
}

func taskErrCode(task map[string]any) string {
	if er, _ := task["error"].(map[string]any); er != nil {
		c, _ := er["code"].(string)
		return c
	}
	return ""
}

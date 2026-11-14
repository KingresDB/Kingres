// SPDX-License-Identifier: PostgreSQL

package main

import (
	"strings"
	"testing"
)

// problemFields returns the field names of a problem's errors.
func problemFields(o map[string]any) []string {
	var out []string
	errs, _ := o["errors"].([]any)
	for _, e := range errs {
		out = append(out, e.(map[string]any)["field"].(string))
	}
	return out
}

func hasString(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func groupNames(out map[string]any) []string {
	var names []string
	for _, g := range out["items"].([]any) {
		names = append(names, g.(map[string]any)["name"].(string))
	}
	return names
}

func TestServerGroupsAndUpdateServer(t *testing.T) {
	e := shared(t)
	srv := "127.0.0.1"
	if got := e.expect(t, 200, "GET", "/server-groups", nil); len(got["items"].([]any)) != 0 {
		t.Fatalf("groups before: %v", got)
	}

	// Creating: 201 with the group, names compared exactly.
	g := e.expect(t, 201, "POST", "/server-groups", map[string]any{"name": "rack-a", "description": "Row 1"})
	valid(t, "createServerGroup", "201", g)
	if g["name"] != "rack-a" || g["description"] != "Row 1" || len(g["serverIds"].([]any)) != 0 {
		t.Errorf("created group %v", g)
	}
	e.expect(t, 201, "POST", "/server-groups", map[string]any{"name": "Rack-A"})
	if st, o, _ := e.call("POST", "/server-groups", map[string]any{"name": "rack-a"}, nil); st != 409 || problemCode(o) != "already_exists" {
		t.Errorf("duplicate: %d %v", st, o)
	}
	for name, body := range map[string]map[string]any{
		"pattern": {"name": "-x"}, "empty": {"name": ""}, "missing": {}, "unknown member": {"name": "x1", "serverIds": []string{"a"}},
		"long description": {"name": "x2", "description": strings.Repeat("d", 201)}, "type": {"name": 4},
	} {
		st, o, _ := e.call("POST", "/server-groups", body, nil)
		if st != 400 || problemCode(o) != "validation_failed" || len(problemFields(o)) == 0 {
			t.Errorf("%s: %d %v", name, st, o)
		}
	}
	list := e.expect(t, 200, "GET", "/server-groups", nil)
	valid(t, "listServerGroups", "200", list)
	if names := groupNames(list); len(names) != 2 || names[0] != "Rack-A" || names[1] != "rack-a" {
		t.Errorf("groups %v", names)
	}

	// Assigning the server; the answer is the whole Server.
	out := e.expect(t, 200, "PATCH", "/servers/"+srv, map[string]any{"serverGroup": "rack-a"})
	valid(t, "updateServer", "200", out)
	if out["serverGroup"] != "rack-a" || out["id"] != srv {
		t.Errorf("updated server %v", out)
	}
	got := e.expect(t, 200, "GET", "/servers/"+srv, nil)
	valid(t, "getServer", "200", got)
	if got["serverGroup"] != "rack-a" {
		t.Errorf("getServer %v", got["serverGroup"])
	}
	list = e.expect(t, 200, "GET", "/server-groups", nil)
	for _, gr := range list["items"].([]any) {
		m := gr.(map[string]any)
		ids := m["serverIds"].([]any)
		if m["name"] == "rack-a" && (len(ids) != 1 || ids[0] != srv) || m["name"] == "Rack-A" && len(ids) != 0 {
			t.Errorf("members of %v: %v", m["name"], ids)
		}
	}
	// An equal value is a no-op that still answers 200.
	e.expect(t, 200, "PATCH", "/servers/"+srv, map[string]any{"serverGroup": "rack-a"})

	// Refusals.
	st, o, _ := e.call("DELETE", "/server-groups/rack-a", nil, nil)
	if st != 409 || problemCode(o) != "server_group_not_empty" || !strings.Contains(o["detail"].(string), srv) {
		t.Errorf("delete non-empty: %d %v", st, o)
	}
	if st, _, _ := e.call("GET", "/server-groups", nil, nil); st != 200 || !hasString(groupNames(e.expect(t, 200, "GET", "/server-groups", nil)), "rack-a") {
		t.Error("the group was deleted by a refused request")
	}
	st, o, _ = e.call("PATCH", "/servers/"+srv, map[string]any{"serverGroup": "nope"}, nil)
	if st != 422 || problemCode(o) != "unprocessable" || !hasString(problemFields(o), "/serverGroup") {
		t.Errorf("unknown group: %d %v", st, o)
	}
	if st, o, _ := e.call("PATCH", "/servers/10.9.9.9", map[string]any{"serverGroup": nil}, nil); st != 404 || problemCode(o) != "not_found" {
		t.Errorf("unknown server: %d %v", st, o)
	}
	for name, body := range map[string]map[string]any{"empty": {}, "status": {"status": "live"}, "unknown": {"color": 1}, "type": {"shouldHaveShards": "no"}} {
		st, o, _ := e.call("PATCH", "/servers/"+srv, body, nil)
		if st != 400 || problemCode(o) != "validation_failed" {
			t.Errorf("%s: %d %v", name, st, o)
		}
	}
	if st, o, _ := e.call("DELETE", "/server-groups/missing", nil, nil); st != 404 || problemCode(o) != "not_found" {
		t.Errorf("delete missing: %d %v", st, o)
	}

	// Roles: read-only may list but nothing else; ops may do all three.
	ro := e.accountWithRole(t, "sg_readonly", "kingres_readonly")
	ops := e.accountWithRole(t, "sg_ops", "kingres_ops")
	for _, c := range []struct{ method, path string }{{"POST", "/server-groups"}, {"DELETE", "/server-groups/rack-a"}, {"PATCH", "/servers/" + srv}} {
		if st, _, _ := e.as(ro).call(c.method, c.path, map[string]any{"name": "zz", "serverGroup": nil}, nil); st != 403 {
			t.Errorf("%s %s as readonly: %d", c.method, c.path, st)
		}
	}
	if st, _, _ := e.as(ro).call("GET", "/server-groups", nil, nil); st != 200 {
		t.Errorf("list as readonly: %d", st)
	}
	if st, _, _ := e.as(ops).call("POST", "/server-groups", map[string]any{"name": "by-ops"}, nil); st != 201 {
		t.Errorf("create as ops: %d", st)
	}
	if st, _, _ := e.as(ops).call("DELETE", "/server-groups/by-ops", nil, nil); st != 204 {
		t.Errorf("delete as ops: %d", st)
	}

	// shouldHaveShards is real: the catalog changes and goes back.
	out = e.expect(t, 200, "PATCH", "/servers/"+srv, map[string]any{"shouldHaveShards": false})
	valid(t, "updateServer", "200", out)
	if out["shouldHaveShards"] != false || out["serverGroup"] != "rack-a" {
		t.Errorf("after shouldHaveShards=false: %v", out)
	}
	if got := e.expect(t, 200, "GET", "/servers/"+srv, nil); got["shouldHaveShards"] != false {
		t.Errorf("catalog does not show shouldHaveShards=false: %v", got["shouldHaveShards"])
	}
	e.expect(t, 200, "PATCH", "/servers/"+srv, map[string]any{"shouldHaveShards": false}) // no-op
	out = e.expect(t, 200, "PATCH", "/servers/"+srv, map[string]any{"shouldHaveShards": true, "serverGroup": nil})
	if out["shouldHaveShards"] != true || out["serverGroup"] != nil {
		t.Errorf("after restoring: %v", out)
	}

	// Unassigned, the group can go; a second delete is 404.
	e.expect(t, 204, "DELETE", "/server-groups/rack-a", nil)
	e.expect(t, 204, "DELETE", "/server-groups/Rack-A", nil)
	if st, o, _ := e.call("DELETE", "/server-groups/rack-a", nil, nil); st != 404 {
		t.Errorf("second delete: %d %v", st, o)
	}
	e.expect(t, 204, "DELETE", "/roles/sg_readonly", nil)
	e.expect(t, 204, "DELETE", "/roles/sg_ops", nil)

	// Every operation is in the audit log, refusals as failures.
	for _, action := range []string{"createServerGroup", "deleteServerGroup", "updateServer"} {
		a := e.expect(t, 200, "GET", "/audit?action="+action+"&limit=200", nil)
		var ok, failed, denied int
		for _, it := range a["items"].([]any) {
			switch it.(map[string]any)["outcome"] {
			case "success":
				ok++
			case "failure":
				failed++
			case "denied":
				denied++
			}
		}
		if ok == 0 || failed == 0 || denied == 0 {
			t.Errorf("audit of %s: %d successes, %d failures, %d denied", action, ok, failed, denied)
		}
	}
}

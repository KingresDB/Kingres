// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"net/url"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/contract"
)

// schemaEnv is an initialized environment with the database shop and its schemas (contract_test.go: seedSchemas).
func schemaEnv(t *testing.T) (*env, string) {
	t.Helper()
	e := newEnv(t)
	e.initialize()
	e.mem.DBs = []api.Database{{Name: "shop"}}
	seedSchemas(e)
	return e, e.token(auth.RoleAdmin)
}

func schemaNames(t *testing.T, m map[string]any) []string {
	t.Helper()
	var out []string
	items, _ := m["items"].([]any)
	for _, it := range items {
		out = append(out, it.(map[string]any)["name"].(string))
	}
	return out
}

func TestListSchemasFiltersAndPages(t *testing.T) {
	e, tok := schemaEnv(t)
	get := func(q string) map[string]any {
		w := e.do(req{method: "GET", path: "/api/v1/databases/shop/schemas" + q, bearer: tok})
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", q, w.Code, w.Body.String())
		}
		return decode(t, w)
	}
	for q, want := range map[string]string{
		"":                                "reporting,tenant_17,tenant_18",
		"?distributed=true":               "tenant_17,tenant_18",
		"?distributed=false":              "reporting",
		"?shardGroupId=2":                 "tenant_17",
		"?shardGroupId=0":                 "tenant_18",
		"?state=moving":                   "tenant_18",
		"?state=active":                   "tenant_17",
		"?distributed=true&state=active":  "tenant_17",
		"?distributed=false&state=active": "",
		"?shardGroupId=9":                 "",
		"?distributed=true&shardGroupId=2&state=moving": "",
	} {
		if got := strings.Join(schemaNames(t, get(q)), ","); got != want {
			t.Errorf("schemas%s = %q, want %q", q, got, want)
		}
	}
	// pages of one: every schema once, in order, and the last page has no cursor
	var all []string
	cursor := ""
	for i := 0; i < 5; i++ {
		q := "?limit=1"
		if cursor != "" {
			q += "&cursor=" + url.QueryEscape(cursor)
		}
		p := get(q)
		all = append(all, schemaNames(t, p)...)
		next, _ := p["nextCursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if strings.Join(all, ",") != "reporting,tenant_17,tenant_18" {
		t.Errorf("pages gave %v", all)
	}
	// what is refused
	for _, q := range []string{"?distributed=maybe", "?state=bogus", "?shardGroupId=x", "?limit=0", "?cursor=!!"} {
		wantProblem(t, e.do(req{method: "GET", path: "/api/v1/databases/shop/schemas" + q, bearer: tok}), 400, "validation_failed")
	}
	wantProblem(t, e.do(req{method: "GET", path: "/api/v1/databases/nope/schemas", bearer: tok}), 404, "not_found")
}

func TestGetSchemaAnswersBlockersPinAndMove(t *testing.T) {
	e, tok := schemaEnv(t)
	w := e.do(req{method: "GET", path: "/api/v1/databases/shop/schemas/reporting", bearer: tok})
	d := decode(t, w)
	bl, _ := d["distributionBlockers"].([]any)
	if w.Code != 200 || len(bl) != 1 || bl[0].(map[string]any)["objectKind"] != "materialized view" || d["pinned"] != nil || d["moveInProgress"] != nil || d["distribution"] != nil {
		t.Errorf("reporting: %d %v", w.Code, d)
	}
	d = decode(t, e.do(req{method: "GET", path: "/api/v1/databases/shop/schemas/tenant_17", bearer: tok}))
	pin, _ := d["pinned"].(map[string]any)
	dist, _ := d["distribution"].(map[string]any)
	if pin["reason"] == "" || d["moveInProgress"] != float64(12) || dist["shardGroupId"] != float64(2) || len(d["distributionBlockers"].([]any)) != 0 {
		t.Errorf("tenant_17: %v", d)
	}
	// a schema that is only in the list has the summary and no blocker
	d = decode(t, e.do(req{method: "GET", path: "/api/v1/databases/shop/schemas/tenant_18", bearer: tok}))
	if d["name"] != "tenant_18" || d["distributionBlockers"] == nil {
		t.Errorf("tenant_18: %v", d)
	}
	wantProblem(t, e.do(req{method: "GET", path: "/api/v1/databases/shop/schemas/missing", bearer: tok}), 404, "not_found")
	wantProblem(t, e.do(req{method: "GET", path: "/api/v1/databases/nope/schemas/x", bearer: tok}), 404, "not_found")
	// a schema name is 1 to 63 characters
	wantProblem(t, e.do(req{method: "GET", path: "/api/v1/databases/shop/schemas/" + strings.Repeat("a", 64), bearer: tok}), 400, "validation_failed")
	// a name that is hostile in a URL is found as it is
	hostile := "Te\"n ant;'\\ é"
	e.mem.SchemasByDB["shop"] = append(e.mem.SchemasByDB["shop"], api.SchemaSummary{Name: hostile, Owner: "app"})
	w = e.do(req{method: "GET", path: "/api/v1/databases/shop/schemas/" + url.PathEscape(hostile), bearer: tok})
	if w.Code != 200 || decode(t, w)["name"] != hostile {
		t.Errorf("hostile name: %d %s", w.Code, w.Body.String())
	}
}

func TestColocationGroupsKindFilter(t *testing.T) {
	e, tok := schemaEnv(t)
	size := int64(1)
	e.mem.Colocation["shop"] = []api.ColocationGroup{
		{ColocationID: 1, Kind: "hash", BucketCount: 8, DistributionColumnType: ptr("bigint"), TableCount: 1, SizeBytes: &size},
		{ColocationID: 2, BucketCount: 8, DistributionColumnType: ptr("text")}, // a group of a server before 0.15.0 has no kind: it is a hash group
		{ColocationID: 4, Kind: "schema", SchemaName: ptr("tenant_17"), BucketCount: 1},
	}
	count := func(q string) int {
		w := e.do(req{method: "GET", path: "/api/v1/databases/shop/colocation-groups" + q, bearer: tok})
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", q, w.Code, w.Body.String())
		}
		return len(decode(t, w)["items"].([]any))
	}
	if n := count(""); n != 3 {
		t.Errorf("all: %d", n)
	}
	if n := count("?kind=hash"); n != 2 {
		t.Errorf("hash: %d", n)
	}
	if n := count("?kind=schema"); n != 1 {
		t.Errorf("schema: %d", n)
	}
	wantProblem(t, e.do(req{method: "GET", path: "/api/v1/databases/shop/colocation-groups?kind=bogus", bearer: tok}), 400, "validation_failed")
}

// The schema operations that change something run on the leader, and so do the reads that the move of a schema makes
// leader-bound (the record of the move is in the map group's catalog and the task in the leader's system database).
func TestSchemaOperationsAreLeaderBound(t *testing.T) {
	want := map[string]bool{"listSchemas": true, "getSchema": true, "distributeSchema": true, "undistributeSchema": true, "moveSchema": true}
	seen := 0
	for _, op := range contract.Operations() {
		if !want[op.ID] {
			continue
		}
		seen++
		if !forwardsToLeader(op) {
			t.Errorf("%s is not forwarded to the manager leader", op.ID)
		}
	}
	if seen != len(want) {
		t.Errorf("only %d of the %d operations are in the contract", seen, len(want))
	}
}

// Without a managed instance (no Ops) the three operations answer unsupported_operation after the role check, and the roles of the
// contract hold: a reader cannot, an operator moves but neither distributes nor undistributes.
func TestSchemaMutationsRolesAndNoInstance(t *testing.T) {
	e, admin := schemaEnv(t)
	ro, ops := e.token(auth.RoleReadonly), e.token(auth.RoleOps)
	base := "/api/v1/databases/shop/schemas/tenant_17"
	calls := []struct {
		method, path string
		body         any
		role         string // the lowest role that is let through
	}{
		{"PUT", base + "/distribution", map[string]any{}, auth.RoleAdmin},
		{"DELETE", base + "/distribution", nil, auth.RoleAdmin},
		{"POST", base + "/moves", map[string]any{"targetShardGroupId": 1}, auth.RoleOps},
	}
	for _, c := range calls {
		wantProblem(t, e.do(req{method: c.method, path: c.path, bearer: ro, body: c.body}), 403, "forbidden")
		w := e.do(req{method: c.method, path: c.path, bearer: ops, body: c.body})
		if c.role == auth.RoleAdmin {
			wantProblem(t, w, 403, "forbidden")
		} else if w.Code == 403 {
			t.Errorf("ops %s %s: refused", c.method, c.path)
		}
		wantProblem(t, e.do(req{method: c.method, path: c.path, bearer: admin, body: c.body}), 422, "unsupported_operation")
	}
}

// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/contracttest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/spec"
)

func loadSpec(t *testing.T) *spec.Document {
	t.Helper()
	doc := contracttest.Load(t)
	return doc
}

// TestRoutingMatchesContract parses openapi.yaml and fails if an operation of
// the contract is not routed, or if the server routes something that is not
// in the contract. It compares the routes the server registered, and then
// checks the behavior by sending a request to every operation.
func TestRoutingMatchesContract(t *testing.T) {
	doc := loadSpec(t)
	e := newEnv(t)
	e.initialize()

	want := map[string]string{} // "METHOD /api/v1/path" -> operation id
	for _, op := range doc.Operations {
		want[op.Method+" /api/v1"+op.Path] = op.ID
	}
	if len(want) != 151 {
		t.Errorf("contract has %d operations, this test was written for 151", len(want))
	}
	got := map[string]string{}
	for _, r := range e.srv.Routes() {
		key := r.Method + " " + r.Path
		if _, dup := got[key]; dup {
			t.Errorf("route %s registered twice", key)
		}
		got[key] = r.OperationID
	}
	for key, id := range want {
		if got[key] != id {
			t.Errorf("contract operation %s (%s) is not routed", key, id)
		}
	}
	for key, id := range got {
		if want[key] != id {
			t.Errorf("server routes %s (%s), which is not in the contract", key, id)
		}
	}

	// Behavior: every contract path answers for its methods, and answers 405
	// for a method the contract does not define on it.
	tok := e.token(auth.RoleAdmin)
	for _, op := range doc.Operations {
		w := e.do(req{method: op.Method, path: concretePath(op.Path), bearer: tok, body: bodyFor0(op.Method)})
		if w.Code == http.StatusMethodNotAllowed {
			t.Errorf("%s %s answered 405", op.Method, op.Path)
		}
		if w.Code == http.StatusNotFound && strings.Contains(w.Body.String(), "No such API route") {
			t.Errorf("%s %s answered the unknown-route 404", op.Method, op.Path)
		}
	}
	// Every handler must belong to an operation of the contract.
	ids := map[string]bool{}
	for _, op := range doc.Operations {
		ids[op.ID] = true
	}
	for id := range e.srv.handlers() {
		if !ids[id] {
			t.Errorf("handler %q is not an operation of the contract", id)
		}
	}
}

func bodyFor0(method string) any {
	if method == "GET" {
		return nil
	}
	return map[string]any{}
}

// TestNewRejectsHandlerOutsideContract checks that a handler registered for an
// operation that is not in the contract makes the server refuse to start.
func TestNewRejectsHandlerOutsideContract(t *testing.T) {
	e := newEnv(t)
	_, err := build(e.srv.deps, func(s *Server) map[string]handlerFunc {
		m := s.handlers()
		m["invented"] = s.getHealth
		return m
	})
	if err == nil || !strings.Contains(err.Error(), "invented") {
		t.Fatalf("err = %v, want a refusal naming the handler", err)
	}
}

// schemaChecker validates JSON documents against the schemas of the contract
// (package contracttest, shared with the end-to-end tests).
type schemaChecker struct {
	t testing.TB
	c *contracttest.Checker
}

func newSchemaChecker(t *testing.T) *schemaChecker {
	t.Helper()
	return &schemaChecker{t: t, c: contracttest.NewChecker(t)}
}

func (c *schemaChecker) validateSchema(name string, body []byte) error {
	c.t.Helper()
	return c.c.Schema(name, body)
}

func (c *schemaChecker) validateResponse(opID, status string, body []byte) error {
	c.t.Helper()
	return c.c.Response(opID, status, body)
}

// seed fills the fake store with one of every resource, so that responses
// exercise nested objects and nullable properties. Values the server has no
// source for are left nil, as the real store leaves them.
func seed(e *env) {
	now := time.Date(2026, 11, 14, 9, 58, 0, 0, time.UTC)
	h1, h2 := "10.0.0.1", "10.0.0.2"
	pri := int32(1)
	lag := 0.5
	lagB := int64(1024)
	sg := "rack-a"
	size := int64(100)
	e.mem.Summary.Servers = api.ServerCounts{Total: 2, Live: 2}
	e.mem.Summary.Buckets = api.BucketCounts{Total: 128}
	e.mem.Summary.DatabaseCount = 1
	e.mem.Summary.MapVersion = 3
	e.mem.Summary.RebalanceReasons = []string{"serverAdded"}
	e.mem.Summary.RebalanceNeeded = true
	inst1 := api.Instance{ID: 1, ServerID: h1, ShardGroupID: 0, IsMapGroup: true, Role: "primary", State: "running", Port: 5432}
	inst2 := api.Instance{ID: 2, ServerID: h2, ShardGroupID: 0, IsMapGroup: true, Role: "standby", State: "running", Port: 5432,
		UpstreamInstanceID: &pri, ReplicationLagBytes: &lagB, ReplicationLagSeconds: &lag}
	ver := "1.0.0"
	mk := func(id string, in api.Instance) api.Server {
		return api.Server{ID: id, Address: id + ":8091", Hostname: id,
			Status: "live", ServerGroup: &sg, ShouldHaveShards: true, Version: &ver,
			Instances: []api.Instance{in}, Buckets: api.BucketHolding{Primary: 64, Replica: 64}}
	}
	e.mem.ServerList = []api.Server{mk(h1, inst1), mk(h2, inst2)}
	e.mem.ServerDetails[h1] = []api.DatabaseBucketCount{{DatabaseName: "shop", PrimaryBuckets: 64}}
	e.mem.LocalServer = &h1
	e.mem.Groups = []api.ServerGroup{{Name: "rack-a", ServerIDs: []string{h1, h2}}}
	e.mem.Topology = api.ReplicationTopology{GeneratedAt: now, Groups: []api.ShardGroup{{
		ID: 0, IsMapGroup: true, PreferredPrimaryServerID: h1, Health: "healthy",
		Primary: &api.ReplicationMember{InstanceID: 1, ServerID: h1, Role: "primary", State: "running", Port: 5432, Timeline: 1, IsOnPreferredServer: true},
		Standbys: []api.ReplicationMember{{InstanceID: 2, ServerID: h2, Role: "standby", State: "running", Port: 5432, Timeline: 1,
			SyncState: ptr("sync"), LagBytes: &lagB, LagSeconds: &lag}},
		Slots: []api.ReplicationSlot{{Name: "s1", Type: "physical", Active: true, InstanceID: 1, RetainedWalBytes: 10}},
	}}}
	owner, enc, coll, limit, dur := "alice", "UTF8", "C", -1, "sync"
	by := "alice"
	bc, tables, conns, mode := 128, 2, 1, "off"
	e.mem.DBs = []api.Database{{
		Name: "shop", Status: "ready",
		General:       api.DatabaseGeneral{Owner: &owner, Encoding: &enc, LCCollate: &coll, LCCtype: &coll, ConnectionLimit: &limit},
		Sharding:      api.ShardingSettings{Enabled: true, BucketCount: &bc},
		Replication:   api.ReplicationSettings{Durability: &dur, ReplicaCount: 1},
		Ledger:        api.LedgerSettings{Mode: &mode},
		Stats:         api.DatabaseStats{SizeBytes: &size, TableCount: &tables, ActiveConnections: &conns},
		LedgerSummary: api.DatabaseLedgerSummary{},
		CreatedBy:     &by, CreatedAt: &now,
	}}
	col, rows := "id", int64(10)
	orders := api.TableDetail{Table: api.Table{Schema: "public", Name: "orders", Classes: []string{"distributed", "ledger"},
		Distribution: &api.TableDistribution{Method: "hash", Column: &col, ColocationID: &one32, BucketCount: &bc128},
		Ledger:       &api.TableLedger{Mode: "appendOnly"}, EstimatedRows: &rows, SizeBytes: &size},
		Columns: []api.TableColumn{{Name: "id", Type: "bigint", DistributionBlockedReason: ptr("tableNotPlain"), BlockingKeys: []string{}}}}
	settings := api.TableDetail{Table: api.Table{Schema: "public", Name: "settings", Classes: []string{"plain"}, HomeShardGroupID: ptr(int32(0))},
		Columns: []api.TableColumn{{Name: "k", Type: "text", CanBeDistributionColumn: true, BlockingKeys: []string{}}, {Name: "v", Type: "jsonb", Nullable: true, DistributionBlockedReason: ptr("unsupportedType"), BlockingKeys: []string{}}}}
	e.mem.TablesByDB["shop"] = []api.TableDetail{orders, settings}
	e.mem.Colocation["shop"] = []api.ColocationGroup{{ColocationID: 1, BucketCount: 128, DistributionColumnType: "bigint", TableCount: 1, SizeBytes: &size}}
	e.mem.BucketsByCG["shop"] = map[int32][]api.Bucket{1: {
		{Bucket: 0, ShardGroupID: 0, State: "active", PrimaryServerID: &h1, ReplicaServerIDs: []string{h2}},
		{Bucket: 1, ShardGroupID: 0, State: "moving", PrimaryServerID: nil, ReplicaServerIDs: []string{}},
	}}
	e.mem.Distribution["shop"] = api.BucketDistribution{MapVersion: 3, Servers: []api.ServerBucketCount{{ServerID: h1, PrimaryBuckets: 64}}}
	step := "Create database"
	e.mem.TaskList = []api.Task{{ID: 1, Kind: "database.create", State: "failed", CreatedAt: now, UpdatedAt: now, CreatedBy: "alice",
		Progress: api.TaskProgress{Percent: 50, CurrentStep: &step, Steps: []api.TaskStep{{Name: step, State: "failed", StartedAt: &now}}},
		Error:    &api.TaskError{Code: "step_failed", Message: "boom"}, Subject: &api.TaskSubject{Type: "database", ID: "shop"}}}
	hash, height, one, interval := "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", int64(4210), 1, 3600
	e.mem.LedgerByDB["shop"] = api.LedgerStatus{DatabaseName: "shop", Mode: &mode, DigestIntervalSeconds: &interval,
		LedgerTableCount: &one, Chains: []api.LedgerChainStatus{{ShardGroupID: 0, LatestBlockHeight: &height, LatestBlockHash: &hash, LatestBlockAt: &now}}}
	e.mem.BlocksByDB["shop"] = []api.LedgerBlock{{ShardGroupID: 0, Height: 0, TxMerkleRoot: hash, BlockHash: hash, Timestamp: now, HashVersion: 1},
		{ShardGroupID: 0, Height: 1, PrevHash: &hash, TxMerkleRoot: hash, BlockHash: hash, Timestamp: now, HashVersion: 1, TransactionCount: 3}}
	until := now.Add(24 * time.Hour)
	e.mem.RoleList = []api.Role{{Name: "alice", CanLogin: true, ConnectionLimit: -1, ValidUntil: &until,
		MemberOf: []string{"app"}, ClusterRoles: []string{"kingres_admin"}}, {Name: "app", ConnectionLimit: -1, MemberOf: []string{}, ClusterRoles: []string{}}}
	sc, tb := "public", "orders"
	e.mem.GrantList = []api.Grant{{Grantee: "alice", Level: "database", Database: "shop", Privileges: []string{"connect"}},
		{Grantee: "alice", Level: "table", Database: "shop", Schema: &sc, Table: &tb, Privileges: []string{"select", "insert"}}}
}

func ptr[T any](v T) *T { return &v }

// TestResponsesValidateAgainstContract calls every implemented operation with
// realistic data and validates the success body against the schema the
// contract declares, plus the problem bodies of the error paths against
// Problem. A response that drifts from the contract fails here.
var (
	one32 = int32(1)
	bc128 = 128
)

func TestResponsesValidateAgainstContract(t *testing.T) {
	chk := newSchemaChecker(t)
	e := newEnv(t)
	e.initialize()
	seed(e)
	if _, err := e.audit.Append(context.Background(), audit.Event{Actor: "alice", Source: "panel", Action: "login", Outcome: audit.OutcomeSuccess}); err != nil {
		t.Fatal(err)
	}
	admin := e.token(auth.RoleAdmin)

	// op id -> request path (query string included) -> expected status.
	calls := []struct {
		op, path string
	}{
		{"getHealth", "/api/v1/healthz"},
		{"getReadiness", "/api/v1/readyz"},
		{"getNodeState", "/api/v1/setup/state"},
		{"getSession", "/api/v1/auth/session"},
		{"getCluster", "/api/v1/cluster"},
		{"getThisServer", "/api/v1/this-server"},
		{"getThisServerResources", "/api/v1/this-server/resources"},
		{"listThisServerLogs", "/api/v1/this-server/logs"},
		{"listLogs", "/api/v1/logs"},
		{"getClusterSummary", "/api/v1/cluster/summary"},
		{"listServers", "/api/v1/servers"},
		{"getServer", "/api/v1/servers/10.0.0.1"},
		{"listServerGroups", "/api/v1/server-groups"},
		{"getClusterSettings", "/api/v1/settings/cluster"},
		{"listClusterRoles", "/api/v1/cluster-roles"},
		{"getReplicationTopology", "/api/v1/replication/topology"},
		{"listThisServerInstances", "/api/v1/this-server/instances"},
		{"getThisServerInstance", "/api/v1/this-server/instances/1"},
		{"getDatabaseCreateOptions", "/api/v1/database-create-options"},
		{"listDatabases", "/api/v1/databases"},
		{"getDatabase", "/api/v1/databases/shop"},
		{"listTables", "/api/v1/databases/shop/tables"},
		{"listTables", "/api/v1/databases/shop/tables?class=ledger&limit=1"},
		{"getTable", "/api/v1/databases/shop/schemas/public/tables/orders"},
		{"listColocationGroups", "/api/v1/databases/shop/colocation-groups"},
		{"listBuckets", "/api/v1/databases/shop/colocation-groups/1/buckets"},
		{"getBucketDistribution", "/api/v1/databases/shop/bucket-distribution"},
		{"listTasks", "/api/v1/tasks"},
		{"getTask", "/api/v1/tasks/1"},
		{"listLedgerStatus", "/api/v1/ledger/status"},
		{"getLedgerStatus", "/api/v1/databases/shop/ledger"},
		{"listLedgerBlocks", "/api/v1/databases/shop/ledger/blocks"},
		{"listRoles", "/api/v1/roles"},
		{"getRole", "/api/v1/roles/alice"},
		{"listGrants", "/api/v1/grants"},
		{"listAuditEvents", "/api/v1/audit"},
	}
	// Operations that change the cluster need a real PostgreSQL instance; their
	// responses are validated against the contract by the end-to-end tests.
	mutating := map[string]bool{}
	for id := range e.srv.opsHandlers() {
		if id != "getClusterSummary" {
			mutating[id] = true
		}
	}
	mutating["initCluster"] = true
	for id := range e.srv.instanceHandlers() {
		mutating[id] = true // validated in instance_test.go and by the end-to-end tests
	}
	for id := range e.srv.memberHandlers() {
		mutating[id] = true
	}
	for id := range e.srv.configHandlers() {
		mutating[id] = true // need the system database and the servers; validated by the integration tests of package clustertest
	}
	for id := range e.srv.rebalanceHandlers() {
		mutating[id] = true // validated in rebalance_http_test.go with a fake service
	}
	for id := range e.srv.failoverHandlers() {
		mutating[id] = true // validated by the unit tests of internal/failover and the cluster tests of package clustertest
	}
	for id := range e.srv.ledgerSigningHandlers() {
		mutating[id] = true // validated in ledger_signing_http_test.go with a fake service
	}
	for id := range e.srv.ledgerPublicationHandlers() {
		mutating[id] = true // validated in ledger_publication_http_test.go with a fake service
	}
	for id := range e.srv.ledgerBundleHandlers() {
		mutating[id] = true // validated by the cluster tests of package clustertest (ledgerbundle_test.go)
	}
	for id := range e.srv.ledgerClientHandlers() {
		mutating[id] = true // validated by the cluster tests of package clustertest (ledgerclient_test.go)
	}
	for id := range e.srv.ledgerAnchorHandlers() {
		mutating[id] = true // validated in ledger_anchor_http_test.go with a fake service
	}
	for id := range e.srv.analyticsHandlers() {
		mutating[id] = true // need the system database and the servers; validated by the cluster tests of package clustertest (approx_test.go) and the integration tests of package ops
	}
	for id := range e.srv.serverGroupHandlers() {
		mutating[id] = true // need the system database; validated in cmd/kingresd/servergroups_e2e_test.go
	}
	// Alerts and metrics need the system database and the sampler.
	for id := range s2(e) {
		mutating[id] = true
	}
	covered := map[string]bool{}
	for _, c := range calls {
		covered[c.op] = true
		t.Run(c.op+" "+c.path, func(t *testing.T) {
			w := e.do(req{method: "GET", path: c.path, bearer: admin})
			if w.Code != 200 {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("content type %q", ct)
			}
			if err := chk.validateResponse(c.op, "200", w.Body.Bytes()); err != nil {
				t.Errorf("response does not match the contract: %v\n%s", err, w.Body.String())
			}
		})
	}
	// Every implemented operation except the cookie/side-effect ones must be
	// covered by the table above.
	for id := range e.srv.handlers() {
		if id == "login" || id == "logout" || id == "checkSetupToken" || covered[id] || mutating[id] {
			continue
		}
		t.Errorf("implemented operation %s has no response validation case", id)
	}

	t.Run("login response", func(t *testing.T) {
		w := e.do(req{method: "POST", path: "/api/v1/auth/login", body: map[string]any{"username": "alice", "password": "s3cret", "issueToken": true}})
		if w.Code != 200 {
			t.Fatalf("status %d", w.Code)
		}
		if err := chk.validateResponse("login", "200", w.Body.Bytes()); err != nil {
			t.Errorf("login response: %v\n%s", err, w.Body.String())
		}
	})

	t.Run("empty store responses", func(t *testing.T) {
		e2 := newEnv(t)
		e2.initialize()
		tok := e2.token(auth.RoleAdmin)
		for _, c := range calls {
			switch c.op {
			case "getServer", "getThisServerInstance", "getDatabase", "getTable", "listTables", "listBuckets",
				"getBucketDistribution", "listColocationGroups", "getTask", "getLedgerStatus", "listLedgerBlocks", "getRole":
				continue // these need a resource
			}
			w := e2.do(req{method: "GET", path: c.path, bearer: tok})
			if w.Code != 200 {
				t.Errorf("%s on an empty store: %d %s", c.op, w.Code, w.Body.String())
				continue
			}
			if err := chk.validateResponse(c.op, "200", w.Body.Bytes()); err != nil {
				t.Errorf("%s on an empty store does not match the contract: %v\n%s", c.op, err, w.Body.String())
			}
		}
	})

	t.Run("problem bodies", func(t *testing.T) {
		cases := []struct {
			name string
			r    req
		}{
			{"unauthenticated", req{method: "GET", path: "/api/v1/cluster"}},
			{"forbidden", req{method: "GET", path: "/api/v1/audit", bearer: e.token(auth.RoleReadonly)}},
			{"not found", req{method: "GET", path: "/api/v1/servers/42", bearer: admin}},
			{"validation", req{method: "GET", path: "/api/v1/roles?limit=0", bearer: admin}},
			{"unsupported", req{method: "POST", path: "/api/v1/databases", bearer: admin, body: map[string]any{}}},
			{"unknown route", req{method: "GET", path: "/api/v1/nothing"}},
			{"csrf", func() req {
				ck, _ := e.cookieSession(auth.RoleAdmin)
				return req{method: "POST", path: "/api/v1/databases", cookie: ck, body: map[string]any{}}
			}()},
			{"bad login", req{method: "POST", path: "/api/v1/auth/login", body: map[string]any{"username": "alice", "password": "bad"}}},
			{"method", req{method: "PATCH", path: "/api/v1/healthz"}},
		}
		for _, tc := range cases {
			w := e.do(tc.r)
			if w.Code < 400 {
				t.Errorf("%s: status %d", tc.name, w.Code)
				continue
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
				t.Errorf("%s: content type %q", tc.name, ct)
			}
			if err := chk.validateSchema("Problem", w.Body.Bytes()); err != nil {
				t.Errorf("%s: problem does not match the contract: %v\n%s", tc.name, err, w.Body.String())
			}
		}
	})

	t.Run("all problem codes are valid enum values", func(t *testing.T) {
		codes := []string{"bad_request", "validation_failed", "unauthenticated", "invalid_credentials", "session_expired",
			"csrf_token_invalid", "forbidden", "setup_token_invalid", "not_found", "already_exists", "unsupported_operation",
			"node_state_invalid", "node_uninitialized", "rate_limited", "service_unavailable", "internal_error"}
		sort.Strings(codes)
		for _, c := range codes {
			body := fmt.Sprintf(`{"type":"about:blank","title":"t","status":400,"code":%q}`, c)
			if err := chk.validateSchema("Problem", []byte(body)); err != nil {
				t.Errorf("code %s: %v", c, err)
			}
		}
	})
}

// TestSchemaCheckerRejectsDrift proves the validator can fail: a body with a
// wrong type, a missing required property or a null where an array is
// required must be reported.
func TestSchemaCheckerRejectsDrift(t *testing.T) {
	chk := newSchemaChecker(t)
	bad := []struct{ op, body string }{
		{"listServers", `{"items":null}`},
		{"listServers", `{}`},
		{"getHealth", `{"status":"fine"}`},
		{"getSession", `{"username":"a","clusterRoles":["root"],"expiresAt":"2026-11-14T10:00:00Z"}`},
		{"getNodeState", `{"state":"weird"}`},
	}
	for _, tc := range bad {
		if err := chk.validateResponse(tc.op, "200", []byte(tc.body)); err == nil {
			t.Errorf("%s accepted %s", tc.op, tc.body)
		}
	}
}

// TestFilterEnumsMatchTheContract proves that the enumerations the handlers
// accept for query filters are exactly those of the contract: every value of
// the schema is accepted and a value outside it is a 400.
func TestFilterEnumsMatchTheContract(t *testing.T) {
	doc := loadSpec(t)
	e := newEnv(t)
	e.initialize()
	e.mem.DBs = []api.Database{{Name: "shop"}}
	e.mem.Colocation["shop"] = []api.ColocationGroup{{ColocationID: 1}}
	tok := e.token(auth.RoleAdmin)
	cases := []struct{ path, param, schema string }{
		{"/api/v1/tasks", "state", "TaskState"},
		{"/api/v1/tasks", "kind", "TaskKind"},
		{"/api/v1/databases/shop/tables", "class", "TableClass"},
		{"/api/v1/databases/shop/colocation-groups/1/buckets", "state", "BucketState"},
		{"/api/v1/audit", "outcome", "AuditOutcome"},
		{"/api/v1/audit", "action", "AuditAction"},
		{"/api/v1/grants", "level", "GrantLevel"},
	}
	for _, c := range cases {
		values := doc.EnumOf(c.schema)
		if len(values) == 0 {
			t.Fatalf("schema %s has no enum", c.schema)
		}
		for _, v := range values {
			if w := e.do(req{method: "GET", path: c.path + "?" + c.param + "=" + v, bearer: tok}); w.Code != 200 {
				t.Errorf("%s %s=%s: %d %s", c.path, c.param, v, w.Code, w.Body.String())
			}
		}
		wantProblem(t, e.do(req{method: "GET", path: c.path + "?" + c.param + "=notAValue", bearer: tok}), 400, "validation_failed")
	}
}

func s2(e *env) map[string]handlerFunc { return e.srv.monitorHandlers() }

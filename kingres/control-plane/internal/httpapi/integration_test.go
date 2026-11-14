// SPDX-License-Identifier: PostgreSQL

package httpapi

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store/pgstore"
)

// Full-stack tests: the real PostgreSQL store and the real PostgreSQL
// authenticator against the fixture instance (scripts/). They run only when
// KINGRES_TEST_DSN is set.

func newIntegrationEnv(t *testing.T) *env {
	t.Helper()
	dsn := os.Getenv("KINGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("KINGRES_TEST_DSN is not set; skipping the PostgreSQL integration test")
	}
	pg, err := pgstore.New(pgstore.Options{
		DSN: dsn, Logger: slog.New(slog.DiscardHandler), PanelPort: "8091", Version: "test",
		Started: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pg.Close)
	authn, err := auth.NewPostgres(dsn, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, func(d *Deps) {
		d.Store = pg
		d.Authenticator = authn
		d.Now = time.Now
		d.RequestTimeout = 30 * time.Second
		d.LoginRateLimit = 100
	})
	e.initialize()
	return e
}

// login signs in through the API and returns the cookie value and CSRF token.
func (e *env) login(user, pass string) (cookie, csrf string, status int) {
	e.t.Helper()
	w := e.do(req{method: "POST", path: "/api/v1/auth/login", body: map[string]any{"username": user, "password": pass}})
	if w.Code != 200 {
		return "", "", w.Code
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == CookieName {
			cookie = c.Value
		}
	}
	return cookie, decode(e.t, w)["csrfToken"].(string), 200
}

func TestIntegrationLoginAgainstPostgreSQL(t *testing.T) {
	e := newIntegrationEnv(t)
	tests := []struct {
		user, pass string
		status     int
		code       string
		roles      []string
	}{
		{"cp_admin", "cp_admin_pw", 200, "", []string{"kingres_admin"}},
		{"cp_ro", "cp_ro_pw", 200, "", []string{"kingres_readonly"}},
		{"cp_member", "cp_member_pw", 200, "", []string{"kingres_ops"}},
		{"cp_none", "cp_none_pw", 403, "forbidden", nil},
		{"cp_super", "cp_super_pw", 403, "forbidden", nil},
		{"cp_admin", "nope", 401, "invalid_credentials", nil},
		{"cp_disabled", "x", 401, "invalid_credentials", nil},
		{"ghost", "x", 401, "invalid_credentials", nil},
	}
	for _, tc := range tests {
		t.Run(tc.user+"/"+tc.pass, func(t *testing.T) {
			w := e.do(req{method: "POST", path: "/api/v1/auth/login", body: map[string]any{"username": tc.user, "password": tc.pass}})
			if tc.status != 200 {
				wantProblem(t, w, tc.status, tc.code)
				return
			}
			m := decode(t, w)
			got := m["clusterRoles"].([]any)
			if len(got) != len(tc.roles) || got[0] != tc.roles[0] {
				t.Errorf("clusterRoles = %v, want %v", got, tc.roles)
			}
		})
	}
	// Neither the passwords nor any session secret is in the logs.
	for _, secret := range []string{"cp_admin_pw", "cp_super_pw", "cp_none_pw"} {
		if strings.Contains(e.logs.String(), secret) {
			t.Errorf("password %q leaked into the logs", secret)
		}
	}
}

func TestIntegrationRolesLimitAccess(t *testing.T) {
	e := newIntegrationEnv(t)
	roCookie, roCSRF, _ := e.login("cp_ro", "cp_ro_pw")
	opsCookie, _, _ := e.login("cp_member", "cp_member_pw")
	adminCookie, adminCSRF, _ := e.login("cp_admin", "cp_admin_pw")

	if w := e.do(req{method: "GET", path: "/api/v1/cluster", cookie: roCookie}); w.Code != 200 {
		t.Errorf("readonly /cluster: %d %s", w.Code, w.Body.String())
	}
	wantProblem(t, e.do(req{method: "GET", path: "/api/v1/roles", cookie: roCookie}), 403, "forbidden")
	wantProblem(t, e.do(req{method: "GET", path: "/api/v1/audit", cookie: opsCookie}), 403, "forbidden")
	wantProblem(t, e.do(req{method: "POST", path: "/api/v1/servers", cookie: roCookie, body: map[string]any{},
		header: map[string]string{"X-CSRF-Token": roCSRF}}), 403, "forbidden")
	// ops may reach an ops operation (not implemented yet) but not an admin one.
	wantProblem(t, e.do(req{method: "POST", path: "/api/v1/server-groups", cookie: opsCookie, body: map[string]any{},
		header: map[string]string{"X-CSRF-Token": csrfOf(t, e, opsCookie)}}), 422, "unsupported_operation")
	if w := e.do(req{method: "GET", path: "/api/v1/roles", cookie: adminCookie}); w.Code != 200 {
		t.Errorf("admin /roles: %d", w.Code)
	}
	// The audit log recorded the denied attempt and the permitted one.
	w := e.do(req{method: "GET", path: "/api/v1/audit?outcome=denied", cookie: adminCookie})
	if w.Code != 200 || len(decode(t, w)["items"].([]any)) != 1 {
		t.Errorf("audit denied: %d %s", w.Code, w.Body.String())
	}
	_ = adminCSRF
}

func csrfOf(t *testing.T, e *env, cookie string) string {
	t.Helper()
	w := e.do(req{method: "GET", path: "/api/v1/auth/session", cookie: cookie})
	if w.Code != 200 {
		t.Fatalf("session: %d", w.Code)
	}
	return decode(t, w)["csrfToken"].(string)
}

// TestIntegrationResponsesValidateAgainstContract validates real responses of
// every implemented read operation, produced from real PostgreSQL data,
// against the schemas of the contract.
func TestIntegrationResponsesValidateAgainstContract(t *testing.T) {
	e := newIntegrationEnv(t)
	chk := newSchemaChecker(t)
	cookie, _, status := e.login("cp_admin", "cp_admin_pw")
	if status != 200 {
		t.Fatalf("login: %d", status)
	}
	calls := []struct{ op, path string }{
		{"getReadiness", "/api/v1/readyz"},
		{"getNodeState", "/api/v1/setup/state"},
		{"getSession", "/api/v1/auth/session"},
		{"getCluster", "/api/v1/cluster"},
		{"listServers", "/api/v1/servers"},
		{"getServer", "/api/v1/servers/127.0.0.1"},
		{"getServer", "/api/v1/servers/127.0.0.4"},
		{"getServer", "/api/v1/servers/localhost"},
		{"getDatabaseCreateOptions", "/api/v1/database-create-options"},
		{"listServerGroups", "/api/v1/server-groups"},
		{"getClusterSettings", "/api/v1/settings/cluster"},
		{"getReplicationTopology", "/api/v1/replication/topology"},
		{"listThisServerInstances", "/api/v1/this-server/instances"},
		{"getThisServerInstance", "/api/v1/this-server/instances/1"},
		{"listDatabases", "/api/v1/databases"},
		{"getDatabase", "/api/v1/databases/kingres_cp"},
		{"getDatabase", "/api/v1/databases/plain_cp"},
		{"listTables", "/api/v1/databases/kingres_cp/tables?limit=3"},
		{"getTable", "/api/v1/databases/kingres_cp/schemas/public/tables/orders"},
		{"getTable", "/api/v1/databases/kingres_cp/schemas/public/tables/audit_log"},
		{"listColocationGroups", "/api/v1/databases/kingres_cp/colocation-groups"},
		{"listBuckets", "/api/v1/databases/kingres_cp/colocation-groups/1/buckets"},
		{"getBucketDistribution", "/api/v1/databases/kingres_cp/bucket-distribution"},
		{"listTasks", "/api/v1/tasks"},
		{"getTask", "/api/v1/tasks/3"},
		{"getTask", "/api/v1/tasks/1"},
		{"listLedgerStatus", "/api/v1/ledger/status"},
		{"getLedgerStatus", "/api/v1/databases/kingres_cp/ledger"},
		{"getLedgerStatus", "/api/v1/databases/plain_cp/ledger"},
		{"listLedgerBlocks", "/api/v1/databases/kingres_cp/ledger/blocks?shardGroupId=1"},
		{"listRoles", "/api/v1/roles"},
		{"getRole", "/api/v1/roles/cp_member"},
		{"listGrants", "/api/v1/grants?grantee=cp_ro"},
		{"listAuditEvents", "/api/v1/audit"},
	}
	for _, c := range calls {
		t.Run(c.op+" "+c.path, func(t *testing.T) {
			w := e.do(req{method: "GET", path: c.path, cookie: cookie})
			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if err := chk.validateResponse(c.op, "200", w.Body.Bytes()); err != nil {
				t.Errorf("response does not match the contract: %v\n%s", err, w.Body.String())
			}
		})
	}

	t.Run("cluster overview content", func(t *testing.T) {
		m := decode(t, e.do(req{method: "GET", path: "/api/v1/cluster", cookie: cookie}))
		if m["mapVersion"] != float64(7) || m["health"] != "degraded" || m["clusterId"] != "7d1f2c0e-3b2a-4d58-9c51-2f6f0a1b9e11" ||
			m["managerLeader"] != nil || m["mapGroupAvailable"] != true {
			t.Errorf("overview = %v", m)
		}
	})
	t.Run("errors from real data are problems", func(t *testing.T) {
		for _, p := range []string{"/api/v1/servers/no-such-host", "/api/v1/databases/nope", "/api/v1/databases/kingres_cp/schemas/public/tables/nope",
			"/api/v1/roles/nope", "/api/v1/tasks/99", "/api/v1/databases/kingres_cp/colocation-groups/99/buckets"} {
			w := e.do(req{method: "GET", path: p, cookie: cookie})
			wantProblem(t, w, 404, "not_found")
			if err := chk.validateSchema("Problem", w.Body.Bytes()); err != nil {
				t.Errorf("%s: %v", p, err)
			}
		}
		wantProblem(t, e.do(req{method: "GET", path: "/api/v1/databases/kingres_cp/ledger/blocks", cookie: cookie}), 400, "validation_failed")
	})
	t.Run("PostgreSQL going away is a 503, not a crash", func(t *testing.T) {
		bad, err := pgstore.New(pgstore.Options{DSN: "postgres://cp_service:cp_service_pw@127.0.0.1:1/x?sslmode=disable&connect_timeout=1",
			Logger: slog.New(slog.DiscardHandler), PanelPort: "8091", ConnectTimeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		defer bad.Close()
		e.srv.deps.Store = bad
		w := e.do(req{method: "GET", path: "/api/v1/servers", cookie: cookie})
		wantProblem(t, w, 503, "service_unavailable")
		wantProblem(t, e.do(req{method: "GET", path: "/api/v1/readyz"}), 503, "service_unavailable")
	})
}

// valuesAt returns the JSON values at a path such as "items[].resources.cpu";
// "[]" steps into every element of an array. Missing keys are reported as an
// error so that a typo in the list below cannot pass silently.
func valuesAt(v any, path string) ([]any, error) {
	cur := []any{v}
	for _, step := range strings.Split(path, ".") {
		each := strings.HasSuffix(step, "[]")
		key := strings.TrimSuffix(step, "[]")
		var next []any
		for _, c := range cur {
			m, ok := c.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%q: not an object at %q", path, step)
			}
			val, present := m[key]
			if !present {
				return nil, fmt.Errorf("%q: key %q is missing", path, key)
			}
			if !each {
				next = append(next, val)
				continue
			}
			arr, ok := val.([]any)
			if !ok {
				return nil, fmt.Errorf("%q: %q is not an array", path, key)
			}
			next = append(next, arr...)
		}
		cur = next
	}
	return cur, nil
}

// noSource lists, per request, the response fields for which the server has
// no data source in this version. Each must be present and null in the real
// responses: the server never invents a zero or placeholder for them
// (kingres/api/README.md, "`null` means not available"). When a source is
// added (the agent, the manager, a settings table) the entry moves out of
// this list and gets its own value test.
var noSource = map[string][]string{
	"/api/v1/servers": {
		"items[].joinedAt", "items[].lastHeartbeatAt", "items[].sizeBytes", "items[].replicationLagMaxSeconds",
		"items[].resources.cpuUsagePercent", "items[].resources.memoryUsedBytes", "items[].resources.memoryTotalBytes",
		"items[].resources.diskUsedBytes", "items[].resources.diskTotalBytes", "items[].instances[].sizeBytes",
	},
	"/api/v1/servers/127.0.0.4": {"uptimeSeconds", "joinedAt", "lastHeartbeatAt", "sizeBytes", "resources.cpuUsagePercent"},
	"/api/v1/servers/127.0.0.1": {
		"bucketsByDatabase[].primarySizeBytes", "bucketsByDatabase[].replicaSizeBytes", "instances[].sizeBytes",
	},
	"/api/v1/cluster":               {"managerLeader"},
	"/api/v1/this-server/instances": {"items[].sizeBytes"},
	"/api/v1/databases": {
		"items[].ledger.digestIntervalSeconds", "items[].stats.transactionsPerSecond",
		"items[].ledgerSummary.verificationState", "items[].ledgerSummary.lastVerifiedBlockHeight",
	},
	"/api/v1/databases/kingres_cp/bucket-distribution": {"servers[].primarySizeBytes", "servers[].replicaSizeBytes"},
	// The cluster has several shard groups and this node sees only its own
	// partitions, so totals over "all shards" are not available.
	"/api/v1/databases/kingres_cp/colocation-groups":            {"items[].sizeBytes"},
	"/api/v1/databases/kingres_cp/colocation-groups/1/buckets":  {"items[].sizeBytes", "items[].movingToShardGroupId"},
	"/api/v1/databases/kingres_cp/schemas/public/tables/orders": {"sizeBytes", "estimatedRows"},
	"/api/v1/databases/kingres_cp/ledger":                       {"verificationState", "lastVerifiedAt", "chains[].verificationState", "chains[].lastVerifiedBlockHeight"},
	"/api/v1/ledger/status":                                     {"items[].verificationState", "items[].lastVerifiedAt", "items[].chains[].verificationState"},
	"/api/v1/setup/state":                                       {"setupTask"},
}

func TestIntegrationValuesWithoutSourceAreNull(t *testing.T) {
	e := newIntegrationEnv(t)
	cookie, _, status := e.login("cp_admin", "cp_admin_pw")
	if status != 200 {
		t.Fatalf("login: %d", status)
	}
	for path, fields := range noSource {
		w := e.do(req{method: "GET", path: path, cookie: cookie})
		if w.Code != 200 {
			t.Errorf("GET %s: %d %s", path, w.Code, w.Body.String())
			continue
		}
		var body any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		for _, f := range fields {
			vals, err := valuesAt(body, f)
			if err != nil {
				t.Errorf("GET %s: %v", path, err)
				continue
			}
			if len(vals) == 0 {
				t.Errorf("GET %s: %s matches no value (the list entry proves nothing)", path, f)
			}
			for _, v := range vals {
				if v != nil {
					t.Errorf("GET %s: %s = %v, want null (no data source)", path, f, v)
				}
			}
		}
	}
	// The local server is the one value the process knows: its uptime.
	m := decode(t, e.do(req{method: "GET", path: "/api/v1/servers/127.0.0.1", cookie: cookie}))
	if m["uptimeSeconds"] == nil {
		t.Error("the local server's uptime is known and must not be null")
	}
}

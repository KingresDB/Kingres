// SPDX-License-Identifier: AGPL-3.0-only

package clustertest_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

const (
	// markerPassword proves that passwords never reach logs, the audit file or
	// the task table; markerKey that no private key does either.
	markerPassword = "MARKER-pw-8c1f2e7a-do-not-leak"
	latePassword   = "Another-Long-Password-77"
)

var (
	sharedMu sync.Mutex
	shared   *clustertest.Cluster
)

func TestMain(m *testing.M) {
	code := m.Run()
	if shared != nil {
		shared.Destroy()
	}
	os.Exit(code)
}

func mkRole(t *testing.T, n *clustertest.Node, name, pw, clusterRole string) {
	t.Helper()
	n.Expect(201, "POST", "/roles", map[string]any{"name": name, "password": pw, "clusterRoles": []string{clusterRole}})
}

func mkDBLedger(t *testing.T, n *clustertest.Node, name string) {
	t.Helper()
	body := n.Expect(202, "POST", "/databases", map[string]any{"name": name, "sharding": map[string]any{"enabled": true, "bucketCount": 32}, "ledger": map[string]any{"mode": "optIn"}})
	n.WaitTaskDone(clustertest.TaskID(body), 4*time.Minute)
}

func mkDB(t *testing.T, n *clustertest.Node, name string) {
	t.Helper()
	body := n.Expect(202, "POST", "/databases", map[string]any{"name": name, "sharding": map[string]any{"enabled": true, "bucketCount": 32}})
	n.WaitTaskDone(clustertest.TaskID(body), 4*time.Minute)
}

// sharedCluster returns the three-server cluster shared by the tests that do
// not change the membership. A role and a database exist before the servers
// join.
func sharedCluster(t *testing.T) *clustertest.Cluster {
	t.Helper()
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if shared == nil {
		shared = clustertest.FormWith(t, 3, func(leader *clustertest.Node) {
			mkRole(t, leader, "early", markerPassword, "kingres_readonly")
			mkDBLedger(t, leader, "before_db")
		})
		shared.Retain()
	}
	shared.Use(t)
	sharedHealth(t, shared)
	return shared
}

func count(t *testing.T, n *clustertest.Node, sql string, args ...any) int {
	t.Helper()
	var v int
	if err := n.ServicePG("postgres").QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		t.Fatalf("node %d: %s: %v", n.Index, sql, err)
	}
	return v
}

func TestFormThreeServers(t *testing.T) {
	c := sharedCluster(t)
	for _, n := range c.Nodes {
		got := n.Servers()
		if len(got) != 3 {
			t.Errorf("node %d lists %d servers: %v", n.Index, len(got), got)
		}
		for id, s := range got {
			if id != c.Leader().Host && s["status"] != "pendingRebalance" && s["status"] != "live" {
				t.Errorf("node %d: server %s has status %v", n.Index, id, s["status"])
			}
		}
	}
	// The leader is the first server and every server knows it.
	for _, n := range c.Nodes {
		body := n.Expect(200, "GET", "/cluster", nil)
		ml, _ := body["managerLeader"].(map[string]any)
		if ml == nil || ml["serverId"] != c.Leader().Host {
			t.Errorf("node %d: managerLeader = %v", n.Index, body["managerLeader"])
		}
	}
}

var objectsOnce sync.Once

// ensureObjects creates the role late and the database after_db, which did not
// exist when the servers joined (once).
func ensureObjects(t *testing.T, c *clustertest.Cluster) {
	t.Helper()
	objectsOnce.Do(func() {
		mkRole(t, c.Leader(), "late", latePassword, "kingres_readonly")
		mkDB(t, c.Leader(), "after_db")
	})
}

func TestObjectsBeforeAndAfterExistOnAllServers(t *testing.T) {
	c := sharedCluster(t)
	ensureObjects(t, c)
	for _, n := range c.Nodes {
		for _, role := range []string{"early", "late", "root_admin"} {
			if got := count(t, n, "SELECT count(*) FROM pg_roles WHERE rolname = $1", role); got != 1 {
				t.Errorf("node %d: role %s: %d", n.Index, role, got)
			}
		}
		for _, db := range []string{"before_db", "after_db"} {
			if got := count(t, n, "SELECT count(*) FROM pg_database WHERE datname = $1", db); got != 1 {
				t.Errorf("node %d: database %s: %d", n.Index, db, got)
			}
			// A sharded database knows the primary of all three servers. The
			// standbys of the shared cluster are registered in every database
			// too, and how many exist at this point depends on the tests that
			// ran before, so only the primaries are counted.
			c2, err := n.PG(context.Background(), "kingresd", servicePassword(t, n), db)
			if err != nil {
				t.Fatal(err)
			}
			var nodes int
			if err := c2.QueryRow(context.Background(), "SELECT count(*) FROM kingres.node WHERE role = 'primary'").Scan(&nodes); err != nil || nodes != 3 {
				t.Errorf("node %d: database %s lists %d primary nodes (%v)", n.Index, db, nodes, err)
			}
			_ = c2.Close(context.Background())
		}
	}
	// The ledger extension, installed before the servers joined, is on all of them.
	for _, n := range c.Nodes {
		c2, err := n.PG(context.Background(), "kingresd", servicePassword(t, n), "before_db")
		if err != nil {
			t.Fatal(err)
		}
		var ok bool
		_ = c2.QueryRow(context.Background(), "SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'kingres_ledger')").Scan(&ok)
		_ = c2.Close(context.Background())
		if !ok {
			t.Errorf("node %d: before_db has no kingres_ledger", n.Index)
		}
	}
}

func servicePassword(t *testing.T, n *clustertest.Node) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(n.DataDir, "service-credentials"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// distributed creates and distributes the table t of after_db (once) and
// inserts rows 1..60 through the node the role signs in to.
var tableOnce sync.Once

func prepareTable(t *testing.T, c *clustertest.Cluster) {
	t.Helper()
	ensureObjects(t, c)
	tableOnce.Do(func() {
		ctx := context.Background()
		admin := c.Leader().MustPG(clustertest.AdminUser, clustertest.AdminPass, "after_db")
		if _, err := admin.Exec(ctx, "CREATE TABLE t (id int PRIMARY KEY, v text)"); err != nil {
			t.Fatalf("creating the table: %v", err)
		}
		task := c.Leader().Expect(202, "PUT", "/databases/after_db/schemas/public/tables/t/distribution",
			map[string]any{"method": "hash", "distributionColumn": "id"})
		c.Leader().WaitTaskDone(clustertest.TaskID(task), 4*time.Minute)
		if _, err := admin.Exec(ctx, "GRANT SELECT, INSERT, UPDATE, DELETE ON t TO late"); err != nil {
			t.Fatalf("grant: %v", err)
		}
		if _, err := admin.Exec(ctx, "GRANT USAGE ON SCHEMA public TO late"); err != nil {
			t.Fatalf("grant: %v", err)
		}
		for i := 1; i <= 60; i++ {
			if _, err := admin.Exec(ctx, "INSERT INTO t VALUES ($1, $2)", i, fmt.Sprintf("row-%d", i)); err != nil {
				t.Fatalf("insert %d: %v", i, err)
			}
		}
	})
}

func TestDistributedTableHasBucketsOnAllServers(t *testing.T) {
	c := sharedCluster(t)
	prepareTable(t, c)
	admin := c.Leader().MustPG(clustertest.AdminUser, clustertest.AdminPass, "after_db")
	var groups int
	if err := admin.QueryRow(context.Background(), "SELECT count(DISTINCT group_id) FROM kingres.bucket_map").Scan(&groups); err != nil || groups != 3 {
		t.Fatalf("buckets are in %d shard groups (%v); want 3", groups, err)
	}
	// Every server owns buckets, and the API says so.
	for _, n := range c.Nodes {
		for id, s := range n.Servers() {
			if s["status"] == "pendingRebalance" {
				t.Errorf("node %d: server %s is still pendingRebalance with buckets", n.Index, id)
			}
		}
	}
}

// owner returns the index of the node that holds the row with this key.
func owner(t *testing.T, c *clustertest.Cluster, key int) int {
	t.Helper()
	admin := c.Leader().MustPG(clustertest.AdminUser, clustertest.AdminPass, "after_db")
	var host string
	err := admin.QueryRow(context.Background(), `SELECT n.host FROM kingres.bucket_map b JOIN kingres.node n ON n.group_id = b.group_id AND n.role = 'primary'
		WHERE b.colocation_id = (SELECT colocation_id FROM kingres.dist_table WHERE relid = 't'::regclass)
		  AND b.bucket = kingres.bucket_for('t'::regclass, $1::int)`, key).Scan(&host)
	if err != nil {
		t.Fatalf("owner of %d: %v", key, err)
	}
	for _, n := range c.Nodes {
		if n.Host == host {
			return n.Index
		}
	}
	t.Fatalf("no node for %s", host)
	return -1
}

func TestStatementsRouteBetweenServersOverCertificates(t *testing.T) {
	c := sharedCluster(t)
	prepareTable(t, c)
	ctx := context.Background()
	// One key owned by each node.
	keyOf := map[int]int{}
	for k := 1; k <= 60 && len(keyOf) < 3; k++ {
		if _, ok := keyOf[owner(t, c, k)]; !ok {
			keyOf[owner(t, c, k)] = k
		}
	}
	if len(keyOf) != 3 {
		t.Fatalf("keys of only %d nodes found", len(keyOf))
	}
	for _, from := range c.Nodes {
		sess := from.MustPG("late", latePassword, "after_db")
		for to, key := range keyOf {
			var v string
			if err := sess.QueryRow(ctx, "SELECT v FROM t WHERE id = $1", key).Scan(&v); err != nil || v != fmt.Sprintf("row-%d", key) {
				t.Errorf("node %d reading key %d owned by node %d: %q %v", from.Index, key, to, v, err)
				continue
			}
			if to == from.Index {
				continue
			}
			// The receiving side authenticated the sender's certificate and ran the
			// statement as the same role, over TLS.
			recv := c.Nodes[to].ServicePG("after_db")
			var ssl bool
			var dn, user string
			err := recv.QueryRow(ctx, `SELECT s.ssl, coalesce(s.client_dn, ''), a.usename FROM pg_stat_ssl s JOIN pg_stat_activity a ON a.pid = s.pid
				WHERE a.datname = 'after_db' AND a.usename = 'late' AND s.ssl AND s.client_dn LIKE '%' || $1 || '%' ORDER BY a.backend_start DESC LIMIT 1`, from.Host).Scan(&ssl, &dn, &user)
			if err != nil || !ssl || user != "late" || !strings.Contains(dn, from.Host) {
				t.Errorf("node %d -> node %d: no TLS session of role late with the certificate of %s on the receiving side: %v %v %q %q",
					from.Index, to, from.Host, err, ssl, dn, user)
			}
		}
		// Writes through any node land on the owner.
		if _, err := sess.Exec(ctx, "UPDATE t SET v = v WHERE id = $1", keyOf[(from.Index+1)%3]); err != nil {
			t.Errorf("node %d: routed update: %v", from.Index, err)
		}
	}
	// Rows inserted through one node are read through the others.
	w := c.Nodes[1].MustPG("late", latePassword, "after_db")
	if _, err := w.Exec(ctx, "INSERT INTO t VALUES (1001, 'via-node-1')"); err != nil {
		t.Fatalf("insert through node 1: %v", err)
	}
	for _, k := range []int{1002, 1003} {
		if _, err := w.Exec(ctx, "INSERT INTO t VALUES ($1, 'via-node-1')", k); err != nil {
			t.Fatalf("insert through node 1: %v", err)
		}
	}
	for _, n := range c.Nodes {
		var cnt int
		if err := n.MustPG("late", latePassword, "after_db").QueryRow(ctx, "SELECT count(*) FROM t WHERE v = 'via-node-1'").Scan(&cnt); err != nil || cnt != 3 {
			t.Errorf("node %d reads %d of the 3 rows (%v)", n.Index, cnt, err)
		}
	}
}

func TestRoleOfOneServerSignsInToAnother(t *testing.T) {
	c := sharedCluster(t)
	ensureObjects(t, c)
	for _, n := range c.Nodes {
		admin := n.Token
		if !n.Login("late", latePassword) {
			t.Errorf("late cannot sign in to the panel API of node %d", n.Index)
			n.Token = admin
			continue
		}
		n.Expect(200, "GET", "/cluster", nil)
		if st, _ := n.Do("POST", "/roles", map[string]any{"name": "nope"}, nil); st != 403 {
			t.Errorf("node %d: a readonly role created a role: %d", n.Index, st)
		}
		n.Token = admin
	}
}

func TestMutationOnAFollowerIsForwardedAndAuditedOnce(t *testing.T) {
	c := sharedCluster(t)
	follower := c.Nodes[1]
	body := follower.Expect(201, "POST", "/roles", map[string]any{"name": "fwd_role", "password": markerPassword, "clusterRoles": []string{"kingres_readonly"}})
	if body["name"] != "fwd_role" {
		t.Fatalf("answer %v", body)
	}
	// The role exists on every server.
	for _, n := range c.Nodes {
		if got := count(t, n, "SELECT count(*) FROM pg_roles WHERE rolname = 'fwd_role'"); got != 1 {
			t.Errorf("node %d: fwd_role: %d", n.Index, got)
		}
	}
	// The audit list is the cluster's: the events a server wrote are those that name it as serverId.
	events := func(n *clustertest.Node) []any {
		out := c.Leader().Expect(200, "GET", "/audit?action=createRole&limit=200", nil)
		items, _ := out["items"].([]any)
		var mine []any
		for _, it := range items {
			m, _ := it.(map[string]any)
			d, _ := m["details"].(map[string]any)
			if m["serverId"] != n.Host || m["actor"] != clustertest.AdminUser || m["outcome"] != "success" {
				continue // the denied attempt of the readonly role is audited where it was made
			}
			if n != c.Leader() || d["forwardedBy"] != nil {
				mine = append(mine, it)
			}
		}
		return mine
	}
	if got := events(c.Leader()); len(got) != 1 {
		t.Fatalf("the leader audited %d forwarded createRole events; want exactly 1: %v", len(got), got)
	} else if d, _ := got[0].(map[string]any)["details"].(map[string]any); d["forwardedBy"] != follower.Host {
		t.Errorf("audit details %v do not name the forwarding server", d)
	}
	if got := events(follower); len(got) != 0 {
		t.Errorf("the forwarding server audited the operation too: %v", got)
	}
	// Reads that only the leader knows are answered through the follower too.
	if st, _ := follower.Do("GET", "/tasks", nil, nil); st != 200 {
		t.Errorf("listing tasks through a follower: %d", st)
	}
}

// SPDX-License-Identifier: PostgreSQL

package pgstore

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
)

// These tests need a PostgreSQL instance loaded with scripts/fixture_*.sql and
// a streaming standby; fixture_test.go builds both with the instance manager.
// They run only when KINGRES_PG_BIN is set.

func newTestStore(t *testing.T) *Store {
	t.Helper()
	if !pgtest.Available() {
		t.Skip("KINGRES_PG_BIN is not set; skipping the PostgreSQL integration test")
	}
	dsn, err := setupFixture(t)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	s, err := New(Options{
		DSN: dsn, Logger: slog.New(slog.DiscardHandler), PanelPort: "8091", Version: "test",
		Started: time.Now().Add(-90 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestIntegrationPingAndVersion(t *testing.T) {
	s := newTestStore(t)
	if err := s.Ping(ctx(t)); err != nil {
		t.Fatal(err)
	}
	v, err := s.PostgresVersion(ctx(t))
	if err != nil || len(v) < 2 || v[:2] != "18" {
		t.Errorf("version = %q, %v", v, err)
	}
}

func TestIntegrationServers(t *testing.T) {
	s := newTestStore(t)
	servers, err := s.Servers(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	// Hosts: 127.0.0.1 (node 1), localhost (2), 127.0.0.3 (3), 127.0.0.4 (4), 127.0.0.5 (5).
	if len(servers) != 5 {
		t.Fatalf("servers = %d: %+v", len(servers), servers)
	}
	byID := map[string]api.Server{}
	for _, sv := range servers {
		byID[sv.ID] = sv
	}
	s1 := byID["127.0.0.1"]
	if s1.Address != "127.0.0.1:8091" || s1.Status != "live" || s1.Version == nil || *s1.Version != "test" || s1.UptimeSeconds == nil || *s1.UptimeSeconds < 60 ||
		s1.ServerGroup == nil || *s1.ServerGroup != "rack-a" || len(s1.Instances) != 1 {
		t.Errorf("server 1 = %+v", s1)
	}
	// The shard group id is the catalog group_id (the map group is 0); node 1 holds the primary copies of
	// the 6 active+moving buckets of group 0 in colocation 1 (0..5) plus 4 of colocation 2.
	if s1.Instances[0].ShardGroupID != 0 || !s1.Instances[0].IsMapGroup || s1.Instances[0].Role != "primary" {
		t.Errorf("instance of server 1 = %+v", s1.Instances[0])
	}
	if s1.Buckets.Primary != 10 || s1.Buckets.Replica != 0 {
		t.Errorf("server 1 buckets = %+v", s1.Buckets)
	}
	if s2 := byID["localhost"]; s2.Buckets.Replica != 10 || s2.Instances[0].UpstreamInstanceID == nil || *s2.Instances[0].UpstreamInstanceID != 1 {
		t.Errorf("server 2 = %+v", s2)
	}
	// Group 1: bucket 6 is moving (counted as held), bucket 7 is an orphan (not held).
	if s3 := byID["127.0.0.3"]; s3.Buckets.Primary != 1 || s3.Status != "live" {
		t.Errorf("server 3 = %+v", s3)
	}
	if s4 := byID["127.0.0.4"]; s4.Status != "dead" || s4.Instances[0].State != "stopped" {
		t.Errorf("server 4 = %+v", s4)
	}
	// Server 5 should not hold shards and holds none: not draining, not pending.
	if s5 := byID["127.0.0.5"]; s5.ShouldHaveShards || s5.Status != "live" {
		t.Errorf("server 5 = %+v", s5)
	}

	detail, err := s.Server(ctx(t), "127.0.0.1")
	if err != nil || len(detail.BucketsByDatabase) != 1 || detail.BucketsByDatabase[0].DatabaseName != "kingres_cp" ||
		detail.BucketsByDatabase[0].PrimaryBuckets != 10 {
		t.Errorf("Server(1) = %+v, %v", detail.BucketsByDatabase, err)
	}
	if _, err := s.Server(ctx(t), "no-such-host"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Server(99): %v", err)
	}
	id, err := s.LocalServerID(ctx(t))
	if err != nil || id != "127.0.0.1" {
		t.Errorf("LocalServerID = %q, %v", id, err)
	}
	groups, err := s.ServerGroups(ctx(t))
	if err != nil || len(groups) != 2 || groups[0].Name != "rack-a" || groups[1].Name != "rack-b" ||
		len(groups[1].ServerIDs) != 3 {
		t.Errorf("ServerGroups = %+v, %v", groups, err)
	}
}

func TestIntegrationClusterSummaryAndSettings(t *testing.T) {
	s := newTestStore(t)
	sum, err := s.ClusterSummary(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if sum.MapVersion != 7 || !sum.MapGroupAvailable || sum.Servers.Total != 5 || sum.Servers.Dead != 1 || sum.Servers.Live != 4 {
		t.Errorf("summary = %+v", sum)
	}
	// 8 + 4 buckets in total; 1 moving, 1 orphan; group 1 has no live standby
	// but the replica count is 1, so its 1 held bucket is under-replicated.
	if sum.Buckets.Total != 12 || sum.Buckets.Moving != 1 || sum.Buckets.Orphan != 1 || sum.Buckets.UnderReplicated != 1 || sum.Buckets.Unavailable != 0 {
		t.Errorf("buckets = %+v", sum.Buckets)
	}
	if sum.DatabaseCount != 3 || sum.Health != "degraded" {
		t.Errorf("databaseCount=%d health=%s", sum.DatabaseCount, sum.Health)
	}
	st, err := s.ClusterSettings(ctx(t))
	if err != nil || st.DefaultReplicaCount != 1 || st.DefaultDurability != "sync" || !st.Features.Sharding || !st.Features.Ledger {
		t.Errorf("settings = %+v, %v", st, err)
	}
}

func TestIntegrationReplicationTopology(t *testing.T) {
	s := newTestStore(t)
	topo, err := s.ReplicationTopology(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(topo.Groups) != 3 {
		t.Fatalf("groups = %d", len(topo.Groups))
	}
	g0 := topo.Groups[0]
	if g0.ID != 0 || g0.PreferredPrimaryServerID != "127.0.0.1" || !g0.IsMapGroup || g0.Primary == nil || g0.Primary.InstanceID != 1 || !g0.Primary.IsOnPreferredServer ||
		g0.Primary.Timeline < 1 {
		t.Errorf("map group = %+v", g0)
	}
	if len(g0.Standbys) != 1 {
		t.Fatalf("standbys = %+v", g0.Standbys)
	}
	sb := g0.Standbys[0]
	if sb.InstanceID != 2 || sb.State != "running" || sb.SyncState == nil || *sb.SyncState != "async" || g0.Health != "healthy" {
		t.Errorf("standby = %+v health=%s", sb, g0.Health)
	}
	if len(g0.Slots) != 1 || g0.Slots[0].Name != "kingres_slot_2" || g0.Slots[0].Type != "physical" || g0.Slots[0].InstanceID != 1 {
		t.Errorf("slots = %+v", g0.Slots)
	}
	// Group 2 (API id 2): primary unreachable, standby stopped: degraded, not an error.
	if g1 := topo.Groups[1]; g1.ID != 1 || g1.Primary == nil || g1.Health != "degraded" || len(g1.Standbys) != 1 || g1.Standbys[0].State != "stopped" {
		t.Errorf("group 1 = %+v", g1)
	}
}

func TestIntegrationDatabases(t *testing.T) {
	s := newTestStore(t)
	dbs, err := s.Databases(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]api.Database{}
	for _, d := range dbs {
		byName[d.Name] = d
	}
	kcp, plain := byName["kingres_cp"], byName["plain_cp"]
	if kcp.Name == "" || plain.Name == "" {
		t.Fatalf("databases = %v", byName)
	}
	if byName["template0"].Name != "" || byName["template1"].Name != "" {
		t.Error("templates must not be listed")
	}
	if str(kcp.General.Owner) != "cp_admin" || str(kcp.General.Encoding) != "UTF8" || kcp.Status != "ready" || !kcp.Sharding.Enabled ||
		kcp.Sharding.BucketCount == nil || *kcp.Sharding.BucketCount != 8 || kcp.Stats.SizeBytes == nil || *kcp.Stats.SizeBytes <= 0 ||
		kcp.Stats.TableCount == nil || *kcp.Stats.TableCount != 7 || str(kcp.Replication.Durability) != "sync" ||
		kcp.Replication.ReplicaCount != 1 || kcp.CreatedAt != nil {
		t.Errorf("kingres_cp = %+v", kcp)
	}
	// The ledger catalog exists, but no catalog stores the mode or the interval: null.
	if kcp.Ledger.Mode != nil || kcp.Ledger.DigestIntervalSeconds != nil || kcp.Stats.TransactionsPerSecond != nil || kcp.LedgerSummary.VerificationState != nil {
		t.Errorf("values without a source must be null: %+v %+v", kcp.Ledger, kcp.Stats)
	}
	if kcp.Health.UnderReplicatedBuckets == nil || *kcp.Health.UnderReplicatedBuckets != 1 || !kcp.RebalanceRequired {
		t.Errorf("health = %+v", kcp.Health)
	}
	if plain.Sharding.Enabled || str(plain.Ledger.Mode) != "off" || str(plain.Replication.Durability) != "syncApply" || plain.Sharding.BucketCount != nil {
		t.Errorf("plain_cp = %+v", plain)
	}
	one, err := s.Database(ctx(t), "kingres_cp")
	if err != nil || one.Name != "kingres_cp" {
		t.Errorf("Database: %+v %v", one, err)
	}
	if _, err := s.Database(ctx(t), "template0"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("template0: %v", err)
	}
	if _, err := s.Database(ctx(t), "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("nope: %v", err)
	}
}

func TestIntegrationTables(t *testing.T) {
	s := newTestStore(t)
	page, err := s.Tables(ctx(t), "kingres_cp", store.TableFilter{}, store.Page{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]api.Table{}
	var order []string
	for _, tb := range page.Items {
		got[tb.Schema+"."+tb.Name] = tb
		order = append(order, tb.Schema+"."+tb.Name)
	}
	want := []string{"public.audit_log", "public.customers", "public.docs", "public.events", "public.orders", "public.settings", "shop.items"}
	if len(order) != len(want) {
		t.Fatalf("tables = %v", order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("tables = %v, want %v", order, want)
		}
	}
	orders := got["public.orders"]
	if len(orders.Classes) != 1 || orders.Classes[0] != "distributed" || orders.Distribution == nil || orders.Distribution.Method != "hash" ||
		*orders.Distribution.Column != "id" || orders.Distribution.ColocationID != 1 || orders.Distribution.BucketCount != 8 ||
		orders.HomeShardGroupID != nil {
		t.Errorf("orders = %+v", orders)
	}
	// A distributed table has data on every group; this node sees only its own
	// partitions, and the cluster has several groups: the totals are null.
	if orders.SizeBytes != nil || orders.EstimatedRows != nil {
		t.Errorf("sizes of a distributed table must be null: %+v", orders)
	}
	// A partitioned table that is not distributed is measured over its partitions.
	if ev := got["public.events"]; ev.SizeBytes == nil || *ev.SizeBytes <= 0 || ev.EstimatedRows == nil || *ev.EstimatedRows != 50 {
		t.Errorf("events = %+v", ev)
	}
	// Never analyzed: the row estimate is unknown, not 0.
	if al := got["public.audit_log"]; al.EstimatedRows != nil || al.SizeBytes == nil {
		t.Errorf("audit_log estimate = %+v", al)
	}
	if al := got["public.audit_log"]; len(al.Classes) != 1 || al.Classes[0] != "ledger" || al.Ledger == nil ||
		al.Ledger.Mode != "appendOnly" || al.HomeShardGroupID == nil || *al.HomeShardGroupID != 0 {
		t.Errorf("audit_log = %+v", al)
	}
	if st := got["public.settings"]; st.Distribution != nil || st.Ledger != nil || st.EstimatedRows == nil || *st.EstimatedRows != 2 {
		t.Errorf("settings = %+v", st)
	}
	for _, name := range []string{"orders_b0000", "orders_b0001"} {
		if _, ok := got["public."+name]; ok {
			t.Errorf("partition %s listed as a table", name)
		}
	}

	// Filters and paging.
	page, _ = s.Tables(ctx(t), "kingres_cp", store.TableFilter{Class: "ledger"}, store.Page{Limit: 10})
	if len(page.Items) != 1 || page.Items[0].Name != "audit_log" {
		t.Errorf("class=ledger: %+v", page.Items)
	}
	page, _ = s.Tables(ctx(t), "kingres_cp", store.TableFilter{Schema: "shop"}, store.Page{Limit: 10})
	if len(page.Items) != 1 || page.Items[0].Name != "items" {
		t.Errorf("schema=shop: %+v", page.Items)
	}
	p1, _ := s.Tables(ctx(t), "kingres_cp", store.TableFilter{}, store.Page{Limit: 2})
	if len(p1.Items) != 2 || p1.NextCursor == nil {
		t.Fatalf("page 1: %+v", p1)
	}
	p2, err := s.Tables(ctx(t), "kingres_cp", store.TableFilter{}, store.Page{Limit: 2, Cursor: *p1.NextCursor})
	if err != nil || len(p2.Items) != 2 || p2.Items[0].Name != "docs" {
		t.Errorf("page 2: %+v %v", p2, err)
	}
	if _, err := s.Tables(ctx(t), "kingres_cp", store.TableFilter{}, store.Page{Limit: 2, Cursor: "bad"}); !errors.Is(err, store.ErrInvalidCursor) {
		t.Errorf("bad cursor: %v", err)
	}

	one, err := s.Table(ctx(t), "kingres_cp", "shop", "items")
	if err != nil || one.Name != "items" {
		t.Errorf("Table: %+v %v", one, err)
	}
	if _, err := s.Table(ctx(t), "kingres_cp", "shop", "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing table: %v", err)
	}
	if _, err := s.Table(ctx(t), "nope", "shop", "items"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing database: %v", err)
	}
	// Without the kingres catalog tables are plain.
	page, err = s.Tables(ctx(t), "plain_cp", store.TableFilter{}, store.Page{Limit: 10})
	if err != nil || len(page.Items) != 1 || page.Items[0].Name != "plain_t" || page.Items[0].Distribution != nil {
		t.Errorf("plain_cp tables = %+v, %v", page, err)
	}
}

func TestIntegrationShardCatalog(t *testing.T) {
	s := newTestStore(t)
	groups, err := s.ColocationGroups(ctx(t), "kingres_cp")
	if err != nil || len(groups) != 2 {
		t.Fatalf("groups = %+v %v", groups, err)
	}
	if g := groups[0]; g.ColocationID != 1 || g.BucketCount != 8 || g.DistributionColumnType != "bigint" || g.TableCount != 2 || g.SizeBytes != nil {
		t.Errorf("group 1 = %+v", g)
	}
	if g := groups[1]; g.ColocationID != 2 || g.DistributionColumnType != "text" || g.TableCount != 0 {
		t.Errorf("group 2 = %+v", g)
	}
	if groups, err := s.ColocationGroups(ctx(t), "plain_cp"); err != nil || len(groups) != 0 || groups == nil {
		t.Errorf("plain_cp groups = %+v %v", groups, err)
	}

	page, err := s.Buckets(ctx(t), "kingres_cp", 1, store.BucketFilter{}, store.Page{Limit: 3})
	if err != nil || page.MapVersion != 7 || len(page.Items) != 3 || page.NextCursor == nil {
		t.Fatalf("buckets page = %+v %v", page, err)
	}
	b0 := page.Items[0]
	if b0.Bucket != 0 || b0.ShardGroupID != 0 || b0.State != "active" || b0.PrimaryServerID == nil || *b0.PrimaryServerID != "127.0.0.1" ||
		len(b0.ReplicaServerIDs) != 1 || b0.ReplicaServerIDs[0] != "localhost" || b0.SizeBytes != nil || b0.MovingToShardGroupID != nil {
		t.Errorf("bucket 0 = %+v", b0)
	}
	moving := "moving"
	page, _ = s.Buckets(ctx(t), "kingres_cp", 1, store.BucketFilter{State: moving}, store.Page{Limit: 10})
	if len(page.Items) != 1 || page.Items[0].Bucket != 6 || page.Items[0].ShardGroupID != 1 {
		t.Errorf("moving: %+v", page.Items)
	}
	srv := "127.0.0.3"
	page, _ = s.Buckets(ctx(t), "kingres_cp", 1, store.BucketFilter{ServerID: &srv}, store.Page{Limit: 10})
	if len(page.Items) != 2 {
		t.Errorf("server 3 holds %d buckets", len(page.Items))
	}
	grp := int32(0)
	page, _ = s.Buckets(ctx(t), "kingres_cp", 1, store.BucketFilter{ShardGroupID: &grp}, store.Page{Limit: 50})
	if len(page.Items) != 6 || page.NextCursor != nil {
		t.Errorf("group 1 buckets = %d", len(page.Items))
	}
	for _, bad := range []struct {
		db string
		cg int32
	}{{"kingres_cp", 99}, {"plain_cp", 1}, {"nope", 1}} {
		if _, err := s.Buckets(ctx(t), bad.db, bad.cg, store.BucketFilter{}, store.Page{Limit: 5}); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Buckets(%s, %d): %v", bad.db, bad.cg, err)
		}
	}

	dist, err := s.BucketDistribution(ctx(t), "kingres_cp", nil)
	if err != nil || dist.MapVersion != 7 || len(dist.Servers) != 5 {
		t.Fatalf("distribution = %+v %v", dist, err)
	}
	if d := dist.Servers[0]; d.ServerID != "127.0.0.1" || d.PrimaryBuckets != 10 || d.ReplicaBuckets != 0 || d.PrimarySizeBytes != nil || d.ReplicaSizeBytes != nil {
		t.Errorf("server 1 = %+v", d)
	}
	cg2 := int32(2)
	dist, _ = s.BucketDistribution(ctx(t), "kingres_cp", &cg2)
	if d := dist.Servers[0]; d.PrimaryBuckets != 4 || dist.Servers[1].ReplicaBuckets != 4 {
		t.Errorf("colocation 2 = %+v", dist.Servers)
	}
	dist, err = s.BucketDistribution(ctx(t), "plain_cp", nil)
	if err != nil || dist.Servers == nil {
		t.Errorf("plain_cp distribution = %+v %v", dist, err)
	}
}

func TestIntegrationTasks(t *testing.T) {
	s := newTestStore(t)
	page, err := s.Tasks(ctx(t), store.TaskFilter{}, store.Page{Limit: 2})
	if err != nil || len(page.Items) != 2 || page.Items[0].ID != 3 || page.NextCursor == nil {
		t.Fatalf("page 1 = %+v %v", page, err)
	}
	if e := page.Items[0].Error; e == nil || e.Code != "step_failed" || e.Retryable == nil || !*e.Retryable || page.Items[0].Subject.Type != "server" {
		t.Errorf("failed task = %+v", page.Items[0])
	}
	if page.Items[0].Progress.Steps == nil {
		t.Error("steps must be an array even when progress is NULL")
	}
	page, err = s.Tasks(ctx(t), store.TaskFilter{}, store.Page{Limit: 2, Cursor: *page.NextCursor})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != 1 || page.NextCursor != nil {
		t.Errorf("page 2 = %+v %v", page, err)
	}
	if p := page.Items[0].Progress; p.Percent != 100 || len(p.Steps) != 1 || p.Steps[0].StartedAt == nil {
		t.Errorf("progress = %+v", p)
	}
	page, _ = s.Tasks(ctx(t), store.TaskFilter{State: "running"}, store.Page{Limit: 10})
	if len(page.Items) != 1 || page.Items[0].Kind != "rebalance" || !page.Items[0].Cancellable {
		t.Errorf("running = %+v", page.Items)
	}
	page, _ = s.Tasks(ctx(t), store.TaskFilter{Kind: "server.add"}, store.Page{Limit: 10})
	if len(page.Items) != 1 {
		t.Errorf("kind filter = %+v", page.Items)
	}
	task, err := s.Task(ctx(t), 2)
	if err != nil || task.State != "running" {
		t.Errorf("Task(2) = %+v %v", task, err)
	}
	if _, err := s.Task(ctx(t), 42); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Task(42): %v", err)
	}
}

func TestIntegrationLedger(t *testing.T) {
	s := newTestStore(t)
	st, err := s.LedgerStatus(ctx(t), "kingres_cp")
	if err != nil || st.Mode != nil || st.LedgerTableCount == nil || *st.LedgerTableCount != 1 || len(st.Chains) != 1 || st.VerificationState != nil {
		t.Fatalf("status = %+v %v", st, err)
	}
	ch := st.Chains[0]
	if ch.ShardGroupID != 0 || ch.VerificationState != nil || ch.LatestBlockHeight == nil || *ch.LatestBlockHeight != 2 || ch.LatestBlockHash == nil ||
		*ch.LatestBlockHash != strings.Repeat("b2", 32) {
		t.Errorf("chain = %+v", ch)
	}
	off, err := s.LedgerStatus(ctx(t), "plain_cp")
	if err != nil || str(off.Mode) != "off" || len(off.Chains) != 0 || off.Chains == nil {
		t.Errorf("plain_cp ledger = %+v %v", off, err)
	}
	if _, err := s.LedgerStatus(ctx(t), "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing db: %v", err)
	}
	all, err := s.LedgerStatuses(ctx(t))
	if err != nil || len(all) != 1 || all[0].DatabaseName != "kingres_cp" {
		t.Errorf("statuses = %+v %v", all, err)
	}

	group := int32(0)
	blocks, err := s.LedgerBlocks(ctx(t), "kingres_cp", &group, store.Page{Limit: 2})
	if err != nil || len(blocks.Items) != 2 || blocks.Items[0].Height != 2 || blocks.NextCursor == nil {
		t.Fatalf("blocks = %+v %v", blocks, err)
	}
	if b := blocks.Items[0]; b.PrevHash == nil || *b.PrevHash != strings.Repeat("b1", 32) || b.HashVersion != 1 || b.TransactionCount != 1 || b.Signed {
		t.Errorf("block 2 = %+v", b)
	}
	blocks, err = s.LedgerBlocks(ctx(t), "kingres_cp", &group, store.Page{Limit: 2, Cursor: *blocks.NextCursor})
	if err != nil || len(blocks.Items) != 1 || blocks.Items[0].Height != 0 || blocks.Items[0].PrevHash != nil || blocks.NextCursor != nil {
		t.Errorf("last page = %+v %v", blocks, err)
	}
	// A sharded database needs the chain; another group's chain is not readable here.
	var arg *store.ArgError
	if _, err := s.LedgerBlocks(ctx(t), "kingres_cp", nil, store.Page{Limit: 2}); !errors.As(err, &arg) || arg.Field != "shardGroupId" {
		t.Errorf("missing shardGroupId: %v", err)
	}
	other := int32(1)
	if b, err := s.LedgerBlocks(ctx(t), "kingres_cp", &other, store.Page{Limit: 2}); err != nil || len(b.Items) != 0 || b.Items == nil {
		t.Errorf("other chain = %+v %v", b, err)
	}
	if b, err := s.LedgerBlocks(ctx(t), "plain_cp", nil, store.Page{Limit: 2}); err != nil || len(b.Items) != 0 {
		t.Errorf("plain_cp blocks = %+v %v", b, err)
	}
}

func TestIntegrationRolesAndGrants(t *testing.T) {
	s := newTestStore(t)
	page, err := s.Roles(ctx(t), store.RoleFilter{}, store.Page{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]api.Role{}
	for _, r := range page.Items {
		got[r.Name] = r
	}
	for _, hidden := range []string{"pg_monitor", "kingres_admin", "kingres_ops", "kingres_readonly", "kingres_stats"} {
		if _, ok := got[hidden]; ok {
			t.Errorf("%s must not be listed as an account", hidden)
		}
	}
	if r := got["cp_admin"]; !r.CanLogin || !r.CanCreateDatabase || r.IsSuperuser || len(r.ClusterRoles) != 1 || r.ClusterRoles[0] != "kingres_admin" ||
		len(r.MemberOf) != 0 || r.MemberOf == nil || r.ValidUntil != nil {
		t.Errorf("cp_admin = %+v", r)
	}
	if r := got["cp_member"]; len(r.ClusterRoles) != 1 || r.ClusterRoles[0] != "kingres_ops" || len(r.MemberOf) != 1 || r.MemberOf[0] != "cp_team" ||
		r.ValidUntil == nil || r.ValidUntil.Year() != 2099 {
		t.Errorf("cp_member = %+v", r) // kingres_ops reached through cp_team
	}
	if r := got["cp_super"]; !r.IsSuperuser || len(r.ClusterRoles) != 0 {
		t.Errorf("cp_super = %+v", r)
	}
	if r := got["cp_ro"]; r.ConnectionLimit != 7 {
		t.Errorf("cp_ro = %+v", r)
	}
	if r := got["cp_disabled"]; r.CanLogin || len(r.ClusterRoles) != 1 {
		t.Errorf("cp_disabled = %+v", r)
	}
	for i := 1; i < len(page.Items); i++ {
		if page.Items[i-1].Name >= page.Items[i].Name {
			t.Fatalf("not ordered by name: %s, %s", page.Items[i-1].Name, page.Items[i].Name)
		}
	}
	no := false
	page, _ = s.Roles(ctx(t), store.RoleFilter{CanLogin: &no, Search: "CP_"}, store.Page{Limit: 100})
	names := map[string]bool{}
	for _, r := range page.Items {
		names[r.Name] = true
	}
	if !names["cp_team"] || !names["cp_disabled"] || names["cp_admin"] || len(names) != 2 {
		t.Errorf("filtered roles = %v", names)
	}
	small, _ := s.Roles(ctx(t), store.RoleFilter{Search: "cp_"}, store.Page{Limit: 3})
	if len(small.Items) != 3 || small.NextCursor == nil {
		t.Errorf("paging: %+v", small)
	}
	one, err := s.Role(ctx(t), "cp_ro")
	if err != nil || one.Name != "cp_ro" {
		t.Errorf("Role: %+v %v", one, err)
	}
	if _, err := s.Role(ctx(t), "kingres_admin"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("builtin role as account: %v", err)
	}
	if _, err := s.Role(ctx(t), "nobody_here"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing role: %v", err)
	}

	grants, err := s.Grants(ctx(t), store.GrantFilter{Grantee: "cp_ro", Database: "kingres_cp"}, store.Page{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	type key struct{ level, schema, table string }
	seen := map[key][]string{}
	for _, g := range grants.Items {
		k := key{level: g.Level}
		if g.Schema != nil {
			k.schema = *g.Schema
		}
		if g.Table != nil {
			k.table = *g.Table
		}
		seen[k] = g.Privileges
	}
	check := func(k key, want ...string) {
		t.Helper()
		got := seen[k]
		if len(got) != len(want) {
			t.Errorf("grants %+v = %v, want %v", k, got, want)
			return
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("grants %+v = %v, want %v", k, got, want)
			}
		}
	}
	check(key{level: "database"}, "connect")
	check(key{level: "schema", schema: "public"}, "usage")
	check(key{level: "table", schema: "public", table: "orders"}, "insert", "select")
	check(key{level: "table", schema: "public", table: "settings"}, "select")
	tbl, _ := s.Grants(ctx(t), store.GrantFilter{Grantee: "cp_ro", Level: "table", Schema: "public", Table: "orders"}, store.Page{Limit: 50})
	if len(tbl.Items) != 1 {
		t.Errorf("table filter = %+v", tbl.Items)
	}
}

// str dereferences an optional string for comparisons.
func str(p *string) string {
	if p == nil {
		return "<null>"
	}
	return *p
}

func TestIntegrationTableColumns(t *testing.T) {
	s := newTestStore(t)
	reasons := func(db, schema, table string) map[string]string {
		t.Helper()
		tb, err := s.Table(ctx(t), db, schema, table)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, c := range tb.Columns {
			if c.CanBeDistributionColumn != (c.DistributionBlockedReason == nil) {
				t.Errorf("%s.%s column %s: canBe=%v reason=%v", schema, table, c.Name, c.CanBeDistributionColumn, c.DistributionBlockedReason)
			}
			out[c.Name] = str(c.DistributionBlockedReason)
		}
		return out
	}
	tests := []struct {
		db, schema, table string
		want              map[string]string
	}{
		// Already distributed: every column is blocked by tableNotPlain.
		{"kingres_cp", "public", "orders", map[string]string{"id": "tableNotPlain", "customer": "tableNotPlain", "amount": "tableNotPlain"}},
		// Plain tables of a sharded database: a column must be in every unique key and hashable.
		{"kingres_cp", "public", "settings", map[string]string{"k": "<null>", "v": "notInUniqueKey"}},
		{"kingres_cp", "shop", "items", map[string]string{"id": "<null>", "label": "notInUniqueKey"}},
		// unsupportedType wins over notInUniqueKey (point has no hash operator class).
		{"kingres_cp", "public", "docs", map[string]string{"id": "<null>", "loc": "unsupportedType", "body": "notInUniqueKey"}},
		// Sharding is not enabled in plain_cp.
		{"plain_cp", "public", "plain_t", map[string]string{"id": "shardingDisabled", "label": "shardingDisabled"}},
	}
	for _, tc := range tests {
		got := reasons(tc.db, tc.schema, tc.table)
		if len(got) != len(tc.want) {
			t.Errorf("%s.%s columns = %v, want %v", tc.schema, tc.table, got, tc.want)
			continue
		}
		for col, w := range tc.want {
			if got[col] != w {
				t.Errorf("%s.%s column %s: reason %q, want %q", tc.schema, tc.table, col, got[col], w)
			}
		}
	}
	tb, _ := s.Table(ctx(t), "kingres_cp", "public", "docs")
	if len(tb.Columns) != 3 || tb.Columns[0].Name != "id" || tb.Columns[0].Type != "integer" || tb.Columns[0].Nullable ||
		tb.Columns[1].Type != "point" || !tb.Columns[1].Nullable {
		t.Errorf("docs columns = %+v", tb.Columns)
	}
}

func TestIntegrationDatabaseCreateOptions(t *testing.T) {
	s := newTestStore(t)
	o, err := s.DatabaseCreateOptions(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	enc := map[string]bool{}
	defaults := 0
	for _, e := range o.Encodings {
		enc[e.Name] = true
		if e.IsDefault {
			defaults++
		}
	}
	if !enc["UTF8"] || !enc["LATIN1"] || enc["SJIS"] || enc["GBK"] || enc["UHC"] || defaults != 1 {
		t.Errorf("encodings = %+v", o.Encodings)
	}
	col := map[string]api.CollationOption{}
	defaults = 0
	for _, c := range o.Collations {
		col[c.Name] = c
		if c.IsDefault {
			defaults++
		}
	}
	if c, ok := col["C"]; !ok || c.Encodings != nil {
		t.Errorf("collation C = %+v (must exist and fit every encoding)", c)
	}
	if defaults != 1 {
		t.Errorf("%d default collations", defaults)
	}
	if o.BucketCount != (api.IntegerRange{Default: 128, Minimum: 1, Maximum: 4096}) ||
		o.LedgerDigestIntervalSeconds != (api.IntegerRange{Default: 3600, Minimum: 60, Maximum: 86400}) ||
		o.ReplicaCount != 1 || o.DefaultDurability != "sync" {
		t.Errorf("options = %+v", o)
	}
}

// The system database kingres_system exists in every real cluster. It is not
// listed by Databases, so nothing that reports on "the databases" may count
// or visit it: the ledger list and the grant list (without filters) answered
// 404 because they visited it, and the cluster summary counted it.
func TestIntegrationSystemDatabaseIsHiddenEverywhere(t *testing.T) {
	s := newTestStore(t)
	dbs, err := s.Databases(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dbs {
		if d.Name == "kingres_system" {
			t.Fatal("kingres_system is listed")
		}
	}
	sum, err := s.ClusterSummary(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if sum.DatabaseCount != len(dbs) {
		t.Errorf("databaseCount = %d, listed databases = %d", sum.DatabaseCount, len(dbs))
	}
	if _, err := s.LedgerStatuses(ctx(t)); err != nil {
		t.Errorf("LedgerStatuses: %v", err)
	}
	if _, err := s.Grants(ctx(t), store.GrantFilter{}, store.Page{Limit: 50}); err != nil {
		t.Errorf("Grants without filters: %v", err)
	}
}

// The listings of tables, roles and grants page with a keyset in SQL: a page
// reads one row more than it returns, any number of pages give every row once in
// order, and a cursor of the old kind is refused.
func TestIntegrationKeysetPagingOfTablesRolesAndGrants(t *testing.T) {
	s := newTestStore(t)
	// Tables: every table of the fixture database page by page, of every size.
	all, err := s.Tables(ctx(t), "kingres_cp", store.TableFilter{}, store.Page{Limit: 500})
	if err != nil || all.NextCursor != nil || len(all.Items) < 4 {
		t.Fatalf("all tables: %d %v", len(all.Items), err)
	}
	for size := 1; size <= 3; size++ {
		var names []string
		cursor := ""
		for i := 0; i < 100; i++ {
			pg, err := s.Tables(ctx(t), "kingres_cp", store.TableFilter{}, store.Page{Limit: size, Cursor: cursor})
			if err != nil {
				t.Fatal(err)
			}
			for _, it := range pg.Items {
				names = append(names, it.Schema+"."+it.Name)
			}
			if pg.NextCursor == nil {
				break
			}
			if len(pg.Items) != size {
				t.Fatalf("a page with a next page holds %d, want %d", len(pg.Items), size)
			}
			cursor = *pg.NextCursor
		}
		if len(names) != len(all.Items) {
			t.Fatalf("page size %d: %d tables, want %d", size, len(names), len(all.Items))
		}
		for i, it := range all.Items {
			if names[i] != it.Schema+"."+it.Name {
				t.Errorf("page size %d: position %d is %s, want %s.%s", size, i, names[i], it.Schema, it.Name)
			}
		}
	}
	// The class filter works with the paging (it runs in the same query).
	for _, class := range []string{"plain", "ledger", "distributed", "reference"} {
		want := 0
		for _, it := range all.Items {
			for _, c := range it.Classes {
				if c == class {
					want++
				}
			}
		}
		got := 0
		cursor := ""
		for i := 0; i < 100; i++ {
			pg, err := s.Tables(ctx(t), "kingres_cp", store.TableFilter{Class: class}, store.Page{Limit: 1, Cursor: cursor})
			if err != nil {
				t.Fatal(err)
			}
			got += len(pg.Items)
			if pg.NextCursor == nil {
				break
			}
			cursor = *pg.NextCursor
		}
		if got != want {
			t.Errorf("class %s: %d tables by page of 1, %d in the whole list", class, got, want)
		}
	}
	// Roles.
	roles, err := s.Roles(ctx(t), store.RoleFilter{Search: "cp_"}, store.Page{Limit: 100})
	if err != nil || len(roles.Items) < 5 {
		t.Fatalf("roles: %d %v", len(roles.Items), err)
	}
	var rn []string
	cursor := ""
	for i := 0; i < 100; i++ {
		pg, err := s.Roles(ctx(t), store.RoleFilter{Search: "cp_"}, store.Page{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range pg.Items {
			rn = append(rn, r.Name)
		}
		if pg.NextCursor == nil {
			break
		}
		cursor = *pg.NextCursor
	}
	if len(rn) != len(roles.Items) {
		t.Fatalf("roles by page of 2: %v, want %d", rn, len(roles.Items))
	}
	for i := range rn {
		if rn[i] != roles.Items[i].Name {
			t.Errorf("role %d: %s, want %s", i, rn[i], roles.Items[i].Name)
		}
	}
	// Grants, across the databases and the levels.
	allG, err := s.Grants(ctx(t), store.GrantFilter{}, store.Page{Limit: 500})
	if err != nil || len(allG.Items) < 4 {
		t.Fatalf("grants: %d %v", len(allG.Items), err)
	}
	var gk []string
	cursor = ""
	for i := 0; i < 500; i++ {
		pg, err := s.Grants(ctx(t), store.GrantFilter{}, store.Page{Limit: 3, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range pg.Items {
			gk = append(gk, grantCursor(g))
		}
		if pg.NextCursor == nil {
			break
		}
		cursor = *pg.NextCursor
	}
	if len(gk) != len(allG.Items) {
		t.Fatalf("grants by page of 3: %d, want %d", len(gk), len(allG.Items))
	}
	for i := range gk {
		if gk[i] != grantCursor(allG.Items[i]) {
			t.Errorf("grant %d differs", i)
		}
	}
	// Offsets and cursors of another list are refused.
	for _, bad := range []string{store.EncodeOffset(2), store.EncodeKey(5), store.EncodeTextKey("a", "b", "c"), "x"} {
		if _, err := s.Roles(ctx(t), store.RoleFilter{}, store.Page{Limit: 2, Cursor: bad}); !errors.Is(err, store.ErrInvalidCursor) {
			t.Errorf("roles cursor %q: %v", bad, err)
		}
		if _, err := s.Grants(ctx(t), store.GrantFilter{}, store.Page{Limit: 2, Cursor: bad}); !errors.Is(err, store.ErrInvalidCursor) {
			t.Errorf("grants cursor %q: %v", bad, err)
		}
		if _, err := s.Tables(ctx(t), "kingres_cp", store.TableFilter{}, store.Page{Limit: 2, Cursor: bad}); !errors.Is(err, store.ErrInvalidCursor) {
			t.Errorf("tables cursor %q: %v", bad, err)
		}
	}
}

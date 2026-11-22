// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
)

// nilUUID is reported as the cluster id of a node whose state file does not
// carry one yet.

func (s *Server) getCluster(_ *call, w http.ResponseWriter, r *http.Request) error {
	sum, err := s.deps.Store.ClusterSummary(r.Context())
	if err != nil {
		return s.storeProblem(r, err, "")
	}
	settings, err := s.deps.Store.ClusterSettings(r.Context())
	if err != nil {
		return s.storeProblem(r, err, "")
	}
	snap := s.deps.Setup.Snapshot()
	out := api.ClusterOverview{
		ClusterID: optStr(snap.ClusterID), Name: optStr(snap.ClusterName), Health: sum.Health,
		Version: s.deps.Version, MapVersion: sum.MapVersion, MapGroupAvailable: sum.MapGroupAvailable,
		ManagerLeader: s.managerLeader(r.Context()),
		Servers:       sum.Servers, Buckets: sum.Buckets, DatabaseCount: sum.DatabaseCount,
		Rebalance: api.RebalanceSummary{Needed: sum.RebalanceNeeded, Reasons: sum.RebalanceReasons, Running: sum.RebalanceRunning, TaskID: sum.RebalanceTaskID},
		AutoFailover: api.AutoFailoverSummary{Enabled: settings.AutoFailover.Enabled,
			EventsSinceReset: settings.AutoFailover.EventsSinceReset, MaxCount: settings.AutoFailover.MaxCount},
		Features:  settings.Features,
		UpdatedAt: s.now().UTC(),
	}
	if out.Rebalance.Reasons == nil {
		out.Rebalance.Reasons = []string{}
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

func (s *Server) listServers(c *call, w http.ResponseWriter, r *http.Request) error {
	items, err := s.deps.Store.Servers(r.Context())
	if err != nil {
		return s.storeProblem(r, err, "")
	}
	ptrs := make([]*api.Server, len(items))
	for i := range items {
		ptrs[i] = &items[i]
	}
	s.completeServers(c, r, ptrs)
	s.writeJSON(w, r, http.StatusOK, api.ServerList{Items: nonNil(items)})
	return nil
}

func (s *Server) getServer(c *call, w http.ResponseWriter, r *http.Request) error {
	pp := newParams(r)
	id := pp.pathServerID("serverId")
	if p := pp.err(); p != nil {
		return p
	}
	srv, err := s.deps.Store.Server(r.Context(), id)
	if err != nil {
		return s.storeProblem(r, err, "Server "+r.PathValue("serverId")+" does not exist.")
	}
	s.completeServers(c, r, []*api.Server{&srv.Server})
	s.writeJSON(w, r, http.StatusOK, srv)
	return nil
}

func (s *Server) listServerGroups(_ *call, w http.ResponseWriter, r *http.Request) error {
	items, err := s.deps.Store.ServerGroups(r.Context())
	if err != nil {
		return s.storeProblem(r, err, "")
	}
	s.writeJSON(w, r, http.StatusOK, api.ServerGroupList{Items: nonNil(items)})
	return nil
}

func (s *Server) getClusterSettings(_ *call, w http.ResponseWriter, r *http.Request) error {
	v, err := s.deps.Store.ClusterSettings(r.Context())
	if err != nil {
		return s.storeProblem(r, err, "")
	}
	if v.Restart.ServerIDs == nil {
		v.Restart.ServerIDs = []string{}
	}
	s.writeJSON(w, r, http.StatusOK, v)
	return nil
}

// clusterRoleDescriptions are the descriptions of the four built-in roles.
var clusterRoleDescriptions = map[string]string{
	auth.RoleAdmin:    "Full control: roles, grants, network access, settings, databases, audit log, and everything the lower roles can do.",
	auth.RoleOps:      "Operate the cluster: servers, failover, rebalance, tasks, instances, ledger digests and verification, alerts.",
	auth.RoleReadonly: "Read the state of the cluster, databases, tables, shards, replication, tasks, ledger and logs; run read-only queries.",
	auth.RoleStats:    "Read metrics and the own session only.",
}

func (s *Server) listClusterRoles(_ *call, w http.ResponseWriter, r *http.Request) error {
	items := make([]api.ClusterRoleInfo, 0, len(auth.ClusterRoles))
	for _, name := range auth.ClusterRoles {
		items = append(items, api.ClusterRoleInfo{Name: name, Description: clusterRoleDescriptions[name]})
	}
	s.writeJSON(w, r, http.StatusOK, api.ClusterRoleInfoList{Items: items})
	return nil
}

func (s *Server) getReplicationTopology(_ *call, w http.ResponseWriter, r *http.Request) error {
	v, err := s.deps.Store.ReplicationTopology(r.Context())
	if err != nil {
		return s.storeProblem(r, err, "")
	}
	if v.Groups == nil {
		v.Groups = []api.ShardGroup{}
	}
	s.writeJSON(w, r, http.StatusOK, v)
	return nil
}

// localInstances returns the instances of the server this node runs on; the
// list is empty when the node is not in the catalog.
func (s *Server) localInstances(r *http.Request) ([]api.Instance, error) {
	if s.deps.Ops != nil && s.deps.Ops.LocalInstanceDown() {
		return s.instancesFromMemory(r)
	}
	items, err := s.readLocalInstances(r)
	if err == nil {
		cp := append([]api.Instance(nil), items...)
		s.inst.last.Store(&cp)
	}
	return items, err
}

func (s *Server) readLocalInstances(r *http.Request) ([]api.Instance, error) {
	id, err := s.deps.Store.LocalServerID(r.Context())
	if err != nil {
		if isNotFound(err) {
			return []api.Instance{}, nil
		}
		return nil, err
	}
	srv, err := s.deps.Store.Server(r.Context(), id)
	if err != nil {
		return nil, err
	}
	return nonNil(srv.Instances), nil
}

// instancesProblem is storeProblem for localInstances, which may also answer
// with a problem of its own (the instance is stopped and nothing is known).
func (s *Server) instancesProblem(r *http.Request, err error) error {
	var p *problem.Problem
	if errors.As(err, &p) {
		return p
	}
	return s.storeProblem(r, err, "")
}

func (s *Server) listThisServerInstances(_ *call, w http.ResponseWriter, r *http.Request) error {
	items, err := s.localInstances(r)
	if err != nil {
		return s.instancesProblem(r, err)
	}
	s.writeJSON(w, r, http.StatusOK, api.InstanceList{Items: items})
	return nil
}

func (s *Server) getThisServerInstance(_ *call, w http.ResponseWriter, r *http.Request) error {
	pp := newParams(r)
	id := pp.pathInt32("instanceId")
	if p := pp.err(); p != nil {
		return p
	}
	items, err := s.localInstances(r)
	if err != nil {
		return s.instancesProblem(r, err)
	}
	for _, in := range items {
		if in.ID == id {
			s.writeJSON(w, r, http.StatusOK, in)
			return nil
		}
	}
	return problem.New(problem.NotFound, "Instance "+r.PathValue("instanceId")+" does not run on this server.")
}

func (s *Server) listDatabases(c *call, w http.ResponseWriter, r *http.Request) error {
	items, err := s.deps.Store.Databases(r.Context())
	if err != nil {
		return s.storeProblem(r, err, "")
	}
	s.completeDatabases(c, r, "listDatabases", nil, items)
	s.writeJSON(w, r, http.StatusOK, api.DatabaseList{Items: nonNil(items)})
	return nil
}

func (s *Server) getDatabase(c *call, w http.ResponseWriter, r *http.Request) error {
	pp := newParams(r)
	name := pp.pathName("databaseName")
	if p := pp.err(); p != nil {
		return p
	}
	db, err := s.deps.Store.Database(r.Context(), name)
	if err != nil {
		return s.storeProblem(r, err, "Database \""+name+"\" does not exist.")
	}
	one := []api.Database{db}
	s.completeDatabases(c, r, "getDatabase", map[string]string{"databaseName": name}, one)
	db = one[0]
	s.writeJSON(w, r, http.StatusOK, db)
	return nil
}

func (s *Server) listTables(_ *call, w http.ResponseWriter, r *http.Request) error {
	pp := newParams(r)
	db := pp.pathName("databaseName")
	f := store.TableFilter{
		Schema: pp.maxLen("schema", 63),
		Class:  pp.enum("class", "plain", "distributed", "reference", "ledger"),
	}
	page := pp.page()
	if p := pp.err(); p != nil {
		return p
	}
	v, err := s.deps.Store.Tables(r.Context(), db, f, page)
	if err != nil {
		return s.storeProblem(r, err, "Database \""+db+"\" does not exist.")
	}
	s.writeJSON(w, r, http.StatusOK, v)
	return nil
}

func (s *Server) getTable(_ *call, w http.ResponseWriter, r *http.Request) error {
	pp := newParams(r)
	db, schema, name := pp.pathName("databaseName"), pp.pathName("schemaName"), pp.pathName("tableName")
	if p := pp.err(); p != nil {
		return p
	}
	t, err := s.deps.Store.Table(r.Context(), db, schema, name)
	if err != nil {
		return s.storeProblem(r, err, "Table \""+schema+"."+name+"\" does not exist in database \""+db+"\".")
	}
	s.writeJSON(w, r, http.StatusOK, t)
	return nil
}

func (s *Server) listColocationGroups(_ *call, w http.ResponseWriter, r *http.Request) error {
	pp := newParams(r)
	db := pp.pathName("databaseName")
	if p := pp.err(); p != nil {
		return p
	}
	kind := pp.enum("kind", "hash", "schema")
	if p := pp.err(); p != nil {
		return p
	}
	items, err := s.deps.Store.ColocationGroups(r.Context(), db)
	if err != nil {
		return s.storeProblem(r, err, "Database \""+db+"\" does not exist.")
	}
	if kind != "" {
		kept := items[:0:0]
		for _, g := range items {
			if k := g.Kind; k == kind || k == "" && kind == "hash" {
				kept = append(kept, g)
			}
		}
		items = kept
	}
	s.writeJSON(w, r, http.StatusOK, api.ColocationGroupList{Items: nonNil(items)})
	return nil
}

func (s *Server) listBuckets(_ *call, w http.ResponseWriter, r *http.Request) error {
	pp := newParams(r)
	db := pp.pathName("databaseName")
	cg := pp.pathInt32("colocationId")
	f := store.BucketFilter{
		ShardGroupID: pp.int32Query("shardGroupId"),
		ServerID:     pp.serverIDQuery("serverId"),
		State:        pp.enum("state", "active", "moving", "orphan", "splitting"),
	}
	page := pp.page()
	if p := pp.err(); p != nil {
		return p
	}
	v, err := s.deps.Store.Buckets(r.Context(), db, cg, f, page)
	if err != nil {
		return s.storeProblem(r, err, "Colocation group "+r.PathValue("colocationId")+" does not exist in database \""+db+"\".")
	}
	s.writeJSON(w, r, http.StatusOK, v)
	return nil
}

func (s *Server) getBucketDistribution(_ *call, w http.ResponseWriter, r *http.Request) error {
	pp := newParams(r)
	db := pp.pathName("databaseName")
	cg := pp.int32Query("colocationId")
	if p := pp.err(); p != nil {
		return p
	}
	v, err := s.deps.Store.BucketDistribution(r.Context(), db, cg)
	if err != nil {
		return s.storeProblem(r, err, "Database \""+db+"\" does not exist.")
	}
	if v.Servers == nil {
		v.Servers = []api.ServerBucketCount{}
	}
	s.writeJSON(w, r, http.StatusOK, v)
	return nil
}

func (s *Server) listTasks(_ *call, w http.ResponseWriter, r *http.Request) error {
	pp := newParams(r)
	f := store.TaskFilter{
		State: pp.enum("state", "pending", "running", "paused", "failed", "done"),
		Kind: pp.enum("kind", "cluster.init", "cluster.join", "server.add", "server.remove", "server.failover",
			"server.leave", "instance.start", "instance.stop", "instance.restart", "database.create",
			"database.update", "database.drop", "table.distribute", "table.undistribute", "table.alter", "table.partition", "rebalance", "bucket.split", "schema.distribute", "schema.move", "schema.undistribute",
			"replication.restorePreferredPrimaries", "ledger.verify", "networkAccess.apply", "replicate.apply",
			"certificate.renew", "certificate.revoke", "standby.reconcile", "config.apply", "config.restart",
			"ledger.key.create", "ledger.key.rotate", "ledger.key.retire", "ledger.key.revoke", "ledger.key.distribute",
			"ledger.bundle.create", "ledger.epoch.start", "ledger.upgrade"),
	}
	page := pp.page()
	if p := pp.err(); p != nil {
		return p
	}
	v, err := s.deps.Store.Tasks(r.Context(), f, page)
	if err != nil {
		return s.storeProblem(r, err, "")
	}
	for i := range v.Items {
		v.Items[i] = withCancelGuard(v.Items[i])
	}
	s.writeJSON(w, r, http.StatusOK, v)
	return nil
}

func (s *Server) getTask(_ *call, w http.ResponseWriter, r *http.Request) error {
	pp := newParams(r)
	id := pp.pathInt64("taskId")
	if p := pp.err(); p != nil {
		return p
	}
	t, err := s.deps.Store.Task(r.Context(), id)
	if err != nil {
		return s.storeProblem(r, err, "Task "+r.PathValue("taskId")+" does not exist.")
	}
	t = withCancelGuard(t)
	s.writeJSON(w, r, http.StatusOK, t)
	return nil
}

func (s *Server) listLedgerStatus(c *call, w http.ResponseWriter, r *http.Request) error {
	items, err := s.deps.Store.LedgerStatuses(r.Context())
	if err != nil {
		return s.storeProblem(r, err, "")
	}
	s.completeLedgerStatus(c, r, "listLedgerStatus", nil, items)
	for i := range items {
		normalizeLedgerCoverage(&items[i])
	}
	s.writeJSON(w, r, http.StatusOK, api.LedgerStatusList{Items: nonNil(items)})
	return nil
}

func (s *Server) getLedgerStatus(c *call, w http.ResponseWriter, r *http.Request) error {
	pp := newParams(r)
	db := pp.pathName("databaseName")
	if p := pp.err(); p != nil {
		return p
	}
	v, err := s.deps.Store.LedgerStatus(r.Context(), db)
	if err != nil {
		return s.storeProblem(r, err, "Database \""+db+"\" does not exist.")
	}
	one := []api.LedgerStatus{v}
	s.completeLedgerStatus(c, r, "getLedgerStatus", map[string]string{"databaseName": db}, one)
	v = one[0]
	normalizeLedgerCoverage(&v)
	if v.Chains == nil {
		v.Chains = []api.LedgerChainStatus{}
	}
	s.writeJSON(w, r, http.StatusOK, v)
	return nil
}

func (s *Server) listLedgerBlocks(_ *call, w http.ResponseWriter, r *http.Request) error {
	pp := newParams(r)
	db := pp.pathName("databaseName")
	group := pp.int32Query("shardGroupId")
	page := pp.page()
	if p := pp.err(); p != nil {
		return p
	}
	v, err := s.deps.Store.LedgerBlocks(r.Context(), db, group, page)
	if err != nil {
		return s.storeProblem(r, err, "Database \""+db+"\" does not exist.")
	}
	s.writeJSON(w, r, http.StatusOK, v)
	return nil
}

func (s *Server) listRoles(_ *call, w http.ResponseWriter, r *http.Request) error {
	pp := newParams(r)
	f := store.RoleFilter{CanLogin: pp.boolQuery("canLogin"), Search: pp.maxLen("search", 63)}
	page := pp.page()
	if p := pp.err(); p != nil {
		return p
	}
	v, err := s.deps.Store.Roles(r.Context(), f, page)
	if err != nil {
		return s.storeProblem(r, err, "")
	}
	s.writeJSON(w, r, http.StatusOK, v)
	return nil
}

func (s *Server) getRole(_ *call, w http.ResponseWriter, r *http.Request) error {
	pp := newParams(r)
	name := pp.pathName("roleName")
	if p := pp.err(); p != nil {
		return p
	}
	v, err := s.deps.Store.Role(r.Context(), name)
	if err != nil {
		return s.storeProblem(r, err, "Role \""+name+"\" does not exist.")
	}
	s.writeJSON(w, r, http.StatusOK, v)
	return nil
}

func (s *Server) listGrants(_ *call, w http.ResponseWriter, r *http.Request) error {
	pp := newParams(r)
	f := store.GrantFilter{
		Grantee:  pp.maxLen("grantee", 63),
		Database: pp.maxLen("database", 63),
		Level:    pp.enum("level", "database", "schema", "table"),
		Schema:   pp.maxLen("schema", 63),
		Table:    pp.maxLen("table", 63),
	}
	if f.Table != "" && f.Schema == "" {
		pp.fail("table", "requires the schema parameter", "required")
	}
	page := pp.page()
	if p := pp.err(); p != nil {
		return p
	}
	v, err := s.deps.Store.Grants(r.Context(), f, page)
	if err != nil {
		return s.storeProblem(r, err, "")
	}
	s.writeJSON(w, r, http.StatusOK, v)
	return nil
}

// nonNil returns s, or an empty slice when s is nil, so that it is encoded as
// [] and not null.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func (s *Server) getDatabaseCreateOptions(_ *call, w http.ResponseWriter, r *http.Request) error {
	v, err := s.deps.Store.DatabaseCreateOptions(r.Context())
	if err != nil {
		return s.storeProblem(r, err, "")
	}
	v.Encodings = nonNil(v.Encodings)
	v.Collations = nonNil(v.Collations)
	s.writeJSON(w, r, http.StatusOK, v)
	return nil
}

// optStr returns nil for the empty string ("not available").
func optStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// managerLeader describes the manager leader: the server that holds the
// primary of shard group 0 (design control-plane-3.md, D6). It is nil while
// the catalog has none or cannot be read. The same catalog answer feeds
// ClusterOverview.managerLeader, Server.isManagerLeader and
// LocalServerInfo.isManagerLeader, so the three always agree.
func (s *Server) managerLeader(ctx context.Context) *api.ManagerLeader {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	id, err := s.deps.Store.ManagerLeader(ctx)
	if err != nil || id == "" {
		return nil
	}
	l := &api.ManagerLeader{ServerID: id, Address: id}
	if srv, err := s.deps.Store.Server(ctx, id); err == nil {
		l.Address = srv.Address
	}
	return l
}

// normalizeLedgerCoverage makes the coverage of a ledger status explicit: a store
// that named no uncovered shard group read every chain there is.
func normalizeLedgerCoverage(st *api.LedgerStatus) {
	if st.UncoveredShardGroupIDs == nil {
		st.UncoveredShardGroupIDs = []int32{}
		st.Complete = true
	}
}

// SPDX-License-Identifier: AGPL-3.0-only

package pgstore

import (
	"context"
	"net"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
)

// node is one row of kingres.node.
type node struct {
	ID               int32
	Group            int32
	Host             string
	Port             int
	Role             string
	Active           bool
	ShouldHaveShards bool
	ServerGroup      *string
	// Weight is kingres.node.weight, 0 when the extension has no such column.
	Weight float64
}

// serverInfo is a machine: all nodes that share a host name.
type serverInfo struct {
	// ID is the host exactly as stored in kingres.node: the contract defines a
	// server as one distinct host.
	ID string
	// first is the smallest node id on the host; it only orders servers.
	first  int32
	Host   string
	Nodes  []node
	Group  *string
	Should bool
	Active bool
	// Weight is the weight of the nodes of the server (the largest, they are equal
	// when the control plane set them); 0 when the extension has no weights.
	Weight float64
}

// catalog is the cluster layout read from kingres.node.
type catalog struct {
	nodes    []node
	servers  []*serverInfo
	byNode   map[int32]*serverInfo
	byServer map[string]*serverInfo
	// groups maps the catalog group id to its nodes.
	groups map[int32][]node
}

// loadCatalog reads kingres.node from the default database and derives the
// servers: a server is one distinct host of the catalog and its id is that
// host string (kingres/api/README.md, "Identifiers").
func (s *Store) loadCatalog(ctx context.Context) (*catalog, error) {
	p, err := s.pool(ctx, "")
	if err != nil {
		return nil, err
	}
	cat := &catalog{
		byNode: map[int32]*serverInfo{}, byServer: map[string]*serverInfo{}, groups: map[int32][]node{},
	}
	ok, err := s.regclassExists(ctx, p, "kingres.node")
	if err != nil {
		return nil, err
	}
	if !ok {
		return cat, nil
	}
	weightCol := "0::float8"
	var hasWeight bool
	if err := p.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_attribute WHERE attrelid = 'kingres.node'::regclass
		AND attname = 'weight' AND NOT attisdropped)`).Scan(&hasWeight); err == nil && hasWeight {
		weightCol = "weight"
	}
	rows, err := p.Query(ctx, `SELECT node_id, group_id, host, port, role, is_active, should_have_shards, server_group, `+weightCol+`
		FROM kingres.node ORDER BY node_id`)
	if err != nil {
		return nil, s.wrap("reading kingres.node", err)
	}
	defer rows.Close()
	for rows.Next() {
		var n node
		if err := rows.Scan(&n.ID, &n.Group, &n.Host, &n.Port, &n.Role, &n.Active, &n.ShouldHaveShards, &n.ServerGroup, &n.Weight); err != nil {
			return nil, s.wrap("scanning kingres.node", err)
		}
		cat.nodes = append(cat.nodes, n)
	}
	if err := rows.Err(); err != nil {
		return nil, s.wrap("reading kingres.node", err)
	}
	byHost := map[string]*serverInfo{}
	for _, n := range cat.nodes {
		cat.groups[n.Group] = append(cat.groups[n.Group], n)
		sv := byHost[n.Host]
		if sv == nil {
			sv = &serverInfo{ID: n.Host, first: n.ID, Host: n.Host, Should: true}
			byHost[n.Host] = sv
			cat.servers = append(cat.servers, sv)
		}
		sv.Nodes = append(sv.Nodes, n)
		if n.ID < sv.first {
			sv.first = n.ID
		}
		if n.Active {
			sv.Active = true
		}
		if !n.ShouldHaveShards {
			sv.Should = false
		}
		if n.Weight > sv.Weight {
			sv.Weight = n.Weight
		}
		if n.ServerGroup != nil && sv.Group == nil {
			g := *n.ServerGroup
			sv.Group = &g
		}
		cat.byNode[n.ID] = sv
	}
	sort.Slice(cat.servers, func(i, j int) bool { return cat.servers[i].first < cat.servers[j].first })
	for _, sv := range cat.servers {
		cat.byServer[sv.ID] = sv
	}
	return cat, nil
}

// activePrimary returns the active primary node of a catalog group.
func (c *catalog) activePrimary(group int32) (node, bool) {
	for _, n := range c.groups[group] {
		if n.Role == "primary" && n.Active {
			return n, true
		}
	}
	return node{}, false
}

// replicaCount is the replica count of the cluster as far as the catalog
// shows it: the largest number of standbys of any group, at most 3.
func (c *catalog) replicaCount() int {
	most := 0
	for _, g := range c.groups {
		n := 0
		for _, m := range g {
			if m.Role == "standby" {
				n++
			}
		}
		if n > most {
			most = n
		}
	}
	if most > 3 {
		most = 3
	}
	return most
}

// groupBuckets counts the buckets of one group by state.
type groupBuckets struct{ Active, Moving, Orphan int }

// held is the number of buckets a group serves (active and moving).
func (g groupBuckets) held() int { return g.Active + g.Moving }

// shardDatabases returns the names of the connectable databases in which the
// shard catalog exists.
func (s *Store) shardDatabases(ctx context.Context) ([]string, error) {
	names, err := s.databaseNames(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range names {
		p, err := s.pool(ctx, n)
		if err != nil {
			return nil, err
		}
		ok, err := s.regclassExists(ctx, p, "kingres.bucket_map")
		if err != nil {
			s.opts.Logger.Debug("skipping database", "database", n, "error", err)
			continue
		}
		if ok {
			out = append(out, n)
		}
	}
	return out, nil
}

// databaseNames lists the databases that accept connections and are not
// templates, without the system database kingres_system: it is not listed by
// GET /databases, so it is neither counted nor visited by anything that
// reports on "the databases" (the same set everywhere).
func (s *Store) databaseNames(ctx context.Context) ([]string, error) {
	p, err := s.pool(ctx, "")
	if err != nil {
		return nil, err
	}
	rows, err := p.Query(ctx, `SELECT datname FROM pg_database
		WHERE NOT datistemplate AND datallowconn AND datname <> 'kingres_system' ORDER BY datname`)
	if err != nil {
		return nil, s.wrap("listing databases", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, s.wrap("scanning databases", err)
		}
		out = append(out, n)
	}
	return out, s.wrap("listing databases", rows.Err())
}

// bucketStats reads the number of buckets per catalog group and state in one
// database. It returns nil when the database has no shard catalog.
func (s *Store) bucketStats(ctx context.Context, db string, colocationID *int32) (map[int32]*groupBuckets, int64, error) {
	p, err := s.pool(ctx, db)
	if err != nil {
		return nil, 0, err
	}
	ok, err := s.regclassExists(ctx, p, "kingres.bucket_map")
	if err != nil || !ok {
		return nil, 0, err
	}
	var cg any
	if colocationID != nil {
		cg = *colocationID
	}
	rows, err := p.Query(ctx, `SELECT group_id, state, count(*) FROM kingres.bucket_map
		WHERE $1::int IS NULL OR colocation_id = $1 GROUP BY group_id, state`, cg)
	if err != nil {
		return nil, 0, s.wrap("reading kingres.bucket_map", err)
	}
	defer rows.Close()
	out := map[int32]*groupBuckets{}
	for rows.Next() {
		var g int32
		var state string
		var n int
		if err := rows.Scan(&g, &state, &n); err != nil {
			return nil, 0, s.wrap("scanning kingres.bucket_map", err)
		}
		gb := out[g]
		if gb == nil {
			gb = &groupBuckets{}
			out[g] = gb
		}
		switch state {
		case "moving":
			gb.Moving += n
		case "orphan":
			gb.Orphan += n
		default:
			gb.Active += n
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, s.wrap("reading kingres.bucket_map", err)
	}
	ver, err := s.mapVersion(ctx, p)
	return out, ver, err
}

// mapVersion reads kingres.map_version; 0 when the table is missing or empty.
func (s *Store) mapVersion(ctx context.Context, p *pgxpool.Pool) (int64, error) {
	ok, err := s.regclassExists(ctx, p, "kingres.map_version")
	if err != nil || !ok {
		return 0, err
	}
	var v int64
	if err := p.QueryRow(ctx, "SELECT version FROM kingres.map_version LIMIT 1").Scan(&v); err != nil {
		if isNoRows(err) {
			return 0, nil
		}
		return 0, s.wrap("reading kingres.map_version", err)
	}
	return v, nil
}

// instanceOf converts a node to the API instance.
func (c *catalog) instanceOf(n node) api.Instance {
	state := "stopped"
	if n.Active {
		state = "running"
	}
	in := api.Instance{
		ID: n.ID, ServerID: c.byNode[n.ID].ID, ShardGroupID: n.Group,
		IsMapGroup: n.Group == 0, Role: n.Role, State: state, Port: n.Port,
	}
	if n.Role == "standby" {
		if pr, ok := c.activePrimary(n.Group); ok {
			id := pr.ID
			in.UpstreamInstanceID = &id
		}
	}
	return in
}

// statusOf derives the server status from the catalog. primary is the number of
// buckets the server serves as primary, and anyBuckets tells whether buckets exist
// in the cluster at all. A server that should not hold shards is draining while it
// is the primary of buckets (a rebalance moves them away); the replicas it holds do
// not count, a rebalance does not move them. A server whose removal task runs is
// decommissioning (applyRecords).
//
// A server that joined waits for a rebalance as long as it is the primary of no
// bucket. The replicas it holds do not count: the standby that a join creates
// for a neighboring group copies that group's buckets, so whether the server
// held "some data" depended on how far that copy had come when the status was
// read, and the same server was live in one answer and pendingRebalance in the
// next.
func statusOf(sv *serverInfo, primary int, anyBuckets bool) string {
	switch {
	case !sv.Active:
		return "dead"
	case !sv.Should && primary > 0:
		return "draining"
	case sv.Should && primary == 0 && anyBuckets:
		return "pendingRebalance"
	default:
		return "live"
	}
}

// holdings computes, per server, the buckets held as primary and as replica
// over all shard databases, and the number of buckets in total.
func (s *Store) holdings(ctx context.Context, cat *catalog) (perServer map[string]*api.BucketHolding,
	perDB map[string]map[int32]*groupBuckets, version int64, err error) {
	perServer = map[string]*api.BucketHolding{}
	perDB = map[string]map[int32]*groupBuckets{}
	for _, sv := range cat.servers {
		perServer[sv.ID] = &api.BucketHolding{}
	}
	dbs, err := s.shardDatabases(ctx)
	if err != nil {
		return nil, nil, 0, err
	}
	for _, db := range dbs {
		stats, v, err := s.bucketStats(ctx, db, nil)
		if err != nil {
			return nil, nil, 0, err
		}
		if v > version {
			version = v
		}
		perDB[db] = stats
		for g, gb := range stats {
			for _, n := range cat.groups[g] {
				if !n.Active {
					continue
				}
				h := perServer[cat.byNode[n.ID].ID]
				if n.Role == "primary" {
					h.Primary += gb.held()
				} else {
					h.Replica += gb.held()
				}
			}
		}
	}
	return perServer, perDB, version, nil
}

// Servers implements store.Store.
func (s *Store) Servers(ctx context.Context) ([]api.Server, error) {
	cat, err := s.loadCatalog(ctx)
	if err != nil {
		return nil, err
	}
	hold, perDB, _, err := s.holdings(ctx, cat)
	if err != nil {
		return nil, err
	}
	return s.buildServers(ctx, cat, hold, anyHeld(perDB)), nil
}

func (s *Store) buildServers(ctx context.Context, cat *catalog, hold map[string]*api.BucketHolding, anyBuckets bool) []api.Server {
	localHost := s.localHost(ctx, cat)
	out := make([]api.Server, 0, len(cat.servers))
	for _, sv := range cat.servers {
		h := *hold[sv.ID]
		server := api.Server{
			ID: sv.ID, Address: net.JoinHostPort(sv.Host, s.opts.PanelPort), Hostname: sv.Host,
			Status:      s.withLiveness(sv.ID, statusOf(sv, h.Primary, anyBuckets)),
			ServerGroup: sv.Group, ShouldHaveShards: sv.Should, Weight: weightPtr(sv.Weight),
			Version:   versionPtr(s.opts.Version),
			Instances: make([]api.Instance, 0, len(sv.Nodes)), Buckets: h,
		}
		if sv.Host == localHost && localHost != "" {
			up := int64(s.now().Sub(s.opts.Started).Seconds())
			server.UptimeSeconds = &up
		}
		server.LastHeartbeatAt = s.lastHeartbeat(sv.ID)
		if n, ok := cat.activePrimary(0); ok && cat.byNode[n.ID] == sv {
			server.IsManagerLeader = true
		}
		for _, n := range sv.Nodes {
			server.Instances = append(server.Instances, cat.instanceOf(n))
		}
		out = append(out, server)
	}
	s.applyRecords(ctx, out)
	return out
}

// weightPtr is the weight of a server, nil when the catalog has none.
func weightPtr(w float64) *float64 {
	if w <= 0 {
		return nil
	}
	return &w
}

// localHost returns the host of this node from kingres.local_node, or "".
func (s *Store) localHost(ctx context.Context, cat *catalog) string {
	id, err := s.localNodeID(ctx)
	if err != nil {
		return ""
	}
	if sv := cat.byNode[id]; sv != nil {
		return sv.Host
	}
	return ""
}

// localNodeID reads kingres.local_node.
func (s *Store) localNodeID(ctx context.Context) (int32, error) {
	p, err := s.pool(ctx, "")
	if err != nil {
		return 0, err
	}
	ok, err := s.regclassExists(ctx, p, "kingres.local_node")
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, store.ErrNotFound
	}
	var id int32
	if err := p.QueryRow(ctx, "SELECT node_id FROM kingres.local_node LIMIT 1").Scan(&id); err != nil {
		if isNoRows(err) {
			return 0, store.ErrNotFound
		}
		return 0, s.wrap("reading kingres.local_node", err)
	}
	return id, nil
}

// LocalServerID implements store.Store.
func (s *Store) LocalServerID(ctx context.Context) (string, error) {
	id, err := s.localNodeID(ctx)
	if err != nil {
		return "", err
	}
	cat, err := s.loadCatalog(ctx)
	if err != nil {
		return "", err
	}
	sv := cat.byNode[id]
	if sv == nil {
		return "", store.ErrNotFound
	}
	return sv.ID, nil
}

// Server implements store.Store.
func (s *Store) Server(ctx context.Context, id string) (api.ServerDetail, error) {
	cat, err := s.loadCatalog(ctx)
	if err != nil {
		return api.ServerDetail{}, err
	}
	if cat.byServer[id] == nil {
		return api.ServerDetail{}, store.ErrNotFound
	}
	hold, perDB, _, err := s.holdings(ctx, cat)
	if err != nil {
		return api.ServerDetail{}, err
	}
	var found *api.Server
	all := s.buildServers(ctx, cat, hold, anyHeld(perDB))
	for i := range all {
		if all[i].ID == id {
			found = &all[i]
		}
	}
	detail := api.ServerDetail{Server: *found, BucketsByDatabase: []api.DatabaseBucketCount{}}
	sv := cat.byServer[id]
	dbs := make([]string, 0, len(perDB))
	for db := range perDB {
		dbs = append(dbs, db)
	}
	sort.Strings(dbs)
	for _, db := range dbs {
		c := api.DatabaseBucketCount{DatabaseName: db}
		for g, gb := range perDB[db] {
			for _, n := range cat.groups[g] {
				if !n.Active || cat.byNode[n.ID] != sv {
					continue
				}
				if n.Role == "primary" {
					c.PrimaryBuckets += gb.held()
				} else {
					c.ReplicaBuckets += gb.held()
				}
			}
		}
		if c.PrimaryBuckets+c.ReplicaBuckets > 0 {
			detail.BucketsByDatabase = append(detail.BucketsByDatabase, c)
		}
	}
	return detail, nil
}

// ServerGroups implements store.Store.
func (s *Store) ServerGroups(ctx context.Context) ([]api.ServerGroup, error) {
	if groups, ok, err := s.recordedGroups(ctx); err != nil || ok {
		return groups, err
	}
	cat, err := s.loadCatalog(ctx)
	if err != nil {
		return nil, err
	}
	byName := map[string]*api.ServerGroup{}
	for _, sv := range cat.servers {
		if sv.Group == nil {
			continue
		}
		g := byName[*sv.Group]
		if g == nil {
			g = &api.ServerGroup{Name: *sv.Group, ServerIDs: []string{}}
			byName[*sv.Group] = g
		}
		g.ServerIDs = append(g.ServerIDs, sv.ID)
	}
	out := make([]api.ServerGroup, 0, len(byName))
	for _, g := range byName {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ClusterSummary implements store.Store.
func (s *Store) ClusterSummary(ctx context.Context) (store.ClusterSummary, error) {
	cat, err := s.loadCatalog(ctx)
	if err != nil {
		return store.ClusterSummary{}, err
	}
	hold, perDB, version, err := s.holdings(ctx, cat)
	if err != nil {
		return store.ClusterSummary{}, err
	}
	names, err := s.databaseNames(ctx)
	if err != nil {
		return store.ClusterSummary{}, err
	}
	sum := store.ClusterSummary{
		MapVersion: version, DatabaseCount: len(names), RebalanceReasons: []string{},
	}
	_, sum.MapGroupAvailable = cat.activePrimary(0)
	if len(cat.nodes) == 0 {
		sum.MapGroupAvailable = true // no catalog yet: nothing is failing over
	}
	// The counts are made from the very list Servers returns (same catalog snapshot, same
	// status function including the heartbeats), so the overview and the list agree.
	for _, sv := range s.buildServers(ctx, cat, hold, anyHeld(perDB)) {
		sum.Servers.Total++
		switch sv.Status {
		case "live":
			sum.Servers.Live++
		case "suspect":
			sum.Servers.Suspect++
		case "draining":
			sum.Servers.Draining++
		case "dead":
			sum.Servers.Dead++
		case "pendingRebalance":
			sum.Servers.PendingRebalance++
		case "decommissioning":
			sum.Servers.Decommissioning++
		}
	}
	replicas := cat.replicaCount()
	for _, stats := range perDB {
		for g, gb := range stats {
			total := gb.Active + gb.Moving + gb.Orphan
			sum.Buckets.Total += total
			sum.Buckets.Moving += gb.Moving
			sum.Buckets.Orphan += gb.Orphan
			un, under := groupHealth(cat, g, replicas)
			if un {
				sum.Buckets.Unavailable += gb.held()
			} else if under {
				sum.Buckets.UnderReplicated += gb.held()
			}
		}
	}
	if sum.Servers.PendingRebalance > 0 {
		sum.RebalanceReasons = append(sum.RebalanceReasons, "serverAdded")
	}
	if sum.Servers.Draining > 0 {
		sum.RebalanceReasons = append(sum.RebalanceReasons, "serverRemoved")
	}
	if sum.Buckets.UnderReplicated > 0 {
		sum.RebalanceReasons = append(sum.RebalanceReasons, "replicasMissing")
	}
	sum.RebalanceNeeded = len(sum.RebalanceReasons) > 0
	sum.RebalanceTaskID = s.runningRebalance(ctx)
	sum.RebalanceRunning = sum.RebalanceTaskID != nil
	sum.Health = "healthy"
	switch {
	case sum.Buckets.Unavailable > 0 || !sum.MapGroupAvailable:
		sum.Health = "critical"
	case sum.Buckets.UnderReplicated > 0 || sum.Servers.Dead > 0 || sum.Servers.Suspect > 0 || sum.Servers.Draining > 0 || sum.Servers.PendingRebalance > 0:
		sum.Health = "degraded"
	}
	return sum, nil
}

// groupHealth tells whether a catalog group has no live primary
// (unavailable) or fewer live standbys than the replica count
// (underReplicated).
func groupHealth(cat *catalog, group int32, replicas int) (unavailable, underReplicated bool) {
	if _, ok := cat.activePrimary(group); !ok {
		return true, false
	}
	live := 0
	for _, n := range cat.groups[group] {
		if n.Role == "standby" && n.Active {
			live++
		}
	}
	return false, live < replicas
}

func versionPtr(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

// ManagerLeader implements store.Store: the server that holds the active
// primary of catalog group 0, "" when there is none.
func (s *Store) ManagerLeader(ctx context.Context) (string, error) {
	cat, err := s.loadCatalog(ctx)
	if err != nil {
		return "", err
	}
	n, ok := cat.activePrimary(0)
	if !ok {
		return "", nil
	}
	return cat.byNode[n.ID].ID, nil
}

// anyHeld tells whether any shard group serves a bucket in any database. A
// cluster with a sharded database but no distributed table has a bucket map
// and no buckets: a server that owns nothing then is not waiting for a
// rebalance, because there is nothing to move.
func anyHeld(perDB map[string]map[int32]*groupBuckets) bool {
	for _, stats := range perDB {
		for _, gb := range stats {
			if gb.held() > 0 {
				return true
			}
		}
	}
	return false
}

// runningRebalance returns the task of the rebalance that is pending or running,
// nil when there is none or this server's copy of the system database has no task table.
func (s *Store) runningRebalance(ctx context.Context) *int64 {
	p, err := s.pool(ctx, "")
	if err != nil {
		return nil
	}
	if ok, err := s.regclassExists(ctx, p, "kingres_cp.task"); err != nil || !ok {
		return nil
	}
	var id int64
	if err := p.QueryRow(ctx, `SELECT id FROM kingres_cp.task WHERE kind = 'rebalance' AND state IN ('pending', 'running') ORDER BY id DESC LIMIT 1`).Scan(&id); err != nil {
		return nil
	}
	return &id
}

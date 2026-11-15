# Cluster management panel (web admin console + control plane) for KingresDB, modeled on the Couchbase Server Web Console (state as of Nov 2026)

Research note: about 20 tool calls. Several fetches failed or returned thin pages (see Gaps). Anything not listed under "Cited Findings" is either labeled an inference or a gap. Items marked [background, unverified] come from general knowledge and were not confirmed in this session.

## Q1. How Couchbase's cluster manager and web console work (ns_server, orchestrator, chronicle, REST, rebalance, failover, vBuckets, stats, UI)

### Takeaway
Each Couchbase node runs two Erlang processes: a non-cluster-aware babysitter and ns_server. One elected "orchestrator" (Master Services) runs cluster-wide operations. Config and metadata sit in Chronicle, a Raft implementation. The Web Console, CLI and everything else are clients of one REST API on 8091/18091. The pattern is a good template for KingresDB: a node-local supervisor, a single elected orchestrator, a Raft-backed config store, and a REST API as the only interface.

### Cited Findings
- The Cluster Manager runs on every node as two processes, the babysitter and ns-server. The babysitter starts and monitors Couchbase processes (including ns-server) and restarts them if they die. "The babysitter is not cluster-aware." — [Couchbase docs: Cluster Manager](https://docs.couchbase.com/server/current/learn/clusters-and-availability/cluster-manager.html)
- ns-server modules: REST Administration (used by UI and CLI), Authentication, Master Services, Per-Node Services, Per-Node Bucket Services, and generic distributed/local facilities. — [Cluster Manager](https://docs.couchbase.com/server/current/learn/clusters-and-availability/cluster-manager.html)
- Master Services is also called the Orchestrator. Only one instance is in charge at a time, the instances elect it, and another takes over if it dies. It handles cluster-wide operations: failover, rebalance, bucket add/delete. Config changes are managed by Master Services and pushed to the other nodes. — [Cluster Manager](https://docs.couchbase.com/server/current/learn/clusters-and-availability/cluster-manager.html)
- Every cluster-manager instance sends periodic heartbeats with basic node statistics. If auto-failover is enabled and no heartbeat arrives within the timeout, Master Services may fail the node over. — [Cluster Manager](https://docs.couchbase.com/server/current/learn/clusters-and-availability/cluster-manager.html)
- Chronicle is an implementation of Raft used by Couchbase Server to manage its internal metadata. Features: voting and non-voting participants, pre-vote, multiple pluggable state machines on one replicated log, local reads by default (sequential consistency) with linearizable reads available, exactly-once writes, and manual quorum-loss disaster recovery. License is Apache-2.0. — [github.com/couchbase/chronicle](https://github.com/couchbase/chronicle)
- ns_server is an Erlang/OTP application (Erlang 25 required per the README). The repo has `apps/`, `deps/`, `priv/`, `pylib/`, `cluster_tests/`, and a `cluster_run` script for multi-node dev clusters. Dev ports include 8091, 11211 and 11212. The repo carries `LICENSE.txt` ("Copyright (c) 2016, Couchbase, Inc."), but the license type was not visible on the fetched page. — [github.com/couchbase/ns_server](https://github.com/couchbase/ns_server)
- vBuckets: the count is fixed at bucket creation. Couchstore uses 1024 on Linux/Windows. Magma uses 128 or 1024. macOS uses 64. Keys are hashed with CRC32 to pick the vBucket. The Cluster Manager holds the vBucket map and notifies clients when it changes. Clients hash the key, look up the map and connect directly to the node holding the active vBucket. Writes go only to active vBuckets. An active vBucket and its replicas always sit on different nodes. On node failure, replicas are promoted. — [Couchbase docs: vBuckets](https://docs.couchbase.com/server/current/learn/buckets-memory-and-storage/vbuckets.html)
- The docs page for vBuckets does not state the replica range; its example is 1024 vBuckets with 2 replicas, giving 3072 vBuckets in total. The 0-3 replica range was not confirmed in this session. — [vBuckets](https://docs.couchbase.com/server/current/learn/buckets-memory-and-storage/vbuckets.html)
- Rebalance spreads vBuckets evenly across Data Service nodes. Replica vBucket move phases: backfill, then book-keeping. Active vBucket move phases: backfill, book-keeping (including persistence), active takeover, final book-keeping. — [Couchbase docs: Rebalance](https://docs.couchbase.com/server/current/learn/clusters-and-availability/rebalance.html)
- Bucket rank (7.6 and later): 0 to 1000, default 0. Higher-ranked buckets rebalance first. — [Rebalance](https://docs.couchbase.com/server/current/learn/clusters-and-availability/rebalance.html)
- Swap rebalance: when the number of nodes added equals the number removed, data moves only between those nodes. — [Rebalance](https://docs.couchbase.com/server/current/learn/clusters-and-availability/rebalance.html)
- vBucket transfers run sequentially, so a stopped rebalance resumes where it stopped. The per-node limit on concurrent vBucket moves during backfill is 1 to 64, default 4. Each rebalance produces a JSON report, available in the UI, via the "Getting Cluster Tasks" REST call, and as the last five reports under the node's logs/rebalance directory. — [Rebalance](https://docs.couchbase.com/server/current/learn/clusters-and-availability/rebalance.html)
- If too few Data nodes remain for the configured replica count, rebalance builds as many replicas as it can. — [Rebalance](https://docs.couchbase.com/server/current/learn/clusters-and-availability/rebalance.html)
- Auto-failover (8.0 docs):
  - Enabled by default, default timeout 120 s. The minimum is 1 s via REST and 5 s via the UI, and the maximum is 3600 s. Default `maxCount` is 1. It can be raised up to the node count in Enterprise Edition only (Community is always 1).
  - A rebalance resets the failover count.
  - A majority quorum of nodes must remain.
  - Nodes holding buckets with no replicas are not auto-failed over.
  - Each service needs at least 2 nodes running it, with exceptions for the Data Service.
  - Disk-error failover triggers when at least 60% of the recent period (default 120 s) is disk-related failure.
  - Auto-failover is always a hard failover. If it occurs during a rebalance, the rebalance stops and is not retried automatically.
  - If the orchestrator fails, failover is slower. An arbiter node (no services) speeds this up.
  - Failing over a whole server group needs `maxCount` at least the group size and a remaining quorum.
  - It does not work with XDCR, where failover is application-controlled.
  — [Couchbase docs: Automatic Failover](https://docs.couchbase.com/server/current/learn/clusters-and-availability/automatic-failover.html)
- REST endpoints (port 8091, 18091 for TLS): `POST /controller/addNode` (params hostname, user, password, services), `POST /controller/rebalance`, `POST /controller/failOver` (hard, `otpNode` param), `POST /controller/startGracefulFailover`, `POST /controller/setRecoveryType`, plus `joinNode` and `ejectNode` under the nodes and clusters API. — [Couchbase REST API search results](https://docs.couchbase.com/server/7.0/rest-api/rest-endpoints-all.html), [rebalance REST page](https://docs.couchbase.com/server/current/rest-api/rest-cluster-rebalance.html), [addNodes](https://docs.couchbase.com/server/6.0/rest-api/rest-cluster-addnodes.html)
- The Web Console is browser-based and served on 8091 by default. A 2017 redesign drew on Material Design. — [Couchbase manage UI docs](https://docs.couchbase.com/server/7.6/manage/manage-ui/manage-ui.html), [blog](https://www.couchbase.com/blog/?p=1140)
- ns_server has a `/couchBase` pluggable UI proxy endpoint that the UI does not use (proposed for deprecation). — [MB-61950](https://issues.couchbase.com/browse/MB-61950)
- Prometheus integration: `/prometheus_sd_config` discovery endpoint on 8091 (18091 for TLS). Requires an "External Stats Reader" role user. Supports `file_sd_configs` and `http_sd_configs`. The old `.yaml` variant was deprecated before 7.2.1. SDK application telemetry can report via the same endpoint (disabled by default in 8.0). — [Couchbase: Set up Prometheus](https://docs.couchbase.com/server/current/manage/monitor/set-up-prometheus-for-monitoring.html)
- Release status: Couchbase Server 8.0.1 (Mar 2026) and 8.0.2 (Jun 2026) are listed in current docs. 8.0 notes mention user lock/unlock and temporary passwords. — [Release notes](https://docs.couchbase.com/server/current/release-notes/relnotes.md)

### Inferences
- The babysitter/orchestrator split suggests KingresDB node agents should be dumb supervisors, with all cluster decisions in one elected leader (or a leader holding a Raft lease).
- The Couchbase failover safety rules map directly onto a PostgreSQL-shard design: quorum, no auto-failover when no replica exists, max auto-failover count reset by rebalance, and hard failover only for automatic events.
- Rebalance as sequential, resumable per-shard moves with a concurrency cap and a JSON report fits shard moves in KingresDB.
- A fixed vBucket count maps to a fixed number of logical shards or slots chosen at cluster creation. That makes rebalance a move of whole shards, not a resharding.

### Gaps
- AngularJS to Angular history of the console: not confirmed. Search returned nothing. [background, unverified: the UI lives in `ns_server/priv/public/ui`, started on AngularJS and migrated progressively to Angular.]
- ns_server license type: the page did not show it. [background, unverified: Apache-2.0 in recent releases. The `LICENSE.txt` in the repo must be read before reuse.]
- The Chronicle docs page returned 403, so how ns_server uses Chronicle, metakv and the 7.0 transition was not verified.
- The Prometheus scrape path for metrics, retention, and how the stats UI queries Prometheus: not covered in the fetched page. [background, unverified: Prometheus runs as a child process with per-node `/metrics` and the UI queries via PromQL.]
- Web Console screen list, server groups, durability levels and couchbase-cli details were not fetched.

## Q2. Comparable consoles and control planes

### Takeaway
Most distributed databases either embed the console in the server or in a core control component (CockroachDB per node, TiDB Dashboard inside PD). Others run a separate admin API plus web app (Vitess VTAdmin). Citus has no UI, so users build on SQL views. Patroni gives a REST API plus patronictl and leaves the UI to others.

### Cited Findings
- CockroachDB DB Console, Cluster Overview:
  - Nodes are grouped by locality. Columns: address, node ID, uptime, replicas, capacity, memory, vCPUs, version. Decommissioned nodes get their own table.
  - Node states: LIVE, SUSPECT, DRAINING, DECOMMISSIONING, DEAD. A node is DEAD after 5 minutes without a liveness update (`server.time_until_store_dead`).
  - It shows total, under-replicated and unavailable range counts.
  - A node map needs `--locality` flags and drills region to node.
  — [CockroachDB: Cluster Overview page](https://docs.cockroachlabs.com/docs/stable/ui-cluster-overview-page)
- TiDB Dashboard ships inside PD and is reachable from any PD address. It has a Key Visualizer heatmap of read/write traffic (must be enabled in Settings first). Cluster Info shows the status of TiDB, TiKV, PD and TiFlash instances and their hosts. PD stores data-distribution metadata and topology. — [TiDB Dashboard intro v4.0](https://docs.pingcap.com/tidb/v4.0/dashboard-intro), [Key Visualizer](https://docs.pingcap.com/tidb/dev/dashboard-key-visualizer/), [Cluster Info v6.6 (archived)](https://docs-archive.pingcap.com/tidb/v6.6/dashboard-cluster-info/)
- Vitess VTAdmin has two components: vtadmin-web (browser UI) and vtadmin-api (HTTP(S) plus gRPC). The API queries vtgates and vtctlds across one or more clusters, configured by `clusters.yaml`. A page load fans out parallel requests and aggregates them into JSON. The docs list RBAC, cluster discovery and a Vitess Operator mode (page dated 29 Oct 2025). — [VTAdmin architecture](https://vitess.io/docs/24.0/reference/vtadmin/architecture/), [VTAdmin reference](https://vitess.io/docs/reference/vtadmin/)
- Patroni REST API (port 8008 in examples):
  - Health: `/primary`, `/replica`, `/sync`, `/async`, `/health`, `/liveness`, `/readiness`, with `?lag=` limits.
  - Monitoring: `/patroni`, `/metrics` (Prometheus), `/cluster` (members, roles, lag), `/history`.
  - Config: `GET`, `PATCH` and `PUT /config`.
  - Operations: `POST /switchover` (optionally scheduled), `POST /failover`, `POST /restart`, `POST /reload`, `POST /reinitialize` (replicas only).
  - It serves Patroni itself, patronictl, HAProxy and monitoring.
  — [Patroni REST API](https://patroni.readthedocs.io/en/latest/rest_api.html)
- Citus: no built-in dashboard found. The monitoring surface is SQL: `pg_dist_node`, `citus_shards` (node, size per shard), `get_rebalance_table_shards_plan` (read-only), `get_rebalance_progress()` (shard sizes on source and target since 10.1), `pg_dist_background_job`, and `citus_stat_activity` / `citus_dist_stat_activity` (Citus 11). — [Citus 10.1 rebalancing blog](https://www.citusdata.com/blog/2021/09/03/shard-rebalancing-in-citus-10-1/), [Citus metadata tables](https://docs.citusdata.com/en/v11.3/develop/api_metadata.html), [citus_stat_activity blog](https://www.citusdata.com/blog/2022/07/21/citus-stat-activity-views-for-distributed-postgres/)
- pgwatch v5.1.0 (published 16 Mar 2026) is BSD-3-Clause. The pgwatch2 line is archived. — [pkg.go.dev pgwatch/v5](https://pkg.go.dev/github.com/cybertec-postgresql/pgwatch/v5)
- Pigsty docs say the core is Apache-2.0 (docs CC BY 4.0). One older-looking page says AGPLv3, so the sources conflict. Its monitoring stack is Grafana, VictoriaMetrics and VictoriaLogs. — [Pigsty license](https://pigsty.io/docs/about/license), [Pigsty features](https://pigsty.io/docs/about/feature/)
- ClusterControl licensing is contradictory: the PostgreSQL wiki calls it open source, while a 2026 comparison lists it as commercial. — [PostgreSQL wiki Monitoring](https://wiki.postgresql.org/wiki/Monitoring), [aimultiple](https://aimultiple.com/open-source-database-monitoring-tools)

### Inferences
- UI patterns seen in sources: a node table with state badges and replica counts (CockroachDB), a heatmap for hotspots (TiDB), and an under-replicated/unavailable counter as a top-level health signal (CockroachDB). KingresDB should copy all three.
- Citus shows the gap KingresDB can fill: the SQL views exist, but no operations UI. If KingresDB keeps analogous catalog views, the panel can sit on them.
- VTAdmin's stateless API-aggregates-from-existing-components design is the lowest-risk pattern if KingresDB already has a coordinator with catalog tables.

### Gaps
- Not researched or fetch failed: YugabyteDB (yugabyted UI, YB-Master UI, YBA), MongoDB Ops Manager/Atlas, Redis Enterprise, ScyllaDB Manager, Kibana shard allocation, Cassandra, StackGres, CloudNativePG, Crunchy PGO, pgEdge, OpenTenBase, PMM, pgAdmin, pgdash. No sourced facts for these.
- CockroachDB DB Console serving model (per node, port 8080), React stack and license: not verified [background, unverified: React/TypeScript in `pkg/ui`, BSL/CCL with changes in 2024-25, check current terms].
- TiDB Dashboard repo license and tech stack: fetch failed [background, unverified: Apache-2.0, Go backend, React frontend].
- VTAdmin license and frontend tech: not shown on fetched pages [background, unverified: Apache-2.0, Go API, React web].

## Q3. Control plane architecture, engine data sources, tech stack, proposed screens/API, reusable code for KingresDB

### Takeaway
Recommended shape (inference from the sources above plus general practice): a per-node agent supervising local PostgreSQL, a leader-elected manager holding topology in a Raft or etcd-style store, and a Go (or Rust) REST/gRPC API behind a React/TypeScript SPA. Shard placement, replication and rebalance state come from catalog tables plus pg_stat_* views. Time series come from Prometheus-compatible scraping. Most of this section is inference. The cited facts below are the only verified ones.

### Cited Findings
- Verified building blocks for the design: Couchbase's babysitter/ns_server/orchestrator split ([Cluster Manager](https://docs.couchbase.com/server/current/learn/clusters-and-availability/cluster-manager.html)); Chronicle as an Apache-2.0 Raft library ([github.com/couchbase/chronicle](https://github.com/couchbase/chronicle)); Patroni's REST API for switchover, failover, reinitialize and config ([Patroni REST](https://patroni.readthedocs.io/en/latest/rest_api.html)); VTAdmin's separate API plus web split ([VTAdmin architecture](https://vitess.io/docs/24.0/reference/vtadmin/architecture/)); Citus rebalance planning and progress functions as an example of engine-side primitives ([Citus 10.1 blog](https://www.citusdata.com/blog/2021/09/03/shard-rebalancing-in-citus-10-1/)).
- Prometheus-based stats with HTTP service discovery is the Couchbase model since 7.x, and Pigsty uses VictoriaMetrics/Grafana. — [Couchbase Prometheus](https://docs.couchbase.com/server/current/manage/monitor/set-up-prometheus-for-monitoring.html), [Pigsty](https://pigsty.io/docs/about/feature/)

### Inferences
Architecture
- Prefer agent-per-node plus a central manager, not an HTTP background worker inside postgres. An HTTP worker restarts with postgres and cannot start, stop or rebuild it. The agent plays the babysitter role: start/stop, `pg_basebackup` for new replicas, promote, `pg_rewind`.
- Store topology and desired state (nodes, shard map, replica count, task state) in a Raft-backed store: embedded Raft (hashicorp/raft or etcd raft in Go; openraft in Rust) or external etcd. The coordinator catalog table can mirror state for SQL access but should not be the source of truth during coordinator failure.
- Run operations as persisted tasks with ids, a state machine (pending, running, paused, failed, done), idempotent steps, per-shard progress, and a single cluster-wide rebalance lock. Mirror Couchbase: sequential resumable moves, a concurrency cap (default 4), a stored report per run.
- Failover: either integrate Patroni per shard group (it gives REST, DCS-based leader election and switchover) or write your own. Whichever you choose, copy Couchbase's guards: quorum, no auto-failover for shards without a replica, failover count cap reset by rebalance, and manual failover for ambiguous cases.
- Security: TLS on API, session or token auth, RBAC roles (admin, ops, read-only, stats-reader), and an append-only audit log. Couchbase has an External Stats Reader role for Prometheus. KingresDB should have a similar scoped metrics role.

Engine data sources (standard PostgreSQL views, [background, unverified in this session])
- `pg_stat_replication` (sent/write/flush/replay LSN and lag), `pg_stat_wal_receiver`, `pg_replication_slots`, `pg_stat_database`, `pg_stat_activity`, `pg_stat_statements`, `pg_database_size`, plus KingresDB shard-metadata tables and ledger verification state. postgres_exporter covers much of this.
- Time series: embedded or sidecar Prometheus/VictoriaMetrics. Storing metrics in the database itself would load the database being monitored.

Stack (reasoning only, no project evidence gathered here beyond Vitess)
- Backend: Go is the pragmatic choice. Patroni is Python, Vitess and Citus-adjacent tools are Go/C, Chronicle/ns_server are Erlang. Erlang/Elixir gives supervision and distribution but has a small hiring pool. Rust is viable if KingresDB core is Rust.
- API: gRPC internally, REST/OpenAPI externally (grpc-gateway), mirroring VTAdmin's HTTP plus gRPC.
- Frontend: React + TypeScript SPA, a component library, ECharts or uPlot for charts, React Flow for topology. No source gathered on these specific choices.

Proposed screens and endpoints (modeled on Couchbase; endpoint names are proposals)
- Screens: Dashboard/overview; Servers (nodes, state, services, server groups/racks, add/remove/failover); Shards map (shard x node matrix with primary/replica roles and sizes, under-replicated badge); Replication (topology graph, lag, slots); Rebalance (plan preview, progress, history reports); Failover (manual, auto settings); Ledger (verification status, last verified block, mismatches); Query workbench; Security (users, roles, certificates, audit); Settings (auto-failover timeout/maxCount, replica count, rebalance concurrency); Logs and alerts.
- REST: `GET /api/v1/cluster`, `GET/POST /nodes`, `POST /nodes/{id}/add`, `POST /nodes/{id}/remove`, `POST /nodes/{id}/failover` (hard or graceful), `GET /shards`, `GET /shards/{id}`, `POST /rebalance/plan`, `POST /rebalance`, `POST /rebalance/stop`, `GET /tasks`, `GET /tasks/{id}`, `GET /replication`, `PUT /settings/replica-count`, `PUT /settings/auto-failover`, `GET /ledger/status`, `POST /ledger/verify`, `POST /query`, `GET /audit`, `GET /alerts`. These follow Couchbase's `/pools/default`, `/controller/*` style but with resource-oriented naming.
- CLI: a `kingres-cli` that calls only the REST API, as couchbase-cli does. couchbase-cli details were not verified here.

Reuse and licensing
- Chronicle (Apache-2.0) is verified reusable only if you use Erlang. Patroni (MIT in background knowledge, unverified). pgwatch v5 (BSD-3-Clause, verified). Pigsty core (Apache-2.0 per official docs, conflict noted). ns_server: license must be read from its `LICENSE.txt`. CockroachDB's code has had non-OSS license terms [background, unverified], so study but do not copy. VTAdmin and TiDB Dashboard are the closest architectural analogues but their licenses were not verified.

### Gaps
- No evidence collected on: Go vs Rust vs Erlang choices in existing projects, React Flow/D3/ECharts usage in existing consoles, auto-failover designs in Patroni (DCS, ttl, loop_wait), pg_rewind handling, Prometheus vs in-DB metrics choices of other projects, StackGres/CloudNativePG/Crunchy UIs, ledger/blockchain verification UIs.
- Couchbase Web Console screens, server groups, durability settings, XDCR and the Query workbench were not fetched, so they are not documented here.
- Nothing was marked outdated beyond: Citus 10.1/11 blog material is 2021-2022, TiDB Dashboard pages cited are v4.0 and archived v6.6, and Couchbase REST pages cited include 6.0 and 7.0 versions. Check the 8.0 docs before implementing.

<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Build KingresDB on extensions, not on a deep fork

**Short answer:** Build KingresDB as a thin fork of a stable PostgreSQL branch, with a small number of core patches and most of the sharding and ledger logic in extensions. The name "KingresDB" is fine from a trademark standpoint, provided the word "Postgres/PostgreSQL" and the Slonik logo do not appear in the name, domain or logo. The recommended base is `REL_18_STABLE` (PostgreSQL 18.7 is the latest GA release as of 2026-11-14), with PG19, whose GA is scheduled for 2026-11-19, as the next option ([PostgreSQL versioning policy](https://www.postgresql.org/support/versioning), [PG19 RC1](https://www.postgresql.org/about/news/postgresql-19-rc-1-released-3409/)). The historical evidence shows that deep sharding forks (Postgres-XL, Greenplum) fall years behind upstream, while Citus stays in sync because it is built as an extension ([Cloudberry vs Greenplum](https://cloudberry.apache.org/docs/introduction/cbdb-vs-gp-features), [Postgres-XL, Wikipedia](https://en.wikipedia.org/wiki/Postgres-XL)). For sharding, the recommendation is to rebuild the Citus architecture conceptually (not by copying code, because of the AGPL): a central catalog, a fixed and large number of hash buckets, a query path built on `planner_hook` and CustomScan, distributed transactions with 2PC, bucket movement with logical replication, and per-group HA with Patroni. Every server hosts a writable primary and a client router, so reads and writes are accepted on any node while each bucket keeps a single writer. For the blockchain feature, "blockchain" has three different meanings. The recommendation is to first build tamper-evident ledger tables with an external digest (the Microsoft SQL Ledger pattern), then add external signing and anchoring, and to start multi-node consensus only with a specific consortium use case. For the panel, the Couchbase architecture (a local agent + an elected orchestrator + etcd used only for leader election + a single REST API; the shard map lives in PostgreSQL itself on the "map group") with a Vue interface and a Go backend is a suitable pattern, and no PostgreSQL sharding product with a Couchbase-like panel was found in the research notes; this is a real gap in the market. Everything ships as one installable package, and every node serves the same panel, as in Couchbase.

## Reading guide: fact, recommendation, and items that need verification

This document separates three kinds of statements. Statements that have a source (URL) are "documented fact". Statements marked **[Recommendation]** are the author's inferences and design decisions and have no direct source. Statements marked **[Needs verification]** are either background knowledge that was not confirmed in the research notes, or known gaps; they must be checked against code or documentation before being relied on. None of the "needs verification" items should be treated as fact. This document is not legal advice.

## Where the implementation departs from this document

This document is the research and the original design. Decisions taken while building are recorded as ADRs and design notes, and where they contradict a paragraph below, they win. The departures so far:

| Topic | This document proposed | What was built | Record |
|---|---|---|---|
| Failover engine | Patroni bundled in the package, with an etcd member embedded in `kingresd` | Standby management and failover inside `kingresd`, fenced by leases granted by a majority of the cluster's servers. Neither Patroni nor etcd is shipped | [ADR 0021](adr/0021-failover-in-kingresd.md), [design/ha-phase5.md](design/ha-phase5.md) |
| Manager leader election | Taken from etcd | The manager leader is the `kingresd` on the server that hosts the primary of the map group | [ADR 0018](adr/0018-manager-leader-follows-the-map-group-primary.md) |
| Where cluster state is kept | Catalog tables of the map group and etcd | A system database, `kingres_system`, on the map group | [ADR 0019](adr/0019-cluster-state-in-a-system-database.md) |
| Metrics | A bundled single-binary store (Prometheus or VictoriaMetrics) | No bundled store: `kingresd` keeps a short in-memory history for the panel's charts and exposes `/metrics` for an external Prometheus | [ADR 0020](adr/0020-no-bundled-metrics-store.md) |
| Panel framework | React | Vue 3 | [ADR 0017](adr/0017-vue-for-the-web-panel.md) |
| Licence of KingresDB's own code | The PostgreSQL License, as upstream | The GNU Affero General Public License, version 3 only. PostgreSQL's code keeps the PostgreSQL License | [ADR 0024](adr/0024-agpl-for-kingresdb-code.md) |
| PostgreSQL majors | Based on PostgreSQL 18, with a port to 19 on a new branch later | One source of every extension that builds and is tested against 18 and 19; a product branch per major with the same core patches and the same KingresDB directories. Both majors are built: the first release is made for 18 and 19 at the same time | [ADR 0023](adr/0023-one-extension-source-for-several-majors.md) |
| Package and unit name | `kingresdb`, `kingresdb.service` | Package `kingres`, unit `kingres.service`; client programs carry a `kingres-` prefix | [packaging/README.md](../packaging/README.md) |
| Internal traffic between servers | Decided when needed | An internal HTTP API with mutual TLS on its own port, under a cluster certificate authority created at cluster creation | [design/control-plane-3.md](design/control-plane-3.md) |
| Placement of standbys | Chained: every server hosts one primary and the standby of a neighbor | As proposed; details, including what a two-server cluster can and cannot do, are in the phase 5 design | [design/ha-phase5.md](design/ha-phase5.md) |

Paragraphs below that mention Patroni, etcd or a bundled metrics store are kept as the record of what was considered.

## Fork and rebrand: a stable branch, a limited patch set, and a name without "Postgres"

### Branch choice and mirror mechanics

PostgreSQL ships one major release per year, each release is supported for 5 years, and minor releases come at least every 3 months. Version 18 is supported until 2030-11-14, and version 14 reached EOL on 2026-11-12 with its final minor release, 14.25 ([versioning policy](https://www.postgresql.org/support/versioning)). PG19 is at RC2 and its GA is scheduled for 2026-11-19 ([PG19 RC1](https://www.postgresql.org/about/news/postgresql-19-rc-1-released-3409/)). The official clone URL is `https://git.postgresql.org/git/postgresql.git`, and github.com/postgres/postgres is a mirror ([Docs: git](https://www.postgresql.org/docs/current/git.html)).

**[Recommendation]** Start from `REL_18_STABLE`; stable branches receive only fixes and low-risk changes, whereas `master` means chasing unreleased changes. If the team can wait a few days, PG19 after GA gives the longest support horizon (about 2031). Suggested mechanics: a remote named `upstream` pointing at postgres/postgres; a pure mirror branch (fast-forward only) for each stable branch; KingresDB changes on a separate branch with periodic rebases onto minor tags (for example `REL_18_x`); new code in separate directories in the style of `contrib/`; and each core change as a small, documented commit so the patch series stays reviewable. The reason for preferring rebase: the Postgres-XL team reported that merging upstream master produced far more conflicts than expected, and they had to rebase onto a later commit first ([Working towards Postgres-XL 9.5](https://enterprisedb.com/blog/working-towards-postgres-xl-95)). This is an empirical reason, not official PostgreSQL policy; the primary sources that were accessible give no guidance on private branches ([Developer FAQ](https://wiki.postgresql.org/wiki/Developer_FAQ)). **[Needs verification]** The exact branch names (`REL_18_STABLE`, `REL_19_STABLE`) were not confirmed on github.com/postgres/postgres; check them with `git branch -r` after cloning. The existence of `REL_19_STABLE` is still unconfirmed, and the list of breaking changes in version 19 (release-19.html) was not read.

### License and trademark

The PostgreSQL License permits use, modification and distribution "for any purpose", provided the copyright notices, the license text and both disclaimer paragraphs are retained in all copies ([PostgreSQL License](https://www.postgresql.org/about/licence/)). The marks "Postgres", "PostgreSQL" and the Slonik logo are registered trademarks of the PostgreSQL Community Association of Canada. A factual statement that your software is "derived from PostgreSQL" is allowed without approval, but using these marks in a company name, product name or domain requires permission, which is granted "at their sole discretion" ([Trademark policy](https://www.postgresql.org/about/policies/trademarks)). The suggested attribution wording from the policy itself is: "Postgres, PostgreSQL and the Slonik Logo are trademarks or registered trademarks of the PostgreSQL Community Association of Canada, and used with their permission" ([Trademark policy](https://www.postgresql.org/about/policies/trademarks)).

**[Recommendation]** Keep the `COPYRIGHT` file untouched and add a copyright line for KingresDB; write "derived from PostgreSQL" in the documentation; do not use Slonik or the name Postgres in the domain or logo. The policy prohibits use "on competing products, that is, databases derived from PostgreSQL", yet the same document permits the phrase "derived from" as a factual statement; the interaction of these two clauses is ambiguous, and no source was found that clarifies it. **[Needs verification]** Before public release, have legal counsel review this clause.

### Rebrand checklist

No authoritative source was found for a list of rebrand points, so everything below is **[Recommendation] and [Needs verification]** by grepping the real tree. Likely places to change: `PACKAGE_NAME`/`PACKAGE_STRING` in `configure.ac` and `project()` in `meson.build` (both files carry the version; the Beta3 stamp commit in PG19 changed both, according to [this mirror](https://forger.sitiv.fr/open-dsi/postgres/src/tag/REL_19_BETA1)), the `version()` banner, the `--version` output, the service and installer names, and the documentation title. Suggested items to keep: the meaning of `PG_VERSION_NUM` and `PG_MAJORVERSION`, the `pg_config` output for PGXS, the ABI check in `PG_MODULE_MAGIC`, the names of the `postgres/psql/initdb/pg_dump` binaries, protocol version v3, and respect for `CATALOG_VERSION_NO` when changing the catalog. Reason: drivers and extensions detect capabilities from these. Expose the KingresDB version through a separate function (for example `kingres_version()`) and only append to `version()`. Which drivers parse `version()` strictly has not been checked. Babelfish shows that behavior changes (the wire protocol) can be made with added hooks instead of renaming the core ([Babelfish hooks](https://www.babelfishpg.org/docs/internals/postgresql-hooks)).

### The pattern of existing forks: a small patch set plus extensions

The data show one pattern: deep forks fall behind. Greenplum is based on PG 9.4; Apache Cloudberry 2.x is on PG 14 and the main branch is on PG 16, and the upgrade from 14.4 to 16.9 brought in more than 5,700 upstream commits ([Cloudberry vs Greenplum](https://cloudberry.apache.org/docs/introduction/cbdb-vs-gp-features), [Cloudberry 2025 review](https://cloudberry.apache.org/blog/apache-cloudberry-2025-review.md)). YugabyteDB stayed on PG11 until v2025.1 and then moved to PG15, with a mixed-version online upgrade ([YugabyteDB moves beyond PG11](https://www.yugabyte.com/blog/yugabytedb-moves-beyond-postgresql-11/)). By contrast, OrioleDB, Babelfish and Percona pg_tde all have a small, published patch set plus extensions, and they aim to upstream their patches ([Supabase acquires Oriole](https://supabase.com/blog/supabase-acquires-oriole), [babelfishpg.org](https://babelfishpg.org/source/), [Percona PGConf EU 2025](https://www.postgresql.eu/events/pgconfeu2025/sessions/session/7191/slides/807/PGConf%20EU%202025%20Riga%20TDE%20slides.pdf)). Citus also reduced the cost of tracking PG versions by restructuring into an extension (secondary source: [Percona](https://www.percona.com/blog/your-datas-not-safe-until-its-tde-safe-heres-how/)). **[Recommendation]** This is where the first architectural decision is made: a limited, documented core patch set, with the sharding and ledger logic in extensions.

### Build and test tooling

Both meson and autoconf are still maintained. The meson documentation requires Meson and Ninja, plus Perl `IPC::Run` for TAP tests. The workflow is: `meson setup build`, `meson configure -Dcassert=true`, `ninja`, `meson test`, and `-Dtap_tests` for TAP ([Docs: meson](https://www.postgresql.org/docs/current/install-meson.html)). The developer FAQ recommends building with `--enable-cassert --enable-debug` ([Developer FAQ](https://wiki.postgresql.org/wiki/Developer_FAQ)). For Windows, a 2023 report on pgsql-hackers says the meson problems were only with MSYS2 and that MSVC worked well, but it may be outdated ([hackers thread](https://www.postgrespro.com/list/id/3e9b9c1934d02d7236bc880515058415@postgrespro.ru)). **[Recommendation]** On your Windows 11 machine, WSL2 (Ubuntu) or Docker is the lowest-risk everyday path, and the MSVC build should be a compatibility job in CI; CI should include a Linux matrix with `-Dtap_tests=enabled -Dcassert=true`, a Windows job, and a nightly job that merges the latest upstream stable tag and runs the full test suite. **[Needs verification]** The official Windows installation page for the current version returned a 404 error; the current Windows build requirements for PG18/19 have not been confirmed.

## Sharding: rebuild the Citus architecture conceptually and build the router path first

### The key decision: an extension, not changes to the executor

Citus is a pure extension and uses `planner_hook`, CustomScan, `ExecutorRun` hooks, xact callbacks (pre-commit, post-commit, abort), a utility hook for DDL and COPY, and a maintenance background worker (deadlock detection, 2PC recovery, cleanup) ([Citus distributed README](https://raw.githubusercontent.com/citusdata/citus/main/src/backend/distributed/README.md)). Citus 14.0 (February 2026) brought PG18 support, and the 14.x line covers versions 16 through 18 ([Citus 14.0](https://www.citusdata.com/updates/v14-0/)). According to PGXN metadata, Citus is licensed under AGPL-3 ([PGXN](https://pgxn.org/dist/citus/12.1.5)), so **[Recommendation]** rebuild the concepts and do not copy code, unless AGPL is acceptable to you.

The Timescale lesson is a warning: multi-node was deprecated in version 2.13 and removed in 2.14 (February 2024), because only about 1% of deployments used it and every new feature needed extra multi-node work ([MultiNodeDeprecation.md](https://github.com/jnidzwetzki/timescaledb/blob/4f096c0800a51b19e30f842733f75441c1752e47/docs/MultiNodeDeprecation.md)). The PostgreSQL wiki likewise says sharding forks lag behind community versions and require a lot of code change ([Built-in Sharding](https://wiki.postgresql.org/wiki/Built-in_Sharding)). The conclusion **[Recommendation]**: keep sharding in a layer that has the least entanglement with the core.

### Catalog and bucket mapping

Citus keeps the shard mapping in five catalog tables on the coordinator: `pg_dist_partition` (distributed tables, method, key, `colocationid`), `pg_dist_shard` (hash range), `pg_dist_placement` (the `groupid` object), `pg_dist_node` and `pg_dist_colocation`. The important point is that placements refer to a `groupid` and not a host, so failover changes only `pg_dist_node`; and `shouldhaveshards=false` drains a node during rebalance ([Citus metadata tables](https://docs.citusdata.com/en/stable/develop/api_metadata.html)). Couchbase uses 1024 vBuckets with CRC32, and the Cluster Manager sends the map to clients; each replica is a separate set placed on a different node ([Couchbase vBuckets](https://docs.couchbase.com/server/current/learn/buckets-memory-and-storage/vbuckets.html)).

**[Recommendation]** Choose a fixed, large number of buckets (the implemented default is 128, because each bucket is stored as one hash partition; see [design/shard-mvp.md](design/shard-mvp.md). The research notes suggested, for example, 1024 or more) when creating the cluster, because changing N means a complete resplit; that way rebalance is only the movement of whole buckets, not a rehash. The suggested catalog design (the names are suggestions):

```sql
CREATE SCHEMA kingres;

CREATE TABLE kingres.node (
  node_id int PRIMARY KEY, group_id int NOT NULL, host text, port int,
  role text CHECK (role IN ('primary','replica')),
  is_active bool DEFAULT true, should_have_shards bool DEFAULT true,
  server_group text           -- rack / zone
);

CREATE TABLE kingres.dist_table (
  relid regclass PRIMARY KEY, method text,      -- hash | reference | schema
  dist_column text, colocation_id int, replica_count int
);

CREATE TABLE kingres.bucket_map (
  colocation_id int, bucket int, group_id int NOT NULL,
  state text,                 -- active | moving | orphan
  PRIMARY KEY (colocation_id, bucket)
);

CREATE TABLE kingres.colocation (
  colocation_id int PRIMARY KEY, bucket_count int,
  dist_column_type regtype, dist_collation oid
);

CREATE TABLE kingres.task (
  task_id bigserial PRIMARY KEY, kind text, state text, -- pending|running|paused|failed|done
  payload jsonb, progress jsonb, error text, created_at timestamptz
);
```

The bucket function: `bucket = hash(key) % N`, using the `hashint4extended`/`hashtextextended` functions and a fixed seed, so that coordinator routing is consistent with a local `PARTITION BY HASH (MODULUS N, REMAINDER r)` on the worker. **[Needs verification]** The details of the partitioning hash function (the seed and `hash_combine64`) come from background knowledge; check them in `src/backend/partitioning/partbounds.c`. The Citus default of 32 shards has also not been confirmed. An alternative to Citus: shards are plain `tbl_<shardid>` tables, with a 32-bit hash space divided into N contiguous ranges. Consistent hashing with vnodes and range sharding (useful for range scans, but with a hotspot risk) are rejected because a fixed bucket count is the simplest model for rebalancing and replica placement.

### Map nodes: the data mapping inside PostgreSQL itself

**[Recommendation]** The source of truth for the data mapping is the catalog tables above (`kingres.node`, `kingres.dist_table`, `kingres.bucket_map`, `kingres.colocation`, `kingres.task`), kept on a "map group": one PostgreSQL primary with one or two synchronous replicas (quorum commit) and automatic failover with Patroni. This is the same role the coordinator plays in Citus (group 0). No separate Raft store is built for the map; etcd (which Patroni needs anyway) holds only leader election and node health. Reason: one fewer component to build and keep coordinated, and a map change happens in the same PostgreSQL transaction as the rest of the metadata.

Design rules (all **[Recommendation]**):

- **The unit of mapping is the bucket, not the row.** With N=1024 the whole map is about a thousand rows per colocation group, it is cached in the memory of every routing node, and each query only computes `hash(key) % N`. Row-to-node mapping (directory-based) is rejected because the mapping table grows as large as the data itself and every query needs an extra lookup.
- **One map group, several readable copies.** The map is synchronized to the other nodes (eventually all nodes) so that each one can route queries. Writes to the map happen only from the primary of the map group and are propagated to the others with 2PC; otherwise two nodes could hold two different maps.
- **Map versioning.** Every change gets a monotonic `map_version` number. The routing node sends its cached version to the worker along with the query; if the worker no longer holds the relevant bucket, it returns a "stale map" error, and the router reloads the map and retries the query.
- **Behavior during a map group failure.** Until the map group's failover completes, rebalance, distributed DDL and topology changes stay paused. Ordinary queries continue on the other routing nodes using the cached map.
- **A shard group's failover must not depend on writing to the map.** `bucket_map` points to `group_id`, not a host, so the bucket map does not change on failover. The router reads each group's current primary from etcd/Patroni and `kingres.node` is updated later; this way a simultaneous failure of the map group and a shard group does not lock up routing. **[Needs verification]** This behavior must be tested with a prototype and a simultaneous-failure test.

### Query path

Citus divides the planner into layers: fast-path router (equality on the distribution key, without `standard_planner()`), router (pruning to one shard), recursive planning (subqueries that cannot be pushed down, as intermediate results), pushdown (colocated joins), the logical multi-shard planner, and the combine planner ([Citus distributed README](https://raw.githubusercontent.com/citusdata/citus/main/src/backend/distributed/README.md)). Single-shard routing rewrites the SQL to the shard table and the worker runs it with the ordinary PG planner ([Citus query processing](https://docs.citusdata.com/en/stable/develop/reference_processing.html)). **[Recommendation]** The MVP should be only the router layer (`planner_hook` + CustomScan + a connection pool to the workers), followed by pushdown of colocated tables and aggregates, with the multi-shard optimizer layer deferred until it is needed. The `postgres_fdw` path plus partitioning needs less code but is weak at shard-aware pruning, parallel async execution over many shards, and distributed DML/2PC; **[Recommendation]** use it only for the first prototype. **[Needs verification]** The details of the adaptive executor (slow start of connections, `citus.max_adaptive_executor_pool_size`) were not confirmed.

### Distributed transactions: 2PC, without a global snapshot at the start

Citus says cross-node transactions are atomic, durable and consistent but do not have full distributed snapshot isolation; the maintenance daemon performs 2PC recovery and distributed deadlock detection ([Citus README](https://raw.githubusercontent.com/citusdata/citus/main/src/backend/distributed/README.md)). In PG, `PREPARE TRANSACTION` is the XA-style first phase and `max_prepared_transactions` defaults to 0, so it must be raised and the server restarted; a prepared transaction holds locks and, if orphaned, blocks VACUUM ([PG two-phase docs](https://www.postgresql.org/docs/current/two-phase.html)). In Citus mode, Patroni sets `max_prepared_transactions` to `2*max_connections` if it is not configured ([Patroni Citus](https://patroni.readthedocs.io/en/latest/citus.html)).

For global visibility, the "Global snapshots" patches (Clock-SI, from 2018-05-01) on pgsql-hackers did not make it into the core ([thread index](https://postgrespro.ru/list/thread-id/2385436)), and Postgres Pro Shardman does this with a CSN snapshot based on a physical clock in a patched PG ([Shardman architecture](https://postgrespro.com/docs/shardman/17/tutorial-arch)). **[Recommendation]** Three options in order of complexity: (a) 2PC only, with Read Committed in each shard and no global snapshot promise; (b) a Postgres-XL-style GTM/timestamp oracle, with a SPOF and added latency; (c) CSN/HLC with a core patch. Start with (a) and accept the documented anomaly "a cross-shard read may see a partially committed distributed transaction"; single-shard transactions are fully correct. (c) only after the rest has stabilized. **[Needs verification]** The Citus prepared-transaction naming pattern (`citus_<groupid>_<pid>_<xid>_<n>`), the `pg_dist_transaction` table and the reconciliation algorithm in `transaction_recovery.c` come from background knowledge, as does the wait-for graph for distributed deadlock.

### Bucket movement and rebalance

Citus performs movement with logical replication (the source is the publisher, the destination is the subscriber); the write lock is short and held only for the metadata flip, and movement without blocking reads/writes has been supported since Citus 11.0. Moves are sequential by default (`citus.max_background_task_executors_per_node` defaults to 1), and tables need a replica identity for this (a primary key or a unique key that includes the distribution column) ([Citus cluster management](https://docs.citusdata.com/en/stable/admin_guide/cluster_management.html)). Couchbase also balances its vBuckets; each transfer has backfill, book-keeping and takeover phases, transfers are sequential so that a stopped rebalance can resume, the concurrent-transfer limit is 1 to 64 with a default of 4, swap rebalance moves data only between the nodes involved when the number of added and removed nodes is equal, and every run has a JSON report ([Couchbase Rebalance](https://docs.couchbase.com/server/current/learn/clusters-and-availability/rebalance.html)).

**[Recommendation]** Rebalance design for KingresDB: the unit of work is a "move bucket" with one row in `kingres.task`; the steps are (1) create the destination shard and schema, (2) create a slot and publication at the source and a subscription with `copy_data=true` at the destination, (3) wait until the lag is near zero, (4) lock writes at the source, (5) drain the remaining WAL, (6) change `bucket_map` and synchronize it, (7) drop the old shard with a deferred cleanup record, because failures leave orphaned shards behind. Features: sequential and resumable execution, a concurrency cap (default 4), a JSON report for every run, a plan preview (similar to `get_rebalance_table_shards_plan`), Couchbase 7.6-style bucket/table ranking (0 to 1000), and a single cluster-wide rebalance lock. **[Needs verification]** Logical replication does not carry DDL, sequences and TRUNCATE the way it carries data (this is general knowledge), and the `shard_transfer_mode` options were not checked.

### HA, replication, DDL and backup

For Citus, Patroni treats each group as a separate Patroni cluster with its own `citus.group` (coordinator = 0), sets `synchronous_mode` to quorum if it is not configured, keeps `pg_dist_node` updated after a failover, and during a switchover temporarily pauses that group's traffic on the coordinator ([Patroni Citus](https://patroni.readthedocs.io/en/latest/citus.html)). **[Recommendation]** Each shard group is a Patroni cluster or equivalent; the coordinator is also made HA with Patroni, and the metadata is synchronized to all nodes so any node can plan a query, while DDL goes through only one path; quorum-type commit with `synchronous_standby_names = 'ANY 1 (...)'` to prevent loss of acknowledged commits; and 2PC recovery after failover. **[Needs verification]** The fencing design and the persistence of in-doubt transactions after failover come from background knowledge.

For DDL (**[Recommendation]**): resolve the relation in `ProcessUtility_hook`, run the statement, and repeat it on the shard tables of all workers inside a single 2PC transaction. Limitations (**[Needs verification]**, background knowledge): unique and foreign keys are allowed only when they include the distribution column or point to a reference table; global identifiers use per-node sequence ranges, Snowflake-style IDs or UUIDv7; global backup uses `pg_create_restore_point()` on every node after blocking distributed commits (the `citus_create_restore_point` pattern). Reference tables are copied to all nodes and writes to them use 2PC ([Citus README](https://raw.githubusercontent.com/citusdata/citus/main/src/backend/distributed/README.md)).

### Writes on any node: a primary on every server plus a router on every node

**[Recommendation]** This whole subsection is a design decision with no direct source. The goal is that a client can connect to any server in the cluster and both read and write ("multi-master" from the client's point of view), without giving up strong consistency.

**What "multi-master" means here.** Every server accepts writes, but each bucket still has exactly one writable primary at any moment; a write that arrives at a server that does not own the bucket is forwarded to the owner. There is no conflict resolution, because two nodes never write the same bucket concurrently. Conflict-resolving active-active replication, where several nodes write the same rows and conflicts are resolved afterwards (the pgEdge Spock / EDB PGD model listed in the examples table), is rejected for KingresDB: it gives up strong consistency, and it has no single commit order per shard, which the ledger's hash chain depends on.

**Part 1: every server hosts a primary (chained placement).** With N servers there are N shard groups. Server *i* runs one PostgreSQL instance that is the primary of group *i*, and one instance per configured replica that is a standby of a neighboring group (for `replica_count = 1`, the standby of group *i-1*). Each instance has its own port and data directory. The members of the map group are placed on servers in the same way. Rules:

- The primary and the standbys of one group never share a server, and when `server_group` (rack/zone) is set, never share a server group.
- Because metadata is synchronized to all nodes (phase 5), every primary can plan and route any statement: buckets it owns run locally, other buckets are forwarded to the owning group's primary through the connection pool, and multi-shard writes use 2PC as described above.
- When server *i* fails, Patroni promotes group *i*'s standby on its neighbor. That neighbor then runs two primaries and carries roughly double the write load until server *i* returns, so capacity planning must leave headroom. After recovery, the panel offers a "restore preferred primaries" action that switches each group back to its home server.

**Part 2: a client router in every agent.** `kingres-agent` on each server also listens on one well-known client port and proxies PostgreSQL connections. It does not parse SQL; it only chooses a target instance, and the SQL-aware routing is done by the primary it forwards to.

- The read-write port forwards to the local primary when it is healthy, and otherwise to any healthy primary taken from the etcd/Patroni state. A client therefore gets a writable session from any server, including one whose local primary is down or has been demoted.
- An optional read-only port forwards to a local standby, for reads that tolerate replication lag. Read-your-writes is not guaranteed on this port when replication is asynchronous.
- Clients can still list several servers in the connection string for client-side failover; libpq supports multiple hosts and `target_session_attrs` ([libpq connection docs](https://www.postgresql.org/docs/current/libpq-connect.html)).

**What can be done from where:**

| Operation | Any server, router port | Primary instance directly | Standby instance directly |
|---|---|---|---|
| Reads | Yes | Yes | Yes (may lag if asynchronous) |
| Writes, single-shard and multi-shard | Yes | Yes | No (standbys are read-only) |
| DDL on distributed tables | Yes, forwarded to the map group primary | Yes, forwarded to the map group primary | No |
| Topology changes and rebalance | Only through the manager API (panel or CLI) | Only through the manager API | Only through the manager API |

DDL and topology changes pause while the map group is failing over; reads and writes continue with the cached map. Before phase 5 (no metadata on the other nodes yet), all clients must connect through the map group primary.

**Costs.** A write to a bucket owned by another server takes one extra network hop. Two or more instances per server split that server's memory (`shared_buffers`, work memory, connection slots). Every primary keeps pooled connections to every other primary, so the number of connections grows with the square of the number of servers and the pool needs per-node limits. **[Needs verification]** Running several Patroni-managed instances on one host, the pool sizing, and the behavior of the router during a switchover must be confirmed with a prototype and failure tests.

## Blockchain: tamper-evident ledger tables first, consensus last

### Three meanings of "blockchain model"

The notes divide the term into three models: model 1, a centralized tamper-evident ledger (a hash chain or Merkle tree, with an external digest); model 2, a multi-node permissioned blockchain database (Raft/BFT consensus and deterministic execution); model 3, anchoring a digest to a public chain or a timestamp authority. **[Recommendation]** These form three separate phases, and the honest answer is that consensus is needed only when several mutually distrusting parties write to shared data without a trusted operator ([Wüst & Gervais](https://eprint.iacr.org/2017/375)). Microsoft also says its ledger allows verification of centrally hosted data "without the complexity and performance cost of network consensus" ([MS Learn ledger overview](https://learn.microsoft.com/en-us/sql/relational-databases/security/ledger/ledger-overview)). QLDB reached end of support on 2025-07-31 and AWS pointed to Aurora PostgreSQL with pgAudit and Database Activity Streams, which provides auditing but not cryptographic verifiability ([AWS DB blog](https://aws.amazon.com/blogs/database/migrate-an-amazon-qldb-ledger-to-amazon-aurora-postgresql/)). The reading of "low market demand" from this episode is the author's inference.

### Phase 1: a ledger table with a block structure similar to SQL Ledger

The documented reference pattern: in SQL Server/Azure SQL Ledger, the rows changed in each transaction are placed into a Merkle tree using SHA-256; the transaction roots become the root of a Merkle block; the block hash = hash(block root, previous block hash); and the digest is simply the hash of the latest block. There are two table types: updatable (with a history table) and append-only. A block is closed at 100K transactions, and `sp_generate_database_ledger_digest` closes the open block. Microsoft states explicitly that an admin with control of the machine can edit the files; the ledger does not prevent this, it guarantees detection ([ledger overview](https://learn.microsoft.com/en-us/sql/relational-databases/security/ledger/ledger-overview), [blocks view](https://learn.microsoft.com/sql/relational-databases/system-catalog-views/sys-database-ledger-blocks-transact-sql)). Oracle blockchain tables are insert-only with a hash chain, deletion is possible only after a retention period, and they have `verify_rows` and delegate signing; there are 32 system chains per instance ([DBMS_BLOCKCHAIN_TABLE](https://docs.oracle.com/en/database/oracle/oracle-database/23/arpls/dbms_blockchain_table.html)). The reason Oracle uses multiple chains (reducing contention) is an inference.

Implementation options in PostgreSQL. Documented: hash-chain triggers using `pgcrypto digest()` are simple and restrict UPDATE/DELETE writes with exceptions and grants, but a privileged user can rewrite rows, and the remedy is a periodic checkpoint to an external destination ([AppMaster guide](https://appmaster.io/blog/tamper-evident-audit-trails-postgresql)). A custom WAL resource manager uses `RegisterCustomRmgr` in `_PG_init`, IDs 128 to 255 (with `RM_EXPERIMENTAL_ID` for development; the id of a released module is registered on the PostgreSQL wiki list of custom resource managers), and the module must remain in `shared_preload_libraries` as long as its WAL records exist ([PG docs custom-rmgr](https://www.postgresql.org/docs/19/custom-rmgr.html)). **[Recommendation]** A comparison (all the mechanical statements below are **[Needs verification]** against the PG documentation): (a) row trigger: simple, but bypassed with `session_replication_role` or `DISABLE TRIGGER`, and it serializes writes at the chain tail; (b) `XACT_EVENT_PRE_COMMIT` to compute the transaction's Merkle root before commit, with block membership determined after commit based on commit order (commit LSN or a ledger sequence taken under a lock); (c) logical decoding output for an out-of-band block builder (asynchronous, and the ledger sits outside the transactional guarantee); (d) a custom rmgr or table AM in the fork, the strongest option: the ledger record is in the WAL, is replicated to standbys, and the chain state is reconstructed in crash recovery. Recommendation: for the MVP, path (b) plus a block-builder background worker; for a production product, forbid UPDATE/DELETE/TRUNCATE/DROP in the core, not only through triggers, and protect the ledger catalogs even from superusers, except through the ledger code path.

Table and API design **[Recommendation]**:

```sql
CREATE TABLE accounts (id bigint, owner text, balance numeric)
  WITH (ledger = append_only);          -- or ledger = updatable (history table + view)

-- hidden columns: ledger_txid, ledger_seq_in_tx, ledger_start, ledger_end

CREATE TABLE kingres.ledger_blocks (
  height bigint PRIMARY KEY, prev_hash bytea, tx_merkle_root bytea,
  block_hash bytea, ts timestamptz, hash_version smallint, signature bytea
);
CREATE TABLE kingres.ledger_tx (
  ledger_txid bigint PRIMARY KEY, block_height bigint, tx_index int,
  merkle_root bytea, user_name name, client_sig bytea
);

SELECT kingres.ledger_generate_digest();        -- close the open block and return (height, hash)
SELECT * FROM kingres.ledger_verify(digests);   -- recompute and report inconsistencies
SELECT kingres.ledger_inclusion_proof('accounts', pk);  -- Merkle path + block header
SELECT kingres.ledger_consistency_proof(d1, d2);        -- RFC 6962 style
```

Block structure: header = {height, prev_hash, merkle_root over the transaction hashes, timestamp, server signature, `hash_version`}. Canonical serialization (**[Recommendation]**): typed, length-prefixed encoding of the column values in catalog order, with a version tag, independent of `DateStyle`, `extra_float_digits`, `bytea_output` and locale; a hash must never be computed over a cast to text; leaf and node domain separation with the prefixes 0x00/0x01 as in RFC 6962. Block closing: by size (the pattern: 100K transactions in SQL Ledger), by time, or on a digest request. Compatibility bounds: version the hash format with `hash_version` so that `pg_upgrade`, dump/restore and logical replication do not break it.

### Phase 2: signing and external anchoring

The only thing that defeats an internal inserter is periodically anchoring the digest to an independent root of trust. Documented options: OpenTimestamps (calendar servers aggregate hashes into a Merkle tree and commit the root to Bitcoin; it gives only an upper bound on time, with an accuracy of about 2 hours) and RFC 3161 (a TSA signature, common in EU legal settings) ([IETF draft-fassbender-scitt-time-anchor](https://datatracker.ietf.org/doc/html/draft-fassbender-scitt-time-anchor-07)); Microsoft places the digest in an immutable blob, Confidential Ledger or WORM storage ([MS Learn](https://learn.microsoft.com/en-us/sql/relational-databases/security/ledger/ledger-overview)); pg_credereum writes block hashes into an Ethereum contract ([GitHub](https://github.com/postgrespro/pg_credereum)). For a third party to verify inclusion without trusting the server, the tile-based transparency log model (tiles of 256 hashes and a signed checkpoint) is the reference pattern ([transparency.dev](https://transparency.dev/articles/tile-based-logs/), [C2SP sunlight](https://www.github.com/C2SP/C2SP/blob/main/sunlight.md)).

**[Recommendation]** Phase 2: an ed25519 client signature over the canonical transaction payload, with public keys registered in the catalog and key rotation; a server signature over each block with the key in an HSM/KMS and the key-id in the header; scheduled export of the digest to at least two independent destinations (WORM such as S3 Object Lock + one of OpenTimestamps or a TSA) and a witness copy held by auditors (as a defense against split-view); and an independent verifier CLI that does not trust the DB. **[Needs verification]** ed25519 in standard pgcrypto (probably not supported; in the fork, libsodium or OpenSSL can be linked), the semantics of S3 Object Lock, and the cost of Ethereum gas were not checked. An honest limitation: a superuser, the files, restoring an older backup and replacing the binary cannot be prevented; the only defense is an external digest and verification off the host.

### Phase 3: multi-node with consensus

IBM Research (VLDB 2019) built a blockchain relational database by modifying PostgreSQL (about 4,000 lines of C): two designs, order-then-execute (OE) and execute-order-in-parallel (EOP), with SSI based on block height, a block containing a number, transactions, the previous hash, its own hash and the orderer's signature, stored in an append-only `pgBlockstore` file and a `pgLedger` catalog; the reported throughput is about 1,800 TPS for OE and 2,700 TPS for EOP versus about 800 TPS for serial execution, and BFT-SMaRt drops from about 3,000 to 650 TPS as orderers are added ([arXiv 1903.01919 (ar5iv)](https://ar5iv.labs.arxiv.org/html/1903.01919)). Their determinism restrictions: restrictions on time/random/sequence functions, a required `ORDER BY` on the primary key for `LIMIT/FETCH`, and a ban on `xmin/xmax` in WHERE and on `SELECT *`. No maintained public implementation of this prototype was found; Kwil (PostgreSQL + CometBFT) and Chromia/Postchain are live application-chain examples that run plain PostgreSQL next to a consensus engine ([kwil-db](https://github.com/kwilteam/kwil-db), [Chromia node docs](https://docs.chromia.com/intro/about/architecture/node)). CometBFT requires that the post-block state be a function only of the previous state and the block, and if validators holding more than one third of the voting power disagree on the app hash, consensus halts ([CometBFT ABCI requirements](https://github.com/cometbft/cometbft/blob/main/spec/abci/abci++_app_requirements.md)).

**[Recommendation]** Start phase 3 only with a specific consortium use case. First OE on a deterministic subset of SQL: the contract = allow-listed PL/pgSQL, volatile functions forbidden, `now()` replaced with block time, identifiers derived from block height + transaction index, ordering with Raft (CFT) and then CometBFT through an ABCI sidecar for BFT (embedding BFT inside the postmaster is much harder), and app hash = ledger head + a state checkpoint hash. Evaluate EOP only after OE works. Sources of nondeterminism that must be removed or virtualized **[Needs verification]**: `random()`, `gen_random_uuid`, sequences, unordered scans, floating point and locale-dependent collation (pinned ICU or the C collation), parallel query, hash-aggregate ordering, visibility that depends on VACUUM/xmin, extension versions; all nodes must have identical binaries and settings.

### Interaction with sharding

No source was found for a ledger on sharded PG; this design space is open. The only documented reference: Chromia scales with independent sidechains in clusters ([Chromia docs](https://docs.chromia.com/intro/about/architecture/node)). **[Recommendation]** Each shard group has its own chain and digest (no global serialization point), and the map group (coordinator) stores each shard's head hash in its catalog and builds an "upstream chain" or a Merkle tree over the shard head hashes so that a single global digest exists for anchoring. A 2PC transaction records its global ID and prepared hash in the block of every participating shard; an inclusion proof = the local Merkle path + the shard's block header + the inclusion of that header in the aggregate digest. A simpler alternative, but limited to the commit rate of a single node: a global chain with a single sequencer.

## Management panel: the Couchbase pattern with a local agent, an elected orchestrator and a single REST API

### The Couchbase reference pattern

Every Couchbase node has two processes: the babysitter (not cluster-aware; it starts and restarts processes) and ns-server. The Master Services module (the orchestrator) has only one active instance at any moment, determined by election, and manages failover, rebalance and adding/removing buckets; periodic node heartbeats are the basis of auto-failover ([Cluster Manager](https://docs.couchbase.com/server/current/learn/clusters-and-availability/cluster-manager.html)). Metadata is kept in Chronicle, an Apache-2.0-licensed Raft implementation ([couchbase/chronicle](https://github.com/couchbase/chronicle)). The Web Console and the CLI are both clients of one REST API on port 8091 (and 18091 with TLS), with endpoints such as `POST /controller/addNode`, `/controller/rebalance`, `/controller/failOver` and `/controller/startGracefulFailover` ([rebalance REST](https://docs.couchbase.com/server/current/rest-api/rest-cluster-rebalance.html)). The auto-failover safety rules in the 8.0 documentation: enabled by default with a timeout of 120 seconds (minimum 1 second via REST, maximum 3600), `maxCount` defaulting to 1, a quorum requirement, no automatic failover for a bucket without a replica, the counter reset by a rebalance, and auto-failover always being hard ([Automatic Failover](https://docs.couchbase.com/server/current/learn/clusters-and-availability/automatic-failover.html)). Stats are served from Prometheus with the `/prometheus_sd_config` discovery endpoint and the "External Stats Reader" role ([Prometheus setup](https://docs.couchbase.com/server/current/manage/monitor/set-up-prometheus-for-monitoring.html)).

### Proposed architecture (recommendation)

**[Recommendation]** This section is an inference from the patterns above. Three layers: (1) On each node, `kingres-agent` plays the babysitter role: start/stop, `pg_basebackup` for a new replica, promote and `pg_rewind`; the agent must not be a worker inside postgres because it would restart with postgres and could not start it. (2) `kingres-manager` with an elected leader (the orchestrator role) that is itself stateless: it reads and writes the desired state (nodes, bucket map, replica count, task status) in the catalog tables of the map group (see the "Map nodes" section), and it takes manager leader election and node health from the same etcd that Patroni uses. No separate Raft store is built for the map; the cost is that until the map group's failover completes, rebalance and topology changes stay paused. (3) API: the external REST API is defined first, by hand, in `kingres/api/openapi.yaml`, and both the Go server and the TypeScript client are written against that contract ([ADR 0016](adr/0016-go-backend-react-frontend-openapi-first.md)); an internal RPC layer between agents and the manager is decided when it is needed. The split into a web component and an API component follows the VTAdmin pattern ( two components, vtadmin-web and vtadmin-api, with HTTP and gRPC, [VTAdmin architecture](https://vitess.io/docs/24.0/reference/vtadmin/architecture/)). Operations should run as durable tasks with an ID, a state machine (pending, running, paused, failed, done), idempotent steps and per-bucket progress. Security: TLS, token/session, RBAC (admin, ops, read-only, stats-reader), and an append-only audit log. Stack: a Go backend, a Vue 3 + TypeScript frontend (the project owner chose Vue over the React originally proposed here; ADR 0017), charts with ECharts or uPlot, and a graph library for the topology view; no source was gathered for choosing these libraries. Time series go in Prometheus/VictoriaMetrics next to the server, not inside the DB itself (so that no load is added to the monitored database).

Engine data: `pg_stat_replication`, `pg_stat_wal_receiver`, `pg_replication_slots`, `pg_stat_database`, `pg_stat_activity`, `pg_stat_statements`, `pg_database_size`, `pg_prepared_xacts` (for pending 2PC, [PG docs](https://www.postgresql.org/docs/current/two-phase.html)) and the KingresDB catalog tables. **[Needs verification]** The column names of these views come from background knowledge and must be checked against the documentation of the base version. The Citus pattern for engine primitives: `citus_shards`, `citus_stat_activity`, `citus_lock_waits`, `get_rebalance_table_shards_plan` ([Citus metadata](https://docs.citusdata.com/en/stable/develop/api_metadata.html)); Citus has no user interface, so the SQL layer is all there is. KingresDB should ship similar views along with the panel.

### One package, and a panel on every node

**[Recommendation]** This subsection is a design decision. The goal is the Couchbase experience: install one package on a server and that node has everything it needs; open that server's own panel to create a cluster or join one. In Couchbase, every node runs the same cluster-manager processes and the Web Console and CLI are both clients of the node's REST API (see "The Couchbase reference pattern" above); CockroachDB likewise serves its DB Console from every node.

**What the package contains.** One package (named `kingres` as built), built as a `.deb` and a Docker image; an `.rpm` is not built, see [the limitations](guide/limitations.md) (Linux first; a Windows server package is out of scope for the first releases, and development on Windows uses WSL2 or Docker):

- The PostgreSQL fork binaries and the `kingres_shard` and `kingres_ledger` extensions.
- `kingresd`, one Go binary that contains the agent (instance lifecycle), the manager, the client router, the REST API, and the web panel as embedded static files. `kingres-agent` and `kingres-manager` in the architecture above are roles inside this one binary, not separate installs.
- The standalone ledger verifier, and `kingres-cli` (planned; it is not built yet: `kingres/control-plane/cmd` has only `kingresd` and `kingres-watchdog`).
- *(Not built; superseded by [ADR 0021](adr/0021-failover-in-kingresd.md).)* An etcd member embedded in `kingresd`, so that no separate etcd install is needed; etcd publishes a Go package for embedding a server ([etcd embed package](https://pkg.go.dev/go.etcd.io/etcd/server/v3/embed)).
- *(Not built; superseded by [ADR 0021](adr/0021-failover-in-kingresd.md).)* Patroni with its Python runtime, bundled inside the package. The alternative is to implement failover natively in the agent and drop the Python dependency; that is more work and is only worth doing if bundling Python proves to be a packaging problem.
- *(Not built; superseded by [ADR 0020](adr/0020-no-bundled-metrics-store.md).)* A single-binary metrics store (Prometheus or VictoriaMetrics) for the panel's charts.

One systemd unit (`kingres.service` as built) runs `kingresd`, which starts and supervises the local PostgreSQL instances. The node exposes one panel/REST port, one read-write client port and one optional read-only client port; instance, etcd and metrics ports are internal.

**First run.**

1. After installation the service starts in an "uninitialized" state: no PostgreSQL instance exists yet, and the panel shows only a setup wizard.
2. The administrator opens the panel on that server and chooses one of two paths.
3. **Create a new cluster:** set the administrator account, data paths and memory quotas, and choose the features to enable (sharding, ledger, default replica count). The agent initializes the map group primary and the first shard group primary and starts a single-member etcd.
4. **Join an existing cluster:** enter the address of any existing node and the administrator credentials. The node registers with the manager leader, receives the cluster certificates and configuration, creates the primary of a new shard group and the standby of a neighboring group, and appears on the Servers page as "pending rebalance". As in Couchbase, adding a node and rebalancing are two separate steps; data moves only when the administrator starts a rebalance.

The same two paths are planned for `kingres-cli` as `kingres-cli cluster init` and `kingres-cli node join`; `kingres-cli` is not built, and the REST operations `initCluster` and `joinCluster` do the work.

**A panel on every node, with two scopes.**

- **This server.** Local instances and their roles, service status, disk, memory and CPU, local logs, local settings (data paths, memory quota), restart or stop of local instances, and "leave cluster". These pages work even when the node is standalone or cut off from the rest of the cluster.
- **Cluster.** The pages in the table below. They are identical from whichever node they are opened on, because cluster state lives in the map group catalog and etcd, not in the node serving the panel. A mutating cluster operation received by a node that is not the manager leader is forwarded to the leader, so there is no dedicated panel server and no single point of failure for management.

A single node is a valid deployment: a PostgreSQL-compatible server with its panel. Replicas and sharding can be added later by joining more nodes.

**Feature settings.** The Settings page lets the administrator enable the sharding extension and the ledger extension independently (the first enable requires a restart, because the extensions are loaded through `shared_preload_libraries`), and set the default replica count and synchronous or asynchronous replication. Each table is then plain, distributed, ledger, or both. The ledger cannot be turned off for a table once it is on, because that would defeat tamper evidence.

**Security of setup.** An uninitialized node must not be claimable by anyone who can reach its port: the wizard requires a one-time setup token written to a root-readable file at install time, and by default listens only on localhost until the token is used. Joining requires administrator credentials, and node-to-node traffic uses mutual TLS with a cluster CA created at cluster init.

**[Needs verification]** How embedded etcd membership should grow as nodes join (how many voting members, and learners for the rest), bundling Patroni with a Python runtime in `.deb`/`.rpm`, the licenses of every bundled component, the resulting package size, and the rolling-upgrade procedure (minor upgrades one node at a time after a switchover; major PostgreSQL upgrades are a separate procedure) all need to be confirmed before the packaging work starts.

### Databases, users and servers in the panel

**[Recommendation]** This subsection is a design decision. It defines the three management areas an administrator uses most: creating a database with its sharding, replication and ledger settings (the equivalent of creating a bucket in Couchbase), managing users and access, and seeing all servers with their join and sharding status.

**Databases page: create a database like a bucket.** The page lists every database with its size, bucket distribution, replication health, ledger status (last verified block) and activity. "Create database" opens one dialog with four groups of settings:

| Group | Settings | Can it change later? |
|---|---|---|
| General | Name, owner role, encoding and collation, connection limit | Owner and connection limit: yes. Encoding and collation: no |
| Sharding | Off or on; bucket count (default 128, see [design/shard-mvp.md](design/shard-mvp.md)) | Sharding can be turned on later. The bucket count is fixed at creation, as the number of vBuckets is in Couchbase |
| Replication | Durability level: asynchronous, synchronous (quorum), or synchronous with apply. The replica count is a cluster setting (decision 6) and is shown here read-only | Durability: yes. The replica count is changed in the cluster settings and is followed by a rebalance |
| Ledger | Off; opt-in per table; or on by default for every new table (append-only or updatable). Digest interval and, from phase 7, the anchoring target | It can be turned on later. It cannot be turned off for a table that already has it |

What happens on "Create": the manager runs one durable task that creates the database on every group, installs the `kingres_shard` and `kingres_ledger` extensions in it as selected, records the settings in a cluster-level registry, and applies the durability level. The durability level maps to PostgreSQL's `synchronous_commit` setting, which can be set per database ([PostgreSQL docs: synchronous_commit](https://www.postgresql.org/docs/current/runtime-config-wal.html#GUC-SYNCHRONOUS-COMMIT)). Tables that are not distributed live on one home group (by default the map group). Because the `kingres.*` catalog tables exist once per database, the registry of databases lives in a small system database on the map group:

```sql
-- in the kingres_system database on the map group
CREATE TABLE kingres.database (
  datname name PRIMARY KEY, sharding bool, bucket_count int,
  durability text,                      -- async | sync | sync_apply
  ledger_mode text,                     -- off | opt_in | append_only | updatable
  created_by name, created_at timestamptz
);
```

Each database has a detail view with its tables and their type (plain, distributed, reference, ledger), the distribution key, a "distribute table" action, and "drop database" behind a type-the-name confirmation. **[Needs verification]** `CREATE DATABASE` cannot run inside a transaction block, so it cannot be part of a 2PC; the task must be idempotent and clean up a partially created database. This must be checked against the base version.

**Security page: users and access.** One list of accounts, not two: panel accounts are PostgreSQL roles, and the panel signs a user in by authenticating against PostgreSQL.

- **Cluster roles** are memberships in four built-in roles: `kingres_admin` (everything, including users), `kingres_ops` (servers, rebalance, failover; no user management), `kingres_readonly` (view only) and `kingres_stats` (metrics only).
- **Database users**: create, edit, disable and drop roles; password and expiry; login and connection limit; role membership.
- **Access**: a grant matrix per database, schema and table that issues `GRANT` and `REVOKE`, with three templates (read-only on a database, read-write on a database, database owner).
- **Network access**: allowed client networks, TLS required or not (the `pg_hba.conf` rules, applied on every node), and client certificates.
- **Audit**: an append-only log of sign-ins and of every administrative action taken through the panel, CLI or REST API.
- **Ledger rights**: who may generate digests and run verification. No role, including `kingres_admin`, can turn the ledger off for a table.

Roles and grants are propagated to every group through the same path as DDL. **[Needs verification]** Role propagation must carry the stored password verifier rather than the plaintext password, and the exact mechanism must be checked. External identity providers (LDAP, OIDC) are out of scope for the first releases.

**Servers page: every server, joining, and sharding status.** One row per server with: address, status badge (LIVE, SUSPECT, DRAINING, DECOMMISSIONING, DEAD, or PENDING REBALANCE for a server that has joined but holds no data yet), server group, the instances it runs and their roles (for example "primary of group 3, standby of group 2"), the number of buckets it holds as primary and as replica per database, data size, replication lag, CPU, memory and disk, version and uptime. Actions: add server (enter the new server's address and its setup token; the same join as in the first-run wizard, started from an existing node), remove server (drain, then rebalance), failover, and "restore preferred primaries". A banner shows when a rebalance is needed or running, with its progress. Selecting a server opens the Shards page filtered to that server.

REST endpoints for these three areas (names are **[Recommendation]**): `GET/POST /databases`, `GET/PATCH/DELETE /databases/{name}`, `GET /databases/{name}/tables`, `GET/POST /roles`, `PATCH/DELETE /roles/{name}`, `GET/POST/DELETE /grants`, `GET/PUT /settings/network-access`.

### Pages and REST API

| Page | Content | Reference pattern |
|---|---|---|
| This server | Local instances and roles, service status, disk/memory/CPU, local logs and settings, leave cluster | Couchbase (per-node console) |
| Databases | List and create databases with sharding, replication and ledger settings; tables per database | Couchbase Buckets |
| Dashboard | Cluster health, counts of under-replicated and unavailable items, alerts | CockroachDB Cluster Overview |
| Servers | Node table with status badges (LIVE/SUSPECT/DRAINING/DECOMMISSIONING/DEAD), server group, add/remove/failover | Couchbase, CockroachDB |
| Shards | Bucket-by-node matrix with primary/replica role and size | Couchbase vBucket map |
| Replication | Topology graph, lag, slots | Patroni `/cluster` |
| Rebalance | Plan preview, progress, historical reports | Couchbase Rebalance |
| Failover | Manual and auto settings (timeout, maxCount) | Couchbase |
| Ledger | Verification status, last verified block, inconsistencies, digests | (new design) |
| Query workbench, Security, Settings, Logs | Queries, users/roles/certificates/audit, settings, logs | Couchbase |

Documented interface examples: CockroachDB groups nodes by locality, shows the states LIVE/SUSPECT/DRAINING/DECOMMISSIONING/DEAD, and considers a node dead after 5 minutes without liveness ([CockroachDB Cluster Overview](https://docs.cockroachlabs.com/docs/stable/ui-cluster-overview-page)); the TiDB Dashboard is served inside PD and has a Key Visualizer (a traffic heatmap) ([TiDB Dashboard](https://docs.pingcap.com/tidb/v4.0/dashboard-intro)). **[Recommendation]** Adopt all three patterns: a node table with badges, a heatmap for hotspots, and an under-replicated counter at the top level.

The Patroni REST API (`/cluster`, `/switchover`, `/failover`, `/reinitialize`, `/config`) is a usable pattern for HA operations ([Patroni REST API](https://patroni.readthedocs.io/en/latest/rest_api.html)). Proposed endpoints (the names are a **[Recommendation]**): `GET /api/v1/cluster`, `GET/POST /nodes`, `POST /nodes/{id}/add`, `POST /nodes/{id}/remove`, `POST /nodes/{id}/failover` (hard or graceful), `GET /shards`, `GET /shards/{id}`, `POST /rebalance/plan`, `POST /rebalance`, `POST /rebalance/stop`, `GET /tasks`, `GET /tasks/{id}`, `GET /replication`, `PUT /settings/replica-count`, `PUT /settings/auto-failover`, `GET /ledger/status`, `POST /ledger/verify`, `POST /query`, `GET /audit`, `GET /alerts`; plus `kingres-cli`, which calls only the REST API. **[Recommendation]** The Couchbase safety rules transfer directly: quorum, no automatic failover without a replica, a cap on the number of failovers with a reset after rebalance, and manual failover in ambiguous cases. **[Needs verification]** The actual Web Console pages, server groups, durability and the details of couchbase-cli were not checked; read the 8.0 documentation before implementing (some of the cited REST pages were for versions 6.0/7.0).

### Reusable code and licenses

Chronicle (Apache-2.0) is usable only if you use Erlang. pgwatch version v5.1.0 (16 March 2026) is under BSD-3-Clause ([pkg.go.dev](https://pkg.go.dev/github.com/cybertec-postgresql/pgwatch/v5)). The Pigsty license is reported inconsistently across sources (Apache-2.0 according to the official documentation, AGPLv3 on an older page) ([Pigsty license](https://pigsty.io/docs/about/license)). The ClusterControl license is also reported inconsistently ([PostgreSQL wiki Monitoring](https://wiki.postgresql.org/wiki/Monitoring)). The ns_server license was not visible on the fetched page; read `LICENSE.txt` before any use. **[Needs verification]** The licenses of VTAdmin, TiDB Dashboard and Patroni (MIT in background knowledge) have not been confirmed; for CockroachDB, study it only and do not copy.

## Existing examples and the 2026 landscape

### Sharding and distribution

| System | Type | Status (November 2026) | Lesson for KingresDB |
|---|---|---|---|
| Citus 14.x | Extension, AGPL-3 | Active, PG 16-18, 14.1 with a Major.Minor cycle ([v14.1](https://www.citusdata.com/updates/v14-1/)) | The main architectural reference |
| Azure elastic clusters | Managed Citus | GA around 2025, PG18 with Citus 14.0 ([Azure docs](https://docs.azure.cn/en-us/postgresql/elastic-clusters/concepts-elastic-clusters)) | The expectation model for managed-service users |
| Postgres Pro Shardman | Patched PG, commercial | Active, docs for 14/17/18; etcd + BiHA + CSN snapshot ([architecture](https://postgrespro.com/docs/shardman/17/tutorial-arch)) | Global snapshot design |
| YugabyteDB | PG layer over DocDB | PG15 from v2025.1 ([PG15 features](https://docs.yugabyte.com/stable/api/ysql/pg15-features/)) | The cost of upgrading a fork |
| Apache Cloudberry | MPP fork | 2.1.0 in April 2026, still incubating; the Greenplum repository is archived ([incubation report](https://cloudberry.apache.org/blog/apache-cloudberry-incubation-report-202605)) | Deep fork = falling behind |
| Postgres-XL | Core fork | Last release 2018, development stopped ([Wikipedia](https://en.wikipedia.org/wiki/Postgres-XL)) | GTM = SPOF; a failure example |
| TBase/OpenTenBase | XL fork, BSD-3 | 2026 activity not confirmed ([Tencent/TBase](https://github.com/Tencent/TBase)) | Study only |
| TimescaleDB multi-node | Extension | Removed in 2.14 (February 2024) | The maintenance tax of every feature |
| SPQR | External router | v3.0 (June 2026) with a distributed table and 2PC for DDL; used at Yandex ([GitHub](https://github.com/pg-sharding/spqr)) | Control plane pattern |
| PgDog | Rust proxy, AGPL-3.0 | Active, pre-1.0, resharding with logical replication ([docs](https://docs.pgdog.dev/)) | Resharding pattern |
| Multigres | External router, Apache-2.0 | Alpha, single shard; horizontal sharding is still in the future ([Supabase](https://supabase.com/blog/multigres-v0-1-alpha)) | Too early |
| Neki (PlanetScale) | Router + standard PG | Preview, each shard has one primary and at least two replicas ([Neki](https://planetscale.com/blog/introducing-neki)) | HA topology pattern |
| pgcat | Proxy | Experimental sharding; the repository is waiting/stagnant (secondary source) ([RepoPilot](https://repopilot.app/r/postgresml/pgcat)) | Ignore |
| pgEdge Spock / EDB PGD 6.x | Multi-master | Spock under the PostgreSQL license since September 2025; PGD 6.4.0 in May 2026 ([pgEdge](https://www.pgedge.com/blog/pgedge-goes-open-source), [PGD 6.4.0](https://www.enterprisedb.com/docs/pgd/latest/rel_notes/pgd_6.4.0_rel_notes/)) | HA/geo, not data sharding |
| Aurora Limitless / DSQL | Managed | GA ([AWS](https://docs.aws.amazon.com/AmazonRDS/latest/AuroraUserGuide/limitless-architecture.html)) | Sharded/reference/standard classes |

The common pattern of the routers: online resharding with logical replication without a core patch, and none of them has cross-shard ACID transactions with a global snapshot; this gap is exactly where a fork could add something. CockroachDB abandoned its open core in November 2024 and introduced an Enterprise license with a 10M dollar revenue threshold ([Cockroach Labs](https://www.cockroachlabs.com/blog/enterprise-license-announcement/)), which is a warning for open-core positioning. **[Needs verification]** The license columns and shard model of some cells in the research notes' comparison table (for example the licenses of Greenplum/Cloudberry, YugabyteDB, SPQR and Postgres-XL) came from background knowledge and were not included in this document. Not checked: EDB Advanced Server, openGauss, TDSQL-PG, Spanner PG, PL/Proxy, Pgpool-II, ShardingSphere.

### Ledger and blockchain

| System | Model | Status | Notes |
|---|---|---|---|
| Azure SQL / SQL Server Ledger | Model 1 | Commercial, the most complete documented design | Phase 1 pattern ([MS Learn](https://learn.microsoft.com/en-us/sql/relational-databases/security/ledger/ledger-overview)) |
| Oracle blockchain tables | Model 1 | Commercial | Retention and signing ([Oracle](https://docs.oracle.com/en/database/oracle/oracle-database/23/arpls/dbms_blockchain_table.html)) |
| immudb | Model 1, its own engine with the PG protocol | Active, v1.11.0 on 2026-04-23 with a focus on PG compatibility ([release notes](https://docs.immudb.io/master/releasenotes)) | Not PostgreSQL; license not confirmed |
| pg_credereum | Model 1+3, PG10 only | Effectively abandoned since 2018 (GPL-3.0, "not production-ready") ([GitHub](https://github.com/postgrespro/pg_credereum)) | Study the idea, do not use |
| IBM Blockchain Relational DB | Model 2, PG fork | Research prototype; no maintained public version found ([arXiv](https://arxiv.org/pdf/1903.01919)) | OE/EOP pattern |
| Kwil DB | Model 2, PG + CometBFT | Repository exists; PostgreSQL since v0.7 (March 2024) ([post](https://medium.com/kwildb/kwil-v0-7-open-source-postgresql-appchains-253470233827)) | 2026 activity and license not confirmed |
| Chromia / Postchain | Model 2 | Live; each dapp has its own chain ([docs](https://docs.chromia.com/intro/about/architecture/node)) | License and TPS not confirmed |
| BigchainDB | Model 2, NoSQL | Discontinued ([DB-Engines](https://db-engines.com/en/system/BigchainDB%3BDrizzle%3BLeanXcale%3BTigris)) | A failure example |
| Amazon QLDB | Managed ledger | End of support 2025-07-31 | AWS: Aurora PostgreSQL + pgAudit |
| Alibaba LedgerDB | Model 1 | VLDB 2020 paper; current availability not confirmed ([paper](https://www.vldb.org/pvldb/vol13/p3138-yang.pdf)) | bAMT, clue index |
| Dolt / DoltLite | Versioned SQL with a prolly tree, MySQL-compatible | Dolt 2.0, DoltLite beta in August 2026 ([DoltHub](https://dolthub.com/blog/2026-08-31-doltlite-beta)) | Structure-sharing Merkle pattern; not a blockchain |
| Veritas, FalconDB, BlockchainDB, GlassDB | Research | Papers ([Veritas CIDR](https://www.vldb.org/cidrdb/2019/veritas-shared-verifiable-databases-and-tables-in-the-cloud.html)) | Code status not found |

Analytical conclusion: the only live Model 2 products on PostgreSQL are application-chain platforms that pass transactions through their own layer rather than arbitrary SQL; the IBM approach is the only one that keeps PG's full transactional semantics, and it is only a research prototype (the inference of the notes' author). immudb's pivot to PG compatibility and the abandonment of pg_credereum suggest a market preference for model 1 with an external digest (inference).

### Management panel

Panel references: the Couchbase Web Console (REST on 8091, [UI docs](https://docs.couchbase.com/server/7.6/manage/manage-ui/manage-ui.html)); the CockroachDB DB Console (on every node); the TiDB Dashboard (inside PD); VTAdmin (separate API and web); Patroni (REST and patronictl without a UI); pgwatch and Pigsty for monitoring. None of the PG sharding systems reviewed has a Couchbase-like panel; the closest are the portals of Multigres, Neki and Azure. Not checked: the yugabyted UI, MongoDB Ops Manager, ScyllaDB Manager, StackGres, CloudNativePG, pgAdmin, PMM.

## Repository layout, roadmap and risks

### Proposed repository layout (recommendation)

This is the layout that was adopted when implementation started (2026-11-14); it replaces an earlier sketch that kept PostgreSQL in a `postgres/` subdirectory. The repository root is the PostgreSQL tree itself, so that rebasing onto upstream never touches a path prefix, and everything KingresDB adds is either an extension under `contrib/` or lives under `kingres/`. The authoritative description is [dev/repository-layout.md](dev/repository-layout.md).

```
<repository root>            # the PostgreSQL source tree, branch kingres-18 on tag REL_18_7
  contrib/kingres_shard/    # sharding extension (planner hook, CustomScan, catalog, node connections)
  contrib/kingres_ledger/   # ledger extension (capture trigger, Merkle and block hashing, protection hook)
  kingres/
    api/                     # openapi.yaml: the REST contract of the panel
    control-plane/           # Go: agent, manager, client router, REST API, CLI
    web/                     # Vue 3 + TypeScript panel
    verifier/                # standalone CLI for verifying digests and proofs
    packaging/               # the single kingres package: deb, Docker image, systemd unit, release script
    dev/                     # build and test scripts
    docs/                    # architecture.md, design/, adr/, dev/, research/, worklog.md
```

### Phased roadmap (recommendation)

**Phase 0 (foundation):** fork, mirror, branching, minimal rebrand, CI, COPYRIGHT and attribution, legal review of the name. **Phase 1 (sharding MVP):** extension skeleton, catalog, `create_distributed_table` with N buckets, node registry, router/fast-path planner with CustomScan and a connection pool. **Phase 2:** DML and DDL propagation, 2PC and a recovery worker, reference tables, scan and aggregate pushdown. **Phase 3 (panel MVP):** agent, manager (state in the map group catalog, leader election with etcd), REST API and read-only Dashboard/Servers/Shards pages, monitoring with Prometheus, the single installable package and the first-run wizard (create or join a cluster). **Phase 4:** bucket movement with logical replication, task engine and rebalance, adding/removing nodes from the panel, the Databases page (create a database with its sharding, replication and ledger settings) and the Security page (roles and grants). **Phase 5 (HA):** Patroni for each group (including the map group with a synchronous replica), metadata synchronized to all nodes, one primary per server (chained placement) and the client router in the agent so that every server accepts reads and writes, manual and automatic failover with safety rules, deadlock detection and restore points. **Phase 6 (ledger):** append-only and updatable ledger tables, the block builder, digest/verify/inclusion proof, the Ledger page. **Phase 7:** client/server signing, anchoring (WORM + OTS/TSA), tile publication and a standalone verifier, a per-shard ledger with an aggregate digest. **Phase 8 (optional):** global snapshot (CSN/HLC) and/or a multi-node ledger with Raft/CometBFT, only with a specific use case. In every phase, the deliverable must include TAP tests, documentation and a release tag. No sourced time estimate was found for any of these phases (even from the Citus or Yugabyte teams); therefore this document gives no person-month estimates.

### Scope of the first release, and the future roadmap (decided 2026-11-15)

The project owner decided what the first release contains: every phase of this roadmap except phase 8, that is phases 0 to 7, 2b and 9 to 13. Phases 14a, 14b and 14c (export and import of a table and of a database) were added to that list later. All of them are built; the state of every phase is in [../README.md](../README.md). The design of phase 7 (signing, anchoring, tile publication, the aggregate digest and the verifier), written against what of it already exists, is [design/ledger-phase7.md](design/ledger-phase7.md).

**Future roadmap, not part of the first release:** phase 8, a global snapshot across shard groups (CSN or hybrid logical clocks) and a multi-node ledger with consensus (Raft or CometBFT). It stays described in this document as it was researched, and is taken up after the first release if a concrete use case asks for it. Until then the guarantees are the ones stated for the earlier phases: no global snapshot (a statement that spans shard groups reads each group at its own snapshot), and one ledger chain per shard group with the aggregate digest of phase 7.

### Phases 9 to 12: table lifecycle, elasticity, distributed schemas, real-time analytics (added 2026-11-15)

A comparison with the documentation of Citus 13.0 (2026-11-15) showed what a user of that system would miss here. The project owner asked for every item below to become part of the roadmap, each with its action in the web console. Phases 7 and 8 keep their numbers; the new phases are numbered after them and do not depend on them. Every one of these phases is built under one standing rule ([ADR 0022](adr/0022-the-complete-scan-stays.md)): the distributed scan that moves rows to the originating node and evaluates the statement there stays, in every version, as the path that answers any query shape correctly. Pushdown, repartitioning, sketches and columnar storage are faster paths chosen when their conditions hold; none of them replaces it.

| Phase | What it adds | In the web console | Depends on | Design |
|---|---|---|---|---|
| 2b | Aggregate, sort and limit pushdown; colocated joins; the row router for multi-row `INSERT`, `INSERT ... SELECT` and `COPY FROM` | `EXPLAIN` in the workbench shows the path taken | phase 2a | [design/shard-phase2b.md](design/shard-phase2b.md) |
| 9 (table lifecycle) | Distributing a table that already has rows; turning a distributed or reference table back into an ordinary table; changing the distribution column, the bucket count or the colocation group of a table; partitioned tables as distributed tables | Tables of a database: distribute, undistribute and alter as tasks with progress | phase 4 (bucket movement), phase 2b (row router) | [design/shard-phase9.md](design/shard-phase9.md) |
| 10 (elasticity) | Splitting a bucket; rebalancing in the background with a strategy (by bucket count or by disk size) and a threshold; draining a server | Shards page: split; Rebalance page: strategy, threshold, automatic rebalance on or off, progress and history; Servers page: drain and weight | phase 4, phase 5 (standbys follow a move) | [design/shard-phase10.md](design/shard-phase10.md) |
| 11 (distributed schemas) | Schema-based sharding: every table of a schema lives in one shard group, schemas are placed and moved as units, new schemas can be distributed by default | Database settings: sharding by row or by schema; Schemas list with placement, size and move | phase 4, phase 9 | [design/shard-phase11.md](design/shard-phase11.md) |
| 12 (real-time analytics) | Joins on columns other than the distribution column (repartitioning); `MERGE` on distributed tables; approximate aggregates with pushdown (distinct count, top-n, percentiles); columnar storage; time partitioning with creation ahead of time and retention | Database settings: enable approximate aggregates; Tables: storage format, time partitioning and retention policy | phase 2b | [design/analytics-phase12-queries.md](design/analytics-phase12-queries.md), [design/analytics-phase12-storage.md](design/analytics-phase12-storage.md) |

Versions of `kingres_shard`: 0.4 is the fix round for the reviews of 0.2 and 0.3 (it changed SQL objects of an installed 0.3, so it needed a version of its own). The numbers after it are therefore one higher than the design documents say, which were written before that round: 0.5 is the high-availability part of phase 5 (the designs say 0.4), 0.6 is phase 2b (0.5), 0.7 phase 9 (0.6), 0.8 phase 10 (0.7), 0.9 phase 11 (0.8), and the changes phase 12 needs in it take the next free number. The numbers inside the design documents are not corrected one by one; this paragraph is the reference. Phase 12 adds three extensions: `kingres_sketch` (approximate aggregates, written from the published algorithms), `kingres_columnar` (a table access method written in this repository) and `kingres_timepart` (partition policies). No existing columnar or top-n implementation is bundled: the ones examined are AGPL, derived from AGPL code, or not table storage; the comparison with licences is in the storage design. None of the five designs needs a patch of the PostgreSQL core beyond `subdir` lines in `contrib/meson.build`. The revision numbers of the REST contract that the designs mention are placeholders; each set of additive changes gets its number when it is merged.

Points the designs leave to be settled when each phase is implemented, because they cross phase boundaries:

- After a split (phase 10) a bucket has its own modulus. Every place that computes a bucket as `hash % bucket_count`, in the code and in the phase 2b design, has to use the catalog lookup instead; a missed place gives a wrong answer without an error.
- `colocation.placement_epoch`, a counter, replaces the comparison of placement hashes of version 0.3 in every phase (it also closes review finding S4-01).
- `CREATE TABLE ... (LIKE ... INCLUDING ALL)` does not carry the access method. Bucket moves, the rebuild of phase 9 and the split of phase 10 create their copies that way and must set the access method of each leaf explicitly once columnar tables exist.
- A columnar table is moved with logical decoding of inserts (`kingres_columnar` 0.2) and is not split; a ledger table cannot be columnar; a distributed schema that holds a ledger table is pinned and cannot be moved or drained.
- The phase 2b design needs four corrections before it is implemented: the placement check (epoch instead of a map version in the fragment), `ON CONFLICT DO UPDATE` in a colocated `INSERT ... SELECT` that is shipped whole, an allow-list of extensions whose functions and aggregates may be shipped, and an explicit destination for the batch sender of the row router.
- Time partition policies run as tasks of the manager leader in `kingresd`; its scheduler runs each policy at its interval.

### Phase 13: configuration from the console (added 2026-11-15)

Requested by the project owner: the settings of PostgreSQL on the servers of a cluster, and the time zone the databases work in, are managed from the web console instead of by editing files on each server.

| Part | What it adds | In the web console |
|---|---|---|
| PostgreSQL parameters | Every parameter that the control plane does not own can be set at three levels: for the whole cluster, for a server group, and for one server. A narrower level overrides a wider one. The change is validated against what PostgreSQL says about the parameter, applied to every server it concerns as a task, and reloaded; a parameter that needs a restart is shown as pending until the instance is restarted | A Configuration page: search and categories, the effective value on every server with the level it comes from, differences between servers, pending restarts, an edit dialog with a preview of what changes where, history and revert |
| Time | The clock and time zone of every server are shown, with the difference between the servers' clocks and an alert when it is too large. The time zone of PostgreSQL (and of its log) is set for the cluster, and a database can have its own | A Time tab on the same page; the dashboard shows servers that wait for a restart, parameters that differ inside a group, and the largest clock difference |

The control plane does not set the system clock of a server: that needs the rights of the operating system's administrator and is the job of time synchronization (NTP or chrony). It measures and reports the difference. The design is [design/config-phase13.md](design/config-phase13.md). This phase depends on phase 3 only and is built independently of the sharding phases.

Not requested and therefore not scheduled: isolating one tenant on a bucket of its own (phase 10's split is the mechanism it would use), row-level security on distributed tables, statistics per tenant, distributed functions. The phases were implemented in the order 2b, 9, 10, 11, 12, after phases 4 and 5 were complete.

### Phases 14 and 15: export and import, and third-party extensions

**Phase 14 (export and import).** 14a exports and imports a table as CSV. 14b exports a whole database as one archive and validates an archive without touching a database. 14c creates a new database from an archive. All three are built and are part of the first release; the transfers are staged encrypted files that run as the role that asked ([ADR 0027](adr/0027-transfers-are-staged-encrypted-files-run-as-the-requesting-role.md)), and the ledger of an imported database starts a new chain ([ADR 0028](adr/0028-ledger-tables-are-carried-as-data-and-restart-their-chain.md)). The design is [design/data-export-import.md](design/data-export-import.md); the operator's view is section 13 of the [operator guide](guide/operator-guide.md#13-export-and-import-of-a-database).

**Phase 15 (management of third-party extensions).** Install and manage extensions that are not part of KingresDB from the console, on every server. It is designed and is not part of the first release: it is merged after 0.1.0, and nothing in this document or in the guide describes it as shipped. Extensions that duplicate a function of KingresDB, such as sharding, are refused.

### Risks

| Risk | Evidence | Mitigation (recommendation) |
|---|---|---|
| Annual rebase cost on each new major | The Cloudberry upgrade involved 5,700+ commits; Yugabyte stayed on PG11 for years | A minimal core patch set, logic in extensions, a stable base version |
| The maintenance tax of every feature on sharding | Timescale multi-node was removed | Automated tests of every feature on a cluster, a limited and documented support scope |
| Anomalies without a global snapshot | Citus also has no global snapshot | Documentation, routing transactions to a single shard, optional phase 8 |
| The coordinator as a SPOF and DDL bottleneck; rebalance and topology changes paused until the map group's failover completes | Inference | A synchronous replica and Patroni for the map group, a map cache on routing nodes, metadata synchronization to all nodes |
| A server that takes over a failed neighbor runs two primaries and about double the write load | Inference from the chained placement | Capacity headroom, the "restore preferred primaries" action, alerts in the panel |
| An uninitialized node's setup wizard is claimed by someone else on the network | Inference | One-time setup token, localhost-only until it is used |
| AGPL in Citus | PGXN | Conceptual rebuild, not copying |
| Trademark | PGCAC policy | The name KingresDB, no Slonik or Postgres in the name/domain, legal review |
| The ledger only detects, it does not prevent | MS Learn | External anchoring, a witness, prohibition in the core |
| Ledger and determinism in model 2 | The IBM restrictions, CometBFT | Phase 3 only with a specific use case |
| Dependence on a background knowledge base | Several sections above | Tick off the "Needs verification" list before starting each phase |

### Decisions

**Status (2026-11-14):** the project owner accepted the recommended option for all ten decisions below. The repository of decision 2 is the private repository `KingresDB/Kingres`. Each decision is recorded as an ADR in [adr/](adr/README.md).

The table is kept as the record of the options that were considered.

| # | Decision | Options | Recommendation | Needed before |
|---|---|---|---|---|
| 1 | Base PostgreSQL version | PG18 (`REL_18_STABLE`); PG19 after its GA | PG18 now, and rebase to PG19 once the tooling around it has caught up | Phase 0 |
| 2 | Repository | Hosting account or organization, public or private; one repository or two | One repository: the fork, with the extensions under `contrib/` and the control plane and panel in top-level directories | Phase 0 |
| 3 | License of KingresDB's own code | PostgreSQL License; Apache-2.0; AGPL; proprietary | PostgreSQL License, the same as upstream. Anything other than AGPL means no Citus code can be copied | Phase 0 |
| 4 | Build environment on the developer's Windows machine | WSL2 (Ubuntu); Docker | WSL2 for daily work, Docker for CI images | Phase 0 |
| 5 | What to build first after phase 0 | Sharding MVP; ledger; panel | Sharding MVP, following the roadmap. The ledger does not depend on sharding and can be moved earlier | Phase 1 |
| 6 | Replica count: per database or per cluster | Per cluster, with a per-database durability level; per database, using logical replication or separate node pools | Per cluster. Physical replication copies a whole PostgreSQL instance, so a per-database count needs logical replication, which is slower and does not carry DDL | Phase 4 |
| 7 | Failover engine | Patroni bundled in the package; failover implemented in the agent | Patroni bundled. **Changed later:** failover is implemented in `kingresd` ([ADR 0021](adr/0021-failover-in-kingresd.md)) | Phase 5 |
| 8 | Blockchain scope | Ledger tables and external anchoring only; also multi-node consensus | Ledger and anchoring only, unless there is a concrete consortium use case | Phase 6 |
| 9 | First operating system for the package | Ubuntu/Debian; RHEL family | Ubuntu 24.04 LTS and Debian first | Phase 3 |
| 10 | Panel accounts | PostgreSQL roles; a separate panel user store | PostgreSQL roles, one user list | Phase 4 |

**Note of 2026-11-20 on decision 3:** the project owner chose the AGPL, not the recommended PostgreSQL License: KingresDB's own code is AGPL-3.0-only since 2026-11-15 ([ADR 0024](adr/0024-agpl-for-kingresdb-code.md), which supersedes [ADR 0004](adr/0004-postgresql-license-no-citus-code.md) in its choice of licence). PostgreSQL's own code keeps the PostgreSQL License. Other statements of this document that recommend the PostgreSQL License for KingresDB's own code are superseded by that ADR in the same way.

Not a design decision but still open: a legal review of the name and of the trademark policy before any public release.

## Conclusion

Taken together, the evidence gives a simple conclusion: the main value of KingresDB lies in thin, maintainable added logic, not in a deep fork. The experience of Postgres-XL, Greenplum and Timescale shows that the real cost of any sharding fork is "tracking upstream", and the combination of sharding and a ledger is an open design space: no source was found for cross-shard inclusion proofs in PostgreSQL systems, so the aggregate-digest design above is a design proposal, not a proven solution, and it can become a product differentiator, provided it is validated early with a prototype.

The second conclusion concerns priority order. The market has shown that a centralized ledger with an external digest (SQL Ledger, immudb) has survived, while multi-node blockchain databases have either been abandoned or turned into application chains; so treat the ledger as the main product and consensus as an optional experiment. And because no Couchbase-like panel for sharded PostgreSQL was found in the sources, investing in a control plane with a task engine, a resumable rebalance and failover safety rules probably gives the greatest operational differentiation over Citus and the routers; this assessment is an inference, not the result of a market study.

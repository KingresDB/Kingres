<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# PostgreSQL Sharding / Distribution Landscape (as of November 2026) - input for KingresDB design

Method note: ~22 web searches/fetches. Search-tool answers are summaries; where a claim comes only from my background knowledge (no URL) it is placed under Inferences or Gaps and labelled "unverified". Dates of "last activity" were often not retrievable; those are flagged as gaps.

## 1. Extension-based solutions (Citus, Timescale multi-node, pgEdge/Spock, EDB PGD, Postgres Pro Shardman)

### Takeaway
Citus is the only widely-deployed, actively maintained sharding extension (14.x line, PG 16-18, Microsoft-backed, AGPL). Extension-based designs that tried to do sharding plus everything else (Timescale multi-node) were dropped for maintenance cost; multi-master products (Spock, EDB PGD) solve geo-distribution/HA, not horizontal data partitioning.

### Cited Findings
- Citus 14.0 (Feb 2026) headline is PostgreSQL 18 support (18.1); launch blog dated 2026-02-17 — [Citus v14.0](https://www.citusdata.com/updates/v14-0/), [blog](https://www.citusdata.com/blog/2026/02/17/distribute-postgresql-18-with-citus-14/)
- Citus 14.1 (May 2026) includes PG18 support and announces a move to a PostgreSQL-style Major.Minor cadence — [v14.1](https://www.citusdata.com/updates/v14-1/)
- Citus 14.2 / 13.4 paired minor releases; 14.x covers PG 16, 17, 18 — [v14.2](https://www.citusdata.com/updates/v14-2/)
- Citus license is AGPL-3 per PGXN package metadata (10.2.4, 11.3.0, 12.1.5) — [PGXN citus 12.1.5](https://pgxn.org/dist/citus/12.1.5). No source found on license history.
- Citus SIGMOD 2021 paper: Cubukcu, Erdogan, Pathak, Sannakkayala, Slot, "Citus: Distributed PostgreSQL for Data-Intensive Applications", pp. 2490-2502, DOI 10.1145/3448016.3457551; uses PostgreSQL extension APIs; identifies four workload patterns (multi-tenant, real-time analytics, high-performance CRUD, data warehousing) — [DBLP](https://dblp.org/rec/conf/sigmod/CubukcuEPSS21), [paper copy](https://paper-notes.zhjwpku.com/assets/pdfs/citus.pdf)
- Microsoft ships Citus as Azure Database for PostgreSQL "elastic clusters" (row-based and schema-based sharding, HA, read replicas, pooling); GA around Oct/Nov 2025 (sources disagree on month); PG18 support GA "powered by Citus 14.0"; IaC GA March 2026. This replaces the earlier Azure Cosmos DB for PostgreSQL / Hyperscale (Citus) brand — [Azure CN docs](https://docs.azure.cn/en-us/postgresql/elastic-clusters/concepts-elastic-clusters), [AZ523583](https://mc.merill.net/message/AZ523583), [AZ571047](https://mc.merill.net/message/AZ571047), [AZ558145](https://mc.merill.net/message/AZ558145), [Cosmos DB for PG updates](https://learn.microsoft.com/azure/cosmos-db/postgresql/product-updates)
- TimescaleDB multi-node: deprecated in 2.13, removed from 2.14 (Feb 2024). Stated reasons in the deprecation doc: only ~1% of deployments used it, maintenance complexity "a significant obstacle" (every new feature needed multi-node work), inherent architectural limits, and a strategic focus on single-node performance plus object-storage-based scaling. Standard PG replication unaffected — [MultiNodeDeprecation.md (fork copy of the doc)](https://github.com/jnidzwetzki/timescaledb/blob/4f096c0800a51b19e30f842733f75441c1752e47/docs/MultiNodeDeprecation.md), [2.14.0 release](https://github.com/timescale/timescaledb/releases/tag/2.14.0), [forum](https://timescale.com/forum/t/standard-postgres-replication/2595)
- pgEdge relicensed Spock (async multi-master logical replication, active-active), Snowflake Sequences and LOLOR under the PostgreSQL License in Sept 2025 (previously PgEdge Community License); Spock README lists PG 15-19 support (other pgEdge pages list 16-18) — [pgEdge goes open source](https://www.pgedge.com/blog/pgedge-goes-open-source), [Spock repo](https://github.com/pgEdge/spock), [OSFY](https://www.opensourceforu.com/2025/09/pgedge-enterprise-postgres-now-fully-open-source-and-scalable-for-mission-critical-workloads/)
- EDB Postgres Distributed (PGD) 6.x: BDR extension (multi-master mesh) + built-in Connection Manager (replaces PGD Proxy from v6); Raft for leader election/membership; nodes in groups/subgroups with a write leader. Releases: 6.0.1 (9 Jun 2025), 6.1.0 (19 Aug 2025), 6.2.0 (5 Dec 2025, PG18), 6.4.0 (26 May 2026) — [PGD architecture](https://www.enterprisedb.com/docs/pgd/6/overview/basic-architecture/), [6.4.0 notes](https://www.enterprisedb.com/docs/pgd/latest/rel_notes/pgd_6.4.0_rel_notes/), [6.2.0 notes](https://www.enterprisedb.com/docs/pgd/6.2/rel_notes/pgd_6.2.0_rel_notes/)
- Postgres Pro Shardman (commercial, Russian vendor): config in etcd, `shardmand` daemon manages PG instances; per-shard HA via BiHA leader/follower/referee groups with synchronous replicas only; distributed visibility via global snapshots based on physical clocks (CSN snapshots, `enable_csn_snapshot`), syncpoints (consistent LSN sets) for backups; available for versions 14, 17 (docs also have 18) — [architecture](https://postgrespro.com/docs/shardman/17/tutorial-arch), [BiHA](https://postgrespro.com/docs/shardman/18/biha-architecture), [backup](https://postgrespro.com/docs/shardman/17/shardman-backup), [product](https://postgrespro.com/products/shardman), [Habr](https://habr.com/ru/articles/932572)

### Inferences
- Citus remains the closest reference for "extension" architecture (coordinator metadata + distributed/reference tables + schema-based sharding + shard rebalancer, as in its SIGMOD paper) but AGPL licensing and the extension-API ceiling (no true global snapshot) constrain reuse; a fork can embed these ideas natively.
- Timescale's lesson: a sharding layer imposes a per-feature tax on every future feature; a fork inherits this tax unless sharding is designed into core abstractions.
- Shardman's CSN/physical-clock global snapshot is the most directly relevant "core patch" approach for atomic visibility on PG (unverified: it is a patched Postgres Pro Enterprise, not an extension).
- pg_shardman (the Postgres Pro precursor to Shardman) appears superseded by Shardman (unverified; not found in searches).

### Gaps
- Citus shard-rebalancer details (logical replication-based online moves, `rebalance_table_shards`), reference-table/schema-based sharding specifics, and exact license history: the Citus 14 blog fetch failed (ECONNRESET); docs.citusdata.com was not fetched. From background knowledge (unverified): Citus was AGPL then fully open-sourced in 2019 after Microsoft acquisition, and uses 2PC with distributed deadlock detection, no global snapshot.
- Microsoft's long-term commitment to Citus: only indirect evidence (continued 14.x releases, Azure elastic clusters built on it).
- pg_shardman status and last-activity date; Shardman 2026 release dates and license terms (commercial assumed).

## 2. Core forks (Postgres-XC/XL, TBase/OpenTenBase, Greenplum/Cloudberry, openGauss, YugabyteDB, CockroachDB, Spanner PG)

### Takeaway
The GTM/coordinator/datanode lineage (XC -> XL -> TBase -> OpenTenBase) survives mainly in Chinese-vendor forks; Postgres-XL itself is dead. Cloudberry is the live open-source MPP successor to Greenplum. YugabyteDB is the only mature open-source system that reuses real PG query-layer code atop a distributed Raft storage layer.

### Cited Findings
- Postgres-XL: final release 2018, development stopped shortly after, website offline (Wikipedia; no formal EOL notice found) — [Wikipedia](https://en.wikipedia.org/wiki/Postgres-XL)
- XC/XL model: Coordinators hold only metadata, Datanodes hold data, GTM provides global transaction IDs/snapshots; GTM is a SPOF mitigated by a GTM-Standby — [Postgres-XC architecture docs](https://postgres-xc.sourceforge.net/docs/1_2/tutorial-arch.html), [XC internals](https://postgres-xc.sourceforge.net/docs/1_0/xc-overview.html)
- TBase (Tencent) is based on Postgres-XL; OpenTenBase is its open-source successor with coordinator/datanode/GTM nodes, BSD-3-Clause; uses `pgxc_ctl` for deployment — [Tencent/TBase](https://github.com/Tencent/TBase), [OpenTenBase mirror](https://gitee.com/weiqpzs/OpenTenBase). No 2026 activity confirmed.
- PostgreSQL wiki on sharding states that sharding forks "lag behind community releases and require large code changes" — the stated motivation for the FDW-based approach — [Built-in Sharding wiki](https://wiki.postgresql.org/wiki/Built-in_Sharding)
- Apache Cloudberry (Greenplum fork): still in the Apache Incubator; 2.1.0 released April 2026 (second Apache release); no graduation confirmed in May or Aug 2026 incubator reports; the open-source Greenplum repo is archived (date not found) — [May 2026 report](https://cloudberry.apache.org/blog/apache-cloudberry-incubation-report-202605), [Incubator Aug 2026](https://cwiki.apache.org/confluence/spaces/INCUBATOR/pages/446070925/August2026), [history](https://cloudberry.apache.org/docs/introduction/cloudberry-history.md)
- YugabyteDB: YSQL rebased on PostgreSQL 15 starting with stable v2025.1 / preview v2.25; major-version YSQL upgrade path is documented (early access); v2025.2 reportedly Jan 2026 (low-reliability source) — [PG15 features](https://docs.yugabyte.com/stable/api/ysql/pg15-features/), [BusinessWire](https://www.businesswire.com/news/home/20250130293922/en), [major upgrade](https://docs.yugabyte.com/stable/manage/ysql-major-upgrade-local/)
- CockroachDB retired its free "Core" in Nov 2024; single Enterprise license, free for orgs under $10M revenue (self-attested), source-available — [Cockroach Labs](https://www.cockroachlabs.com/blog/enterprise-license-announcement/), [The Register](https://www.theregister.com/2024/08/19/cockroachdb_abandons_open_core/)

### Inferences
- XC/XL died because it was a deep fork of an old PG base (XL ended on 9.5/9.6-era code) with a centralized GTM bottleneck/SPOF; TBase/OpenTenBase survive on vendor backing but trail upstream (their base version was not verified).
- Yugabyte's cost of keeping a fork current (PG11 -> PG15 took until 2025, and a special upgrade procedure) is the best empirical data on how expensive "PG layer on custom storage" is; KingresDB should keep changes in PG isolated and upstream-mergeable.
- CockroachDB's license shift is a cautionary tale for "open core" positioning of a PG-compatible distributed DB.

### Gaps
- Yugabyte details (DocDB tablets, Raft per tablet, hash/range sharding, tablet splitting, hybrid logical clocks, license Apache 2.0 vs. features) came from background knowledge only; not cited. Fetch docs.yugabyte.com architecture pages.
- openGauss distributed, Tencent TDSQL-PG, Greenplum's last version/archival date, OpenTenBase 2026 releases and base PG version: not found.
- Spanner PostgreSQL interface: no search result obtained.

## 3. Proxy / middleware (PgDog, pgcat, SPQR, Multigres, Neki, ShardingSphere, Pgpool-II, PL/Proxy)

### Takeaway
2025-26 saw a wave of PG-native routers with online resharding via logical replication: SPQR (production at Yandex), PgDog (AGPL, fast-moving), PlanetScale Neki (preview) and Supabase Multigres (alpha, no horizontal sharding yet). None offers cross-shard ACID via a global snapshot; they rely on router-level scatter/gather and (SPQR 3.0) 2PC for DDL.

### Cited Findings
- SPQR (Yandex): 1.0.0 announced 2023-12-05 as first production-ready; at that time automatic shard rebalancing was not production-ready. Postgres Weekly (24 Jun 2026) lists SPQR 3.0 with distributed query support, distributed and reference tables, and 2PC for DDL/migrations across shards. Docker image has stable/nightly tags. Feb 2026 benchmark measured SPQR ~0.159 ms added latency — [PG news 1.0.0](https://www.postgresql.org/about/news/stateless-postgres-query-router-100-released-2759), [Postgres Weekly 654](https://postgresweekly.com/issues/654), [GitHub](https://github.com/pg-sharding/spqr), [benchmark](https://gitlab.com/postgres-ai/postgresql-consulting/tests-and-benchmarks/-/work_items/72)
- PgDog: Rust pooler/load balancer/sharder, AGPL-3.0; resharding via logical replication protocol (copy tables while sharding rows in flight); list/range/hash sharding; releases at least up to v0.1.61 (v0.1.46 blocks sharding-key-mutating UPDATEs on cascading FKs; v0.1.61 reports >10x faster resharding COPY reads and closed the repo to external PRs due to AI-generated submissions); new experimental query parser; Helm chart and ECS Terraform exist; still pre-1.0 — [GitHub releases](https://github.com/pgdogdev/pgdog/releases), [docs](https://docs.pgdog.dev/), [list/range sharding](https://pgdog.dev/blog/list-and-range-sharding), [v0.1.61](https://newreleases.io/project/github/pgdogdev/pgdog/release/v0.1.61), [v0.1.46](https://newreleases.io/project/github/pgdogdev/pgdog/release/v0.1.46)
- Multigres (Supabase): announced June 2025 as "Vitess for Postgres" (Sugu Sougoumarane); v0.1 open-sourced June 2026 under Apache 2.0; private alpha on Supabase platform (post 29 Oct 2026): 3 nodes across AZs, auto-failover, pooling, create-time only, not under SLA; not a PG fork; v0.1 is single-shard, horizontal sharding is future work. One third-party digest calls v0.1 pooling/failover production-grade, contradicting Supabase's own "not production-ready" — [announcement](https://supabase.com/blog/multigres-vitess-for-postgres), [private alpha](https://supabase.com/blog/multigres-private-alpha), [v0.1 alpha](https://supabase.com/blog/multigres-v0-1-alpha), [docs](https://supabase.com/docs/guides/database/multigres), [digest](https://releases.sh/collections/serverless-postgres/digest/2026-06-01)
- Neki (PlanetScale): sharded Postgres by Vitess team, not a Vitess fork, not a PG fork; now in platform preview (beta, no SLA): each shard is standard PG with 1 primary + >=2 replicas across AZs; router handles routing, resharding, online schema changes, upgrades via one connection string; can run unsharded — [introducing Neki](https://planetscale.com/blog/introducing-neki), [changelog](https://planetscale.com/changelog/neki)
- pgcat (PostgresML): Rust pooler; automatic sharding labelled "Experimental" (uses Postgres' own partition hash function); a May 2026 health card flagged repo "stale, last commit ~1y ago" (secondary source) — [README](https://raw.githubusercontent.com/postgresml/pgcat/main/README.md), [RepoPilot](https://repopilot.app/r/postgresml/pgcat)

### Inferences
- Treat pgcat as effectively unmaintained for sharding purposes; PgDog is its de facto successor in the Rust niche.
- Logical-replication-based online resharding (copy + CDC + cutover) is now the common pattern across SPQR, PgDog, Neki, Multigres; it needs no core patches, so KingresDB's core can offer it natively with better integration (shard map in catalog, atomic cutover).
- Proxy-only designs cannot offer atomic cross-shard visibility; this is the gap a fork can fill.

### Gaps
- ShardingSphere-Proxy for PostgreSQL, Pgpool-II, PL/Proxy (Skype; last-release date), "pg_dog" naming: searches returned nothing. Background (unverified): PL/Proxy is stored-procedure remote-call routing with hash partitioning and is largely dormant; Pgpool-II has a parallel mode that was removed long ago; ShardingSphere is Apache 2.0 with Java-based proxy.
- Exact SPQR 3.0 release date/changelog and its production adoption beyond Yandex.

## 4. Native PostgreSQL "built-in sharding" effort

### Takeaway
Core still has the building blocks (partitioning, postgres_fdw pushdown) but not a global transaction/snapshot manager, atomic commit via FDW, parallel/async shard dispatch completeness, or shard management; the wiki page itself has not been updated since March 2018.

### Cited Findings
- Proposed approach: FDW + declarative partitioning, minimal core change; modelled on XC/XL/Citus — [Built-in Sharding](https://wiki.postgresql.org/wiki/Built-in_Sharding)
- Marked done/maybe done: postgres_fdw join/sort/update/delete pushdown (9.6), aggregate pushdown (planned PG10), partitioning overhaul/pruning (PG10) — same page
- Listed missing: parallel FDW requests; query dispatch/result gathering from partition catalogs; global transaction manager (atomic commit across shards, prepared transactions + crash cleanup, XA suggested); global snapshot manager (also needed for multi-shard backups); user API for creating/managing shards — same page
- Use cases of increasing difficulty: (1) read-only analytics on read-only shards, (2) non-aggregate reads, (3) reads on read/write shards need global snapshots, (4) read-write across shards needs both managers — same page; last edited March 2018

### Inferences
- Async append (PG14) and partition-wise join/aggregate (PG11-12) exist from my background knowledge (unverified here) and improve scatter-gather, but atomic cross-shard commit/visibility remain absent in core as of the sources found. No 2026 pgsql-hackers evidence was retrieved to confirm any change.
- Shardman's CSN snapshots and Postgres Pro's patches are the closest existing implementations of the missing piece.

### Gaps
- Current (2026) pgsql-hackers status of FDW 2PC / global snapshot patches (e.g. Masahiko Sawada's FDW atomic-commit thread, CSN snapshot proposals), PGConf.dev 2025/2026 talks: not found.

## 5. Cloud and other 2025-2026 developments

### Takeaway
Hyperscalers all converge on "PG-compatible frontend over a custom distributed layer": AWS Limitless (router + shards, distributed transactions), Aurora DSQL (OCC, disaggregated), Azure elastic clusters (Citus). Neon/AlloyDB are scale-up/storage-disaggregation rather than sharding.

### Cited Findings
- Aurora PostgreSQL Limitless: DB shard group of routers and shards behind one endpoint; router creates/manages the distributed transaction, atomic commit across shards; table types sharded / reference / standard (standard tables stored on a system-chosen shard per AWS docs; a third-party tutorial disagrees); built on Aurora PG 16.4 limitless — [AWS architecture](https://docs.aws.amazon.com/AmazonRDS/latest/AuroraUserGuide/limitless-architecture.html), [GA blog](https://aws.amazon.com/blogs/aws/amazon-aurora-postgresql-limitless-database-is-now-generally-available), [shard groups](https://docs.aws.amazon.com/en_en/AmazonRDS/latest/AuroraUserGuide/limitless-shard.html)
- Aurora DSQL: PG-compatible, optimistic concurrency control with repeatable-read snapshot isolation; conflicts detected at commit, apps must retry; compute/storage/IO disaggregated; feature gaps (third-party Nov 2025 reports no foreign keys, ~10k row transaction limit, possibly outdated) — [AWS concurrency docs](https://docs.aws.amazon.com/aurora-dsql/latest/userguide/working-with-concurrency-control.html), [Yugabyte comparison (vendor)](https://www.yugabyte.com/blog/aurora-dsql-compared-to-yugabytedb/)
- Neon shifted defaults to PG18 (June 2026 digest; secondary) — [digest](https://releases.sh/collections/serverless-postgres/digest/2026-06-01)

### Inferences
- Limitless' router-as-distributed-transaction-coordinator, with sharded/reference/standard table classes, maps closely to Citus and is the model users of managed PG will expect.

### Gaps
- AlloyDB, Neon architecture details, Limitless current PG version (17/18?), DSQL limit updates: not retrieved. Spanner PG interface: not retrieved.

## 6. Comparison table (shard key, routing, distributed transactions, rebalancing, HA)

Cells marked (bg) are background knowledge not verified by a source above.

| System | Build type | License | Status Nov 2026 | Shard model | Cross-shard txn | Online rebalance | HA |
|---|---|---|---|---|---|---|---|
| Citus 14.x | Extension | AGPL-3 | Active, PG16-18, Microsoft | hash distributed, reference, schema-based (bg) | 2PC (bg) | Logical-replication shard moves (bg) | PG replication/Patroni or Azure |
| Azure elastic clusters | Managed Citus | Proprietary service | GA 2025, PG18 | row + schema | as Citus | as Citus | built-in |
| TimescaleDB multi-node | Extension | n/a | Removed in 2.14 (Feb 2024) | hypertable chunks | - | - | - |
| Postgres Pro Shardman | Patched PG + tooling | Commercial (bg) | Active (14/17/18 docs) | hash partitions via FDW (bg) | CSN global snapshot, physical clocks | tooling (details gap) | BiHA + etcd |
| pgEdge Spock | Extension | PostgreSQL License | Active | none (full replicas) | async multi-master | n/a | multi-master |
| EDB PGD 6.x | Extension/distribution | Commercial | Active, 6.4.0 May 2026 | none (replicas; groups) | async + conflict resolution | n/a | Raft |
| Postgres-XL | Core fork | MPL (bg) | Dead since 2018 | hash/replicated | GTM | offline | GTM standby |
| TBase / OpenTenBase | Core fork (of XL) | BSD-3 | Unverified 2026 | hash/range/list (bg) | GTM | n/a | n/a |
| Greenplum / Cloudberry | Core fork MPP | Apache-2.0 (bg) | Cloudberry 2.1.0 Apr 2026, incubating; Greenplum OSS archived | hash/random/replicated distribution (bg) | segment 2PC (bg) | gpexpand | segment mirrors |
| YugabyteDB | PG query layer on DocDB | Apache-2.0 + commercial (bg) | Active, PG15 | hash/range tablets (bg) | Raft + HLC (bg) | tablet splitting (bg) | Raft per tablet |
| CockroachDB | Non-PG code, PG wire | Source-available enterprise (Nov 2024) | Active | range | Raft + HLC (bg) | automatic | Raft |
| SPQR | External router | check repo | v3.0 (Jun 2026), Yandex prod | key ranges, hash | 2PC for DDL | rebalancer via logical repl (bg) | external (Patroni) |
| PgDog | External proxy | AGPL-3.0 | Active, pre-1.0 | hash/list/range | limited | logical replication | external |
| Multigres | External | Apache-2.0 | Alpha, single-shard | planned | planned | planned | built-in |
| Neki | External router + std PG | Proprietary SaaS | Preview | hash (bg) | limited | online resharding | 1P+2R per shard |
| pgcat | Proxy | MIT (bg) | Stale | hash (experimental) | none | none | external |
| Aurora Limitless | Managed, PG-compatible | Proprietary | GA | hash sharded + reference | router-coordinated atomic | managed | Aurora |
| Aurora DSQL | Managed | Proprietary | GA | automatic | OCC/snapshot | automatic | multi-AZ/region |

## 7. Failures and lessons (summary)
- Postgres-XL: dead since 2018 (deep fork falling behind upstream, GTM SPOF) — [Wikipedia](https://en.wikipedia.org/wiki/Postgres-XL), [wiki](https://wiki.postgresql.org/wiki/Built-in_Sharding)
- TimescaleDB multi-node: 1% adoption, per-feature maintenance tax — [doc](https://github.com/jnidzwetzki/timescaledb/blob/4f096c0800a51b19e30f842733f75441c1752e47/docs/MultiNodeDeprecation.md)
- Greenplum OSS archived; community moved to Cloudberry — [Cloudberry history](https://cloudberry.apache.org/docs/introduction/cloudberry-history.md)
- pgcat sharding stagnated — [RepoPilot](https://repopilot.app/r/postgresml/pgcat)
- CockroachDB abandoned open-source Core — [Register](https://www.theregister.com/2024/08/19/cockroachdb_abandons_open_core/)

## 8. Suggested reference architectures for KingresDB (opinion, derived from above)
1. Citus (coordinator metadata, distributed/reference/schema-sharded tables, shard rebalancer) — best match for scope; adopt concepts natively; check AGPL before copying code.
2. Postgres Pro Shardman (CSN global snapshots on physical clocks, etcd config + BiHA HA, syncpoint backups) — blueprint for the atomic-visibility feature missing from core.
3. YugabyteDB (PG layer over Raft tablets, tablet splitting) — blueprint if choosing storage-level distribution rather than PG heap shards; also the cautionary tale on PG upgrade cost.
4. Vitess-style routers (SPQR/PgDog/Neki/Multigres) — blueprint for the control plane: shard map, online resharding via logical replication, router, and a management UI (Neki: Insights, branching; no Couchbase-like panel found in any system surveyed).

### Gaps (overall)
- No source found for a Couchbase-style admin panel in any PG sharding product; Multigres/Neki/Azure portals are the nearest.
- Multiple background-knowledge cells in the table (bg) need verification before the design document relies on them.

<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Limitations of the first release

Read this before you decide what to run on KingresDB. Each item is a boundary that the design or a review stated, not an accident. Sections marked **To be checked when `<feature>` is merged** depend on work that was not on the product branches when this page was written.

## Consistency

- **No global snapshot.** A statement that spans shard groups reads each group at its own snapshot. Two-phase commit makes a transaction commit on all nodes or on none, but each node commits at its own moment, so a reader can see a transaction's effects on one group before it sees them on another. In `READ COMMITTED` every scan node of one statement has its own snapshot, so one statement that scans two distributed tables can see the same node in two states. A read that must see one state of several tables should join them on the distribution key (a join of colocated tables is routed to one group). `SERIALIZABLE` is serializable per node only. A global snapshot is **phase 8**, the future roadmap, and is not part of this release ([../architecture.md](../architecture.md), "Scope of the first release").
- **One ledger chain per shard group**, with the aggregate digest of phase 7 over the chains. There is no multi-node ledger with consensus (also phase 8).
- **The distributed scan is slow for large tables by design: it moves rows.** It is the fallback that answers every statement correctly ([ADR 0022](../adr/0022-the-complete-scan-stays.md)). Pushdown, partial aggregation, sorted streams and colocated joins are faster paths used when their conditions hold; `EXPLAIN (VERBOSE)` says why a statement was not pushed.

## Sharding

- Differences from a single node that pushdown makes visible: a cursor without `SCROLL` over a pushed ordered statement cannot scan backward; a pushed statement can fail with `KS001` while versions or placement change, and the client repeats it; a floating point aggregate can differ in the last digits; `SET ROLE` to a role that cannot log in breaks reads that span groups.
- Not pushed (the scan answers): grouping sets, a `FULL JOIN` with a reference table, aggregates without a combine function in a query that is not closed (`json_agg`, `jsonb_agg`, `range_agg`, `xmlagg` and others), `DISTINCT` or `ORDER BY` inside an aggregate, sub-selects, CTEs, views, local tables, tables of two colocation groups, row-level security, row locks.
- `UPDATE ... FROM`, `DELETE ... USING` and `MERGE` that read a distributed table and write another table are refused. A write that uses a function that is not built in cannot be shipped.
- The number of buckets of a database is fixed at creation (1 to 4096). A table is distributed by hash, or kept whole on every node as a reference table.
- Nodes must agree: the same collation settings and the same version of the extension. A node of another PostgreSQL major is refused.
- A cluster runs one PostgreSQL major, and there is no move between majors ([upgrade.md](upgrade.md)).
- A bucket move waits for long transactions at its switch, and writers of that group's tables wait for a short moment.

**To be checked when `m18` is merged:** splitting a bucket (a bucket then has its own modulus) and distributed schemas (a schema placed and moved as a unit) are phases 10 and 11 and are on that branch. What the design leaves out of them: plans of buckets do not move a schema; `check_cluster` has no schema checks yet; and a distributed schema that holds a ledger table cannot be moved.

**To be checked when `shard-p12` is merged:** joins on columns other than the distribution column (repartitioned joins) and `MERGE` on distributed tables (phase 12, in `kingres_shard`). Until then `MERGE` on a distributed table is refused and such joins are answered by the scan. The leftovers the design lists as optional (a moved preserved side, two moved tables) stay with the scan after the merge.

## High availability

- Failover and switchover are built into `kingresd`; they were reviewed in rounds and fixed, and the cluster tests pass on both majors. The hardening of the whole part (measured loops, a review of everything together) is not finished: treat failover as new.
- **The key of the certificate authority is not escrowed.** After a failover of the map group the new leader raises a critical alert until the operator restores `pki/ca.key` and `pki/revoked.json` from a backup of the old leader.
- Switching over the primary of the map group in a planned way, a hard failover of a server whose leader is alive, restoring preferred primaries, and draining a server that hosts the map group are not available on the product branches: **to be checked when `fo-r2wip` and `cp-1011` are merged**.
- Automatic failover is off by default. Two failures at once, and a failure during a rebalance, are not tested on a cluster.
- A cluster of one server has no failover; the phase 5 design says what two servers can and cannot do.
- A failover cannot restore data that no standby had. The dry run says whether writes may be lost, and a real request must accept that explicitly.

## The control plane and the console

- **Phase 12 has no control plane or console part yet.** The three analytics extensions exist, but there are no console dialogs or API operations for approximate aggregates, for the storage format of a table, for time-partitioning policies or for their schedule. They are used with SQL. The workbench does not summarize `EXPLAIN` as a path. Time partitioning of distributed tables is not built (`kingres_timepart` works on ordinary tables).
- Columnar tables cannot be distributed yet (the sharding extension does not carry the access method), cannot be moved or rebuilt, and a ledger table cannot be columnar.
- `kingres-cli` is planned and not built; use the console and the REST API.
- The console after 2026-11-15 and the Configuration page have no independent review, and the console's table-lifecycle pages are on `web-15` (**to be checked when `web-15` is merged**).
- A session belongs to the server that issued it.

## The ledger

- A check inside the database is relative to a digest that you keep elsewhere. The verifier proves what is in the files you give it and nothing else; it does not prove that a row was not changed later in the table or that no other rows exist ([../../verifier/README.md](../../verifier/README.md), "What it can and cannot prove").
- `TRUNCATE` is rejected on ledger tables; ledger tables must be ordinary permanent tables (no partitioned, inheriting, temporary or unlogged tables), and there is no conversion between append-only and updatable for an existing table.
- A database can be dropped with its ledger by every role that has the privileges of its owner or can `SET ROLE` to it; nothing inside the database prevents that. Publication and anchoring are what makes it visible.
- Anchoring to RFC 3161 authorities and to OpenTimestamps was tested against local fakes only. The scheduled audit of the publication and the check of anchors inside `startLedgerVerification` are not built.
- After a restore of the system database the aggregate log and the events cannot be re-derived: the operator accepts the loss, and the logs fork against what was published ([operator-guide.md](operator-guide.md), section 7).
- The signing key's passphrase and the private keys must be backed up by you, apart from each other.

## Columnar storage, sketches and time partitioning

- `kingres_columnar`: append-only (no update or delete of rows), B-tree indexes only, no TOAST, serial and parallel scans only, no vectorised execution, logical decoding covers inserts only. Up to about 8.8 x 10^12 rows per table.
- **The library is not preloaded by `kingresd`.** `kingresd` writes `shared_preload_libraries = 'kingres_shard,kingres_ledger'` and refuses a change of it. `CREATE EXTENSION kingres_columnar` works and an existing columnar table is read, but `CREATE TABLE ... USING columnar`, inserts, `COPY`, and so every conversion of `kingres_timepart` are refused. There is no supported way to turn it on yet; the planned change and its rolling-upgrade note are in [../../packaging/README.md](../../packaging/README.md), "The preload of `kingres_columnar`".
- `kingres_columnar` uses the experimental WAL resource manager id (128). A real id must be reserved before it is used for data that matters; a change of the id changes what replay accepts.
- `kingres_sketch` aggregates (distinct count, top-n, percentiles) are approximate by design and can be computed in part by the shard groups; the rewrite of `count(DISTINCT)` into them is not built.
- `kingres_timepart` works on ordinary tables. A `REPEATABLE READ` or `SERIALIZABLE` transaction whose snapshot is older than a conversion and has not read the table yet sees the converted partition empty.

## Packaging and distribution

- Debian package and container image, amd64, Ubuntu 24.04 only. No RPM, no arm64, no Windows server. No `-dbgsym` package, no `kingres-dev` package.
- The packages and images are not signed, there is no apt repository, no SBOM, and no published image registry. Checksums are in `SHA256SUMS`, not signed.
- No Patroni, no etcd and no bundled metrics store (by design: [../adr/0021-failover-in-kingresd.md](../adr/0021-failover-in-kingresd.md), [../adr/0020-no-bundled-metrics-store.md](../adr/0020-no-bundled-metrics-store.md)).
- No automated backup, no point-in-time recovery archive configuration, and no backup of the certificate authority key ([operator-guide.md](operator-guide.md), section 7).
- The compose cluster is for development; its test does not exercise removing a server, failover, switchover or rebalance.
- No NLS, GSSAPI, LDAP, PAM, JIT or procedural languages other than PL/pgSQL are built in.
- `kingres-19` is on the PostgreSQL 19 release candidate until the final release is taken in; nothing is tagged from it before ([install.md](install.md)).

## Outside the code

- KingresDB is derived from PostgreSQL. It is not PostgreSQL and is not endorsed by the PostgreSQL project. The name deliberately avoids the PostgreSQL marks, and a legal review of the name is still open ([../architecture.md](../architecture.md), the list of decisions).
- The licence of KingresDB's own code is the GNU Affero General Public License, version 3 only; PostgreSQL's code keeps the PostgreSQL License ([../adr/0024-agpl-for-kingresdb-code.md](../adr/0024-agpl-for-kingresdb-code.md)).
- The custody of the verifier's release signing key is the project owner's decision and is not made yet.

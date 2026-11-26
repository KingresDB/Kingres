<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Changelog

All notable changes of KingresDB are listed here, by release. A release is made for PostgreSQL 18 and PostgreSQL 19 at the same time from one source, and has one version (`kingres_version` in the top-level `meson.build`); see [docs/dev/releases.md](docs/dev/releases.md). The dated, detailed record of the work is [docs/worklog.md](docs/worklog.md), and the independent reviews are in [docs/reviews/](docs/reviews/).

The release script ([packaging/release/release.sh](packaging/release/release.sh)) takes the notes of a release from the section of this file whose heading is `## [<version>]`. It refuses a section that does not exist or is empty, and a section that has a `### Pending` part. The date in the heading of a release is set by hand, in the commit that is tagged; the script does not edit this file.

## [Unreleased]

Nothing yet.

## [0.1.0] - not released

The first release. KingresDB is a thin fork of PostgreSQL 18.7 and PostgreSQL 19.0 with sharding, a tamper-evident ledger, a control plane and a web console, and extensions for analytics. The REST contract is 0.34.0 (178 operations on 142 paths).

### Added

- **Bases.** PostgreSQL 18.7 (`kingres-18`, upstream tag `REL_18_7`) and PostgreSQL 19.0 (`kingres-19`, upstream tag `REL_19_0`).
- **Fork and packaging.**
  - A thin fork with a small register of core patches ([docs/dev/core-patches.md](docs/dev/core-patches.md)).
  - One source of every extension for PostgreSQL 18 and 19 ([ADR 0023](docs/adr/0023-one-extension-source-for-several-majors.md)).
  - KingresDB code is under the AGPL-3.0-only; PostgreSQL code keeps its licence ([ADR 0024](docs/adr/0024-agpl-for-kingresdb-code.md)).
  - A reproducible release build, a Debian package per major (`kingres_<version>+pg18-<revision>_amd64.deb`, `+pg19`), a container image per major (`kingres:<version>-pg18`, `-pg19`), a three-node development cluster in Docker Compose ([kingres/packaging](packaging/README.md)), and a release script.
- **Sharding** (`kingres_shard` 0.9).
  - Tables split into hash buckets over several servers; writes and reads on any server; two-phase commit with recovery; reference tables; distributed DDL; bucket moves with logical replication.
  - Standbys, `promote_node`, prepared transactions across a failover, distributed deadlock detection and coordinated restore points.
  - Pushdown: closed queries, partial aggregation, sorted streams with limit and joins of colocated tables run in the shard groups, and a row router handles multi-row `INSERT`, `INSERT ... SELECT` and `COPY FROM`. The complete scan stays as the fallback for every query ([ADR 0022](docs/adr/0022-the-complete-scan-stays.md)).
  - Table lifecycle: distribute a table that already holds rows, in blocking and online form; turn a table back into an ordinary table; change the distribution column, the bucket count or the colocation group; distribute user-partitioned tables; carry dependent views and storage parameters.
  - Elasticity: split a bucket, weights per server, plans by disk size.
  - A bucket move or split does not drop an old copy that objects of its owner depend on (a view, a materialized view, a foreign key, a function on its row type). The copy is left in place, the record of the move says what depends on it, `check_cluster()` reports `orphan_blocked`, and the next cleanup pass after the owner dropped the dependents finishes the move. The locks of a move or split are bounded and are never left behind, and the recovery worker does not undo a move or split that its session still runs.
  - Distributed schemas: a schema is placed and moved as a unit, schemas are part of the rebalance plan and the drain, and `check_cluster` checks them.
  - Analytics: repartitioned joins on columns other than the distribution column, `MERGE` on distributed tables, columnar tables that can be distributed and moved (blocking variants), approximate `count(DISTINCT)` through `kingres_sketch`.
  - Ledger tables are refused by `create_distributed_table` and `create_reference_table` (SQLSTATE `KS016`).
  - Own SQLSTATEs `KS017` to `KS022` for the refusals that the control plane tells apart: no receiving shard group, unreadable bucket sizes, a busy bucket, a pinned schema, a failed verification, and a call on a node that is not the map group primary. A split of a table without a primary key is refused with `0A000`. `kingresd` matches the code, not the wording (see "SQLSTATEs of the answers" in the [README of `kingres_shard`](../contrib/kingres_shard/README.md)).
- **Control plane** (`kingresd`, REST contract 0.34.0). Cluster creation and joining, servers, roles and grants, network access, databases, tasks, alerts, logs and metrics (also a Prometheus endpoint, `GET /metrics`, outside the REST contract; it needs a bearer token of a session with the `kingres_stats` role or the scrape token). Server groups (racks or zones), the audit log of sign-ins and administrative actions (merged across the servers) and a SQL workbench (a row limit, a time limit and cancel of a running statement) are part of the contract. The manager leader runs on the primary of the map group, the cluster state is in a system database, and certificates are renewed and revoked.
  - Starting an instance whose postmaster is still shutting down waits for the shutdown (up to the stop timeout) and then starts it, or reports an error.
  - Rebalance as a task with stop, resume and reports. Strategies by bucket count or disk size, a threshold, weights, an automatic mode (off by default; a run makes one move at a time), draining a server and splitting a bucket.
  - A drain hands the map group over by a planned switchover (contract 0.27.0).
  - A rebalance is held back, or refused with `group_degraded` unless `force` is sent, while a standby lags by more than `maxStandbyLagBytes`.
  - Distributed schemas: distribute, move and make local as tasks, and the sharding mode of a database. A rebalance and a drain plan and move schemas as items of the plan (contract 0.31.0): `kind` and `schemaName` appear in the plan, the progress and the report. A drain that would need to move a schema that cannot move (pinned by a ledger table, or a table without a key) is refused before a task exists.
  - Table lifecycle as tasks: distribute, make a table ordinary again, change its distribution, create a partition.
  - Distribute with related tables (contract 0.32.0):
    - `listRelatedTables` gives the plan: the ordinary tables that foreign keys link to a table (the whole closure, not through a table that is already distributed), the column each would get, the keys, and what would stop it.
    - `distributeTable` takes `relatedTables` and distributes them with the table in one task, colocated with it. The keys between the tables are taken out, each table is distributed in a step of its own, and the keys are added back `NOT VALID` and validated. A failure puts the keys back where it can and names the ones it could not. Only the blocking copy is used.
    - `Table.operation` in `listTables` and `getTable` says which long operation runs on a table, including operations that were started from SQL. The console shows it as a badge on the row and turns the conflicting buttons off. Design: [docs/design/shard-phase9.md](docs/design/shard-phase9.md).
  - Time partitioning of distributed tables (contract 0.25.0, `kingres_timepart` 0.3): policies, plans and runs on a user-partitioned distributed table. Partitions are created ahead on every server that holds a bucket, old leaves are converted to columnar storage, and expired partitions are dropped or detached.
  - Analytics (contract 0.18.0): approximate aggregates per database, conversion of a table or partition between row and columnar storage with a preview, time-partitioning policies with a plan, runs and a scheduler, and the preload of `kingres_columnar`. Conversion and policies are open for tables of superusers unless the cluster setting `analytics.allowNonSuperuserOwners` is on.
  - `pg_stat_statements` is preloaded by every instance and created in every database that `kingresd` makes (it is not installed in databases that already exist); `plpgsql` is installed in every such database. Its settings can be changed on the Configuration page.
  - Configuration of PostgreSQL parameters and of time from the console, with preview warnings.
  - Export and import of a table as CSV (contract 0.19.0): resumable uploads and downloads of sealed staged files, and an all-or-nothing import with a commit marker. It is off until the cluster setting `dataTransfer.enabled` is on.
  - Escrow of the key of the certificate authority (contract 0.26.0: `getCaEscrow`, `setCaEscrow`, `rotateCaEscrow`). It is off until switched on. When it is on, a new manager leader opens its own sealed copy and goes on issuing, renewing and revoking certificates. The Certificate authority tab of the Security page shows and changes it.
- **High availability.** Failover is part of `kingresd` and is fenced by leases.
  - Manual failover, automatic failover with a counter, planned switchover of an ordinary group and of the map group, restore of preferred primaries (it works also when the leader was the old primary of the restore), and a call that gives up a stuck bootstrap of the map group. The undo of the access rules of the old primary after a promotion carries the term that the promotion made.
  - A group lease is enforced only once a standby that a failover may use is ready, so a primary is not fenced while its first standby is being copied.
  - A planned switchover of the map group survives a clean stop that outlasts the lease of the task. A stop that outlasts that too is rolled back. A repeated clean stop of the old primary asks only for the proof that it stopped and does not release the lease again. A planned switchover must not run while servers of two versions run, because an older server does not honour the proof-only request (see the [upgrade guide](docs/guide/upgrade.md)).
  - Hard failover of the map group while its server is alive (contract 0.28.0): the leader judges the loss, fences its own primary of the map group and hands the lease to a standby's server. `abandonMapGroupBootstrap` takes `lostServerId` to declare a lost candidate. A hard failover that accepts a loss of data needs `kingres_admin` (contract 0.33.0).
  - A switchover ends when the held-back old server has forgotten the leader: state, unquiesce and unfreeze are accepted from the leader that a majority of ready members names.
  - Local recovery of a map group that is held back for good (contract 0.34.0, audit action `releaseMapGroupHold`): `kingresd -release-map-hold -lost <serverId> -accept-data-loss`, run on the server of the old leader while `kingresd` is stopped. It ends the hold when the candidate died and the cluster has only one standby of the map group, which leaves every REST call answering 503.
    - It refuses without both words, for the local server, while the data directory is in use, for another server than the one the hold was made for, and while a bootstrap is in progress.
    - It is audited and logged, and it can be repeated after a crash.
  - The declaration `lostServerId` carries the proof of the operator's session and goes only to the server whose primary of the map group is held back. That server checks it against the keys of the administrators that it cached on disk, so one member's certificate is not enough to forge it. The lost server must be seen dead before the release.
  - A server that runs the lease protocol does not start without the program `kingres-watchdog`. The setting `allow-no-watchdog` is for tests and development.
- **Database export and archive validation** (contract 0.29.0). `exportDatabase` writes one zip archive of a database, and `validateDatabaseArchive` checks an uploaded archive without touching any database.
  - The archive holds the structure from `pg_dump` of the same build (taken by the service role and normalised), a catalog document for the distribution, the access methods, the ledger and time-partition policies and the sequences, one CSV entry per table, and a manifest with SHA-256 sums.
  - The console has the dialogs and the report page. Design: [docs/design/data-export-import.md](docs/design/data-export-import.md).
- **Database import** (contract 0.30.0). `importDatabase` creates a new database from an archive made by `exportDatabase`. The guide: [section 13 of the operator guide](docs/guide/operator-guide.md#13-export-and-import-of-a-database).
  - The archive is read again from its bytes and every statement is classified before anything is made. A role that is dangerous, or that has a login member that can act as it, is refused. The free space of every server is checked against the expanded archive.
  - The structure is loaded as the importing role into a private database that only the service role can reach during its own steps. Every table the role made is locked and checked before the service role acts on it.
  - Distribution, ledger and time-partition policies are set again. The ledger starts a new chain, which is verified ([ADR 0028](docs/adr/0028-ledger-tables-are-carried-as-data-and-restart-their-chain.md)). Time-partition policies come back switched off.
  - The database gets its final name only at the end. A failed import removes the private database.
  - Every step before the commit point can be cancelled and is undone. A wait for a server that does not answer ends after 10 minutes, the import is undone, and the message says that a private database may stay on that server (it does not stop the removal of that server; drop it by hand if the server comes back).
  - The last step has one commit point, a mark in the task itself. After it the import only rolls forward: it is opened on every server and the marker is removed, with every error tried again, also after a restart of `kingresd` or a change of leader. It is never undone by the system.
  - A drain, a rebalance, the addition or removal of a server and a change of a weight or of `shouldHaveShards` are refused with 409 `operation_in_progress` while an import runs. A failover is not followed by the private database of an import, so an import fails, to be run again, when the primaries are not on their preferred servers at its start, or when a primary changed or a failover is unfinished at its last step.
  - An import that cannot go on is paused with a detail that starts with what to do. Repeating the import request (same upload, same owner, `database` if given) resumes any paused import of that upload. A cancel of a paused import is always accepted and ends the task as failed with code `cancelled`; for an import that is committed or possibly committed nothing is undone, and the message names the database and the servers where it is and is not open.
  - The janitor of the staged files keeps the upload of an import that is pending, running or paused. A drain and the removal of a server count the buckets of a kept private database and name it.
  - The console has the dialog and the report page. Design: [docs/design/data-export-import.md](docs/design/data-export-import.md).
- **Security hardening of the extensions.**
  - `kingres_shard`, `kingres_ledger` and `kingres_columnar` do not trust a schema that lets another role create objects, an extension catalog that is not the installed extension's own, or a registry that cannot be trusted. The ledger refuses `UPDATE`, `DELETE` and `TRUNCATE` of a table that looks like a ledger table while it cannot be one.
  - The check of the schema runs in every install and upgrade script. The search path of the privileged SQL of `kingres_shard` is fixed. What an extension moves into the schema `kingres` belongs to the bootstrap superuser. The schemas `kingres` and `kingres_*` are refused to other extensions.
  - `kingresd` raises the alert `extensionSchemaUntrusted` (contract 0.24.0) and reads nothing in a schema that it does not trust.
  - Copying extensions to an added server checks the package, the schema owner and the leader's version. An owner difference of an extension schema on an added server skips that extension and raises the alert `extensionSkipped` (contract 0.30.0), one for each skipped extension (subject `server:<id>|database:<name>|extension:<name>`). It clears when a later apply of the extension on that server succeeds or the database is dropped.
  - The service connections of `kingresd` use a fixed search path, and the marker tables of the transfers have row security and a checked tag.
- **Console.** A Vue 3 web console on every node: dashboard, servers, shards, rebalance, replication and failover, databases, tables, security, configuration, settings, ledger, workbench, alerts, tasks, logs and this server.
  - The table page has the lifecycle dialogs. The analytics part has approximate aggregates, the Storage column, partitions, policies and their runs, and the plan summary in the workbench.
  - The transfers panel has the export and import dialogs.
  - The Extensions tab of Configuration says whether `kingres_columnar` and `pg_stat_statements` are preloaded on every server.
  - The failover dialog of the map group can declare the candidate server lost (`lostServerId`).
- **Ledger** (`kingres_ledger` 0.3).
  - Append-only and updatable ledger tables with blocks, digests, inclusion proofs and verification; client signatures with a signature policy per table; a write guard.
  - The REST contract has 37 ledger operations (status, blocks, digests, verifications, settings, signing keys, the trust file, the publication, anchor targets and anchors, aggregates, bundles, client keys, upgrade of the extension and acknowledgement of a divergence).
  - Signing keys, a transparency log per chain and an aggregate log per database, published on a listener of its own.
  - Anchoring to a WORM object store, an RFC 3161 authority and OpenTimestamps (the RFC 3161 and OpenTimestamps paths were tested against local fakes only); bundles; an on-demand audit of the publication (the check `publication` of a verification); a rotation generation of the credentials of anchor targets.
  - The acknowledgement of a diverged chain, and the acceptance of a restored system database as a signed event ([ADR 0025](docs/adr/0025-restore-of-the-system-database-is-accepted-by-an-operator.md)).
- **Verifier** (`kingres-verify`). A standalone command, built from the Go standard library only, that checks digests, proofs, signed checkpoints, tile logs, consistency proofs, aggregates, bundles, client signatures and anchors without trusting the database. It has a reproducible build and a release script that produces signed checksums. The release files are signed with the project's release key `kingres-verify-release+dc5771ec+AW28IvL0KLUPGpksjDjO9JpNUPiUkg9oJmwQ8Z2Ik5mt` (fingerprint `81ac03cf18cf327bcdb757473e71851ec1d8c19eb6d6a8f33a08cb2574bcffd5`); [verifier/README.md](verifier/README.md) says how to check them.
- **Analytics extensions.**
  - `kingres_sketch` 1.0: distinct count, top-n and percentiles, with partial aggregation that the shard groups compute.
  - `kingres_columnar` 0.2: an append-only columnar table access method with B-tree indexes, parallel scan, logical decoding of inserts and reuse of free space. It logs its WAL under the resource manager id 255, the id registered for `KingresColumnar` on the [PostgreSQL wiki list of custom resource managers](https://wiki.postgresql.org/wiki/CustomWALResourceManagers). WAL that a development build wrote under the experimental id 128 is not replayed by this version.
  - `kingres_timepart` 0.3: time partitions created ahead and dropped by policy, conversion of old partitions to columnar storage and back, and policies on user-partitioned distributed tables.
- **Documentation.** A user guide ([docs/guide](docs/guide/README.md)): quickstart, installation, operator guide, upgrade and limitations.

### Security

- The hardening of the extensions is listed under Added.
- The container image listens on `0.0.0.0:8091` over plain HTTP and accepts setup from other machines, guarded only by the setup token. Publishing the port with `docker run -p 8091:8091` on a network that is not trusted sends the setup token and the first password in clear text. Publish it on `127.0.0.1` as the compose file does, or put TLS in front ([install guide](docs/guide/install.md#from-a-container-image)). The database port 5432 also listens on `0.0.0.0` inside the container: publish it on `127.0.0.1` or on a private network only.
- The archive of a database import is treated as hostile input, and the transfers run as the role that asked ([ADR 0027](docs/adr/0027-transfers-are-staged-encrypted-files-run-as-the-requesting-role.md)).

### Known limitations

- No global snapshot across shard groups and no multi-node ledger (phase 8, future roadmap).
- No move of a cluster between PostgreSQL majors; no signed packages, no apt repository.
- Not in the first release: an external signer command for the ledger, publication mirrors, and the anchor probe from another server.
- The Extensions tab shows whether each extension is preloaded; the install state of an extension in a database is not shown in the console.
- Export and import of a database carry no roles, grants, functions, triggers, policies or large objects; a ledger table restarts its chain.
- A failover of the manager leader at every step of a table transfer or of a database import is not tested on a cluster.
- Automatic failover is off by default; two failures at once, and a failure during a rebalance, are not tested on a cluster. A cluster of one server has no failover.
- `kingres_columnar` tables are append-only (no `UPDATE` or `DELETE`), and `CREATE INDEX CONCURRENTLY` and `REINDEX CONCURRENTLY` on them are refused.
- The check `anchors` of a ledger verification and a scheduled audit of the publication are not built (the check answers 422 `unsupported_operation`).
- No automated backup and no point-in-time recovery configuration.
- Packages are Debian (`.deb`) and a container image for amd64 only: no `.rpm`, no arm64.
- The full list is in [docs/guide/limitations.md](docs/guide/limitations.md).

<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Operator guide

How to run a KingresDB cluster. It assumes the cluster exists ([quickstart.md](quickstart.md)). Parts marked **In progress** or **Open** are not finished (see [README.md](README.md), "How to read the markers").

## 1. The control plane

Every server runs `kingresd`. It manages the database instance on its server (it starts and stops it, writes its managed configuration, creates standbys), serves the console and the REST API, and takes part in the cluster:

- **The manager leader** is the `kingresd` on the server that hosts the primary of the map group (shard group 0). It keeps the cluster certificate authority, the system database `kingres_system`, and runs the task engine. If a request reaches another server, it is forwarded to the leader. When the leader changes, another server takes over the state from the system database ([../adr/0018-manager-leader-follows-the-map-group-primary.md](../adr/0018-manager-leader-follows-the-map-group-primary.md), [../adr/0019-cluster-state-in-a-system-database.md](../adr/0019-cluster-state-in-a-system-database.md)).
- **Servers talk to each other** on the internal API (mutual TLS, the console port plus one) under the cluster certificate authority. Servers renew their own certificates before they expire.
- **Tasks.** Every long operation (add or remove a server, failover, rebalance, a table change, key and anchor jobs) is a task with steps. A task is stored, survives a restart of `kingresd` and a change of leader, and is fenced so that a server that lost its lease writes nothing. The Tasks page lists them; `GET /api/v1/tasks/{id}` reads one; `POST /api/v1/tasks/{id}/cancel` cancels one while it can be cancelled.
- **Failover is built into `kingresd`.** Neither Patroni nor etcd is shipped ([../adr/0021-failover-in-kingresd.md](../adr/0021-failover-in-kingresd.md)). Failover is fenced by leases that a majority of the servers grant.

**Roles.** The console and the API sign in with a PostgreSQL role that can log in and is a member of at least one of four cluster roles: `kingres_readonly`, `kingres_stats` (metrics and the shell of the console), `kingres_ops` (run operations such as failover and rebalance, remove a server) and `kingres_admin` (change settings, add servers, manage roles and the ledger configuration). Each operation's required role is in `kingres/api/openapi.yaml` (`x-required-role`). A session belongs to the server that issued it: sign in on each server you use.

**Where things are.**

| What | Where |
|---|---|
| Settings of `kingresd` | `/etc/kingres/kingresd.json`, `/etc/kingres/kingresd.env` (all keys: [../../control-plane/README.md](../../control-plane/README.md), "Configuration") |
| State, certificates, audit log | `/var/lib/kingres` |
| Log of `kingresd` | `journalctl -u kingres` |
| Server log | `/var/lib/kingres/instances/1/pgdata/log/` (the Logs page reads both, cluster-wide) |
| Metrics | `/metrics` (Prometheus format; scrape with `KINGRES_METRICS_TOKEN`); there is no bundled metrics store |
| Audit | the Audit list of the API (`GET /api/v1/audit`) and `audit.jsonl` on each server |

## 2. The console

Every server serves the console on its console address (default `127.0.0.1:8091`; use TLS before you listen on a routable address). It is a single-page application served by `kingresd`, over the same REST API as any client. Pages:

| Page | What you do there |
|---|---|
| Dashboard | Cluster health at a glance: servers, alerts, tasks, restarts that wait, clock differences |
| Servers, Server | Add, update and remove servers, see the shard groups and standbys of a server, certificates, fail a server over |
| Shards | Shard groups, colocation groups and the bucket distribution |
| Rebalance | Plan, start, stop and watch a rebalance; reports of earlier runs |
| Replication | The topology of primaries and standbys, switchover, the settings of automatic failover, reports of failovers |
| Databases, Database | Create a database (sharding by rows, schemas or both; durability; ledger), see its tables and schemas, the approximate aggregates card, and the transfers panel |
| Table | The page of one table: distribution, partitions, storage format, partition policy, the tasks that changed it, export and import |
| Security | Roles, memberships, grants, network access rules (`pg_hba`) |
| Configuration | PostgreSQL parameters per cluster, group or server, with a preview, history and revert; the Time tab |
| Ledger | Status, digests, verification, signing keys, publication, anchor targets and records, client keys |
| Workbench | Run SQL against a database |
| Settings | Cluster settings: default replica count, automatic failover, rebalance and its automatic mode, data transfer, and the analytics owner switch |
| Alerts, Tasks, Logs | What needs attention, what is running, the logs of every server |
| This server | The local instance: start, stop, restart, settings, leaving the cluster |

Everything the console does is an API call; the contract is `kingres/api/openapi.yaml`, so every operation in this guide can be scripted.

The table page (`/databases/<name>/tables/<schema>/<table>`) has the dialogs that distribute a table, turn it back into an ordinary one, alter its distribution, and create, attach, detach and drop partitions (phase 9, increment 9.7). Each dialog shows a preview of what the change would do before it starts ([section 5](#5-table-lifecycle)). Not in the console: distributing a table together with the tables it is linked to by foreign keys, and a progress indicator on a row of the tables list (**Open**).

## 3. Adding and removing servers

**Add.** From a server of the cluster: `POST /api/v1/servers` with `{"address": "<host>:8091", "setupToken": "<token of the new server>"}` (`kingres_admin`), or the Servers page. The new server must be installed from the same major and the same version, configured as in [quickstart.md](quickstart.md) (`server-host`, `internal-listen`, `pg-listen`) and uninitialized. The task `server.add` makes the server trust the cluster, creates its instance, copies the roles and the databases with their extensions, registers the server with the sharding extension and records it. Or let the new server join: its setup wizard, or `POST /api/v1/setup/join-cluster` on it. Pin the certificate of the other side with `X-Kingres-Pin-Sha256` when the network is not trusted.

An added server owns no bucket. Run a rebalance to give it data ([section 4](#4-rebalance)).

**Remove.** `DELETE /api/v1/servers/{serverId}` (`kingres_ops`), or the Servers page; a server can also leave on its own (`POST /api/v1/this-server/leave-cluster`, `kingres_admin`, or the This server page). The task is `server.remove` or `server.leave`. It refuses, with a code that names the reason:

| Refusal | Meaning | What to do |
|---|---|---|
| 409 `server_owns_buckets` | The server still holds a copy of a bucket | Move the buckets away first (rebalance) |
| 409 `server_hosts_map_group` | The server has an instance of shard group 0, or is the manager leader | Move the map group's primary elsewhere first with a planned switchover ([section 6](#6-failover-and-switchover)); a drain does not hand the map group over (drain step 4, **Open**) and refuses the same way |
| 409 `rebalance_in_progress` | A rebalance is unfinished | Let it finish, or stop it |
| 409 `operation_in_progress` | Another task of this server is unfinished | Wait for it |
| 409 `last_server` | The only server | Not possible |
| 422 `node_state_invalid` | The server is dead | Fail it over first |

After removal the server resets itself: its instance and certificates are deleted, and it can join again with a new setup token. A leaving server's certificate is revoked on every server.

**Certificates.** Each server renews its own before expiry. An operator can renew or revoke a server's certificates (`POST /servers/{id}/certificates/renewals` and `/revocations`, `kingres_admin`). The cluster authority is not rotated. Its key (`pki/ca.key`) lives on the manager leader only: back it up (section 7).

## 4. Rebalance

A rebalance moves buckets so that servers hold about the same number. It is one task (`rebalance`), one move at a time, run by the manager leader.

1. Preview: the Rebalance page, or `POST /api/v1/rebalance/plan`. This changes nothing.
2. Start: `POST /api/v1/rebalance` (`kingres_ops`). Writers keep writing; a move copies the bucket with logical replication and switches over at the end for a short time, during which writers of that group's tables wait. Long transactions delay a switch.
3. Watch: the Rebalance page, `GET /api/v1/rebalance`, and the Tasks page.
4. Stop: `POST /api/v1/rebalance/stop`. The move in flight is cancelled if it has not switched, no other starts, and the task is paused; starting again continues the same task.
5. Read the report of a run: `GET /api/v1/rebalance/reports`.

**Distributed schemas.** The plan and the run include the distributed schemas of the database (phase 11, `kingres_shard` 0.9): a schema is one item that moves whole, shown in the plan, in the progress and in the report with `kind` `schema` and its `schemaName`. A schema with a ledger table is pinned and is not planned; its size stays on its group as load. A move of a schema is a failure of the run like a move of a bucket when it is given up.

What to know:

- A failed move is tried again after a pause (up to three tries); a bucket given up ends the run as failed with a report and an alert. A move skipped because of a condition (a group's lease lost, a required synchronous standby not in sync) is in the report and is not a failure.
- A rebalance survives a restart of `kingresd` and a change of leader.
- A failover while a move is in flight fails the move and undoes it; the cluster stays correct.
- The plan is by bucket count by default. All of the following need `kingres_shard` 0.8 or later; against an older extension anything but the plan by bucket count is refused with 422 `unsupported_operation`.

**Strategies, threshold and weights (phase 10).** A rebalance can plan `byBucketCount` or `byDiskSize`, with a threshold (how far a server may be from the ideal before a move is planned; the default is 0 by bucket count and 0.1 by disk size) and a limit of moves per server at a time. What a request leaves out comes from the cluster settings (`rebalance.strategy`, `threshold`, `maxMovesPerServer`, `maxMoveAttempts`, `maxDiskUsedFraction`, `maxStandbyLagBytes`; `PATCH /api/v1/settings/cluster`, the Settings page). The preview names the strategy and the threshold, the imbalance, and the load of each server before and after. A server has a weight (`PATCH /api/v1/servers/{serverId}` with `weight`, or the Servers page): a larger weight gets more; nothing moves until the next rebalance.

**Automatic mode.** Off by default (`rebalance.automatic.enabled`). When on, the leader looks at the cluster at an interval and starts a rebalance only when the imbalance has held for a settle time, the servers have not changed lately, enough time has passed since the last automatic run, and no safety rule holds it back (a failover, a primary that changed lately, a group without a primary or without the standbys the replica count asks for, a dead or suspect server, a failed earlier move nobody acknowledged, a failing cluster check). A run pauses when a rule fails and resumes when the rules have held again. A failed automatic run raises `rebalanceFailed` and suspends the mode until an operator starts a rebalance. The Rebalance page shows the last evaluation.

**Drain.** `POST /api/v1/servers/{serverId}/drain` (preview: `.../drain-preview`) empties a server so that it can be removed: it stops new buckets from landing on it, moves its buckets away, relocates the standbys it hosts to other servers, writes a report and, if you asked, removes the server. A drain is a task of kind `rebalance`, so it excludes failover and other server tasks, and its progress is on the Rebalance page. It is refused before a task exists for an unknown server, the last server, no server that can receive buckets, a server that has the map group (409 `server_hosts_map_group`), too few servers to keep the replicas (`replicas_cannot_be_kept`; `acceptFewerReplicas` overrides), a degraded group, a dead server, and a server that holds ordinary tables that would leave with it (`acknowledgeLocalData`). A drain moves the distributed schemas (phase 11) of the server with its buckets: each schema is an item of the plan (`kind` `schema`, `schemaName`; the preview and the report name it) and is moved by the extension's schema move, with the same retries, report and audit as a bucket. A schema that cannot move refuses the drain before a task exists, naming it: 409 `schema_pinned` for a schema that holds a ledger table, 422 `unprocessable` for a table that is not logged or has neither a key nor a full replica identity (set `REPLICA IDENTITY FULL`, then try again). The drain ends only when no bucket and no schema is left on the server, and a schema whose old copy is not yet emptied still counts. **Open:** the hand-over of the map group in a drain (step 4) is not built, and the rule that holds a move back while a standby lags by more than `maxStandbyLagBytes` is not evaluated yet.

**Split a bucket.** The Shards page and `POST /api/v1/databases/{db}/colocation-groups/{colocationId}/buckets/{bucket}/split` (preview: `.../split-preview`) split a bucket into several, so that a hot or large bucket can be spread. It is the task `bucket.split`; it cannot be undone. A cancel works until the backfill ends, and the console says truthfully when it came too late and the split completed. A table without a primary key is refused when the split starts.

**Distributed schemas (phase 11).** A database can be sharded by rows, by schemas, or both (`sharding.mode` of `createDatabase`; the create dialog's "Shard by"). A schema is placed on one shard group and moved as a unit: `distributeSchema`, `moveSchema` and `undistributeSchema` (Schemas on the database page), tasks `schema.distribute`, `schema.move` and `schema.undistribute`. Tables that a move cannot copy (replica identity NOTHING, or no primary key and no replica identity) are listed in the refusal; `setReplicaIdentityFull` sets it for you. A schema that holds a ledger table is pinned and cannot move. These tasks exclude failover and the server tasks. The extension's plan (`plan_rebalance`) and `check_cluster` know schemas in `kingres_shard` 0.9; a rebalance and a drain plan and move schemas as items of the plan (section 4). A schema counts as one bucket by `byBucketCount` and as its size (at least 1 MiB) by `byDiskSize`, which is the strategy to use when schemas differ much in size.

## 5. Table lifecycle

On a database with sharding on:

| Operation | API | Task |
|---|---|---|
| Distribute a table, even one that holds rows (`hash` or `reference`) | `PUT .../tables/{table}/distribution` | `table.distribute` |
| Turn a distributed or reference table back into an ordinary table | `DELETE .../distribution` | `table.undistribute` |
| Change the distribution column, the bucket count or the colocation group | `PATCH .../distribution` | `table.alter` |
| Preview any of them: what would prevent it, how it would run | `POST .../distribution/preview` | none |
| Create, attach, detach, drop a partition | `.../partitions...` | `table.partition` |

(`...` is `/api/v1/databases/{db}/schemas/{schema}/tables/{table}`.) A change runs in mode `auto` (the default), `online` (writers go on; the table needs a primary key or a unique index over `NOT NULL` columns) or `blocking` (writers wait for the whole copy). `auto` is online when the table has a usable key and more than 100000 estimated rows. Dependent views make a change refuse unless you ask for `recreate`. These calls refuse `REPEATABLE READ` and `SERIALIZABLE` sessions; a table partitioned by the user (RANGE or LIST) can be distributed. The extension README ([../../../contrib/kingres_shard/README.md](../../../contrib/kingres_shard/README.md), "The table lifecycle") says exactly what each does.

## 6. Failover and switchover

Standbys, leases and failover are part of `kingresd`. A group has a primary on one server and standbys on others (the number is the cluster setting `defaultReplicaCount`, and a database's durability decides whether commits wait for a standby). Standbys are placed by the chained rule of the phase 5 design ([../design/ha-phase5.md](../design/ha-phase5.md)): a server hosts the primary of its own group and the standby of a neighbour.

**Failover of a server (manual).** `POST /api/v1/servers/{serverId}/failover` (`kingres_ops`), or the Servers page. It promotes a standby for every group whose primary was on that server, one group at a time, as the task `server.failover`. Ask for a dry run first: it shows the plan and changes nothing. A real request needs an explicit decision (`allowDataLoss`) when the data on the best standby may be behind what the old primary acknowledged. After a promotion the old primary is rebuilt as a standby of the new one (by `pg_rewind`, or a new copy when the log it needs is gone) when it comes back, and only a directory that is proved to be a former primary is touched. The report (`GET /api/v1/failover/reports`) says what happened, including incidents.

**The map group.** Failing over the server that holds the primary of the map group (the manager leader) is supported in mode `hard`: a standby of group 0 takes over from a journal that survives a restart of `kingresd`. The key of the certificate authority is **not** escrowed. The new leader raises the critical alert `certificateAuthorityUnavailable` until the operator restores `pki/ca.key` and `pki/revoked.json` of the old leader from its backup into the `pki` directory of the new leader and restarts `kingresd`. Keep that backup (section 7).

**Automatic failover.** Off by default. Turn it on in the cluster settings (`PATCH /api/v1/settings/cluster`, `autoFailover`: `enabled`, `timeoutSeconds`, `maxCount`) or on the Replication page. A server is failed over when it has been heard of by nobody for the time you set and a majority of the servers agree, this server hears a majority, no failover is running, and none of its groups was failed over in the last 5 minutes. The counter of automatic failovers stops further ones at `maxCount` and raises the alert `autoFailoverLimitReached`; reset it with `POST /api/v1/settings/cluster/auto-failover/reset` (a finished rebalance resets it too). Two failures at once and a failure during a rebalance are not tested on a cluster.

**Switchover (planned).** `failoverServer` with `mode: graceful` moves the primary of each ordinary group on a server to a standby that streams from it: it pauses the writes, lets distributed transactions end, stops the old primary cleanly, proves that the standby replayed everything and then promotes it. Any failure before the promotion puts the old primary back in service. The pause of the writes is not kept across a restart of `kingresd` in the middle of a switchover.

**Switchover of the map group.** `mode: graceful` for the server that runs the primary of the map group, sent while that leader answers, is a planned switchover like any other with the map group last: the chosen standby's server starts the bootstrap, the old leader stops cleanly and sends the proof, the standby promotes and becomes the manager leader, and its task engine finishes the task. Read the task from the new leader once the old leader's system database is stopped. If the move gets stuck, `POST /api/v1/failover/map/abandon` (`kingres_admin`) gives up the bootstrap; for a planned switchover it makes the old leader roll back. The old leader of a switchover is held back (it keeps its lease) until a clean stop is over; a hold that nothing owns is warned about once a minute and shown in the status.

**Restoring preferred primaries.** After a failed server returns, `POST /api/v1/replication/restore-preferred-primaries` (`kingres_ops`; the Replication page has the dialog) hands each group back to its home server by a planned switchover, one group at a time. A group whose home server has no ready standby is skipped and listed in the report; a group that fails before its promotion is rolled back and the task ends `partial`. Ask for `dryRun` first. The map group is restored like any other group.

**Open (phase 5).** A hard failover of a server whose map group primary still answers is refused with 422 `unsupported_operation`; only the planned switchover and the restore include group 0. The hardening loops of the whole failover part (measured repeated runs, a review of everything together) and escrow of the certificate authority key are in progress: until they are done, treat failover as new and keep the backup of `pki/ca.key` (section 7). An old leader whose hold cannot be resolved because every other server is gone does not resume by itself; an operator ends the hold with the abandon call once a standby answers.

**What a failover cannot do.** It does not make a cluster of one server safe, and the phase 5 design says what a two-server cluster can and cannot do ([../design/ha-phase5.md](../design/ha-phase5.md)); failover is fenced by a majority of the servers. It does not restore data that no standby had. See [limitations.md](limitations.md).

## 7. Backups, and the system database

KingresDB does not make backups for you: there is no schedule, no archive configuration and no point-in-time recovery set up by the product. What it gives you:

- **Logical backups.** `kingres-pg_dump -h 127.0.0.1 -U <admin> -Fc shop > shop.dump` on a server dumps what that server holds; restore the dump into an empty database. A dump of one node of a cluster holds that node's partitions and the reference tables in full, not the cluster catalog. The extension README ([../../../contrib/kingres_shard/README.md](../../../contrib/kingres_shard/README.md), "Restoring a node from a logical dump") gives the steps, tested, to put a node back, and says what a restore of an old dump loses (rows written after it, buckets that moved).
- **Physical backups.** `kingres-pg_basebackup -h 127.0.0.1 -U <role with REPLICATION> -D DIR -X stream` (a replication rule on the network access page is needed). A physical backup is the way to get a node back exactly as it was.
- **Coordinated restore points.** On the map group primary (a superuser), `SELECT * FROM kingres.create_restore_point('name')` makes a restore point on every primary at a moment when no distributed transaction is half committed. Restoring every server to that name (`recovery_target_name`) gives a state without a half-committed transaction. Standbys get no restore point (they are rebuilt from the restored primary), the values handed out by sequences after the point are handed out again, and a restored map group primary must claim the map group again (`SELECT kingres.promote_node(<its node id>)`; see the extension README, "High availability").
- **A cold copy** of `/var/lib/kingres` with the service stopped. It holds the certificate authority key and the service credentials: treat it as a secret.

**What to back up outside the cluster, separately from the data:**

| What | Where | Why |
|---|---|---|
| `pki/ca.key` and `pki/revoked.json` of the manager leader | `/var/lib/kingres/pki/` | Needed to bring a new leader up after the old one is lost (escrow is not built) |
| The private signing keys of the ledger and their passphrase file | the `ledger-keys-dir` of every holder (default `<data-dir>/ledger/keys`), `ledger-key-passphrase-file` | Without the keys the ledger cannot be signed; keep the passphrase apart from the keys |
| The system database `kingres_system` | the map group primary | Holds the cluster's state: servers, tasks, alerts, settings, the ledger's logs, keys and events |
| The record of what was published | `published.json` in the key directory of every holder | See below |

**Restoring the system database.** After a restore of `kingres_system` from a backup, restart every `kingresd` of the cluster. The manager leader raises the term of the ledger when it takes over, and writes of the ledger's key set from the old term fail until the leader has taken over again, which the restart does.

A restored system database may hold fewer ledger checkpoints and events than were published. The publisher refuses to sign while that is so and raises the critical alert `ledgerSignerUnavailable` (subject `published-state`), which says what was published, what the database holds, and a confirmation of sixteen hexadecimal digits. The operator decides ([../adr/0025-restore-of-the-system-database-is-accepted-by-an-operator.md](../adr/0025-restore-of-the-system-database-is-accepted-by-an-operator.md); the status of this decision is Accepted, 2026-11-23):

1. Read the alert. Check that the database really was restored and that no newer backup of it exists. If a mirror of the publication holds the lost rows, prefer to recover from it (no importer is built).
2. Restart every `kingresd` after the restore (as above).
3. On the host of the manager leader, write the confirmation of the alert, and on a second line a reason of up to 200 printable ASCII characters, into `<ledger-keys-dir>/accept-restore`.
4. The leader makes its record what the database holds, one generation higher, keeps the old record as `published.json.before-restore-<generation>`, removes the file and publishes the signed event `restore-acknowledged` before it signs anything else.

The logs fork against a mirror that holds the old checkpoints; the event is the public statement of that, with the reason you gave. A confirmation of another situation is ignored. There is no REST operation for this: it needs access to the leader's host.

## 8. Ledger, anchoring and the verifier

A database created with a ledger mode (`appendOnly` or `updatable`) gets `kingres_ledger`. Ledger tables keep a hash chain; blocks close at the digest interval (60 to 86400 seconds, default 3600); `kingres.ledger_verify()` checks the chain inside the database. A ledger mode can be raised from off and switched between the two modes for new tables, never set back to off. The extension README ([../../../contrib/kingres_ledger/README.md](../../../contrib/kingres_ledger/README.md)) has the rules; read its "Limitations of this version".

A check inside the database proves nothing against someone who controls it, so the ledger has three outside parts.

**Digests and the verifier.** A digest is a small document; keep it where the database's operators cannot change it. Generate one on the Ledger page or with `SELECT kingres.ledger_generate_digest()`, and give it, with proofs exported from the database, to `kingres-verify`, which runs on the auditor's machine and does not trust the database:

```
kingres-verify proof --proof proof.json --digest digest.json --table-id 16384 --row-hash <hex>
kingres-verify digest-chain --digests ./digests
```

An auditor must not use a verifier that came from the cluster. Build it from the release tag with `kingres/verifier/scripts/build-one.sh` and compare the SHA-256 with the release's `SHA256SUMS`, or take the release files ([../../verifier/README.md](../../verifier/README.md), "Releases, and how a third party gets the verifier"). The exit statuses mean 0 verified, 1 a verification failed, 2 an input error, 3 incomplete (something asked for could not be checked), 4 a fork or rollback was found and its evidence could not be saved. The package also installs a copy as `kingres-verify`; that copy is a convenience, not an independent verifier.

**Signing keys and publication.** The manager leader signs checkpoints of a transparency log for every chain and an aggregate log for every database, with a key that you create on the Ledger page (`POST /api/v1/ledger/signing/keys`), retire or revoke (`.../retire`, `.../revoke`) and distribute to the servers that may sign. Set `ledger-key-passphrase-file` (mode 0600, one line) so that the key files are sealed at rest. The leader serves the publication (checkpoints, tiles, entry bundles, events, the key ring) with no authentication on its own listener, which is off until you set `ledger-publication-listen` (use `ledger-publication-tls-cert` and `-key` for TLS). Give auditors the trust file (`GET /api/v1/ledger/signing/trust-file`) through a channel the operators cannot alter. The verifier follows key events from a pinned trust file, checks aggregates, bundles, client signatures and runs a monitor (`kingres-verify monitor`).

**Anchoring.** Anchoring puts a checkpoint's hash somewhere the operator cannot rewrite. Targets (Ledger page, `POST /api/v1/ledger/anchor-targets`, `kingres_admin`): a WORM object store (S3 style with object lock), an RFC 3161 time-stamp authority, or OpenTimestamps. Credentials are write-only: nothing returns them. Test a target (`.../anchor-targets/{id}/test`), set the interval and whether anchoring is required (`PUT /api/v1/ledger/settings`), anchor now (`POST /api/v1/databases/{db}/ledger/anchors`), retry a failed record, and download the evidence for an auditor. Only a receipt that the target's own parser reads back moves a record to `anchored`; an OpenTimestamps record is shown as submitted until a Bitcoin block confirms it. Alerts: `ledgerNotAnchored`, `ledgerAnchorBehind`, `ledgerAnchorFailed` and `ledgerAnchorConflict`. The RFC 3161 and OpenTimestamps targets were tested against local fakes only, and the verifier checks anchors against certificates and block headers that the auditor supplies. The scheduled audit job of the publication and the check of anchors in `startLedgerVerification` are not built (the latter answers 422 `unsupported_operation`).

**A diverged chain.** If a chain is rolled back, rewritten or forked, the leader stops signing that log, raises an alert, and keeps serving what it published. `POST /api/v1/databases/{db}/ledger/chains/{shardGroupId}/acknowledge` (a typed confirmation and a reason) closes the old log with the signed event `epoch-closed` and starts a new epoch.

**Client signatures.** A client can register a public key and sign what it writes; a table can require signatures (`setTableSignaturePolicy`). The extension must be 0.3 in every group (`POST .../ledger/upgrade` runs `ALTER EXTENSION kingres_ledger UPDATE` on every server; see [upgrade.md](upgrade.md)).

**Credentials of anchor targets.** Each credentials file carries a rotation generation. The leader gives a file to a holder with a lower generation and takes the file of a holder with a higher one first; a server refuses to write a lower generation over a higher one (409 `older_generation`). When two holders have different files of the same generation, the alert `ledgerAnchorFailed` asks you to enter the credentials again, which makes a higher generation. After an acknowledged restore the event `restore-acknowledged` names the generation and the digest of the record it replaced, so a second restore that reaches the same number is announced too. The fix rounds of these parts (reviews P7R, P7F, P7G, P7H) are merged. Phase 7 step 24 (an external signer command, publication mirrors to a directory or an S3 bucket, and the anchor probe from another server) is dropped from the first release: the leader signs with its own key files, and you mirror the publication with your own tools.

## 9. Configuration and time

The Configuration page and `/api/v1/config/...` manage PostgreSQL parameters at three levels: the cluster, a server group and one server (a narrower level overrides a wider one). A change is validated against what PostgreSQL says about the parameter, previewed, applied to every server it concerns as a task and reloaded; a parameter that needs a restart is shown as pending until you restart the instance (`POST /api/v1/config/restarts`). History and revert are there. Parameters that `kingresd` owns (for example `shared_preload_libraries`) cannot be changed. The Time tab shows each server's clock and time zone and alerts when the clocks differ too much; the product measures the difference and does not set a clock (use NTP or chrony).

The preview of a change also gives warnings that never make a request invalid: `config_value_unusual` for a memory value above ten times or below a tenth of the default of the parameter, and `config_reserved_connections` when on some server `max_connections` would not exceed `superuser_reserved_connections + reserved_connections`. Every successful start keeps the override file as the last good one, and the manager retries a failed start with it (the empty file when there is none). Some parameters are refused outright (for example `archive_mode`, `client_encoding`, `restart_after_crash`, `recovery_min_apply_delay`, `max_slot_wal_keep_size`), and `max_worker_processes` has a floor of 8. A server that went back to its last good file at a reboot is not shown as such on the server page (**Open**), and `max_parallel_workers` and `max_locks_per_transaction` have no floor.

## 10. Keeping watch

- Alerts (page, `GET /api/v1/alerts`, acknowledge with `.../acknowledgement`): version mismatch between servers (`versionMismatch`), certificates expiring, failover and anchoring alerts, tasks that failed, the oldest pending ledger transaction.
- Servers that run different versions of `kingresd` or of the extensions raise `versionMismatch`: finish an upgrade before anything else ([upgrade.md](upgrade.md)).
- `SELECT * FROM kingres.check_cluster()` on the map group primary reports a node that lacks the extension, runs another major, has lost its secret or differs from the catalog.

## 11. Analytics: approximate aggregates, storage format and time partitioning

Phase 12 puts the three analytics extensions under the control plane. Reads need `kingres_readonly`; changes need `kingres_admin`. The console parts are the card on the database page, the Storage column and the table page, and the workbench. These operations act on the tables of the instance that the manager leader runs. A table of another shard group's instance answers 404 with a detail that says so, and distributed tables are not converted (see below).

**Approximate aggregates.** `GET` and `PUT /api/v1/databases/{db}/approximate-aggregates` (the card on the database page). Turning it on is the task `database.approximateAggregates`: it checks and installs `kingres_sketch` on every server, verifies the version, and sets two database settings, `kingres_shard.count_distinct_mode` (`exact` or `approximate`) and `count_distinct_error_rate` (the console takes a percent between 0.05 and 10). In `approximate` mode a `count(DISTINCT x)` over a hash-distributed table is rewritten to `kingres_sketch.approx_count_distinct`, so the answer is approximate by design. The rewrite also runs on a cluster of one shard group, so an answer does not depend on the number of groups. A server that has another version of the sketch extension stops the task (409 `extension_version_mismatch`).

**Storage format.** `PUT .../tables/{table}/storage` and `PUT .../partitions/{partition}/storage` (task `table.setStorage`; the dialog is "Convert storage") convert a table or one partition between row storage and columnar storage, with a codec, a level (1 to 19), stripe and chunk sizes, and for a partition a sort order. A plain table is rewritten under an `ACCESS EXCLUSIVE` lock, so readers and writers wait; a partition is built beside the old one and swapped in one short step. Add `dryRun=true` to get a preview: the blockers (a partitioned table, a ledger table, a distributed table, a publication, foreign keys, an owner that is not trusted) and notes. A refusal is 409 `conversion_refused` with the same blockers. The conversion asks for its lock with a short `lock_timeout` and tries again for at most ten minutes, then fails with the retryable `operation_in_progress`. **A conversion to columnar needs the library `kingres_columnar` loaded by every instance of every server**, standbys included. `kingresd` puts it in `shared_preload_libraries`, and the change takes effect at a restart: restart the instances the console lists as pending, standbys of a group before its primary.

**Time-partitioning policies.** `GET`, `PUT` and `DELETE .../tables/{table}/partition-policy` manage the policy of a table that is range-partitioned on a `date`, `timestamp` or `timestamptz` column. The fields are `partitionEvery` (for example `P1D`, `P7D`, `P1M`, or hours that divide a day for a timestamp column), `createAhead` (default `P7D`), `retention` and `retentionAction` (`drop` or `detach`), `columnarAfter` with `columnarSortBy`, `compression` and `compressionLevel`, `timezone`, `runEvery` (default `PT1H`) and `enabled`. `POST .../partition-policy/plan` shows what a run would do and changes nothing; `POST .../partition-policy/runs` runs it now; `GET .../runs` and `.../runs/{runId}` read the history. The leader's scheduler runs each policy at its interval as the task `partition.maintain`: it creates partitions ahead, converts old ones to columnar storage, and drops or detaches expired ones. In the console a new or shorter retention that deletes data needs a preview and the table name typed. A run that fails is not started again at every tick: the scheduler leaves the table alone for five minutes, and the alerts `partitionPolicyFailed` and `partitionsAheadLow` say what is wrong. A scheduled run refuses a clock that stepped (more than seven days plus `createAhead` after the last run), and a run by hand is bounded to five minutes of skew.

**Who owns the table.** The code of the table's owner (an index expression, a default, a trigger) runs in the service role's session for the time of a conversion or a run. So by default these operations are open only for tables whose owner is a superuser. The cluster setting `analytics.allowNonSuperuserOwners` (default `false`; `PATCH /api/v1/settings/cluster`, or the switch on the Settings page, which asks you to confirm) opens them for other owners: turn it on only if you trust the owners of those tables as you trust yourself. With it off, a conversion is refused (`conversion_refused`, blocker `owner_not_trusted`), a policy cannot be set, and the scheduler logs the refusal once and raises one `partitionPolicyFailed` alert for the table, which clears when the owner changes or the setting is turned on. A policy and a storage preview show `tableOwner` and `ownerTrusted`.

**In progress.** Time partitioning and conversion of distributed tables are being written and are not in the first release as it stands: policies and conversion are for ordinary tables.

**Joins and `MERGE` on distributed tables** need no operation. In `kingres_shard` 0.9 a join on columns other than the distribution column can be repartitioned, `MERGE` on a distributed table runs in the shard groups, and a hash-distributed table can use columnar storage (the blocking variants of the table operations only). `EXPLAIN` prints `Repartition:` or the reason it was not used, and the workbench summarizes the path above the plan. Shapes that are not accepted are refused with their reason, or answered by the complete scan; [limitations.md](limitations.md) lists them.

## 12. Export and import of a table (CSV)

Phase 14a. A table can be exported to a CSV file and loaded from one, from the console or the API. This is the **table** export and import; export and import of a whole **database** (phases 14b and 14c) come after the first release.

**Turn it on.** It is off. Set `dataTransfer.enabled` to `true` (the Data transfer group of the Settings page, or `PATCH /api/v1/settings/cluster` with `dataTransfer`: `enabled`, `retentionHours` (1 to 168), `maxFileBytes`, `stagingQuotaBytes`, `maxConcurrent`). Turning it on is recorded in the audit log. Every operation then needs `kingres_admin` and a request over TLS (start `kingresd` with `-tls-cert-file` and `-tls-key-file`). The data is bound by the PostgreSQL privileges of the signed-in role, so a role that cannot read a column cannot export it. A role with SUPERUSER, REPLICATION or BYPASSRLS, or one that reaches server files, programs or logs, is refused.

**Export.** *Export CSV* in the tables list or in the header of the table page, or `POST .../tables/{table}/export` (task `table.export`). The file is written by one `COPY` in a read-only transaction of the role, sealed (encrypted at rest) in the staging directory of the manager leader (`-transfer-dir`, default `<data-dir>/transfer`) and kept for `retentionHours`. The result shows the size, the number of rows, the SHA-256, the expiry and the consistency label: `single-snapshot` for a local or reference table and for a distributed table on one group, `per-group` otherwise. **There is no global snapshot**: export twice and compare the sums if you need to know that the table did not change. The option `spreadsheetSafe` (on by default) prefixes risky text cells with an apostrophe so that a spreadsheet does not run them; it is a convenience, not a security feature, and a file written with it has an apostrophe in front of a header cell that starts like a formula (import it with `header: skip`). A table with row-level security is exported only by its owner or a role with `BYPASSRLS`, and a read that would run a function of another role needs `acknowledgeOtherRolesCode`. Download the file from the Transfers panel (`GET /api/v1/data-exports/{id}/content`; the console resumes a broken download with a `Range` request and never saves a file whose SHA-256 differs) and delete it when you are done.

**Import.** *Import CSV*, or the calls `createDataUpload`, `uploadDataChunk` and `POST .../tables/{table}/import` (task `table.import`). The console previews the first 64 KiB, guesses the encoding, delimiter and header, and uploads in chunks of up to 64 MiB; an upload can be paused and resumed. One import takes at most 2 GiB of decompressed CSV (256 MiB for a reference table). **An import is all or nothing**: it runs in one transaction, and a marker row proves after any interruption whether it committed. If that cannot be read, the task fails with `import_outcome_unknown` and is never repeated: look at the table. Errors name a column and a line, never a value. The triggers and defaults of the table run as the importing role. Identity columns of a distributed table draw from the sequence of the node the statement runs on, so rows loaded without the column from different servers can repeat values: list the column in the file. Batches, skipping rows and `onError` are not built.

**Look after the staging area.** A staged file belongs to its creator (another role sees 404; any administrator may delete it). The metrics `transferStagingUsedBytes`, `transferStagingFreeBytes`, `transfersRunning` and `transfersStaged` and the alert `transferStagingLow` (warning at 90 percent of the quota, critical at 98) are for this. The key that seals the files is `-transfer-key-file` (default `<data-dir>/transfer.key`); it is in file-level backups of the data directory. Turning the setting off ends a task that has not begun to move data; a task that is moving data is cancelled with `cancelTask`.

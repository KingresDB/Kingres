<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Operator guide

How to run a KingresDB cluster. It assumes the cluster exists ([quickstart.md](quickstart.md)). Sections marked **To be checked when `<feature>` is merged** describe work that was not on the product branches when this was written (see [README.md](README.md), "How to read the markers").

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
| Databases, Database | Create a database (sharding, durability, ledger), see and distribute its tables |
| Security | Roles, memberships, grants, network access rules (`pg_hba`) |
| Configuration | PostgreSQL parameters per cluster, group or server, with a preview, history and revert; the Time tab |
| Ledger | Status, digests, verification, signing keys, publication, anchor targets and records, client keys |
| Workbench | Run SQL against a database |
| Alerts, Tasks, Logs | What needs attention, what is running, the logs of every server |
| This server | The local instance: start, stop, restart, settings, leaving the cluster |

Everything the console does is an API call; the contract is `kingres/api/openapi.yaml`, so every operation in this guide can be scripted.

**To be checked when `web-15` is merged:** the table pages of the console for turning a distributed table back into an ordinary one, altering its distribution, and managing the partitions of a table (phase 9, increment 9.7). Today the console has the dialog that distributes a table; the other table operations are in the API ([section 5](#5-table-lifecycle)).

## 3. Adding and removing servers

**Add.** From a server of the cluster: `POST /api/v1/servers` with `{"address": "<host>:8091", "setupToken": "<token of the new server>"}` (`kingres_admin`), or the Servers page. The new server must be installed from the same major and the same version, configured as in [quickstart.md](quickstart.md) (`server-host`, `internal-listen`, `pg-listen`) and uninitialized. The task `server.add` makes the server trust the cluster, creates its instance, copies the roles and the databases with their extensions, registers the server with the sharding extension and records it. Or let the new server join: its setup wizard, or `POST /api/v1/setup/join-cluster` on it. Pin the certificate of the other side with `X-Kingres-Pin-Sha256` when the network is not trusted.

An added server owns no bucket. Run a rebalance to give it data ([section 4](#4-rebalance)).

**Remove.** `DELETE /api/v1/servers/{serverId}` (`kingres_ops`), or the Servers page; a server can also leave on its own (`POST /api/v1/this-server/leave-cluster`, `kingres_admin`, or the This server page). The task is `server.remove` or `server.leave`. It refuses, with a code that names the reason:

| Refusal | Meaning | What to do |
|---|---|---|
| 409 `server_owns_buckets` | The server still holds a copy of a bucket | Move the buckets away first (rebalance) |
| 409 `server_hosts_map_group` | The server has an instance of shard group 0, or is the manager leader | Move the map group's primary elsewhere first; there is no drain of it yet (**to be checked when `fo-r2wip` and `cp-1011` are merged**, which carry the switchover of the map group and the drain) |
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

What to know:

- A failed move is tried again after a pause (up to three tries); a bucket given up ends the run as failed with a report and an alert. A move skipped because of a condition (a group's lease lost, a required synchronous standby not in sync) is in the report and is not a failure.
- A rebalance survives a restart of `kingresd` and a change of leader.
- A failover while a move is in flight fails the move and undoes it; the cluster stays correct.
- The first release moves buckets by their count. The plan is by bucket count, one move at a time.

**To be checked when `m18` and `cp-1011` are merged:** phase 10, which adds strategies (by bucket count or by disk size), a threshold, weights per server, several moves at a time, automatic rebalancing, draining a server and splitting a bucket, with their dialogs on the Rebalance, Servers and Shards pages. The extension side is `kingres_shard` 0.8 on branch `m18` and the control plane and console are on `cp-1011`. The same merge brings phase 11 (distributed schemas: placing and moving a whole schema as a unit).

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

**To be checked when `fo-r2wip` is merged:** the second round of the failover work: switching the primary of the map group in a planned way (today `graceful` of the map group is refused with 422 `unsupported_operation`), restoring the preferred primaries after a failed server returns (`POST /api/v1/replication/restore-preferred-primaries`; the console has the dialog, and the server side of this operation is on that branch), a plan for a switchover, the phases of a switchover in `GET /failover`, and the hardening loops. Until it is merged, bring primaries back to their home servers with a graceful switchover of each server, group by group.

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

A restored system database may hold fewer ledger checkpoints and events than were published. The publisher refuses to sign while that is so and raises the critical alert `ledgerSignerUnavailable` (subject `published-state`), which says what was published, what the database holds, and a confirmation of sixteen hexadecimal digits. The operator decides ([../adr/0025-restore-of-the-system-database-is-accepted-by-an-operator.md](../adr/0025-restore-of-the-system-database-is-accepted-by-an-operator.md); the status of this decision is Proposed):

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

**To be checked when `p7f-fix` is merged:** the fix rounds of phase 7 (the rotation generation of credentials, the events of a restore, the set of a dry run, and the fixes of the reviews P7R and P7F). Check this section against the merged code, in particular what is said about restore events and credentials.

## 9. Configuration and time

The Configuration page and `/api/v1/config/...` manage PostgreSQL parameters at three levels: the cluster, a server group and one server (a narrower level overrides a wider one). A change is validated against what PostgreSQL says about the parameter, previewed, applied to every server it concerns as a task and reloaded; a parameter that needs a restart is shown as pending until you restart the instance (`POST /api/v1/config/restarts`). History and revert are there. Parameters that `kingresd` owns (for example `shared_preload_libraries`) cannot be changed. The Time tab shows each server's clock and time zone and alerts when the clocks differ too much; the product measures the difference and does not set a clock (use NTP or chrony).

**To be checked when `p13-fin` is merged:** two more preview warnings (`config_value_unusual` and `config_reserved_connections`) and four more integration tests for the Configuration page.

## 10. Keeping watch

- Alerts (page, `GET /api/v1/alerts`, acknowledge with `.../acknowledgement`): version mismatch between servers (`versionMismatch`), certificates expiring, failover and anchoring alerts, tasks that failed, the oldest pending ledger transaction.
- Servers that run different versions of `kingresd` or of the extensions raise `versionMismatch`: finish an upgrade before anything else ([upgrade.md](upgrade.md)).
- `SELECT * FROM kingres.check_cluster()` on the map group primary reports a node that lacks the extension, runs another major, has lost its secret or differs from the catalog.

<!-- SPDX-License-Identifier: AGPL-3.0-only -->

# KingresDB web panel

The management console of KingresDB: a single-page application (Vue 3, TypeScript, Vite) that is a client
of the REST API in `kingres/api/openapi.yaml` (revision 0.13.0, 132 operations; the generated files still follow 0.12.0, see "Contract revisions" below) and nothing else. The Go server (`kingresd`) will embed the
production build from `dist/` and serve it on every node. The information architecture follows the
"Management panel" chapter of `docs/architecture.md`: a first-run wizard, a **Cluster** scope and a
**This server** scope.

## Quick start

```
npm ci
npm run dev          # http://localhost:5173 with the in-memory mock API (no backend needed)
```

Sign in with `admin`, `ops`, `viewer` or `metrics` and the password `demo-password`. The roles are
`kingres_admin`, `kingres_ops`, `kingres_readonly` and `kingres_stats`, so you can see how pages, buttons and navigation
change with the cluster role.

The mock cluster has three servers (chained placement: a primary on every server and a standby of the
neighbouring group), a map group, three shard groups and two databases: `shop` (sharded, 128 buckets, ledger
tables) and `reports` (plain). Mutations are tasks that progress for a few seconds and then change the state, so
adding a server, rebalancing, failing over or creating a database visibly changes the console.

Development switches in the URL (mock API only):

| Parameter                                                                             | Effect                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| ------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `?mock=uninitialized`                                                                 | Starts as a fresh server and shows the first-run wizard. The setup token is `demo-setup-token`. A cluster named `failme` makes setup fail (to see the retry).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `?unsupported=createInclusionProof,listAlerts`                                        | Those operations (0.8.0: for example `listConfigParameters,getClusterTime,getConfigSummary`) answer `unsupported_operation`, to see the "not available on this server version yet" state.                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| `?mock=hostile`                                                                       | Adds names and messages as a low-privileged database user could make them (markup, bidirectional and zero-width characters, look-alike letters, 5000-character names, a database called `..`). 0.8.0: also hostile parameter names, categories, descriptions, enum values, values on the servers and at every level, and the error text of a failed server (5000-character values included). 0.11.0: also a hostile origin prefix, public URL, listener address, key holders, cluster identifier, alarm text, event types, aggregate flags and statement.                                                                                      |
| `?partial=10.0.0.12:timeout,10.0.0.13:unreachable`                                    | Logs, alerts and metrics answer as partial answers (contract 0.5.0): `partial: true` and `missingServers` with the given ids and reasons (`timeout`, `unreachable`, `refused`, `invalidAnswer`). `?mock=hostile` does this with hostile server ids.                                                                                                                                                                                                                                                                                                                                                                                            |
| `?cursors=reject`                                                                     | Every request that carries a cursor answers 400, as a 0.5.0 server does for a cursor of 0.4.0.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `?silent=10.0.0.12`                                                                   | 0.7.0: the server stops sending heartbeats when the page loads: suspect after 5 s, dead after 10 s (the console shows the time since its last heartbeat). `10.0.0.12:suspect` and `10.0.0.12:dead` start in that state; several ids with commas. The answering server (10.0.0.11) cannot be silent.                                                                                                                                                                                                                                                                                                                                            |
| `?lease=lost:1`                                                                       | 0.7.0: the primary of shard group 1 lost its lease (only groups whose primary runs on the answering server, 10.0.0.11: groups 0 and 1; a group with a standby has its primary stopped, a group without one keeps running). `lost:0,lost:1` for several. `?lease=absent`: the server reports no leases (`ShardGroup.lease` absent, an older revision).                                                                                                                                                                                                                                                                                          |
| `?standby=stalled:3@10.0.0.11`                                                        | 0.7.0: the standby of group 3 on that server does not stream. During the grace period (30 s) the primary still waits for it; `stalled:3@10.0.0.11:past` puts the stop beyond the grace period (the primary no longer waits, the alert `groupWithoutSynchronousStandby` is raised).                                                                                                                                                                                                                                                                                                                                                             |
| `?workbench=busy`                                                                     | 0.7.0: every workbench statement answers 429 `workbench_busy` with `Retry-After: 2`. (The real rule, two statements per role and four per server, is also in the mock: two running `select pg_sleep(30)` fill it.)                                                                                                                                                                                                                                                                                                                                                                                                                             |
| `?leader=lost`                                                                        | 0.7.0: this server has not heard the manager leader: every request that needs a session check answers 503 `leader_unavailable` with `Retry-After: 5` (not the probes, the node state, `getSession`, the instance list and start).                                                                                                                                                                                                                                                                                                                                                                                                              |
| `?rebalance=refuse:<code>`                                                            | 0.9.0: the next start of a rebalance answers 409 with that problem code (`nothing_to_rebalance`, `cluster_inconsistent`, `version_mismatch`, `failover_in_progress`, `no_receiving_server`, `operation_in_progress`, ...). Without the switch the mock refuses by itself when the plan is empty, no server should hold shards or a run is going.                                                                                                                                                                                                                                                                                               |
| `?rebalance=fail`                                                                     | 0.9.0: the next run ends failed: one bucket is given up after three attempts (the task fails after its report was written).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| `?failover=dataloss`                                                                  | 0.10.0: the plan of every hard failover says that writes may be lost (verdict `possible`, the group ran without its synchronous standby): the request is refused with 409 `failover_needs_decision` until it says `allowDataLoss`; a dry run shows the plan and its blocker.                                                                                                                                                                                                                                                                                                                                                                   |
| `?failover=needs-decision`                                                            | 0.10.0: the dry run says `none`, the task starts and pauses at "Choose the standby" because the verdict turned `possible` while it ran (the leader restarted, as in increment 3); the status is `paused` with `needsDecision`, an alert `failoverNeedsDecision` is raised, and a repeat of the request with `allowDataLoss` goes on (without it: 409).                                                                                                                                                                                                                                                                                         |
| `?failover=blocked:rebalance`, `blocked:lease`, `blocked:quorum`, `blocked:noreplica` | 0.10.0: the plan lists the blocker and the request is refused with 409 `rebalance_in_progress`, 409 `lease_settings_differ`, 503 `quorum_unavailable` (a dry run is refused too) or 409 `no_replica_available`; every group's readiness carries the same blocker.                                                                                                                                                                                                                                                                                                                                                                              |
| `?failover=fail:<step>[:<group>]`                                                     | 0.10.0: the task ends failed at that step of the first group (or of the group given) with a task error code and a report; `<step>` is one of `check`, `freeze`, `stop`, `lease`, `choose`, `handover`, `promote`, `catalog`, `repoint`, `lift`, `verify`, `pause`, `report`. With two groups (`map`) a failure in the second leaves the first promoted: the report is `partial`.                                                                                                                                                                                                                                                               |
| `?failover=slow`                                                                      | 0.10.0: the steps of a failover take about three times as long as a task does, so that the phases, the lease wait and Cancel can be tried.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| `?failover=warnings`                                                                  | 0.12.0: a finished failover ends `completedWithWarnings` ("Completed, with remarks").                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| `?failover=noplan`                                                                    | 0.12.0: a refusal `failover_needs_decision` carries no plan (a server of 0.11.0): the dialog asks with a dry run.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| `?failover=map`                                                                       | 0.10.0: the map group can be failed over (the real server answers 422 `unsupported_operation` for it until increment 5; so does the mock without the switch). The manager moves first: the status has no `taskId` and the phase `bootstrap` until the catalog follows.                                                                                                                                                                                                                                                                                                                                                                         |
| `?failover=graceful`                                                                  | 0.10.0: the planned switchover is accepted (the real server answers 422 `unsupported_operation` until increment 7; so does the mock without the switch): the steps are the switchover's, the groups report `writesPausedSince`. `?failover=switchover:behind` also makes its verification fail: rolled back, task error `standby_behind`.                                                                                                                                                                                                                                                                                                      |
| `?tables=partitions`                                                                  | 0.14.0: adds to the database `shop` a distributed table `measurements` that the user partitioned by RANGE on `taken_at` (three partitions, the last one the DEFAULT) and a table `measurements_old` of the same colocation group that can be attached to it.                                                                                                                                                                                                                                                                                                                                                                                   |
| `?tables=fail:space`, `fail:copy`                                                     | 0.14.0: the next change of a table fails at its first step (not enough free space on a server) or in the copy (a connection closed); the table stays as it was.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| `?tables=slow`                                                                        | 0.14.0: the tasks of a table take a minute, so that their steps and Cancel can be tried (a task can be cancelled until its switch, at 70 percent).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| `?tables=busy`                                                                        | 0.14.0: a bucket move or another operation holds the colocation groups: the preview of a change of a distributed table lists `group_busy`, and the change is refused with 409 `operation_in_progress`.                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `?tables=fk:<table>`                                                                  | 0.14.0: that table has a foreign key to an ordinary table (the report of the extension lists it; a table with foreign keys cannot detach a partition).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `?analytics=<a>,<b>,...`                                                              | 0.18.0 (phase 12, combinable): `on` (approximate aggregates on in `shop`), `timeseries` (in `shop`: a table `clicks` that can be converted, and `metrics`, partitioned by day, with a policy and three runs), `ahead-low` (the same, one day ahead, the alert), `sketch:missing`, `sketch:mismatch`, `sketch:hash`, `columnar:missing`, `timepart:missing`, `convert:fail`, `run:lock`, `run:name`, `run:fail`, `publication`, `old` (the 11 new operations answer 422 `unsupported_operation`).                                                                                                                                               |
| `?failover=old:rebuilding`, `old:waiting`, `old:running`, `old:failed`                | 0.10.0: the state of the old primary in the report (rebuilding with `pg_rewind` and 12 MiB, waiting for its server, still running, rebuild failed); `running` and `failed` also raise the alerts `oldPrimaryRunning` and `oldPrimaryNotRebuilt`.                                                                                                                                                                                                                                                                                                                                                                                               |
| `?failover=auto:10.0.0.12`                                                            | 0.10.0: an automatic failover of that server is under way (`trigger: automatic`, started by `system`).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `?failover=limit`                                                                     | 0.10.0: the counter of automatic failovers is at its maximum (`eventsSinceReset` equals `maxCount`, `limitReached` true in the overview).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| `?failover=count:<n>[:<m>]`                                                           | 0.10.0 (console part 2): the counter of automatic failovers is at `n` of `m` (the maximum stays as it is when `m` is left out), with a time of the last event; at `n >= m` the alert `autoFailoverLimitReached` is raised as well.                                                                                                                                                                                                                                                                                                                                                                                                             |
| `?failover=off`                                                                       | 0.10.0 (console part 2): automatic failover is turned off in the settings.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |
| `?rebalance=slow`                                                                     | 0.9.0: moves take about three times as long, so a stop in the middle is easy to try. A stop takes about 1.5 s to end the move in flight (state `stopping`, then `stopped`); starting again goes on with the same task.                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `?client=on` / `?client=standby`                                                      | 0.7.0: the client entry point is on (`standby`: its standby entry point too): the 8 (16) `client*` metrics exist. Without it `queryMetrics` answers those names with no series, as a server whose entry point is off.                                                                                                                                                                                                                                                                                                                                                                                                                          |
| `?ledger=all` / `?ledger=absent`                                                      | 0.7.0: the ledger status covers every shard group / carries no `complete` (an older server). The default is what kingresd 0.7.0 does: the chains of the shard group of the answering server only, the others in `uncoveredShardGroupIds`.                                                                                                                                                                                                                                                                                                                                                                                                      |
| `?config=failed:10.0.0.12`                                                            | 0.8.0: the next configuration change fails on that server (PostgreSQL rejects the file): the task ends failed with `config_incomplete`, the other servers are applied, the server shows the text, and **Retry** (the same change again) works. Several servers: comma.                                                                                                                                                                                                                                                                                                                                                                         |
| `?config=waiting:10.0.0.13`                                                           | 0.8.0: that server cannot be reached for a configuration change: the task holds at its step ("Waiting for server 10.0.0.13, which cannot be reached") and never gives up; it can be cancelled. `__kingresMock.configServerBack(id)` brings the server back.                                                                                                                                                                                                                                                                                                                                                                                    |
| `?config=restart-conflict`                                                            | 0.8.0: a restart is pending in which `max_connections` goes up and `max_wal_senders` goes down: `restartPendingInstances` answers 409 `config_restart_order_conflict`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `?phase7=today`                                                                       | 0.11.0: the 21 ledger operations that `kingresd` does not build yet answer 422 `unsupported_operation`, as the real server does today (only the settings, the signing keys and the trust file work). Without it the mock answers all 28 new operations as the design of phase 7 says they will.                                                                                                                                                                                                                                                                                                                                                |
| `?signing=<a>,<b>`                                                                    | 0.11.0: the signing keys: `none` (no key, no locked origin prefix, publication off), `rotating` (two keys active), `revoked` (a revoked key and a third that signs), `plain` (key files not sealed by a passphrase), `backup` (a backup is advised), `alarm` (this server cannot sign: alert `ledgerSignerUnavailable`), `discontinuity`, `command` (external signer), `restored` (0.12.0: the system database is older than what was published: alert `ledgerSignerUnavailable` with the subject `published-state`), `fail:<code>` (the next key task ends failed with that task error code: `key_files_remain`, `passphrase_mismatch`, ...). |
| `?publication=<a>,<b>`                                                                | 0.11.0: `off`, `plain` (the listener has no TLS), `nolistener`, `behind` (the newest checkpoint is old: alert `ledgerPublicationBehind`), `unread` (a group could not be read: a stale member, alert `ledgerGroupUnread`), `diverged` (the chain of group 2 of `shop` no longer extends its published log: alert `ledgerChainDiverged`).                                                                                                                                                                                                                                                                                                       |
| `?anchors=<a>,<b>`                                                                    | 0.11.0: `none` (the alert `ledgerNotAnchored`, info), `required` (the same as a warning), `ok`, `behind`, `failed` (a WORM target `archive-worm` exists and the newest aggregate is anchored in time, late, or its record failed). Without the switch no target exists and no alert is raised.                                                                                                                                                                                                                                                                                                                                                 |
| `?anchors=all`                                                                        | 0.11.0 (Console B): three targets, one of each kind (the WORM store anchors in time, the time-stamp authority is late, OpenTimestamps waits), and a record in every state in every database that has a ledger: waiting, sent and not final, anchored, failed with an error text, never confirmed.                                                                                                                                                                                                                                                                                                                                              |
| `?anchors=disabled`                                                                   | 0.11.0 (Console B): a WORM target exists and is turned off, so nothing is anchored (the notice, and the alert `ledgerNotAnchored`).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |
| `?anchors=testfail`, `testwarn`                                                       | 0.11.0 (Console B): the test of a target finds a failed check (the store refused the credentials, the authority did not answer, no calendar answered) or a warning. A failed check is a result, never an HTTP error.                                                                                                                                                                                                                                                                                                                                                                                                                           |
| `?anchors=refuse:<code>`                                                              | 0.11.0 (Console B): every change (add, change, delete, test, anchor now, retry) answers that problem code (`validation_failed`, `already_exists`, `immutable_field`, `conflict`, `not_found`, `forbidden`, `leader_unavailable`, `ledger_not_published`, `unprocessable`, ...); reading still works.                                                                                                                                                                                                                                                                                                                                           |
| `?clock=off:10.0.0.12:-20000`                                                         | 0.8.0: the clock of that server is that many milliseconds ahead (negative: behind). The default is `10.0.0.13` at +1820 ms, which raises the alert `clockDifference`.                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| `?clock=unreachable:10.0.0.12`                                                        | 0.8.0: the clock of that server cannot be read (`reachable: false`, no values, never 0).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| `?latency=0`                                                                          | No artificial delay (the default is 150 ms).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |

In a development page, `window.__kingresMock` changes these states while the page is open (`config.leaderLost = true`,
`silence('10.0.0.12')`, `revive(...)`, `loseLease(1)`, `regainLease(1)`; 0.8.0: `failConfigOn(id, text)`, `configServerDown(id)`, `configServerBack(id)`,
`clockOff(id, ms)`, `clockUnreachable(id)`; 0.10.0: `failServer(id)` (a server silent for longer than the failure timeout), `reviveServer(id)`,
`setReadiness(group, state)` (what the card of a group says about failover: `possible`, `possibleLoss`, `unknownLoss`, `noStandby`,
`noneReady`, `noMajority`, `versionsDiffer`; `null` clears it), `resetCounter()`, `anchorsSwitch('all')` (0.11.0: a value of `?anchors=` while the page is open; `window.__queryClient.invalidateQueries()` makes the pages read again), `failoverSwitch('limit')` (any value of `?failover=` while the page is open) and `resetFailover()` (the cluster as it was before any
failover); the browser tests use it). The values of `?failover=` can be combined with commas (`?failover=map,slow,fail:promote:1`).
Reload the page to reset the mock state. Creating a database named `failme` fails half way, to see a failed
task. The `metrics` account has only `kingres_stats`: it lands on the Dashboard with the charts it may read.
The third server and the `reports` database report some values as `null` (not available), which the panel
shows as "n/a" with an accessible label, never as 0.

### Against a real backend

```
npm run dev:backend  # mocks off; Vite proxies /api to http://127.0.0.1:8091
```

The proxy target is in `vite.config.ts`. `VITE_MOCKS=off` (see `.env.backend`) turns the mocks off. The mock layer
is imported only when `import.meta.env.DEV` is true, so it is not in the production build (checked: no `msw`
string in `dist/`).

## Scripts

| Script                        | What it does                                                                                                                                                                                                    |
| ----------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `npm run dev` / `dev:backend` | Dev server with the mock API / with the proxy to `127.0.0.1:8091`                                                                                                                                               |
| `npm run build`               | Type-check, then build to `dist/` (the directory the Go server embeds)                                                                                                                                          |
| `npm run typecheck`           | `vue-tsc --noEmit` (scripts and templates, `strict`, `noUncheckedIndexedAccess`)                                                                                                                                |
| `npm run lint`                | ESLint (flat config, Vue and Vue accessibility plugins) and `prettier --check`                                                                                                                                  |
| `npm run lint:fix` / `format` | Fix lint problems / format with Prettier                                                                                                                                                                        |
| `npm test`                    | Vitest and Testing Library, against the mock API (jsdom, no browser needed)                                                                                                                                     |
| `npm run test:browser`        | The tests that need a real Chrome or Edge: layout at 1366, 683 and 320 px, keyboard, axe, and the production build under the content security policy (needs `CHROME_PATH` if Chrome is not in a standard place) |
| `npm run api:generate`        | Regenerate the API types from the contract                                                                                                                                                                      |
| `npm run api:check`           | Fail if the generated files are out of date (also run by `npm test`)                                                                                                                                            |
| `npm run brand:sync`          | Copy the favicons from `kingres/brand/` into `public/` and regenerate the logo path data                                                                                                                        |
| `npm run brand:check`         | Fail if the committed brand files differ from `kingres/brand/` (also run by `npm test`)                                                                                                                         |

## End-to-end test against the real server

`npm run e2e` is one command. It needs Windows with WSL2 (distribution `Ubuntu-24.04`, user `kingres`), Go, Chrome or Edge, and
an installed KingresDB with the current extensions inside WSL (`~/kw-cp/install` when it has `kingres_ledger` 0.2, else
`~/kingres-work/install`; `E2E_PG_INSTALL` names another, for example a PostgreSQL 19 installation, and the same run then proves the console on that major). It:

1. builds the production panel (`vite build`), then `kingresd` for Linux from `kingres/control-plane` (or uses `KINGRESD_BIN`) with
   the panel embedded: a Go build overlay puts `dist/` in place of the placeholder in `internal/webui/dist` for this build only,
   so nothing in the control plane is changed, and the panel the browser sees is served by the server with its own headers,
2. copies the installation to `~/kw-web/install`, starts `kingresd` there on a fresh data directory (API port 58201,
   PostgreSQL port 58200; override with `E2E_API_PORT`, `E2E_PG_PORT`),
3. also builds the standalone verifier `kingres-verify` (`kingres/verifier`) for this machine,
4. drives a headless browser through the real flow (`scripts/e2e/flow.mjs`): wizard with the real setup token and the server's own fingerprint (wrong
   token and wrong data directory first), sign-in, reload, roles (duplicate, disable, a stats-only account), a sharded
   database with the ledger, tables made with psql, distribute and enable-ledger from the panel, workbench query and
   SQL error, grant, network rule, every page, calm notices for the operations the server does not implement yet (failover,
   rebalance), the cluster settings, the audit log, drop database, server groups (create, assign, refused delete, unassign,
   delete), the certificate and fingerprint of a server, the add-server dialog (pin or explicit unpinned choice; the server refuses a request without either), a ledger proof (pending
   until a digest, then shown), the proof and digest files compared byte for byte with the server's answer and accepted by
   `kingres-verify`, a ledger verification and its record, local settings (log level, memory quota and the persistent
   restart-required state), a restart of the instance, stopping the instance that hosts the system database (one clear state,
   no error storm, a reload while stopped through the degraded session, start from the panel), the leave-cluster refusal for the only server, the audit log of these, the headers
   (security headers, caching, cookie attributes, no Content-Security-Policy violation on any page), a server restart that
   signs everyone out, and the stats-only and read-only accounts,
5. prints a report (notes, per-page state, every HTTP error the browser saw) and tears everything down.

The exit code is 0 only if every step passed. Screenshots are in `.e2e/shots/` (ignored by git). `E2E_KEEP_INSTALL=1`
keeps `~/kw-web/install` for faster re-runs; `E2E_KEEP_RUNNING=1` leaves the processes running. `scripts/e2e/backend.sh` is
the WSL side (`prepare`, `start`, `restart`, `token`, `psql`, `stop`). `puppeteer-core` is a development dependency.

The runner has steps for contract 0.7.0 (a running statement is cancelled; "Stop waiting" then "Cancel it on the server"; the
Replication page shows the lease and the commit rule of every group; the server page lists its leases; changing the replica count
of a one-server cluster), and the audit step expects `cancelStatement`. **These steps were written but not run**: when the
increment was made, a cluster test of the control plane (three `kingresd` processes) was running on the same machine, and the
runner must not start next to it. Run `npm run e2e` (with `E2E_API_PORT` and `E2E_PG_PORT` of your own) when the machine is free.
The selectors the new steps use (`group-lease`, `commit-rule`, `replica-count`, `server-leases`) are the ones the unit tests use.

### What the run against kingresd 0.2.0-dev found (2026-11-15, contract 0.6.0)

All 45 steps pass against the real server (built from the working tree, contract 0.6.0), with the panel served by the server
itself. What the run now proves for 0.6.0:

- The setup wizard shows the server's own fingerprint (`NodeStateInfo.internalFingerprint`, 64 digits) and it equals what
  `GET /setup/state` answers to the setup token.
- The add-server dialog does not pre-select the unpinned choice, blocks a submit with neither a pin nor the choice, and
  explains the unpinned choice; the server itself answers `POST /servers` without a pin with 400 `validation_failed` at
  `/expectedFingerprint`, and a malformed pin with 400 (checked with a bearer token, so the panel's session is untouched).
- The network-rules review dialog shows the server's dry-run answer ("accepts them ... Nothing is saved yet") before Apply.
- A ledger verification record says "Passed, all groups" with "All shard groups of the database" (`complete: true`).
- After the instance of the system database is stopped, a reload keeps the panel in the stopped state (no sign-in page): `getSession`
  answers `degraded`, the panel shows what the server says, names "Start instance #1" (from `listThisServerInstances`), holds
  nothing in web storage but the theme, and the start works. The stopped state lasted four failed requests (all
  `system_database_stopped`) in about 8 seconds.

Two things in the run are consequences of 0.6.0 and not defects: the stopped state begins when a restart is accepted, so the
page and the dialog on it are replaced by the stopped state while the instance restarts (the shell's notice "Restart instance #N:
finished." tells the result; the step accepts that or the dialog's "Finished."), and the "Start" button names the instance
(`Start instance #N`).

Earlier findings that still hold: the server sends `Cross-Origin-Resource-Policy: same-origin` and, with TLS,
`Strict-Transport-Security: max-age=63072000; includeSubDomains` (stronger than the `max-age=31536000` recommended below);
`kingresd` must be started with `-internal-listen` for leave-cluster, add-server and remove-server (without it they answer 409
`internal_api_disabled` since 0.6.0, which the panel explains as configuration; the run passes the flag).

## What the server must send

The panel is static files plus calls to `/api/v1/` on the **same origin**. It sets no security header itself (a
`<meta>` tag cannot set `frame-ancestors`, and caching is the server's business), so `kingresd` has to. This section is
written to be implemented without reading anything else. The values are checked: `tests/server-headers.test.ts` fails
when a value here differs from `browser-tests/server-headers.ts`, and `npm run test:browser` (`csp.test.ts`) builds
`dist/`, serves it with exactly these headers, and visits every page, the sign-in page, the first-run wizard and the
dialogs with hostile data: there is **no** policy violation and no console message about one.

### Headers on every response (the files of the panel and `/api/v1/`)

| Header                       | Value                                                                                                                                                                                          |
| ---------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `Content-Security-Policy`    | `default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; font-src 'none'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'` |
| `X-Frame-Options`            | `DENY` (old browsers; `frame-ancestors 'none'` above is the real protection)                                                                                                                   |
| `X-Content-Type-Options`     | `nosniff`                                                                                                                                                                                      |
| `Referrer-Policy`            | `no-referrer` (task ids, database and server names are in the path, and the sign-in redirect is in the query)                                                                                  |
| `Cross-Origin-Opener-Policy` | `same-origin`                                                                                                                                                                                  |
| `Permissions-Policy`         | `camera=(), microphone=(), geolocation=()`                                                                                                                                                     |

The exact lines, to copy:

```
Content-Security-Policy: default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; font-src 'none'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'
X-Frame-Options: DENY
X-Content-Type-Options: nosniff
Referrer-Policy: no-referrer
Cross-Origin-Opener-Policy: same-origin
Permissions-Policy: camera=(), microphone=(), geolocation=()
```

kingresd also sends `Cross-Origin-Resource-Policy: same-origin` on every response (the panel does not need it; nothing else may embed the responses) and, with TLS on, `Strict-Transport-Security`.

What the policy relies on, so that nobody weakens it by accident: the build has no inline script and no inline style
element, no `eval`, no `data:` URL, no web font, no external origin and no service worker. `'unsafe-inline'` and
`'unsafe-eval'` are **not** needed and must not be added. Vue sets styles through the CSSOM, which `style-src` does not
block; a `style="..."` attribute written into a statically hoisted template would be blocked (`style-src-attr`), so
the browser test fails if one appears. If the server also serves the panel under another path prefix or over another
origin, `connect-src` and `default-src` must name it (the panel only ever calls its own origin).

### Caching

| Response                                                                                                                   | `Cache-Control`                       |
| -------------------------------------------------------------------------------------------------------------------------- | ------------------------------------- |
| `index.html` and every path that falls back to it (`/login`, `/servers`, ...)                                              | `no-cache`                            |
| Files under `/assets/` (their names contain a content hash)                                                                | `public, max-age=31536000, immutable` |
| Every response of `/api/v1/`, errors included (the data is sensitive and depends on the role)                              | `no-store`                            |
| `/favicon.svg`, `/favicon-32.png`, `/apple-touch-icon.png`, `/icon-512.png`, `/manifest.webmanifest` (no hash in the name) | `no-cache`                            |

### The session cookie and the CSRF token

- `kingres_session=<value>; HttpOnly; Secure; SameSite=Strict; Path=/`, as the contract states. The panel works over
  HTTPS and at `http://localhost` (browsers keep a `Secure` cookie there); over plain HTTP on another host the browser
  drops the cookie and the panel says so after sign-in.
- The CSRF token comes in the JSON body of `login` and `GET /auth/session` and is held in memory only. The server must
  require `X-CSRF-Token` on **every** non-GET request, and must not accept `login` from another site (`SameSite=Strict`
  on the cookie covers it; a cross-site login would otherwise sign a victim into the attacker's account).
- A restart of the server or an expired session must answer `401` with `code: "session_expired"` or `"unauthenticated"`
  on **every** protected path (also with an HTML body from a proxy: the panel treats any `401` as the end of the session).

### Routing

- Serve `index.html` for every path that is not a file and does **not** start with `/api/`, so that a reload on
  `/databases/shop` works. An unknown path under `/api/` must answer a JSON problem (`404 not_found`), never HTML.
- Path parameters arrive percent-encoded once (`/api/v1/databases/a%2Fb%3Fc%23d%25e` is the database `a/b?c#d%e`). The
  router of the server must decode a segment after splitting the path, and must not treat `%2F` as a separator. A
  name that is `.` or `..` (or empty) cannot be sent by a browser at all (the URL parser removes it, also written
  `%2E`): the panel refuses to send such a request and shows the object as "cannot be opened by address".
- Do not serve `mockServiceWorker.js` or any development file; the production build contains none (the browser test
  checks the bundle for the mock).
- Recommended in addition (not tested here): `Strict-Transport-Security: max-age=31536000` when the panel is served over
  HTTPS.

## Brand

The logos are the files the project owner supplied in `kingres/brand/` (read its `README.md` for variants, colorings
and rules). They are the source of truth and are not redrawn:

- `BrandLogo` (`src/components/BrandLogo.vue`) draws the logo as inline SVG with the exact path data from the SVG
  files. Props: `variant` (`mark`, `horizontal`, `stacked`), `size` (height in px) and `decorative`. The crown and
  "KINGRES" use `--logo-crown`, "DB" uses `--logo-db`, so the light theme shows the `color-light` coloring and the
  dark theme `color-dark`, with no background; the shield always has the gold gradient. Gradient and clip-path ids are
  unique per instance. The logo is named "KingresDB"; use `decorative` when the name is written next to it.
- `npm run brand:sync` copies `public/favicon.svg`, `favicon-32.png`, `apple-touch-icon.png` and `icon-512.png` from
  `kingres/brand/` and writes `src/components/brand.generated.ts` (the path data). The results are committed;
  `tests/brand-files.test.ts` fails when they differ from the source (run the script again).
- `public/manifest.webmanifest` and the `<head>` of `index.html` give the name, the icons and a `theme-color` for light
  and dark.
- Where it appears: the top bar (horizontal lockup, the mark alone below 860 px, links to the dashboard), the sign-in
  page (stacked), every step of the first-run wizard (horizontal), the splash screen while the application starts,
  and the not-found and not-allowed pages (mark).
- Brand colours are tokens in `tokens.css` (`--brand-*`). The gold is a decoration and logo colour only: the top bar
  strip, the sign-in card strip and the active navigation mark (`--c-brand`, a darker gold on light surfaces). It is never
  a text colour and never replaces the semantic status colours; tests check both.

## API types

`src/api/schema.d.ts` is generated by `openapi-typescript` from `kingres/api/openapi.yaml`, and
`src/api/operations.generated.ts` (operation id to `x-required-role`, method and path) by the same script
(`scripts/gen-api.mjs`). Both are committed and never edited by hand. `tests/contract.test.ts` regenerates them in
memory and fails when the committed files differ from the spec, so after changing the contract run
`npm run api:generate` and commit the result. The script also writes the closed list of audit actions. The same test checks that the mock API has a handler for every
one of the operations (the count is read from the spec).

## Project structure

```
src/
  api/          client.ts (typed fetch, CSRF and setup-token headers, unwrap), endpoints.ts (one function per
                operation), queries.ts (TanStack Query hooks and keys), problem.ts (Problem -> message, keyed
                on `code`), roles.ts (role checks from the contract), generated files
  stores/       Pinia: session, theme
  composables/  useTask.ts: follow a 202 + Task by polling /tasks/{id}; useTaskAction wraps start + follow + refresh
  router/       routes (lazy), guard: setup state -> session -> sign-in -> role
  layouts/      ShellLayout: top bar, navigation, banners
  components/   AppDialog, TaskActionDialog, ConfirmDialog, GateButton, QueryBoundary, ProblemNotice,
                TaskProgress, StatusBadge, LineChart, BucketGrid, TabBar, FormField, ... and security/, tables/
  pages/        one file per route
  lib/          format.ts, status.ts (status -> tone, icon, label), validators.ts, cluster.ts, metrics.ts,
                config-value.ts (the checker of a parameter value, shared with the mock), config.ts (words, clock states, filters)
  styles/       tokens.css (the only place for colours, sizes, fonts), base.css (shared classes)
  mocks/        the in-memory cluster (state.ts), request handlers (handlers.ts, http.ts), browser and node setup
  bootstrap.ts  builds pinia + query client + router, and the one end-of-session path (used by main.ts and the tests)
tests/          Vitest; helpers.ts renders the real app against the mock API
browser-tests/  Real-browser tests (puppeteer-core, own vitest config): layout, keyboard, axe, content security policy
scripts/        gen-api.mjs
```

## Conventions

- **Server state** only through TanStack Query (`src/api/queries.ts`); components never call `fetch`. Pinia
  holds only the session and the theme. Polling: cluster-wide status every 3 s, lists every 10 s; background
  tabs are not polled.
- **Errors** are `ApiError` (RFC 9457 `Problem`). `describeProblem` is the one place that maps `code` to text,
  tone and retry advice; `Record<ProblemCode, ...>` makes the compiler fail if the contract adds a code without
  a message. `unsupported_operation` is shown as calm information ("Not available on this server version yet").
- **Roles:** the role an operation needs comes from `x-required-role`. Navigation items are hidden when the role is
  too low (route `meta.operation`), actions use `GateButton op="..."`, which stays visible but `aria-disabled`
  with the reason. The backend enforces the same rules; the UI is a convenience, not a security boundary.
- **Setup** never uses `GET /tasks/{id}` (it needs a session): the wizard checks the token with
  `POST /setup/token-check` and follows `setupTask` in `GET /setup/state`.
- **`null` means "not available":** use `NullableValue` / `NotAvailable` (shows "n/a", read as "Not available"), never
  a formatter that turns null into 0.
- **Tasks:** long operations return 202 + `Task`. `TaskActionDialog` states the consequence, asks for confirmation,
  follows the task with `useTask` (progress bar, steps, `aria-live` summary) and invalidates the affected queries.
- **Accessibility:** semantic HTML, every control has a label (`FormField`), dialogs trap focus and close on
  Escape, status is icon plus text, `aria-live` for progress and errors, visible focus ring, tabular numerals.
  Contrast of the token pairs is tested in both themes.
- **Design tokens:** use `var(--...)` from `tokens.css`; do not write colours or font sizes in components.
- **Names and messages from the database** (roles, databases, schemas, tables, columns, server groups, and the text of
  logs, alerts, audit entries, tasks and errors) are chosen by whoever may create them, so they are shown only through
  `SafeName` (or `safeText()` where markup cannot go, such as an `<option>` or an `aria-label`). It makes bidirectional
  controls, zero-width characters and control characters visible as `U+202E`-style marks with a description, isolates the
  text direction, flags names that mix scripts or use look-alike letters, and folds very long values with "Show all N
  characters". Confirmations show the escaped name and still require the real one (with a "Copy the name" button).
  Nothing is stripped or normalised before it is sent. `tests/hostile.test.ts` goes through every page with hostile data.
- **One end of session.** Every way a session can end (sign-out, expiry seen by any query, mutation or task poll, a
  different user in the session, another tab signing in) goes through `session.adopt`, which calls
  `resetServerState()` (`src/api/server-state.ts`): the query cache, the in-flight requests, the task pollers and the task
  notices are cleared or cancelled. A new store that holds API data registers its own reset there.
- **Path parameters** are encoded in one place (`pathSerializer` in `src/api/client.ts`). A value that no URL can carry
  (`.`, `..`, empty) is refused with an error before any request is made.
- **Forms.** A form whose action changes the whole cluster, or cannot be undone, has no implicit submission
  (`v-block-enter-submit` from `src/lib/forms.ts`) and a button that opens a confirmation; dialog forms with their own
  visible submit button (create role, create server group, sign-in) keep Enter. Fields that were not edited are not sent.
- **Dates** from the API are UTC and are shown as UTC (`formatDateTime` adds "UTC"); a date field says "(UTC)" and
  is read as UTC; an untouched date is not sent back.
- **Partial cluster-wide answers (0.5.0).** `listLogs`, `listAlerts` and `queryMetrics` answer for the whole cluster; a
  server that did not answer is named in `missingServers` with a reason. The views show `PartialNotice` ("This list is
  incomplete", every server and the reason in plain words, server ids through `SafeName`), announce it once through the
  live region (`useCompletenessAnnouncement`: when the set of missing servers changes, not on every poll), remove it when a
  later answer is complete, and never show a partial empty list as "No alerts" or "No log lines" (the empty state says
  "from the servers that answered" and that this does not mean there are none).
- **Cursors (0.5.0 changed their format).** A cursor lives only in the memory of one query (`usePagedQuery`): never in the
  address, never in browser storage, so a reload or a new session starts at the first page. A 400 for a request that
  carries a cursor is not an error dead end: the list starts again from the first page and says so
  (`CursorResetNotice`); `fetchAllPages` restarts once, silently.
- **Alert ids** are integers below 2^53 and are never parsed, rounded or re-serialized by the panel (tested with 2^52+1);
  an id that a JSON number cannot hold exactly is not acknowledged from the panel.
- **Browser storage: only the theme.** `localStorage` holds `kingres.theme` and nothing else; `sessionStorage` holds nothing
  (0.6.0 removed the copy of the session and CSRF token and the saved instance id: see "What the panel does for contract
  0.6.0"). The router's own scroll key is removed after every navigation, and the transient key of the cross-tab channel is
  removed at once; the stopped state is told to the other tabs (`BroadcastChannel`, or a `storage` event where there is none):
  one boolean, no session, no token. `tests/degraded-session-0-6.test.ts` scans both stores after sign-in, in the degraded
  state and after sign-out.
- **Text** is English and kept next to the feature that shows it. Every source file starts with an
  `SPDX-License-Identifier: AGPL-3.0-only` comment (checked by a test).

## Adding a page

1. Add the operation(s) it needs to `src/api/endpoints.ts` and a hook (with a key) in `src/api/queries.ts`.
2. Create `src/pages/FooPage.vue` (`<script setup lang="ts">`). Wrap data in `QueryBoundary` so loading, error,
   "not available yet" and empty states are handled; use `GateButton` for actions.
3. Register the route in `src/router/index.ts` with `meta: { title, operation, nav }`; the navigation entry and the
   role check follow from `operation`.
4. Add the endpoint handler to `src/mocks/handlers.ts` (the contract test fails otherwise), add the page to
   `tests/pages-a11y.test.ts`, and add behavioural tests for what is special about it.

## Testing

`npm test` runs everything in about 15 seconds. The tests render the real app (router, pinia, query client)
and talk to the mock API through MSW in Node, so the same handlers serve `npm run dev` and the tests.

| File                                                                                                                           | Covers                                                                                                                                                                                                                                                                                                                                                                                       |
| ------------------------------------------------------------------------------------------------------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `problem.test.ts`                                                                                                              | Problem to message mapping for every code, field errors, retry policy                                                                                                                                                                                                                                                                                                                        |
| `useTask.test.ts`                                                                                                              | Polling, progress, failure, paused, tolerance of lost polls, cleanup                                                                                                                                                                                                                                                                                                                         |
| `roles.test.ts`                                                                                                                | Role hierarchy from the contract, hidden navigation, disabled actions with reason, forbidden page                                                                                                                                                                                                                                                                                            |
| `create-database.test.ts`                                                                                                      | Four groups, immutable-field notes, validation, submit, task, list update, focus handling                                                                                                                                                                                                                                                                                                    |
| `setup-wizard.test.ts`                                                                                                         | Token, create, join, validation, wrong token, task, hand-off to sign-in                                                                                                                                                                                                                                                                                                                      |
| `servers-page.test.ts`                                                                                                         | Status badges (icon and text), instances, failover and add-server flows, unsupported and error states                                                                                                                                                                                                                                                                                        |
| `shards-grid.test.ts`                                                                                                          | Bucket grid for a given bucket map, filters, bucket lookup                                                                                                                                                                                                                                                                                                                                   |
| `pages-a11y.test.ts`                                                                                                           | Every page: renders data, logs no Vue warnings, no axe violations                                                                                                                                                                                                                                                                                                                            |
| `flows.test.ts`                                                                                                                | Sign-in/out, CSRF, expired session, add server then rebalance, workbench, security, ledger, settings, theme, distribute table, drop database                                                                                                                                                                                                                                                 |
| `tokens-contrast.test.ts`                                                                                                      | WCAG AA contrast of the token pairs in the light and dark theme                                                                                                                                                                                                                                                                                                                              |
| `contract.test.ts`                                                                                                             | Generated files up to date, mock covers every operation in the spec (count read from the spec), audit actions, SPDX headers                                                                                                                                                                                                                                                                  |
| `session-reset.test.ts`                                                                                                        | Every way a session ends clears the client (sign-out, expiry on a query, a mutation or a task poll, another user, another tab)                                                                                                                                                                                                                                                               |
| `api-client.test.ts`                                                                                                           | Exact request URLs for hostile path parameters, answers that are not problem documents, unknown problem codes, the sign-in redirect allow-list                                                                                                                                                                                                                                               |
| `task-notices.test.ts`                                                                                                         | A hidden task is still followed and its failure reported; typed confirmations for remove server, hard failover, disable role                                                                                                                                                                                                                                                                 |
| `network-access.test.ts`, `network-rules.test.ts`                                                                              | Network rules: no Enter submission, confirmation with the changes, lock-out warnings, lists that round-trip                                                                                                                                                                                                                                                                                  |
| `grants.test.ts`, `role-dialog.test.ts`                                                                                        | A failed grant is not shown as granted, template confirmation, an unchanged role is not sent (also under another time zone)                                                                                                                                                                                                                                                                  |
| `workbench.test.ts`                                                                                                            | NULL, the text NULL and the empty string, stale results, timeout, stop waiting, large results, duplicate columns                                                                                                                                                                                                                                                                             |
| `plumbing.test.ts`                                                                                                             | Unreadable ledger status shown, cursor paging with a bound and a "more results" notice                                                                                                                                                                                                                                                                                                       |
| `hostile.test.ts`                                                                                                              | Every list and detail page and dialog with hostile names and messages                                                                                                                                                                                                                                                                                                                        |
| `forms.test.ts`, `dialogs.test.ts`, `task-poller.test.ts`                                                                      | Enter in forms, what is sent on save, inert page behind a dialog, live regions, no polling in a hidden tab                                                                                                                                                                                                                                                                                   |
| `dependencies.test.ts`                                                                                                         | `DEPENDENCIES.md` names every package of the production tree and every exceptional development package                                                                                                                                                                                                                                                                                       |
| `server-headers.test.ts`                                                                                                       | The values in "What the server must send" are the ones the browser test uses                                                                                                                                                                                                                                                                                                                 |
| `contract-0-2.test.ts`                                                                                                         | Contract 0.2.0: stats-only landing page, n/a for null, create options, distribution columns, server groups, report detail, audit filter                                                                                                                                                                                                                                                      |
| `ledger-console-0-11.test.ts`                                                                                                  | The Ledger page of 0.11.0 (Console A): the tab bar, the status with publication and anchoring, the signed column, the aggregate dialog, the Signing tab (first key with the origin prefix confirmed, rotation, retirement, revocation with the fingerprint typed, failed key tasks in words, the trust file as sent), the Publication tab, the dashboard card, the alerts, hostile text, axe |
| `ledger-0-11.test.ts`                                                                                                          | Contract 0.11.0: words for the nine alert kinds, sixteen audit actions, eight task kinds, the task subject `ledgerKey` and five problem codes; the mock of the 28 new operations checked against the schemas of the contract and against what the contract refuses; the alerts of the new kinds; `?phase7=today` is the real server of today                                                 |
| `failover-0-10.test.ts`                                                                                                        | Contract 0.10.0: words for the seven alert kinds, five audit actions and two problem codes; the mock of the four new operations (status, reports, report, counter reset) and of a dry run, checked against the schemas of the contract; the decision about data loss; a request that starts a failover narrows its answer to a task                                                          |
| `failover-dialog-0-10.test.ts`                                                                                                 | The failover dialog: the mode by the state of the server, the groups and the standby per group, the plan with the verdict in words, the decision that sets `allowDataLoss`, the typed address, the refusals of the dry run and of the start in words of their own, keyboard and focus                                                                                                        |
| `failover-progress-0-10.test.ts`                                                                                               | The progress and the report: the steps in a block per group, the phase and the lease wait, Cancel before and after the promotion, a run that waits for a decision and goes on, a failed group, a partly done run, the map group's bootstrap, an automatic run, the report with its numbers                                                                                                   |
| `failover-views-0-10.test.ts`                                                                                                  | The verdict in words for every verdict, the readiness line of a group, every refusal code with words of its own, the plan table, the Tasks page and the alerts, what each role sees and does (8.6), hostile text                                                                                                                                                                             |
| `failover-mock-0-10.test.ts`                                                                                                   | The mock states of the design (8.7): every URL switch reaches its state, and what the mock answers satisfies the schemas of the contract; what the real server refuses for now is refused                                                                                                                                                                                                    |
| `failover-console-part2.test.ts`                                                                                               | The failover console, part 2: the banner, the settings and the counter with its reset, the limit on the dashboard and in the alerts, the roles, the new mock states                                                                                                                                                                                                                          |
| `ledger-anchor-lib-0-11.test.ts`, `ledger-anchoring-0-11.test.ts`, `ledger-anchoring-mock-0-11.test.ts`                        | Console B: the words and forms of the three kinds of target, the Anchoring tab in every state, the dialogs, write-only credentials, the test, records, retry, evidence, each problem code, roles, the settings of anchoring, alerts and card, the mock switches and `?phase7=today`                                                                                                          |
| `failover-a11y-0-10.test.ts`                                                                                                   | axe on the dialog in each step, the progress card running, waiting and failed, the report, the Replication and Tasks pages                                                                                                                                                                                                                                                                   |
| `contract-0-4.test.ts`                                                                                                         | Contract 0.4.0: every problem code has words and a mock status, new required properties of Task and Server                                                                                                                                                                                                                                                                                   |
| `servers-0-4.test.ts`                                                                                                          | Certificate and fingerprint display, expected fingerprint (add, join), server groups (create, assign, refused delete), remove server                                                                                                                                                                                                                                                         |
| `ledger-0-4.test.ts`, `raw-json.test.ts`                                                                                       | Proof and digest files byte for byte, pending proofs, refusals, verification list and detail                                                                                                                                                                                                                                                                                                 |
| `this-server-instances.test.ts`, `system-database-stopped.test.ts`, `local-settings.test.ts`, `leave-cluster.test.ts`          | Instance start, stop and restart with their refusals, the stopped-system-database state, local settings, leave cluster                                                                                                                                                                                                                                                                       |
| `certificates-0-5.test.ts`                                                                                                     | Renew now (ordinary confirmation, task), Revoke certificates (what happens, typed server id, nothing pre-ticked), revoked marks, blocked actions with reasons, the two new problems                                                                                                                                                                                                          |
| `partial-answers.test.ts`, `alerts-0-5.test.ts`                                                                                | Partial notices on Logs, Alerts and the dashboard, announced once; alert kinds and links; alert ids above 2^52; cursors never kept and a 400 starts again                                                                                                                                                                                                                                    |
| `ledger-0-5.test.ts`, `system-state-sharing.test.ts`, `hostile-0-5.test.ts`                                                    | Digest paging, digest against proof (identifiers only), the stopped state across tabs, the hostile seed over the 0.5.0 places                                                                                                                                                                                                                                                                |
| `review-0-5.test.ts`                                                                                                           | One test per finding of the second review (W2-01 to W2-16 and the residuals), named by finding                                                                                                                                                                                                                                                                                               |
| `task-detail.test.ts`, `kind-labels.test.ts`, `role-guards.test.ts`, `unsupported-operations.test.ts`, `workbench-0-4.test.ts` | Task detail text, kind labels, log access needs `kingres_ops`, unsupported operations stay calm and start working when supported                                                                                                                                                                                                                                                             |
| `pin-and-fingerprint-0-6.test.ts`, `roles.test.ts`                                                                             | Add server needs kingres_admin (403 explained), the pin or the explicit unpinned choice, the 400 at /expectedFingerprint, own fingerprint, setup detail, internal_api_disabled                                                                                                                                                                                                               |
| `degraded-session-0-6.test.ts`                                                                                                 | Reload while stopped through the degraded session, the instance from the list, disabled actions with a reason, web storage holds only the theme (sign-in, degraded, sign-out)                                                                                                                                                                                                                |
| `network-dry-run-0-6.test.ts`, `audit-0-6.test.ts`, `alerts-settings-0-6.test.ts`                                              | Dry run before saving network rules, clientAddress, audit server column, partial notice and cursor restart, 503 audit log, replicationPending, default paths                                                                                                                                                                                                                                 |
| `workbench-0-7.test.ts`, `alerts-0-7.test.ts`, `ledger-status-0-7.test.ts`                                                     | 0.7.0: statement ids, Cancel statement and Stop waiting, `workbench_busy` and Retry-After; alert subjects and the new alert kinds; coverage of the ledger status                                                                                                                                                                                                                             |
| `leases-0-7.test.ts`, `liveness-0-7.test.ts`, `standbys-0-7.test.ts`                                                           | 0.7.0: leases on Replication and the server page, heartbeats (suspect, dead, time since the last one), `leader_unavailable`, the commit rule, the replica count dialog and the task with its skipped step                                                                                                                                                                                    |
| `config-value.test.ts`                                                                                                         | 0.8.0: the shared test vectors of the value checker (booleans, integers, units of `kB` and `8kB` and `ms`, ranges, reals, enums, strings), the messages                                                                                                                                                                                                                                      |
| `config-mock-0-8.test.ts`                                                                                                      | 0.8.0: the mock of the tag Configuration: catalogue, managed, refused, hidden, cluster-only, per-server values, preview, change, failed and waiting servers, retry, history and revert, restart and its order, clocks, summary, roles                                                                                                                                                        |
| `config-page-0-8.test.ts`, `config-dialog-0-8.test.ts`                                                                         | 0.8.0: the page (every state, every role, the filters in the address, paging, the four server states) and the dialogs (levels, live validation of every type, preview, apply, retry, remove an override, revert and superseded, restart)                                                                                                                                                     |
| `config-time-0-8.test.ts`, `config-hostile-0-8.test.ts`                                                                        | 0.8.0: the Time tab, the dashboard card and its links, the server section, the alert; every place a value or name is shown with hostile data                                                                                                                                                                                                                                                 |
| `audit-metrics-0-7.test.ts`, `a11y-0-7.test.ts`                                                                                | 0.7.0: audit labels and filter, the client entry point group; axe and live regions for the new states                                                                                                                                                                                                                                                                                        |
| `ledger-0-6.test.ts`, `workbench-0-6.test.ts`                                                                                  | Passed-but-incomplete badges, uncovered shard groups in list, dialog and next to downloads, LedgerProofBlock, truncated DML was applied, timeout and size limits                                                                                                                                                                                                                             |

jsdom has no layout engine, so axe cannot compute colour contrast there; that is why contrast is tested from the
tokens instead.

## Contract revisions and the checks against them

The generated files (`src/api/schema.d.ts`, `src/api/operations.generated.ts`) follow contract **0.12.0** (the last one generated from, in commit "the panel follows contract 0.12.0"; the newest note of a revision in them is "Added in 0.12.0"). The revision of `kingres/api/openapi.yaml` is now **0.13.0** (the switchover of a group, the failover of the map group, the alert `certificateAuthorityUnavailable`), which the generated files do not contain yet. `npm run api:check` and the contract tests compare
with that file. While the working tree holds a draft of a later revision they fail, and that failure says nothing about the
panel; to check against another copy of the contract (for example the committed one), point `KINGRES_API_SPEC` at it:

```
git show HEAD:kingres/api/openapi.yaml > openapi-copy.yaml   # outside the repository
KINGRES_API_SPEC=openapi-copy.yaml npm run api:check
KINGRES_API_SPEC=openapi-copy.yaml npm test
```

`scripts/gen-api.mjs` and the contract tests read the same variable, so `npm run api:generate` with it set regenerates from that
copy. Adapting to the next revision is a separate step.

### The failover console, part 3 (increment 8, after increment 7: the switchover and the restore)

- **Switch over...** (`ReplicationPage.vue`, on every group card, `GateButton op="failoverServer"`, `kingres_ops`): the planned switchover of that one group. It is
  `FailoverDialog` with the prop `group`: the mode is graceful and fixed, the server is the one that runs the primary of the group, only the standby is chosen
  (the default is the one that holds the most WAL), then the plan (a dry run with `shardGroupIds: [group]`), then **Switch over**. No typed address and no decision about
  lost data: a switchover loses no commit (`allowDataLoss` is ignored by it). The button is blocked with the reason when the group has no primary, no standby, or the
  servers are not read yet.
- **Restore preferred primaries** (header of the Replication page, `kingres_ops`): the header says how many groups are away from their preferred server and the button is
  blocked with the reason when none is. `RestorePrimariesDialog.vue` has its own plan view: the groups that are away (from server X back to server Y; the map group says it moves
  the manager), then the plan (`previewRestore`, a dry run: the time writes pause, the steps, the blockers), then **Restore primaries**. All of the groups that are away are sent
  as no list (so that a group that comes back meanwhile is not left out by a stale list); a group left out is sent as `shardGroupIds`.
- **Refusals** in words, in the dialog, for the plan and for the start, with the same codes as a failover (`FailoverRefusal`): `quorum_unavailable`, `lease_settings_differ`,
  `rebalance_in_progress`, `failover_in_progress`, `operation_in_progress`, `no_replica_available`, `version_mismatch`. A blocker of the plan is listed and the start stays disabled.
  `unsupported_operation` is the calm "Not available on this server version yet": for a restore it says that restoring the preferred primaries is not part of this server version
  yet, for a switchover that the planned switchover of a group is not (without sending the person to the hard mode), and "Nothing was changed".
- **Mock.** A restore plan carries the blockers of `?failover=blocked:lease` and `blocked:rebalance` (and `blocked:quorum` is a refusal); a restore start is refused with
  `failover_in_progress` while a failover runs and with the blocker's code; `?unsupported=restorePreferredPrimaries` is the server of today for the restore; the switchover
  is "not available" until `?failover=graceful`. Tests: `failover-console-part3.test.ts`; browser states: the switchover plan, the switchover not available, the restore plan.

### The Ledger console, part C (step 19 of the plan of phase 7: the Client keys tab)

The Ledger page has a sixth tab, **Client keys** (`?tab=clients`, after Anchoring; cluster-wide like Anchoring, with its own database choice). Components under
`src/components/ledger/`, words and checks in `lib/ledger-client.ts`:

- **Keys** (`ClientKeysTab.vue`): the public keys that clients registered to sign the rows they write, with label, key id, a short public key, the role (or "Any role"), who
  registered it and when, and active or revoked (with the reason). The text says what a client key is for and what an attestation does not prove (the application must
  recompute the row hash itself; the label and the role say nothing about who holds the key). **Register a key** (`RegisterClientKeyDialog.vue`): the public key as 32 bytes of
  standard base64, an optional role, a label of 1 to 100 characters; there is no field for a private key and the dialog says where the key pair is made. **Revoke**
  (`RevokeClientKeyDialog.vue`): a reason of 1 to 200 characters and the words of what a revocation means (entries signed before stay valid; a revoked key is never
  registered again).
- **The signature policy of a table** (`SignaturePolicyDialog.vue`): the ledger tables of the database and **Require signatures...**. Before the click the dialog says what it does
  and what it cannot undo: from then on a transaction that writes the table commits only with an attestation for every entry (otherwise it fails at COMMIT with KL010); **the
  switch waits for the open writers of the table** (every open transaction that wrote it, a prepared one until it ends, and later writers wait for the switch; the wait is bounded and
  a switch that is not over in time fails and changes nothing); entries committed before the switch are not asked for an attestation; it cannot be switched back; the table
  name is typed. While the request is open the dialog says that it waits.
- **The upgrade of the extension** (`UpgradeExtensionDialog.vue`): **Upgrade the extension** starts the task `ledger.upgrade` (ALTER EXTENSION kingres_ledger UPDATE in every shard
  group); the dialog says that a group that cannot be upgraded fails the task and its error names the server.
- **Refusals** in words (`clientRefusal`, `LedgerRefusal.vue`) for each action and code: `already_exists` (a key is registered once; a revoked key never again), `conflict` (a key that
  is revoked already, a policy that is required already: told as information, and the table is then shown as Required), `unprocessable` (not a ledger table), `not_found`,
  `operation_in_progress` (an upgrade runs), and `unsupported_operation` (the extension is older than 0.3, the server has no OpenSSL, or this server version does not build it yet): the
  calm "not available" with "Nothing was changed". Any other code falls back to the generic words.
- **Roles.** Everybody who may read the ledger reads the keys and the tables; Register, Revoke, Require signatures and Upgrade need `kingres_admin`: a role that may not sees the
  button disabled with the reason.
- **Mock.** `?clientkeys=keys` (three keys: any role, bound to a role, revoked), `legacy` (the extension is older than 0.3: every client-key operation answers 422
  `unsupported_operation`), `nossl` (a key cannot be registered), `refuse:<code>` (every change and the upgrade answer that problem code); the registration checks the key as the
  registry does (32 bytes; the identity point and its aliases of small order are refused at `publicKey`), that the role exists, and never registers a key twice; `?phase7=today` is the
  server of today (all of it answers `unsupported_operation`); `?mock=hostile` adds keys with hostile labels, roles, registrars and reasons; `__kingresMock.urlSwitch(name, text)`
  applies any URL switch while the page is open.
- **Waits for the contract or the server.** The contract's `TableLedger` has no member for the signature policy of a table (and the extension has `signature_required_from`), so the
  console cannot show the policy of a table it did not switch: the column says "Not reported" until this session switched it or the server said it is required already. The
  extension version of a database (or of each group) is not in `LedgerStatus`, so the upgrade button is always offered to an administrator (design: shown when a group reports 0.2) and
  the dialog says what it does for a group that is on the newest version: nothing. The error code of a switch that timed out waiting for writers (`55P03`) has no problem code of its own in the contract.
- **Tests.** `ledger-client-0-11.test.ts` (every state, the dialogs, the refusals, the roles, hostile text, axe, the library); the tab is in `pages-a11y.test.ts` and `hostile.test.ts`; the
  browser tests have the route and four states (the tab, the register dialog with its errors, the policy dialog, the revoke dialog).

### The Ledger console, part D (step 22 of the plan of phase 7: bundle, acknowledgement, audit, checks)

- **Bundle** (`BundleDialog.vue`; **Create a bundle** on the Digests tab and on the Publication tab): which logs (all, or chosen from the published ones), only the part after a size, whether the
  anchor evidence goes in, and optionally the inclusion proof of one row with the warning that a proof shows the hash of the row to everyone who holds the bundle. A task makes it; when it is done
  the dialog offers **Download the bundle** (kept 24 hours; saved as the bytes the server sent). The contract has no member for the result of a task, so the bundle id is read from the task detail
  ("bundle N"); without it the dialog says to look on the Tasks page. `ledger_not_published`, `not_found`, a bundle that expired (410) and `unsupported_operation` have words (`lib/ledger-ops.ts`).
- **Acknowledge a diverged chain** (`AcknowledgeDivergenceDialog.vue`; the section "Chains that diverged" of the Overview tab, shown while the status says the publication diverged, one button per group,
  groups from the open alerts `ledgerChainDiverged`): a danger dialog that says what acknowledging means for the history (nothing is repaired or hidden; the old log is closed as it is and a new epoch
  starts; the acknowledgement is part of the history; the alert goes away, the fact does not; it cannot be undone), asks what happened (restore, failover loss, other) and a reason of 1 to 200
  characters, and needs the database name typed (sent as `confirm`). `chain_not_diverged` is told as information. Needs `kingres_admin`.
- **The alert `ledgerChainDiverged`** names the shard group, the height and why the log ended in its own text (also for a closed log that answers again: `?publication=reopened`); the alert list
  adds a note and the link "Acknowledge the divergence on the Overview tab".
- **Audit of the publication** (`PublicationAudit.vue`, Publication tab): **Audit the publication now** starts a verification of the chosen database with `checks: ["publication"]` and scope full, and
  says what it does (re-reads every chain from block 0; the server also does it daily); needs `kingres_ops`.
- **Verification with checks** (Overview tab): checkboxes for the chain, the publication and the anchors with words for each; the chain alone is the default and sends no `checks`; none chosen blocks the start.
- **Mock.** `?bundle=expired` (a bundle answers 410), `?bundle=refuse:<code>` (a bundle or an acknowledgement answers that code), `?publication=reopened`; `?phase7=today` is the server of today.
- **Waits for the contract or the server.** The result of the bundle task (an id) is read from the task detail; a verification record has no member that says which checks ran; one proof per bundle is offered
  (the contract takes a list). Tests: `ledger-ops-0-11.test.ts`; the browser tests have three new states (the bundle dialog, the acknowledgement dialog, the audit card).

### The table page and the table lifecycle (increment 9.7 of phase 9, contract 0.14.0)

A table of a database has a page of its own, `/databases/<name>/tables/<schema>/<table>` (opened from the list of tables), and every change of its distribution is a dialog that shows what the change would do before it starts.

- **The page** (`TableDetailPage`): the class, rows, size, home group or distribution (method, column, buckets, colocation group), `Table.partitioning` (RANGE or LIST, the key, the number of partitions), the columns, the partitions, and the changes of the table that run or have run (`TableTasksPanel`).
  The buttons are "Distribute table" (a plain table), "Undistribute table" and "Change the distribution" (a distributed or reference table); a role below kingres_admin sees them disabled with the reason, and so does a table whose change is running (task named) or that is
  user-partitioned (the contract refuses undistribute and alter for it in this version).
- **The preview** (`previewTableChange`, `TableChangePanel`, `useTableChange`): read again, a little after the last keystroke, whenever the dialog's request changes. It lists the blockers (the lines that would make the server answer 422 `table_not_eligible`; they keep the start button off), the warnings, the notes, the tables the change
  touches (the table, the tables of its colocation group, the tables linked by foreign keys) and the estimate (rows, bytes, the mode the server would choose, whether an online copy is possible and why not, who waits). A server that cannot preview (422 `unsupported_operation`) does not stop the change. The lines are shown in words by their code when the panel knows it (`lib/tables.ts`) and as sent when it does not.
- **Distribute** (`DistributeTableDialog`): the rows that will be copied, the method, the column, the colocation, the bucket count of a new group (1 to 4096), the mode (automatic, online, blocking; online is offered only when the preview says it is possible) and the views (refuse, or make them again). Only what differs from the defaults is sent.
  **Undistribute** (`UndistributeTableDialog`): the shard group that receives the table, the tables linked by foreign keys together or not, the mode and the views. **Alter** (`AlterDistributionDialog`): the column, the bucket count (a multiple of the old one moves no row), the colocation (keep, a group of the same type and bucket count, a group of its own, the group of a table), the tables of the colocation group together (blocking only), the mode and the views;
  at least one of the first three has to change.
- **Partitions**: the list (bound, default, leaves, size on this server), "Create partition" (the name of at most 57 bytes, the bound as the text after FOR VALUES or DEFAULT, "do nothing when it exists"), "Attach a table" (only distributed tables of the same colocation group and column are offered), "Detach" (no row moves) and "Drop" (the rows are deleted; the name of the partition is typed). A name that exists is 409 `already_exists`; a partition that does not exist is 404.
- **Tasks** (`table.distribute`, `table.undistribute`, `table.alter`, `table.partition`): the dialog follows the task with its steps; "Cancel the task" is offered while the task can be cancelled (a copy until its switch) and a cancelled task says that the table is as it was; a failed task shows the message, the step and the code of the server. After the dialog is closed the page shows the running task with Cancel,
  the last failure in words (it is dropped once a later change finished) and the last change that finished. `TaskActionDialog` has two new props for this, `cancelable` and `cancelledText`.
- **Contract and generated files.** The page uses the generated types and operations of contract 0.14.0 as they are in `src/api/schema.d.ts` (no stand-in types). The mock handlers of the eight operations call `src/mocks/tables.ts`. `Table` has no `operation` field in 0.14.0, so the list of tables has no progress indicator on a row; the table page finds an unfinished change through the task list (`useTasks`).
- **Mock.** `src/mocks/tables.ts`: the preview from the state (a dependent view, a trigger, a foreign key between `orders` and `customers`, a column outside the unique key, a user-partitioned table, a busy group), 422 `table_not_eligible` with the same lines, 409 `operation_in_progress` while the table or its colocation group is held, the four task kinds with their steps,
  and the partitions. The switches `?tables=...` are in the table above. Tests: `tables-0-14-mock.test.ts` (the contract) and `tables-0-14-ui.test.ts` (the page and the dialogs); the browser tests have the route of a table and four states.
- **Not in the console yet: "distribute with related tables"** (increment 9.7, the control plane). It needs, in the contract, an operation that takes a list of tables with a column for each and answers a task of its own kind (with the order the tables are distributed in and the keys that were dropped and re-added), a preview of it (the foreign keys that
  would be dropped, the order, the window in which no key is enforced, the blocking variant that shortens it), and the problem codes for a failure half way (which keys were re-added). The console would add a dialog on the database page (choose the tables, a column for each, the blocking variant), the preview of it in `TableChangePanel` and the task states above. Nothing of it is built here.

### The analytics console (phase 12, step W1 and W2, contract 0.18.0)

Approximate aggregates, the storage format of a table and the time-partitioning of a table are on the database page, the tables list, the table page and the workbench. The generated files follow contract 0.18.0 (11 operations, commit `7d43a79a3f`).

- **Approximate aggregates** (`ApproximateAggregatesCard`, `ApproximateAggregatesDialog`; `getApproximateAggregates`, `configureApproximateAggregates`): a card on the database page next to the ledger card. It shows the state (on or off), the extension version, the mode of count(DISTINCT) with its error rate in percent, and a table of the servers (`SafeName` for the ids, a state in words: installed, not installed, different version, not reachable). "Configure" (kingres_admin; the others see the card and the button disabled with the reason) opens a dialog with a switch, the mode (exact or approximate) and the error rate (0.05 to 10 percent, sent as the fraction 0.0005 to 0.1). It says what approximate means, that turning it off never removes the types hll, topn and tdigest, and follows the task `database.approximateAggregates` with its steps. `extension_unavailable`, `extension_version_mismatch` and `sketch_hash_mismatch` have their own words in `api/problem.ts`. A server that does not know the operation (422 `unsupported_operation`) shows calm information.
- **The Storage column and filter** (`StorageBadge`, `lib/analytics.ts`): Row, Columnar, or `Mixed (3 columnar of 31)` for a partitioned table; a columnar table also shows its ratio (`6.2 x`) and its bytes on disk, `n/a` when the ratio is null (never 0); a table whose `storage` is null says "Not available". The filter next to the list is made in the panel from `Table.storage` (a partitioned table matches Columnar when any partition is); the server's `storage` query parameter is not used because the whole list is read anyway. The list has "Convert storage..." and "Time partitioning..." for each table.
- **Convert storage** (`SetStorageDialog`; `setTableStorage`, `setPartitionStorage`, task `table.setStorage`): a table or one partition, Row or Columnar, the codec, the level (zstd, 1 to 19) and under "Advanced" the rows per stripe (1000 or more) and per chunk group (100 or more); a partition may also be sorted by columns. The notice depends on the kind (a plain table is rewritten under an ACCESS EXCLUSIVE lock and blocks readers and writers; a partition is swapped in one short step). What the console knows stops the start (a ledger table, a table that is partitioned, a distributed table in this version, a default partition); what only the server knows comes back as 409 `conversion_refused` with its reasons listed one by one. The task can be cancelled until its switch and a cancelled task says the table is as it was. There is no preview operation for a storage change in the contract, so index, trigger and publication reasons appear after the start is asked, not before.
- **Partitions panel** (the Partitions section of `TableDetailPage`): the format of each partition (a running conversion marks its row with the progress), the range, the size, the estimated rows, the ratio; a filter by format; "Convert storage" for a partition. For a table that is not distributed the create, attach, detach and drop buttons of 9.7 are not shown.
- **Time-partitioning** (`PartitionPolicyPanel`, `PartitionPolicyDialog`, `RunPolicyDialog`, `RemovePolicyDialog`, `PartitionRunDialog`, `PartitionActionsTable`; `getPartitionPolicy`, `putPartitionPolicy`, `deletePartitionPolicy`, `runPartitionPolicy`, `planPartitionPolicy`, `listPartitionPolicyRuns`, `getPartitionPolicyRun`): the panel shows the policy, the health line (partitions ahead, with a warning below what the policy keeps), the next and the last run, "Set a policy" or "Edit policy", "Run now" and "Remove policy", and the list of the runs (started, took, why, state, by, with a button that opens the actions of a run; a failed action says what happened in words for `lock_timeout`, `swap_lock_timeout`, `retry_later` and `name_conflict`, and any other detail is shown as sent). The dialog has the interval, how far ahead, the retention (drop or detach), when to convert and with what, the time zone and how often to run. **Preview** asks `planPartitionPolicy` with the policy of the form and lists what the next run would create, convert and drop, with sizes. A retention that is new or shorter (or detach turned into drop) deletes data at the next run, so it needs a preview of the form as it is now, and the table name typed; changing the form makes the preview stale and the save off again. A refusal of the server (`validation_failed` with JSON pointers) is shown next to each field; `partition_policy_conflict` and `extension_missing` are shown with their own words. A table that is not range-partitioned on a time column gets the explanation and the statement, and no form; a table with a default partition gets the refusal and the way out. "Run now" lists the plan first and warns about what it would drop.
- **Alerts**: `partitionsAheadLow` and `partitionPolicyFailed` have labels (from the contract revision) and `AlertSubject` links them to the table page when the alert's `aspect` is `table:<schema>.<table>`. The contract does not say what these alerts put in the subject (see the notes of the console); a qualifier that does not read like that is shown as sent.
- **Workbench** (`lib/plan-summary.ts`): a result whose first column is `QUERY PLAN` is shown as lines in a monospaced block, with a one-line summary above it read from the text the sharding extension prints (`Pushed:`, `Repartition:`, `Pushdown: not used:`, `Rewritten:`, `Remote groups:`): "Path: repartitioned join", "colocated join", "partial aggregate", "scan"; "Not used: <reason>"; "Rewritten: count(DISTINCT) -> approximate". The summary is a convenience and is never used for a decision; a text with no line it knows gets none. When the database has the approximate mode on, a help text lists the sketch functions. The note of the workbench says that EXPLAIN ANALYZE and MERGE run the statement too. This is also the phase 2b item "EXPLAIN path summary in the workbench".
- **Mock** (`src/mocks/analytics.ts`): the eleven operations as the contract says: tasks (`database.approximateAggregates`, `table.setStorage`, `partition.maintain`) with their steps, 422 `extension_unavailable`, 409 `extension_version_mismatch`, `extension_missing`, `conversion_refused` (reasons one per line), `partition_policy_conflict`, 409 `operation_in_progress`, 400 `validation_failed` with the JSON pointer of a policy field, 404 for a table without a policy, idempotent configure and put, a dry run that changes nothing and is not audited, the roles, and `unsupported_operation` for every one on an older server. A run does what its plan said (creates, converts, drops or detaches), a partial run raises `partitionPolicyFailed`. Switches: `?analytics=on` (approximate aggregates on in `shop`), `timeseries` (a table `clicks` that can be converted, and `metrics`, partitioned by day with a policy, three runs and two columnar partitions), `ahead-low` (one day ahead and the alert), `sketch:missing|mismatch|hash`, `columnar:missing`, `timepart:missing`, `convert:fail`, `run:lock|name|fail`, `publication`, `old`. `listTables` and `getTable` answer `storage`, and `partitioning` with `columnType`, `hasPolicy` and `defaultPartition`; `listTablePartitions` answers the format, the range, the estimated rows and the ratio.
- **Tests**: `analytics-lib-0-18.test.ts` (durations, the policy form, when a policy deletes, the storage column, the plan summary on the texts of 4.12 and 12 of 2b), `analytics-mock-0-18.test.ts` (the contract of the eleven operations), `analytics-ui-0-18.test.ts` (the card, the dialogs, the list, the table page, the policy and its runs, the alerts, the workbench, each role); `browser-tests/analytics-0-18.test.ts` runs nine states (the card, its dialog, the Storage column, the table page, the storage dialog, the policy dialog with a deleting preview, a run with its actions, the plan summary) through axe in light and dark at 1366 and 320 pixels, and checks that no page scrolls sideways and a dialog keeps its buttons in reach. The states are also in `STATES`, so the older axe, layout and mock loops run them.

## What the panel does for contract 0.12.0

Contract 0.12.0 adds no operation (132) and four small things that the panel follows: the new outcome `completedWithWarnings`, the plan in the problem
`failover_needs_decision` (`Problem.plan`), the corrected text of `OldPrimaryReport.state = notScheduled`, and the refusal `failover_in_progress` of a
database or table action.

- **`completedWithWarnings`** is shown like `completed` (green, a tick) with the label "Completed, with remarks" and the hint "The failover ended. The check of the cluster
  afterwards reported something: open the report to read it." Every place that shows an outcome (the report dialog, the Replication page, the Tasks page) uses
  `outcomeMeta`, and the report button is on every finished run.
- **The plan of the refusal.** `FailoverDialog` shows `Problem.plan` when the start is refused with 409 `failover_needs_decision` and asks for the decision from it; when the
  member is absent (a server of 0.11.0) it asks again with a dry run, as before. `FailoverProgress` uses the plan of a refused repeat for the verdict of the groups that wait
  (it wins over the report, which is older).
- **`notScheduled`** now reads "No rebuild of it is scheduled. It stays stopped and fenced (an earlier version of the server never rebuilt it, or it has no directory to rebuild)."
- **`failover_in_progress`** has neutral words in `api/problem.ts` ("A failover or a restore of the preferred primaries is running, and the cluster does not change its databases or
  its servers while it does. Nothing was changed. Wait until it has finished, then try again."), so create database, drop database and distribute table show them; a failover
  refused with `operation_in_progress` or `failover_in_progress` keeps its own words in `lib/failover.ts`.
- **Mock.** The refusal carries the plan (`?failover=noplan`: it does not); a finished run ends `completedWithWarnings` with `?failover=warnings`; create and drop database
  and distribute table are refused with 409 `failover_in_progress` while a failover or a restore runs. Tests: `contract-0-12.test.ts`; the browser tests have the state "failover report: completed with remarks".

## What the panel does for contract 0.11.0

Contract 0.11.0 (the ledger of phase 7: signing, publication and anchoring, `kingres/docs/design/ledger-phase7.md`) adds 28 operations (132
in all, 108 paths), nine alert kinds, sixteen audit actions, five problem codes, eight task kinds and the task subject `ledgerKey`, and
nothing that is not additive (`LedgerBlock.signed` now means that a signed checkpoint covers the block). Of the 28, `kingresd` implements seven
today (`getLedgerSettings`, `updateLedgerSettings`, `getLedgerSigning`, `createLedgerSigningKey`, `retireLedgerSigningKey`,
`revokeLedgerSigningKey`, `getLedgerTrustFile`); the other 21 answer 422 `unsupported_operation` until their step is built.

- **Words.** `lib/status.ts` has a label for every new alert kind (and a sentence that says what to do for each), audit action and task kind;
  `taskSubjectText` tells the subject `ledgerKey` / `signing` as "Signing keys of the ledger" (the Tasks page uses it); any other subject is
  shown as sent. `api/problem.ts` has a heading and an explanation for `ledger_not_published`, `origin_prefix_locked`,
  `signing_key_missing`, `last_signing_key` and `chain_not_diverged`. The subject of an alert about an anchor target (`aspect` `target:<id>`)
  reads "Anchor target <id>" when the id is a number; anything else is shown as sent.
- **Typed callers.** `api/endpoints.ts` has one function per new operation (the three key operations and the others that start a task return
  the `Task`; `getLedgerTrustFile` returns the text exactly as sent, so that the download can be saved unchanged).
- **The mock** (`mocks/ledger.ts`, `ledger-view.ts`, `ledger-data.ts`) answers all 28 operations with bodies that satisfy the schemas of the
  contract and refuses what the contract says it refuses: a key task at a time (409 `operation_in_progress`), `last_signing_key` for a
  retirement or revocation that would leave no key in force (a revocation never creates a key), the fingerprint typed to revoke
  (400 `validation_failed`), the revocation time clamped to between the creation of the key and now, the origin prefix locked once a key exists
  (409 `origin_prefix_locked`), `ledger_not_published`, `chain_not_diverged`, anchor credentials that are never returned. A private key, a seed
  or a passphrase is not part of any answer. The states are reached with `?signing=`, `?publication=`, `?anchors=` and `?phase7=today` (the table
  above); the alerts of the new kinds are raised from those states (a plain mock raises none). The status of a database carries
  `publication` and `anchoring`, a digest `aggregateSize`, the blocks `signed`.
- **What the mock cannot say.** A task has no `result` member in the contract, so a bundle is named in the `detail` of its task ("bundle 1")
  and nowhere else; the real server will have to say how a client learns the bundle id.

### The Ledger console, part A (step 9 of the plan of phase 7)

The Ledger page (`/ledger`) has a tab bar (`TabBar.vue`, as the Configuration page; the tab and the database are in the address:
`?db=shop&tab=signing`) and each tab is a component under `src/components/ledger/`:

| Tab         | Component                                                                             | What it shows and does                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| ----------- | ------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Overview    | `OverviewTab.vue`                                                                     | The status of a database (as before) with two lines new in 0.11.0: what is published (`LedgerStatus.publication`) and what is anchored (`LedgerStatus.anchoring`), and the notice of design 6.9 when no target is enabled ("Not anchored: this ledger can be rewritten undetected by anyone who controls this cluster and its backups"); the verification (as before); the blocks with a column **Signed** (a signed checkpoint of the chain log covers the block)                                                                                                                                                                                                                      |
| Digests     | `DigestsTab.vue`, `AggregateDialog.vue`                                               | The digests with the aggregate hash, its size and a button **Open aggregate**, and the column of the anchors that says "Not anchored" instead of a blank; the aggregate checkpoints (a page of ten, **Load more**, **Download checkpoint**); the row inclusion proof, which the page compares with the digests listed here (so it is on this tab). The aggregate dialog shows the size, the root, the members (chain, epoch, height, hashes, flags `stale`, `diverged`, `retired` in words), the groups it does not cover, the anchors, the statement of what it proves and does not prove (the server's own text, design 5.6) and the signed checkpoint with **Copy** and **Download** |
| Signing     | `SigningTab.vue`, `CreateKeyDialog.vue`, `RetireKeyDialog.vue`, `RevokeKeyDialog.vue` | The cluster identifier, where the private key lives (file or external signer; a warning when the key files are not sealed by a passphrase), the origin prefix, the keys with their state, dates and fingerprint **in groups of four characters**, the servers that hold the key, the signed events, the trust file (**Download trust file**: the server's own bytes, saved as `trust.json`) and the six things an operator must keep outside the cluster (design 2.3). Notices for an alarm of the signer, a discontinuity of the key ring, a missing key, no active key and the backup reminder (9.2)                                                                                  |
| Publication | `PublicationTab.vue`                                                                  | Whether the logs are published (**Turn publication on** first says what the publication reveals, design 4.9), the origin prefix (editable before the first key, locked after, with the reason), the public URL, the interval; the listener (a warning for plain HTTP and for none), the logs with size, last checkpoint and state, the addresses to give to auditors, and what is revealed and what is not                                                                                                                                                                                                                                                                              |

- **Keys.** The first key: the dialog shows the origin prefix of the settings (the default `<server address>/kingres-ledger` is only a proposal until
  now) and does not create the key until it is confirmed; a changed prefix is stored first (`updateLedgerSettings`), then the key is made, and
  the prefix is locked from then on (409 `origin_prefix_locked`). A rotation asks for the overlap in days (1 to 365). Retiring and revoking
  follow the existing confirmation patterns: a danger dialog with the consequences, and for a revocation the fingerprint typed (the server checks it
  too), the time since which the key is compromised (never later than now), a reason of at most 200 printable ASCII characters, and the words of
  design 3.3 about what a revocation means for old signatures. A key that no other active key replaces is not offered for retirement or revocation
  (a revocation never creates a key and the server would answer 409 `last_signing_key`); the button says why. A failed key task has words for its code
  (`key_files_remain`: the key is still on a holder, repeat the action; `passphrase_mismatch`: a holder cannot open its file; the others of the contract's
  list) next to the server's own text.
- **What is never asked, shown, stored or logged.** A private key, a seed or a passphrase: the console shows public keys, fingerprints and
  states only, the dialogs have no field for a secret (the only key field is the _public_ key of an external signer, when the mode is `command`),
  and the trust file is a download that the server provides.
- **An operation the server does not build yet** (`?phase7=today`) is shown as what it is: the calm "not available on this server version yet" notice (not an
  error of the operator, never a success) in the place of that part (the logs of the publication, the aggregate list, the aggregate dialog), and the
  rest of the tab keeps working.
- **The dashboard card "Ledger integrity"** (`LedgerSummaryCard.vue`, `lib/ledger-card.ts`): one line of state (`Off` when no database uses the ledger;
  `Not anchored`, information and never green, while a database has no enabled target; `Healthy`; `Attention` with the worst open alert of a ledger kind
  and the reasons), the age of the newest anchored aggregate, the chains read of the chains expected (not claimed when the server did not say), the last
  verification and its result, and links to the tabs. An account that cannot read the ledger status does not see it.
- **Alerts.** The nine new kinds have labels and a sentence (the `kind-labels` test fails for a kind without one); the subject `target:<id>` reads
  "Anchor target <id>"; an alert about the ledger links to the tab where it is handled.
- **Tests.** `ledger-console-0-11.test.ts` (component tests of every tab, dialog and state above, the hostile-text test, accessibility with axe); the
  Ledger tabs are also in `pages-a11y.test.ts` and `hostile.test.ts`; the browser tests cover the three new tabs and three dialogs in light and
  dark, at 1366 and 320 px, with hostile data, and under the content security policy.

What is not in this part (the later consoles of the plan): the Anchoring tab and the dialogs of the targets (**Console B**, step 16, done: see below), the Client keys tab and the
extension upgrade (**Console C**, step 19), the bundle dialog, the acknowledge dialog of a diverged chain, the publication audit and the `checks` of
a verification (**Console D**, step 22).

### The Ledger console, part B (step 16 of the plan of phase 7: the Anchoring tab)

The Ledger page has a fifth tab, **Anchoring** (`?tab=anchoring`, between Digests and Signing; it is about the cluster, so it has its own database
choice for the records and works without a database that uses the ledger). Components under `src/components/ledger/`, words and forms in
`lib/ledger-anchor.ts`:

| Part        | Component                                   | What it shows and does                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           |
| ----------- | ------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Targets     | `AnchoringTab.vue`                          | The anchor targets with kind, whether they are used, state (`none`, `ok`, `behind`, `failed` in words; "Nothing anchored yet" is neutral, never green), how old the newest anchor is (the live length of the window), whether credentials are stored ("Stored" or "Not set", never a value) and the result of the last test. Actions per row: **Test**, **Edit**, **Turn on** / **Turn off**, **Delete** (the name is typed; deleting or turning off the last used target says that the ledger is not anchored afterwards). The notice of design 6.9 ("Not anchored: ... Add a target.") with the button **Add a target** while no target is used, the advice that one kind of target is one point of failure, and a line that says how often and how many targets anchor (from the settings; a link to the Settings page for the role that may change them)                                                                                     |
| Add, edit   | `AnchorTargetDialog.vue`                    | A form for each of the three kinds (radio choice on add; the kind is fixed on edit): WORM store (endpoint, region, bucket, key prefix, lock mode, retention in days; more settings: address style, refuse to overwrite, certificates, allowed clock difference, no TLS), time-stamp authority (address, certificates, policy, time to wait, clock difference, pin of the signing key, no TLS) and OpenTimestamps (calendars one per line with **Use the public calendars**, calendars that must answer, days to wait). Every field is checked before it is sent and a message of the server (`config.<member>`, `credentials.<member>`, `name`) sits next to its field. Compliance mode (the default, cannot be undone) is saved only after the word is typed, with the date on which the lock would end; governance mode shows its warning. Only what was changed is sent; a member of the configuration that the console does not know is kept |
| Credentials | (same dialog)                               | **Write-only.** The fields are empty when the dialog opens, also on an edit (the placeholder says "Stored"); an empty field on an edit keeps the stored credentials; what was typed is cleared after the request, after a refusal and when the dialog closes; the console reads no secret from any answer, keeps none in browser storage and shows none in a list. WORM: access key id, secret access key, optional session token; time-stamp authority: optional user name and password together; OpenTimestamps: none                                                                                                                                                                                                                                                                                                                                                                                                                          |
| Test        | `AnchorTestDialog.vue`                      | Shows the last result, says before the click what a WORM test leaves behind (one empty probe object, locked for a while), runs the test (up to 30 seconds), and shows each named check with its status in words and a symbol: OK, Warning, Failed; one word for the whole test. A failed check is a result, not an error                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| Records     | `AnchorRecordsPanel.vue`                    | The anchor records of the chosen database, filter by target and by state, **Load more records**; state in words (waiting to be sent, sent and not final, anchored, failed, replaced by a newer checkpoint, never confirmed), attempts, next attempt, time of the anchor, the time the target vouches for, the last error; **Retry** (failed and expired records), **Evidence** in the formats of the kind (JSON for all, `.ots` for OpenTimestamps, `.tsr` for a time-stamp authority; the bytes are saved as they came), **Anchor now** (blocked with the reason while no target is used)                                                                                                                                                                                                                                                                                                                                                       |
| Settings    | `AnchorSettings.vue` (on the Settings page) | How often the newest checkpoint is sent (60 to 86400 s), how many targets must have anchored (1 to 8), whether anchoring is required (a warning instead of a notice); saved by `updateLedgerSettings`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            |

- **Not anchored.** The notice is on the Anchoring tab (also while targets exist and none is used), on the Overview tab (with a link to the Anchoring tab),
  in the Digests list ("Not anchored" in the Anchors column), in the aggregate dialog and on the dashboard card; the card now has **Add a target**, the
  number of anchor targets in use and a link to the tab (`LedgerCard.targetsUsed`).
- **Alerts.** `ledgerNotAnchored`, `ledgerAnchorBehind`, `ledgerAnchorFailed` and `ledgerAnchorConflict` link to the Anchoring tab ("Anchoring of the ledger");
  the subject `target:<id>` reads "Anchor target <id>".
- **Roles** (`x-required-role`): read-only reads everything and downloads evidence; `kingres_ops` may also retry and anchor now; `kingres_admin` may add, edit, test,
  turn on and off, delete and change the settings. A role that may not do an action sees the button disabled with the reason, as everywhere.
- **A server that does not build anchoring yet** (`?phase7=today`): the list of targets, the records and the evidence say "not available on this server version yet"
  in their own place, **Add a target**, **Anchor now** and the rest stay and answer the same calm notice when used (nothing changes, the typed secrets are
  cleared), and the settings of anchoring, which work today, keep working. While the list of targets cannot be read, **Anchor now** is not blocked by a guess.
- **The mock** checks the configuration of each kind as the design says (required members, ranges, no member of another kind, no TLS without the box) and names
  the field (`config.bucket`), checks the credentials of each kind, never returns a credential, and has the switches `?anchors=all|disabled|testfail|testwarn|refuse:<code>`
  (table above); `?mock=hostile` adds targets (turned off) and a record of a deleted target with hostile names, settings, test results and errors.
- **Waits for the contract or the server.** Round 2 settled most of it (the header source is `headerSource.url`, the credential names are confirmed, `{}` removes credentials, `credentialsPath` is offered; see the section on the server code below). Still open: the 30-day warning before a WORM retention ends needs the retention date of an anchored object, which the record does not carry; the checkboxes of the verification (`checks`) and the audit of the publication are Console D.
- **Tests.** `ledger-anchor-lib-0-11.test.ts` (words, checks of the forms of the three kinds, what is sent), `ledger-anchoring-0-11.test.ts` (every state of the tab, the dialogs,
  the secrets, the test, turning off and deleting, the records, each problem code in its words, the roles, the settings, the alerts and the card, hostile text, axe),
  `ledger-anchoring-mock-0-11.test.ts` (the switches, the schemas, what the mock refuses, `?phase7=today`); the Anchoring tab is also in `pages-a11y.test.ts` and `hostile.test.ts`; the
  browser tests have the tab in their routes and five new states (the tab with every state of a record, add a WORM target, add an OpenTimestamps target, a failed test,
  delete a target) in the axe, layout and content-security runs.

#### The Anchoring tab against the server code (round 2)

`kingresd` now builds anchoring (`internal/ledgeranchor`). Where its code and the design differ the code is followed:

- **WORM.** Members `endpoint`, `region`, `bucket`, `keyPrefix`, `pathStyle`, `mode`, `retentionDays`, `ifNoneMatch`, `caBundlePath`, `allowInsecure`, `maxSkewSeconds` (0 to 86400, 0 means 300)
  and `credentialsPath`. Credentials `accessKeyId` (4 to 128 characters), `secretAccessKey` (4 to 256), `sessionToken` (4 to 4096): printable ASCII, no space. A WORM target cannot be turned on
  without credentials (or a credentials file).
- **Time-stamp authority.** Members `url` (http or https, no query; TLS only matters for credentials), `caBundlePath` (required: the trust anchors for the token, a path on the servers),
  `policyOid`, `timeoutSeconds`, `maxSkewSeconds`, `signerKeySha256`, `allowInsecure`, `credentialsPath`. Credentials `username` and `password` (1 to 256; a password may hold spaces), both or neither.
- **OpenTimestamps.** Members `calendars` (at most 16), `minCalendars`, `upgradeTimeoutDays`, `allowInsecure`, `contradictionConfirmMinutes` (1 to 10080) and `headerSource: {"url": ...}` (https; http only
  with `allowInsecure`): an explorer that answers Bitcoin block headers. The dialog says what it is for: without it a claim of a calendar stays a claim and the record stays "Sent, not final".
  No credentials.
- **Credentials.** `{}` removes the stored credentials: the edit dialog has "Remove the stored credentials" (typed values are then ignored). A credentials file (`credentialsPath`, under "More settings") and
  typed credentials exclude each other. The name follows the server: a letter or a digit first, then letters, digits, space, dot, hyphen, underscore, at most 63.
- **Messages of the server.** One message at `/config` (not one per member), `/credentials`, `/name`, `/kind`, `/config/caBundlePath`, `/config/credentialsPath`; the dialog shows them in the server's words.
  The mock validates as the server does, with the same member names and the same short messages (`worm: bucket name is not valid`, `credentials.accessKeyId must have 4 to 128 characters`, ...).
- **Evidence** is `json` for every kind, `ots` (OpenTimestamps) and `tsr` (the DER of the time-stamp token); a claim arrives as the state `submitted`.
- **The alert of a system database older than what was published** is `ledgerSignerUnavailable` with the subject `published-state`: its own words in the alert list ("The system database has gone back
  to an older state than what the ledger published ... Nothing is signed until this is resolved") and a link to the Publication tab. Mock: `?signing=restored`.
- Tests: `ledger-anchoring-server-0-12.test.ts`; the mock tests of Console B follow the server's fields and words. Browser: the state "alert: the system database is older than what was published".

## What the panel does for contract 0.10.0

Contract 0.10.0 (the failover started by hand, increment 3 of `kingres/docs/design/ha-phase5-part2.md`) adds four operations
(104 in all: `getFailoverStatus`, `listFailoverReports`, `getFailoverReport`, `resetAutoFailoverCounter`), a request body for
`failoverServer` and `restorePreferredPrimaries` that can ask for a plan (`dryRun`, answered 200 `FailoverPlan` instead of 202 with a
task), seven alert kinds, five audit actions, two problem codes and a minimum of 30 seconds for the automatic failover timeout.
The panel followed first with words, mock answers and one narrowing; the first part of the console (below) now follows. The console
work is increment 8 of the design ("The console", in three parts that follow increments 3, 6 and 7).

- `endpoints.failoverServer` and `endpoints.restorePreferredPrimaries` never ask for a plan, so they return a task; a server that
  answers a plan anyway makes them throw instead of a plan being read as a task. `previewFailover` is the call for a dry run.
- The mock plans a failover (`dryRun`), refuses a group without a standby (409 `no_replica_available`) and a possible loss of data
  without `allowDataLoss` (409 `failover_needs_decision`, see the switch `?failover=dataloss`), writes a report per finished run
  and answers the status, the report list and the counter reset. The settings page asks for 30 to 3600 seconds.

### The failover console, part 1 (increment 8, after increment 3: the dialog, the progress and the report)

- **The dialog** (`FailoverDialog.vue`, replaces the failover branch of `ServerActions.vue`, which no longer knows the kind). Step 1
  chooses the mode (a live server defaults to graceful, any other to hard), the groups the server leads (the map group is marked
  "moves the manager of the cluster"), the standby per group (with lag and sync state) and a comment for the audit event. Step 2 is
  the dry run (`previewFailover`), a block per group (`FailoverPlanTable.vue`, `FailoverCandidates.vue`) with the verdict about lost data
  in words (`DataLossText.vue`, one sentence for each of `none`, `asyncOnly`, `possible` and `unknown`), the estimated time, for a switchover the
  time writes pause, the steps, the blockers and "the running rebalance will be stopped first". Step 3 confirms: the address is typed
  for a hard failover, and a plan whose verdict is not `none` asks for the box "I accept that data may be lost", which is what sets
  `allowDataLoss`. A refusal of the dry run or of the start is shown in words of its own for each problem code
  (`FailoverRefusal.vue`, `lib/failover.ts`), with the server's `detail` below them.
- **Progress** (`FailoverProgress.vue`, on the Servers and Replication pages while a run goes on, waits or has failed): the state and who
  started it, the phase, per group its state, from and to, how long writes have been paused, the lease wait, the steps of the task in
  one block per group (`TaskProgress.vue` groups any task whose steps carry `shardGroupId`), Cancel (disabled with the reason after the
  promotion), and for a paused run the panel "Waiting for you" with the blockers, the verdict of the group and **Accept data loss and
  continue** (a repeat of the request built from the options in the run's report, plus `allowDataLoss`) and **Cancel**.
- **Report** (`FailoverReportDialog.vue`): reached from the Tasks page (a button on every finished, failed or paused failover), the
  Replication page ("Last failover") and the progress card. Alerts about a failover link to the progress and report.
- **Readiness** (`FailoverReadiness.vue`): a line on every group's card of the Replication page, from `ShardGroup.failover`.
- **What the real server answers for now** (increment 3): `graceful`, a group that is the map group and `restorePreferredPrimaries` answer
  422 `unsupported_operation`. The dialog offers both modes and the map group and finds out by asking for the plan: the answer is shown
  as information ("not available on this server version yet", with what is not available), the choice stays open, and nothing is
  hard-coded about the server's version. The mock refuses the same unless `?failover=graceful` or `?failover=map` is given.
- **A contract observation.** The 409 `failover_needs_decision` is documented as carrying the plan, but `Problem` has no member for it.
  The dialog therefore reads the plan again with a dry run after such a refusal (it shows it and asks for the decision), and the
  progress card reads the verdict of a paused run from its report.
- Not in this part: the automatic failover settings, the counter and the banner (done in part 2, below), the switchover and restore actions
  (after increment 7).

### The failover console, part 2 (increment 8, after increment 6: the settings, the counter, the banner, the alerts)

- **Settings** (`SettingsPage.vue`, fieldset "Auto-failover"): the timeout is checked for 30 to 3600 seconds, as the contract has it since
  0.10.0 (the panel asked for 1 before); the hint says why a shorter time cannot work and that the cluster waits at least three times the time
  it takes to call a server failed. `AutoFailoverCounter.vue` shows "Events since the last reset: n of m" and the time of the last event (or
  that there was none), the line "The limit is reached: further failures need an operator." when `eventsSinceReset >= maxCount` (computed
  from the numbers, so it also holds for a server that sends no `limitReached`), and **Reset counter**
  (`GateButton op="resetAutoFailoverCounter"`, `kingres_admin`; blocked with the reason "The counter is at 0 already." at 0). A confirmation says
  "Automatic failover will be allowed again up to m time(s)"; a refusal is shown in the words of its problem code in the dialog and nothing
  changes; `unsupported_operation` is the calm "not available on this server version yet". The requirements are listed in words (three servers,
  a standby for every group, a synchronous standby for a loss-free failover).
- **Banner** (`GlobalBanners.vue`, `banner-failover`, in the same polite live region; words in `failoverBanner` of `lib/failover.ts`): "Failover
  of server X is running: group 2 of 3." and the phase; "Automatic failover of server X ..."; "Restoring the preferred primaries ...";
  danger and "is waiting for you" with the number of groups that need a decision; "The manager is moving to X." during the bootstrap of the map
  group. It is not shown for an idle or failed run, to `kingres_stats` (it may not call `getFailoverStatus`), or when the server does not know
  the operation yet. Announcements (start, a promotion, the end, a need for a decision) stay in `FailoverProgress`, which is on the Servers and
  Replication pages.
- **Dashboard.** The card "Auto-failover" says that the limit is reached (a warning with text and symbol) and links to Settings for the roles that
  may open it.
- **Alerts.** `autoFailoverLimitReached` links to the counter on Settings; `autoFailoverTriggered` links to the failover progress and report;
  the `oldPrimary*` alerts link to their server through the subject, as before.
- **The mock** now counts only an automatic failover (a failover started by a person is not counted and not limited, design 4.4), raises
  `autoFailoverTriggered` (info) when it ends and `autoFailoverLimitReached` (warning) at the limit, and clears the second one when the counter
  is reset by `resetAutoFailoverCounter` or by a rebalance. Switches: `?failover=count:n:m`, `?failover=off`, and the existing `limit`, `auto:<server>`,
  `needs-decision`, `map`; `?unsupported=resetAutoFailoverCounter,getFailoverStatus` for the server of today.
- **Waits for the contract.** The effective timeout that the leader uses (`max(timeout, 3 x failure timeout)`, design 4.2) and the cool-down of five
  minutes are not members of `ClusterSettings`; the panel states the rule in words and shows no figure. The 409 `failover_needs_decision` still has no
  member for its plan (see part 1). The switchover (`Switch over...` on a group card) and **Restore preferred primaries** are part 3 and are not built
  in this part.
- **Tests.** `failover-console-part2.test.ts` (the banner in words for every state and in the shell, the counter words, the settings, the reset with
  each refusal, the roles, the dashboard, the alerts, the mock states, hostile text, axe); the browser tests have four new states (settings at the limit,
  the reset dialog, the banner while a run goes on, the banner waiting for you) in the axe, layout and content-security runs, and the banner is
  checked at 1366, 683 and 320 px.

## What the panel does for contract 0.8.0

Phase 13, configuration from the console (design: `kingres/docs/design/config-phase13.md`). Contract 0.8.0 adds 11 operations (100
operations in all), the tag `Configuration`, 8 problem codes, the alert kind `clockDifference`, the task kinds `config.apply` and
`config.restart`, 3 audit actions and `ClusterSettings.clock`. Nothing of 0.7.0 changed.

**The page `Configuration`** (`/configuration`, in the Cluster navigation after Settings; `kingres_readonly` and up, a
`kingres_stats` account is sent to the "not allowed" page) has three tabs, kept in the address (`?tab=changes`, `?tab=time`):

- **Parameters** (`ParametersPanel.vue`): every parameter with its category, description, the value on every server with the level
  it comes from (cluster, server group, server, "set by initdb", "derived from the memory quota", "managed by KingresDB", "postgresql.conf
  or the command line"), a state badge for what is not applied (pending restart with the value that waits, failed with PostgreSQL's
  text, waiting for the server, not known), the default, the type, unit and range, enum values in the details, a `restart` chip, and
  the words "differs between servers" with a symbol. Managed and refused parameters are listed read-only with the reason and no Edit
  button; a hidden (superuser-only) parameter shows "hidden" instead of a value, because no role is ever sent it. The filters are in the
  address, so the dashboard links to them: `?q=` (name and description, debounced), `?category=` (top-level category), `?changed=1`,
  `?differing=1`, `?pendingRestart=1`, `?state=` (`applied`, `pendingRestart`, `failed`, `waiting`), `?kind=` (`settable`, `managed`,
  `refused`) and `?all=1` (the toggle "Show managed and refused"; without it and without `kind` the list is `kind=settable`). Paging is
  by cursor ("Load more") with a live region ("100 parameters shown; more can be loaded"). States: loading, empty result of a
  filter (with "Clear filters"), error with "Try again", partial (`PartialNotice`, the servers named, their values "Not known"),
  unsupported (calm notice), a refused cursor (the list starts again and says so). Above the table: which servers wait for a restart,
  where a change failed and which servers wait for a server that cannot be reached.
- **Changes** (`ChangesPanel.vue`): the history, newest first, filtered by parameter name (`?name=`). Revert is offered on the latest
  change of a level and scope (`revertible`); an older one says why it has none. Revert opens the edit dialog filled with the earlier
  value (or "no override"), previewed like any change, as a new change. A 409 `config_change_superseded` is explained.
- **Time** (`TimePanel.vue`): the servers table (clock, difference with its uncertainty, "within the measurement" when the difference
  is smaller than the uncertainty, a state badge, the operating system zone, `TimeZone`, `log_timezone`, a mark on a zone that differs
  from the others), the panel "What to do about a clock that differs" (KingresDB does not set the system clock: it needs the rights
  of the operating system administrator and a clock that is set once drifts; run NTP or chrony; `chronyc tracking`, `timedatectl
timesync-status`; not by hand with `date`), the cluster time zone (a form row with a zone list; `TimeZone` and `log_timezone` are
  changed together through the same dialog), the time zone of each database (Change, Remove this override) and the threshold of the
  alert (`updateClusterSettings`, `clock.maxDifferenceMs`, answers 200). The state of a clock is the difference less the uncertainty
  against the threshold: in sync, differs, far off (ten times the threshold), cannot be read (no number, never 0). The page measures
  again every 10 seconds and says when it last did.

**The edit dialog** (`ConfigEditDialog.vue`, one dialog for a parameter, a revert and a time zone): the level as a radio group with
the scope select of the chosen level; a level the parameter does not allow is disabled with the reason ("This setting must be equal
on every server, so it is set for the cluster."); the override that is set at the chosen level with "Remove this override"; a
control for the type (on/off select, enum select, text with a unit select that rewrites the unit, a time zone field with the zone
list); the value is checked **as it is typed** by `src/lib/config-value.ts` (below) and the message is in a polite live region;
a debounced `previewConfigChange` shows the servers that change, wait for a restart, do not change or are down, the sentence "N of M
servers will change. K need a restart; nothing is restarted by this change.", and, when a restart is needed, the order of the
instances, the downtime of a primary and the conflict of a mixed change. Apply is for administrators (`GateButton`) and is off until
the servers have checked the change. Then `TaskProgress` and one line per server (applied, pending restart with what it waits for,
failed with PostgreSQL's text, waiting for the server), **Retry** for a failed or waiting server (the same change is sent again; it is
not a second change), "Cancel the task" for a task that waits for a server, and "Hide (keeps running)".

**The value checker** (`src/lib/config-value.ts`) is used by the dialog and by the mock, and is the narrow subset of PostgreSQL's
syntax that the design names: booleans `on off true false yes no 1 0` (any case), decimal integers without a leading zero, no
hexadecimal, octal or exponent, units `B kB MB GB TB` and `us ms s min h d` converted to the base unit of the parameter (which may be
`8kB`) and exact (no rounding), the range, enum values ignoring case, strings of at most 4096 bytes without NUL, CR or LF. The
test vectors are one table (`tests/config-value-vectors.ts`); the control plane keeps the same cases.

**Restart** (`RestartPendingDialog.vue`, `restartPendingInstances`): says that restarting a primary is downtime for its shard group and
that failover is not available, lists the servers that wait and what for, needs the acknowledge box (not pre-ticked) and follows the
task `config.restart`. The steps of the task are the order the server follows. The contract has no read of that order before the
restart (only `previewConfigChange` carries `restartOrder`), so the dialog describes the rule and shows the steps once the task starts.
Also on the server page (restarts the instances of that server) and from the page's header button.

**Elsewhere:** the dashboard card "Configuration" (`ConfigSummaryCard.vue`: servers waiting for a restart, parameters that differ inside
a server group, the largest clock difference with its server, failed and waiting servers when there are some; each a link, a zero is
"none", a number the server cannot know is "not available"); the section "Configuration" of the server page
(`ServerConfigSection.vue`: group, revision held, the overrides that apply, what waits for a restart with the restart button, what
failed, clock and zones); the alert kind `clockDifference` (label, explanation, a link to the Time tab); labels for the two task kinds
and three audit actions; words for the 8 problem codes.

Everything that comes from the server (parameter names, categories, descriptions, values, enum values, PostgreSQL's error texts,
server ids, group and database names, zone names) goes through `SafeName` or `safeText`; a value is never markup, never an `id`, never
the `for` of a label. `tests/config-hostile-0-8.test.ts` checks it in every place a value is shown.

**The mock** (`src/mocks/config*.ts`) follows the design, and is never more permissive than the contract: about 380 generated
PostgreSQL-style parameters (categories, types, units, ranges, enum values, contexts) plus the parameters of `kingres_shard` and
`kingres_ledger`; kinds `settable`, `managed` (port, listen_addresses, ssl*, synchronous_standby_names, wal_level, ...) and `refused`
(archive_command, restore_command, ssl_passphrase_command, dynamic_library_path, `*_preload_libraries`, fsync, the Developer Options,
...); superuser-only parameters are `hidden` with null values; the cluster-only parameters (`TimeZone`, `log_timezone`, `DateStyle`,
`lc_*`, `kingres_shard.default_bucket_count`, ...) have no server group or server level; per-server values with source, level and state;
the four levels; the same checks as the server in the same order (names and levels, then the value with the shared checker, then the
floors `max_prepared_transactions` at least 1, `max_wal_senders` at least 4 + the replica count, `max_replication_slots` at least 2 + the
replica count); the preview with the effects per server, `restartOrder` (standbys first when one of `max_connections`,
`max_worker_processes`, `max_wal_senders`, `max_prepared_transactions`, `max_locks_per_transaction` goes up, primaries first when it
goes down) and `restartOrderConflict`; mutations are tasks with a step per server (a server that fails does not stop the others, the
task ends failed with `config_incomplete`; a server that cannot be reached holds the task, which never gives up); history and revert
(only the latest change of a level, scope and name is `revertible`, else 409 `config_change_superseded`); `restartPendingInstances`
(409 `nothing_pending`, 409 `config_restart_order_conflict`); a list of about 240 time zone names; the alert `clockDifference`.
The seed: revision 6, `shared_buffers` pending a restart on one server, `work_mem` overridden on one server, one clock that is 1.82 s
ahead (so the seed has an open alert more than before 0.8.0: tests that count alerts call `calmClocks()`), the time zone Asia/Tehran.

### Limits of what the panel can do with contract 0.8.0

- The restart order of what already waits is not readable before the restart (see above).
- `listConfigParameters` has no total, so the page says how many it has loaded, not how many there are.
- The category filter is a fixed list of PostgreSQL's top-level categories; a category of an extension is reached through the search.
- A role's own setting (`ALTER ROLE ... SET`) is out of scope of the contract; the Time tab says it beats the database level.
- `listConfigServers` gives names that wait for a restart, not their values; the dialog and the server page take values from
  `listConfigParameters`.
- The dashboard and server page links use the query `?tab=time` where the design wrote `/configuration/time`: the page is one route with tabs
  in its query, like the tab of other pages.

## What the panel does for contract 0.7.0

Every item of the changelog (`kingres/api/README.md`, "0.7.0") and where the panel uses it:

1. **`Alert.subject`** (`AlertSubject`): `AlertSubject.vue` shows what an alert is about as links, in the alert list and in the
   open alerts of the dashboard: the server (the leader that keeps a change waiting, for `replicationPending`), the server that
   is behind (`behindServerId`), the shard group (a link to its card on Replication), the database, and the qualifier (`ca`:
   "Cluster certificate authority"; any other value is shown as text). The message is never parsed. A server of an older
   revision sends no subject: then `serverId` and `databaseName` of the alert are used as before. A server id that no URL can
   carry (`..`) is shown without a link. Tests: `alerts-0-7.test.ts`, `hostile` case in the same file.
2. **`LedgerStatus.complete`, `uncoveredShardGroupIds`**: the Ledger page says that the chains of the status are those of the
   shard group of the server that answered and names the others ("not the status of the whole database"); a server that sends
   neither member gets "did not say"; the groups that are not covered can still be chosen for the blocks, marked as not in the
   status. `src/lib/ledger-coverage.ts`; `ledger-status-0-7.test.ts`. The database summary (`ledgerSummary`) has no coverage
   member in the schema, so it still says "Last check passed" only (see the limits).
3. **`workbench_busy`** (429, `Retry-After`): its own words ("The workbench is busy"), the `Retry-After` of the answer in the
   notice (`ApiError.retryAfterSeconds`, read in `client.ts`), and a hint that a statement the person stopped waiting for still
   holds a slot. `workbench-0-7.test.ts`, `problem.test.ts`, `contract-0-4.test.ts` (words for every code).
4. **`cancelStatement`, `QueryRequest.statementId`**: every statement is sent with a fresh random id (32 hexadecimal digits).
   While it runs the workbench offers **Cancel statement** (ends it on the server, immediately, not a task; the page then says
   "You cancelled this statement") and keeps **Stop waiting** (only the page stops waiting; the statement keeps running and
   keeps one of the role's two slots). After "Stop waiting" the panel still knows the id and offers **Cancel it on the
   server**. A 404 of the cancel (the statement ended, or runs on another server) is explained. Audited as `cancelStatement`.
   `workbench-0-7.test.ts`; the end-to-end runner has two steps for it.
5. **Liveness**: `suspect` and `dead` come from heartbeats and are the answering server's view. Servers page: the time since
   the last heartbeat under every status that is not LIVE, and a note that the two statuses are one server's view. Server page:
   a notice for suspect and dead (what it means, when the last heartbeat was) and "(7s ago)" next to the last heartbeat. Dashboard:
   the counts (they come from one snapshot with the list) and, when some server is not live, the list of those servers with their
   status and heartbeat age. A server whose agent cannot be reached has no uptime and no resources ("n/a", never 0).
   `leader_unavailable` (503): the shell shows a banner on every page ("This server has lost contact with the manager leader"),
   says what it means and names the other servers the panel knew from its last answers (the response itself does not say which
   server to open; see the gaps), and the page errors have the same words. `liveness-0-7.test.ts`.
6. **Leases** (`ShardGroup.lease`): `GroupLeaseInfo.vue` on every group of Replication and, for the groups whose primary a server
   runs, on the server page ("Leases of the primaries on this server"): held / granted / lost / none with the holder, the time
   left, **Enforced** (the group has a standby) or **Watched only**, and, for a lost enforced lease, "The primary was stopped on
   purpose". "Granted" says that the answering server only knows what it granted. Absent `lease`: "this server does not report
   leases". `leases-0-7.test.ts`. The Shards page lists buckets, not groups, so it shows no lease.
7. **Alert kinds** `leaseLost`, `groupWithoutSynchronousStandby`, `groupUnderReplicated` (and the heartbeat kinds `serverSuspect`,
   `serverDown`): a label, a sentence of explanation, the severity badge of the contract; the dashboard banner counts them.
   `alerts-0-7.test.ts`, `kind-labels.test.ts`.
8. **Standbys**: task kind `standby.reconcile` ("Add or remove standbys"); its step `standby.register_in_catalog` is shown
   **skipped on purpose, not a failure** (the symbol `↷`, the word "skipped", the reason, a note) both for the contract's step
   state `skipped` and for what kingresd 0.7.0 does (the step is `done` and the task `detail` is `skipped: needs kingres_shard 0.5`).
   The Replication page: the replica count and **Change replica count** (administrators; a task with progress inside the dialog),
   per group the primary, every standby with state, sync state (and whether a commit can wait for it) and lag, a standby of
   kingresd by `instanceId` 1048576 plus its id ("standby 7, managed by kingresd"), a standby that is starting ("not streaming"),
   and the answer to "Does a commit wait for a standby?" (yes: the standbys in `quorum` or `sync` state; no: no standby, or the
   alert `groupWithoutSynchronousStandby` is open because the grace period ran out; not yet: the standby is being made;
   not streaming: still inside the grace period). The newest `standby.reconcile` task is shown on the page.
   `defaultReplicaCount` (`updateClusterSettings`) answers the new settings, not a task, so the dialog looks for the task that
   the change started (one that did not exist before the request) and follows it; when none starts it says so. The Settings
   page no longer says that a change needs a rebalance. `standbys-0-7.test.ts`, `leases-0-7.test.ts`.
9. **Audit actions**: all 57 actions have words (`AUDIT_ACTION_LABEL`, the compiler fails when the contract adds one); the filter
   shows "words (name)" and the table shows both. `audit-metrics-0-7.test.ts`.
10. **Client entry point metrics**: the dashboard group "Client entry point" (eight charts, and eight more under "Standby entry
    point") appears only when a probe (`clientConnectionsActive`, `clientStandbyConnectionsActive`) finds a series with points;
    otherwise nothing, not even a heading. Counters are totals since the start of kingresd and are labelled so. An error of the
    probe that is not a 4xx is shown (it is not "off"). `audit-metrics-0-7.test.ts`.
11. **Documentation entries**: `ServerCounts.suspect` is counted on the dashboard, `ReplicationMember.instanceId` is opaque
    (the label above), the text of `updateClusterSettings` and of `leader_unavailable` is in the words of the panel;
    `autoFailover.timeoutSeconds` has a heartbeat source now (the settings page keeps its text); `failoverServer` and
    `restorePreferredPrimaries` are untouched (still answer `unsupported_operation` on a real server).

The mock (`src/mocks`) follows 0.7.0 and is never more permissive: a running statement that can be cancelled (`select pg_sleep(n)`;
a second running statement with the same id is 409, another role's id is 404), the slots (two per role, four per server, then
`workbench_busy`), heartbeats (a silent server is suspect after 5 s and dead after 10 s, alerts `serverSuspect`/`serverDown` with
a subject, cleared when it is heard again), leases (held, granted, lost, enforced or not, audit events `leaseLost`, `leaseFenced`,
`leaseAcquired`), standbys created and removed by the task `standby.reconcile` with the skipped step and a `1048576 + id` instance id,
the synchronous rule with its grace period and the alerts, `ledger` coverage and the client metrics. The switches are in the table
at the top. Limits of the mock: the leader's automatic ten-second check is not simulated (standbys change when the replica count is
patched, or by the tests), and the lease of a group whose primary runs elsewhere is always `granted` (the contract says the answering
server only knows what it granted).

## What the panel does for contract 0.6.0

Breaking changes (the panel follows them; nothing of the 0.5.0 behaviour is kept):

- **Add server is administrator-only** (`addServer`, `kingres_admin`). Visibility follows the generated `x-required-role`: an
  operator sees the button disabled with "Requires the kingres_admin cluster role"; a 403 `forbidden` is explained (`tests/pin-and-fingerprint-0-6.test.ts`, `tests/roles.test.ts`).
- **A pin is the normal path** (add-server dialog and join form, `PinChoice`): the fingerprint field is required and validated
  (64 hex digits, colons and case accepted). The only other way is a checkbox that is never pre-selected, "Do not check the
  certificate: trust whatever answers at this address", with a warning in plain words (the server will trust whatever answers
  there; for a join the administrator's password is sent to it; for an add the setup token). It sends `allowUnpinned: true` and
  no pin. Without either, the submit is blocked on the field; the server's 400 at `/expectedFingerprint` is shown on the field.

Additive changes (each tested; the file is named in the report of the increment):

- **The server's own fingerprint** (`NodeStateInfo.internalFingerprint`, `LocalServerInfo.internalFingerprint`): shown in the
  setup wizard (only the loopback caller or a request with the setup token gets it) and on the This server page, in groups of
  eight digits with a copy button (the plain 64 digits). The "read it from the log line" text is the fallback when the value is
  null or absent. `SetupTaskSummary.detail` is shown by the join wizard through `SafeName`; the wizard says that a fingerprint
  was checked only from that text.
- **Degraded session** (`Session.degraded`, `degradedReason`, `clientAddress`; `getSession` and `listThisServerInstances` work while
  the system database is stopped): the copy of the session in `sessionStorage` and the saved instance id are gone. After a
  reload the panel calls `getSession`; `degraded: true` shows the stopped state with the reason, and the instance to start is
  read from `listThisServerInstances` (an answer 503 leaves the "does not know the instance" text, no button). In this state only
  `startThisServerInstance`, `getSession` and `listThisServerInstances` work, so navigation and sign-out are shown
  disabled with the reason (strike-through, not colour alone). `clientAddress` is "your address" on the network rules page (behind a
  reverse proxy it is the proxy's address, and the page says so).
- **Dry run** (`replaceNetworkAccess?dryRun=true`): the review dialog asks the server first; the server's 422/409/400 is shown as
  for a real change and keeps Apply off; "accepted" says that nothing is saved yet. The panel's own address check stays as an
  immediate hint, labelled as one. If the dry run cannot be answered the dialog says so and does not block.
- **`internal_api_disabled`** (409): its own explanation (the server runs without its internal API: configuration, not a
  missing feature) on add, remove, leave, renew and revoke.
- **Ledger coverage** (`complete`, `uncoveredShardGroupIds`): "Passed, all groups", "Passed, incomplete" (different text and
  symbol) and "Passed, coverage not stated" (a server that did not say); the uncovered groups are named in the verification
  list, the detail dialog, the digest list and next to the downloads (the saved file stays byte-exact). The database summary
  says "Last check passed", not "Verified".
- **Audit:** server column, `PartialNotice`, the restart after a refused old cursor, and the explanation of a 503 when the audit
  log cannot be written (nothing was changed). Event ids are opaque and are never sorted, parsed or re-serialized.
- **`replicationPending`** has a label and links to the server of the alert (see the limits for the subject).
- **`LedgerProofBlock`:** the generated type is used; the panel's own `ProofView` is removed (integers are still read from the raw text).
- **`dataPaths.walDirectory` / `logDirectory`:** null is shown as "default".
- **Documentation entries:** the workbench says that a result cut at the limit of a statement that changes data was still
  committed (with its real command tag), explains the timeout (422, SQLSTATE 57014), the 1 MiB / 16 MiB limits and the two-statement
  limit per role; `pendingRebalance` is worded as the contract defines it; the sign-in page says a sign-in belongs to one server;
  digest `generatedBy` and the byte-exact `document` are explained next to the downloads.

The mock (`src/mocks`) is never more permissive than 0.6.0: pin required, administrator for add-server, degraded session, dry run,
`complete`/uncovered, audit `serverId`, switches for `internal_api_disabled` and the unwritable audit log.

## What the panel does for contract 0.5.0

- **Certificates of a server** (server page, "Certificate and first contact"): _Renew now_ (`renewServerCertificates`, ordinary
  confirmation, follows the task `certificate.renew`) and _Revoke certificates_ (`revokeServerCertificates`, `kingres_admin`).
  The revoke dialog says what the contract and the control plane README say: every other server and every database instance
  refuses the server in both directions, open connections run until they end, the server never gets a new certificate, the only
  way back is to remove it and join it again, which works only for a server that holds no bucket copy (so for a server with data
  it is a one-way door), the leader's own certificates cannot be revoked. It needs the server id typed and has nothing
  pre-ticked. A revoked server is marked "Certificates revoked" with an icon in the list and on its page; Renew and Revoke are
  disabled with the reason written next to them (revoked, or the manager leader); `certificate_revoked`,
  `certificate_authority_unavailable` ("the manager leader does not hold the CA key; nothing was changed") and
  `server_hosts_map_group` have plain words.
- **Partial answers, alert kinds, cursors, alert ids:** see "Conventions". An alert that names a server links to it (when the id
  can be put in a URL); the new kinds have labels.
- **Ledger:** the digest list pages ("Load more digests"). When a proof is on screen, every listed digest says whether it has the
  proof's chain (`chain_id` of the document of the chain with the proof's `shard_group_id`) and is not older than the proof's
  block; saving a digest repeats that. It compares identifiers only and says so; the check is `kingres-verify`.
- **System database stopped:** the state is shared across tabs and has a Start button that names the instance (see 0.6.0 for how
  it is learned after a reload).

## Known limits of this increment

The SQL editor is a plain textarea, the replication page is a table rather than a graph, and the charts are simple
SVG lines without zoom or tooltips.

What the contract (revision 0.6.0) does not let the panel do, and what the panel does instead:

- **Cancel a workbench statement (0.7.0).** Possible with `cancelStatement` while the page knows the id of the statement. After a
  reload the id is gone (nothing is kept in the browser), so a statement that kept running is cancelled by its timeout; a
  cancel is bound to the server that started the statement, and a 404 says so.
- **Read-only workbench.** The API has no read-only mode, so the workbench says "Mode: read-write" and that INSERT,
  UPDATE, DELETE and DDL are executed. A `readOnly` flag on `QueryRequest` (a read-only transaction) is needed.
- **Truncation marker for names.** A result cell, a log line or a name cannot say that the server cut it; the panel cuts
  what it draws (with "Show all N characters") but cannot tell a cut value from a complete one.
- **Names that no URL can carry** (`.` and `..`). They are listed without a link and the router refuses them, but they cannot
  be opened. The server should refuse to create such a database or table through the API, or define an escape for them.
- **Paging.** The contract does not paginate `GET /databases`, `/servers`, `/server-groups` and
  `/this-server/instances` ("Not paginated"); the panel shows what they return and cannot tell if a very large cluster
  makes them long. Every list that has cursors can be paged ("Load more"); the lists that are read completely
  (roles, grants, rebalance reports) are followed up to a bound and say when more exists. A refused cursor restarts them
  without a notice.
- **Start from a new tab, or when the server never answered the instance list.** There is no saved copy any more: the panel
  needs `getSession` (the server marks it `degraded`, and only for a session that server checked before the stop, at most an
  hour old) and `listThisServerInstances` (from the instance manager's memory, 503 if this process never read the list). Without
  them (a session that server never checked, a role that was changed, a server restarted after the stop) the panel is at the
  sign-in page, which cannot sign in either; it says to start the instance on the machine (for example `pg_ctl start`) or to
  restart `kingresd`.
- **The ledger summaries.** `LedgerStatus` states its coverage since 0.7.0 and the panel says so, but `Database.ledgerSummary` (the
  database list and page) has no such member, although the changelog says "the database summary can state its coverage"; it says
  "Last check passed" and never "verified". `LedgerChainStatus` has no coverage inside one chain (deferred by the contract).
- **Which server to open when `leader_unavailable`.** The answer does not name one; the panel lists the servers it knew from its last
  answers (and marks the manager leader as last known). A panel that was opened on a server that has already lost the leader has
  none to list.
- **The replica count task.** `updateClusterSettings` answers 200 with the settings, not 202 with the task, so the panel finds the task
  by listing `standby.reconcile` tasks before and after the change. If the leader starts it later than ten attempts of one
  second, the dialog says that no task was started (the Tasks page and the Replication page still show it).
- **Leases of other servers.** A lease is the answering server's knowledge (`GroupLease`); there is no cluster-wide view (deferred by
  the contract), so the panel shows, for a server, the lease as seen from the server that answered. The panel never says that a
  primary is safe because a lease is held.
- **Client entry point.** When it is off the contract does not say what `queryMetrics` answers for the `client*` names; the panel shows
  the group only for an answer with points, and nothing for an answer without series or a 4xx.
- **The dry run is not a reservation.** It checks the request against the revision at that moment; someone can still change the
  rules before the administrator confirms, and the real save then answers 409 as before.
- **`clientAddress` behind a reverse proxy** is the proxy's address (the contract says so); the lock-out check of the server
  uses the same address.
- **Opaque to the panel:** the manager leader's CA key. `certificate_authority_unavailable` is explained, not predicted: the panel
  cannot tell beforehand whether a revoke will be refused.

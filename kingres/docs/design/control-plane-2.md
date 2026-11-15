# Control plane: design of the second increment

Status: accepted for implementation, 2026-11-14.
Builds on the first increment of `kingres/control-plane` (the server
`kingresd`) and on REST contract 0.2.0. It covers the parts of phases 3 and 4
of the roadmap in [architecture.md](../architecture.md) that need one server
only: managing the local PostgreSQL instance, creating a cluster, a task
engine, and the operations behind the Databases, Security, Ledger, Query and
Settings pages. Joining servers, rebalance and failover are the third
increment; they need more of the sharding extension first.

This increment may revise the contract (revision 0.3.0).

## Decisions

### D1. The manager leader is the server of the map group primary

There is no separate consensus store yet. The `kingresd` that runs on the
server holding the primary instance of the map group (shard group 0) is the
manager leader: it runs tasks and accepts mutating cluster operations. Any
other `kingresd` forwards them to it. Reason: the map group is already the
source of truth for cluster state (ADR 0008), so the process next to its
primary can act on that state without a second agreement protocol. An
external store for leader election arrives with automatic failover in
phase 5, where the failover engine needs one anyway. Cost: while the map
group has no primary, nothing that changes the cluster can run; reads keep
working.

### D2. Cluster state lives in a system database

`kingresd` keeps its own state in the database `kingres_system` on the map
group, in the schema `kingres_cp`. The schema is created and upgraded by
versioned SQL migrations embedded in the binary and applied at start-up by
the leader, each in one transaction, recorded in
`kingres_cp.schema_migration`. A binary older than the schema refuses to
start. The first migration creates:

| Table | Content |
|---|---|
| `task` | id, kind, state (`pending`, `running`, `paused`, `failed`, `done`), subject, input, progress (percent and steps), error, created/updated timestamps, created by, a lease (holder and expiry) |
| `database_setting` | per database: sharding enabled, bucket count, durability, ledger mode, digest interval, created at, created by |
| `cluster_setting` | key and JSON value: default replica count, auto-failover settings, rebalance concurrency, feature flags, cluster id and name |
| `server` | server id (host), server group, joined at, version, last heartbeat |
| `server_group` | name and description |
| `alert` | id, severity, kind, subject, message, raised/cleared/acknowledged timestamps |
| `network_rule` | the managed `pg_hba.conf` rules and a revision number |

The catalogs of the extensions (`kingres.*`) stay where they are, one set per
user database.

### D3. `kingresd` manages the local instances

The "agent" role is part of `kingresd`. It owns a data directory
(`<data-dir>/instances/<node id>/`), runs `initdb`, writes the managed part
of the configuration into a file that `postgresql.conf` includes, writes
`pg_hba.conf` from the managed rules, and starts, stops and restarts the
instance with `pg_ctl`, waiting for readiness. The PostgreSQL binaries are
found through a `pg-bin-dir` setting. It never runs as root, and it refuses
to touch a data directory it did not create (a marker file identifies its
own).

Every managed instance has `shared_preload_libraries =
'kingres_shard,kingres_ledger'` (both libraries are always loaded; whether a
database uses them is decided by `CREATE EXTENSION`), TLS on when
certificates are configured, `password_encryption = scram-sha-256`, and
`max_prepared_transactions` greater than zero.

### D4. Tasks

Every operation the contract defines as long-running is a task: a row in
`task` executed by the leader as an ordered list of idempotent steps. A step
records its completion before the next one starts, so a task that is
interrupted (restart, lost leadership) resumes from the first step that is
not done. The leader takes a lease on the task row and renews it; a task
whose lease expired is picked up again. One task per subject at a time; a
second one gets the contract's conflict problem. A task can be cancelled
between steps; steps that cannot be undone say so in their description.
Tasks never store secrets: a password in a task's input is used and then
removed before the row is first written (the step that needs it runs
inside the request).

### D5. Creating a cluster

`POST /setup/init-cluster` becomes real: check the setup token, then run the
setup task, reported through `setupTask` in `GET /setup/state`. Steps:
create the instance for node 1 (shard group 0), create the administrator
role with the given password and membership in `kingres_admin` (the four
cluster roles are created first), create `kingres_system` and apply the
migrations, record the cluster id, name and settings, install the two
extensions in `kingres_system` and call `kingres.init_cluster`, write the
node state as `clustered`, remove the setup token. After it, the server is a
one-server cluster and the panel's sign-in works with the administrator
account.

`join-cluster`, adding and removing servers, failover and rebalance still
answer "not supported yet" in this increment.

### D6. Operations made real

- **Databases:** create (on every primary of the cluster, here one), with
  the four settings groups; update of the mutable settings; drop. Creating
  installs `kingres_shard` and `kingres_ledger` as selected, records the
  settings, and sets the durability level through
  `ALTER DATABASE ... SET synchronous_commit`. Tables: list, detail,
  distribute (`kingres.create_distributed_table`), enable ledger
  (`kingres.ledger_enable`).
- **Security:** roles (create, update, disable, drop, password, memberships,
  cluster roles) and grants (grant, revoke, the three templates) executed as
  SQL built with identifier and literal quoting from validated input, never
  by concatenating request text; the last `kingres_admin` cannot be removed;
  network access rules (stored, rendered into `pg_hba.conf` of the local
  instance, reloaded).
- **Ledger:** status, blocks, digests (generate and list), verification
  (start as a task, list, get), inclusion proof, using the functions and
  tables of `kingres_ledger` 0.2 ([ledger-0.2.md](ledger-0.2.md)).
- **Query workbench:** runs one statement as the signed-in role. The
  session keeps the authenticated PostgreSQL connection it opened at sign-in
  for this purpose (the password is never stored); the statement runs with a
  statement timeout, a row limit enforced by fetching at most that many
  rows, and in a read-write or read-only transaction as requested.
- **Settings:** read and update; server groups (create, delete, assign).
- **This server:** instance start, stop and restart; resources (CPU, memory,
  disk) from the operating system; local logs from the instance's log files.
- **Alerts and logs:** alerts raised and cleared by periodic health checks
  (instance down, replication lag, disk nearly full, ledger verification
  problems, pending ledger transactions too old); cluster logs from the
  local server only in this increment.
- **Metrics:** `kingresd` samples a fixed set of metrics from the instances
  it manages (connections, transactions per second, replication lag, sizes,
  ledger heights) every few seconds into an in-memory ring buffer that
  answers `GET /metrics/query`, and exposes the same values at `/metrics` in
  the Prometheus text format for an external Prometheus. No metrics store is
  bundled (this replaces the bundled store mentioned in the architecture
  document; reason: one less service to ship and supervise, and operators
  who want history already run Prometheus).

### D7. Contract revision 0.3.0

Fix the defects reported against 0.2.0 (fields without a data source that
are not nullable; `LedgerStatus` nullability; the wording of
`notInUniqueKey`, of the permitted setup addresses and of
`TableColumn.type`; `LocalServerInfo` nullability; the unique keys that
block a distribution column; steps and timestamps in `SetupTaskSummary`;
the meaning of an empty versus null `encodings`; a stats-level cluster
summary), add what this increment needs (for example the `op` of an
inclusion proof and updatable ledger tables), keep the rule checker and the
lint passing, and record everything in the changelog.

## Security requirements

- No SQL is built from request text. Identifiers go through an
  identifier-quoting function and are validated against the catalogs where
  they name existing objects; values are bind parameters; privilege and
  object-type keywords come from closed enumerations.
- Mutations run with the least privilege that works: role and grant changes
  run as the signed-in administrator's own connection where PostgreSQL
  permits it, so that PostgreSQL's own permission checks apply in addition
  to the cluster-role check; only operations that need it use the service
  role.
- Every mutation is audited with actor, operation, subject and outcome;
  passwords, tokens and query text of the workbench are never written to the
  audit file or the logs (the workbench records that a statement ran, its
  length and a hash, not its text).
- The instance manager validates every path it is given and never follows a
  path outside its data directory.

## Tests required

- Unit tests for the task engine: step ordering, resume after interruption
  at every step boundary, lease expiry and takeover, cancellation, the
  one-task-per-subject rule.
- Integration tests against real instances started by the instance manager
  itself (in WSL): the whole `init-cluster` flow from an uninitialized node
  to a signed-in administrator; create, update and drop a database with each
  combination of sharding and ledger settings; distribute a table and enable
  the ledger on a table; every role and grant operation, including the
  refusals; network rule changes taking effect; ledger digest, verification
  and proof through the API; the workbench as an unprivileged role being
  stopped by PostgreSQL's permissions; instance restart; alerts raised and
  cleared; metrics present in both forms.
- Contract tests as before: routing completeness, response validation for
  every implemented operation, and the "no invented values" check.
- A negative security test set: SQL metacharacters and quoting in every
  name and value the API accepts; a non-administrator calling each mutation;
  path traversal in every path-like input.

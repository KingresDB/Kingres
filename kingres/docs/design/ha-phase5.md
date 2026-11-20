# High availability: design of phase 5

Status: accepted for implementation, 2026-11-14.
Builds on sharding 0.3 ([shard-phase4.md](shard-phase4.md)) and on the
multi-server control plane ([control-plane-3.md](control-plane-3.md)).
Phase 5 of the roadmap in [architecture.md](../architecture.md): standbys
for every shard group, failover, a client entry point on every server,
distributed deadlock detection and coordinated restore points.

It changes one earlier decision: the failover engine is part of `kingresd`,
not a bundled Patroni with etcd
([ADR 0021](../adr/0021-failover-in-kingresd.md), which supersedes ADR 0012).

## Part A: standbys

### Placement

Every shard group has one primary and `replicaCount` standbys (a cluster
setting, 0 to 3; ADR 0011). Standbys are placed by the chained rule of the
architecture ("Writes on any node"): the standbys of the group whose primary
is on server *i* go to the next servers in a fixed ring order, skipping
servers of the same server group (rack or zone) while other choices exist. A
server therefore runs one primary and up to `replicaCount` standby
instances, each with its own data directory and port. With fewer servers
than `replicaCount + 1` the cluster runs with the standbys it can place and
reports the groups as under-replicated; it never places two instances of one
group on one server.

### Creating and maintaining a standby

`kingresd` on the target server creates a standby with `pg_basebackup` from
the group's primary over a certificate-authenticated replication connection,
with a physical replication slot per standby created on the primary first,
and starts it in recovery with `primary_conninfo` and `primary_slot_name`.
The standby is registered in the sharding catalog as a node with
`role = 'standby'` (it owns no buckets and receives no forwarded statements
in this phase).

Durability follows the per-database level (ADR 0011) through
`synchronous_standby_names` on the primary: `ANY 1 (<the group's standbys>)`
when the group has at least one standby that is connected and streaming, and
empty otherwise. `kingresd` on the primary's server maintains it. Removing
the last streaming standby from the list is what lets a group keep accepting
writes when its standby is lost; it is done only after the standby has been
unreachable for longer than a configurable grace period, it is recorded as
an alert ("group N is running without a synchronous standby"), and it is
never done while a failover of that group is being decided. This is the
usual trade between availability and the guarantee that an acknowledged
commit exists on two servers, and the documentation states it in those
words.

A standby that falls too far behind for its slot, or that was the old
primary of a failed-over group, is rebuilt: `pg_rewind` when possible,
otherwise a new base backup. Slots of removed standbys are dropped, so that
a dead standby cannot fill the primary's disk; a slot that retains more WAL
than a configurable limit raises an alert first and is dropped when the
limit is exceeded.

Changing `replicaCount` is a task that adds or removes standbys group by
group.

## Part B: failover

### What a failover is

Promoting one standby of a group to primary and making every part of the
cluster agree on it:

1. choose the standby that has replayed the most WAL among those that are
   reachable (ties broken by ring order);
2. make sure the old primary cannot accept writes any more (fencing, below);
3. promote the chosen standby and wait until it accepts writes;
4. in one atomic catalog change, swap the roles of the two nodes in the
   sharding catalog (`kingres.promote_node`, part C) so that routing on
   every node goes to the new primary, and raise the map version;
5. point the group's other standbys at the new primary, and set its
   `synchronous_standby_names`;
6. resolve prepared transactions of the group (part C);
7. schedule the old primary to be rebuilt as a standby when its server is
   back.

A **switchover** is the same with a healthy old primary: it stops accepting
new transactions, waits for the chosen standby to replay everything, and
then the steps above run with no possibility of lost commits.

### Fencing by lease

The danger in any failover is two primaries for one group. PostgreSQL
cannot prevent it by itself, so `kingresd` does, with leases:

- The `kingresd` that runs a primary instance holds a **lease** for that
  group. It renews the lease every `lease-renew` (default 2 s) by getting
  acknowledgements from a majority of the cluster's servers (over the
  internal API). The lease is valid for `lease-duration` (default 10 s) from
  the moment the renewal round **started**.
- If a renewal has not succeeded by `lease-duration` minus a safety margin
  (default 3 s, covering clock rate differences and scheduling delay), that
  `kingresd` fences its own primary: it stops the instance (immediate
  shutdown) and does not start it as a primary again until it holds a lease
  or has been told it is now a standby. If `kingresd` itself is not running,
  nothing renews the lease; to cover a dead or hung `kingresd` next to a
  live primary, the instance is started under a small watchdog process that
  stops it when the lease file it reads has expired.
- Servers grant a lease for a group to one holder at a time and remember
  until when; a majority will not grant the lease to a new holder before the
  old one has expired **plus** the safety margin.

A failover for a group therefore proceeds only when a majority of servers
agree that the old holder's lease has expired and grant the lease to the
server of the chosen standby. After that point the old primary is either
stopped or will be within the margin, and promotion is safe. Time is
measured with monotonic clocks on each server; only durations travel, never
wall-clock times.

Consequences, stated in the documentation: a cluster needs at least three
servers for automatic failover to tolerate the loss of one; with two
servers, the loss of either leaves no majority, so no automatic failover
happens and the surviving primaries fence themselves when their leases
expire (the alternative, continuing without a majority, cannot be made safe).
A one-server cluster grants itself its leases. An administrator can force a
failover without a majority with an explicit "I confirm the old primary is
down" override, which is audited.

### The map group and the manager

The same mechanism covers the map group. After a failover of group 0 the
manager leader moves with it (ADR 0018 still holds: the leader is the server
of the map group's primary). Because the system database is on the map
group, the decision to fail over group 0 cannot depend on it: lease state is
kept by each `kingresd` in its own data directory, and the failover of any
group is driven by whichever server holds the group's most advanced standby
once it wins the lease, recording the task in the system database as soon as
that is writable again.

### Automatic failover and its rules

Automatic failover is a cluster setting (off by default until an
administrator turns it on; the panel explains the requirements). With it on,
a group fails over without an administrator when its primary's lease has
expired, under the rules taken from the Couchbase model in the architecture:

- the timeout before acting is `autoFailover.timeoutSeconds` (default 120,
  minimum 30 here because of the lease durations);
- at most `autoFailover.maxCount` (default 1) automatic failovers happen
  until an administrator resets the counter or a rebalance completes;
- a group without a reachable, streaming-capable standby is never failed
  over automatically (there is nothing to promote), and a standby that was
  not synchronous at the time of the failure is promoted automatically only
  if the group's databases all allow asynchronous durability; otherwise the
  group waits for an administrator, who is told how much may be lost;
- when several groups lose their primaries at once because one server
  failed, they count as one event.

Manual failover and switchover are always available to an administrator
through the API and the panel, with the same fencing.

## Part C: the sharding extension (version 0.4)

- **Standby nodes.** `kingres.add_node(..., role => 'standby')` registers a
  standby of an existing group; standbys own no buckets and are skipped by
  routing, catalog synchronization and reference-table writes (they receive
  all of these through physical replication).
- **`kingres.promote_node(node_id int)`**: on the map group primary, in one
  atomic catalog change, makes the given standby the primary of its group
  and the former primary a standby, and raises the map version. Connections
  cached to the old primary are dropped on every node. For the map group
  itself the function is run on the newly promoted primary, which is the
  only node that can write the catalog at that moment.
- **Prepared transactions across a failover.** Recovery decides by the
  origin's decision log, which is an ordinary table and therefore exists on
  the origin's standby after promotion. The transaction identifier embeds
  the origin **group**, not the origin node, so that the new primary of a
  group resolves what the old one started. "The origin transaction is no
  longer in progress" is trivially true for transactions of a previous
  timeline; the recovery rule records the timeline and treats every
  undecided prepared transaction of an older timeline as aborted, after the
  ordering rule of the 0.2 fix round (check the decision log last, with a
  fresh snapshot).
- **Distributed deadlock detection.** The recovery worker on the map group
  primary periodically (default every 2 s, only when some backend has waited
  longer than `deadlock_timeout`) collects from every primary the edges
  "distributed transaction A waits for distributed transaction B" (from
  `pg_locks` and the transaction identifiers the extension assigns to
  transactions that span nodes), builds the wait-for graph, and cancels the
  youngest transaction of each cycle on its origin node with an error that
  says a distributed deadlock was detected. Local deadlocks stay PostgreSQL's
  business.
- **Coordinated restore points.** `kingres.create_restore_point(name text)`:
  on the map group primary, blocks new multi-node commits, waits for those
  in flight to finish their second phase, calls `pg_create_restore_point`
  with the same name on every primary, and releases. Restoring every server
  to that name gives a state in which no distributed transaction is half
  committed. The function reports each node's location.

## Part D: the client entry point

`kingresd` listens on the client port (default 5432 on every server; the
instances themselves use other ports) and forwards each incoming PostgreSQL
connection, without interpreting SQL, to the primary instance on the same
server if it holds a valid lease, and otherwise to another server's primary
taken from the catalog. A second port (default 5433) forwards to a local
standby for reads that tolerate lag, when hot standby reads are enabled.
The proxy passes the startup packet through unchanged, so authentication
(including TLS, which the client negotiates end to end with the instance) is
exactly PostgreSQL's; it only chooses the destination. When the destination
changes because of a failover, existing connections are closed, and new
ones go to the new primary. Clients may list several servers; any of them
accepts writes while it can reach a primary.

> **Note of 2026-11-20.** The code differs from the default stated above: the client entry point is **off** unless it is configured. `client-listen` (`KINGRES_CLIENT_LISTEN`) and `client-standby-listen` (`KINGRES_CLIENT_STANDBY_LISTEN`) are empty by default, and `kingresd` forwards no client connection while both are empty (`internal/config/ha.go`, `cmd/kingresd/ha_proxy.go`; the control-plane README has the table of settings). The ports 5432 and 5433 are the ones the design proposes for an operator who turns it on; in the image and the compose file 5432 is the port of the PostgreSQL instance itself.

## What the control plane exposes

Contract additions: standbys in the server and replication views (already
modelled), `PATCH /settings/cluster` for `replicaCount` driving the standby
task, `POST /servers/{id}/failover` (hard or graceful) and
`POST /replication/restore-preferred-primaries` becoming real, lease state
and the automatic-failover counter in the cluster overview, the alerts
`groupUnderReplicated`, `groupWithoutSynchronousStandby`, `leaseLost` and
`failoverPerformed`, and a restore-point operation.

## Tests required

Extension (TAP): promotion with prepared transactions in every state on the
old primary, resolved correctly by the new one; routing after
`promote_node` from every node, including statements in flight; a deadlock
across two and across three nodes detected and one victim cancelled; a
restore point taken under a multi-node write load, each server restored to
it, and no transaction half committed.

Control plane (three or more `kingresd` processes in WSL): standbys created
for every group and streaming; `replicaCount` raised and lowered;
switchover with a write load and no lost acknowledged commit at synchronous
durability; hard failover after killing a primary's server (instance and
`kingresd`), with the load continuing on the new primary and every commit
that was acknowledged at synchronous durability present; the old primary
returning and being rebuilt as a standby; **partition tests**, the heart of
this phase: a primary's server cut off from the majority while clients can
still reach it — it must stop accepting writes before any other server
promotes, shown by a client that keeps writing to it and by comparing what
it acknowledged with what survives; the minority side never promotes;
`kingresd` killed while its primary keeps running (the watchdog stops the
instance); clock steps and a slow `kingresd` (paused with a signal) do not
lead to two primaries; the map group's own failover and the manager moving
with it; the automatic-failover rules (timeout, maximum count, no standby,
asynchronous standby with a synchronous database); the proxy sending new
connections to the new primary.

A test that cannot be made deterministic is run many times in a loop, and
the count of runs is reported.

<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Control plane: design of the third increment (several servers)

Status: accepted for implementation, 2026-11-14.
Builds on the second increment ([control-plane-2.md](control-plane-2.md)) and
on `kingres_shard` 0.2 ([shard-phase2a.md](shard-phase2a.md)). It makes a
cluster of more than one server: servers trust each other, a server can join
or be added and (while it holds no data) removed, every `kingresd` forwards
changes to the manager leader, and the operations that the second increment
left out for lack of a decision are finished. Moving data between servers
(rebalance) and failover are the next increments; they need bucket movement
in the sharding extension and standby management first.

This increment revises the contract to 0.4.0.

## Decisions

### D1. Cluster certificate authority and server certificates

Creating a cluster creates a certificate authority for it: an ECDSA P-256
key and a self-signed certificate, kept by the manager leader under
`<data-dir>/pki/` with the key readable only by the service user. Every
server gets a certificate signed by it, with the server's host name and
addresses as subject alternative names. The CA signs only server
certificates of this cluster; it is not used for users.

Each server uses its certificate for three things:

1. the TLS listener of `kingresd` (unless the operator configures their own
   certificate for the public API, in which case the cluster certificate is
   used only for the internal API);
2. the PostgreSQL instances it manages (`ssl = on`, server certificate and
   key, the cluster CA as `ssl_ca_file`);
3. as a client certificate, when its instances connect to instances on other
   servers and when its `kingresd` calls another `kingresd`.

Certificates are valid for one year and are renewed by the leader when less
than a third of the lifetime is left; the CA is valid for ten years. Rotation
of the CA itself is not in this increment and is recorded as a limit.

### D2. The internal API

`kingresd` serves a second set of endpoints under `/internal/v1/`, on the
same listener, that only accepts connections presenting a client certificate
signed by the cluster CA (mutual TLS); the public API never accepts a client
certificate as authentication and the internal API never accepts a session.
It is not part of the public contract; it is specified in
`kingres/control-plane/internal-api.md` and versioned with the binary. A
server refuses an internal call from a peer whose binary speaks a different
internal version, with an error that names both versions.

Through it the leader asks a server's `kingresd` to: report its state and
resources, create, configure, start, stop and remove an instance, apply the
managed `pg_hba.conf` rules, run a statement on a local instance through the
local socket, and return local logs and metric samples.

### D3. Trust between PostgreSQL instances

Instances of different servers authenticate to each other with the server
certificates: `pg_hba.conf` on every instance gets a managed rule
`hostssl all all <address of each other server> cert map=kingres_nodes`, and
`pg_ident.conf` maps the certificate name of any server of the cluster to
any database role. `kingres_shard.node_conninfo` carries the client
certificate, key and CA for outgoing node connections. A statement forwarded
by the sharding extension therefore runs on the other server as the same
role that ran it locally, without that role's password being known to the
cluster. The consequence is stated plainly in the documentation: every
server of a cluster is fully trusted by every other; someone who controls
one server's key can act as any role on all of them.

### D4. Joining and adding a server

Both `POST /setup/join-cluster` (called on the new server, with its setup
token, the address of an existing server and administrator credentials) and
`POST /servers` (called on the cluster, with the new server's address and
its setup token) lead to the same task on the leader, `server.add`:

1. **Introduce.** The side that starts the operation authenticates to the
   other: with administrator credentials when joining, with the new
   server's setup token when adding. The new server generates its key and a
   certificate request; the leader verifies the request, signs it and
   returns the certificate and the CA certificate. The first contact is over
   TLS without a common CA, so the caller must pin it: the request carries
   the SHA-256 fingerprint of the other side's certificate when the
   administrator supplied it, and when it did not, the fingerprint that was
   seen is recorded in the task and shown to the administrator (trust on
   first use). The wizard and the "add server" dialog display the
   fingerprints so that they can be compared.
2. **Prepare the instance.** The leader asks the new server to create its
   instance for a new shard group, with the cluster's settings and the
   managed rules, and to start it.
3. **Copy the global objects.** Roles (with their password verifiers and
   memberships) and every user database that exists in the cluster are
   created on the new instance, with the extensions each database uses, so
   that the node can take part in every database.
4. **Register.** For every database that uses `kingres_shard`, the leader
   calls `kingres.add_node` on the map group primary; the catalog and the
   tables' parents reach the new node through the extension. The server is
   recorded in `kingres_cp.server`.
5. **Finish.** The new server's state becomes `clustered`, its setup token
   is removed, and it is listed with status `pendingRebalance`: it holds no
   buckets until a rebalance moves some to it.

Every step is idempotent and the task can resume after any of them. A task
that fails before step 4 leaves the cluster unchanged and tells the new
server to discard what it created.

After this increment, operations that act on "every primary of the cluster"
(creating and dropping databases, role and grant changes, network rules)
really run on every server, as one task with one step per server; a server
that is down makes the task wait and report which server it is waiting for,
it does not skip it.

### D5. Removing a server

`DELETE /servers/{id}` removes a server that owns no buckets in any database
(the state a new server is in before a rebalance, or after one has drained
it). The task unregisters the node from every database, removes it from the
managed rules of the others, asks the server to stop and delete its
instances, revokes its certificate (the leader keeps a list of revoked
serials that the internal API and the generated `pg_hba` rules honour; the
PostgreSQL instances get the list as a certificate revocation list file),
and resets it to `uninitialized`. A server that still owns buckets is
refused with the contract's conflict problem and a hint to rebalance first.
Removing the server of the map group is refused.

### D6. Forwarding to the leader

Every `kingresd` knows the leader: the server that holds the primary of
shard group 0, read from the catalog. A mutating cluster operation that
arrives at another server is forwarded to the leader over the internal API
together with the authenticated identity (role name and cluster role), and
the leader's answer is returned unchanged; the leader trusts the identity
because only a server of the cluster can call the internal API. Reads are
answered locally from the server's own copy of the catalog and, for values
that only another server knows (its resources, its logs), fetched from that
server. `ClusterOverview.managerLeader` is filled.

### D7. Operations finished in this increment

- Cluster settings update and server groups (create, delete, assign).
- Inclusion proofs, and the list and detail of ledger verifications.
- Starting and stopping an instance, and leaving a cluster, with the rule
  that an operation which would stop the instance holding the system
  database is refused with a conflict problem that says why.
- Cluster-wide logs, alerts and metrics gathered from every server.

### D8. Contract revision 0.4.0

Add what D4 to D7 need (fingerprints in the join and add requests and in
the node state; per-server step detail in tasks that run on every server;
`managerLeader`), and resolve the points raised against 0.3.0:
`LedgerVerification.taskId` nullable, the shape of `InclusionProof` aligned
with what the extension returns (the proof document is passed through as an
opaque object next to the fields the panel shows), the subject of a
verification task, the alert kinds `ledgerVerificationFailed`,
`ledgerPendingTooOld` and `versionMismatch`, and the conflict answer for
stopping the only instance.

## Security requirements

- Private keys never leave the server that generated them; only certificate
  requests and certificates travel.
- The internal API authenticates by client certificate only, checks the
  revocation list, and authorizes by a fixed table of which calls a peer may
  make; it never executes a path, a program name or SQL text taken from a
  request, except the statement-execution call, which is restricted to the
  leader as caller and is logged with its length and hash.
- Role synchronization copies password verifiers, never passwords, and never
  writes either to a log, a task row or the audit file.
- A join that fails or is abandoned leaves no certificate signed for a
  server that is not in the cluster: signing happens in the same step that
  records the pending server, and an abandoned pending server's certificate
  is revoked.
- All of the second increment's requirements still apply.

## Tests required

- Unit tests for the certificate authority (issuing, renewal threshold,
  revocation, refusing a request with the wrong key type or names) and for
  the forwarding logic.
- Integration tests in WSL with three `kingresd` processes, each with its
  own data directory, ports and instance, started by the tests: create a
  cluster on the first; join the second from its own API and add the third
  from the cluster's API; both fingerprint modes; a database created before
  and one created after the servers were added exist on all three with their
  roles; a distributed table created afterwards has buckets on all three; a
  statement run on each server for a key owned by each other server returns
  the right row over certificate-authenticated connections; a role created
  on one server can sign in to the panel of another; a mutation sent to a
  non-leader is forwarded and audited once; removing an empty server, and
  the refusal for one that owns buckets; a join interrupted at every step
  resumes or cleans up; a server down during a cluster-wide task makes the
  task wait and then finish when the server is back.
- Negative tests: an internal call without a certificate, with a certificate
  of another CA, with a revoked certificate, and from a peer with another
  internal version; a join with a wrong fingerprint; replay of a setup token.
- Contract tests as before, updated for 0.4.0.

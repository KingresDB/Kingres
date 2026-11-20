<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# ADR 0014: One package and a panel on every node

- Status: Accepted
- Date: 2026-11-14
- Source in architecture.md: "One package, and a panel on every node"; "The Couchbase reference pattern"; Open decisions #9; Risks table

## Context

Sourced facts: in Couchbase every node runs the same cluster-manager processes, and the Web Console and CLI are both clients of the node's REST API on port 8091; CockroachDB serves its DB Console from every node; etcd publishes a Go package for embedding a server. The document found no PostgreSQL sharding product with a Couchbase-like panel (the inference is that this is a market gap).

**[Recommendation]** in the document: one package `kingresdb` (`.deb`, `.rpm`, Docker image; Linux first) containing the PostgreSQL fork, both extensions, `kingresd` (agent, manager, client router, REST API, embedded panel), `kingres-cli`, the standalone verifier, an embedded etcd member, Patroni with its runtime and a single-binary metrics store. One systemd unit, `kingresdb.service`. First run offers "create a new cluster" or "join an existing cluster"; adding a node and rebalancing are separate steps. The panel has "This server" and "Cluster" scopes. Setup requires a one-time token, and the wizard listens only on localhost until used. Open decision #9 recommends Ubuntu 24.04 LTS and Debian first. A Windows server package is out of scope for the first releases.

## Decision

KingresDB ships as one installable package, and every node serves the same panel and REST API. A single node is a valid deployment. Cluster-state operations received by a node that is not the manager leader are forwarded to the leader. Ubuntu 24.04 LTS and Debian are the first target systems.

## Consequences

- Easy installation and no dedicated management server; management has no single point of failure.
- The package is large and complex: it bundles PostgreSQL, Go services, embedded etcd, Python and Patroni, and a metrics store.
- Every bundled component's licence must be checked. **[Needs verification]** Licences of all bundled components, the package size, how embedded etcd membership grows as nodes join (voting members versus learners), bundling Patroni with Python in `.deb` and `.rpm`, and the rolling-upgrade procedure (minor upgrades one node at a time after a switchover; major PostgreSQL upgrades are separate) must be confirmed before packaging work starts.
- An uninitialised node could be claimed by someone on the network. The document lists this as a risk with the one-time token and localhost-only mitigation.
- RHEL-family packages come later. Windows packages are out of scope initially.
- The panel is on every node, so every node must run the same version of the panel and API during upgrades; the mixed-version policy is not designed.

## Alternatives considered

- **A dedicated management server** (VTAdmin pattern with separate API and web components). The document cites VTAdmin as a pattern for the API layer, but the chosen model follows Couchbase and CockroachDB with an identical node set; a dedicated server would add a single point of failure for management.
- **Separate installs of agent, manager, etcd and Patroni.** Not chosen: the goal is the Couchbase experience of installing one package and opening the node's own panel. The document states `kingres-agent` and `kingres-manager` are roles inside `kingresd`, not separate installs.
- **RHEL family first.** Listed in decision #9; Ubuntu and Debian recommended first without further reasoning in the document.

## Note of 2026-11-20: etcd and Patroni

The package does not contain an embedded etcd or Patroni: [ADR 0021](0021-failover-in-kingresd.md) moved failover into `kingresd` and does not bundle them. `kingres-cli` is planned and is not built; the control plane has two executables today, `kingresd` and `kingres-watchdog`.

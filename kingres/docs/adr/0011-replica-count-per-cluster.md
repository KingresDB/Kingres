<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# ADR 0011: Replica count per cluster, durability level per database

- Status: Accepted
- Date: 2026-11-14
- Source in architecture.md: Open decisions #6; "Databases, users and servers in the panel"; "HA, replication, DDL and backup"

## Context

Sourced facts: physical replication copies a whole PostgreSQL instance, so a per-database replica count would need logical replication, which is slower and does not carry DDL (stated in the Open decisions table; the document also lists, as **[Needs verification]** general knowledge, that logical replication does not carry DDL, sequences and TRUNCATE the way it carries data). PostgreSQL's `synchronous_commit` setting can be set per database. Couchbase sets replica counts per bucket, which is the pattern behind the "Databases page: create a database like a bucket".

**[Recommendation]**: replica count is per cluster. Durability level per database (asynchronous, synchronous quorum, synchronous with apply) maps to `synchronous_commit`. Quorum-type commit with `synchronous_standby_names = 'ANY 1 (...)'` is suggested to prevent loss of acknowledged commits.

## Decision

The replica count is a cluster setting, applied by physical replication of whole groups. Each database chooses its durability level, which maps to `synchronous_commit`. The Databases page shows replica count as a cluster value and durability as a per-database value.

## Consequences

- Replication stays physical and simple: standbys are exact copies, DDL needs no special handling, and failover through Patroni works on whole instances.
- A database cannot have fewer or more copies than the cluster setting. Needing, for example, three copies of one database and one of another requires a different mechanism (separate node pools or logical replication), which is not offered.
- Changing the replica count is a cluster-wide operation followed by a rebalance, as the Databases page table notes.
- The document's Databases table says "Replica count: see 'Open decisions'" in the per-database creation dialog, which is now to be read as "shown, not editable per database". The creation dialog design must reflect that. The `kingres.database` registry in the document still contains a `replica_count` column; this ADR makes it informational unless a later decision changes it (architecture.md is ambiguous here; the project owner should confirm and `architecture.md` should be updated by its owner).
- **[Needs verification]** Whether per-database `synchronous_commit` in a shard setting is honoured by standbys in quorum mode in the way the panel will present it has to be confirmed against the base version documentation. Fencing and in-doubt transactions after failover are also from background knowledge (ADR 0009).

## Alternatives considered

- **Per-database replica count with logical replication or separate node pools.** Rejected: slower, DDL is not carried, and the machinery would be larger.
- **Per-database replica count with physical replication.** Not possible: physical replication copies a whole PostgreSQL instance (Open decisions #6).

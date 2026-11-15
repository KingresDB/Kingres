<!-- SPDX-License-Identifier: PostgreSQL -->
# ADR 0019: Control-plane state lives in a system database with embedded migrations

- Status: Accepted
- Date: 2026-11-14
- Source: [design/control-plane-2.md](../design/control-plane-2.md), decision D2; architecture.md, "Databases, users and servers in the panel"

## Context

The control plane has state of its own that is not part of either extension's catalog: tasks, per-database settings chosen in the create-database dialog, cluster settings, server groups, alerts and the managed network-access rules. The extensions' catalogs exist once per user database, so they cannot hold cluster-wide records, and the first increment of `kingresd` had nowhere to keep these values and returned "not available".

## Decision

`kingresd` keeps this state in the database `kingres_system` on the map group, in the schema `kingres_cp`. The schema is defined by versioned SQL migrations embedded in the `kingresd` binary and applied by the manager leader at start-up, each migration in one transaction and recorded in a migration table. A binary older than the schema refuses to start.

## Consequences

- The state is replicated, backed up and failed over together with the map group, with no extra machinery.
- The schema version is tied to the binary, so an upgrade is "install the new binary and start it"; a downgrade below the schema version is refused instead of corrupting state.
- The control plane depends on the map group being reachable for anything that reads or writes this state, as in ADR 0008 and ADR 0018.
- A third SQL surface exists next to the two extensions. It is owned by `kingresd` alone and is not a public interface.

## Alternatives considered

- **A third extension for the registry.** It would version the schema with `ALTER EXTENSION`, but ties control-plane releases to extension releases and needs the extension installed before the control plane can start.
- **Files on each server.** They are not shared between servers and not transactional.

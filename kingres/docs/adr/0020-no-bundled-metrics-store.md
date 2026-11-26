<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# ADR 0020: No bundled metrics store; kingresd samples and exports metrics itself

- Status: Accepted
- Date: 2026-11-14
- Supersedes: the "single-binary metrics store" item of the package contents in architecture.md, "One package, and a panel on every node"
- Source: [design/control-plane-2.md](../design/control-plane-2.md), decision D6

## Context

The architecture proposed bundling a metrics store (Prometheus or VictoriaMetrics) in the package to feed the panel's charts. That is one more service to ship, configure, supervise and upgrade on every node, and one more licence and security surface, for charts that show recent values.

## Decision

`kingresd` samples a fixed set of metrics from the instances it manages every few seconds into an in-memory ring buffer, answers the panel's metric queries from it, and exposes the same values at `/metrics` in the Prometheus text format. Nothing else is bundled. Operators who want history, alerting rules or long retention point their own Prometheus at that endpoint.

## Consequences

- The package is smaller and has one service.
- The panel's charts cover only the retention of the ring buffer and start empty after a `kingresd` restart.
- Cluster-wide charts need each server's `kingresd` to answer for its own instances; aggregation across servers is the manager's job and arrives with multi-server support.
- Integration with existing monitoring is the standard one.

## Alternatives considered

- **Bundle a metrics store.** Rejected for the reasons above; it can be reconsidered if users ask for built-in history.
- **Store samples in the system database.** It would add write load to the database being monitored.

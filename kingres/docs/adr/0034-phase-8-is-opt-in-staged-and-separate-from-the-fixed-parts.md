<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# ADR 0034: Phase 8 is opt-in, staged, and kept apart from the code other lines change

- Status: Proposed
- Date: 2026-11-28
- Source: [../design/phase8-global-snapshot.md](../design/phase8-global-snapshot.md) (sections 8 and 11), [../design/phase8-multinode-ledger.md](../design/phase8-multinode-ledger.md) (sections 8 and 12), [ADR 0023](0023-one-extension-source-for-several-majors.md)

## Context

Phase 8 touches the server (hooks), a new extension, `kingres_shard` (the files that the phase 15 and join lines change now), `kingres_ledger`, `kingresd`, the verifier and two new binaries. Clusters of 0.1.0 exist, and rolling upgrades mix versions.

## Decision

1. Both parts are off until an operator switches them on: `snapshot.mode` (a cluster setting; a checked task that refuses until every instance and every extension is ready), and witnesses and the notary (configured). Off behaves as 0.1.0.
2. The global snapshot lives in a new extension, `kingres_snapshot`. `kingres_shard` gets one new version (0.11, new objects only, after the lines that change 0.10 have merged) with small commits in the existing files. Released scripts stay identical to tag `kingres-0.1.0-pg18`.
3. The ledger witnesses and the notary are separate binaries. `kingresd` keeps its standard-library-first dependency policy, and CometBFT enters only `kingres-notary` (pinned, licence checked, SBOM).
4. Order: spike and gate, hooks, extension and test harness (no `kingres_shard` edits), the `kingresd` surface, then the `kingres_shard` integration, move and failover interplay, and the ledger cut. Ledger layer 1 can ship first.
5. Every REST change goes through the contract with the matching `kingres-cli` command and console page.
6. Everything builds from one source for PG18 and PG19.

## Consequences

- Early increments can run beside the other lines; the `kingres_shard` increments wait for their merge.
- The first release with phase 8 ships it off; the default changes by a later ADR.

## Alternatives considered

- On by default: a restart, a horizon and a new binary change behaviour for every existing cluster.
- The clock in `kingres_shard`: conflicts with the lines changing it and couples a single-node feature to sharding.
- CometBFT embedded in `kingresd`: moves a large dependency tree into the control plane.

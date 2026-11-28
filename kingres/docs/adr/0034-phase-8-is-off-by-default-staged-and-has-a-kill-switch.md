<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# ADR 0034: Phase 8 is off by default, staged, has a kill switch, and ships as one release with both parts

- Status: Proposed
- Date: 2026-11-28
- Source: [../design/phase8-atomic-visibility.md](../design/phase8-atomic-visibility.md) (sections 9, 10 and 13), [../design/phase8-multinode-ledger.md](../design/phase8-multinode-ledger.md) (sections 8 and 12), [ADR 0023](0023-one-extension-source-for-several-majors.md)

## Context

Phase 8 touches `kingres_shard` (the files the phase 15 and join lines change now), `kingresd`, the console, the CLI, the verifier and new binaries. Clusters of 0.1.0 exist, and rolling upgrades mix versions. The owner asked on 2026-11-28 for explicit risk minimisation and decided that **both parts ship together in one release, including the BFT notary**, and that the project runs its own test witnesses and test validators (no outside organisation yet).

## Decision

1. **Correctness before code:** the model P8-M with a mutant for every assumption and the differential tests come first. A Jepsen-style harness (node crash, network cut, wall-clock step, failover mid-transaction, in-doubt 2PC at every fault point) gates every increment, on PG18 and PG19, with negative controls that must fail on broken builds. An independent review per increment, with a record.
2. **Never a wrong answer; an error is fine:** every uncertain point of the protocol fails the statement with a clear SQLSTATE (ADR 0031, design 7).
3. **Off by default, per database:** modes `off`, `record`, `observe`, `enforce`, and a scope `autocommit`, `statements`, `transactions`. The mode may not be raised until every node has the version and has been recording for the retention window; the change is a checked task.
4. **Staged rollout:** record, observe (validates and counts, never waits or fails), enforce for autocommit statements, then read-only parts of transactions, then `REPEATABLE READ` first queries (which may ship disabled).
5. **A kill switch:** one setting, a reload, cluster-wide through `kingres-cli`, returns to today's behaviour for every new statement at once.
6. **Metrics and alerts** for wait times, rounds, decided in-doubt transactions, errors by SQLSTATE and a monotonic-clock anomaly.
7. **Internal-API risk:** one table of every PostgreSQL internal that the extension relies on, in the design and in `dev/upstream-sync.md`, checked first on every minor and major upgrade; version differences only in `pg_compat.h`.
8. **One release with both parts:** atomic visibility and the multi-node ledger (witnesses and the BFT notary). `kingres_shard` 0.11 holds the new objects (new files, small commits in existing ones after the lines that change 0.10 have merged); released scripts stay identical to tag `kingres-0.1.0-pg18`. The witness and the notary are separate binaries; CometBFT enters only `kingres-notary`.
9. Every REST change goes through the contract with the matching `kingres-cli` command and console page.

## Consequences

- The release is larger and later than a staged one; the owner chose it. Risk is contained by the harness gate per increment and by the off-by-default modes.
- Early increments (model, harness, spike, `kingresd` surface, the whole ledger part) run beside the other lines; the `kingres_shard` increments wait for their merge.
- The notary is exercised by the project's own test validators; a real consortium use is not claimed.

## Alternatives considered

- On by default: a new write per distributed commit and a new read protocol would change behaviour for every cluster.
- Two releases (witnesses first): rejected by the owner.
- A cluster-wide switch only: the owner asked for per-database enabling.

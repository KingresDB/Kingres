<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# ADR 0033: The multi-node ledger is witnesses first and a BFT notary of checkpoints second; no Raft, no replicated SQL

- Status: Proposed
- Date: 2026-11-28
- Source: [../design/phase8-multinode-ledger.md](../design/phase8-multinode-ledger.md), [ADR 0013](0013-ledger-tables-and-anchoring-first.md), [../architecture.md](../architecture.md) ("Phase 3: multi-node with consensus")

## Context

ADR 0013 accepted consensus only with a concrete consortium use case. On 2026-11-28 the owner decided to build phase 8, including a multi-node ledger with consensus. The threats beyond phase 7 are a split view or rollback by one operator, and agreement among organisations that do not trust each other's operators. Replicated SQL execution among organisations (the IBM design) needs a deterministic SQL contract and a replicated ledger table, which the product does not have. Raft tolerates crashes only, and every member is in one trust domain; the cluster already has leases and failover.

Sourced: C2SP tlog-cosignature (a witness cosigns a checkpoint after verifying consistency; clients can require a quorum), CometBFT (BFT, one third of voting power, ABCI), the IBM prototype's costs as quoted in architecture.md.

## Decision

1. Layer 1: independent witnesses follow the published checkpoints, verify consistency and cosign (C2SP formats); the verifier accepts a checkpoint when k of n listed witnesses cosigned it.
2. Layer 2: a notary, a CometBFT network run by member organisations, whose application is a deterministic state machine that verifies and records members' signed checkpoints and consistency proofs. It executes no SQL.
3. Raft is not used for the ledger.
4. Replicated SQL (order-then-execute) is not built; it is reopened only with a named consortium use case.
5. Consensus is over closed checkpoints, never on a transaction's commit path; the chain format is unchanged.
6. With the global snapshot on, the aggregate may be a consistent cut (leaf format 2).

## Consequences

- Phase 7's limits stay and are stated: nothing prevents a change; the correctness of data is not proven.
- A notary needs at least four validators for any fault tolerance; the console says so for fewer.
- Layer 1 can ship alone and before the global snapshot.

## Alternatives considered

- Raft (etcd/raft, hashicorp/raft): crash faults only; duplicates failover; one trust domain.
- CometBFT executing SQL: determinism contract, no maintained public implementation, throughput drops with orderers.
- Anchoring only (phase 7): no k-of-n proof and no agreement among organisations.
- A global chain with one sequencer: limited to one node's commit rate; one trust domain.

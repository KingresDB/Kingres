<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# ADR 0033: The multi-node ledger is witnesses and a BFT notary of checkpoints, in one release; no Raft, no replicated SQL

- Status: Proposed
- Date: 2026-11-28
- Source: [../design/phase8-multinode-ledger.md](../design/phase8-multinode-ledger.md), [ADR 0013](0013-ledger-tables-and-anchoring-first.md), [../architecture.md](../architecture.md) ("Phase 3: multi-node with consensus")

## Context

ADR 0013 accepted consensus only with a concrete consortium use case. On 2026-11-28 the owner decided to build phase 8, including a multi-node ledger with consensus, **both layers in one release with the atomic visibility part**, and that the project runs its own test witnesses and test validators because no outside organisation exists yet. The threats beyond phase 7 are a split view or rollback by one operator, and agreement among organisations that do not trust each other's operators. Replicated SQL execution among organisations (the IBM design) needs a deterministic SQL contract and a replicated ledger table, which the product does not have. Raft tolerates crashes only, and every member is in one trust domain; the cluster already has leases and failover.

Sourced: C2SP tlog-cosignature (a witness cosigns a checkpoint after verifying consistency; clients can require a quorum), CometBFT (BFT, one third of voting power, ABCI), the IBM prototype's costs as quoted in architecture.md.

## Decision

1. Layer 1: independent witnesses follow the published checkpoints, verify consistency and cosign (C2SP formats). A trust file lists a **witness policy**: the witnesses, a threshold `k`, and a maximum age of a cosignature for current-state checks (archives verify as of a time). The verifier rejects a policy with `k <= n/2` (a split view needs at least `2k - n` colluding witnesses), and a checkpoint below the threshold, stripped of cosignatures or too old is a failure; it only warns when the trust file has no policy. An epoch change is accepted only with a closing statement at the witnesses' stored head, with one successor per closed log, and is visible and counted, not prevented.
2. Layer 2: a notary, a CometBFT network run by member organisations, whose application is a deterministic state machine that verifies and records members' signed checkpoints and consistency proofs and records equivocation as evidence in a successful transaction. It executes no SQL. **It is built in the same release**, and run by the project's own test validators (four or more) until a real counterparty exists.
3. Raft is not used for the ledger.
4. Replicated SQL (order-then-execute) is not built; it is reopened only with a named consortium use case.
5. Consensus is over closed checkpoints, never on a transaction's commit path; the chain format is unchanged.
6. The consistent cut across chains of the first draft is dropped: it needed a global order (ADR 0032).

## Consequences

- **Trust domain (review).** Witnesses and validators run by the project are one trust domain with the operator they constrain: they prove that the mechanism works under faults, not independence. The default trust file lists no project witness as production trust; the console and the verifier show how many witnesses of a policy are independent; "agreement among organisations" is claimed only with real independent members (design, section 18).
- Phase 7's limits stay and are stated: nothing prevents a change; the correctness of data is not proven; an offline check proves "agreed by `k` witnesses as of the cosignature time", not that the checkpoint is the latest.
- A notary needs at least four validators for any fault tolerance; the console says so for fewer. Over witnesses it adds a total order and a single recorded equivocation; cross-witnessing between organisations gives agreement on histories too, and a real deployment may use only layer 1.
- Both layers are tested by the harness with faulty and partitioned members (design, section 11).

## Alternatives considered

- Raft (etcd/raft, hashicorp/raft): crash faults only; duplicates failover; one trust domain.
- CometBFT executing SQL: determinism contract, no maintained public implementation, throughput drops with orderers.
- Anchoring only (phase 7): no k-of-n proof and no agreement among organisations.
- A global chain with one sequencer: limited to one node's commit rate; one trust domain.
- Witnesses first, notary in a later release: rejected by the owner.

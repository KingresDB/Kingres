<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Phase 8, part 2: a multi-node ledger with consensus (design)

Status: Proposed design, 2026-11-28, revised the same day after an independent review (section 17); not implemented. Where this document says "exists" it
describes `kingres-18` at d1344c1aa9 and names the file.

Builds on [ledger-phase7.md](ledger-phase7.md) (chains per shard group, the aggregate digest,
publication, anchoring, the verifier), [ledger-mvp.md](ledger-mvp.md), [ledger-0.2.md](ledger-0.2.md),
[ADR 0013](../adr/0013-ledger-tables-and-anchoring-first.md), [ADR 0021](../adr/0021-failover-in-kingresd.md),
[ADR 0028](../adr/0028-ledger-tables-are-carried-as-data-and-restart-their-chain.md) and
[phase8-atomic-visibility.md](phase8-atomic-visibility.md). The decisions are the proposed ADRs 0033
and 0034. **This part ships in the same release as atomic visibility (owner, 2026-11-28), both layers
including the BFT notary, run by the project's own test witnesses and test validators.** Statement kinds as in architecture.md: **[Fact]**, **[Recommendation]**,
**[Needs verification]**; a source marked "background" was not re-fetched on 2026-11-28.

## 1. What exists, and what "multi-node ledger with consensus" can mean

### 1.1 Facts

1. **[Fact]** Each shard group has its own chain per database (`kingres.ledger_blocks`,
   `chain_id`), built by the extension's block workers; physical standbys share the chain of
   their primary; a failover keeps the chain (ledger-phase7.md, 5.5). Order inside a block is by
   transaction id, not commit order (`contrib/kingres_ledger/README.md`).
2. **[Fact]** A ledger table cannot be distributed or a reference table (`KS016`), cannot be
   partitioned; it lives on one group. A distributed schema that holds one is pinned
   ([../guide/limitations.md](../guide/limitations.md), "The ledger").
3. **[Fact]** The manager leader (one per cluster, the server of the map group primary, ADR 0018)
   runs the aggregation round: it reads each group's head through `ledger/read`
   (`kingres/control-plane/internal/ledgerround`), builds the aggregate log (tag `0x23`,
   ledger-phase7.md 5.2), signs the checkpoints with a key held in `kingresd`'s files, publishes
   tiles (`internal/ledgerpub`), and anchors the aggregate checkpoint to WORM, RFC 3161 and
   OpenTimestamps targets (`internal/ledgeranchor`).
4. **[Fact]** The verifier (`kingres/verifier`, `kingres-verify`) checks a bundle against a trust
   file, offline, with the standard library only; `monitor` keeps the last checkpoint it saw and
   reports a rollback.
5. **[Fact]** Phase 7 states what it does not give: no split-view defence beyond monitors, mirrors
   and anchors; no consistent cut across groups; no protection against an operator who controls the
   cluster, every anchor target, every mirror and the auditor's trust file (ledger-phase7.md 2.2,
   5.6).
6. **[Fact]** Control-plane dependencies are the standard library plus pgx and `x/*`
   (`kingres/control-plane/DEPENDENCIES.md`, policy: MIT, BSD and Apache-2.0 accepted); a consensus
   library would be the first large dependency.

### 1.2 Three meanings, three threats

architecture.md separates three things people mean by "a multi-node ledger":

| Meaning | Threat it answers | Parties |
|---|---|---|
| **M1. Agreement on one history of checkpoints** (no split view, no silent rollback) | One operator (or a compromised leader with the signing key) shows different histories to different verifiers, or restores an old state and signs it | One cluster, many observers |
| **M2. A shared notary among organisations**: several clusters of different organisations agree on every member's checkpoint history | Any one organisation rewriting its own history after the others relied on it | Several organisations, none trusts another's operator |
| **M3. A replicated ledger database**: nodes of several organisations execute the same transactions in the same order and keep the same ledger tables (IBM's blockchain relational database, Kwil) | Mutually distrusting parties writing to shared rows | Several organisations, shared data |

architecture.md's own statement of when consensus is needed ("only when several mutually
distrusting parties write shared data without a trusted operator", Wuest and Gervais) is about M3.
M1 and M2 do not need consensus on SQL execution. The owner's decision of 2026-11-28 is to build
"a multi-node ledger with consensus"; this design builds M1 with a quorum of witnesses and M2 with
consensus on checkpoints, and does **not** build M3 now (section 4).

## 2. Goals and non-goals

Goals:

1. A verifier can require that **k of n independent parties** have seen and agreed on a
   checkpoint, so that no single operator, including the cluster's own administrators, can
   present a history different from the one the others have seen.
2. Several organisations can run clusters (or single servers) that **agree on each member's
   checkpoint history** by Byzantine-fault-tolerant consensus, with membership and keys managed
   by the members.
3. No change to the chain format (phase 7, principle 1), no slowdown of the commit path
   (principle 2): consensus is over **closed checkpoints**, never in a transaction's commit.
4. Offline-first verification stays: a bundle verifies with the trust file alone.
5. One extension source for PG18 and PG19 (ADR 0023): this part adds no extension code; it is `kingresd`, the verifier and two new binaries.

Non-goals:

1. **Replicated SQL execution** (M3, order-then-execute). Section 4 gives the reasons and the
   conditions under which it would be reopened.
2. **Preventing** a change. The ledger still detects.
3. **Consensus inside a cluster for availability.** The chain is already replicated by physical
   replication and failover (ADR 0021); a second replication layer for the same data adds nothing
   (option R, section 3).
4. Public blockchains as a consensus engine (anchoring to them stays as in phase 7).
5. Confidentiality between members. A notary stores hashes, never rows.

## 3. The options

### 3.1 Raft (etcd/raft, hashicorp/raft)

Raft is a crash-fault-tolerant protocol: `2f+1` nodes tolerate `f` crashes, and it assumes nodes
that are not malicious ([Ongaro and Ousterhout, "In Search of an Understandable Consensus
Algorithm", 2014](https://raft.github.io/raft.pdf), background). Libraries: etcd's `etcd-io/raft`
(Go, Apache-2.0) and `hashicorp/raft` (Go, MPL-2.0) (background; licences to be checked).
**What it answers beyond phase 7:** availability of a single sequencer. **What it does not
answer:** a malicious node or operator, which is the threat. All members of one Raft group are
in the same trust domain; one operator controlling a majority controls the log. Inside one
cluster KingresDB already has leader election with leases and fencing in `kingresd` (ADR 0021).
Raft would be a second, independent mechanism for the same job. **Rejected** as the ledger
consensus. It remains the right tool if the project later needs a replicated sequencer inside one
trust domain (for a single global chain, an alternative named in architecture.md, "Interaction with
sharding"), which is not planned.

### 3.2 CometBFT (BFT, Tendermint family)

CometBFT is a Byzantine-fault-tolerant state machine replication engine: `3f+1` validators tolerate
`f` Byzantine ones; the application connects through ABCI (ABCI++ and 2.0 since v0.38)
([CometBFT repository](https://github.com/cometbft/cometbft); Apache-2.0; the application
requirements are quoted in architecture.md: the post-block state must be a function of the previous
state and the block, and if validators holding more than one third of the voting power disagree on
the app hash, consensus halts). **Threat model:** up to one third of the voting power may be
malicious or faulty and safety and liveness hold; above one third liveness can be lost; above two
thirds the history can be rewritten. Live PostgreSQL-adjacent users: Kwil (application chains next
to PostgreSQL) and Chromia (architecture.md, "Phase 3"). **Cost:** a large dependency tree (Go),
validator key management, upgrade coordination, and the cost of running a separate process.
**Chosen** for M2, with the application restricted to a deterministic state machine that does
not execute SQL (section 5).

### 3.3 A signed quorum of witnesses over the existing chains

The transparency-log ecosystem (the same tile format phase 7 uses) has a protocol for exactly this:
independent **witnesses** that keep the latest checkpoint of each log they follow, accept a new
checkpoint only with a valid consistency proof from the one they hold, and then cosign it. A
verifier requires cosignatures from k of n witnesses; this defends against split views (C2SP
`tlog-cosignature`: "a signature added to a checkpoint ... confirms it checked that the log's new
state is consistent with what it had seen before. Clients can require a quorum of these";
[spec](https://www.github.com/C2SP/C2SP/blob/main/tlog-cosignature.md); the witness HTTP API is
C2SP `tlog-witness`, and `transparency-dev/witness` implements it,
[pkg.go.dev](https://pkg.go.dev/github.com/transparency-dev/witness)). **Threat model:** the log
operator (and the cluster) may be wholly malicious. Each witness acts alone, so there is no
consensus protocol, no leader and no membership agreement, and **therefore quorums must intersect**:
with `n` witnesses and a threshold `k`, an operator can show fork A to one set of `k` witnesses and
fork B to another set of `k`; the two sets share at least `2k - n` witnesses, and an honest one of
them refuses the second fork. A split view therefore needs at least `2k - n` colluding witnesses.
The policy rule is **`2k - n > f`**, where `f >= 1` is the number of colluding witnesses the auditor
assumes; `k <= n/2` is **rejected** by the verifier (two disjoint quorums exist), and `k >= floor(2n/3) + 1`
is recommended. "Fewer than k collude" is not the condition. For liveness `k` witnesses must be
reachable. **Freshness is separate:** a validly cosigned old checkpoint stays valid for ever, so a
verifier that is shown only an old bundle cannot tell. A cosignature carries a timestamp
(C2SP `tlog-cosignature`); the trust file has `max_cosignature_age` and the verifier fails a
checkpoint whose newest cosignature is older (`--max-age` overrides), and `kingres-witness` answers
anyone's request for the latest checkpoint it holds so that an auditor or a monitor can compare
witnesses (a gossip by pull). An offline verification then proves "agreed by `k` witnesses as of the
cosignature time", and no more. **Chosen** for M1, as the first and cheapest increment, because it adds the
property phase 7 lacks (a third party that will notice, and prove, a split view or a rollback)
with the formats phase 7 already publishes.

### 3.4 Replicated SQL (M3): order-then-execute with BFT ordering

IBM Research's blockchain relational database modified PostgreSQL (about 4 000 lines of C) with
order-then-execute and execute-order-in-parallel, snapshot isolation by block height and a
block store; reported 1 800 to 2 700 TPS against about 800 TPS serial, and a BFT orderer that drops
from about 3 000 to 650 TPS as orderers grow ([arXiv 1903.01919](https://ar5iv.labs.arxiv.org/html/1903.01919),
quoted in architecture.md). Its determinism restrictions (no time, random or sequence functions,
an `ORDER BY` primary key on `LIMIT`, no `xmin` or `SELECT *`) and the sources of nondeterminism
architecture.md lists (collation, floating point, parallel query, VACUUM-dependent visibility,
extension versions) would apply to every statement of a replicated ledger table. It also conflicts
with the present design: a ledger table is local to one group and cannot be distributed (limitations.md);
the replicated rows would exist in several clusters, so ledger tables of several organisations
would have to be identical in every cluster, which no part of KingresDB supports; bucket moves and
the row router would need a deterministic mode. **Not built.** See section 4.

### 3.5 Summary

| Option | Threat model | Trust assumption | Adds beyond phase 7 | Cost | Decision |
|---|---|---|---|---|---|
| Phase 7 aggregate + anchors + monitors (exists) | Rewrite is detected if an anchor or monitor copy exists elsewhere | Auditor keeps trust file and a monitor | n/a | exists | keep |
| Witness quorum (C2SP cosignatures) | Malicious operator; split view; rollback | Quorums intersect (`2k - n > f`) and fewer than `2k - n` witnesses collude; freshness is checked separately | k-of-n proof that others saw the same history, checked offline | Small to medium | **build first (M1)** |
| Raft | Crash faults only | Same trust domain | Availability of a sequencer (already have failover) | Medium | reject |
| CometBFT notary of checkpoints | Up to f of 3f+1 Byzantine members | Fewer than a third of voting power malicious | Agreement among organisations on each other's histories; one global order of checkpoints; members cannot equivocate | Large | **build second (M2)** |
| Replicated SQL (M3) | Mutually distrusting writers of shared rows | Same as BFT | Shared data | Very large; determinism contract | not built; open question 1 |

## 4. Why not M3 now

1. The cost is the one architecture.md and ADR 0013 already gave (determinism contract, no
   maintained public implementation, throughput drops with orderers). Nothing in the owner's
   decision names a consortium of writers.
2. M3 is a different product: a deterministic SQL subset, all nodes on identical binaries and
   settings and a pinned collation. The notary of M2 gives such a consortium its first
   deliverable (agreement on who published which checkpoint) without a deterministic SQL contract.
3. M3 can be added on top of M2: the notary's membership, keys and ABCI application are the base
   on which an execution layer would be built. The design below keeps the application's state
   machine open for that (section 5.2, "application version").

**Reopen M3 when:** the owner names a consortium use case, with the parties, the shared tables and the
SQL they need. Then a new design and ADR decide the deterministic subset.

## 5. The design

### 5.1 Layer 1: witnesses (M1)

**Roles.**

- The cluster's leader (exists, `ledgerround`/`ledgerpub`) publishes the checkpoints of the chain
  logs and of the aggregate log, signed with the server key (phase 7, 3).
- A **witness** is a service that follows the aggregate log (and optionally every chain log) of
  one or more clusters. It is a small program, `kingres-witness` (Go, standard library plus the
  Ed25519 of the standard library), packaged separately and runnable by anyone: an independent
  organisation, the auditor, or another KingresDB cluster. It implements the `tlog-witness` API
  (add-checkpoint with the old size and a consistency proof) and signs with the `cosignature/v1`
  format of C2SP `tlog-cosignature`. **[Needs verification]** the exact current text of
  `tlog-witness` and `tlog-cosignature` (the search of 2026-11-28 found the cosignature page and the
  Go witness's description of the API, not the `tlog-witness` page itself) and whether the
  cosignature timestamp field matches how phase 7's notes are laid out.

**Protocol.**

1. After an aggregation round signs a new aggregate checkpoint, the publisher sends it to each
   configured witness with the consistency proof from the size that witness last cosigned (the
   witness tells the publisher its size: the API replies with its current size when asked).
2. The witness checks the origin line and the server signature against the public key pinned in its
   own configuration, checks the consistency proof against its stored `(size, root)`, stores the
   new pair and returns a cosignature. A checkpoint that is not an extension of the stored one is
   refused with the stored size, and the witness records the refusal: the operator or the cluster
   attempted a rollback or a fork. This is the detection.
3. The publisher stores the cosignatures with the checkpoint (new files in the publication layout,
   section 4.6 of phase 7; the format is the note format with extra signature lines, which the
   verifier already parses) and `kingresd` shows them on the Ledger page.
4. The verifier's trust file gains a **witness policy**: the witnesses' names and public keys and a
   threshold `k`. `kingres-verify` accepts a checkpoint as "witnessed" when it carries valid
   cosignatures from at least `k` of the listed witnesses. The anchors of phase 7 are unchanged and
   independent.

**Failure handling.** A witness that is unreachable delays only that cosignature (the publisher
retries with backoff; the checkpoint is published without it and shown "k-1 of n"). A witness that
refuses a checkpoint raises an alert `ledgerWitnessRefused` (critical): the operator's own
cluster is shown a state it cannot extend, which is exactly the signal of a restored old backup
(phase 7, 9.4 describes the operator procedure after a restore; the witness now makes it
visible to third parties until the new epoch is accepted: a new epoch is a new log origin, so
witnesses and verifiers start it as a new log and record the old one as closed by an event).
A restored system database that forks the aggregate log (limitations.md, "The ledger") is thus
detected, where today only anchors and monitors would notice.

**Epochs (H7).** "A new epoch is a new log origin" would be a hole: an operator could rewrite
everything and publish under a fresh origin. The rule is: a new epoch is accepted by a witness for a
known cluster key **only** together with a **closing statement of the old log**, a note signed by the
cluster key that names the old origin and the `(size, root)` that the witness itself holds (the
largest it cosigned), and the new log's first checkpoint embeds that `(size, root)` and a count of
epochs. The tail between the restored state and the witnessed size is declared lost in the statement
(the statement is how the loss becomes visible). A witness refuses a new origin without it, rate-limits
epoch changes, and publishes each as an event. **One successor per closed log (N8):** the witness
records "closed, successor = X" for the old origin and refuses any other successor, and the first
checkpoint of the new log must name the closing statement; otherwise an operator could close O1 and
open O2 at witnesses {1,2} and O2' at {3,4}, a split view across epochs that the `2k - n` rule does
not cover (each epoch is a different log); the console and `kingres-verify` show "epoch changed
N times, last on ...". The old epoch stays verifiable against its cosigned head, so proofs under the old
epoch cannot be replaced by anything the new epoch says. This **makes an epoch change visible and
countable; it does not prevent one**: an operator can still start a new epoch and lose history, and
the auditor sees it.

**Age and archived bundles (N9).** `max_cosignature_age` applies to verification of the **current
state** (the question "is this the latest agreed state"). An archived bundle is verified **"as of
the time of the bundle"**: `kingres-verify --as-of <time>` (or the bundle's own recorded time, when
it carries one that a witness cosigned) checks the age against that time, and the output says
"verified as of ...; not checked for freshness". Offline-first does not turn into a time bomb: a bundle
kept for a year still verifies as an archive.

**Policy and downgrade (H7).** When the trust file lists a witness policy, a checkpoint with
fewer than `k` valid cosignatures, with none (stripped), or older than `max_cosignature_age`, is a
**failure** of `kingres-verify`, exactly as a bad signature is: the policy is the auditor's own file. Only
when the trust file has **no** witness policy does the verifier warn that witnesses are not checked.
The flag `--require-witnesses` is not needed for this; it remains as a way to fail a trust file that has
no policy.

**What it adds, precisely.** If an attacker with the cluster and its signing key rewrites history
and publishes it, a verifier that requires k cosignatures rejects the rewritten checkpoints unless
the attacker also controls k witnesses, and at least one honest witness holds proof of the fork.
It does not prove the data is right, does not prevent an attacker from signing and publishing a
different history to a verifier who has no witness policy, and does not defend against the
witnesses themselves colluding with the operator.

**Operational shape.** Witnesses run outside the cluster, in other administrative domains; the
console's Ledger page has a "Witnesses" tab (name, public key, last cosigned size, lag, last error)
and the trust file download includes the witness policy. A cluster can also run a witness for
another cluster (two clusters of two organisations witness each other).

### 5.2 Layer 2: the notary (M2) with CometBFT

**Idea.** A network of validators, each run by a member organisation, agrees on a single ordered
log of **checkpoint submissions** from the members' clusters. The state machine verifies and
records; it executes no SQL and no member data.

**Components.**

- `kingres-notary`: a Go binary (package `kingres-notary`, separate from `kingresd`) that embeds the
  CometBFT node and the ABCI application. Reason for a separate binary: the dependency tree and
  the upgrade cadence of CometBFT must not enter `kingresd`, whose dependency policy is the standard
  library first (DEPENDENCIES.md); an exception would need its own ADR.
  **[Needs verification]** whether to embed CometBFT as a library or run it as a child process with
  ABCI over a socket; the second keeps the licence and the dependency out of the application's
  address space and is the default recommendation, and the first is simpler to package.
- A **submitter** in `kingresd` (the manager leader, as the anchoring agent is): after each
  signed aggregate checkpoint it sends a submission to its member's notary node.
- The verifier gains a notary verification: a **notary proof** is the inclusion of the submission in
  a notary block plus the block's commit signatures from validators holding more than two thirds of
  the voting power under the validator set pinned in the trust file (the light client check of
  CometBFT/Tendermint, **[Needs verification]** whether to implement it from the specification in
  the verifier with the standard library only, as the verifier's policy requires, or to define
  the proof format simpler: the application hash chain plus a list of validator signatures over it).

**The application (deterministic state machine).**

State: the validator set and the member registry (member id, public key of the member's cluster
signing key, the origin prefix of its logs); for each member and log: `(size, root, last_height)`.
Transaction kinds:

1. `submit(member, log_origin, checkpoint_note, consistency_proof)`: valid only if the note's
   signature verifies under the member's registered key, `size` is greater than the stored size
   (or equal with the same root: idempotent), and the consistency proof from the stored `(size, root)`
   verifies (the same RFC 9162 verification the verifier and `kingres_ledger` already have). On success the
   stored pair is replaced. On failure the transaction is rejected in `CheckTx` and `FinalizeBlock`
   with no state change. **Evidence of equivocation is a separate, successful transaction**
   `report_equivocation(note1, note2)` (two notes signed by the member's key for the same size and
   different roots): when it verifies, the member is marked equivocating in the state and both notes
   are stored. Any member can send it, including after a rejected `submit` (a rejected transaction
   leaves no record by itself).
2. `register`, `rotate_key`, `retire`: membership and key changes of members, authorised by the
   members' own keys and by a vote rule (a member change needs signatures from a majority of
   members, a validator-set change needs the CometBFT validator update rules; **[Needs verification]**
   exact rule; the default recommendation is "a change is accepted when signed by more than two
   thirds of the registered members' keys").
3. `set_params`: protocol version.

The app hash is the Merkle root of the state (members and their latest checkpoints); a block
contains submissions only. No clock reads (block time of CometBFT is the BFT time, used only for
display), no randomness, no map iteration order (the state is a sorted structure): the
determinism requirement of CometBFT's ABCI application rules is met by construction because the
transition function is a pure verification of hashes and signatures. **Application version**: the
state carries a version so that M3 or a new transaction kind can be added by a coordinated upgrade
(ABCI's handshake and CometBFT's `app_version`; **[Needs verification]**).

**Who runs validators.** The members. A member that is a single KingresDB cluster runs one validator
(`kingres-notary`) on a server of that cluster, which `kingresd` can start, stop and monitor as
it does `postgres` (the CLI and console show its state). A cluster of one organisation alone does
not need a notary and uses only layer 1.

**Guarantees.** With `n >= 4` validators and fewer than a third of the voting power malicious or
faulty: every member's published checkpoint history is a single history that every correct
validator agrees on; a member cannot present two histories (equivocation is recorded with both
signed notes as evidence); a restored old state cannot be submitted; the order of the
members' checkpoints relative to one another is the notary's block order (not real time).
With a third or more faulty the notary can halt (no new checkpoints are recorded; clusters keep
working, the ledger keeps building, the aggregate waits and the alert `ledgerNotaryStalled` shows); with more than two
thirds malicious, history can be rewritten. A cluster in a notary with `n = 1` or `2` has
no fault tolerance and the console says so.

**What the notary does not do:** order or validate the members' SQL transactions; hide data (it holds
roots, sizes and public keys); make a member's ledger correct; decide membership by itself.

### 5.3 The consistent cut: dropped

The first draft let the aggregate round read all heads at one timestamp of a global snapshot. That needs
a global order of commits, which the planned atomic visibility does not give (the owner declined the
core patches it would need, [ADR 0032](../adr/0032-phase-8-makes-no-core-patch.md)). **The aggregate
keeps the statement of phase 7 (5.6): the heads are read one group after another.** The leaf format
stays 1; there is no `head_at`, no `kingres_ledger` 0.4 for this, and no format 2 leaf. If causal
order is ever built (the upstream proposal, atomic-visibility design section 15), the cut can be
reopened by a new ADR.

### 5.4 Interaction with the rest

- **Anchoring** (phase 7) is unchanged and stays independent: witnesses and the notary add
  signatures to the same checkpoints; a verifier policy can require any combination (an anchor, k
  witnesses, a notary proof).
- **Failover of the manager leader:** the submitter and the witness client are duties of the leader
  like the anchoring agent: they pick up after a leader change from stored state
  (`ledger_log` and the cosignature table), idempotently (a submission and a witness request that are
  repeated give the same answer).
- **Restore of the system database** (ADR 0025) forks the aggregate log; the witnesses and the
  notary refuse the forked checkpoint, and the operator procedure of phase 7, 9.4 starts a new
  epoch, which is a new origin for them.
- **Ledger tables carried by import** (ADR 0028) start a new chain and appear in the aggregate as a
  new member: nothing special.
- **Keys:** the server signing key is the one of phase 7; the notary uses a separate validator key
  per validator (CometBFT's `priv_validator_key.json`), kept outside PostgreSQL in the files of
  `kingres-notary` (phase 7, principle 4), backed up as phase 7 9.2 describes, **except `priv_validator_state.json`, which must never be restored from a backup** (an old state makes the validator double-sign, the same rule as for a witness). Witness keys belong
  to the witness.

## 6. Guarantees, in precise words

| Statement | Phase 7 (exists) | + witnesses (layer 1) | + notary (layer 2) |
|---|---|---|---|
| A rewritten history is detectable by a third party | Yes, if an anchor or a monitor copy exists elsewhere | Yes, and offline, if k witnesses saw the older checkpoint | Yes, by every member, and recorded as equivocation |
| A split view (two histories to two verifiers) is detectable | Only by a monitor that saw both | Yes, if `k > n/2 + f` and the verifier checks freshness: two forks each need `k` cosignatures, and the quorums share `2k - n` witnesses, an honest one of which refuses the second | Yes: one history by consensus |
| Collusion needed to rewrite undetected | The operator, every anchor target, every mirror and the trust file | The operator and at least `2k - n` witnesses (to hide a fork), or `k` (to forge one history) | The operator and a third (to halt) or two thirds (to rewrite) of the voting power |
| Agreement among organisations on each other's checkpoints | No | No (each witness is independent) | Yes |
| Total order of checkpoints of different members | No | No | Yes (block order; not real time) |
| Consistent cut across shard groups | No | No | No (dropped, 5.3) |
| Data correct, authorised writes, prevention | No | No | No |
| Availability: ledger keeps working if witnesses or the notary are down | n/a | Yes (cosigning is delayed) | Yes (submissions are queued) |

## 7. Failure handling

| Event | Behaviour |
|---|---|
| Witness unreachable | Retry with backoff; checkpoint shown with fewer cosignatures; alert after the publication interval; the verifier's policy decides |
| Witness refuses (stored size larger, or not a consistent extension) | Critical alert; the publisher stops extending the log it cannot extend (as for divergence, phase 7 4.7) and the operator uses the restore procedure; the refusal is kept as evidence |
| Two witnesses disagree | Each is independent; the verifier needs k, so a lie by one is not enough; the console shows the divergent sizes |
| Notary below quorum (fewer than 2n/3 reachable) | `ledgerNotaryStalled`; submissions wait in `kingresd`'s queue (bounded, oldest first); the aggregate, anchors and witnesses continue |
| A member submits a checkpoint that does not extend its stored one | Rejected and recorded; the member's cluster is told through the submitter's alert |
| A member's cluster lost (all servers) | Its stored checkpoint stays in the notary; a replacement cluster restored from a backup older than the stored checkpoint is refused until a new epoch is registered by the member's key and countersigned by the members' rule |
| Validator key compromise | `rotate_key` by the member's rule; the compromised key is removed from the validator set; history before stays valid (as phase 7 key revocation) |
| Leader failover during a submission | Idempotent repeat |
| Clock skew | Not used by the notary or witnesses for correctness; the cosignature timestamp is informational |

## 8. Upgrade and compatibility

- Layer 1 changes no extension, no chain, no leaf format. A cluster of 0.1.0 with phase 7
  publication gets witnesses by upgrading `kingresd` (new publication files, new REST) and the
  verifier (witness policy in `trust.json`: **[Needs verification]** whether the trust file gets a
  new optional field with version 1 staying valid for old verifiers; the recommended rule is
  an optional field that an old verifier ignores with a warning that it cannot check witnesses).
- Layer 2 adds `kingres-notary` as a new package or a part of the main package (open question 3)
  and nothing to the extension. A cluster that does not use it is untouched.
- One source for PG18 and PG19: `kingres_ledger` 0.4 change goes through its `pg_compat.h`;
  `kingres-notary`, the witness and the verifier do not depend on the major.

## 9. Interfaces

| REST operation (new, one contract revision per increment, number taken at implementation time) | `kingres-cli` command |
|---|---|
| `listLedgerWitnesses`, `addLedgerWitness`, `removeLedgerWitness`, `getLedgerWitnessStatus` | `kingres-cli ledger witness list/add/remove/status` |
| Digest and aggregate views gain `witnessCosignatures[]`, `cutTimestamp`, `notaryProof` (null when absent) | `kingres-cli ledger digest --verbose` |
| `getLedgerNotary` (members, validators, height, state), `setLedgerNotary`, `submitLedgerNotaryMember` | `kingres-cli ledger notary status/join/leave` |
| Trust file download includes the witness and notary policy | `kingres-cli ledger trust-file` |
| Alerts `ledgerWitnessRefused`, `ledgerWitnessLag`, `ledgerNotaryStalled`, `ledgerNotaryEquivocation` | `kingres-cli alerts` |

Console: tabs "Witnesses" and "Notary" on the Ledger page, the new statements in the "what this proves" text. `kingres-verify`: `verify` accepts a witness and
notary policy; `monitor` can run as a witness-like observer; a new `notary` subcommand checks a notary
proof. Rule since 2026-11-28: the CLI command comes with the REST change.

## 10. Security review points

1. **A witness is a trust root** of the verifier: its key is pinned in the trust file; a stolen
   witness key lets an attacker forge one of k signatures. Policy: choose independent
   operators; k above n/2 and recommended at least floor(2n/3) + 1 (the verifier rejects a policy with k <= n/2); the key rotation event of phase 7, 3.3, applies.
2. **The witness API is a network service** that parses a checkpoint and a proof: input size
   limits (the phase 7 limits for proofs), no state change before verification, constant-time
   signature checks (standard library), rate limits per cluster key; it never writes anything but
   its own `(size, root)` per log.
3. **Replay and rollback of the witness's own storage:** a witness that is restored from an old backup
   forgets a larger checkpoint it cosigned and could cosign a fork. The witness stores its state with
   an fsync before it answers, and documents that it must not be restored from backup (it can be
   replaced by a new witness with a new key).
4. **Notary:** validator keys in files, permissions as phase 7 secrets; the p2p port is a new
   listening port (documented in the package, off by default, TLS from CometBFT's secret
   connection, plus the cluster's firewall guidance); the RPC port of CometBFT bound to localhost only.
   A member that is a validator can see all submissions, which are hashes only.
5. **Equivocation evidence** is two signed notes; both are kept and published (not a secret).
6. **Dependency review:** CometBFT's Go module tree enters `kingres-notary` only; licence check of
   every module (Apache-2.0 and MIT accepted; DEPENDENCIES.md); a pinned version, digest recorded,
   SBOM entry (the extension packaging line's rules for third-party sources apply).
7. **Denial of service by a member:** a member that floods submissions is limited by a per member rate
   in `CheckTx` and by size limits on notes and proofs.
8. **Time:** nothing depends on wall time; BFT time is display only.
9. **A compromised leader** can still refuse to send (no submission), which the alerts show, and can
   sign a wrong aggregate; witnesses and the notary record it. It cannot make an honest witness or
   validator accept a fork.

## 11. Test plan

1. **Unit and vectors:** consistency proof verification shared with the verifier; cosignature
   encoding with the vectors from the C2SP specification where the spec has them (**[Needs
   verification]**) and cross-checks against `transparency-dev/witness`; notary state machine
   transitions (accept, idempotent, reject, equivocation); determinism (the same blocks give the
   same app hash on 5 runs, with shuffled input maps).
2. **Witness protocol tests:** a fake publisher that tries to fork (two different checkpoints of one
   size, a rollback, an inconsistent proof): every honest witness refuses; k of n honest witnesses
   needed; witness crash and restart keeps its state; a witness restored from an old state (negative
   test of the documented hazard).
3. **Notary tests:** four validators in a test network (processes in the cluster tests) with
   one crashed, one partitioned, one Byzantine (signs two checkpoints for a member; sends invalid
   transactions; double-votes): safety (no two histories) and liveness (with 3f+1) hold; with f+1
   faulty the network halts and the alert fires; the member change rule.
4. **End to end:** cluster tests: a three-group cluster with aggregation, two witnesses and a
   four-validator notary; restore of the system database (fork) is refused; leader failover at
   every step of a round; clock-step, node-kill and network-cut nemeses of the shared harness (the
   harness of the atomic visibility part is reused).
5. **Verifier tests:** a bundle with k, k-1 and zero cosignatures under a policy; with a wrong key; with
   a notary proof of a validator set that is not the pinned one; with a forged proof (mutants: break
   the quorum count, the signature check, the set pin, and see the tests fail).
6. **Performance:** consensus is off the commit path; measure the delay from a closed aggregate to
   the notary block (target: a few seconds, **[Needs verification]** by measurement) and that commit
   throughput of a ledger workload is unchanged with the layers on.
7. **Upgrade:** a cluster with phase 7 publication enables witnesses without a restart of the data
   path; an old verifier on a bundle with witness files ignores them and says so.

## 12. Increments and order

Sizes are relative (S, M, L).

| # | Increment | Size | Depends on | Touches |
|---|---|---|---|---|
| L-1 | Witness protocol library: checkpoint note cosignature encode and verify, consistency proof check shared with the verifier | S | none | `kingres/verifier`, a shared Go package |
| L-2 | `kingres-witness` service (binary, packaging, tests, README) | M | L-1 | new directory |
| L-3 | Publisher side in `kingresd`: witness list, send after an aggregate checkpoint, cosignature store (migration), alerts, REST, console tab, `kingres-cli` | M | L-1; the CLI line | `kingres/control-plane` (`ledgerpub`, `ledgerround`), contract, console |
| L-4 | Verifier: witness policy in the trust file, `verify` with a threshold, `monitor` as observer, bundle layout | S | L-1 | `kingres/verifier` |
| L-5 | Notary application (state machine, proof format, tests with a fake consensus engine) | M | L-1 | new directory |
| L-6 | `kingres-notary` with CometBFT, packaging, key handling, `kingresd` supervision and submitter, REST, console, CLI | L | L-5, L-3 | new directory, `kingresd` |
| L-7 | Notary proof in the verifier | M | L-5 | `kingres/verifier` |
| L-9 | Network tests, faulty-member and partition tests, reviews, documentation (operator guide chapter, limitations) | L | L-6, L-7 | tests, docs |

Order: L-1 to L-4 (witnesses) can start now; none touches `kingres_shard` or needs the atomic
visibility part. L-5 starts in parallel. L-6 and L-7 follow. **All of L-1 to L-7 and L-9 ship in the
same release as atomic visibility** (the owner's decision of 2026-11-28); L-8 is dropped (5.3). Every
increment passes the harness and an independent review before it merges (atomic visibility design 9.1).
All of it can run beside work in `kingres_shard` (none of it touches that extension).

## 13. Open questions only the owner can decide

1. **Is M3 (replicated SQL tables among organisations) wanted?** Default: no; reopen with a named use
   case (section 4).
2. **Answered on 2026-11-28:** no outside organisation yet; the project runs its own test witnesses
   (two or more, on separate hosts) and test validators (four or more) for the tests and the release.
3. **Packaging of `kingres-notary` and `kingres-witness`:** inside the main package, or separate
   packages. Default: `kingres-witness` separate (it is meant to run elsewhere), `kingres-notary`
   in its own package, both versioned with the release.
4. **Validator model:** one validator per member organisation (default), or one per server.
5. **Dependency exception:** accept CometBFT as a pinned third-party dependency of `kingres-notary`
   (Apache-2.0, large). Default: yes, limited to that binary, with an ADR (0034).
6. **Name of the policy in the trust file.** Decided by the review (H7): a trust file with a witness policy makes a checkpoint below the threshold, stripped of cosignatures, or older than `max_cosignature_age` a failure; a warning only when there is no policy. No open decision remains here except the field names.

## 14. Sources

Fetched or searched on 2026-11-28: the C2SP tlog-cosignature specification (via search results),
the `transparency-dev/witness` package description, CometBFT's repository page and release notes
(ABCI 2.0, Apache-2.0). Quoted in architecture.md and not re-fetched: IBM Research's blockchain
relational database (arXiv 1903.01919), Wuest and Gervais (ePrint 2017/375), CometBFT's ABCI
application requirements. Background, to check before the ADRs are accepted: the Raft paper and the
licences of `etcd-io/raft` and `hashicorp/raft`; the exact text of C2SP `tlog-witness`; the CometBFT
light client specification and `app_version` handling.

## 15. The notary after the review (M8)

1. **Validator state.** The validator's signing state file must not be restored from a backup (an old
   state makes the validator double-sign, which the notary itself would record as equivocation by that
   validator). The backup rule for the key does not cover the state file.
2. **Validator-set updates and long-range attacks.** A verifier that pins a validator set must verify
   validator-set changes the way a light client does, with a **trust period**: a pinned set older than
   the period is not trusted until the auditor refreshes it from another channel. Old keys that are
   no longer validators could otherwise sign a rewritten history later (weak subjectivity). The trust
   file has `notary_trust_period`.
3. **Evidence** is a successful `report_equivocation` transaction (5.2), not a rejected one.
4. **Freshness** of a notary proof is the same problem as for cosignatures (3.3): a proof shows that a
   checkpoint was recorded at a block height, not that it is the latest. The verifier takes the
   newest block it can reach (online) or fails a proof older than `max_notary_age` from the trust file.
5. **Value, stated plainly.** Over witnesses the notary adds a **total order** of the members'
   checkpoints and a **single recorded equivocation**; agreement among organisations on each other's
   histories can also be had by cross-witnessing. The owner decided on 2026-11-28 to build it in the
   same release anyway, with the project's own test validators; a deployment can use layer 1 alone.

## 16. Open questions, revised

The questions of section 13 stand with these changes: 2. is answered (no outside organisation; own
test witnesses and validators); 5. the CometBFT decision is taken now: a pinned dependency of
`kingres-notary` only, run as a child process over the ABCI socket unless the spike of L-6 shows the
library is simpler to package; 6. is decided (above). The consistent cut is dropped (5.3).

## 17. Review of 2026-11-28 and where each finding is answered

Findings H6 to H7, M8 and L4 of the independent review concern this document; the others are in
[phase8-timestamp-snapshot-rejected.md](phase8-timestamp-snapshot-rejected.md) section 16 (the rejected design; its findings do not apply to the planned one).

| Finding | Answer | Where |
|---|---|---|
| H6 quorum intersection, freshness | Rule `2k - n > f`, `k <= n/2` rejected, `k >= floor(2n/3) + 1` recommended; `max_cosignature_age`, a pull-based "latest checkpoint" answer for monitors; table rows corrected | 3.3, 6 |
| H7 epochs and stripped cosignatures | Epoch change needs a closing statement at the witness's stored head and is visible and counted, not prevented; a policy in the trust file makes below-threshold a failure; warning only without a policy | 5.1 |
| M8 (a) validator state backup | Never restore the state file | 5.4 (keys), 15 |
| M8 (b) validator-set updates, long range | Light-client verification with a trust period | 15 |
| M8 (c) equivocation record vs rejection | `report_equivocation` is its own successful transaction | 5.2 |
| M8 (d) notary proof freshness | `max_notary_age`, newest block | 15 |
| M8 (e) value over witnesses | Stated; layer 2 deferred until a counterparty | 15, 16 |
| L4 block builder under a rewritten snapshot; format 2 | Void: the consistent cut is dropped, no format 2 leaf | 5.3 |
| Open question 6, 2, 5 | Revised | 16 |
| N8 epoch successor uniqueness (second review) | A witness records "closed, successor = X" and refuses any other successor | 5.1 |
| N9 `max_cosignature_age` vs archived bundles (second review) | Age applies to current-state checks; archives verify "as of" a time | 5.1 |

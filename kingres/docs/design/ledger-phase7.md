<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# kingres_ledger, kingresd and the verifier: design of phase 7 (signing, anchoring, tile publication, the standalone verifier, the per-shard ledger with an aggregate digest)

Status: Proposed design, 2026-11-15; not implemented. Nothing in this document is built. Where it
says "exists", it describes the committed state of the repository and says which file shows it.

Builds on [ledger-mvp.md](ledger-mvp.md) (`kingres_ledger` 0.1), [ledger-0.2.md](ledger-0.2.md)
(0.2), [control-plane-3.md](control-plane-3.md) and [ha-phase5.md](ha-phase5.md) (task engine,
leases, the manager leader), [config-phase13.md](config-phase13.md) (the conventions for a console
feature with a contract revision), [ADR 0013](../adr/0013-ledger-tables-and-anchoring-first.md)
(ledger first, anchoring, consensus only with a use case), [ADR 0023](../adr/0023-one-extension-source-for-several-majors.md)
(one extension source for PostgreSQL 18 and 19) and the roadmap in
[architecture.md](../architecture.md): "Phase 7: client/server signing, anchoring (WORM + OTS/TSA),
tile publication and a standalone verifier, a per-shard ledger with an aggregate digest". Phase 7 is
part of the first release. Phase 8 (a global snapshot and a multi-node ledger) is not.

The words of the design, once, so that they are used the same way below:

- **Chain**: the hash chain of blocks of one database on one shard group (`kingres.ledger_blocks`),
  identified by its `chain_id`. Format version 1, unchanged by this phase.
- **Log**: a Merkle tree over a sequence of leaves, published as tiles. Phase 7 adds two kinds: the
  **chain log** (one leaf per block of one chain) and the **aggregate log** (one leaf per aggregation
  round of one database).
- **Checkpoint**: a signed note (C2SP `signed-note`) that states the size and root of one log.
- **Aggregate digest**: the checkpoint of the aggregate log of a database; it commits to the head of
  every chain of the database at the rounds it records.
- **Anchor**: a copy or a timestamp of an aggregate checkpoint that lives outside the cluster's control.
- **Bundle**: the files a third party needs to verify without the database, in the layout of the
  publication (section 4.6).

## Sources read

| Source | What it settled |
|---|---|
| `kingres/docs/architecture.md`, "Blockchain: tamper-evident ledger tables first", "Phased roadmap", "Scope of the first release" | The phase 7 sentence; the recommendations for signing (ed25519, key id in the header, HSM/KMS), anchoring (at least two independent destinations, a witness copy), the aggregate design (a Merkle tree over the shard heads, "a design proposal, not a proven solution"), and that phase 8 is future roadmap |
| `kingres/docs/adr/0013`, `0018`, `0021`, `0022`, `0023`, `0024` | Ledger detects, does not prevent; the manager leader is the server of the map group primary; failover is in `kingresd`; one extension source for two majors; AGPL header on new files |
| `kingres/docs/design/ledger-mvp.md`, `ledger-0.2.md` | The data model, the hash format version 1, protections, workers, records, what 0.2 already said phase 7 would add |
| `contrib/kingres_ledger/README.md`, `kingres_ledger--0.2.sql`, `src/*.c` (`merkle.c`, `proof.c`, `capture.c`, `protect.c`, `worker.c`, `pg_compat.h`) | Exact formats (tags `0x10` to `0x14`, the 87-byte block hash input, JSON of digests and proofs), the catalog, the status view, what the protections cannot stop, the dump and restore limit, the proof that grows linearly with the chain |
| `kingres/verifier/README.md`, `internal/{merkle,format,chain,proof,rowfile,cli}` | What `kingres-verify` verifies today and from which input (digest, proof, block header export, digest sets); standard library only; exit statuses 0, 1, 2 |
| `kingres/api/openapi.yaml` at 0.8.0 (committed), `kingres/control-plane/README.md`, `internal/ops/ledger.go`, `internal/api/requests.go` | The ledger operations, `LedgerDigest` with `aggregateHash`, `anchors`, `complete`, `uncoveredShardGroupIds`; digests of one group only; `signed` always false; no scheduler for `digestIntervalSeconds` |
| `kingres/web/src/pages/LedgerPage.vue`, `lib/ledger-coverage.ts`, `components/ledger/*`, `pages/DashboardPage.vue` | The Ledger page (chains, digests with "Aggregate hash" and "Anchors" columns, verification, proofs); the Dashboard has no ledger card |
| `kingres/docs/design/ha-phase5.md`, `control-plane-3.md`, `kingres/control-plane/README.md` ("Task engine", "Leases and fencing", "What has to move with the manager"), `internal-api.md` | Tasks are resumable and at-least-once; one leader; the CA key is a file `pki/ca.key` that has to move with the leader (not yet built); the internal API is a closed set of calls; a compromised leader is a compromised cluster |
| `kingres/docs/design/shard-phase10.md` 3.5, `shard-phase11.md` 11.1, `shard-phase2b.md` 11.8, `contrib/kingres_shard/README.md` (committed) | A ledger table cannot be a distributed or reference table; in a distributed schema it is pinned to its owner group; `move_bucket` moves partitions with logical replication |
| `kingres/docs/reviews/2026-11-14-kingres-ledger-0.1.md`, `-0.2.md` | What the reviewers attacked; the protections are not a security boundary |
| External specifications, fetched on 2026-11-15 (URLs in the sections that use them) | C2SP `tlog-tiles`, `tlog-checkpoint`, `signed-note`; RFC 3161, RFC 9162; S3 Object Lock and PutObject; OpenTimestamps client and calendar behaviour from its source |

## 1. What exists, what exists in part, what is missing

### 1.1 Exists (not to be redesigned)

| Piece | Where | State |
|---|---|---|
| Hash format version 1: RFC 6962 trees (`leaf_hash = SHA256(0x00 || data)`, `node_hash = SHA256(0x01 || l || r)`), entry, transaction and block leaves, block hash, chain identity | `contrib/kingres_ledger/src/merkle.c`, `row_hash.c`, `kingres_ledger--0.2.sql` (`ledger_leaf_hash`, `ledger_node_hash`, `ledger_block_hash`, `ledger_genesis_hash`) | Done. This phase does not change it; digests and proofs made by 0.1 and 0.2 stay valid |
| Chain per database and shard group in the sense that each group's primary has its own `kingres.ledger_*` tables and its own random `chain_id` | `ledger_chain` | Done by construction (the extension is installed per group); physical standbys share the chain of their primary |
| Digest, inclusion proof (I and D), chain-walk verification | `ledger_generate_digest`, `ledger_inclusion_proof`, `ledger_verify_proof`, `ledger_verify_digest` | Done. A proof carries every header between its block and the digest: size linear in the distance |
| Recorded digests and verifications, status view | `ledger_digest`, `ledger_verification`, `ledger_status` | Done; not a root of trust (README) |
| Standalone verifier | `kingres/verifier` | Done for: one digest, one proof (`proof`), a header export (`blocks`), a set of digests (`digest-chain`), `row-hash`. Standard library only, 0 / 1 / 2 exit statuses, fixtures cross-checked against a real server |
| Contract placeholders | `openapi.yaml` 0.8.0: `LedgerDigest.aggregateHash` (always null), `LedgerDigest.anchors` (always `[]`), `DigestAnchor.target` enum `worm, openTimestamps, tsa, ethereum`, `LedgerBlock.signed` (always false), `complete` and `uncoveredShardGroupIds` on digest, status and verification | Shapes exist, nothing fills them |
| Ledger operations of `kingresd` for one chain | `internal/ops/ledger.go`, `ledger_proof.go`, `ledger_verifications.go`; `kingres_cp.ledger_digest_actor`, `kingres_cp.ledger_verification` | Done for the chain of the group whose instance the server runs; `kingres/control-plane/README.md` "Limits that remain": digests and verifications cover only the chain of the server that makes them |
| Console | `LedgerPage.vue` (chains per group with coverage text, digests with the two placeholder columns, verifications, proofs); `EnableLedgerDialog.vue` | Done for what the contract has |
| Ledger tables stay on one group | `create_distributed_table` refuses a table with triggers (`contrib/kingres_shard/README.md`, "without inheritance, foreign keys, triggers, ..."), and `shard-phase2b.md` 11.8, `shard-phase11.md` 11.1 say so | Done by effect; the refusal has no ledger-specific message |

### 1.2 Exists in part

| Piece | What exists | What is missing |
|---|---|---|
| Scheduled digests | `Database.ledger.digestIntervalSeconds` is stored (`ops/databases.go`, `ledger_digest_interval`) and the digest list labels a digest of the service role `system` | Nothing runs it: `kingresd` has no recurring scheduler (architecture.md, "Phases 9 to 12": "which has no recurring scheduler yet") |
| Cluster-wide ledger answers | `complete`, `uncoveredShardGroupIds`, `chains[]` in the schema | The answer holds one chain; the other groups' chains need connections the control plane does not make (`ops/ledger.go` `otherGroups`) |
| Proof against a later digest | The verifier walks headers | No O(log n) path; no consistency proof between two digests except by listing all headers |
| Anchor state in the console | A column and a `DigestAnchor` schema | No target, no status source, no controls |

### 1.3 Missing (the content of this design)

1. Signing: no server key, no checkpoint, no client key, no signature anywhere.
2. Anchoring: no target of any kind, no credential storage, no status.
3. Tile publication: no log over the blocks, no tiles, no checkpoint, no HTTP endpoint.
4. The aggregate: no structure, no cluster-wide read, no scheduler to build it.
5. The verifier: no signature, no checkpoint, no consistency proof, no anchor, no aggregate, no bundle, no exit status for "could not check".
6. Interfaces: the SQL read functions `kingresd` needs for publication, the client-key catalog, the REST operations, the console pages, the alert kinds.
7. Restore and fork handling, key backup, and the release process of the verifier.

## 2. Principles and the trust model

### 2.1 Principles (decided)

1. **No format change of the chain.** Hash format version 1 stays. Everything new is built on top of
   it: a log over the block headers, signatures made by `kingresd`, anchors made by `kingresd`, and
   client signatures stored as rows of ledger tables of the extension itself. A digest or proof made
   today verifies after this phase.
2. **Nothing on the commit path.** A transaction never waits for a signature, a publication or an
   anchor. Blocks are built by the existing workers; everything else reads closed blocks afterwards.
   The one exception is the optional client-signature policy `required` (3.7), which checks, at
   pre-commit, that attestations the client already supplied are present; it makes no network call.
3. **Offline first.** With no anchor target, no publication listener and no network, the cluster
   runs, builds blocks, signs checkpoints (the key is local) and produces a bundle that a verifier
   checks offline. The console shows the absence of anchors as a state, not as an error (6.9).
4. **Keys and credentials are outside PostgreSQL.** A superuser of the database is the first attacker
   of the threat model. Signing keys, anchor credentials and the passphrase live in files of
   `kingresd` (3.2), never in a table, a task row, a log or the audit file.
5. **The verifier trusts what the operator pinned, not what the server says.** Everything the server
   publishes is a claim; the verifier accepts a claim only under a key, a root or a timestamp
   that the auditor holds from another channel (7.2).
6. **Decide for the unreachable.** A group that cannot be read is carried into the aggregate as
   stale and flagged; a log that diverges is stopped and alerted, never repaired silently (5.5, 9.4).

### 2.2 What each mechanism defeats

Attackers: **DB superuser** (SQL access with every right, can edit catalogs, can use `COPY` to
files, cannot read `kingresd`'s files); **host operator** (root on a server, reads every file of
`kingresd` and the data directory, can restore backups, replace binaries); **compromised
`kingresd`** (code execution in the process; for the manager leader this includes the signing key and
the anchor credentials, and the control-plane README already states "a compromised leader is a
compromised cluster"); **network attacker** (reads and changes traffic between the verifier or a
monitor and the publication endpoint, or between `kingresd` and an anchor target).

| Mechanism | DB superuser | Host operator | Compromised `kingresd` | Network attacker |
|---|---|---|---|---|
| Chain only (today) | Detected only against a digest kept outside | Not defeated (can rewrite everything, including the digest list in the database) | Not defeated | n/a |
| Server-signed checkpoints, key in a file of `kingresd` (3.2) | **Defeated for rewriting history**: a rewritten chain cannot carry a valid signature for a size already signed, and the publisher refuses to extend a log whose published prefix changed (4.7). Not defeated if the superuser also controls `kingresd` | Not defeated: reads the key and signs a rewritten history | Not defeated | **Defeated** for tampering with tiles in transit (signature, hashes); not for blocking or delaying |
| Server key held outside the host (external signer, 3.2.3) | As above | Partly: cannot copy the key, but can still ask the signer to sign while the host runs; the signer's own limits and audit matter | Same as host operator | As above |
| Anchors, WORM in another administrative domain (6.3) | Defeated | **Defeated if the operator has no right on the bucket**: cannot delete or overwrite a locked version, cannot hide that a different aggregate was published | Defeated for past anchors (cannot remove them); can anchor wrong future aggregates | Defeated (TLS plus signed content); not for blocking submissions |
| Anchors, RFC 3161 (6.4) and OpenTimestamps (6.5) | Defeated | Defeated for the *time* of a past aggregate: the evidence is a signature of a third party or a Bitcoin attestation that the operator cannot produce for a different aggregate | Same | Same |
| Mirrors and monitors that copy checkpoints (4.8, 7.5) | Defeated once a copy of an old checkpoint exists elsewhere | Defeated for history a monitor already saw | Same | Defeated, if the monitor uses a path the attacker does not control |
| Client signatures (3.6) | Cannot forge a row of a key it does not hold; **can** insert unsigned rows unless the policy `required` is set; can register its own key (visible: a registration is a ledger entry) | Same, plus can steal a client key that lives on that host | Same | Defeated in transit |
| Not provided in phase 7: a split view (two histories shown to two clients), a consistent cut across groups, who is allowed to write | Not defeated | Not defeated | Not defeated | Not defeated |

What no mechanism of phase 7 does: prevent a change; prove that a legitimate-looking change was
authorised; stop an operator who controls the cluster and every anchor target and every mirror and
the auditor's trust file; show a consistent cut across groups (5.6).

### 2.3 What an operator must keep outside the cluster for verification to mean anything

Verification means something only against evidence the cluster's administrators cannot change. The
console says this on the pages concerned:

1. **The trust file** (`trust.json`, 7.2): the public signing key(s), the cluster origin prefix, and
   the roots of the time-stamping authorities used. It is created once (console, Signing, "Download
   trust file") and then copied to the auditor and to a safe place that is not the cluster. The
   public key fingerprint is shown on screen so it can be compared by voice or on paper.
2. **At least one anchor in a different administrative domain**: the WORM bucket under other
   administrators, or the time-stamp tokens, or the OpenTimestamps proofs (the console offers the
   bundle download for exactly this).
3. **A monitor** (7.5): a place that periodically fetches the publication and keeps the last
   checkpoint it saw (`kingres-verify monitor`), so that a rollback of the cluster is seen even if
   every anchor target is silent.
4. **A backup of the signing key and of the anchor credentials** (9.2), where the cluster's database
   administrators cannot reach it.
5. **The verifier**, obtained from a source other than the cluster (7.7).

## 3. Signing

### 3.1 What is signed, and what is not

**Decision.** The server signs **checkpoints of logs** (a size and a root), not blocks and not rows.
The block hash and the chain stay exactly as they are; the ledger extension holds no private key
and signs nothing. This differs from the recommendation in architecture.md ("a server signature
over each block with the key in an HSM/KMS and the key-id in the header") on purpose: a signature in
the block header would change the hash format, would put a private key (or a call to one) on the
path of the block builder inside PostgreSQL, and would not give what a checkpoint gives (one
signature covers all blocks up to its size, and a consistency proof ties two of them together).
`LedgerBlock.signed` keeps its name and now means "the block is covered by a signed checkpoint of
its chain log" (true when the size of the latest signed checkpoint is greater than the height).

**Algorithm.** Ed25519 (RFC 8032), signature type `0x01` of
[signed-note](https://c2sp.org/signed-note), so every checkpoint is a standard signed note that
generic tooling can read. The Go standard library signs and verifies it, so the verifier stays
standard library only. ECDSA and RSA are rejected: more parameters, a nonce hazard, no advantage.

### 3.2 Server keys

#### 3.2.1 Scope

**Decision: one signing key per cluster**, not one per shard group. The signer is the manager
leader's `kingresd`, which reads every group anyway; per-group keys would isolate nothing (the
leader holds all of them) and would multiply what a verifier must pin. Chains are told apart by the
key *name* inside the signature (the origin of the log, 4.2), so one public key serves every log of
the cluster. The key id of a signature is computed per name as signed-note defines it:
`key_id = SHA-256(key_name || 0x0A || 0x01 || public_key)[:4]`.

**Cluster identifier.** `kingresd` creates `cluster_id` (32 random bytes) when the first signing key
is created and stores it in `kingres_cp.ledger_cluster`. It is public (it is hashed into the
aggregate identifier, 4.2) and the console shows it.

#### 3.2.2 Where the private key lives and how `kingresd` holds it

- File `<ledger-keys-dir>/signing/<fingerprint hex>.key`, mode 0600, owned by the `kingresd` user,
  written with the atomic procedure `pki.WriteFile` uses for `ca.key`
  (`kingres/control-plane/internal/pki/files.go`). `<ledger-keys-dir>` defaults to
  `<data-dir>/ledger/keys`; `--ledger-keys-dir` / `KINGRES_LEDGER_KEYS_DIR` moves it, for example
  onto another volume. The fingerprint is the SHA-256 of the 32-byte public key.
- Format: one line `KINGRES-KEY-1`, then JSON. Without a passphrase: `{"seed": "<base64>"}`, as
  `ca.key` is stored in clear, and the console says so on the Signing page. With a passphrase file
  (`--ledger-key-passphrase-file` / `KINGRES_LEDGER_KEY_PASSPHRASE_FILE`, read at start, 0600,
  not kept in memory longer than needed): `{"kdf": "pbkdf2-hmac-sha256", "iterations": 600000,
  "salt": "<16 bytes b64>", "nonce": "<12 bytes b64>", "ciphertext": "<seed sealed with AES-256-GCM,
  b64>"}`. Everything is in the Go standard library (`crypto/pbkdf2`, `crypto/aes`, `crypto/cipher`).
- In memory the seed is held in one object (`internal/ledgerkeys.fileSigner`) that exposes `Sign`
  only. The process never logs, audits, serializes or returns the seed. Core dumps are off in the
  unit file shipped by the packaging (`LimitCORE=0`); the packaging step checks it.
- **The leader holds it, and so do the leader candidates.** The manager leader is the server of the
  map group's primary (ADR 0018), so only a server that runs an instance of shard group 0 can become
  leader. The key is copied (task `ledger.key.distribute`, internal call
  `POST /internal/v1/ledger/key/install`, leader-only caller, mutual TLS) to exactly those servers
  and removed (`.../key/remove`) from a server that stops running an instance of group 0. This is
  the mechanism the CA key needs for a leader change (control-plane README, "What has to move with
  the manager"); the step is written so that the CA key can use it. Until leader change exists (HA
  part 2) there is one holder and the distribution is a no-op that a cluster test exercises with a
  second server.
- A server that is not a holder never has the file; a request that needs a signature there is
  forwarded to the leader like every mutating operation.

#### 3.2.3 The signer interface and the external signer

`internal/ledgerkeys` defines

```go
type Signer interface {
    Public() ed25519.PublicKey
    Sign(ctx context.Context, msg []byte) ([]byte, error) // Ed25519, 64 bytes
}
```

with two implementations. **`file`** (default): the local key above. **`command`** (optional, built
in the last step): the operator configures a program and arguments (`--ledger-signer-command`, no
shell); `kingresd` writes the message to its standard input and reads 64 bytes from its standard
output, timeout 5 seconds; the public key is entered in the console, and `kingresd` **verifies every
returned signature with it before using it**, so a broken or hostile program cannot make `kingresd`
publish a bad checkpoint. This is the hook for an HSM, a KMS or a remote signer without linking a
vendor library into `kingresd`. With it the key is not on the host (the "outside the host" row of
2.2). While the signer is unavailable, publication keeps extending logs and tiles but signs no new
checkpoint, and the alert `ledgerSignerUnavailable` (6.9) says so.

#### 3.2.4 What is signed: the formats

All server signatures are signed notes (C2SP `signed-note`, fetched 2026-11-15: the text ends in a
newline; then a blank line; then one or more lines of U+2014, a space, the key name, a space, and
`base64(4-byte key id || signature)`; the text must be valid UTF-8 without control characters other
than newline). Key names must be non-empty and contain no Unicode space and no `+`; the origins
below satisfy that.

**Checkpoint** ([tlog-checkpoint](https://c2sp.org/tlog-checkpoint): three non-empty lines, the
origin, the size in ASCII decimal, the root in standard base64 with padding), signed with the
cluster key under the key name `<origin>`:

```
<origin>
<size>
<base64(root)>
```

No extension lines (the specification says they are not recommended and not auditable). During a key
overlap (3.3) it carries two signature lines, one per key.

**Event** (a signed note; the key name is the cluster origin prefix):

```
kingres-ledger-event/1
seq: <decimal>
time: <RFC 3339 UTC, whole seconds>
type: <key-added | key-retired | key-revoked | epoch-closed | chain-retired | group-removed>
<type-specific "name: value" lines in the order given in 3.3 and 9.4>
```

Events are numbered from 0 without gaps and published as `events/<seq as 6 digits>` (4.6). The text
is ASCII; free text is at most 200 bytes of printable ASCII.

### 3.3 Rotation, retirement, revocation, and old signatures

States of a key in `kingres_cp.ledger_signing_key`: `next` (generated, not yet signing), `active`,
`retired`, `revoked`. At most two keys sign at a time (during an overlap).

**Rotation (planned)**, task `ledger.key.rotate`, console "Rotate key":

1. Generate the new key K2 (state `next`); distribute it (3.2.2).
2. Publish event `key-added`: lines `key: <base64 public key K2>`, `fingerprint: <hex>`,
   `not-before: <time>`. The note is signed by **both K1 and K2** (K1 authorises, K2 proves
   possession). A verifier that pinned K1 accepts K2 from this moment: this is how trust moves
   without a new out-of-band step.
3. K2 becomes `active`; K1 stays `active`; for the overlap (default 30 days, `overlapDays` 1 to 365)
   every checkpoint carries signatures of both.
4. At the end of the overlap (or on "Retire now") publish `key-retired` for K1 (signed by K2); K1
   becomes `retired`: it signs nothing more and its private file is deleted from every holder (the
   console says the operator may archive an offline copy first).

**Old signatures after rotation.** A checkpoint signed by a retired key stays valid for ever. The
verifier's rule: a signature by key K is acceptable if K is pinned (directly, or through `key-added`
events from a pinned key) and K is not revoked as of the time the signed statement was made. The
time of the statement is shown by an anchor (6) or, without one, is the signer's own claim; the
verifier prints which it used.

**Revocation (compromise)**, console "Revoke key", type-the-fingerprint confirmation, role
`kingres_admin`. The operator states `compromised-since` (default: now). `kingresd` publishes
`key-revoked` (lines `key`, `compromised-since`, `reason`), signed by a *different* key that is
active at that moment (if none exists the dialog generates one in the same task). In the verifier a
signature by a revoked key is rejected unless the statement it signs is provably older than
`compromised-since`: the signed checkpoint's size and root must be covered by an anchor with an
earlier time (the aggregate checkpoint of that period is anchored, and the chain checkpoint is
checked by consistency against the anchored aggregate's member `log_root`). A checkpoint without
such evidence is untrusted after the compromise date even if its text is old; that is the honest
consequence of a stolen key, because the thief can sign a back-dated checkpoint and only an anchor
tells the difference. The revoked key's private file is deleted from every holder.

**An auditor whose only pinned key is revoked** cannot check the event (it is signed by a key the
auditor does not trust) and needs a new trust file out of band. The revoke dialog says so.

### 3.4 What the verifier needs to trust, and how it gets it

Pinned in `trust.json` (format in 7.2): the cluster origin prefix and one or more Ed25519 public
keys with an optional validity window. The console shows the fingerprint and offers the file; the
publication also serves `<prefix>/keyring.json` (public keys plus the events chain) so that a
verifier that pinned K1 can follow a rotation. **The keyring fetched from the publication is never
a source of first trust**: `kingres-verify trust init` takes the public key from a file or an
argument the user typed and checks the typed fingerprint.

### 3.5 Threat model of the server signature

The row of 2.2. In words: it defeats a database superuser and a network attacker; it does not
defeat the operator of the host that holds the key, nor a compromised `kingresd`; the external signer
narrows the second case; anchors are what defeat the operator.

### 3.6 Client-side signing: what is proven

A **client attestation** is a signature by a client-held Ed25519 key over one ledger entry of one
row version. The client states: "the holder of key K wrote (or removed) this row version in this
chain". It proves that the holder of K's private key signed exactly that `row_hash` for that
`table_id`, `op` and `chain_id`; that the signature is in the chain no later than the block that
holds the attestation (and, through anchors, no later than the anchor's time); and that K was
registered and not revoked in the chain before that block.

It does **not** prove: that the client saw more than the hash (a client that signs a hash the server
returned proves only that it approved the server's claim; the client must recompute the hash from
its own data, with `kingres-verify row-hash` or the same encoding in its library); that the row was
not deleted later (the ledger covers that); that the person who holds the key is the person named
(the mapping from key to person is the registry's `label` and `role_name`, chosen by an
administrator); that rows without an attestation were not written by someone else (the policy
`required` and the verifier's checks, 3.7 and 7.4).

### 3.7 Client signing: the SQL interface and the stored form

Everything is in `kingres_ledger` **0.3**. It changes nothing in hash format version 1. The
attestation and the key registry are stored as rows of **ledger tables of the extension itself**, so
the existing machinery (entries, blocks, proofs, `ledger_verify`) makes them tamper-evident and
provable without a new hash tag.

**Catalog** (schema `kingres`; created by `kingres_ledger--0.3.sql` and by the upgrade script
`kingres_ledger--0.2--0.3.sql`; each table is registered in `ledger_table` as `append_only` by an
internal registration function `kingres.ledger_register_internal(regclass)` that the script calls;
the hook lets the script's own statements through, `protect.c`, the scoped rule of finding L2-01;
the first step of the extension work proves this, section 10, step 8):

```sql
-- Registrations and revocations of client keys.  Both are ledger tables.
kingres.ledger_client_key (
  key_id        bytea  NOT NULL,          -- 16 bytes: SHA-256(public_key)[:16]
  algorithm     text   NOT NULL,          -- 'ed25519'
  public_key    bytea  NOT NULL,          -- 32 bytes
  role_name     name,                     -- NULL: any role may attest with it
  label         text   NOT NULL,          -- at most 200 characters
  registered_by name   NOT NULL,
  registered_at timestamptz NOT NULL
)
kingres.ledger_client_key_revocation (
  key_id     bytea NOT NULL,
  reason     text  NOT NULL,              -- at most 200 characters
  revoked_by name  NOT NULL,
  revoked_at timestamptz NOT NULL
)
-- One row per attested entry.  A ledger table.  Unique (key_id, nonce).
kingres.ledger_attestation (
  ledger_txid bigint NOT NULL,            -- the entry's transaction
  seq         int    NOT NULL,            -- the entry's sequence number in it
  key_id      bytea  NOT NULL,
  nonce       bytea  NOT NULL,            -- 16 random bytes chosen by the client
  signature   bytea  NOT NULL             -- 64 bytes
)
```

A key is active iff it has no revocation row. One column is added to `ledger_table`:
`signature_policy text NOT NULL DEFAULT 'optional' CHECK (signature_policy IN ('optional',
'required'))`, placed **sixth** (`protect.c` reads the fifth column, `history_relid`, by position).

**Functions** (all `SECURITY DEFINER`, fixed `search_path`, not executable by PUBLIC unless said):

| Function | Does |
|---|---|
| `ledger_client_key_register(public_key bytea, role_name name, label text) returns bytea` | Checks lengths, inserts into `ledger_client_key`, returns `key_id`. The same public key cannot be registered twice. Granted to the service role only (the console calls it) and to roles an administrator grants `EXECUTE` |
| `ledger_client_key_revoke(key_id bytea, reason text) returns void` | Inserts into `ledger_client_key_revocation`. Same grants |
| `ledger_attest(relation regclass, row_hash bytea, op "char", key_id bytea, nonce bytea, signature bytea) returns void` | In the current transaction: finds the entry (`ledger_txid` of this top-level transaction, `table_id` of `relation`, `row_hash`, `op`); checks the key is registered, not revoked, and that its `role_name` is NULL or equals `ledger_outer_user()`; builds the message below; verifies the Ed25519 signature with OpenSSL (`EVP_DigestVerify` with `EVP_PKEY_ED25519`; compiled only when PostgreSQL was built with OpenSSL, otherwise the function raises `0A000` "client signatures need a server built with OpenSSL"); inserts the attestation row, which the capture trigger enters in the same transaction. Errors: `KL011` no such entry in this transaction; `KL012` unknown, revoked or not allowed for this role; `KL013` bad signature; `KL014` nonce reused. `EXECUTE` is granted by an administrator to the roles that write ledger tables |
| `ledger_attestation_message(chain_id bytea, table_id int, op "char", row_hash bytea, key_id bytea, nonce bytea) returns bytea` | PUBLIC. The message below, for a client that wants to compare its own encoding |
| `ledger_set_signature_policy(relation regclass, policy text) returns void` | Owner of the table (and administrators granted `EXECUTE`). `optional` to `required` only; there is no way back, like the mode of a table |

**The message** the client signs (byte exact; `||` is concatenation; integers big-endian):

```
"kingres-ledger/client-signature/v1" || 0x00 ||
chain_id (32 bytes) || u32 table_id || u8 op ('I' = 0x49, 'D' = 0x44) ||
row_hash (32 bytes) || key_id (16 bytes) || nonce (16 bytes)
```

The signature is plain Ed25519 over these bytes. `chain_id` makes a signature useless in another
chain. The nonce makes a replay in the chain a unique-key violation (`KL014`); a replay by someone
who rewrites the table is a duplicate `(key_id, nonce)` that `ledger_verify` reports (new check
`attestation_duplicate`).

**Client workflow.** (1) Read `chain_id` and `table_id` (both readable by PUBLIC). (2) Compute the
row hash from the row the client holds, with the canonical encoding (README, "Hash format version
1"). (3) In the same transaction as the write, call `ledger_attest(...)`. When the server fills
columns the client cannot know (`now()`, a serial), the client inserts `... RETURNING
kingres.ledger_row_hash(t.*)`, **recomputes the hash from its own copy of the data and compares**,
signs, and calls `ledger_attest` in the same transaction (one more round trip).

**The policy `required`.** For a table with the policy, the capture code keeps the entries of the
transaction that still lack an attestation (it already holds the per-transaction sequence counter)
and clears one when `ledger_attest` accepts it. At `XACT_EVENT_PRE_COMMIT` and
`XACT_EVENT_PRE_PREPARE` any entry left raises `KL010` "ledger entry without client signature". Every
entry counts, `I` and `D`: an `UPDATE` needs two attestations (old version removed, new inserted). A
trigger that changes a row again produces entries the client did not foresee, so `required` is for
tables written by statements the client controls; the README says so. `ledger_verify` gains the
check `unattested_entry` for entries of a `required` table with no attestation row. The policy is
not covered by a hash, so a superuser can switch it back by editing the catalog; `ledger_verify`
and the verifier cannot see entries written unattested while it was off. The standalone verifier
checks one entry at a time (7.4).

**Registration and revocation.** Not by clients: an administrator registers a key (console "Client
keys", role `kingres_admin`; REST `registerLedgerClientKey`) and is responsible for checking its
holder. Revocation is the same. A revoked key's attestations made **before** the revocation stay
valid. The verifier's rule: an attestation is valid iff the block height of its transaction is
**lower** than the block height of the revocation's transaction (the same block counts as after,
because the order inside a block is by transaction id, not by commit order).

**Cost.** One Ed25519 verification per attested entry (OpenSSL; tens of microseconds, to be
measured in step 8) plus one row of about 250 bytes and one entry. Writes without attestation are
unchanged.

### 3.8 Threat model of client signatures

They defeat a database superuser who wants to forge a row *as* a registered client. The superuser
can insert a row without an attestation (visible when the policy is `required` and the check
runs), can register a key of their own (the registration is a ledger entry; the verifier's
`--client-key` pins the expected key, so a row signed by a key the auditor did not pin fails), and
cannot remove an attestation without breaking the block roots. They do not defeat a client whose
key is stolen (revocation closes the window from the revocation block on) nor a host operator who
can read client keys that live on that host.

## 4. Tile publication

References, fetched 2026-11-15: [tlog-tiles](https://c2sp.org/tlog-tiles) (hash SHA-256, tile height 8,
paths, partial tiles, entry bundles, HTTP rules), [tlog-checkpoint](https://c2sp.org/tlog-checkpoint),
[signed-note](https://c2sp.org/signed-note), RFC 9162 section 2.1
([datatracker.ietf.org/doc/html/rfc9162](https://datatracker.ietf.org/doc/html/rfc9162): tree
definition, inclusion proof verification 2.1.3.2, consistency proof verification 2.1.4.2), and
[static-ct-api](https://github.com/C2SP/C2SP/blob/main/static-ct-api.md), which shows how a real log
layers its own entry format on the same tiles (the CT-specific parts are not used here).

### 4.1 The mapping from the ledger's tree, and what differs

The ledger of today has **three levels of RFC 6962 trees and one hash chain**: a tree over the
entries of a transaction, a tree over the transactions of a block, and a **linear chain** of blocks
(`prev_hash`). The hashing is exactly RFC 6962 (SHA-256, `0x00` for leaves, `0x01` for nodes, a
tree of `n` leaves splits at the largest power of two below `n`: the README, "Hash format version
1", and `merkle.c`; the verifier's `internal/merkle` implements RFC 9162's path verification). So:

- **Compatible**: the hash function, the leaf and node prefixes, the shape of the tree. Any two of
  its trees can be put in tiles unchanged.
- **Different**: the top level is not a tree. A digest names the latest block hash; a proof walks
  every header between its block and the digest (cost linear in the distance, noted in the README
  "Limitations"). There is no consistency proof between two digests except the list of headers. A
  tile log needs a Merkle tree over a sequence with consistency proofs.

**Decision: publish in tile layout, without changing the extension's tree.** A **chain log** is a new
tree whose leaves are the **block headers** of one chain, in height order; leaf `i` is the block of
height `i`. The log is built and published by `kingresd` from the closed blocks; the chain, the block
hash and the digest are untouched.

Rejected: *adapting the chain* (replacing the linear `prev_hash` by a Merkle mountain range or by
`prev_hash` plus a tree root in the block hash). It would be a format change `hash_version = 2`
that every digest, proof and the verifier would have to carry for ever, for a gain that the extra
log gives without it. *A native layout* (a tree that is not RFC 6962 tiles) was rejected because
every generic client, mirror and witness of the transparency-log ecosystem would be unusable.

Soundness of the extra log: the leaf data is exactly the **block hash preimage** (4.3), so the
leaf commits to `prev_hash`, and the root commits to every header and, through `prev_hash`, to
the chain. The two structures cannot disagree without one of them failing a check the verifier
makes (`block_hash = SHA-256(leaf data)` and `prev_hash` of leaf `i + 1` equals the block hash of
leaf `i`; the `monitor` checks it on every entry it fetches).

### 4.2 Identity of a log: origin, epoch, URLs

- **Publication settings** (in `kingres_cp.ledger_cluster`, console "Publication"): `originPrefix`
  (a name without scheme and without trailing slash, for example `ledger.example.com/kingres`;
  default `<server address>/kingres-ledger`, which the console asks the operator to confirm), and
  `publicUrl` (where the endpoint is reachable, for the links the console shows). **`originPrefix`
  cannot be changed after the first checkpoint was signed**: it is the name under which every
  signature is made. The operator who needs another prefix creates a new cluster generation by the
  procedure of 9.4 (a documented, evented break), not by editing a setting.
- **Chain log origin**: `<originPrefix>/c/<chain_id as 64 hex>/e<epoch>`. `epoch` starts at 1 and
  grows only by the restore procedure (9.4). The chain identifier is public; the database name
  and the shard group id are not part of any public name.
- **Aggregate log origin**: `<originPrefix>/a/<aggregate_id as 64 hex>`, with
  `aggregate_id = SHA-256("kingres-aggregate-id" || 0x00 || cluster_id || database_name in UTF-8)`.
  It hides the database name (the name is not recoverable from the id without guessing it) and binds
  the log to the cluster.
- **Cluster origin**: `<originPrefix>`, the key name of events and the keyring.
- **URL prefix of a log**: `<publicUrl>/c/<chain_id>/e<epoch>` or `<publicUrl>/a/<aggregate_id>`.
  The log serves `checkpoint`, `tile/<L>/<N>[.p/<W>]`, `tile/entries/<N>[.p/<W>]` under it.

The specification says the origin SHOULD be a URL prefix with the scheme removed; ours is when
`publicUrl` is `https://<originPrefix>`. A deployment that serves the same content under other
prefixes (a CDN) is allowed by tlog-tiles ("logs MAY offer multiple URL prefixes").

### 4.3 The leaf: the block entry

For the block of height `h` the leaf data is the **87 bytes** the block hash covers:

```
u8   0x13 (tag "block")
u16  hash_version (1)
u64  height
[32] prev_hash
[32] tx_merkle_root
u32  tx_count
i64  closed_at, microseconds since 2000-01-01 00:00:00 UTC
```

so `block_hash = SHA-256(leaf data)` and the leaf hash is `SHA-256(0x00 || leaf data)`. The leaf holds
no transaction, no row, no user name, no table id. It is what `kingres.ledger_block_preimage(...)` of
the new extension version returns (8.1).

### 4.4 Tile layout (tlog-tiles, as fetched)

- **Tile height 8**: a full tile is 256 hashes, 8192 bytes. Level 0 tiles hold leaf hashes; level
  `l > 0` hold the tree hashes of full tiles of level `l - 1`
  (`tile(l, n)[i] = MTH(D[(n*256 + i) * 256^l : (n*256 + i + 1) * 256^l])`).
- **Paths** under the log prefix: `tile/<L>/<N>` with `L` the level (0 to 63) and `N` the tile index
  in three-digit groups, every group but the last prefixed with `x` (index 1234067 is `x001/x234/067`);
  a partial tile of width `W` (1 to 255) is `tile/<L>/<N>.p/<W>`. Content type
  `application/octet-stream`.
- **Partial tiles**: width at level `l` for tree size `s` is `floor(s / 256^l) mod 256`; an empty
  tile is never served; a partial tile is served for every size that has a checkpoint, and
  may be deleted once its full tile exists (the publisher deletes them 24 hours after the full tile
  appears, the limit of what a client with an old checkpoint may still ask for).
- **Entry bundles**: `tile/entries/<N>[.p/<W>]`: a sequence of entries, each prefixed by a
  big-endian `u16` length, each equal to the leaf data of 4.3 (so 89 bytes per entry, 22784 bytes
  for 256). The bundle format of tlog-tiles is used, so the standard path is correct.
- **Checkpoint**: `checkpoint`, `text/plain; charset=utf-8`, the signed note of 3.2.4, short cache.
  Tiles and bundles: long cache, immutable. No redirects. `gzip` is accepted.
- **Minimum index** is always 0 (nothing is pruned). Retention of tiles is for ever; the storage
  cost is in 9.7.

### 4.5 Who writes the tiles, from what, where they live

`kingresd` on the manager leader runs the **publisher** (a loop of the scheduler of 6.7). It is the
only writer. For each chain log:

1. Read from the chain's group (the internal call `ledger/read`, 8.3) the head `(height, block_hash)`
   and, when `published_size < height + 1`, the leaves `published_size .. height` through
   `ledger_blocks_entries` (8.1), at most 10 000 per round.
2. Check that leaf `published_size` has `prev_hash` equal to the block hash of the last published
   leaf (kept in `kingres_cp.ledger_log.head_block_hash`) and that every leaf's hash links to the
   previous one. If not: **divergence** (4.7).
3. Compute leaf hashes, extend the tree (the state is the compact range of at most 64 hashes,
   `frontier`, kept in the row of the log), write every changed or new tile and bundle.
4. Sign the checkpoint for the new size (one signature per active key) and store it.
5. All of 3 and 4 in **one transaction** of the system database together with the update of the log
   row (`size`, `root`, `frontier`, `head_block_hash`): a crash leaves the previous state; a
   rerun produces the same bytes (tile content is a function of the leaves).

**Storage.** The canonical copy of tiles, bundles, checkpoints and events is in the system database
(`kingres_cp.ledger_tile`, `ledger_checkpoint`, `ledger_event`), because it follows the map group
(its standbys and the leader) and survives a leader change with no extra mechanism. These rows are
public data: if a superuser rewrites them, signatures, anchors and mirrors tell (4.7). Optional
mirrors (a directory, an S3-compatible bucket without object lock; 10 step 24) copy the same
objects after commit; they are for serving, not for trust.

**Serving.** An optional HTTP listener (`--ledger-publication-listen host:port`, default off; the
console "Publication" shows whether it is on) serves `GET` and `HEAD` for the layout of 4.6 and
nothing else, no authentication, no cookies, `Cache-Control: public, max-age=31536000, immutable`
for tiles and bundles and `max-age=5` for `checkpoint` and the listings; TLS with the certificate
files the operator configures (`--ledger-publication-tls-cert`, `-key`); without them, plain HTTP,
and the console warns. A server that is not the leader answers by asking the leader over the internal
API (`ledger/publication/get`, a closed call that returns a stored object or "not found"), with an
in-process cache for immutable objects. The listener never touches a user database. Concurrency
cap and rate limit are settings of the listener (default 200 concurrent, 50 requests per second per
address).

### 4.6 The layout of a publication (and of a bundle)

```
<prefix>/
  keyring.json                         public keys, states, events seen
  events/<seq 6 digits>                signed notes (3.2.4)
  logs.json                            OPTIONAL list of logs with labels (off by default: it names databases)
  c/<chain_id>/e<epoch>/
      checkpoint
      tile/<L>/<N>[.p/<W>]
      tile/entries/<N>[.p/<W>]
  a/<aggregate_id>/
      checkpoint
      tile/..., tile/entries/...
      anchors/<size>/<target id>.json   anchor evidence (6.2), receipts as base64 fields
```

A **bundle** is a directory of exactly this layout (or its `.tar.gz`), restricted to the logs and
sizes asked for, with one more file at the top, `bundle.json` (7.3). The verifier reads a directory,
a `.tar.gz` or an HTTP(S) base URL with the same code.

### 4.7 The publisher is itself a monitor: divergence

The publisher compares what it reads with what it has already signed. It stops a log, marks it
`diverged`, raises the alert `ledgerChainDiverged` (critical) and publishes no further checkpoint
of that log when:

1. a leaf it has published no longer matches the database (checked by hash on the head block of
   every round, and on a full range once a day and on request: the verification option
   `publication`);
2. the chain does not link (`prev_hash` mismatch at the new leaf);
3. the head height is **lower** than the published size minus one (a rollback: an older backup was
   restored, or an asynchronous failover lost blocks).

It never overwrites a published tile or checkpoint; the primary key of `ledger_tile` and
`ledger_checkpoint` makes the write fail. The way out is the explicit procedure of 9.4 (a new epoch
and a signed event); there is no "repair" button. For the aggregate, the member of a diverged chain
carries the flag `diverged` and its last good head (5.2).

### 4.8 Mirrors, monitors and witnesses

- **Mirrors and monitors** are any client that copies the publication. `kingres-verify monitor`
  (7.5) is the supported one; the layout is the standard one, so third-party tools for tiled logs can
  mirror it.
- **Witness cosigning is future work.** C2SP defines cosignatures
  ([tlog-cosignature](https://c2sp.org/tlog-cosignature), [tlog-witness](https://c2sp.org/tlog-witness));
  those two documents were not read for this design (statements that need verification, section 13). A
  checkpoint has room for more signature lines, so a witness adds lines and nothing else
  changes. The extension point is the checkpoint store: `ledger_checkpoint.note` is the full signed
  note, and the verifier accepts extra lines and counts those under pinned witness keys
  (`trust.json`, field `witnesses`, reserved and unused in the first release). A witness would
  defeat a split view; phase 7 does not.

### 4.9 Privacy: what the publication reveals

Hashes only. A third party who reads the tiles learns: that a chain exists (its `chain_id`), the
number of blocks, the **time of each block** (`closed_at`), the **number of transactions in each
block** (`tx_count`), the root of each block's transaction tree, and the previous-block hash chain.
So it learns when the database is busy and roughly how busy (transactions per block over time),
and, from the number of chains of one aggregate, the number of shard groups of a database. It does
not learn table names, user names, row contents, row hashes, the database name (hidden in the
aggregate id) or the shard group ids. A row hash cannot be recovered. A person who already holds a
row and its block can prove inclusion; a person who does not hold it cannot find it. The console's
"Publication" page states these facts before the operator turns the listener on, and the
checkpoint interval (default 60 seconds) is the granularity of timing information beyond block
times. An operator who does not want the activity pattern published keeps the listener off and
exports bundles on demand.

### 4.10 Proofs with the log

- **Inclusion of a block in a chain log** (RFC 9162 2.1.3.2): leaf index `h`, tree size `N`, path
  of at most `ceil(log2 N)` hashes, computed from tiles by the verifier or by `kingresd`
  (`getLedgerBundle`). For a tree size of an older checkpoint the path is computed from the same
  tiles (the right edge of an older size is derived from the level 0 tile).
- **Consistency between two checkpoints** (2.1.4.2): sizes `m < n`, a path of at most `2*ceil(log2 n)`
  hashes. This replaces "list every header" for the question "is the later state an extension of
  the earlier one".
- **A row proof with the log** is the existing format-1 proof with `chain` empty (the document
  allows that when the proof's block is the digest's block; the verifier's new mode ignores the
  digest) plus a **log leg**: the checkpoint (or the aggregate member that names it), the leaf
  index `h`, the tree size and the inclusion path. Its size is `O(log n)` in the number of blocks,
  not linear. Verification in 7.4. The old linear proof stays valid and stays produced.

## 5. Per-shard ledgers and the aggregate digest

### 5.1 Ledger tables stay on one group: decision

Each shard group runs the extension in the user database and has **its own chain** (its own
`chain_id`, its own blocks), as today. A ledger table lives on the group where it was created:
a local table of that group, or a table of a distributed schema pinned to its owner group
(`shard-phase11.md` 11.1). **Phase 7 does not lift the refusal** to make a ledger table a
hash-distributed or reference table. What lifting it would take (recorded so that nobody has to
rediscover it):

1. Capture on the **partitions** of a distributed table: `ledger_enable` rejects partitioned
   and inheriting tables because the capture and the guard know one relation, not a tree.
2. A bucket move copies rows by logical replication, and the apply worker fires the `ENABLE ALWAYS`
   capture trigger of the target (README, "Logical replication into a ledger table works"): the
   target chain would record the moved rows as **new `I` entries**, and the source chain would lose
   rows with no `D` entry (the orphan cleanup drops the table), which `ledger_verify` of the source
   reports as tampering. A ledger-aware move would write a `D` entry with a link record on the
   source and an `I` entry with the same link on the target, in a transaction pair, and would need
   the move to be atomic with the block structure.
3. The split of phase 10 and the row router of phase 2b write rows through the parent or the
   partitions in ways the capture must not duplicate (`shard-phase10.md` 3.5).
4. Inclusion proofs would need the group of a row's *current* and *former* chains.

That is a design of its own (it touches `kingres_shard`, which other work is changing now) and
belongs after the first release. What phase 7 does instead: the refusal gets an explicit message
(10 step 25, optional) and the aggregate (below) is over chains, so it already works the day the
chains of a database are many for any reason.

**Bucket moves, therefore, never move a ledger row.** No chain changes by a move, a split or a
drain; a drain or a move that would need to move a ledger table's schema is already refused
(`schema_pinned`, `shard-phase11.md`). The aggregate needs no move handling.

### 5.2 The aggregate entry

The aggregate log of a database has one leaf per **aggregation round that changed something**. Leaf
data (byte exact; integers big-endian):

```
u8   0x23 (tag "aggregate")
u16  format (1)
u64  seq                      0-based; equals the leaf index
i64  generated_at_us          microseconds since 2000-01-01 UTC, the leader's clock
u16  member_count             1 to 512
member, member_count times, sorted by (chain_id, epoch) ascending as byte strings:
  u8   flags                  bit 0 stale, bit 1 retired, bit 2 diverged
  [32] chain_id
  u32  epoch
  u64  height                 latest block height at the time (the chain has height + 1 blocks)
  [32] block_hash
  [32] log_root               root of the chain log at size height + 1, under that epoch
```

A member is 109 bytes, the leaf at most 21 + 512 * 109 = 55 829 bytes, inside the `u16` limit of an
entry bundle. A database with more than 512 chains is refused by the collector (alert) until the
limit is raised in a later format. `0x23` avoids `0x00`, `0x01` and the tags `0x10` to `0x14`.

Flags: **stale** the group could not be read in this round; the member repeats the last good head.
**retired** this is the last aggregate in which the chain appears, because the group was removed or
the chain was closed (9.4); the next aggregate omits it. **diverged** the chain log is stopped (4.7);
the member repeats the last good head. A chain with no block yet has no member.

An aggregate round is **recorded** only when it differs from the previous leaf in a member's
`height`, `block_hash`, `log_root`, `flags` or in the set of members; otherwise nothing is appended.
At least one leaf is appended per `digestIntervalSeconds` when anything changed, which keeps the
leaf count proportional to activity.

### 5.3 The aggregate digest

The **aggregate digest** of a database at a moment is the checkpoint of its aggregate log: origin
`<originPrefix>/a/<aggregate_id>`, `size` leaves, `root` the Merkle root. It replaces the
placeholder "Merkle root over the chain heads" of the contract: `LedgerDigest.aggregateHash` is the
**hex of that root** and the digest's new member `aggregateSize` is the size; both are null when the
database has no aggregate (publication off, or one chain and the operator chose not to aggregate,
see below). A checkpoint commits to every leaf, hence to every member of every round so far, hence
(through `log_root` and `block_hash`) to every chain. Consistency of the aggregate log
shows that no earlier round was changed.

**One chain**: a database that lives in one group has an aggregate of one member per leaf. It is
still built when publication is on, because the aggregate is the unit that is anchored (6.1), so a
single-group cluster is anchored the same way.

### 5.4 The aggregation round (what the leader does)

A round is a function of the scheduler (6.7) per database that has ledger tables, every
`publication.intervalSeconds` and on demand (`generateLedgerDigest`):

1. List the shard groups that hold the database (the catalog). For each group call `ledger/read`
   on the server that runs its primary: `op: "head"` (or, once per `digestIntervalSeconds` and on
   demand, `op: "digest"`, which runs `ledger_generate_digest()` and so closes the open block and
   records the digest in the extension's table), with a timeout of 10 seconds. The service role is
   used; the internal call is a closed set and takes no SQL.
2. Update each chain's log (4.5) from its head. Sign the chain checkpoints.
3. Build the aggregate leaf from the heads and the new `log_root`s, compare with the previous leaf
   (5.2), append, sign the aggregate checkpoint.
4. Mark the new aggregate checkpoint `to be anchored` (6.2).
5. For a scheduled or requested digest: store the `LedgerDigest` view (the per-chain documents of the
   extension, the aggregate size and hash) in `kingres_cp.ledger_digest_view`.

All state of a round is written in one transaction per log (steps 2 and 3 are separate
transactions; a crash between them leaves chain logs ahead of the aggregate, and the next round
catches up). Rounds of different databases run one after another, with a limit of 8 rounds in
flight at once by a worker count, so a slow group delays only its own database.

### 5.5 Unreachable groups, added and removed groups, moves, failover

- **A group cannot be read** (the server is down, the call times out, the chain's group has no
  primary): its member is repeated from the previous leaf with the flag **stale**; the round still
  completes for the others. The digest's `complete` is false and `uncoveredShardGroupIds` names the
  group (the contract fields that exist). An alert `ledgerGroupUnread` (warning after the
  publication interval, critical after one hour) names it. An aggregate that has a stale member
  is still anchored: it proves the other heads. The stale head is old news, not a claim.
- **A group is added** (a database starts to use a new group): the first round after its first
  block adds a member. Nothing else changes. A verifier treats a new `chain_id` as "new chain" and
  prints it.
- **A group is removed** (server group removal after a drain, `removeServer`): a chain on it cannot
  vanish silently. The removal operation of `kingresd` (the hook added in step 6) runs
  a final round for the chain, appends an aggregate leaf with the member flagged **retired**, signs
  an event `chain-retired` (lines `chain: <hex>`, `epoch`, `final-height`, `final-block-hash`,
  `reason`), and stops the log. A ledger on a removed server leaves with it: the existing
  `previewServerDrain` already names ledger tables (shard-phase10, 11.5) so the operator exports a
  bundle first. A chain that disappears from the next aggregate without a `retired` flag is reported
  by the verifier as "chain removed without record".
- **A database is dropped**: same as removal, for every chain of it, and the aggregate log is
  closed by an event.
- **Failover of a group**: the standby is a physical copy, so the chain continues with the same
  `chain_id`. If the new primary has every block the old one had (synchronous replication, the
  default), nothing is seen. If it has fewer (asynchronous commit, lost tail), the head is lower than
  the published size: **divergence** (4.7); the member is flagged `diverged` with the last good head,
  the alert is critical, and the operator takes the procedure of 9.4 (acknowledge, new epoch).
- **Buckets move**: no effect (5.1).
- **Leader change**: the new leader reads the system database, takes the key (3.2.2), and continues
  from `ledger_log.size`; the rounds are idempotent (primary keys on `(log_id, size)`).

### 5.6 What the aggregate proves, precisely

Given an anchored aggregate checkpoint `A` (size `n`, root `R`) and its leaf `k`:

1. **Containment.** For every member `m` of leaf `k`, the chain `m.chain_id` (epoch `m.epoch`) had
   a block of height `m.height` with hash `m.block_hash`, and the chain log of size `m.height + 1`
   has root `m.log_root`, at some instant of the round that the signer reports as `generated_at`.
   Together with the anchor: no later than the anchor's time.
2. **Continuity.** For two leaves `j < k` of the same aggregate log, each chain present in `j` is
   present in `k` (or flagged `retired` in the last leaf in which it appears), and its
   `height` does not decrease and its chain log at `k` is a consistent extension of the log at `j`
   (a consistency proof between the two `log_root`s exists). No history was rewritten between the
   two rounds, for any chain, unless the aggregate says `diverged`.
3. **Completeness of the set.** The set of chains is the set the leader found in the catalog; the
   signature states it; a chain that exists and is missing cannot be shown missing by the leaf alone
   (the catalog is the leader's claim), but the verifier flags a chain that was present and
   disappears without `retired`.

It does **not** prove: that the heads were read at the same instant (the reads of one round are
sequential and take up to seconds; the leader reports `generated_at` as the end of the round);
that a **cross-group transaction** is seen in all its groups at the same time (a two-phase
commit that finished on group A may not yet have a block on group B when the heads are read;
nothing relates blocks of different chains; ADR 0009: no global snapshot); an order of commits
between groups; that a group's chain is the whole database's data (only the tables that are ledger
tables are in a chain, as everywhere). A statement about a *consistent state of the whole
database at one time* needs the global snapshot of phase 8. This sentence is reproduced in the
console on the aggregate view and in the output of `kingres-verify aggregate`.

### 5.7 Contract mapping

`LedgerDigest.chains[]` now has one element per chain of the aggregate leaf (all groups; the
`complete` and `uncoveredShardGroupIds` fields keep their meaning and are now normally `true` and
empty); `aggregateHash` and the new `aggregateSize` come from 5.3; `anchors[]` are the anchors of
the aggregate checkpoint of the round the digest belongs to (6.2). Each chain's `document` stays
the digest JSON of the extension, byte for byte.

## 6. Anchoring

References, fetched 2026-11-15: [RFC 3161](https://www.rfc-editor.org/rfc/rfc3161) (time-stamp
protocol), [S3 Object Lock](https://docs.aws.amazon.com/AmazonS3/latest/userguide/object-lock.html)
and [PutObject](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutObject.html),
[MinIO object locking](https://docs.min.io/enterprise/aistor-object-store/administration/object-locking-and-immutability/),
[OpenTimestamps](https://opentimestamps.org/) and, for the byte formats and the calendar requests, the
reference client's source ([notary.py](https://raw.githubusercontent.com/opentimestamps/python-opentimestamps/master/opentimestamps/core/notary.py),
[timestamp.py](https://raw.githubusercontent.com/opentimestamps/python-opentimestamps/master/opentimestamps/core/timestamp.py),
[calendar.py](https://raw.githubusercontent.com/opentimestamps/python-opentimestamps/master/opentimestamps/calendar.py)).
Details that these did not settle are in section 13.

### 6.1 What is anchored, and how often

**What**: the **aggregate checkpoint** of a database (5.3). It commits to every chain of the database
(heads and chain-log roots) and to every earlier round (the log is append-only), so one anchor
covers everything before it; anchoring chain checkpoints one by one would multiply the cost for no
extra guarantee. The **payload** is the signed note; the **digest** that TSA and OpenTimestamps
receive is `SHA-256(note text)`, where the text is the three lines of 3.2.4 including the final
newline and excluding the signature lines. A WORM target stores the full signed note.

**When**: every `anchor.intervalSeconds` (cluster setting, default 3600, range 60 to 86400) for each
database whose latest aggregate checkpoint is newer than the one last anchored, and on demand
("Anchor now", `anchorLedgerNow`). Only the **latest** checkpoint is anchored: a backlog of
unanchored older checkpoints is coalesced (6.6), because the latest covers them.

**The window** the README speaks of is therefore: block interval (10 s) + publication interval
(60 s) + anchor interval (3600 s) + the target's own latency (OpenTimestamps: hours). The console
shows the age of the newest anchored aggregate per target, which is the live length of the window.

### 6.2 Targets: the interface and the records

```go
// package anchor (kingresd, internal/anchor)
type Target interface {
    Kind() string                                      // "worm" | "tsa" | "ots"
    Validate(cfg json.RawMessage) error                // shape of the non-secret config
    Test(ctx context.Context) TestResult               // 6.8: what the Test button runs
    Submit(ctx context.Context, p Payload) (Receipt, error)   // idempotent per (Payload.ID)
    Upgrade(ctx context.Context, r Receipt) (Receipt, error)  // pending -> final; identity for worm and tsa
}
type Payload struct {
    ID          string // "<aggregate_id hex>/<size>"
    Origin      string
    Size        uint64
    NoteText    []byte // the three lines
    SignedNote  []byte // text, blank line, signatures
    Digest      [32]byte
}
type Receipt struct {
    State     string          // "submitted" | "anchored"
    Evidence  json.RawMessage // target-specific, below
    EvidenceTime *time.Time   // the time the target vouches for, when known
    NextCheck *time.Time
}
```

Registered kinds: `worm`, `tsa`, `ots`. A target is a row of `kingres_cp.ledger_anchor_target` (id,
kind, name, enabled, non-secret config JSON, created by and at). **Which is the default:** none is
on by default (the first release works fully offline, principle 3). The console's setup suggests
**WORM first** (it works on a private network, under a different administrator, with MinIO or any
compatible store, needs no third party) and **one of TSA or OpenTimestamps second**, and warns when
only one kind is enabled ("a single anchor target is one point of failure and one administrator").
`tsa` and `ots` are optional.

**Anchor record** `kingres_cp.ledger_anchor`: `(id, target_id, log_id, size, payload_sha256, state,
attempts, next_attempt_at, evidence jsonb, evidence_time, anchored_at, last_error, created_at)`,
unique `(target_id, log_id, size)`. States: `pending` (to submit), `submitted` (accepted, not final:
OpenTimestamps before Bitcoin, or a WORM write whose verification read failed), `anchored`,
`failed` (permanent error: needs a person), `superseded` (a newer checkpoint replaced it before it
was submitted), `expired` (OpenTimestamps never confirmed within the limit). The contract's
`DigestAnchor.status` `pending|anchored|failed` maps: `pending` and `submitted` to `pending`,
`superseded` is not shown, `expired` to `failed`.

**Evidence file** `a/<aggregate_id>/anchors/<size>/<target id>.json` of the publication (public,
written when the record reaches `anchored`, and again when `submitted` holds something to keep):

```json
{"format": 1, "kind": "worm|tsa|ots",
 "origin": "<aggregate origin>", "size": 17, "payload_sha256": "<hex>",
 "created_at": "2026-12-01T10:00:00Z",
 "evidence": { ... one of the three shapes below ... }}
```

### 6.3 WORM: S3 Object Lock and compatible stores

**Semantics that matter (fetched):** Object Lock works only in buckets with versioning; retention is
per object **version**, so a new PUT of the same key makes a new version and does not touch the
locked one; **compliance** mode: no user, including the account root, can overwrite or delete the
version or shorten the retention until it expires; **governance** mode: users with
`s3:BypassGovernanceRetention` can, if they send `x-amz-bypass-governance-retention: true`;
a **legal hold** is independent of retention, has no expiry and is set and removed by anyone with
`s3:PutObjectLegalHold`; a default retention can be set on the bucket; a simple DELETE only adds a
delete marker. MinIO says its locking is "feature and API compatible with AWS S3", needs versioning
(turned on automatically when locking is enabled at bucket creation) and, since release
`RELEASE.2025-05-20T20-30-00Z`, can enable locking on an existing bucket with `mc retention set
--default`. Other S3-compatible stores (Ceph RGW, Backblaze B2 S3 API, Wasabi, Cloudflare R2, and
others) are **not** claimed: whether each supports Object Lock, `If-None-Match` and the checksum
headers is for the operator to learn from the **Test** button, which checks it.

**Config (non-secret)**: `endpoint` (URL; empty for AWS), `region`, `bucket`, `keyPrefix` (default
`kingres-anchors`), `pathStyle` (default true when `endpoint` is set), `mode` (`governance` |
`compliance`; default **governance**, because compliance is irreversible; choosing compliance in the
console needs the type-"compliance" confirmation and shows the retention date), `retentionDays`
(default 3650, range 1 to 36500), `ifNoneMatch` (`auto` | `on` | `off`; `auto` uses it and falls back
to a check-before-write when the store rejects it with 501 or 400), `caBundlePath` (PEM, optional).
**Secret**: access key id and secret access key (and an optional session token), 6.8. (Replaced by correction 63 of section 14.)

**Submit (steps, exact):**

1. Key = `<keyPrefix>/<aggregate_id hex>/<size as 20 decimal digits, zero padded>.note`.
2. Body = the signed note bytes. `PutObject` with SigV4 (own implementation, 6.8) and the headers
   `Content-Type: text/plain; charset=utf-8`, `x-amz-checksum-sha256: <base64 SHA-256 of body>`,
   `If-None-Match: *`, `x-amz-object-lock-mode: <GOVERNANCE|COMPLIANCE>`,
   `x-amz-object-lock-retain-until-date: <now + retentionDays, RFC 3339 UTC>`.
3. `200`: read `x-amz-version-id` and `ETag`. Then `HeadObject` with `versionId`; record
   `x-amz-object-lock-mode`, `x-amz-object-lock-retain-until-date`, `Last-Modified` (the store's
   own clock: the time evidence of this anchor) and `Content-Length`. If the mode is absent or
   differs from the config the record goes to `failed` with "the store did not lock the object".
4. `412` (key exists) or `409` `ConditionalRequestConflict`: `GetObject`; if the bytes equal the
   body the earlier attempt succeeded (a crash before the record was written): treat as success, read
   the version as in 3. If the bytes differ: **conflict**: a different aggregate checkpoint of the
   same size exists in the bucket. State `failed`, alert `ledgerAnchorConflict` (critical), no retry,
   no overwrite. This is exactly what rewriting history looks like to the cluster. (Replaced by correction 63 of section 14.)
5. Evidence: `{"endpoint": "...", "bucket": "...", "key": "...", "version_id": "...", "etag": "...",
   "lock_mode": "COMPLIANCE", "retain_until": "...", "last_modified": "...", "recorded_by": "cluster"}`.
   `EvidenceTime` = `last_modified`. (Replaced by correction 61 of section 14.)

**What the verifier checks later (7.4):** the note bytes in the evidence or fetched equal a
well-formed signed note whose text hashes to the payload digest and whose signature verifies under
the pinned key; with `--worm-url` (anonymous or presigned) it fetches the object version itself and
compares bytes; the retention fields are shown as "recorded by the cluster" unless the fetch
returned them (a HEAD through a presigned URL does). An evidence record that the verifier could not
re-fetch counts as **not independently checked** (exit status 3 if anchors were required).

**Not done in this phase:** lifecycle expiry handling beyond the retention date (the console shows
the retain-until date and warns 30 days before it passes; extending retention is a manual step
with the store's tools, 9.6), multi-region replication, legal hold management.

### 6.4 RFC 3161 time-stamp authority

**Config (non-secret)**: `url`, `hashAlgorithm` (`sha256` only), `policyOid` (optional),
`caBundlePath` (PEM file with the TSA's trust anchors and intermediates; **required**, it is what
makes the token checkable), `timeoutSeconds` (default 20). **Secret** (optional): HTTP Basic
username and password.

**Submit (steps, exact), following RFC 3161 sections 2.4 and 3:**

1. Build a `TimeStampReq` in DER: `version 1`, `messageImprint` = `{hashAlgorithm
   2.16.840.1.101.3.4.2.1 (SHA-256), hashedMessage = Digest}`, `reqPolicy` if configured, `nonce` =
   64 random bits (a positive INTEGER), `certReq TRUE`.
2. `POST url` with `Content-Type: application/timestamp-query` and `Accept: application/timestamp-reply`
   (RFC 3161 defines these two media types; it also uses `application/timestamp-response` in one
   place, so a reply with either is accepted).
3. Parse `TimeStampResp`. `status` must be 0 (granted) or 1 (grantedWithMods); then a token is
   present. Any other status (2 rejection, 3 waiting, 4 and 5 revocation notices) is an error: 2 is
   `failed`, 3 is retried. A reply without a token for status 0 or 1 is an error.
4. Verify the token **before accepting it**: the CMS `SignedData`, content type `id-ct-TSTInfo`;
   `TSTInfo.messageImprint` equals the request's; `TSTInfo.nonce` equals the request's;
   `TSTInfo.genTime` is read; the signing certificate (from the token's certificates, identified by the
   `ESSCertID`) carries `id-kp-timeStamping` as the only extended key usage, marked **critical**, as
   RFC 3161 requires; the certificate chains to `caBundlePath` with the validity checked **at
   `genTime`**; the CMS signature verifies (RSA PKCS#1 v1.5 or ECDSA, SHA-256 or SHA-384; others
   rejected); `genTime` is within 5 minutes of the leader's clock, else the record is accepted but the
   alert `ledgerAnchorBehind` carries the skew. (Replaced by correction 62 of section 14.)
5. Evidence: `{"tsa_url": "...", "token_der_b64": "...", "gen_time": "...", "serial": "...",
   "policy": "...", "recorded_by": "cluster"}`. `EvidenceTime` = `genTime`; the record is `anchored`
   at once (no upgrade).

The CMS and X.509 handling is in `kingresd` code of its own (`encoding/asn1`, `crypto/x509`,
`crypto/rsa`, `crypto/ecdsa`); **not shared with the verifier**, which has a second implementation
(its independence is a stated property, `kingres/verifier/README.md`); a shared set of vectors
(tokens from public time-stamping authorities and from a local test authority) is run by both.
**Not checked in this phase**: revocation status of the TSA certificate (no OCSP, no CRL; long-term
validation is out of scope, the evidence keeps the whole token and the certificates so a
long-term-validation tool can add it); the token's `accuracy` and `ordering` (printed, not used).

**What the verifier checks later:** the above on the stored token, offline, against the auditor's
own `tsa_roots` in `trust.json`, and that the message imprint equals `SHA-256` of the checkpoint
text it is verifying. It prints `genTime` as the time.

### 6.5 OpenTimestamps

**Behaviour taken from the reference client's source (fetched):** a calendar accepts `POST
<calendar>/digest` with the raw digest as the body and the header `Accept:
application/vnd.opentimestamps.v1` and returns, with status 200, a serialized timestamp fragment that
ends in a **pending attestation** (tag `83dfe30d2ef90c8e`, payload: the calendar URI as
length-prefixed UTF-8, at most 1000 bytes, characters limited to letters, digits and `-._/:`); the
later answer is fetched with `GET <calendar URI>/timestamp/<hex of the commitment>` with the same
`Accept`; a `404` is reported by the client as "the calendar does not have that commitment"; the
**Bitcoin attestation** has tag `0588960d73d71901` followed by the block height; the detached file
starts with the magic `00 4f 70 65 6e 54 69 6d 65 73 74 61 6d 70 73 00 00 50 72 6f 6f 66 00 bf 89
e2 e8 84 e8 92 94` ("\x00OpenTimestamps\x00\x00Proof\x00" and eight bytes) and a major version `1`;
a response is limited to 10 000 bytes by the client. The calendars are free and need no
registration (the site's text).

**Config (non-secret)**: `calendars` (a list of HTTPS URLs; at least one; the console proposes the
public calendars that opentimestamps.org lists, with a button "Use the public calendars", which
fills them in; nothing is submitted until the operator saves), `minCalendars` (default 2, or the
number configured if smaller), `upgradeTimeoutDays` (default 7). No secret.

**Submit (steps):**

1. `d = Digest`. For each calendar in parallel: `POST <calendar>/digest` body `d`, timeout 20 s.
   Collect the fragments (each a tree of operations ending in pending attestations). We submit `d`
   itself: it is derived from a checkpoint that is public by design, so the usual privacy nonce is
   not needed (whether the reference client's `stamp` adds one is not verified here).
2. If at least `minCalendars` answered: the record is `submitted`, evidence `{"digest": d_hex,
   "calendars": [{"uri": "...", "fragment_b64": "..."}], "bitcoin": null}`; with fewer it stays
   `pending` and is retried (a calendar that answered is not asked again; its fragment is kept). (Replaced by correction 61 of section 14.)
3. **Upgrade**, a job every 10 minutes for the first 3 hours and then every hour, for each
   `submitted` record: for each pending attestation in each fragment, take the commitment (the
   digest at that point of the operation tree) and `GET <uri>/timestamp/<hex commitment>`.
   `200`: parse the returned fragment; if it contains a Bitcoin attestation, replace the pending
   attestation by it (the fragment grows by the operations that lead to the block's Merkle root);
   `404`: still pending, no error. When **any one** fragment contains a Bitcoin attestation the
   record is `anchored` with `EvidenceTime` = unknown until the header is known; kingresd does not
   fetch Bitcoin data (no node, no explorer is trusted by the server), so the time is stated as
   "included in Bitcoin block height H" and the verifier computes the block time from a header the
   **auditor** supplies. After `upgradeTimeoutDays` without a Bitcoin attestation the record is
   `expired` and the alert `ledgerAnchorFailed` fires. (Replaced by correction 60 of section 14.)
4. Export: the `.ots` detached timestamp file of the record is assembled from the fragments on
   demand (`downloadLedgerAnchor`) as `magic || version || file-hash-op || d || tree`, in the
   serialization of the reference library; it can be checked by the reference client too
   (`ots verify`).

**What the verifier checks later:** the operation tree from `d` to the Bitcoin attestation (the
operations are append, prepend, SHA-256, and a few others that the library defines; the verifier
implements the set it supports and rejects the rest as "unsupported operation", exit 1), then that
the resulting value equals the Merkle root of the Bitcoin block header the auditor supplies for
height H (`--btc-header H:<80-byte hex>`, from their own node or an explorer they trust), that the
header's hash meets its own `bits` target (a work check that does not prove it is on the best
chain), and prints the header's `time` field as "no later than". Without a header the result is
"commitment tree valid; Bitcoin block H claimed; not checked against Bitcoin" and exit status 3.
**Accuracy**: a Bitcoin block time is a miner-set field; an attestation proves existence before
that block, with an error of the order of hours (the architecture note says about two hours).

### 6.6 Failures, partial anchoring, backlog, retries, alerting

| Case | Behaviour |
|---|---|
| Target unreachable, timeout, 5xx, 429 | Record stays `pending`; `attempts + 1`; next attempt after `min(30 s * 2^attempts, 1 h)` with 20 percent jitter; the first success resets. `Retry-After` is honoured if larger (Replaced by correction 64 of section 14.) |
| Authentication or authorisation error (401, 403, TSA 2 "rejection" for a policy, bucket missing, lock not enabled) | `failed` after 3 attempts at 1, 5 and 15 minutes (it may be a propagation delay), alert `ledgerAnchorFailed` (critical) with the error text without any secret; needs an edit of the target or "Retry" (Replaced by correction 64 of section 14.) |
| WORM conflict (4) | `failed`, no retry, alert `ledgerAnchorConflict` (critical) |
| Target accepts but the store did not lock (3) | `failed`, alert `ledgerAnchorFailed` |
| OpenTimestamps: fewer than `minCalendars` answer | `pending`, retried; anchored only when the minimum answered; one calendar's failure does not block the others |
| OpenTimestamps never confirms | `expired` after `upgradeTimeoutDays`; alert; the operator can "Retry" which submits a fresh record for the latest checkpoint |
| Several targets, one fails | Independent records. The aggregate is `anchored` for the digest view when at least `anchor.minimumAnchored` (cluster setting, default 1) targets are `anchored`; the digest lists every target's status |
| Backlog (a target was down for days) | Only the newest checkpoint per `(target, log)` is `pending` or `submitted`; when a newer checkpoint is created, an older record still `pending` becomes `superseded`. A `submitted` OpenTimestamps record keeps upgrading until it ends. So the backlog is at most one `pending` and a few `submitted` per target and database |
| Anchor not older than `3 * anchor.intervalSeconds + 1 h` for a target | Alert `ledgerAnchorBehind`, `warning`; at `12 * ...` `critical` |
| `kingresd` restarts or the leader changes | The records are in the system database; the new leader's anchor job reads `pending` and `submitted` rows and continues; every Submit is idempotent per `(target, aggregate, size)` (WORM by the key and `If-None-Match`, TSA by being repeatable with a new nonce and the first stored token winning by the unique key, OpenTimestamps by resubmission being harmless) |

### 6.7 Background work: the scheduler, the term, and a leader change

`kingresd` has no recurring scheduler today. Phase 7 needs one, and phases 10 and 12 want one for
rebalancing and partition policies. **The first of those phases to be built creates
`internal/scheduler`; the others use it**; the interface (the part this phase needs) is:

```go
type Job struct {
    Name     string
    Every    time.Duration
    Jitter   float64                      // fraction of Every
    Run      func(ctx context.Context, term Term) error
}
type Term int64  // the leader's term, 6.7 below
```

The scheduler runs on **the manager leader only**. It subscribes to the leader view of `internal/leader`
(which already follows the catalog every 2 seconds). On becoming leader it waits one `lease-margin`
(so that the previous leader's last act is over, the timeline of the README "Why this is safe"),
increments the **term** in one transaction (`UPDATE kingres_cp.ledger_cluster SET term = term + 1
RETURNING term`), and starts its jobs with a context that is cancelled when leadership is lost.
**Every write of a job is in a transaction that begins with `SELECT 1 FROM kingres_cp.ledger_cluster
WHERE term = $mine FOR SHARE`** and fails the job if no row is returned, so a deposed leader that
wakes up cannot write (the same idea as the task engine's `lease_epoch`; step 4 of the plan
reads `internal/task/pgstore.go` and `internal/fence` and uses their helper where one fits).
Calls to the other servers carry the fencing token as the existing peer client does.

Jobs of this phase (names are the `Job.Name`):

| Job | Every | Does |
|---|---|---|
| `ledger.round` | `publication.intervalSeconds` (60 s; 10 to 3600), one pass over all databases with ledger tables | 5.4 steps 1 to 4 |
| `ledger.digest` | per database `digestIntervalSeconds` (the existing setting; 60 to 86400) | 5.4 with `op: digest`; this finally gives `digestIntervalSeconds` its effect |
| `ledger.anchor.submit` | 30 s | records in state `pending` whose `next_attempt_at` has passed |
| `ledger.anchor.upgrade` | 60 s (each record has its own `next_check`) | OpenTimestamps upgrade |
| `ledger.anchor.enqueue` | `anchor.intervalSeconds` | creates the records of 6.2 for the newest aggregate checkpoints |
| `ledger.anchor.credentials` | 60 s | brings the credentials files of the holders to the state of the targets (correction 232) |
| `ledger.publication.audit` | 24 h and on request | re-reads every chain from block 0 and compares with the published leaves (4.7, 1) |
| `ledger.monitor` | 10 s | the alert reconciliation of 6.9 (no write unless an alert changes) |

Tasks (the engine of `internal/task`) are used for the operator-started, resumable, multi-step
actions: `ledger.key.create`, `ledger.key.rotate`, `ledger.key.distribute`, `ledger.anchor.test`
is not a task (synchronous), `ledger.bundle.create`, `ledger.epoch.start` (9.4), and the existing
`ledger.verify`. Jobs are loops, not tasks, because a task per minute per database would fill the
task table (`kingres_cp.task`) with rows nobody reads.

### 6.8 Configuration and credentials

- **Where the settings are**: cluster settings `anchor.intervalSeconds`, `anchor.minimumAnchored`,
  `publication.*` and the target rows are in `kingres_cp` (replicated with the map group); they are
  not PostgreSQL settings and not in `postgresql.conf`.
- **Where the credentials are**: not in `kingres_cp`, not in a task row, not in any response. File
  `<ledger-keys-dir>/anchor/<target id>.json`, mode 0600, the same envelope as the signing key
  (3.2.2: clear without a passphrase file, sealed with it), distributed to the key holders
  by `ledger.key.distribute`. A target may instead name `credentialsPath`, a file that already exists
  on every holder (the operator's own secret mount); the content is read at each use. The API takes a
  write-only `credentials` object on create and update and answers `credentialsSet: true`; the audit
  log records "credentials changed" and nothing else (`config-phase13.md` already keeps secrets out of
  the log). The error text of a failed submission is passed through a filter that removes the
  credential values before it is stored.
- **SigV4**: `kingresd` implements AWS Signature Version 4 for the five S3 calls it needs
  (`PutObject`, `GetObject`, `HeadObject`, `GetBucketVersioning`, `GetObjectLockConfiguration`) in
  about 250 lines of standard library, tested with the published SigV4 test vectors and against a
  MinIO started by the test script when it is present. An AWS SDK is not added: it is a large
  dependency tree for five calls (`DEPENDENCIES.md` lists what the control plane carries today).
- **The Test button** (`testLedgerAnchorTarget`, synchronous, 30 s limit, role `kingres_admin`, no
  state changed except one probe object): for **WORM**: bucket reachable, versioning `Enabled`, object
  lock `Enabled`, default retention (shown), a probe `PutObject` of an empty body at
  `<keyPrefix>/_probe/<random>` **without** lock headers and with `If-None-Match: *` (tells whether
  the store takes the header and the checksum header), then `HeadObject` of it; the probe object is
  zero bytes and, where the bucket has a default retention in compliance mode, cannot be deleted before
  it ends (the dialog says so before the click); for **TSA**: a real request for the SHA-256 of an empty
  string is sent and the token fully verified against `caBundlePath`, giving the `genTime`, the policy
  and the certificate subject; for **OpenTimestamps**: a `POST /digest` of 32 random bytes to each
  calendar (nothing is kept), reporting which answered. The result is a list of named checks, each
  `ok`, `warning` or `failed`, with the text of the failure; it is stored as `last_test` of the
  target and shown.

### 6.9 The absence of anchors is visible, and the alerts

With no enabled target: the Ledger page's "Anchoring" tab and the Dashboard card say
**"Not anchored: this ledger can be rewritten undetected by anyone who controls this cluster and its
backups. Add a target."** with a button, and the digest list shows "not anchored" in the Anchors
column. An alert of kind `ledgerNotAnchored` (severity `info`) stays open while a database has
ledger tables and no target is enabled; the cluster setting `anchor.required` (default false)
raises it to `warning`. Nothing is blocked either way.

New alert kinds (the contract's `AlertKind`; subjects follow `AlertSubject`):

| Kind | Severity | Raised when | Subject |
|---|---|---|---|
| `ledgerNotAnchored` | info (warning if `anchor.required`) | a database has ledger tables and no enabled anchor target | `database:<name>` |
| `ledgerAnchorBehind` | warning, critical | 6.6 | `target:<id>` |
| `ledgerAnchorFailed` | critical | a record is `failed` or `expired` | `target:<id>` |
| `ledgerAnchorConflict` | critical | WORM conflict (6.3, 4) | `database:<name>|target:<id>` |
| `ledgerChainDiverged` | critical | 4.7 | `database:<name>|group:<n>` |
| `ledgerGroupUnread` | warning, critical | 5.5 | `database:<name>|group:<n>` |
| `ledgerPublicationBehind` | warning | the newest signed checkpoint of a chain is older than `5 * publication.intervalSeconds` while its head has advanced | `database:<name>|group:<n>` |
| `ledgerSignerUnavailable` | critical | no checkpoint could be signed for 2 rounds | `cluster` |
| `ledgerDigestOverdue` | warning | no digest within `2 * digestIntervalSeconds` (the scheduler's own check) | `database:<name>` |

The existing `ledgerPendingTooOld` and `ledgerVerificationFailed` stay.

## 7. The standalone verifier

`kingres/verifier` keeps its rules: standard library only, one binary `kingres-verify`, an
independent second implementation written from the documents (never from the server's or
`kingresd`'s code), no network unless a flag asks for it, and every result says what it did not
prove. The existing subcommands (`proof` against a digest, `digest-chain`, `blocks`, `row-hash`,
`version`) and their exit statuses 0, 1, 2 are unchanged.

### 7.1 What it verifies at the end of phase 7, without the database

| Check | Input | Subcommand |
|---|---|---|
| A signed checkpoint: signature under a pinned key, origin, size, root | a checkpoint note, `trust.json` | `checkpoint` |
| Consistency between two checkpoints of one log (RFC 9162 2.1.4.2), computed from tiles | two checkpoints, tiles | `consistency` |
| A block is in a chain log (inclusion, 2.1.3.2) and a row is in the block (the existing format-1 steps) | a proof, tiles or a bundle | `proof --bundle` |
| An aggregate checkpoint, its leaf, its members against the chain logs, continuity with an earlier aggregate | aggregate tiles, chain tiles | `aggregate` |
| Anchors of an aggregate checkpoint: WORM, RFC 3161, OpenTimestamps | evidence files, optional network | `anchors` |
| Client attestations: signature, key pinned, key not shown revoked at that block | proofs of the row and of the attestation row | `attestation` |
| Everything applicable in one run | a bundle | `bundle verify` |
| A rollback or a rewrite since the last look | a state file, the publication | `monitor` |
| Mirror a publication to a directory | an HTTP base URL | `bundle fetch` |
| Trust file maintenance | public key, events | `trust` |

### 7.2 The trust file

`trust.json` (UTF-8, strict JSON like every input of the verifier: duplicate keys rejected):

```json
{"format": 1,
 "origin_prefix": "ledger.example.com/kingres",
 "keys": [{"name": "2026", "public_key": "<base64, 32 bytes>", "fingerprint": "<hex SHA-256 of the key>",
           "not_before": "2026-12-01T00:00:00Z", "not_after": null}],
 "tsa_roots": ["<PEM text>", "..."],
 "client_keys": [{"key_id": "<hex 16 bytes>", "public_key": "<base64>", "label": "teller-7"}],
 "witnesses": []}
```

`origin_prefix` and at least one key are required for any signature check. `fingerprint` must
equal the SHA-256 of `public_key` (a typo is caught). `tsa_roots` (PEM certificates) are needed
only for TSA anchors, `client_keys` only for attestations. `trust init` writes the file from a key
the user supplies and refuses to run without `--fingerprint` (the user types or pastes the value
that the console shows; the tool compares). `trust follow` reads the `events/` of a publication,
verifies each `key-added`, `key-retired` and `key-revoked` note under the keys already trusted, and
writes a new file (never the same file; the old one stays). Key ids of signatures are derived as the
signed-note specification says, so any key whose name is `origin` or `origin_prefix` and whose id
matches is tried; a signature line that matches no pinned key is ignored (a checkpoint needs one
valid signature of a pinned, non-revoked key; during an overlap there are two).

### 7.3 The bundle

A directory or `.tar.gz` of the layout of 4.6 limited to what is asked for, plus `bundle.json`:

```json
{"format": 1, "created_at": "2026-12-01T10:05:00Z", "cluster_id": "<hex>",
 "origin_prefix": "ledger.example.com/kingres",
 "logs": [{"kind": "chain", "origin": "<origin>", "chain_id": "<hex>", "epoch": 1,
           "checkpoint": "c/<hex>/e1/checkpoint", "size": 4211},
          {"kind": "aggregate", "origin": "<origin>", "checkpoint": "a/<hex>/checkpoint", "size": 17}],
 "anchors": ["a/<hex>/anchors/17/2.json"],
 "proofs": [{"file": "proofs/acc5.json", "attestation_file": "proofs/acc5.att.json"}],
 "notes": "free text, at most 500 characters"}
```

A bundle holds hashes and, for requested proofs, the proof documents (which contain `row_hash`,
`user_name`, table id; the console warns before it creates a bundle with proofs, because those
fields are what the privilege model of the README keeps from PUBLIC). `bundle.json` is a convenience
index and is **not trusted**: every fact is re-derived from the signed notes and the tiles.

### 7.4 Command line, exit codes, output

Common flags: `--source DIR|FILE.tar.gz|https://...` (default: the current directory), `--trust
trust.json` (required for anything with a signature), `--json`, `--now RFC3339` (for validity
windows; default the system clock, printed). Network is used only for `--source https://...`,
`--worm-url`, `monitor` and `bundle fetch`; never for a bundle in a directory. Each subcommand
prints, as the existing ones do, one line per step (`ok  step-name  text`) and a final
`VERIFIED:`, `FAILED:` or `INCOMPLETE:` line, followed by the "not proved" statements.

| Status | Meaning |
|---|---|
| 0 | everything that was asked for was verified; `VERIFIED` is printed |
| 1 | a verification failed (the step is named); `VERIFIED` is never printed; a failure wins over an incomplete check |
| 2 | usage or input error: bad flags, unreadable file, malformed trusted input (trust file, checkpoint note given as the anchor of trust) |
| 3 | **new**: nothing failed, but something that was asked for could not be checked with the inputs and flags given (an OpenTimestamps proof without a Bitcoin header, a WORM record not re-fetched, a pending proof, an anchor required by `--require` that is absent); `INCOMPLETE` is printed and the missing checks are listed |

Subcommands (flags in addition to the common ones):

```
kingres-verify checkpoint  --log chain:<chain_id>[:e<N>]|aggregate:<aggregate_id>   [--min-size N]
kingres-verify consistency --log <as above> --old <note file>|<size>:<root b64> [--new <note file>]
kingres-verify proof       --proof proof.json (--digest digest.json | --bundle B --log-leg chain|aggregate)
                           [--table-id N --row-hash HEX --op I|D|any]
kingres-verify aggregate   --aggregate-id <hex> [--previous <note file>] [--chains-from <source>]
kingres-verify anchors     --aggregate-id <hex> [--size N] [--require worm,tsa,ots|any] [--worm-url URL]
                           [--btc-header HEIGHT:HEX80 ...]
kingres-verify attestation --proof row.json --attestation row.att.json [--client-key HEX] [--revocations dir]
kingres-verify bundle verify  B   [--require-anchors N]
kingres-verify bundle fetch   --source URL --out DIR [--log ...] [--since-size N]
kingres-verify monitor     --source URL --state state.json [--once]
kingres-verify trust init|show|follow
kingres-verify release verify --sums SHA256SUMS --note SHA256SUMS.note --key BASE64   (7.7)
```

**What `proof --bundle --log-leg chain` does** (the new verification of a row, step numbers continue
the README's list): 0 to 3 as in the README (the proof's `chain` may be empty); 4' recompute the block hash
from the proof's header, form the 87-byte leaf data (4.3), check that `SHA-256(leaf data)` equals the
block hash; compute the leaf hash and verify the inclusion path to the root of the chain checkpoint
at size `N` (RFC 9162 2.1.3.2; the path is taken from the bundle's tiles, the verifier computes
it); 5' verify the chain checkpoint: signature under a pinned key, or, with `--log-leg aggregate`,
verify the aggregate checkpoint (signature, then anchors with `anchors`), find the member of the
chain, check that its `block_hash` and `log_root` equal the ones used. The result says which root of
trust ended the chain: *signature of key X*, or *anchored aggregate (WORM, TSA genTime T, ...)*.

**What `attestation` does**: the attestation row is a row of the ledger table
`kingres.ledger_attestation`, so it has its own proof. Steps: (1) verify the row proof as above
(op `I` or `D`); (2) recompute the attestation row hash from the columns file the bundle carries
and check the attestation row's inclusion proof (same machinery, `table_id` of the attestation
table); (3) check its `ledger_txid` and `seq` name the row proof's entry; (4) build the message of
3.7 with the proof's `chain_id`, `table_id`, `op`, `row_hash`, the attestation's `key_id` and
`nonce`, and verify the Ed25519 signature under the public key pinned in `client_keys` (a key not
pinned fails: the server's registry is not trusted); (5) if revocation rows were supplied
(`--revocations`, each with a proof), a revocation of that key in a block at or before the
attestation's block fails the check. **The verifier cannot prove that no revocation exists**: an
inclusion proof shows presence, not absence. The result therefore says "no revocation shown in the
evidence given", and the document says that completeness of the list is a claim of the server unless
the auditor has read the whole table through `ledger_verify` on a copy. `--require-attestation`
makes a row proof without an attestation fail; it cannot see rows that were never presented.

### 7.5 `monitor`: a third party that notices a rollback

`monitor --once` fetches the checkpoint of every log named in `--state` (first run: of the logs in
`logs.json` or given with `--log`), verifies the signature, fetches the consistency proof tiles
from the stored size to the new size and verifies it (2.1.4.2), checks the entries it can see (`prev_hash`
links, `block_hash = SHA-256(entry)`, times not going back) and rewrites the state (checkpoint
notes, never anything else). It fails with status 1 when a checkpoint is smaller than a stored one
(rollback), is inconsistent with it (rewrite), or has a bad signature; status 3 when the
publication is unreachable. Run from cron or a timer on a machine that is not part of the cluster,
it turns "a copy of an old checkpoint exists elsewhere" into a standing control (2.2). It sees one
view; a **split view** (the server shows two monitors different histories) needs witnesses (4.8,
future work).

### 7.6 Structure, tests, vectors

New internal packages, each small and with its own tests: `signednote` (parse, key id, verify),
`tlog` (tile path encoding and decoding, hash reading, inclusion and consistency, partial tiles,
RFC 9162 verification), `checkpoint`, `aggregate`, `anchor/worm`, `anchor/tsa` (own `encoding/asn1`
CMS), `anchor/ots` (operations, attestations, `.ots` parser), `attest`, `trust`, `bundle`, `monitor`,
`release`. `go.mod` keeps no `require` line.

Test vectors (committed under `testdata/`): (1) RFC 6962 trees of sizes 1 to 600 over generated leaves
with roots computed by a throwaway script that uses only SHA-256 (committed with the vectors), inclusion
and consistency proofs for every pair of a set of sizes, and **negative vectors** for each
(wrong index, wrong size, a path of the right length for another tree, a truncated path);
(2) tile paths: the example of tlog-tiles (`x001/x234/067`), partial widths for sizes 255 to 66000;
(3) signed notes: the examples of the signed-note specification as far as it gives them, otherwise
notes made with an independent tool; wrong key id, two lines same name and id (rejected), non-UTF-8,
control characters; (4) aggregate leaves with 1, 2, 512 members, unsorted members (rejected),
duplicate members, a retired chain, a stale and a diverged flag, a leaf over the `u16` limit; (5) WORM
evidence of a real MinIO run; (6) TSA tokens from at least two public authorities and from a local
test authority, with a wrong imprint, wrong nonce, an expired certificate at `genTime`, a certificate
without the critical extended key usage; (7) OpenTimestamps files produced by the reference client for
a known digest, pending and upgraded, with a header for the block; (8) client attestations, with a
revoked key, a nonce replay, another chain's id; (9) a **cross-check against `kingresd`**: the
fixture generator (a test of `kingresd`, like `scripts/gen-fixtures.sh` today) writes a publication,
an aggregate, anchors against a local test store, and the verifier's tests read them. Fuzz targets
for every parser (`FuzzParseNote`, `FuzzParseTile`, `FuzzParseAggregate`, `FuzzParseOTS`,
`FuzzParseTSAToken`).

### 7.7 Reproducible builds, and how a third party gets the verifier

- **Build**: `CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags "-s -w -buildid=
  -X main.version=<tag>" ./cmd/kingres-verify`, with the Go toolchain version recorded in `go.mod`
  (`toolchain` line, added by the release step). No third-party module, so the build depends on the
  Go release only.
- **Reproduction check**: `kingres/verifier/scripts/repro-check.sh` builds twice from two checkouts at
  different paths and compares SHA-256 (checks run locally; Actions is not used,
  the project does not use GitHub Actions).
- **Release files** (made with `scripts/release.sh`, at the first release only; no tag exists before
  it): `kingres-verify-<os>-<arch>[.exe]` for linux/amd64, linux/arm64, darwin/arm64, windows/amd64,
  `SHA256SUMS`, and `SHA256SUMS.note`, a **signed note** over the sums text signed with the project's
  release key (Ed25519, key name `kingres-verify-release`), whose public key is printed in the
  README of the verifier and in the documentation. `kingres-verify release verify` checks the sums
  note with the key the user passes. Where the release key is kept and who holds it is a decision of
  the project owner (marked in section 12).
- **How a third party gets it**: from the source repository at the release tag (build it with the
  command above and compare with the release sums), or from the release files; **not from the
  cluster it is meant to check**. The package that installs KingresDB also puts a copy of
  `kingres-verify` on the server for convenience; the console says in its text that an auditor must
  not rely on it.

## 8. Interfaces

### 8.1 `kingres_ledger` 0.3 (SQL and C)

One source for PostgreSQL 18 and 19 ([ADR 0023](../adr/0023-one-extension-source-for-several-majors.md)):
no `PG_VERSION_NUM` outside `src/pg_compat.h`. Files: `kingres_ledger--0.3.sql`,
`kingres_ledger--0.2--0.3.sql`, `default_version = '0.3'`, `DATA` in `Makefile` and
`meson.build`, new `src/sign.c` (attestation, OpenSSL), changes in `capture.c` (the `required`
policy), `kingres_ledger.h` (new SQLSTATEs `KL010` to `KL014`), `protect.c` (the new tables are ledger
tables; the hook's scoped rule lets the install script create them; `ledger_table` gets its sixth
column).

| Object | Kind | Purpose | Readable or executable by |
|---|---|---|---|
| `ledger_block_preimage(hash_version smallint, height bigint, prev_hash bytea, tx_merkle_root bytea, tx_count int, closed_at timestamptz) returns bytea` | C, IMMUTABLE | The 87 bytes of 4.3 (the same code `ledger_block_hash` hashes) | PUBLIC |
| `ledger_head() returns table (chain_id bytea, height bigint, block_hash bytea, closed_at_us bigint)` | SQL, STABLE, `SECURITY DEFINER` | The latest block without building one and without recording a digest; no row for an empty ledger | the service role (granted by `kingresd`); not PUBLIC |
| `ledger_blocks_entries(from_height bigint, max_count int) returns table (height bigint, entry bytea)` | SQL, STABLE | The preimages of the blocks from `from_height`, at most `least(max_count, 10000)`; each entry is checked inside against `block_hash` (an error `XX001` if a stored block does not match its own hash, so the publisher never signs a corrupt chain) | the service role |
| `ledger_client_key_register`, `ledger_client_key_revoke`, `ledger_attest`, `ledger_attestation_message`, `ledger_set_signature_policy` | 3.7 | client signatures | 3.7 |
| tables `ledger_client_key`, `ledger_client_key_revocation`, `ledger_attestation` | ledger tables | 3.7 | the same readability rule as `ledger_tx` (not PUBLIC; the service role and administrators) |
| column `ledger_table.signature_policy` | catalog | 3.7 | PUBLIC reads `ledger_table` already |
| `ledger_verify` additions | checks | `attestation_duplicate`, `unattested_entry`, `client_key_unknown` (an attestation names a key with no registration) | as `ledger_verify` |
| `ledger_status` additions | view columns appended at the end | `client_keys_active bigint`, `attestations bigint`, `required_tables bigint` | not PUBLIC, as today |

Not added: any function that signs, any GUC for keys, any network call. **No change to hash format
version 1**; the existing format tests pass unmodified (a requirement, as for 0.2). **Upgrade** of a
0.2 database: the script adds the column and tables, registers them (the ledger gets new
`ledger_table` rows and its first entries are the registrations of nothing; no entry is written by
the upgrade itself), keeps every existing block, digest and proof valid; test `007_upgrade.pl` style,
`011_upgrade_0_3.pl`. `pg_dump` and restore keep the limit documented in the README (empty ledger on
restore).

### 8.2 `kingresd` packages and configuration

New packages: `internal/ledgerkeys` (3.2), `internal/ledgerlog` (tile builder, frontier, checkpoint
text and signing, aggregate leaf codec), `internal/ledgerpub` (storage in `kingres_cp`, the HTTP
listener, mirrors), `internal/ledgerround` (the collector of 5.4), `internal/anchor` (+ `worm`, `tsa`,
`ots`, `sigv4`), `internal/scheduler` (6.7). Migration `internal/sysdb/migrations/0008_ledger_phase7.sql`
(the number is the next free one at the time it is written; 0007 is phase 13), tables in
`kingres_cp`:

| Table | Columns (names as used above) |
|---|---|
| `ledger_cluster` | singleton: `cluster_id`, `term`, `publication_enabled`, `origin_prefix`, `public_url`, `publication_interval_seconds`, `anchor_interval_seconds`, `anchor_minimum`, `anchor_required`, `first_checkpoint_at` |
| `ledger_signing_key` | `fingerprint` (pk), `public_key`, `state`, `mode`, `created_at`, `activated_at`, `retired_at`, `revoked_at`, `compromised_since`, `reason` |
| `ledger_log` | `log_id` (pk), `kind`, `database_name`, `shard_group_id`, `chain_id`, `epoch`, `origin` (unique), `state` (`active`, `diverged`, `closed`), `size`, `root`, `frontier`, `head_block_hash`, `updated_at` |
| `ledger_tile` | `(log_id, level, idx, width)` pk, `data`, `created_at` (level -1 stands for entry bundles) |
| `ledger_checkpoint` | `(log_id, size)` pk, `root`, `note`, `signed_at`, `anchored` |
| `ledger_event` | `seq` pk, `note`, `created_at` |
| `ledger_anchor_target` | `id`, `kind`, `name` unique, `enabled`, `config jsonb`, `credentials_set`, `last_test jsonb`, `created_by`, `created_at`, `updated_at` |
| `ledger_anchor` | 6.2 |
| `ledger_digest_view` | `(database_name, digest_id)` linking a digest to its aggregate size (5.4 step 5) |
| `ledger_bundle` | `id`, `database_name`, `created_by`, `task_id`, `file`, `size`, `expires_at` |

Flags and environment (each the pair `--x` / `KINGRES_X`, as in the README table of settings):
`--ledger-keys-dir`, `--ledger-key-passphrase-file`, `--ledger-signer-command`,
`--ledger-publication-listen`, `--ledger-publication-tls-cert`, `--ledger-publication-tls-key`.
The settings that the console changes (intervals, prefix) are in the table, not flags.

### 8.3 The internal API (new calls; internal protocol version stays 1)

Closed sets, no SQL text, the database name validated as an identifier (`sqlx.NewIdent`) as the
existing calls do. Authorization table rows (`internal-api.md` style):

| Call | Caller | Does |
|---|---|---|
| `POST /internal/v1/ledger/read` | leader | `{"op": "head"|"digest"|"entries"|"status", "database": "...", "from": n, "max": n}`; runs the matching function (`ledger_head`, `ledger_generate_digest`, `ledger_blocks_entries`, `ledger_status`) on this server's local primary instance **only if this server runs the primary of a group that holds the database** (else `409 not_primary`), as the service role |
| `POST /internal/v1/ledger/key/install`, `/key/remove` | leader | install or delete the signing key file or the credentials file of a target (3.2.2, 6.8); the body carries the sealed file as the leader holds it, never a passphrase |
| `POST /internal/v1/ledger/publication/get` | member, answered only by the leader | `{"path": "c/.../checkpoint"}`: the stored object or 404, for a follower's listener |
| `POST /internal/v1/ledger/upgrade` | leader | run `ALTER EXTENSION kingres_ledger UPDATE` in one database on this server's primary instance (9.5); `409 not_primary` as above |
| `POST /internal/v1/ledger/anchor/probe` | leader | run the Test of a target **on this server** (used when the target is only reachable from another server; optional, step 24) |

### 8.4 REST operations (contract revision "0.9.0", a placeholder number like the others)

Style as the existing ledger operations: tag `Ledger`, camel-case `operationId`, `x-required-role`,
tasks answered `202 AcceptedTask`, problems as RFC 9457 with the stable `code`. Everything is additive.

| operationId | Method and path | Role | Request (fields) | Response | Errors beyond the common ones |
|---|---|---|---|---|---|
| `getLedgerSettings` | `GET /ledger/settings` | readonly | | `LedgerSettingsCluster`: `publication {enabled, originPrefix, publicUrl, intervalSeconds, originPrefixLocked}`, `anchor {intervalSeconds, minimumAnchored, required}` | |
| `updateLedgerSettings` | `PUT /ledger/settings` | admin | same shape (all optional) | the settings | 409 `origin_prefix_locked`; 400 `validation_failed` (ranges: publication 10 to 3600, anchor 60 to 86400, minimum 1 to 8) |
| `getLedgerSigning` | `GET /ledger/signing` | readonly | | `LedgerSigning`: `clusterId`, `mode` (`file`, `command`), `passphraseProtected`, `holders[]` (server ids), `keys[]` (`fingerprint`, `publicKey`, `state`, `createdAt`, `activatedAt`, `retiredAt`, `revokedAt`, `compromisedSince`), `events[]` (seq, type, time) | |
| `createLedgerSigningKey` | `POST /ledger/signing/keys` | admin | `{overlapDays?: 1..365, publicKey?: base64}` (`publicKey` only for mode `command`) | `202` task (first key: create and distribute; later: rotation) | 409 `operation_in_progress`; 409 `signing_key_missing` is for rotation without an active key |
| `retireLedgerSigningKey` | `POST /ledger/signing/keys/{fingerprint}/retire` | admin | `{}` | `202` task | 409 `last_signing_key`; 404 |
| `revokeLedgerSigningKey` | `POST /ledger/signing/keys/{fingerprint}/revoke` | admin | `{compromisedSince: date-time, reason: string(<=200), confirm: <the fingerprint>}` | `202` task | 400 `validation_failed` (confirm differs); 404 |
| `getLedgerTrustFile` | `GET /ledger/signing/trust-file` | readonly | | `application/json`, `Content-Disposition: attachment; filename="trust.json"`, the public parts of 7.2 | 409 `signing_key_missing` |
| `getLedgerPublication` | `GET /ledger/publication` | readonly | | `LedgerPublication`: `enabled`, `listener {address, tls, running}`, `logs[]` (`kind`, `origin`, `size`, `lastCheckpointAt`, `state`), `lastCheckpointAt`, `checks` | |
| `listLedgerAnchorTargets` | `GET /ledger/anchor-targets` | readonly | | `{items: LedgerAnchorTarget[]}`: `id`, `kind`, `name`, `enabled`, `config` (non-secret), `credentialsSet`, `lastTest`, `newestAnchoredAt`, `state` | |
| `createLedgerAnchorTarget` | `POST /ledger/anchor-targets` | admin | `{kind, name, enabled, config, credentials?}` (shapes per kind in 6.3 to 6.5) | `201 LedgerAnchorTarget` | 400 `validation_failed`; 409 `already_exists` |
| `getLedgerAnchorTarget` | `GET /ledger/anchor-targets/{targetId}` | readonly | | one target | 404 |
| `updateLedgerAnchorTarget` | `PATCH /ledger/anchor-targets/{targetId}` | admin | any of `name`, `enabled`, `config`, `credentials` (replaces) | the target | 400; 404; 409 `immutable_field` for `kind` |
| `deleteLedgerAnchorTarget` | `DELETE /ledger/anchor-targets/{targetId}` | admin | | `204` (its anchor records stay, marked with the target's name) | 404 |
| `testLedgerAnchorTarget` | `POST /ledger/anchor-targets/{targetId}/test` | admin | `{}` | `LedgerAnchorTest`: `checks[]` (`name`, `status` ok/warning/failed, `detail`), `ok` | 404; 504-like failures are in the result, not an error |
| `getLedgerAggregate` | `GET /databases/{db}/ledger/aggregate` | readonly | `?size=` (default latest) | `LedgerAggregate`: `size`, `root`, `checkpoint` (the note), `generatedAt`, `members[]` (`shardGroupId`, `chainId`, `epoch`, `height`, `blockHash`, `logRoot`, `flags`), `complete`, `uncoveredShardGroupIds`, `anchors[]`, `statement` (the 5.6 text) | 404; 409 `ledger_not_published` |
| `listLedgerAggregates` | `GET /databases/{db}/ledger/aggregates` | readonly | `limit`, `cursor` | page of the above without members | 400, 404 |
| `listLedgerAnchors` | `GET /databases/{db}/ledger/anchors` | readonly | `targetId?`, `state?`, `limit`, `cursor` | page of `LedgerAnchor` (`id`, `targetId`, `targetName`, `kind`, `aggregateSize`, `state`, `attempts`, `nextAttemptAt`, `anchoredAt`, `evidenceTime`, `lastError`) | |
| `anchorLedgerNow` | `POST /databases/{db}/ledger/anchors` | ops | `{}` | `202` with the created records | 409 `ledger_not_published`; 409 if no target enabled |
| `retryLedgerAnchor` | `POST /databases/{db}/ledger/anchors/{anchorId}/retry` | ops | `{}` | the record | 409 (state not `failed`, `expired`); 404 |
| `downloadLedgerAnchor` | `GET /databases/{db}/ledger/anchors/{anchorId}/evidence` | readonly | `?format=json|ots|tsr` | the evidence file (6.2); `ots` and `tsr` only for those kinds | 404; 422 for a format the kind lacks |
| `createLedgerBundle` | `POST /databases/{db}/ledger/bundles` | ops | `{logs?: string[], sinceSize?, includeAnchors: bool, proofs?: InclusionProofRequest[]}` | `202` task; the task's result names the bundle | 409 `ledger_not_published` |
| `getLedgerBundle` | `GET /databases/{db}/ledger/bundles/{bundleId}` | ops | | `application/gzip`, kept 24 hours | 404, 410 |
| `listLedgerClientKeys` | `GET /databases/{db}/ledger/client-keys` | readonly | | `{items: {keyId, publicKey, roleName, label, registeredBy, registeredAt, revokedAt, revocationReason}[]}` | 422 if `kingres_ledger` < 0.3 |
| `registerLedgerClientKey` | `POST /databases/{db}/ledger/client-keys` | admin | `{publicKey (base64, 32 bytes), roleName?, label}` | `201` the key | 400; 409 `already_exists`; 422 (server without OpenSSL: `unsupported_operation`) |
| `revokeLedgerClientKey` | `POST /databases/{db}/ledger/client-keys/{keyId}/revoke` | admin | `{reason}` | the key | 404; 409 (already revoked) |
| `setTableSignaturePolicy` | `PUT /databases/{db}/schemas/{s}/tables/{t}/ledger/signature-policy` | admin | `{policy: "required"}` | the table | 409 (already required, cannot go back); 422 (not a ledger table) |
| `upgradeLedgerExtension` | `POST /databases/{db}/ledger/upgrade` | admin | `{}` | `202` task `ledger.upgrade` (9.5) | 409 `operation_in_progress`; a group that cannot be upgraded fails the task, whose error names the server |
| `acknowledgeLedgerDivergence` | `POST /databases/{db}/ledger/chains/{shardGroupId}/acknowledge` | admin | `{kind: restore|failoverLoss|other, reason, confirm: <database name>}` | `202` task `ledger.epoch.start` | 409 `chain_not_diverged`; 400 |

**Changes to existing operations and types**: `generateLedgerDigest` runs a full round (all groups,
5.4) and returns every chain, `aggregateHash`, the new `aggregateSize` and the anchors (it stays
synchronous and answers 201 as today, but may now take up to the group timeout of 15 s; `503 service_unavailable`
if no group answers); `startLedgerVerification` gains `checks` (array of `chain`, `publication`,
`anchors`; default `["chain"]`) and, for `publication`, re-reads the chains from block 0 and compares
with the published leaves, and verifies every stored checkpoint signature and consistency
between consecutive stored checkpoints; `LedgerStatus` gains `publication` (`enabled`, `lastCheckpointAt`,
`state`) and `anchoring` (`targets`, `newestAnchoredAt`, `state`: `none`, `ok`, `behind`, `failed`);
`LedgerBlock.signed` is redefined (3.1); `LedgerDigest.anchors[].target` still lists `worm`,
`openTimestamps`, `tsa`; `ethereum` stays in the enum and is never produced (removing it would be a
breaking change); `AlertKind` gains the nine kinds of 6.9; `AuditAction` gains the operation ids
above (it lists operation ids). The README of the contract and `check-contract.mjs` are updated by the
contract step. New problem codes: `ledger_not_published`, `origin_prefix_locked`, `signing_key_missing`,
`last_signing_key`, `chain_not_diverged`.

### 8.5 The console

Every capability above has a control. The Ledger page (`/ledger`, `LedgerPage.vue`) keeps what it has
(status, verification, proofs) and becomes a page with a tab bar (`TabBar.vue`, as the Configuration
page uses it); each tab is its own component under `components/ledger/` so the page stays reviewable.

| Tab | Shows | Controls (each calls an operation) |
|---|---|---|
| **Overview** (existing) | per-chain status with coverage text; **new**: signed or not, size of the chain log, last checkpoint time | Generate digest; Start verification (checkbox group: chain, publication, anchors); Upgrade ledger extension (shown when a group reports 0.2) |
| **Digests** (existing, extended) | digest list with the **Aggregate hash** and **Anchors** columns now filled; an aggregate dialog: members table, the 5.6 statement, flags (stale, diverged, retired), checkpoint note with a copy button | Generate digest; download a chain document (exists); Download aggregate checkpoint; Create bundle (dialog: logs, since, include anchors, include proofs with the warning of 7.3), then the download of the finished bundle |
| **Anchoring** | the list of targets with kind, state, newest anchored time ("age of the window"), last test; the anchor records of the selected database with status per target; the "Not anchored" notice of 6.9; the advice "one target is one point of failure" | Add target (dialog per kind, 6.3 to 6.5, credentials fields that are write-only and cleared after save), Edit, Enable or disable, Delete (type the name), **Test** (shows the named checks), Anchor now, Retry a failed record, Download evidence (json, ots, tsr), compliance mode needs the type-"compliance" confirmation |
| **Signing** | cluster id; mode (file or command) and "key not protected by a passphrase" warning; keys with state and dates; holders; the events list; the **fingerprint** in groups of four characters with the text of 2.3 | Create first key; Rotate key (overlap days); Retire now; Revoke key (compromised since, reason, type the fingerprint); **Download trust file**; Copy public key |
| **Publication** | enabled, listener address, TLS or "plain HTTP" warning, the logs with size and last checkpoint, the URL prefixes to give to auditors, the 4.9 text about what is exposed, the verifier text of 7.7 | Enable or disable; edit origin prefix (locked once a checkpoint exists, with the explanation), public URL, interval; Create bundle; "Audit publication now" (starts verification with `publication`) |
| **Client keys** (per database, shown when `kingres_ledger` is 0.3) | registered keys with role binding, label, revoked state; the tables with their signature policy | Register key; Revoke key; Set policy `required` for a table (also reachable from the table's detail on the Databases page). `EnableLedgerDialog` offers no policy at enable time: the policy is set afterwards, because a table that starts `required` could not be loaded without attestations |
| **Chains** (inside Overview, only when a chain is `diverged`) | what diverged, the old and new heights, the 9.4 text | Acknowledge divergence (type the database name, kind, reason) |

**Dashboard card** "Ledger integrity" (`DashboardPage.vue`, next to the existing cards): one line of
state (`Off` when no database uses the ledger; `Not anchored` info; `Healthy`; `Attention` with the
worst alert), the age of the newest anchored aggregate, the number of chains read of the number
expected (`complete`), the last verification and its result, with links to the tabs. **Alerts**: the
nine kinds have labels and subjects in `AlertSubject.vue` and the kind label table
(`tests/kind-labels.test.ts` fails when a kind has no label). The Settings page of the cluster (the
existing one with the alert thresholds) gets the anchor and publication intervals. All text is
English; nothing the server sends (names, errors) is interpreted as markup (the existing `SafeName`
and `safeText` helpers are used).

## 9. Security and operations

### 9.1 Where each secret lives (summary)

| Secret | Where | Never in |
|---|---|---|
| Server signing key (seed) | `<ledger-keys-dir>/signing/<fingerprint>.key` on the leader and on the leader candidates (3.2.2); optionally sealed with a passphrase; or in an external signer | PostgreSQL, task rows, logs, audit, API answers |
| Passphrase of the key files | a file named by `--ledger-key-passphrase-file`, 0600 | the environment of other processes, the command line |
| Anchor credentials | `<ledger-keys-dir>/anchor/<target id>.json` (or the operator's `credentialsPath`), same sealing | as above |
| Client private keys | the clients' own custody; the server never sees them | everything |
| Public keys, checkpoints, tiles, events | `kingres_cp` and the publication; public | n/a |

### 9.2 Key backup, loss, and restore of the key files

- **Back up** `<ledger-keys-dir>` and the passphrase file **separately, off the cluster**, whenever a
  key is created, rotated or an anchor credential changes (the console shows "last changed" and a
  reminder, and `getLedgerSigning` reports `backupAdvised: true` until the operator ticks "I have
  made a backup" in the dialog; the tick is not a proof, only a reminder).
- **Loss of the signing key with no other active key** (every holder lost the file): the chain and
  the published history are not harmed; no further checkpoint can be signed. The console offers
  "Replace lost key": it generates K2, publishes a `key-added` event signed by K2 **only** (K1
  cannot authorise), marks the key ring `discontinuity: true`, and every auditor must replace their
  trust file by a new one obtained out of band (the dialog says so). Old checkpoints stay valid under
  K1 for auditors who still pin it; the discontinuity is visible to them as a new key they did not
  authorise, which is correct.
- **Loss of anchor credentials**: the target shows `failed`; the operator enters new ones; nothing
  already anchored is affected.
- **A restored host** (disaster recovery) puts the key directory back from backup before `kingresd`
  starts; a started `kingresd` that finds no key for an `active` key row raises `ledgerSignerUnavailable`
  and signs nothing.

### 9.3 Roles and audit

Reads need `kingres_readonly`; actions that change nothing but start work (digest, anchor now,
retry, bundle, verification) need `kingres_ops`; keys, targets, settings, client keys, policy and
acknowledgement need `kingres_admin`. Every changing operation is audited by its operation id
(the existing audit log); credentials are never in an entry. The signing and anchoring jobs
write no audit entry per round (volume); failures are alerts and log lines.

### 9.4 Restore from backup, point-in-time recovery, loss at failover: a fork, and the procedure

**What happens.** The chain of a group is part of the database. Restoring the database to an
earlier state (a base backup, PITR to a target time) or losing the last blocks at an asynchronous
failover gives a chain **shorter than what was already published and anchored**, and, as soon as it
accepts new transactions, **a different history from that height on**: two histories with the same
`chain_id` that share a prefix and then differ. That is a fork. Anchors make it visible (an anchored
aggregate names a head that the new chain does not have, or has with another hash), the publisher
makes it visible (4.7), and a monitor makes it visible (7.5). The ledger cannot prevent a restore
and does not try.

**Detection.** The publisher finds the head lower than the published size, or a hash that differs
at a published height, stops that chain log (`diverged`), flags its aggregate member `diverged`,
raises `ledgerChainDiverged` (critical), and the Overview tab shows the explanation and the "Acknowledge"
button. Until acknowledged, nothing is published for that chain, new checkpoints of other chains
continue, and the database itself works normally (the ledger does not block writes).

**Procedure (console, "Acknowledge divergence"; REST `acknowledgeLedgerDivergence`).**

1. The operator decides the restore or the loss was intended or unavoidable, and writes a reason.
   The dialog states the facts it can see: the published size and head (height `P`), the current
   head (height `C`), the first height at which the hashes differ, and "transactions committed in
   blocks above height `X` of the old history are not in the current database; their hashes stay
   in the published tiles and in every anchor and mirror, and no row data of them exists in the
   publication". It asks for the database name as confirmation.
2. Task `ledger.epoch.start` (steps idempotent, resumable): (a) publish the event `epoch-closed`
   (lines `chain`, `epoch`, `final-size`, `final-root`, `common-height` (the highest height whose hash is
   the same in both histories, `-1` if none), `kind` (`restore`, `failover-loss`, `other`), `reason`,
   `acknowledged-by`), signed; (b) freeze the old log (its tiles and last checkpoint stay served for
   ever, `state = closed`); (c) create the log of epoch `e + 1` with origin `.../e<e+1>`, size 0;
   (d) build it from block 0 of the current chain; (e) append an aggregate leaf in which the old
   member is flagged `retired` with its last good head and the new member (epoch `e + 1`) is present;
   (f) clear the alert.
3. Anchors continue with the next aggregate checkpoint. The old epoch's last anchored checkpoint
   remains as evidence of what existed.

**What a verifier concludes.** The new epoch verifies on its own. Comparing it with the old epoch
(`consistency` across epochs, `aggregate --previous` with a checkpoint of the old epoch) **fails**
and says why: "epoch 1 was closed by event <seq> (restore, common height X); N blocks above X
existed and are not in epoch 2". That failure is the point: an auditor who holds an anchored head of
the old history learns that the history was cut, when, and by whom (the acknowledging role is in the
event). The flag `--accept-epoch-closed <seq>` turns that failure into a printed finding with exit
status 0 for a verification of the new epoch alone; it never hides the finding.

**Restoring only the system database** (`kingres_cp`; correction 230). What can be derived again
and what cannot:

- The **chain logs** are derived from the chains, which a restore of `kingres_cp` does not touch: the
  leaves are the blocks, so the same leaves give the same tiles, and Ed25519 is deterministic, so a
  checkpoint of a chain log that is signed again is byte-identical to the one that was published. A
  chain log that the restored database holds at a smaller size is caught up by the next rounds
  as for any log.
- The **aggregate log and the events cannot be derived again.** An aggregate leaf holds the clock time
  of its round and what the chains looked like at that moment; an event holds its time and the state of
  the key set. A restored database that holds fewer of them cannot get back to what was published by
  any computation, and signing the next size of such a log would sign another root for a size that was
  published, and the next event another note for a number that was published. (An earlier text of this
  section said that everything is re-derived and re-signed byte for byte: that holds for the chain
  logs only.)
- Anchor records that were lost are created again for the newest checkpoint (6.1), and WORM
  re-submission finds the same bytes (the idempotent path of 6.3, 4).

A restored database is therefore **refused**: every holder keeps `published.json` (correction 171) and the
publisher signs nothing, writes nothing and raises the critical alert `ledgerSignerUnavailable` while the
database is behind it or contradicts it. That refusal has one end besides restoring the database that was
lost, a deliberate act of an operator, **accepting the loss**:

1. The alert says what was published (events, logs) and what the restored database holds, and names a
   confirmation of sixteen hexadecimal digits that depends on exactly these two states.
2. The operator reads the text, decides the loss is real and unavoidable, and writes the confirmation (and,
   on a second line, a reason of up to 200 printable ASCII characters) into the file `accept-restore` in the
   key directory of the manager leader. There is no setting, no call and no way of removing the record that
   does the same: a confirmation for another situation is ignored.
3. The leader replaces its record by what the restored database holds, one **generation** higher,
   keeps the old record beside it as `published.json.before-restore-<generation>`, removes the file and goes on. A record of a
   higher generation replaces the records of the other holders when it reaches them, and one of a lower
   generation never takes the floor back; a server that is about to lead with the old record asks the other
   holders first.
4. In the first round after, and in every round until it succeeds, the signed event `restore-acknowledged`
   is published (lines `generation`, `published-events`, `published-logs`, `published-state-sha256`,
   `database-events`, `database-logs`, `database-state-sha256`, `reason`, `acknowledged-by`), before anything else
   is signed. A crash between the acceptance and the event leaves the note in the record, and the next
   round publishes the event.

What a verifier concludes: the logs continue from the restored database, so the aggregate log and the events
show a **fork** against a mirror that holds the old checkpoints, with the event that says why next to it. That is
the finding the event is for, as the failure of `--previous` across an epoch (above) is for a rolled-back chain.
Nothing that was published is signed again with other content without this act. A restored `ledger_cluster.term`
smaller than a deposed leader's last term is the one case this design does not close (a deposed leader
that wakes up with a higher term could keep writing to a restored table): the restore procedure
documented in the README says to restart every `kingresd` after restoring the system database.

*Note of 2026-11-20:* the README of the control plane did not contain this step when this was written; it does now ("Restoring the system database"). The code does require it in practice: the guard of every write of the key set compares the stored term with the one the leader holds, so after a restore to an older term those writes fail with `term_over` until the leader has taken over again, which a restart does. The case this design leaves open is unchanged.

**Clones and `pg_dump`.** A physical clone of a database shares `chain_id` until the two diverge;
published under a different `originPrefix` the logs are distinguishable, and the verifier only
accepts the pinned prefix. A `pg_dump` and restore gives an **empty ledger** with a **new chain
identity** (the README's documented limit); it is a new chain, and the old chain's logs stay what
they were. The operator who drops the old database gets a `chain-retired` event for it
(`dropDatabase` runs the final round).

### 9.5 Upgrade from the current ledger version

Version 0.2 to 0.3 per database: `ALTER EXTENSION kingres_ledger UPDATE` (the script changes no
existing object's meaning and writes no entry). The cluster-wide orchestration is a new operation,
`upgradeLedgerExtension` (`POST /databases/{db}/ledger/upgrade`, role `kingres_admin`, task
`ledger.upgrade`, one step per group, calling the internal call `ledger/upgrade` which runs the
statement on that group's primary; it refuses to run while the groups' `kingres_ledger` versions
differ from the installed library, naming the server); the Overview tab shows a button when a group
reports 0.2. The existing `versionMismatch` alert covers mixed versions. A cluster on 0.2 keeps
working with everything that exists (not so since the write guard, see correction 25 in section 14:
the applications keep writing, and what writes the ledger itself waits for the update): `kingresd` detects the version by object (as it does for 0.1,
`ops/ledger.go` `ledgerRecords`) and answers `unsupported_operation` for the new operations that need
0.3 (`ledger_head`, `ledger_blocks_entries`, client keys); publication needs 0.3 and says so.
Digests and proofs made before the upgrade verify after it (a test).

### 9.6 Operations the operator must know

- **Retention of the WORM objects** ends: the console shows the retain-until date of the newest and
  oldest anchor and warns 30 days before the oldest anchor of a target passes it. Extending retention
  is done with the store's tools (S3 `PutObjectRetention` can extend, not shorten; the text of the
  fetched documentation); `kingresd` does not do it in phase 7.
- **TSA certificates expire**: a token stays valid as evidence if the certificate was valid at
  `genTime`; the console shows the TSA's certificate expiry from the last test.
- **Clock**: block times and `generated_at` are server clocks; the existing `clockDifference` alert
  (phase 13) covers the servers; anchors are the time evidence.
- **The window** (6.1) is shown, not hidden.
- **Disk**: 9.7.

### 9.7 Performance cost and limits

**Write path.** A transaction pays nothing new unless it attests: `ledger_attest` is one Ed25519
verification (an estimate of tens of microseconds with OpenSSL; step 8 measures it and records the
number in the README), one 250-byte row and one entry. The `required` policy adds an in-memory list
and a check at pre-commit. Signing, tiles and anchoring run on the leader after blocks are closed,
read blocks with one range query per chain per round, and write to the system database. A round of a
database with 8 groups is 8 small calls plus one transaction per log.

**Read side.** `ledger_blocks_entries` reads `ledger_blocks` by primary key. A publication audit
(re-reading every block) is a sequential scan of `ledger_blocks` once a day; it is a verification
option that an operator can switch off.

**Storage** (upper bounds, to be measured): a block every 10 seconds all day gives 8640 blocks, about
34 entry bundles of 22 784 bytes and 34 level-0 tiles of 8192 bytes, about 1.05 MB a day or 385 MB a year per
chain; blocks are built only when transactions are pending, so a quiet chain costs less. Checkpoints:
about 250 bytes each, one a minute at most, 130 MB a year per chain if all are kept; they are thinned to
the anchored ones plus one per day after 30 days. Partial tiles are deleted 24 hours after the full tile exists.

**Limits** (enforced, with a clear error or alert): 512 chains per database aggregate; 10 000 leaves
per read; a checkpoint at most once per 10 seconds; at most 8 anchor targets; an evidence note at
most 64 KB; a bundle at most 2 GB (otherwise the request is refused with the sizes, "ask for fewer logs
or a later `sinceSize`"); the listener 200 concurrent connections; label and reason fields 200 bytes;
OpenTimestamps `minCalendars` 1 to 8.

## 10. Implementation plan

Each step builds, passes its own tests and leaves every existing suite passing (extension suites on
PostgreSQL 18 and 19, the Go suites, the console suites, the contract check). Directories are the
ones named; two steps in different directories can run in parallel when their dependencies are
met. No step depends on phase 2b or on failover; the cluster tests of the leader change need HA part
2 and are marked **(HA)**: they are written then, and the code is written so that a leader change
is a loop restart (6.7).

| # | Step | Directory | Depends on | Tests it needs |
|---|---|---|---|---|
| 1 | **Verifier core**: `signednote`, `tlog` (tile paths, hash reading, inclusion and consistency from tiles, partial tiles), `checkpoint`, exit status 3, subcommands `checkpoint`, `consistency`, `trust init|show`; the `trust.json` format | `kingres/verifier` | none | vectors of 7.6 (1) to (3) with negative vectors; fuzz of parsers; every existing verifier test unchanged |
| 2 | **Extension 0.3, part A**: `ledger_block_preimage`, `ledger_head`, `ledger_blocks_entries`, scripts `0.3` and `0.2--0.3`, `default_version`, `DATA`, `meson.build` | `contrib/kingres_ledger` | none | regress `ledger_head` (empty ledger, one block, range limits, tamper: a block whose stored hash does not match gives `XX001`); `ledger_privs` expected output extended (no assertion removed); TAP `011_upgrade_0_3` (0.2 database with blocks, digests, proofs; all still verify); format tests of 0.1 pass unmodified; **all on 18 and 19** |
| 3 | **Contract 0.9.0**: every operation and schema of 8.4, the alert kinds, problem codes, `AuditAction`, the README, `check-contract.mjs`; all new operations answer 422 `unsupported_operation` in the server until implemented (the established convention) | `kingres/api`, then regenerate `kingres/control-plane/internal/contract/operations_gen.go` and `kingres/web/src/api/*.generated.ts` in the steps that consume them | the other people's current contract work merged | the contract checker; schema examples validate; generated tables compile |
| 4 | **Control plane foundations**: migration 0008, `internal/scheduler` with the term guard, `internal/ledgerkeys` (file signer, envelope, passphrase), tasks `ledger.key.create|rotate|retire|distribute` and revocation, events, keyring, trust file, `getLedgerSigning`, settings operations, internal calls `ledger/key/*` | `kingres/control-plane` | 3 | unit tests of the envelope, of the signer, of the key state machine and the events; crash-point tests of the new tasks (the engine's `FaultPoints`); the term guard test (a deposed writer cannot write); 2-server cluster test of key distribution; **(HA)** key available after a leader change |
| 5 | **Collector and chain logs**: `internal/ledgerround`, internal call `ledger/read`, `internal/ledgerlog` (frontier, tiles, bundles, checkpoint text and signing), `internal/ledgerpub` storage, divergence (4.7), jobs `ledger.round` and `ledger.digest`, `generateLedgerDigest` over all groups with `complete` true | `kingres/control-plane` | 2, 4 | tree vectors shared with step 1's script (same sizes and roots); a real PostgreSQL with two groups (the existing cluster test harness): digests of both groups, tiles grow, checkpoint verifies; restart mid-round; divergence tests: restore an older backup, delete a block, rewrite a block, lose the tail; the job survives `kingresd` kill -9 at each write |
| 6 | **Aggregate and publication endpoint**: aggregate leaf codec (5.2), aggregate log, stale, retired and diverged flags, group removal hook (final round and `chain-retired` event), `dropDatabase` hook, `getLedgerAggregate`, `listLedgerAggregates`, `getLedgerPublication`, the HTTP listener and the follower read, `LedgerDigest.aggregateHash` filled | `kingres/control-plane` | 5 | codec vectors including the 512-member limit; a group made unreachable, then back; group removed; database dropped; HTTP behaviour (paths, caching headers, no redirects, `HEAD`, 404 for unknown, partial tiles); a listener with TLS |
| 7 | **Verifier: aggregate, proof with the log leg, bundle, monitor**: subcommands `aggregate`, `proof --bundle`, `bundle verify`, `bundle fetch`, `monitor`, `trust follow`; the cross-check with fixtures written by a `kingresd` test (a publication of a real two-group database) | `kingres/verifier` | 1; fixtures from 6 | vectors of 7.6 (4); the cross-check; mutation tests (every byte of a tile, a note, a leaf) rejected; `monitor` against a local HTTP server that rolls back and rewrites |
| 8 | **Extension 0.3, part B: client signatures**: first a spike (a test that registers an internal ledger table from the install script under the hook), then `sign.c`, the three tables, the functions of 3.7, the `required` policy in `capture.c`, new `ledger_verify` checks, `ledger_status` columns, README | `contrib/kingres_ledger` | 2 (same files) | regress `ledger_sign` (every error `KL010` to `KL014`, a replayed nonce, another chain's id, a revoked key, a wrong role, a tampered attestation row, `ledger_verify` detects a deleted attestation); TAP `012_attest_policy` (a prepared transaction, a savepoint rollback of an attested entry, concurrency, a standby, crash recovery); a build without OpenSSL raises `0A000`; the hash format tests unmodified; **18 and 19**; the measured cost |
| 9 | **Console A**: tab bar of the Ledger page, Signing, Publication, aggregate dialog, signed column, dashboard card (states without anchors), alert labels for the kinds of 6.9, the mock for the new operations | `kingres/web` | 3 | component tests; `kind-labels` test; accessibility (axe) and layout browser tests as for the Configuration page; hostile-text tests (the existing `hostile` pattern) |
| 10 | **Anchoring framework and WORM**: `internal/anchor` (interface, records, jobs `enqueue`, `submit`, `upgrade`, backoff, supersede), `sigv4`, `worm`, credentials files and write-only API, `testLedgerAnchorTarget`, `listLedgerAnchors`, `anchorLedgerNow`, `retryLedgerAnchor`, alerts of 6.6 and 6.9 | `kingres/control-plane` | 4, 6 | SigV4 published vectors; a fake S3 server of the test that can answer 412, 409, 403, 5xx, lock headers missing; MinIO when present (WSL, skipped otherwise); crash at each point of Submit; no credential in any log, task row or response (a test greps them); alert transitions |
| 11 | **Verifier: WORM anchors and `anchors`** | `kingres/verifier` | 7, evidence format from 10 | vector of 7.6 (5); `--worm-url` against a local server; status 3 when not re-fetched |
| 12 | **RFC 3161 target** (DER request, response, token verification) | `kingres/control-plane` | 10 | tokens of 7.6 (6); a local test authority built in the test; every rejection case of 6.4 |
| 13 | **Verifier: TSA anchors** (second implementation) | `kingres/verifier` | 11 | the same vectors; fuzz of the token parser |
| 14 | **OpenTimestamps target** (calendar client, fragments, upgrade job, `.ots` assembly) | `kingres/control-plane` | 10 | a fake calendar; the reference client's `.ots` files as vectors; expiry; `ots verify` on the exported file if the client is installed |
| 15 | **Verifier: OpenTimestamps anchors** | `kingres/verifier` | 11 | vectors of 7.6 (7); status 3 without a header; a wrong header |
| 16 | **Console B**: Anchoring tab, target dialogs for the three kinds, Test, anchor records, retry, evidence download, the "Not anchored" notices, dashboard card complete, alerts | `kingres/web` | 3, 9; end to end with 10 | component tests with the mock; the end-to-end script of the console against a real `kingresd` for one WORM target on a fake store |
| 17 | **Client keys in `kingresd`**: `listLedgerClientKeys`, `registerLedgerClientKey`, `revokeLedgerClientKey`, `setTableSignaturePolicy`, `upgradeLedgerExtension` (task `ledger.upgrade`, internal call), detection by object for 0.3 | `kingres/control-plane` | 8, 3 | e2e against a real instance with 0.3; with 0.2 the answers are `unsupported_operation`; the upgrade task across two groups; a group on 0.2 is named |
| 18 | **Verifier: `attestation`** | `kingres/verifier` | 8 (fixtures from a real server) | vectors of 7.6 (8); a revoked key; a key not pinned; the "no revocation shown" wording |
| 19 | **Console C**: Client keys tab, signature policy control, the extension upgrade button, per-table display | `kingres/web` | 3, 9; e2e with 17 | component and browser tests |
| 20 | **Bundles, epochs, verification checks**: `createLedgerBundle`, `getLedgerBundle` (files under the data directory, 24 hours), task `ledger.epoch.start`, `acknowledgeLedgerDivergence`, the `checks` of `startLedgerVerification` (publication audit and anchor checks), the `publication` audit job | `kingres/control-plane` | 6, 10 | a bundle that the verifier of step 7 and 11 accepts; every divergence case of 9.4 to the new epoch; a crash during the epoch task; the audit finds a changed tile |
| 21 | **Verifier: epochs** (the findings of 9.4, `--accept-epoch-closed`), `bundle verify` completion | `kingres/verifier` | 7, 20 | a fixture with a closed epoch from step 20's test |
| 22 | **Console D**: bundle dialog, acknowledge dialog, publication audit control, verification dialog with `checks` | `kingres/web` | 3, 9; e2e with 20 | component and browser tests |
| 23 | **Release of the verifier**: `scripts/release.sh`, `scripts/repro-check.sh`, `release verify`, README of the verifier, the packaging step that installs the binary and checks `LimitCORE=0` | `kingres/verifier`, `kingres/packaging` | 21 | the reproducibility check passes twice from different paths; the sums note verifies |
| 24 | **Optional**: external signer `command`; publication mirrors (directory, S3 without lock); `ledger/anchor/probe` | `kingres/control-plane`, `kingres/web` (the fields) | 4, 6, 10 | a fake signer program that returns a wrong signature, hangs, returns garbage; a mirror that falls behind |
| 25 | **Optional**: the explicit refusal of a ledger table in `create_distributed_table` and `create_reference_table` with its own SQLSTATE and message, and `check_cluster` | `contrib/kingres_shard` | the shard work now in progress is merged | a regress test; no change otherwise |

Parallelism: steps 1, 2 and 3 start together (three directories). After 3: step 4 and step 9 (control
plane and console) run in parallel; after 2 and 4: step 5; step 8 follows 2 in the extension
directory while 5 and 6 proceed in the control plane and 7 in the verifier. The anchoring kinds 12,
14 (control plane) and 13, 15 (verifier) are independent of each other. Each step updates the README
of its directory and adds a line to the work log. The order puts everything that does not need the
anchoring or the client keys (1 to 7, 9) first, so a first usable result exists after step 7: signed
checkpoints, an aggregate and a verifier that checks them, offline.

## 11. Out of scope, with reasons

| Item | Reason |
|---|---|
| A global snapshot and a consistent cut across groups, a multi-node ledger, consensus | Phase 8, future roadmap (ADR 0013, architecture.md "Scope of the first release"); 5.6 states what the aggregate does not prove |
| Ledger tables over distributed or reference tables | 5.1: a ledger-aware bucket move and capture on partitions are a design of their own, and `kingres_shard` is being changed by other work |
| Witness cosigning and a witness network | 4.8: needs the cosignature specifications read and a third party to run witnesses; the checkpoint format leaves room |
| Anchoring to a public smart-contract chain (`ethereum` stays in the enum, never produced) | The architecture lists it as an option with unchecked cost; OpenTimestamps already uses a public chain |
| Batch client signatures (one signature over a set of rows) and client-side transaction signatures | 3.7 signs one entry; a batch needs a second canonical format and a decision on partial failure |
| Hiding which rows exist (confidentiality), erasing rows (the right to erasure) | The ledger keeps hashes for ever; a row hash of a low-entropy row can be confirmed by guessing (README); no change here |
| Long-term validation of time-stamp tokens (OCSP, CRL, re-timestamping) | The evidence keeps token and certificates; the tools exist elsewhere |
| A Bitcoin node or explorer inside `kingresd` | The server must not be the judge of its own evidence; the auditor supplies headers |
| Key escrow, secret sharing of the signing key, a key ceremony | Operator's process; the external signer hook (3.2.3) is where an HSM policy lives |
| Proving the absence of a revocation | An inclusion proof cannot prove absence (7.4); stated, not solved |
| Moving the CA key and changing the manager leader | HA part 2; the key distribution of 3.2.2 is written to be reused for the CA key |
| Dump and restore of a ledger with its history | Documented limit of the extension; a physical backup keeps the chain |
| Per-database signing keys, per-group keys | 3.2.1 |
| Changing the hash format, a block signature | 2.1 and 3.1 |
| A Windows `EXEC_BACKEND` run of the extension's new code | Not tested for 0.2 either (README limitations) |

## 12. Decisions

"Owner" marks a decision that belongs to the project owner; the default written here is what will be
built unless the owner says otherwise.

| # | Decision | Alternative rejected | Why | Owner |
|---|---|---|---|---|
| 1 | Hash format version 1 is unchanged; everything is added around it | A format 2 with a Merkle tree over blocks, or a signature in the block header | Every digest, proof and the verifier would carry two formats for ever; the extra log gives the same benefit (2.1, 4.1) | |
| 2 | The server signs checkpoints of logs, not blocks or rows | A signature in every block header (the architecture's recommendation) | Format change, a key near the block builder inside PostgreSQL, no consistency proof (3.1) | |
| 3 | Ed25519 as signed notes | ECDSA or RSA; a custom signature envelope | Standard library, deterministic, the type of signed-note that generic tools read | |
| 4 | One signing key per cluster; logs are told apart by key name | A key per shard group | The leader holds all of them anyway; more to pin (3.2.1) | |
| 5 | The signer is `kingresd` (the leader); the key is a file outside PostgreSQL, sealed by an optional passphrase; the leader candidates hold a copy | The key in the system database or in the extension; an external KMS client library | A superuser of the database must not hold it; no vendor dependency (3.2.2) | |
| 6 | An optional `command` signer for HSM or KMS | Linking vendor libraries | Same trust effect, no dependency (3.2.3) | |
| 7 | Rotation with an overlap and a `key-added` event signed by both keys; revocation with `compromised-since` and anchors as the arbiter of time | A flat key list; a revocation that voids all signatures | Trust moves without a new out-of-band step; a stolen key cannot back-date silently (3.3) | |
| 8 | Client signatures are rows of ledger tables (`ledger_client_key`, `ledger_attestation`) with an Ed25519 message bound to chain, table, op, row hash, key id and nonce | A new hash tag for signatures; a signature column in each ledger table | No format change; the existing proofs and `ledger_verify` cover them; the hash of a table's rows is unchanged (3.7) | |
| 9 | Ed25519 verification in the extension through OpenSSL; absent OpenSSL, the function refuses | A bundled C implementation of Ed25519; verifying only later | A bundled implementation is code to audit; the product is built with OpenSSL for TLS; an invalid signature should fail at write time | |
| 10 | The policy `required` exists, one way only, and counts every entry | An always-on policy; a policy of insert entries only | A table that must be signed is a property of the application; deletions need an author too | |
| 11 | Chain log leaf = the 87-byte block hash preimage; tiles by tlog-tiles | A native tree layout; a leaf per transaction | Generic tooling works; the leaf is small and reveals only block-level facts (4.1, 4.3, 4.9) | |
| 12 | Tiles live in the system database and are served by an optional listener (followers ask the leader); mirrors are optional | Files in the leader's data directory only | The system database follows the leader and its standbys; no new replication mechanism (4.5) | |
| 13 | Publication is off by default; the console explains what it exposes | On by default | Timing and block volume are exposed (4.9); the first release works offline | yes |
| 14 | `originPrefix` cannot change after the first checkpoint | A free setting | It is the name every signature is made under; changing it breaks every pin (4.2) | |
| 15 | The aggregate is a log (a tile log of rounds) whose leaves list the heads and chain-log roots of all chains; `aggregateHash` is its root | A Merkle root over the heads of one moment; a hash chain of aggregates | Consistency proofs, the same tooling, explicit membership and flags (5.2, 5.3) | |
| 16 | Members are ordered by `(chain_id, epoch)`, not by shard group id | Ordering by group id | Group ids can change meaning; the chain identity is the stable key | |
| 17 | An unreachable group is carried as `stale`; the round is not blocked | Refuse the round; skip the member | Skipping would look like removal; refusing would stop anchoring for the others (5.5) | |
| 18 | Ledger tables stay on one group; phase 7 does not lift the refusal for distributed or reference tables | Lift it now with a ledger-aware move | Needs a design of its own and touches `kingres_shard` (5.1) | yes |
| 19 | The aggregate checkpoint is what is anchored, newest only, with backlog coalesced | Anchor every chain checkpoint; anchor every digest | One anchor covers everything before it; bounded backlog (6.1, 6.6) | |
| 20 | No anchor target is on by default; the console suggests WORM first and a TSA or OpenTimestamps second | Ship with public OpenTimestamps calendars on | Offline first; sending a digest to a third party is an operator's choice | yes |
| 21 | WORM mode default is governance; compliance needs a typed confirmation | Compliance by default | Compliance cannot be undone by anyone for the retention period | yes |
| 22 | WORM writes use `If-None-Match: *`, a SHA-256 checksum header, object lock headers, a fixed key per aggregate size; a different body under the same key is a critical conflict | Overwrite-and-version; random keys | Idempotent retries and a loud signal for a rewritten history (6.3) | |
| 23 | Own SigV4 for five S3 calls | The AWS SDK or another S3 client library | A large dependency tree for five calls | |
| 24 | RFC 3161 token verification at submit, with an operator-supplied CA bundle; two implementations (server and verifier) | Accept any token; share code between server and verifier | An unverifiable token is not evidence; the verifier's independence is a stated property (6.4) | |
| 25 | OpenTimestamps: submit to at least two calendars (when configured), keep fragments, upgrade by polling, never fetch Bitcoin data on the server; the auditor supplies headers | The server checks Bitcoin; one calendar | The server must not judge its own evidence; one calendar is one point of failure (6.5) | |
| 26 | Public OpenTimestamps calendars are offered by a button, not preset | Preset the four public calendars | Same as 20; the list has to be confirmed at build time (section 13) | yes |
| 27 | Anchoring and publication run as scheduler jobs of the leader with a term guard, not as one task per round | A task per round | The task table would fill with rows nobody reads (6.7) | |
| 28 | The scheduler is shared with phases 10 and 12; whoever is built first creates it | A scheduler of its own | One recurring-work mechanism, one leader-change behaviour | |
| 29 | Credentials of anchor targets are files like the signing key, never rows | The system database | A superuser reads it; the threat model (2.2) | |
| 30 | A restore or a loss at failover is a fork; the way out is an acknowledged new epoch with a signed event, never a repair | Overwrite the old log; renumber silently | The history that anchors hold must stay readable and comparable (9.4) | |
| 31 | The verifier gets exit status 3 (incomplete) and keeps 0, 1, 2 | Fold "could not check" into 1 or 0 | A pending or offline check is neither a pass nor a failure | |
| 32 | The verifier trusts a pinned `trust.json`, not the keyring the server serves; `trust follow` extends it only through signed events | Trust on first use | The first key is the root of trust (3.4) | |
| 33 | Verifier releases are reproducible, with a signed sums note; the release key's custody is the owner's | Unsigned binaries; no release | A verifier you cannot check is not independent (7.7) | yes |
| 34 | `LedgerBlock.signed` means "covered by a signed checkpoint"; `aggregateHash` means the root of the aggregate log; `ethereum` stays in the enum unused | New fields; removing the enum value | Additive contract; removing an enum value is breaking | |
| 35 | The extension upgrade is an operation with a console control (`upgradeLedgerExtension`) | `ALTER EXTENSION` by hand | Every capability has a control; mixed versions are named | |
| 36 | Defaults of numbers: checkpoint every 60 s, anchor every 3600 s, rotation overlap 30 days, WORM retention 3650 days, 512 chains, OpenTimestamps expiry 7 days | Other values | They bound the cost and the window; all are settings except the 512 limit | yes |

## 13. Statements that need verification

Not checked at the source, checked only in part, or taken from memory; none is presented as fact above
unless it is listed under the sources of its section. Before the step that depends on one is built, the
implementer checks it and records the answer in the work log.

**Specifications**

1. [tlog-cosignature](https://c2sp.org/tlog-cosignature) and [tlog-witness](https://c2sp.org/tlog-witness)
   were not read (4.8 is future work and says so).
2. The `signed-note` and `tlog-checkpoint` pages were read through a summarising fetch: the key id
   formula, the em dash form, the key-name rules and the checkpoint lines are as stated there; their
   own test vectors, if any, were not read, so the vectors of step 1 may need to be replaced by the
   specification's.
3. [tlog-tiles](https://c2sp.org/tlog-tiles) was read through a summarising fetch: tile height 8, the path
   encoding, `.p/<W>`, the entry bundle with `u16` lengths, headers and pruning are as stated; the
   rule that "alternative entry formats must use a different URL" was taken to mean that ours (the standard
   bundle format, our own entry content) is fine; confirm against the full text.
4. RFC 9162: the fetch covered the first 100 000 of 149 395 characters; sections 2.1.3.2 and 2.1.4.2
   are as quoted. That RFC 9162 obsoletes RFC 6962 is stated by the document; the incompatibilities of
   RFC 9162's *entry formats* with the extension's use of only the tree and hash rules were not studied
   (the extension needs only the tree).
5. RFC 3161: the fetched text does not say POST (the RFC defines the media types; POST is the practice);
   the media type `application/timestamp-response` in 3.4 versus `application/timestamp-reply`; the
   value of the `id-kp-timeStamping` OID, the `ESSCertID` or `ESSCertIDv2` forms (RFC 2634, RFC 5035) and the
   CMS signed-attribute verification (RFC 5652) were not read. The PKIStatus values, the nonce, the
   message imprint and the critical extended key usage are as in the fetch.
6. S3: that PutObject takes `x-amz-object-lock-retain-until-date` (the name was not seen in the fetched
   excerpt; `x-amz-object-lock-mode` and `If-None-Match` were); whether a checksum header or
   `Content-MD5` is **required** when lock headers are used (not found in the fetched page; the design
   sends `x-amz-checksum-sha256` anyway); that `HeadObject` returns the lock mode and retain-until
   headers and `Last-Modified` as used in 6.3 step 3; the behaviour of `If-None-Match` on a versioned,
   locked bucket (the fetched text covers it for a bucket without mention of versioning); the exact
   SigV4 canonical rules for each call (the implementer uses the published SigV4 documentation and
   test suite, not read for this document).
7. MinIO: the statements are those of the fetched page. Ceph RGW, Backblaze B2, Wasabi, Cloudflare R2
   and others were **not** checked and are not claimed to work; the Test button is how an operator finds out.
8. OpenTimestamps: everything about calendars comes from the reference client's source (`calendar.py`,
   `notary.py`, `timestamp.py`), not from a protocol document: the `POST /digest` and `GET /timestamp/<hex>`
   paths, the `Accept` header, the tags `0588960d73d71901` and `83dfe30d2ef90c8e`, the file magic, the version,
   the 1000-byte URI limit and the 10 000-byte response limit. **Not checked**: the operation tags of the
   serialization (SHA-256, append, prepend and the others), the exact order of forks and the file hash
   operation byte, the byte order in which the Bitcoin block's Merkle root is compared with the result
   of the operations, whether a calendar answers `404` for "not yet confirmed" or something else, whether the
   `stamp` command adds a nonce before submitting, the default calendar URLs (the site names Alice, Bob,
   Finney and Catallaxy, not their addresses), and the "about two hours" accuracy (taken from architecture.md).
   The layout of a Bitcoin block header (80 bytes, `time` at offset 68) is from memory.
9. Sunlight and the static-ct-api page were summarised by a fetch; Sunlight itself was not read.

**The repository and the platform**

10. That `ledger_register_internal` can register a table of schema `kingres` from the install script
    under the protection hook (step 8 begins with this spike; if the hook or the event triggers refuse it,
    the fallback is a new hash tag and format version 2 for attestations, a larger change that would be
    brought back to the owner).
11. That `XACT_EVENT_PRE_PREPARE` and `XACT_EVENT_PRE_COMMIT` can raise the error of the `required` policy
    on both majors (PostgreSQL's transaction callback documentation was not re-read for this design).
12. Ed25519 through OpenSSL's one-shot `EVP_DigestVerify` (OpenSSL 1.1.1 or later) and that the OpenSSL
    builds the product packages use on every platform expose it; `USE_OPENSSL` as the compile-time switch.
13. Go: `crypto/pbkdf2` in the standard library at the toolchain `go.mod` names (Go 1.27.3 is what the
    committed `go.mod` says; it was not checked against an installed toolchain here); that no extra module
    is needed for TLS, ASN.1, ECDSA, RSA and Ed25519.
14. The internal names the design depends on: the leader-change notification of `internal/leader`, the helper
    of `internal/fence` or `task.lease_epoch` for the term guard, the exact shape of the internal API
    authorization table, `AlertSubject` formats, whether a Settings page with thresholds exists in the
    console at the place 8.5 puts the new settings (the router has `settings`; its content was not read).
    Phase 10's section 6.3 (its scheduler) was read by heading only.
15. That the committed `contrib/kingres_shard` has no ledger-specific refusal (a search of `src` for "ledger"
    found nothing); the refusal comes from the trigger and hook rules and from the documents quoted in 1.1.
    The working tree of that directory is being changed by others.
16. The numbers of 9.7 (Ed25519 verification cost, storage per chain per year, round costs) are computed
    or estimated, not measured.
17. That the existing test harnesses (the two-group cluster test, the WSL PostgreSQL scripts, MinIO in WSL)
    can run the tests listed in section 10; MinIO in the test environment was not looked for.
18. The statement that the Dashboard has no ledger card and that the Ledger page has no tab bar is from the
    committed `DashboardPage.vue` and `LedgerPage.vue` by search, not by a full reading.

## 14. Corrections found while building

Step 1 (the verifier core) was built against the specifications themselves. Where this document and a
specification differed, the specification was followed; the points are listed here so that later steps
build to the corrected rule. The verifier's README has the same list with the reasons
("Findings about the phase 7 design (step 1)").

1. **Length of a consistency proof (4.10).** The bound is `ceil(log2(n)) + 1` hashes (RFC 9162, 2.1.4.1),
   not `2*ceil(log2 n)`. The verifier accepts at most 65.
2. **Extension lines of a checkpoint (3.2.4).** `tlog-checkpoint` allows lines after the third. The
   verifier accepts and reports them; `kingresd` writes none.
3. **Origin of a checkpoint.** The origin follows the rules of `tlog-cosignature`: at most 255 bytes, no
   space, no `+`, no control character. The origin prefix of a cluster is limited to 160 bytes so that the
   longest log origin fits.
4. **Key names (7.2).** The key name on a signature line of a checkpoint is the origin of the log
   (`<prefix>/c/<id>/e<N>` or `<prefix>/a/<id>`). The member `name` of a key in the trust file is a label
   only. The cluster-level name `<prefix>` is for the signed events of a later step.
5. **Validity windows of keys (3.3 against 7.4).** A checkpoint carries no time, and a retired key's
   checkpoints stay valid. `not_before` is enforced against `--now`; `not_after` was reported and not
   enforced (superseded by 14: it is enforced on `--now`, and `max_tree_size` and `revoked` were added; superseded in part by 17 for a checkpoint the auditor holds). Which signatures of a revoked key are still trusted is decided by anchors, as 3.3 says.
6. **Exit status for a malformed note (7.4).** When the note is the thing under test (`checkpoint`), a
   malformed note is status 1. When it is the anchor of trust (`consistency --old`), an unreadable note is
   status 2 and a bad signature or origin is status 1.
7. **Order of the output (7.4).** The headline (VERIFIED, FAILED, INCOMPLETE) is the first line, as in
   the commands that already existed, not the last.
8. **Duplicate signature lines.** Two lines with the same name and key id are rejected for every note.
9. **A bad signature from a known key** always rejects the note (the specification says SHOULD).
10. **Epoch in `--log` (7.4).** Without `:e<N>` the epoch is taken from the source only if exactly one
    epoch directory exists; with `--note` it must be given.
11. **Partial tiles (4.4).** A reader that does not find a partial tile falls back to the full tile, as
    `tlog-tiles` allows, so deleting partial tiles after 24 hours is safe for the verifier.
12. **Sizes.** A proof that verifies for a size can also verify for a neighbouring size with the same
    root, so a size is only meaningful when it comes from a signed checkpoint. Every command takes sizes
    from checkpoints.
13. **Sources (4.6, 7.4).** Step 1 reads a directory. `.tar.gz` and HTTP sources belong to step 7.
14. **Key validity in the trust file (7.2, 7.4; replaces the second half of correction 5; a checkpoint the auditor holds is not subject to the window, see 17).** A checkpoint
    carries no time that anyone can trust, so a rule that depends on "when the checkpoint was signed"
    cannot be applied; a rule that depends on a tree size can, because the size is in the signed text.
    The review of step 1 (finding L7-02) showed that a `not_after` that is only reported looks like a
    control and is not one: a user who writes the end date of a leaked key into it expects the key to stop
    counting, and got a `VERIFIED`. The trust file (format 1, not released, so changed in place) now has
    four validity members per key, and `trust show` says for each one whether and how it is enforced:
    - `not_before`, `not_after`: enforced, on the time the auditor gives (`--now`, default the system
      clock), never on the time of the signature. A key outside its window is left out of the keys that
      may sign, with a note that says why. When no key is left the command ends with status 2 and the
      error lists each key and why it was left out. This fails closed: after `not_after` the checkpoints
      of that key are no longer accepted unless the auditor says `--now <a time before not_after>`, which
      is the auditor's statement and is printed ("fixed by --now"). Reporting only was rejected because
      the failure mode of a member that does not act is the one the user cannot see; failing closed has a
      visible failure and a visible remedy.
    - `revoked` (boolean, default false): the key is never used, at any `--now`. It is the way to say "do
      not trust this key" for a key that leaked.
    - `max_tree_size` (integer from 1, default none): a checkpoint counts as signed by this key only when
      its tree size is at most this. It is the way to retire a key that was good until a known size: the
      checkpoints it signed up to the size stay valid for ever (3.3), and a checkpoint it signs beyond
      the size, which a leaked key could produce, is refused (step `key-size`). It does not stop a leaked
      key from signing a different history at a size within the limit: that is caught only by a
      checkpoint or a consistency proof the auditor already holds. `revoked` and `max_tree_size` together
      are refused as contradictory. Unknown members are still refused.
    A trust file with no key is refused when it is read (it can verify nothing), so `trust show`
    no longer calls such a file valid; `trust show` also says, for the time it is given, how each key
    stands and ends with status 2 when no key can be used then, as every verifying command would.
15. **Rollback and a state file (7.4; superseded in part by 17: the state moves only by proof, and the order named here is withdrawn).** `checkpoint` shows that a checkpoint is signed, not that it is the
    latest: an older genuine checkpoint verifies, which hides every later entry (L7-03). Its list of what
    is not proved says so and names the three remedies (keep the last verified checkpoint and run
    `consistency --old`, `--min-size`, `--state`). `--state FILE` names a file holding the last
    checkpoint this command verified (the signed note itself, so that it is checked again with the trust
    file at every use and can be given to `consistency --old`): a smaller size fails (step `rollback`),
    the same size with another root fails (step `fork`), and the file is replaced atomically after a
    success. A larger size is accepted but is **not** shown to extend the logged history (that needs
    tiles): the output says so and points to `consistency`. Replacing the state loses the older
    checkpoint, so the order for a full check is `consistency --old STATE`, then `checkpoint --state
    STATE`. A state that does not verify against the trust file is an input error (status 2), never
    silently replaced.
16. **What the publication does not hold, and what it holds wrongly (7.4).** One rule for every command
    (L7-07): a file or directory the auditor names (trust file, `--note`, `--old`, `--new`, `--state`,
    `--source`) that cannot be read is an input error, status 2. A thing the publication should contain and
    does not (a checkpoint, a tile, an epoch directory) is status 3 ("could not check"), and the output
    calls it an alert: a publisher must serve what a checkpoint it signed needs, so absence is at
    best neglect and at worst a way to hide a failure. A thing the publication holds in a form it must not
    have (a symbolic link or reparse point on the way, a directory, device or pipe, a file over its size
    limit, a tile of the wrong size) is a failed verification, status 1 (L7-01, L7-05). Every file is read
    through one helper that never blocks on open, requires a regular file, and bounds the size before and
    while reading. Links in a source directory are refused rather than resolved inside it: a publication
    written by the publisher's tools has no link, and resolving some would need a rule for which aliases
    are lawful. A file named on the command line may be a link (the auditor chose it) but must be regular
    after resolution.

17. **The state file only moves forward by proof, and what a held checkpoint is (supersedes the order
    of correction 15 and the second half of correction 14, and changes correction 6 for `--old`).**
    The second review (L8-01, L8-03) showed that a state that moves on a comparison of sizes can be walked
    from one branch of a forked log to the other with status 0, and that the two-command order of
    correction 15 reads the publication twice. The invariant is now: **the state file only ever moves from
    a checkpoint to one that was proved, in the same run and for the same bytes, to extend it; status 0
    with `--state` means exactly that.** `checkpoint --state` therefore reads the checkpoint once, verifies
    its signature, and for a larger size reads the proof tiles from `--source` and verifies RFC 9162
    2.1.4.2 against the stored and the new root before it replaces the state. An equal size and root needs
    nothing; an empty stored tree needs no tile; no state yet records the checkpoint and says nothing was
    known. Without tiles the result is **status 3** (the publication lacks what a signed checkpoint needs;
    the rule of correction 16) and the state stays; a tile that does not lead to the signed new root is
    status 1 and the state stays. A fork (a path that does not lead to the stored root, or the same size
    with another root) and a rollback are status 1; the state is **never** overwritten, and the conflicting
    signed note is saved beside it (`<state>.conflict-<size>-<root prefix>`), because two notes signed by
    one key for trees that are not one history are the proof of misbehaviour; the output names both
    files. `consistency --old` stays what it was, a read-only proof for a checkpoint held by hand; it is
    no longer part of a two-command sequence, which is withdrawn. The state file is a JSON record (the note, the
    log, the time and the keys it was verified with) and `--old` accepts it. The record is information for the
    reader, not an input to any decision, because whoever can write the file can already delete it or put an
    older genuine note in it, and the note must still carry a valid signature of a key that is not revoked.
    **A checkpoint the auditor holds is a statement about the past**: it is accepted when a key of the trust
    file signed it, no signature of a key of the file is wrong, no signer is revoked, and the size is within
    the key's `max_tree_size`; the validity window is not applied to it (a window says when a key may sign new
    checkpoints). Retiring a key with `not_after` therefore does not make the auditor discard their state;
    `revoked` does make the key's past signatures unusable as an anchor, because they are suspect. Every
    failure of a held checkpoint is **status 2 in both commands** (it is the auditor's anchor, not the thing
    under test, which stays status 1, correction 6), and the advice never says to delete the state: it says to
    keep the file, take a checkpoint signed by a trusted key from a trusted source, and start a new state with
    `--min-size N` for the size believed, so that no rollback is opened. `not_after` is measured on the
    auditor's clock and `revoked` is not, which the trust file documentation says.
18. **Locks, replacement and crash safety of the state (7.4).** The state is read, compared, proved and
    replaced under an exclusive lock on a lock file next to it (`flock`, `LockFileEx`), waited for at most
    10 s, status 2 on timeout, so two runs cannot move it backwards. The replace writes a temporary file in the
    same directory, flushes it, renames it over the state and flushes the directory (a write-through rename on
    Windows); helper files have a short name that does not depend on the state's name; a state named through a
    link is replaced where the link points; the file mode is kept. A fault hook in the helper stops after each step and a test
    checks that the state is the old or the new one, whole, and that the next run works.
19. **Text from inputs, input caps and the exit status table (7.4).** (a) Every string that reaches the text
    output passes one function that escapes control characters, non-UTF-8 bytes, bidirectional and other format
    characters and line separators, and clips long values; the JSON output carries the strings as they are. (b)
    `digest-chain` lists a directory by name through the safe helper (no glob), reads at most 1,024 files and 64 MiB. (c)
    A chain directory without an epoch is status 3 whether or not `:e<N>` is given; two epochs without `:e<N>`
    is status 2; a held checkpoint is 2 in both commands (correction 17); a publication file that the auditor's
    account cannot read stays status 2, since it is the auditor's copy. (d) `--now` must be given a value when it
    is given, with a point as the decimal mark, and times are printed with their fraction; the window is inclusive.
    (e) `blocks` reads its export line by line and keeps one header (1.2 million blocks, 400 MB of input, used 12 MB
    where the whole-file version used 1.15 GB).
20. **Who writes the ledger tables (5, 9.3; closes the gap that a member of `pg_write_all_data` could
    insert a valid successor block).** The nine tables of the ledger are written by the C code of the
    extension, by the scripts of the extension while they run, and by a superuser acting as itself who has
    set `kingres_ledger.allow_manual_write` in the session; by nothing else, whatever privilege the role
    holds. The code raises a marker that lives in the backend and that no SQL function raises or lowers; the
    functions of the extension write through fixed statements inside it. The setting cannot come from a
    configuration file, from `ALTER SYSTEM`, `ALTER ROLE`, `ALTER DATABASE` or a connection option. Two
    things enforce the rule: `ExecutorCheckPerms_hook` (the same signature on 18 and 19) and a trigger
    before INSERT and DELETE on each of the nine tables. An UPDATE or DELETE is refused with the message it
    always had (`KL001`).
21. **Why the trigger is needed beside the hook.** The apply worker and the table synchronisation worker of
    logical replication do not call `ExecutorCheckPerms_hook`, on 18 or on 19. The trigger is what refuses
    a subscription that writes a ledger table.
22. **The time of a block (3.1).** `ledger_build_block` refuses (`KL004`) to build on a block that was
    closed more than `kingres_ledger.max_clock_skew` (one hour by default, a superuser's setting, `0` for
    no limit) ahead of the clock of the server; the transactions stay pending. `ledger_verify` reports a
    block closed in the future whatever the setting is (`block_time`), a block with no transaction
    (`block_empty`) and a transaction with no entry (`tx_empty`). The hash format is unchanged.
23. **Recorded digests are checked (6).** `ledger_verify` checks every recorded digest against the chain:
    the block it names (`digest_block`), the hash (`digest_hash`) and the document (`digest_json`). A
    chain that was rewritten makes the digests recorded before it findings.
24. **Removing what a hand left (9.4).** `kingres.ledger_remove_stray(reason)` is for the case that rows
    were written above the head by hand: superuser only, with the manual-write setting on, under the lock of
    the builder. It copies the rows above the head and every digest that names a missing block or another
    hash into `kingres.ledger_removed`, puts the transactions that have entries back in the queue and
    deletes the rows; no trigger is switched off. `ledger_verify` then reports one `operator_removal` for
    each removed row, for good. That record is covered by no hash (covering it would change the hash
    format), so the operator hands the exported record to the auditor; a removed block that had been
    published is a fork to an auditor, which is intended.
25. **A library newer than the SQL (9.5).** The guard is in the library and the functions that write are
    in the SQL of the same version. A database whose SQL is 0.1 or 0.2 is refused with the statement to run
    (`ALTER EXTENSION kingres_ledger UPDATE`) and is never half protected. The capture triggers are in the
    library and keep working, so the applications are not stopped; the block builder, the workers and the
    calls of the control plane wait for the update, and the transactions stay pending. `kingresd` checks
    the version before it enables the ledger on a table, makes a digest or records a verification, and
    answers 422 `unsupported_operation` with that statement, not the "forbidden" that PostgreSQL's
    "insufficient privilege" would become.
26. **`pg_upgrade`.** In binary upgrade mode the hook, the trigger and the event triggers do nothing
    (`IsBinaryUpgrade`, `ledger_binary_upgrade()`), so `pg_upgrade` carries a ledger to the next major
    as it is.
27. **`LOCK TABLE`.** A lock stronger than `ROW EXCLUSIVE` on a table of the ledger is refused also for a
    role that PostgreSQL would let take it (`pg_maintain`, `pg_write_all_data`): such a lock stops the
    capture triggers and with them every write of every ledger table of the applications.
28. **Dump and restore.** `pg_dump` does not dump the tables of the ledger. A restored database has a new,
    empty ledger with a new identity; it does not verify as the old one, and `ledger_verify` names every
    table whose capture trigger came with the dump. A ledger is carried by a physical backup or by
    `pg_upgrade`, not by a dump.
29. **The verifier: bounds on what it reads and stores, and a fifth exit status (7.4; adds to 17 to 19).**
    (a) A signed note is at most 64 KiB, 32 lines of text, 32 signature lines and 1,024 bytes a line, and a
    checkpoint has at most 16 extension lines; a digest is at most 64 KiB. Before a state is stored it is
    read back through the functions that will read it, so the verifier never writes a state that a later
    run cannot read. (b) The JSON reader has limits on nesting, values, members, key and string length and
    white space, and the peak memory of every command that reads a file is measured. (c) Exit status 4: a
    fork or a rollback was found (what status 1 says) and the conflicting checkpoint could not be saved;
    evidence is said to be saved only when a file with that content is there. (d) The lock is one file per
    directory (`.kingres-state.lock`), opened without following a link; on Unix the directory of the state
    must be private to the user, or `--allow-shared-state-dir` is given and the output says so. (e) The
    escaping of the text output is one to one, and a clipped value is marked in a way that no input can
    print. (f) The table of exit statuses is derived from the source and tested, and `blocks` states what
    it did and did not show about an export.
30. **The invariant of the guard, and how it holds (5, 9.3; replaces the mechanism of correction 20).** No
    statement that a role can issue writes a table of the ledger, whatever the role inherits or owns, except
    through the public functions of the extension doing what they are documented to do, and a superuser
    acting as itself with `kingres_ledger.allow_manual_write` set in the session. The first form of the guard
    had a function, `kingres.ledger_internal_write`, that took a statement number and its values and was
    protected by its ACL only; a member of the role that owns the extension, an owner that lost SUPERUSER
    and a superuser without the setting could call it. It is removed. No function that SQL can call takes
    the content of a write: the functions that write are C entry points that read the ledger and compute
    every value they write (the block builder, the digest, the recorded verification, the registration of a
    table and of the rows it holds, `ledger_register_table`), and the capture trigger, which checks on
    insert, as it did on update and delete, that the identity in its argument is the one the registry has
    for the table. `ledger_history_sync` and `ledger_history_acl_sync`, which changed a table of the
    caller's choice through the catalog API, refuse a caller that is not a superuser.
31. **A write by hand leaves a record (9.3, 9.4).** The guard trigger of the tables that hold the chain
    writes a row into `kingres.ledger_removed` with the action `inserted` (who, when, the table, the row as
    JSON) in the transaction of the write, and `ledger_verify` reports one `manual_write` finding per
    table for as long as the record exists. No hash covers the record. A delete by hand is recorded only
    when `ledger_remove_stray` makes it.
32. **What can stop the builder (corrects 27).** The builder and `ledger_remove_stray` take turns on a
    lock on the object of the extension, with a sub-object number that no statement uses, and not on a
    table lock. A lock above `ACCESS SHARE` on a catalog table of the ledger is refused to a role that is
    not a superuser; a row lock on any of the nine tables is refused to everybody who has not turned the
    manual-write setting on; `CLUSTER` and `REINDEX` of such a table are refused inside a transaction
    block or a function. One maintenance statement on a large table still holds its lock while it runs.
33. **The refusal of older SQL has its own SQLSTATE (adds to 25).** `KL005`, with the message and the
    hint unchanged; `42501` stays for the other refusals of the guard.
34. **The same tests on both majors.** The suites of the extension gave one test fewer on 19 than on 18
    because one test skipped, without saying so, a setting that PostgreSQL 19 refuses
    (`standard_conforming_strings = off`); it now asserts the refusal there. Tests of honest paths run with
    the guard closed, and the manual-write setting is used only where a test writes a row by hand.
35. **Evidence of an anchor, as written (6.2, 7.4).** Evidence is compact JSON with its members in a fixed
    order and every member present; times are UTC in whole seconds; a parser refuses unknown members. The
    WORM evidence holds no note (endpoint, bucket, key, version, ETag, lock mode, retention date, time of
    the store): the verifier takes the note from the publication or from the object it fetches. The TSA
    evidence holds the token, which is the CMS `ContentInfo` exactly as the authority sent it, not the
    whole `TimeStampResp`. The OpenTimestamps evidence holds one fragment per calendar (the tree of
    operations rooted at the digest) and, once upgraded, the height of the Bitcoin block and the calendar
    that attested it; it carries no time until an auditor supplies the block header.
36. **RFC 3161 (6.4).** SHA-512 is accepted beside SHA-256 and SHA-384 as the digest of a signature; both
    forms of the signing-certificate attribute are accepted; a token with unsigned attributes is refused,
    and so is DER that is not strict. A rejection of the authority with `systemFailure` or
    `timeNotAvailable` is retried; every other rejection counts as refused.
37. **OpenTimestamps (6.5).** The operations are append, prepend, SHA-1, RIPEMD-160 and SHA-256;
    `keccak256`, `reverse` and `hexlify` are refused by name (one example of the reference client uses
    `keccak256`), other tags as unknown; an attestation of an unknown kind is kept as it is. A proof is at
    most 1 MiB and 8,192 nodes.
38. **Refused answers (6.6).** A refused answer (credentials, a missing bucket, a lock that is not enabled,
    a rejection of the authority) is tried again after 1, 5 and 15 minutes; the fourth refusal in a row
    fails the record. The WORM target has no "submitted" state: a read-back that fails after the object
    was stored is retried, and the retry finds the object through the answer 412. The WORM client follows
    no redirect at all; the other two follow one to the same host and scheme, three times at most.
39. **Configuration and test vectors (6.8, 7.6).** The WORM and OpenTimestamps targets have a field
    `allowInsecure` that permits an endpoint without TLS. The test vectors of other projects (the AWS
    signature test suite, the examples of the reference OpenTimestamps client) are not kept in the
    repository: a manifest of their sums and a script that fetches them from a pinned commit are, and the
    tests that need them skip without them.
40. **What the state of the verifier holds (7.4; adds to 17 and 29).** The state and the evidence of a
    fork hold the canonical form of the verified note: the text, a blank line, and the signature lines of
    the keys that verified and counted, written again from the parsed signature in the order of key name
    and key id. Nothing that a publisher without a key can add or vary is stored (lines of unknown keys,
    lines of keys that did not count, their order and number). A later change of the trust file never
    makes a stored state unreadable and never makes it accept less. A note under test keeps the strict
    rule: a line that names a usable key and does not verify refuses the note. A note that is held is
    judged by the signatures that verify. States written before this correction are still read.
41. **The private path of the state (7.4; replaces the check of one directory in 18).** On Unix every
    component of the path, links included, must be changeable only by the auditor or by root (a sticky
    directory is accepted above the directory that holds the state); the holding directory, the state and
    the lock file are held to a stricter rule, and the name is looked at again after the lock is taken. A
    lock file that is not private is replaced. On Windows none of this is checked.
    `--allow-shared-state-dir` accepts the risk and prints its note.
42. **No size limit on a proof or on a block export (4.8, 7.4; replaces the caps of 16 MiB and 1 GiB in
    19 and 29).** A proof carries one header for every block after its block, so the proof of a row older
    than about six days was larger than 16 MiB. A proof is read in pieces and twice: the first reading
    checks the whole file and keeps no header, the second walks the headers, and the hashes of the two
    readings must be equal. The peak memory for the proof of a row one year old (1.06 GB) is about 13 MiB.
43. **A verdict that was not written is not status 0 (7.4; adds to 19).** A failed write to the standard
    output turns status 0 into 2 and says so on the standard error; a verdict that is not 0 keeps its
    status.
44. **One cut and one escape rule in the output (adds to 19).** The only cut of a value is the one of the
    output layer, after escaping, at a character boundary. A backslash is written as two, and the README
    states the rule so that a decoder written from it reads the output back.
45. **`blocks` does not claim more than it shows (adds to 29).** Status 0 means that the headers form a
    chain from the genesis hash; `info.anchored_to_digest` says whether a digest ties that chain to
    anything outside the database.
46. **Smaller points of the verifier.** A `trust init` that fails to write removes the file it made. The
    nesting limit of the JSON reader is 32 levels exactly.
47. **The key file and the key set in `kingresd` (3.2, 3.3).** The key file is a magic line and compact
    JSON; a sealed key uses PBKDF2-HMAC-SHA256 with 600,000 to 6,000,000 iterations and AES-256-GCM, with
    the magic line, the name of the derivation, the iteration count and the salt as authenticated header;
    a file that is not canonical byte for byte is refused. A key set has at most 16 keys (the limit of the
    trust file). Retiring or revoking a key needs another active key. A retired key is written to the
    trust file without `not_after` (correction 14). **At a rotation the latest checkpoint of every log
    is signed again with the new key**: a log that stops growing would otherwise keep only the signature
    of the old one.
48. **Tiles (4.4, 4.5).** A checkpoint of size 0 is signed and written, and no tile exists for it. Tiles
    are built from the frontier; only the partial tiles at the right edge (one per level, and the partial
    entry bundle) are read, once when the log is opened, and checked against the frontier. Each append
    writes the partial tile of the new size and leaves the older ones. After a crash a log whose tiles are
    ahead of its frontier is opened again and the same leaves are appended: identical tiles are not an
    error, different ones are the divergence of 4.7. The verifier of step 1 reads no entry bundle, so a
    damaged bundle is found only by the audit of the publisher; a later step of the verifier reads the
    bundles on the path of `proof --bundle`.
49. **A second invariant of the guard: what a role can stop or remove (5, 9.3).** A role that is not a
    superuser cannot stop the block builder or the capture of rows for longer than one of its own
    statements runs, and cannot remove the ledger or the protection of a ledger table. The exceptions are
    named in the README of the extension: a statement of the role that runs long; the locks that the owner
    of an application table takes on its own table; a client in pipeline mode that never sends Sync
    (ended by `idle_in_transaction_session_timeout`); `setval` on the sequence of the registry, which no
    hook sees (the next `ledger_enable` then fails on the primary key and nothing is registered or
    forged); a table that has an armed capture trigger and no row in the registry; and what a superuser
    does.
50. **A refusal of a lock is about the relation that is locked, not the one that is named (corrects 27
    and 32).** `LOCK TABLE` of a view locks the tables of its stored query, so the statement hook follows
    views, and views over views, to the end. A statement that fails releases its locks at once; the harm
    of a statement that is going to be refused is its wait for the lock behind a reader, with every
    writer queued behind it. So the hook never asks for a lock on a catalog table of the ledger before it
    has decided, and `TRUNCATE` of one is refused before any lock. A foreign key that references a catalog
    table of the ledger is refused (it takes a strong lock when it is created and puts triggers on the
    table).
51. **Nothing of the ledger is dropped through a dependency.** `object_access_hook` sees every object
    that any `DROP` removes. While a table is under ledger control, nothing that belongs to the ledger
    (the extension, its schema and members, the ledger tables and their history tables and columns, the
    capture triggers) can be dropped by any role, through any cascade, except by the scripts of the
    extension itself; with an empty registry `DROP EXTENSION` is the ordinary statement. This closes the
    drop through `DROP EXTENSION plpgsql CASCADE`; twelve functions of 0.3 are still written in PL/pgSQL.
    `ALTER ... DEPENDS ON EXTENSION` of an object of the ledger is refused.
52. **The builders run as a statement of their own.** The lock of the builder must be held to the end of
    the transaction (released earlier, the next builder would wait for the rows of the uncommitted block
    or build the same height). So a caller that is not a superuser may call `ledger_build_block` and
    `ledger_generate_digest` only as a statement of its own: inside a transaction block, a savepoint or
    an exception block of PL/pgSQL the call is refused (`25001`). Superusers and the worker are unchanged,
    which is what `kingresd` uses.
53. **The owner of a ledger table, the schema and the sequences.** The owner of a table under ledger
    control may do what changes neither what is recorded nor how; the list is in the README and is run
    statement by statement by a test. `GRANT`, `REVOKE` and a change of owner on the schema of the
    extension, and `ALTER SEQUENCE` on its sequences, are refused to a role that is not a superuser.
54. **The registry lookup of the capture trigger** is a hash for the transaction with one entry in front
    of it; it holds positive answers only and is dropped at the end of the transaction and when a
    subtransaction aborts. Rows that alternate between two ledger tables cost about 7 percent more CPU
    than before the guard (20 percent with the first form of the lookup).
55. **What a held checkpoint rests on (7.4; replaces the clause of 17 that makes the past signatures of a
    revoked key unusable as an anchor).** The identity of a checkpoint is its signed text (origin, size,
    root, extension lines) and never its signature lines. A held checkpoint is usable when its text
    verifies under any key of the trust file, whatever the standing of that key now: a revocation says
    that the key must not be trusted for new notes, not that a checkpoint the auditor already verified is
    unknown to them, and a held checkpoint only ever makes a run stricter. One that rests only on revoked
    or over-limit keys stays the anchor, is named so in the output with a line that it was not shown
    genuine now, and is replaced by the first checkpoint that extends it and is signed by a key that
    counts (what a run reports when it replaces or completes such a state: 86). A plain note given as `--old FILE` keeps the strict rule. The trust file has no "compromised
    since": a checkpoint carries no time that can be trusted, and `max_tree_size` is the form of "since"
    that the data proves. Removing from the trust file every key that signed a held checkpoint is the one
    change that makes the state unreadable (status 2, with the rebase named); a key is retired by
    `not_after`, `max_tree_size` or `revoked`, not by deleting it.
56. **The lines of one text only accumulate (7.4; replaces the canonical form of 40).** The canonical
    form holds the text and every line that verifies under any key of the trust file, written again in
    the order of key name and key id. A served note with the text of the held one is merged into it, and
    the state is rewritten only when it gains a line. The evidence of a fork or rollback is one file per
    signed text, extended in place with new lines of that text (for a state that counts; for a weak
    state see 85). Which of the genuine lines a party
    without a key serves changes neither the status, the stored size, the held text nor the verdict of a later run (it can
    change the wording, the JSON members and the names of the evidence files of a later conflict, see
    69 and 85); there is no limit on the number of evidence files, since a limit would be
    a lever for status 4.
57. **The lock file is never replaced, and a write never goes over a state it did not read (7.4;
    replaces "a lock file that is not private is replaced" in 41; adds to 18).** The lock file is locked
    as it is and then made private in place; one that belongs to another user is refused with the command
    to remove it; the name is checked again after the lock. Just before the rename the state is read
    again, and a run that finds other bytes than the ones it decided on writes nothing (status 2).
58. **A closed standard output (adds to 43).** A standard output that is the null device opened for
    reading and writing is what the runtime puts in place of a closed descriptor: status 2 on every
    command, with a line on the standard error (unless the caller says it is on purpose, see 70). The null device opened for writing only is the caller's
    request for no output and runs as usual.
59. **A limit on one element of a proof (4.8, 7.4; adds to 42).** One element of the chain of a proof is
    at most 4,096 bytes and what is outside the chain at most 1 MiB, checked while the document is read;
    a larger one fails at the parse step. The memory of a proof is bounded by one header whatever the
    file holds.
60. **What "anchored" means for OpenTimestamps (replaces 6.5 step 3; the first form made a record
    anchored on the word of a calendar).** A Bitcoin attestation that a calendar returns is a claim. It
    is accepted as a claim only after the checks that need no block header: the message it sits on is 32
    bytes, the path from the digest is made of allowed operations and is recomputed from the digest, the
    height is possible for the time of submission, and no other root is claimed at the same height. The
    record then has the state `claimed`, which the contract shows as pending and which never counts as
    anchored. It is `anchored` only when a source of block headers that the operator configures returned
    the header of that height and the claim matched it (the merkle root, the proof of work, the block
    time against the submission and the clock); the header is stored in the evidence. That the header is
    on the best chain is for the auditor to check.
61. **Evidence as written (replaces the evidence part of 35).** The WORM evidence adds the SHA-256 and the
    length of the object, and its version id is never empty. The OpenTimestamps evidence adds the time of
    submission, and its Bitcoin member holds the height, the calendar, the merkle root, a status
    (`claimed` or `header_checked`) and the header. The parsers accept the canonical bytes only, so
    evidence is stored as text, not as a JSON column that reorders it.
62. **RFC 3161 (6.4; adds to 36).** A token whose time is further from the clock than a tolerance (300
    seconds by default) in either direction is refused, and the chain must be valid now as well as at the
    time of the token. Added checks: an RSA key of at least 2,048 bits, the issuer in the
    signing-certificate attribute, the name of the authority in the token, the key usage of the signer.
63. **WORM (6.3; adds to 38).** Compliance is the default mode; governance is an explicit choice with a
    warning, since a principal with the bypass permission can delete the version. The time of the store
    is bounded against the clock. The read-back is by version id; a later version written by somebody
    else is not a conflict when our locked version exists; a version of ours that was stored without a
    lock gets its retention applied by the next attempt.
64. **The record of an anchor (6.6; replaces the counting in 38).** One table of states and events; an
    event that is not allowed leaves the record unchanged. Refusals are counted apart from other
    failures (the fourth refusal in a row fails the record); retryable failures end after 200 attempts or
    7 days, with the alert. Every fragment of an OpenTimestamps proof has its own budget of lookups in a
    round, and the answer of one calendar that is bad, oversized or implausible is dropped alone.
65. **Clients and payloads (6.8).** The packages never use a proxy from the environment; the HTTP client
    is built from given roots with TLS 1.2 or later and timeouts. Every target checks that the digest of
    a payload is the SHA-256 of its note.
66. **Identifiers do not come from sequences (replaces the exception about `setval` in 49).** A member
    of `pg_write_all_data` holds the right to move every sequence through the role itself, which nothing
    can revoke, and no hook sees `setval`. So no writer of the ledger takes an identifier from a
    sequence: the registry of tables, the digests, the recorded verifications and the removal records
    each take the maximum plus one under a lock of the writer and set the sequence to follow. No
    identifier is part of a hash.
67. **How long a role may hold a lock of the ledger (replaces the remedies named in 49 and 52).** A role
    that is not a superuser holds a lock of the extension for at most
    `kingres_ledger.lock_hold_limit` (one minute by default; only a superuser sets it): a session that
    is still in the same transaction at that time is ended. The earlier advice does not hold:
    `idle_in_transaction_session_timeout` does not end a pipeline that waits for Sync, and a role can
    switch off `statement_timeout` and `transaction_timeout` for itself. Superusers and the workers
    are not limited.
68. **The owner of the database (9.3).** A database can be dropped by its owner, who need not be a
    superuser, and nothing loaded in that database can prevent it. A deployment makes a superuser the
    owner of every database that holds a ledger, which the control plane must ensure, and keeps digests
    outside the database. `ledger_status` reports the owner and whether it is a superuser, and
    `ledger_enable` warns when it is not.
69. **A weak anchor does not accuse the log (7.4; adds to 55).** When a held checkpoint that rests only
    on revoked or over-limit keys disagrees with the log that is served, the failure line names the keys
    the state rests on and why they no longer count, says that the conflict may come from the state and
    not from the log, and gives the rebase command. "Both are signed by a key of the trust file" is
    printed only for a state that counts. The evidence of such a conflict is bounded per held state
    and never one file per run (two files at most, see 85). The JSON carries the fact in
    `info.state.anchor_weak` with the keys and the reason, on every exit after the state was judged (86).
    The exit status stays 1.
70. **The null device on purpose (adds to 58).** A caller that gives the null device opened for reading
    and writing as the standard output on purpose (a process library's "discard", a daemon) sets
    `KINGRES_VERIFY_NO_OUTPUT=1`. The refusal stays the default, and its line names the variable.
71. **The order of the replacement of the state (7.4; adds to 57).** The temporary file is written and
    flushed first, and the state is read again only then, just before the rename. The lock is what
    excludes another run; this second reading only narrows the window for a writer that ignores the lock
    and does not close it. Measured with such writers: 1 of 1,200 overwritten on Linux (265 of 1,200
    with the reading before the write), 189 of 3,000 on Windows (1,346 of 3,000 before), where the rename
    with write-through is itself the window. These are counts on one machine, one file system and one
    model of the other writer, not rates. The README of the verifier gives them.
72. **A stored signature line is kept as stored (7.4; adds to 56).** When a state or an evidence file is
    rewritten, every line it holds stays, also the line of a key that has left the trust file. Such a
    line makes nothing usable: usability is decided from the trust file of the run, as in 55.
73. **Files on Windows (7.4).** A rename that another program blocks (a scanner, an indexer) is retried
    for three seconds before the run fails with status 2 (a read-only target or a directory at the name
    is named at once, see 87). On every platform a new evidence file is written under a temporary name,
    flushed, and linked into place, so that a reader never sees half of one and an
    existing file of that name is never replaced.
74. **Who can drop the database (9.3; replaces the first two sentences of 68 and amends the second
    invariant of 49).** A role that is not a superuser cannot remove the ledger or the protection of a
    ledger table by any statement that runs in the database that holds it. `DROP DATABASE` from another
    database is open to superusers, to the owner, to every role with the privileges of the owner (a
    membership with INHERIT, directly or through a chain), and to every role that can `SET ROLE` to one of
    those or to a superuser; a superuser as the owner is therefore not enough, and the words "in a
    database that a superuser owns" are withdrawn. What protects a deployment: the owner is a superuser
    role without members that are not superusers, no role that is not a superuser may `SET ROLE` to a
    superuser, and the digests are kept outside the database. `ledger_status` reports whether such a role
    exists and lists them (`database_droppable_by_non_superuser`, `database_droppers`), and `ledger_enable`
    warns with the same list. It is not a finding of `ledger_verify`: it is a property of the roles that
    changes with the next GRANT and damages nothing, and a row of `ledger_verify` means damage.
75. **A lock of the ledger and PREPARE (adds to 67).** A transaction of a role that is not a superuser
    and that holds a lock of the extension cannot be prepared (`0A000`): a prepared transaction would
    keep the lock beyond every limit. A superuser may prepare; the limit never covered a superuser. With
    `kingres_ledger.lock_hold_limit = 0` nothing is tracked.
76. **What the limit follows (adds to 67).** The limit follows the locks per subtransaction: a lock that
    went with a rolled back savepoint no longer counts, and the timer is cleared at commit, at abort, at
    `COMMIT AND CHAIN` and `ROLLBACK AND CHAIN`, and when the subtransaction that took the last lock
    aborts. The server log names the role, the locks and the limit of a session that the limit ended; the
    client sees the same FATAL as for a session ended by an administrator.
77. **What an identifier is (replaces the wording of 66).** An identifier is the key of a row among the
    rows that exist: the largest identifier committed now plus one, taken under the lock of the writer
    and read with a snapshot taken after the lock was granted. It is not used once only: after a rollback
    the next writer takes it again, and it means a row only after the transaction that took it has
    committed. A caller of `ledger_verify_recorded` in a longer transaction keeps the identifier it
    returns only after the commit. The builders, `ledger_enable`, `ledger_register_table` and
    `ledger_remove_stray` require READ COMMITTED; `ledger_verify_recorded` and the record of a write by
    hand work at every isolation level.
78. **One order of locks for every writer.** The table lock of `ledger_enable`, then the locks of the
    builders, the registry, the verifications and the removals. A write by hand (a superuser with
    `allow_manual_write`) takes all four first, and an identifier made by hand that a writer took in the
    meantime is replaced by the next free one.
79. **A wait for a lock of the extension can be ended.** A cancel, a timeout or a termination ends the
    wait for any lock of the extension at once. Only the run of `ledger_verify` inside
    `ledger_verify_recorded` is caught, and the record of a run that was cancelled is written only when
    the lock is free.
80. **Smaller points of the same round.** An index on `ledger_removed` ends the scan of that table under
    the lock of the builders. A role that is not a superuser cannot revoke from PUBLIC the EXECUTE of a
    function of the extension (`KL002`).
81. **WORM: the time of the store and the first attempt (6.3; replaces the time bound of 63).** A
    payload carries the time of the first attempt to anchor the checkpoint for the key (the creation of
    the record, kept by a Retry), and every target refuses a payload without it. The time that the store
    states for a version is never later than the clock plus the tolerance (300 seconds by default) and
    never earlier than the first attempt less the tolerance; for a version just written it must also be
    within the tolerance of the clock. A version of our own bytes that is complete and locked has no
    other age limit, so a recovery after a crash, or an operator who takes a month to repair a bucket,
    still completes the key. A store whose clock was refused for a fresh write is refused again on every
    later attempt, because the object is found again and judged by the same rule. A store ahead of the
    clock is refused (code `store_time_skew`); a store behind the first attempt is permanent (code
    `store_time_early`), with a text that says it is a matter of clocks and not of credentials and what
    to do. An existing version whose retention is earlier than the first attempt asked, less five
    minutes, gets the retention applied and is verified again. Not closed: a store behind by more than
    the tolerance and by less than the duration of the first submit can pass on a retry, with a time
    within the tolerance plus thirty seconds of the first attempt; closing it needs the caller to
    remember the refusal. The first attempt of a key is the earliest creation time among the records for
    the same target, log and size, which the wiring must ensure.
82. **OpenTimestamps: a claim does not end the collection (6.5; replaces the end of 60).** A record in
    the state `claimed` keeps asking the calendars whose fragments still hold pending attestations, until
    it is anchored with a checked header, expires, or nothing is pending. A claim that a block header
    contradicts is dropped from the fragments as an implausible one is; a header that cannot be used, or
    a block time ahead of the clock, contradicts nothing. When no plausible claim is left the record
    falls back to `submitted` while another calendar can still deliver, and fails with a conflict only
    when a claim was contradicted and nothing is left that could deliver. A `claimed` record expires
    after four times the timeout of a submitted one. A round in which every lookup failed is retryable
    even when every failure was permanent; only a stored fragment that cannot be read is permanent.
83. **The limits of a payload (6.8; adds to 65).** They are those of the verifier: an origin of 1 to 255
    bytes of UTF-8 without white space, plus signs or control characters, a size below 2^62, at most 32
    signature lines, no line longer than 1,024 bytes. The two modules repeat the numbers, and a test of
    the library reads them from the source of the verifier.
84. **RFC 3161 names and the text of an alert (6.4, 6.6; adds to 62 and 64).** The name in a token
    corresponds to the certificate when it is the subject or a name of the subject alternative name,
    compared by decoded attribute values (any string type, case and white space folded), or the same DNS
    name, address or other name; a token is refused only when no name of the certificate corresponds. The
    alert for a failure is worded from the code and the text of the error and not from its class: the
    class "refused" covers credentials and also clocks and the validity of a certificate.
85. **The evidence of a weak state is two files at most (7.4; replaces the evidence sentence of 69).** A
    conflict with a held checkpoint that rests only on revoked or over-limit keys is kept in at most two
    files: one for the first conflicting note that is smaller than the held one (a rollback), one for
    the first with the same size and another root or a larger size that the proof does not connect (a
    fork). A replay of an old genuine checkpoint, which a party without a key can always serve, takes
    the first file only and never the place of a fork. A note whose file holds another checkpoint, or
    whose save failed for any reason, is printed in full by the run (base64 in the text, `note_base64`
    in the JSON), as for a state that counts; a note whose signed text is already in the file counts as
    saved and its new lines are added. The bound is fixed and independent of what a party without a
    key serves; such a party can decide which of two conflicts of one kind is kept and cannot hide
    either from the auditor.
86. **What a run reports about the held state (7.4; adds to 55 and 69).** `info.state.anchor_weak`
    (`info.old_anchor_weak` for `consistency --old`) is present on every exit after the held state was
    judged; a run that stops before it read the state, for instance at the limit of the wait for the
    lock, has none. A run that writes a state signed by a key that counts reports the state as it is
    after the write: weak before, not weak after (`anchor_weak_repaired`), and it does not list as not
    proved that the held checkpoint was genuine, because it proved it a prefix of a checkpoint under a
    key that counts or added that key's line to it. A key rotation makes a state weak without any act
    of the publisher (the state was made while only the old key signed, and the old key is then
    revoked); an auditor runs once with both keys in the trust file before revoking the old one, so
    that the state gains the line of the new key.
87. **Temporary files, a failed flush of the directory, a read-only target (7.4; adds to 71 and 73).**
    The next run removes, under the lock, every temporary file of the program in the directory of the
    state: the name must match the pattern of the program exactly, the file must be regular and owned
    by the user; nothing else in the directory is touched. When a rename or a link succeeded and the
    flush of the directory failed, the run says that the file was replaced or saved and that a power
    loss may bring the old state of the directory back; the status is unchanged (2 for the state, 4 for
    the evidence of a state that counts, 1 for the evidence of a weak state), and the JSON says which
    of the two happened. On Windows a
    target with the read-only attribute, or a directory at the name, is named at once without the wait
    of three seconds; the wait stays for the error codes that an open handle of another program
    produces, which an access control list that denies the rename shares, so in that one case the
    message names another program wrongly after the system's own text.
88. **Retry from expired, and what ends a claimed record (6.6; adds to 64, replaces the last sentence
    but one of 82).** An anchor record in the state `expired` takes the operator's Retry as a `failed`
    one does: it becomes `pending`, due at once, with its counters and its error cleared and its creation
    time set to the time of the Retry, so that its waiting period starts again. A claimed record ends at
    the first of two limits: its own timeout (four times that of a submitted record, from creation), and
    the limits on failures when its calendars all fail (200 attempts or 7 days of failure by default);
    with the defaults the second ends it first.
89. **WORM: the time of the store and the start of the first submit (6.3; replaces 81 from "A payload
    carries" to "to do.", and its closing sentences).** The time a payload carries is the start of the
    first Submit call for the key, which the caller makes durable before that call and never changes; a
    Retry keeps it, and a new start needs a new key. Every put also writes that call's own start into the
    user metadata of the object. A version written by the call must be within the tolerance of the
    clock. A version found by a later call must not be earlier than the start of the first Submit less
    the tolerance nor later than the clock plus the tolerance; when it carries the start of the call that
    wrote it, the window is narrowed to that start less the tolerance up to that start plus the timeout
    of a call plus the tolerance. For a version the library wrote, the time the evidence vouches for is
    therefore never further from the true time of the write than the tolerance plus the duration of one
    Submit call, whatever the age of the record, the attempts, the Retries or the crashes. For a version
    found without that metadata the time is only known to lie between the start of the first Submit less
    the tolerance and the clock plus the tolerance, and the evidence says so (`submit_start`, `found`).
    The retention floor of a found version is its own time plus the configured days less the
    tolerances, never a time the caller states. A found version in COMPLIANCE mode is accepted where
    GOVERNANCE is configured; GOVERNANCE found where COMPLIANCE is configured is refused. A store ahead
    of the clock stays refused when the clock has caught up with the object. The cluster should have a
    bucket that nobody else writes: a party that can write it can make a key end in a conflict.
90. **A contradicted claim is kept aside (6.5; replaces "A claim that a block header contradicts is
    dropped" of 82; adds to 60).** A block header that contradicts a claim of a calendar does not drop it
    on that one answer. The claim stays in its fragment, is recorded in the evidence with the header and
    the time, is neither plausible nor anchored, and its calendar keeps being asked (a target with a
    source of headers keeps the pending attestation of a merged answer for that). It is dropped for good
    only when the source contradicts it again in a round at least `contradictionConfirmMinutes` (60 by
    default) after the first; a header that confirms it later puts it back. The receipt returns the
    numbers of contradicted, aside and dropped claims so that the caller can alert. At most 32 claims
    are kept aside in one evidence. `Upgrade` has a bound of its own (five minutes by default) beside the
    caller's.
91. **The work of a header (6.5; replaces the plain check of the proof of work of 60).**
    `header_checked` means a header whose hash meets its own target, whose target is no easier than a
    sixteenth of the difficulty of the nearest real header of the embedded table at or below the height
    of the claim (the factor is an option), whose merkle root is the message of the claim and whose
    time fits. It shows that this much work was spent on that header; it does not show that the header
    is in the best chain, which the auditor checks against a node of their own. The work of the accepted
    header is reported.
92. **One set of rules for a payload, and a narrow comparison of names (6.8, 6.4; replaces 83 and the
    name rule of 84).** A payload is parsed by the rules of the verifier for the signed note and the
    checkpoint (the name rule, at least the four bytes of a key id, strict canonical base64, no two lines
    with the same name and key id, the root of the empty tree for size 0, up to 16 extension lines); the
    library is stricter on purpose only for the byte 0x7f and for a note over 16 KiB, and a corpus of
    notes with the verdict of each side is in its tests. The name of a time-stamp token is compared with
    a narrow profile: a value that is not valid in its string type matches nothing, only ASCII letters
    are folded and only ASCII space is collapsed, and a TeletexString that is not ASCII is compared by
    its bytes. An operator who means one authority pins it by the SHA-256 of the public key of the
    signer; a token under another key is then refused.
93. **Keys: what the set refuses (3; adds to 14 and 47).** A maximum tree size of a retired key is 1
    to 2^62-1, the range the verifier reads; `Retire` takes either such a size or an explicit
    "unbounded", never a bare zero, and unbounded means that a retired key that leaks can sign any
    larger tree until it is revoked. `Retire` and `Revoke` need another key that is in force at that
    moment and leave no stale end of an overlap. Asking for the signers of a set in which nothing may
    sign is an error, and a set that has been in service and holds no active key is refused when it is
    loaded (a new set whose keys all wait for their first activation is legal). Two different keys with
    the same 32-bit key id under one origin cannot both sign a checkpoint: the verifier refuses a note
    with two such lines, and every checkpoint of the origin while its trust file lists both, so the
    collision is looked for before a key is activated and reported with both fingerprints. A set that
    is full (16 keys) needs a new generation of the trust file under a new origin prefix. The plain key
    file is read and written from memory that is zeroed; the documentation of the package names where a
    secret cannot be.
94. **Logs: what the store and the reader guarantee (4.4, 4.5; adds to 48).** A reader checks the
    length of every tile it is handed, so a store that lies gives an error. A tile is published by a
    hard link only: where one cannot be made the put fails with an error that says the publication
    directory needs a file system with hard links, and nothing is ever renamed over a tile. A put
    flushes the file, its directory, and the parent of every directory it created, up to the root of
    the store. A checkpoint whose rename another program blocks on Windows is retried for a time the
    store sets (three seconds by default) and then fails with an error that says a retry may succeed,
    with the old checkpoint whole. The text of a checkpoint is refused when writing whenever it would be
    refused when reading. A request for the tiles of a size above 2^28 is refused, and files are read
    with a limit and with a check that the file opened is the file examined. Signing a stored
    checkpoint again takes the keys the caller trusts and adds no line to a note none of whose lines
    verifies.
95. **WORM: the window that was applied, a generation of the key, later versions (6.3; adds to 89).** A
    version that a call wrote is accepted only when the read-back returns the metadata of the attempt
    exactly as it was sent; a store that drops, changes or repeats it is refused (`no_user_metadata`,
    permanent: the object is written and locked, and the operator uses a store or a gateway that keeps
    user metadata). The attempt of a found version must be exactly one value in the form the library
    writes; anything else is an error for that version (`attempt_malformed`), never "absent". The
    evidence states the window that was applied: `attempt` is the time that was used, or null for a
    version that had none, whose time is then bounded only by `submit_start`; a caller warns on null.
    An object that carries its attempt and lies outside its window (a clock fault of the cluster during
    one call, a put that a proxy carried out late) is refused on every later call, and the text says
    that neither waiting nor a Retry heals it. The way out for one record is the generation of the key:
    generation 0 is the key as it was (`<size>.note`), generation n is `<size>.g<n>.note`, the caller
    keeps it with the record and raises it by one, with a new start, only on an operator's explicit
    choice after a permanent refusal for time, never by itself, because each generation is one more
    locked object that claims to be the anchor of that checkpoint; the evidence records it. When the
    latest version with our bytes fails its checks, the other versions are examined and the first of
    ours that passes completes the record, so a copy placed after ours by another principal cannot end
    it. A found version that is older than its own retention period gets its retention extended to now
    plus the configured days and is verified again.
96. **OpenTimestamps: what is kept (6.5; adds to 90 and 91).** The pending attestation of a merged
    claim is kept in every configuration, so that a source of headers that is configured later can
    still have the calendar asked; without a source no request is made for it. A claim that was dropped
    for good is remembered in the evidence (calendar, height, root, time; at most 32, the oldest
    forgotten first), and the same claim answered again is put aside before it enters a fragment. The
    error of a contradicted claim carries the evidence from before the call; the caller keeps it and
    passes it on a Retry, which then keeps the proof and the time of the submission. The factor of the
    work rule is at most 2^20, which never admits difficulty 1 at a height of today; the package
    reports the height and the block time of the newest entry of its table, the table is extended at
    each release, and the caller warns when it is old.
97. **RFC 3161: the pin in stored evidence, and the corpus of notes (6.4, 6.8; adds to 92).** The
    verifiers of a stored token and of stored evidence take the pin of the signer; the evidence
    carries the SHA-256 of the signer's public key and is checked against the token; every certificate
    of a token that matches the signer identifier is tried. The name check can refuse an authority
    whose certificate has characters outside its declared string type or another normalisation; that
    refusal is permanent, and the operator uses another authority. The corpus of notes of 92 is held to
    the real parsers of the verifier by a copy in the verifier module, and a test of the library fails
    when the two copies differ.
98. **What `ledger_enable` adds to the limit, and the timer after an error (replaces "gets a new period
    every thousand rows" of 67; adds to 76).** The time that `ledger_enable` really spent entering a
    batch of rows is added to the deadline, and nothing else is. It is added only while the lock of
    the registry is the only lock of the extension that the transaction holds: with the lock of the
    builders, the verifications or the removals held there is no addition at all. It is kept per entry
    of the subtransaction stack and taken back when the subtransaction aborts, and the rows of a table
    are credited once in a transaction, so entering the same rows again after a rollback buys nothing.
    The deadline is therefore never more than one period ahead, and a lock taken now is held for one
    period at most. What remains, and is stated as an exception: a role that really enters many rows of
    many tables holds the lock of the registry while it does, which blocks other calls of
    `ledger_enable` and a write by hand, and not the block builder. An honest `ledger_enable` whose
    single batch of a thousand rows takes longer than the period is ended. PostgreSQL disables every
    timeout of a backend when an error reaches its main loop, before it aborts a subtransaction; the
    extension starts its timer again at the deadline in the abort of the subtransaction, so an error
    inside a savepoint does not leave a lock of the parent without a limit. The log line of an ended
    session lists the locks held at that moment.
99. **The order of the locks is enforced across calls (replaces 78).** A call that would take a lock
    of the extension that is earlier in the order (builders, registry, verifications, removals) than
    one the transaction already holds fails at once with `KL006` and a hint that names the order,
    instead of waiting into a deadlock; a lock that is already held is never refused. It holds for
    every role and within one statement, one transaction or one savepoint: a caller runs the writers
    in that order or in transactions of their own. `ledger_remove_stray` and a write by hand take all
    four locks first, also a write by hand into the entries. The table lock of `ledger_enable` against
    ordinary statements on the same table after a write by hand cannot be ordered this way; the
    deadlock detector ends one of the two.
100. **Who can drop the database, and identifiers at the edges (adds to 74 and 77).** `ledger_status`
    also lists the roles that cannot drop the database now and can make themselves able to without a
    superuser (`database_potential_droppers`): a role with ADMIN OPTION, directly or through a
    membership, on a role that can, which covers a role with CREATEROLE for the roles it created;
    `ledger_enable` warns about them in a sentence of its own. A database owned by
    `pg_database_owner` can be dropped by every role that owns a database of the cluster, and the
    lists say so. A delete by hand of a recorded digest is recorded as a removal, so its identifier
    does not come back, and `ledger_verify` reports it. At the largest value of the type the writers
    raise an error that says no identifier is left, and nothing wraps. A cancel inside the run of
    `ledger_verify` within `ledger_verify_recorded` is raised when the lock of the verifications is
    held by another session and recorded as a failed run when it is free.
101. **Key id collisions are judged as the verifier judges them (3; replaces the sentence about
    collisions of 93).** The verifier checks a fresh checkpoint against the usable keys of the trust
    file and a held note (a state file, `--old`) against every key of the file, whatever its standing.
    Two keys with the same 32-bit key id under one origin therefore stop every auditor's state file for
    that log also when one of them is revoked or retired, and no later transition cures it: revoking is
    not a remedy. The keys to check before a key enters the set are all keys of the set under every
    origin that will be signed (`AddChecked` refuses such a key; `CheckSetKeyIDs` applies the rule to a
    set, with and without the next keys). When a collision is found after the fact, the log is
    published under another origin (a new epoch or prefix), or the auditors start a new state file, or
    a new generation of the trust file is made.
102. **Times of key transitions, and what the set refuses (3; adds to 93).** Every time of a key is in
    the years 1 to 9999 and never zero; such a time is refused when it is given and when a stored set
    is loaded, since the verifier cannot read the trust file otherwise. An activation later than the
    latest time the set records by more than 366 days is refused, which catches a clock that is wrong
    by years and nothing smaller. A key that is active and not yet in force can be retired without a
    revocation, at its own activation time, so that an activation in the future does not wedge the
    overlap; the rules for who may sign, who may be retired and who is due agree about which keys are
    in force. A loaded set with two active keys must carry exactly one end of an overlap, equal to the
    activation of the other key plus whole days; a set whose keys were all turned back to "next"
    cannot be told from a new one.
103. **Publication: a repeated put, link errors, re-signing (4.4, 4.5; adds to 94).** A put that finds
    the tile there and equal flushes the file, its directory and every ancestor up to the root before
    it returns, so appending the same leaves again after a crash makes them durable. Only the errors
    that mean a file system without hard links are reported as that; any other failure of the link is
    returned as it is. A checkpoint target that is a directory, or read-only on Windows, is named at
    once without the retry. The stores and the audit honour the caller's context. Signing a stored
    checkpoint again takes every key of the trust file as trusted, refuses a collision between a new
    signer and any of them, and refuses a line under the name of the log whose key id belongs to none
    of them, so that the result is never a note the verifier refuses for a reason that could be seen.
    The tests of both libraries that need the verifier say so when they skip, and fail with
    `KINGRES_REQUIRE_VERIFY=1`.
104. **A state must not carry a temporary name of the program (7.4; adds to 87).** `--state FILE` is
    refused with status 2, before anything is read, written or removed, when the resolved name of FILE
    has the form of the program's own temporary files. The cleanup of leftovers under the lock removes
    every regular file of that form, so a state with such a name was removed before it was read, and a
    rollback served to that run was then reported as verified. Skipping only the run's own file would
    not be enough: a run of another state in the same directory would still delete it. The cleanup
    also never removes the file the run was told to use or its lock.
105. **WORM and the record (6.3, 6.6; adds to 95).** The refusal for a store that does not keep user
    metadata says what holds for each case: when the header was dropped, a Retry finds the object and
    accepts it under the wide window, and the evidence says `attempt: null`; when it came back changed
    or repeated, a Retry is refused for good, a new generation does not help, and only another store
    or gateway does. The Test button writes its probe with the attempt header and requires the echo,
    compares the time the store states for the probe with the clock within the tolerance, and tries
    the retention call on the probe in the configured mode (a warning when it is refused: the repair
    path needs it); the probe leaves one empty version under `<prefix>/_probe/`, locked for ten
    minutes. A refusal whose verdict is final (an object that carries its attempt and lies outside its
    window) fails the record at once, not after three waits. The row of a record holds the start of
    the first submit and the generation; the payload of a call is built from the record; a new
    generation is exactly one step with a start later than the old one, written together with the
    state, and still only an operator's choice. When no version with our bytes passed and the walk
    over the versions stopped at its bound (25), the error says that our own version may be hidden
    behind later copies and does not send the operator to a new generation.
106. **OpenTimestamps: bounds per calendar, memory across a Retry, the age of the table (6.5; adds to
    96).** The claims kept aside and the memory of dropped claims are bounded for each calendar (8 and
    32 entries; 128 and 512 in all). A calendar over its own share loses its own surplus claims and
    never a claim of another calendar; a claim contradicted for the first time is never dropped
    because a list is full. An answer of one calendar with more than eight Bitcoin attestations is
    refused whole as an implausible answer of that calendar. A new submission from prior evidence
    carries the claims kept aside that still name an attestation of a kept fragment, and the claims
    dropped; a target that can resume from prior evidence says so through an optional interface, and
    a Retry of a record that holds evidence goes through it. The package gives the age of the newest
    entry of its table of real headers against a time the caller states; the daemon warns when it is
    older than a year, and extending the table is a duty of every release (it is older than a year at
    the time of this correction).
107. **Keys and the log: who vouches for a note, and the times a set records (5.3, 5.4; adds to 93,
    94 and 101 to 103).** Signing a stored checkpoint again takes the entries of the trust file with
    the standing of each key, and is refused when no line of the note that verifies is of a key the
    verifier counts for a fresh checkpoint of that size: a line of a revoked key, or of a retired key
    whose bound is below the size, vouches for nothing, and the signature of a good key beside it
    would make the note genuine. A line of such a key beside a line of a key that counts is kept; the
    window of a key is not applied, since the package has no clock. The checked form of adding a key
    checks the new key against every origin and no longer refuses it for a collision between two
    older keys; building the trust file refuses such a collision. Every operation that records a
    time (add, activate, retire, revoke) refuses a time later than the latest time the set records
    plus 366 days, so that no operation can record a far time that moves the limit of the next one;
    a set that was idle for longer is brought forward in steps with that limit as the time, and a
    revocation is never delayed for it. The times of a key are in the order a key can have
    (creation, activation, retirement or revocation). A key that is active in the record but whose
    activation has not come gives an error of its own, which is not the alarm of a damaged set, and
    a caller that signs treats both as "do not sign". A next key that waited longer than 366 days is
    replaced in a fixed order: add a fresh key, activate it, then revoke the stale one. A link that
    is refused with EPERM is reported as a directory that takes no new links, which is wider than a
    file system without hard links. Both packages state the duties of the caller in one place.
108. **A role that is not a superuser takes a lock of the ledger only in a statement of its own
    (replaces 52, the remedy of 67, 75, 76 and the timer part of 98).** Three rounds patched the limit
    on holding a lock across statements (a prepared transaction kept the lock, a renewal outlived
    rolled-back work, the timer was lost on a second error in an aborted subtransaction), so the cause
    is removed. Every function that takes a lock of the extension (the builders, `ledger_enable`,
    `ledger_register_table`, `ledger_verify_recorded`) is refused with `25001` for such a role in a
    transaction block, a multi-command string, a savepoint, an EXCEPTION block or any other
    subtransaction, and after the first statement of a pipeline; the check is made again for every
    lock that is granted. A superuser and the worker are not restricted. The lock therefore lives for
    one statement of an autocommit transaction, and every error that reaches the main loop ends that
    transaction and releases it. `lock_hold_limit` bounds that one statement. The only code of
    PostgreSQL that disables every timeout of a backend is the error handler of the main loop; the
    abort of a subtransaction only reschedules, and a statement timeout, a lock timeout and a cancel
    are errors of one of those two kinds, so no path loses the timer while the lock is held. A client
    in pipeline mode could open a transaction block after the call; while the timer is armed BEGIN,
    SAVEPOINT, RELEASE, ROLLBACK TO and PREPARE are refused. The tracking of the timer per
    subtransaction and the refusal of PREPARE for a holder are removed; the list of held locks stays
    for the order check of 99. An application that enabled a table inside the transaction of a
    migration runs `ledger_enable` as a statement of its own after that transaction, or as a superuser.
109. **What `ledger_enable` adds to the limit (replaces the credit rules of 98).** The time really spent
    entering a batch of rows is added to the deadline while the lock of the registry is the only lock
    held, and the total for one transaction is bounded by `kingres_ledger.enable_credit_limit`, a
    setting of a superuser (30 minutes; 0 for no bound). The call that would pass it fails with
    `54000` and the hint that a table which needs longer is enabled by a superuser. The exception that
    remains: a role with a very large table holds the lock of the registry for up to the limit plus
    that bound, which blocks other calls of `ledger_enable` and a write by hand, and neither the block
    builder nor the capture.
110. **The order of the locks in both directions, and who can drop the database (adds to 99 and
    100).** Allowed in one transaction of a superuser: the lock of the builders, then the registry's,
    then the verifications'; a write by hand or `ledger_remove_stray` first. Refused with `KL006`: a
    builder after `ledger_enable` or after a recorded verification; `ledger_enable` after a recorded
    verification; a write by hand or `ledger_remove_stray` after either. The check runs before
    `ledger_enable` waits for the lock of its table. Members of `pg_execute_server_program` and of
    `pg_write_server_files` (directly, through a chain, or by `SET ROLE`) can run any command or write
    any file of the server and are listed among the roles that can drop the database; roles with ADMIN
    OPTION on those two are listed among those that can make themselves able to. The other predefined
    roles of 18 and 19 are not listed, each for a reason the README gives.
111. **The identifiers of the three tables of the client signatures (3.7, 8.1; adds to 66 and 77).**
    The tables of keys, revocations and attestations are registered in `ledger_table` with the next
    free identifier under the lock of the registry (the maximum plus one, no sequence): 1 to 3 in a
    new database, after the user's tables in a database that is upgraded. The script cannot name the
    number, because the capture trigger carries it as its argument, so `ledger_register_internal`
    arms the trigger itself, and the hook lets through exactly the two statements it issues, for one
    of the three names, only while the install or upgrade script of the extension runs. Any other
    statement, and the same text at any other time, is refused as before. No code, test or verifier
    may assume a number: the tables are found by name.
112. **What "a table under ledger control" means (adds to 51).** The three tables are in the
    registry from the start, so an empty registry no longer means "no ledger". The rules that hold
    "while tables are under ledger control" (a drop through a dependency, `DROP EXTENSION`,
    `DROP SCHEMA`, `DROP OWNED`) count the three tables only while they hold a row (a key, a
    revocation, an attestation), because those rows are entries of the chain. A database with no
    ledger table of a user and no row in the three drops the extension as before; once a key is
    registered nothing drops it. The tables themselves are catalog tables of the ledger from the
    moment they exist: `DROP`, `TRUNCATE`, `ALTER`, `LOCK`, a foreign key, a `GRANT` other than
    `SELECT`, a trigger or an index on them is refused.
113. **The switch to `required` has a date, and it is the one update of the registry (3.7).**
    Without a date `ledger_verify` would report every older entry of a table for ever. `ledger_table`
    has a column `signature_required_from` (the top-level transaction that switched the policy;
    NULL while it is optional), tied to the policy by a constraint. `unattested_entry` covers the
    entries of transactions after that one; the switching transaction itself is covered by the check
    at commit. A transaction that was already running at the switch can be reported once. The switch
    is the only UPDATE of a catalog table of the ledger that the code of the extension makes: a fixed
    statement under the marker of the guard, and the blocking trigger lets through exactly "optional
    to required, with the date set from NULL". Neither value is hashed, so a superuser who edits the
    catalog can switch it back; the README says so.
114. **Arguments and errors of the new functions (3.7).** Beside `KL010` to `KL014`: a malformed
    length, an operation other than I or D, a negative table identifier or an unknown policy is
    `22023`, and a signature that is not 64 bytes is `KL013`; a NULL argument is `22004` (a call that
    does nothing must not look done); a relation not under ledger control is `42P01`, a table of the
    ledger itself `0A000`, a table the caller does not own `42501`; a key registered twice, revoked
    or not, is `23505`, an unknown role `42704`, a label or reason over 200 characters `22001`;
    revoking an unknown or an already revoked key is `KL012`. `KL011` also covers "every entry for
    this row version is attested already". `ledger_attest` attests the entry with the lowest `seq`
    for that table, operation and hash that has no attestation yet, so two identical rows in one
    transaction need two calls, and an entry is attested once.
115. **What the key check reads (3.7, 3.8).** The key and its revocation are read with the latest
    snapshot, whatever the isolation level of the caller: a key whose revocation is committed is
    refused to a transaction that began before the revocation. The role of a key is compared with
    the user the call is made as, so `SET ROLE` counts and membership does not. The check at the
    call is a convenience; the rule of the verifier by block height decides.
116. **`ledger_status` (8.1).** `append_only_tables` and `updatable_tables` keep their meaning, the
    tables of users only. The three columns of 8.1 are appended at the end: `client_keys_active`,
    `attestations`, `required_tables`. A reader that counts the rows of `ledger_table` counts the
    three tables of the extension too and must leave them out.
117. **No lock, and no statement of its own (adds to 108).** `ledger_client_key_register`,
    `ledger_client_key_revoke`, `ledger_attest` and `ledger_set_signature_policy` take no lock of
    the extension, since no identifier is allocated, so rule 108 does not apply to them, and
    `ledger_attest` has to run in the writing transaction: in a block, after a savepoint, in an
    EXCEPTION block. `ledger_register_internal` takes the lock of the registry like
    `ledger_register_table`, and only the scripts can call it.
118. **Cost (9.7; replaces "tens of microseconds").** One Ed25519 verification takes about 90
    microseconds on the build machine; a call of `ledger_attest` adds about 175 microseconds to the
    insert it attests (with its statements prepared and kept; run as text they added about 400). A
    write without an attestation is unchanged, except that the first write of a table in a
    transaction reads the policy in the same statement as the identifier.
119. **What `ledger_verify` checks (3.7).** The three tables are compared with their entries like
    every ledger table, so a deleted, changed or inserted row is found. New checks:
    `attestation_duplicate`, `client_key_unknown` (also for a revocation of a key that is not
    registered), `client_key_id` (a key row whose identifier is not the hash of its public key),
    `attestation_orphan`, `unattested_entry`. `ledger_verify` does not verify signatures; that is
    the work of the standalone verifier, from the proof of the entry and of the key.
120. **A server without OpenSSL (3.7).** Only `ledger_attest` needs OpenSSL and raises `0A000`
    without it; the registry, the message, the policy and every check work. A table whose policy is
    `required` cannot be written on such a server (`KL010`).
121. **The origin prefix is locked by the first key (3.2, 4.2, 8.4).** The event of a new key is
    signed under the prefix, and an event cannot be signed again under another name, so the prefix is
    locked once a key or a checkpoint exists, not only after the first checkpoint (409
    `origin_prefix_locked`). Before the first key it is free; the console shows the default
    (`<server address>/kingres-ledger`) for confirmation before the first key is created.
122. **The migration and its runner (8.2, 10).** The migration is 0030 (0008 was taken long ago, and
    0015 to 0029 are left to failover). The runner applies every migration it has not recorded, in
    the order of the versions, allows gaps, and refuses a database that holds a version the binary
    does not know.
123. **`anchorLedgerNow` answers 201 with the records (8.4).** A 202 of the contract is always a task,
    and the submission is made by the job.
124. **Subjects of the key tasks (6.7, 8.4).** The kinds `ledger.key.*` have the subject
    `ledgerKey`/`signing`, so one key task runs at a time in the cluster; retiring and revoking are
    kinds of their own. `ledger.bundle.create`, `ledger.epoch.start` and `ledger.upgrade` have a
    database as their subject.
125. **`compromised-since` is clamped (3.3).** It is never later than the time of the revocation and
    never before the creation of the key; the event and the record carry the clamped value.
126. **A revocation never creates a key (3.3; replaces "the dialog generates one in the same
    task").** With no other active key in force the answer is 409 `last_signing_key`: a key made by
    the revocation would be a key that no key authorises, which is the discontinuity of 9.2 and is
    not built yet.
127. **Holders (3.2.2).** The leader and the servers that hold an active node of group 0 in the
    catalog of the system database. A server stays on record until its files were removed. A holder
    that cannot be reached does not fail a creation (it is named in the detail of the task and the
    distribution job retries); for a retirement and a revocation it fails the task with
    `key_files_remain`, which can be retried.
128. **The bound of a retired key (3.3; adds to 93).** At most the largest size of a stored
    checkpoint, and at least 1; it is written in the event of the retirement and read back by the
    step that retires. A key that signed nothing is bound to 1.
129. **Signing and retiring exclude each other (a duty of step 5).** The retirement reads the
    largest checkpoint size in its first step; a checkpoint signed afterwards by the retiring key
    beyond that size would be refused by the verifier.
130. **Adoption after a crash.** A key file with no row is adopted by a repeated step only if it was
    modified after the task was created, less the allowed clock difference. A next key with its file
    and no event is adopted unless it is older than 300 days.
131. **Passphrases.** A sealed key file is opened with the passphrase of the holder that has it and
    written again under the sealing of the holder that receives it; a plain file is accepted by a
    holder that seals. A holder that cannot open a file answers 409 `passphrase_mismatch`, and the
    task names the server.
132. **The trust file.** It is built without next keys. `getLedgerTrustFile` answers 503 when the
    stored trust file differs from the one built from the set.
133. **Columns beyond the list of 8.2.** `ledger_cluster`: `keys_revision`, `trust_file`, `holders`,
    `backup_advised`, `discontinuity`; `ledger_signing_key`: `label`, `overlap_ends`,
    `max_tree_size`, `task_id`; `ledger_event`: `type`, `subject`, unique together. The request that
    creates a key can say that a backup was made, which clears the reminder. A key operation on a
    server without a term answers 503 with `Retry-After`; a task step that meets another term ends
    with the task error `term_over`, which can be retried.
134. **`ledgerSignerUnavailable` (6.9).** It is also raised for a damaged key set, a trust file that
    differs, and a missing key file of an active or next key. Step 5 adds "no checkpoint signed for
    two rounds".
135. **A stale next key** stays in the set and takes one of the 16 places until it is revoked.
136. **A forwarded answer keeps its `Content-Disposition`**, which the download of the trust file
    through a follower needs.

137. **The order of a round and what a checkpoint may be (replaces step 5 of 4.5).** Each write of a
    round is a transaction of its own that begins with the term guard, in the order: append,
    frontier, signature, checkpoint. A checkpoint is refused when it is not for the stored size and
    root, when it is for a smaller tree, when it would be a second root for a size, or when the log
    is not active.
138. **Signing and retiring exclude each other (the duty of 129), as one mechanism.** A key whose
    `key-retired` event exists is not offered for signing. A checkpoint is stored only while the
    revision of the key set equals the one the signers were read at, and every change of the key
    set, an event included, raises the revision. Both commits of a retirement carry the bound and
    check it under the guard; the step that retires ends with `bound_exceeded` when a checkpoint
    above the bound was stored meanwhile.
139. **The first checkpoint belongs to the collector.** The store of the keys never writes the time
    of the first checkpoint back and never changes the origin prefix once it is set: a key change
    that had read the row before the first checkpoint was stored would have unlocked the prefix.
140. **A deadlock or a serialization failure is "busy", never a conflict.** Otherwise a key task
    that commits at the same moment would stop a log as diverged.
141. **What is a divergence (4.7).** A head below the published size; a head block that is not the
    published block at its height; a first block read that is not the last published one; an entry
    that is not a header, or blocks that do not link; tiles that differ. A diverged log never wakes
    up, also when the chain returns to the published history. A group that now holds another chain
    gets `chain-replaced`: the old log is retired and a new one is made.
142. **The aggregate leaf holds a time**, so it is stored in the row of the log before it is
    appended, and the repair after a crash appends the same bytes.
143. **The aggregate log is never closed (replaces the sentence of 5.5).** A dropped database ends
    with one leaf in which every member is `retired`; a database of the same name continues the log.
    `retired` appears in exactly one leaf per chain, and the next leaf leaves the chain out.
144. **`ledger/read` names the group.** A group that gives an error, or has no ledger or no
    database, is "not covered": its members are `stale`, the digest says `complete: false` and names
    it. When no group answers, the answer is 503.
145. **Digests.** `listLedgerDigests` lists the stored views, with identifiers of their own; the
    older digests of the extension stay in each database and are not listed. `generateLedgerDigest`
    waits up to 20 seconds for the term of a new leader before it answers 503.
146. **The listener** serves whatever is stored while it runs, also when the publication is switched
    off. `keyring.json` is the trust file. Partial tiles are kept (no pruning after 24 hours). 429
    per address, 503 at the cap of concurrent requests.
147. **The event `chain-retired`.** Its subject is `<chain>:<epoch>`; its lines are `chain`, `epoch`,
    `final-height`, `final-block-hash` and `reason`, one of `dropped`, `group-removed`,
    `database-missing` and `chain-replaced`.
148. **A database that disappears without `dropDatabase` (decided here, as section 10 left it to
    step 6).** A database can be dropped from SQL by its owner, where no hook of the control plane
    sees it. When every group answers "no database" in two rounds in a row, the collector retires
    its chains with the reason `database-missing`: a signed event, one leaf with `retired` members,
    closed logs, no divergence alert; the event states the last published head, since the database
    is gone. An auditor tells it from a rollback, which stays `diverged` with a critical alert and
    the old head. One group that answers "no database" while others answer is only a group that was
    not read. Rejected: changing who owns databases or revoking the right to drop (it changes
    existing databases); an event trigger (`DROP DATABASE` fires none); treating it as a rollback (a
    deletion could not be told from a restore); doing nothing (a log that is stale for ever).
149. **Limits and ownership.** A round appends at most 9 999 blocks (the first block read is the
    last published one), and rounds run one database after another. The alert
    `ledgerSignerUnavailable` has one owner, the check job of the key service, which also asks the
    collector after two rounds that could not sign.

150. **The switch to `required` is ordered with the writers by a table lock (replaces what 113 says
    about a transaction that was running; adds to 108, 110 and 117).** `ledger_set_signature_policy`
    takes SHARE ROW EXCLUSIVE on the table before it reads or changes anything and holds it to the
    end of its transaction, so it waits for every open writer of the table, a prepared one included,
    and later writers wait for it. After the switch has committed, no transaction commits or
    prepares an entry of the table without an attestation, whatever its isolation level and
    whenever it began.
151. **A writer reads the policy with a snapshot of now and keeps only that answer (adds to 113).**
    The capture trigger reads the policy at the first write of a table in a transaction with the
    latest snapshot, not the transaction's own; since the writer then holds its lock on the table,
    the answer is true until the transaction ends. An answer read by anything that does not hold
    the lock (`ledger_row_hash`, `ledger_attest`, a check) is never kept for a writer.
152. **The date of the switch (replaces "the top-level transaction that switched the policy" of
    113).** `signature_required_from` is the largest transaction id that is not asked for an
    attestation: the id the next transaction would get, read under the lock, less one. Every
    transaction that wrote the table before the switch has a lower id, so `ledger_verify` reports no
    entry that was committed before the switch. A transaction that began before the date and wrote
    after the switch is held by the check at commit only; `ledger_verify` does not look for its
    entries. The column, its constraint and the comparison are unchanged.
153. **The entries of the switching transaction (3.7; replaces the claim of 113).** Its own earlier
    entries of the table without an attestation are owed one: they are put on the list of the
    commit check when the switch runs and follow the subtransaction that made it. A switch in the
    transaction that enabled a table with rows owes an attestation for every row entered; switch in
    a later transaction.
154. **`ledger_set_signature_policy` under the rules of 108 (replaces 117 for this function).** A
    role that is not a superuser calls it as a statement of its own (`25001` otherwise); its table
    lock is in the list of held locks for the timer; the wait for the writers is bounded by
    `lock_hold_limit` (`55P03`); a transaction that holds a lock of the extension and would have to
    wait for the table gets `KL006` before it waits; the lock on a table comes first in the order
    of the locks. No lock of the extension is taken, so the builder and the capture never wait for
    it; only the writers of that one table do.
155. **Which keys the registry accepts (3.7, 3.8; adds to 114).** Only the canonical encoding of a
    point of the curve that does not have small order: `22023` at registration, and `KL012` at
    `ledger_attest` for a row that exists already. Plain Ed25519 verification accepts a signature
    that nobody made under a key of small order, and several encodings give the same point. The
    check is the extension's own arithmetic (canonical, on the curve, eight times the point is not
    the identity), with and without OpenSSL. The standalone verifier must apply the same three
    tests to every public key it reads.
156. **The default of `enable_credit_limit` (replaces the 30 minutes of 109).** One minute, so that
    with the defaults a role that is not a superuser holds the lock of the registry for two minutes
    at most. A table that needs longer is enabled by a superuser or by a role whose settings were
    raised.
157. **Copies of a database (3.6).** A restored backup, a promoted standby and a copy made from a
    template share the chain identity, so an attestation is valid in each copy. No new identity is
    made for a copy.
158. **What the policy does not cover (3.7).** `COPY` goes through the capture trigger like every
    write. A ledger table is never partitioned, so there is no partitioned case. A subscriber of
    logical replication cannot hold a table whose policy is `required` (the apply fails with
    `KL010`). A commit callback of another module that was registered before this library's could
    write after the check.

The number 159 is not used.

160. **The inputs of `attestation` (7.4).** The proof of the attestation row is a proof of the usual
    format in a file of its own, with the columns file of the row. The verifier cannot know which
    table is the table of attestations (111: no number is assumed), so the table identifiers of the
    attestations and of the revocations are flags, and the output says that they are claims of the
    server; the signature under a key the auditor pinned is what a row of another table cannot have.
    `bundle.json` names the columns file beside the attestation file.
161. **The attestation row is in the block of its entry (3.7).** `ledger_attest` runs in the
    transaction of the entry and a transaction is in one block, so the attestation row must name the
    transaction and the sequence number of the entry and be in the same block.
162. **The flags of an aggregate member (5.5, 5.6).** `diverged` is status 1: the publisher says the
    chain was rolled back or rewritten, and a status of 0 would hide it from a script. `stale` is
    status 3. `retired` needs a `chain-retired` event that the trust chain authorises and that names
    the same final height and block hash: no such event is status 3, an event that no authorised key
    signed is status 1, another head is status 1. A member is checked against the signed checkpoint
    of the chain log it names, whose size may be larger than the member's.
163. **The state of `monitor` is a directory (7.5).** One file per log in the form of
    `checkpoint --state`, so every property the reviews of the verifier established holds for it,
    and one file for what is not a checkpoint (the audited size of each log, a digest of the text of
    each event, the chains that were retired). An event is identified by the text of its note, never
    by its signature lines. A run stops at the first finding.
164. **Sources (4.4, 7.4).** An HTTP source follows no redirect and uses no proxy; plain `http` is
    for this machine only; a server that cannot be reached is status 3. An HTTP source cannot list a
    directory, so the epoch of a chain log is given and the tile directories of older sizes are not
    checked, which a note says. An archive is read under limits.
165. **The authority of an event (3.3, 3.4).** An event counts when a key signed it that is, at its
    number, a key of the trust file that the file does not call revoked, or a key that an earlier
    verified event added, and that no earlier event retired or revoked. A key the file calls revoked
    never vouches for an event, so `trust follow` starts from the file the auditor pinned. The
    validity windows are not applied to events. A `key-added` event is signed by an authorised key
    other than the new one and by the new key; a `key-retired` and a `key-revoked` event by an
    authorised key other than the one they are about.
166. **`bundle verify` checks every file of the tile directories (7.3)**, not only those the
    checkpoint needs: a partial tile or entry bundle of an older size must be a prefix of the tile or
    bundle of the signed tree, and a file the tree does not have is a failure.
167. **The three tests of a public key (155)** are applied to every key the verifier reads: the
    pinned server keys, the client keys, and the key of a `key-added` event.
168. **A revoked key is revoked (3.3).** The rule that old checkpoints of a revoked key may stay good
    when an anchor dates them needs anchors; until the verifier reads anchors, `trust follow` writes
    `revoked`, and a held checkpoint rests on the rules of 55 and 69.
169. **The log leg (4.10, 7.4).** A block at a height the signed chain log does not cover is a
    failure. The leg through the aggregate shows the block through the member of the last leaf of
    the signed aggregate; its root of trust is the signature on the aggregate checkpoint, and the
    output says that no anchor was checked.
170. **`bundle fetch` (4.6, 7.4)** needs the logs named: `logs.json` is optional and has no format
    yet. A partial tile that is gone is not an error. Without a trust file nothing is verified and
    the word is `FETCHED`, not `VERIFIED`.
171. **What was published is kept outside the system database (5.x, P8-01).** A restore of the system
    database to an older state made the publisher sign a second history. Every holder of the key
    files keeps a record (`published.json` in the key directory: the highest size and root of every
    log, the number of events and the hash of the last one, with a checksum). A round, a retirement
    and a key task that appends an event first compare the database with the record; a database
    that is behind it or disagrees with it signs and writes nothing, and a critical alert is raised.
    The leader sends the record to the holders every ten seconds, and a server without one asks the
    others before it takes the database as the start. The record is not signed: who can write the
    key directory can sign. The limit: a restore when every copy of the record is lost or older
    than the publication is not detected by the publisher, only by a mirror or an auditor.
172. **A retirement (3.x, P8-02)** is refused only for a checkpoint above the bound that carries the
    signature of the retiring key, not for any checkpoint above it; the other key keeps signing.
173. **A database that vanished (147, P8-03)** ends its chains after every shard group answered "no
    such database" for 30 minutes and the system instance does not have it either, with an alert
    from the first such answer; rounds are not counted. A chain that was closed and answers again
    under the same chain identity is not reopened: a critical `ledgerChainDiverged` names the
    group, the height and why the log ended, and no leaf, event or tile is written. Reopening needs
    the epoch of step 20.
174. **An answer that names the open chain of another group (P8-04)** makes both logs diverged with
    critical alerts and retires nothing; a group whose chain was really replaced raises a critical
    alert too.
175. **Small rules (P8-05 to P8-07).** The table of the rate limit evicts its oldest entry and
    counts an IPv6 client by its /64. A checkpoint note is replaced only by a note with the same
    text under the same revision of the key set. The marker that lets a key file without a row be
    adopted names the task and the fingerprint; it is no protection against someone who can write
    the directory.
176. **A diverged chain that is retired stays diverged (P8-08)**: the flags of its last leaf say
    both (0x06), and its alert is not cleared. This is the only change of published bytes in this
    round.
177. **Decisions stated and not changed (P8-10)**: the caching headers of files that cannot change;
    an aggregate identifier can be guessed as far as the name of the database can; the certificate
    of the listener is read once; the answer of a peer is bounded; an unknown hash version stops
    the log.
178. **`logs.json` (170)** has a format now: `{"format":1,"logs":[{"kind","origin","state","size"}]}`
    on one line, in the order the logs were made, at the root of a publication.
179. **An anchor record that is claimed (6.x)** is shown as `submitted`: `LedgerAnchor.state` of the
    contract has no `claimed`.
180. **Outgoing requests of anchoring** go only to the host and port of the configured target. An
    address written as a literal is used as written; a name that resolves to a loopback, link-local,
    unspecified or multicast address is refused; a private address is allowed.
181. **Credentials of a target** are kept in files of the key directory (clear, or sealed with
    AES-GCM under the passphrase file), sent to the holders by two internal calls, and never
    returned; each value is at least four printable ASCII characters.
182. **"Behind"** is the age of the oldest checkpoint that no anchor of the target covers, counted
    from no earlier than the time the target was enabled: a warning at three intervals and one
    hour, critical at twelve intervals and one hour.
183. **`retryLedgerAnchor` never raises the generation of a WORM object**; the contract does not
    offer it.
184. **The evidence format `tsr`** is the DER of the time-stamp token (the CMS structure of the
    evidence), not a whole time-stamp response, which the cluster does not keep. The bundle of trust
    anchors of a time-stamp target is for the token; the TLS connection to the authority is checked
    against the system roots.
185. **The OpenTimestamps target has a member `headerSource`** that names an explorer which answers
    block headers (http or https). With it a claim can become anchored on a header that confirms
    it; without it a claim stays a claim. Its host is one of the hosts the egress rule allows. The
    answer is bounded, and the hash it names must be the hash of the header it gives.
186. **Every submitted or claimed OpenTimestamps record has a time for its next look**; a claim that
    the header source contradicts is written into the record's last error (there is no alert kind
    for it); the test of a target and a log line at start warn when the table of real headers is
    older than a year. The outcome "anchored" through a header source is not tested through the
    control plane, since a real header cannot be made in a test.

190. **A verified event that revokes or retires a key counts in every command of the verifier that
    judges a signature**, by the rule of 165 and 168: a checkpoint vouched for only by a key that a
    verified event of the same evidence revoked, or that lies above the bound of a retired key, is a
    failure. A key that an event added is trusted only through `trust follow`, from the file the
    auditor pinned.
191. **The flags of a member of an aggregate** are judged together: `diverged` is always a failure,
    also together with `retired` (0x06); `stale` is "not verified" and is never dropped, also together
    with `retired` (0x03); a retired chain whose own signed log goes past the retirement height is a
    failure. The README of the verifier gives the whole table.
192. **Events are numbered without a gap**; a gap is a failure where the source can list the events.
193. **The 136 signed bytes of an attestation** cover the chain, the table, the operation, the hash
    of the row, the key id and the nonce, not the transaction or the block; the verifier checks that
    the attestation row is an insert of the entry's own transaction in the entry's block by other
    means.
194. **The monitor's state is required**: a missing or emptied state is an error, and a first run is
    asked for with `--init`.
195. **`bundle fetch` is bounded** by a number of files and of bytes when no trust file is given,
    checks the size a checkpoint claims against that bound before it requests a tile, and compares
    what it already holds before it replaces anything.
196. **`anchors`** checks the anchors of a signed aggregate checkpoint. The digest is computed again from
    the signed aggregate log, never taken from an evidence file; for an older size from the first
    leaves, proved a prefix of the signed log.
197. **The statuses of `anchors`.** Not verified (3): a WORM object not fetched again, a time-stamp
    token without pinned certificates, an OpenTimestamps proof without a header given by the auditor
    or only pending, no evidence for the size, an anchored size above the checkpoint served. A failure
    (1): any check that fails, also an operation of a proof the verifier does not compute; whether such
    paths of calendars (RIPEMD-160) should be "not verified" instead is open.
198. **`--require`** names the kinds that must hold a verified anchor; other files that cannot be
    verified are notes; a failure of any file is a failure.
199. **A WORM evidence file** carries the hash and length of the object but not the note, so nothing
    about the object is checked offline; only a fetch by the auditor makes it an anchor.
200. **The time of a time-stamp token** is the authority's word within its accuracy; the revocation of
    the authority's certificate is not read; the certificates are the ones the auditor pinned.
201. **The header of a Bitcoin block** comes from the auditor; the one the cluster kept is never used;
    a header that does not meet its own target, or is easier than difficulty 1, is refused.
202. **A time-stamp token** whose list of digest algorithms differs from its signer's, or that has
    extensions, is refused.
203. **The byte sweeps of the verifier's tests** change a fixed sample by default and every byte with
    `KINGRES_VERIFY_FULL_SWEEP=1`.

187. **A database without writes grows nothing**: no block, digest, log size or event. The 305 blocks
    for five rows of an earlier test were one digest per row of the test helper; a digest closes a
    block.
188. **The fixture of a whole publication** is made by the cluster test of a database published on
    two groups, saved twice so that every log has two checkpoints, with one anchored WORM record and
    its evidence; it is regenerated with `-Fixtures` of `run-integration.ps1`.
189. **Process kills inside a round** are tested at every kind of write (digest, log, unread, state,
    retired, an event of the key set, note): after each restart no log is diverged or closed
    wrongly, the logs audit against their tiles and pass the standalone verifier, and the record of
    what was published agrees with the database.
204. **The duties of a WORM target** are reported by its test and the monitor: `retention`,
    `retention-ends` (a warning within 30 days), `retention-ended` (a failure), `evidence-attempt`,
    `evidence-found`, and `server-clock` (the store's clock against the database's: a warning above
    30 seconds, a failure above five minutes).
205. **The limits of retries** are read from the environment (`KINGRES_ANCHOR_MAX_RETRY_ATTEMPTS`,
    `KINGRES_ANCHOR_MAX_RETRY_AGE_HOURS`), since the contract has no setting for them.
206. **Evidence files** are served under the publication root at `a/<id>/anchors/<size>/<target>.json`,
    at that path only and never listed. A log closed because its database was dropped raises no
    alert of a returning chain for 30 minutes, the time the drop task needs between ending the
    publication and dropping the database.

The review of the wiring of anchoring and of the fixes of the publication
(`docs/reviews/2026-11-20-kingresd-anchoring-wiring-and-publication-fixes.md`, P7A-01 to P7A-17) gave:

230. **The restore of the system database** is refused until an operator accepts the loss with a confirmation
    that fits the situation; the acceptance is a signed event (9.4). The aggregate log and the events cannot be
    derived again, the chain logs can. The confirmation is a file in the key directory and not a call of the REST
    contract, because accepting is a decision made at the host with the old record and the restored database
    in view, and a call would have to be authorised against a database that the publisher distrusts.
231. **The list of records that are due** (`Store.Due`) is selected by state and by time in the query and limited for
    each target after that (100 per target and set): the submit job asks for the pending records, the upgrade job for the
    submitted and the claimed ones. A claimed record that waits for a block header no longer takes the place of a
    pending record, and one target with many records no longer takes the places of the others (P7A-01).
232. **The credentials follow the targets.** The job `ledger.anchor.credentials` (60 s, on the leader) fetches the
    files this server lacks from the other holders, gives the holders the files they lack, takes away the files of
    targets that are gone, and keeps what it could not do as an alert (`ledgerAnchorFailed`, subject
    `target:<id>|credentials`: warning when a holder lacks a file, critical when this server lacks it for an enabled
    target). A worker that finds the file missing asks the holders at once. The REST answer of a create or an update
    stays 201 or 200 when the distribution failed (the contract has no member for warnings); the alert comes with the
    next pass of the monitor, 10 s later. The internal calls `credentials/list` and `credentials/fetch` are new (P7A-03).
233. **A target that cannot be built** (no credentials file, a credentials path that cannot be used, a bundle that cannot
    be read) fails its records at once with the code `target_unbuildable` and the cause, which is in the alert; it is
    not retried with a backoff. After the fix the operator uses Retry (P7A-03).
234. **A pass of the submit or upgrade job** starts one worker for each target and waits for them at most 20 s; a target
    that is still being worked on keeps its worker, and the next passes leave it alone and work on the others (P7A-04).
235. **The evidence of a receipt is compared with the record** before the record becomes anchored: for a WORM object
    the key (`<aggregate id>/<size>[.g<generation>].note`), the SHA-256 and the length of the stored note; for OpenTimestamps the
    digest; for a time-stamp token its message imprint. A difference fails the record for good with the code
    `evidence_mismatch` (P7A-05).
236. **One answer of another chain id** does not retire a chain log. The same new id must answer in every round for as
    long as a database must be missing to count as vanished (30 minutes by default); an alert is raised from the first
    answer, the group counts as unread meanwhile, and an answer of the real chain ends the wait. The clock is in memory:
    a restart or a change of leader starts it again, the safe direction (P7A-06, P7A-16).
237. **The result of a call is applied only to the target it was made for:** the write carries the revision of the target
    that the client was built from and is dropped when the target was changed, switched off or deleted since (P7A-07).
238. **A failed record is not an alert once a newer checkpoint of the same log is anchored for the target**, and a Retry of a
    record that a newer record replaced is refused with 409 (P7A-08).
239. **The evidence file `json` is served for an anchored record only** (404 for another state), with the time of the
    anchoring as `created_at`; the format `ots` is served for any record that has evidence, its content says what it proves (P7A-09).
240. **The redactor removes a secret in its URL-escaped, hexadecimal and base64 forms**, and the base64 of `user:password` of
    a time-stamp authority (the pair is a secret of its own). A credential member is at least four characters, the length
    below which the redactor ignores a value; a stored file with a shorter member is no longer read (P7A-10).
241. **A record of what was published that cannot be written** (a full disk, a directory that is gone, a file larger than
    32 MiB, which was never read back) stops all signing and raises the critical alert until it is written again, which is
    tried before the next signature is allowed. The rename is made durable with a sync of the directory (P7A-11, P7A-14).
242. **A `credentialsPath` that cannot be used** says which of three reasons it is: the file does not exist, it can be read by group or others, or its content
    is not the members of the kind (P7A-12).
243. **Two enqueues of one checkpoint** are not an error: the loser of the unique key returns the record of the winner (P7A-13).
244. **Decisions that stand (P7A-15).** `credentialsPath` and `caBundlePath` take any path the daemon can read and a failed read says
    whether the file exists and whether it parses: the configuration of a target is for an administrator, who may already read what the
    daemon reads through the test of a target; the contents of the file are never shown. An address written as an IP is dialled as written,
    link-local and loopback included, as 6.8 documents; the Test of a target lets an administrator reach any host and port the
    configuration names, which is what a target is. `kingres_readonly` reads the configuration of the targets, the last test and the
    last error of every record: those texts hold no credential (the redactor) and the path of a credentials file is not a secret. A probe
    object of the Test of a WORM target stays in a bucket whose default retention is a compliance one and cannot be removed; an operator
    who tests such a bucket often leaves objects behind, and this is stated here and in the README, not prevented.
    `anchor_minimum` is compared with the targets when a target is anchored and again when a checkpoint is anchored; the flag `anchored` of a checkpoint is
    not computed again when the setting changes, because a flag that went from true to false would withdraw a statement that was made.
245. **The tests that the mutants of the review asked for** (P7A-17) exist: the evidence of another database, the evidence of a record
    that has none, the number of iterations of a sealed file, the claim of a record in a process, the thresholds of the behind warning, the start of its clock, a
    receipt whose evidence does not read back, a call cut short by the end of the leadership, the root of a published size, an unreachable group and a damaged record.

Correction 168 (an old checkpoint of a revoked key dated by an anchor) is not applied: the design must
state which kinds of anchor may date a checkpoint, whether the comparison with the time of compromise is
strict, and what it means that an anchor covers a checkpoint of a chain.

Built of step 10: the anchoring framework and the WORM target. Not built in it: the publication of
evidence files under the publication root, the cluster settings for the limits of retries (the
defaults of the library apply), the warnings about retention of a WORM object, the monitoring of the
store's clock, and tests that kill the process inside a round (P8-09 in part). Steps 12 (RFC 3161)
and 14 (OpenTimestamps) register their kinds in the same framework and are built (184 to 186).

Of section 13: item 2 (test vectors) is settled for signed notes (the example key and note of the
specification are in the tests; the checkpoint example has no published key), item 3 (entry bundles at
the standard path) and item 4 (RFC 9162 sections 2.1.3.2 and 2.1.4.2) were checked against the source.

## 15. Notes from building steps 21 and 23 (2026-11-20)

### Step 21: the verifier and the epochs

These notes say what step 21 built, where it differs from the text of the design, and what the design should now say. Plain statements, in the order a reader of 9.4 will want them.

1. The event. The event `epoch-closed` that step 20 publishes has the lines chain, epoch, final-size, final-root, common-height, kind, reason and acknowledged-by. The verifier reads exactly these lines, in any order after seq, time and type, refuses a missing or extra line, a second event for one epoch, a kind other than restore, failover-loss or other, and a number that is not canonical. The publisher writes `common-height: unknown` today, because the highest height whose block is the same in both histories cannot be found once the old blocks are gone from the database. The design says the value is a height or -1. The verifier accepts a height, -1 and `unknown`. The design should say that `unknown` is a legal value and what follows from it: a finding then cannot say how many blocks were lost, only that blocks above the common height existed in the old epoch and are not in the new one. If the publisher ever learns the common height (for example from a mirror that holds the old entries), it can write it, and the findings get sharper without any change in the verifier.

2. What a closed epoch is, to an auditor. Each epoch verifies on its own. The new epoch is a different log with a different origin, built from block 0 of the chain as it is after the restore. It is not an extension of the old one, so no consistency proof between them exists, and none is attempted. What the verifier adds is that the cut is never silent. Three places show it. A member of the aggregate that is flagged diverged, which the publisher also flags retired in the one leaf that closes the epoch, is a failure at step epoch-closed unless the auditor accepts the finding. A round of the aggregate that the auditor holds from before the closing, compared with the last round by `aggregate --previous`, is a failure at step epoch-closed that quotes the event (its number, the kind, who acknowledged, the reason, the number of blocks the old epoch published) and says that the held head is a prefix of the frozen old log, so the old epoch is genuine and the new one is not its continuation. And `consistency` between a checkpoint of epoch 1 and the log of epoch 2 is a failure that says the same thing; there is nothing to accept there, because nothing is proved.

3. The flag. `--accept-epoch-closed SEQ` takes the number of the event. It exists on `aggregate`, `proof --bundle` and `bundle verify`, the commands that can reach a closed epoch while verifying the new one. It turns the failure of that one event into a printed finding and exit status 0. It does not cover a disagreement between the event and the log it closes (final size or root), a held head that is not in the frozen log (a fork), an event that no authorised key signed, a diverged member without an event, or any other event. A number that no finding of the run needed is reported in a note, so that a typo cannot pass for an acceptance. The design said the flag turns the failure into a finding for a verification of the new epoch alone. That is what was built; the design should add that the finding is a step of its own (`finding epoch-closed`) and an entry of the JSON output, never only a line of text.

4. The new epoch alone. `aggregate` of a publication whose last round is the new epoch's verifies with status 0, as the design says, and prints a note for every verified epoch-closed event about a chain of the aggregate: nothing was compared with the old epoch, give `--previous`. A single-log command (`checkpoint`, `consistency` inside one epoch) prints the same kind of note when the log it checks is of a closed epoch. The design should say that the verification of the new epoch alone is a statement about the new epoch alone, and that the note is the only trace of the cut in that case.

5. A stricter choice in `bundle verify`. A bundle of a database with a closed epoch holds both logs and the event. The design does not say what `bundle verify` should do. The verifier ends with status 1, step epoch-closed, unless the auditor accepts the event with the flag, and the reason is the same as for `aggregate --previous`: a bundle is the "everything in one run" check, and an auditor who reads VERIFIED must not learn only later that the history was cut. The failure is checked before it is a finding: the closed log in the bundle must have the size and root that the event states, and a log of epoch e + 1 needs a verified event that closed epoch e. A proof whose block is in a closed epoch is named in the failure, unless the event states a common height at or above the block. The design should record this choice, because it makes every bundle of a database that was ever restored need the flag.

6. Proofs across epochs. A proof file carries no epoch. In a bundle with several epochs of the chain the verifier shows the proof in the epochs newest first and takes the first whose log holds the block at its height. The aggregate leg is made only against the epoch the aggregate names in its last round. The design says the bundle index names the proofs; it should also say whether the index may name the epoch of a proof. The control plane does not write one, and the verifier does not need it.

7. Anchors in `bundle verify`. The design (7.1) says `bundle verify` checks everything applicable in one run. Until step 21 it listed anchors and ended with status 3. It now verifies the anchors of every aggregate checkpoint of the bundle with the code of `anchors`, from the directories of the source, or from the paths the index names when the source cannot list them. `--require-anchors N` used to mean "any N above 0 ends with 3". It now means that at least N anchors must verify, and an anchor that cannot be checked is then a note. Without it every anchor the bundle holds must verify, and one that cannot is status 3, as it was. `--worm-url` and `--btc-header` are flags of `bundle verify` too. The design should say which of the two meanings of `--require-anchors` is the contract. The second is the one the console's bundle dialog needs.

8. What was not tested against a real publication. The cluster test of step 20 was not run for step 21. The fixture of a closed epoch is built by the verifier's tests from the documents (the aggregate rounds, the event, the frozen log and the log of the new epoch), with the formats taken from the code that writes them. A real closed-epoch publication should be added to `testdata/publication` the way the other one was made, by a cluster test run with the fixtures variable set, and the cross-check of the verifier should read it. Until then the byte formats of the event are checked against the writer by reading, not by running.

9. The eighth review. All twelve findings of the review of the publication and the client signatures (V8-01 to V8-12) were already fixed in the verifier; the project README still listed four of them as open. Step 21 re-ran every test of those fixes and mutated the fixes of the four medium findings; the results are in the verifier README.

### Step 23: the release of the verifier

What was built (branch ver-rel):

1. `kingres/verifier/scripts/build-one.sh GOOS GOARCH VERSION OUTPUT` holds the reproducible build
   flags in one place: CGO off, -trimpath, no VCS stamping, empty build id, no symbols, fixed
   micro-architecture levels (GOAMD64=v1, GOARM64=v8.0), read-only module graph, GOWORK off,
   GOTOOLCHAIN=local, and a cleaned environment (no GOFLAGS, compiler or level inherited).
2. `scripts/release.sh --version TAG --out DIR (--key-file FILE | --unsigned) [--targets]` builds
   the four named binaries through build-one.sh, writes SHA256SUMS (sha256sum -c format, checked
   with sha256sum -c), signs it into SHA256SUMS.note (Ed25519 signed note, key name
   kingres-verify-release) and verifies the note with `release verify` before it reports.
3. `scripts/repro-check.sh` copies the source twice into paths of different depth and length, gives
   each an empty Go cache and GOPATH, builds all four files in each and compares every byte.
4. `kingres-verify release verify --sums F --note F --key KEY [--binary F]... [--name N]`: new
   package `internal/release`, new file `internal/cli/cli_release.go`, three lines in the shared
   dispatch (one case, one usage line, the unknown-command text).
5. `cmd/kingres-release-tool` (keygen, pub, sums, sign): the signing side, not part of a release.
6. Packaging: `LimitCORE=0` in kingres.service, a guard in build-deb.sh that refuses a unit
   without it, build.sh builds kingres-verify through build-one.sh and records its SHA-256 in
   BUILD-INFO, the man page and README.Debian say the server copy is for convenience only,
   new `test/test-verifier-package.sh` (no Docker), extra checks in the Docker-based `test-deb.sh`
   (not run here).
7. README sections of the verifier and of the packaging directory.

Decisions where the design was silent or open:

- Signing is a separate program, not a subcommand of the verifier, so the verifier binary never
  holds signing code and the tool is not shipped. A key is read only from a file of mode 0600
  (never from the command line or environment); nothing is ever overwritten.
- No private key exists in the repository; tests make theirs from a fixed seed or with keygen in a
  temporary directory. The release key itself, its custody and its public value are the owner's
  decision (design decision 33); the README says "not created yet" where the key will be printed.
- go.mod is not edited by the release (the design said a toolchain line is added). Editing a
  tracked file would make the built tree differ from the tag. The go line already names the exact
  release; release.sh insists the installed Go equals it (or the toolchain line if one is added
  later), with an explicit override for trials.
- --version is mandatory and never guessed, because no tag exists before the first release. The
  product version (0.1.0) is the intended value so the packaged binary equals the linux/amd64
  release file; the packaging test proves this byte for byte.
- The package's verifier is built by the same script as a release (no PIE, no build id), instead of
  the PIE flags of the other Go programs, so it can match the release file. BUILD-INFO records its hash.
- Exit codes follow 7.4: 0, 1 (step named), 2. Status 3 is not used by `release verify` because
  every requested check is made or fails; with no --binary the note alone is checked and the output
  lists "no binary was checked" under not proved.
- The key argument is the raw base64 (exactly 44 characters, always read as raw) or the full key
  string. The sums file is strict: plain file names only, two-space form, no duplicates, no blank
  lines, and the note text must equal the file byte for byte.
- --unsigned exists only so the reproducibility check and the packaging test run without a key; it
  writes no note and prints that it is not a release.
- Docker-free packaging test: the existing packaging tests build the whole product and run Docker
  with systemd, far beyond the 2 GB limit, so the new test builds a real .deb from a small staged tree
  with the real build-deb.sh. The running-node checks (systemctl show LimitCORE, /proc limits) were
  added to test-deb.sh but not run.

Bug found and fixed on the way: `signednote.ParseVKey` split the key string on every plus sign, but the
base64 part of a key string may contain plus signs (about half of all keys), so those keys could not be
read. It now splits into three parts. Only the new release code calls it.

## 16. Fixes for the review of the anchoring fixes and steps 21 and 23 (2026-11-21)

The review of the fixes of the anchoring wiring (P7A-01 to P7A-17) and of the verifier steps 21 and 23 found no critical and no high finding, two medium findings (P7R-01, P7R-02), eight low findings (P7R-03 to P7R-10) and one note (P7R-11). This section says what was done for each, in the order of the review, and what the design now says that it did not say before. The contract is not changed: no REST or internal call, no file format of the publication and no exit status changed. The two additions that a reader of the contract will notice are listed at the end.

1. **P7R-01, the comparison of a binary with the signed sums.** No Go test reached the comparison: the case "binary changed" used a file name that is not in the sums, so the run ended at the name lookup. `TestReleaseVerifyRefusesARightNamedBinaryWithOtherBytes` gives a binary the right name and one changed byte (also an appended byte, and an empty file): status 1, step `binary`, and the message holds both SHA-256 values. It also covers a renamed file with other bytes under `--name`, the right content under a name that is not in the sums (refused without `--name`) and a run with one genuine and one changed file. The mutant of the review (`if false && !bytes.Equal(...)` at the comparison) now fails the test.

2. **P7R-02, a common height that the run has just contradicted.** The note "by that statement it is in the history that goes on as well" is gone from both places. In `bundle verify`, when the event states a common height at or above the height of a proof found in the closed epoch, the block must be leaf *h* of the signed log of epoch *e + 1* in the bundle (inclusion proof from the tiles against the signed root): a block that is not there is step `epoch-closed`, "the event and the logs disagree", whatever is accepted, the same rule as for the final size and root. When the log of the next epoch is not in the bundle the note says that the statement was not tested and that this run does not show that the block is in the history that goes on. In `proof --bundle`, which opens the one log it was asked for, the finding quotes the statement and says that the log of the next epoch was not opened. Tests: the reviewer's case (common height 3, a proof of block 3 of epoch 1, which is not in epoch 2), the case without the next log, and the single `proof` command. A related defect that the test found: the aggregate leg of a proof inside `bundle verify` did not receive the acceptance of `--accept-epoch-closed`, so a proof of a member that the event closed failed there although the finding was accepted; it now receives it.

3. **P7R-03, the error of a late worker.** `setFatal` keeps the term of the worker that found `ErrDeposed`; `takeFatal` returns the error only to a pass of that term and drops it otherwise. A server that leads again after a lost leadership no longer ends its first pass of the new term for an old worker's news.

4. **P7R-04, the tests that did not isolate their guard.** One test for each surviving mutant, each failing without its guard: M03b (a hung target gets exactly one request from all passes while its worker is in its first call, and `beginTarget` refuses a second worker for one set and target and accepts the other set); M05c (on PostgreSQL: another revision is dropped, the right one and revision 0 are applied, a target switched off without a change of revision drops the result); M06b2 (on PostgreSQL, `Failures` lists the failure before and does not list it after a newer anchored record of the same target and log, and lists a failure of a newer size that is not anchored); M14 (the job `ledger.anchor.credentials` is looked up among `Jobs()` and run through it); M15 (a holder's file of a target that exists is not removed); M36, M36b, M36c, M36d (each of the nine forms of a secret is the only thing in a text and the output must be the text with that one form replaced, to the last character; a secret is chosen whose forms all differ); M23 (see P7R-11); M04b (evidence with the right hash and length and another object key does not bind the record).

5. **P7R-05, the epoch guards.** E06 (an event that disagrees with the frozen log, with a held head shorter than that log), E09 (a closed log that continues past the size the event states) and E14 (an acceptance that nothing used is reported, while the used one is not). Each has a fixture with a longer frozen log or a number that no event has.

6. **P7R-06, the build is isolated from `go env -w`.** `build-one.sh` exports `GOENV=off` and unsets `GOEXPERIMENT`, `GOFIPS140`, `GODEBUG` and the other variables that change the code the compiler makes. `repro-check.sh` builds one more binary under a hostile Go environment (a Go environment file with `GOEXPERIMENT` and `GOFIPS140`, and exported variables of the same names) and requires the same bytes; without the fix it fails with "differs under a hostile Go environment".

7. **P7R-07, a release says which release it is.** The signed text is still the sums file and its format did not change. A fifth file, `RELEASE` (five lines: version, commit, commit date in UTC, Go release), is listed in `SHA256SUMS` like a binary, so the signature covers it. `release verify --binary RELEASE` checks it against the sums and shows the content (step `release-info`; strict format, `internal/release/info.go`). The "not proved" list says that **this is the newest release** cannot be known (rollback: a genuine older release with a known fault verifies as well), and whether the signed sums carry a `RELEASE` file. `release.sh` refuses a signed release unless the working tree is clean and `HEAD` is the tag of the version (`--tag`, default `v<version>`), and never creates a tag; `--dry-run` turns the refusals into warnings, writes `commit: unknown` and says in capitals that the result is not a release. An unsigned build is not checked (the reproducibility check builds from copies without git data) and records the commit only when the checkout is clean. The package test makes its release with `--dry-run`, and checks in a throwaway git repository that a signed release without a tag, from a dirty tree and from a commit that is not the tag is refused, and that one from the tagged clean commit is made and shows its version and commit. The name of the tag is the project's decision: `v<version>` is a default that `--tag` replaces.

8. **P7R-08, a holder that cannot be asked.** `Fetch` answers `ErrNoCredentials` only when every holder answered that it has no file (the 404 with the text of `FetchNoneMessage`; a server that is alone counts as such), and `ErrHoldersUnreached` otherwise (`FetchFirst`, tested for all the combinations). On `ErrHoldersUnreached` the records of the target are failed with the retryable class (`credentials_unreached`), so they wait with backoff and the usual limits apply, and the job states in the problem text that not every holder could be asked and asks again every minute. A 404 with another text, a 503, a 422 and a holder that did not answer are all "unreached". The permanent failure with the operator's instruction stays for the case that every holder was asked and none has the file.

9. **P7R-09, the verifier reads `restore-acknowledged`.** The type is known (`internal/events/restore.go`, strict: every line, canonical numbers, a generation from 1, the same generation not twice, an event that no authorised key signed is skipped and listed). `aggregate`, `bundle verify`, `monitor`, and `checkpoint` and `consistency` of the aggregate log say in a note that the publication holds the verified event and what it states; the chain logs say nothing, because a restore does not cut them. The failure of a comparison of the aggregate log with a checkpoint you hold (`aggregate --previous`: rollback, fork, a failed extension; `consistency`; `checkpoint --state`; `monitor`) names the event and says that it explains the failure when the held checkpoint is from before it, and that it does not show that the cut was honest. No acceptance flag and no change of status: the finding is information. The text of 9.4, "the event shown next to the fork", is now what the verifier does. The event is now interpreted, so a malformed one that an authorised key signed ends a run like a malformed `epoch-closed` event; the publisher writes exactly the format the verifier reads (`ledgerround/restore.go`).

10. **P7R-10, the job of the credentials and a target that is being created.** `CreateTarget` commits the row and writes the files after. The job now takes a file away from the holders only for a target that is absent from a list of the targets read after the holders were asked: a file that the report shows belongs to a row that was committed before the report, so the later list holds it. A target created between the two reads is never taken for an orphan (a test creates one inside the holder call); a target that is deleted keeps no file after the next pass. If the second list cannot be read the removal waits for the next pass.

11. **P7R-11, the copy before a restore and the two notes.** The copy `published.json.before-restore-<generation>` is written into a temporary file, synced, renamed, and the directory is synced; the acceptance still fails with "nothing was changed" when it cannot be written (a test makes the directory unwritable and checks that the record on disk and in memory did not change). A copy that is already there is never written over: a lost record that is regenerated from the database starts at generation 0 and reaches the same number, so the new copy gets the suffix `.1`, `.2`. The two other notes are written down and need no code: (a) `announceRestore` runs at the start of a round, but a key task can append an event between the acceptance and that round, so the event `restore-acknowledged` is not always the first event after the acceptance (ADR 0025 says "before anything else is signed" about the round, not about the events of tasks); the record is consistent either way and the verifier reads the event wherever it is. (b) `Merge` at one generation takes the note `Restore` from a peer when this copy has none, so a cleared note can come back from a stale peer; it is harmless, because the event is keyed by `generation:<n>` and `AppendEvent` finds the existing one.

**Two additions that are visible outside the code.** `release verify` prints a `release-info` step and `info.release_*` members when the `RELEASE` file is given, and its "not proved" list has two more lines; `release.sh` has the options `--tag` and `--dry-run`. Both are additive and neither changes an exit status. The internal call `credentials/fetch` answers the same 404 as before, with a fixed text that the caller now compares.

**Mutants.** Each fix has a one-line mutant that fails its test; they are listed in the commit message and in the report of the work.

## 17. Fixes for the review of the fixes P7R (2026-11-22)

The review of commit d9f79e78d2 (`docs/reviews/2026-11-22-ledger-phase7-p7r-fixes.md`) found no critical and no high finding, one medium (P7F-01), five low and one note. All are handled below; the reviews are in `docs/reviews/` with the earlier one (`2026-11-21-ledger-phase7-anchoring-and-verifier-fixes.md`).

1. **P7F-01, a rotation that a holder missed.** The job gave a file only to a holder that had none, so a holder with the old file was never given the new one, and the alert of a failed distribution was replaced by the job's own list within a minute. Now `credentials/list` carries, for each file that opens, `copies: [{id, digest, installed}]`: a SHA-256 over the id and the members with their values in a fixed order (the same for a plain and a sealed file, whatever the salt; no value is in it) and the time the file was written on that server. The leader compares: a holder whose digest differs and whose file is not newer is given the leader's file; a holder whose file differs and is newer than the leader's gives it to the leader first (`credentials/fetch` of that holder), and the leader gives nothing away before that. Equal ages or an age that is not known keep the leader's file. The alert of a distribution stays while a holder cannot be asked (the pass does not know that it is current) and goes when the pass saw every holder current or gave the file to every holder that was behind; its text ("given again every minute") is now true. **A failover to a holder with old credentials:** the worker asks the holders before it builds a target (at most once in 30 s for each target and term, `CredGateTTL`) and takes a newer file first; when a newer file exists and cannot be taken, the records wait (`credentials_stale`, retryable) and nothing is sent with the old credentials; when no holder can be asked (right after a failover the old leader is down) the worker goes on with its own file, because refusing then would stop anchoring after every failover. **Limits that stand:** a stale leader whose only holder with the new file is down anchors with the old file (the old key may still work or may fail with a refused record, which the alert of 6.9 shows); the order of two rotations that are closer than the clock error of two servers can be wrong; a holder of an older version sends no copies and is not judged. The choice of the write time instead of a counter in the database avoids a migration; a counter in `kingres_cp.ledger_anchor_target` (set at every change of the credentials, written next to the file) would remove the first two limits and is the next step if they matter.
2. **P7F-02, a fetched file that cannot be written.** `Install` says `ErrInstallWrite` for a file that decoded and could not be written, and `ErrCredentials` only for one that does not open here. The first is `credentials_unreached` (retryable) and its own text in the alert, the second stays final. The agent answers a write failure with `credentials_not_written` (500), not as a passphrase mismatch.
3. **P7F-03, the survivors and the dead note.** Tests for O2 (the removal of orphans stops when the targets cannot be read again), P1 (a same-size fork against a held checkpoint names the restore event) and P5 (only a fork is explained by the event, not a damaged tile). The success note "it is leaf N of the signed log of epoch e+1 in the bundle as well" is removed: `proofEpochs` tries the newest epoch first, so a block that is also in the next log is found there and is never listed as found in the closed one; the failing branch stays as a second line behind it. The test of the unwritable directory (K2) still skips on Windows and as root; it runs in the Linux runs of the package.
4. **P7F-04, a dry-run set.** `release verify` with the RELEASE file of a set that says commit or date `unknown` ends `VERIFIED (NOT A RELEASE)` (exit status 0, the signature is genuine), with the reason in the headline and a note, and `info.not_a_release` in the JSON. Without the RELEASE file the run cannot know (the "not proved" list says to give it).
5. **P7F-05, a stale error.** `setFatal` keeps the error of the highest term: a later term replaces an earlier error that is still kept, and a late error of an earlier term never replaces one of a later term. The first error of one term is kept.
6. **P7F-06, the strict parser.** A `restore-acknowledged` event that is not in the strict form (a missing or unknown line, a number that is not canonical, a generation twice) is not read at all and is listed in `Walk.UnreadRestores`; the walk goes on, so the events after it and every command that does not report restores (`trust follow`, the key events) are not affected. The commands that report restores (`aggregate`, `bundle verify`, `monitor`) print a `finding:` line that names the event and the reason and says that nothing of it is used, and the failure of a comparison with a held checkpoint says that an unread event exists and that the run cannot say whether it explains the failure. The content is as strict as before; only the reach of a refusal changed. A later publisher that adds a line to the event makes older verifiers say so instead of stopping.
7. **P7F-07, the subject.** The subject of the event is `generation:<n>:<first 16 hex digits of the sum of the record that the acceptance replaced>`. Announcing the same restore again gives the same subject and finds the event; a second restore that reaches the same generation number (a record that was made again from the database) replaces another record and has another subject, so it is announced. The verifier does not read the subject.

**Mutants.** Control plane: the judging of ages and of stale copies (4), the carrying of the alert, the gate (off, error ignored, holders not asked, no cache), the take-newer branch of the job and its continue, the install classes (3 places), the term of `setFatal` (2), the digest (id, values, time), the report (`Add`), the orphan guard (O2); verifier: P1, P5, the unknown release, the date of a release, the strict parser, the notes and the remark of unread events; the subject of the event. Each is killed by a test of this section. One mutant is equivalent and was left alive: the gate ignoring the error of the question to the holders, because the report that comes with such an error holds no copies. The survivors O2, P1 and P5 of the review are killed by `TestNoFileIsTakenFromAHolderWhenTheTargetsCannotBeReadAgain`, `TestAForkOfTheSameSizeAgainstAHeldCheckpointNamesTheRestoreEvent` and `TestOnlyAForkIsExplainedByARestoreEventAndADamagedTileIsNot`.

### 17.1 Fixes for the review of the fixes P7F (2026-11-22, `docs/reviews/2026-11-22-ledger-phase7-p7f-fixes.md`)

The review found no critical and no high finding and two medium ones. Item 1 of the list above (the write time of the file as the age of a copy, and the plain digest) is **replaced** by what follows; the rest of it stands.

1. **P7G-02 and P7G-03, the generation replaces the file time.** The credentials file carries a rotation generation (a counter) and the stamp of its rotation (16 random bytes), inside the file body (inside the sealed text of a sealed file, so both are authenticated): `{"credentials":{...},"generation":3,"stamp":"<32 hex>"}`. A rotation writes a generation one above the highest that the leader and the holders it could ask hold (`generationFloor`, and its own file); every copy keeps generation and stamp. Which copy is newer is decided by the number alone: a holder with a higher generation gives its file to the leader (which takes it before it gives anything away, and the worker takes it before it anchors), a holder with a lower one is given the leader's file, a holder with the same generation and another digest is given nothing and given from nothing (two changes made at once: the alert asks the operator to enter the credentials again, which makes a generation above both). No time decides anything: not the time of the file system (a restore from a backup resets it, a server whose clock is ahead looks newer) and not a time that a holder reports (there is none in the call any more). A server also refuses to write a file of a lower generation over a higher one, or another of the same generation (`ErrOlderGeneration`, `409 older_generation` on `credentials/install`), whoever sends it, so a restored holder cannot revert a rotation even through a bug of the leader; the refusal is an alert through the failed distribution. A holder that said it has a newer file and serves an older one is refused in the same way and the alert says so. **Files of earlier versions** have neither member: they are read as generation 0 without a stamp, are older than every rotation, and two of them are current to each other (they cannot be told apart); the first rotation after the upgrade writes the new form and makes every holder of an old file behind. An earlier version cannot read a file of the new form (a mixed cluster during an upgrade gives `passphrase_mismatch` on install until every server is new). The database migration the design named as the next step was not needed.
2. **P7G-05, the digest.** The digest is an HMAC-SHA256 keyed with the stamp (16 random bytes that are in the file and never in this call), over the label, the id, the generation and the members. It is the same for every copy of one rotation, differs between two rotations of the same credentials, cannot be tested against guesses of a secret by whoever sees only the digest, and is compared with `hmac.Equal`. A file without a stamp has no digest.
3. **P7G-04, the classes of failure.** A passphrase file that cannot be read is `ErrPassphraseFile` and, in `Install`, a failure of this server like a full disk (retryable, with its own text), not "no holder has it" and not "does not open". A file that does not open here (another passphrase, a plain server) is `fetchUnopenable` in the job and `takeUnopenable` for a newer file: final at once, with an alert that says which. A newer file that cannot be taken for another reason (the holder did not give it, this server could not store it, the holder gave an older one) is waited for `CredStaleLimit` (30 minutes from the first failure); after that the records of the target fail for good (`credentials_stale`, class permanent) and the alert says that they are not tried again until Retry. The alert text before the limit says "asked again every minute", which is true then.
4. **P7G-01, the verifier and the subject.** The verifier reads a second `restore-acknowledged` event of the same generation when it replaced another record (another `published-state-sha256`), the case that the subject of P7F-07 exists for; the same generation with the same replaced record is a repetition and is not read. Tests: two cuts of one generation, a later generation with the same replaced record, a true repetition.
5. **P7G-06, small things.** The gate is pruned (entries of other terms when a term changes, entries and failure notes of targets that are gone at every pass of the job). `release verify` of a set that a dry run signed ends with exit status 5 (`ExitNotRelease`) and the verdict `VERIFIED (NOT A RELEASE)`; `--accept-dry-run` gives 0 for whoever wants such a set, and the JSON carries `not_a_release` either way. Tests kill the survivors of the review: M6 (an alert is cleared by a pass that gave the file even when a holder did not answer the question), M7 (the holders behind the file that was just taken are given it in the same pass), M14 (a worker without the file of a target reaches the fetch through the gate), V2 (a dry-run set whose commit only is unknown).

**Mutants.** The generation judging (higher, lower, equal with another digest, three), the install guards (lower, rival), the rotation floor and the own generation, the key of the digest, the empty digest, the three classes and the bound of the waiting, the refusal and the local failure of a newer file, the pruning (three), `delivered`, the recomputation after taking a file, the gate without a file, the fetch outcomes, the classes of the passphrase file (two), the floor of a rotation by the REST call, the status of the agent, the final records, the duplicate rule of the verifier (two), the exit status of a dry-run set (three), the commit-only case. Each is killed by a test. Mutants that did not compile or did not apply were repeated in a form that does.

### 17.2 Fixes for the review of the fixes P7G (2026-11-22, `docs/reviews/2026-11-22-ledger-phase7-p7g-fixes.md`)

The review found no critical and no high finding, three medium and three low ones (P7H-01 to P7H-06).

1. **P7H-01, a plain file with a chosen generation.** The generation of a file in clear is not authenticated, so: a server that seals its files refuses a file in clear above generation 0 (`ErrPlainFile`, in `Install`; the plain files of earlier versions are generation 0 and are still taken), and a file in clear that is put into the directory of such a server counts as generation 0 without a stamp in `Digest`, so it takes no part in the ordering. Every install also refuses a generation above the one of the file that is here plus `MaxGenerationJump` (64: a rotation counts one at a time; `ErrGenerationJump`), and a holder that reports a generation above that bound is not believed: its file is neither taken nor replaced (`judgeCopies` lists it as `jump`), the floor of a rotation here ignores it, and the alert says what to do. **The way out of a stuck or poisoned generation** is on the holder that has the file: remove `<ledger-keys-dir>/anchor/<target id>.json` there (with kingresd running or not); the next pass of the job gives it the leader's file, and a rotation here always works because it counts from the leader's own file. A leader whose own file is at the bound of 2^40 (`ErrGenerationExhausted`, a message that names this) removes its own file and enters the credentials again; if a holder has the file the leader can also take it with the file removed. Entering the credentials again does not change a holder that is above the bound: it refuses a file below its own number, which is the point.
2. **P7H-02, the wait of an episode.** The note that starts the 30 minute wait of a target is now removed whenever the judging finds no newer holder (in the gate after a report that was read, and in the job), not only after a successful take; a pass that could not ask the holders leaves it. A later episode starts its own wait.
3. **P7H-03, a rotation that is undone, and a rotation that has not seen every holder.** A file taken from a holder (in the gate or in the job) is an alert of its own, a warning that stays 24 hours (`CredNoticeFor`): "this server replaced its credentials file ... by the newer file of the holder ...; a change made here while that holder was away is undone by it". A change of the credentials of a target that already had a file is **refused** (503 with Retry-After, "The credentials were not changed ...") while a holder could not be asked which generation it holds, or the list of the holders could not be read: the refusal is the safer choice, because the answer of the call has no member for a warning and a holder that comes back with a higher number would undo the change without a sign; the cost is that a holder that is down for good blocks a change until it comes back or is removed from the cluster (the cluster's membership decides who a holder is). A new target and credentials for a target that had none are not refused (no earlier file exists anywhere).
4. **P7H-04, concurrent changes.** `Rotate` and `Install` take a lock for the target (`CredStore`), so two rotations cannot read the same generation, and the service holds a lock for the target around the rotation and the hand-over, so holders get the generations in order and one at a time.
5. **P7H-05.** `release.sh` expands the empty array in the form `${accept[@]+"${accept[@]}"}`, which bash before 4.4 accepts under `set -u`; the packaging test checks the form.
6. **P7H-06.** The bound is pinned: just before `CredStaleLimit` the wait goes on, at it the records fail, and the constant is 30 minutes in a test; the job-path clearing and the gate-path clearing each have a test.

**Mutants.** Control plane (each killed by a test): the plain file refused (3 places: the sealing check, the digest of a planted file, the bound of the parser), the jump bound (install, judging, the floor), the exhausted generation, the clearing of the wait (gate, job, the job that asked nobody), the limit (`>`, 60 minutes, 0), the notice (gate, job, expiry), the refusal of a partial change (unreached, error, create, no earlier file), the per-target locks (store and service); packaging: the portable form.

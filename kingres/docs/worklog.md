<!-- SPDX-License-Identifier: PostgreSQL -->
# Work log

The dated record of what was done on KingresDB, how it was verified, and what
is still open. Newest entry first. The required content of an entry is defined
in [dev/documentation.md](dev/documentation.md).

---

## 2026-11-14: Sharding 0.2, ledger 0.2, the verifier, packaging, and the control plane up to one real server

### What was done

- **`kingres_shard` 0.2** (phase 2a,
  [design/shard-phase2a.md](design/shard-phase2a.md)): two-phase commit with
  a decision log and a recovery worker, atomic catalog changes, reference
  tables, DDL propagation, a distributed scan for any `SELECT` that spans
  shard groups, cross-group `UPDATE` and `DELETE`, and
  `kingres_shard.node_conninfo`.
- **`kingres_ledger` 0.2** ([design/ledger-0.2.md](design/ledger-0.2.md)):
  updatable ledger tables with a history table, a background worker that
  builds blocks, and digest and verification records. The format
  documentation was completed from the gaps the verifier found, and four
  defects in the server-side proof check were fixed on the way (the format
  member was not checked, the operation accepted any character, a malformed
  later header was ignored, and the user name was hashed in the database
  encoding).
- **`kingres/verifier`**: `kingres-verify`, a standalone verifier written
  from the format documentation alone, standard library only.
- **`kingres/packaging`**: a release build recipe, a container image, a
  three-node compose topology, a Debian package with the unit
  `kingres.service`, and tests of each. A CI workflow exists but is not in
  the repository yet (see "Still open").
- **REST contract 0.3.0** (69 paths, 86 operations).
- **Control plane** ([design/control-plane-2.md](design/control-plane-2.md),
  [design/control-plane-3.md](design/control-plane-3.md)): `kingresd`
  manages the local instance, keeps its state in the system database, runs
  durable tasks, creates a cluster through the API, and implements 64 of
  the 86 operations (databases and tables, roles and grants, network rules,
  the query workbench, ledger status, digests and verification, this
  server, alerts, logs, metrics, cluster settings). The first part of the
  multi-server increment is in: a cluster certificate authority and an
  internal API with mutual TLS.
- **Web panel**: follows contract 0.3.0 and has an end-to-end run against
  the real server (`npm run e2e`).
- **Decisions** recorded as ADRs 0018 (manager leader), 0019 (system
  database) and 0020 (no bundled metrics store).
- **Design** of phase 4 of sharding,
  [design/shard-phase4.md](design/shard-phase4.md).

### How it was verified

- `kingres_shard` 0.2: 11 pg_regress tests and 1850 TAP subtests in 15 files
  pass out of tree, and the whole tree built from the last commit plus this
  version passes both extension suites in tree (29 of 29), run by the
  integrator.
- `kingres_ledger` 0.2: 10 pg_regress tests and 199 TAP subtests in 9 files
  pass out of tree and in tree (21 of 21 at that commit), run by the
  integrator.
- Verifier: `go build`, `go vet`, `gofmt -l` clean and `go test ./...`
  passing, run by the integrator, on fixtures regenerated from a server
  running ledger 0.2; the cross-check passed unchanged, which shows 0.2 did
  not change hash format version 1.
- Packaging: the integrator built the image from a clean snapshot and ran
  its test (23 checks passed). The package test (61 checks: install, service
  start, upgrade, remove, purge in a container with systemd) and the compose
  test (16 checks) were run by the author only.
- Control plane: Windows build, Linux build, `go vet`, `gofmt -l` and
  `go test ./...` clean, and the full WSL integration run (21 packages, no
  skipped test) passing, run by the integrator after each of its three
  commits.
- Contract 0.3.0: Redocly lint and the rule checker pass.
- Panel: typecheck, lint, 180 tests, build and the generated-file checks
  pass, run by the integrator. The end-to-end run (28 steps) was run by the
  author only; the integrator did not repeat it because the control plane
  it builds from was being changed at the time.
- **Two mistakes of the integrator's own tooling, found and fixed.** The
  first in-tree check of sharding 0.2 failed because the check reused a
  build directory and both ninja and the temporary installation decide by
  modification time, so a stale control file was tested; the check now makes
  overlaid files newer and discards the temporary installation. And
  `ext.sh` did not apply an extension's settings file while the in-tree run
  does, so one expected output could not be right in both; `ext.sh` now
  applies it.

### Needs-verification items checked

- "Shard moves use logical replication; does not replicate DDL, sequences,
  TRUNCATE": not checked yet; phase 4 will.
- "2PC recovery by comparing prepared transactions with a local log":
  implemented and tested with fault injection. One rule from the design did
  not survive contact with PostgreSQL: "the transaction id in the gid is not
  running" is not enough after a crash, because a killed backend's
  transaction id can still be reported as in progress; the backend pid is
  compared as well.
- "A hot standby rejects writes; write forwarding would need a core patch":
  not checked; phase 5.

### Decisions and deviations

- Deviations are in the "Implementation notes and deviations" sections of
  the design notes. Those a user notices: an `UPDATE` that assigns the
  distribution column is always refused; multi-row `INSERT`, `INSERT ...
  SELECT` and `COPY FROM` across shard groups are still refused;
  reference-table writes accept only built-in immutable expressions; the
  distributed scan fetches rows only, with no aggregate, sort or limit
  pushdown; the internal API of `kingresd` listens on its own port.
- `kingresd` signs users in and then acts as the signed-in role through the
  local socket with a role map, instead of keeping the sign-in connection
  (which is bound to one database).
- The package installs client programs with a `kingres-` prefix
  (`kingres-psql` and so on) so that they cannot collide with a
  distribution's PostgreSQL packages.

### Still open

- **Reviews in progress or pending**: `kingres_shard` 0.2 (in progress);
  `kingres_ledger` 0.2 was reviewed (eight findings,
  [reviews/2026-11-14-kingres-ledger-0.2.md](reviews/2026-11-14-kingres-ledger-0.2.md))
  and its fix round is in progress; the control plane and the panel have not
  had an independent review.
- **CI**: the workflow file is committed on a local branch only. The access
  token used for pushing may not create workflow files; it needs the
  workflow permission.
- The control plane cannot yet join servers, rebalance or fail over; 22
  operations answer "not supported yet".
- Seven mismatches between the server and the contract found by the panel's
  end-to-end run (for example `GET /ledger/status` answering 404) are being
  fixed.
- Phase 2b of sharding (pushdown for speed), phase 4 (moving buckets) and
  phase 5 (standbys and failover) are not started.
- For a release: signing of the package and image, an apt repository, RPM,
  arm64.

### Core patches

None added in this entry.

---

## 2026-11-14: Reviews, fix rounds, REST contract 0.2 and the first control plane and panel

### What was done

- **Independent reviews** of both extensions, saved in
  [reviews/](reviews/): `2026-11-14-kingres-shard-0.1.md` and
  `2026-11-14-kingres-ledger-0.1.md`.
- **Fix round for `kingres_shard`.** Every finding of the review was fixed,
  each with a test written first: the privilege escalation through the
  `SECURITY DEFINER` event trigger and through the catalog reload (every
  function now pins a `search_path` that ends with `pg_temp`, one function
  keeps definer rights and qualifies everything); session settings leaking
  into forwarded statements (transmission modes around deparsing, forced
  settings on remote sessions); the forwarded-session flag (now
  connection-only, and a forwarded session refuses anything not provably
  local); collation-aware routing; a `ProcessUtility` hook that refuses DDL
  which would touch one node only; a cluster-wide catalog lock and a unique
  primary per group; and statements that reach a distributed table through
  an inlined SQL-language function, which used to return partial results.
- **Fix round for `kingres_ledger`.** All twelve findings were fixed, and the
  hash format was hardened while it can still change freely: a domain tag
  per kind of hashed data, a random chain identity that seeds block 0 and is
  carried by digests and proofs, non-decreasing block times, the
  transaction's user name inside the block transaction leaf, and the table
  bound into proofs. A second protection layer of event triggers enforces
  the rules by OID at the point of action. `ledger_enable` and the tables
  that reveal row hashes are no longer open to PUBLIC.
- **REST contract revision 0.2.0** (`kingres/api/`): 67 paths, 84
  operations, with a rule checker (`scripts/check-contract.mjs`). It fixes
  the defects the two implementers reported against 0.1.0; the changelog is
  in `kingres/api/README.md`.
- **Control plane, first increment** (`kingres/control-plane`, the server
  `kingresd`): the whole contract is routed; 33 of the 84 operations are
  implemented for real (health, setup state and setup token, sign-in and
  sessions, the read side of cluster, servers, shards, databases, tables,
  replication, tasks, roles, grants, settings, ledger and audit), and the
  rest answer with the contract's "not supported yet" problem after
  authentication and authorization have run.
- **Web panel, first increment** (`kingres/web`, Vue 3): every page of the
  first increment against a mock API that is checked against the contract.
  Vue replaced the React originally proposed
  ([ADR 0017](adr/0017-vue-for-the-web-panel.md)).
- **Brand assets** supplied by the project owner are in `kingres/brand/`,
  with usage notes; the README files and the server's placeholder page show
  the logo.
- **Identity.** The developer is Mohammad Rasoul Kazemi (RooyeKhat) and the
  copyright holder is RooyeKhat Media Co; see `kingres/AUTHORS` and
  `kingres/COPYRIGHT`. The Go module path is
  `github.com/KingresDB/Kingres/kingres/control-plane`.
- **History.** The clone holds the full history behind `REL_18_7`, which a
  later rebase needs.

### How it was verified

- **Whole tree:** `build.sh test-all` on the final tree: 338 tests OK, 0
  failed, 23 skipped, 0 timed out. The 23 skips are the same count as on the
  unmodified base; their reasons were not examined.
- **Extensions in tree:** `meson test --suite setup --suite kingres_shard
  --suite kingres_ledger`: 18 of 18 OK (8 pg_regress tests and 7 TAP files
  for sharding; 7 pg_regress tests and 6 TAP files for the ledger).
- **Exploits re-run by the integrator** on a throwaway two-node cluster.
  Before the fix round: an ordinary role became superuser through the
  `sql_drop` event trigger (with one distributed table present), a date
  written under `DateStyle = 'SQL, DMY'` was stored as a different day when
  the statement was forwarded, and an ordinary role could set the
  forwarded-session flag. After it: the role stays unprivileged, the local
  and the forwarded row hold the same date, and setting the flag fails.
- The sharding review file disagreed with the reviewer's first report and
  declared two real findings sound; it carries an integrator's note at its
  top, which is authoritative.
- **A length that depended on the product name.** The ledger's protection
  hook compared trigger names against a prefix with a hand-written length.
  The length was wrong for the final name, and the regression test
  `ledger_protect` caught it (the second protection layer still rejected the
  statement). The length is now derived from the literal.
- **Control plane:** `go build`, `go vet` and `gofmt -l` clean and
  `go test ./...` passing, run by the integrator. The author also reports
  `golangci-lint` with 0 issues, the race detector, and integration tests
  against a real PostgreSQL primary and standby; the integrator did not
  repeat those three.
- **Contract:** Redocly lint reports the file valid and the rule checker
  reports that all rules hold.
- **Panel:** `npm run typecheck`, `npm run lint`, `npm test` (160 tests in 14
  files), `npm run build`, `npm run api:check` and `npm run brand:check`
  passed when run by the integrator on the final tree, with the logos in
  place. The author looked at 17 routes in a headless browser in both themes
  and did one keyboard-only pass; screenshots were examined for about a
  dozen screens only.

### Needs-verification items checked

- "Whether ledger catalogs can be protected even from superusers without a
  core patch": partly. The hook and the event triggers reject the
  statements, including for superusers, but a superuser who edits catalogs
  directly, switches event triggers off, or removes the library from the
  configuration gets past them. The extension's README lists every such
  path. Detection then rests on verification against an external digest.

### Decisions and deviations

- Deviations from the design notes are recorded at the end of each note
  ([design/shard-mvp.md](design/shard-mvp.md),
  [design/ledger-mvp.md](design/ledger-mvp.md)). The ones a user notices:
  on a cluster with more than one shard group, DDL on distributed tables is
  refused until it can be propagated; a comparison under a different
  collation is not used for routing; a statement that reaches a distributed
  table through an inlined SQL-language function is refused; enabling the
  ledger on a table is an administrator action and is permanent.
- **A role named like the extension schema.** With the default
  `search_path` (`"$user", public`), a role named `kingres` resolves
  unqualified names in the schema `kingres` first. The development user in
  WSL has that name, so the tests set `search_path` explicitly. Product
  documentation must warn about it.
- Phase 2 of sharding is split into 2a (correctness on a multi-group
  cluster) and 2b (pushdown for speed);
  [design/shard-phase2a.md](design/shard-phase2a.md).

### Still open

- Six small defects in contract 0.2.0 reported by the implementers (fields
  without a data source that are not nullable, and wording), for revision
  0.3.
- The control plane does not manage instances, run tasks or change anything
  yet; the panel is not connected to it.
- Not reviewed yet: the fixed versions of both extensions (the fix rounds
  were verified by tests and by re-running the exploits, not by a second
  independent review), the control plane, and the panel.
- Known limits of `kingres_shard` 0.1: statements that span shard groups are
  refused; catalog synchronization and cross-node DDL are not atomic;
  user-defined types and non-default collations are not portable between
  nodes; node-to-node authentication is whatever `pg_hba.conf` provides.
- Not supported in `kingres_ledger` 0.1: updatable ledger tables, `pg_dump`
  and restore of a ledger database, partitioned or inherited ledger tables,
  and both extensions on the same table.
- CI, and the mirror branches described in
  [dev/upstream-sync.md](dev/upstream-sync.md).
- A legal review of the name before any public release, including its
  similarity to the name of the Ingres database.

### Core patches

KP-0001 (`meson.build`), KP-0002 (`README.md`), KP-0003
(`contrib/meson.build`).

---

## 2026-11-14: Phase 1 (sharding MVP) and first ledger increment

### What was done

- **`contrib/kingres_shard`, version 0.1** (phase 1), implemented from
  [design/shard-mvp.md](design/shard-mvp.md): the catalog in schema
  `kingres` with a map version, the bucket function, the node registry and
  catalog synchronization, `create_distributed_table`, and statement routing
  through a planner hook and a custom scan, with remote transactions joined
  to the local one. About 4,000 lines of C and a 640-line install script.
- **`contrib/kingres_ledger`, version 0.1** (first increment of phase 6),
  implemented from [design/ledger-mvp.md](design/ledger-mvp.md): append-only
  ledger tables, the capture trigger, RFC 6962 Merkle trees, blocks and
  digests, `ledger_verify`, inclusion proofs that are checked offline, and a
  ProcessUtility hook that rejects DDL which would switch the ledger off.
  About 2,700 lines of C and a 1,000-line install script.
- Both extensions were added to the meson build (core patch KP-0003), and
  `kingres_shard` got a test configuration file so its regression tests run
  in-tree.
- **REST contract.** `kingres/api/openapi.yaml`: OpenAPI 3.1, 65 paths, 82
  operations, with the conventions in `kingres/api/README.md`.
- **Panel framework changed to Vue 3** at the project owner's request
  ([ADR 0017](adr/0017-vue-for-the-web-panel.md)). The few minutes of React
  scaffolding were discarded.

### How it was verified

- Out of tree (PGXS), run by the integrator after the authors:
  `ext.sh kingres_shard all` gives 5 of 5 pg_regress tests and 191 TAP
  subtests in 3 files passing; `ext.sh kingres_ledger all` gives 4 of 4
  pg_regress tests and 45 TAP subtests in 3 files passing.
- In tree: after `build.sh sync configure build`,
  `meson test --suite setup --suite kingres_shard --suite kingres_ledger`
  reports 11 of 11 OK, 0 failed, 0 skipped.
- The full PostgreSQL suite has not been re-run since the extensions were
  added to the build.
- `npx @redocly/cli lint kingres/api/openapi.yaml` reports the file valid.
- Independent code reviews of both extensions were started; their findings
  are not in yet and nothing here should be read as reviewed.

### Needs-verification items checked

- "Partition hash internals (seed, `hash_combine64`)": the extension calls
  PostgreSQL's own `compute_partition_hash_value()` with
  `HASH_PARTITION_SEED`, and a regression test confirms `bucket_for` equals
  the partition a row is really stored in, for int4, int8, text, uuid, date
  and varchar.
- "A hook comparison for capturing writes (trigger, xact callback, logical
  decoding, custom resource manager)": the trigger route was implemented and
  works for the MVP; `ENABLE ALWAYS` triggers do fire under
  `session_replication_role = replica`. The other routes were not tried.
- "ed25519 in pgcrypto", "S3 Object Lock semantics": not checked; signing and
  anchoring are phase 7.

### Decisions and deviations

- Deviations from the two design notes are recorded by the authors in the
  section "Implementation notes and deviations" at the end of each note. The
  ones with consequences for users:
  - A planner hook cannot force a custom plan, so a prepared statement whose
    routing depends on a parameter fails under `plan_cache_mode =
    force_generic_plan` with a hint.
  - The stale-map retry only works when the remote transaction has run no
    earlier statement.
  - Ledger row hashes are computed with the client encoding switched to
    UTF-8, so they do not depend on a session setting.
  - `ledger_enable` rejects composite types, arrays of user-defined types and
    virtual generated columns.
- **Hash format corrected before release.** The design note left the
  transaction's user name outside every hash. It is now part of the block
  transaction leaf, still as format version 1, since no ledger exists yet.
- **A role named `kingres` shadows the schema.** With the default
  `search_path` (`"$user", public`), a role whose name equals the extension
  schema resolves unqualified names in that schema first. The development
  user in WSL is called `kingres`, so the tests set `search_path`
  explicitly. Product documentation must warn about this, or the schema
  should be renamed; not decided.

### Still open

- Review findings for both extensions, and the fixes that follow.
- Three format changes proposed by the ledger author that are free to make
  only before a first release: a domain tag per leaf kind, a chain identity
  so that a digest of one database cannot be checked against another, and a
  rule for `closed_at` (monotonic or documented as advisory).
- Known limits of `kingres_shard` 0.1, all inside the phase 1 scope:
  statements that span shard groups are rejected; catalog synchronization
  and cross-node DDL are not atomic; `COPY`, `TRUNCATE` and later DDL on a
  distributed table are not propagated; user-defined types and non-default
  collations are not portable between nodes.
- Not supported yet in `kingres_ledger` 0.1: `pg_dump` and restore of a
  ledger database, partitioned or inherited ledger tables, and using both
  extensions on the same table.
- The Go control plane and the Vue panel are in progress.

### Core patches

KP-0003 (`contrib/meson.build`).

---

## 2026-11-14: Phase 0, repository, build environment and minimal rebrand

### What was done

- **Repository.** Initialized a git repository in the project directory with
  `core.autocrlf=false` and `core.eol=lf`, added the remote `upstream`
  (https://github.com/postgres/postgres.git), fetched the tag `REL_18_7`
  (commit `f5c41f6`, "Stamp 18.7.") with depth 1, and checked it out as the
  branch `kingres-18`. The clone is shallow; it has to be deepened before the
  first rebase and before pushing to a hosting remote.
- **Layout.** KingresDB's own files live under `kingres/` and
  `contrib/kingres_*`. The design document moved to
  `kingres/docs/architecture.md` and the research notes to
  `kingres/docs/research/`.
- **Minimal rebrand** (core patches KP-0001 and KP-0002, see
  [dev/core-patches.md](dev/core-patches.md)): `meson.build` defines the
  product version 0.1.0, exports `KINGRES_VERSION` and
  `KINGRES_VERSION_NUM`, and appends `(KingresDB 0.1.0)` to
  `PG_VERSION_STR`. The top-level `README.md` gained a KingresDB section.
  Added `kingres/COPYRIGHT`.
- **Build environment.** Installed the WSL2 distribution Ubuntu 24.04 and
  wrote three scripts: `kingres/dev/setup-ubuntu.sh` (toolchain and an
  unprivileged user), `kingres/dev/build.sh` (mirror, configure, build,
  install, test) and `kingres/dev/ext.sh` (build and test one extension with
  PGXS).
- **Developer documentation.** `kingres/README.md`, the documents in
  `kingres/docs/dev/`, and ADRs 0001 to 0016 in `kingres/docs/adr/`.
- **Design notes** for the two extensions:
  [design/shard-mvp.md](design/shard-mvp.md) and
  [design/ledger-mvp.md](design/ledger-mvp.md).

### How it was verified

- `setup-ubuntu.sh` ran to completion as root; gcc 13.3.0, meson 1.3.2,
  ninja 1.11.1, bison 3.8.2, flex 2.6.4 and Perl `IPC::Run` are present.
- `build.sh all`: 1979 build steps completed; `postgres --version` prints
  `postgres (PostgreSQL) 18.7`; the core regression suite passed
  (`regress/regress`: 235 subtests, 4 of 4 meson tests OK).
- `build.sh test-all`: 323 tests OK, 0 failed, 23 skipped, 0 timed out. The
  reasons for the 23 skips were not examined; upstream skips suites that need
  optional libraries or the `PG_TEST_EXTRA` setting.
- `pg_config.h` in the build directory contains
  `KINGRES_VERSION "0.1.0"`, `KINGRES_VERSION_NUM 100` and
  `PG_VERSION_STR "PostgreSQL 18.7 on x86_64-linux, compiled by gcc-13.3.0, 64-bit (KingresDB 0.1.0)"`;
  `PG_VERSION` is still `"18.7"` and `pg_config --version` still prints
  `PostgreSQL 18.7`.
- The installed PGXS tree contains the Perl test modules and `pg_regress`
  that extension tests need.
- Not verified: `ext.sh` has not been run end to end yet; the first extension
  build will exercise it.

### Needs-verification items checked

- "The exact branch names were not confirmed": `git ls-remote` shows both
  `REL_18_STABLE` and `REL_19_STABLE` on the upstream repository.
- "Rebrand checklist, to verify by grep on the real tree": `PG_VERSION_STR` is
  defined in `meson.build` (and in `configure.ac` for autoconf) and used in
  three C files (`version.c`, `postmaster.c`, psql's `startup.c`). Changing
  the one definition covers all three.
- "Current Windows build requirements unconfirmed": not checked. The build
  runs in WSL2, and a native Windows build is not attempted.

### Decisions and deviations

- **Repository layout.** The repository root is the PostgreSQL tree and
  everything else is under `kingres/`, instead of the `postgres/`
  subdirectory in the architecture document's first sketch. Reason: a rebase
  onto upstream then never involves a path prefix. The architecture document
  was updated. (ADR 0003.)
- **Meson only.** The autoconf build files are not patched. (Recorded in
  core-patches.md.)
- **Default bucket count 128 instead of 1024**, because a bucket is stored as
  one hash partition and 1024 partitions per table is too many relations on a
  small cluster. It remains a setting. The project owner should confirm or
  overrule this. (design/shard-mvp.md.)
- **Replica count is per cluster.** The architecture document's database
  dialog and registry table still listed a per-database replica count; both
  were corrected to match decision 6.
- **OpenAPI first.** The architecture document mentioned generating REST from
  gRPC; it now says the hand-written OpenAPI file is the contract. (ADR 0016.)
- **Copyright holder.** `kingres/COPYRIGHT` named a placeholder holder at
  this point; the project owner supplied the real one later the same day
  (see the newest entry).

### Still open

- Each core patch must be its own commit, as
  [dev/core-patches.md](dev/core-patches.md) requires.
- Create the mirror branches described in
  [dev/upstream-sync.md](dev/upstream-sync.md).
- CI: no pipeline exists yet; it depends on the hosting choice.
- A legal review of the name and of the PostgreSQL trademark policy before
  any public release.
- In progress at the time of this entry: the `kingres_shard` and
  `kingres_ledger` extensions and the REST API contract.

### Core patches

KP-0001 (`meson.build`), KP-0002 (`README.md`).

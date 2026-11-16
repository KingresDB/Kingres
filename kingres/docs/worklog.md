<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Work log

The dated record of what was done on KingresDB, how it was verified, and what
is still open. Newest entry first. The required content of an entry is defined
in [dev/documentation.md](dev/documentation.md).

---

## 2026-11-16: The history of both branches rewritten once, rebalancing in the control plane, kingresd on PostgreSQL 19, phase 7 designed and begun, fix rounds and their reviews

### What was done

- **History rewritten, once, at the request of the project owner.** Two
  scripts of the control plane carried, in eleven early commits, a default
  value that was a path of the developer's machine. The line was replaced
  in those commits (the scripts now ask for `KINGRES_REPO` there), so every
  commit of `kingres-18` after `f78f85220f` has a new id: 75 commits. The
  trees of the last 64 of them are unchanged, and so are authors, dates and
  messages, except that a commit id cited in a message was replaced by the
  new id. The sync commits of `kingres-19` had their messages changed in
  the same way (24 commits, no tree changed). Both branches were
  force-pushed. Every commit id in the documents of the repository was
  replaced by its new id in this commit. An id noted outside the repository
  before this date no longer resolves. See
  [dev/releases.md](dev/releases.md), "History rewritten on 2026-11-16".
- **Phase 4: rebalancing driven by the control plane** (5196e3da5a,
  contract 0.9.0). One task per cluster: checks, a plan asked from the
  extension before every move, one bucket at a time, stop and start again,
  taken over after a restart or a leader change, a retry rule, a report
  with the placement before and after. The task engine gained the states
  paused and skipped. The Rebalance page of the panel. Defects recorded in
  the earlier entry were fixed with it: `distributeTable` with a reference
  table, `removeServer` counting standbys and a stopped rebalance,
  `wal_level` of the instances `kingresd` creates, the coverage of the
  ledger summary.
- **Its independent review**
  ([reviews/2026-11-16-kingresd-rebalance.md](reviews/2026-11-16-kingresd-rebalance.md),
  6b3a1b9847): fourteen findings, RB-01 to RB-14, two of them high and both
  about fencing: a holder that lost the lease of a task still ran the
  failure hook against the new holder's run, and the lease is not proved
  for the call that starts a move.
- **kingresd on PostgreSQL 19** (2aba6ea01e). `VerifyBinaries` accepts
  both majors since 5196e3da5a; the first complete run of the integration
  suite on 19 failed one test (two new settings of 19 had no reason of
  their own in the configuration policy). The installation of a run is
  selectable (`run-integration.ps1 -Install`, `KINGRES_INSTALL`).
- **Phase 7 designed**
  ([design/ledger-phase7.md](design/ledger-phase7.md), 3af21523d8): what of
  it exists, the decisions (signed checkpoints, tile logs per chain and for
  the aggregate, three anchor targets, epochs after a restore), 25 steps.
  **Step 1** (8ceed6b88b): the verifier checks signed notes, checkpoints,
  tile logs and consistency proofs and has a trust file; thirteen
  corrections to the design came out of building it against the
  specifications. **Step 2** (86e15fcae3): `kingres_ledger` 0.3 with
  `ledger_block_preimage`, `ledger_head` and `ledger_blocks_entries`; hash
  format 1 is unchanged.
- **`kingres_shard` 0.5, fixes for S7** (f47ce21bf5): the second phase of
  a commit is sent only when the commit record is confirmed; forced
  `synchronous_commit` on participants; identities of the deadlock
  detector from shared memory; a catalog term; a resumable `promote_node`;
  tagged names of prepared transactions. **Its review**
  ([reviews/2026-11-16-kingres-shard-0.5-fixes.md](reviews/2026-11-16-kingres-shard-0.5-fixes.md),
  f2ba3d95e4): eight findings, S8-01 to S8-08, one high (the durability
  check protects the origin and not a participant) and three medium; a
  verdict on each call sequence the control plane must follow.
- **`kingres_columnar`**: fixes for C2 (87611197d3); a third review
  ([reviews/2026-11-16-kingres-columnar-0.1-second-fixes.md](reviews/2026-11-16-kingres-columnar-0.1-second-fixes.md),
  1069c409f3) with six findings, C3-01 to C3-06, one critical (the
  SERIALIZABLE fix did not hold for a stripe flushed inside a
  subtransaction, and the tests that should have caught it did not flush);
  fixes for C3 (bb4519e787).
- **`kingres_sketch` and `kingres_timepart`**: the review of their fix
  rounds
  ([reviews/2026-11-15-phase12-extensions-fixes.md](reviews/2026-11-15-phase12-extensions-fixes.md),
  6d539e84a5), three findings, and their fixes (f33498c9a5): the policy
  and history tables of `kingres_timepart` are reached through
  security-barrier views instead of row-level security, so a backup role
  that is not a superuser can dump the database.

### How it was verified

Run by the integrator on the tree that was committed, not taken from the
reports of the authors, unless it says otherwise.

- The rewrite: the tree of the new tip of each branch is the tree of the
  old tip; of the 75 commits of `kingres-18`, 64 have an unchanged tree
  and 11 differ in that one line; author, committer and both dates of
  every commit are unchanged; no blob and no message of the new history
  contains the path; every id cited in a message or a document resolves to
  a commit of the new history. The old tip is kept under a local backup
  reference that is not published.
- `kingresd` at 2aba6ea01e, with `kingres_shard` of f47ce21bf5 and
  `kingres_ledger` 0.3 installed into both test installations: one
  complete integration run on each major, 619 top-level tests, none
  failing, none skipped, on PostgreSQL 19rc2 and on 18.7.
- Phase 4 at 5196e3da5a on PostgreSQL 18: 617 of 618 top-level tests
  passed in the complete run; the one that failed (leaving and joining
  again) passed six of six alone; the three-server smoke script, every
  check passed; the panel: 1010 unit tests, 43 browser tests, the
  end-to-end run against a real `kingresd`, 50 of 50 steps.
- `kingres_shard` at f47ce21bf5: out of tree on 18.7, 12 regression tests
  and 69 TAP files with 6858 tests; on 19rc2, 7516 tests; in tree on 18,
  86 of 86 suites; no compiler warning.
- `kingres_columnar` at bb4519e787: in tree on 18 twice, 19 of 19; out of
  tree on 19rc2, 11 regression tests and 15 TAP files with 2949 tests.
- `kingres_ledger` 0.3 at 86e15fcae3: in tree on 18 twice with the
  verifier named (its checks ran on the output of the extension); on
  19rc2, 374 tests. The verifier at 8ceed6b88b: 103 top-level tests.
- `kingres_sketch` and `kingres_timepart` at f33498c9a5: in tree on 18
  twice; on 19rc2, 348 and 134 tests.
- The branch `kingres-19` after each synchronisation: built in the tree
  without a warning, the suites of the five extensions; at the last one,
  113 passed and one skipped.
- Shown by the authors only: that each new test fails without its fix;
  the smoke script and the end-to-end run of the panel on PostgreSQL 19;
  the fuzz targets of the verifier.

### Needs-verification items

- The statements of section 13 of the phase 7 design, except the three
  that step 1 settled (section 14 of the design).

### Decisions and deviations

- The rewrite of history and the force-push of both branches, by decision
  of the project owner. The rule of [dev/releases.md](dev/releases.md)
  that `kingres-19` is never force-pushed by a sync stands; this was not a
  sync.
- The commit message of the phase 4 work claimed that a superseded leader
  can neither record nor start a move; the review showed that it can still
  start one. The commit message of the second fix round of
  `kingres_columnar` claimed a coverage of flushes by size that the tests
  did not have. Both are corrected in the reports and in the README of
  the repository, not in the messages.
- Defaults chosen in the phase 7 design that the project owner may
  change: publication off, ledger tables on one shard group, no anchor
  target on by default, governance mode for write-once storage.

### Still open

- `kingresd`: the findings RB-01 to RB-14 (a fix round is in progress and
  is not committed); failover, switchover and
  `restorePreferredPrimaries` are not written.
- `kingres_shard`: the findings S8-01 to S8-08 (a fix round is in
  progress and is not committed).
- `kingres_columnar`: the fixes for C3 were not reviewed again; five
  tests that the third review asked for are not written.
- `kingres_sketch`, `kingres_timepart`: the fixes for K12R were not
  reviewed again.
- The package tests were not run on PostgreSQL 19 by the integrator.
- Phase 7: steps 3 to 25. Phase 2b, phases 9, 10 and 11 and the rest of
  phase 12 are not started.
- Copies of the old history that were fetched before the rewrite, and the
  hosting service's own cache of unreachable commits, are outside the
  repository's control.

### Core patches

None.

---

## 2026-11-15: One source for PostgreSQL 18 and 19 and two product branches, kingres_shard 0.4 and 0.5, three new extensions, high availability part 1, configuration from the console, AGPL

### What was done

- **One extension source for two majors**
  ([ADR 0023](adr/0023-one-extension-source-for-several-majors.md), commit
  721ff7f034). Every extension builds and passes its tests on PostgreSQL
  18 and 19 from one source; the differences between the majors live in
  one header per extension (`src/pg_compat.h`). A cluster runs one major.
  `kingres_ledger` was ported first (3c04aad2b8), then `kingres_shard`
  (9944f08d01); the three new extensions were written that way from the
  start.
- **Two product branches** ([dev/releases.md](dev/releases.md), commits
  fd834804e3, 23ba13bd2b, ec7d6b1a3e): `kingres-18` on `REL_18_7`, where
  development happens, and `kingres-19` on `REL_19_RC2`, which is the
  upstream tag, the core patches and one "sync from kingres-18" commit per
  synchronisation, so that the KingresDB directories of the two branches
  are byte-identical. Both are published. Releases will be named
  `kingres-<version>-pg18` and `kingres-<version>-pg19`.
- **`kingres_shard` 0.4** (37293a7e77): the fix round for the reviews of
  0.2 and 0.3 (S3-01 to S3-18, S4-01 to S4-10). Later versions moved up by
  one (b59008bb1b). Its independent review
  ([reviews/2026-11-15-kingres-shard-0.4.md](reviews/2026-11-15-kingres-shard-0.4.md),
  d29a408a7d, findings S5) was answered together with the port to
  PostgreSQL 19 (9944f08d01); the review of the port
  ([reviews/2026-11-15-kingres-shard-0.4-port.md](reviews/2026-11-15-kingres-shard-0.4-port.md),
  7cfde8ebd0, findings S6-01 to S6-10) was answered in 0ec398f6a2, which
  also lets the extensions be created in any order (an extension that
  finds the schema `kingres` already made by another no longer fails).
- **`kingres_shard` 0.5** (5cc5f078db): the part of phase 5 that belongs
  to the extension. Standby nodes in the catalog, `kingres.promote_node`,
  prepared transactions that survive a failover (the name of a prepared
  transaction carries the group, the timeline, the backend and the
  target), distributed deadlock detection, `kingres.create_restore_point`.
  Its independent review
  ([reviews/2026-11-15-kingres-shard-0.5.md](reviews/2026-11-15-kingres-shard-0.5.md),
  bb0f2813d9) has ten findings, S7-01 to S7-10: one critical (the second
  phase of a commit is sent after a cancelled wait for the synchronous
  standby) and six high. The README says that failover is not to be
  relied on yet.
- **`kingres_sketch` 1.0 and `kingres_timepart` 0.1** (5fc2a70fad), the
  parts of phase 12 that depend on nothing else: approximate distinct
  count, top-n and approximate percentiles as types with aggregates that
  support partial aggregation; creation of time partitions ahead of time
  and retention for ordinary partitioned tables. Their review
  ([reviews/2026-11-15-phase12-extensions.md](reviews/2026-11-15-phase12-extensions.md),
  findings K12-01 to K12-17) was answered in e6ee42f265 and 873cba63ce.
- **`kingres_columnar` 0.1** (b25c43c884): columnar storage as a table
  access method, with compression, skipping by minimum and maximum, and a
  custom scan that reads only the columns a query needs. Two independent
  reviews:
  [the first](reviews/2026-11-15-kingres-columnar-0.1.md) (4988395b25,
  C1-01 to C1-14) was answered in 5555b252a0, which changed the on-disk
  format; [the second](reviews/2026-11-15-kingres-columnar-0.1-fixes.md)
  (9e19a38979) confirmed those fixes and has nine new findings, C2-01 to
  C2-09, one of them critical (under SERIALIZABLE a write that a
  concurrent reader cannot see is not detected as a conflict). The README
  warns against real use.
- **High availability, part 1, in `kingresd`** (acebb486b6, contract
  0.7.0): heartbeats and liveness of servers, leases with self-fencing
  (enforced only in a group that has a standby), the watchdog
  `kingres-watchdog`, creation and supervision of standbys, the client
  entry point. Two defects found by the three-node package test were
  fixed in d05808e9ff: a primary started before it held its lease and was
  stopped by its own fence at the first failed renewal (it now waits for
  the lease at a cold start), and an orderly stop of all instances could
  be taken for a lost lease (the stop now stands the watchdog down).
  Failover and switchover themselves are not written.
- **Phase 13, configuration from the console** (design 2604fcaaab,
  implementation 1a7a04f3c2, contract 0.8.0 with 82 paths and 100
  operations): the settings of PostgreSQL and the time zone, read and
  changed for the whole cluster, a server group or one server, with a
  preview, a history, reverting a change, and a restart of the instances
  that wait for one. `kingresd` writes the settings it manages into a file
  of its own that the instance includes.
- **Web console**: the top bar is one black row in both themes, chart
  legends, aligned form rows, a footer on every page with the copyright,
  the link to the project site, the licence and a link to the source of
  the running version, the horizontal logo on the sign-in page
  (2f4ab3bbbf and later); the pages of contract 0.7.0 (d546dcc06d) and
  the configuration page of 0.8.0 (1a7a04f3c2).
- **Packaging** (e48d2e4ec4, 240c26897f, bad3732c05, 3e04d655bf): the
  watchdog is shipped; the image, the compose file and the Debian package
  are built for either major (`kingres_<version>+pg18-<rev>_amd64.deb`,
  `kingres:<version>-pg18`, and the same for 19); the package of one major
  refuses to install over the data of the other; the licence metadata
  says AGPL-3.0-only.
- **Licence** ([ADR 0024](adr/0024-agpl-for-kingresdb-code.md),
  daa471e852): KingresDB's own code is `AGPL-3.0-only`; the code of
  PostgreSQL keeps the PostgreSQL License; the two files that adapt
  PostgreSQL code say `AGPL-3.0-only AND PostgreSQL`. Every file of
  KingresDB carries the identifier. ADR 0004 is superseded in its choice
  of licence.
- **Roadmap** (7fcbc2e6ad): the first release is every phase except
  phase 8 (several map groups and geo-distribution), which moves to the
  future roadmap. Nothing is tagged before all of them are finished. The
  design of phase 2b was revised against `kingres_shard` 0.4 (5754aa4f5b).

### How it was verified

Run by the integrator on the tree that was committed, not taken from the
reports of the authors, unless it says otherwise.

- `kingres_shard` at 0ec398f6a2: out of tree on PostgreSQL 18.7, 12
  regression tests and 60 TAP files with 6610 tests; on 19rc2, the same 12
  and 60 files with 6716 tests; no compiler warning with `-Werror` on
  either; in the tree on 18, 75 of 75 suites. No test of the version of
  the server outside the compatibility header.
- The branch `kingres-19` after the synchronisation at 0ec398f6a2: built
  in the tree with no warning; the suites of the five extensions, 95
  passed and one skipped (`t/046_two_majors`, which runs only when both
  installations are named).
- `kingres_ledger`: 281 subtests on 18 and 280 on 19. `kingres_sketch`:
  339 subtests on 19. `kingres_timepart`: 105 on 19. Each also in the tree
  on 18.
- `kingres_columnar` at 5555b252a0: on 19rc2 out of tree, 11 regression
  tests and 8 TAP files with 993 tests; in the tree on 18 twice, 12 of 12
  suites.
- `kingresd` at 1a7a04f3c2: build for Windows and Linux, `go vet`,
  `gofmt`, unit tests; the integration run against real instances of
  PostgreSQL 18: 40 test binaries, 594 top-level tests, no failure, none
  skipped; the three-server smoke script, 20 of 20 steps. The contract
  checker passes on 0.8.0.
- Web console at 1a7a04f3c2: type check, lint, 998 unit tests (passed
  twice at normal load; one to three tests that poll failed in three runs
  while the machine was saturated by other work), 43 browser tests,
  production build, generated-API and brand checks, and the end-to-end run
  against a real `kingresd`, 50 of 50 steps.
- Packaging at 5555b252a0 on PostgreSQL 18: the image test, 129 passed and
  one known failure; the compose test of three nodes, 162 passed; the
  Debian package, 202 passed and one known failure, `lintian` without an
  error. The check that the package of the other major is refused was run
  by its author only, and so were all package tests on PostgreSQL 19.
- The reviews ran on an export of the committed tree, by a reviewer who
  had not seen the work; what each could not cover is listed at the end of
  its report.

### Needs-verification items

- "PostgreSQL 19 is released on 2026-11-19": the date is the announced
  one; `kingres-19` stays on `REL_19_RC2` until the tag `REL_19_0` exists.
- The statements the new design documents label as unchecked stay
  unchecked.

### Decisions and deviations

- [ADR 0023](adr/0023-one-extension-source-for-several-majors.md) and
  [ADR 0024](adr/0024-agpl-for-kingresdb-code.md), both at the request of
  the project owner.
- **No check runs on GitHub.** GitHub Actions is switched off for the
  repository and no workflow of KingresDB is published; every check runs
  on the developer's machine ([dev/releases.md](dev/releases.md)). This
  replaces the open item "CI workflow" of the earlier entries.
- **No tag before the first beta**, and no tag of upstream PostgreSQL is
  pushed to the repository of KingresDB.
- **Version numbers of `kingres_shard`**: 0.4 is the fix round, 0.5 the
  high-availability part, 0.6 phase 2b, 0.7 to 0.9 phases 9 to 11. The
  design documents written before this still show the older numbers; the
  version paragraph of [architecture.md](architecture.md) is the
  reference.
- **Order of work**, set by the project owner: phases 5 and 4 are
  finished first, then phase 2b, then phases 9, 10, 11, the rest of 12 and
  13, then phase 7.
- A lease is enforced only in a group that has a standby, as decided in
  the previous entry; it is implemented that way.

### Still open

- `kingres_shard` 0.5: the findings S7-01 to S7-10. A fix round is in
  progress and is not committed. The fixes of S6 were not reviewed again.
- `kingres_columnar`: the findings C2-01 to C2-09. A fix round is in
  progress and is not committed.
- `kingres_sketch` and `kingres_timepart`: the fix rounds of K12 were not
  reviewed again.
- `kingresd`: failover, switchover and `restorePreferredPrimaries` are
  not written; the six rebalance operations are in progress and are not
  committed. `kingresd` accepts only the binaries of PostgreSQL 18
  (`VerifyBinaries`), so the integration suite was never run on
  PostgreSQL 19 and the package tests fail there.
- Phase 2b, phases 9, 10 and 11, the rest of phase 12 (repartitioned
  joins, `MERGE`, columnar storage for distributed tables, time
  partitioning for distributed tables and its scheduler, pushdown of
  sketches) and phase 7 are not started.
- The contract and control-plane defects listed under "Still open" of the
  previous entry are part of the rebalance work in progress.
- `kingres-19` must be moved to `REL_19_0` when upstream tags it; that
  push changes an upstream workflow file and needs a credential with the
  permission for it.

### Core patches

KP-0003 (`contrib/meson.build`, now five `subdir` lines: columnar,
ledger, shard, sketch, timepart) and KP-0004 (`LICENSE`, a new file at
the top of the tree with the text of the GNU Affero General Public
License, version 3; the `COPYRIGHT` file of PostgreSQL is not changed).
Register: [dev/core-patches.md](dev/core-patches.md).

---

## 2026-11-15: Contract 0.6.0, the fix round of the second kingresd review, the panel through 0.6.0, kingres_shard 0.3 and its review, phases 9 to 12 added to the roadmap

### What was done

- **Control plane, fix round for the second review** (commit 8d1b2dfc04;
  findings CP2-01 to CP2-22 of
  [reviews/2026-11-15-kingresd-0.4.md](reviews/2026-11-15-kingresd-0.4.md)).
  A forwarded request now carries a proof of the caller's identity that
  only the manager leader can make, per request; task leases use the clock
  of the system database and carry a fencing epoch; the audit log is read
  cluster-wide; the rate limiter no longer counts successful sign-ins; a
  server whose internal API is switched off answers `internal_api_disabled`.
- **REST contract 0.6.0** (same commit), 71 paths and 88 operations. Two
  breaking changes: `addServer` needs `kingres_admin`, and `addServer` and
  `joinCluster` without a fingerprint pin are refused with
  `validation_failed` unless the request says `allowUnpinned: true`.
  Additive: `internalFingerprint` on the state and the local-server
  answers, `complete` and `uncoveredShardGroupIds` on ledger digests and
  verifications, `AuditEvent.serverId`, partial answers of the audit log,
  the alert kind `replicationPending`, `LedgerProofBlock`, a degraded
  session (`Session.degraded`, `degradedReason`, `clientAddress`), a dry
  run of `replaceNetworkAccess`.
- **Web panel**: an independent review
  ([reviews/2026-11-15-web-panel-0.5.md](reviews/2026-11-15-web-panel-0.5.md),
  findings W2-01 to W2-16, commit d349e59483); its fixes together with the
  certificate actions and the display of incomplete cluster-wide answers
  (39652843fc); then contract 0.6.0 (e64277c568): choosing or refusing a
  pin when a server is added or joins, the server's own fingerprint, ledger
  coverage, the degraded session, the dry run of network access. The copy of
  the session in `sessionStorage` that 39652843fc introduced was removed
  again in e64277c568; the browser stores the theme only.
- **Packaging**: the bucket count reported by a follower is an ordinary
  check of the three-node test again (053b7a3437).
- **`kingres_shard` 0.3** (6ae61063eb, design
  [design/shard-phase4.md](design/shard-phase4.md)): `kingres.move_bucket`
  (copy, logical replication with a marker row that proves the copy is
  complete and replayed, a two-phase switch that detaches on the source and
  attaches on the target), `kingres.rebalance_plan`,
  `kingres.check_cluster`, `kingres.set_node_property`. Scans and fanned-out
  writes compare a hash of the placement before and after they open their
  cursors and fail with the retryable `KS001` when it changed.
- **A defect that was already in 0.2**, found while verifying 0.3: the
  recovery worker sent its two `SET` commands as one statement of the
  extended protocol, which PostgreSQL refuses, so its remote sessions had no
  statement or lock timeout. Fixed in `src/recovery.c` (one statement each;
  a node whose session cannot be bounded is skipped and logged), with a
  check in `t/022_timeouts_and_worker.pl` that the logs of the nodes do not
  hold the refusal.
- **Independent review of `kingres_shard` 0.3**
  ([reviews/2026-11-15-kingres-shard-0.3.md](reviews/2026-11-15-kingres-shard-0.3.md),
  commit 3c0f21b1a9): ten findings, S4-01 to S4-10. Three are serious and
  were reproduced: a bucket that moves away and back while a scan waits
  leaves the placement hash unchanged, and the scan returns an incomplete
  answer without an error (S4-01, critical); the recovery worker neither
  resumes nor cleans an interrupted move (S4-02); a write made directly into
  a bucket partition is not covered by the marker proof (S4-03).
- **Roadmap**: phases 9 to 12 were added to
  [architecture.md](architecture.md) after a comparison with the
  documentation of Citus 13.0: table lifecycle (distributing a table with
  rows, undistributing, changing the distribution column or the bucket
  count, partitioned tables), elasticity (bucket split, background
  rebalancing with strategies, draining a server), distributed schemas, and
  real-time analytics (repartitioned joins, `MERGE`, approximate aggregates,
  columnar storage, time partitioning with retention). Each has its design
  document, linked from the roadmap table.
  [ADR 0022](adr/0022-the-complete-scan-stays.md) records the rule these
  phases are built under: the row-moving distributed scan stays as the path
  that answers every query.

### How it was verified

Run by the integrator on the tree that was committed, not taken from the
reports of the authors, unless it says otherwise.

- `kingresd` at 8d1b2dfc04: build for Windows and Linux, `go vet`, `gofmt`,
  unit tests of 32 packages; the integration run in WSL against real
  instances: 32 test binaries, 456 top-level tests, no failure, none
  skipped. The three-server smoke script was run by its author only.
  `TestFanoutCallsEveryServerInParallelWithItsOwnDeadline` failed once
  under load; its deadlines were too short for a loaded machine and were
  lengthened (30 repeats passed afterwards).
- Contract: `scripts/check-contract.mjs` passes on 0.6.0.
- Web panel at e64277c568: type check, lint, 577 unit tests, 22 browser
  tests, production build, generated-API and brand checks. The end-to-end
  run against a real `kingresd` (45 of 45) was run by its author only.
- Packaging at 8d1b2dfc04: the compose test of three nodes joined into one
  cluster, 139 checks passed and one known failure (a stopped follower is
  still listed as healthy: there are no heartbeats before phase 5).
- `kingres_shard` 0.3 at 6ae61063eb: out of tree, 12 regression tests and
  32 TAP files with 5151 subtests; in the tree twice, 47 of 47 suites, no
  compiler warning; `t/022` thirty times under CPU load without a failure.
  Before the fix of the worker and of the test, `t/022` failed 2 of 7 runs
  under load.
- The review of 0.3 ran the whole suite once (35 of 36; `t/027` failed
  under load and passed alone) and did not cover `COPY`, user two-phase
  transactions, or long `REPEATABLE READ` transactions across a switch.
- The design documents of phases 9 to 12 are designs: nothing in them was
  built or run.

### Needs-verification items

None of the items of `architecture.md` was checked in this unit of work.
The new design documents label their own unchecked statements.

### Decisions and deviations

- Lease loss stops a primary only in a group that has a standby; in a group
  without one it only raises an alert (decided for the phase 5 work in
  progress; to be recorded in the phase 5 design when that work lands).
- Work on `contrib/kingres_shard` is done by one line of work at a time;
  parallel changes to it conflicted.
- [ADR 0022](adr/0022-the-complete-scan-stays.md), at the request of the
  project owner.

### Still open

- The findings S3-01 to S3-18 and S4-01 to S4-10 of `kingres_shard`: a fix
  round is in progress and is not committed. Until S4-01 to S4-03 are fixed
  a bucket move is not safe for real use.
- `kingresd`: the six rebalance operations, `failoverServer` and
  `restorePreferredPrimaries` are not implemented (80 of 88 operations
  are); the fencing epoch is not yet sent to remote callees; ledger digests
  cover one shard group each. Phase 5 part 1 (heartbeats, leases, standbys,
  the client proxy port) is in progress and is not committed.
- Phase 2b and phases 9 to 12 are designs only. The five new designs each end
  with a list of the statements that still need verification (14, 14, 17,
  17 and 27 of them), and the points that cross
  phase boundaries are listed under the roadmap table in
  [architecture.md](architecture.md).
- Defects in the existing code and contract that the design work found, not
  fixed yet:
  - the contract says `distributeTable` moves rows and that a distributed
    table can be a ledger table at the same time; the extension needs an
    empty table and refuses a ledger table;
  - `kingresd` answers `distributeTable` with `method: reference` as not
    supported, although the extension has `create_reference_table`;
  - `removeServer` counts the standbys a server hosts as buckets it owns,
    so with chained placement no server can be removed before its standbys
    are relocated, which the contract does not say;
  - the task engine declares the state `paused` but nothing sets it, and a
    stopped rebalance is counted as running;
  - `create_distributed_table` does not look at the access method of the
    table, and `ledger_enable` does not either;
  - `choose_colocation` takes `max(colocation_id) + 1`, which can reuse the
    number of a dropped group;
  - `COPY <table> TO` of a distributed table was never examined and probably
    returns the local partitions only;
  - a `SELECT` that reads a distributed table of another shard group
    together with a local table is refused, which ADR 0022 does not allow
    for reads;
  - DDL runs on the primary of the map group only, so a client that creates
    schemas or tables through another server is refused.
- The CI workflow is not in the repository: the access token in use cannot
  push workflow files.

### Core patches

None.

---

## 2026-11-15: Contract 0.4.0 complete and 0.5.0, certificates, cluster-wide reads, a real three-node cluster test, second review of kingresd

### What was done

- **Control plane, 80 of 88 operations** (commits a51d630d53 and
  f6f00796fe):
  - the rest of contract 0.4.0: server groups, inclusion proofs, the list of
    ledger verifications, start, stop and restart of an instance, leaving a
    cluster, local settings; the security headers and the cookie attributes
    the panel's review asked for; crash points in the task engine with a
    test that interrupts a join at every step;
  - certificates: every server renews its own certificate when a third of
    its lifetime is left, the key stays, nothing is restarted, and the
    revocation list is issued again long before it runs out; an
    administrator can renew or revoke a server's certificates;
  - `listLogs`, `listAlerts` and `queryMetrics` answer for the whole cluster
    from any server and name the servers that did not answer;
  - four alerts are raised: `certificateExpiring`,
    `certificateRenewalFailed`, `ledgerPendingTooOld`, `versionMismatch`.
- **REST contract 0.5.0**, additive only: the two certificate operations,
  `partial` and `missingServers` on the log, alert and metric answers, two
  problem codes, two task kinds, one alert kind.
- **Web panel** (commit 36af22e61f): follows every entry of the 0.4.0
  changelog (ledger proof and byte-exact digest download, fingerprint pin
  when adding or joining a server, instance start and stop, local settings,
  leave cluster, server groups), shows one calm state while the system
  database is stopped, and is generated from contract 0.5.0.
- **Packaging** (commit 16da482e03): the compose test joins the three nodes
  into one cluster through the REST API, once with `addServer` and once with
  `joinCluster`, both with the certificate fingerprint pinned, and then
  writes on one node and reads on all three. The compose file had the
  internal API on a port no peer looks at; it is corrected.
- **Second independent review of `kingresd`**
  ([reviews/2026-11-15-kingresd-0.4.md](reviews/2026-11-15-kingresd-0.4.md)):
  22 findings, none critical, four high.

### How it was verified

Run by the integrator on the committed tree unless it says otherwise.

- Control plane: Windows and Linux builds, `go vet`, `gofmt -l`, unit tests
  of 30 packages, and the WSL integration run with real PostgreSQL: 30 test
  binaries, 391 top-level tests, none failed, none skipped. The race
  detector was run by the author only, on Windows, without PostgreSQL.
- Contract 0.5.0: the rule checker passes (71 paths, 88 operations, 172
  schemas).
- Panel: typecheck, lint, build, generated-files check and brand check
  clean; 396 unit and component tests; 17 tests in a headless browser. The
  end-to-end run against the real server (44 steps, including a proof
  checked by `kingres-verify`) was run by its author only.
- Packaging: the compose cluster test from a clean snapshot of the commit,
  138 checks, none failed, two marked as known defects of `kingresd` (below).
  The image and package tests were not run again by the integrator after
  this change; the author ran the image test (79 checks).

### Decisions and deviations

- A renewed certificate keeps the server's key and names, and the previous
  certificate stays valid until it expires. The CA therefore records every
  serial it issues, and a revocation covers all of them.
- A revoked server gets back in by being removed and joining again. That
  works only for a server that holds no bucket copy until rebalance exists.
- Alerts stay on the server that raised them; an alert id carries the
  server's ordinal and stays below 2^53.
- Cursors of logs and alerts changed format with 0.5.0; a cursor from 0.4.0
  answers 400.
- The panel's unit tests run with six workers: two timer-dependent tests
  failed now and then with one worker per core.

### Still open

- **`kingresd`, second review**: a forwarded identity is accepted by name
  from any holder of a cluster certificate (CP2-01); `addServer` lets
  `kingres_ops` reach `kingres_admin` (CP2-02, by reading only); a pinned
  fingerprint is dropped when `addServer` goes through a follower (CP2-03);
  task leases use the claiming server's clock (CP2-06); ledger verification
  covers the leader's shard group only but answers `passed` (CP2-08). The
  fix round is running. CP2-04 (no renewal) is closed by the certificate
  work above.
- **`kingresd`, found by the compose test**: `getDatabase` on a follower
  answers a null bucket count, and a stopped follower stays listed as live
  because there is no heartbeat (phase 5).
- **Eight operations** still answer "not supported yet": the six of
  rebalance, `failoverServer` and `restorePreferredPrimaries`.
- **`kingres_shard` 0.3** (moving buckets) is written but not committed: one
  of about fifteen thousand scans during a move returned a wrong count
  without an error. The cause is being looked for. The fix round for S3-01
  to S3-18 has not started.
- **Panel**: no buttons for renewing or revoking certificates and no notice
  for incomplete cluster-wide answers yet; both are being built. An
  independent review of the fix round and of the 0.4.0 change is running.
- A file pushed in commit 57b8924ca2 contained a machine-local path; the
  file is corrected, the line remains in history.

---

## 2026-11-15: Several servers in one cluster, four independent reviews and their fix rounds, contract 0.4.0

### What was done

- **Control plane, several servers**
  ([design/control-plane-3.md](design/control-plane-3.md)): cluster
  operations are forwarded to the manager leader; a server joins a cluster,
  and servers are added and removed, with certificate trust between the
  instances (`hostssl` with client certificates mapped through
  `kingres_nodes`). Seven mismatches with the contract found by the panel's
  end-to-end run were fixed.
- **Independent reviews**, each by a reviewer who did not write the code and
  who had to reproduce what was claimed:
  - `kingres_shard` 0.2
    ([reviews/2026-11-14-kingres-shard-0.2.md](reviews/2026-11-14-kingres-shard-0.2.md)),
    then a second review of its fix round
    ([reviews/2026-11-14-kingres-shard-0.2-fixes.md](reviews/2026-11-14-kingres-shard-0.2-fixes.md));
  - `kingresd`, as a security review
    ([reviews/2026-11-14-kingresd-control-plane.md](reviews/2026-11-14-kingresd-control-plane.md),
    findings CP-01 to CP-07, two of them critical);
  - the web panel
    ([reviews/2026-11-14-web-panel.md](reviews/2026-11-14-web-panel.md),
    findings W-01 to W-18, none critical).
- **Fix rounds**, every fix with a test that fails without it:
  - `kingres_shard` 0.2: the findings of the first review (the recovery
    ordering rule, comparison of session settings, serialized writers of
    reference tables, authentication of forwarded sessions with a token
    derived from a cluster secret, bounded waits).
  - `kingres_ledger` 0.2 and the verifier: the eight findings of the review.
  - `kingresd`: CP-01 to CP-07. Connection strings are built in one place
    that quotes every value; the roles of a session are derived again on
    every request and a role change ends that role's sessions on every
    server; failed statements are not written to the log; workbench results
    are bounded; dangerous memberships and attributes are refused.
  - Web panel: W-01 to W-18, plus nine defects found in pages the review
    had not examined.
- **REST contract 0.4.0** (first part): reading logs needs `kingres_ops`,
  `Task.detail`, `expectedFingerprint` on the join and add-server requests,
  the `replicate.apply` task kind, three alert kinds.
- **Packaging** for the `kingresd` that manages instances: the unit is
  `Type=notify` with `KillMode=process`, so restarting the manager leaves
  the database running; the three artifact tests now create a real cluster
  through the API and restart it in every mode.
- **Brand**: the project's logos are used in the panel (sign-in page, top
  bar, favicon) and in the top-level README; the sources are in
  `kingres/brand`.
- **Design**: phase 5, high availability
  ([design/ha-phase5.md](design/ha-phase5.md)), with
  [ADR 0021](adr/0021-failover-in-kingresd.md): failover is part of
  `kingresd` and is fenced by leases granted by a majority of servers;
  Patroni and etcd are not shipped. Phase 2b of sharding
  ([design/shard-phase2b.md](design/shard-phase2b.md)): pushdown of
  aggregates, sort and limit, joins of colocated tables, and row routing
  for bulk writes.
- **Architecture document**: a table near its top lists where the
  implementation departs from the original design, with the ADR of each.

### How it was verified

Everything below was run by the integrator on the tree that was committed,
unless it says otherwise.

- `kingres_shard` 0.2 after its fix round: 11 pg_regress tests and 4405 TAP
  subtests in 24 files out of tree, and both extension suites in tree (39 of
  39). The second reviewer reverted 17 fixes one at a time and saw the
  guarding test fail each time, and ran 16 crashes under load with no
  half-committed and no lost acknowledged transaction (reviewer's run).
- `kingres_ledger` 0.2 after its fix rounds: 11 pg_regress tests and 281 TAP
  subtests in 10 files; in tree 30 of 30 at that commit.
- Control plane: Windows and Linux builds, `go vet`, `gofmt -l`, `go test`
  (27 packages), and the WSL integration run with real PostgreSQL (27 test
  binaries, no failure, no skipped test), after each commit.
- Contract 0.4.0: the rule checker passes (69 paths, 86 operations).
- Packaging, built from a clean snapshot of the commit: image test 79
  checks, compose test 58, package test under systemd 158, none failed.
- Panel: typecheck, lint, build and brand check clean; 291 of 292 unit and
  component tests; 17 tests in a headless browser (layout at 1366, 683 and
  320 px, axe in light and dark, and a run of every page under the
  Content-Security-Policy the README prescribes). The one failing test is
  the guard that compares the generated API types with the contract: the
  contract moved to 0.4.0 while the fix round was running, and the panel
  is adapted in the next change.
- The panel's end-to-end run against the real server was run by its author
  only (28 of 28 steps mid-work; a later run stopped at a step that expects
  an operation to be unsupported which the server meanwhile implements).

### Needs-verification items checked

- "The fencing design and the persistence of in-doubt transactions after
  failover come from background knowledge": designed in
  [design/ha-phase5.md](design/ha-phase5.md); not implemented or tested
  yet.
- "Bundling Patroni with a Python runtime; embedded etcd membership": no
  longer applies; see ADR 0021.
- "Node-to-node traffic uses mutual TLS with a cluster CA created at cluster
  init": implemented and tested with three servers.

### Decisions and deviations

- ADR 0021 supersedes ADR 0012 (see above). Automatic failover will be off
  by default, and needs three servers to survive the loss of one.
- A session's roles are re-derived on every request with a cache of three
  seconds; a server that cannot ask its database answers 503 instead of
  trusting the session.
- Any change to a role, including the role changing its own password, ends
  that role's sessions.
- The managed PostgreSQL configuration never logs the text of a failed
  statement, and local settings cannot switch that back on.
- The panel refuses to address objects named `.` or `..`: a browser removes
  such path segments even when they are percent-encoded. The contract needs
  an escape for them or the server must refuse to create them.
- The package's unit restarts the database on `systemctl restart`; a
  restart of the manager alone is `systemctl kill --kill-whom=main
  --signal=SIGTERM kingres`.

### Still open

- **`kingres_shard`**: the second review found eighteen new defects (S3-01
  to S3-18). Two are critical and leave the copies of a reference table
  different without an error: a write made by a function or trigger inside
  a routed statement reaches one copy only, and a cached plan of a write
  keeps writing the local copy after the table becomes a reference table.
  Their fix round has not started. Phase 4 (moving buckets) is being
  implemented; phases 2b and 5 are designed only.
- **Control plane**: 19 of 86 operations still answer "not supported yet"
  (server groups, inclusion proofs, the verification list, instance start
  and stop, leave-cluster, local settings, rebalance, failover). The
  certificate revoke and renew operations and their renewal job, cluster-
  wide gathering of logs, alerts and metrics, and the security headers the
  panel's review asks for are not written.
- **Panel**: not yet adapted to contract 0.4.0; the workbench cannot cancel
  a statement on the server (the contract has no such operation).
- **Packaging**: the compose test does not join the three nodes into one
  cluster yet.
- **CI**: still on a local branch only, for the reason given in the
  previous entry.
- The fix rounds of `kingresd` and of the panel have not had a second
  independent review.

### Core patches

None added in this entry.

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

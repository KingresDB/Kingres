<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Testing

## Rules

1. Every phase deliverable includes tests. The architecture document's roadmap says: "In every phase, the deliverable must include TAP tests, documentation and a release tag." A phase is not finished while its features have no tests.
2. Every change is "done" only when it has tests, documentation and a work-log entry (see "Definition of done").
3. A bug fix starts with a test that fails without the fix.
4. Tests do not depend on the developer's machine: no fixed ports, no fixed paths outside the test's temporary directory, no network access except to servers the test starts itself.
5. A test that is flaky is a bug. Fix it or remove it; do not retry until it passes.

## Strategy per layer

| Layer | What is tested | Tool | Where the tests live |
|---|---|---|---|
| Extension, single server | SQL interface, catalog behaviour, error codes and messages, GUCs, upgrade scripts, hook chaining | pg_regress | `contrib/kingres_*/sql/` and `expected/` |
| Extension, process and WAL level | Crash recovery, restart, background workers, `shared_preload_libraries` loading, replication of ledger records, behaviour with a standby | PostgreSQL TAP tests (Perl, `PostgreSQL::Test::Cluster`) | `contrib/kingres_*/t/` |
| Multi-node cluster | Routing across groups, 2PC and its recovery after a crash, bucket movement and rebalance, failover and "stale map" retry, map group failure, simultaneous map-group and shard-group failure | TAP tests that start several `Cluster` instances; for failover, the cluster tests of `kingresd` that start real PostgreSQL instances (failover is part of `kingresd`, [ADR 0021](../adr/0021-failover-in-kingresd.md); Patroni and etcd are not used) | `contrib/kingres_shard/t/` for extension-level; `kingres/dev/` or `kingres/control-plane/` for those that need the control plane (location to be confirmed) |
| Ledger verification | Hash chain and Merkle roots against known test vectors; tamper detection after direct file or row modification; independence of the verifier from the server | pg_regress and TAP for the extension; Go tests in `kingres/verifier/`; a test that runs the verifier against a dumped database | as above |
| Go control plane, unit | Pure logic: state machines, task engine steps, planners for rebalance, config parsing | `go test` with table-driven tests, `-race` | `*_test.go` next to the code |
| Go control plane, integration | Agent and manager against real PostgreSQL instances, task resume after restart, leader election | `go test` with a build tag or an environment guard for tests that need a PostgreSQL build (names to be confirmed) | `*_test.go` next to the code |
| API contract | The running server conforms to `kingres/api/openapi.yaml`: every route in the spec exists, every handler is in the spec, responses and error bodies validate against the schemas | A contract-test harness validating requests and responses against the spec (`kingres/control-plane/internal/httpapi/contract_test.go` for the server, and `npm run api:check` and the contract tests in `kingres/web/` for the client). Also: the spec itself is linted with Redocly and checked with `kingres/api/scripts/check-contract.mjs`, run locally | `kingres/control-plane/` for server-side; `kingres/web/` for client generation check |
| Web unit and component | Components, hooks, forms, accessibility queries, behaviour on API errors | Vitest and Testing Library; network mocked using responses valid against the OpenAPI spec | `*.test.tsx` next to the component |
| Web end to end | Wizard (create or join cluster), create database, rebalance flow | `npm run e2e` (`kingres/web/scripts/e2e/run.mjs`: the production panel served by a real `kingresd`, driven by a headless browser through `puppeteer-core`) and `npm run test:browser` | `kingres/web/` |
| Packaging | The package installs, the service starts "uninitialized", first-run token behaviour, uninstall | Container-based tests (to be designed in phase 3) | `kingres/packaging/` |

Notes on specific areas:

- **Hook chaining.** At least one test per extension loads another extension that uses the same hook and checks that both work (coding-standards.md, 1.7).
- **Failure tests.** The architecture document marks several behaviours **[Needs verification]** and says they must be tested with a prototype and failure tests: simultaneous failure of the map group and a shard group, several instances managed by `kingresd` on one host, router behaviour during a switchover, persistence of in-doubt transactions after failover. Each such item gets a test when its feature is built, and the result goes in the work log.
- **Documented anomaly.** With 2PC and no global snapshot (ADR 0009), a cross-shard read may see a partially committed distributed transaction. A test documents this behaviour so that a later change is noticed. Single-shard transactions are tested for full correctness.
- **Ledger.** Canonical serialization is tested with fixed vectors that are checked in, including cases that vary `DateStyle`, `extra_float_digits`, `bytea_output` and locale, which must not change any hash.
- **Upgrade.** Each extension has a test that upgrades from the oldest supported version to the current one and compares the result with a fresh install.

## Commands

The commands below are the standard ones for each tool. There is no CI: GitHub Actions is switched off for the repository ([releases.md](releases.md), "Where the checks run"), and every check is run locally. The scripts that wire them are in `kingres/dev`, `kingres/control-plane/scripts`, `kingres/web` and `kingres/packaging/test`; paths and flags not yet verified are to be confirmed.

- PostgreSQL tree, meson: `meson test` (TAP tests require `-Dtap_tests=enabled` at setup; the architecture document cites the meson install page). Autoconf: `make check-world` with `--enable-tap-tests` at configure time. Build and test steps for WSL2 are in [getting-started.md](getting-started.md).
- Go: `go test ./...`, `go test -race ./...`, `go vet ./...`, `golangci-lint run`.
- Web: the package scripts will expose `test` (Vitest), `lint` (ESLint), `typecheck` (`tsc --noEmit`) and `format:check` (Prettier); names to be confirmed when `package.json` exists.

## Running the suites on both PostgreSQL majors

Every extension must pass its regression and TAP suites on every supported major, 18 and 19 (ADR 0023).  The extension scripts build and test out of tree with PGXS against an installed server, so a second major is a second installation; the server itself is not rebuilt for an extension change.

1. **A PostgreSQL 18 installation** is the one `kingres/dev/build.sh` makes (`$KINGRES_WORK/install`).
2. **A PostgreSQL 19 installation** is the one `kingres/dev/build.sh` makes from a checkout of `kingres-19`, which is on `REL_19_0` since 2026-11-22. Before `kingres-19` existed it was built from an upstream tag in a scratch tree (see [upstream-sync.md](upstream-sync.md), "A new major version"); the steps, first run on `REL_19_RC2` and valid for `REL_19_0`, with `$W` as the work directory:

   ```sh
   git clone --depth 1 --branch REL_19_0 https://github.com/postgres/postgres.git $W/src
   git -C <repository> diff REL_18_7 HEAD -- meson.build contrib/meson.build README.md | git -C $W/src apply
   cd $W && meson setup build src --prefix $W/install --buildtype debugoptimized
       -Dcassert=true -Dtap_tests=enabled -Ddocs=disabled
   ninja -C build -j4 && ninja -C build install
   ```

   The options are those of `build.sh`.  The `subdir('kingres_*')` lines of the core patch make the in-tree build look for the extension directories; link or copy them into `$W/src/contrib/` before `meson setup`, or leave the `contrib/meson.build` hunk out of the scratch tree and build the extensions out of tree only.
3. **One extension, one major**: `KINGRES_PG=<pg_config or installation directory> kingres/dev/ext.sh <extension> all`.  With `KINGRES_PG` set the copy, the throwaway data directory and the port carry the major version, so two majors can run from one work area, even at the same time.  Without it the script behaves as before.  `EXT_WERROR=1` turns compiler warnings of the extension into errors, and `ext.sh <extension> clean build` makes a build that reports all of them.
4. **One extension, all majors**: `KINGRES_PG18=<installation> KINGRES_PG19=<installation> kingres/dev/ext-majors.sh <extension>`.  It runs `sync clean build install regress tap` on each configured major with warnings as errors, prints one line per major and exits non-zero if any failed.  `KINGRES_PG18` defaults to the `build.sh` installation; with no `KINGRES_PG19` only 18 is run.  `PROVE_TESTS` (a list of TAP files) and `KINGRES_JOBS` are passed on.  `ext.sh` installs the extension into the installation it is given, so each major needs an installation of its own that is not shared with another job.
5. **A TAP file that fails** is rerun alone (`PROVE_TESTS=t/027_move_under_load.pl`) before it is called a real failure; on a loaded machine the timing-dependent tests of `kingres_shard` can fail without a defect.  A failure that repeats is a defect on that major.

## Definition of done

A change is done when all of these hold:

1. **Tests.** New behaviour has tests at the right layer from the table above; a bug fix has a regression test. The relevant suites pass locally. For C code, tests were run on a build with assertions enabled.
2. **Standards.** The formatters and linters in [coding-standards.md](coding-standards.md) pass.
3. **Documentation.** Everything a user or developer needs to know about the change is in the right document (see [documentation.md](documentation.md)). If the change contradicts or settles something in `architecture.md`, that is stated in the work log and, for a decision, in an ADR; `architecture.md` is changed only by its owner.
4. **Core patches.** If the change touches an upstream file, it has a row in [core-patches.md](core-patches.md).
5. **Work-log entry.** A dated entry in `docs/worklog.md` records what was done, how it was verified, and what is still open.
6. **Needs-verification items.** If the change depends on a **[Needs verification]** item, the item was checked and the result is in the work-log entry.

A phase is done when every item of its deliverable is done in this sense, the phase's failure tests have been run, and a release tag exists (architecture document, roadmap).

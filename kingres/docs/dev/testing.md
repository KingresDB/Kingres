<!-- SPDX-License-Identifier: PostgreSQL -->
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
| Multi-node cluster | Routing across groups, 2PC and its recovery after a crash, bucket movement and rebalance, failover and "stale map" retry, map group failure, simultaneous map-group and shard-group failure | TAP tests that start several `Cluster` instances; for Patroni and etcd scenarios, a scripted environment (to be designed with phase 5) | `contrib/kingres_shard/t/` for extension-level; `kingres/dev/` or `kingres/control-plane/` for those that need the control plane (location to be confirmed) |
| Ledger verification | Hash chain and Merkle roots against known test vectors; tamper detection after direct file or row modification; independence of the verifier from the server | pg_regress and TAP for the extension; Go tests in `kingres/verifier/`; a test that runs the verifier against a dumped database | as above |
| Go control plane, unit | Pure logic: state machines, task engine steps, planners for rebalance, config parsing | `go test` with table-driven tests, `-race` | `*_test.go` next to the code |
| Go control plane, integration | Agent and manager against real PostgreSQL instances, task resume after restart, leader election | `go test` with a build tag or an environment guard for tests that need a PostgreSQL build (names to be confirmed) | `*_test.go` next to the code |
| API contract | The running server conforms to `kingres/api/openapi.yaml`: every route in the spec exists, every handler is in the spec, responses and error bodies validate against the schemas | A contract-test harness validating requests and responses against the spec (tool to be chosen and recorded; candidates exist for both Go and TypeScript). Also: the spec itself is linted and validated in CI | `kingres/control-plane/` for server-side; `kingres/web/` for client generation check |
| Web unit and component | Components, hooks, forms, accessibility queries, behaviour on API errors | Vitest and Testing Library; network mocked using responses valid against the OpenAPI spec | `*.test.tsx` next to the component |
| Web end to end | Wizard (create or join cluster), create database, rebalance flow | Browser-driven end-to-end tool (to be chosen when the panel has more than read-only pages) | `kingres/web/` |
| Packaging | The package installs, the service starts "uninitialized", first-run token behaviour, uninstall | Container-based tests (to be designed in phase 3) | `kingres/packaging/` |

Notes on specific areas:

- **Hook chaining.** At least one test per extension loads another extension that uses the same hook and checks that both work (coding-standards.md, 1.7).
- **Failure tests.** The architecture document marks several behaviours **[Needs verification]** and says they must be tested with a prototype and failure tests: simultaneous failure of the map group and a shard group, several Patroni-managed instances on one host, router behaviour during a switchover, persistence of in-doubt transactions after failover. Each such item gets a test when its feature is built, and the result goes in the work log.
- **Documented anomaly.** With 2PC and no global snapshot (ADR 0009), a cross-shard read may see a partially committed distributed transaction. A test documents this behaviour so that a later change is noticed. Single-shard transactions are tested for full correctness.
- **Ledger.** Canonical serialization is tested with fixed vectors that are checked in, including cases that vary `DateStyle`, `extra_float_digits`, `bytea_output` and locale, which must not change any hash.
- **Upgrade.** Each extension has a test that upgrades from the oldest supported version to the current one and compares the result with a fresh install.

## Commands

The commands below are the standard ones for each tool. How they are wired into this repository (targets, CI) is not written yet; paths and flags are to be confirmed when each layer is built.

- PostgreSQL tree, meson: `meson test` (TAP tests require `-Dtap_tests=enabled` at setup; the architecture document cites the meson install page). Autoconf: `make check-world` with `--enable-tap-tests` at configure time. Build and test steps for WSL2 are in [getting-started.md](getting-started.md).
- Go: `go test ./...`, `go test -race ./...`, `go vet ./...`, `golangci-lint run`.
- Web: the package scripts will expose `test` (Vitest), `lint` (ESLint), `typecheck` (`tsc --noEmit`) and `format:check` (Prettier); names to be confirmed when `package.json` exists.

## Definition of done

A change is done when all of these hold:

1. **Tests.** New behaviour has tests at the right layer from the table above; a bug fix has a regression test. The relevant suites pass locally. For C code, tests were run on a build with assertions enabled.
2. **Standards.** The formatters and linters in [coding-standards.md](coding-standards.md) pass.
3. **Documentation.** Everything a user or developer needs to know about the change is in the right document (see [documentation.md](documentation.md)). If the change contradicts or settles something in `architecture.md`, that is stated in the work log and, for a decision, in an ADR; `architecture.md` is changed only by its owner.
4. **Core patches.** If the change touches an upstream file, it has a row in [core-patches.md](core-patches.md).
5. **Work-log entry.** A dated entry in `docs/worklog.md` records what was done, how it was verified, and what is still open.
6. **Needs-verification items.** If the change depends on a **[Needs verification]** item, the item was checked and the result is in the work-log entry.

A phase is done when every item of its deliverable is done in this sense, the phase's failure tests have been run, and a release tag exists (architecture document, roadmap).

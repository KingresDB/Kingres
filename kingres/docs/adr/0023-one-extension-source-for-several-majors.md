<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# ADR 0023: One source of every extension builds against each supported PostgreSQL major (18 and 19)

- Status: Accepted
- Date: 2026-11-15
- Source: [../dev/upstream-sync.md](../dev/upstream-sync.md) ("A new major version"), [0001](0001-thin-fork-with-extensions.md), [0002](0002-base-on-postgresql-18.md)

## Context

KingresDB is a thin fork: three small patches of the PostgreSQL tree ([../dev/core-patches.md](../dev/core-patches.md)), and everything else in extensions under `contrib/kingres_*` and in the `kingres/` directory. ADR 0002 based the product on PostgreSQL 18 and planned a later move to 19 as a port: a new branch on which the extensions are changed to the new interfaces.

A port per major means two diverging copies of the extensions as soon as two majors are maintained, and every fix has to be made twice. PostgreSQL ships a major every year and supports each for five years, so the project owner asked for the opposite: one KingresDB per PostgreSQL major, released side by side, without much change between them.

At the time of this decision upstream has `REL_19_RC2`; the general release of PostgreSQL 19 is scheduled for 2026-11-19. Nothing of KingresDB has been built against 19 yet.

Note of 2026-11-22: PostgreSQL 19.0 came out on 2026-11-16, and `kingres-19` is on `REL_19_0` since 2026-11-22 (see ../dev/releases.md). The decision is unchanged.

## Decision

1. **One source.** Each extension (`kingres_shard`, `kingres_ledger`, and those of later phases) has one source tree that compiles, without warnings, against every supported PostgreSQL major. The supported majors are 18 and 19. The files under `contrib/kingres_*` and `kingres/` are identical on every product branch.
2. **Differences live in one place.** An extension has one compatibility header (`src/pg_compat.h`). Code outside it does not test `PG_VERSION_NUM`; it calls a wrapper or a macro that the header defines for each major. The only exception is a feature that exists on a newer major alone: its file is compiled on that major only, and on the older major the SQL function it backs raises a clear "needs PostgreSQL N" error.
3. **The same SQL.** Installation and upgrade scripts are the same on every major. Where a catalog differs, the script or the C code decides at run time.
4. **The same tests on every major.** The regression and TAP suites of an extension run against each supported major, and a change is committed only when they pass on all of them. Expected output that legitimately differs between majors uses PostgreSQL's alternative expected files.
5. **Build by `pg_config`.** An extension is built for a major out of tree against that major's `pg_config`; the build inside the tree of a product branch stays, and gives the same result.
6. **Product branches.** `kingres-18` and `kingres-19` are each an upstream release tag plus the same series of core patches plus the same `contrib/kingres_*` and `kingres/` directories. Development happens once; the procedure that carries it to the other branch is in [../dev/upstream-sync.md](../dev/upstream-sync.md).
7. **One major per cluster.** All servers of a cluster run the same PostgreSQL major. Adding a server of another major is refused. Moving a cluster from one major to the next is a separate procedure that is not designed yet.
8. **The control plane, the panel and the verifier do not depend on the major.** Where `kingresd` writes a setting or reads a catalog that differs, it asks the server for its version.

Supporting a further major, or dropping one, is decided by a new ADR.

## Consequences

- A fix or a feature is written once and reaches every major. A new PostgreSQL major costs a new section in the compatibility headers and a test run, not a second code base.
- Everything is tested twice, which doubles the time of the test suites.
- The extensions may use only what both majors offer, or must wrap the difference. An interface that changed shape between majors (planner and executor hooks, the custom scan interface, partitioning internals, logical replication, the table access method interface for the columnar extension of phase 12) needs a wrapper that hides the change, and some wrappers will not be trivial.
- The most exposed parts are the ones that reach deepest into the server: the planner integration of `kingres_shard` and, later, `kingres_columnar`.
- Until the port is done and the suites pass on 19, this decision is a rule for new code and a task for existing code, not a property the product has.

## Needs verification

- The list of interface differences between 18 and 19 that the extensions meet. It is produced by compiling them against 19 and is recorded in the work log.
- That the three core patches apply to the 19 tree unchanged.
- That `kingresd` and the packaging need no change other than the major as a build parameter.

## Alternatives considered

- **A port per major, on its own branch** (the plan of ADR 0002). Simple at first; rejected because every later change has to be made on each branch.
- **Support only the newest major.** No compatibility layer, but users on a major that upstream still supports for years would have to upgrade PostgreSQL to get a KingresDB fix.
- **Version tests wherever a difference appears.** Less ceremony than a header; rejected because the differences then spread through the code and nobody can see what a new major costs.

Note of 2026-11-28: the rule "the same series of core patches" (decision item 6) and "pg_compat.h" (item 2) cover the extensions and the three patches of the register at that date, which apply to both trees unchanged. A core patch to code that differs between the majors (the commit and snapshot code touched by phase 8, KP-0005 and KP-0006, proposed in [ADR 0032](0032-phase-8-core-patches-are-two-hooks-one-per-branch.md)) is carried once per product branch, as separate commits of one register row; `pg_compat.h` is a header of the extensions and cannot hold a server patch. The decision is otherwise unchanged.

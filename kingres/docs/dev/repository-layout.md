<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Repository layout

KingresDB uses one repository (accepted decision #2 in the architecture document's "Open decisions"; see [ADR 0003](../adr/0003-single-repository-layout.md)). The repository root is the PostgreSQL source tree. KingresDB's own code is added next to it and never mixed into upstream directories except through registered core patches.

The architecture document's "Proposed repository layout" sketch shows a `postgres/` subdirectory. The accepted decision is different: the fork is the repository root, with the extensions under `contrib/` and everything else under `kingres/`. This document is the current layout; the sketch is superseded.

## Directory tree

Directories marked "planned" do not exist yet or are empty.

```
<repository root>/                 Upstream PostgreSQL tree (branch kingres-18, base REL_18_7)
  contrib/
    kingres_shard/                Sharding extension, C
    kingres_ledger/               Ledger extension, C
    kingres_columnar/             Columnar table access method, C
    kingres_sketch/               Approximate aggregates, C
    kingres_timepart/             Time partitioning and retention, SQL and PL/pgSQL
    ...                            Upstream contrib modules, untouched
  src/, doc/, config/, ...         Upstream PostgreSQL, changed only by registered core patches
  kingres/
    README.md                      Project entry point
    api/                           OpenAPI contract (openapi.yaml); the REST API is defined here first
    control-plane/                 Go module: kingresd and kingres-watchdog (kingres-cli is planned)
      cmd/                           One subdirectory per executable (main packages only)
      internal/                      All other Go packages (agent, manager, router, task engine, REST handlers)
    web/                           Vue 3 and TypeScript panel
    verifier/                      Standalone ledger verifier (does not trust the database)
    packaging/                     deb, rpm, Docker image, systemd unit for the kingres package
    dev/                           Developer scripts
    docs/
      architecture.md              Design and decisions (source of truth)
      research/                    Research notes behind the architecture document
      adr/                         Architecture decision records, one decision each
      dev/                         How to work on the project
        getting-started.md           Development machine setup and build
        coding-standards.md          Standards per language
        repository-layout.md         This file
        core-patches.md              Register of changes to upstream files
        upstream-sync.md             Branching and rebase policy
        testing.md                   Test strategy and definition of done
        documentation.md             Documentation rules
      worklog.md                   Dated record of work done
```

Notes:

- The architecture document's sketch also lists `deploy/` (Docker, Helm, Patroni templates) and `ci/`. They are not part of the current tree. When they are needed they go under `kingres/deploy/` and `kingres/ci/` so that the top level of the repository stays upstream's.
- The sketch's `control-plane/agent`, `manager`, `api` and `cli` are roles, not separate directories: the architecture document says `kingres-agent` and `kingres-manager` are roles inside the single `kingresd` binary. They are Go packages under `kingres/control-plane/internal/`.
- The sketch's `patches/` directory is replaced by the register in [core-patches.md](core-patches.md).

## Where new code goes

Decide in this order. Stop at the first rule that fits.

1. **Can it be an extension?** Anything that runs inside a PostgreSQL backend or background worker and can be done with the hooks, CustomScan, xact callbacks, background workers, custom WAL resource managers (if loaded through `shared_preload_libraries`) or SQL that upstream already provides goes in `contrib/kingres_shard/` (anything about sharding, buckets, routing, distributed transactions, rebalance primitives) or `contrib/kingres_ledger/` (anything about ledger tables, blocks, digests, proofs). A function that belongs to neither needs a discussion and, if it is truly shared, an ADR before a third extension or a shared library is created.
2. **Is it a process outside PostgreSQL?** Instance lifecycle, the client router, cluster management, the task engine, REST API, CLI, metrics integration go in `kingres/control-plane/` (Go). The agent must not be a background worker inside postgres: it has to be able to start and restart postgres (architecture document, "Proposed architecture").
3. **Is it user interface?** It goes in `kingres/web/` and talks only to the REST API defined in `kingres/api/openapi.yaml`. A panel feature needs the API change first.
4. **Is it verification that must not trust the server?** It goes in `kingres/verifier/` and must not import the control-plane module or the PostgreSQL server code.
5. **Is it packaging, service files or images?** `kingres/packaging/`.
6. **Only if none of the above can work: a core patch.** See the next section.

### Core patch rule

Core patches are changes to files that exist upstream, outside `contrib/kingres_*`. They are the most expensive code in the project, because each one must be carried through every rebase (architecture document, "The pattern of existing forks"). Therefore:

- A core patch is allowed only when an extension cannot do the job. The reason must be written in the register entry, in the column "why it cannot be an extension".
- Each core patch is one small commit (or a short, ordered series) that changes only what the purpose requires, with a commit message that states the purpose and the register ID. It is not mixed with any other change.
- Each core patch gets an ID (`KP-0001`, `KP-0002`, ...) and a row in [core-patches.md](core-patches.md) in the same commit or pull request.
- Prefer a patch that adds a hook, a callback or an extension point and is small enough to propose upstream, over a patch that embeds KingresDB logic in the core. Upstream status is tracked in the register.
- The rebrand edits listed in the architecture document's "Rebrand checklist" are also upstream-file changes and are registered the same way.
- Additions to `src/tools/pgindent/typedefs.list` for KingresDB types are the only upstream-file changes that do not need their own ID (see [coding-standards.md](coding-standards.md)); keep them in a separate commit.

### Other placement rules

- Documentation goes where [documentation.md](documentation.md) says.
- Test code lives next to the code it tests (see [testing.md](testing.md)): `sql/`, `expected/` and `t/` in each extension; `*_test.go` beside Go code; `*.test.ts` beside Vue components.
- Do not create new top-level directories in the repository root. If something does not fit under `kingres/`, discuss it first: the root belongs to upstream and every new entry there is a potential merge conflict.
- Do not edit upstream documentation under `doc/` to describe KingresDB features. KingresDB documentation lives in `kingres/docs/`. Documentation changes caused by a core patch (for example a changed message) are part of that patch.

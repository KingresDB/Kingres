<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# ADR 0003: Single repository layout

- Status: Accepted
- Date: 2026-11-14
- Source in architecture.md: "Proposed repository layout (recommendation)"; Open decisions #2

## Context

KingresDB has C extensions that must build inside the PostgreSQL tree (and with PGXS), a Go control plane, a web panel (Vue; see ADR 0017), a verifier and packaging. Open decision #2 asks for the hosting account, public or private, and one repository or two. The recommendation in the document is one repository: the fork, with the extensions under `contrib/` and the control plane and panel in top-level directories.

The project owner accepted "one repository". It is hosted as the private repository `KingresDB/Kingres`.

Differences from the document's layout sketch: the sketch shows a `postgres/` subdirectory. The accepted layout makes the repository root the PostgreSQL tree itself, with KingresDB's own non-extension code under `kingres/`.

## Decision

KingresDB lives in one repository. The root is the PostgreSQL source tree on the branch `kingres-18`. The extensions are `contrib/kingres_shard/` and `contrib/kingres_ledger/`. Everything else of KingresDB's own is under the top-level `kingres/` directory: `api/`, `control-plane/` (Go), `web/` (Vue and TypeScript), `verifier/`, `packaging/`, `dev/` and `docs/`. The full tree and the placement rules are in `kingres/docs/dev/repository-layout.md`.

## Consequences

- One place to change an extension, the control plane and the API contract together, and one set of tags and releases.
- The tree stays buildable with the upstream build systems, and the extensions can be built both in-tree and with PGXS.
- All KingresDB history sits on top of a PostgreSQL clone, so the repository is large and the Go and web code share history with upstream. Upstream files stay at the root, so new top-level directories must be avoided to limit merge conflicts.
- Tools for Go and Node expect their own roots (`go.mod`, `package.json`); they live in subdirectories and CI must run from them.
- Hosting, public or private status, access control and CI platform remain open.

## Alternatives considered

- **Two repositories** (the fork, and a separate repository for the control plane and panel), the other option in decision #2. The document recommends one repository; no further argument is given. The practical argument for one is that the API, the extensions and the panel change together; the document does not state this, so treat it as inference.
- **The sketched `postgres/` subdirectory inside a wrapper repository.** Superseded by the accepted layout, where the fork is the root, which keeps the `git` relationship with upstream direct.

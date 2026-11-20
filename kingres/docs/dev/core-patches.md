<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Core patch register

This file lists every change KingresDB makes to files that exist in upstream PostgreSQL (outside `contrib/kingres_shard/` and `contrib/kingres_ledger/`). The reason for the register is in the architecture document: deep forks fall behind upstream, and a small, documented patch set is what keeps the rebase cost low ("The pattern of existing forks: a small patch set plus extensions").

As of the base `REL_18_7`, the register has four entries (KP-0001 to KP-0004): the version of the product and a short section in the top-level README (the minimal rebrand), the line that adds the extensions to the build, and the `LICENSE` file with the text of the AGPL ([ADR 0024](../adr/0024-agpl-for-kingresdb-code.md)). It had three entries until KP-0004 was added.

**Build system scope.** KingresDB is built with meson only. The autoconf build files (`configure.ac`, `configure`, `src/include/pg_config.h.in`) are left exactly as upstream ships them, so an autoconf build of this tree produces a server without the KingresDB version symbols and is not a supported way to build KingresDB. The reason is that `configure` is a generated file that upstream regenerates with one specific autoconf version; patching it by hand would add an untested code path and a recurring rebase conflict for no benefit.

## Rules

1. Write an extension first. A core patch is allowed only when the extension cannot do the job, and the reason is recorded in the table.
2. One patch is one small commit (or a short ordered series) that does one thing. Do not mix formatting, refactoring or unrelated fixes into it.
3. Each patch gets the next free ID, `KP-NNNN`, never reused, even if the patch is later dropped. The commit subject starts with the ID, for example `KP-0001: add hook X to Y`.
4. The register row is added in the same commit or pull request as the patch. A reviewer rejects a change to an upstream file that has no row.
5. Prefer adding a hook or extension point that could be offered upstream over embedding KingresDB logic in core code.
6. Every patch has a regression or TAP test that fails without it. A patch without a test is not accepted.
7. Keep the table current: when a patch is proposed upstream, accepted, rejected or made obsolete by a later upstream release, update "Upstream status" and note the date and the base tag.
8. Before each rebase (see [upstream-sync.md](upstream-sync.md)), review every row: drop patches that upstream now provides, and re-test the rest.
9. The rebrand edits from the architecture document's "Rebrand checklist" are core patches and are registered here. That checklist is itself a recommendation that needs verification against the real tree; do not assume it is complete.

## Register

Columns:

- **ID**: `KP-NNNN`.
- **Files**: upstream files changed, one path per line.
- **Purpose**: what the patch does and what feature needs it.
- **Why it cannot be an extension**: the specific missing hook or capability; name what was tried.
- **Upstream status**: one of `local only`, `proposed (link, date)`, `accepted in <version>`, `rejected (link)`, `obsolete since <version>`.

| ID | Files | Purpose | Why it cannot be an extension | Upstream status |
|---|---|---|---|---|
| KP-0001 | `meson.build` | Defines the KingresDB product version (`kingres_version`), exports it as `KINGRES_VERSION` and `KINGRES_VERSION_NUM` in `pg_config.h`, and appends `(KingresDB <version>)` to `PG_VERSION_STR`, which is what `version()`, the server start-up log line and psql's `VERSION` variable show. Every PostgreSQL version symbol (`PG_VERSION`, `PG_VERSION_NUM`, `PG_MAJORVERSION`, `server_version`, `pg_config --version`) is unchanged, and the string keeps its `PostgreSQL <version> on ...` prefix, because drivers, extensions and PGXS parse those. Tested by the `version` regression test of `contrib/kingres_shard`. | `PG_VERSION_STR` and `pg_config.h` are produced by the build system; an extension cannot change what `version()` returns or add a compile-time symbol for other extensions. | local only (product branding; not suitable for upstream) |
| KP-0003 | `contrib/meson.build` | Adds one `subdir` line per KingresDB extension (`kingres_columnar`, `kingres_ledger`, `kingres_shard`, `kingres_sketch`, `kingres_timepart`), so the extensions are built, installed and tested with the rest of the tree. `contrib/Makefile` is not changed, in line with the meson-only scope above. Tested by the meson suites named after the extensions. | The list of contrib modules is a core build file; a directory is not built unless it is listed there. (Building out of tree with PGXS stays possible and is what `kingres/dev/ext.sh` does.) | local only |
| KP-0002 | `README.md` | Adds a short KingresDB section above the unchanged PostgreSQL README, pointing to `kingres/README.md`. Documentation only, so rule 6 (a test) does not apply. | The top-level README is what a repository host displays; it cannot live elsewhere. | local only |
| KP-0004 | `LICENSE` (new file) | The text of the GNU Affero General Public License, version 3, the licence of KingresDB's own code ([ADR 0024](../adr/0024-agpl-for-kingresdb-code.md)). PostgreSQL's `COPYRIGHT` file is not changed and keeps stating the licence of PostgreSQL's code. Documentation only, so rule 6 (a test) does not apply. | A repository host and a reader look for the licence at the top of the tree; the same text is in `kingres/LICENSE`. | local only |

## Patch commit message format

```
KP-NNNN: short imperative summary

Why: the feature that needs this change and why an extension cannot
provide it.
What: the change, in terms a reviewer can check against the diff.
Tests: the regression or TAP test that covers it.
Upstream: local only | link to the proposal.
```

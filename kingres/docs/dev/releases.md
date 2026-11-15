<!-- SPDX-License-Identifier: PostgreSQL -->
# Releases: one KingresDB per PostgreSQL major

KingresDB is released for every supported PostgreSQL major at the same time, from one state of its own code. Whoever installs it chooses the major: the newest KingresDB on PostgreSQL 18, or the newest KingresDB on PostgreSQL 19. The decision behind this is [ADR 0023](../adr/0023-one-extension-source-for-several-majors.md); the mechanics of following upstream are in [upstream-sync.md](upstream-sync.md).

## Product branches

| Branch | PostgreSQL | What it is |
|---|---|---|
| `kingres-18` | 18 | Upstream tag `REL_18_x`, the core patches, and the KingresDB directories. Development happens here; it is the default branch |
| `kingres-19` | 19 | Upstream tag `REL_19_x`, the same core patches, and the same KingresDB directories |

The KingresDB directories are `contrib/kingres_*` and `kingres/`. They are byte for byte the same on both branches at every release: `git diff kingres-18 kingres-19 -- 'contrib/kingres_*' kingres` prints nothing. The core patches ([core-patches.md](core-patches.md)) are the same changes applied to each major's files.

## How a change reaches `kingres-19`

1. The change is made and committed on `kingres-18`. It may be committed only when the suites of every extension it touches pass on both majors ([testing.md](testing.md), "Running the suites on both PostgreSQL majors").
2. A sync commit on `kingres-19` takes the KingresDB directories from `kingres-18` as they are at a named commit: `git checkout <commit of kingres-18> -- 'contrib/kingres_*' kingres`, with the message `sync from kingres-18 at <short hash>`. Nothing is edited by hand on `kingres-19` inside those directories. A file that exists on `kingres-19` and no longer on `kingres-18` is removed in the same commit.
3. A core patch that changes is applied to `kingres-19` as its own commit, with the register entry updated on `kingres-18` first.

`kingres-19` is never rebased and never force-pushed by a sync. A new upstream minor release is taken as described in [upstream-sync.md](upstream-sync.md).

## What a release is

A release is one commit of `kingres-18` and the sync commit of `kingres-19` made from it, both tagged, with the artifacts built from each.

| Thing | PostgreSQL 18 | PostgreSQL 19 |
|---|---|---|
| Tag | `kingres-<version>-pg18` | `kingres-<version>-pg19` |
| Debian package | `kingres_<version>+pg18-<revision>_amd64.deb` | `kingres_<version>+pg19-<revision>_amd64.deb` |
| Image tag | `kingres:<version>-pg18` | `kingres:<version>-pg19` |
| What the server reports | `18.x (KingresDB <version>)` | `19.x (KingresDB <version>)` |

`<version>` is `kingres_version` in the top-level `meson.build`, the same on both branches. `kingres:<version>` without a suffix is not published: the major is always chosen explicitly.

Before a release is tagged, on each branch:

- the whole tree builds without a compiler warning and the upstream regression suite passes;
- the suites of every KingresDB extension pass in tree;
- the integration suite of the control plane passes against instances of that major;
- the package tests pass (the Debian package, the image, and the three-node compose cluster);
- the console's checks pass (they do not depend on the major, and are run once).

A cluster runs one major: all its servers are installed from the same branch. Moving a cluster from one major to the next is a separate procedure that is not designed yet.

## State on 2026-11-15

- `kingres-18` exists and is the only branch that is published.
- `kingres-19` does not exist yet. Upstream has `REL_19_RC2`; the general release of PostgreSQL 19 is scheduled for 2026-11-19. The branch is created on the release candidate as soon as `kingres_shard` builds and passes on 19 (its port is in progress; `kingres_ledger`, `kingres_sketch` and `kingres_timepart` already do), and moves to `REL_19_0` when that tag exists.
- Not verified yet for 19: the control plane's integration suite against PostgreSQL 19 instances, and the packaging. The packaging builds one major today and names its artifacts without the major.
- No release has been tagged.

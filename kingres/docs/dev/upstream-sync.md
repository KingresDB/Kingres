<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Upstream sync: branches and rebase policy

This document turns the architecture document's "Branch choice and mirror mechanics" into working rules. The distinction used there applies here: items marked **[Recommendation]** are design decisions (accepted by the project owner where the Open decisions table says so, but not backed by an upstream source), and items marked **[Needs verification]** must be confirmed in practice.

## Facts as of 2026-11-14

- The base is upstream tag `REL_18_7` (commit `f5c41f6`, "Stamp 18.7."), checked out as the local branch `kingres-18`.
- The remote `upstream` is `https://github.com/postgres/postgres.git`. The official clone URL is `https://git.postgresql.org/git/postgresql.git`; GitHub is a mirror (architecture document, citing the PostgreSQL docs).
- `git ls-remote` shows that upstream has both `REL_18_STABLE` and `REL_19_STABLE` branches. This settles the open question in the architecture document about the exact branch names.
- The local clone holds the full history behind `REL_18_7`. The hosting remote is `origin` (the private repository `KingresDB/Kingres`), and its default branch is `kingres-18`.
- Git is configured with `core.autocrlf=false` and `core.eol=lf`. The tree must stay LF on Windows.
- PostgreSQL 18 is supported until 2030-11-14. PostgreSQL 19 GA is scheduled for 2026-11-19 (architecture document, citing the versioning policy and the PG19 RC1 announcement).
- The newest upstream tag of PostgreSQL 19 on 2026-11-15 is `REL_19_RC2`; there is no `REL_19_0` yet. The three core patches (`meson.build`, `contrib/meson.build`, `README.md`) applied to `REL_19_RC2` without a change (checked 2026-11-15 with `git apply`).

## Branches

**[Recommendation]** (accepted as part of decision #1 and the architecture document's mechanics):

| Branch | Kind | Rule |
|---|---|---|
| `upstream/REL_18_STABLE`, `upstream/REL_19_STABLE` | Remote-tracking refs of upstream | Read only. |
| Local mirror branches, one per upstream stable branch that is used (proposed names: `mirror/REL_18_STABLE`, `mirror/REL_19_STABLE`) | Pure mirror | Fast-forward only to the matching upstream branch. No KingresDB commits, ever. The names are a proposal. |
| `kingres-18` | Product branch | Upstream tag `REL_18_x` plus the KingresDB commits on top. Rebased onto each new minor tag. |
| Feature branches | Short-lived | Branch from `kingres-18`, merged back through review. |
| `kingres-19` | Product branch for PostgreSQL 19 | An upstream `REL_19_x` tag plus the same core patches as `kingres-18` plus the same `contrib/kingres_*` and `kingres/` directories (ADR 0023). Not created yet; the first upstream tag to base it on is the one after the general release (scheduled for 2026-11-19, to be confirmed). Until then `REL_19_RC2` is used for build and test only. See "A new major version". |

Why rebase instead of merge: the Postgres-XL team reported that merging upstream master produced far more conflicts than expected and that they had to rebase onto a later commit first. This is an empirical reason, not official PostgreSQL policy; the primary sources reviewed in the research give no guidance on private branches (architecture document, "Branch choice and mirror mechanics"). A rebase keeps the KingresDB changes as a small, reviewable series on top of a known upstream tag. The cost is that published history changes; see "Shared history" below.

`kingres-18` is based on a tag, not on the moving `REL_18_STABLE` branch tip. Between minor releases upstream stable branches may contain unreleased fixes; basing on tags means every KingresDB build is on a released upstream state.

## Shared history

Rebasing rewrites commits. Rules, **[Recommendation]**:

- While nobody else has cloned the repository, `kingres-18` may be rebased freely and force-pushed with `git push --force-with-lease`.
- Once the repository is shared, announce each rebase in the work log, tag the pre-rebase state (for example `kingres-18-pre-18.8`), and make force-pushes to `kingres-18` happen only for a rebase onto a new upstream tag, done by a maintainer.
- Never rebase feature branches that others have based work on; ask them to rebase.
- Release tags for KingresDB point to commits on `kingres-18` and are never moved.

The hosting platform, and therefore whether branch protection can enforce these rules, is not decided yet (decision #2 is only half settled).

## Routine: rebasing onto a new minor release

Run when upstream publishes a new `REL_18_x` tag (minor releases come at least every three months).

1. Make sure the working tree is clean and `kingres-18` is pushed or backed up with a tag.
2. Fetch upstream, including tags: `git fetch upstream --tags`.
3. Fast-forward the mirror branch: `git switch mirror/REL_18_STABLE && git merge --ff-only upstream/REL_18_STABLE`. If this is not a fast-forward, stop: something committed to the mirror.
4. Read the upstream release notes for the new minor release, looking for changes to the areas KingresDB touches (planner, executor, transaction callbacks, WAL, logical replication, GUC handling, build files).
5. Rebase the product branch onto the new tag: `git rebase --onto <new-tag> <old-tag> kingres-18`. Resolve conflicts patch by patch. Each conflict is recorded in the work log with the core patch ID, because conflicts are the metric that tells whether the thin-fork policy is working.
6. Update [core-patches.md](core-patches.md): drop patches that upstream now provides; update upstream status.
7. Run the verification list below.
8. Record the rebase in the work log (old tag, new tag, conflicts, test results, anything left open).

Do not use `git merge` of upstream into the product branch.

### What to run after a rebase

At minimum, in WSL2 (build instructions are in [getting-started.md](getting-started.md)):

1. A clean build with assertions enabled (the architecture document recommends `-Dcassert=true` for CI; the developer FAQ recommends `--enable-cassert --enable-debug` for development).
2. The upstream test suite, including TAP tests (`meson test`, or `make check-world` with `--enable-tap-tests` for autoconf builds). Failures that also occur on the unmodified upstream tag are reported upstream-first and recorded, not fixed locally.
3. The KingresDB extension regression and TAP tests, with both build systems (`Makefile` and `meson.build`) for each extension.
4. The multi-node cluster tests (see [testing.md](testing.md)), once they exist.
5. `pgindent --check` on KingresDB's C files, with the typedefs list updated.
6. The Go and web test suites, if the API or the extension SQL interface changed.
7. A check that the `PG_MODULE_MAGIC` ABI check still accepts the extensions (a minor release should not change the ABI; confirm by loading them).
8. Confirm that `core.eol` and `core.autocrlf` still keep the tree LF (`git diff --stat` after the rebase must show only intended changes).

**[Needs verification]** The architecture document recommends a nightly CI job that merges the latest upstream stable tag and runs the full suite. How that job is built (merge in a throwaway branch versus a dry-run rebase), and when CI exists, are open.

## A new major version

PostgreSQL ships one major release per year, and KingresDB follows ADR 0023: one KingresDB per PostgreSQL major, released side by side, from **one source**. The files under `contrib/kingres_*` and `kingres/` are identical on `kingres-18` and `kingres-19`; only the base tag and the build parameter "major" differ. The mechanics below are **[Recommendation]** until the first `kingres-19` release has been made; steps marked "tried" were run on `REL_19_RC2`, the others are to be confirmed.

1. **Before the general release** (scheduled for 2026-11-19, to be confirmed), work on the newest `REL_19_*` tag, in a scratch tree outside this repository: build it with the options of `kingres/dev/build.sh`, apply the core patches (tried: they apply unchanged to `REL_19_RC2`), build and test each extension out of tree with `kingres/dev/ext-majors.sh` (see [testing.md](testing.md)). Compile errors and warnings are the list of interface differences; each goes into the extension's `src/pg_compat.h` (see [coding-standards.md](coding-standards.md), 1.13), not into the code that uses it.
2. Create `mirror/REL_19_STABLE` as a fast-forward mirror of `upstream/REL_19_STABLE`.
3. Create `kingres-19` from the chosen upstream tag and apply the core-patch series of `kingres-18` in the order of the register, so that each patch can be re-tested and re-justified; a patch that upstream now makes unnecessary is dropped.
4. Carry `contrib/kingres_*` and `kingres/` from `kingres-18` to `kingres-19`: development happens once, on one branch (the one the change is first tested on), and the same commit is cherry-picked onto the other with `git cherry-pick -x`. Because both branches hold identical files in these directories, the cherry-pick applies without conflicts unless a core patch or a file outside these directories is touched; a conflict there is a sign that something was committed to the wrong place and is reported, not resolved by editing both copies. A commit is complete only when `ext-majors.sh` passes for every extension it touches on both majors (ADR 0023, point 4). A check that the two branches stay identical in these directories (`git diff kingres-18 kingres-19 -- contrib/kingres_ledger contrib/kingres_shard kingres`, expected empty) is to be added to the routine; it is to be confirmed.
5. Run the full verification list above on `kingres-19`, then run cluster upgrade tests (upgrade from a PostgreSQL 18 based KingresDB cluster to 19 is a separate procedure and is not designed yet; ADR 0023, point 7, says one major per cluster until it is).
6. Keep `kingres-18` as a maintained branch for as long as the supported releases require; the supported period for PG18 is until 2030-11-14. The policy for how long KingresDB itself supports each major is not decided.
7. A new major costs a new section in each `pg_compat.h`, a test run on all supported majors, and a check of the core-patch register; it does not cost a second code base. Supporting a further major, or dropping one, is decided by a new ADR.

The cost of this step is the main risk named in the architecture document ("Annual rebase cost on each new major"). The mitigation is the core patch rules (few patches, small, registered) and the single compatibility header per extension.

## Things this document does not decide

- The hosting platform and its branch protection.
- How long each major is maintained.
- The exact CI jobs.
- The KingresDB version numbering scheme relative to PostgreSQL's.

<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
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
- the console's checks pass (they do not depend on the major, and are run once);
- the heading of the release in `kingres/CHANGELOG.md` carries its date, set by hand in the commit that is tagged (for example `## [0.1.0] - 2026-11-28` in place of `## [0.1.0] - not released`). The release script reads the changelog from the commit `--commit18` and never edits it, so a heading left as it is would be tagged as it stands. The heading must still start with `## [<version>]`, which is what the script matches, and the section must have no `### Pending` part.

A cluster runs one major: all its servers are installed from the same branch. Moving a cluster from one major to the next is a separate procedure that is not designed yet.

## State on 2026-11-22

- Upstream tagged `REL_19_0` (PostgreSQL 19.0) on 2026-11-16, 47 commits after `REL_19_RC2`. `kingres-19` merged it on 2026-11-22. The only file that both upstream and the core patches change is `meson.build` (upstream the version line, KP-0001 the KingresDB version), and the merge had no conflict. Outside `contrib/kingres_*` and `kingres/`, `kingres-19` now differs from `REL_19_0` only by the core patches (`LICENSE`, `README.md`, `contrib/meson.build`, `meson.build`).
- Checked on `kingres-19` after the merge: the tree builds with assertions enabled and without a compiler warning, the core regression suite passes, and the in-tree suites of the extensions pass (319 passed, 0 failed, 2 skipped: the test that needs an installation of the other major, and the columnar upgrade test that needs an older installation). The integration suite of the control plane runs against 19.0 installations from then on.
- `kingres-19` is no longer a pre-release branch. The first release, 0.1.0, was tagged from the two branches on 2026-11-28 (`kingres-0.1.0-pg18`, `kingres-0.1.0-pg19`).

## State on 2026-11-15 (history)

- `kingres-18` was on `REL_18_7` and is the default branch.
- `kingres-19` existed since 2026-11-15 on `REL_19_RC2` (a pre-release then; it moved to `REL_19_0` on 2026-11-22, see above, and is on `REL_19_0` now). It consisted of the upstream tag, one commit with the core patches that existed then, KP-0001 to KP-0003 (they applied without a change to their content), one commit for KP-0004 (the `LICENSE` file), and sync commits. The register has four entries.
- Checked on `kingres-19` at the sync from `d504f44464`, on the tree exactly as committed: the whole tree builds with assertions enabled and without a compiler warning; the core regression suite passes (243 tests); the suites of the four extensions pass in tree (73 suites, and the test that needs an installation of the other major skips). Out of tree, every extension passes on 18.7 and on 19rc2 with `-Werror`; `kingres_shard` runs 47 TAP files with 5847 subtests on each.
- Checked on 19 later (the work log of 2026-11-16 and the following days has the runs, and [../../packaging/README.md](../../packaging/README.md) has the numbers): the integration suite of the control plane passes against PostgreSQL 19 instances, and the package, image and compose tests give the same numbers on both majors; the packaging builds one major at a time and names its artifacts with the major (`+pg18`, `+pg19`). When this section was first written (2026-11-15) neither had been checked on 19.
- That was the state before the first release; 0.1.0 was tagged on 2026-11-28.

## History rewritten on 2026-11-16

The history of both product branches was rewritten once, on 2026-11-16, by decision of the project owner, to remove a path of a developer's machine that two scripts carried as a default value in eleven early commits. Every commit of `kingres-18` after `f78f85220f` has a new id since then, and so has every sync commit of `kingres-19`; the trees of the tips did not change. A commit id of either branch that was noted before that date (in an issue, a message or a clone) does not exist in the published history any more; the ids cited in the documents and commit messages of the repository were replaced by the new ones. A clone made before that date must be made again or reset to the published branches.

This is not a precedent: the branches are not rewritten or force-pushed in ordinary work, and nothing is rewritten after the first release is tagged.

## Where the checks run

GitHub Actions is switched off for the repository, by decision of the project owner: no check of this project runs on GitHub, and no workflow of the project is published. The tree of PostgreSQL 19 contains upstream's own workflow file (`.github/workflows/pg-ci.yml`); it stays in the tree unchanged, as every upstream file does, and does not run. Every check named in this document is run on a developer's machine with the scripts in `kingres/dev`, `kingres/control-plane/scripts`, `kingres/web` and `kingres/packaging/test`, and its result is recorded in the work log.

Pushing `kingres-19` needs credentials that may write workflow files whenever that upstream file changes, which happens when the branch moves to a new upstream tag.

## The release script

[`kingres/packaging/release/release.sh`](../../packaging/release/release.sh) makes a release from the two product branches. The header of the script is its full reference; this is what it does and does not do.

```
kingres/packaging/release/release.sh --version <version> --commit18 <sha> --commit19 <sha> \
    --checks-file <record> --key-file <release key> --out <dir> [--date YYYY-MM-DD] [--work <dir>] [--revision N] [--skip-images] [--sign-tags] [--dry-run]
```

1. **Gates** (all are checked, every failure is listed, nothing is built if one fails): a clean working tree; `kingres-18` and `kingres-19` at the commits you name; `kingres_version` of `meson.build` equal to `--version` on both; PostgreSQL major 18 and 19 and both final releases (a pre-release such as `19rc2` is refused); `contrib/kingres_*` and `kingres` identical on both commits; neither tag exists; `kingres/CHANGELOG.md` has a section `## [<version>]` without a `### Pending` part; the release-notes template exists; the record of the final checks says every required check passed on the right commit; a git committer identity is set (user.name and user.email, or GIT_COMMITTER_NAME and GIT_COMMITTER_EMAIL; the tags are the last step and would fail after the builds without one); a release key file is given (not for a dry run); the output directory is empty; Docker is available unless `--skip-images` is given.
2. **The tool and the key**, before the first build: `kingres-release-tool` and `kingres-verify` are built with go from `kingres/verifier` of the commit of `kingres-18`, and the key is read once, so that a key that cannot be used is found now (go must be on the PATH of the machine that makes the release).
3. **Builds**, per major, from the commit of the branch: `build/build.sh`, `deb/build-deb.sh` and, unless skipped, `docker/build-image.sh` and `docker save`. Artifacts go to `<dir>/pg18` and `<dir>/pg19`.
4. **Checksums, signed**: `<dir>/kingres-SHA256SUMS`, one line per package and image of both majors by the plain name of the file, checked with `sha256sum -c` (in a folder that holds the files by those names) and signed with the release key into `<dir>/kingres-SHA256SUMS.note`, a signed note of the same format as the verifier's; the script then checks the note and every file with `kingres-verify release verify` and the public key of the key file. The names differ on purpose from the verifier's sums (`kingres-verify-SHA256SUMS` and its `.note`, made by `kingres/verifier/scripts/release.sh`), because the release page of a tag holds both. The packages and images themselves are not signed.
5. **Release notes**: `<dir>/RELEASE-NOTES.md`, dated by `--date` or, by default, the local date of the machine (not UTC; the date is printed at the start and with the plan), made from [`kingres/docs/release-notes-template.md`](../release-notes-template.md) with the changelog section, the artifacts and the recorded checks.
6. **Tags**: `kingres-<version>-pg18` and `kingres-<version>-pg19` are created **locally** as annotated tags (signed with `--sign-tags`) on the two commits, as the very last step.
7. It prints the manual steps and stops: pushing the branches and the tags, and creating the GitHub release of each tag with its files. **The script never pushes and never contacts GitHub.** No GitHub Actions are used.

`--dry-run` runs every gate and prints the plan and the manual steps; it builds nothing, signs nothing, writes nothing and creates no tag, and exits non-zero when a gate fails. A real run without `--key-file` is refused before anything is built; a dry run without one says that it skipped the key. (The verifier's own script marks a dry run by recording the commit as `unknown` in its signed `RELEASE` file; the product has nothing signed in a dry run, so there is nothing to mark.)

**Checking a file of a release.** `kingres-verify release verify --sums kingres-SHA256SUMS --note kingres-SHA256SUMS.note --key <release key> --binary <file>` checks a package or an image the same way as a verifier binary (the line is found by the name of the file; `--name` names a renamed one), with the key from the documentation ([../../verifier/README.md](../../verifier/README.md), "The release key"), not from the download. The packaging README ([../../packaging/README.md](../../packaging/README.md), "Checking the files of a release") has the details. 0.1.0 was published before this: its `SHA256SUMS` is not signed and its lines carry the directory (`pg18/...`).

**The record of the final checks** is a text file that whoever ran the checks keeps, one line per run: `<commit> <major> <check> <pass|fail> [text]`, where `<major>` is `18`, `19` or `all`. The required checks are the list under "What a release is" above: for each major `tree-build` (builds without a warning), `core-regress` (the upstream suite), `extension-suites` (every KingresDB extension in tree), `integration` (the control plane's suite), `package-deb`, `package-image` and `package-compose` (the last two are not required with `--skip-images`), and for `all` the `console` checks. A later line replaces an earlier one, and a check recorded on another commit does not count. The script does not run these multi-hour suites; it refuses to release without them.

**Without Docker.** A build machine without Docker can build the packages; use `--skip-images`. The script then says so, the notes say the images were not built, and the image checks are not required. Build and test the images on a machine with Docker before a release that is meant to carry them.

**Test.** `kingres/packaging/test/test-release.sh` runs the real script against a small test repository with stand-ins for the three build scripts and for docker, and checks every gate, the dry run, the checksums, the notes, the tags and that nothing is pushed.

After the script: push, then publish. Signing the packages and images themselves and publishing them in a registry or an apt repository are the project owner's decisions and are not part of the script. What the script changes for the next release compared with 0.1.0 (where the tags, the date of the notes and the names of the sums were made or fixed by hand): it refuses at the start when no git identity is set (0.1.0: the tag step failed after the builds in a clone without one, and the tags were made by hand with the script's message), it writes the local date or `--date` (0.1.0 said 2026-11-27 for a release made on 2026-11-28 by the local clock), and the sums are signed and named so that they do not meet the verifier's on the release page (0.1.0: the verifier's sums were renamed by hand to `kingres-verify-SHA256SUMS` and `kingres-verify-SHA256SUMS.note`, and the product's `SHA256SUMS` was not signed).

**The tag of the verifier.** A KingresDB release makes only the product tags `kingres-<version>-pg18` and `kingres-<version>-pg19`; no separate `v<version>` tag is made. The signed release of the verifier (`kingres/verifier`, see its README) is therefore cut with `--tag kingres-<version>-pg18`.

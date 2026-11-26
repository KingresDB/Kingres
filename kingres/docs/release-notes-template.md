<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# KingresDB @VERSION@

Released @DATE@, for PostgreSQL 18 and PostgreSQL 19 from one source.

| | PostgreSQL 18 | PostgreSQL 19 |
|---|---|---|
| Tag | `@TAG18@` | `@TAG19@` |
| PostgreSQL | @PG18_VERSION@ | @PG19_VERSION@ |
| Commit | `@COMMIT18@` | `@COMMIT19@` |

A cluster runs one major. Choose it, and install every server of the cluster from the same one. There is no move between majors yet.

## What is in this release

The links in this list are relative to the directory `kingres/` of the tagged tree.

@CHANGELOG@

## Files

All files are listed in `SHA256SUMS`; check them with `sha256sum -c SHA256SUMS`.

@ARTIFACTS@

## Checks that were run before the tags were made

Each line is a check of the release process (docs/dev/releases.md, "What a release is"), recorded by whoever ran it, with the commit it ran on.

@CHECKS@

## Install and upgrade

The user guide is in `kingres/docs/guide/` of the tagged tree: start with `quickstart.md`, then `install.md` and `upgrade.md`. The limits of this release are in `limitations.md`; read it before you decide what to run on KingresDB.

## Verifying what you downloaded

The checksums are not signed in this release unless the files say otherwise. To check that the files are the ones the tag describes, build from the tagged tree (`kingres/packaging/build/build.sh --rev <tag>`) and compare. The standalone ledger verifier is built from the same tag with `kingres/verifier/scripts/build-one.sh`; an auditor must not use a verifier that came from the cluster they check.

## Licence

The code of KingresDB is released under the GNU Affero General Public License, version 3 only. PostgreSQL code keeps the PostgreSQL License. KingresDB is derived from PostgreSQL; it is not PostgreSQL and is not endorsed by the PostgreSQL project.

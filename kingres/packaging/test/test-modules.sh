#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (c) 2026, RooyeKhat Media Co
#
# kingres/packaging/test/test-modules.sh
#
# Checks that the two modules of PostgreSQL that the product relies on are in a
# package or in an installation tree, for the PostgreSQL major of that package:
#
#   pg_stat_statements   preloaded by every instance (kingresd), and created in
#                        every database that kingresd makes;
#   plpgsql              in template1 of every cluster; kingres_timepart and the
#                        functions of the users are written in it.
#
# For each module the control file, at least one install script and the library
# are looked for under share/postgresql/extension and lib/postgresql (or, for an
# installation made by kingres/dev, share/extension and lib).  It reads file
# lists only and writes nothing.
#
# Usage:
#   test-modules.sh --deb FILE.deb      the package (needs dpkg-deb)
#   test-modules.sh --tree DIR          an installation tree (the staged
#                                       WORK/stage/opt/kingres, or a prefix)
#   test-modules.sh --list FILE         a file of paths, one per line (for tests of this script)

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "${here}/lib.sh"

mode="" arg=""
while [ "$#" -gt 0 ]; do
    case "$1" in
        --deb | --tree | --list) mode="${1#--}"; arg="${2:?${1} needs a value}"; shift 2 ;;
        *) echo "test-modules.sh: unknown argument '$1'" >&2; exit 2 ;;
    esac
done
[ -n "${mode}" ] || { echo "usage: test-modules.sh --deb FILE | --tree DIR | --list FILE" >&2; exit 2; }

case "${mode}" in
    deb)
        command -v dpkg-deb >/dev/null || { echo "test-modules.sh: dpkg-deb not found" >&2; exit 2; }
        [ -f "${arg}" ] || { echo "test-modules.sh: ${arg} is not a file" >&2; exit 2; }
        paths="$(dpkg-deb -c "${arg}" | awk '{ print $NF }')" ;;
    tree)
        [ -d "${arg}" ] || { echo "test-modules.sh: ${arg} is not a directory" >&2; exit 2; }
        paths="$(cd "${arg}" && find . -type f -o -type l)" ;;
    list)
        [ -f "${arg}" ] || { echo "test-modules.sh: ${arg} is not a file" >&2; exit 2; }
        paths="$(cat "${arg}")" ;;
esac

has() { grep -Eq "$1" <<<"${paths}"; }

for m in ${POSTGRES_MODULES}; do
    note "${m}"
    check "${m}: control file" has "(^|/)share/(postgresql/)?extension/${m}[.]control\$"
    check "${m}: install script" has "(^|/)share/(postgresql/)?extension/${m}--[0-9][0-9.]*[.]sql\$"
    check "${m}: library" has "(^|/)lib/([^/]+/)?(postgresql/)?${m}[.]so\$"
done
summary

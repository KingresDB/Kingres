#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (c) 2026, RooyeKhat Media Co
#
# kingres/packaging/test/test-modes.sh
#
# The packaging scripts call each other directly (build.sh runs snapshot.sh and
# source-info.sh, build-image.sh runs source-info.sh, release.sh runs the build
# scripts), and the build works on a "git archive" snapshot.  A script that is
# committed without the executable bit works on a Windows checkout (every file
# looks executable there) and fails on every Linux clone, snapshot and release
# build with "Permission denied".  This test reads the committed modes of a
# revision and checks that
#
#   - every *.sh file and every Debian maintainer script of kingres/packaging
#     (preinst, postinst, prerm, postrm) is committed as 100755;
#   - after "git archive" of that revision the same files are executable.
#
# Usage:
#   test-modes.sh [--rev REV]     (default HEAD)
# Needs: git, tar.  It writes nothing.

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "${here}/../../.." && pwd)"
rev=HEAD
while [ "$#" -gt 0 ]; do
    case "$1" in
        --rev) rev="$2"; shift 2 ;;
        *) echo "test-modes.sh: unknown argument '$1'" >&2; exit 2 ;;
    esac
done

g() { git -c safe.directory='*' -C "${repo}" "$@"; }

fail=0
checked=0
list="$(g ls-tree -r "${rev}" -- kingres/packaging | awk '{ print $1 " " $4 }' |
    grep -E '\.sh$|/deb/(preinst|postinst|prerm|postrm)$' || true)"
[ -n "${list}" ] || { echo "test-modes.sh: no packaging scripts found in ${rev}" >&2; exit 2; }

while read -r mode path; do
    checked=$((checked + 1))
    if [ "${mode}" != 100755 ]; then
        echo "FAIL  ${path} is committed with mode ${mode}, expected 100755 (git update-index --chmod=+x)"
        fail=$((fail + 1))
    fi
done <<<"${list}"

# The modes recorded in the archive itself (a file system test would depend on
# the host: on Windows every file with a shebang looks executable).
listing="$(g archive --format=tar "${rev}" kingres/packaging | tar -tv)"
while read -r _ path; do
    if ! printf '%s\n' "${listing}" | grep -qE "^-rwx[r-][w-]x[r-][w-]x .* ${path}\$"; then
        echo "FAIL  ${path} is not executable in a git archive of ${rev}"
        fail=$((fail + 1))
    fi
done <<<"${list}"

if [ "${fail}" -gt 0 ]; then
    echo "test-modes.sh: ${fail} problem(s) in ${checked} script(s) of ${rev}"
    exit 1
fi
echo "test-modes.sh: ${checked} packaging scripts of ${rev} are executable in the repository and in an archive"

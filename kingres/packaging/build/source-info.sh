#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (c) 2026, RooyeKhat Media Co
#
# kingres/packaging/build/source-info.sh
#
# Say what a source tree is, so that nothing about the release is typed
# twice: the KingresDB version and the PostgreSQL version and major are read
# from the tree's top-level meson.build, and the names of the artifacts are
# derived from them (docs/dev/releases.md: one release per PostgreSQL major).
# Used by build.sh, build-image.sh and the tests.
#
# Usage:
#   source-info.sh SRC            prints KEY=value lines
#   source-info.sh SRC KEY        prints the value of one key
#
# Keys:
#   kingres_version   kingres_version of meson.build              0.1.0
#   pg_version        version of the project() call               18.7 or 19rc2
#   pg_major          its leading integer, one of the supported   18
#   deb_version       version of a package, without the revision  0.1.0+pg18
#   image_tag         tag of the image, without the repository    0.1.0-pg18
#
# A major that is not supported is refused: the release has two (18 and 19).

set -euo pipefail

supported_majors="18 19"

[ "$#" -ge 1 ] || { echo "usage: source-info.sh SRC [KEY]" >&2; exit 2; }
src="$1"
meson="${src}/meson.build"
[ -f "${meson}" ] || { echo "source-info.sh: ${src} has no meson.build" >&2; exit 1; }

kingres_version="$(sed -n "s/^kingres_version = '\\(.*\\)'\$/\\1/p" "${meson}")"
[ -n "${kingres_version}" ] || { echo "source-info.sh: no kingres_version in ${meson}" >&2; exit 1; }

# The first "version:" inside the first lines of the file is the one of
# project('postgresql', ...).
pg_version="$(sed -n "1,40s/^[[:space:]]*version:[[:space:]]*'\\([^']*\\)'.*/\\1/p" "${meson}" | head -1)"
pg_major="$(printf '%s' "${pg_version}" | sed -n 's/^\([0-9][0-9]*\).*/\1/p')"
[ -n "${pg_major}" ] || { echo "source-info.sh: cannot read the PostgreSQL version from ${meson}" >&2; exit 1; }
case " ${supported_majors} " in
    *" ${pg_major} "*) ;;
    *) echo "source-info.sh: PostgreSQL ${pg_version} is not a supported major (${supported_majors})" >&2; exit 1 ;;
esac

declare -A info=(
    [kingres_version]="${kingres_version}"
    [pg_version]="${pg_version}"
    [pg_major]="${pg_major}"
    [deb_version]="${kingres_version}+pg${pg_major}"
    [image_tag]="${kingres_version}-pg${pg_major}"
)

if [ "$#" -ge 2 ]; then
    [ -n "${info[$2]:-}" ] || { echo "source-info.sh: unknown key '$2'" >&2; exit 2; }
    printf '%s\n' "${info[$2]}"
else
    for k in kingres_version pg_version pg_major deb_version image_tag; do
        printf '%s=%s\n' "${k}" "${info[${k}]}"
    done
fi

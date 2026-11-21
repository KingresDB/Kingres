#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# kingres/dev/ext-majors.sh
#
# Run the build and the suites of one KingresDB extension against every
# supported PostgreSQL major, one after the other, and print one line per
# major at the end.  An extension is one source for all majors (ADR 0023), so
# a change is complete only when this script passes.
#
# The builds are always clean builds with warnings turned into errors, because
# "zero warnings on every major" is part of the rule.
#
# Usage:
#   kingres/dev/ext-majors.sh <extension> [command...]
#
# The commands are those of kingres/dev/ext.sh; the default is
# "sync clean build install regress tap".
#
# Environment:
#   KINGRES_PG18   the installation of the PostgreSQL 18 build: a pg_config
#                  program or an installation directory (default:
#                  $KINGRES_WORK/install, the one build.sh makes)
#   KINGRES_PG19   the same for PostgreSQL 19 (no default)
#   KINGRES_MAJORS the majors to run, in order (default: the majors whose
#                  variable above is set, 18 always)
#
# Every other variable of ext.sh (KINGRES_WORK, KINGRES_JOBS, EXT_SRC,
# PROVE_TESTS, ...) is passed on.  The exit status is non-zero when any major
# failed.  The server of every major must be an installation of its own;
# "install" writes the extension into it.

set -uo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

work_dir="${KINGRES_WORK:-${HOME}/kingres-work}"

die() {
    printf 'ext-majors.sh: error: %s\n' "$*" >&2
    exit 2
}

[ "$#" -ge 1 ] || die "missing extension name (see the header of this script)"
ext="$1"
shift
if [ "$#" -eq 0 ]; then
    set -- sync clean build install regress tap
fi

# The installation of one major, or nothing.
pg_for_major() {
    case "$1" in
        18) printf '%s' "${KINGRES_PG18:-${work_dir}/install}" ;;
        19) printf '%s' "${KINGRES_PG19:-}" ;;
        *)  printf '' ;;
    esac
}

majors="${KINGRES_MAJORS:-}"
if [ -z "${majors}" ]; then
    majors="18"
    [ -z "${KINGRES_PG19:-}" ] || majors="18 19"
fi

summary=()
failed=0
for major in ${majors}; do
    pg="$(pg_for_major "${major}")"
    if [ -z "${pg}" ]; then
        die "no installation configured for PostgreSQL ${major} (set KINGRES_PG${major})"
    fi
    printf '\n######## %s on PostgreSQL %s (%s)\n' "${ext}" "${major}" "${pg}"
    if KINGRES_PG="${pg}" EXT_WERROR=1 "${script_dir}/ext.sh" "${ext}" "$@"; then
        summary+=("PostgreSQL ${major}: ok")
    else
        summary+=("PostgreSQL ${major}: FAILED")
        failed=1
    fi
done

printf '\n==== %s ====\n' "${ext}"
printf '%s\n' "${summary[@]}"
exit "${failed}"

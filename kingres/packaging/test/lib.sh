# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (c) 2026, RooyeKhat Media Co
# shellcheck shell=bash
#
# Small test helpers shared by test-image.sh and test-deb.sh (sourced, not run).
#   note TEXT                      prints a section heading
#   check DESCRIPTION COMMAND...   runs the command; records pass or fail
#   check_eq DESCRIPTION EXPECTED ACTUAL
#   check_known DESCRIPTION REASON COMMAND...
#                                  a check that documents the right behaviour of
#                                  something that is known to be broken: a failure is
#                                  reported as KNOWN (with the reason) and does not
#                                  fail the run; a pass is reported, so that the
#                                  marker is removed once the defect is fixed
#   summary                        prints the totals; non-zero when a check failed

passed=0
failed=0
known=0

note() { printf '\n--- %s\n' "$*"; }

check() {
    local what="$1"
    shift
    if "$@" >/dev/null 2>&1; then
        printf 'PASS  %s\n' "${what}"
        passed=$((passed + 1))
    else
        printf 'FAIL  %s\n' "${what}"
        failed=$((failed + 1))
    fi
}

check_known() {
    local what="$1" reason="$2"
    shift 2
    if "$@" >/dev/null 2>&1; then
        printf 'PASS  %s (known defect "%s" seems fixed: remove the marker)\n' "${what}" "${reason}"
        passed=$((passed + 1))
    else
        printf 'KNOWN %s: %s\n' "${what}" "${reason}"
        known=$((known + 1))
    fi
}

check_eq() {
    if [ "$2" = "$3" ]; then
        printf 'PASS  %s (%s)\n' "$1" "$3"
        passed=$((passed + 1))
    else
        printf 'FAIL  %s: expected [%s], got [%s]\n' "$1" "$2" "$3"
        failed=$((failed + 1))
    fi
}

summary() {
    printf '\n%s passed, %s failed\n' "${passed}" "${failed}"
    [ "${known}" -eq 0 ] || printf '%s known failures (documented defects, not counted as failed)\n' "${known}"
    [ "${failed}" -eq 0 ]
}

pass() { printf 'PASS  %s\n' "$1"; passed=$((passed + 1)); }
fail() { printf 'FAIL  %s\n' "$1"; failed=$((failed + 1)); }

# ---------------------------------------------------------------- releases
# The KingresDB extensions that every package and image of a release carries
# (contrib/kingres_*).  build.sh refuses a staged tree that does not hold every
# extension of its source tree; this list is what the tests expect, so that a
# new extension has to be added here on purpose.
KINGRES_EXTENSIONS="kingres_columnar kingres_ledger kingres_shard kingres_sketch kingres_timepart"

# check_extension_files PREFIX COMMAND...: every extension is installed in the
# tree under /opt/kingres that COMMAND... looks into (for example: docker exec NAME).
check_extension_files() {
    local p="$1" e
    shift
    local share=/opt/kingres/share/postgresql/extension lib=/opt/kingres/lib/postgresql
    for e in ${KINGRES_EXTENSIONS}; do
        check "${p}: ${e} control file" "$@" test -f "${share}/${e}.control"
        check "${p}: ${e} install script" "$@" sh -c "ls ${share}/${e}--*.sql"
        # A module_pathname in the control file means a C library.
        check "${p}: ${e} library (when it has one)" "$@" sh -c \
            "! grep -q '^module_pathname' ${share}/${e}.control || test -f ${lib}/${e}.so"
    done
    check_eq "${p}: BUILD-INFO lists the extensions" "${KINGRES_EXTENSIONS}" \
        "$("$@" sed -n 's/^kingres extensions: //p' /opt/kingres/share/BUILD-INFO 2>/dev/null)"
}

# check_smoke_output PREFIX MAJOR OUTPUT: what test/pg-smoke.sh printed for the
# installed server of PostgreSQL MAJOR.
check_smoke_output() {
    local p="$1" major="$2" out="$3" e
    check "${p}: version() is PostgreSQL ${major} with KingresDB" \
        grep -Eq "^version: PostgreSQL ${major}[^ ]* .*\(KingresDB [0-9]+\.[0-9]+\.[0-9]+\)" <<<"${out}"
    check "${p}: server_version starts with ${major}" grep -Eq "^server_version: ${major}([.]|rc|beta|$)" <<<"${out}"
    for e in kingres_shard kingres_ledger kingres_sketch kingres_timepart; do
        check "${p}: CREATE EXTENSION ${e}" grep -q "^created: ${e}\$" <<<"${out}"
    done
    # kingres_columnar is not preloaded by default.  Its README: without the
    # preload the extension loads and reads, and refuses to write.  What the
    # build does: CREATE EXTENSION works; a columnar table cannot be created
    # (the message names shared_preload_libraries); with the preload (an
    # experiment, not the default) a table is created and written; and when the
    # preload is taken away again the table is read and a write is refused.
    check "${p}: kingres_columnar: CREATE EXTENSION works without the preload" \
        grep -q '^columnar-without-preload: create-extension ok$' <<<"${out}"
    check "${p}: kingres_columnar: creating a columnar table is refused without the preload, naming shared_preload_libraries" \
        grep -q '^columnar-without-preload: create-table refused.*shared_preload_libraries' <<<"${out}"
    check "${p}: kingres_columnar: with the preload (not the default) a table is created and written" \
        grep -q '^columnar-with-preload: insert accepted$' <<<"${out}"
    check "${p}: kingres_columnar: with the preload the rows are read back" \
        grep -q '^columnar-with-preload: count 3$' <<<"${out}"
    check "${p}: kingres_columnar: the preload taken away again: the table is still read" \
        grep -q '^columnar-preload-removed: count 3$' <<<"${out}"
    check "${p}: kingres_columnar: ... and a write is refused" \
        grep -q '^columnar-preload-removed: insert refused.*shared_preload_libraries' <<<"${out}"
}

# check_licence_files PREFIX COMMAND...: the licence files an installation
# carries (the Debian package and the image have the same ones).
check_licence_files() {
    local p="$1"
    shift
    local doc=/usr/share/doc/kingres
    check "${p}: the full text of the AGPL is in ${doc}/AGPL-3.0.txt" "$@" sh -c \
        "grep -q 'GNU AFFERO GENERAL PUBLIC LICENSE' ${doc}/AGPL-3.0.txt && grep -q 'Version 3, 19 November 2007' ${doc}/AGPL-3.0.txt && [ \"\$(wc -l < ${doc}/AGPL-3.0.txt)\" -gt 600 ]"
    check "${p}: the copyright file is machine-readable" "$@" sh -c "head -1 ${doc}/copyright | grep -q '^Format: https://www.debian.org/doc/packaging-manuals/copyright-format/1.0/'"
    check "${p}: the copyright file puts KingresDB's code under AGPL-3.0-only" "$@" sh -c "grep -q '^License: AGPL-3.0-only' ${doc}/copyright"
    check "${p}: the copyright file keeps PostgreSQL's files under the PostgreSQL License" "$@" sh -c "grep -q '^License: PostgreSQL' ${doc}/copyright"
    check "${p}: the copyright file no longer puts kingresd under the PostgreSQL License" "$@" sh -c \
        "! sed -n '/^Files: opt\/kingres\/bin\/kingresd/,/^$/p' ${doc}/copyright | grep -q '^License: PostgreSQL'"
    check "${p}: the lists of third-party modules are there" "$@" sh -c "test -s ${doc}/DEPENDENCIES.control-plane.md && test -s ${doc}/DEPENDENCIES.web.md"
}

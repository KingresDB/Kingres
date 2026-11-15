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

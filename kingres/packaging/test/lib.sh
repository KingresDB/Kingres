# SPDX-License-Identifier: PostgreSQL
# Copyright (c) 2026, RooyeKhat Media Co
# shellcheck shell=bash
#
# Small test helpers shared by test-image.sh and test-deb.sh (sourced, not run).
#   note TEXT                      prints a section heading
#   check DESCRIPTION COMMAND...   runs the command; records pass or fail
#   check_eq DESCRIPTION EXPECTED ACTUAL
#   summary                        prints the totals; non-zero when a check failed

passed=0
failed=0

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
    [ "${failed}" -eq 0 ]
}

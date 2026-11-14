#!/usr/bin/env bash
# SPDX-License-Identifier: PostgreSQL
#
# Runs Linux test binaries inside WSL against the private KingresDB
# installation in ~/kw-cp/install. Called by run-integration.ps1; arguments:
# the test binaries (each built with "go test -c" for linux/amd64) and, after
# "--", extra flags for them (for example -test.run Integration).
#
# Every instance the tests start lives under ~/kw-cp/test/run-<pid> on ports
# 56000-56199 and is removed afterwards, also after a failure.
set -uo pipefail

INSTALL="${KINGRES_INSTALL:-$HOME/kw-cp/install}"
ROOT="$HOME/kw-cp/test/run-$$"
mkdir -p "$ROOT"
export KINGRES_PG_BIN="$INSTALL/bin"
export KINGRES_TEST_ROOT="$ROOT"
export KINGRES_FIXTURES="$HOME/kw-cp/test/bin/fixtures"
export KINGRES_CONTRACT="$HOME/kw-cp/test/bin/openapi.yaml"
export LD_LIBRARY_PATH="$INSTALL/lib/x86_64-linux-gnu:$INSTALL/lib:${LD_LIBRARY_PATH:-}"
export PATH="$INSTALL/bin:$PATH"

bins=()
while [ $# -gt 0 ] && [ "$1" != "--" ]; do bins+=("$1"); shift; done
[ "${1:-}" = "--" ] && shift

cleanup() {
  # Stop anything left over from this run, then delete the directories.
  for pidfile in "$ROOT"/*/*/pgdata/postmaster.pid; do
    [ -f "$pidfile" ] || continue
    pid="$(head -1 "$pidfile")"
    kill -TERM "$pid" 2>/dev/null || true
  done
  sleep 1
  rm -rf "$ROOT"
}
trap cleanup EXIT

rc=0
for b in "${bins[@]}"; do
  echo "=== $(basename "$b")"
  ( cd "$(dirname "$b")" && "$b" -test.count=1 -test.v "$@" ) || rc=1
done
exit $rc

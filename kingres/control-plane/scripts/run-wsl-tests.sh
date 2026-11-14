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
#
# Files the tests need from the source tree are copied next to the binaries and
# named by environment variables (KINGRES_CONTRACT, KINGRES_OPERATIONS_GEN,
# KINGRES_FIXTURES); a test that cannot find its file fails, it never skips.
#
# Every test that skips must be on the allow-list wsl-allowed-skips.txt (one
# test name per line, the reason after a space); any other skip fails the run,
# so an integration test cannot silently stop running.
set -uo pipefail

INSTALL="${KINGRES_INSTALL:-$HOME/kw-cp/install}"
ROOT="$HOME/kw-cp/test/run-$$"
BIN="$HOME/kw-cp/test/bin"
mkdir -p "$ROOT"
export KINGRES_PG_BIN="$INSTALL/bin"
export KINGRES_TEST_ROOT="$ROOT"
export KINGRES_FIXTURES="$BIN/fixtures"
export KINGRES_OPERATIONS_GEN="$BIN/operations_gen.go.txt"
export KINGRES_CONTRACT="$BIN/openapi.yaml"
export KINGRES_README="$BIN/README.md"
export LD_LIBRARY_PATH="$INSTALL/lib/x86_64-linux-gnu:$INSTALL/lib:${LD_LIBRARY_PATH:-}"
export PATH="$INSTALL/bin:$PATH"

bins=()
while [ $# -gt 0 ] && [ "$1" != "--" ]; do bins+=("$1"); shift; done
[ "${1:-}" = "--" ] && shift

cleanup() {
  # Stop anything left over from this run (only instances below $ROOT), then delete the directories.
  for pidfile in "$ROOT"/*/*/pgdata/postmaster.pid "$ROOT"/*/pgdata/postmaster.pid; do
    [ -f "$pidfile" ] || continue
    pid="$(head -1 "$pidfile")"
    kill -TERM "$pid" 2>/dev/null || true
  done
  sleep 1
  rm -rf "$ROOT"
}
trap cleanup EXIT

ALLOW="$BIN/wsl-allowed-skips.txt"
rc=0
for b in "${bins[@]}"; do
  echo "=== $(basename "$b")"
  out="$ROOT/out.txt"
  ( cd "$(dirname "$b")" && "$b" -test.count=1 -test.v "$@" ) >"$out" 2>&1 || rc=1
  cat "$out"
  while read -r name; do
    [ -n "$name" ] || continue
    if ! grep -q "^$name\( \|\$\)" "$ALLOW"; then
      echo "UNEXPECTED SKIP: $name"
      rc=1
    fi
  done < <(grep -E '^[[:space:]]*--- SKIP: ' "$out" | sed -E 's/^[[:space:]]*--- SKIP: ([^ ]+).*/\1/')
done
exit $rc

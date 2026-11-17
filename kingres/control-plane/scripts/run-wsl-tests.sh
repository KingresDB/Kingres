#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Runs Linux test binaries inside WSL against the private KingresDB
# installation in ~/kw-cp/install. Called by run-integration.ps1; arguments:
# the test binaries (each built with "go test -c" for linux/amd64) and, after
# "--", extra flags for them (for example -test.run Integration).
#
#   run-wsl-tests.sh --reap     only clean up what earlier runs of THIS script left behind
#
# Every run owns
#   * a root directory ~/kw-cp/test/run-<pid>-<epoch> below which every instance and
#     kingresd data directory lives, and
#   * a slice of 200 TCP ports (20000-27999, below the range the kernel hands out to
#     outgoing connections) exported as KINGRES_TEST_PORT_BASE/END, so that two runs
#     never choose the same port and a killed run cannot hold the ports of the next.
#
# What the run started is identified by proof, never by a name pattern: the run
# exports KINGRES_TEST_ROOT=<its root>, which is unique to the run and inherited by
# everything the tests start (kingresd, pg_ctl, postmaster and its backends). A process
# belongs to a run if and only if that exact variable is in its environment. Each run
# records itself (pid, start time, root, port slice) in a ledger below
# ~/kw-cp/test/ledger; the next run reads the ledger, and for every entry whose script
# process is gone (pid and start time no longer match) it stops the processes that carry
# that root in their environment, removes the root and releases the port slice. Processes
# of other people's runs (other roots, or none) are never touched.
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
TESTDIR="$HOME/kw-cp/test"
BIN="$TESTDIR/bin"
LEDGER="$TESTDIR/ledger"
SLICES="$TESTDIR/slices"
PORT_LO=20000
SLICE_SIZE=200
SLICE_COUNT=40
mkdir -p "$LEDGER" "$SLICES"

# starttime PID prints field 22 of /proc/PID/stat (the start time in clock ticks since
# boot), which together with the pid identifies a process even when pids are reused.
starttime() {
  local s
  s=$(cat "/proc/$1/stat" 2>/dev/null) || return 1
  s=${s##*) }
  # shellcheck disable=SC2086
  set -- $s
  echo "${20}"
}

alive() { [ -n "$1" ] && [ "$(starttime "$1")" = "$2" ]; }

# carries ROOT PID succeeds when the process carries KINGRES_TEST_ROOT=ROOT. It reads
# the environment with shell builtins only, so that no helper process is created.
carries() {
  local kv
  while IFS= read -r -d '' kv; do
    [ "$kv" = "KINGRES_TEST_ROOT=$1" ] && return 0
  done 2>/dev/null <"/proc/$2/environ"
  return 1
}

# stop_root ROOT stops every process that carries ROOT (except this script): TERM first
# (fast shutdown for the instances), KILL for what is still there after 20 seconds.
stop_root() {
  local root="$1" p pids=() left=() i
  for p in /proc/[0-9]*; do
    p=${p#/proc/}
    [ "$p" = "$$" ] && continue
    carries "$root" "$p" && pids+=("$p")
  done
  [ ${#pids[@]} -eq 0 ] && return 0
  kill -TERM "${pids[@]}" 2>/dev/null || true
  for i in $(seq 1 40); do
    left=()
    for p in "${pids[@]}"; do
      # a zombie has no environment any more, so it no longer carries the root
      carries "$root" "$p" && left+=("$p")
    done
    [ ${#left[@]} -eq 0 ] && return 0
    sleep 0.5
  done
  kill -KILL "${left[@]}" 2>/dev/null || true
  sleep 1
}

locked() { exec 9>"$TESTDIR/.lock"; flock 9; }
unlock() { flock -u 9; }

# reap cleans the leftovers of runs whose script process is gone. Called with the lock.
reap() {
  local f pid st root
  for f in "$LEDGER"/*; do
    [ -f "$f" ] || continue
    pid="" st="" root=""
    { read -r pid; read -r st; read -r root; } <"$f" || true
    if [ -z "$pid" ] || [ -z "$root" ]; then rm -f "$f"; continue; fi
    alive "$pid" "$st" && continue # that run is still going (another one of this script)
    echo "reaping the leftovers of an earlier run: $root"
    case "$root" in "$TESTDIR"/run-*) ;; *) echo "refusing to touch $root"; continue ;; esac
    stop_root "$root"
    rm -rf "$root"
    rm -f "$f"
  done
  local d opid ost
  for d in "$SLICES"/s*; do
    [ -d "$d" ] || continue
    opid="" ost=""
    { read -r opid; read -r ost; } <"$d/owner" 2>/dev/null || true
    alive "$opid" "$ost" || rm -rf "$d"
  done
}

if [ "${1:-}" = "--reap" ]; then
  locked
  reap
  unlock
  exit 0
fi

ROOT="$TESTDIR/run-$$-$(date +%s)"
SLICE_N=-1

locked
reap
for ((n = 0; n < SLICE_COUNT; n++)); do
  if mkdir "$SLICES/s$n" 2>/dev/null; then
    printf '%s\n%s\n%s\n' "$$" "$(starttime $$)" "$ROOT" >"$SLICES/s$n/owner"
    SLICE_N=$n
    break
  fi
done
if [ "$SLICE_N" -lt 0 ]; then
  unlock
  echo "no free slice of test ports (all $SLICE_COUNT are owned by running runs)"
  exit 1
fi
mkdir -p "$ROOT"
printf '%s\n%s\n%s\n' "$$" "$(starttime $$)" "$ROOT" >"$LEDGER/$(basename "$ROOT")"
unlock

cleanup() {
  trap - EXIT
  stop_root "$ROOT"
  # KINGRES_TEST_KEEP keeps the directories of the run (the logs of every kingresd and instance)
  # for a post-mortem. Its ledger entry stays, so the next run reaps them: copy what is needed first.
  if [ -z "${KINGRES_TEST_KEEP:-}" ]; then
    rm -rf "$ROOT"
    rm -f "$LEDGER/$(basename "$ROOT")"
  fi
  [ "$SLICE_N" -ge 0 ] && rm -rf "$SLICES/s$SLICE_N"
}
trap cleanup EXIT
trap 'exit 130' INT TERM HUP

export KINGRES_PG_BIN="$INSTALL/bin"
export KINGRESD_BIN="$BIN/kingresd"
export KINGRES_VERIFY_BIN="$BIN/kingres-verify"
[ -x "$BIN/kingres-watchdog" ] && export KINGRES_WATCHDOG_BIN="$BIN/kingres-watchdog"
export KINGRES_TEST_ROOT="$ROOT"
export KINGRES_TEST_PORT_BASE=$((PORT_LO + SLICE_N * SLICE_SIZE))
export KINGRES_TEST_PORT_END=$((PORT_LO + SLICE_N * SLICE_SIZE + SLICE_SIZE - 1))
export KINGRES_FIXTURES="$BIN/fixtures"
export KINGRES_OPERATIONS_GEN="$BIN/operations_gen.go.txt"
export KINGRES_CONTRACT="$BIN/openapi.yaml"
export KINGRES_README="$BIN/README.md"
export LD_LIBRARY_PATH="$INSTALL/lib/x86_64-linux-gnu:$INSTALL/lib:${LD_LIBRARY_PATH:-}"
export PATH="$INSTALL/bin:$PATH"
echo "run root $ROOT, ports $KINGRES_TEST_PORT_BASE-$KINGRES_TEST_PORT_END"

bins=()
while [ $# -gt 0 ] && [ "$1" != "--" ]; do bins+=("$1"); shift; done
[ "${1:-}" = "--" ] && shift

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

#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Runs every fuzz target of the verifier for a short, bounded time. The seed
# corpora (the published vectors) also run in plain "go test ./...".
# Usage: scripts/fuzz.sh [seconds per target, default 8]
set -euo pipefail
cd "$(dirname "$0")/.."
secs="${1:-8}"
# Few workers: the machine is shared with long jobs.
export GOMAXPROCS="${GOMAXPROCS:-2}"
targets=(
  "chain FuzzParseDigest" "chain FuzzParseBlocks" "format FuzzDecodeHex" "format FuzzEncodeRow"
  "jsonx FuzzParse" "proof FuzzVerify" "rowfile FuzzParse"
  "signednote FuzzParse" "checkpoint FuzzVerify" "tlog FuzzParseTilePath" "tlog FuzzParseTile"
  "trust FuzzParse" "cli FuzzParseLogSpec" "safefile FuzzCleanRel"
)
for t in "${targets[@]}"; do
  set -- $t
  echo "== $1 $2"
  go test "./internal/$1" -run '^$' -fuzz "^$2\$" -fuzztime "${secs}s" -parallel 2
done

#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Builds kingres_shard, kingres_ledger and the analytics extensions kingres_columnar, kingres_timepart and
# kingres_sketch from a COMMIT (not from the working
# tree, which other engineers change) into the private installation
# ~/kw-cp/install, or the one named by KINGRES_INSTALL (the installation of
# the other PostgreSQL major, for example): the committed contrib
# directories are exported with git archive and built with PGXS.
# Usage: install-extensions.sh [commit] (default: HEAD of the repository).
# Records the commit in extensions-commit.txt of the installation.
set -euo pipefail
# The repository root is three levels above this script
# (kingres/control-plane/scripts); KINGRES_REPO overrides it.
repo="${KINGRES_REPO:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)}"
commit="$(git -c safe.directory='*' -C "$repo" rev-parse "${1:-HEAD}")"
inst="${KINGRES_INSTALL:-$HOME/kw-cp/install}"
for ext in kingres_shard kingres_ledger kingres_columnar kingres_timepart kingres_sketch; do
  src="$(dirname "$inst")/$ext-src"
  rm -rf "$src"; mkdir -p "$src"
  git -c safe.directory='*' -C "$repo" archive "$commit" "contrib/$ext" | tar -x -C "$src"
  make -C "$src/contrib/$ext" USE_PGXS=1 PG_CONFIG="$inst/bin/pg_config" install >/dev/null
  grep default_version "$inst/share/postgresql/extension/$ext.control"
done
echo "$commit" > "$inst/extensions-commit.txt"
echo "installed kingres_shard, kingres_ledger, kingres_columnar, kingres_timepart and kingres_sketch from commit $commit into $inst"

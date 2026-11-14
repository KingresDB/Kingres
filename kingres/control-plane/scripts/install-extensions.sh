#!/usr/bin/env bash
# SPDX-License-Identifier: PostgreSQL
#
# Builds kingres_shard and kingres_ledger from a COMMIT (not from the working
# tree, which other engineers change) into the private installation
# ~/kw-cp/install: the committed contrib directories are exported with
# git archive and built with PGXS. Usage: install-extensions.sh [commit]
# (default: HEAD of the repository). Records the commit in
# ~/kw-cp/install/extensions-commit.txt.
set -euo pipefail
repo="${KINGRES_REPO:?set KINGRES_REPO to the root of the repository}"
commit="$(git -c safe.directory='*' -C "$repo" rev-parse "${1:-HEAD}")"
inst="$HOME/kw-cp/install"
for ext in kingres_shard kingres_ledger; do
  src="$HOME/kw-cp/$ext-src"
  rm -rf "$src"; mkdir -p "$src"
  git -c safe.directory='*' -C "$repo" archive "$commit" "contrib/$ext" | tar -x -C "$src"
  make -C "$src/contrib/$ext" USE_PGXS=1 PG_CONFIG="$inst/bin/pg_config" install >/dev/null
  grep default_version "$inst/share/postgresql/extension/$ext.control"
done
echo "$commit" > "$inst/extensions-commit.txt"
echo "installed kingres_shard and kingres_ledger from commit $commit"

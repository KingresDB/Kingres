#!/usr/bin/env bash
# SPDX-License-Identifier: PostgreSQL
#
# Installs kingres_shard 0.2 into the private installation ~/kw-cp/install from
# a COMMIT (not from the working tree, which other engineers change): the
# committed directory contrib/kingres_shard is exported with git archive and
# built with PGXS. Usage: install-shard-0.2.sh [commit]   (default c283dbf6bc,
# "kingres_shard: version 0.2").
set -euo pipefail
commit="${1:-c283dbf6bc}"
repo="${KINGRES_REPO:?set KINGRES_REPO to the root of the repository}"
src="$HOME/kw-cp/shard-src"
rm -rf "$src"; mkdir -p "$src"
git -c safe.directory='*' -C "$repo" archive "$commit" contrib/kingres_shard | tar -x -C "$src"
export PATH="$HOME/kw-cp/install/bin:$PATH"
make -C "$src/contrib/kingres_shard" USE_PGXS=1 PG_CONFIG="$HOME/kw-cp/install/bin/pg_config" install >/dev/null
echo "installed kingres_shard from commit $commit"

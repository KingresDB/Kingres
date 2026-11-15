#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (c) 2026, RooyeKhat Media Co
#
# kingres/packaging/test/pg-smoke.sh
#
# Smoke test of an INSTALLED KingresDB server: create a throw-away cluster,
# start it with the two extensions preloaded, print version(), create both
# extensions, stop it.  Runs as an unprivileged user with the programs of
# /opt/kingres/bin on the PATH.  Used inside the image and inside the package
# test container (where it is run through "runuser -u kingres").
#
# Usage: pg-smoke.sh [WORKDIR]   (default: a temporary directory)

set -euo pipefail

PATH=/opt/kingres/bin:${PATH}
work="${1:-$(mktemp -d)}"
data="${work}/pgdata"
sock="${work}/sock"
mkdir -p "${sock}"

initdb -D "${data}" -A trust >/dev/null
printf "shared_preload_libraries = 'kingres_shard,kingres_ledger'\n" >>"${data}/postgresql.conf"
pg_ctl -D "${data}" -o "-k ${sock} -c listen_addresses=" -l "${work}/pg.log" -w start >/dev/null
trap 'pg_ctl -D "${data}" -m immediate -w stop >/dev/null 2>&1 || true; rm -rf "${data}"' EXIT

psql -h "${sock}" -d postgres -Atc 'select version()'
psql -h "${sock}" -d postgres -Atc 'create extension kingres_shard'
psql -h "${sock}" -d postgres -Atc 'create extension kingres_ledger'
psql -h "${sock}" -d postgres -Atc "select 'extension ' || extname || ' ' || extversion from pg_extension where extname like 'kingres%' order by extname"

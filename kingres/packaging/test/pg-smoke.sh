#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (c) 2026, RooyeKhat Media Co
#
# kingres/packaging/test/pg-smoke.sh
#
# Smoke test of an INSTALLED KingresDB server: create a throw-away cluster and
# look at every KingresDB extension in it.  Runs as an unprivileged user with
# the programs of /opt/kingres/bin on the PATH.  Used inside the image and
# inside the package test container (where it is run through "runuser -u
# kingres").  It prints "key: value" lines that the tests read:
#
#   version: ...                 select version()
#   server_version: ...          show server_version (18.7, 19rc2, ...)
#   created: NAME                CREATE EXTENSION NAME worked
#   create-failed: NAME ...      it did not (with the first line of the error)
#   extension NAME VERSION       pg_extension afterwards
#   columnar-without-preload: .. what kingres_columnar does when the library
#                                is NOT in shared_preload_libraries (its
#                                README: it loads and reads, and refuses to
#                                write): create, create table, insert, count
#   columnar-with-preload: ..    the same with the library preloaded
#
# kingres_shard and kingres_ledger are preloaded as the product does it;
# kingres_columnar is not preloaded by default (it registers a WAL resource
# manager), so the first run is the product's default.
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

sql() { psql -X -h "${sock}" -d postgres -v ON_ERROR_STOP=1 -Atq "$@"; }

printf 'version: %s\n' "$(sql -c 'select version()')"
printf 'server_version: %s\n' "$(sql -c 'show server_version')"

# The four that need no special setup in this cluster (two are preloaded, two
# need no preload at all).
for e in kingres_shard kingres_ledger kingres_sketch kingres_timepart; do
    if err="$(sql -c "create extension ${e}" 2>&1)"; then
        printf 'created: %s\n' "${e}"
    else
        printf 'create-failed: %s %s\n' "${e}" "$(printf '%s' "${err}" | head -1)"
    fi
done

# kingres_columnar without the preload.
col() { # col LABEL: the steps of the columnar check, printed under LABEL
    local label="$1" err n
    if err="$(sql -c 'create extension if not exists kingres_columnar' 2>&1)"; then
        printf '%s: create-extension ok\n' "${label}"
    else
        printf '%s: create-extension FAILED %s\n' "${label}" "$(printf '%s' "${err}" | head -1)"
        return 0
    fi
    sql -c "drop table if exists public.col_t" >/dev/null 2>&1 || true
    if err="$(sql -c 'create table public.col_t (a int, b text) using columnar' 2>&1)"; then
        printf '%s: create-table ok\n' "${label}"
    else
        printf '%s: create-table refused (%s)\n' "${label}" "$(printf '%s' "${err}" | head -1)"
        return 0
    fi
    if err="$(sql -c "insert into public.col_t select g, 'x' from generate_series(1,3) g" 2>&1)"; then
        printf '%s: insert accepted\n' "${label}"
    else
        printf '%s: insert refused (%s)\n' "${label}" "$(printf '%s' "${err}" | head -1)"
    fi
    n="$(sql -c 'select count(*) from public.col_t' 2>&1 | head -1)"
    printf '%s: count %s\n' "${label}" "${n}"
}
col columnar-without-preload

# The extensions as the catalog sees them.
sql -c "select 'extension ' || extname || ' ' || extversion from pg_extension where extname like 'kingres%' order by extname"

# And with the preload, to see that the files of the package are a working
# access method (not the default configuration of the product).
pg_ctl -D "${data}" -m fast -w stop >/dev/null
printf "shared_preload_libraries = 'kingres_shard,kingres_ledger,kingres_columnar'\n" >>"${data}/postgresql.conf"
pg_ctl -D "${data}" -o "-k ${sock} -c listen_addresses=" -l "${work}/pg2.log" -w start >/dev/null
col columnar-with-preload

# The preload taken away again: the table of the last step is read, a write is refused.
pg_ctl -D "${data}" -m fast -w stop >/dev/null
sed -i "/^shared_preload_libraries = 'kingres_shard,kingres_ledger,kingres_columnar'\$/d" "${data}/postgresql.conf"
pg_ctl -D "${data}" -o "-k ${sock} -c listen_addresses=" -l "${work}/pg3.log" -w start >/dev/null
printf 'columnar-preload-removed: count %s\n' "$(sql -c 'select count(*) from public.col_t' 2>&1 | head -1)"
if err="$(sql -c "insert into public.col_t values (9, 'y')" 2>&1)"; then
    printf 'columnar-preload-removed: insert accepted\n'
else
    printf 'columnar-preload-removed: insert refused (%s)\n' "$(printf '%s' "${err}" | head -1)"
fi

#!/usr/bin/env bash
#
# contrib/kingres_columnar/perf/perf.sh
#
# Size on disk and scan speed of the columnar table access method against
# heap, on four data shapes (shapes.sql): a time series of 20 columns, a wide
# table of 100 columns, random text, and highly repetitive data.  It starts
# its own throwaway server (shared_buffers = 1GB, fsync off), loads the data,
# and prints one table of results; nothing is asserted.
#
# Usage:   perf.sh [-p <installation directory>] [-s <scale>] [-r <runs>]
#   -p   the PostgreSQL installation to use (default: the one found in PATH);
#        the extension must be installed in it
#   -s   scale factor of the row counts (default 1: 2M time-series rows, 200k
#        wide rows, 1M text rows, 2M repetitive rows)
#   -r   runs per query (default 3); the best and the median are reported
#
# Serial is compared with serial: "heap serial" runs with
# max_parallel_workers_per_gather = 0, "heap par" with 2; the columnar table
# has no parallel scan.  Run it under nice on a loaded machine.  The timings
# of a server built with assertions are not representative of a release
# build; the sizes are.
#
# Portions Copyright (c) 2026, RooyeKhat Media Co
# SPDX-License-Identifier: AGPL-3.0-only

set -euo pipefail

scale=1
runs=3
pgdir=""
while getopts "p:s:r:" o; do
    case $o in
        p) pgdir=$OPTARG ;;
        s) scale=$OPTARG ;;
        r) runs=$OPTARG ;;
        *) echo "usage: $0 [-p dir] [-s scale] [-r runs]" >&2; exit 2 ;;
    esac
done
if [ -n "$pgdir" ]; then
    export PATH="$pgdir/bin:$PATH"
    export LD_LIBRARY_PATH="$pgdir/lib/x86_64-linux-gnu:$pgdir/lib:${LD_LIBRARY_PATH:-}"
fi
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

work="$(mktemp -d "${TMPDIR:-/tmp}/kcperf.XXXXXX")"
port=$((20000 + RANDOM % 20000))
cleanup() {
    pg_ctl -D "$work/data" -m immediate stop >/dev/null 2>&1 || true
    rm -rf "$work"
}
trap cleanup EXIT

initdb -D "$work/data" --no-sync >/dev/null
cat >> "$work/data/postgresql.conf" <<EOC
port = $port
unix_socket_directories = '$work'
listen_addresses = ''
shared_buffers = 1GB
fsync = off
synchronous_commit = off
max_wal_size = 16GB
checkpoint_timeout = 30min
autovacuum = off
shared_preload_libraries = 'kingres_columnar'
EOC
pg_ctl -D "$work/data" -l "$work/log" -w start >/dev/null
P="psql -X -q -At -h $work -p $port postgres"
$P -c "CREATE EXTENSION kingres_columnar" >/dev/null
echo "server: $(postgres --version)  assertions: $($P -c 'SHOW debug_assertions')  scale $scale  runs $runs"

ts_rows=$((2000000 * scale))
wide_rows=$((200000 * scale))
rtext_rows=$((1000000 * scale))
rep_rows=$((2000000 * scale))

now() { date +%s.%N; }
secs() { awk -v a="$1" -v b="$2" 'BEGIN { printf "%.1f", b - a }'; }

t0=$(now)
$P -v ts_rows=$ts_rows -v wide_rows=$wide_rows -v rtext_rows=$rtext_rows \
   -v rep_rows=$rep_rows -f "$here/shapes.sql" >/dev/null
echo "source tables built in $(secs "$t0" "$(now)") s"

# best/median (ms) of running a query $runs times in one session
# usage: timeit <setup sql> <query>
timeit() {
    local f="$work/q.sql"
    { echo "$1"; echo '\timing on'; for ((i = 0; i < runs; i++)); do echo "$2;"; done; } > "$f"
    $P -f "$f" 2>&1 | awk '/^Time:/ { print $2 }' | sort -n | awk '
        { v[NR] = $1 }
        END { if (NR == 0) { print "err"; exit } printf "%.0f/%.0f", v[1], v[int((NR + 1) / 2)] }'
}

# the queries of each shape, "name|sql" with %T for the table
declare -A Q
Q[ts]="count|SELECT count(*) FROM %T
agg2|SELECT dev, avg(m1) FROM %T GROUP BY dev
skip|SELECT count(*), avg(m2) FROM %T WHERE t >= timestamp '2024-01-12 13:46:40' AND t < timestamp '2024-01-12 13:56:40'"
Q[wide]="count|SELECT count(*) FROM %T
sum42|SELECT sum(c42) FROM %T
sum3|SELECT sum(c3), sum(c42), max(c99) FROM %T"
Q[rtext]="count|SELECT count(*) FROM %T
like|SELECT count(*) FROM %T WHERE s2 LIKE 'abc%'
id|SELECT max(id), min(id) FROM %T"
Q[rep]="count|SELECT count(*) FROM %T
sum|SELECT k, sum(v) FROM %T GROUP BY k ORDER BY k
filter|SELECT count(*), sum(v) FROM %T WHERE k = 3"
declare -A SRC=([ts]=src_ts [wide]=src_wide [rtext]=src_rtext [rep]=src_rep)

variants="none pglz lz4 zstd3 zstd9"
out=()

for shape in ts wide rtext rep; do
    src=${SRC[$shape]}
    mapfile -t qs <<< "${Q[$shape]}"
    qnames=()
    for q in "${qs[@]}"; do qnames+=("${q%%|*}"); done

    # heap: load, vacuum (hint bits, visibility map), size, queries
    $P -c "CREATE TABLE h_$shape (LIKE $src)" >/dev/null
    t0=$(now)
    $P -c "INSERT INTO h_$shape SELECT * FROM $src" >/dev/null
    hload=$(secs "$t0" "$(now)")
    $P -c "VACUUM h_$shape" >/dev/null
    hsize=$($P -c "SELECT pg_relation_size('h_$shape')")
    for mode in serial par; do
        setup="SET max_parallel_workers_per_gather = $([ "$mode" = serial ] && echo 0 || echo 2);"
        res=""
        for q in "${qs[@]}"; do
            sql="${q#*|}"; sql="${sql//%T/h_$shape}"
            res="$res  $(timeit "$setup" "$sql")"
        done
        out+=("$(printf '%-6s %-12s %9.1f %6.2f %7s %s' "$shape" "heap $mode" \
            "$(awk -v s="$hsize" 'BEGIN { print s / 1048576 }')" 1.00 "$hload" "$res")")
    done

    for v in $variants; do
        case $v in
            none)  opt="compression => 'none'" ;;
            pglz)  opt="compression => 'pglz'" ;;
            lz4)   opt="compression => 'lz4'" ;;
            zstd3) opt="compression => 'zstd', compression_level => 3" ;;
            zstd9) opt="compression => 'zstd', compression_level => 9" ;;
        esac
        t=c_${shape}_$v
        $P -c "CREATE TABLE $t (LIKE $src) USING columnar" >/dev/null
        $P -c "SELECT kingres.columnar_set_options('$t', $opt)" >/dev/null
        t0=$(now)
        $P -c "INSERT INTO $t SELECT * FROM $src" >/dev/null
        cload=$(secs "$t0" "$(now)")
        $P -c "VACUUM $t" >/dev/null
        csize=$($P -c "SELECT pg_relation_size('$t')")
        res=""
        for q in "${qs[@]}"; do
            sql="${q#*|}"; sql="${sql//%T/$t}"
            res="$res  $(timeit "SET max_parallel_workers_per_gather = 0;" "$sql")"
        done
        out+=("$(printf '%-6s %-12s %9.1f %6.2f %7s %s' "$shape" "col $v" \
            "$(awk -v s="$csize" 'BEGIN { print s / 1048576 }')" \
            "$(awk -v c="$csize" -v h="$hsize" 'BEGIN { printf "%.2f", c / h }')" "$cload" "$res")")
    done
    out+=("$(printf '       [%s] queries: %s' "$shape" "${qnames[*]}")")
    $P -c "DROP TABLE $src, h_$shape" >/dev/null
    for v in $variants; do $P -c "DROP TABLE c_${shape}_$v" >/dev/null; done
done

echo
printf '%-6s %-12s %9s %6s %7s  %s\n' shape variant MB 'x heap' 'load s' \
    'query times best/median ms, in the order listed under each shape'
printf '%s\n' "${out[@]}"

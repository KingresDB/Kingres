# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (c) 2026, RooyeKhat Media Co
# shellcheck shell=bash
# shellcheck disable=SC2016
#
# The "real" test flow of a KingresDB node, shared by test-deb.sh,
# test-image.sh and test-compose.sh (sourced, after lib.sh): turn an
# uninitialized node into a one-server cluster through the REST API, then
# work with the database.  It needs these variables from the caller:
#
#   API_CURL    array: the command that runs curl where the API is reachable
#               (for example (curl) on the host or (docker exec -i NAME curl))
#   API_BASE    base URL of the API, e.g. http://127.0.0.1:8091/api/v1
#   PSQL_CMD    array: command prefix that runs a psql which can reach the
#               instance over TCP as the administrator, with PGPASSWORD set
#               (for example (docker exec -e PGPASSWORD=... NAME kingres-psql
#               -h 127.0.0.1 -p 5432 -U root_admin))
#   TOKEN_CMD   function name: prints the setup token (kept out of the log)
#   DATA_DIR    data directory kingresd was started with
#
# The secrets of the test are throw-away values of a throw-away node.

FLOW_ADMIN=root_admin
FLOW_PASSWORD='Flow-Passw0rd-1'
FLOW_TOKEN=""

# api METHOD PATH [JSON]: prints the body, then the HTTP status on its own line.
api() {
    local method="$1" path="$2" body="${3:-}" args=(-s -X "$1" -w $'\n%{http_code}' -H 'Content-Type: application/json')
    [ -z "${FLOW_TOKEN}" ] || args+=(-H "Authorization: Bearer ${FLOW_TOKEN}")
    [ -z "${body}" ] || args+=(-d "${body}")
    "${API_CURL[@]}" "${args[@]}" "${API_BASE}${path}" || true
    : "${method}"
}
api_body() { local o="$1"; printf '%s' "${o%$'\n'*}"; }
api_code() { local o="$1"; printf '%s' "${o##*$'\n'}"; }

psql_run() { "${PSQL_CMD[@]}" "$@"; }

# flow_init_cluster NAME: POST init-cluster with the setup token, wait for the task.
flow_init_cluster() {
    local cname="$1" token out
    token="$("${TOKEN_CMD}")"
    out="$("${API_CURL[@]}" -s -X POST -w $'\n%{http_code}' -H 'Content-Type: application/json' \
        -H "X-Setup-Token: ${token}" \
        -d "{\"clusterName\":\"${cname}\",\"admin\":{\"username\":\"${FLOW_ADMIN}\",\"password\":\"${FLOW_PASSWORD}\"},\"dataPaths\":{\"dataDirectory\":\"${DATA_DIR}\"},\"memoryQuota\":{\"totalMegabytes\":1024}}" \
        "${API_BASE}/setup/init-cluster" || true)"
    check_eq "init-cluster accepted" 202 "$(api_code "${out}")"
    local s=""
    for _ in $(seq 1 240); do
        s="$("${API_CURL[@]}" -s "${API_BASE}/setup/state" || true)"
        case "${s}" in *'"state":"clustered"'* | *'"state":"standalone"'*) break ;; esac
        case "${s}" in *'"state":"failed"'*) break ;; esac
        sleep 0.5
    done
    case "${s}" in *'"state":"clustered"'* | *'"state":"standalone"'*) pass "setup task finished: node is no longer uninitialized" ;;
        *) fail "setup task did not finish: ${s:0:300}" ;;
    esac
}

flow_login() {
    local out
    FLOW_TOKEN=""
    out="$("${API_CURL[@]}" -s -X POST -w $'\n%{http_code}' -H 'Content-Type: application/json' \
        -d "{\"username\":\"${FLOW_ADMIN}\",\"password\":\"${FLOW_PASSWORD}\",\"issueToken\":true}" "${API_BASE}/auth/login" || true)"
    if [ "$(api_code "${out}")" = 200 ]; then
        FLOW_TOKEN="$(api_body "${out}" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')"
    fi
    check "sign in as ${FLOW_ADMIN}" test -n "${FLOW_TOKEN}"
}

# wait_for DESCRIPTION SECONDS COMMAND...
wait_for() {
    local what="$1" secs="$2"
    shift 2
    for _ in $(seq 1 $((secs * 2))); do
        if "$@" >/dev/null 2>&1; then pass "${what}"; return 0; fi
        sleep 0.5
    done
    fail "${what} (timed out)"
    # The failure is recorded; a script that runs with "set -e" must go on to the
    # next check, so the status is 0 (callers that need to know compare "failed").
    return 0
}

flow_database_has() { # flow_database_has REGEX
    api GET /databases/shop | grep -Eq "$1"
}

flow_create_data() {
    local out
    out="$(api POST /roles '{"name":"app_user","password":"App-Passw0rd-12","clusterRoles":["kingres_readonly"]}')"
    check_eq "create role app_user" 201 "$(api_code "${out}")"
    out="$(api POST /databases '{"name":"shop","sharding":{"enabled":true,"bucketCount":32},"ledger":{"mode":"optIn"}}')"
    case "$(api_code "${out}")" in 200 | 201 | 202) pass "create database shop (sharding, ledger)" ;; *) fail "create database: $(api_code "${out}") $(api_body "${out}" | cut -c1-200)" ;; esac
    wait_for "database shop has sharding enabled" 90 flow_database_has '"sharding":\{[^}]*"enabled":true'
    check "create tables with psql" "${PSQL_CMD[@]}" -d shop -v ON_ERROR_STOP=1 \
        -c "create table public.orders (id bigint primary key, customer text)" \
        -c "create table public.audit_rows (id bigint primary key, note text)"
    out="$(api PUT /databases/shop/schemas/public/tables/orders/distribution '{"method":"hash","distributionColumn":"id"}')"
    case "$(api_code "${out}")" in 200 | 202) pass "distribute table orders through the API" ;; *) fail "distribute: $(api_code "${out}") $(api_body "${out}" | cut -c1-200)" ;; esac
    wait_for "orders is a distributed table" 90 flow_orders_distributed
    out="$(api PUT /databases/shop/schemas/public/tables/audit_rows/ledger '{"mode":"appendOnly"}')"
    case "$(api_code "${out}")" in 200 | 202) pass "enable the ledger on audit_rows through the API" ;; *) fail "enable ledger: $(api_code "${out}") $(api_body "${out}" | cut -c1-200)" ;; esac
    sleep 2
    check "insert rows with psql" "${PSQL_CMD[@]}" -d shop -v ON_ERROR_STOP=1 \
        -c "insert into public.audit_rows values (1, 'first')" -c "insert into public.orders values (7, 'acme')"
    check_eq "read the ledger row back" first "$(psql_run -d shop -Atc 'select note from public.audit_rows where id = 1' 2>/dev/null)"
    out="$(api POST /databases/shop/ledger/digests)"
    case "$(api_code "${out}")" in 200 | 201 | 202) pass "generate a ledger digest" ;; *) fail "digest: $(api_code "${out}") $(api_body "${out}" | cut -c1-200)" ;; esac
}

# flow_check_data PREFIX: the data created by flow_create_data is there.
flow_check_data() {
    local p="$1"
    check_eq "${p}: row of orders" acme "$(psql_run -d shop -Atc 'select customer from public.orders where id = 7' 2>/dev/null)"
    check_eq "${p}: row of audit_rows" first "$(psql_run -d shop -Atc 'select note from public.audit_rows where id = 1' 2>/dev/null)"
    check_eq "${p}: both extensions present" "kingres_ledger kingres_shard" \
        "$(psql_run -d shop -Atc "select string_agg(extname, ' ' order by extname) from pg_extension where extname like 'kingres%'" 2>/dev/null)"
}

flow_check_api() { # flow_check_api PREFIX: sign in again, read what the API knows
    local p="$1" out
    flow_login
    out="$(api GET /databases/shop)"
    check_eq "${p}: GET /databases/shop" 200 "$(api_code "${out}")"
    out="$(api GET /databases/shop/ledger/digests)"
    check "${p}: a ledger digest is listed" bash -c 'case "$1" in *\"digest*|*\"hash*|*\"blockNumber*) exit 0;; *) exit 1;; esac' _ "$(api_body "${out}")"
    check_eq "${p}: node state" clustered "$("${API_CURL[@]}" -s "${API_BASE}/setup/state" | sed -n 's/^{"state":"\([a-z]*\)".*/\1/p')"
}

flow_orders_distributed() {
    api GET /databases/shop/schemas/public/tables/orders | grep -Eq '"(hash|distributed)"'
}

# flow_task_state ID: the state of a task as the node at API_BASE reports it.
flow_task_state() {
    api GET "/tasks/$1" | sed -n 's/^{"id":[0-9]*,"kind":"[^"]*","state":"\([a-z]*\)".*/\1/p'
}

# flow_wait_task DESCRIPTION ID SECONDS: wait until the task is done (pass) or
# failed (fail, with the task's error).  Uses the signed-in node (FLOW_TOKEN).
flow_wait_task() {
    local what="$1" id="$2" secs="$3" s=""
    for _ in $(seq 1 $((secs * 2))); do
        s="$(flow_task_state "${id}")"
        case "${s}" in done | failed | cancelled) break ;; esac
        sleep 0.5
    done
    if [ "${s}" = "done" ]; then
        pass "${what}"
    else
        fail "${what} (task ${id} is '${s}'): $(api GET "/tasks/${id}" | sed -n 's/.*"error":\({[^}]*}\).*/\1/p' | cut -c1-300)"
    fi
}

# flow_check_extensions PREFIX: every KingresDB extension can be created in a
# database of the running node (a scratch database, dropped afterwards).
# kingres_columnar is preloaded by the product (it registers a WAL resource
# manager), so a columnar table can be created and written.
# It needs SERVICE_PSQL, a command prefix like PSQL_CMD that runs psql as the
# service role kingresd (a superuser; CREATE EXTENSION needs one and the
# administrator of the flow is not), with its password from the node's
# service-credentials file.
flow_check_extensions() {
    local p="$1" e out
    check_eq "${p}: the instance preloads kingres_shard, kingres_ledger, kingres_columnar and pg_stat_statements" "kingres_shard,kingres_ledger,kingres_columnar,pg_stat_statements" \
        "$(svc -d postgres -Atc 'show shared_preload_libraries' 2>/dev/null)"
    # kingresd makes pg_stat_statements in the databases it creates, and the library is loaded, so the view answers (this statement is in it).
    check_eq "${p}: pg_stat_statements is in the database postgres and collects statements" "t" \
        "$(svc -d postgres -Atc 'select count(*) > 0 from pg_stat_statements' 2>/dev/null)"
    # plpgsql is in template1 of the cluster: a function written in it works in a database made from the template.
    check "${p}: a plpgsql function can be created and called" svc -d postgres -v ON_ERROR_STOP=1 \
        -c 'create function pg_temp.kingres_plpgsql_check() returns int language plpgsql as $$ begin return 42; end $$' \
        -c 'select pg_temp.kingres_plpgsql_check()'
    check "${p}: create the scratch database ext_check" svc -d postgres -v ON_ERROR_STOP=1 -c 'create database ext_check'
    # kingres_shard first, as the product creates them; the other order is
    # checked below.
    for e in kingres_shard ${KINGRES_EXTENSIONS/kingres_shard/}; do
        if out="$(svc -d ext_check -v ON_ERROR_STOP=1 -c "create extension ${e}" 2>&1)"; then
            pass "${p}: CREATE EXTENSION ${e} in the node's own server"
        else
            fail "${p}: CREATE EXTENSION ${e} in the node's own server: $(printf '%s' "${out}" | head -3 | tr '\n' ' ' | cut -c1-300)"
        fi
    done
    check_eq "${p}: all five are in pg_extension" "${KINGRES_EXTENSIONS}" \
        "$(svc -d ext_check -Atc "select string_agg(extname, ' ' order by extname) from pg_extension where extname like 'kingres%'" 2>/dev/null)"
    # The library is preloaded, so a columnar table can be created and written.
    check "${p}: a columnar table can be created and written (the library is preloaded)" svc -d ext_check -v ON_ERROR_STOP=1 \n        -c 'create table public.col_t (a int) using columnar' -c 'insert into public.col_t values (1)'
    check "${p}: drop the scratch database" svc -d postgres -v ON_ERROR_STOP=1 -c 'drop database ext_check'
    # The order must not matter: every extension that uses the schema kingres
    # installs whether or not another one made the schema first.  An earlier
    # install script of kingres_shard ran "CREATE SCHEMA kingres;" and failed
    # after kingres_columnar or kingres_timepart (schema = kingres in their
    # control files).
    check "${p}: create a second scratch database for the order of the extensions" svc -d postgres -v ON_ERROR_STOP=1 -c 'create database ext_check2'
    check "${p}: kingres_shard can be created after kingres_columnar in one database" \
        flow_shard_after_columnar
    check "${p}: drop the second scratch database" svc -d postgres -v ON_ERROR_STOP=1 -c 'drop database ext_check2'
    flow_check_extension_versions "${p}"
    flow_check_distribute_rows "${p}"
    flow_check_timepart_columnar "${p}"
}

# flow_check_extension_versions PREFIX: ALTER EXTENSION ... UPDATE from the
# versions that an earlier package carried (kingres_shard 0.6, kingres_timepart
# 0.1, kingres_columnar 0.1, kingres_ledger 0.1) to the ones of this package.
# The library is the new one all the time, as after a package upgrade.
flow_check_extension_versions() {
    local p="$1" e
    check "${p}: create the scratch database ext_upgrade" svc -d postgres -v ON_ERROR_STOP=1 -c 'create database ext_upgrade'
    check "${p}: install the old versions (shard 0.6, ledger 0.1, timepart 0.1, columnar 0.1)" \
        svc -d ext_upgrade -v ON_ERROR_STOP=1 \
        -c "create extension kingres_shard version '0.6'" -c "create extension kingres_ledger version '0.1'" \
        -c "create extension kingres_timepart version '0.1'" -c "create extension kingres_columnar version '0.1'" \
        -c "create extension kingres_sketch"
    check_eq "${p}: the old versions are in the catalog" "kingres_columnar 0.1 kingres_ledger 0.1 kingres_shard 0.6 kingres_sketch 1.0 kingres_timepart 0.1" \
        "$(svc -d ext_upgrade -Atc "select string_agg(extname || ' ' || extversion, ' ' order by extname) from pg_extension where extname like 'kingres%'" 2>/dev/null)"
    for e in kingres_shard kingres_ledger kingres_timepart kingres_columnar; do
        check "${p}: ALTER EXTENSION ${e} UPDATE" svc -d ext_upgrade -v ON_ERROR_STOP=1 -c "alter extension ${e} update"
    done
    check_eq "${p}: every extension is at the default version of this package" \
        "kingres_columnar 0.2 kingres_ledger 0.3 kingres_shard 0.8 kingres_sketch 1.0 kingres_timepart 0.2" \
        "$(svc -d ext_upgrade -Atc "select string_agg(extname || ' ' || extversion, ' ' order by extname) from pg_extension where extname like 'kingres%'" 2>/dev/null)"
    check "${p}: the updated kingres_timepart has the conversion procedure of 0.2" \
        svc -d ext_upgrade -v ON_ERROR_STOP=1 -c "select 'kingres.convert_partition'::regproc" -c "select 'kingres.table_change_report'::regproc"
    check "${p}: drop the scratch database ext_upgrade" svc -d postgres -v ON_ERROR_STOP=1 -c 'drop database ext_upgrade'
    # the step of this release: kingres_shard 0.7 (the previous package) to 0.8 (distributed schemas, the bucket layout, splits)
    check "${p}: create the scratch database ext_upgrade78" svc -d postgres -v ON_ERROR_STOP=1 -c 'create database ext_upgrade78'
    check "${p}: install kingres_shard 0.7" svc -d ext_upgrade78 -v ON_ERROR_STOP=1 -c "create extension kingres_shard version '0.7'"
    check "${p}: ALTER EXTENSION kingres_shard UPDATE (0.7 to 0.8)" svc -d ext_upgrade78 -v ON_ERROR_STOP=1 -c "alter extension kingres_shard update"
    check_eq "${p}: kingres_shard is at 0.8" "kingres_shard 0.8" \
        "$(svc -d ext_upgrade78 -Atc "select extname || ' ' || extversion from pg_extension where extname = 'kingres_shard'" 2>/dev/null)"
    check "${p}: the 0.8 objects are there (schemas view, distribute_schema, split_bucket)" svc -d ext_upgrade78 -v ON_ERROR_STOP=1 \
        -c "select 'kingres.schemas'::regclass" -c "select 'kingres.distribute_schema'::regproc" -c "select 'kingres.split_bucket'::regproc"
    check "${p}: drop the scratch database ext_upgrade78" svc -d postgres -v ON_ERROR_STOP=1 -c 'drop database ext_upgrade78'
}

# flow_check_distribute_rows PREFIX: create_distributed_table of a table that
# holds rows (kingres_shard 0.7) on the cluster this package made: the rows are
# still there, counted by every path, and a new row is routed.
flow_check_distribute_rows() {
    local p="$1"
    check "${p}: create a table with 1000 rows in shop" "${PSQL_CMD[@]}" -d shop -v ON_ERROR_STOP=1 \
        -c "create table public.stock (id bigint primary key, qty int not null default 0, note text)" \
        -c "insert into public.stock select g, g % 7, 'n' || g from generate_series(1,1000) g"
    check "${p}: kingres.create_distributed_table of a table that holds rows" \
        svc -d shop -v ON_ERROR_STOP=1 -c "select kingres.create_distributed_table('public.stock', 'id')"
    check_eq "${p}: the table is registered as distributed" 1 \
        "$(svc -d shop -Atc "select count(*) from kingres.dist_table where relid = 'public.stock'::regclass" 2>/dev/null)"
    check_eq "${p}: all 1000 rows are there after the creation (count, sum)" "1000 500500" \
        "$(psql_run -d shop -Atc "select count(*) || ' ' || sum(id) from public.stock" 2>/dev/null)"
    check_eq "${p}: a row is found by its key" n421 "$(psql_run -d shop -Atc 'select note from public.stock where id = 421' 2>/dev/null)"
    check "${p}: a new row is routed" "${PSQL_CMD[@]}" -d shop -v ON_ERROR_STOP=1 -c "insert into public.stock values (1001, 1, 'new')"
    check_eq "${p}: and counted" 1001 "$(psql_run -d shop -Atc 'select count(*) from public.stock' 2>/dev/null)"
}

# flow_check_timepart_columnar PREFIX: kingres_timepart 0.2 converts a partition
# to columnar storage and back, and a columnar table can have a B-tree index
# (kingres_columnar 0.2).  Both need kingres_columnar preloaded, so what the
# test expects depends on what the instance preloads: with the library
# (flow_check_extensions asserts the list) both work; without it the conversion
# and the creation of a columnar table are refused with the name of the
# setting, and the partition stays a heap table with its rows.
flow_check_timepart_columnar() {
    local p="$1" out preload am
    preload="$(svc -d postgres -Atc 'show shared_preload_libraries' 2>/dev/null)"
    check "${p}: create the scratch database ext_tp" svc -d postgres -v ON_ERROR_STOP=1 -c 'create database ext_tp'
    check "${p}: kingres_shard, kingres_columnar and kingres_timepart in it" svc -d ext_tp -v ON_ERROR_STOP=1 \
        -c 'create extension kingres_shard' -c 'create extension kingres_columnar' -c 'create extension kingres_timepart'
    check "${p}: a time-partitioned table with 1000 rows" svc -d ext_tp -v ON_ERROR_STOP=1 \
        -c "create table public.ev (ts timestamptz not null, n int) partition by range (ts)" \
        -c "create table public.ev_2026_01 partition of public.ev for values from ('2026-01-01 00:00:00+00') to ('2026-02-01 00:00:00+00')" \
        -c "insert into public.ev select '2026-01-05 00:00:00+00'::timestamptz + g * interval '1 minute', g from generate_series(1,1000) g"
    am="select a.amname from pg_class c join pg_am a on a.oid = c.relam where c.oid = 'public.ev_2026_01'::regclass"
    out="$(svc -d ext_tp -v ON_ERROR_STOP=1 -c "call kingres.convert_partition('public.ev', 'public.ev_2026_01')" 2>&1 || true)"
    case ",${preload}," in
        *,kingres_columnar,*)
            check_eq "${p}: the partition is columnar after the conversion" columnar "$(svc -d ext_tp -Atc "${am}" 2>/dev/null)"
            check_eq "${p}: ... with its rows" "1000 500500" "$(svc -d ext_tp -Atc 'select count(*) || chr(32) || sum(n) from public.ev' 2>/dev/null)"
            check "${p}: convert it back to heap" svc -d ext_tp -v ON_ERROR_STOP=1 -c "call kingres.convert_partition('public.ev', 'public.ev_2026_01', 'heap')"
            check_eq "${p}: the partition is heap again" heap "$(svc -d ext_tp -Atc "${am}" 2>/dev/null)"
            check_eq "${p}: ... with its rows" "1000 500500" "$(svc -d ext_tp -Atc 'select count(*) || chr(32) || sum(n) from public.ev' 2>/dev/null)"
            check "${p}: a columnar table with a B-tree index" svc -d ext_tp -v ON_ERROR_STOP=1 \
                -c 'create table public.col_i (id int, v text) using columnar' \
                -c "insert into public.col_i select g, 'v' || g from generate_series(1,2000) g" \
                -c 'create index col_i_id on public.col_i (id)'
            check_eq "${p}: ... and a lookup through it" v1234 "$(svc -d ext_tp -Atc 'select v from public.col_i where id = 1234' 2>/dev/null)"
            ;;
        *)
            check "${p}: the conversion to columnar is refused, naming shared_preload_libraries (${out:0:120})" \
                bash -c 'grep -q "shared_preload_libraries" <<<"$1"' _ "${out}"
            check_eq "${p}: the partition is still heap" heap "$(svc -d ext_tp -Atc "${am}" 2>/dev/null)"
            check_eq "${p}: ... with all its rows" "1000 500500" "$(svc -d ext_tp -Atc 'select count(*) || chr(32) || sum(n) from public.ev' 2>/dev/null)"
            out="$(svc -d ext_tp -v ON_ERROR_STOP=1 -c 'create table public.col_i (id int) using columnar' 2>&1 || true)"
            check "${p}: a columnar table (and so a B-tree index on one) is refused, naming shared_preload_libraries" \
                bash -c 'grep -q "shared_preload_libraries" <<<"$1"' _ "${out}"
            ;;
    esac
    check "${p}: drop the scratch database ext_tp" svc -d postgres -v ON_ERROR_STOP=1 -c 'drop database ext_tp'
}
flow_shard_after_columnar() {
    svc -d ext_check2 -v ON_ERROR_STOP=1 -c 'create extension kingres_columnar' &&
        svc -d ext_check2 -v ON_ERROR_STOP=1 -c 'create extension kingres_shard'
}
# svc ARGS: psql as the service role (see SERVICE_PSQL above).
svc() { "${SERVICE_PSQL[@]}" "$@"; }

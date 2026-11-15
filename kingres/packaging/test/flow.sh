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
    return 1
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

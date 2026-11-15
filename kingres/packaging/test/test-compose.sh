#!/usr/bin/env bash
# SPDX-License-Identifier: PostgreSQL
# Copyright (c) 2026, RooyeKhat Media Co
#
# kingres/packaging/test/test-compose.sh
#
# Test the three-node development cluster of docker/compose.yaml.  The nodes
# start from a clean snapshot, become healthy, each reports "uninitialized"
# on its own published port, each has a different setup token (read with
# tokens.sh; the tokens are not printed) and the nodes reach each other by
# name.  Then the nodes become ONE cluster, through the REST API only:
#
#   - kingres-1 is initialized as a new cluster (setup token, initCluster);
#   - kingres-2 is added by the cluster (addServer on kingres-1), with the
#     fingerprint of its internal-API certificate pinned;
#   - kingres-3 joins by its own request (joinCluster), with the fingerprint
#     of kingres-1 pinned; a join with a wrong pin is refused first.
#
# It then checks the three servers on every node, the administrator's sign-in
# on every node, a sharded database created on kingres-2, a table distributed
# and written on kingres-3 and read on all three nodes, the restart of one
# non-leader node (also with a write while it is down), and the stop and start
# of the whole cluster.  "down --volumes" removes everything it created.
#
# Usage:
#   test-compose.sh [--context DIR] [--rev REV] [--port-base N] [--keep]
#
#   --context DIR  build context (default: a snapshot of REV plus the
#                  working-tree kingres/packaging directory)
#   --rev REV      commit the default snapshot is made from (default HEAD)
#   --port-base N  host ports N+1, N+2, N+3 for the panels and N+11, N+12,
#                  N+13 for the databases (default 58100)
#   --keep         leave the nodes (and the snapshot) in place when the test
#                  ends or fails, to look at them; remove them with
#                  "docker compose -f CONTEXT/kingres/packaging/docker/compose.yaml
#                  down --volumes"

# shellcheck disable=SC2016
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "${here}/lib.sh"
# shellcheck source=flow.sh
. "${here}/flow.sh"

context=""
rev=HEAD
base=58100
keep=0
while [ "$#" -gt 0 ]; do
    case "$1" in
        --context)   context="$2"; shift 2 ;;
        --rev)       rev="$2"; shift 2 ;;
        --port-base) base="$2"; shift 2 ;;
        --keep)      keep=1; shift ;;
        *) echo "test-compose.sh: unknown argument '$1'" >&2; exit 2 ;;
    esac
done

tmp="$(mktemp -d)"
if [ -z "${context}" ]; then
    context="${tmp}/context"
    "${here}/../build/snapshot.sh" "${context}" --rev "${rev}" --overlay kingres/packaging
fi
compose="${context}/kingres/packaging/docker/compose.yaml"
export KINGRES_BUILD_CONTEXT="${context}"
export KINGRES_IMAGE="${KINGRES_IMAGE:-kingres:compose-test}"
export KINGRES_PANEL_PORT_1=$((base + 1)) KINGRES_PANEL_PORT_2=$((base + 2)) KINGRES_PANEL_PORT_3=$((base + 3))
export KINGRES_PG_PORT_1=$((base + 11)) KINGRES_PG_PORT_2=$((base + 12)) KINGRES_PG_PORT_3=$((base + 13))

cleanup() {
    if [ "${keep}" = 1 ]; then
        echo "--keep: the nodes are left running (docker compose -f ${compose} down --volumes)"
        return
    fi
    docker compose -f "${compose}" down --volumes --remove-orphans >/dev/null 2>&1 || true
    rm -rf "${tmp}"
}
trap cleanup EXIT

note "building and starting three nodes"
start=$SECONDS
docker compose -f "${compose}" up --build --detach --quiet-pull 2>&1 | tail -5
echo "up time: $((SECONDS - start)) s"

for n in 1 2 3; do
    h=starting
    for _ in $(seq 1 90); do
        h="$(docker inspect --format '{{.State.Health.Status}}' "kingres-${n}")"
        [ "${h}" = healthy ] && break
        sleep 1
    done
    check_eq "kingres-${n} health" healthy "${h}"
    port=$((base + n))
    state="$(curl -fsS "http://127.0.0.1:${port}/api/v1/setup/state" || true)"
    check_eq "kingres-${n} state on port ${port}" uninitialized \
        "$(printf '%s' "${state}" | sed -n 's/^{"state":"\([a-z]*\)".*/\1/p')"
    check_eq "kingres-${n} hostname" "kingres-${n}" "$(docker exec "kingres-${n}" hostname)"
done

note "tokens"
"${here}/../docker/tokens.sh" >"${tmp}/tokens.txt"
check_eq "tokens.sh lists three tokens" 3 "$(grep -c 'setup token: ' "${tmp}/tokens.txt")"
check_eq "the three tokens are different" 3 "$(sed -n 's/.*setup token: //p' "${tmp}/tokens.txt" | sort -u | wc -l)"

# ------------------------------------------------------------ node helpers
# use_node N points the flow.sh helpers (api, psql_run, ...) at node N and
# restores the bearer token that node issued: a session is kept in the memory
# of one server and is not valid on another.
tokens=("" "" "" "")
use_node() {
    NODE="kingres-$1"
    API_CURL=(curl)
    API_BASE="http://127.0.0.1:$((base + $1))/api/v1"
    DATA_DIR=/var/lib/kingres
    TOKEN_CMD=node_token
    PSQL_CMD=(docker exec -e "PGPASSWORD=${FLOW_PASSWORD}" "${NODE}" psql -X -h 127.0.0.1 -p 5432 -U "${FLOW_ADMIN}")
    FLOW_TOKEN="${tokens[$1]}"
}
node_token() { docker exec "${NODE}" cat /var/lib/kingres/setup-token; }
signin_node() { # a fresh sign-in on node N with the cluster's administrator
    use_node "$1"
    flow_login
    tokens[$1]="${FLOW_TOKEN}"
}
node_up() { [ "$(docker inspect --format '{{.State.Health.Status}}' "kingres-$1")" = healthy ]; }
db_up() { psql_run -d postgres -Atc 'select 1' >/dev/null 2>&1; }

# SHA-256 (lower-case hex, no colons) of the DER form of a PEM certificate on stdin.
pem_fingerprint() {
    sed -n '/-----BEGIN CERTIFICATE-----/,/-----END CERTIFICATE-----/{/-----/d;p}' | base64 -d | sha256sum | cut -d' ' -f1
}

# servers_line: "kingres-1=live kingres-2=live ..." as the node at API_BASE lists them.
servers_line() {
    api GET /servers | grep -o '"id":"[^"]*","address":"[^"]*","hostname":"[^"]*","status":"[A-Za-z]*"' \
        | sed -E 's/"id":"([^"]*)".*"status":"([A-Za-z]*)"/\1=\2/' | tr '\n' ' ' | sed 's/ $//'
}
# A server that has joined but holds no data yet is "pendingRebalance" (the
# contract), a working one "live": both are healthy.  suspect, dead, draining
# and decommissioning are not.
servers_all_live() {
    [ "$(servers_line | sed 's/=pendingRebalance/=live/g')" = "kingres-1=live kingres-2=live kingres-3=live" ]
}
# wait_servers_live DESCRIPTION N...: each listed node, signed in again,
# reports the three servers as live.
wait_servers_live() {
    local what="$1" n
    shift
    for n in "$@"; do
        signin_node "${n}" >/dev/null
        wait_for "${what}: listServers on kingres-${n} shows three live servers" 90 servers_all_live || {
            echo "info: kingres-${n} lists: $(servers_line) / $(api GET /servers | cut -c1-400)"
            return 1
        }
    done
}
event_count() { psql_run -d mesh -Atc 'select count(*) from public.events' 2>/dev/null; }
event_count_is() { [ "$(event_count)" = "$1" ]; }
# insert_events FROM TO: one INSERT per row (a statement that spans shard
# groups is not supported yet); prints how many were accepted.
insert_events() {
    local i ok=0
    for i in $(seq "$1" "$2"); do
        if psql_run -d mesh -v ON_ERROR_STOP=1 -qc "insert into public.events values (${i}, 'row ${i}')" >/dev/null 2>&1; then
            ok=$((ok + 1))
        fi
    done
    echo "${ok}"
}

# ------------------------------------------------ kingres-1: a new cluster
# The same flow as the single-node tests: setup token, initCluster, sign-in,
# a role, a sharded database with the ledger, tables, rows.  Everything that
# exists before the joins must reach the servers that join later.
note "kingres-1: create the cluster through the setup API"
use_node 1
flow_init_cluster kingres-cluster
signin_node 1
flow_create_data
flow_check_data "kingres-1"

# ----------------------------------------------------------- fingerprints
# A server that is not in a cluster presents a self-signed "bootstrap"
# certificate on its internal API port: its fingerprint is logged at start and
# is the SHA-256 of pki/bootstrap.crt.  A server of a cluster presents
# pki/server.crt.  The fingerprint to trust goes in X-Kingres-Pin-Sha256.
note "certificate fingerprints"
fp_log2="$(docker logs kingres-2 2>&1 | sed -n 's/.*"internal API bootstrap certificate","sha256":"\([^"]*\)".*/\1/p' | head -1)"
fp_file2="$(docker exec kingres-2 cat /var/lib/kingres/pki/bootstrap.crt | pem_fingerprint)"
check_eq "kingres-2: the logged fingerprint is the SHA-256 of pki/bootstrap.crt" \
    "${fp_file2}" "$(printf '%s' "${fp_log2}" | tr -d ':' | tr 'A-F' 'a-f')"
fp_node1="$(docker exec kingres-1 cat /var/lib/kingres/pki/server.crt | pem_fingerprint)"
check_eq "kingres-1: digits of the fingerprint of its cluster certificate" 64 "${#fp_node1}"
check "the internal API port is not published to the host" \
    bash -c '! docker port kingres-1 | grep -q "^8092/tcp"'

# ------------------------------------- way 1: addServer, node 2, pinned
# An administrator of the cluster (kingres_ops or better) names the new
# server's REST address and gives its setup token; the cluster's leader asks
# the new server to join.  The pin is the new server's bootstrap fingerprint
# (the logged form, with colons, is accepted).
note "kingres-2: addServer on kingres-1, fingerprint pinned"
use_node 1
add_body="{\"address\":\"kingres-2:8091\",\"setupToken\":\"$(docker exec kingres-2 cat /var/lib/kingres/setup-token)\"}"
out="$(curl -s -X POST -w $'\n%{http_code}' -H 'Content-Type: application/json' \
    -H "Authorization: Bearer ${FLOW_TOKEN}" -H "X-Kingres-Pin-Sha256: ${fp_log2}" \
    -d "${add_body}" "${API_BASE}/servers" || true)"
check_eq "addServer kingres-2 accepted" 202 "$(api_code "${out}")"
task_id="$(api_body "${out}" | sed -n 's/^{"id":\([0-9]*\),"kind":"server.add".*/\1/p')"
check "addServer answers a server.add task" test -n "${task_id}"
flow_wait_task "task server.add of kingres-2 is done" "${task_id}" 240

# ----------------------------------- way 2: joinCluster, node 3, pinned
# The new server is told where the cluster is and which administrator vouches
# for it.  The pin is the fingerprint of the existing server's cluster
# certificate.  A wrong pin is refused first: the handshake fails before the
# password is sent, and the setup token stays valid.
join_body() {
    printf '{"existingNodeAddress":"kingres-1:8091","admin":{"username":"%s","password":"%s"},"dataPaths":{"dataDirectory":"/var/lib/kingres"},"memoryQuota":{"totalMegabytes":1024}}' \
        "${FLOW_ADMIN}" "${FLOW_PASSWORD}"
}
join_request() { # join_request PIN: POST join-cluster on the node at API_BASE; prints the HTTP status
    "${API_CURL[@]}" -s -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
        -H "X-Setup-Token: $(node_token)" -H "X-Kingres-Pin-Sha256: $1" \
        -d "$(join_body)" "${API_BASE}/setup/join-cluster" || true
}
setup_state() { "${API_CURL[@]}" -s "${API_BASE}/setup/state" | sed -n 's/^{"state":"\([a-z]*\)".*/\1/p'; }
setup_task_state() {
    "${API_CURL[@]}" -s "${API_BASE}/setup/state" \
        | sed -n 's/.*"setupTask":{"id":[0-9]*,"kind":"[a-z.]*","state":"\([a-z]*\)".*/\1/p'
}

note "kingres-3: joinCluster with a wrong pin is refused"
use_node 3
token_before="$(node_token)"
code="$(join_request "$(printf '0%.0s' $(seq 1 64))")"
case "${code}" in
    202) pass "join with a wrong pin: accepted as a task (which must fail)" ;;
    4*) pass "join with a wrong pin: refused at once (${code})" ;;
    *) fail "join with a wrong pin answered ${code}" ;;
esac
wrong_pin_refused() { [ "$(setup_task_state)" = failed ] || [ "${code:0:1}" = 4 ]; }
wait_for "the join with the wrong pin failed" 60 wrong_pin_refused
check_eq "kingres-3 is still uninitialized" uninitialized "$(setup_state)"
check_eq "kingres-3 still has the same setup token" "${token_before}" "$(node_token)"

note "kingres-3: joinCluster, fingerprint of kingres-1 pinned"
check_eq "joinCluster accepted" 202 "$(join_request "${fp_node1}")"
join_done() { [ "$(setup_state)" = clustered ] && [ "$(setup_task_state)" = "done" ]; }
wait_for "kingres-3 is clustered and its join task is done" 240 join_done

# ------------------------------------------------- three servers, every node
note "three servers"
wait_servers_live "after the joins" 1 2 3
use_node 1
check_eq "only kingres-1 is the manager leader" kingres-1 \
    "$(api GET /servers | grep -o '"id":"[^"]*","address":"[^"]*"[^{}]*"isManagerLeader":true' | sed 's/^"id":"\([^"]*\)".*/\1/')"
check_eq "kingres-2: the leader recorded the pin of addServer" "true:${fp_file2}" \
    "$(api GET /servers | sed -n 's/.*"id":"kingres-2".*"seenFingerprint":"\([^"]*\)","fingerprintPinned":\([a-z]*\).*/\2:\1/p')"
for n in 1 2 3; do
    check_eq "kingres-${n}: node state" clustered \
        "$(curl -s "http://127.0.0.1:$((base + n))/api/v1/setup/state" | sed -n 's/^{"state":"\([a-z]*\)".*/\1/p')"
done

# A session lives in the memory of the server that issued it (control-plane
# README, "What has to move with the manager"): the contract does not promise
# that a token works on another server, so the token of kingres-1 is only
# reported, and the same administrator signs in again on the others.
note "the administrator on every server"
tok1="${tokens[1]}"
for n in 2 3; do
    use_node "${n}"
    FLOW_TOKEN="${tok1}"
    echo "info: the bearer token of kingres-1 on kingres-${n}: HTTP $(api_code "$(api GET /servers)")"
    signin_node "${n}"
    check_eq "kingres-${n}: listServers with its own sign-in" 200 "$(api_code "$(api GET /servers)")"
    check_eq "kingres-${n}: the session names ${FLOW_ADMIN}" "${FLOW_ADMIN}" \
        "$(api GET /auth/session | sed -n 's/.*"username":"\([^"]*\)".*/\1/p' | head -1)"
done

# ------------------------------ what existed before the joins is everywhere
# audit_rows is an ordinary table (not distributed): it lives on kingres-1 only.
# What every server has is the distributed table orders and the extensions.
shop_shared_data() { # shop_shared_data PREFIX
    check_eq "$1: row of orders (distributed)" acme "$(psql_run -d shop -Atc 'select customer from public.orders where id = 7' 2>/dev/null)"
    check_eq "$1: both extensions present" "kingres_ledger kingres_shard" \
        "$(psql_run -d shop -Atc "select string_agg(extname, ' ' order by extname) from pg_extension where extname like 'kingres%'" 2>/dev/null)"
}

note "roles and databases of kingres-1 reach the servers that joined"
for n in 2 3; do
    use_node "${n}"
    check_eq "kingres-${n}: role app_user exists" 200 "$(api_code "$(api GET /roles/app_user)")"
    check_eq "kingres-${n}: database shop exists" 200 "$(api_code "$(api GET /databases/shop)")"
    wait_for "kingres-${n}: its database accepts connections" 60 db_up
    shop_shared_data "kingres-${n}"
done

# ---------- a database created on kingres-2, a table written on kingres-3
# A cluster operation received by a follower is forwarded to the leader; the
# task is read back through the node that received the request.
note "createDatabase on kingres-2"
use_node 2
out="$(api POST /databases '{"name":"mesh","sharding":{"enabled":true,"bucketCount":32}}')"
check_eq "createDatabase mesh on kingres-2 accepted" 202 "$(api_code "${out}")"
flow_wait_task "task database.create is done (read through kingres-2)" \
    "$(api_body "${out}" | sed -n 's/^{"id":\([0-9]*\),"kind":"database.create".*/\1/p')" 120
mesh_ready() { api GET /databases/mesh | grep -Eq '"status":"ready".*"sharding":\{"enabled":true'; }
for n in 1 2 3; do
    use_node "${n}"
    wait_for "database mesh is visible, ready and sharded on kingres-${n}" 90 mesh_ready
done
mesh_bucket_counts() {
    local n c=""
    for n in 1 2 3; do
        use_node "${n}"
        c="${c}$(api GET /databases/mesh | sed -n 's/.*"sharding":{"enabled":true,"bucketCount":\([0-9a-z]*\)}.*/\1/p') "
    done
    [ "${c}" = "32 32 32 " ]
}
check "mesh reports bucketCount 32 on every node" mesh_bucket_counts

# DDL of a sharded database goes through the map group primary (kingres-1).
# The table is distributed by an API call received by kingres-3; the rows are
# written on kingres-3 and read back on every node.
note "a distributed table: created on kingres-1, distributed and written on kingres-3"
use_node 1
check "create table events on kingres-1 (the map group primary)" \
    psql_run -d mesh -v ON_ERROR_STOP=1 -c 'create table public.events (id bigint primary key, note text)'
use_node 3
out="$(api PUT /databases/mesh/schemas/public/tables/events/distribution '{"method":"hash","distributionColumn":"id"}')"
check_eq "distribute events through kingres-3" 202 "$(api_code "${out}")"
flow_wait_task "task table.distribute is done (read through kingres-3)" \
    "$(api_body "${out}" | sed -n 's/^{"id":\([0-9]*\),"kind":"table.distribute".*/\1/p')" 120
check_eq "20 rows written through SQL on kingres-3" 20 "$(insert_events 1 20)"
for n in 1 2 3; do
    use_node "${n}"
    check_eq "kingres-${n}: count(*) of events" 20 "$(event_count)"
done

# ------------------------------------ a non-leader node restarts (kingres-2)
note "docker compose restart of kingres-2 (not the leader)"
docker compose -f "${compose}" restart kingres-2 >/dev/null 2>&1
wait_for "kingres-2 healthy after the restart" 120 node_up 2
wait_servers_live "after the restart of kingres-2" 1 2 3
use_node 2
wait_for "kingres-2: its database answers" 60 db_up
wait_for "kingres-2: reads the 20 rows" 60 event_count_is 20

note "kingres-2 stopped, a row written on kingres-3, kingres-2 started"
docker compose -f "${compose}" stop kingres-2 >/dev/null 2>&1
use_node 3
# A row whose bucket lives on the stopped server is refused; the others are
# accepted.  What is accepted must be there when kingres-2 is back.
written="$(insert_events 21 30)"
echo "info: ${written} of 10 rows were accepted while kingres-2 was down"
check "at least one row was written while kingres-2 was down" test "${written}" -ge 1
# The contract says a server without liveness for the failure timeout is
# "dead": the servers exchange heartbeats, and a stopped follower is no
# longer listed as live by the others.
down_seen() { ! servers_line | grep -Eq 'kingres-2=(live|pendingRebalance)'; }
down_seen_within() { local i; for i in $(seq 1 "$1"); do down_seen && return 0; sleep 1; done; return 1; }
signin_node 1
check "kingres-1 stops listing the stopped kingres-2 as healthy" down_seen_within 30
docker compose -f "${compose}" start kingres-2 >/dev/null 2>&1
wait_for "kingres-2 healthy after it was started" 120 node_up 2
wait_servers_live "after kingres-2 came back" 1 2 3
total=$((20 + written))
for n in 2 1 3; do
    use_node "${n}"
    wait_for "kingres-${n}: its database answers" 60 db_up
    wait_for "kingres-${n}: count(*) of events is ${total} (rows written while kingres-2 was down)" 60 event_count_is "${total}"
done

# ------------------------------------------ stop and start the whole cluster
note "stop and start all nodes (compose stop honors the 120 s grace period)"
start=$SECONDS
docker compose -f "${compose}" stop >/dev/null 2>&1
echo "stop time: $((SECONDS - start)) s"
check_eq "all containers exited with code 0" "0 0 0" "$(for n in 1 2 3; do docker inspect --format '{{.State.ExitCode}}' "kingres-${n}"; done | tr '\n' ' ' | sed 's/ $//')"
docker compose -f "${compose}" start >/dev/null 2>&1
for n in 1 2 3; do wait_for "kingres-${n} healthy again" 90 node_up "${n}"; done
for n in 1 2 3; do
    use_node "${n}"
    wait_for "kingres-${n}: its database is back" 60 db_up
done
wait_servers_live "after the stop and start of all nodes" 1 2 3
use_node 1
flow_check_api "kingres-1 after stop/start"
flow_check_data "kingres-1 after stop/start"
for n in 1 2 3; do
    use_node "${n}"
    shop_shared_data "kingres-${n} after stop/start"
    wait_for "kingres-${n}: count(*) of events is ${total} after stop/start" 60 event_count_is "${total}"
done
check "no node needed crash recovery" bash -c '
    for n in 1 2 3; do
        docker exec "kingres-$n" sh -c "cat /var/lib/kingres/instances/*/pgdata/log/*" | grep -qi "automatic recovery in progress" && exit 1
    done; exit 0'

note "network"
check "kingres-1 resolves kingres-2 and kingres-3 by name" \
    bash -c 'docker exec kingres-1 getent hosts kingres-2 && docker exec kingres-1 getent hosts kingres-3'
check_eq "all nodes on the network kingres-net" 3 \
    "$(docker network inspect kingres-net --format '{{len .Containers}}')"
check "database ports are published on the host loopback" bash -c '
    for n in 1 2 3; do docker port "kingres-$n" 5432/tcp | grep -q "^127.0.0.1:$((${0} + 10 + n))$" || exit 1; done' "${base}"
check "ports are bound to loopback only" \
    bash -c '! docker ps --format "{{.Ports}}" --filter name=kingres- | grep -q "0.0.0.0"'

note "tear down"
docker compose -f "${compose}" down --volumes --remove-orphans >/dev/null 2>&1
check_eq "no containers left" 0 "$(docker ps -aq --filter name=kingres- | wc -l)"
check_eq "no volumes left" 0 "$(docker volume ls -q --filter name=kingres-.-data | wc -l)"

summary

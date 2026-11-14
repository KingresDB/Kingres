#!/usr/bin/env bash
# SPDX-License-Identifier: PostgreSQL
# Copyright (c) 2026, RooyeKhat Media Co
#
# kingres/packaging/test/test-compose.sh
#
# Test the three-node development cluster of docker/compose.yaml: the nodes
# start from a clean snapshot, become healthy, each reports "uninitialized"
# on its own published port, each has a different setup token (read with
# tokens.sh; the tokens are not printed), the nodes reach each other by name
# on the shared network, and "down --volumes" removes everything it created.
#
# Usage:
#   test-compose.sh [--context DIR] [--port-base N]
#
#   --context DIR  build context (default: a snapshot of HEAD plus the
#                  working-tree kingres/packaging directory)
#   --port-base N  host ports N+1, N+2, N+3 (default 58100)

# shellcheck disable=SC2016
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "${here}/lib.sh"
# shellcheck source=flow.sh
. "${here}/flow.sh"

context=""
base=58100
while [ "$#" -gt 0 ]; do
    case "$1" in
        --context)   context="$2"; shift 2 ;;
        --port-base) base="$2"; shift 2 ;;
        *) echo "test-compose.sh: unknown argument '$1'" >&2; exit 2 ;;
    esac
done

tmp="$(mktemp -d)"
if [ -z "${context}" ]; then
    context="${tmp}/context"
    "${here}/../build/snapshot.sh" "${context}" --overlay kingres/packaging
fi
compose="${context}/kingres/packaging/docker/compose.yaml"
export KINGRES_BUILD_CONTEXT="${context}"
export KINGRES_IMAGE=kingres:compose-test
export KINGRES_PANEL_PORT_1=$((base + 1)) KINGRES_PANEL_PORT_2=$((base + 2)) KINGRES_PANEL_PORT_3=$((base + 3))
export KINGRES_PG_PORT_1=$((base + 11)) KINGRES_PG_PORT_2=$((base + 12)) KINGRES_PG_PORT_3=$((base + 13))

cleanup() {
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

# ------------------------------------------- each node becomes its own cluster
# Every node is initialized as an independent one-server cluster.  Joining the
# nodes into one cluster is not exercised here yet.
node_flow() { # node_flow N FULL(1|0)
    local n="$1" full="$2" cn="kingres-$1"
    API_CURL=(curl)
    API_BASE="http://127.0.0.1:$((base + n))/api/v1"
    DATA_DIR=/var/lib/kingres
    NODE="${cn}"
    TOKEN_CMD=node_token
    PSQL_CMD=(docker exec -e "PGPASSWORD=${FLOW_PASSWORD}" "${cn}" psql -X -h 127.0.0.1 -p 5432 -U "${FLOW_ADMIN}")
    note "${cn}: create a cluster"
    flow_init_cluster "node-${n}"
    flow_login
    if [ "${full}" = 1 ]; then
        flow_create_data
        flow_check_data "${cn}"
    else
        check "${cn}: database answers" bash -c '"$@" -d postgres -Atc "select 1" | grep -q 1' _ "${PSQL_CMD[@]}"
        check_eq "${cn}: cluster name" "node-${n}" "$(api GET /cluster | sed -n 's/.*"name":"\([^"]*\)".*/\1/p' | head -1)"
    fi
}
node_token() { docker exec "${NODE}" cat /var/lib/kingres/setup-token; }

node_flow 1 1
node_flow 2 0
node_flow 3 0

note "database ports and the network"
for n in 1 2 3; do
    check "kingres-${n}: database port published on the host loopback" \
        bash -c 'docker port "$1" 5432/tcp | grep -q "^127.0.0.1:$2$"' _ "kingres-${n}" "$((base + 10 + n))"
done
echo "info: login from kingres-2 to the database of kingres-1 over the network:"
docker exec -e "PGPASSWORD=${FLOW_PASSWORD}" kingres-2 psql -X -h kingres-1 -p 5432 -U "${FLOW_ADMIN}" -d postgres -Atc 'select 1' 2>&1 | head -2 || true

note "stop and start all nodes (compose stop honors the 120 s grace period)"
start=$SECONDS
docker compose -f "${compose}" stop >/dev/null 2>&1
echo "stop time: $((SECONDS - start)) s"
check_eq "all containers exited with code 0" "0 0 0" "$(for n in 1 2 3; do docker inspect --format '{{.State.ExitCode}}' "kingres-${n}"; done | tr '\n' ' ' | sed 's/ $//')"
docker compose -f "${compose}" start >/dev/null 2>&1
node_up() { [ "$(docker inspect --format '{{.State.Health.Status}}' "kingres-$1")" = healthy ]; }
for n in 1 2 3; do wait_for "kingres-${n} healthy again" 90 node_up "${n}"; done
node_flow_again() {
    API_BASE="http://127.0.0.1:$((base + 1))/api/v1"
    PSQL_CMD=(docker exec -e "PGPASSWORD=${FLOW_PASSWORD}" kingres-1 psql -X -h 127.0.0.1 -p 5432 -U "${FLOW_ADMIN}")
    db_up() { psql_run -d postgres -Atc 'select 1' >/dev/null 2>&1; }
    wait_for "kingres-1 database is back" 60 db_up
    flow_check_api "kingres-1 after stop/start"
    flow_check_data "kingres-1 after stop/start"
}
node_flow_again
check "no node needed crash recovery" bash -c '
    for n in 1 2 3; do
        docker exec "kingres-$n" sh -c "cat /var/lib/kingres/instances/1/pgdata/log/*" | grep -qi "automatic recovery in progress" && exit 1
    done; exit 0'

note "network"
check "kingres-1 resolves kingres-2 and kingres-3 by name" \
    bash -c 'docker exec kingres-1 getent hosts kingres-2 && docker exec kingres-1 getent hosts kingres-3'
check_eq "all nodes on the network kingres-net" 3 \
    "$(docker network inspect kingres-net --format '{{len .Containers}}')"
check "ports are bound to loopback only" \
    bash -c '! docker ps --format "{{.Ports}}" --filter name=kingres- | grep -q "0.0.0.0"'

note "tear down"
docker compose -f "${compose}" down --volumes --remove-orphans >/dev/null 2>&1
check_eq "no containers left" 0 "$(docker ps -aq --filter name=kingres- | wc -l)"
check_eq "no volumes left" 0 "$(docker volume ls -q --filter name=kingres-.-data | wc -l)"

summary

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

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "${here}/lib.sh"

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
        "$(printf '%s' "${state}" | sed -n 's/.*"state":"\([a-z]*\)".*/\1/p')"
    check_eq "kingres-${n} hostname" "kingres-${n}" "$(docker exec "kingres-${n}" hostname)"
done

note "tokens"
"${here}/../docker/tokens.sh" >"${tmp}/tokens.txt"
check_eq "tokens.sh lists three tokens" 3 "$(grep -c 'setup token: ' "${tmp}/tokens.txt")"
check_eq "the three tokens are different" 3 "$(sed -n 's/.*setup token: //p' "${tmp}/tokens.txt" | sort -u | wc -l)"

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

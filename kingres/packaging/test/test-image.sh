#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (c) 2026, RooyeKhat Media Co
#
# kingres/packaging/test/test-image.sh
#
# Build the container image from a clean snapshot of the repository and test
# it:
#   - the image builds (the multi-stage build runs the release recipe);
#   - a container becomes healthy and reports the node state "uninitialized";
#   - the panel's index page and favicon are served;
#   - the container runs as a non-root user, with a read-only root file system
#     and no capabilities;
#   - the setup token is written to the volume, readable only by the service
#     user, announced in the log without being printed, and survives a restart;
#   - the server binary reports the KingresDB version (initdb, start,
#     SELECT version(), CREATE EXTENSION of both extensions);
#   - no secret is baked into the image.
#
# Usage:
#   test-image.sh [--context DIR] [--port PORT] [--tag TAG] [--keep]
#
#   --context DIR  build context (a clean snapshot).  Default: a snapshot of
#                  HEAD plus the working-tree kingres/packaging directory
#                  (so that packaging work that is not committed yet is built).
#   --port PORT    host port for the panel (default 58001, bound to 127.0.0.1)
#   --tag TAG      image tag (default kingres:test)
#   --keep         leave the container and the volume behind
#
# Needs docker (the user must be in the docker group) and curl.

# The test commands are single-quoted on purpose: they run inside the container.
# shellcheck disable=SC2016
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "${here}/lib.sh"
# shellcheck source=flow.sh
. "${here}/flow.sh"

context=""
port=58001
tag=kingres:test
keep=0
while [ "$#" -gt 0 ]; do
    case "$1" in
        --context) context="$2"; shift 2 ;;
        --port)    port="$2"; shift 2 ;;
        --tag)     tag="$2"; shift 2 ;;
        --keep)    keep=1; shift ;;
        *) echo "test-image.sh: unknown argument '$1'" >&2; exit 2 ;;
    esac
done

name="kingres-image-test"
volume="kingres-image-test-data"
tmp="$(mktemp -d)"

cleanup() {
    if [ "${keep}" -eq 0 ]; then
        docker rm -f "${name}" >/dev/null 2>&1 || true
        docker volume rm -f "${volume}" >/dev/null 2>&1 || true
    fi
    rm -rf "${tmp}"
}
trap cleanup EXIT
docker rm -f "${name}" >/dev/null 2>&1 || true
docker volume rm -f "${volume}" >/dev/null 2>&1 || true

wait_healthy() {
    local h=starting
    for _ in $(seq 1 90); do
        h="$(docker inspect --format '{{.State.Health.Status}}' "${name}")"
        [ "${h}" = healthy ] && break
        sleep 1
    done
    printf '%s' "${h}"
}

# ------------------------------------------------------------------ build
if [ -z "${context}" ]; then
    context="${tmp}/context"
    "${here}/../build/snapshot.sh" "${context}" --overlay kingres/packaging
fi
commit="$(cat "${context}/.kingres-commit" 2>/dev/null || echo unknown)"

note "building the image from ${commit}"
start=$SECONDS
docker build --file "${context}/kingres/packaging/docker/Dockerfile" \
    --build-arg "REVISION=${commit}" --tag "${tag}" "${context}"
echo "image build time: $((SECONDS - start)) s"
echo "image size: $(docker image inspect "${tag}" --format '{{.Size}}') bytes"

# ------------------------------------------------------------------- run
note "starting the container"
docker run --detach --name "${name}" --read-only --tmpfs /tmp --stop-timeout 120 \
    --cap-drop ALL --security-opt no-new-privileges \
    --publish "127.0.0.1:${port}:8091" --volume "${volume}:/var/lib/kingres" \
    "${tag}" >/dev/null

check_eq "container health" healthy "$(wait_healthy)"

base="http://127.0.0.1:${port}"
state="$(curl -fsS "${base}/api/v1/setup/state" || true)"
check_eq "setup state" uninitialized "$(printf '%s' "${state}" | sed -n 's/^{"state":"\([a-z]*\)".*/\1/p')"
check_eq "readiness endpoint" 200 "$(curl -s -o /dev/null -w '%{http_code}' "${base}/api/v1/readyz")"
index="$(curl -fsS "${base}/" || true)"
check "index page is the panel (HTML with a script)" grep -q '<script' <<<"${index}"
check_eq "favicon status" 200 "$(curl -s -o /dev/null -w '%{http_code}' "${base}/favicon.svg")"
check_eq "favicon type" image/svg+xml "$(curl -s -o /dev/null -w '%{content_type}' "${base}/favicon.svg" | sed 's/;.*//')"
check_eq "unknown API path is a problem, not HTML" 404 "$(curl -s -o /dev/null -w '%{http_code}' "${base}/api/v1/nope")"

note "identity and files"
check_eq "process user id" 10001 "$(docker exec "${name}" id -u)"
check_eq "setup token mode and owner" "600 kingres" \
    "$(docker exec "${name}" stat -c '%a %U' /var/lib/kingres/setup-token)"
check_eq "data directory mode and owner" "750 kingres" "$(docker exec "${name}" stat -c '%a %U' /var/lib/kingres)"
logs="$(docker logs "${name}" 2>&1)"
check "log says where the token is" grep -q 'setup-token' <<<"${logs}"
token="$(docker exec "${name}" cat /var/lib/kingres/setup-token)"
check "log does not contain the token" bash -c '! grep -qF -- "$1" <<<"$2"' _ "${token}" "${logs}"
writable="$(docker exec "${name}" find /opt/kingres /var/lib/kingres -perm -o+w ! -type l | head -1)"
check_eq "no world-writable path in the tree and the volume" "" "${writable}"

note "restart keeps the node state"
start=$SECONDS
docker restart "${name}" >/dev/null
echo "restart time: $((SECONDS - start)) s"
check_eq "healthy again after restart" healthy "$(wait_healthy)"
check "same setup token after restart" test "${token}" = "$(docker exec "${name}" cat /var/lib/kingres/setup-token)"

note "the server binary and the extensions (as the image's own user)"
version_out="$(docker run --rm --read-only --tmpfs /tmp --cap-drop ALL \
    --volume "${here}/pg-smoke.sh:/pg-smoke.sh:ro" --entrypoint bash "${tag}" /pg-smoke.sh /tmp/smoke 2>&1 || true)"
echo "${version_out}"
check "version() says KingresDB" grep -q 'KingresDB 0\.' <<<"${version_out}"
check "extension kingres_shard created" grep -q 'extension kingres_shard' <<<"${version_out}"
check "extension kingres_ledger created" grep -q 'extension kingres_ledger' <<<"${version_out}"
check "kingres-verify runs" docker run --rm --entrypoint kingres-verify "${tag}" --help
check_eq "kingresd reports its version" "0.1.0" \
    "$(curl -fsS "${base}/api/v1/setup/state" | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')"

note "no secrets baked into the image"
env_dump="$(docker image inspect "${tag}" --format '{{json .Config.Env}}')"
check "image environment holds no password or token" bash -c '! grep -Eiq "password|secret|token=" <<<"$1"' _ "${env_dump}"
check "image contains no setup token" bash -c '! docker run --rm --entrypoint sh "$1" -c "test -e /var/lib/kingres/setup-token"' _ "${tag}"

# ----------------------------------------------------- the real thing: a cluster
# The panel's port is published, so these requests reach kingresd from the
# Docker bridge address, not from loopback: this also proves that
# KINGRES_ALLOW_REMOTE_SETUP=true works.
API_CURL=(curl)
API_BASE="${base}/api/v1"
DATA_DIR=/var/lib/kingres
TOKEN_CMD=read_token
read_token() { docker exec "${name}" cat /var/lib/kingres/setup-token; }
PSQL_CMD=(docker exec -e "PGPASSWORD=${FLOW_PASSWORD}" "${name}" psql -X -h 127.0.0.1 -p 5432 -U "${FLOW_ADMIN}")
db_up() { psql_run -d postgres -Atc 'select 1' >/dev/null 2>&1; }
postmaster_pid() { docker exec "${name}" head -1 /var/lib/kingres/instances/1/pgdata/postmaster.pid 2>/dev/null || echo none; }
# PIDs repeat in a fresh container, so the start time (line 3) identifies a postmaster.
postmaster_start() { docker exec "${name}" sed -n 3p /var/lib/kingres/instances/1/pgdata/postmaster.pid 2>/dev/null || echo none; }
volume_logs() { docker run --rm --entrypoint sh --volume "${volume}:/var/lib/kingres:ro" "${tag}" -c 'cat /var/lib/kingres/instances/1/pgdata/log/* 2>/dev/null'; }
wait_api() { wait_for "API answers" 60 curl -fsS "${base}/api/v1/readyz"; }

note "create a cluster through the API"
flow_init_cluster image-test
check "setup token removed after setup" bash -c '! docker exec "$1" test -e /var/lib/kingres/setup-token' _ "${name}"
check_eq "container stays healthy" healthy "$(wait_healthy)"
flow_login
check_eq "instance process user id" 10001 "$(docker exec "${name}" sh -c 'ps -o uid= -C postgres | sort -u | tr -d " "')"
check "instance socket is in the volume" docker exec "${name}" test -S /var/lib/kingres/run/1/.s.PGSQL.5432
flow_create_data
flow_check_data "after setup"
pm1="$(postmaster_pid)"

note "docker stop: clean shutdown within the grace period"
start=$SECONDS
docker stop "${name}" >/dev/null
stop_time=$((SECONDS - start))
echo "stop time: ${stop_time} s"
check "stopped within the grace period (120 s)" test "${stop_time}" -lt 120
check_eq "exit code after docker stop" 0 "$(docker inspect --format '{{.State.ExitCode}}' "${name}")"
logs="$(volume_logs)"
check "instance log: database system is shut down" grep -q 'database system is shut down' <<<"${logs}"
check "no postmaster.pid left in the volume" bash -c '! docker run --rm --entrypoint sh --volume "$1:/v:ro" "$2" -c "test -e /v/instances/1/pgdata/postmaster.pid"' _ "${volume}" "${tag}"

note "docker start: the same database comes back"
docker start "${name}" >/dev/null
check_eq "healthy again" healthy "$(wait_healthy)"
wait_for "database is back" 60 db_up
check_eq "node state: no wizard" clustered "$(curl -fsS "${base}/api/v1/setup/state" | sed -n 's/^{"state":"\([a-z]*\)".*/\1/p')"
flow_check_api "after docker start"
flow_check_data "after docker start"
logs="$(volume_logs)"
check "no crash recovery was needed" bash -c '! grep -qi "automatic recovery in progress\|not properly shut down" <<<"$1"' _ "${logs}"

note "docker restart"
pm2="$(postmaster_start)"
start=$SECONDS
docker restart "${name}" >/dev/null
echo "restart time: $((SECONDS - start)) s"
check_eq "healthy again" healthy "$(wait_healthy)"
wait_for "database is back" 60 db_up
check "postmaster was restarted (start time ${pm2} -> $(postmaster_start))" test "$(postmaster_start)" != "${pm2}"
flow_check_api "after docker restart"
flow_check_data "after docker restart"
logs="$(volume_logs)"
check "still no crash recovery" bash -c '! grep -qi "automatic recovery in progress\|not properly shut down" <<<"$1"' _ "${logs}"
: "${pm1}"

note "a new container on the same volume"
docker stop "${name}" >/dev/null
docker rm "${name}" >/dev/null
docker run --detach --name "${name}" --read-only --tmpfs /tmp --stop-timeout 120 \
    --cap-drop ALL --security-opt no-new-privileges \
    --publish "127.0.0.1:${port}:8091" --volume "${volume}:/var/lib/kingres" \
    "${tag}" >/dev/null
check_eq "healthy" healthy "$(wait_healthy)"
wait_for "database is back" 60 db_up
check_eq "node state: no wizard, no new token" clustered "$(curl -fsS "${base}/api/v1/setup/state" | sed -n 's/^{"state":"\([a-z]*\)".*/\1/p')"
flow_check_api "new container"
flow_check_data "new container"
logs="$(volume_logs)"
check "no crash recovery after removing the container with the database running" \
    bash -c '! grep -qi "automatic recovery in progress\|not properly shut down" <<<"$1"' _ "${logs}"
docker stop "${name}" >/dev/null

summary

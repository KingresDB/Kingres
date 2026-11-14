#!/usr/bin/env bash
# SPDX-License-Identifier: PostgreSQL
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
docker run --detach --name "${name}" --read-only --tmpfs /tmp \
    --cap-drop ALL --security-opt no-new-privileges \
    --publish "127.0.0.1:${port}:8091" --volume "${volume}:/var/lib/kingres" \
    "${tag}" >/dev/null

check_eq "container health" healthy "$(wait_healthy)"

base="http://127.0.0.1:${port}"
state="$(curl -fsS "${base}/api/v1/setup/state" || true)"
check_eq "setup state" uninitialized "$(printf '%s' "${state}" | sed -n 's/.*"state":"\([a-z]*\)".*/\1/p')"
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

note "graceful stop"
start=$SECONDS
docker stop "${name}" >/dev/null
echo "stop time: $((SECONDS - start)) s"
check_eq "exit code after docker stop" 0 "$(docker inspect --format '{{.State.ExitCode}}' "${name}")"

summary

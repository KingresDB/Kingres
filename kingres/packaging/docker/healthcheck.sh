#!/bin/bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (c) 2026, RooyeKhat Media Co
#
# kingres-healthcheck: container health check of the KingresDB image.
#
# Asks the readiness endpoint of the local kingresd (GET /api/v1/readyz) and
# succeeds on HTTP 200.  An uninitialized node is ready (it serves the setup
# wizard), so a fresh container is healthy.  Uses bash's /dev/tcp so that the
# image needs no curl.
#
# Usage: kingres-healthcheck        (port taken from $KINGRES_LISTEN)

set -euo pipefail

listen="${KINGRES_LISTEN:-127.0.0.1:8091}"
port="${listen##*:}"

exec 3<>"/dev/tcp/127.0.0.1/${port}"
printf 'GET /api/v1/readyz HTTP/1.0\r\nHost: localhost\r\nConnection: close\r\n\r\n' >&3
IFS= read -r -t 3 status <&3 || { echo "no answer from kingresd" >&2; exit 1; }
case "${status}" in
    "HTTP/1.0 200"* | "HTTP/1.1 200"*) exit 0 ;;
    *) printf 'not ready: %s\n' "${status}" >&2; exit 1 ;;
esac

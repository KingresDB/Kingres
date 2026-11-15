#!/usr/bin/env bash
# SPDX-License-Identifier: PostgreSQL
# Copyright (c) 2026, RooyeKhat Media Co
#
# kingres/packaging/docker/tokens.sh
#
# Print the one-time setup token and the panel address of every node of the
# development cluster (compose.yaml).  A node that is no longer uninitialized
# has no token any more.  The tokens are secrets: run this on your own
# terminal, do not paste its output into tickets or logs.
#
# Usage: tokens.sh [NODE...]       default nodes: kingres-1 kingres-2 kingres-3
#
# Environment: KINGRES_PANEL_PORT_1/2/3 and KINGRES_PANEL_BIND as in
# compose.yaml, to print the right addresses.

set -euo pipefail

if [ "$#" -gt 0 ]; then
    nodes=("$@")
else
    nodes=(kingres-1 kingres-2 kingres-3)
fi

bind="${KINGRES_PANEL_BIND:-127.0.0.1}"
for node in "${nodes[@]}"; do
    n="${node##*-}"
    case "${n}" in
        1) port="${KINGRES_PANEL_PORT_1:-8091}" ;;
        2) port="${KINGRES_PANEL_PORT_2:-8092}" ;;
        3) port="${KINGRES_PANEL_PORT_3:-8093}" ;;
        *) port="?" ;;
    esac
    if ! docker inspect --type container "${node}" >/dev/null 2>&1; then
        printf '%s: container not found (docker compose up -d)\n' "${node}"
        continue
    fi
    if token="$(docker exec "${node}" cat /var/lib/kingres/setup-token 2>/dev/null)"; then
        printf '%s  panel http://%s:%s/  setup token: %s\n' "${node}" "${bind}" "${port}" "${token}"
    else
        printf '%s  panel http://%s:%s/  no setup token (node is not uninitialized)\n' \
            "${node}" "${bind}" "${port}"
    fi
done

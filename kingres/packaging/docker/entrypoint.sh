#!/bin/sh
# SPDX-License-Identifier: PostgreSQL
# Copyright (c) 2026, RooyeKhat Media Co
#
# Entry point of the KingresDB image: say where the first-run setup token
# will be, then replace this shell with kingresd (arguments are passed on).
# The token itself is never printed here or logged by kingresd.

set -eu

data_dir="${KINGRES_DATA_DIR:-/var/lib/kingres}"
cat >&2 <<EOF
KingresDB node starting.  Panel and API: port ${KINGRES_LISTEN##*:}.
If this node is uninitialized, the one-time setup token is written to
  ${data_dir}/setup-token   (readable only by the service user)
Read it with:   docker exec $(hostname) cat ${data_dir}/setup-token
EOF
exec /opt/kingres/bin/kingresd "$@"

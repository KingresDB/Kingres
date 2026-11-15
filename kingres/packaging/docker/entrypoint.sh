#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (c) 2026, RooyeKhat Media Co
#
# Entry point of the KingresDB image (run by tini as the main process).
#
# kingresd starts the database instance as an independent process and leaves
# it running when kingresd exits.  A container has no service manager that
# stops it afterwards, so this script plays that role:
#   - it starts kingresd and forwards SIGTERM/SIGINT to it;
#   - when kingresd has exited (after a signal or by itself), it runs
#     "kingresd -stop-instances", which shuts the instance down with a fast
#     shutdown, and only then exits with kingresd's exit status.
# kingresd no longer renews the group lease from its exit until the instance is
# down, and a fast shutdown can take longer than the lease.  "-stop-instances"
# therefore tells the lease watchdog (which outlives kingresd) to stand down as
# soon as the shutdown has been requested; a kingresd that hangs or is killed
# never does, so the watchdog still covers that case.
# "docker stop" must therefore be given enough time (compose.yaml sets
# stop_grace_period: 120s; with docker run use --stop-timeout 120).
#
# It also says where the one-time setup token is.  The token itself is never
# printed here or logged by kingresd.

set -eu

data_dir="${KINGRES_DATA_DIR:-/var/lib/kingres}"
cat >&2 <<EOF
KingresDB node starting.  Panel and API: port ${KINGRES_LISTEN##*:}.
If this node is uninitialized, the one-time setup token is written to
  ${data_dir}/setup-token   (readable only by the service user)
Read it with:   docker exec $(hostname) cat ${data_dir}/setup-token
EOF

/opt/kingres/bin/kingresd "$@" &
child=$!

stopping=0
# shellcheck disable=SC2317 # called through the trap
forward() {
    stopping=1
    kill -TERM "${child}" 2>/dev/null || true
}
trap forward TERM INT

status=0
# "wait" returns early (status above 128) when a trapped signal arrives; wait
# again until kingresd is really gone.
while kill -0 "${child}" 2>/dev/null; do
    wait "${child}" || status=$?
done
[ "${stopping}" -eq 0 ] || status=0

echo "kingresd exited; stopping the database instance" >&2
/opt/kingres/bin/kingresd "$@" -stop-instances || echo "stopping the instances failed" >&2
exit "${status}"

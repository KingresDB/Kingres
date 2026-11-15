#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (c) 2026, RooyeKhat Media Co
#
# Runs inside WSL2 (user kingres). Starts and stops a private kingresd for the end-to-end test.
# Everything lives under ~/kw-web; nothing else in the home directory is touched.
#
#   backend.sh prepare <kingresd-binary>   copy the installed server and the binary (once)
#   backend.sh start                        fresh data directory, start kingresd, wait for /healthz
#   backend.sh restart                      stop and start kingresd again on the same data (sessions are lost)
#   backend.sh token                        print the one-time setup token
#   backend.sh psql <database> <sql>        run SQL as the cluster administrator (needs E2E_ADMIN_PASSWORD)
#   backend.sh log                          print the kingresd log
#   backend.sh stop [--remove-install]      stop the instances and kingresd, remove the data
set -u
ROOT="$HOME/kw-web"
INST="$ROOT/install"
DATA="$ROOT/data"
API_PORT="${E2E_API_PORT:-58201}"
PG_PORT="${E2E_PG_PORT:-58200}"
export LD_LIBRARY_PATH="$INST/lib/x86_64-linux-gnu"

cmd="${1:-}"; shift || true
case "$cmd" in
  prepare)
    mkdir -p "$ROOT"
    # The PostgreSQL installation to copy: the control plane's own (it carries the current extensions,
    # kingres_ledger 0.2 among them) when it exists, else the first one (E2E_PG_INSTALL overrides both).
    SRC="${E2E_PG_INSTALL:-}"
    if [ -z "$SRC" ]; then
      if [ -f "$HOME/kw-cp/install/share/postgresql/extension/kingres_ledger--0.2.sql" ]; then SRC="$HOME/kw-cp/install"; else SRC="$HOME/kingres-work/install"; fi
    fi
    # A kept copy is replaced when its source changed (the extension version decides what the server can do).
    if [ -d "$INST" ] && ! cmp -s "$SRC/extensions-commit.txt" "$INST/extensions-commit.txt" 2>/dev/null; then rm -rf "$INST"; fi
    if [ ! -d "$INST" ]; then cp -a "$SRC" "$INST"; fi
    cp "$1" "$ROOT/kingresd" && chmod +x "$ROOT/kingresd"
    "$ROOT/kingresd" -version
    ;;
  start)
    # A kingresd of an earlier run that was kept (E2E_KEEP_RUNNING) would still hold the port.
    pkill -f "$ROOT/kingresd -listen" 2>/dev/null && sleep 2
    "$ROOT/kingresd" -pg-bin-dir "$INST/bin" -data-dir "$DATA" -stop-instances >/dev/null 2>&1 || true
    rm -rf "$DATA"; mkdir -p "$DATA"
    nohup "$ROOT/kingresd" -listen "127.0.0.1:$API_PORT" -data-dir "$DATA" -pg-bin-dir "$INST/bin" \
      -pg-port "$PG_PORT" -server-host 127.0.0.1 -internal-listen "127.0.0.1:$((API_PORT + 1))" -log-format json >"$ROOT/kingresd.log" 2>&1 &
    echo $! >"$ROOT/kingresd.pid"
    for _ in $(seq 1 100); do
      curl -sf "http://127.0.0.1:$API_PORT/api/v1/healthz" >/dev/null && { echo "kingresd is up on $API_PORT"; exit 0; }
      sleep 0.2
    done
    echo "kingresd did not start" >&2; tail -20 "$ROOT/kingresd.log" >&2; exit 1
    ;;
  restart)
    old="$(cat "$ROOT/kingresd.pid")"
    kill "$old" 2>/dev/null || true
    # kingresd shuts down gracefully (up to its shutdown timeout); wait until the port is free.
    for _ in $(seq 1 150); do kill -0 "$old" 2>/dev/null || break; sleep 0.2; done
    nohup "$ROOT/kingresd" -listen "127.0.0.1:$API_PORT" -data-dir "$DATA" -pg-bin-dir "$INST/bin" \
      -pg-port "$PG_PORT" -server-host 127.0.0.1 -internal-listen "127.0.0.1:$((API_PORT + 1))" -log-format json >>"$ROOT/kingresd.log" 2>&1 &
    echo $! >"$ROOT/kingresd.pid"
    for _ in $(seq 1 100); do
      curl -sf "http://127.0.0.1:$API_PORT/api/v1/healthz" >/dev/null && { echo "kingresd restarted"; exit 0; }
      sleep 0.2
    done
    echo "kingresd did not restart" >&2; exit 1
    ;;
  token) cat "$DATA/setup-token" ;;
  psql)
    PGPASSWORD="${E2E_ADMIN_PASSWORD:?E2E_ADMIN_PASSWORD is not set}" "$INST/bin/psql" -X -A -t -h 127.0.0.1 \
      -p "$PG_PORT" -U "${E2E_ADMIN_USER:-root_admin}" -d "$1" -c "$2"
    ;;
  log) cat "$ROOT/kingresd.log" ;;
  stop)
    if [ -x "$ROOT/kingresd" ]; then "$ROOT/kingresd" -pg-bin-dir "$INST/bin" -data-dir "$DATA" -stop-instances >/dev/null 2>&1 || true; fi
    if [ -f "$ROOT/kingresd.pid" ]; then kill "$(cat "$ROOT/kingresd.pid")" 2>/dev/null || true; fi
    sleep 1
    # Last resort for a hung instance (immediate shutdown), only inside our own data directory.
    for pgdata in "$DATA"/instances/*/pgdata; do
      [ -d "$pgdata" ] && "$INST/bin/pg_ctl" -D "$pgdata" -m immediate stop >/dev/null 2>&1
    done
    rm -rf "$DATA" "$ROOT/kingresd.pid"
    if [ "${1:-}" = "--remove-install" ]; then rm -rf "$ROOT"; fi
    echo "stopped"
    ;;
  *) echo "usage: backend.sh prepare|start|token|psql|log|stop" >&2; exit 2 ;;
esac

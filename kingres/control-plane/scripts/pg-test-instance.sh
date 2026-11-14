#!/usr/bin/env bash
# SPDX-License-Identifier: PostgreSQL
#
# Starts or stops the throwaway PostgreSQL instances used by the integration
# tests of kingresd (a primary and a streaming standby, SCRAM authentication,
# reachable from Windows through WSL2 localhost forwarding).
#
#   pg-test-instance.sh start [primary-port] [standby-port]
#   pg-test-instance.sh stop  [primary-port]
#
# Everything lives under $KINGRES_CP_TEST_ROOT (default
# ~/kingres-work/cp-test/<primary-port>) and is deleted by "stop". The ports
# must be in the range 56000-56099.
set -euo pipefail

BIN="${KINGRES_PG_BIN:-$HOME/kingres-work/install/bin}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cmd="${1:?usage: $0 start|stop [primary-port] [standby-port]}"
P1="${2:-56010}"
P2="${3:-$((P1 + 1))}"
ROOT="${KINGRES_CP_TEST_ROOT:-$HOME/kingres-work/cp-test}/$P1"
export LD_LIBRARY_PATH="$BIN/../lib/x86_64-linux-gnu:$BIN/../lib:${LD_LIBRARY_PATH:-}"

check_port() {
  if [ "$1" -lt 56000 ] || [ "$1" -gt 56099 ]; then
    echo "port $1 is outside 56000-56099" >&2
    exit 2
  fi
}
check_port "$P1"
check_port "$P2"

psql_local() { "$BIN/psql" -X -q -v ON_ERROR_STOP=1 -h "$ROOT/sock" -p "$P1" -U kingres "$@"; }

case "$cmd" in
start)
  if [ -e "$ROOT" ]; then
    echo "$ROOT already exists; run '$0 stop $P1' first" >&2
    exit 1
  fi
  mkdir -p "$ROOT/sock"
  "$BIN/initdb" -D "$ROOT/primary" -U kingres --auth-local=trust --auth-host=scram-sha-256 >"$ROOT/initdb.log"
  {
    echo "port = $P1"
    echo "listen_addresses = '*'"
    echo "unix_socket_directories = '$ROOT/sock'"
    echo "wal_level = replica"
    echo "max_wal_senders = 5"
    echo "max_replication_slots = 5"
    echo "password_encryption = 'scram-sha-256'"
    echo "log_min_messages = warning"
  } >>"$ROOT/primary/postgresql.conf"
  # Windows reaches WSL2 through the NAT gateway: allow any address, with SCRAM.
  echo "host all all 0.0.0.0/0 scram-sha-256" >>"$ROOT/primary/pg_hba.conf"
  echo "host all all ::/0 scram-sha-256" >>"$ROOT/primary/pg_hba.conf"
  echo "local replication all trust" >>"$ROOT/primary/pg_hba.conf"
  "$BIN/pg_ctl" -D "$ROOT/primary" -l "$ROOT/primary.log" -w start >/dev/null 2>&1 </dev/null

  psql_local -d postgres -f "$HERE/fixture_roles.sql"
  psql_local -d plain_cp -c "create table public.plain_t (id int primary key, label text)"
  psql_local -d kingres_cp -v p1="$P1" -v p2="$P2" -f "$HERE/fixture_catalog.sql"

  # A real streaming standby, so that pg_stat_replication has a row. Its
  # application_name is the one the topology reader matches (kingres_node_2).
  "$BIN/pg_basebackup" -h "$ROOT/sock" -p "$P1" -U kingres -D "$ROOT/standby" -R -X stream >/dev/null
  {
    echo "port = $P2"
    echo "unix_socket_directories = '$ROOT/sock'"
    echo "primary_conninfo = 'host=$ROOT/sock port=$P1 user=kingres application_name=kingres_node_2'"
  } >>"$ROOT/standby/postgresql.auto.conf"
  "$BIN/pg_ctl" -D "$ROOT/standby" -l "$ROOT/standby.log" -w start >/dev/null 2>&1 </dev/null
  # Wait until the standby streams.
  for _ in $(seq 1 50); do
    n="$(psql_local -d postgres -At -c "select count(*) from pg_stat_replication where state = 'streaming'")"
    [ "$n" = "1" ] && break
    sleep 0.2
  done
  # A physical slot, to be listed by the topology.
  psql_local -d postgres -c "select pg_create_physical_replication_slot('kingres_slot_2')" >/dev/null
  echo "primary on port $P1, standby on port $P2"
  ;;
stop)
  if [ -d "$ROOT/standby" ]; then "$BIN/pg_ctl" -D "$ROOT/standby" -m immediate stop >/dev/null 2>&1 || true; fi
  if [ -d "$ROOT/primary" ]; then "$BIN/pg_ctl" -D "$ROOT/primary" -m immediate stop >/dev/null 2>&1 || true; fi
  case "$ROOT" in
  */cp-test/*) rm -rf "$ROOT" ;;
  *) echo "refusing to remove $ROOT: not under a cp-test directory" >&2 ;;
  esac
  echo "stopped and removed $ROOT"
  ;;
*)
  echo "usage: $0 start|stop [primary-port] [standby-port]" >&2
  exit 2
  ;;
esac

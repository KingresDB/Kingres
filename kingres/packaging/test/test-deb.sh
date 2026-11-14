#!/usr/bin/env bash
# SPDX-License-Identifier: PostgreSQL
# Copyright (c) 2026, RooyeKhat Media Co
#
# kingres/packaging/test/test-deb.sh
#
# Install the .deb into a clean ubuntu:24.04 container that runs systemd as
# PID 1 and check the whole life cycle with a real database:
#   install     dependencies resolve; users, owners and modes; the unit is
#               Type=notify and active as soon as "apt install" returns; it
#               runs as the system user; the API answers uninitialized
#   cluster     create a cluster through the API with the setup token, sign
#               in, create a role and a database (sharding, ledger), create
#               tables with kingres-psql, distribute one and enable the
#               ledger on the other through the API, insert and read rows,
#               generate a ledger digest
#   restarts    restart only the manager (the postmaster keeps its PID and the
#               database answers meanwhile); kill -9 of the manager;
#               "systemctl restart" (the database restarts, data stay);
#               "systemctl stop" then "start" (the cluster comes back, no
#               wizard); the instance log shows no crash recovery
#   upgrade     a package with a higher revision keeps data and restarts
#   remove      the service and the database stop; data and settings stay
#   reinstall   the cluster comes back with its data
#   purge       data, settings, unit and user are removed
#
# Usage:
#   test-deb.sh --deb FILE [--work DIR] [--keep]
#
#   --deb FILE   the package to test
#   --work DIR   the build work area; when given, a second package with
#                revision 2 is built from the same staged tree to test an
#                upgrade
#   --keep       leave the container running (name kingres-deb-test)
#
# The container needs --privileged and the host cgroup namespace to run
# systemd; it is removed at the end unless --keep is given.  Needs docker.

# The test commands are single-quoted on purpose: they run inside the container.
# shellcheck disable=SC2016
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "${here}/lib.sh"
# shellcheck source=flow.sh
. "${here}/flow.sh"

deb=""
work=""
keep=0
while [ "$#" -gt 0 ]; do
    case "$1" in
        --deb)  deb="$2"; shift 2 ;;
        --work) work="$2"; shift 2 ;;
        --keep) keep=1; shift ;;
        *) echo "test-deb.sh: unknown argument '$1'" >&2; exit 2 ;;
    esac
done
[ -f "${deb}" ] || { echo "test-deb.sh: --deb FILE is required" >&2; exit 2; }

name=kingres-deb-test
image=kingres-systemd-test
tmp="$(mktemp -d)"
x() { docker exec "${name}" "$@"; }
sh_() { docker exec "${name}" bash -c "$1" "${@:2}"; }

cleanup() {
    if [ "${keep}" -eq 0 ]; then docker rm -f "${name}" >/dev/null 2>&1 || true; fi
    rm -rf "${tmp}"
}
trap cleanup EXIT
docker rm -f "${name}" >/dev/null 2>&1 || true

# Variables of flow.sh: everything runs inside the container.
API_CURL=(docker exec -i "${name}" curl)
API_BASE=http://127.0.0.1:8091/api/v1
DATA_DIR=/var/lib/kingres
PSQL_CMD=(docker exec -e "PGPASSWORD=${FLOW_PASSWORD}" "${name}" kingres-psql -X -h 127.0.0.1 -p 5432 -U "${FLOW_ADMIN}")
TOKEN_CMD=read_token
read_token() { x cat /var/lib/kingres/setup-token; }

wait_ready() { wait_for "API answers" 60 x curl -fsS http://127.0.0.1:8091/api/v1/readyz; }
postmaster_pid() { x head -1 /var/lib/kingres/instances/1/pgdata/postmaster.pid 2>/dev/null || echo none; }
main_pid() { x systemctl show -p MainPID --value kingres; }
db_up() { psql_run -d postgres -Atc 'select 1' >/dev/null 2>&1; }
instance_logs() { sh_ 'cat /var/lib/kingres/instances/1/pgdata/log/* 2>/dev/null'; }

# ------------------------------------------------------------ the container
note "starting a clean ubuntu:24.04 with systemd"
docker build --quiet --file "${here}/Dockerfile.systemd" --tag "${image}" "${here}" >/dev/null
docker run --detach --name "${name}" --privileged --cgroupns=host \
    --volume /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /run/lock \
    "${image}" >/dev/null
for _ in $(seq 1 60); do
    s="$(docker exec "${name}" systemctl is-system-running 2>/dev/null || true)"
    case "${s}" in running | degraded) break ;; esac
    sleep 1
done
check "systemd is PID 1 and running (state: ${s:-unknown})" test -n "${s:-}" -a "${s:-}" != offline
docker cp "${deb}" "${name}:/tmp/kingres.deb"
docker cp "${here}/pg-smoke.sh" "${name}:/tmp/pg-smoke.sh"
x apt-get update -qq >/dev/null

# ---------------------------------------------------------------- install
note "install"
if x apt-get install -y --no-install-recommends /tmp/kingres.deb >"${tmp}/install.log" 2>&1; then
    pass "apt-get install of the package"
else
    fail "apt-get install of the package"
    tail -30 "${tmp}/install.log"
fi
grep -A8 'KingresDB is installed' "${tmp}/install.log" || true

check_eq "package status" "install ok installed" "$(x dpkg-query -W -f '${Status}' kingres)"
check_eq "system user (uid below 1000, nologin)" "yes /usr/sbin/nologin /var/lib/kingres" \
    "$(sh_ 'u=$(getent passwd kingres); uid=$(echo "$u" | cut -d: -f3); [ "$uid" -lt 1000 ] && echo -n "yes " ; echo "$u" | cut -d: -f7,6 | awk -F: "{print \$2, \$1}"')"
check_eq "group exists" kingres "$(x getent group kingres | cut -d: -f1)"
check_eq "data directory" "750 kingres kingres" "$(x stat -c '%a %U %G' /var/lib/kingres)"
check_eq "settings file" "644 root root" "$(x stat -c '%a %U %G' /etc/kingres/kingresd.json)"
check_eq "environment file (may hold secrets)" "600 root root" "$(x stat -c '%a %U %G' /etc/kingres/kingresd.env)"
check_eq "unit file" "644 root root" "$(x stat -c '%a %U %G' /usr/lib/systemd/system/kingres.service)"
check_eq "service enabled" enabled "$(x systemctl is-enabled kingres)"
check_eq "unit type" notify "$(x systemctl show -p Type --value kingres)"
check_eq "service active when apt returns" active "$(x systemctl is-active kingres)"
# Type=notify: the unit is only "active" after kingresd sent READY=1, which it
# does when the API listens.  So the first request must already succeed.
check_eq "API answers at once (readiness reached systemd)" 200 \
    "$(x curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8091/api/v1/readyz)"
check_eq "process user" kingres "$(x ps -o user= -C kingresd | tr -d ' ' | head -1)"
check_eq "setup state" uninitialized \
    "$(x curl -fsS http://127.0.0.1:8091/api/v1/setup/state | sed -n 's/^{"state":"\([a-z]*\)".*/\1/p')"
check_eq "setup token mode and owner" "600 kingres kingres" "$(x stat -c '%a %U %G' /var/lib/kingres/setup-token)"
check "panel index page" sh_ 'curl -fsS http://127.0.0.1:8091/ | grep -q "<script"'
check_eq "favicon status" 200 "$(x curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8091/favicon.svg)"
check "panel listens on loopback only" sh_ 'grep -qi "0100007F:1F9B" /proc/net/tcp && ! grep -qi "00000000:1F9B" /proc/net/tcp'
check_eq "runtime directory" "750 kingres" "$(x stat -c '%a %U' /run/kingres)"
check "kingresd -version shows version, contract and commit" sh_ 'kingresd -version | grep -Eq "^kingresd [0-9.]+ \(contract [0-9.]+, commit [0-9a-f]{40}"'
check "no database exists before setup" sh_ '! pgrep -x postgres'

note "command-line names and the distribution's names"
check "kingres-psql reports PostgreSQL 18" sh_ 'kingres-psql --version | grep -q "psql (PostgreSQL) 18"'
check "kingres-pg_dump runs" x kingres-pg_dump --version
check "kingres-pg_isready runs" sh_ 'kingres-pg_isready -p 1; [ $? -ne 127 ]'
check "kingres-verify runs" x kingres-verify --help
check "kingresd -h runs" sh_ 'kingresd -h 2>&1 | grep -q "Usage of kingresd"'
check "no /usr/bin/psql was installed" sh_ '! test -e /usr/bin/psql && ! test -e /usr/bin/pg_dump'
check "no linked name without the prefix (other than kingresd)" \
    sh_ '[ -z "$(dpkg -L kingres | grep "^/usr/bin/" | grep -v "^/usr/bin/kingres")" ]'
check "man pages are in the package" sh_ 'dpkg -L kingres | grep -q man8/kingresd.8.gz && dpkg -L kingres | grep -q man1/kingres-psql.1.gz'

note "a throw-away server of the package starts with both extensions"
smoke="$(docker exec "${name}" runuser -u kingres -- bash /tmp/pg-smoke.sh /tmp/smoke 2>&1 || true)"
echo "${smoke}"
check "version() says KingresDB" grep -q 'KingresDB 0\.' <<<"${smoke}"
check "both extensions created" bash -c 'grep -q "extension kingres_shard" <<<"$1" && grep -q "extension kingres_ledger" <<<"$1"' _ "${smoke}"

# ---------------------------------------------------------------- cluster
note "create a cluster through the API"
flow_init_cluster "deb-test"
check "setup token removed after setup" sh_ '! test -e /var/lib/kingres/setup-token'
flow_login
check_eq "instance process user" kingres "$(x ps -o user= -C postgres | sort -u | tr -d ' ')"
check "instance socket is in /run/kingres" x test -S /run/kingres/1/.s.PGSQL.5432
check "instance data below /var/lib/kingres, mode 0700" sh_ '[ "$(stat -c %a /var/lib/kingres/instances/1/pgdata)" = 700 ]'
check "database listens on 127.0.0.1:5432 only" sh_ 'grep -qi "0100007F:1538" /proc/net/tcp && ! grep -qi "00000000:1538" /proc/net/tcp'
check "pki directory is private to the service user" sh_ '[ "$(stat -c "%a %U" /var/lib/kingres/pki)" = "700 kingres" ]'
check "CA key is 0600" sh_ '[ "$(stat -c %a /var/lib/kingres/pki/ca.key)" = 600 ]'
check "database encoding is UTF8" sh_ 'PGPASSWORD="$1" kingres-psql -X -h 127.0.0.1 -U "$2" -d postgres -Atc "show server_encoding" | grep -q UTF8' _ "${FLOW_PASSWORD}" "${FLOW_ADMIN}"
flow_create_data
flow_check_data "after setup"
note "kingres-psql through the local socket (peer authentication is mapped to kingresd only)"
check "socket login of the admin role without a password is refused" \
    sh_ '! kingres-psql -X -h /run/kingres/1 -U root_admin -d postgres -w -Atc "select 1"'

note "sandbox"
exposure="$(x systemd-analyze security kingres.service --no-pager 2>/dev/null | tail -1 || true)"
echo "systemd-analyze: ${exposure}"
check "no capabilities in the manager process" sh_ 'grep -q "^CapEff:.0000000000000000" /proc/$(systemctl show -p MainPID --value kingres)/status'
check "no capabilities in the postmaster" sh_ 'grep -q "^CapEff:.0000000000000000" /proc/$(head -1 /var/lib/kingres/instances/1/pgdata/postmaster.pid)/status'
check "NoNewPrivileges is set" sh_ 'grep -q "^NoNewPrivs:.1" /proc/$(systemctl show -p MainPID --value kingres)/status'
check "service cannot write outside its directories" \
    sh_ 'nsenter --target "$(systemctl show -p MainPID --value kingres)" --mount -- bash -c "touch /etc/kingres-write-test" 2>&1 | grep -qi "read-only\|denied\|not permitted"'
check "dynamic shared memory works (parallel query)" \
    sh_ 'PGPASSWORD="$1" kingres-psql -X -h 127.0.0.1 -U "$2" -d shop -Atc "set min_parallel_table_scan_size=0; set parallel_setup_cost=0; set parallel_tuple_cost=0; select count(*) from generate_series(1,200000) g join public.orders o on true" >/dev/null' _ "${FLOW_PASSWORD}" "${FLOW_ADMIN}"
check "no system call was denied by the filter" sh_ '! journalctl -u kingres --no-pager 2>/dev/null | grep -qi "bad system call\|SIGSYS"'

# --------------------------------------------------------------- restarts
note "restart only the manager"
pm1="$(postmaster_pid)"; mp1="$(main_pid)"
x systemctl kill --kill-whom=main --signal=SIGTERM kingres
for _ in $(seq 1 30); do [ "$(main_pid)" != "${mp1}" ] && [ "$(main_pid)" != 0 ] && break; sleep 0.5; done
mp2="$(main_pid)"
check "manager has a new process (${mp1} -> ${mp2})" test "${mp2}" != "${mp1}" -a "${mp2}" != 0
check_eq "postmaster PID unchanged (${pm1})" "${pm1}" "$(postmaster_pid)"
check "database answered while the manager restarted" db_up
check_eq "unit is active" active "$(x systemctl is-active kingres)"
check "API answers again" wait_ready
flow_check_api "after manager restart"
flow_check_data "after manager restart"
check_eq "postmaster PID still unchanged" "${pm1}" "$(postmaster_pid)"

note "kill -9 of the manager"
mp1="$(main_pid)"
x kill -9 "${mp1}"
sleep 6
mp2="$(main_pid)"
check "manager restarts after kill -9 (pid ${mp1} -> ${mp2})" test "${mp2}" != "0" -a "${mp2}" != "${mp1}"
check_eq "postmaster PID unchanged" "${pm1}" "$(postmaster_pid)"
check "API answers again" wait_ready
flow_check_api "after kill -9"

note "restart everything (systemctl restart)"
x systemctl restart kingres
check "API answers" wait_ready
wait_for "database is back" 60 db_up
pm2="$(postmaster_pid)"
check "postmaster restarted (${pm1} -> ${pm2})" test "${pm2}" != "${pm1}" -a "${pm2}" != none
flow_check_api "after systemctl restart"
flow_check_data "after systemctl restart"

note "stop, then start (what a reboot does)"
x systemctl stop kingres
check_eq "unit inactive after stop" inactive "$(x systemctl is-active kingres || true)"
check "no postgres process after stop" sh_ '! pgrep -x postgres && ! pgrep -x kingresd'
check "no postmaster.pid after a clean stop" sh_ '! test -e /var/lib/kingres/instances/1/pgdata/postmaster.pid'
# After a stop the runtime directory is gone, as after a reboot: kingresd has
# to create the instance's socket directory again when it starts the instance.
check "runtime directory removed by the stop" sh_ '! test -e /run/kingres/1'
x systemctl start kingres
check_eq "unit active after start" active "$(x systemctl is-active kingres)"
check "API answers" wait_ready
wait_for "database is back" 60 db_up
check_eq "node state: no wizard" clustered \
    "$(x curl -fsS http://127.0.0.1:8091/api/v1/setup/state | sed -n 's/^{"state":"\([a-z]*\)".*/\1/p')"
check_eq "socket directory recreated by kingresd" "700 kingres" "$(x stat -c '%a %U' /run/kingres/1)"
check "no setup token" sh_ '! test -e /var/lib/kingres/setup-token'
flow_check_api "after stop and start"
flow_check_data "after stop and start"
logs="$(instance_logs)"
check "instance log: clean shutdowns, no crash recovery so far" bash -c '! grep -qi "automatic recovery in progress\|not properly shut down" <<<"$1"' _ "${logs}"
check "instance log: shut down cleanly at least once" grep -q "database system is shut down" <<<"${logs}"

# ---------------------------------------------------------------- upgrade
if [ -n "${work}" ]; then
    note "upgrade to revision 2 with data present"
    "${here}/../deb/build-deb.sh" --work "${work}" --revision 2 --out "${tmp}/up" >/dev/null
    docker cp "${tmp}/up/kingres_"*"-2_amd64.deb" "${name}:/tmp/kingres2.deb"
    sh_ 'echo "# local change" >>/etc/kingres/kingresd.env'
    check "apt-get install of revision 2" x apt-get install -y --no-install-recommends -o Dpkg::Options::=--force-confold /tmp/kingres2.deb
    check_eq "version after upgrade" "0.1.0-2" "$(x dpkg-query -W -f '${Version}' kingres)"
    check_eq "service active after upgrade" active "$(x systemctl is-active kingres)"
    check "API answers after upgrade" wait_ready
    wait_for "database is back after the upgrade" 60 db_up
    flow_check_api "after upgrade"
    flow_check_data "after upgrade"
    check "local configuration change kept" sh_ 'grep -q "# local change" /etc/kingres/kingresd.env'
fi

# ----------------------------------------------------------------- remove
note "remove (not purge)"
x apt-get remove -y kingres >"${tmp}/remove.log" 2>&1 || cat "${tmp}/remove.log"
check_eq "service stopped" inactive "$(x systemctl is-active kingres || true)"
check "no kingresd and no postgres process" sh_ '! pgrep -x kingresd && ! pgrep -x postgres'
check "installation tree is gone" sh_ '! test -e /opt/kingres && ! test -e /usr/bin/kingres-psql'
check "unit file is gone" sh_ '! test -e /usr/lib/systemd/system/kingres.service'
check "data directory (the database) kept" x test -f /var/lib/kingres/instances/1/pgdata/PG_VERSION
check "settings kept" x test -f /etc/kingres/kingresd.json
check "user kept" x getent passwd kingres
check_eq "package state" "config-files" "$(x dpkg-query -W -f '${db:Status-Status}' kingres)"

note "reinstall after remove: the cluster comes back"
check "apt-get install again" x apt-get install -y --no-install-recommends /tmp/kingres.deb
check "API answers" wait_ready
wait_for "database is back" 60 db_up
check_eq "node state: no wizard" clustered \
    "$(x curl -fsS http://127.0.0.1:8091/api/v1/setup/state | sed -n 's/^{"state":"\([a-z]*\)".*/\1/p')"
flow_check_api "after reinstall"
flow_check_data "after reinstall"

# ------------------------------------------------------------------ purge
note "purge"
x apt-get purge -y kingres >"${tmp}/purge.log" 2>&1 || cat "${tmp}/purge.log"
check "no kingresd and no postgres process" sh_ '! pgrep -x kingresd && ! pgrep -x postgres'
check "data directory deleted" sh_ '! test -e /var/lib/kingres'
check "settings deleted" sh_ '! test -e /etc/kingres'
check "user deleted" sh_ '! getent passwd kingres'
check "group deleted" sh_ '! getent group kingres'
check "unit file and enablement links gone" sh_ '! test -e /usr/lib/systemd/system/kingres.service && ! test -e /etc/systemd/system/multi-user.target.wants/kingres.service'
check_eq "package state" "not-installed" "$(x dpkg-query -W -f '${db:Status-Status}' kingres 2>/dev/null || echo not-installed)"
check "no kingres files left" sh_ '! test -e /opt/kingres && ! test -e /run/kingres'

summary

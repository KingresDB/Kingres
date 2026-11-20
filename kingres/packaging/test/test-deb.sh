#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
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
#   other major the package of the other PostgreSQL major is refused, with the
#               old package, its service and its data untouched
#   remove      the service and the database stop; data and settings stay
#   reinstall   the cluster comes back with its data
#   purge       data, settings, unit and user are removed
#
# Usage:
#   test-deb.sh --deb FILE (--other-deb FILE | --without-other-major) [--work DIR] [--keep]
#
#   --deb FILE   the package to test; its version (0.1.0+pg18-1) says which
#                PostgreSQL major the test expects everywhere else
#   --other-deb FILE  the package of the OTHER major, for the tests that it is
#                refused (with data present, after removal, and from a
#                leftover data directory) and that it installs after a purge
#   --without-other-major  do not run those tests (they are then reported as
#                not run)
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
other_deb=""
no_other=0
work=""
keep=0
while [ "$#" -gt 0 ]; do
    case "$1" in
        --deb)  deb="$2"; shift 2 ;;
        --other-deb) other_deb="$2"; shift 2 ;;
        --without-other-major) no_other=1; shift ;;
        --work) work="$2"; shift 2 ;;
        --keep) keep=1; shift ;;
        *) echo "test-deb.sh: unknown argument '$1'" >&2; exit 2 ;;
    esac
done
[ -f "${deb}" ] || { echo "test-deb.sh: --deb FILE is required" >&2; exit 2; }
if [ "${no_other}" -eq 0 ]; then
    [ -f "${other_deb}" ] || { echo "test-deb.sh: --other-deb FILE (the package of the other major) or --without-other-major is required" >&2; exit 2; }
fi

# What is being tested, from the package itself: version 0.1.0+pg18-1.
deb_version="$(dpkg-deb -f "${deb}" Version)"
major="$(printf '%s' "${deb_version}" | sed -n 's/^[0-9.]*+pg\([0-9][0-9]*\)-.*/\1/p')"
[ -n "${major}" ] || { echo "test-deb.sh: the version ${deb_version} of the package has no +pgNN" >&2; exit 2; }
base_version="${deb_version%-*}"
other_major=""
if [ "${no_other}" -eq 0 ]; then
    other_major="$(dpkg-deb -f "${other_deb}" Version | sed -n 's/^[0-9.]*+pg\([0-9][0-9]*\)-.*/\1/p')"
    if [ -z "${other_major}" ] || [ "${other_major}" = "${major}" ]; then echo "test-deb.sh: --other-deb must be a package of another major than ${major}" >&2; exit 2; fi
fi

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
# The service role kingresd (a superuser) for the checks that need one.
SERVICE_PSQL=(docker exec "${name}" bash -c 'PGPASSWORD="$(cat /var/lib/kingres/service-credentials)" exec kingres-psql -X -h 127.0.0.1 -p 5432 -U kingresd "$@"' _)

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
if [ "${no_other}" -eq 0 ]; then docker cp "${other_deb}" "${name}:/tmp/kingres-other.deb"; fi
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
check_eq "installed version carries the major" "${deb_version}" "$(x dpkg-query -W -f '${Version}' kingres)"
check "the file name of the package carries the major" test "$(basename "${deb}")" = "kingres_${deb_version}_amd64.deb"
check_eq "BUILD-INFO names the major" "${major}" "$(x sed -n 's/^PostgreSQL: .* (major \([0-9]*\))$/\1/p' /opt/kingres/share/BUILD-INFO)"
pg_version="$(x sed -n 's/^PostgreSQL: \(.*\) (major .*$/\1/p' /opt/kingres/share/BUILD-INFO)"
check "BUILD-INFO says what the package version says (PostgreSQL ${pg_version})" test "${pg_version}" != ""
check_eq "the server is that PostgreSQL" "postgres (PostgreSQL) ${pg_version}" "$(x /opt/kingres/bin/postgres --version)"
check_eq "the server's major is the one in the package version" "${major}" \
    "$(x /opt/kingres/bin/postgres --version | sed -n 's/^postgres (PostgreSQL) \([0-9][0-9]*\).*/\1/p')"
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

# Everything that needs the node to run is gated on this: when the service does not
# come up (say so, once), those checks are reported as not run instead of failing
# one by one for minutes.
node_down=0
if ! x curl -fsS http://127.0.0.1:8091/api/v1/readyz >/dev/null 2>&1; then
    node_down=1
    echo "info: the service does not answer; its journal:"
    x journalctl -u kingres --no-pager -n 12 2>&1 | cut -c1-240
fi

note "command-line names and the distribution's names"
check_eq "kingres-psql reports PostgreSQL ${pg_version}" "psql (PostgreSQL) ${pg_version}" "$(x kingres-psql --version)"
check "kingres-pg_dump runs" x kingres-pg_dump --version
check "kingres-pg_isready runs" sh_ 'kingres-pg_isready -p 1; [ $? -ne 127 ]'
check "kingres-verify runs" x kingres-verify --help
check "kingres-verify has the release subcommand" sh_ 'kingres-verify release --help | grep -q "release verify"'
check "kingres-verify is the file of /opt/kingres/bin, linked from /usr/bin" sh_ \
    '[ "$(readlink /usr/bin/kingres-verify)" = /opt/kingres/bin/kingres-verify ] && [ -x /opt/kingres/bin/kingres-verify ]'
check "BUILD-INFO has the SHA-256 of the installed kingres-verify" sh_ \
    '[ "$(sed -n "s/^kingres-verify sha256: \([0-9a-f]*\) .*/\1/p" /opt/kingres/share/BUILD-INFO)" = "$(sha256sum /opt/kingres/bin/kingres-verify | cut -d" " -f1)" ]'
# The ledger signing key can be in the memory of kingresd: no core file may be written.
check_eq "the unit says LimitCORE=0" 0 "$(x systemctl show -p LimitCORE --value kingres)"
check "the manager runs with a zero core file limit" sh_ \
    'grep -Eq "^Max core file size +0 +0" /proc/$(systemctl show -p MainPID --value kingres)/limits'
check "kingresd -h runs" sh_ 'kingresd -h 2>&1 | grep -q "Usage of kingresd"'
check "no /usr/bin/psql was installed" sh_ '! test -e /usr/bin/psql && ! test -e /usr/bin/pg_dump'
check "no linked name without the prefix (other than kingresd)" \
    sh_ '[ -z "$(dpkg -L kingres | grep "^/usr/bin/" | grep -v "^/usr/bin/kingres")" ]'
check "man pages are in the package" sh_ 'dpkg -L kingres | grep -q man8/kingresd.8.gz && dpkg -L kingres | grep -q man1/kingres-psql.1.gz'

note "every KingresDB extension is installed, and a throw-away server of the package creates them"
check_extension_files "package" docker exec "${name}"
check_licence_files "package" docker exec "${name}"
smoke="$(docker exec "${name}" runuser -u kingres -- bash /tmp/pg-smoke.sh /tmp/smoke 2>&1 || true)"
echo "${smoke}"
check_smoke_output "package" "${major}" "${smoke}"

if [ "${node_down}" -eq 0 ]; then
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
flow_check_extensions "package"
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
wait_ready
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
wait_ready
flow_check_api "after kill -9"

note "restart everything (systemctl restart)"
x systemctl restart kingres
wait_ready
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
wait_ready
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

else
    echo "NOT RUN  create a cluster, sandbox, restarts, stop and start (the service does not come up, so there is no cluster)"
fi

# ---------------------------------------------------------------- upgrade
if [ -n "${work}" ] && [ "${node_down}" -eq 0 ]; then
    note "upgrade to revision 2 with data present"
    "${here}/../deb/build-deb.sh" --work "${work}" --revision 2 --out "${tmp}/up" >/dev/null
    docker cp "${tmp}/up/kingres_"*"-2_amd64.deb" "${name}:/tmp/kingres2.deb"
    sh_ 'echo "# local change" >>/etc/kingres/kingresd.env'
    check "apt-get install of revision 2" x apt-get install -y --no-install-recommends -o Dpkg::Options::=--force-confold /tmp/kingres2.deb
    check_eq "version after upgrade" "${base_version}-2" "$(x dpkg-query -W -f '${Version}' kingres)"
    check_eq "service active after upgrade" active "$(x systemctl is-active kingres)"
    wait_ready
    wait_for "database is back after the upgrade" 60 db_up
    flow_check_api "after upgrade"
    flow_check_data "after upgrade"
    check "local configuration change kept" sh_ 'grep -q "# local change" /etc/kingres/kingresd.env'
fi

# ------------------------------------------------------------ other major
# "+pg19" sorts after "+pg18", so the package manager sees the package of the
# other major as an upgrade (or, from 19 to 18, a downgrade).  It must be
# refused before anything is unpacked: the programs of one major cannot run
# the data directory of the other.
refused() { # refused LOG: apt failed and the preinst of the other major said why
    local log="$1"
    grep -q "refusing to install KingresDB for PostgreSQL ${other_major}" "${log}" &&
        grep -q "cannot be moved between PostgreSQL majors" "${log}"
}
if [ "${no_other}" -eq 1 ]; then
    echo "NOT RUN  the package of the other major is refused (--without-other-major)"
else
    note "the package of PostgreSQL ${other_major} is refused while this one is installed with data"
    cur_version="$(x dpkg-query -W -f '${Version}' kingres)"
    pm_before="$(postmaster_pid)"
    svc_before="$(x systemctl is-active kingres || true)"
    info_before="$(x sha256sum /opt/kingres/share/BUILD-INFO)"
    if x apt-get install -y --allow-downgrades --no-install-recommends /tmp/kingres-other.deb >"${tmp}/other-install.log" 2>&1; then
        fail "apt-get install of the package of the other major was accepted"
    else
        pass "apt-get install of the package of the other major fails"
    fi
    # (--allow-downgrades: without it apt itself refuses a downgrade from 19 to 18 before the package is looked at)
    echo "--- what apt printed:"; grep -v '^$' "${tmp}/other-install.log" | tail -14
    check "the message says what was refused and why" refused "${tmp}/other-install.log"
    check "the message names the installed version" grep -q "Version ${cur_version}" "${tmp}/other-install.log"
    check_eq "installed version is unchanged" "${cur_version}" "$(x dpkg-query -W -f '${Version}' kingres)"
    check_eq "package state is unchanged" "install ok installed" "$(x dpkg-query -W -f '${Status}' kingres)"
    check_eq "the programs are unchanged (BUILD-INFO)" "${info_before}" "$(x sha256sum /opt/kingres/share/BUILD-INFO)"
    check_eq "the server is still PostgreSQL ${pg_version}" "postgres (PostgreSQL) ${pg_version}" "$(x /opt/kingres/bin/postgres --version)"
    if [ "${node_down}" -eq 0 ]; then
        check_eq "service still in the state it was in (${svc_before})" "${svc_before}" "$(x systemctl is-active kingres)"
    fi
    check_eq "postmaster untouched (PID ${pm_before})" "${pm_before}" "$(postmaster_pid)"
    if [ "${node_down}" -eq 0 ]; then
        wait_ready
        flow_check_api "after the refusal"
        flow_check_data "after the refusal"
    else
        echo "NOT RUN  the API and the data after the refusal (the service does not come up, there is no cluster)"
    fi
    check "dpkg -i of the same package is refused the same way" \
        sh_ '! dpkg -i /tmp/kingres-other.deb >/tmp/other-dpkg.log 2>&1 && grep -q "cannot be moved between PostgreSQL majors" /tmp/other-dpkg.log'
    check_eq "installed version is still unchanged" "${cur_version}" "$(x dpkg-query -W -f '${Version}' kingres)"
fi

# ----------------------------------------------------------------- remove
note "remove (not purge)"
x apt-get remove -y kingres >"${tmp}/remove.log" 2>&1 || cat "${tmp}/remove.log"
check_eq "service stopped" inactive "$(x systemctl is-active kingres || true)"
check "no kingresd and no postgres process" sh_ '! pgrep -x kingresd && ! pgrep -x postgres'
check "installation tree is gone" sh_ '! test -e /opt/kingres && ! test -e /usr/bin/kingres-psql'
check "unit file is gone" sh_ '! test -e /usr/lib/systemd/system/kingres.service'
if [ "${node_down}" -eq 0 ]; then
    check "data directory (the database) kept" x test -f /var/lib/kingres/instances/1/pgdata/PG_VERSION
fi
check "settings kept" x test -f /etc/kingres/kingresd.json
check "user kept" x getent passwd kingres
check_eq "package state" "config-files" "$(x dpkg-query -W -f '${db:Status-Status}' kingres)"

if [ "${no_other}" -eq 0 ]; then
    note "the package of PostgreSQL ${other_major} is refused after a removal that kept the data"
    if x apt-get install -y --no-install-recommends /tmp/kingres-other.deb >"${tmp}/other-removed.log" 2>&1; then
        fail "the package of the other major was accepted over a removed package with data"
    else
        pass "the package of the other major is refused over a removed package"
    fi
    check "the message names the removed version" grep -q "Version ${cur_version}" "${tmp}/other-removed.log"
    check "the message says what was refused and why" refused "${tmp}/other-removed.log"
    check_eq "package state is still config-files" "config-files" "$(x dpkg-query -W -f '${db:Status-Status}' kingres)"
    [ "${node_down}" -eq 1 ] || check "the data directory is untouched" x test -f /var/lib/kingres/instances/1/pgdata/PG_VERSION
fi

if [ "${node_down}" -eq 0 ]; then
note "reinstall after remove: the cluster comes back"
check "apt-get install again" x apt-get install -y --no-install-recommends /tmp/kingres.deb
wait_ready
wait_for "database is back" 60 db_up
check_eq "node state: no wizard" clustered \
    "$(x curl -fsS http://127.0.0.1:8091/api/v1/setup/state | sed -n 's/^{"state":"\([a-z]*\)".*/\1/p')"
flow_check_api "after reinstall"
flow_check_data "after reinstall"

else
    echo "NOT RUN  reinstall after remove: the cluster comes back (no cluster exists)"
fi

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

if [ "${no_other}" -eq 0 ]; then
    note "a data directory of PostgreSQL ${major} left behind refuses the package of PostgreSQL ${other_major}, even when no package is installed"
    sh_ "mkdir -p /var/lib/kingres/instances/1/pgdata && echo ${major} >/var/lib/kingres/instances/1/pgdata/PG_VERSION"
    if x apt-get install -y --no-install-recommends /tmp/kingres-other.deb >"${tmp}/other-data.log" 2>&1; then
        fail "the package of the other major was accepted over a data directory of this major"
    else
        pass "the package of the other major is refused over a data directory of this major"
    fi
    check "the message names the data directory" grep -q "data directory /var/lib/kingres/instances/1/pgdata is a PostgreSQL ${major} cluster" "${tmp}/other-data.log"
    check_eq "nothing was installed" "not-installed" "$(x dpkg-query -W -f '${db:Status-Status}' kingres 2>/dev/null || echo not-installed)"
    sh_ 'rm -rf /var/lib/kingres'

    note "the package of PostgreSQL ${other_major} installs on a system without KingresDB data"
    if x apt-get install -y --no-install-recommends /tmp/kingres-other.deb >"${tmp}/other-clean.log" 2>&1; then
        pass "apt-get install of the other major after the purge"
    else
        fail "apt-get install of the other major after the purge"
        tail -20 "${tmp}/other-clean.log"
    fi
    check_eq "installed version is the other major" "${other_major}" \
        "$(x dpkg-query -W -f '${Version}' kingres | sed -n 's/^[0-9.]*+pg\([0-9][0-9]*\)-.*/\1/p')"
    check "kingres-psql is that major's" sh_ "kingres-psql --version | grep -q 'psql (PostgreSQL) ${other_major}'"
    echo "info: unit enabled: $(x systemctl is-enabled kingres 2>&1), active: $(x systemctl is-active kingres 2>&1)"
    wait_ready
    if ! x curl -fsS http://127.0.0.1:8091/api/v1/readyz >/dev/null 2>&1; then
        echo "info: the service of the other major did not come up; its journal:"
        x journalctl -u kingres --no-pager -n 25 2>&1 | cut -c1-220
    fi
    check_eq "setup state" uninitialized \
        "$(x curl -fsS http://127.0.0.1:8091/api/v1/setup/state | sed -n 's/^{"state":"\([a-z]*\)".*/\1/p')"
    check "and the package of the first major is refused in turn" \
        sh_ '! dpkg -i /tmp/kingres.deb >/tmp/back.log 2>&1 && grep -q "cannot be moved between PostgreSQL majors" /tmp/back.log'
    x apt-get purge -y kingres >"${tmp}/purge2.log" 2>&1 || cat "${tmp}/purge2.log"
    check "purged again" sh_ '! test -e /var/lib/kingres && ! test -e /opt/kingres'
fi

summary

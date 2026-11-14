#!/usr/bin/env bash
# SPDX-License-Identifier: PostgreSQL
# Copyright (c) 2026, RooyeKhat Media Co
#
# kingres/packaging/test/test-deb.sh
#
# Install the .deb into a clean ubuntu:24.04 container that runs systemd as
# PID 1 and check the whole life cycle:
#   install     dependencies resolve; the kingres user, the data directory
#               and the files get the intended owners and modes; the service
#               is enabled, active and runs as the system user; the API
#               answers (uninitialized, setup token mode 0600, panel and
#               favicon served); the client programs work under their
#               kingres- names; a throw-away server starts with both extensions
#   resilience  the service restarts after kill -9; a restart keeps the token
#   upgrade     a package with a higher revision (optional) keeps data and
#               token and restarts the service
#   remove      the service stops and the files go; data, settings and user stay
#   reinstall   comes back with the same data (same setup token)
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
sh_() { docker exec "${name}" bash -c "$1"; }

cleanup() {
    if [ "${keep}" -eq 0 ]; then docker rm -f "${name}" >/dev/null 2>&1 || true; fi
    rm -rf "${tmp}"
}
trap cleanup EXIT
docker rm -f "${name}" >/dev/null 2>&1 || true

wait_ready() { # wait_ready: the API answers on the container's loopback
    for _ in $(seq 1 60); do
        if x curl -fsS http://127.0.0.1:8091/api/v1/readyz >/dev/null 2>&1; then return 0; fi
        sleep 1
    done
    return 1
}

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
    printf 'PASS  apt-get install of the package\n'; passed=$((passed + 1))
else
    printf 'FAIL  apt-get install of the package\n'; failed=$((failed + 1))
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
check_eq "service active" active "$(x systemctl is-active kingres)"
check "API answers" wait_ready
check_eq "process user" kingres "$(x ps -o user= -C kingresd | tr -d ' ' | head -1)"
check_eq "setup state" uninitialized \
    "$(x curl -fsS http://127.0.0.1:8091/api/v1/setup/state | sed -n 's/.*"state":"\([a-z]*\)".*/\1/p')"
check_eq "setup token mode and owner" "600 kingres kingres" "$(x stat -c '%a %U %G' /var/lib/kingres/setup-token)"
check "panel index page" sh_ 'curl -fsS http://127.0.0.1:8091/ | grep -q "<script"'
check_eq "favicon status" 200 "$(x curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8091/favicon.svg)"
check "listens on loopback only" sh_ 'grep -qi "0100007F:1F9B" /proc/net/tcp && ! grep -qi "00000000:1F9B" /proc/net/tcp'
check_eq "runtime directory" "750 kingres" "$(x stat -c '%a %U' /run/kingres)"
tokenA="$(x cat /var/lib/kingres/setup-token)"

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

note "a server of the package starts with both extensions"
smoke="$(docker exec "${name}" runuser -u kingres -- bash /tmp/pg-smoke.sh /tmp/smoke 2>&1 || true)"
echo "${smoke}"
check "version() says KingresDB" grep -q 'KingresDB 0\.' <<<"${smoke}"
check "both extensions created" bash -c 'grep -q "extension kingres_shard" <<<"$1" && grep -q "extension kingres_ledger" <<<"$1"' _ "${smoke}"

note "sandbox"
exposure="$(x systemd-analyze security kingres.service --no-pager 2>/dev/null | tail -1 || true)"
echo "systemd-analyze: ${exposure}"
check "no capabilities in the service process" sh_ 'grep -q "^CapEff:.0000000000000000" /proc/$(systemctl show -p MainPID --value kingres)/status'
check "NoNewPrivileges is set" sh_ 'grep -q "^NoNewPrivs:.1" /proc/$(systemctl show -p MainPID --value kingres)/status'
check "service cannot write outside its directory" \
    sh_ 'nsenter --target "$(systemctl show -p MainPID --value kingres)" --mount -- bash -c "touch /etc/kingres-write-test" 2>&1 | grep -qi "read-only\|denied\|not permitted"'

note "resilience"
pid1="$(x systemctl show -p MainPID --value kingres)"
x kill -9 "${pid1}"
sleep 8
pid2="$(x systemctl show -p MainPID --value kingres)"
check "service restarts after kill -9 (pid ${pid1} -> ${pid2})" test "${pid2}" != "0" -a "${pid2}" != "${pid1}"
check "API answers again" wait_ready
x systemctl restart kingres
check "API answers after systemctl restart" wait_ready
check "same setup token after restarts" test "${tokenA}" = "$(x cat /var/lib/kingres/setup-token)"

# ---------------------------------------------------------------- upgrade
if [ -n "${work}" ]; then
    note "upgrade to revision 2"
    "${here}/../deb/build-deb.sh" --work "${work}" --revision 2 --out "${tmp}/up" >/dev/null
    docker cp "${tmp}/up/kingres_"*"-2_amd64.deb" "${name}:/tmp/kingres2.deb"
    sh_ 'echo preserved >/var/lib/kingres/marker; chown kingres: /var/lib/kingres/marker'
    sh_ 'echo "# local change" >>/etc/kingres/kingresd.env'
    check "apt-get install of revision 2" x apt-get install -y --no-install-recommends -o Dpkg::Options::=--force-confold /tmp/kingres2.deb
    check_eq "version after upgrade" "0.1.0-2" "$(x dpkg-query -W -f '${Version}' kingres)"
    check_eq "service active after upgrade" active "$(x systemctl is-active kingres)"
    check "API answers after upgrade" wait_ready
    check "token kept by the upgrade" test "${tokenA}" = "$(x cat /var/lib/kingres/setup-token)"
    check_eq "data kept by the upgrade" preserved "$(x cat /var/lib/kingres/marker)"
    check "local configuration change kept" sh_ 'grep -q "# local change" /etc/kingres/kingresd.env'
fi

# ----------------------------------------------------------------- remove
note "remove (not purge)"
sh_ 'echo preserved >/var/lib/kingres/marker; chown kingres: /var/lib/kingres/marker'
x apt-get remove -y kingres >"${tmp}/remove.log" 2>&1 || cat "${tmp}/remove.log"
check_eq "service stopped" inactive "$(x systemctl is-active kingres || true)"
check "no kingresd process" sh_ '! pgrep -x kingresd'
check "installation tree is gone" sh_ '! test -e /opt/kingres && ! test -e /usr/bin/kingres-psql'
check "unit file is gone" sh_ '! test -e /usr/lib/systemd/system/kingres.service'
check "data directory kept" sh_ 'test "$(cat /var/lib/kingres/marker)" = preserved'
check "settings kept" x test -f /etc/kingres/kingresd.json
check "user kept" x getent passwd kingres
check_eq "package state" "config-files" "$(x dpkg-query -W -f '${db:Status-Status}' kingres)"

note "reinstall after remove"
check "apt-get install again" x apt-get install -y --no-install-recommends /tmp/kingres.deb
check "API answers" wait_ready
check "same setup token (data survived remove)" test "${tokenA}" = "$(x cat /var/lib/kingres/setup-token)"

# ------------------------------------------------------------------ purge
note "purge"
x apt-get purge -y kingres >"${tmp}/purge.log" 2>&1 || cat "${tmp}/purge.log"
check "data directory deleted" sh_ '! test -e /var/lib/kingres'
check "settings deleted" sh_ '! test -e /etc/kingres'
check "user deleted" sh_ '! getent passwd kingres'
check "group deleted" sh_ '! getent group kingres'
check "unit file and enablement links gone" sh_ '! test -e /usr/lib/systemd/system/kingres.service && ! test -e /etc/systemd/system/multi-user.target.wants/kingres.service'
check_eq "package state" "not-installed" "$(x dpkg-query -W -f '${db:Status-Status}' kingres 2>/dev/null || echo not-installed)"
check "no kingres files left outside the package manager's knowledge" \
    sh_ '! test -e /opt/kingres && ! test -e /run/kingres'

summary

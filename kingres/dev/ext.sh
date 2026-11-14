#!/usr/bin/env bash
#
# kingres/dev/ext.sh
#
# Build and test one KingresDB extension (contrib/kingres_*) against the
# KingresDB installation produced by kingres/dev/build.sh, without
# rebuilding the server.  This is the fast inner loop for extension work.
#
# The extension is built with PGXS in its own copy under the work area, so
# several extensions can be built and tested at the same time.
#
# Usage:
#   kingres/dev/ext.sh <extension> [command...]
#
#   <extension>   directory name under contrib/, e.g. kingres_shard
#
# Commands (run in the order given; default is "all"):
#   sync      copy contrib/<extension> into the work area
#   build     compile with PGXS
#   install   install into the KingresDB installation
#   regress   start a throwaway server with the extension preloaded and run
#             the pg_regress tests ("make installcheck")
#   tap       run the TAP tests under t/ (they start their own servers)
#   all       sync, build, install, regress, tap
#
# Environment:
#   KINGRES_WORK   work area (default: $HOME/kingres-work)
#   KINGRES_JOBS   parallel jobs for make (default: nproc)
#   EXT_PRELOAD     value of shared_preload_libraries for the throwaway
#                   server (default: the extension name)
#   PROVE_TESTS     TAP test files to run (default: all of t/*.pl)

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"

work_dir="${KINGRES_WORK:-${HOME}/kingres-work}"
jobs="${KINGRES_JOBS:-$(nproc)}"
install_dir="${work_dir}/install"
pg_config="${install_dir}/bin/pg_config"

log() {
    printf '\n==> %s\n' "$*"
}

die() {
    printf 'ext.sh: error: %s\n' "$*" >&2
    exit 1
}

[ "$#" -ge 1 ] || die "missing extension name (see the header of this script)"
ext="$1"
shift

src_dir="${repo_root}/contrib/${ext}"
ext_dir="${work_dir}/ext/${ext}"
data_dir="${work_dir}/ext/${ext}.pgdata"
preload="${EXT_PRELOAD:-${ext}}"

[ -d "${src_dir}" ] || die "no such extension directory: ${src_dir}"
[ -x "${pg_config}" ] || die "no KingresDB installation at ${install_dir}; run kingres/dev/build.sh first"
[ "$(id -u)" -ne 0 ] || die "do not run as root; PostgreSQL refuses to start as root"

# A port derived from the extension name, so two extensions tested at the same
# time do not collide.  Range 55000-55999.
port=$((55000 + $(printf '%s' "${ext}" | cksum | cut -d ' ' -f 1) % 1000))

run_make() {
    make -C "${ext_dir}" USE_PGXS=1 PG_CONFIG="${pg_config}" "$@"
}

stop_server() {
    if [ -f "${data_dir}/postmaster.pid" ]; then
        "${install_dir}/bin/pg_ctl" -D "${data_dir}" -m immediate -w stop >/dev/null 2>&1 || true
    fi
}

cmd_sync() {
    log "copying ${src_dir} -> ${ext_dir}"
    mkdir -p "${ext_dir}"
    rsync -a --delete --exclude 'results/' --exclude 'tmp_check/' --exclude '*.o' --exclude '*.so' \
        "${src_dir}/" "${ext_dir}/"
    chmod -R u=rwX,go=rX "${ext_dir}"
}

cmd_build() {
    log "building ${ext}"
    run_make -j "${jobs}"
}

cmd_install() {
    log "installing ${ext}"
    run_make install >/dev/null
}

cmd_regress() {
    log "running pg_regress tests for ${ext} on port ${port}"
    stop_server
    rm -rf "${data_dir}"
    "${install_dir}/bin/initdb" -D "${data_dir}" --no-sync --auth trust >/dev/null
    {
        echo "port = ${port}"
        echo "listen_addresses = ''"
        echo "unix_socket_directories = '/tmp'"
        echo "shared_preload_libraries = '${preload}'"
        echo "max_prepared_transactions = 20"
        echo "fsync = off"
    } >>"${data_dir}/postgresql.conf"
    # The in-tree (meson) run of the same tests starts its server with the
    # extension's own settings file.  Apply it here too, after the defaults
    # above, so that both ways of running the tests see the same settings and
    # share one set of expected outputs.
    if [ -f "${ext_dir}/${ext}.conf" ]; then
        cat "${ext_dir}/${ext}.conf" >>"${data_dir}/postgresql.conf"
    fi
    trap stop_server EXIT
    "${install_dir}/bin/pg_ctl" -D "${data_dir}" -l "${data_dir}/server.log" -w start >/dev/null
    local status=0
    # TAP_TESTS= (empty) keeps "make installcheck" from also running the TAP
    # tests of an extension that sets TAP_TESTS = 1; the "tap" command runs them.
    PGHOST=/tmp PGPORT="${port}" run_make installcheck TAP_TESTS= || status=$?
    stop_server
    trap - EXIT
    if [ "${status}" -ne 0 ]; then
        # Show the differences so a failing run is diagnosable from the output.
        if [ -f "${ext_dir}/regression.diffs" ]; then
            cat "${ext_dir}/regression.diffs"
        fi
        die "pg_regress tests failed (server log: ${data_dir}/server.log)"
    fi
}

cmd_tap() {
    if ! ls "${ext_dir}"/t/*.pl >/dev/null 2>&1; then
        log "no TAP tests under t/; skipping"
        return
    fi
    log "running TAP tests for ${ext}"
    local pgxs_dir
    pgxs_dir="$(dirname "$("${pg_config}" --pgxs)")/../.."
    rm -rf "${ext_dir}/tmp_check"
    mkdir -p "${ext_dir}/tmp_check"
    (
        cd "${ext_dir}"
        # The same environment PGXS sets up for "make installcheck" with
        # TAP_TESTS, written out so the tests can be run selectively.
        export PATH="${install_dir}/bin:${PATH}"
        export TESTLOGDIR="${ext_dir}/tmp_check/log"
        export TESTDATADIR="${ext_dir}/tmp_check"
        export PG_REGRESS="${pgxs_dir}/src/test/regress/pg_regress"
        export top_builddir="${pgxs_dir}"
        # shellcheck disable=SC2086
        prove -I "${pgxs_dir}/src/test/perl" -I "${ext_dir}" --timer ${PROVE_TESTS:-t/*.pl}
    )
}

if [ "$#" -eq 0 ]; then
    set -- all
fi

for command in "$@"; do
    case "${command}" in
        sync)    cmd_sync ;;
        build)   cmd_build ;;
        install) cmd_install ;;
        regress) cmd_regress ;;
        tap)     cmd_tap ;;
        all)     cmd_sync; cmd_build; cmd_install; cmd_regress; cmd_tap ;;
        *)       die "unknown command '${command}' (see the header of this script)" ;;
    esac
done

log "done"

#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# kingres/dev/build.sh
#
# Build, install and test KingresDB on Linux (native or WSL2).
#
# Why this script exists: the repository usually lives on a Windows drive and
# is seen from WSL2 through /mnt/<drive>.  That filesystem is slow and does
# not keep Unix permissions, and PostgreSQL refuses to run on a data directory
# whose permissions are not 0700.  So when the source tree is on /mnt/*, this
# script first mirrors it into the Linux filesystem and builds the mirror.
# On a native Linux checkout the tree is built in place (out of tree).
#
# Usage:
#   kingres/dev/build.sh [command...]
#
# Commands (run in the order given; default is "all"):
#   sync       mirror the source tree into the work area (only when on /mnt/*)
#   configure  run "meson setup" (or reconfigure an existing build directory)
#   build      compile with ninja
#   install    install into the private prefix
#   test       run the core regression suite (suites "setup" and "regress")
#   test-all   run every test suite, including TAP tests
#   all        sync, configure, build, install, test
#
# Environment:
#   KINGRES_WORK   work area for the mirror, build and install directories
#                   (default: $HOME/kingres-work)
#   KINGRES_JOBS   parallel jobs for ninja and meson test (default: nproc)
#
# PostgreSQL must not be run as root, so run this as an unprivileged user.

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"

work_dir="${KINGRES_WORK:-${HOME}/kingres-work}"
jobs="${KINGRES_JOBS:-$(nproc)}"

build_dir="${work_dir}/build"
install_dir="${work_dir}/install"

# Decide where the compiler reads the sources from.
case "${repo_root}" in
    /mnt/*) src_dir="${work_dir}/src"; needs_sync=1 ;;
    *)      src_dir="${repo_root}";    needs_sync=0 ;;
esac

log() {
    printf '\n==> %s\n' "$*"
}

die() {
    printf 'build.sh: error: %s\n' "$*" >&2
    exit 1
}

if [ "$(id -u)" -eq 0 ]; then
    die "do not run as root; PostgreSQL refuses to start as root"
fi

cmd_sync() {
    if [ "${needs_sync}" -eq 0 ]; then
        log "source tree is on a Linux filesystem; nothing to sync"
        return
    fi
    log "mirroring ${repo_root} -> ${src_dir}"
    mkdir -p "${src_dir}"
    # --delete keeps the mirror exact.  Version-control data and JavaScript
    # dependencies are not needed to build the server and are large.
    rsync -a --delete \
        --exclude '/.git/' \
        --exclude 'node_modules/' \
        --exclude '/kingres/web/dist/' \
        "${repo_root}/" "${src_dir}/"
    # Files copied from a Windows drive all arrive as mode 0777.  Normalize
    # them so that tools which check permissions behave as on a real checkout.
    chmod -R u=rwX,go=rX "${src_dir}"
}

cmd_configure() {
    [ -f "${src_dir}/meson.build" ] || die "no source tree at ${src_dir}; run 'sync' first"
    if [ -f "${build_dir}/build.ninja" ]; then
        log "reconfiguring ${build_dir}"
        meson setup --reconfigure "${build_dir}" "${src_dir}"
    else
        log "configuring ${build_dir}"
        # cassert enables the internal assertion checks every developer build
        # should run with; tap_tests makes the Perl TAP suites mandatory so a
        # missing IPC::Run is an error instead of silently skipped tests.
        meson setup "${build_dir}" "${src_dir}" \
            --prefix "${install_dir}" \
            --buildtype debugoptimized \
            -Dcassert=true \
            -Dtap_tests=enabled \
            -Ddocs=disabled
    fi
}

cmd_build() {
    log "building with ${jobs} jobs"
    ninja -C "${build_dir}" -j "${jobs}"
}

cmd_install() {
    log "installing into ${install_dir}"
    ninja -C "${build_dir}" install >"${work_dir}/install.log"
    "${install_dir}/bin/postgres" --version
}

cmd_test() {
    log "running the core regression suite"
    # The "setup" suite creates the temporary installation the other suites
    # run against, so it must always be selected together with them.
    meson test -C "${build_dir}" --num-processes "${jobs}" --print-errorlogs \
        --suite setup --suite regress
}

cmd_test_all() {
    log "running every test suite"
    meson test -C "${build_dir}" --num-processes "${jobs}" --print-errorlogs
}

if [ "$#" -eq 0 ]; then
    set -- all
fi

for command in "$@"; do
    case "${command}" in
        sync)      cmd_sync ;;
        configure) cmd_configure ;;
        build)     cmd_build ;;
        install)   cmd_install ;;
        test)      cmd_test ;;
        test-all)  cmd_test_all ;;
        all)       cmd_sync; cmd_configure; cmd_build; cmd_install; cmd_test ;;
        *)         die "unknown command '${command}' (see the header of this script)" ;;
    esac
done

log "done"

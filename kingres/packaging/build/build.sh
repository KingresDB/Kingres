#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (c) 2026, RooyeKhat Media Co
#
# kingres/packaging/build/build.sh
#
# The release build recipe of KingresDB.  From a clean source tree it builds
#   1. the server, its client programs and the contrib modules (including every
#      KingresDB extension, contrib/kingres_*) with meson, as an optimized
#      build with assertions OFF;
#   2. the web panel (npm ci, type-check, vite build);
#   3. kingresd with the panel embedded, and kingres-verify when its module
#      exists (static Linux binaries, -trimpath, version embedded);
# and stages the installation tree under STAGE/<prefix> (default prefix
# /opt/kingres).  The staged tree is what the container image and the .deb
# package are made from.
#
# One recipe builds either supported PostgreSQL major.  Which one is decided by
# the source tree it is given: the version of the project() call in its
# meson.build (see source-info.sh), checked again against the server that was
# built.  Nothing is typed twice.  The result says it in share/BUILD-INFO and
# in the files of WORK/info (pg-version, pg-major).
#
# The developer scripts (kingres/dev/*.sh) are separate and unchanged: they
# build with assertions for testing; this script never replaces them.
#
# Usage:
#   build.sh [--src DIR] [--rev REV] [--overlay PATH]... [--work DIR]
#            [--prefix PREFIX] [--jobs N]
#
#   --src DIR       build this source tree as it is (it must be a clean
#                   export: no node_modules, no build directories).  Without
#                   it the script exports a snapshot of REV (default HEAD)
#                   with snapshot.sh, so uncommitted work is never built.
#   --rev REV       commit to export when --src is not given
#   --overlay PATH  passed to snapshot.sh (working-tree files to add)
#   --work DIR      work area (default: $HOME/kingres-packaging).  Receives
#                   src/ (snapshot), build/ (meson), stage/ (installation
#                   tree), info/ (build info and dependency list).
#   --prefix PREFIX installation prefix inside the package (default /opt/kingres)
#   --jobs N        parallel jobs (default: nproc)
#
# Requirements: the pinned Go and Node.js of versions.env on PATH (install them
# with toolchain.sh), and the packages listed in kingres/dev/setup-ubuntu.sh
# plus libsystemd-dev, libxml2-dev, liburing-dev, libnuma-dev, uuid-dev and
# dpkg-dev.  Network access is needed for "npm ci" and "go mod download" only.
#
# Environment:
#   KINGRES_ALLOW_TOOL_MISMATCH=1   accept other Go/Node versions (the result
#                                   is then not the reproducible release)
#   KINGRES_BUILD_AS_ROOT=1         allow running as root (throw-away
#                                   containers only; the image build uses it)
#   SOURCE_DATE_EPOCH               timestamp for the staged files (default:
#                                   the commit time of the snapshot)

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=versions.env
. "${here}/versions.env"

src=""
rev=HEAD
overlays=()
work="${HOME}/kingres-packaging"
prefix=/opt/kingres
jobs="$(nproc)"

while [ "$#" -gt 0 ]; do
    case "$1" in
        --src)     src="$2"; shift 2 ;;
        --rev)     rev="$2"; shift 2 ;;
        --overlay) overlays+=("$2"); shift 2 ;;
        --work)    work="$2"; shift 2 ;;
        --prefix)  prefix="$2"; shift 2 ;;
        --jobs)    jobs="$2"; shift 2 ;;
        *) echo "build.sh: unknown argument '$1' (see the header of this script)" >&2; exit 2 ;;
    esac
done

log() { printf '\n==> %s\n' "$*"; }
die() { printf 'build.sh: error: %s\n' "$*" >&2; exit 1; }

if [ "$(id -u)" -eq 0 ] && [ -z "${KINGRES_BUILD_AS_ROOT:-}" ]; then
    die "run as an unprivileged user (KINGRES_BUILD_AS_ROOT=1 inside a throw-away container)"
fi

# ---------------------------------------------------------------- toolchain
need_tool() {
    local p
    p="$(command -v "$1" || true)"
    [ -n "${p}" ] || die "$1 not found on PATH (see toolchain.sh)"
    case "${p}" in
        /mnt/*) die "$1 resolves to ${p}, a Windows program; put the Linux toolchain first on PATH" ;;
    esac
}
for t in go node npm meson ninja cc; do need_tool "${t}"; done

go_have="$(go env GOVERSION)"
node_have="$(node --version)"
if [ "${go_have}" != "go${GO_VERSION}" ] || [ "${node_have}" != "v${NODE_VERSION}" ]; then
    msg="toolchain mismatch: have ${go_have} and ${node_have}, pinned go${GO_VERSION} and v${NODE_VERSION}"
    [ -n "${KINGRES_ALLOW_TOOL_MISMATCH:-}" ] || die "${msg} (KINGRES_ALLOW_TOOL_MISMATCH=1 overrides)"
    echo "warning: ${msg}" >&2
fi

# ------------------------------------------------------------ source tree
mkdir -p "${work}"
if [ -z "${src}" ]; then
    src="${work}/src"
    rm -rf "${src}"
    snap_args=("${src}" --rev "${rev}")
    for o in "${overlays[@]}"; do snap_args+=(--overlay "${o}"); done
    log "exporting a snapshot of ${rev}"
    "${here}/snapshot.sh" "${snap_args[@]}"
fi
[ -f "${src}/meson.build" ] || die "${src} is not a KingresDB source tree"
commit="$(cat "${src}/.kingres-commit" 2>/dev/null || echo unknown)"

"${here}/source-info.sh" "${src}" >/dev/null || die "cannot read the versions from ${src}/meson.build"
version="$("${here}/source-info.sh" "${src}" kingres_version)"
pg_version="$("${here}/source-info.sh" "${src}" pg_version)"
pg_major="$("${here}/source-info.sh" "${src}" pg_major)"
log "building KingresDB ${version} on PostgreSQL ${pg_version} (major ${pg_major})"

# The KingresDB extensions of this tree: contrib/kingres_*/ with a control
# file.  Every one of them has to end up in the staged tree.
mapfile -t kingres_exts < <(
    for c in "${src}"/contrib/kingres_*/kingres_*.control; do
        [ -e "${c}" ] || continue
        basename "${c}" .control
    done | sort)
[ "${#kingres_exts[@]}" -gt 0 ] || die "no contrib/kingres_*/*.control in ${src}"

if [ -z "${SOURCE_DATE_EPOCH:-}" ]; then
    full="${commit%%+*}"
    if [ "${full}" != unknown ] && git -c safe.directory='*' -C "${here}" cat-file -e "${full}" 2>/dev/null; then
        SOURCE_DATE_EPOCH="$(git -c safe.directory='*' -C "${here}" show -s --format=%ct "${full}")"
    else
        SOURCE_DATE_EPOCH="$(date +%s)"
    fi
fi
export SOURCE_DATE_EPOCH

build_dir="${work}/build"
stage="${work}/stage"
info="${work}/info"
rm -rf "${stage}" "${info}"
mkdir -p "${info}"

# ----------------------------------------------------------------- server
# Every option, and why:
#   --buildtype=release   -O3 and no debug info.  Assertions are separate.
#   -Dcassert=false       assertions off: they cost speed and the product
#                         must not abort on an internal consistency check.
#   -Dtap_tests=disabled  tests are not part of the product (developers run them
#                         with kingres/dev and the test scripts of this directory).
#   -Ddocs=disabled       the documentation is not built or installed.
#   -Dicu=enabled         ICU collations; required for portable sorting.
#   -Dssl=openssl         TLS for client and replication connections.
#   -Dlz4/-Dzstd=enabled  compression for TOAST, WAL, base backups.
#   -Dreadline=enabled    line editing in psql (not libedit: licence/behaviour).
#   -Dzlib=enabled        pg_dump -Fc, base backup compression.
#   -Dsystemd=enabled     sd_notify support; harmless when the server is not
#                         started by systemd.
#   -Dlibxml=enabled      SQL/XML functions that applications expect.
#   -Duuid=e2fs           uuid-ossp through libuuid (util-linux), already on
#                         every Ubuntu system.
#   -Dliburing=enabled    io_method=io_uring of PostgreSQL 18 (the default
#                         method, worker, does not need it).
#   -Dlibnuma=enabled     NUMA-aware memory placement (numa views).
#   -Dsystem_tzdata=/usr/share/zoneinfo
#                         time zone rules come from the operating system's
#                         tzdata package, so time zone updates need no new
#                         KingresDB build.  The package depends on tzdata.
#   -Dllvm=disabled       no JIT: avoids a large LLVM runtime dependency.
#   -Dgssapi/ldap/pam/selinux/libcurl/libxslt/nls/plperl/plpython/pltcl=disabled
#                         not part of the first release; each adds a run-time
#                         dependency and an attack surface that is not needed
#                         yet (see packaging/README.md, "What is not included").
#   -Dbonjour/-Ddtrace/-Dbsd_auth=disabled  not on Linux / not wanted.
#   --libdir lib          libraries in <prefix>/lib (not the multiarch directory),
#                         so that the layout is the same in every container.
#   -Dc_link_args=-Wl,-z,relro,-z,now
#                         full RELRO and immediate binding (hardening).
#   -Drpath=true          (default) the installed binaries find libpq in
#                         <prefix>/lib without ld.so configuration.
# pgport, blocksize and the other compile-time constants keep the upstream
# defaults so that data directories and drivers behave as documented.
meson_opts=(
    --prefix "${prefix}"
    --libdir lib
    --buildtype release
    -Dcassert=false
    -Dtap_tests=disabled
    -Ddocs=disabled
    -Dicu=enabled
    -Dssl=openssl
    -Dlz4=enabled
    -Dzstd=enabled
    -Dreadline=enabled
    -Dzlib=enabled
    -Dsystemd=enabled
    -Dlibxml=enabled
    -Duuid=e2fs
    -Dliburing=enabled
    -Dlibnuma=enabled
    -Dsystem_tzdata=/usr/share/zoneinfo
    -Dllvm=disabled
    -Dgssapi=disabled
    -Dldap=disabled
    -Dpam=disabled
    -Dselinux=disabled
    -Dlibcurl=disabled
    -Dlibxslt=disabled
    -Dnls=disabled
    -Dplperl=disabled
    -Dplpython=disabled
    -Dpltcl=disabled
    -Dbonjour=disabled
    -Ddtrace=disabled
    -Dbsd_auth=disabled
    "-Dc_link_args=['-Wl,-z,relro,-z,now']"
)

log "configuring the server (release build, assertions off)"
rm -rf "${build_dir}"
meson setup "${build_dir}" "${src}" "${meson_opts[@]}"
log "compiling with ${jobs} jobs"
ninja -C "${build_dir}" -j "${jobs}"
log "staging the server into ${stage}"
DESTDIR="${stage}" meson install -C "${build_dir}" --no-rebuild --strip >"${info}/meson-install.log"

# Development files are not part of the runtime package: C headers, static
# libraries, pkg-config files, the PGXS build kit, ecpg and its libraries.
# (Building third-party extensions needs a future kingres-dev package.)
rm -rf "${stage}${prefix}/include" "${stage}${prefix}/lib/pkgconfig" \
       "${stage}${prefix}/lib/postgresql/pgxs"
rm -f  "${stage}${prefix}"/lib/*.a "${stage}${prefix}"/lib/libpq.so \
       "${stage}${prefix}"/lib/libecpg* "${stage}${prefix}"/lib/libpgtypes* \
       "${stage}${prefix}/bin/ecpg"

# ------------------------------------------------------------------- panel
web="${src}/kingres/web"
cp_dir="${src}/kingres/control-plane"
[ -d "${web}" ] || die "missing ${web}"
log "building the web panel"
(
    cd "${web}"
    npm ci --no-audit --no-fund
    npm run build
)
[ -f "${web}/dist/index.html" ] || die "the panel build produced no dist/index.html"
embed="${cp_dir}/internal/webui/dist"
rm -rf "${embed}"
mkdir -p "${embed}"
cp -a "${web}/dist/." "${embed}/"

# ------------------------------------------------------------ Go binaries
export GOTOOLCHAIN=local      # never download another Go release
export GOFLAGS=-mod=readonly  # go.mod and go.sum must already be complete
export CGO_ENABLED=0          # static binaries without libc
export GOOS=linux GOARCH=amd64
bin="${stage}${prefix}/bin"
mkdir -p "${bin}"

go_build() { # go_build MODULE_DIR OUTPUT PACKAGE
    ( cd "$1"
      go mod download
      go build -trimpath -buildvcs=false -buildmode=pie \
          -ldflags "-s -w -X main.version=${version} -X main.commit=${commit}" \
          -o "$2" "$3" )
}

log "building kingresd ${version}"
go_build "${cp_dir}" "${bin}/kingresd" ./cmd/kingresd
# The lease watchdog stops the instance when kingresd hangs or dies and the
# lease of the group runs out; kingresd looks for it next to itself.
go_build "${cp_dir}" "${bin}/kingres-watchdog" ./cmd/kingres-watchdog

if [ -f "${src}/kingres/verifier/go.mod" ]; then
    log "building kingres-verify ${version}"
    # The one script that knows the reproducible flags of the verifier builds it, so that this
    # binary is the same bytes as the linux/amd64 file of a release of the same version made with
    # the same Go release (kingres/verifier/scripts/release.sh).  It is a copy for convenience: an
    # auditor must not rely on a verifier that came from the cluster it checks.
    bash "${src}/kingres/verifier/scripts/build-one.sh" linux amd64 "${version}" "${bin}/kingres-verify" ||
        die "kingres/verifier has a go.mod but does not build"
    verify_status="built"
else
    # HOOK: kingres-verify is not in this source tree yet.  When
    # kingres/verifier/go.mod appears, the block above builds it, and the
    # image and the package pick it up from the staged bin directory.
    verify_status="absent from the source tree (hook in build.sh)"
    log "kingres/verifier has no go.mod: kingres-verify is skipped"
fi

# ------------------------------------------------------------ dependencies
# The run-time packages of the dynamically linked programs.  dpkg-shlibdeps
# needs a debian/control file to exist, so a throw-away tree is created.  The
# Go programs are static and are left out.
log "computing run-time package dependencies"
command -v dpkg-shlibdeps >/dev/null || die "dpkg-shlibdeps not found (apt install dpkg-dev)"
tmp="$(mktemp -d)"
mkdir -p "${tmp}/debian"
printf 'Source: kingres\n\nPackage: kingres\nArchitecture: amd64\n' >"${tmp}/debian/control"
# The staged tree stands in for the package's own build directory, so that
# our private libraries (libpq, libecpg, ...) are recognised as belonging to
# the package instead of being looked up in the system.
ln -s "${stage}" "${tmp}/debian/kingres"
mapfile -t elfs < <(
    { find "${tmp}/debian/kingres${prefix}/bin/" -type f ! -name kingresd ! -name kingres-verify
      find "${tmp}/debian/kingres${prefix}/lib/" -type f -name '*.so*'; } | sort)
shl_args=()
for f in "${elfs[@]}"; do shl_args+=("-e${f}"); done
(
    cd "${tmp}"
    dpkg-shlibdeps -O --warnings=1 "${shl_args[@]}" \
        2>"${info}/shlibdeps.warnings" | sed 's/^shlibs:Depends=//' >"${info}/shlibs.depends"
)
rm -rf "${tmp}"
[ -s "${info}/shlibs.depends" ] || die "no dependency list was produced"

# --------------------------------------------- documents the package carries
# The text of the licence of KingresDB's own code and the lists of the
# third-party modules compiled into kingresd (the console is part of it).  The
# image and the package both take them from here.
mkdir -p "${info}/doc"
grep -q "^ *GNU AFFERO GENERAL PUBLIC LICENSE" "${src}/LICENSE" 2>/dev/null ||
    die "${src}/LICENSE is not the text of the GNU Affero General Public License"
cp "${src}/LICENSE" "${info}/doc/AGPL-3.0.txt"
for d in control-plane web; do
    [ -f "${src}/kingres/${d}/DEPENDENCIES.md" ] || die "missing ${src}/kingres/${d}/DEPENDENCIES.md"
    cp "${src}/kingres/${d}/DEPENDENCIES.md" "${info}/doc/DEPENDENCIES.${d}.md"
done

# ----------------------------------------------------------- verification
log "checking the staged tree"
for f in postgres initdb pg_ctl psql pg_dump pg_basebackup pg_isready kingresd; do
    [ -x "${bin}/${f}" ] || die "missing staged program ${bin}/${f}"
done
ext_share="${stage}${prefix}/share/postgresql/extension"
ext_dir="${stage}${prefix}/lib/postgresql"
kingres_ext_versions=()
for e in "${kingres_exts[@]}"; do
    ctl="${src}/contrib/${e}/${e}.control"
    [ -f "${ext_share}/${e}.control" ] || die "control file of ${e} was not installed"
    # A module_pathname in the control file means a C library.
    if grep -q '^module_pathname' "${ctl}"; then
        [ -f "${ext_dir}/${e}.so" ] || die "extension library ${e}.so was not installed"
    fi
    default_version="$(sed -n "s/^default_version[[:space:]]*=[[:space:]]*'\\([^']*\\)'.*/\\1/p" "${ctl}")"
    [ -f "${ext_share}/${e}--${default_version}.sql" ] ||
        die "install script ${e}--${default_version}.sql of ${e} was not installed"
    kingres_ext_versions+=("${e} ${default_version}")
    # Every install and update script of the source tree is in the staged tree
    # (a new version that is missing from the install list of meson.build
    # would give an extension that cannot be installed or updated), nothing
    # else of that name is, and the DATA list of the Makefile (the build
    # outside the tree) names the same files.
    src_scripts="$(find "${src}/contrib/${e}" -maxdepth 1 -name "${e}--*.sql" -printf '%f\n' | sort)"
    [ -n "${src_scripts}" ] || die "${e} has no install script in the source tree"
    staged_scripts="$(find "${ext_share}" -maxdepth 1 -name "${e}--*.sql" -printf '%f\n' | sort)"
    [ "${src_scripts}" = "${staged_scripts}" ] ||
        die "the scripts of ${e} in the staged tree differ from the source tree: source [$(echo "${src_scripts}" | tr '\n' ' ')] staged [$(echo "${staged_scripts}" | tr '\n' ' ')]"
    make_scripts="$(grep -o "${e}--[0-9][0-9.]*\\(--[0-9][0-9.]*\\)\\?\\.sql" "${src}/contrib/${e}/Makefile" | sort -u)"
    [ "${src_scripts}" = "${make_scripts}" ] ||
        die "the DATA list of the Makefile of ${e} differs from its scripts: Makefile [$(echo "${make_scripts}" | tr '\n' ' ')] files [$(echo "${src_scripts}" | tr '\n' ' ')]"
done
# And nothing else named kingres_* may be there that the source does not have.
for f in "${ext_share}"/kingres_*.control; do
    case " ${kingres_exts[*]} " in *" $(basename "${f}" .control) "*) ;; *) die "unexpected extension $(basename "${f}")" ;; esac
done
# The major of the server that was built is the one of the source tree.
built_major="$(sed -n 's/^#define PG_MAJORVERSION "\(.*\)"/\1/p' "${build_dir}/src/include/pg_config.h")"
[ "${built_major}" = "${pg_major}" ] || die "the tree says PostgreSQL ${pg_major}, the server built is ${built_major}"
"${bin}/postgres" --version | grep -q "^postgres (PostgreSQL) ${pg_version}\$" ||
    die "postgres --version does not report PostgreSQL ${pg_version}: $("${bin}/postgres" --version)"
readelf -d "${bin}/postgres" | grep -q BIND_NOW || die "the server was not linked with immediate binding"
"${bin}/kingresd" -version | grep -q "${commit}" || die "kingresd -version does not show the build commit"
ldd "${bin}/kingresd" 2>&1 | grep -Eq 'statically linked|not a dynamic' ||
    die "kingresd is dynamically linked"
if grep -q "^#define USE_ASSERT_CHECKING" "${build_dir}/src/include/pg_config.h"; then
    die "assertions are enabled in this build"
fi

# ------------------------------------------------------------- build info
bi="${stage}${prefix}/share/BUILD-INFO"
mkdir -p "$(dirname "${bi}")"
{
    echo "KingresDB ${version}"
    echo "PostgreSQL: ${pg_version} (major ${pg_major})"
    echo "kingres extensions: ${kingres_exts[*]}"
    echo "kingres extension versions: $(printf '%s, ' "${kingres_ext_versions[@]}" | sed 's/, $//')"
    echo "source commit: ${commit}"
    echo "prefix: ${prefix}"
    echo "kingres-verify: ${verify_status}"
    if [ "${verify_status}" = built ]; then
        echo "kingres-verify sha256: $(sha256sum "${bin}/kingres-verify" | cut -d" " -f1) (built by kingres/verifier/scripts/build-one.sh, flags of its own)"
    fi
    echo "go: $(go version)"
    echo "node: $(node --version)  npm: $(npm --version)"
    echo "meson: $(meson --version)  ninja: $(ninja --version)"
    echo "compiler: $(cc --version | head -1)"
    echo "libc: $(ldd --version | head -1)"
    echo "SOURCE_DATE_EPOCH: ${SOURCE_DATE_EPOCH}"
    echo "meson options: ${meson_opts[*]}"
    echo "go flags: CGO_ENABLED=0 -trimpath -buildmode=pie -ldflags '-s -w -X main.version=${version} -X main.commit=${commit}'"
} >"${bi}"
cp "${bi}" "${info}/BUILD-INFO"

# Normalize timestamps so that identical inputs give identical archives.
find "${stage}" -exec touch -h --date="@${SOURCE_DATE_EPOCH}" {} +
chmod -R go-w "${stage}"

printf '%s\n' "${version}" >"${info}/version"
printf '%s\n' "${pg_version}" >"${info}/pg-version"
printf '%s\n' "${pg_major}" >"${info}/pg-major"
printf '%s\n' "${kingres_exts[@]}" >"${info}/extensions"
printf '%s\n' "${kingres_ext_versions[@]}" >"${info}/extension-versions"
printf '%s\n' "${prefix}" >"${info}/prefix"
( cd "${stage}" && find . -type f -print0 | sort -z | xargs -0 sha256sum ) >"${info}/SHA256SUMS"

log "done: KingresDB ${version} on PostgreSQL ${pg_version} staged in ${stage}${prefix}"
du -sh "${stage}${prefix}"

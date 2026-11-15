#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (c) 2026, RooyeKhat Media Co
#
# kingres/packaging/deb/build-deb.sh
#
# Build kingres_<version>+pg<major>-<revision>_amd64.deb from the tree that
# kingres/packaging/build/build.sh staged.  The PostgreSQL major is the one the
# build recorded (WORK/info/pg-major); it is part of the package version, so
# one package name serves both majors and dpkg never has two of them.
#
# Usage and options are below.  No source is compiled here: the
# package is the staged tree plus the files of this directory.
#
# Usage:
#   build-deb.sh [--work DIR] [--out DIR] [--revision N] [--lint]
#
#   --work DIR     the work area of build.sh (default: $HOME/kingres-packaging)
#   --out DIR      where the .deb is written (default: WORK/deb)
#   --revision N   Debian revision (default 1)
#   --lint         run lintian on the result and fail on errors
#
# The result is reproducible for a given staged tree: file times are the
# commit time (SOURCE_DATE_EPOCH, taken from the staged files), owners are
# root, and the archive is xz-compressed with fixed options.
#
# Layout of the package (see packaging/README.md):
#   /opt/kingres/...                     the product (staged tree)
#   /usr/bin/kingres-psql, kingresd ...  symbolic links to /opt/kingres/bin
#   /usr/lib/systemd/system/kingres.service
#   /etc/kingres/kingresd.json, kingresd.env     (conffiles)
#   /usr/share/doc/kingres/...           copyright, AGPL-3.0.txt (the licence text),
#                                        changelog, README.Debian, dependency lists
#   /var/lib/kingres                     created by the postinst, not shipped

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
work="${HOME}/kingres-packaging"
out=""
revision=1
lint=0

while [ "$#" -gt 0 ]; do
    case "$1" in
        --work)     work="$2"; shift 2 ;;
        --out)      out="$2"; shift 2 ;;
        --revision) revision="$2"; shift 2 ;;
        --lint)     lint=1; shift ;;
        *) echo "build-deb.sh: unknown argument '$1' (see the header of this script)" >&2; exit 2 ;;
    esac
done
out="${out:-${work}/deb}"

die() { printf 'build-deb.sh: error: %s\n' "$*" >&2; exit 1; }

stage="${work}/stage"
info="${work}/info"
if [ ! -d "${stage}" ] || [ ! -f "${info}/version" ]; then die "no staged tree in ${work}; run build/build.sh first"; fi
version="$(cat "${info}/version")"
pg_version="$(cat "${info}/pg-version")"
pg_major="$(cat "${info}/pg-major")"
prefix="$(cat "${info}/prefix")"
[ "${prefix}" = /opt/kingres ] || die "the package layout needs the prefix /opt/kingres, not ${prefix}"
deb_version="${version}+pg${pg_major}"
full_version="${deb_version}-${revision}"
shlibs="$(cat "${info}/shlibs.depends")"
epoch="$(sed -n 's/^SOURCE_DATE_EPOCH: //p' "${stage}${prefix}/share/BUILD-INFO")"
[ -n "${epoch}" ] || die "no SOURCE_DATE_EPOCH in the build info"

root="${work}/deb-root"
rm -rf "${root}"
mkdir -p "${root}" "${out}"

# ---- the product ------------------------------------------------------------
mkdir -p "${root}/opt"
cp -a "${stage}${prefix}" "${root}${prefix}"

# ---- command-line names -----------------------------------------------------
# The clients keep their upstream names inside /opt/kingres/bin and get a
# "kingres-" prefix on the PATH, because /usr/bin/psql, pg_dump and the others
# belong to the distribution's postgresql-client packages (and to any other
# PostgreSQL on the machine).  A prefix cannot collide with those, needs no
# update-alternatives, and makes it plain which server a tool talks to.
mkdir -p "${root}/usr/bin"
for c in psql pg_dump pg_dumpall pg_restore pg_basebackup pg_isready pg_config; do
    [ -x "${root}${prefix}/bin/${c}" ] || die "missing client program ${c}"
    ln -s "${prefix}/bin/${c}" "${root}/usr/bin/kingres-${c}"
done
ln -s "${prefix}/bin/kingresd" "${root}/usr/bin/kingresd"
if [ -x "${root}${prefix}/bin/kingres-verify" ]; then
    ln -s "${prefix}/bin/kingres-verify" "${root}/usr/bin/kingres-verify"
fi

# ---- service, configuration, documentation ----------------------------------
install -D -m 0644 "${here}/kingres.service" "${root}/usr/lib/systemd/system/kingres.service"
install -D -m 0644 "${here}/kingresd.json" "${root}/etc/kingres/kingresd.json"
install -D -m 0600 "${here}/kingresd.env" "${root}/etc/kingres/kingresd.env"
doc="${root}/usr/share/doc/kingres"
install -D -m 0644 "${here}/copyright" "${doc}/copyright"
install -D -m 0644 "${here}/README.Debian" "${doc}/README.Debian"
sed -e "s/@DEB_VERSION@/${deb_version}/" -e "s/@REVISION@/${revision}/" \
    -e "s/@PG_MAJOR@/${pg_major}/" -e "s/@PG_VERSION@/${pg_version}/" "${here}/changelog" |
    gzip -9n >"${doc}/changelog.Debian.gz"
chmod 0644 "${doc}/changelog.Debian.gz"
install -D -m 0644 "${here}/lintian-overrides" "${root}/usr/share/lintian/overrides/kingres"
# The licence text of KingresDB's own code and the third-party dependency lists,
# which build.sh took from the sources it built.
for f in AGPL-3.0.txt DEPENDENCIES.control-plane.md DEPENDENCIES.web.md; do
    [ -f "${info}/doc/${f}" ] || die "${info}/doc/${f} is missing; run build/build.sh again"
    install -D -m 0644 "${info}/doc/${f}" "${doc}/${f}"
done
# BUILD-INFO also lives with the documentation.
ln -s "${prefix}/share/BUILD-INFO" "${doc}/BUILD-INFO"

# ---- manual pages ------------------------------------------------------------
# One short page per command on the PATH.  The clients are the PostgreSQL major of the build
# programs of the same name; their full reference is the PostgreSQL manual.
man_page() { # man_page NAME SECTION SUMMARY TEXT
    local name="$1" sec="$2" summary="$3" text="$4"
    local dir="${root}/usr/share/man/man${sec}"
    mkdir -p "${dir}"
    {
        printf '.TH %s %s "KingresDB" "KingresDB %s"\n' "$(echo "${name}" | tr '[:lower:]' '[:upper:]')" "${sec}" "${version}"
        printf '.SH NAME\n%s \\- %s\n' "${name}" "${summary}"
        printf '.SH DESCRIPTION\n%s\n' "${text}"
        printf '.SH FILES\n/opt/kingres/share/BUILD-INFO\n.br\n/usr/share/doc/kingres/README.Debian\n'
        printf '.SH AUTHOR\nRooyeKhat Media Co\n'
    } | sed -e 's/\\-/-/g' -e 's/-/\\-/g' | gzip -9n >"${dir}/${name}.${sec}.gz"
}
for c in psql pg_dump pg_dumpall pg_restore pg_basebackup pg_isready pg_config; do
    man_page "kingres-${c}" 1 "the PostgreSQL ${c} program of KingresDB" \
"This is ${c} of PostgreSQL ${pg_major}, installed in /opt/kingres/bin/${c} and available under this name so that it does not collide with the distribution's PostgreSQL packages. Its options and behaviour are those of ${c} in the PostgreSQL ${pg_major} documentation."
done
man_page kingresd 8 "the KingresDB node service: management panel, REST API and instance manager" \
"kingresd serves the web management panel and the REST API on one port (default 127.0.0.1:8091). It is started by the systemd unit kingres.service as the user kingres. Settings come from a JSON file (option -config, default /etc/kingres/kingresd.json), from environment variables KINGRES_* (see /etc/kingres/kingresd.env) and from command-line options, in increasing precedence. Run kingresd -h for the list. While the node is uninitialized, the one-time setup token is in /var/lib/kingres/setup-token."
man_page kingres-verify 1 "verify the tamper-evident ledger of KingresDB without trusting the database" \
"kingres-verify checks the digests and inclusion proofs of the kingres_ledger extension independently of the server. Run kingres-verify --help for its commands."
if [ ! -x "${root}${prefix}/bin/kingres-verify" ]; then
    rm -f "${root}/usr/share/man/man1/kingres-verify.1.gz"
fi

# ---- permissions -------------------------------------------------------------
# The staged files come from a build user; the package owns everything as root
# and nothing is writable by group or others.
find "${root}" -type d -exec chmod 0755 {} +
find "${root}${prefix}" -type f -perm -u+x -exec chmod 0755 {} +
find "${root}${prefix}" -type f ! -perm -u+x -exec chmod 0644 {} +
find "${root}${prefix}/lib" -type f -name '*.so*' -exec chmod 0644 {} +
chmod 0600 "${root}/etc/kingres/kingresd.env"

# ---- control files -----------------------------------------------------------
ctl="${root}/DEBIAN"
mkdir -p "${ctl}"
install -m 0755 "${here}/postinst" "${here}/prerm" "${here}/postrm" "${ctl}/"
# The preinst knows the major of this package (it refuses the other one).
sed "s/^PG_MAJOR=@PG_MAJOR@$/PG_MAJOR=${pg_major}/" "${here}/preinst" >"${ctl}/preinst"
chmod 0755 "${ctl}/preinst"
grep -q "^PG_MAJOR=${pg_major}$" "${ctl}/preinst" || die "the major was not put into the preinst"
printf '/etc/kingres/kingresd.json\n/etc/kingres/kingresd.env\n' >"${ctl}/conffiles"

# Debian policy wants md5sums of every regular file outside DEBIAN.
( cd "${root}" && find . -path ./DEBIAN -prune -o -type f -print0 | sort -z |
    xargs -0 md5sum | sed 's# \./# #' ) >"${ctl}/md5sums"
chmod 0644 "${ctl}/md5sums" "${ctl}/conffiles"

installed_size="$(du -sk --exclude=DEBIAN "${root}" | cut -f1)"
cat >"${ctl}/control" <<EOF
Package: kingres
Version: ${full_version}
Architecture: amd64
Maintainer: Mohammad Rasoul Kazemi (RooyeKhat) <rooyekhat.media@gmail.com>
Installed-Size: ${installed_size}
Depends: ${shlibs}, adduser, init-system-helpers (>= 1.62), tzdata
Recommends: ca-certificates
Section: database
Priority: optional
Homepage: https://github.com/KingresDB/Kingres
Description: KingresDB, a PostgreSQL-based distributed database with a management panel
 KingresDB is a PostgreSQL ${pg_major} (${pg_version}) based database server with the
 kingres_columnar, kingres_ledger, kingres_shard, kingres_sketch and
 kingres_timepart extensions, and kingresd, the control plane that serves the
 web management panel and the REST API on every node.
 .
 One package serves each supported PostgreSQL major: the major is in the
 version (+pg18, +pg19), only one of them can be installed, and the package
 refuses to replace one by the other.
 .
 The package installs one self-contained tree in /opt/kingres, so it does not
 interfere with the distribution's PostgreSQL packages, and one systemd
 service, kingres.service.  After installation the node is uninitialized and
 the panel shows the first-run wizard.  Client programs are available on the
 PATH as kingres-psql, kingres-pg_dump and so on.
EOF
chmod 0644 "${ctl}/control"

# ---- timestamps and archive --------------------------------------------------
find "${root}" -exec touch -h --date="@${epoch}" {} +
export SOURCE_DATE_EPOCH="${epoch}"
deb="${out}/kingres_${full_version}_amd64.deb"
rm -f "${deb}"
dpkg-deb --root-owner-group -Zxz -z6 --build "${root}" "${deb}"
printf 'built %s (%s bytes)\n' "${deb}" "$(stat -c %s "${deb}")"

if [ "${lint}" -eq 1 ]; then
    command -v lintian >/dev/null || die "lintian is not installed"
    # --fail-on error: exit non-zero only for errors; warnings are listed.
    lintian --info --display-info --fail-on error --tag-display-limit 0 "${deb}" |
        tee "${out}/lintian.txt" || die "lintian reported errors (see ${out}/lintian.txt)"
fi

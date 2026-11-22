#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (c) 2026, RooyeKhat Media Co
#
# kingres/packaging/test/test-verifier-package.sh
#
# Tests the part of the package that carries kingres-verify, without Docker and
# without building PostgreSQL (phase 7, step 23).  It builds the verifier with the
# one script both the package and a release use, stages it in a small fake tree
# (the other programs are empty stand-ins), runs the real deb/build-deb.sh on it and
# looks into the package:
#
#   - kingres-verify is installed in /opt/kingres/bin, linked from /usr/bin, with its
#     manual page and the README.Debian text that says an auditor must not rely on it
#   - the unit file carries LimitCORE=0, and build-deb.sh refuses a unit without it
#   - the installed binary runs, and with a release made in the test (a key made in
#     the test, never a real one) it is the very same bytes as the release file of
#     the same version, and `release verify` of the packaged copy accepts that
#     release and rejects a changed binary and a note of another key
#   - release.sh refuses a signed release from a tree that is not clean or whose HEAD is
#     not the tag of the version, unless --dry-run (checked in a throwaway git repository)
#
# The checks that need a running node (systemd shows LimitCORE=0 and the process
# runs with a zero core limit) are in test-deb.sh, which needs Docker.
#
# Usage: test-verifier-package.sh [--work DIR]     (default $HOME/kingres-vpkg-test)
# Needs: bash, go (the release recorded in kingres/verifier/go.mod), dpkg-deb, tar,
# sha256sum.  It makes one directory in DIR and removes it.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
pkg="$(cd "${here}/.." && pwd)"
ver="$(cd "${pkg}/../verifier" && pwd)"
work="${HOME}/kingres-vpkg-test"
while [ "$#" -gt 0 ]; do
    case "$1" in
        --work) work="$2"; shift 2 ;;
        *) echo "test-verifier-package.sh: unknown argument '$1'" >&2; exit 2 ;;
    esac
done
for c in go dpkg-deb tar sha256sum; do
    command -v "${c}" >/dev/null || { echo "test-verifier-package.sh: ${c} is not on the PATH" >&2; exit 2; }
done
# shellcheck source=lib.sh
. "${here}/lib.sh"

mkdir -p "${work}"
run="$(mktemp -d "${work}/run.XXXXXX")"
case "${run}" in "${work}"/run.*) ;; *) echo "unexpected directory ${run}" >&2; exit 2 ;; esac
# shellcheck disable=SC2317  # run by the EXIT trap
cleanup() { chmod -R u+w "${run}" 2>/dev/null || true; rm -rf "${run}"; }
trap cleanup EXIT

version=0.1.0
prefix=/opt/kingres
stage="${run}/w/stage"
info="${run}/w/info"
bin="${stage}${prefix}/bin"
mkdir -p "${bin}" "${stage}${prefix}/share" "${stage}${prefix}/lib" "${info}/doc"

note "build the verifier with the script of the package recipe and put it in a staged tree"
bash "${ver}/scripts/build-one.sh" linux amd64 "${version}" "${bin}/kingres-verify"
for c in psql pg_dump pg_dumpall pg_restore pg_basebackup pg_isready pg_config kingresd; do
    printf '#!/bin/sh\nexit 0\n' >"${bin}/${c}"
    chmod 0755 "${bin}/${c}"
done
sha="$(sha256sum "${bin}/kingres-verify" | cut -d' ' -f1)"
{
    echo "KingresDB ${version}"
    echo "SOURCE_DATE_EPOCH: 1700000000"
    echo "kingres-verify: built"
    echo "kingres-verify sha256: ${sha} (built by kingres/verifier/scripts/build-one.sh, flags of its own)"
} >"${stage}${prefix}/share/BUILD-INFO"
printf '%s' "${version}" >"${info}/version"
printf '%s' "18.7" >"${info}/pg-version"
printf '%s' "18" >"${info}/pg-major"
printf '%s' "${prefix}" >"${info}/prefix"
printf '%s' "libc6 (>= 2.39)" >"${info}/shlibs.depends"
for f in AGPL-3.0.txt DEPENDENCIES.control-plane.md DEPENDENCIES.web.md; do echo "stand-in" >"${info}/doc/${f}"; done

note "build the package with deb/build-deb.sh"
check "build-deb.sh builds the package" bash "${pkg}/deb/build-deb.sh" --work "${run}/w" --out "${run}/deb"
deb="$(ls "${run}"/deb/kingres_*_amd64.deb 2>/dev/null | head -1 || true)"
check "the package exists" test -n "${deb}"
[ -n "${deb}" ] || { summary; exit 1; }
root="${run}/root"
mkdir -p "${root}"
dpkg-deb -x "${deb}" "${root}"
listing="$(dpkg-deb -c "${deb}")"

note "kingres-verify in the package"
check "binary in /opt/kingres/bin, executable" test -x "${root}${prefix}/bin/kingres-verify"
check_eq "linked from /usr/bin" "${prefix}/bin/kingres-verify" "$(readlink "${root}/usr/bin/kingres-verify")"
check "manual page of kingres-verify" test -f "${root}/usr/share/man/man1/kingres-verify.1.gz"
check "the manual page says an auditor must not rely on the copy" sh -c "gzip -dc '${root}/usr/share/man/man1/kingres-verify.1.gz' | grep -q 'must not rely'"
check "README.Debian says an auditor must not rely on the copy and names release verify" \
    sh -c "grep -q 'Do not' '${root}/usr/share/doc/kingres/README.Debian' && grep -q 'release verify' '${root}/usr/share/doc/kingres/README.Debian'"
check "the package lists the binary" sh -c "printf '%s' '${listing}' | grep -q './opt/kingres/bin/kingres-verify\$'"
check_eq "the installed binary is the one that was staged" "${sha}" "$(sha256sum "${root}${prefix}/bin/kingres-verify" | cut -d' ' -f1)"
check "the installed binary runs" "${root}${prefix}/bin/kingres-verify" version
check "the installed binary has release verify" sh -c "'${root}${prefix}/bin/kingres-verify' release --help | grep -q 'release verify'"

note "LimitCORE=0"
unit="${root}/usr/lib/systemd/system/kingres.service"
check "the packaged unit has LimitCORE=0" grep -qx 'LimitCORE=0' "${unit}"
check "the unit has it once, in [Service]" sh -c "[ \$(grep -c '^LimitCORE=' '${unit}') = 1 ] && awk '/^\\[Service\\]/{s=1} /^\\[Install\\]/{s=0} s && /^LimitCORE=0\$/{f=1} END{exit !f}' '${unit}'"
bad="${run}/deb-without-limit"
cp -a "${pkg}/deb" "${bad}"
sed -i '/^LimitCORE=/d' "${bad}/kingres.service"
check "a unit without LimitCORE=0 is not built" sh -c "! bash '${bad}/build-deb.sh' --work '${run}/w' --out '${run}/deb-bad' >/dev/null 2>&1"
check "no package was made from it" sh -c "! ls '${run}'/deb-bad/*.deb >/dev/null 2>&1"
cp "${pkg}/deb/kingres.service" "${bad}/kingres.service"
sed -i 's/^LimitCORE=0$/LimitCORE=infinity/' "${bad}/kingres.service"
check "a unit with another value is not built" sh -c "! bash '${bad}/build-deb.sh' --work '${run}/w' --out '${run}/deb-bad' >/dev/null 2>&1"

note "the packaged copy and a release"
keyfile="${run}/test-release.key"
rel="${run}/release"
# The key is made here and removed with the directory; no key of the project is used.
( cd "${ver}" && go run -trimpath ./cmd/kingres-release-tool keygen --out "${keyfile}" ) >"${run}/keygen.txt"
pubkey="$(sed -n 's/^public key:  //p' "${run}/keygen.txt")"
check "a key was made for the test" test -n "${pubkey}"
check "release.sh makes a signed release (a dry run: this tree is not a tagged clean checkout)" bash "${ver}/scripts/release.sh" --version "${version}" --out "${rel}" \
    --key-file "${keyfile}" --dry-run --targets "linux/amd64 windows/amd64"
check "the release has the note" test -f "${rel}/SHA256SUMS.note"
check "the packaged binary is the release file of the same version, byte for byte" \
    cmp "${root}${prefix}/bin/kingres-verify" "${rel}/kingres-verify-linux-amd64"
packaged="${root}${prefix}/bin/kingres-verify"
check "the packaged copy accepts the release and its binary" "${packaged}" release verify \
    --sums "${rel}/SHA256SUMS" --note "${rel}/SHA256SUMS.note" --key "${pubkey}" --binary "${rel}/kingres-verify-linux-amd64"
cp "${rel}/kingres-verify-linux-amd64" "${run}/kingres-verify-linux-amd64"
printf 'x' >>"${run}/kingres-verify-linux-amd64"
status=0
"${packaged}" release verify --sums "${rel}/SHA256SUMS" --note "${rel}/SHA256SUMS.note" --key "${pubkey}" \
    --binary "${run}/kingres-verify-linux-amd64" >/dev/null 2>&1 || status=$?
check_eq "a changed binary is status 1" 1 "${status}"
status=0
"${packaged}" release verify --sums "${rel}/SHA256SUMS" --note "${rel}/SHA256SUMS.note" \
    --key "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=" >/dev/null 2>&1 || status=$?
check_eq "another key is status 1" 1 "${status}"


note "release.sh makes a signed release only from the tagged commit of a clean checkout (P7R-07)"
if command -v git >/dev/null; then
    g="${run}/gitsrc"
    mkdir -p "${g}/kingres/verifier"
    ( cd "${ver}" && tar -cf - --exclude='testdata' . ) | ( cd "${g}/kingres/verifier" && tar -xf - )
    # the repository of the test is a throwaway one; no tag of the project is made or touched
    gitq() { git -C "${g}" -c user.name=release-test -c user.email=release-test@example.invalid -c commit.gpgsign=false -c core.hooksPath=/dev/null "$@"; }
    gitq init -q
    gitq add -A
    gitq commit -q -m "the tree of the test"
    head="$(gitq rev-parse HEAD)"
    gs="${g}/kingres/verifier/scripts/release.sh"
    rstatus=0
    rtry() { # rtry OUTDIR LOGFILE ARGS...: release.sh for linux/amd64, the exit status in ${rstatus}
        local o="$1" l="$2"
        shift 2
        rstatus=0
        bash "${gs}" --version 9.9.9 --out "${o}" --key-file "${keyfile}" --targets "linux/amd64" "$@" >"${l}" 2>&1 || rstatus=$?
    }
    rtry "${run}/r-notag" "${run}/r-notag.log"
    check_eq "a signed release without a tag is refused" 1 "${rstatus}"
    check "and it names the missing tag" grep -q 'the tag v9.9.9 does not exist' "${run}/r-notag.log"
    check "and it made no file" sh -c "! ls '${run}/r-notag'/* >/dev/null 2>&1"
    rtry "${run}/r-dry" "${run}/r-dry.log" --dry-run
    check_eq "a dry run without a tag makes the files" 0 "${rstatus}"
    check "and says in capitals that it is not a release" grep -q 'DRY RUN: .* NOT a release' "${run}/r-dry.log"
    check "its RELEASE file records no commit" grep -qx 'commit: unknown' "${run}/r-dry/RELEASE"
    # release verify of that set is not a plain pass: status 5 and the words NOT A RELEASE, unless --accept-dry-run asks for status 0
    dstatus=0
    "${packaged}" release verify --sums "${run}/r-dry/SHA256SUMS" --note "${run}/r-dry/SHA256SUMS.note" --key "${pubkey}" \
        --binary "${run}/r-dry/RELEASE" >"${run}/r-dry-verify.log" 2>&1 || dstatus=$?
    check_eq "release verify of a dry-run set ends with status 5" 5 "${dstatus}"
    check "and says NOT A RELEASE" grep -q '^VERIFIED (NOT A RELEASE):' "${run}/r-dry-verify.log"
    dstatus=0
    "${packaged}" release verify --accept-dry-run --sums "${run}/r-dry/SHA256SUMS" --note "${run}/r-dry/SHA256SUMS.note" --key "${pubkey}" \
        --binary "${run}/r-dry/RELEASE" >/dev/null 2>&1 || dstatus=$?
    check_eq "and with --accept-dry-run ends with status 0" 0 "${dstatus}"
    gitq tag v9.9.9
    rtry "${run}/r-ok" "${run}/r-ok.log"
    check_eq "a signed release from the tagged clean commit is made" 0 "${rstatus}"
    check "its RELEASE file names the version and the commit" sh -c "grep -qx 'version: 9.9.9' '${run}/r-ok/RELEASE' && grep -qx 'commit: ${head}' '${run}/r-ok/RELEASE'"
    check "RELEASE is in the signed sums" grep -q '  RELEASE$' "${run}/r-ok/SHA256SUMS"
    pub2="$(cd "${ver}" && go run -trimpath ./cmd/kingres-release-tool pub --key-file "${keyfile}" | sed -n 's/^public key:  //p')"
    check "release verify shows the signed version and commit" sh -c "'${root}${prefix}/bin/kingres-verify' release verify --sums '${run}/r-ok/SHA256SUMS' --note '${run}/r-ok/SHA256SUMS.note' --key '${pub2}' --binary '${run}/r-ok/RELEASE' | grep -q 'version 9.9.9, commit ${head}'"
    echo "a change that is not committed" >"${g}/kingres/verifier/scripts/untracked.txt"
    rtry "${run}/r-dirty" "${run}/r-dirty.log"
    check_eq "a signed release from a dirty tree is refused" 1 "${rstatus}"
    check "and it says the tree is not clean" grep -q 'the working tree is not clean' "${run}/r-dirty.log"
    rm -f "${g}/kingres/verifier/scripts/untracked.txt"
    gitq commit -q --allow-empty -m "after the tag"
    rtry "${run}/r-moved" "${run}/r-moved.log"
    check_eq "a signed release from a commit that is not the tag is refused" 1 "${rstatus}"
    check "and it says HEAD is not the tag" grep -q 'HEAD is not the commit of the tag' "${run}/r-moved.log"
else
    echo "git is not on the PATH: the release.sh checks of the tree and the tag were not run"
fi
summary

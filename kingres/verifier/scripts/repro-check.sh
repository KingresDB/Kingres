#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Reproduction check of kingres-verify (design ledger-phase7.md, 7.7): builds the
# release files twice, from two copies of the source at different paths, each with
# an empty Go build cache and its own GOPATH, and compares them. Every file must be
# byte for byte the same. It exits 0 when they are, 1 when they are not (it prints
# both sums files and the names that differ), 2 on a usage or tool error.
#
# Usage:
#   scripts/repro-check.sh [--work DIR] [--targets LIST] [--version TAG]
#
#   --work DIR      where the two copies and their caches live (default
#                   $HOME/kingres-repro-check). The script makes one new directory in it
#                   and removes that directory before it exits.
#   --targets LIST  as release.sh (default: its four targets)
#   --version TAG   the version both builds are given (default repro-check)
#
# The checks of this project run on a developer's machine; no workflow runs this
# anywhere else. A third party runs the same script on their own checkout, or builds
# once and compares with SHA256SUMS of a release (README of the verifier).
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
mod="$(cd "${here}/.." && pwd)"
work="${HOME:-.}/kingres-repro-check"
targets=""
version="repro-check"

die() { printf 'repro-check.sh: error: %s\n' "$*" >&2; exit 2; }

while [ "$#" -gt 0 ]; do
    case "$1" in
        --work)    [ "$#" -ge 2 ] || die "--work needs a value"; work="$2"; shift 2 ;;
        --targets) [ "$#" -ge 2 ] || die "--targets needs a value"; targets="$2"; shift 2 ;;
        --version) [ "$#" -ge 2 ] || die "--version needs a value"; version="$2"; shift 2 ;;
        *) die "unknown argument '$1' (see the header of this script)" ;;
    esac
done
command -v tar >/dev/null || die "tar is not on the PATH"
command -v cmp >/dev/null || die "cmp is not on the PATH"

mkdir -p "${work}"
run="$(mktemp -d "${work}/run.XXXXXX")"
case "${run}" in "${work}"/run.*) ;; *) die "unexpected work directory ${run}" ;; esac
# shellcheck disable=SC2317  # run by the EXIT trap
cleanup() { chmod -R u+w "${run}" 2>/dev/null || true; rm -rf "${run}"; }
trap cleanup EXIT

# Two checkouts at paths that differ in depth and in length of every component.
src1="${run}/a/s/kingres/verifier"
src2="${run}/second-checkout-with-a-longer-name/deeper/still/kingres/verifier"
for s in "${src1}" "${src2}"; do
    mkdir -p "${s}"
    ( cd "${mod}" && tar -cf - --exclude='./testdata' --exclude='*/testdata' . ) | ( cd "${s}" && tar -xf - )
done

build() { # build N SRC
    local n="$1" src="$2"
    local args=(--version "${version}" --out "${run}/out-${n}" --unsigned)
    [ -z "${targets}" ] || args+=(--targets "${targets}")
    printf '== build %s from %s\n' "${n}" "${src}"
    GOCACHE="${run}/cache-${n}" GOPATH="${run}/gopath-${n}" GOMODCACHE="${run}/gopath-${n}/pkg/mod" \
        bash "${src}/scripts/release.sh" "${args[@]}" >"${run}/log-${n}.txt" 2>&1 ||
        { cat "${run}/log-${n}.txt" >&2; die "build ${n} failed"; }
}
build 1 "${src1}"
build 2 "${src2}"

# The Go environment of the machine must not change the bytes (P7R-06): one more build of the first target, with a Go environment
# file ("go env -w") and exported variables that all change the code the compiler makes. build-one.sh sets GOENV=off and unsets the
# variables, so the bytes must be the same as those of build 1. A script that lets the setting through fails here.
iso_t="$(printf '%s\n' "${targets:-linux/amd64}" | awk '{print $1}')"
iso_os="${iso_t%/*}"
iso_arch="${iso_t#*/}"
iso_name="kingres-verify-${iso_os}-${iso_arch}"
[ "${iso_os}" = windows ] && iso_name="${iso_name}.exe"
printf 'GOEXPERIMENT=nogreenteagc\nGOFIPS140=v1.0.0\nGOFLAGS=-ldflags=-X=main.version=hostile\n' >"${run}/hostile.env"
printf '== build 3 (a hostile Go environment) of %s\n' "${iso_name}"
GOENV="${run}/hostile.env" GOEXPERIMENT=nosizespecializedmalloc GOFIPS140=v1.0.0 \
    GOCACHE="${run}/cache-3" GOPATH="${run}/gopath-3" GOMODCACHE="${run}/gopath-3/pkg/mod" \
    bash "${src1}/scripts/build-one.sh" "${iso_os}" "${iso_arch}" "${version}" "${run}/hostile-${iso_name}" >"${run}/log-3.txt" 2>&1 ||
    { cat "${run}/log-3.txt" >&2; die "build 3 failed"; }

status=0
if ! diff -r "${run}/out-1" "${run}/out-2" >/dev/null; then
    status=1
fi
if ! cmp -s "${run}/out-1/${iso_name}" "${run}/hostile-${iso_name}"; then
    status=1
    printf 'differs under a hostile Go environment (a go env -w file, GOEXPERIMENT, GOFIPS140): %s\n' "${iso_name}"
fi
if [ "${status}" -eq 0 ]; then
    n="$(wc -l <"${run}/out-1/SHA256SUMS" | tr -d ' ')"
    printf 'REPRODUCIBLE: %s files, identical from %s and from %s, and %s is the same under a hostile Go environment\n' "${n}" "${src1#"${run}"/}" "${src2#"${run}"/}" "${iso_name}"
    cat "${run}/out-1/SHA256SUMS"
else
    printf 'NOT REPRODUCIBLE: the two builds differ\n--- build 1\n'
    cat "${run}/out-1/SHA256SUMS"
    printf -- '--- build 2\n'
    cat "${run}/out-2/SHA256SUMS"
    for f in "${run}"/out-1/*; do
        b="$(basename "${f}")"
        cmp -s "${f}" "${run}/out-2/${b}" || printf 'differs: %s\n' "${b}"
    done
fi
exit "${status}"

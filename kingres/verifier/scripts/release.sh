#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Builds the release files of kingres-verify (design ledger-phase7.md, 7.7):
#
#   kingres-verify-linux-amd64, kingres-verify-linux-arm64,
#   kingres-verify-darwin-arm64, kingres-verify-windows-amd64.exe,
#   SHA256SUMS            the SHA-256 of each file, in the form sha256sum -c reads
#   SHA256SUMS.note       a signed note over the text of SHA256SUMS, signed by the
#                         release key (Ed25519, key name kingres-verify-release)
#
# Usage:
#   scripts/release.sh --version TAG --out DIR --key-file FILE
#   scripts/release.sh --version TAG --out DIR --unsigned        (no note; for checks)
#
#   --version TAG     the version the binaries report (go build -X main.version=TAG)
#   --out DIR         where the files are written; must not exist or must be empty
#   --key-file FILE   the release key file (kingres-release-tool keygen made it; mode
#                     0600). It is read by the signing step only and never copied.
#   --unsigned        build and sum, write no note. The result is not a release: the
#                     script says so. repro-check.sh uses it.
#   --targets LIST    space separated os/arch pairs (default: the four above)
#
# The build is reproducible: CGO off, -trimpath, no VCS stamping, no build id, no
# symbol or debug tables, a fixed toolchain (the version of the go line of go.mod,
# which the script insists on), GOTOOLCHAIN=local so that no other Go release is
# fetched, a read-only module graph (there is no third-party module), and the
# micro-architecture levels fixed. The same source and toolchain give the same
# bytes on any machine and at any path (scripts/repro-check.sh shows it).
#
# Where the release key lives and who holds it is the project owner's decision
# (design section 12, decision 33); this script never creates a key and no key is in
# the repository. kingres-release-tool keygen makes one.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
mod="$(cd "${here}/.." && pwd)"

version=""
out=""
key_file=""
unsigned=0
targets="linux/amd64 linux/arm64 darwin/arm64 windows/amd64"

die() { printf 'release.sh: error: %s\n' "$*" >&2; exit 1; }

while [ "$#" -gt 0 ]; do
    case "$1" in
        --version)  [ "$#" -ge 2 ] || die "--version needs a value"; version="$2"; shift 2 ;;
        --out)      [ "$#" -ge 2 ] || die "--out needs a value"; out="$2"; shift 2 ;;
        --key-file) [ "$#" -ge 2 ] || die "--key-file needs a value"; key_file="$2"; shift 2 ;;
        --unsigned) unsigned=1; shift ;;
        --targets)  [ "$#" -ge 2 ] || die "--targets needs a value"; targets="$2"; shift 2 ;;
        *) die "unknown argument '$1' (see the header of this script)" ;;
    esac
done

[ -n "${version}" ] || die "--version is required (no tag exists before the first release, so it is never guessed)"
case "${version}" in
    *[!0-9A-Za-z._+-]*|[!0-9A-Za-z]*) die "the version '${version}' may hold only letters, digits and . _ + - and must start with a letter or digit" ;;
esac
[ -n "${out}" ] || die "--out is required"
if [ "${unsigned}" -eq 1 ]; then
    [ -z "${key_file}" ] || die "--unsigned and --key-file exclude each other"
else
    [ -n "${key_file}" ] || die "give --key-file, or --unsigned for a build that is not a release"
    [ -f "${key_file}" ] || die "no key file ${key_file}"
fi
if [ -e "${out}" ]; then
    [ -d "${out}" ] || die "${out} exists and is not a directory"
    [ -z "$(ls -A "${out}")" ] || die "${out} is not empty: a release is never written over another"
fi
command -v go >/dev/null || die "go is not on the PATH"
command -v sha256sum >/dev/null || die "sha256sum is not on the PATH"

# ---- the toolchain: exactly the Go release go.mod names ----------------------
want="$(sed -n 's/^toolchain go\([0-9][0-9.]*\)$/\1/p' "${mod}/go.mod")"
[ -n "${want}" ] || want="$(sed -n 's/^go \([0-9][0-9.]*\)$/\1/p' "${mod}/go.mod")"
[ -n "${want}" ] || die "no go or toolchain line in ${mod}/go.mod"
export GOTOOLCHAIN=local
have="$(go env GOVERSION)"
have="${have#go}"
if [ "${have}" != "${want}" ]; then
    if [ "${KINGRES_ALLOW_TOOL_MISMATCH:-}" = 1 ]; then
        printf 'release.sh: warning: Go %s is used, go.mod names %s; the result is not "the" release\n' "${have}" "${want}" >&2
    else
        die "Go ${have} is installed, go.mod names ${want}; install that release (override for a trial: KINGRES_ALLOW_TOOL_MISMATCH=1)"
    fi
fi

# ---- build -------------------------------------------------------------------

mkdir -p "${out}"
out="$(cd "${out}" && pwd)"
files=()
cd "${mod}"
for t in ${targets}; do
    os="${t%/*}"
    arch="${t#*/}"
    if [ "${os}" = "${t}" ] || [ -z "${os}" ] || [ -z "${arch}" ]; then die "bad target '${t}', expected os/arch"; fi
    name="kingres-verify-${os}-${arch}"
    [ "${os}" = windows ] && name="${name}.exe"
    printf 'building %s\n' "${name}"
    bash "${here}/build-one.sh" "${os}" "${arch}" "${version}" "${out}/${name}"
    files+=("${name}")
done

# ---- sums and note -----------------------------------------------------------
tool() { go run -trimpath ./cmd/kingres-release-tool "$@"; }
tool sums --out "${out}/SHA256SUMS" "${files[@]/#/${out}/}"
( cd "${out}" && sha256sum -c SHA256SUMS >/dev/null ) || die "sha256sum does not accept the sums that were just written"

if [ "${unsigned}" -eq 1 ]; then
    printf '\nUNSIGNED: %s holds binaries and SHA256SUMS but no SHA256SUMS.note.\n' "${out}"
    printf 'This is not a release: nobody can check who made these files.\n'
else
    tool sign --key-file "${key_file}" --sums "${out}/SHA256SUMS" --out "${out}/SHA256SUMS.note"
    # The note is checked with the verifier itself, with the public key of the key
    # file, before anything is announced.
    pubkey="$(tool pub --key-file "${key_file}" | sed -n 's/^public key:  //p')"
    go run -trimpath ./cmd/kingres-verify release verify \
        --sums "${out}/SHA256SUMS" --note "${out}/SHA256SUMS.note" --key "${pubkey}" \
        --binary "${out}/${files[0]}" >/dev/null || die "the note that was just made does not verify"
    printf '\nsigned release in %s\npublic key (print it in the README and the documentation): %s\n' "${out}" "${pubkey}"
fi
cat "${out}/SHA256SUMS"

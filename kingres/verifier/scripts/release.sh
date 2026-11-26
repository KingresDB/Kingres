#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (c) 2026, RooyeKhat Media Co
#
# Builds the release files of kingres-verify (design ledger-phase7.md, 7.7):
#
#   kingres-verify-linux-amd64, kingres-verify-linux-arm64,
#   kingres-verify-darwin-arm64, kingres-verify-windows-amd64.exe,
#   RELEASE               which release this is: version, commit, commit date, Go release
#   SHA256SUMS            the SHA-256 of each file (RELEASE too, so the signature covers
#                         the version), in the form sha256sum -c reads
#   SHA256SUMS.note       a signed note over the text of SHA256SUMS, signed by the
#                         release key (Ed25519, key name kingres-verify-release)
#
# Usage:
#   scripts/release.sh --version VERSION --out DIR --key-file FILE [--tag NAME]
#   scripts/release.sh --version VERSION --out DIR --key-file FILE --dry-run
#   scripts/release.sh --version VERSION --out DIR --unsigned        (no note; for checks)
#
#   --version VERSION the version the binaries report (go build -X main.version=VERSION)
#   --out DIR         where the files are written; must not exist or must be empty
#   --key-file FILE   the release key file (kingres-release-tool keygen made it; mode
#                     0600). It is read by the signing step only and never copied.
#   --tag NAME        the git tag of the release (default v VERSION, as in v0.1.0)
#   --dry-run         with --key-file: make and sign the files although the tree is not
#                     clean or the tag does not exist. The result is not a release, the
#                     script says so in capitals, and the RELEASE file then records the
#                     commit as "unknown". Without --dry-run a signed release is refused
#                     unless the working tree is clean and HEAD is exactly the tag.
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
dry=0
tag=""
targets="linux/amd64 linux/arm64 darwin/arm64 windows/amd64"

die() { printf 'release.sh: error: %s\n' "$*" >&2; exit 1; }

while [ "$#" -gt 0 ]; do
    case "$1" in
        --version)  [ "$#" -ge 2 ] || die "--version needs a value"; version="$2"; shift 2 ;;
        --out)      [ "$#" -ge 2 ] || die "--out needs a value"; out="$2"; shift 2 ;;
        --key-file) [ "$#" -ge 2 ] || die "--key-file needs a value"; key_file="$2"; shift 2 ;;
        --unsigned) unsigned=1; shift ;;
        --dry-run)  dry=1; shift ;;
        --tag)      [ "$#" -ge 2 ] || die "--tag needs a value"; tag="$2"; shift 2 ;;
        --targets)  [ "$#" -ge 2 ] || die "--targets needs a value"; targets="$2"; shift 2 ;;
        *) die "unknown argument '$1' (see the header of this script)" ;;
    esac
done

[ -n "${version}" ] || die "--version is required (no tag exists before the first release, so it is never guessed)"
case "${version}" in
    *[!0-9A-Za-z._+-]*|[!0-9A-Za-z]*) die "the version '${version}' may hold only letters, digits and . _ + - and must start with a letter or digit" ;;
esac
[ -n "${out}" ] || die "--out is required"
[ -n "${tag}" ] || tag="v${version}"
if [ "${unsigned}" -eq 1 ]; then
    [ -z "${key_file}" ] || die "--unsigned and --key-file exclude each other"
    [ "${dry}" -eq 0 ] || die "--unsigned is already not a release: --dry-run is for a signed run"
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

# ---- the tree and the tag (P7R-07) -------------------------------------------
# A signed release is made from the tagged commit of a clean checkout, so that the version in RELEASE names the source the files
# were built from. --dry-run turns the refusals into warnings and records the commit as "unknown". An unsigned build is not a
# release and is not checked (repro-check.sh builds from copies that hold no git data); it records the commit when the checkout is
# clean and HEAD is the commit, and "unknown" otherwise.
case "${tag}" in *[!0-9A-Za-z._+/-]*|-*) die "the tag '${tag}' may hold only letters, digits and . _ + - / and must not start with -" ;; esac
commit="unknown"
cdate="unknown"
problems=()
top=""
if command -v git >/dev/null 2>&1 && top="$(git -C "${mod}" rev-parse --show-toplevel 2>/dev/null)" && [ -n "${top}" ]; then
    head_commit="$(git -C "${top}" rev-parse HEAD 2>/dev/null || true)"
    status_out="$(git -C "${top}" status --porcelain --untracked-files=all 2>&1)" || status_out="git status failed"
    tag_commit="$(git -C "${top}" rev-parse -q --verify "refs/tags/${tag}^{commit}" 2>/dev/null || true)"
    [ -z "${status_out}" ] || problems+=("the working tree is not clean (git status lists changes)")
    if [ -z "${tag_commit}" ]; then
        problems+=("the tag ${tag} does not exist (no release is made without one)")
    elif [ "${tag_commit}" != "${head_commit}" ]; then
        problems+=("HEAD is not the commit of the tag ${tag}")
    fi
    if [ -z "${status_out}" ] && [ -n "${head_commit}" ]; then
        commit="${head_commit}"
        cdate="$(TZ=UTC git -C "${top}" log -1 --format=%cd --date=format-local:%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo unknown)"
    fi
else
    problems+=("this is not a git checkout: a release is made from the tagged commit of one")
fi
[ "${commit}" = unknown ] || [ "${#commit}" -eq 40 ] || commit=unknown
if [ "${unsigned}" -eq 0 ] && [ "${#problems[@]}" -gt 0 ]; then
    if [ "${dry}" -eq 1 ]; then
        commit="unknown"; cdate="unknown"
        for p in "${problems[@]}"; do printf 'release.sh: DRY RUN: %s\n' "${p}" >&2; done
    else
        for p in "${problems[@]}"; do printf 'release.sh: error: %s\n' "${p}" >&2; done
        die "refusing to make a signed release (--dry-run makes the files and says they are not a release)"
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

# ---- RELEASE: which release this is, covered by the signature through the sums -
printf 'kingres-verify release\nversion: %s\ncommit: %s\ndate: %s\ngo: go%s\n' "${version}" "${commit}" "${cdate}" "${have}" >"${out}/RELEASE"
files+=(RELEASE)

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
    # A set that records the commit as unknown ends release verify with status 5. A dry run makes such a set on purpose and says so below; a real
    # release must never end that way, so only a dry run passes --accept-dry-run.
    accept=()
    [ "${dry}" -eq 0 ] || accept=(--accept-dry-run)
    go run -trimpath ./cmd/kingres-verify release verify ${accept[@]+"${accept[@]}"} \
        --sums "${out}/SHA256SUMS" --note "${out}/SHA256SUMS.note" --key "${pubkey}" \
        --binary "${out}/${files[0]}" --binary "${out}/RELEASE" >/dev/null || die "the note that was just made does not verify"
    if [ "${dry}" -eq 1 ]; then
        printf '\nDRY RUN: %s holds signed files, but this is NOT a release (%s).\n' "${out}" "${problems[*]:-no reason}"
    else
        printf '\nsigned release %s (commit %s) in %s\n' "${version}" "${commit}" "${out}"
    fi
    printf 'public key (print it in the README and the documentation): %s\n' "${pubkey}"
fi
cat "${out}/SHA256SUMS"

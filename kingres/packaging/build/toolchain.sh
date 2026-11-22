#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (c) 2026, RooyeKhat Media Co
#
# kingres/packaging/build/toolchain.sh
#
# Install the pinned Go and Node.js releases (versions.env) into a directory
# and verify the SHA-256 of every archive before unpacking it.  Nothing is
# piped into a shell: the archive is downloaded to a file, its checksum is
# compared with the pinned value, and only then is it unpacked.
#
# Usage:
#   toolchain.sh [PREFIX]        default PREFIX: $TOOLCHAIN_PREFIX or
#                                /opt/kingres-toolchain (from versions.env)
# Afterwards put PREFIX/go/bin and PREFIX/node/bin on PATH.
#
# Environment:
#   KINGRES_DL_CACHE   directory for downloaded archives (default: a
#                      temporary directory that is removed on exit)

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=versions.env
. "${here}/versions.env"

prefix="${1:-${TOOLCHAIN_PREFIX:-${TOOLCHAIN_PREFIX_DEFAULT}}}"

if [ -n "${KINGRES_DL_CACHE:-}" ]; then
    dl="${KINGRES_DL_CACHE}"
    mkdir -p "${dl}"
else
    dl="$(mktemp -d)"
    trap 'rm -rf "${dl}"' EXIT
fi

fetch() { # fetch URL FILE SHA256
    local url="$1" file="$2" sha="$3"
    if [ ! -f "${dl}/${file}" ]; then
        curl --fail --silent --show-error --location --retry 3 \
            --output "${dl}/${file}.part" "${url}"
        mv "${dl}/${file}.part" "${dl}/${file}"
    fi
    echo "${sha}  ${dl}/${file}" | sha256sum --check --status ||
        { echo "toolchain.sh: checksum mismatch for ${file}" >&2; exit 1; }
}

mkdir -p "${prefix}"

go_file="go${GO_VERSION}.linux-amd64.tar.gz"
fetch "https://go.dev/dl/${go_file}" "${go_file}" "${GO_SHA256}"
rm -rf "${prefix}/go"
tar -C "${prefix}" -xzf "${dl}/${go_file}"

node_file="node-v${NODE_VERSION}-linux-x64.tar.xz"
fetch "https://nodejs.org/dist/v${NODE_VERSION}/${node_file}" "${node_file}" "${NODE_SHA256}"
rm -rf "${prefix}/node"
mkdir -p "${prefix}/node"
tar -C "${prefix}/node" --strip-components=1 -xJf "${dl}/${node_file}"

"${prefix}/go/bin/go" version
"${prefix}/node/bin/node" --version

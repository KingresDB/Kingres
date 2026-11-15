#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (c) 2026, RooyeKhat Media Co
#
# kingres/packaging/build/snapshot.sh
#
# Export a clean, consistent copy of the repository as of one commit
# ("git archive"), so that a build never sees uncommitted work.  Read-only
# for the repository: it changes neither the index nor any ref.
#
# Usage:
#   snapshot.sh OUTDIR [--rev REV] [--overlay PATH]...
#
#   --rev REV       commit to export (default: HEAD)
#   --overlay PATH  copy PATH (relative to the repository root) from the
#                   WORKING TREE over the snapshot.  For files that are not
#                   committed yet (for example the packaging files while they
#                   are being developed).  The build info then says
#                   "+overlay".
#
# OUTDIR must not exist or must be empty.  The exported commit id is written
# to OUTDIR/.kingres-commit.

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "${here}/../../.." && pwd)"

[ "$#" -ge 1 ] || { echo "usage: snapshot.sh OUTDIR [--rev REV] [--overlay PATH]..." >&2; exit 2; }
out="$1"; shift
rev=HEAD
overlays=()
while [ "$#" -gt 0 ]; do
    case "$1" in
        --rev)     rev="$2"; shift 2 ;;
        --overlay) overlays+=("$2"); shift 2 ;;
        *) echo "snapshot.sh: unknown argument '$1'" >&2; exit 2 ;;
    esac
done

# The repository may be owned by another user (a Windows drive seen from WSL).
git_ro() { git -c safe.directory='*' -C "${repo}" "$@"; }

commit="$(git_ro rev-parse --verify "${rev}^{commit}")"
mkdir -p "${out}"
if [ -n "$(ls -A "${out}")" ]; then
    echo "snapshot.sh: ${out} is not empty" >&2
    exit 1
fi
git_ro archive --format=tar "${commit}" | tar -x -C "${out}"
suffix=""
for p in "${overlays[@]}"; do
    [ -e "${repo}/${p}" ] || { echo "snapshot.sh: overlay ${p} does not exist" >&2; exit 1; }
    mkdir -p "${out}/$(dirname "${p}")"
    rm -rf "${out:?}/${p}"
    cp -a "${repo}/${p}" "${out}/${p}"
    suffix="+overlay"
done
# Files copied from a Windows drive arrive as mode 0777; normalize.
chmod -R u=rwX,go=rX "${out}"
printf '%s%s\n' "${commit}" "${suffix}" >"${out}/.kingres-commit"
printf 'snapshot of %s%s in %s\n' "${commit}" "${suffix}" "${out}"

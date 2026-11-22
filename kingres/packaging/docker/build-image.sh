#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (c) 2026, RooyeKhat Media Co
#
# kingres/packaging/docker/build-image.sh
#
# Build the KingresDB image of the PostgreSQL major of a source tree.  The
# version, the PostgreSQL version and the major are read from the tree's
# meson.build (build/source-info.sh), so they are not typed anywhere else, and
# the image is tagged kingres:<version>-pg<major> (kingres:0.1.0-pg18,
# kingres:0.1.0-pg19; releases.md).  There is no tag without the major.
#
# Usage:
#   build-image.sh [--context DIR] [--rev REV] [--tag TAG] [--revision TEXT]
#                  [--print-tag]
#
#   --context DIR   build context: a clean source tree (a snapshot made by
#                   build/snapshot.sh).  Default: a snapshot of REV, so that
#                   uncommitted work is never built.
#   --rev REV       commit or branch the default snapshot is made from
#                   (default HEAD).  kingres-18 and kingres-19 are the product
#                   branches.
#   --tag TAG       full image name instead of kingres:<version>-pg<major>
#   --revision TEXT value of the label org.opencontainers.image.revision
#                   (default: the commit in the context's .kingres-commit)
#   --print-tag     print the tag that would be used and stop
#
# Needs docker.

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
src_info="${here}/../build/source-info.sh"

context=""
rev=HEAD
tag=""
revision=""
print_tag=0
while [ "$#" -gt 0 ]; do
    case "$1" in
        --context)   context="$2"; shift 2 ;;
        --rev)       rev="$2"; shift 2 ;;
        --tag)       tag="$2"; shift 2 ;;
        --revision)  revision="$2"; shift 2 ;;
        --print-tag) print_tag=1; shift ;;
        *) echo "build-image.sh: unknown argument '$1' (see the header of this script)" >&2; exit 2 ;;
    esac
done

tmp=""
cleanup() { if [ -n "${tmp}" ]; then rm -rf "${tmp}"; fi; }
trap cleanup EXIT
if [ -z "${context}" ]; then
    tmp="$(mktemp -d)"
    context="${tmp}/context"
    "${here}/../build/snapshot.sh" "${context}" --rev "${rev}"
fi
[ -f "${context}/kingres/packaging/docker/Dockerfile" ] || { echo "build-image.sh: ${context} is not a KingresDB source tree" >&2; exit 1; }

version="$("${src_info}" "${context}" kingres_version)"
pg_version="$("${src_info}" "${context}" pg_version)"
pg_major="$("${src_info}" "${context}" pg_major)"
[ -n "${tag}" ] || tag="kingres:$("${src_info}" "${context}" image_tag)"
[ -n "${revision}" ] || revision="$(cat "${context}/.kingres-commit" 2>/dev/null || echo unknown)"

if [ "${print_tag}" -eq 1 ]; then
    printf '%s\n' "${tag}"
    exit 0
fi

echo "building ${tag}: KingresDB ${version} on PostgreSQL ${pg_version} from ${revision}"
docker build --file "${context}/kingres/packaging/docker/Dockerfile" \
    --build-arg "VERSION=${version}" --build-arg "PG_VERSION=${pg_version}" \
    --build-arg "PG_MAJOR=${pg_major}" --build-arg "REVISION=${revision}" \
    --tag "${tag}" "${context}"
echo "built ${tag}"

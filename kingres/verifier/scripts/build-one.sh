#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Builds one kingres-verify binary with the flags that make the build
# reproducible (design ledger-phase7.md, 7.7). It is the one place where those
# flags are written: scripts/release.sh builds every release file with it, and
# kingres/packaging/build/build.sh builds the copy that the package installs, so
# the packaged linux/amd64 binary is the same bytes as the release file of the same
# version and Go release.
#
# Usage: scripts/build-one.sh GOOS GOARCH VERSION OUTPUT
#
# The environment is made explicit here: nothing the caller exported (GOFLAGS, a
# C compiler, a micro-architecture level) can change the bytes. The Go release is
# not checked here (release.sh insists on the one go.mod names; the packaging
# recipe pins its own).
set -euo pipefail

[ "$#" -eq 4 ] || { echo "usage: build-one.sh GOOS GOARCH VERSION OUTPUT" >&2; exit 2; }
os="$1" arch="$2" version="$3" output="$4"
case "${version}" in
    *[!0-9A-Za-z._+-]*|[!0-9A-Za-z]*) echo "build-one.sh: the version '${version}' may hold only letters, digits and . _ + - and must start with a letter or digit" >&2; exit 2 ;;
esac
mod="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
case "${output}" in /*) ;; *) output="$(pwd)/${output}" ;; esac

# GOENV=off: the file that `go env -w` writes is not read, so a setting made there (GOEXPERIMENT, GOFIPS140, GOFLAGS, ...)
# cannot change the bytes (P7R-06). The variables that change the code the compiler makes are unset or fixed.
unset GOFLAGS GOEXPERIMENT GOFIPS140 GODEBUG GOWORK GOPROXY GOAMD64 GOARM64 GO386 GOARM GOMIPS GOPPC64 GORISCV64 GOWASM CC CXX CGO_CFLAGS CGO_LDFLAGS
export GOENV=off CGO_ENABLED=0 GOFLAGS=-mod=readonly GOAMD64=v1 GOARM64=v8.0 GOWORK=off GOTOOLCHAIN=local
cd "${mod}"
GOOS="${os}" GOARCH="${arch}" go build -trimpath -buildvcs=false \
    -ldflags "-s -w -buildid= -X main.version=${version}" \
    -o "${output}" ./cmd/kingres-verify

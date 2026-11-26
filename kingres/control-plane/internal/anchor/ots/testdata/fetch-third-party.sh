#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (c) 2026, RooyeKhat Media Co
#
# Fetches the third-party test data of package ots into testdata/third_party
# (or into the directory given as the first argument) and verifies it against
# third_party.sha256. The data is test data only and is never part of a binary.
#
# Source: the examples directory of the OpenTimestamps client,
# https://github.com/opentimestamps/opentimestamps-client (LGPL-3.0-or-later),
# at the pinned commit below. Needs bash, curl and sha256sum (or shasum).
set -euo pipefail

COMMIT=cd71c7609421bed2a07b9642a3c02a58c9fd2cdf
BASE="https://raw.githubusercontent.com/opentimestamps/opentimestamps-client/${COMMIT}/examples"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
dest="${1:-$here/third_party}"
manifest="$here/third_party.sha256"

sum_of() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }

# the manifest paths are relative to third_party and start with
# opentimestamps-client-examples/, which is the examples directory upstream
while read -r sum path; do
	mkdir -p "$dest/$(dirname "$path")"
	curl -fsS --retry 3 -o "$dest/$path" "$BASE/${path#opentimestamps-client-examples/}"
done <"$manifest"

fail=0
while read -r sum path; do
	if [ "$(sum_of "$dest/$path")" != "$sum" ]; then echo "wrong sum: $path" >&2; fail=1; fi
done <"$manifest"
# no file outside the manifest
listing="$(mktemp)"
(cd "$dest" && find . -type f | sed 's|^\./||' | LC_ALL=C sort) >"$listing"
cut -d' ' -f3- "$manifest" | LC_ALL=C sort | diff - "$listing" >&2 || fail=1
rm -f "$listing"
[ "$fail" = 0 ] && echo "ok: $(wc -l <"$manifest") files verified"
exit "$fail"

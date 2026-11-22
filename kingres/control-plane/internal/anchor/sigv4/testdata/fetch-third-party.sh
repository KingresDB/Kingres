#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Fetches the third-party test data of package sigv4 into testdata/third_party
# (or into the directory given as the first argument) and verifies it against
# third_party.sha256. The data is test data only and is never part of a binary.
#
# Source: the "AWS Signature Version 4 test suite" as kept in the botocore
# project (Apache-2.0, the LICENSE and NOTICE files are fetched with it),
# directory tests/unit/auth/aws4_testsuite of https://github.com/boto/botocore
# at the pinned commit below. Needs bash, curl and sha256sum (or shasum).
set -euo pipefail

COMMIT=32302bc372dde1b6173b60f8b85d671e24a0d414
BASE="https://raw.githubusercontent.com/boto/botocore/${COMMIT}/tests/unit/auth/aws4_testsuite"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
dest="${1:-$here/third_party}"
manifest="$here/third_party.sha256"

sum_of() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }

# the manifest paths are relative to third_party; upstream keeps ten cases
# in the groups normalize-path and post-sts-token
upstream() {
	local rel="${1#aws-sig-v4-test-suite/}" case_="${1#aws-sig-v4-test-suite/}"
	case_="${case_%%/*}"
	case "$case_" in
	get-relative | get-relative-relative | get-slash | get-slash-dot-slash | get-slash-pointless-dot | get-slashes | get-space | get-special-character)
		echo "normalize-path/$rel" ;;
	post-sts-header-after | post-sts-header-before)
		echo "post-sts-token/$rel" ;;
	*) echo "$rel" ;;
	esac
}

while read -r sum path; do
	mkdir -p "$dest/$(dirname "$path")"
	curl -fsS --retry 3 -o "$dest/$path" "$BASE/$(upstream "$path")"
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

#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (c) 2026, RooyeKhat Media Co
#
# kingres/packaging/test/test-release.sh
#
# Tests the product release script (release/release.sh) without Docker and without
# building PostgreSQL. It makes a small test repository with the two product
# branches, replaces the three build scripts and docker by stand-ins that write fake
# files (the hooks KINGRES_REL_*), and runs the real release.sh against it:
#
#   - a dry run passes the gates and changes nothing (no output directory, no tag)
#   - every gate refuses: dirty tree, a branch at another commit, a version that
#     differs, a PostgreSQL pre-release, KingresDB directories that differ, an
#     existing tag, a missing changelog section, a missing or failed or stale check,
#     a missing Docker without --skip-images, a non-empty output directory
#   - a run with --skip-images and a run with a stand-in docker build the packages
#     (and the image files), write kingres-SHA256SUMS (the plain names of the files) that sha256sum accepts and that
#     the verifier checks against its signed note (a throwaway key made in the test),
#     write the release notes from the template, prepare both tags on the right commits and
#     push nothing (a bare repository configured as the remote stays empty)
#   - a missing git committer identity, a missing or unusable key, and a bad --date are
#     refused before anything is built; the date of the notes is the local date or --date
#
# What this does not cover: the real builds (build/build.sh, build-deb.sh,
# build-image.sh), which test-deb.sh, test-image.sh and test-compose.sh cover on a
# machine with Docker.
#
# Usage: test-release.sh [--work DIR]      (default $HOME/kingres-release-test)
# Needs: bash, git, sha256sum, gzip, and kingres-release-tool and kingres-verify: built with go
# from kingres/verifier when go is on the PATH, else the programs named by KINGRES_REL_TOOL and
# KINGRES_REL_VERIFY (built for the machine this runs on). It makes one directory in DIR and removes it.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
pkg="$(cd "${here}/.." && pwd)"
release="${pkg}/release/release.sh"
work="${HOME}/kingres-release-test"
while [ "$#" -gt 0 ]; do
    case "$1" in
        --work) work="$2"; shift 2 ;;
        *) echo "test-release.sh: unknown argument '$1'" >&2; exit 2 ;;
    esac
done
for c in git sha256sum gzip; do
    command -v "${c}" >/dev/null || { echo "test-release.sh: ${c} is not on the PATH" >&2; exit 2; }
done
# shellcheck source=lib.sh
. "${here}/lib.sh"

mkdir -p "${work}"
run="$(mktemp -d "${work}/run.XXXXXX")"
case "${run}" in "${work}"/run.*) ;; *) echo "unexpected directory ${run}" >&2; exit 2 ;; esac
# shellcheck disable=SC2317  # run by the EXIT trap
cleanup() { chmod -R u+w "${run}" 2>/dev/null || true; rm -rf "${run}"; }
trap cleanup EXIT

repo="${run}/repo"
remote="${run}/remote.git"
stubs="${run}/stubs"
version=0.1.0
mkdir -p "${repo}" "${stubs}"

# ---- the stand-ins -----------------------------------------------------------
cat >"${stubs}/build.sh" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
rev=""; work=""
while [ "$#" -gt 0 ]; do case "$1" in --rev) rev="$2"; shift 2 ;; --work) work="$2"; shift 2 ;; *) exit 2 ;; esac; done
mkdir -p "${work}"
echo "build ${rev} ${work}" >>"${KINGRES_TEST_LOG}"
STUB
cat >"${stubs}/build-deb.sh" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
work=""; out=""; revision=""
while [ "$#" -gt 0 ]; do case "$1" in --work) work="$2"; shift 2 ;; --out) out="$2"; shift 2 ;; --revision) revision="$2"; shift 2 ;; *) exit 2 ;; esac; done
major="${work##*pg}"
echo "deb ${major} ${revision}" >>"${KINGRES_TEST_LOG}"
printf 'fake package for pg%s\n' "${major}" >"${out}/kingres_${KINGRES_TEST_VERSION}+pg${major}-${revision}_amd64.deb"
STUB
cat >"${stubs}/build-image.sh" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
rev=""; tag=""
while [ "$#" -gt 0 ]; do case "$1" in --rev) rev="$2"; shift 2 ;; --tag) tag="$2"; shift 2 ;; *) exit 2 ;; esac; done
echo "image ${rev} ${tag}" >>"${KINGRES_TEST_LOG}"
STUB
cat >"${stubs}/docker" <<'STUB'
#!/usr/bin/env bash
[ "$1" = save ] || exit 2
printf 'fake image %s\n' "$2"
STUB
chmod +x "${stubs}"/*
export KINGRES_TEST_LOG="${run}/stub.log"
export KINGRES_TEST_VERSION="${version}"
export KINGRES_REL_REPO="${repo}"
export KINGRES_REL_BUILD_SH="${stubs}/build.sh"
export KINGRES_REL_DEB_SH="${stubs}/build-deb.sh"
export KINGRES_REL_IMAGE_SH="${stubs}/build-image.sh"
export KINGRES_REL_DOCKER="${stubs}/docker"

# ---- the signing tools and a throwaway key ---------------------------------------
if [ -z "${KINGRES_REL_TOOL:-}" ] || [ -z "${KINGRES_REL_VERIFY:-}" ]; then
    command -v go >/dev/null || { echo "test-release.sh: go is not on the PATH; give KINGRES_REL_TOOL and KINGRES_REL_VERIFY" >&2; exit 2; }
    mkdir -p "${run}/tools"
    ( cd "${pkg}/../verifier" && GOTOOLCHAIN=local go build -trimpath -o "${run}/tools/kingres-release-tool" ./cmd/kingres-release-tool \
        && GOTOOLCHAIN=local go build -trimpath -o "${run}/tools/kingres-verify" ./cmd/kingres-verify ) || { echo "test-release.sh: cannot build the tools" >&2; exit 2; }
    export KINGRES_REL_TOOL="${run}/tools/kingres-release-tool"
    export KINGRES_REL_VERIFY="${run}/tools/kingres-verify"
fi
keyfile="${run}/test-release.key"
pubkey="$("${KINGRES_REL_TOOL}" keygen --out "${keyfile}" | sed -n 's/^public key:  //p')"
[ -n "${pubkey}" ] || { echo "test-release.sh: keygen made no key" >&2; exit 2; }
unset GIT_COMMITTER_NAME GIT_COMMITTER_EMAIL

# ---- the test repository ------------------------------------------------------
tg() { git -c safe.directory='*' -C "${repo}" "$@"; }
write_meson() { # PG_VERSION KINGRES_VERSION
    cat >"${repo}/meson.build" <<MESON
project('postgresql',
  ['c'],
  version: '$1',
  license: 'PostgreSQL')

kingres_version = '$2'
MESON
}
tg init -q -b kingres-18
tg config user.name "Release Test"
tg config user.email "release-test@example.invalid"
tg config commit.gpgsign false
mkdir -p "${repo}/contrib/kingres_x" "${repo}/kingres/docs"
write_meson 18.7 "${version}"
echo "extension" >"${repo}/contrib/kingres_x/kingres_x.control"
cat >"${repo}/kingres/CHANGELOG.md" <<'CL'
# Changelog

## [Unreleased]

## [0.1.0] - 2026-12-01

- The first release marker line.

## [0.0.1] - 2026-01-01

- An older line.
CL
cat >"${repo}/kingres/docs/release-notes-template.md" <<'TPL'
# KingresDB @VERSION@
Date @DATE@. PostgreSQL @PG18_VERSION@ and @PG19_VERSION@. Tags @TAG18@ and @TAG19@.
Commits @COMMIT18@ and @COMMIT19@.
## Changes
@CHANGELOG@
## Files
@ARTIFACTS@
## Checks
@CHECKS@
TPL
tg add -A
tg commit -q -m "kingres-18 state"
c18="$(tg rev-parse HEAD)"
tg checkout -q -b kingres-19
write_meson 19.0 "${version}"
tg commit -q -a -m "kingres-19 sync"
c19="$(tg rev-parse HEAD)"
tg checkout -q kingres-18
git init -q --bare "${remote}"
tg remote add origin "${remote}"

checks="${run}/checks.txt"
write_checks() { # COMMIT18 COMMIT19 [CHECK_TO_LEAVE_OUT]
    {
        echo "# a record written by hand"
        for k in tree-build core-regress extension-suites integration package-deb package-image package-compose; do
            [ "${k}" = "${3:-}" ] || echo "$1 18 ${k} pass"
            echo "$2 19 ${k} pass"
        done
        [ "${3:-}" = console ] || echo "$1 all console pass"
    } >"${checks}"
}
write_checks "${c18}" "${c19}"

outn=0
rc=0
text=""
# release ARGS...: runs release.sh with the good defaults first; later options win
# only where the script allows, so a test passes the options it needs in full.
release() {
    outn=$((outn + 1))
    set +e
    text="$(bash "${release}" "$@" 2>&1)"
    rc=$?
    set -e
}
good_args=(--version "${version}" --commit18 "${c18}" --commit19 "${c19}" --checks-file "${checks}" --key-file "${keyfile}" --work "${run}/work")
has() { printf '%s\n' "${text}" | grep -q -- "$1"; }
no_tags() { [ -z "$(tg tag -l 'kingres-*')" ]; }
remote_empty() { [ -z "$(git -c safe.directory='*' -C "${remote}" for-each-ref)" ]; }

# ---- dry run -------------------------------------------------------------------
note "dry run"
release "${good_args[@]}" --out "${run}/out-dry" --dry-run
check "the dry run prints the date of the notes" has "Release date: $(date +%Y-%m-%d) (the local date"
check_eq "a dry run with every gate met exits 0" 0 "${rc}"
check "the dry run says it is a dry run" has "Dry run: every gate passed"
check "the dry run prints the manual steps" has "git push <remote> kingres-0.1.0-pg18 kingres-0.1.0-pg19"
check "the dry run created no output directory" test ! -e "${run}/out-dry"
check "the dry run created no tag" no_tags
check "the dry run built nothing" test ! -e "${KINGRES_TEST_LOG}"
check "the short commit prefix of 12 digits is accepted" bash -c "bash '${release}' --version ${version} --commit18 ${c18:0:12} --commit19 ${c19:0:12} --checks-file '${checks}' --out '${run}/out-dry' --dry-run"

# ---- every gate refuses ----------------------------------------------------------
note "gates"
refuses() { # DESCRIPTION PATTERN ARGS...
    local what="$1" pat="$2"
    shift 2
    release "$@"
    if [ "${rc}" -ne 0 ] && has "${pat}"; then pass "refused: ${what}"; else fail "refused: ${what} (rc ${rc}; wanted [${pat}])"; fi
    if no_tags && [ ! -e "${KINGRES_TEST_LOG}" ]; then pass "nothing built or tagged after: ${what}"; else fail "something was built or tagged after: ${what}"; fi
}
refuses "a wrong --version" "kingres_version is 0.1.0, --version is 0.2.0" \
    --version 0.2.0 --commit18 "${c18}" --commit19 "${c19}" --checks-file "${checks}" --out "${run}/o1"
refuses "kingres-18 at another commit" "kingres-18 is at" \
    --version "${version}" --commit18 "0123456789abcdef0123456789abcdef01234567" --commit19 "${c19}" --checks-file "${checks}" --out "${run}/o2"
refuses "kingres-19 at another commit" "kingres-19 is at" \
    --version "${version}" --commit18 "${c18}" --commit19 "0123456789abcdef0123456789abcdef01234567" --checks-file "${checks}" --out "${run}/o3"
refuses "a commit prefix shorter than 12 digits" "kingres-18 is at" \
    --version "${version}" --commit18 "${c18:0:8}" --commit19 "${c19}" --checks-file "${checks}" --out "${run}/o4"
refuses "no record of checks" "no --checks-file" \
    --version "${version}" --commit18 "${c18}" --commit19 "${c19}" --out "${run}/o5"
write_checks "${c18}" "${c19}" integration
refuses "a check that is missing" "check integration on PostgreSQL 18: missing" "${good_args[@]}" --out "${run}/o6"
write_checks "${c18}" "${c19}" console
refuses "the console check is missing" "check console (all majors): missing" "${good_args[@]}" --out "${run}/o7"
write_checks "${c18}" "${c19}"
echo "${c19} 19 package-deb fail one test failed" >>"${checks}"
refuses "a check that failed after it passed" "check package-deb on PostgreSQL 19: fail" "${good_args[@]}" --out "${run}/o8"
write_checks "${c18}" "${c19}"
echo "${c18} 19 package-deb pass" >>"${checks}"
refuses "a check recorded on the commit of the other branch" "check package-deb on PostgreSQL 19: wrong-commit" "${good_args[@]}" --out "${run}/o9"
write_checks "${c18}" "${c19}"
KINGRES_REL_DOCKER="${stubs}/no-such-docker" release "${good_args[@]}" --out "${run}/o10b"
if [ "${rc}" -ne 0 ] && has "give --skip-images to release without images"; then pass "refused: docker is missing and --skip-images is not given"; else fail "docker missing: rc ${rc}"; fi
mkdir -p "${run}/o11"
echo x >"${run}/o11/leftover"
release "${good_args[@]}" --out "${run}/o11"
if [ "${rc}" -ne 0 ] && has "is not empty: a release is never written over another"; then pass "refused: a non-empty output directory"; else fail "non-empty out: rc ${rc}"; fi
echo untracked >"${repo}/untracked-file"
release "${good_args[@]}" --out "${run}/o12" --dry-run
if [ "${rc}" -ne 0 ] && has "the working tree is not clean"; then pass "refused: an untracked file (dry run reports it as well)"; else fail "dirty tree: rc ${rc}"; fi
rm -f "${repo}/untracked-file"
tg checkout -q kingres-19
echo "drift" >"${repo}/contrib/kingres_x/drift.sql"
tg add -A
tg commit -q -m "kingres-19 drift"
c19_drift="$(tg rev-parse HEAD)"
tg checkout -q kingres-18
write_checks "${c18}" "${c19_drift}"
release --version "${version}" --commit18 "${c18}" --commit19 "${c19_drift}" --checks-file "${checks}" --work "${run}/work" --out "${run}/o13" --dry-run
if [ "${rc}" -ne 0 ] && has "differ between kingres-18 and kingres-19"; then pass "refused: the KingresDB directories differ"; else fail "directories differ: rc ${rc}"; fi
tg checkout -q kingres-19
tg reset -q --hard "${c19}"
write_meson 19rc2 "${version}"
tg commit -q -a -m "kingres-19 on a release candidate"
c19_rc="$(tg rev-parse HEAD)"
tg checkout -q kingres-18
write_checks "${c18}" "${c19_rc}"
release --version "${version}" --commit18 "${c18}" --commit19 "${c19_rc}" --checks-file "${checks}" --work "${run}/work" --out "${run}/o14" --dry-run
if [ "${rc}" -ne 0 ] && has "19rc2, which is not a final release"; then pass "refused: PostgreSQL 19rc2"; else
    if [ "${rc}" -ne 0 ] && has "PostgreSQL 19rc2, which is not a final release"; then pass "refused: PostgreSQL 19rc2"; else fail "prerelease: rc ${rc}"; fi
fi
tg checkout -q kingres-19
tg reset -q --hard "${c19}"
tg checkout -q kingres-18
write_checks "${c18}" "${c19}"
tg tag -a kingres-0.1.0-pg18 -m existing "${c18}"
release "${good_args[@]}" --out "${run}/o15" --dry-run
if [ "${rc}" -ne 0 ] && has "the tag kingres-0.1.0-pg18 exists already"; then pass "refused: the tag exists"; else fail "tag exists: rc ${rc}"; fi
tg tag -d kingres-0.1.0-pg18 >/dev/null
sed -i 's/^- The first release marker line\.$/&\n\n### Pending\n\n- a draft line/' "${repo}/kingres/CHANGELOG.md"
tg commit -q -a -m "kingres-18 with a draft changelog"
c18_draft="$(tg rev-parse HEAD)"
release --version "${version}" --commit18 "${c18_draft}" --commit19 "${c19}" --checks-file "${checks}" --work "${run}/work" --out "${run}/o16" --dry-run
if [ "${rc}" -ne 0 ] && has "still has a '### Pending' part"; then pass "refused: the changelog section still has a Pending part"; else fail "pending part: rc ${rc}"; fi
tg reset -q --hard "${c18}"
note "identity, key and date"
# no committer identity: neither in the environment nor in any configuration
tg config --unset user.name
tg config --unset user.email
GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null GIT_CONFIG_NOSYSTEM=1 release "${good_args[@]}" --out "${run}/o20" --skip-images
if [ "${rc}" -ne 0 ] && has "no git committer identity" && no_tags && [ ! -e "${KINGRES_TEST_LOG}" ] && [ ! -e "${run}/o20" ]; then pass "refused before any build: no committer identity"; else fail "no identity: rc ${rc}"; fi
GIT_COMMITTER_NAME="From Env" GIT_COMMITTER_EMAIL="env@example.invalid" GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null GIT_CONFIG_NOSYSTEM=1 release "${good_args[@]}" --out "${run}/o21" --dry-run
if [ "${rc}" -eq 0 ] && has "committer identity is set (From Env <env@example.invalid>)"; then pass "an identity from GIT_COMMITTER_* is accepted"; else fail "env identity: rc ${rc}"; fi
tg config user.name "Release Test"
tg config user.email "release-test@example.invalid"
refuses "no --key-file" "no --key-file" --version "${version}" --commit18 "${c18}" --commit19 "${c19}" --checks-file "${checks}" --work "${run}/work" --out "${run}/o22" --skip-images
release --version "${version}" --commit18 "${c18}" --commit19 "${c19}" --checks-file "${checks}" --work "${run}/work" --out "${run}/o23" --dry-run
if [ "${rc}" -eq 0 ] && has "SKIP  release key"; then pass "a dry run needs no key"; else fail "dry run without key: rc ${rc}"; fi
refuses "a key file that does not exist" "does not exist or is not readable" "${good_args[@]}" --key-file "${run}/no-such.key" --out "${run}/o24" --skip-images
echo "not a key" >"${run}/bad.key"
chmod 600 "${run}/bad.key"
refuses "a file that is not a release key" "is not a release key" "${good_args[@]}" --key-file "${run}/bad.key" --out "${run}/o25" --skip-images
refuses "a date in the wrong form" "--date must be YYYY-MM-DD" "${good_args[@]}" --date 28.11.2026 --out "${run}/o26" --skip-images
refuses "a date that does not exist" "is not a date" "${good_args[@]}" --date 2026-02-30 --out "${run}/o27" --skip-images

check "no output directory was made by a refused run" test ! -e "${run}/o1"

# ---- a release without images ---------------------------------------------------
note "release with --skip-images and no docker"
out1="${run}/release-noimg"
KINGRES_REL_DOCKER="${stubs}/no-such-docker" release "${good_args[@]}" --out "${out1}" --skip-images
check_eq "the run exits 0" 0 "${rc}"
check "the skipped images are announced" has "SKIPPED: container images"
check "the pg18 package exists" test -f "${out1}/pg18/kingres_${version}+pg18-1_amd64.deb"
check "the pg19 package exists" test -f "${out1}/pg19/kingres_${version}+pg19-1_amd64.deb"
check "no image file was written" bash -c "! ls '${out1}'/pg*/*image* "
check_eq "kingres-SHA256SUMS lists the two packages by their plain names" "kingres_${version}+pg18-1_amd64.deb kingres_${version}+pg19-1_amd64.deb " "$(awk '{print $2}' "${out1}/kingres-SHA256SUMS" | sort | tr '\n' ' ')"
check "the checksums are accepted by sha256sum -c in a folder that holds the files" bash -c "d='${run}/flat1' && mkdir -p \"\$d\" && cp '${out1}'/pg*/*.deb \"\$d\"/ && cd \"\$d\" && sha256sum -c '${out1}/kingres-SHA256SUMS'"
check "no file named SHA256SUMS is written (it would collide with the sums of the verifier on the release page)" test ! -e "${out1}/SHA256SUMS"
check "the signed note exists" test -s "${out1}/kingres-SHA256SUMS.note"
check "the verifier accepts the note and every file with the public key" "${KINGRES_REL_VERIFY}" release verify --sums "${out1}/kingres-SHA256SUMS" --note "${out1}/kingres-SHA256SUMS.note" --key "${pubkey}" --binary "${out1}/pg18/kingres_${version}+pg18-1_amd64.deb" --binary "${out1}/pg19/kingres_${version}+pg19-1_amd64.deb"
check "the verifier refuses a changed sums file" bash -c "cp '${out1}/kingres-SHA256SUMS' '${run}/tampered' && sed -i '1s/^./0/' '${run}/tampered' && ! '${KINGRES_REL_VERIFY}' release verify --sums '${run}/tampered' --note '${out1}/kingres-SHA256SUMS.note' --key '${pubkey}' >/dev/null"
check "the notes carry the local date of the machine" grep -q "^Date $(date +%Y-%m-%d)\." "${out1}/RELEASE-NOTES.md"
check "the notes name the sums, the note and the public key" bash -c "grep -q 'kingres-SHA256SUMS.note' '${out1}/RELEASE-NOTES.md' && grep -qF '${pubkey}' '${out1}/RELEASE-NOTES.md'"
check "the manual steps send the sums and the note with each tag" has "kingres-SHA256SUMS.note"
check "the notes carry the version" grep -q "^# KingresDB ${version}\$" "${out1}/RELEASE-NOTES.md"
check "the notes carry the changelog section" grep -q "first release marker line" "${out1}/RELEASE-NOTES.md"
check "the notes do not carry an older section" bash -c "! grep -q 'An older line' '${out1}/RELEASE-NOTES.md'"
check "the notes say the images were not built" grep -q "not built" "${out1}/RELEASE-NOTES.md"
check "the notes carry both commits" bash -c "grep -q '${c18}' '${out1}/RELEASE-NOTES.md' && grep -q '${c19}' '${out1}/RELEASE-NOTES.md'"
check "the notes carry both PostgreSQL versions" grep -q "PostgreSQL 18.7 and 19.0" "${out1}/RELEASE-NOTES.md"
check "no placeholder is left in the notes" bash -c "! grep -q '@[A-Z0-9_]*@' '${out1}/RELEASE-NOTES.md'"
check "the notes carry the checks table" grep -q "| 19 | package-deb | pass |" "${out1}/RELEASE-NOTES.md"
check_eq "the tag of pg18 is on kingres-18" "${c18}" "$(tg rev-parse 'kingres-0.1.0-pg18^{commit}')"
check_eq "the tag of pg19 is on kingres-19" "${c19}" "$(tg rev-parse 'kingres-0.1.0-pg19^{commit}')"
check "the tags are annotated" bash -c "[ \"\$(git -c safe.directory='*' -C '${repo}' cat-file -t kingres-0.1.0-pg18)\" = tag ]"
check "the build was made from the commit of each branch" bash -c "grep -q 'build ${c18} ' '${KINGRES_TEST_LOG}' && grep -q 'build ${c19} ' '${KINGRES_TEST_LOG}'"
check "no image was built" bash -c "! grep -q '^image' '${KINGRES_TEST_LOG}'"
check "the manual steps are printed" has "gh release create kingres-0.1.0-pg19"
check "nothing was pushed" remote_empty

note "a second run into the same directory, and with the tags present"
release "${good_args[@]}" --out "${run}/release-again" --skip-images --dry-run
check "the tags of the first run stop a second release" bash -c "[ ${rc} -ne 0 ]"
check "the message names the tag" has "kingres-0.1.0-pg18 exists already"

# ---- a release with images --------------------------------------------------------
note "release with images (stand-in docker)"
tg tag -d kingres-0.1.0-pg18 kingres-0.1.0-pg19 >/dev/null
rm -f "${KINGRES_TEST_LOG}"
out2="${run}/release-img"
release "${good_args[@]}" --out "${out2}" --revision 2 --date 2026-11-30
check_eq "the run exits 0" 0 "${rc}"
check "the pg18 image file exists and is not empty" test -s "${out2}/pg18/kingres-${version}-pg18-image.tar.gz"
check "the pg19 image file exists and is not empty" test -s "${out2}/pg19/kingres-${version}-pg19-image.tar.gz"
check "the revision 2 package exists" test -f "${out2}/pg18/kingres_${version}+pg18-2_amd64.deb"
check_eq "kingres-SHA256SUMS has four lines" 4 "$(wc -l <"${out2}/kingres-SHA256SUMS" | tr -d ' ')"
check "the checksums are accepted by sha256sum -c in a folder that holds the files" bash -c "d='${run}/flat2' && mkdir -p \"\$d\" && cp '${out2}'/pg*/* \"\$d\"/ && cd \"\$d\" && sha256sum -c '${out2}/kingres-SHA256SUMS'"
check "the verifier accepts an image file with the public key" "${KINGRES_REL_VERIFY}" release verify --sums "${out2}/kingres-SHA256SUMS" --note "${out2}/kingres-SHA256SUMS.note" --key "${pubkey}" --binary "${out2}/pg19/kingres-${version}-pg19-image.tar.gz"
check "the date given with --date is in the notes" grep -q "^Date 2026-11-30\." "${out2}/RELEASE-NOTES.md"
check "the image of pg18 carries the major in its tag" grep -q "image ${c18} kingres:${version}-pg18" "${KINGRES_TEST_LOG}"
check "the image of pg19 carries the major in its tag" grep -q "image ${c19} kingres:${version}-pg19" "${KINGRES_TEST_LOG}"
check "the saved image is the image of that tag" bash -c "gzip -dc '${out2}/pg19/kingres-${version}-pg19-image.tar.gz' | grep -q 'kingres:${version}-pg19'"
check "the notes list the image files" grep -q "docker load" "${out2}/RELEASE-NOTES.md"
check "the notes do not say the images were not built" bash -c "! grep -q 'not built' '${out2}/RELEASE-NOTES.md'"
check "nothing was pushed" remote_empty

summary

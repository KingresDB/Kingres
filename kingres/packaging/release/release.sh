#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (c) 2026, RooyeKhat Media Co
#
# kingres/packaging/release/release.sh
#
# The product release script (docs/dev/releases.md, "What a release is"): from the
# two product branches it makes, per PostgreSQL major, the Debian package and the
# container image, the checksums and the release notes, and it prepares the two tags
#
#   kingres-<version>-pg18   on the expected commit of kingres-18
#   kingres-<version>-pg19   on the expected commit of kingres-19
#
# It NEVER pushes anything. Pushing the branches and the tags and creating the GitHub
# release are manual steps; the script prints them at the end. No GitHub Actions are
# involved (the project forbids them).
#
# Usage:
#   release.sh --version V --commit18 SHA --commit19 SHA --out DIR
#              --key-file FILE [--date YYYY-MM-DD]
#              [--checks-file FILE] [--work DIR] [--revision N]
#              [--skip-images] [--sign-tags] [--dry-run]
#
#   --version V        the release version. It must equal kingres_version in meson.build
#                      on both branches; it is never guessed (source-info.sh reads it)
#   --commit18 SHA     the commit kingres-18 must be at (full id, or 12 or more digits)
#   --commit19 SHA     the commit kingres-19 must be at (the sync commit made from it)
#   --out DIR          where the artifacts, kingres-SHA256SUMS, its signed
#                      note and RELEASE-NOTES.md are written;
#                      must not exist or must be empty (a release is never written over
#                      another)
#   --key-file FILE    the release key (the file kingres-release-tool keygen made; the key of
#                      kingres/verifier). The checksums of the packages and images are
#                      signed with it, in the signed note format the verifier reads
#                      (kingres-verify release verify). A real run is refused without it,
#                      before anything is built; a dry run needs none (it builds and signs
#                      nothing). The key is read by the signing step only and never copied
#   --date DATE        the date written in the release notes, YYYY-MM-DD (default: today by
#                      the clock of this machine, in its time zone, not UTC). It is printed
#                      at the start and again with the notes
#   --checks-file FILE the record of the final checks (format below). Without it every
#                      run is refused; a dry run reports the missing record as a failed gate
#   --work DIR         work area of the builds (default: $HOME/kingres-release-work);
#                      holds pg18/ and pg19/, each the work area of build/build.sh
#   --revision N       Debian revision (default 1)
#   --skip-images      do not build the container images (the build machine has no
#                      Docker); the package-image and package-compose checks are then
#                      not required, and the notes say the images were not built
#   --sign-tags        make the tags with git tag -s (a signing key is then needed);
#                      default: annotated, unsigned
#   --dry-run          run every gate and print the plan; build nothing, write nothing,
#                      create no tag. Exits non-zero when a gate fails
#
# What is checked before anything is built (every failure is listed, then the script
# stops):
#   - the working tree is clean (untracked files count)
#   - kingres-18 and kingres-19 are at the expected commits
#   - meson.build says the same kingres_version on both, equal to --version; the
#     PostgreSQL major is 18 on kingres-18 and 19 on kingres-19; both are final
#     releases of PostgreSQL (a pre-release such as 19rc2 is refused)
#   - the KingresDB directories (contrib/kingres_*, kingres) are identical on both
#     commits, the rule of releases.md
#   - neither tag exists yet
#   - kingres/CHANGELOG.md has a section for the version that no longer has a
#     "### Pending" part (the draft marker), and the release-notes template exists
#   - the record of checks says every required check passed on the expected commit
#   - a git committer identity is set (user.name and user.email in the git configuration of
#     the repository, or GIT_COMMITTER_NAME and GIT_COMMITTER_EMAIL), because the tags are
#     made as the very last step and a missing identity would fail them after the builds
#   - a key file is given and exists (not for a dry run); before the first build the tool
#     of kingres/verifier is built from the commit of kingres-18 and reads the key
#
# The record of checks is a text file with one line per run, written by whoever ran
# the check (the script never runs the multi-hour suites itself):
#
#   <commit> <major> <check> <pass|fail> [free text]
#
# <major> is 18 or 19, or "all" for a check that does not depend on the major. A
# later line for the same major and check replaces an earlier one. Lines that start
# with # and blank lines are ignored. Required checks, per major: tree-build,
# core-regress, extension-suites, integration, package-deb, package-image,
# package-compose (the last two not with --skip-images); for "all": console. The
# commit must be the expected commit of kingres-18 for "all" and for 18, and of
# kingres-19 for 19.
#
# Test hooks (environment): KINGRES_REL_REPO (the repository, default: the one that
# holds this script), KINGRES_REL_BUILD_SH, KINGRES_REL_DEB_SH, KINGRES_REL_IMAGE_SH
# (replacements of build/build.sh, deb/build-deb.sh, docker/build-image.sh),
# KINGRES_REL_DOCKER (the docker command, default docker),
# KINGRES_REL_TOOL and KINGRES_REL_VERIFY (ready-made kingres-release-tool and
# kingres-verify programs; default: built with go from kingres/verifier of the commit of
# kingres-18, GOTOOLCHAIN=local),
# KINGRES_REL_ALLOW_PRERELEASE=1 (accept a PostgreSQL pre-release; for trials only).
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
pkg="$(cd "${here}/.." && pwd)"
repo="${KINGRES_REL_REPO:-$(cd "${pkg}/../.." && pwd)}"
build_sh="${KINGRES_REL_BUILD_SH:-${pkg}/build/build.sh}"
deb_sh="${KINGRES_REL_DEB_SH:-${pkg}/deb/build-deb.sh}"
image_sh="${KINGRES_REL_IMAGE_SH:-${pkg}/docker/build-image.sh}"
docker_cmd="${KINGRES_REL_DOCKER:-docker}"
source_info="${pkg}/build/source-info.sh"

version=""
commit18=""
commit19=""
out=""
checks_file=""
key_file=""
release_date=""
work="${HOME}/kingres-release-work"
revision=1
skip_images=0
sign_tags=0
dry_run=0

die() { printf 'release.sh: error: %s\n' "$*" >&2; exit 1; }
need_value() { [ "$2" -ge 2 ] || die "$1 needs a value"; }

while [ "$#" -gt 0 ]; do
    case "$1" in
        --version)     need_value "$1" "$#"; version="$2"; shift 2 ;;
        --commit18)    need_value "$1" "$#"; commit18="$2"; shift 2 ;;
        --commit19)    need_value "$1" "$#"; commit19="$2"; shift 2 ;;
        --out)         need_value "$1" "$#"; out="$2"; shift 2 ;;
        --checks-file) need_value "$1" "$#"; checks_file="$2"; shift 2 ;;
        --key-file)    need_value "$1" "$#"; key_file="$2"; shift 2 ;;
        --date)        need_value "$1" "$#"; release_date="$2"; shift 2 ;;
        --work)        need_value "$1" "$#"; work="$2"; shift 2 ;;
        --revision)    need_value "$1" "$#"; revision="$2"; shift 2 ;;
        --skip-images) skip_images=1; shift ;;
        --sign-tags)   sign_tags=1; shift ;;
        --dry-run)     dry_run=1; shift ;;
        *) die "unknown argument '$1' (see the header of this script)" ;;
    esac
done

[ -n "${version}" ] || die "--version is required (the version is never guessed)"
case "${version}" in
    *[!0-9A-Za-z.+-]*|[!0-9]*) die "the version '${version}' must start with a digit and hold only letters, digits and . + -" ;;
esac
[ -n "${commit18}" ] || die "--commit18 is required"
[ -n "${commit19}" ] || die "--commit19 is required"
[ -n "${out}" ] || die "--out is required"
case "${revision}" in ''|*[!0-9]*) die "--revision must be a number" ;; esac
date_given=1
if [ -z "${release_date}" ]; then
    release_date="$(date +%Y-%m-%d)"   # the local date: the release is made in the time zone of the machine, not in UTC
    date_given=0
fi
case "${release_date}" in
    [0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]) [ "$(date -d "${release_date}" +%Y-%m-%d 2>/dev/null)" = "${release_date}" ] || die "--date ${release_date} is not a date" ;;
    *) die "--date must be YYYY-MM-DD" ;;
esac
for c in git sha256sum; do command -v "${c}" >/dev/null || die "${c} is not on the PATH"; done

g() { git -c safe.directory='*' -C "${repo}" "$@"; }

tag18="kingres-${version}-pg18"
tag19="kingres-${version}-pg19"

failures=()
gate_pass() { printf 'PASS  %s\n' "$1"; }
gate_fail() { printf 'FAIL  %s\n' "$1"; failures+=("$1"); }

# A full commit id, or a prefix of at least 12 hex digits, matches the commit.
same_commit() { # WANTED ACTUAL
    case "$1" in ''|*[!0-9a-f]*) return 1 ;; esac
    [ "${#1}" -ge 12 ] || return 1
    case "$2" in "$1"*) return 0 ;; *) return 1 ;; esac
}

tmp="$(mktemp -d)"
# shellcheck disable=SC2317  # run by the EXIT trap
cleanup() { rm -rf "${tmp}"; }
trap cleanup EXIT

if [ "${dry_run}" -eq 1 ]; then
    printf 'KingresDB release %s (dry run: nothing is built, written or tagged)\n\n' "${version}"
else
    printf 'KingresDB release %s\n\n' "${version}"
fi
if [ "${date_given}" -eq 1 ]; then
    printf 'Release date: %s (given with --date)\n\n' "${release_date}"
else
    printf 'Release date: %s (the local date of this machine; --date overrides it)\n\n' "${release_date}"
fi

# ---- gate 1: a clean working tree -------------------------------------------
status="$(g status --porcelain --untracked-files=all)"
if [ -z "${status}" ]; then
    gate_pass "the working tree is clean"
else
    gate_fail "the working tree is not clean (commit, stash or remove: $(printf '%s' "${status}" | head -3 | tr '\n' ';'))"
fi

# ---- gate 1b: a committer identity (the tags are the last step) ---------------
# git tag -a needs a committer identity; without one the tag step fails after every build. The identity is looked for where git looks
# for it explicitly: the environment, then the configuration of the repository (a name and an e-mail made up from the host name do not count).
ident_name="${GIT_COMMITTER_NAME:-$(g config --get user.name 2>/dev/null || true)}"
ident_email="${GIT_COMMITTER_EMAIL:-$(g config --get user.email 2>/dev/null || true)}"
if [ -n "${ident_name}" ] && [ -n "${ident_email}" ]; then
    gate_pass "a git committer identity is set (${ident_name} <${ident_email}>)"
else
    gate_fail "no git committer identity: set user.name and user.email (git config) or GIT_COMMITTER_NAME and GIT_COMMITTER_EMAIL; the tags are made after the builds and would fail there"
fi

# ---- gate 1c: the release key -------------------------------------------------
if [ -n "${key_file}" ]; then
    if [ -f "${key_file}" ] && [ -r "${key_file}" ]; then
        gate_pass "the key file ${key_file} exists and is readable"
    else
        gate_fail "the key file ${key_file} does not exist or is not readable"
    fi
elif [ "${dry_run}" -eq 1 ]; then
    printf 'SKIP  release key: no --key-file; a dry run builds and signs nothing (a real run is refused without a key)\n'
else
    gate_fail "no --key-file: the checksums of the packages and images are signed with the release key (kingres-release-tool keygen makes one; --dry-run needs none)"
fi

# ---- gate 2: the branches are at the expected commits -----------------------
actual18="$(g rev-parse --verify --quiet "refs/heads/kingres-18^{commit}" || true)"
actual19="$(g rev-parse --verify --quiet "refs/heads/kingres-19^{commit}" || true)"
if [ -n "${actual18}" ] && same_commit "${commit18}" "${actual18}"; then
    gate_pass "kingres-18 is at ${actual18:0:12}"
else
    gate_fail "kingres-18 is at '${actual18:-no such branch}', expected ${commit18}"
fi
if [ -n "${actual19}" ] && same_commit "${commit19}" "${actual19}"; then
    gate_pass "kingres-19 is at ${actual19:0:12}"
else
    gate_fail "kingres-19 is at '${actual19:-no such branch}', expected ${commit19}"
fi

# ---- gate 3: versions --------------------------------------------------------
pg_version18=""
pg_version19=""
for m in 18 19; do
    actual="actual${m}"
    sha="${!actual}"
    if [ -z "${sha}" ]; then continue; fi
    d="${tmp}/meson${m}"
    mkdir -p "${d}"
    if ! g show "${sha}:meson.build" >"${d}/meson.build" 2>/dev/null; then gate_fail "kingres-${m} has no meson.build"; continue; fi
    if ! info="$("${source_info}" "${d}" 2>&1)"; then gate_fail "kingres-${m}: ${info}"; continue; fi
    kv="$(printf '%s\n' "${info}" | sed -n 's/^kingres_version=//p')"
    pv="$(printf '%s\n' "${info}" | sed -n 's/^pg_version=//p')"
    pm="$(printf '%s\n' "${info}" | sed -n 's/^pg_major=//p')"
    if [ "${m}" = 18 ]; then pg_version18="${pv}"; else pg_version19="${pv}"; fi
    if [ "${kv}" = "${version}" ]; then
        gate_pass "kingres-${m}: kingres_version is ${kv}"
    else
        gate_fail "kingres-${m}: kingres_version is ${kv}, --version is ${version}"
    fi
    if [ "${pm}" = "${m}" ]; then
        gate_pass "kingres-${m}: PostgreSQL major is ${pm}"
    else
        gate_fail "kingres-${m}: PostgreSQL major is ${pm}, expected ${m}"
    fi
    case "${pv}" in
        *[!0-9.]*|*.*.*|*[!0-9])
            if [ "${KINGRES_REL_ALLOW_PRERELEASE:-}" = 1 ]; then
                printf 'WARN  kingres-%s is on PostgreSQL %s, a pre-release (accepted for a trial)\n' "${m}" "${pv}"
            else
                gate_fail "kingres-${m} is on PostgreSQL ${pv}, which is not a final release (releases.md: nothing is tagged from a pre-release)"
            fi ;;
        *) gate_pass "kingres-${m}: PostgreSQL ${pv} is a final release" ;;
    esac
done

# ---- gate 4: the KingresDB directories are identical ------------------------
if [ -n "${actual18}" ] && [ -n "${actual19}" ]; then
    if g diff --quiet "${actual18}" "${actual19}" -- 'contrib/kingres_*' kingres; then
        gate_pass "contrib/kingres_* and kingres are identical on both branches"
    else
        gate_fail "contrib/kingres_* and kingres differ between kingres-18 and kingres-19 (sync kingres-19 first; git diff kingres-18 kingres-19 -- 'contrib/kingres_*' kingres)"
    fi
fi

# ---- gate 5: the tags are free ----------------------------------------------
for t in "${tag18}" "${tag19}"; do
    if g rev-parse --verify --quiet "refs/tags/${t}" >/dev/null; then
        gate_fail "the tag ${t} exists already"
    else
        gate_pass "the tag ${t} does not exist"
    fi
done

# ---- gate 6: the changelog and the template ---------------------------------
section=""
template=""
if [ -n "${actual18}" ]; then
    changelog_text="$(g show "${actual18}:kingres/CHANGELOG.md" 2>/dev/null || true)"
    section="$(printf '%s\n' "${changelog_text}" | awk -v v="${version}" '
        /^## \[/ { if (on) exit; if (index($0, "## [" v "]") == 1) { on = 1; next } }
        on { print }')"
    if [ -n "$(printf '%s' "${section}" | tr -d '[:space:]')" ]; then
        gate_pass "kingres/CHANGELOG.md has a section for ${version}"
    else
        gate_fail "kingres/CHANGELOG.md has no section '## [${version}]' with content"
    fi
    if printf '%s\n' "${section}" | grep -q '^### Pending'; then
        gate_fail "the changelog section of ${version} still has a '### Pending' part: check it, move or remove it, and commit"
    fi
    template="$(g show "${actual18}:kingres/docs/release-notes-template.md" 2>/dev/null || true)"
    if [ -n "${template}" ]; then
        gate_pass "the release-notes template exists"
    else
        gate_fail "kingres/docs/release-notes-template.md does not exist on kingres-18"
    fi
fi

# ---- gate 7: the record of the final checks ---------------------------------
required18="tree-build core-regress extension-suites integration package-deb"
required19="${required18}"
if [ "${skip_images}" -eq 0 ]; then
    required18="${required18} package-image package-compose"
    required19="${required19} package-image package-compose"
fi
checks_lines=""
if [ -z "${checks_file}" ]; then
    gate_fail "no --checks-file: the final checks must be recorded (see the header of this script)"
elif [ ! -f "${checks_file}" ]; then
    gate_fail "the checks file ${checks_file} does not exist"
else
    checks_lines="$(grep -v -e '^[[:space:]]*#' -e '^[[:space:]]*$' "${checks_file}" || true)"
    check_state() { # MAJOR CHECK EXPECTED_COMMIT -> prints pass, fail, wrong-commit or missing
        local want_major="$1" want_check="$2" want_commit="$3" result="missing" c m k r _
        while read -r c m k r _; do
            [ "${m}" = "${want_major}" ] && [ "${k}" = "${want_check}" ] || continue
            if same_commit "${c}" "${want_commit}"; then
                case "${r}" in pass) result=pass ;; *) result=fail ;; esac
            else
                result="wrong-commit"
            fi
        done <<<"${checks_lines}"
        printf '%s' "${result}"
    }
    for m in 18 19; do
        want="actual${m}"
        req="required${m}"
        [ -n "${!want}" ] || continue
        for k in ${!req}; do
            s="$(check_state "${m}" "${k}" "${!want}")"
            if [ "${s}" = pass ]; then
                gate_pass "check ${k} on PostgreSQL ${m}"
            else
                gate_fail "check ${k} on PostgreSQL ${m}: ${s} in ${checks_file}"
            fi
        done
    done
    if [ -n "${actual18}" ]; then
        s="$(check_state all console "${actual18}")"
        if [ "${s}" = pass ]; then
            gate_pass "check console (all majors)"
        else
            gate_fail "check console (all majors): ${s} in ${checks_file}"
        fi
    fi
fi

# ---- gate 8: the machine and the output directory ---------------------------
if [ -e "${out}" ]; then
    if [ ! -d "${out}" ]; then
        gate_fail "${out} exists and is not a directory"
    elif [ -n "$(ls -A "${out}")" ]; then
        gate_fail "${out} is not empty: a release is never written over another"
    else
        gate_pass "the output directory ${out} is empty"
    fi
else
    gate_pass "the output directory ${out} does not exist yet"
fi
if [ "${skip_images}" -eq 1 ]; then
    printf 'SKIP  container images: --skip-images given; no image is built or tested, and the notes say so\n'
elif command -v "${docker_cmd}" >/dev/null 2>&1; then
    gate_pass "${docker_cmd} is available for the images"
else
    gate_fail "${docker_cmd} is not on the PATH, so the container images cannot be built; run on a machine with Docker, or give --skip-images to release without images"
fi
for f in "${build_sh}" "${deb_sh}" "${image_sh}"; do
    [ -f "${f}" ] || gate_fail "the build script ${f} does not exist"
done

if [ "${#failures[@]}" -gt 0 ]; then
    printf '\n%s gate(s) failed:\n' "${#failures[@]}"
    printf '  - %s\n' "${failures[@]}"
    die "the release is refused; nothing was built, written or tagged"
fi

# ---- the plan -----------------------------------------------------------------
deb18="kingres_${version}+pg18-${revision}_amd64.deb"
deb19="kingres_${version}+pg19-${revision}_amd64.deb"
img18="kingres:${version}-pg18"
img19="kingres:${version}-pg19"
with_image18=""
with_image19=""
if [ "${skip_images}" -eq 0 ]; then
    with_image18=", image ${img18}"
    with_image19=", image ${img19}"
fi
printf '\nPlan:\n'
printf '  pg18  commit %s  ->  %s%s\n' "${actual18:0:12}" "${deb18}" "${with_image18}"
printf '  pg19  commit %s  ->  %s%s\n' "${actual19:0:12}" "${deb19}" "${with_image19}"
printf '  then kingres-SHA256SUMS and its signed note, RELEASE-NOTES.md (dated %s) in %s, and the local tags %s and %s\n' "${release_date}" "${out}" "${tag18}" "${tag19}"

manual_steps() {
    cat <<STEPS

Manual steps (this script never pushes and never contacts GitHub):
  1. Look at ${out}/RELEASE-NOTES.md and ${out}/kingres-SHA256SUMS. The signed note ${out}/kingres-SHA256SUMS.note is checked with the public key of the release key (kingres-verify release verify).
  2. Push the branches, then the two tags (replace <remote> by the remote of the project):
       git push <remote> kingres-18 kingres-19
       git push <remote> ${tag18} ${tag19}
  3. Create the GitHub release of each tag with its files and the notes, for example:
       gh release create ${tag18} --title "KingresDB ${version} on PostgreSQL 18" --notes-file ${out}/RELEASE-NOTES.md ${out}/pg18/* ${out}/kingres-SHA256SUMS ${out}/kingres-SHA256SUMS.note
       gh release create ${tag19} --title "KingresDB ${version} on PostgreSQL 19" --notes-file ${out}/RELEASE-NOTES.md ${out}/pg19/* ${out}/kingres-SHA256SUMS ${out}/kingres-SHA256SUMS.note
  4. The checksums are signed (kingres-SHA256SUMS.note); the packages and images themselves are not signed, and publishing them is a separate decision of the project owner.
STEPS
}

if [ "${dry_run}" -eq 1 ]; then
    printf '\nDry run: every gate passed. The real run would build both majors, write %s and create the two tags locally.\n' "${out}"
    manual_steps
    exit 0
fi

# ---- the tool that signs and checks the sums, and the key, before the first build -------------------------
# The tool and the verifier of kingres/verifier are built from the commit of kingres-18 (not from the working tree); the key is read once, so that a
# key that cannot be used is found now and not after the builds.
tool_dir="${tmp}/tools"
mkdir -p "${tool_dir}"
rel_tool="${KINGRES_REL_TOOL:-}"
rel_verify="${KINGRES_REL_VERIFY:-}"
if [ -z "${rel_tool}" ] || [ -z "${rel_verify}" ]; then
    command -v go >/dev/null || die "go is not on the PATH: it builds kingres-release-tool and kingres-verify, which sign and check the checksums (nothing was built or tagged)"
    g archive --format=tar "${actual18}" kingres/verifier | tar -x -C "${tool_dir}" || die "cannot take kingres/verifier from ${actual18:0:12}"
    ( cd "${tool_dir}/kingres/verifier" && GOTOOLCHAIN=local CGO_ENABLED=0 go build -trimpath -o "${tool_dir}/kingres-release-tool" ./cmd/kingres-release-tool \
        && GOTOOLCHAIN=local CGO_ENABLED=0 go build -trimpath -o "${tool_dir}/kingres-verify" ./cmd/kingres-verify ) \
        || die "cannot build the release tool and the verifier from kingres/verifier (nothing was built or tagged)"
    rel_tool="${rel_tool:-${tool_dir}/kingres-release-tool}"
    rel_verify="${rel_verify:-${tool_dir}/kingres-verify}"
fi
pubkey="$("${rel_tool}" pub --key-file "${key_file}" 2>/dev/null | sed -n 's/^public key:  //p' || true)"
[ -n "${pubkey}" ] || die "the key file ${key_file} is not a release key (the file that kingres-release-tool keygen made, mode 0600; nothing was built or tagged)"
printf 'Release key: %s\n' "${pubkey}"

# ---- the builds ---------------------------------------------------------------
mkdir -p "${out}" "${work}"
out="$(cd "${out}" && pwd)"
work="$(cd "${work}" && pwd)"
sums_files=()
artifact_lines=""
for m in 18 19; do
    want="actual${m}"
    sha="${!want}"
    deb="deb${m}"
    img="img${m}"
    mkdir -p "${out}/pg${m}"
    printf '\n==> PostgreSQL %s: build from %s\n' "${m}" "${sha:0:12}"
    "${build_sh}" --rev "${sha}" --work "${work}/pg${m}"
    printf '\n==> PostgreSQL %s: Debian package\n' "${m}"
    "${deb_sh}" --work "${work}/pg${m}" --out "${out}/pg${m}" --revision "${revision}"
    [ -f "${out}/pg${m}/${!deb}" ] || die "build-deb.sh did not produce ${out}/pg${m}/${!deb}"
    sums_files+=("pg${m}/${!deb}")
    artifact_lines="${artifact_lines}- \`pg${m}/${!deb}\`"$'\n'
    if [ "${skip_images}" -eq 0 ]; then
        printf '\n==> PostgreSQL %s: container image %s\n' "${m}" "${!img}"
        "${image_sh}" --rev "${sha}" --tag "${!img}"
        tarball="kingres-${version}-pg${m}-image.tar.gz"
        "${docker_cmd}" save "${!img}" | gzip -n >"${out}/pg${m}/${tarball}"
        [ -s "${out}/pg${m}/${tarball}" ] || die "docker save wrote nothing for ${!img}"
        sums_files+=("pg${m}/${tarball}")
        artifact_lines="${artifact_lines}- \`pg${m}/${tarball}\` (\`docker load\`, image \`${!img}\`)"$'\n'
    fi
done
if [ "${skip_images}" -eq 1 ]; then
    printf '\nSKIPPED: container images (--skip-images). Nothing in %s is an image.\n' "${out}"
    artifact_lines="${artifact_lines}- Container images: **not built** for this release (the build machine has no Docker)."$'\n'
fi

# ---- checksums, signed ------------------------------------------------------------
# One list for both majors, the plain names of the files (the release page of each tag holds them without their directory), so that it is checked in a
# directory that holds the downloaded files. Its name differs from the verifier's (kingres-verify-SHA256SUMS), which is on the same page.
sums_name="kingres-SHA256SUMS"
sums_paths=()
for f in "${sums_files[@]}"; do sums_paths+=("${out}/${f}"); done
"${rel_tool}" sums --out "${out}/${sums_name}" "${sums_paths[@]}" || die "cannot write the checksums"
check_dir="${tmp}/check"
mkdir -p "${check_dir}"
for f in "${sums_files[@]}"; do ln -s "${out}/${f}" "${check_dir}/$(basename "${f}")"; done
( cd "${check_dir}" && sha256sum -c "${out}/${sums_name}" >/dev/null ) || die "sha256sum does not accept the sums that were just written"
"${rel_tool}" sign --key-file "${key_file}" --sums "${out}/${sums_name}" --out "${out}/${sums_name}.note" || die "cannot sign the checksums"
verify_args=()
for f in "${sums_files[@]}"; do verify_args+=(--binary "${out}/${f}"); done
"${rel_verify}" release verify --sums "${out}/${sums_name}" --note "${out}/${sums_name}.note" --key "${pubkey}" "${verify_args[@]}" >/dev/null \
    || die "the signed note that was just made does not verify"
printf '\n%s (signed in %s.note by the release key %s):\n' "${sums_name}" "${sums_name}" "${pubkey}"
cat "${out}/${sums_name}"

# ---- release notes --------------------------------------------------------------
changelog_file="${tmp}/changelog-section"
printf '%s\n' "${section}" >"${changelog_file}"
checks_out="${tmp}/checks"
{
    printf '| Major | Check | Result | Commit |\n|---|---|---|---|\n'
    while read -r c m k r _; do
        [ -n "${c}" ] || continue
        printf '| %s | %s | %s | %s |\n' "${m}" "${k}" "${r}" "${c:0:12}"
    done <<<"${checks_lines}"
} >"${checks_out}"
artifacts_file="${tmp}/artifacts"
{
    printf '%s\n' "${artifact_lines}"
    printf '%s\n' "- \`kingres-SHA256SUMS\` (the checksums of all the files above by their plain names; \`sha256sum -c kingres-SHA256SUMS\` in the folder that holds them) and \`kingres-SHA256SUMS.note\`, a signed note over it made with the release key \`${pubkey}\`; \`kingres-verify release verify --sums kingres-SHA256SUMS --note kingres-SHA256SUMS.note --key <the public key> --binary <file>\` checks a file against it. The packages and images themselves are not signed."
} >"${artifacts_file}"
notes="${out}/RELEASE-NOTES.md"
today="${release_date}"
{
    while IFS= read -r line || [ -n "${line}" ]; do
        case "${line}" in
            '@CHANGELOG@') cat "${changelog_file}" ;;
            '@ARTIFACTS@') cat "${artifacts_file}" ;;
            '@CHECKS@')    cat "${checks_out}" ;;
            *)
                line="${line//@VERSION@/${version}}"
                line="${line//@PG18_VERSION@/${pg_version18}}"
                line="${line//@PG19_VERSION@/${pg_version19}}"
                line="${line//@COMMIT18@/${actual18}}"
                line="${line//@COMMIT19@/${actual19}}"
                line="${line//@TAG18@/${tag18}}"
                line="${line//@TAG19@/${tag19}}"
                line="${line//@DATE@/${today}}"
                printf '%s\n' "${line}" ;;
        esac
    done <<<"${template}"
} >"${notes}"
printf '\nRelease notes written to %s\n' "${notes}"

# ---- the tags (local only) ------------------------------------------------------
tag_cmd=(tag -a)
if [ "${sign_tags}" -eq 1 ]; then tag_cmd=(tag -s); fi
g "${tag_cmd[@]}" "${tag18}" -m "KingresDB ${version} on PostgreSQL ${pg_version18}" "${actual18}"
g "${tag_cmd[@]}" "${tag19}" -m "KingresDB ${version} on PostgreSQL ${pg_version19}" "${actual19}"
printf 'Prepared the local tags %s (%s) and %s (%s). Nothing was pushed.\n' "${tag18}" "${actual18:0:12}" "${tag19}" "${actual19:0:12}"
manual_steps

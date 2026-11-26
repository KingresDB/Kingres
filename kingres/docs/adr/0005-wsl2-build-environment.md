<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# ADR 0005: WSL2 as the build environment

- Status: Accepted
- Date: 2026-11-14
- Source in architecture.md: "Build and test tooling"; Open decisions #4

## Context

The developer machine runs Windows 11. Sourced facts in the document: both meson and autoconf are maintained; the meson documentation requires Meson and Ninja plus Perl `IPC::Run` for TAP tests; a 2023 pgsql-hackers report says the meson problems on Windows were only with MSYS2 and that MSVC worked well, but it may be outdated.

**[Recommendation]** in the document: on a Windows 11 machine, WSL2 (Ubuntu) or Docker is the lowest-risk everyday path; the MSVC build should be a compatibility job in CI; CI should include a Linux matrix with `-Dtap_tests=enabled -Dcassert=true`, a Windows job and a nightly job that merges the latest upstream stable tag.

Verified when this ADR was written: Go and Node.js are installed on Windows, so the control plane and the web panel build natively there. The C code is built and tested in WSL2 (Ubuntu 24.04).

## Decision

C code (the PostgreSQL fork and the extensions) is built and tested in WSL2 with Ubuntu 24.04. Go and Node.js code builds natively on Windows. Docker is used for CI images. The Windows MSVC build is a later compatibility job in CI, not a daily path. The build instructions are in `kingres/docs/dev/getting-started.md`.

## Consequences

- Daily C development uses the same environment as the Linux package target (ADR 0014), which lowers the risk of surprises.
- The source tree lives on the Windows file system while the build runs in WSL2. Line endings must stay LF (`core.autocrlf=false`, `core.eol=lf`); performance and file-permission behaviour across the boundary is a possible cost and is to be settled in getting-started.md after the build is verified.
- Windows-specific behaviour of the fork is not tested daily. A Windows server package is out of scope for the first releases (ADR 0014), so this is acceptable for now, but a Windows CI job is still planned.
- **[Needs verification]** The official Windows installation page for the current version returned a 404 during research; the Windows build requirements for PG18/19 are unconfirmed. This matters only when the Windows CI job is built.
- Developers need WSL2 enabled; other contributors may use Linux directly.

## Alternatives considered

- **Docker as the daily environment.** Listed as the other option; kept for CI images. The document does not give a reason for ranking it below WSL2 for daily work beyond the recommendation.
- **Native Windows build (MSVC, or MSYS2 with meson).** Rejected for daily use: the document reports uncertainty (the MSYS2 problems and the possibly outdated report) and recommends it only as a CI compatibility job.

## Note of 2026-11-20: no CI

This ADR speaks of CI images, a Linux matrix, a Windows job and a nightly job in CI. The project runs no CI: GitHub Actions is switched off for the repository by decision of the project owner, and every check runs locally ([dev/releases.md](../dev/releases.md), "Where the checks run"). The decision that C code is built and tested in WSL2 stands; the CI parts of it were not carried out and are not planned.

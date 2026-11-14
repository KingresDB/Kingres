<!-- SPDX-License-Identifier: PostgreSQL -->
# Getting started: build and test KingresDB

This page takes a new machine to a built, tested KingresDB server. Every
command here was run on 2026-11-14 on Windows 11 with WSL2 and Ubuntu 24.04;
the results are in the [work log](../worklog.md).

## What you need

| Component | Where it builds | Requirement |
|---|---|---|
| Server and extensions (C) | Linux only (WSL2 on Windows) | Ubuntu 24.04 with the packages installed by `kingres/dev/setup-ubuntu.sh` |
| Control plane (Go) | Any OS | The Go version stated in [coding-standards.md](coding-standards.md) |
| Web panel (TypeScript) | Any OS | The Node.js version stated in [coding-standards.md](coding-standards.md) |

KingresDB is built with meson. The autoconf build is not supported; see the
note at the top of [core-patches.md](core-patches.md).

## Git settings (Windows)

The tree must keep LF line endings. A checkout with CRLF breaks the shell and
Perl scripts when they run inside WSL. In the repository:

```
git config core.autocrlf false
git config core.eol lf
```

## One-time setup of the Linux side

On Windows, install the distribution without launching it, then run the setup
script as root. The script installs the toolchain and creates the
unprivileged user `kingres`; PostgreSQL refuses to run as root, and the test
suites start real servers.

```powershell
wsl.exe --install -d Ubuntu-24.04 --no-launch
wsl.exe -d Ubuntu-24.04 -u root -- bash "/mnt/c/<path to the repository>/kingres/dev/setup-ubuntu.sh"
```

On a native Ubuntu machine, run `sudo kingres/dev/setup-ubuntu.sh <your user>`.

## Build, install and test the server

```powershell
wsl.exe -d Ubuntu-24.04 -u kingres -- bash "/mnt/c/<path to the repository>/kingres/dev/build.sh" all
```

`build.sh all` does five things; each is also a command of its own (see the
header of the script):

1. `sync`: when the repository is on a Windows drive, mirrors it to
   `~/kingres-work/src`. The Windows filesystem as seen from WSL is slow and
   does not keep Unix permissions, and PostgreSQL will not start on a data
   directory that is not mode 0700.
2. `configure`: `meson setup` with assertions (`-Dcassert=true`) and mandatory
   TAP tests (`-Dtap_tests=enabled`).
3. `build`: compiles with ninja into `~/kingres-work/build`.
4. `install`: installs into the private prefix `~/kingres-work/install`.
5. `test`: runs the core regression suite.

To run every PostgreSQL test suite, including the TAP tests:

```powershell
wsl.exe -d Ubuntu-24.04 -u kingres -- bash "/mnt/c/<path to the repository>/kingres/dev/build.sh" sync build test-all
```

After editing files on the Windows side, run `sync build` again; ninja only
rebuilds what changed.

## Work on an extension

`kingres/dev/ext.sh` builds one extension with PGXS against the installed
server, without rebuilding the server, and runs its tests:

```powershell
wsl.exe -d Ubuntu-24.04 -u kingres -- bash "/mnt/c/<path to the repository>/kingres/dev/ext.sh" kingres_shard all
```

`regress` starts a throwaway server with the extension in
`shared_preload_libraries` and runs the pg_regress tests; `tap` runs the TAP
tests under the extension's `t/` directory, which start their own servers.
Set `PROVE_TESTS` to run selected TAP files.

## Check what you built

```
~/kingres-work/install/bin/postgres --version     # postgres (PostgreSQL) 18.7
psql -c 'select version()'                         # ... 64-bit (KingresDB 0.1.0)
```

The PostgreSQL version symbols are deliberately unchanged; only the long
version string carries the KingresDB suffix. See patch KP-0001 in
[core-patches.md](core-patches.md).

## A note on running commands through wsl.exe

PowerShell and `wsl.exe` both rewrite quotes and `$` in an inline
`bash -c "..."` string, and nested command substitutions arrive broken. For
anything longer than one simple command, put it in a script file and run
`bash /mnt/c/.../script.sh`.

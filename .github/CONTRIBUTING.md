# Contributing to KingresDB

Thank you for helping. This file says how to report problems, how to propose changes and which rules a contribution has to follow. By taking part you agree to the [code of conduct](CODE_OF_CONDUCT.md). To report a vulnerability, do not use an issue; follow [SECURITY.md](SECURITY.md).

KingresDB (short name `kingres`) is a database derived from PostgreSQL. Its own code is in `kingres/` and `contrib/kingres_*`. Everything else in the tree is PostgreSQL. The state of the project, phase by phase, is in [`kingres/README.md`](../kingres/README.md), and there is a [wiki](https://github.com/KingresDB/Kingres/wiki) that summarises the documentation.

## Reporting an issue

Open an issue at <https://github.com/KingresDB/Kingres/issues>. Please say:

- the branch or commit, and the PostgreSQL major (18 or 19);
- what you did, with the smallest SQL, request or steps that show it;
- what you expected and what happened, with the exact error text;
- the output of `select version()` and how KingresDB was installed.

Search the existing issues and the status text of [`kingres/README.md`](../kingres/README.md) first: some defects are already known and listed there as open.

## Proposing a change

1. For anything larger than a small fix, open an issue first so the design can be agreed. Decisions that change the design are recorded as ADRs in [`kingres/docs/adr`](../kingres/docs/adr).
2. Branch from `kingres-18`, the default branch. Do not edit `kingres-19` by hand: its KingresDB directories are synchronised from `kingres-18` ([`kingres/docs/dev/releases.md`](../kingres/docs/dev/releases.md)).
3. Make the change together with its tests and documentation.
4. Open a pull request that says what changed, why, and how you checked it.

Pull requests are checked locally by the maintainers. The project does not use GitHub Actions, so a pull request has no automatic checks; run the checks below yourself and say what you ran.

## Rules every contribution follows

- **English only.** Code, comments, identifiers, commit messages, documents and issues are in English. Documentation is plain prose without marketing language.
- **Licence.** KingresDB's own code is licensed under AGPL-3.0-only (see [`LICENSE`](../LICENSE) and [ADR 0024](../kingres/docs/adr/0024-agpl-for-kingresdb-code.md)). Every new file of your own carries the header `SPDX-License-Identifier: AGPL-3.0-only`; use the comment syntax of the language and the header the neighbouring files use ([`kingres/docs/dev/coding-standards.md`](../kingres/docs/dev/coding-standards.md)).
- **PostgreSQL files keep their licence.** Files that come from PostgreSQL keep the PostgreSQL License ([`COPYRIGHT`](../COPYRIGHT)) and are changed as little as possible. A change to an upstream file needs a row in the register [`kingres/docs/dev/core-patches.md`](../kingres/docs/dev/core-patches.md).
- **One source for PostgreSQL 18 and 19.** Each extension is one source that builds without warnings and passes its tests on both majors. Differences between the majors are kept in the compatibility header of the extension and nowhere else ([ADR 0023](../kingres/docs/adr/0023-one-extension-source-for-several-majors.md)).
- **Every change has tests.** New behaviour has tests at the right layer; a bug fix starts with a test that fails without the fix. Tests do not depend on your machine: no fixed ports or paths, no network except to servers the test starts ([`kingres/docs/dev/testing.md`](../kingres/docs/dev/testing.md)).
- **Follow the coding standards** of [`kingres/docs/dev/coding-standards.md`](../kingres/docs/dev/coding-standards.md): C code looks like PostgreSQL C code and is formatted with `pgindent`; Go and TypeScript use the formatters and linters named there.
- **Say only what is true.** Documentation states what is built, what has been reviewed and what is still open. Do not describe a feature as finished until its tests pass.
- **Names.** The product is KingresDB; the short name is `kingres`.
- **No secrets and no personal data** in files, tests or commit messages.

Contributions are accepted under the project's licence: what you contribute is licensed under AGPL-3.0-only (or, for changes to PostgreSQL files, under the PostgreSQL License of those files), the same as the rest of the project. There is no contributor licence agreement. By opening a pull request you state that you have the right to contribute the code under these terms.

## Running the checks locally

Set up the machine and build the server as described in [`kingres/docs/dev/getting-started.md`](../kingres/docs/dev/getting-started.md). The scripts are in [`kingres/dev`](../kingres/dev).

```
kingres/dev/build.sh all                 # build, install and run the core regression suite
kingres/dev/build.sh sync build test-all # every suite, including TAP tests
kingres/dev/ext.sh kingres_shard all     # one extension against one installation
KINGRES_PG18=<install> KINGRES_PG19=<install> kingres/dev/ext-majors.sh kingres_shard
```

A change to an extension is complete only when `ext-majors.sh` passes for it on both majors.

For the other components, run the checks listed in their READMEs:

- control plane ([`kingres/control-plane/README.md`](../kingres/control-plane/README.md)): `go vet ./...`, `gofmt -l .`, `golangci-lint run ./...`, `go test ./...`;
- web console ([`kingres/web/README.md`](../kingres/web/README.md)): `npm run typecheck`, `npm run lint`, `npm test`;
- verifier ([`kingres/verifier/README.md`](../kingres/verifier/README.md)): `go test ./...`;
- REST contract ([`kingres/api/README.md`](../kingres/api/README.md)): `npm run check` in `kingres/api`; a change to an endpoint changes `kingres/api/openapi.yaml` first;
- packaging ([`kingres/packaging/README.md`](../kingres/packaging/README.md)): the tests of the artifacts.

## Definition of done

A change is done when it has tests and the suites pass, the formatters and linters pass, the documentation is updated, any change to an upstream file is registered, and a dated entry in [`kingres/docs/worklog.md`](../kingres/docs/worklog.md) records what was done, how it was verified and what is still open.

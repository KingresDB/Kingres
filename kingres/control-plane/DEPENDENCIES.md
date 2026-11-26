<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Third-party dependencies

Policy: `kingres/docs/dev/coding-standards.md`, section 3 (standard library
first; license compatible with the PostgreSQL License: MIT, BSD and Apache-2.0
are accepted, GPL and AGPL are not). Licenses were read from the LICENSE files
of the module versions in the module cache. Everything else (HTTP routing with
Go 1.22 patterns, sessions, CSRF, rate limiting, logging, configuration, JSON,
embedding, TLS) is the standard library.

## Linked into the `kingresd` binary

| Module | Version | License | Why the standard library is not enough |
|---|---|---|---|
| `github.com/jackc/pgx/v5` | v5.13.0 | MIT | PostgreSQL driver: wire protocol, SCRAM-SHA-256, TLS negotiation, pooling (`pgxpool`). `database/sql` has no driver in the standard library. Used for the login connection (the driver performs SCRAM) and for all catalog reads. |
| `github.com/jackc/pgpassfile` | v1.0.0 | MIT | Dependency of pgx (`.pgpass` support). |
| `github.com/jackc/pgservicefile` | v0.0.0-20240606120523-5a60cdf6a761 | MIT | Dependency of pgx (`pg_service.conf` support). |
| `github.com/jackc/puddle/v2` | v2.2.3 | MIT | Dependency of `pgxpool` (generic resource pool). |
| `golang.org/x/sync` | v0.22.0 | BSD-3-Clause | Dependency of puddle (semaphore). |
| `golang.org/x/text` | v0.41.0 | BSD-3-Clause | Dependency of pgx and used directly by `internal/scram` (`precis`, the SASLprep of SCRAM password verifiers). |

## Build and test only (not linked into the binary)

| Module | Version | License | Use |
|---|---|---|---|
| `go.yaml.in/yaml/v3` | v3.0.6 | MIT and Apache-2.0 (the maintained continuation of `gopkg.in/yaml.v3`, whose repository is archived) | Parses `openapi.yaml` in the code generator (`internal/contract/gen`) and in the contract tests. The binary imports no YAML parser: the operation table is generated. |
| `github.com/santhosh-tekuri/jsonschema/v6` | v6.0.3 | Apache-2.0 | JSON Schema 2020-12 validator (the dialect of OpenAPI 3.1) used by the contract tests to validate real responses against the schemas of the contract. |

## Tools (not modules of this repository)

- `golangci-lint` v2 for `.golangci.yml`; `oapi-codegen` v2.8.2 was only
  evaluated (it cannot read the contract, see README) and is not a dependency.
- WSL2 and the KingresDB build for the integration tests (`scripts/`).

The versions are the latest published when they were added (resolved with
`go get ...@latest` on 2026-11-14). `go.sum` is committed; `go mod verify` passes.

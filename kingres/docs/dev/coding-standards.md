<!-- SPDX-License-Identifier: PostgreSQL -->
# Coding standards

These standards apply to all KingresDB code. Each rule names the tool or review step that enforces it. A rule without an enforcing tool is checked in code review.

Tool versions on the current developer machine (checked 2026-11-14): Go `go1.27.3 windows/amd64`, Node.js `v24.22.1`. These are the versions the project is developed with today; they are not a promise about the minimum versions. The minimum versions will be pinned in `go.mod` (the `go` directive) and in `package.json` (the `engines` field) when those files are created.

Commands that are not yet verified in this repository are marked "to be confirmed".

## 1. C (extensions and core patches)

The rule of thumb: KingresDB C code must look like PostgreSQL C code. A reviewer who knows the PostgreSQL tree should not see a style change when moving from `contrib/pg_buffercache` to `contrib/kingres_shard`.

### 1.1 Language level

- Follow "Miscellaneous Coding Conventions" in `doc/src/sgml/sources.sgml` in this tree. For the C standard it says: rely only on C99 features; do not use variable-length arrays, intermingled declarations and code, `//` comments or universal character names; later-standard features need a fallback. Enforced by: code review, and by building with the warnings the tree enables (to be confirmed which flag catches each case; do not add `-Wno-*` flags to hide warnings).
- Declare variables at the top of the block. Write `/* ... */` comments only.
- Use PostgreSQL types and macros (`bool`, `int32`, `Oid`, `Datum`, `Assert`, `lengthof`) rather than raw C equivalents when they exist.

### 1.2 Formatting

- Format with pgindent using the tree's own typedefs list, `src/tools/pgindent/typedefs.list`. The tree's `src/tools/pgindent/README` says pgindent needs `pg_bsd_indent` in the `PATH` (source in `src/tools/pg_bsd_indent`) and a specific perltidy version for Perl files.
- Run from the top of the tree: `src/tools/pgindent/pgindent <paths>`. The script also has `--diff` and `--check` options, which show or test changes without rewriting files. Enforced by: `pgindent --check` in CI (the CI job itself is not written yet).
- Every new type that appears in a `typedef` must be added to `src/tools/pgindent/typedefs.list` (the README says to do this when pgindent makes ugly whitespace around new typedefs). Because that file is an upstream file, the register rule in [core-patches.md](core-patches.md) applies: additions to it are the one kind of upstream-file change that is allowed without a patch ID, and they must be a separate commit. **[Recommendation]**
- Tabs are 4 columns wide, as in upstream. Lines are kept to about 80 columns where pgindent allows it.
- Line endings are LF. The repository is configured with `core.autocrlf=false` and `core.eol=lf`; do not let an editor convert files to CRLF. Enforced by: `.gitattributes` and `.editorconfig` already in the tree, and review of diffs for whole-file changes.

### 1.3 Naming

- Every global symbol, type and macro that is not static uses the prefix `kingres_` (functions, variables) or `Kingres` (types), or `KINGRES_` for macros. Within `kingres_ledger` use `kingres_ledger_` for functions; within `kingres_shard` use `kingres_shard_`. **[Recommendation]**
- SQL-callable C functions are named `kingres_<name>` and exported with `PG_FUNCTION_INFO_V1`.
- Every GUC is named with the extension prefix: `kingres_shard.<name>` or `kingres_ledger.<name>`. Register prefixes with `MarkGUCPrefixReserved()` in `_PG_init`. Enforced by: the regression tests list the GUCs (`SHOW ALL` filtered by prefix) so a GUC without the prefix fails review.
- Static helper functions have no prefix requirement but must not collide with upstream names.

### 1.4 Error reporting

- Report errors with `ereport(ERROR, (errcode(...), errmsg(...)))`. Choose a specific `errcode` from `src/backend/utils/errcodes.txt`; do not use `ERRCODE_INTERNAL_ERROR` for conditions a user can cause. Use `elog` only for "cannot happen" internal errors.
- Follow the message style guide in the PostgreSQL documentation ("Error Message Style Guide"): primary messages are short, start in lower case, have no trailing period; `errdetail` and `errhint` are full sentences.
- Wrap user-visible strings in `_()` only if the extension ships translations. Until then, plain strings. **[Recommendation]**
- Enforced by: review, and regression tests that assert the SQLSTATE and message of every error path (`\set VERBOSITY` as needed).

### 1.5 Memory

- Never call `malloc`, `free`, `realloc` in backend code. Use `palloc`, `pfree`, `repalloc` in the current memory context.
- Long-lived data (caches such as the bucket map) is allocated in a named context created with `AllocSetContextCreate` under `TopMemoryContext` or `CacheMemoryContext`. Per-call data lives in the context the function was called in or in a dedicated short-lived context that is reset.
- Free what you allocated in a long-lived context when the entry is invalidated. Do not rely on process exit.
- Shared memory is requested only in `shmem_request_hook` and initialised in `shmem_startup_hook`, and its size is computed in one place.

### 1.6 PG_TRY and exceptions

- Prefer resource owners, memory contexts and `PG_FINALLY` cleanup over wrapping code in `PG_TRY`.
- Inside a `PG_TRY` block, local variables that are modified and read after a `PG_CATCH` must be declared `volatile`.
- Never `return`, `goto` or `break` out of a `PG_TRY` block; the saved exception-stack state is restored only at the end of the block. Never swallow an error without either re-throwing with `PG_RE_THROW()` or fully cleaning up with `FlushErrorState()` after `CopyErrorData()` in a saved memory context.
- Do not call `ereport(ERROR)` while holding a spinlock. Hold LWLocks only across code that cannot fail, or rely on the abort path releasing them.
- Enforced by: review. A review checklist item exists for every `PG_TRY` in a patch.

### 1.7 Hooks

- Every hook you install saves the previous value in a static variable in `_PG_init` and calls it, or the standard function, from your hook. Pattern:

  ```c
  static planner_hook_type prev_planner_hook = NULL;

  void
  _PG_init(void)
  {
      prev_planner_hook = planner_hook;
      planner_hook = kingres_shard_planner;
  }

  static PlannedStmt *
  kingres_shard_planner(Query *parse, const char *query_string,
                         int cursorOptions, ParamListInfo boundParams)
  {
      /* ... our logic; decline and fall through when the query is not ours ... */
      if (prev_planner_hook)
          return prev_planner_hook(parse, query_string, cursorOptions,
                                   boundParams);
      return standard_planner(parse, query_string, cursorOptions, boundParams);
  }
  ```

  The exact parameter list differs between hooks and between PostgreSQL versions; copy it from the header of the base version.
- A hook that does not apply to the current query must behave as if it were not installed.
- Enforced by: a regression test that loads another extension using the same hook (for example `pg_stat_statements` or `auto_explain`) together with the KingresDB extension. **[Recommendation]**

### 1.8 Files and headers

- Layout of an extension directory: `kingres_<name>.control`, `kingres_<name>--<version>.sql`, upgrade scripts `kingres_<name>--<old>--<new>.sql`, C sources, `Makefile`, `meson.build`, `sql/` and `expected/` for pg_regress, `t/` for TAP tests, `README.md`.
- Each `.c` file has a matching `.h` for anything used from another file. Public declarations for the extension go in `kingres_<name>.h`. Headers have include guards named `KINGRES_<NAME>_<FILE>_H`.
- Include order: `postgres.h` first in every `.c` file (never in a header), then system headers, then PostgreSQL headers in alphabetical order, then local headers. This is the upstream convention.
- Every extension source file contains `PG_MODULE_MAGIC;` exactly once (in the main file). `_PG_init` is the only place that installs hooks, defines GUCs and requests shared memory.

### 1.9 Comments

- Every file starts with a header comment in the upstream form: a banner with the one-line purpose, the copyright and licence notice, and `IDENTIFICATION` with the path. Below the banner, a paragraph says what the file contains and how it fits with the other files. Example:

  ```c
  /*-------------------------------------------------------------------------
   *
   * bucket_map.c
   *    In-memory cache of kingres.bucket_map and the lookup functions
   *    that turn a distribution key into a group.
   *
   * SPDX-License-Identifier: PostgreSQL
   * Copyright (c) 2026, KingresDB contributors
   *
   * IDENTIFICATION
   *    contrib/kingres_shard/bucket_map.c
   *
   *-------------------------------------------------------------------------
   */
  ```

  The exact copyright holder string is to be confirmed once the hosting organisation exists.
- Every non-trivial function has a comment block directly before it that says what the function does and why it exists or why it works this way: inputs, outputs, side effects, locking, memory-context assumptions, error behaviour. "Non-trivial" means more than a few lines or any function with side effects outside its arguments.
- Inside functions, comments explain why (an invariant, a PostgreSQL rule, a race that is being avoided), not what the next line does.
- A comment that refers to a design decision names the section of `docs/architecture.md` or the ADR.

### 1.10 Build files

- Each extension has both a `Makefile` (PGXS) and a `meson.build`. The tree's `contrib/pg_buffercache/Makefile` and `meson.build` are the models: the Makefile supports `USE_PGXS=1` (build outside the tree against an installed `pg_config`) and the in-tree path.
- Both files list the same sources, the same `DATA` / `install_data` files (including every upgrade script), the same regression tests and the same TAP tests. A new file added to one must be added to the other. Enforced by: CI builds the extension with both systems (not written yet); until then, review.
- Both build files set the `PGFILEDESC` / Windows resource description.

### 1.11 SQL install and upgrade scripts

- The control file `default_version` names the current version. The install script `kingres_<name>--<version>.sql` creates the full current state. Each released version has an upgrade script `kingres_<name>--<old>--<new>.sql`.
- Scripts start with the guard `\echo Use "CREATE EXTENSION kingres_<name>" to load this file. \quit`, as contrib scripts do.
- A released script is never edited; schema changes are a new version and a new upgrade script. Enforced by: a test that creates the extension at the oldest supported version and runs `ALTER EXTENSION ... UPDATE` to the latest, then compares the catalog with a fresh install. **[Recommendation]**
- Ledger hash formats are versioned with `hash_version`; an upgrade script must never change how existing hashes are computed (see the ledger section of the architecture document).

### 1.12 Tests

- Each extension has pg_regress tests (`sql/` and `expected/`) and TAP tests (`t/*.pl`) where more than one server or a restart is involved. See [testing.md](testing.md).

### 1.13 Several PostgreSQL majors

One source of each extension builds against every supported major, 18 and 19 (ADR 0023).

- Each extension has one compatibility header, `src/pg_compat.h`. It holds every difference between the majors: a renamed function or type, a changed signature, a header that moved, a changed struct member, a compiler-flag consequence. It is included by the extension's main header, after `postgres.h`.
- Nothing outside `pg_compat.h` tests `PG_VERSION_NUM` or `PG_MAJORVERSION_NUM`, in C or in a build file. Code calls a macro or a `static inline` function that the header defines for each major, named after what the code means (for example `KS_TABLE_BEGINSCAN`), and the header's comment names the upstream change it follows. Enforced by: `grep -rn PG_VERSION_NUM contrib/kingres_*` lists only `pg_compat.h` files; review.
- The header uses the newest major's spelling as the one the code is written in where that is natural, and defines it for the older major; when a major is dropped its branch of each `#if` is removed and the wrapper can go. A difference that cannot be hidden without changing behaviour on the older major is not hidden by guessing: it is described in the pull request and decided there.
- A feature that exists on a newer major alone lives in its own file, compiled on that major only; on the older major the SQL function it backs raises a clear "needs PostgreSQL N" error. The SQL scripts are the same on every major; where a catalog differs, the script or the C code decides at run time.
- No compiler warning on any supported major, and no `-Wno-*` flag to hide one. `kingres/dev/ext-majors.sh` builds with `-Werror` on each major. Enforced by: that script.
- A change is committed only when the regression and TAP suites pass on all supported majors ([testing.md](testing.md)). Expected output that legitimately differs between majors uses PostgreSQL's alternative expected files (`expected/<test>_1.out`).

## 2. SQL and PL/pgSQL

- All KingresDB objects are in the schema `kingres`. Do not create objects in `public`.
- Names: lower case, `snake_case`. Tables are singular nouns (`kingres.node`, `kingres.bucket_map`); functions are verbs for actions (`kingres.ledger_verify`) and nouns for lookups; boolean columns start with `is_` or `should_` when the architecture document does so. Constraints and indexes are named explicitly (`<table>_<column>_key`, `<table>_<column>_idx`).
- Every function sets `search_path` explicitly: `SET search_path = pg_catalog, kingres` (add `pg_temp` last for functions that must not see temporary objects before `pg_catalog`, according to the PostgreSQL documentation for writing `SECURITY DEFINER` functions). Qualify operators and functions with `pg_catalog.` where an unqualified name could be captured by another schema.
- `SECURITY DEFINER` is allowed only when the function must do something the caller may not do. A `SECURITY DEFINER` function must: set `search_path` as above, be owned by a dedicated role with the minimum privileges, have `REVOKE ALL ... FROM PUBLIC` followed by explicit `GRANT EXECUTE` to the intended roles, and validate every argument. Default is `SECURITY INVOKER`.
- Dynamic SQL uses `format()` with `%I` for identifiers and `%L` for literals, or `quote_ident` / `quote_literal`, never string concatenation of user input.
- No `SELECT *` in functions or views that are part of an interface; list columns.
- Set `COMMENT ON` for every table, column and function that is part of the public interface.
- Enforced by: a catalog test in pg_regress that fails if a `kingres` function lacks `proconfig` with `search_path`, or if an object exists outside the `kingres` schema. **[Recommendation]**

## 3. Go (control plane)

Tool version in use today: `go1.27.3`.

- Layout: `cmd/<binary>/main.go` for each executable (`kingresd`, `kingres-cli`, `kingres-verifier`), and `internal/` for everything else. Nothing is exported from the module unless another repository needs it; that decision is recorded in an ADR. One `go.mod` for the control plane. The exact module path is to be confirmed once the repository host exists.
- Formatting and static checks: `gofmt` (or `gofmt -s`) with no diff, `go vet ./...` clean, `golangci-lint run` clean. The `golangci-lint` configuration file (`.golangci.yml`) will be checked into `kingres/control-plane/`; the list of enabled linters is to be confirmed when it is written. Enforced by: CI (not written yet) and a pre-commit check.
- `context.Context` is the first parameter of every function that does I/O, blocks or can be cancelled, named `ctx`. Never store a context in a struct.
- Errors: return errors, do not panic for expected conditions. Wrap with `fmt.Errorf("doing X: %w", err)`; add the operation, not the word "failed". Compare with `errors.Is` / `errors.As`. Define sentinel errors or typed errors for conditions callers branch on.
- Logging: `log/slog` only. Pass a `*slog.Logger` to constructors; do not use the default logger from library code. Log with key-value attributes. Never log secrets, passwords, tokens or connection strings with passwords.
- No global mutable state and no `init()` that does work. Dependencies are passed in constructors. Configuration is parsed once in `main` into a struct.
- Interfaces are defined where they are used (the consumer package), are small, and are not created before a second implementation or a test needs them. Return concrete types from constructors.
- Concurrency: every goroutine has an owner that waits for it or cancels it through its context. Run tests with `-race` in CI. Use `errgroup` only after the dependency policy below is applied (the standard library alone is acceptable).
- Tests: table-driven with `t.Run` subtests; test files next to the code; use `t.Helper()` and `t.Cleanup()`; no sleeping to wait for conditions. See [testing.md](testing.md).
- Documentation: every exported identifier has a doc comment starting with its name. Every package has a `doc.go` or package comment stating its purpose. Enforced by: the `revive` or `staticcheck` rules for exported comments enabled in `golangci-lint` (to be confirmed which linter), and review.
- Dependency policy: standard library first. A new third-party module requires, before it is added to `go.mod`: (1) a check of its licence against the project licence policy in section 5 (permissive licences such as MIT, BSD, Apache-2.0 are candidates; AGPL and other strong copyleft are not accepted without an ADR), (2) a check that it is maintained, and (3) an entry in an ADR or, for small libraries, a note in the work-log entry that adds it, naming the module, version, licence and why the standard library is not enough. `go.sum` is committed. Enforced by: review of `go.mod` changes; a licence-scanning tool in CI is to be decided.
- The REST server code is written against `kingres/api/openapi.yaml`. A handler that is not in the spec is a bug. See section 4 and [testing.md](testing.md).
- Embedded etcd, if used (architecture document, "One package"), is a dependency like any other and goes through the same policy.

## 4. TypeScript and Vue (web panel)

The panel is written in Vue 3 with TypeScript ([ADR 0017](../adr/0017-vue-for-the-web-panel.md)).

Tool version in use today: Node.js `v24.22.1`.

- TypeScript `strict: true` in `tsconfig.json`, plus `noUncheckedIndexedAccess` and `noImplicitOverride`. No `any`: use `unknown` and narrow it. No `@ts-ignore`; `@ts-expect-error` needs a comment explaining why. Enforced by: `vue-tsc --noEmit` in CI (it type-checks the templates as well as the scripts), and the ESLint rule `@typescript-eslint/no-explicit-any` set to `error`.
- Linting and formatting: ESLint with a flat config (`eslint.config.js`) and Prettier. Prettier owns formatting; ESLint owns correctness. CI fails on any lint warning. The plugin list (typescript-eslint, `eslint-plugin-vue`, `eslint-plugin-vuejs-accessibility`) is to be confirmed when the config is written.
- Components are single-file components using `<script setup lang="ts">` and the Composition API. No Options API and no class components. Props and emits are declared with types (`defineProps<...>()`, `defineEmits<...>()`). Reusable stateful logic lives in composables (`useXxx`), not in mixins.
- Server state (anything fetched from the REST API) goes through one query library (TanStack Query for Vue is the candidate; the choice is to be recorded when the web project is created). Components do not call `fetch` directly. Client-only state that is shared between pages (the session, the theme) lives in a Pinia store; local UI state uses `ref`/`reactive`. Do not copy server data into a store or into local state.
- API client: generated from `kingres/api/openapi.yaml`, not written by hand. The generator is to be chosen and recorded in an ADR. The generated code is either committed in a marked directory and never edited by hand, or generated at build time; whichever is chosen, CI fails if the generated output differs from the spec. Request and response types come only from the generated client.
- Accessibility (the panel is a tool used under pressure; it must work with a keyboard and a screen reader):
  - Use semantic HTML elements (`button`, `a`, `table`, `nav`, `main`, headings in order) before ARIA.
  - Every interactive control is reachable and operable with the keyboard, with a visible focus indicator.
  - Every form control has a programmatically associated label; every icon-only button has an accessible name.
  - Status is never conveyed by colour alone: the server status badges (LIVE, SUSPECT, DRAINING, DECOMMISSIONING, DEAD, PENDING REBALANCE) carry text.
  - Dialogs trap and restore focus; destructive actions ask for confirmation.
  - Enforced by: `eslint-plugin-vuejs-accessibility`, Testing Library queries by role and label (a test that cannot find a control by role or label is a finding), and a manual keyboard pass before each phase release.
- Tests: Vitest with Testing Library (`@testing-library/vue`). Test behaviour through roles and visible text, not implementation details. API calls in tests are mocked at the network layer (the tool is to be confirmed, for example MSW) using responses that conform to the OpenAPI spec.
- User-visible text is English. Strings are kept in one place per feature so that translation could be added later.
- Dependencies follow the same policy as Go: check the licence, prefer small and maintained libraries, record the choice. The charting and topology libraries named in the architecture document (ECharts or uPlot, and a graph library for the topology view; the document named React Flow, whose Vue counterpart is Vue Flow) are candidates, not decisions.
- `package-lock.json` (or the lock file of the chosen package manager) is committed. The package manager is to be confirmed.

## 5. Cross-cutting rules

- **English only.** Code, identifiers, comments, commit messages, documentation, file and directory names, test data and log messages are in English. No non-English text anywhere in the repository. Enforced by: a CI check that scans new and changed files for characters outside printable ASCII, with an allow-list for files that legitimately need them (for example upstream files and test data for encoding tests). **[Recommendation]**
- **Comments say why.** A comment that repeats the code is deleted. A comment that records a constraint, an upstream behaviour, a failure mode or a decision is required.
- **No secrets in the repository.** No passwords, tokens, private keys, certificates with private keys, or connection strings with credentials, including in tests and examples. Test credentials are generated at test time. Enforced by: a secret-scanning step in CI and pre-commit (tool to be decided); review.
- **Licences.**
  - KingresDB's own code is under the PostgreSQL License, the same as upstream. The upstream `COPYRIGHT` file stays unchanged. A KingresDB copyright notice is added separately; its exact form is to be settled with the legal review named in the architecture document.
  - Citus is licensed under AGPL-3 (architecture document, "The key decision"). Do not copy, translate or closely paraphrase Citus code, tests or comments. Use Citus documentation and the published architecture only as a source of concepts. If you are unsure whether something is a concept or code, ask before writing it.
  - The same caution applies to any other code whose licence is not compatible with the PostgreSQL License. A third-party file copied into the tree keeps its own licence header and is listed in a `THIRD-PARTY` note (the file will be created with the first such import).
  - Code taken from upstream PostgreSQL keeps its upstream header.
- **SPDX header.** Every new KingresDB source file starts with `SPDX-License-Identifier: PostgreSQL` in the first comment (in the form that the file type allows: `/* */` in C, `--` in SQL, `//` in Go and TypeScript, `<!-- -->` in Markdown, `#` in scripts and YAML). Add `Copyright (c) <year>, KingresDB contributors` after it in source files. Files copied from upstream and edited are not re-headed. Enforced by: a header check in CI (to be written).
- **Commit messages.** One logical change per commit. Core patches follow the extra rules in [core-patches.md](core-patches.md).
- **Platform.** C code is built and tested in WSL2 (Ubuntu 24.04) on the Windows 11 developer machine; Go and Node.js build natively on Windows. Code must not assume either platform where upstream does not.
- **Needs-verification discipline.** A design item labelled **[Needs verification]** in the architecture document is checked against the PostgreSQL source or documentation before code depends on it. The result of the check goes in the work log.

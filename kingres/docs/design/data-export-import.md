<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Data export and import from the console: design

Status: proposed, 2026-11-22; **revised the same day after the independent review**
([../reviews/2026-11-22-data-export-import-design.md](../reviews/2026-11-22-data-export-import-design.md): 0 critical,
5 high, 16 medium, 6 low; every finding is answered in section "Review findings and where they are answered"). Not
implemented. The phase number (14) is a proposal; the integrator assigns it.
Builds on the control plane of phase 3 ([control-plane-3.md](control-plane-3.md)), the table lifecycle of phase 9
([shard-phase9.md](shard-phase9.md)), the configuration of phase 13 ([config-phase13.md](config-phase13.md)) and the
row router of phase 2b ([shard-phase2b.md](shard-phase2b.md)). It revises the REST contract in three additive steps,
**0.19.0** (14a), **0.20.0** (14b) and **0.21.0** (14c); the numbers are placeholders and the integrator renumbers
(none of the steps changes the meaning of an operation, field or enum value of 0.14.0). Decisions that deserve a
record are in [ADR 0027](../adr/0027-transfers-are-staged-encrypted-files-run-as-the-requesting-role.md) and
[ADR 0028](../adr/0028-ledger-tables-are-carried-as-data-and-restart-their-chain.md).

The project owner asked for data export and import in the admin console. This note fixes what that means for a
sharded database: four features (a table to CSV, a CSV into a table, a database to one archive, an archive into a
new database), and everything around them that makes them safe to ship: who may use them, what is recorded, what
they cost, where files live, and what a hostile file can and cannot do. **The review's main advice is taken: the
work is cut into three phases that are released and reviewed one after the other** (section "Phases"), and the
first one carries none of the three hardest risks (untrusted SQL, normalising `pg_dump` for a sharded schema,
consistency across tables).

## Sources read

| Source | What it settled |
|---|---|
| `kingres/control-plane/internal/task/task.go` | The task engine: `Step` (`Interruptible`, `NonIdempotent`, `NeedsSecret`), `Definition` (`Dynamic`, `OnFail`, `Cancellable`), the lease and its epoch, one unfinished task per subject, `Env.SetDetail`, `Pause`/`Skip`, tasks never store a secret |
| `internal/ops/tablelife.go`, `tableprep.go`, `tables.go` | How a table task is built: parameters in the task input, step names, subject `database/schema.table`, codes (`insufficient_disk`, `table_not_eligible`), `EnableLedger` run with the service role |
| `internal/ops/query.go` | The workbench: a connection as the signed-in role (`Roles.Connect`), `refuseDangerousRole`, one statement per request through the extended protocol, the limits on rows, values and bytes, `StatementFingerprint` (length and hash prefix instead of text in the audit log) |
| `internal/cluster/proxy.go`, `httpapi/guard.go` (`checkSession`) | `Proxy.Connect`: a connection as a role that signed in since `kingresd` started, over the unix socket with peer authentication; a forwarded request makes its role connectable on the leader |
| `internal/httpapi/forward.go`, `internalapi/internalapi.go`, `control-plane/internal-api.md` | Forwarding to the leader: the body is read whole, the answer is buffered (`maxForwardedResponse` 32 MiB), calls to the leader accept 16 MiB; `leaderBoundReads`; the proof of a session |
| `internal/httpapi/guard.go` (`serveOp`), `internal/config/config.go`, `cmd/kingresd/main.go` | Request timeout 30 s, read timeout 30 s, write timeout 60 s, body limit 1 MiB for mutating operations, TLS from `tls-cert-file` and `tls-key-file` |
| `internal/ops/ledger_bundle.go`, `httpapi/handlers_ledger_client.go` (`getLedgerBundle`) | The one file the control plane already stages and serves: a numbered file under the data directory of the leader, 24 hours, `Content-Length`, `Content-Disposition`, `io.Copy` |
| `internal/audit/audit.go`, `internal/contract` (`IsAuditAction`), `api/README.md` | Audit events: closed `AuditAction` list, details never hold secrets or statement text |
| `internal/auth/roles.go`, `api/README.md` ("Authentication and roles") | Four cluster roles on a strict ladder (`kingres_stats` < `readonly` < `ops` < `admin`); a role change ends the sessions |
| `kingres/api/openapi.yaml` 0.14.0 | Conventions (`x-required-role`, problem codes, `Task`, `TaskKind`, `AcceptedTask`, `ClusterSettings`, `updateClusterSettings`, `DatabaseCreateRequest`), 140 operations |
| `contrib/kingres_shard/README.md` ("Guarantees", "Bulk writes through the row router", "COPY", limitations, restore notes) and `kingres_shard--0.7.sql` (`dist_table`, `colocation`, `bucket_map`, `dist_partition`) | What `COPY FROM` and `COPY TO` do on each table kind, which options are refused, that there is no global snapshot, that a dump of a node is a dump of its partitions, the catalog tables to read |
| `docs/design/shard-phase11.md` | Distributed schemas (`kingres.schemas`, `dist_table.method = 'schema'`): the catalog document must handle their absence, as extension 0.8 is not in this tree |
| `contrib/kingres_ledger/README.md` ("Dump, restore and replication", "Updatable tables", the signature policy) | A ledger table is a user table with capture triggers; it cannot be partitioned; `ledger_enable` enters existing rows; a plain dump leaves capture triggers of unregistered tables that the verifier names |
| `contrib/kingres_timepart/README.md` ("Dump, restore, rename, drop"), `kingres_timepart--0.2.sql` | Policies are inert after a restore until `timepart_adopt()`; `kingres.timepart_policy` and `timepart_set_policy` |
| `contrib/kingres_columnar/README.md` | The `columnar` access method and its options |
| `kingres/web/src/pages/DatabaseDetailPage.vue`, `DatabasesPage.vue`, `components/TaskActionDialog.vue`, `TaskProgress.vue`, `GateButton.vue`, `router` | Where the actions go, how a task dialog behaves, the row actions of the tables list |
| `docs/design/shard-phase9.md`, `config-phase13.md` | Style, and the conventions this note follows (facts checked, contract additions, increments) |

## Goals

1. **Export one table to CSV**: distributed, reference, local, and partitioned tables, with an exact statement of
   what is consistent, as a task that ends in a file the browser downloads.
   **[14a]**
2. **Import a CSV into an existing table** through routed `COPY`, with the options of CSV (header, delimiter, null
   string, quote, encoding), **all or nothing** (batches are designed and deferred, "Batches"), progress, cancel and
   bounded memory. **[14a]**
3. **Export one database to one archive**: the schema, the KingresDB catalog (distribution, colocation, reference
   tables, access methods, time-partition policies, ledger tables, sequences), the data of every table read through
   the coordinator, and a manifest with versions and checksums; and **validate an archive** without importing it.
   **[14b]**
4. **Import such an archive into a new database**: validate the archive first, recreate the extensions and the
   structure, recreate the distribution **before** the data is loaded, load the data, finish the constraints, and
   refuse archives of incompatible versions. **[14c]**
5. Say who may do each of these, what is audited, what is limited, where files live and how long, how they are
   encrypted, and what a hostile file can and cannot do.

### Not in scope

- **A global snapshot.** Phase 8 is future roadmap. Every consistency statement below is for the cluster as it is.
- **Replacing or merging rows on import** (`TRUNCATE` first, `ON CONFLICT DO UPDATE`). Import adds rows. The router
  refuses `ON CONFLICT DO UPDATE` and routed `COPY` has no such option (README of `kingres_shard`); a "replace"
  that deletes first would be a destructive operation with its own confirmation and is a later feature.
- **Skipping bad rows** (`ON_ERROR ignore`, `REJECT_LIMIT`). Routed `COPY` refuses both (README, "COPY FROM"). The
  console does not offer on a local table what it cannot offer on a distributed one.
- **Formats other than CSV** for a table (JSON lines, Parquet, Excel). **Binary** `COPY` is not used either: its
  encoding depends on the version of the server and of the extension types.
- **Online import of a database into an existing one**, and a **cluster-wide** export (roles, other databases,
  settings). The archive holds one database. Roles are not in it (see "Roles of the archive").
- **Replacing `pg_dump` and backups.** The archive is a portable logical copy for databases that fit the limits
  below. Physical backup and point-in-time recovery are another feature.
- **Encrypting the downloaded file with a passphrase** (see "Known limits"; the staged file is encrypted, the
  file that the browser receives is as readable as a `pg_dump` file).
- **Resuming a task after a leader change.** It is refused with a clear code and repeated (section "Leader change
  and failover").

## Phases

Headings of this note carry the phase that builds the section: **[14a]**, **[14b]**, **[14c]**, or **[14a+]** for a
design that is written down now and built later. A section without a tag applies to every phase.

| Phase | Scope | Contract | Gate |
|---|---|---|---|
| **14a** (first release) | The setting `dataTransfer`, the transfer record, the staging store (sealed files, quota, janitor), the streaming relay, resumable upload, download with `Range`, **table CSV export** for local, reference, distributed and partitioned tables, **table CSV import, all or nothing only**: an explicit `BEGIN`, one `COPY`, a comparison of the row count, then `COMMIT`, with a size cap (`maxAtomicBytes`, 2 GiB) and **error text taken from a whitelist** (never PostgreSQL's message or detail) | 0.19.0: 10 operations (140 to 150) | Spikes S1 and S2 passed; independent review of the relay and of the import transaction |
| **14b** | **Database export** to an archive and **validation of an archive** (read-only: nothing is imported). The archive is useful alone: an operator can inspect it and load it by hand | 0.20.0: 2 operations (150 to 152) | Spike S3 passed; the supported subset of `pg_dump` output is written into this note; independent review |
| **14c** | **Database import**: a shape-grammar classifier for the schema, a private database name, distribution before data, views after distribution, ledger and time-partition policies. **No `allowCode`, no `privileges: restore`** | 0.21.0: 1 operation (152 to 153) | Independent review of the classifier and of the import steps; the round trip on PostgreSQL 18 and 19 |
| **14a+** | **Batch import** with a marker in the same distributed transaction (designed in "Batches"), "continue" after a failed batch, reference tables in several transactions | none | Own review; built only when the owner asks |

What is deliberately **not** in 14a: batches and `skipRows` (a large file is split by the user: the console says
so), reference tables unless spike S2 shows the batch form of the router is accepted (section "Reference tables"),
everything that touches an archive.

## Facts checked

Everything the design depends on was read in the tree at this commit. Items that could not be checked without a
running cluster are **[Needs verification]** and are checked by the first increment that depends on them
(section "Increments"); each has a fallback.

| # | Statement | Source / outcome |
|---|---|---|
| F1 | A request that is forwarded to the leader cannot stream. `tryForward` reads the body with `io.ReadAll`, the leader answers into a `bufferedResponse` capped at 32 MiB, and `POST /internal/v1/forward` accepts at most 16 MiB | `httpapi/forward.go`, `internalapi/internalapi.go` (limit 16 MiB for `ToLeader` calls). A table export or an archive does not fit. **A streaming relay is new work** (increment 2) |
| F2 | The public server ends every request after 30 s of reading (`ReadTimeout`), 60 s of writing (`WriteTimeout`) and 30 s of processing (`RequestTimeout`), and mutating requests have a 1 MiB body limit (`MaxBodyBytes`) | `config.go` defaults, `main.go` (`http.Server`), `guard.go` (`serveOp`). The transfer operations must extend the deadlines per read and per write with `http.ResponseController`, and must not use `MaxBodyBytes` |
| F3 | A connection as the signed-in role exists only on a server where that role signed in or was forwarded since `kingresd` started (`Proxy.Allow`, `Reset` at start, `ErrNotEnabled`), and the workbench refuses a role that is a superuser, has `REPLICATION` or `BYPASSRLS`, or is a member of the four predefined roles that read files, run programs or read the log | `cluster/proxy.go`, `guard.go` (the forwarded identity calls `Proxy.Allow` on the leader), `ops/query.go` (`refuseDangerousRole`, `DangerousPredefinedRoles`) |
| F4 | A task runs on the leader, holds a lease, is resumed by whoever claims it next, and never stores a secret. A step that was running when the lease was lost runs again unless it is `NonIdempotent`, which fails the task with `step_interrupted` | `task/task.go` |
| F5 | One unfinished task per subject. A table task has the subject `database/schema.table`, a database task the database name | `task/task.go` (`ErrSubjectBusy`), `api/README.md` ("Task kinds and subjects") |
| F6 | The control plane already stages a file for a download: `ledger.bundle.create` builds `bundle-<id>.tar.gz` under `<DataDir>/ledger/bundles` (directory mode 0700), keeps it 24 hours and `getLedgerBundle` serves it with `Content-Length` and `Content-Disposition`; the file lives on the leader that built it | `ops/ledger_bundle.go`, `cmd/kingresd/ledger_signing.go`, `handlers_ledger_client.go` |
| F7 | `COPY ... FROM STDIN` into a distributed table is run by the row router: it parses `FORMAT text\|csv\|binary`, `DELIMITER`, `NULL`, `HEADER`, `QUOTE`, `ESCAPE`, `ENCODING`, `FORCE_NOT_NULL`, `FORCE_NULL`, `DEFAULT` and column lists, and **refuses** `FREEZE`, `WHERE`, `ON_ERROR`, `REJECT_LIMIT`, `LOG_VERBOSITY`, row-level security, a reference table and a bucket partition named directly; it is atomic across groups (two-phase commit); it keeps about two batches (`insert_batch_bytes`) per group in memory; `pg_stat_progress_copy` is not updated | README of `kingres_shard`, "Bulk writes through the row router", "COPY FROM". Consequences: the control plane counts progress itself; a reference table needs another path (F8) |
| F8 | `COPY ... FROM` into a reference table is refused on a cluster with several groups; a write to a reference table is run on every node | README of `kingres_shard` ("reference tables"). **[Needs verification] V3:** whether `INSERT INTO ref SELECT u.c::type FROM unnest($1::text[], ...) u(c, ...)` (the batch form of the router) is accepted for a reference target. Fallback: multi-row `INSERT ... VALUES` with typed parameters |
| F9 | `COPY <distributed table> TO` is run as `COPY (SELECT <columns> FROM <table>) TO`; the core refuses `COPY` of a partitioned table with a hint to use the query form | README of `kingres_shard`. The control plane always uses the query form, so one statement shape serves every table kind |
| F10 | **There is no global snapshot.** Two-phase commit makes a transaction commit on all groups or none, but each group commits at its own moment. In `READ COMMITTED` every scan node of a statement opens its own cursor with its own snapshot; the local buckets are read with the statement snapshot taken at the start. Every row read is committed. A reader can see a transaction in one group and not in another | README of `kingres_shard`, "Atomicity is not a global snapshot" |
| F11 | A `REPEATABLE READ` reader delays the switch of a bucket move; a bucket that moved between the plan and the first batch makes a routed write fail with the retryable `KS001` | README of `kingres_shard`, "Buckets that move and groups that change their primary". The transfers use short `READ COMMITTED` statements |
| F12 | `pg_dump` of a node holds that node's partitions (`COPY public.d_b0001 ...`), not the table as the user wrote it; the extension's own catalog tables are in schema `kingres` and are not part of a logical copy | README of `kingres_shard` (restore notes, 963 and 990). **[Needs verification] V1:** the exact text `pg_dump` writes for a distributed table, its bucket partitions, a user-partitioned table and its leaves, on PostgreSQL 18 and 19. The normaliser (below) refuses what it does not recognise |
| F13 | pg_dump of PostgreSQL 18 point releases writes `\restrict <token>` and `\unrestrict <token>` lines in plain output. **[Needs verification] V2** against the pg_dump built from this tree. The lexer below accepts exactly those two forms and refuses every other backslash command |
| F14 | A ledger table is a user table with capture triggers (two for an updatable one, plus its history table `<table>_ledger_history`); it **cannot be partitioned**, a partition or a parent, so it can never be a distributed table; `ledger_enable` enters the existing rows as `I` entries; `COPY` goes through the capture trigger like an insert; a table with the policy `required` refuses unattested writes (`KL010`); a plain dump leaves capture triggers of tables that are not registered in the new database and the verifier names them | README of `kingres_ledger` (updatable tables, signature policy, "Dump, restore and replication") |
| F15 | The policies of `kingres_timepart` come back from a dump inert; `kingres.timepart_set_policy(...)` sets one as the caller, and the view `kingres.timepart_policy` shows the policies of the tables a role owns | README of `kingres_timepart`, `kingres_timepart--0.2.sql` |
| F16 | `DatabaseCreateRequest` creates the database with `kingres_shard` and `kingres_ledger` as selected; the other extensions of the project (`kingres_columnar`, `kingres_timepart`, `kingres_sketch`) are not selectable in it | `openapi.yaml` (`createDatabase`, `DatabaseCreateRequest`). The import installs them in a step of its own, from a closed list |
| F17 | The session cookie is `Secure`, `HttpOnly` and `SameSite=Strict`, so the console works only over HTTPS and a page of another site cannot make the browser send it | `httpapi/cookies.go` |
| F18 | Sequences behind the columns of a distributed table are node-local: a row inserted from another node draws from that node's sequence; `create_distributed_table` raises each node's sequence to the largest value of the column on that node | README of `kingres_shard` |
| F19 | The control plane uses the standard library first (`DEPENDENCIES.md`): `archive/zip`, `compress/gzip`, `crypto/aes`, `crypto/cipher`, `encoding/json` are enough for this design; no new module is needed | `control-plane/DEPENDENCIES.md` |
| F20 | `getTask` and `listTasks` need only `kingres_readonly`, and `ops.PGProblem` passes PostgreSQL's `Message` through for most codes; the `DETAIL` and the message of a `COPY` error carry values (`invalid input syntax for type integer: "abc"`, `Key (email)=(bob@x.example) already exists`, `Failing row contains (...)`) | `openapi.yaml` (`x-required-role` of the task operations), `ops/query.go` (`queryProblem`), `ops/ops.go` (`PGProblem`). Review DXR-03: **no server text may reach a task, a problem, the audit log or the log** (section "Error text") |
| F21 | `createDB` in `ops/databases.go` returns without error when the database exists already (it treats it as an earlier run of the same step), so a check made at request time says nothing about step 4 | `ops/databases.go`. Review DXR-10 (section "The task `database.import`") |
| F22 | A role of cluster role `kingres_admin` has `CREATEROLE` and `CREATEDB` in PostgreSQL, so SQL run as it can add role memberships, comment on roles and create databases | `ops/roles.go`, `ops/query.go` (`refuseDangerousTarget`). Review DXR-04, DXR-05, DXR-23 |
| F23 | The `Proxy` map keeps a role connectable until `kingresd` restarts: a role that signed out, was demoted, or changed its password stays connectable on that server | `cluster/proxy.go` (`Reset` at start only, `Revoke` on role change). Review DXR-23: every step re-checks the role |
| F24 | `kingres_shard.dependent_views` is `refuse` by default: a view over a table stops `create_distributed_table`; materialized views are refused even with `recreate`. The extension also refuses a distribution when a trigger, default, check, generated column or index calls a function owned by a non-superuser (`function_owner_not_superuser`) | README of `kingres_shard` ("Dependent views", the table of refusals). Review DXR-07, DXR-22 |
| F25 | A task step that wrote to the store under a lease is checked against the holder and the epoch, so a leader that lost its lease cannot record anything; a `NonIdempotent` step that was running when the previous run ended fails the task with `step_interrupted` | `task/task.go` (`Store`, `Step.NonIdempotent`). The write-ahead **commit intent** of the import rests on this (section "The load") |
| F26 | `createDB` creates the database from `template0` with the requested encoding and `LC_COLLATE`/`LC_CTYPE`; `DatabaseCreateRequest` has `general.encoding`, `lcCollate`, `lcCtype` | `ops/databases.go`, `openapi.yaml`. Review DXR-19 |
| F27 | A retention policy of `kingres_timepart` drops (or detaches) old partitions at the next run, according to the policy's `enabled`, `retention` and `retention_action` | README of `kingres_timepart`. Review DXR-18 |
| F28 | **[Needs verification] V8:** whether `ALTER DATABASE ... RENAME` of a database can be done on every shard group and in the cluster registry (`kingres_cp`) without breaking the catalog of `kingres_shard`, which keys nothing on the name. Decides the private-name step of the import (section "The task `database.import`"); fallback in the same section |
| F29 | **[Needs verification] V9:** which functions a read of a table can reach (virtual generated columns of PostgreSQL 18, expressions of row-level security policies), and whether `kingres_shard` exposes its `function_owner_not_superuser` predicate for reads. The export uses a catalog query of its own if it does not (section "Table export") |

## Decisions in one table

| # | Decision | Phase | Why |
|---|---|---|---|
| 1 | Every export is **staged** on the leader (sealed, with a retention time) and then downloaded; the task is not the HTTP response | 14a | A COPY that fails with `KS001` after the first byte cannot be retried when the bytes are already in the browser; the audit event should carry the final size and checksum before anyone has the data; a `WriteTimeout` of 60 s and a 32 MiB buffered forward (F1, F2) do not allow a response that is the task; the browser can resume a staged download |
| 2 | Export and import need `kingres_admin`, **and** the setting `dataTransfer.enabled` (off by default), **and** the PostgreSQL privileges of the signed-in role, **re-checked at every step**; a staged file can be read and downloaded only by the role that made it. The creator-only rule is defence in depth, not a boundary (an administrator with `CREATEROLE` can become the creator, F22) | 14a | The ladder has no separate permission and a fifth cluster role changes every client of the ladder; the controls that carry weight are the setting, the audit trail and PostgreSQL's privileges |
| 3 | A CSV import is **one explicit transaction**: `BEGIN`, `COPY`, comparison of the row count, **write-ahead commit intent**, `COMMIT`. The comparison is a precondition of the commit. A file above `maxAtomicBytes` (2 GiB) is refused: the user splits it | 14a | Review DXR-01, DXR-02: batches cannot be exactly-once without a marker, and a count check after the commit is not all-or-nothing |
| 4 | **No server text** reaches a task, a problem, the audit log or the log from a data operation: errors are built from SQLSTATE, a mapped sentence, constraint, table and column names and the absolute line number | 14a | Review DXR-03: `getTask` is readable by `kingres_readonly` |
| 5 | The archive is a **zip** with fixed entry names, one CSV per table, a JSON catalog, three SQL files and a manifest with SHA-256 sums, written by a streaming writer that the validator accepts **exactly** | 14b | The central directory is a table of contents before anything is read; the manifest is written last |
| 6 | The schema is dumped by `pg_dump` run by the **service role, read-only, schema only**, and turned into the logical schema by a normaliser that **removes and rewrites** a closed set of constructs (not "removes only"); the exporter runs its own output through the importer's validator before sealing | 14b | Review DXR-06, DXR-12: `pg_dump` as the requesting role fails for databases with the project's extensions, and the output for a sharded table needs rewrites. Data is still read as the requesting role |
| 7 | The SQL of an archive is **never run as it is**. A **shape grammar** (a recursive-descent parser per allowed statement kind that consumes the whole statement) decides, the whole plan is checked before a database exists, and statements run **as the requesting role**, one per round trip. **No `allowCode`, no `privileges: restore`.** The guarantee rests on PostgreSQL's privileges; the grammar narrows what is attempted and what resources are used, it does not create the guarantee | 14c | Review DXR-04, DXR-05, DXR-22 |
| 8 | The import works in a **database with a name only this task uses** and renames it to the final name as its last step (V8 decides; fallback in "The task `database.import`") | 14c | Review DXR-10 |
| 9 | **Distribute first, load second**; views, materialized views and what depends on a table are created **after** distribution | 14c | The user asked for it; F24 makes views before distribution fail (DXR-07) |
| 10 | **Ledger tables are exported and imported as data**; the imported table is a **new chain**; the ledger policy is set again after `ledger_verify`, time-partition policies are imported **disabled** (ADR 0028) | 14b, 14c | Review DXR-18 |
| 11 | Data is exported as **CSV in UTF-8 with fixed session formats** and a distinct null marker; the archive never changes a value | 14b | Portable across majors |
| 12 | Table CSV export protects spreadsheets by default (`spreadsheetSafe`); it is a convenience and the dialog says it is not a security feature | 14a | Review DXR-26 |
| 13 | A **leader change ends a transfer** with `transfer_leader_changed` (retryable); nothing is resumed on another server | 14a | Files and keys are on the leader's disk; the role connection exists only where the role signed in (F3) |
| 14 | Uploads are **resumable by offset**, with chunks that are multiples of 64 KiB except the last, streamed through a new relay to the leader | 14a | Large files must survive a dropped connection; the relay is needed for downloads (F1) |
| 15 | **Batches are designed and deferred.** The design makes the outcome decidable with a unique marker row in the same two-phase transaction as the batch | 14a+ | Review DXR-01 |

## The transfer record and the staging store

### The record

A new table `kingres_cp.data_transfer` in the system database (migration `0035_data_transfer.sql`), one row per
staged file, export or upload:

| Column | Meaning |
|---|---|
| `transfer_id` | Number, from a sequence. The name of the file on disk is derived from it only (`transfer-<id>.kdx`), as the bundle file is (F6); nothing a client sends is ever part of a path |
| `kind` | `tableExport`, `databaseExport`, `upload` |
| `state` | `receiving` (upload), `building` (export), `ready`, `failed`, `deleted`, `expired`, `lost` |
| `server_id` | The server whose disk holds the file (the leader that created it) |
| `created_by`, `created_by_oid` | The role that made it, by **name and by OID with the role's creation identity** (DXR-23): a role that was dropped and re-created under the same name does not inherit the files, and the janitor deletes the staged files of a role that is dropped or renamed. The only role that may read, download or use the file |
| `database`, `schema_name`, `table_name` | What it is for or from (null for an upload not yet used) |
| `size_bytes`, `declared_bytes` | Size of the file, and for an upload the size the client declared |
| `sha256` | SHA-256 of the plaintext file, 64 hex characters |
| `format` | `csv`, `csv.gz`, `archive/1` |
| `report` | JSON: the result (rows, consistency, options used) or, for an upload, the archive report of a validation |
| `key_id`, `wrapped_key` | The key that encrypts the file (below); the wrapped data key, not the key itself |
| `created_at`, `expires_at`, `downloaded_at` | `expires_at` is the retention time; `downloaded_at` is set when the last byte was served |
| `task_id` | The task that builds or uses it |

The file is the truth about the bytes, the row is the truth about who may have them and for how long. A row whose file
is missing is set to `lost`. A file without a row is deleted by the janitor **only under the rules below** (DXR-17).

### Where files live

`<transfer-dir>`, default `<DataDir>/transfer`, mode 0700 (a flag, `transfer-dir`; it must be on a file system
with room, and an operator may put it on a volume of its own). Files are created with mode 0600, opened with
`O_NOFOLLOW`, checked to be regular files, and written under the name `transfer-<id>.part` and renamed to `.kdx`
only when complete. The table that an upload or export belongs to is never used in a path.

**The directory belongs to `kingresd`** (the ledger bundle directory is the precedent, F6). At first use `kingresd`
refuses a directory that is not empty and has no marker file written by it (`.kingres-transfer`), and refuses a
directory that **is, or contains, the data directory of `kingresd`, a PostgreSQL data directory, or the root**. The
**janitor deletes only names that match `^transfer-[0-9]+\.(part|kdx)$`** in that directory, never anything else,
**never a file younger than one hour**, and **never when the system database reports a lower highest transfer id than
the files show** (a sign of a restored system database, ADR 0025: the janitor then stops, raises an alert and waits for
an operator). Tests: a misconfigured directory, a foreign file, a young file, a restored database.

**Disk exhaustion** is a design input, not an afterthought; four limits apply and each has a code:

1. **Per file**: `maxFileBytes` (default 20 GiB). An export that grows beyond it stops with `export_too_large`; an
   upload that declares more is refused when it is created (422, `validation_failed` on `/sizeBytes`).
2. **Quota of the staging area**: `stagingQuotaBytes` (default the smaller of 100 GiB and 25 percent of the file
   system). An upload reserves its declared size when it is created. An export reserves an estimate that is **not**
   `pg_total_relation_size` (DXR-16: on a coordinator that covers only the partitions of this node, a columnar table
   is 5 to 10 times smaller than its CSV, `bytea` doubles, `FORCE_QUOTE *` adds two bytes per field): it is the
   **per-group sizes from the catalog times a factor of the stored form** (columnar 10, others 1.5), capped by
   `maxFileBytes`. The reservation **grows as bytes are written**, and the writer that exceeds the quota fails
   (`transfer_quota_exceeded`, a task code) and is cleaned up, so a bad estimate stops the offender and **not its
   neighbours**; the free-space guard below is the last line. A reservation that does not fit is refused with 409
   `transfer_quota_exceeded` and says how much is free and which transfers hold the space (the caller's own, by id;
   other roles' by count only).
3. **Free space on the file system**: a transfer is refused (`insufficient_disk`, the code the table tasks already
   use) when after its reservation less than `max(5 GiB, 10 percent)` of the file system would be free, and a running
   export checks `statfs` every 64 MiB and stops with `insufficient_disk` and cleans up when the reserve is
   reached. The check is the leader's own, and it is repeated for every export step, so a disk that filled up by
   other means ends the transfer instead of the database (the reserve exists to protect the database's own volume
   when the staging directory shares it; the README says to put it elsewhere on a large cluster).
4. **Time**: `retentionHours` (default 24, range 1 to 168). A janitor on the leader runs every 5 minutes: it deletes
   expired and `deleted` files, deletes files with no row, deletes uploads that received nothing for 48 hours or that
   were completed and never used for 24 hours, marks rows without files as `lost`, and writes one audit event per
   purge (below). At leader start it runs once before it accepts a transfer. A failed export deletes its partial
   file at once (an `OnFail` hook of the task kind); a failed import keeps its upload for the retention time so that
   it can be tried again.

### Encryption at rest

Each file has its own random 256-bit data key. The file is written as **fixed 64 KiB segments**, each sealed with
AES-256-GCM (`crypto/aes`, `crypto/cipher`; no new module, F19): the nonce is a random 32-bit prefix per file and
a 64-bit segment counter, the additional data are the transfer id, the segment number and a flag that is set on
the last segment, so a reordered, swapped or truncated file fails to open. Because segments are independent, a
reader can seek (a `Range` download, and the `io.ReaderAt` that `archive/zip` needs for validation). The data key
is wrapped with AES-256-GCM under a **master key** that is read from `<transfer-key-file>` (default
`<DataDir>/transfer.key`, mode 0600, created at first use, **not inside the staging directory**), and the wrapped
key and the id of the master key are in the row. The ledger key files of phase 7 are the precedent for a key that
`kingresd` keeps in its own directory. **Operation of the key** (DXR-24): the file is created with `O_EXCL`, written,
synced, renamed and re-read, so two first uses at once cannot make two keys; its mode (0600) and owner are checked at
every start; **a missing or replaced key file turns the `ready` rows into `lost` with a clear code
(`transfer_key_lost`) and never creates a new key while files under the old one exist**; the key file is in the data
directory and so in file-level backups of it, which the README says.

This protects against what disk-level encryption of the staging volume would not: a copy of the staging directory
(a backup, a snapshot that excludes the key file), blocks of deleted files that were not wiped, and a second
account on the host that can read the directory but not the key file. It does **not** protect against `root` on
the host or against a process that can read both (the same limit as every key kept next to the data). Rotation of
the master key is not in the first version: files live at most 7 days, so a new key id is used for new files and
the old key stays readable until its last file expires.

**In transit.** The public listener serves HTTPS when `tls-cert-file` and `tls-key-file` are set, and the session
cookie is `Secure` (F17). The transfer operations additionally **refuse a request that did not arrive over TLS**
(403 `insecure_transport`), so that a bearer token over plain HTTP cannot carry data. A follower relays to the leader
over the mutual-TLS internal listener. The downloaded file is plaintext once it reaches the browser, as a `pg_dump`
file is.

## Roles, privileges and audit

### Who may do what

| Operation | Cluster role | PostgreSQL privilege that decides the data |
|---|---|---|
| `exportTable` | `kingres_admin` | `SELECT` on the columns exported (the connection is the role's) |
| `importTable` | `kingres_admin` | `INSERT` on the columns loaded; the router checks them at the origin and the groups check them again as the same role (README, "Privileges") |
| `exportDatabase` (14b) | `kingres_admin` | `SELECT` on **every** table and `USAGE` on every schema of the database, checked before anything is written; the first 20 objects the role cannot read are named in a 422 `export_not_readable` |
| `validateDatabaseArchive` (14b), `importDatabase` (14c) | `kingres_admin` | (import) the role creates the objects in the new database as their owner; it must be able to log in and must not be dangerous (below) |
| `createDataUpload`, `uploadDataChunk`, `getDataUpload`, `deleteDataUpload` | `kingres_admin` | none; creator only |
| `listDataExports`, `getDataExport`, `downloadDataExport` | `kingres_admin` | creator only; another role sees 404, so the existence of a file is not leaked |
| `deleteDataExport` | `kingres_admin` | the creator, **or any `kingres_admin`** (to free the disk of a colleague who left; deleting reveals nothing) |
| `cancelTask` of a transfer | `kingres_ops` (the existing operation) | stopping a transfer is not a leak; an operator may stop an administrator's import |
| `getClusterSettings` (field `dataTransfer`) | `kingres_readonly` | existing; `updateClusterSettings` needs `kingres_admin`, as today |

**Why not a separate permission.** The roles are four fixed levels (`auth/roles.go`) and every operation of the
contract names one of them; a role that may export but not administer would need a new level in the ladder, in the
contract's `x-required-role`, in the generated types of the console, and in `setRoleClusterRoles`. The three guards
above do the same work with what exists: the setting is an explicit decision of an administrator, the privileges
bound the data, and the creator-only rule stops a colleague from taking a file by accident (ADR 0027, alternatives). **The creator-only rule is defence in depth, not a boundary** (DXR-23): an administrator has `CREATEROLE` (F22) and can often reset the password of another role or add itself to it and then sign in as the creator. What carries the weight is the setting, the audit trail and PostgreSQL's privileges.

**The role is checked again at every step** (F23, DXR-23). The `Proxy` keeps a role connectable until `kingresd` restarts, so a transfer of up to 24 hours would run on with a role that signed out, was demoted from `kingres_admin`, changed its password or expired. Each step therefore re-reads the cluster roles of the role (as the guard does for a forwarded request), `rolcanlogin`, `rolvaliduntil` and the role identity (OID and creation time) of the record, and ends the task with `forbidden` and nothing stored when any of them fails.

**The setting.** `ClusterSettings.dataTransfer` (additive): `enabled` (default `false`), `retentionHours`,
`maxFileBytes`, `stagingQuotaBytes`, `maxConcurrent` (default 2 transfers running in the cluster, 1 per role). While
`enabled` is false every transfer operation answers 403 `data_transfer_disabled`, the console shows the actions
disabled with the reason, and turning it on is an audited `updateClusterSettings`.

**Dangerous roles.** `importDatabase` (14c) runs the statements of the archive as the signed-in role, so it applies the
rule of the workbench (F3): a role with `SUPERUSER`, `REPLICATION` or `BYPASSRLS`, or with access to server files,
programs or the log through a predefined role, is refused (403 `forbidden`, the same text as the workbench). An
administrator whose PostgreSQL role is a superuser must sign in as another role to import a database; the dialog
says so. Table and database **exports** run only statements that the control plane writes itself (a `COPY (SELECT)`
of quoted names, and `pg_dump` with fixed arguments), so they do not need the refusal, and a superuser may export.

### Audit

Events use the existing sink and the closed `AuditAction` list, which gains the new operation ids (all mutating
operations are audited today; `downloadDataExport` is added although it is a read, because it is where data
leaves) and two system events: `dataTransfer.completed` (written by the task when it ends, with the outcome) and
`dataTransfer.purged` (written by the janitor, one per file or one per sweep with a count).

| Event | Details (never data values, never SQL text, never a file name from the client) |
|---|---|
| `exportTable`, `exportDatabase` (request) | database, schema and table, options (delimiter, null string length, encoding, compression, `spreadsheetSafe`, `dataMode`), task id |
| `dataTransfer.completed` (export) | rows and bytes, SHA-256 of the file, `consistency` label per table (below), duration, outcome and the problem code if it failed |
| `downloadDataExport` | transfer id, size, SHA-256, whether it was a range request and whether the last byte was served, source address |
| `createDataUpload`, `deleteDataUpload`, `deleteDataExport` | transfer id, declared size, and for a delete whose actor is not the creator, the creator |
| `importTable` (request) | table, options, upload id and SHA-256, task id |
| `dataTransfer.completed` (import) | rows loaded (the count the server reported, compared with the scanner's), whether the commit intent was written, outcome |
| `validateDatabaseArchive`, `importDatabase` | archive SHA-256, manifest versions, counts of tables by kind, the options as chosen (`onForeignKeyViolation`, `acknowledgeLedgerRestart`), for a refused statement its **kind and fingerprint** (length and hash prefix, as `StatementFingerprint` does for the workbench) and its position, never its text |
| `updateClusterSettings` | the old and new `dataTransfer` values, as for any setting |

An upload chunk is not an event (an upload of 20 GiB is 80 chunks at the default size); the upload's creation, the
completion check and the use of it are. The audit sink being unready refuses a transfer before it starts (`auditReady`,
as every audited operation), so no transfer happens that cannot be recorded.

## Error text [14a]

F20: a task's error and a problem body are readable by `kingres_readonly`, and PostgreSQL's message and detail carry
values. **No transfer operation passes server text on.** One function, `transfer.SafeError(err)`, builds every error
of a transfer from a whitelist:

| Part | Source |
|---|---|
| `code` | the transfer code, or `sql_error` with the SQLSTATE |
| `sqlState` | the five characters |
| sentence | a fixed text **per SQLSTATE**: `22P02` "a value does not have the type of its column", `22001` "a value is too long for its column", `22003` "a number is out of range", `22007` / `22008` "a date or time is not valid", `22021` "the data is not valid in the encoding", `22P04` "the CSV is malformed", `23502` "a required column is empty", `23503` "a foreign key is violated", `23505` "a unique constraint is violated", `23514` "a check constraint is violated", `42501` "the role does not have the privilege", `57014` "the statement was cancelled", `KS001` "a bucket moved; repeat", and for every other state "the database refused the statement (SQLSTATE xxxxx)" |
| names | `column_name`, `constraint_name`, `table_name`, `schema_name` from the error's own fields (they are identifiers, not data), each through `safeText` |
| position | the **absolute line number**: the `line N` that PostgreSQL's context gives, extracted by a parser that accepts only the form `line <digits>`, plus the header line when there is one. The column is the column name above |

The message, the detail, the hint, the context text and the failing row are **never** copied, for any class and for
class `XX`. The function is used for the problem of the HTTP request, `TaskError`, `Task.detail`, `TaskStep.detail`,
the audit event and every log line of a transfer. Tests: a CSV whose failing row carries a canary string in every
column of every type (`22P02`, `22001`, `22003`, `23502`, `23503`, `23505`, `23514`, encoding errors) must leave the
canary in **no** task, problem, audit or log output; a fuzz target feeds `SafeError` with arbitrary `PgError`
fields; a mutant puts `Message` back into the problem.

## Limits and timeouts

All values are defaults; the ones that are cluster settings are marked, the others are flags of `kingresd`.

| Limit | Default | Where it applies, and what happens at the limit |
|---|---|---|
| `maxFileBytes` (setting) | 20 GiB | Any staged file. Upload refused when created; export stops with `export_too_large` |
| `stagingQuotaBytes` (setting) | min(100 GiB, 25% of the file system) | Sum of reservations, 409 `transfer_quota_exceeded` |
| Free-space reserve (flag) | max(5 GiB, 10%) | 409/failed `insufficient_disk` |
| `retentionHours` (setting) | 24 (1 to 168) | Export files; uploads: 48 h without data, 24 h unused |
| `maxConcurrent` (setting) | 2 per cluster, 1 per role | 409 `operation_in_progress` with `Retry-After` |
| Upload chunk alignment | a multiple of 64 KiB except the last chunk | 422 `validation_failed` |
| Upload chunk | at most 256 MiB, 64 MiB offered | 413 `chunk_too_large`; the client may send smaller chunks |
| Idle time of a stream | 60 s without a byte (read or write) | The deadline moves with every read and write (`http.ResponseController`); the request ends, the offset stays |
| Duration of one chunk | 15 min | Then the request ends; the client resumes at the offset |
| `RequestTimeout` (30 s), `ReadTimeout` (30 s), `WriteTimeout` (60 s), `MaxBodyBytes` (1 MiB) | unchanged | Do **not** apply to the two streaming operations (F2); they apply to everything else, including the JSON requests of this feature |
| Duration of a transfer task | 6 h per table and 24 h per task (flag `transfer-max-duration`) | The `COPY` carries a `statement_timeout` of the time left; the task fails `transfer_timeout` |
| Record (one CSV row) | 16 MiB | The scanner (below) fails the import with the row number; the workbench's row limit is the same |
| `maxAtomicBytes` (flag) | 2 GiB (decompressed) | 14a: the only import is atomic, so a larger file is refused with 422 `import_too_large` and "split the file"; 14a+ (batches) lifts it |
| `maxAtomicReferenceBytes` (flag) | 256 MiB | A reference table, written on every node, if the reference import exists |
| `maxImportBytes` (flag) | 100 GiB | 14c: the sum of the sizes and rows of an archive, checked before step 4 and against the free space of the data nodes |
| Archive: entries | 20 000 (10 000 tables) | `archive_invalid` |
| Archive: manifest, catalog, each SQL file | 4 MiB, 16 MiB, 64 MiB | `archive_invalid` |
| Archive: statements, one statement | 200 000, 1 MiB | `archive_statement_refused` (14c) |
| Archive: expansion | declared sizes must equal the manifest and the bytes read, the sum must not exceed `maxImportBytes`. (A ratio per entry is **not** a limit: deflate cannot exceed about 1000 to 1 and the cap would stop nothing, DXR-15) | `archive_invalid` |
| Workbench-style concurrency | not shared | The transfers have their own limit; they do not use the 2 per role of the workbench |

## Table export [14a]

### Request

`POST /databases/{databaseName}/schemas/{schemaName}/tables/{tableName}/export` (`exportTable`, `kingres_admin`), body
`ExportTableRequest`, answer 202 `AcceptedTask` (kind `table.export`):

| Field | Default | Meaning |
|---|---|---|
| `columns` | all, in table order | A list of column names, validated against the catalog; generated columns are exported (they are values) and are skipped on import |
| `delimiter` | `,` | One of `,` `;` tab `\|` `:` |
| `nullString` | empty (an unquoted empty field) | At most 32 bytes, no quote, delimiter, CR or LF |
| `quote` | `"` | One byte; `escape` is the quote |
| `header` | `true` | First record has the column names |
| `encoding` | `UTF8` | One of `UTF8`, `LATIN1`, `LATIN2`, `LATIN9`, `WIN1250`, `WIN1251`, `WIN1252`, `WIN1256`, `ISO_8859_6`, `SJIS`, `EUC_JP`, `GBK`, `BIG5`, `KOI8R`; a value that PostgreSQL cannot convert fails the task with the row's column named. `UTF8` has no byte order mark; `utf8Bom` (default false) adds one for spreadsheets that need it |
| `compression` | `none` | `none` or `gzip` |
| `spreadsheetSafe` | `true` | See below |
| `retentionHours` | cluster setting | 1 to 168 |

The server never accepts a `WHERE`, an order, or SQL of any kind in the request. The statement is
`COPY (SELECT <quoted columns> FROM <quoted table>) TO STDOUT WITH (FORMAT csv, ...)`, built from names that passed
`sqlx.NewIdent`, and it is the same shape for every table kind (F9). A table that does not exist, a view, a foreign
table, a bucket partition named directly, and a table of schema `kingres`, `pg_catalog` or `information_schema` are
refused (404 or 422 `table_not_eligible` with `blockers`, as the table tasks do).

### The task `table.export`

Subject `database/schema.table`, so an export cannot run while the table is being distributed, altered or
undistributed, and two exports of one table are serialized (409 `operation_in_progress`). Steps:

1. *Check the request*: names, options, privileges (`has_table_privilege` for every column as the role), the
   transfer setting, quota and free space (reserves the per-group estimate, "Where files live"), the role again, and
   two checks that exist because the read runs **as the administrator** (DXR-11):
   - **Untrusted code.** `COPY (SELECT)` evaluates, as the administrator, expressions that the table's owner chose: a
     virtual generated column (PostgreSQL 18) and the expressions of a row-level-security policy. When the table is owned
     by another role, the request is refused with 422 `export_untrusted_code` if any such expression of the columns
     exported calls a function owned by a role that is neither the exporter nor a superuser (a catalog query on
     `pg_attrdef`, `pg_policy` and `pg_proc` with `pg_depend`, or the extension's predicate `function_owner_not_superuser` if it
     can be asked for a read, V9). `acknowledgeOtherRolesCode: true` in the request accepts the risk, recorded in the audit event.
   - **Row-level security.** A table with row-level security enabled is exported only if the role has `BYPASSRLS`
     or owns the table; the session sets `row_security = off`, so that **a policy that would hide rows makes the
     statement fail (as `pg_dump` does) and never silently shortens the file**; the report records `rlsApplied: false`.
2. *Read the table class*: from the catalog (`kingres.dist_table`, `colocation`, `bucket_map`, `dist_partition`)
   through the service role: class, group count that holds buckets, the `consistency` label (below).
3. *Write the file*: one `COPY (SELECT) TO STDOUT` on a connection of the role (F3), in a read-only `READ COMMITTED`
   transaction, `row_security = off`, session formats fixed (below), `FROM` the table (for an inheritance parent `FROM ONLY`;
   for a partition root the whole tree, "Partition trees"), bytes written through the sealing writer (gzip first when asked), the
   SHA-256 and the byte and row counts computed on the way. `itemsDone` is the estimated rows done, `itemsTotal` the
   estimate from the catalog (`detail` says the estimate is one); the step is `Interruptible`, so a cancel ends the
   `COPY` with the backend's own cancel and the partial file is removed. A `KS001` or a lost connection **before
   the file is sealed** restarts the step from the beginning (at most 3 times, 5 s apart; the file is rewritten, so
   the step is idempotent).
4. *Seal*: rename, set the row `ready` with size, SHA-256, rows, `expires_at` and the report.

The task ends `done`; the console reads the transfer from the task's `detail` (`transfer N`, the convention the
ledger bundle uses) and from `getDataExport`, because the contract's `Task` has no result member.

### What is consistent

One export is **one statement**, and the guarantee is exactly what the README of `kingres_shard` says for a
statement (F10), no more:

- Every row in the file was committed. Nothing uncommitted and nothing invented.
- For a **local** table, a **reference** table, and a distributed table of which **every bucket is owned by the
  group of the connection** (a cluster with one data group is the case), the file is one snapshot: the statement
  snapshot. The manifest says `single-snapshot`.
- For a distributed table with buckets in **more than one group**, each group's rows come from one snapshot of that
  group, taken when the scan opens the cursor on it, and the groups' snapshots are **not** one instant. A
  transaction that wrote to two groups can be in the file in one group's rows and not in the other's. How far apart
  the cursors are opened is up to the scan; the design does not promise it is short. The manifest says
  `per-group` and the number of groups.
- A user-partitioned table is one distributed table; the same rules apply to all its leaves (**[Needs
  verification] V4:** that a single scan node opens one cursor per group for the whole tree, so that all the
  partitions of a group are one snapshot; if not, the label says `per-group-and-partition`).
- A **bucket move** or a failover during the export makes the statement fail with `KS001` or a connection error
  (F11); the step restarts, so a file is never part of two attempts.
- **Writers are not stopped.** The export takes no lock that blocks them (a `READ COMMITTED` read).

What a user can do about skew: export in a quiet period, or **export twice and compare the SHA-256**: if the two
sums are equal, nothing changed during either run (a property of the data, and the console says it). A global
snapshot, which would make every label `single-snapshot`, is phase 8 and future roadmap.

### Spreadsheet safety

A cell that begins with `=`, `+`, `-`, `@`, a tab or a carriage return is a formula to a spreadsheet and can
exfiltrate data or run a command. `spreadsheetSafe` (default **on** for the console's table export) changes the
`SELECT` list, in SQL, so that the control plane never parses the CSV it exports. **It is a convenience for a person with a
spreadsheet, not a security feature** (DXR-26), and the dialog says so. Every column whose type is in the
string category (`text`, `varchar`, `bpchar`, `name`, `citext`, a domain over them) or an enum is written as

```
CASE WHEN col ~ '^[\x09\x0d\xa0 ]*[=+@|%-]|^[\x09\x0d]|^[\xff1d\xff0b\xff0d\xff20]' THEN '''' || col ELSE col END
```

so a risky cell is prefixed with an apostrophe, which spreadsheets show as text. The pattern covers `=`, `+`, `-`, `@`, `|` and `%`, the full-width forms of `=`, `+`, `-` and `@`, a tab or a carriage return, and a no-break space or spaces before a formula character. Numeric, date and boolean columns
are not changed (a negative number is a number). The change is **lossy**: the file no longer holds the original
value, and importing it back puts the apostrophe in. The dialog says that, and the report records
`spreadsheetSafe: true`; with it off the file is exact and the dialog warns that the file must not be opened in a
spreadsheet. The database archive never uses it. Fields that need it but are not text (a `json` that starts with
`-`) are out of reach of a pattern on the type and are listed under "Known limits".

### Session formats

Every connection of a transfer sets, in the transaction (`SET LOCAL`, after the connection check):
`DateStyle = 'ISO, YMD'`, `IntervalStyle = 'postgres'`, `extra_float_digits = 3` (shortest exact), `bytea_output =
'hex'`, `TimeZone = 'UTC'`, `lc_monetary = 'C'`, `client_encoding` as the file's encoding. They are the settings
the extension already fixes on remote sessions (README), plus `TimeZone` and `lc_monetary`, which it does not
(`TimeZone` is not forced on a remote session; a `money` column is converted under `lc_monetary`, README of
phase 9). The same set is used by the import, so a round trip does not depend on a role's or a database's settings.

## Table import [14a]

### Upload, then import

A CSV reaches the server as an **upload** (resumable, below) and is then used by an import. The two are separate
because a 2 GiB file may take an hour to send and the import may be repeated with other options without sending it
again.

`POST /databases/{databaseName}/schemas/{schemaName}/tables/{tableName}/import` (`importTable`, `kingres_admin`), body
`ImportTableRequest`, answer 202 `AcceptedTask` (kind `table.import`, subject `database/schema.table`):

| Field | Default | Meaning |
|---|---|---|
| `uploadId` | required | A complete upload of the caller |
| `header` | `skip` | `none` (no header), `skip` (a header, ignored), `match` (the header names must equal `columns`; see "Header") |
| `columns` | the table's **non-generated** columns in table order | The columns, in the file's order. A generated column is refused with `validation_failed` (`COPY` cannot set it). A file made by `exportTable` has the generated columns too, so for such a table its `columns` must be given, and the console does it |
| `delimiter`, `nullString`, `encoding` | as for export | `nullString` as in export |
| `quote` | `"` | One byte; with the client-only encodings (`SJIS`, `BIG5`, `GBK`) only an ASCII byte below 0x40, because a byte-oriented scanner cannot tell a trail byte from a quote there |
| `forceNotNull`, `forceNull` | none | Lists of columns |
| `skipBom` | `true` | A UTF-8 byte order mark at the **start of the decompressed stream** is removed (spreadsheets write one); a BOM in front of a gzip member is data |
| `compression` | detected from the upload's format | `none` or `gzip` (the expansion is counted, see "Hostile input") |
| `keepUpload` | `false` | Keep the upload after success (until retention) |

There is no `onError`, no `batchRows` and no `skipRows` in this phase: the import is all or nothing and nothing
else (decision 3). The contract's request schema is closed, so these members can be added by 14a+ without a change
of meaning.

Targets: distributed tables (partitioned or not), local tables (also partitioned and columnar), ledger tables (local;
the capture trigger runs; a table with the policy `required` is refused up front, 422 `import_ledger_policy`), and
**reference tables only if spike S2 passes** (section "Reference tables"). Refused: views, foreign tables, bucket
partitions, `kingres` and system schemas, a table with an unfinished table task (409), a distributed table with
row-level security (the router refuses; the message is mapped, not passed on), and a **local table with row-level
security** (PostgreSQL refuses `COPY FROM` for it; checked up front in the catalog so the answer is a code, not a
failed load).

### The pipeline, and why memory is bounded

```
staged file (sealed segments)
  -> segment reader (64 KiB)
  -> [gunzip with a count of bytes out]
  -> [BOM strip; the encoding is checked by PostgreSQL]
  -> record scanner (finds record ends, counts rows, rejects an unquoted "\." line; copies bytes through)
  -> bounded reader (1 MiB read-ahead) -> PgConn().CopyFrom(COPY ... FROM STDIN) inside the open transaction
```

PostgreSQL parses the CSV; the scanner only knows where a record ends (it tracks the quote state, so a newline
inside quotes does not end a record), counts records and bytes, and enforces the 16 MiB record limit. It holds
one record and the read-ahead is fixed, so **memory does not depend on the file's size**. The data goes in with
`COPY ... FROM STDIN` only: **no file name or program ever reaches PostgreSQL**, so nothing needs
`pg_read_server_files` or `pg_execute_server_program`. The statement is built from names that passed
`sqlx.NewIdent` and from option values that passed the checks above; the delimiter, quote and null string are
quoted as PostgreSQL literals by one function, with a differential test against PostgreSQL's own parser.

**Header.** `header: match` needs the names of the first record. The control plane reads **that one record** with
the scanner's quote rules (names only, never data) when the router does not accept `HEADER MATCH` (spike S2
decides which); where `COPY ... HEADER MATCH` is accepted, PostgreSQL does the check and the control plane keeps
only the code. This is the one place where the control plane looks inside a record, and it keeps nothing of it.

**The end marker.** A line that is only `\.` ends `COPY FROM STDIN` in some situations and PostgreSQL's behaviour
differs between majors and between the local path and the router (V6). The scanner **rejects a file that has an
unquoted `\.` line** (422 `import_end_marker`, with the line number), so the case never reaches the server. The
row-count comparison below stays as the precondition of the commit for every other disagreement between the
scanner and the server about what a record is.

### The load: an explicit transaction

```
BEGIN                                    -- on a connection of the role (F3), SET LOCAL session formats, statement_timeout
COPY t (cols) FROM STDIN WITH (...)      -- through PgConn().CopyFrom; the command tag gives N rows
compare N with the scanner's count       -- must be equal, else ROLLBACK and fail row_count_mismatch
write the commit intent                  -- durable, under the task lease (F25)
COMMIT
```

1. **The count is a precondition of the commit** (DXR-02). A mismatch, an error from `COPY`, a cancel, or a lost
   connection before the next point leaves **nothing stored**: the transaction ends with the connection or is
   rolled back. A disagreement that shortens the load is therefore never committed.
2. **The commit intent** (DXR-01 a). Before `COMMIT` is sent, the step records "commit sent for rows 1..N" in the
   task's step detail through the store, which checks the lease holder and epoch (F25). A leader that lost its lease
   cannot record the intent and so **cannot send the commit** (the write fails first); a zombie `COPY` of an old
   leader can never commit after the new leader decided the outcome.
3. **After the intent, the outcome is unknown until it is observed.** If the task is found later with the intent
   recorded and the step not done (a restart, a lost lease, a lost connection, a cancel that arrived while the
   commit was in flight), the task fails with `import_outcome_unknown`, `retryable: false`, and says that **N rows
   may have been loaded**. It never retries and never reports `rowsCommitted`. The console shows the sentence "Check
   the table before you repeat the import"; for a table with a unique key a repeat answers with the key's own error.
   Without the intent (the failure is before the commit was sent) the transaction was never committed, the task
   fails with a retryable code, and the user may repeat.
4. **Cancel** is accepted until the commit intent is written (`Task.cancellable` is true until then) and ends the
   `COPY` by the backend's cancel; the router ends the statement at once (README, "Waits"). A cancel that arrives
   later is answered 409 `task_not_cancellable`.
5. The load holds row locks until the end and writes dead tuples if it fails at the end: that is the price of all
   or nothing, and the reason for the size cap.

**The size cap.** `maxAtomicBytes` (flag, default **2 GiB** of decompressed stream; the staging limit
`maxFileBytes` is a different, larger bound). A bigger file is refused when the import is requested, 422
`import_too_large`, with the sentence "split the file into parts of at most 2 GiB and import them one after the
other". The console says the same before the upload starts. The cap is lifted only by 14a+ (batches).

### Progress, cancel, timeouts

Steps of `table.import`: *Check the upload* (complete, the caller's, size and SHA-256 equal the record), *Check the
table* (class, privileges, header, ledger policy, row-level security, table task running?), *Load rows*
(`NonIdempotent`; `Interruptible` until the intent; `itemsDone` = bytes of the file consumed by `COPY`, `itemsTotal`
= file size; **`pg_stat_progress_copy` is not updated by routed `COPY` (F7), so the count is the control plane's own**),
*Commit* (the intent and the commit; built as one step *Load rows*, then *Confirm the commit*), *Clean up* (delete the upload unless `keepUpload`). `statement_timeout` is the
time left of `transfer-max-duration`; a timeout fails the task `transfer_timeout` and rolls back. Between steps the
role is checked again (cluster role, `rolcanlogin`, `rolvaliduntil`, still the same role identity, DXR-23): a role that
lost its right ends the task `forbidden`, with nothing stored.

### Reference tables [14a, only if spike S2 passes V3]

`COPY ... FROM` into a reference table is refused on a cluster with several groups (F8). If spike S2 shows that
`INSERT INTO ref (cols) SELECT u.c1::type1, ... FROM unnest($1::text[], ...) AS u(c1, ...)` is accepted for a
reference target, the import **in the same transaction**: `COPY`s the file into a **temporary table** with the same
columns (so PostgreSQL still parses the CSV with the same options), reads it in chunks of 10 000 rows as text and
inserts each chunk with the form above, compares the counts, writes the intent and commits. It stays one atomic
transaction, written on every node, so its cap is smaller: **256 MiB** (`maxAtomicReferenceBytes`). The temporary
table holds the rows on the coordinator's disk for the duration (the free-space check counts it). If V3 fails, the
reference import is **not offered** in 14a: `importTable` answers 422 `table_not_eligible` with the blocker "reference
tables are not importable in this version", and the console hides the action for them. The export of a reference
table is not affected.

### Batches [14a+]: the design, not built

Batches exist to load files above `maxAtomicBytes` and to lose one batch, not the file, on an error. Without more,
they cannot be exactly once (DXR-01): a batch can commit while the task does not learn it (the process dies between
the commit and the record; a cancel races the commit; a leader that lost its lease still has a `COPY` in flight).
**The outcome is made decidable by the database itself, with a unique marker in the same distributed transaction.**

- *The marker table.* On the first batch import into a database, the service role creates the schema
  `kingres_transfer` and the **reference table** `kingres_transfer.batch (transfer_id bigint, batch_no int, rows
  bigint, committed_at timestamptz default now(), primary key (transfer_id, batch_no))`, and grants `INSERT` and
  `SELECT` on it to the importing role for the duration. A reference table is written on **every** node, so it can be
  read after a leader change from wherever the new leader is; a local table would live on one node only.
- *One batch.* `BEGIN; INSERT INTO kingres_transfer.batch VALUES (t, k, n); COPY batch k; compare the tag with
  the scanner's count; write the commit intent; COMMIT`. The marker and the rows commit **together or not at all**
  (two-phase commit of the router, README "Atomicity").
- *Deciding an unknown outcome.* One `SELECT` of the marker `(t, k)` tells the truth: present, the batch is
  committed and is skipped; absent, it is not and may be run. **A zombie cannot double a batch**: a second attempt
  at batch `k` waits on the unique index until the first one commits or rolls back, and then either fails with a
  unique violation (it was committed: skip) or goes on (it was not).
- *Continue.* "Continue from the first batch without a marker" is computed **from the markers**, not from the task's
  detail, so a stale record cannot cause a duplicate, and it is safe for a table without a key. It is offered for any
  failed batch import.
- *Cancel and failure.* The batch in flight rolls back (or its marker tells that it committed). The task reports the
  count of markers it finds, never a count it kept itself.
- *Cleanup.* The grants are revoked and the markers of the transfer are deleted when the import ends; `kingres_transfer`
  stays (empty) so the next one needs no DDL.
- *Costs.* One more write on every node per batch (a few bytes), a role that can write to the marker table (an
  importer can forge a marker, which only harms its own import), and a reference table in a database that did not
  have one. 14a+ also needs the marker for the all-or-nothing import of a **very** large file; the decision on that is
  for the review of 14a+.

Until it is built, the answer to a large file is to split it, and the API and the console offer no batch field.

## Uploads: resumable, with limits [14a]

| Operation | Method and path | Notes |
|---|---|---|
| `createDataUpload` | `POST /data-uploads` | Body `{purpose, sizeBytes, sha256?, fileName?}`: `purpose` is `tableImport` (14b adds `databaseArchive`); `fileName` is for display only and is never used in a path; `sha256` is **for command-line clients** (a browser cannot hash a large file incrementally, DXR-27). The size must be at most `maxFileBytes`; the quota is reserved. 201 with the upload: `uploadId`, `chunkBytesMax`, `chunkAlign` (65536), `offset` 0, `expiresAt` |
| `getDataUpload` | `GET /data-uploads/{uploadId}` | State, `offset`, `sizeBytes`, `sha256` once complete. The offset is **read from the file** (the number of plaintext bytes in whole sealed segments plus the held tail), never from the row, so it is right after a crash |
| `uploadDataChunk` | `PATCH /data-uploads/{uploadId}` | Header `Upload-Offset` (must equal the server's offset, else 409 `upload_offset_mismatch` and the header `Upload-Offset` has the right one), `Content-Type: application/octet-stream`, `Content-Length` of at most `chunkBytesMax` and a **multiple of 64 KiB unless it is the last chunk** (else 422 `validation_failed` on `Content-Length`). Answer 204 with the new `Upload-Offset` |
| `deleteDataUpload` | `DELETE /data-uploads/{uploadId}` | Frees the reservation; 409 while a task uses it |

**Segments and offsets** (DXR-24). The file is sealed in fixed 64 KiB segments. Because a non-final chunk is a
multiple of 64 KiB, **no partial segment is ever kept between requests**; a request that breaks off keeps the whole
segments that were sealed and drops the rest, and the offset (read from the file) says which. **Completion** (the last byte is in) re-reads the sealed file once to compute
the SHA-256: the incremental state cannot survive a restart, so it is not kept. A mismatch with a declared `sha256` is
422 `upload_checksum_mismatch` and the upload is deleted; a chunk beyond `sizeBytes` is 413 `chunk_too_large`.

**Large uploads.** The chunk size is the unit of loss: a break loses at most one chunk. The console sends 64 MiB
chunks strictly in order with one request in flight, so the offset check is the only ordering rule. A 2 GiB file is 32
requests, a 20 GiB file 320. The session of a long upload may expire: the console refreshes the session between
chunks and, when the server answers 401, stops, asks for the password again and resumes at the offset
(section "The console"). No upload uses the 16 MiB limit of `forward`, because it does not use `forward` (F1).

### The streaming relay

A follower must not buffer (F1). Two operations (`uploadDataChunk` and `downloadDataExport`) are marked
`x-streams: upload` and `x-streams: download` in the contract and are handled so:

1. **The guard.** The server that received the request runs the normal guard (session or token, CSRF for the
   cookie, role, rate limit, the TLS check, the transfer setting). These two operations **leave the guard's
   `context.WithTimeout(RequestTimeout)` and its `MaxBytesReader`** (`serveOp` applies both to every operation; the
   stream operations are registered with a flag that skips them), and use instead an idle deadline of 60 s moved
   with every read and every write by `http.NewResponseController` (`SetReadDeadline`, `SetWriteDeadline`), plus an
   overall limit of 15 min per chunk. **HTTP/2** handles these deadlines differently; spike S1 tests it, nothing is
   assumed.
2. **If this server is the leader** it runs the handler. Otherwise it opens `POST /internal/v1/forward-stream` to the
   leader over the mutual-TLS listener. The body is a **frame** and then the raw stream:

   | Direction | Frame | Content |
   |---|---|---|
   | request | 4-byte length (at most 64 KiB), then JSON | operation id, path parameters, **query**, the request headers the operation uses (`Range`, `Upload-Offset`, `Content-Length`, `Content-Type`), the **source address** (for the audit event), the identity and the **proof** (as `forward`), no body |
   | request | then | the raw body: the chunk of an upload (at most 256 MiB), nothing for a download |
   | response | 4-byte length, then JSON | status, and the response headers from the allowlist below |
   | response | then | the raw bytes, flushed as they come |

   **Response headers relayed:** the existing `relayedHeaders` (`Content-Type`, `Location`, `Retry-After`, `Allow`,
   `Content-Language`, `Preference-Applied`, `Content-Disposition`) plus `Content-Length`, `Content-Range`,
   `Accept-Ranges`, `ETag`, `Upload-Offset`, `Cache-Control` and `X-Content-Type-Options`. The leader checks the proof
   exactly as for `forward` (CP2-01) and runs the operation through the same pipeline, so audit and role checks are the
   leader's. The internal protocol version stays 1 (a new call only, as phase 13 did); `internal-api.md` gets the
   section and a test that the call is member-only and answered by the leader only.
3. **Routing.** `forwardsToLeader` and `leaderBoundReads` name `uploadDataChunk`, `createDataUpload`,
   `getDataUpload`, `deleteDataUpload`, `listDataExports`, `getDataExport`, `downloadDataExport` and
   `deleteDataExport`: all read the leader's system database or its disk. The ordinary JSON operations of this feature
   (`exportTable`, `importTable`) use the existing `forward`.
4. Nothing is buffered beyond one 64 KiB segment on either server.

**A stream that fails half way cannot change its status.** The download therefore always sends `Content-Length` of
the whole plaintext (so the browser sees a short file as an error), `ETag` and the SHA-256 in the export record and
in the dialog; the audit event of a download is written **when the stream ends** (by the guard's deferred function),
with whether the last byte was served, and `downloaded_at` is set only then. The dialog shows the size and the
SHA-256 so that a user can check the file they received.

**Spike S1** (first thing built, before the operations depend on the relay): a follower and a leader, a 5 GiB
upload in 64 MiB chunks and a 5 GiB download through the follower, memory of both servers flat, a kill of the follower, of
the leader and of the client in the middle of each, on HTTP/1.1 and HTTP/2, the idle deadline, the frame limits.
Its result is written into this note.

### Download

`GET /data-exports/{exportId}/content` (`downloadDataExport`): 200 or 206 (`Range: bytes=a-b` of the plaintext;
one range; the sealed segments make it a seek), `Content-Length`, `Accept-Ranges: bytes`, `ETag` (the SHA-256),
`Content-Type` `text/csv; charset=<encoding>` for a CSV, `application/zip` for an archive,
`Content-Disposition: attachment; filename="..."` with a name the **server builds** from a safe subset
(`[A-Za-z0-9._-]`, at most 80 characters, from database, table and the UTC time; never from a client string),
`Cache-Control: no-store`, `X-Content-Type-Options: nosniff`. The creator only (404 otherwise). A file that expired is
410 (the bundle's convention); `deleteDataExport` removes it earlier. `downloadDataExport` is audited. The cookie is
`SameSite=Strict`, so a page of another site cannot start a download in the user's name (F17).

## Database export [14b]

### What the archive does not carry

Said first, because an archive that looks complete and is not is the worst outcome (DXR-09). The archive holds the
user tables of the user schemas, their structure, the KingresDB catalog document and the extension list. It does
**not** hold, and the export says so in `warnings` and refuses where marked:

- **Large objects** (`pg_largeobject`) and columns that refer to them (`oid`, `lo`), and columns of the `reg*` types,
  whose numbers do not carry over. The export counts `pg_largeobject_metadata` and these columns first and **refuses**
  with 422 `large_objects_present` unless the request has `acknowledgeLargeObjectsLost: true`, and then writes the
  fact into the manifest's warnings.
- Roles, role memberships, tablespaces, the settings of roles and of the database (`ALTER DATABASE ... SET`), event
  triggers, rules, row-level security policies, publications, subscriptions, replication slots, foreign data wrappers
  and servers, extended statistics, collations, unlogged tables, security labels and the data of the configuration
  tables of extensions that are not in the project's list. A database that uses one of these is **refused** at export
  with 422 `schema_unrecognised` naming the object kind (the supported subset is fixed by spike S3 and written into
  this note when it is known); such a database is a job for `pg_dump` and `pg_restore`.
- The **placement** of buckets (the target places them), the **history** of the ledger and of the time-partition
  runs, the contents of `kingres` (the extension's own catalog), and the state of node-local sequences on nodes other
  than the coordinator.

### The task `database.export`

Subject: the database name. **While a `database.export` or `database.import` task is unfinished, a table task
(`table.distribute`, `.alter`, `.undistribute`, `.partition`, and `table.import` into that database) is refused with
409 `operation_in_progress`**, and the database tasks refuse to start while a table task of the database is
unfinished (the check is one query on the task table at the start of each of them). This stops the catalog, the
schema and the data from being taken at different structural states by the control plane's own operations (DXR-14);
DDL that a user runs in the workbench is caught by the fingerprint below.

Steps, each recorded `done` (a resumed task skips them, but see "Leader change") and idempotent:

1. *Check the request*: the setting, quota and space (reservation, below), the role (cluster role, login, validity),
   `SELECT` on **every** table and `USAGE` on every schema as the role (else 422 `export_not_readable`, first 20
   named), the large-object count, the **untrusted-code** and **row-level-security** checks of "Table export".
2. *Take the structure* in **one short `REPEATABLE READ READ ONLY` transaction of the service role**: the catalog
   document, the list of tables with their columns, the **fingerprint** (SHA-256 of the table list, the columns with
   types, the distribution class, the colocation relations) and the **schema dump**: `pg_dump` of the **same PostgreSQL
   build as the instance** (from the instance's binary directory, never from `PATH`), run by the **service role,
   read-only, `--schema-only`**, so that it needs no privilege on data and works for databases that use `kingres_ledger`
   and `kingres_timepart` (whose tables only a superuser or a member of `pg_read_all_data` can read, DXR-12). The
   arguments are fixed and **no client value is in any of them**: `--schema-only --no-owner --no-tablespaces
   --no-publications --no-subscriptions --no-security-labels --no-privileges --snapshot=<the transaction's
   snapshot>`, `--exclude-schema` for `kingres` and the system schemas, `--exclude-table` for the bucket partitions and
   the history tables of ledger tables (names from the catalog), `--section=pre-data` and `--section=post-data` in two
   runs. A clean environment (`PGPASSFILE`, `PGSERVICE` unset), no shell, a timeout, a pipe with a size cap (64 MiB
   per run). Because the dump is schema only, the service role reveals only definitions, which an administrator
   manages by design; the data is read as the requesting role in step 4.
3. *Normalise the schema* (below) into the three SQL files, **then run them through the importer's validator**
   (the shape grammar of 14c's parser, in its validate-only mode): an archive that the importer would refuse fails
   here, with `schema_unrecognised`, not after the user has uploaded it somewhere.
4. *Write the data*: for each table, **in one `REPEATABLE READ READ ONLY` transaction of the role when the database is
   in the case "every table is local, reference, or on the group of the connection"** (the archive is then one snapshot,
   label `archive-single-snapshot`; the cost is that a bucket move waits for the transaction, F11, bounded by the
   export), otherwise one `READ COMMITTED` statement per table (label per table, below). One `COPY (SELECT) TO STDOUT`
   per **partition root or standalone table** with the fixed options, into its own zip entry (see "Partition trees").
   A table is restarted on its own after `KS001` or a lost connection (3 times); with a single snapshot a restart
   restarts the whole data step, because the snapshot is gone. `itemsDone` counts tables, `detail` names the table
   and the bytes so far.
5. *Check*: the fingerprint is computed again; **a difference fails the task with `schema_changed`** and no file; the
   row count of each partition tree is compared with `count(*)` of its root taken in the same transaction (single
   snapshot) or at the end (otherwise, as a warning only, because rows move under writers).
6. *Write the manifest and seal*: sizes, SHA-256 of every entry, the columns of every data entry, versions, labels, the
   central directory; the SHA-256 of the whole file goes into the row and the audit event.

A cancel ends the running `COPY`; the task fails `cancelled` and no file is kept (`OnFail` deletes the partial file).
**Reservation** (DXR-16): the estimate is the **per-group sizes from the catalog** times a factor of the stored form
(columnar tables 10, others 1.5), capped by `maxFileBytes`; the reservation **grows as bytes are written** and the
writer fails (`export_too_large`, `transfer_quota_exceeded`) when the quota is full, so a bad estimate stops the
offender and not its neighbours; the `statfs` guard is the last line.

### Partition trees and inheritance

For a **partition tree** (a local partitioned table, a time-partitioned table of `kingres_timepart`, a user-partitioned
distributed table) **only the root is exported and loaded**: `SELECT cols FROM root` returns the rows of every leaf, and
the leaves are structure only. For a plain **inheritance parent** the statement is `FROM ONLY`, and each child is
exported on its own. The catalog document names, per table, `treeRole` (`standalone`, `root`, `leaf`, `inheritanceParent`,
`inheritanceChild`) and the manifest has no data entry for a `leaf`. A round trip of a `timepart` table, a partitioned,
an inheritance and a mixed distributed tree is in the tests; the check at step 5 is the independent count (DXR-08).

### The archive

A zip file with these entries and **no others** (the importer refuses an unknown name):

```
manifest.json            the manifest (written last, read first)
catalog.json             the KingresDB catalog document
schema/pre-data.sql      tables, sequences, types, domains, schemas (what the distribution needs first)
schema/views.sql         views and materialized views, and everything that depends on a table (created after distribution)
schema/post-data.sql     indexes, constraints, foreign keys, comments that need the tables
data/00001.csv ...       one entry per exported table (a root or a standalone table), numbered in the order of the catalog
```

Entries are deflate-compressed by `archive/zip` and written by a **streaming writer**, which produces the shape the
validator accepts **exactly** (data descriptors, flag bit 3, sizes zero in the local header; Zip64 when needed); the
validator accepts that shape and no other (DXR-15), and the round trip tests the writer against the validator. The
data entries are CSV in **UTF-8**, written with `FORMAT csv, HEADER true, NULL '\N', FORCE_QUOTE *, ENCODING 'UTF8'`
and the session formats of the table export, so that an empty string (`""`) and a null (`\N`, unquoted) differ and a
text value `\N` is `"\N"`, which the CSV reader of PostgreSQL does not take for the null marker. No value is changed.

**The manifest** (JSON, at most 4 MiB, strict decoding, unknown members refused):

```json
{
  "format": "kingres-export/1",
  "createdAt": "2026-11-22T09:30:00Z",
  "source": { "database": "shop", "kingresVersion": "...", "contractVersion": "0.20.0", "postgresMajor": 18, "groupCount": 3,
              "encoding": "UTF8", "lcCollate": "en_US.UTF-8", "lcCtype": "en_US.UTF-8", "localeProvider": "libc" },
  "extensions": [ { "name": "kingres_shard", "version": "0.7", "schema": "kingres" },
                  { "name": "citext", "version": "1.6", "schema": "public" } ],
  "catalogVersion": 1,
  "fingerprint": "...",
  "entries": [ { "name": "catalog.json", "size": 41233, "sha256": "..." },
               { "name": "data/00001.csv", "size": 90211333, "sha256": "...", "table": "public.orders",
                 "columns": ["id", "customer", "total"], "rows": 1200000, "consistency": "per-group", "groups": 3 } ],
  "totals": { "tables": 14, "rows": 5400000, "uncompressedBytes": 411000000 },
  "options": { "dataMode": "all" },
  "warnings": [ "table public.orders was read per group, not as one snapshot" ]
}
```

`format` is `kingres-export/<major>`. **Locale and extension schemas are in the manifest** (DXR-19): the import creates
the database with the source's encoding and locale (or refuses with `archive_incompatible` if the target cannot) and
installs each extension into its recorded schema. The `columns` of a data entry are checked against the schema at
validation. The checksums find damage and truncation; they do **not** prove who made the file (anyone who can change
the archive can recompute them). The export's result and its audit event carry the SHA-256 of the whole file and
`importDatabase` accepts `expectedSha256`, so an operator who got the value from a second channel can bind the import to it.

### The catalog document

`catalog.json` is what the schema SQL does not say: how the tables are distributed. It is read by the control plane,
with the service role, from the extension's catalog (`kingres.dist_table`, `colocation`, `bucket_map`,
`dist_partition`, the access method of each table, `kingres.ledger_table`, `kingres.timepart_policy`, `pg_sequences`)
and is **structured data**, validated on import, never SQL:

| Part | Content |
|---|---|
| `colocationGroups` | `[{ref, bucketCount, distColumnType, collation}]`. The numeric ids are **not** exported: they are not portable. A table names its group by `ref` |
| `tables` | per table: schema, name, `kind` (`local`, `hash`, `reference`, `schema` when phase 11 exists), `treeRole`, `distributionColumn`, `colocationRef`, `userPartitioning` (`method`, `key`, the user partitions with their bounds from `dist_partition`), `accessMethod` (`heap` or `columnar`) and the `columnar` options the extension reports, `ledger` (`mode`, `policy`) or null, estimated rows and size |
| `distributedSchemas` | present when `kingres.schemas` exists (extension 0.8): schema and its `colocation` position; absent otherwise. An importer that does not know `distributedSchemas` refuses an archive that has a non-empty one (`archive_incompatible`) |
| `timepartPolicies` | the columns of `kingres.timepart_policy` that `timepart_set_policy` takes (`partition_every`, `timezone`, `create_ahead`, `retention`, `retention_action`, `columnar_after`, `columnar_sort`, `columnar_options`, `run_every`); `enabled` is exported as the source had it and **imported as false** (F27); not `next_run_at`, not the history, not the OIDs |
| `sequences` | for each sequence owned by a column: schema, name, `ownedBy`, `lastValue`, `isCalled` of the coordinator's sequence (sequences are node-local, F18) |

**Placement is not exported.** Which group holds which bucket is a property of this cluster. The importer lets the
extension place the new colocation groups as it places any new group (round-robin over the groups that should hold
shards), and phase 10's rebalance moves buckets afterwards. The **bucket count and the colocation relation** are
exported, because tables that are colocated stay colocated and the bucket count decides how the table is cut. The
document has **no list of referenced roles**: the importer builds what it needs from the statements it sees (14c).

### Normalising the dump

F12/F22 and the review (DXR-06): a dump of a node is the dump of its partitions, and `pg_dump` writes statements that
are neither removals nor allowed as they are. The control plane therefore turns `pg_dump`'s output into the logical
schema with a normaliser that does **removals and a small, closed set of rewrites**, and fails the export on anything
else. **The exact list is the result of spike S3 on a three-group cluster with the `pg_dump` built from this tree on
PostgreSQL 18 and 19, run as the service role, for every table kind**, and it is written into this note before 14b is
built. The design expects at least:

- *Removals*: the bucket partitions `<table>_bNNNN` (exact names from the catalog, which are also `--exclude-table` of the
  dump) and their attach and owner statements; the capture triggers of ledger tables (names in `kingres.ledger_table`)
  and their history tables (ADR 0028); the `CREATE EXTENSION` statements (the importer installs the extensions of the
  manifest); the `SET`, `SELECT pg_catalog.set_config` and `\restrict` preamble (replaced by fixed settings).
- *Rewrites*: `PARTITION BY HASH (<dist column>)` is removed from a table the catalog says is `hash`, only if the column
  equals the catalog's; `ALTER TABLE ONLY parent ADD CONSTRAINT ...` on a distributed parent loses `ONLY` (the parent
  index would otherwise stay invalid over bucket partitions that do not exist yet); `ALTER INDEX parent_idx ATTACH
  PARTITION leaf_idx` for a leaf that is removed is removed; `SET default_table_access_method = columnar;` before a
  table is turned into `USING columnar` on that table's `CREATE TABLE` (and the `SET` is dropped).
- *Moved*: views, materialized views and what depends on them go to `schema/views.sql` (DXR-07), in dependency order.
- *Refused* (the subset statement): extended statistics, `CLUSTER ON`, per-column `SET STATISTICS`, unlogged tables,
  collations, policies, rules, event triggers, foreign objects, publications, `COMMENT ON EXTENSION|ROLE|DATABASE`.
  Storage parameters (`SET (autovacuum_...)`) are kept in `CREATE TABLE ... WITH`; `ALTER TABLE ... SET (...)` is refused.

The golden files of S3 are kept per major and are the unit test; the round trip is the proof.

## Archive validation [14b] and database import [14c]

### Validation (14b)

`POST /data-uploads/{uploadId}/validation` (`validateDatabaseArchive`, kind `archive.validate`, subject
`upload:<id>`) is a task that does steps 1 to 4 of the import below and stores the **archive report** in the
upload's row. It reads the archive end to end once (the SHA-256 of every entry, the cost of "validated before anything
runs") and **caches the result by the file's SHA-256 in the transfer row**: the import re-checks only the structure
and the manifest, and refuses a file whose SHA-256 differs from the cached one (DXR-15). It touches no database. In
14b it is the whole of "import": a user can inspect an archive, see what it would do, and load it by hand.

### Request (14c)

`POST /database-imports` (`importDatabase`, `kingres_admin`), body `ImportDatabaseRequest`, answer 202 `AcceptedTask`
(kind `database.import`, subject the new database's name):

| Field | Default | Meaning |
|---|---|---|
| `uploadId` | required | A complete upload of the caller, validated (or validated by the first step) |
| `database` | the manifest's name | The new database; it must not exist at the request (409 `already_exists`) and **must still not exist when the final name is taken** (step 13) |
| `expectedSha256` | none | Binds the import to a value the operator got elsewhere |
| `acknowledgeLedgerRestart` | `false` | **Required `true` when the archive has a ledger table** (ADR 0028) |
| `onForeignKeyViolation` | `fail` | `fail` (the import fails and the database is removed) or `keepNotValid` (the constraints stay `NOT VALID`, the database is kept and the violations are reported by constraint name and count) |
| `keepOnFailure` | `false` | Keep the private database if the import fails (to look at it) |

There is **no `privileges`, no `allowCode`, no `bucketCount`**: owners and grants are not restored (every object is
owned by the importing role, `--no-privileges --no-owner` at export), functions and triggers are refused always, and the
archive's bucket counts are kept. The request is a closed schema so a later phase can add them.

### The task `database.import`

Steps (a failed step ends the task; the `OnFail` hook drops the private database, below). Between steps the role is
checked again (DXR-23):

1. *Validate the archive*: the cached report if the SHA-256 matches, else the whole of "Hostile input"; no database
   is touched. The sum of the manifest's `rows` and sizes is checked against `maxImportBytes` (default 100 GiB, a flag) and
   against the free space of the data nodes (DXR-15).
2. *Check versions*: the compatibility rules below, including encoding and locale (DXR-19).
3. *Check the plan*: parse **every statement** of the three SQL files with the shape grammar and check the catalog
   document against the schema and the data entries against the manifest; the result is a plan (counts by kind). No
   statement is run, no database exists, and a refusal names the statement's kind, position and fingerprint, never its text.
4. *Create the database* under a **private name**, `kingres_import_<task id>` (a name only this task uses), with
   `database.create`'s code (service role) using the archive's encoding and locale, `kingres_shard` and, if listed,
   `kingres_ledger` (F16). **A database that exists is a failure of the step**, not a success (F21: `createDB` as it
   is returns success for an existing database, so the step calls the creation with a strict flag, new in 14c): the
   private name cannot belong to anyone else, so no comment marker is needed and a crash between create and the next
   step leaves a database whose name says whose it is; the task's input records `privateName` before the call.
5. *Install extensions*: the rest of the manifest's list into **their recorded schemas** (`kingres_columnar`,
   `kingres_timepart`, `kingres_sketch`, and the closed list of contrib extensions: `pgcrypto`, `uuid-ossp`, `citext`,
   `hstore`, `pg_trgm`, `btree_gin`, `btree_gist`, `ltree`, `unaccent`, `fuzzystrmatch`) with the service role, at the
   archive's version or newer; any other extension is a refusal at step 3, never an attempt.
6. *Run pre-data*: the statements, one by one, as the role (next section): tables, sequences, types, domains.
7. *Recreate the distribution* (service role, from the catalog document): create the colocation groups in order (the
   first table of a group creates it; the others colocate with it); `create_reference_table` for reference tables;
   `create_distributed_table` for hash tables (user-partitioned ones through the phase 9 call that accepts a partitioned
   table), all **while the tables are empty**, so every call is the cheap one. A call that the extension refuses ends the
   task with the extension's report mapped to codes (`table_not_eligible`, `blockers`).
8. *Create views*: `schema/views.sql`, as the role, **after** distribution (F24); materialized views `WITH NO DATA`.
9. *Load the data*: for each table (roots and standalone tables), the table import pipeline with fixed options (`header
   skip`, `nullString \N`, UTF-8) and **its own transaction with the count check and the commit intent**, one table at a
   time; a ledger table is loaded before its ledger is enabled; a materialized view is refreshed last. Every table of the
   archive is at most `maxAtomicBytes` after decompression, or the import is refused at step 3 (`import_too_large`
   names the table; 14a+ lifts it). `itemsDone` counts tables, `detail` names the table.
10. *Run post-data*: indexes, constraints and foreign keys, as the role. With `onForeignKeyViolation: fail` a
    constraint that fails (the archive was read at different instants, no global snapshot) fails the task with the
    constraint's name and the count; with `keepNotValid` the constraint is added `NOT VALID` and the violations are
    reported.
11. *Sequences*: for each sequence of the catalog, on **every node** (the service role per server, as the fan-out tasks
    do), `setval(seq, greatest(lastValue, max(column)))` of the global maximum of the owning column.
12. *Ledger and policies* (service role): `ledger_enable` for each ledger table with its mode (existing rows are entered,
    F14); then `ledger_verify()`; **then the source's ledger policy is set again** (a table that had `required` has it
    again: a copy that accepts unattested writes is a downgrade, DXR-18; the report says it); `timepart_set_policy` for
    each policy **with `enabled = false`** (F27), listed in the report with the call that enables it. A policy that the
    extension refuses is a warning with the reason.
13. *Verify and take the final name*: per table, the rows equal the manifest's; `ledger_verify()` returns no rows;
    then, after checking once more that the final name is free, the database is **renamed to its final name**. **V8**
    decides whether `ALTER DATABASE ... RENAME` works on every group and in the registry; **if it cannot be made to work,
    the design falls back to this**: step 4 creates the database under the **final** name with `CREATE DATABASE` that
    fails if it exists, writes the marker comment `kingres-import:<task id>` in the same service session before
    anything else, and the hook drops only a database whose marker equals its task id; a crash between the two statements
    leaves an unmarked database that the next run of the task finds because its input records the name, and that the hook
    drops only if it has no user object. The private-name design is preferred because it needs no such rule.

The task is `Cancellable` until step 11 starts; a cancel before that drops the private database as a failure does.

### What may run: the shape grammar (14c)

The first draft classified statements by their **leading keywords**. The review (DXR-04) showed that this lets whole
classes through, because one statement holds others or many actions (`CREATE SCHEMA s CREATE TABLE ... CREATE TRIGGER
...`, `CREATE TABLE t AS SELECT ...`, `ALTER TABLE t ADD CONSTRAINT ..., ENABLE ROW LEVEL SECURITY, OWNER TO y`,
`COMMENT ON ROLE`, a `GENERATED ... AS (pg_sleep(86400))` column). The rule is therefore different:

**A statement is accepted only if a parser of its exact kind consumes all of it.** There is one recursive-descent
parser per allowed kind, and each accepts **only the clauses `pg_dump` writes** for that kind:

| Kind | What the parser accepts | Refused by construction |
|---|---|---|
| `CREATE SCHEMA` | the name only | any schema element, `AUTHORIZATION` |
| `CREATE TABLE` | columns with type, `COLLATE` (an existing one), `DEFAULT`, `NOT NULL`, `GENERATED ... AS IDENTITY`, `GENERATED ALWAYS AS (expr) STORED`, table constraints, `PARTITION BY RANGE|LIST`, `PARTITION OF`, `USING heap|columnar`, `WITH (storage parameters)` | `AS`, `LIKE`, `INHERITS` (unless the catalog says `inheritanceChild`), `UNLOGGED`, `TEMP`, `TABLESPACE`, `OF type` |
| `CREATE SEQUENCE`, `ALTER SEQUENCE ... OWNED BY` | the forms `pg_dump` writes | |
| `CREATE TYPE ... AS ENUM`, composite, range; `CREATE DOMAIN` | the forms `pg_dump` writes | `CREATE TYPE` with functions (base types), `CREATE CAST`, `CREATE OPERATOR` |
| `CREATE INDEX` | not `CONCURRENTLY`, not `TABLESPACE` | |
| `ALTER TABLE` | **exactly one** subcommand, from: `ADD CONSTRAINT`, `ALTER COLUMN ... SET DEFAULT | SET NOT NULL | ADD GENERATED`, `ATTACH PARTITION`, `REPLICA IDENTITY` | a comma list, `ENABLE|DISABLE ROW LEVEL SECURITY`, `SET TABLESPACE`, `OWNER TO`, `SET SCHEMA`, `ENABLE|DISABLE TRIGGER` |
| `CREATE VIEW`, `CREATE MATERIALIZED VIEW ... WITH NO DATA` | the form `pg_dump` writes | |
| `COMMENT ON` | the object kinds table, column, index, constraint, sequence, type, domain, schema, view | `ROLE`, `DATABASE`, `EXTENSION`, anything else |
| everything else | **nothing**: `COPY`, `DO`, `SET`, `RESET`, `SELECT`, `INSERT`/`UPDATE`/`DELETE`, `GRANT`, `REVOKE`, `ALTER DEFAULT PRIVILEGES`, `REASSIGN OWNED`, `DROP OWNED`, `CREATE FUNCTION|PROCEDURE|TRIGGER|POLICY|RULE|EVENT TRIGGER|COLLATION|EXTENSION|PUBLICATION|SUBSCRIPTION|SERVER|FOREIGN ...|TABLESPACE|ROLE|DATABASE`, `ALTER SYSTEM`, `LOAD`, `CALL`, transaction control, a backslash command other than `\restrict` and `\unrestrict` | |

**Expressions** (a default, a check, a generated column, an index expression or predicate, a view body) cannot be
parsed completely, and the design does not pretend to. They are accepted **only after the catalog document and the
statements agree on what they call**: the plan collects every function name that an expression calls (the parser
extracts identifiers followed by `(`), and the plan checks each against `pg_proc` of the target **after** the objects
exist (the check runs in the private database, in the same transaction as the creation of the table, and the
transaction is rolled back if any function is not built in (`pg_catalog`) or from an extension of the manifest). A
`pg_sleep`, `pg_read_file` or `dblink` in an expression is therefore refused, the first as not allowlisted and the
others by the role's privileges too.

**What the grammar does and does not give.** The guarantee that the archive's SQL **cannot do more than the importing
role could do by hand** rests on **PostgreSQL's privileges alone**, with the dangerous-role refusal (below) as the
condition. The grammar does not create that guarantee; it **narrows what is attempted**, bounds the **resources** it can
use (statement count, size, run time), and makes the plan checkable before a database exists. A statement that the
grammar misses is still bound by the role's privileges. This is the claim the first draft made too strongly.

**Execution.** Statements run **one per round trip, as the signed-in role**, on a connection from `Roles.Connect` to the
**private** database (never the service connection), after `refuseDangerousRole`. The importer sets
`statement_timeout` (10 min), `lock_timeout`, `idle_in_transaction_session_timeout` and `search_path = pg_catalog` as
`SET LOCAL` itself; a `SET` in the archive is refused, not honoured. The extended protocol refuses text that holds several
statements (`ops/query.go`), the second line behind the parser. Pre-data runs in one transaction per **table**. The
statements that the role runs can create **generated columns** whose expressions evaluate on every row of the later
load; the function check above and the load's `statement_timeout` bound that. **Nothing from the archive is run as the
service role**: the distribution, the ledger and the policies are made from the structured document, with identifiers
validated by `sqlx.NewIdent` and quoted by the same package, and numbers range-checked (bucket count 1 to 4096).

**The importing role.** `importDatabase` applies the rule of the workbench (F3): a role with `SUPERUSER`, `REPLICATION` or
`BYPASSRLS`, or with access to server files, programs or the log through a predefined role, is refused (403 `forbidden`).
The role has `CREATEROLE` (F22): the grammar has no `GRANT`, no role statement and no `COMMENT ON ROLE`, so the
statements cannot use it; the dangerous-role check is still made at every step.

### Versions and refusing an incompatible archive

| Check | Rule | Code |
|---|---|---|
| Format | the archive's `format` major must be one the importer lists (now: 1) | `archive_incompatible` |
| PostgreSQL | the target's major must be **at least** the source's (a dump of 18 loads into 19; a dump of 19 never into 18) | `archive_incompatible` |
| Extensions | each extension of the manifest must be installable on the target at a version at least the archive's, in the recorded schema, and the importer must know the archive's `catalogVersion` | `archive_incompatible` |
| Encoding and locale | the target must be able to create a database with the source's encoding, `lcCollate`, `lcCtype` and provider | `archive_incompatible` |
| Contract | informational only (shown, never refused) | none |
| Dialect | `distributedSchemas` non-empty needs an importer that supports phase 11; an unknown **required** feature in the manifest (`requires: [...]`) is a refusal | `archive_incompatible` |

All reasons are listed in one answer (`reasons: [...]`), as `table_not_eligible` lists blockers.

### Failure: the private database is removed

`database.import` has an `OnFail` hook that **drops the private database** `kingres_import_<task id>` unless
`keepOnFailure`; it drops **only that name**, so a task cannot drop a database it did not create. The hook is the
compensation `database.create` has for a partial create, and it runs on whichever server is the leader when the task
fails. A database kept with `keepOnFailure` stays marked failed until dropped. After a successful rename the hook has
nothing to drop.

## Hostile input

### Archives [14b validates, 14c imports]

Done by `archive.Validate` (the validate task and the import's first step; before a database exists), by `archive/zip`
over the `io.ReaderAt` of the staged file, **without ever writing an entry to a file system**: entries are streamed into
`PgConn().CopyFrom`, parsed in memory (the manifest, the catalog) or lexed from a stream (the SQL). No name of the
archive is ever used as a path, so path traversal has nothing to act on, and it is still checked:

1. **The file as a whole and its entries' shape** (DXR-15). The end-of-central-directory record is found at the end and
   the comment is at most 64 KiB. For **every entry the local file header is read and compared with the central
   directory**: the local name equals the central name, the local `extra` field is empty (Zip64 sizes are in the central
   directory and the descriptor), the flags and the method are equal, and **a data descriptor is present exactly when flag
   bit 3 says so**. The entry's **extent is the local header, the data and the descriptor**. The extents do not overlap,
   are inside the file, and with the central directory **account for every byte** of the file (no hidden bytes before,
   between or after, which stops polyglots and an archive inside an archive). The streaming writer of the exporter writes
   **exactly** the shape the validator accepts (descriptors, sizes zero in the local header, Zip64 when needed) and
   nothing else is accepted; the round trip tests the writer against the validator. A multi-disk archive is refused.
2. **Names**: exactly `manifest.json`, `catalog.json`, `schema/pre-data.sql`, `schema/views.sql`, `schema/post-data.sql`
   and `data/NNNNN.csv` (five digits, contiguous from 00001); no other name, no directory entry, no duplicate, no
   backslash, NUL, `..`, leading slash or drive letter, at most 64 bytes. The mode bits must say a regular file (no
   symbolic link). The method is Store or Deflate; anything else is refused.
3. **Sizes**: the manifest is read first and is at most 4 MiB; every entry's declared compressed and uncompressed
   sizes must equal the manifest's; **the sum must not exceed `maxImportBytes`** and the free space of the data nodes
   (the real bound: **a ratio per entry is not a limit**, because deflate cannot exceed about 1000 to 1, the cap would stop
   nothing and would refuse legitimate repetitive CSV). The **expansion is counted while reading** with a limit reader at
   the declared size plus one byte, so a header that lies is caught by the data.
4. **Checksums**: every entry is read once and its SHA-256 compared with the manifest (this is the one pass over the
   whole archive; its result is **cached by the file's SHA-256** in the transfer row, so the import re-checks only the
   structure and the manifest); the CRC-32 of the zip is checked too. The manifest's coherence is checked: the table list
   of the catalog document equals the data entries (`table` members), the `columns` of each data entry equal the
   columns of the schema, the counts add up to `totals`.
5. **JSON** (manifest and catalog): size caps (4 MiB, 16 MiB), `DisallowUnknownFields`, a nesting depth of at most 32
   and at most 100 000 members per array, strings of at most 4 KiB; **every identifier** is checked by the rules of
   `sqlx` before it is used, and used only quoted; numbers are range-checked; `format`, `catalogVersion` and
   `requires` are checked before anything else is read.
6. **SQL** (14c): by the lexer and the shape grammar, as part of step 3 of the import, not run.
7. **CSV**: a data entry is bytes for `COPY FROM STDIN`, never SQL (there is no `\copy`, no psql in the pipeline), and
   its records are bounded by the scanner (16 MiB).

The archive is **also not trusted for who made it** (the checksums are not signatures); that is why the audit
event has the file's SHA-256 and why `expectedSha256` exists.

### CSV files [14a]

A CSV is only data for `COPY FROM STDIN` with options that the control plane built from checked values, so the
risks are: a record that is enormous (the scanner's 16 MiB), a gzip bomb (the decompressed stream is counted against
`maxAtomicBytes`, so 2 GiB of output ends it), an encoding that is not what the user said (PostgreSQL's conversion errors
fail the load; the message is mapped by `SafeError`, "Error text"), an unquoted `\.` line (refused by the scanner), a
header that is not the table's (`match`), and values that violate constraints (PostgreSQL's own errors, mapped: **no
value, row, key or message of the server reaches a task, a problem, the audit log or the log**). CSV **injection into a
spreadsheet** is the export's problem, handled above; an imported value that starts with `=` is data like any other and
is stored as it is.

### What a hostile dump can and cannot do [14c]

It can ask for: a statement the grammar refuses (the import ends at step 3, nothing created); an expression that calls a
function that is not built in (refused by the function check); resource use (bounded by the caps, the statement
timeout, the size limits and the concurrency limit); a table that the extension refuses to distribute (the extension's
report); a name that collides with something (the private database name is the task's own; schema names of `kingres`,
`pg_*` and `information_schema` are refused). It cannot: run as the service role or as a superuser (the role is not
dangerous, the service role runs structured calls only), write to the server's file system or run a program (no
`COPY` of a file or a program is in the grammar, and a role with the predefined roles for that is refused), change a
role (there is no `GRANT`, `ALTER ROLE` or `COMMENT ON ROLE` in the grammar, although the role has `CREATEROLE`, F22), a
setting or another database, load a library, or change anything outside the private database (statements are on a
connection to it, names are schema-qualified by `pg_dump`, `search_path` is `pg_catalog`, and a statement that names
another database is a syntax error: PostgreSQL has no cross-database statement). **The bound is PostgreSQL's privileges**
(section "What may run"); what it can still do within them is create objects, with names and bodies that are bounded
text, owned by the importing role, in a database that the failure of any step removes. Functions and triggers are
refused always (there is no `allowCode`, DXR-22): code that runs later with someone's privileges, and that the
extension would refuse on any table it distributes anyway (F24), is not carried by this feature.

## Ledger tables

[ADR 0028](../adr/0028-ledger-tables-are-carried-as-data-and-restart-their-chain.md) records the decision and the
alternatives. In summary:

- **Table CSV export** [14a] of a ledger table is an ordinary export of its current rows. The history table of an
  updatable ledger table is a table of its own and can be exported on its own (the role needs `SELECT`).
- **Table CSV import** [14a] into a ledger table is allowed (a local table): the capture trigger enters every row as for any
  insert (F14), and a table with the policy `required` is refused up front because it would fail on the first row
  (`KL010`). The ledger's own tables (`kingres.ledger_*`) are in schema `kingres` and are refused as targets.
- **Database export** [14b] carries a ledger table as data plus its ledger `mode` and `policy`; its capture triggers and
  its history table are **not** in the schema section (normaliser removal), and the manifest lists it with
  `ledger: {mode, policy, headHeight, headBlockHash}` read **without writing** (no `ledger_generate_digest`, which
  would add a row to the source; the head is read from the extension's block table as the role). The history rows of an updatable
  table are **exported for the record** as a data entry `ledgerHistory: true` and are **not loaded** (the history
  table is created empty by `ledger_enable`, and the guard refuses writes to it by every role).
- **Database import** [14c] loads the rows **first**, then calls `ledger_enable(table, mode)`, which enters all existing rows
  as the first entries of a **new chain**, then `ledger_verify()`, **then sets the source's ledger policy again** (DXR-18:
  the reason to leave the policy off during the load, that the load would fail on its first row, is a reason to set it
  after the load, not to drop it; a copy that silently accepts what the source refused would be a downgrade of a
  protection in the one feature whose point is protection; the report says which tables had `required`). Without
  `acknowledgeLedgerRestart` the import is refused at step 3 (422 `ledger_restart_not_acknowledged`) with the list of
  ledger tables and the sentence below.
- **What the verifier says.** On the new database `ledger_verify()` returns **no rows**: the chain it holds is
  consistent. It says nothing about the source's chain, and cannot: the new database has a new chain identity (README
  of `kingres_ledger`, "Copies of the database"; a digest of the source does not verify against it). The old proofs and
  the digests published for the source stay valid for the source database, which is unchanged by the export. The
  manifest's `headHeight` and `headBlockHash` let an auditor compare the archive with a digest the source published, and
  the import's report and the console say: "The imported ledger starts a new chain at import time. It does not
  carry the history that proves the data was not changed before; keep the digests of the source database."
- **Time-partition policies** [14c] come back **disabled** (DXR-18, F27): a retention policy drops old partitions at the next
  run, and one taken from a file must not delete imported data on a schedule the user did not choose. The report lists
  them with the call that enables each.
- **A plain `pg_dump` of the same database** (which the design does not use) would leave the capture triggers of
  unregistered tables in the new database, and the verifier would name each of them; the normaliser removing them is
  what keeps the verifier quiet and honest.

## Leader change and failover

Files, keys and role connections are the leader's (F3, F6, F4). Rules:

1. **Every transfer step starts by checking its inputs**: the row's `server_id` is this server, the file exists, its
   size equals the row's, the role is still the role of the record and connectable here (DXR-23). If not, the
   task fails with `transfer_leader_changed` (`retryable: true`, message: "repeat the operation"), the row is set
   `lost` and the janitor of the server that holds the file deletes it when that server runs again (under its rules).
   The new leader does not adopt files of others.
2. **A leader that loses its lease mid-step** stops (`Interruptible` steps are cancelled by the lease loss, F4) and its
   `COPY` is cancelled with the context. Because the **commit intent is recorded under the lease before `COMMIT` is sent**
   (F25), a leader that lost its lease **cannot commit**: the write of the intent fails first, so a zombie `COPY` of
   an old leader never commits after the new leader decided the outcome, and a load that was cut before the intent
   stored nothing.
3. **After the intent, the outcome is unknown** until observed (a restart, a lost lease, a lost connection, a cancel
   during the commit): the task fails `import_outcome_unknown`, never retries, and says how many rows may be in.
   (With batches, 14a+, the marker makes the outcome decidable; "Batches".)
4. **A database import** [14c] that fails for any reason, including this one, runs `OnFail` on whichever server is the
   leader then, which drops the private database `kingres_import_<task id>` (the hook needs no file, only the name and
   the service role).
5. **Failover of a shard group during a `COPY`**: the router fails the statement with `KS001` or a connection error
   (README: "a group that changed its primary in the middle of it fails it with `KS001`, nothing stored"). An export
   restarts the step (3 attempts); an import fails with nothing stored and the code `copy_interrupted` (retryable)
   if the commit intent was not written.
6. **The leader restarts** (same disk): the staged files are still there, the role map is empty (F3), so a running
   transfer fails at the next step with `transfer_session_lost`; an upload stays and can be used by a new request,
   which makes the role connectable again.
7. **Uploads** are cheap to lose: `getDataUpload` answers 404 for a `lost` upload and the console starts a new one.

## REST contract, revisions 0.19.0 (14a), 0.20.0 (14b) and 0.21.0 (14c)

All additions are optional properties, new operations, new enum values and new problem codes (the contract's rule:
clients tolerate codes they do not know). Operations go from 140 to **150** (14a), **152** (14b) and **153** (14c). The
numbers are placeholders. Each revision is **its own commit, written before the code that uses it**, so a second
writer can start from it (section "Increments", A0, B0, C0).

### New tag and operations

Tag `Data transfer`. `...` in a path is `/databases/{databaseName}/schemas/{schemaName}/tables/{tableName}`.

| operationId | Method and path | Role | Body / query | Response | Rev |
|---|---|---|---|---|---|
| `exportTable` | `POST .../export` | `kingres_admin` | `ExportTableRequest` | 202 `AcceptedTask`; 403 `data_transfer_disabled`, `insecure_transport`; 404; 409 `operation_in_progress`, `transfer_quota_exceeded`; 422 `table_not_eligible`, `export_untrusted_code`, `validation_failed`, `insufficient_disk` | 0.19.0 |
| `importTable` | `POST .../import` | `kingres_admin` | `ImportTableRequest` | 202 `AcceptedTask`; 403; 404; 409 `operation_in_progress`, `upload_incomplete`; 422 `import_header_mismatch`, `import_ledger_policy`, `import_too_large`, `import_end_marker`, `table_not_eligible`, `validation_failed` | 0.19.0 |
| `createDataUpload` | `POST /data-uploads` | `kingres_admin` | `CreateDataUploadRequest` | 201 `DataUpload`; 403; 409 `transfer_quota_exceeded`; 422 | 0.19.0 |
| `getDataUpload` | `GET /data-uploads/{uploadId}` | `kingres_admin` | none | 200 `DataUpload` (with `report` after a validation); 404 | 0.19.0 |
| `uploadDataChunk` | `PATCH /data-uploads/{uploadId}` | `kingres_admin` | binary body, header `Upload-Offset` | 204 with `Upload-Offset`; 404; 409 `upload_offset_mismatch`; 413 `chunk_too_large`; 422 `upload_checksum_mismatch`, `validation_failed` | 0.19.0 |
| `deleteDataUpload` | `DELETE /data-uploads/{uploadId}` | `kingres_admin` | none | 204; 404; 409 `operation_in_progress` | 0.19.0 |
| `listDataExports` | `GET /data-exports` | `kingres_admin` | query `limit`, `cursor` | 200 `DataExportPage` (the caller's files only) | 0.19.0 |
| `getDataExport` | `GET /data-exports/{exportId}` | `kingres_admin` | none | 200 `DataExport`; 404 | 0.19.0 |
| `downloadDataExport` | `GET /data-exports/{exportId}/content` | `kingres_admin` | header `Range` | 200/206 file; 404; 410 expired; 416 | 0.19.0 |
| `deleteDataExport` | `DELETE /data-exports/{exportId}` | `kingres_admin` | none | 204; 404 | 0.19.0 |
| `exportDatabase` | `POST /databases/{databaseName}/export` | `kingres_admin` | `ExportDatabaseRequest` (`excludeTables`, `dataMode: all\|none`, `acknowledgeLargeObjectsLost`, `retentionHours`) | 202; 403; 404; 409; 422 `export_not_readable`, `schema_unrecognised`, `large_objects_present`, `insufficient_disk` | 0.20.0 |
| `validateDatabaseArchive` | `POST /data-uploads/{uploadId}/validation` | `kingres_admin` | none | 202 (kind `archive.validate`); 404; 409 `upload_incomplete`; 422 | 0.20.0 |
| `importDatabase` | `POST /database-imports` | `kingres_admin` | `ImportDatabaseRequest` | 202; 403 `forbidden` (dangerous role), `data_transfer_disabled`; 409 `already_exists`, `upload_incomplete`; 422 `archive_invalid`, `archive_incompatible`, `archive_statement_refused`, `import_too_large`, `ledger_restart_not_acknowledged` | 0.21.0 |

(`...` is the table path given above.) All list the shared responses
401, 403, 405, 422 and 503, and 400 where a body or query exists, as the checker requires. `uploadDataChunk` and
`downloadDataExport` carry `x-streams: upload` and `x-streams: download`; the checker is extended to know the extension.
Every request and response has an **example** in the contract, which the console's mock and the server's contract tests
both use. `createDataUpload`'s `purpose` is `tableImport` in 0.19.0 and gains `databaseArchive` in 0.20.0.

### Schemas (central types)

```yaml
ExportTableRequest:
  properties:
    columns:         { type: array, items: { type: string }, maxItems: 1600 }
    delimiter:       { type: string, enum: [",", ";", "\t", "|", ":"], default: "," }
    nullString:      { type: string, maxLength: 32, default: "" }
    quote:           { type: string, minLength: 1, maxLength: 1, default: "\"" }
    header:          { type: boolean, default: true }
    encoding:        { type: string, enum: [UTF8, LATIN1, LATIN2, LATIN9, WIN1250, WIN1251, WIN1252, WIN1256, ISO_8859_6, SJIS, EUC_JP, GBK, BIG5, KOI8R], default: UTF8 }
    utf8Bom:         { type: boolean, default: false }
    compression:     { type: string, enum: [none, gzip], default: none }
    spreadsheetSafe: { type: boolean, default: true }
    acknowledgeOtherRolesCode: { type: boolean, default: false }
    retentionHours:  { type: integer, minimum: 1, maximum: 168 }
ImportTableRequest:            # closed schema: batch members can be added by 14a+
  required: [uploadId]
  properties:
    uploadId: { type: string }
    header:   { type: string, enum: [none, skip, match], default: skip }
    columns:  { type: array, items: { type: string } }
    delimiter, quote, nullString, encoding, compression: as in ExportTableRequest (compression: none|gzip, default detected)
    forceNotNull: { type: array, items: { type: string } }
    forceNull:    { type: array, items: { type: string } }
    skipBom:      { type: boolean, default: true }
    keepUpload:   { type: boolean, default: false }
CreateDataUploadRequest:
  required: [purpose, sizeBytes]
  properties: { purpose: { enum: [tableImport, databaseArchive] }, sizeBytes: int64, sha256: string, fileName: string }
DataUpload:
  required: [uploadId, state, sizeBytes, offset, chunkBytesMax, chunkAlign, expiresAt]
  properties:
    uploadId: { type: string }
    state: { type: string, enum: [receiving, complete, validating, valid, invalid, lost, deleted] }
    sizeBytes: { type: integer, format: int64 }
    offset: { type: integer, format: int64 }
    chunkBytesMax: { type: integer, format: int64 }
    chunkAlign: { type: integer, enum: [65536] }
    sha256: { type: [string, "null"] }
    expiresAt: { type: string, format: date-time }
    report: { oneOf: [ { $ref: ArchiveReport }, { type: "null" } ] }      # 0.20.0
DataExport:
  required: [exportId, kind, state, createdAt, expiresAt]
  properties:
    exportId: { type: string }
    kind: { type: string, enum: [tableExport, databaseExport] }
    state: { type: string, enum: [building, ready, failed, expired, lost, deleted] }
    database/schema/table: nullable strings
    sizeBytes: { type: [integer, "null"], format: int64 }
    sha256: { type: [string, "null"] }
    rows: { type: [integer, "null"], format: int64 }
    consistency: { type: [array, "null"], items: { $ref: TableConsistency } }
    rlsApplied: { type: boolean }
    format: { type: string, enum: [csv, csv.gz, archive/1] }
    taskId: { type: integer, format: int64 }
    downloadedAt: { type: [string, "null"], format: date-time }
TableConsistency: { required: [table, level, groups], table: string, level: { enum: [single-snapshot, per-group, per-group-and-partition, archive-single-snapshot] }, groups: integer }
ExportDatabaseRequest:         # 0.20.0
  properties: { excludeTables: [TableRef], dataMode: { enum: [all, none], default: all }, acknowledgeLargeObjectsLost: boolean, retentionHours: integer }
ImportDatabaseRequest:         # 0.21.0, closed schema
  required: [uploadId]
  properties: { uploadId: string, database: string, expectedSha256: string, acknowledgeLedgerRestart: boolean,
                onForeignKeyViolation: { enum: [fail, keepNotValid], default: fail }, keepOnFailure: boolean }
ArchiveReport:                 # 0.20.0; also the answer of a validation
  required: [valid, format, versions, tables, reasons]
  properties:
    valid: boolean
    format: string
    versions: { postgresMajor, kingresVersion, extensions: [{name, version, schema, installed: version|null}], encoding, lcCollate, lcCtype }
    tables: { local, hash, reference, ledger, columnar: integer, rows: int64, bytes: int64 }
    statements: { byKind: { additionalProperties: integer }, refused: [ { kind, position, fingerprint, reason } ] }   # filled from 0.21.0
    needs: { ledgerRestart: boolean, largeObjectsLost: boolean }
    warnings: { type: array, items: string }
    reasons: { type: array, items: { $ref: TableBlocker } }     # the shape of table_not_eligible's blockers
Problem (additive, optional): reasons: array of TableBlocker   # present for archive_incompatible and archive_statement_refused
ClusterSettings (additive): dataTransfer: { enabled, retentionHours, maxFileBytes, stagingQuotaBytes, maxConcurrent }
TaskKind (additive): table.export, table.import  (0.19.0); database.export, archive.validate (0.20.0); database.import (0.21.0)
AuditAction (additive): the new operation ids, dataTransfer.completed, dataTransfer.purged
```

### Problem codes

| Status | Code | When | Rev |
|---|---|---|---|
| 403 | `data_transfer_disabled` | the cluster setting is off | 0.19.0 |
| 403 | `insecure_transport` | the request is not over TLS | 0.19.0 |
| 409 | `transfer_quota_exceeded` | the staging quota or the concurrency limit; `Retry-After` for the second | 0.19.0 |
| 409 | `upload_incomplete` | an import names an upload that has not received all its bytes | 0.19.0 |
| 409 | `upload_offset_mismatch` | `Upload-Offset` is not the server's; the header says the right one | 0.19.0 |
| 413 | `chunk_too_large` | a chunk beyond `chunkBytesMax`, or beyond the declared size | 0.19.0 |
| 422 | `upload_checksum_mismatch` | the SHA-256 of the received file is not the declared one | 0.19.0 |
| 422 | `import_header_mismatch`, `import_ledger_policy` | `header: match` failed (first difference named); the table has policy `required` | 0.19.0 |
| 422 | `import_too_large` | a file above `maxAtomicBytes` (2 GiB decompressed), "split the file" | 0.19.0 |
| 422 | `import_end_marker` | the CSV has an unquoted `\.` line; the line number is given | 0.19.0 |
| 422 | `export_untrusted_code` | a read would run code of another role (DXR-11); `acknowledgeOtherRolesCode` accepts it | 0.19.0 |
| 422 | `export_not_readable` | the role cannot read every table (first 20 named) | 0.20.0 |
| 422 | `large_objects_present` | the database has large objects or `oid` columns (DXR-09) | 0.20.0 |
| 422 | `schema_unrecognised` | the normaliser met a construct it does not support | 0.20.0 |
| 422 | `archive_invalid` | structure, names, sizes, checksums, JSON, limits | 0.20.0 |
| 422 | `archive_incompatible` | versions, locale or required features; `reasons` lists all | 0.20.0 |
| 422 | `archive_statement_refused` | the grammar refused a statement; `reasons` names kind, position and fingerprint | 0.21.0 |
| 422 | `ledger_restart_not_acknowledged` | the archive has ledger tables and `acknowledgeLedgerRestart` is false | 0.21.0 |
| 410 | (code `not_found`, status 410, as the bundle does) | an expired file | 0.19.0 |

Codes of a **failed task** (`TaskError.code`, not HTTP): 14a: `export_too_large`, `insufficient_disk` (existing),
`transfer_quota_exceeded`, `transfer_timeout`, `transfer_leader_changed` (retryable), `transfer_session_lost`
(retryable), `transfer_key_lost`, `copy_interrupted` (retryable), `row_count_mismatch`, `import_outcome_unknown` (not
retryable), `forbidden` (the role lost its right between steps), `cancelled` and `verification_failed` (existing). 14b:
`schema_changed`. 14c: `foreign_key_violation` (with the constraint's name and count), and the extension's blockers as
`table_not_eligible`. **No code carries server text** ("Error text").

README of the contract: the task-kind table gets `table.export`, `table.import` (subject `table`, `database/schema.table`),
`database.export`, `database.import` (subject `database`, the database name), and `archive.validate` (subject
`upload`, `<id>`); a "Data transfer" section states the consistency labels, the creator-only rule, and the limits.

## The console [14a: tables, uploads, settings; 14b: export database; 14c: import database]

**Where.**

| Place | Actions (administrators; others see them disabled with the reason, as `GateButton` does) |
|---|---|
| Tables list of the database page (`DatabaseDetailPage.vue`), a row action next to *Distribute table* and *Enable ledger* | *Export CSV*, *Import CSV* (a table that has an unfinished table task shows them disabled with "waiting for the operation on this table") |
| The table page of phase 9 (`/databases/:db/tables/:schema/:table`) when it exists | The same two actions in its header |
| Header of the database page, next to *Edit settings* and *Drop database* (14b) | *Export database*, *Validate an archive* |
| Header of the databases page (`DatabasesPage.vue`), next to *Create database* (14c) | *Import database* |
| A **Transfers** panel on the database page and on the databases page | The caller's staged exports and uploads (`listDataExports`): name, size, SHA-256, expiry, state, *Download*, *Delete* |
| The settings page (`SettingsPage.vue`) | A **Data transfer** group for `dataTransfer` (the switch, retention, limits) |

**Dialogs** (all are `TaskActionDialog`s or follow its pattern, so progress, cancel and errors look as they do for
the other tasks; the confirm button is disabled while a blocker shows):

- *Export CSV*: delimiter, null string, encoding (with `WIN1256` and `ISO_8859_6` offered under their common names),
  header, compression, retention, **Spreadsheet-safe values** (on), with the explanation "values that begin with
  `=`, `+`, `-`, `@`, `|` or `%` are prefixed with an apostrophe so a spreadsheet shows them as text. This changes those
  values. It is a convenience, not a security feature." The consistency of the table is shown **before** the user confirms: "This table is read as one snapshot"
  or "This table is spread over 3 groups; each group is read at its own moment, so a transaction that touched two
  groups can appear in one and not in the other." While the task runs the dialog shows `TaskProgress`; when it is
  `done` it shows size, rows, SHA-256, the expiry and a **Download** link (a plain `<a href download>` to
  `downloadDataExport`; the cookie and `SameSite=Strict` make it work and make it safe), and the sentence about
  running it twice to compare the sums.
- *Import CSV*: step 1, choose a file (a drop zone and a file input); the **browser reads the first 64 KiB locally**
  (`File.slice`) and shows the first rows and a guess of delimiter, header and encoding, which the user corrects;
  nothing is sent yet. Step 2, the options of the request, the statement that the import is **all or nothing** and what that
  means (nothing is stored on an error; one transaction; a file above 2 GiB must be split, said **before** the upload, with the file's size shown against the limit), the target's row estimate ("This table already has about N rows. Rows are
  added; nothing is replaced."). Step 3, the upload with a progress bar, **pause and resume** (the uploader keeps
  the `uploadId` and the file's name and size in `sessionStorage` inside `try/catch`, and on resume calls
  `getDataUpload` and continues at its offset; a file that differs in size is refused), then the import task with
  `TaskProgress`. **Browser limits** (DXR-21): the file is sent in `Blob.slice` chunks of 64 MiB (a multiple of 64 KiB) one at a
  time, the chunk is never read whole into a variable beyond the one `fetch` body, and the **session may expire during
  an hour-long upload**: the uploader refreshes the session between chunks and, when the server answers 401, stops,
  asks for the password again and resumes at the offset. A failed import shows the **whitelisted** text and the line
  number, never a value. `import_outcome_unknown` is shown as "Check the table before you repeat the import; N rows may
  have been loaded", with no repeat button. There is no "Continue from row N" in 14a. The size limits are shown from the
  settings before the file is chosen, and a file above them is refused in the browser and by the server.
- *Export database* (14b): tables with sizes and a checkbox to leave a table's data out, `dataMode`; the list
  of tables that use `per-group` reads; the label `archive-single-snapshot` when it applies; the large-object refusal and its
  acknowledgement; **a warning before the export when the database has recent write activity** (the cluster has the
  statistics); the sentence that tables are read one after another unless the archive is one snapshot, so tables that
  refer to each other may be read at different moments and that the **import checks the foreign keys at the end**; progress
  by table; the download as above; and the SHA-256 shown for comparison with the value an operator got elsewhere.
- *Import database* (14c): choose the archive, upload (as above), then the **validation** (`validateDatabaseArchive`) with
  `TaskProgress`, then the **archive report** as a page of the dialog: source versions and whether the target can take
  them (a refusal shows `reasons` and stops), tables by kind with rows and size, the extensions, the warnings
  (including each `per-group` label), the **statements the plan refuses** (kind, count, position; the text is never
  shown; **code in a schema is refused, with the sentence that such a database is a job for `pg_dump`**); the ledger tables with the
  required checkbox and the text of ADR 0028; the time-partition policies that will come back **disabled**; the option
  `onForeignKeyViolation`; the name of the new database; then the import
  task with its steps; on failure the message names the step and says the private database was removed (or kept).
  A role that is dangerous sees the dialog explaining that it must sign in as another administrator.

**Safety of the UI.** Names, tables and messages from an archive or a CSV header are shown through `SafeName` and
`safeText` as everywhere else; a statement's text from an archive is **not** shown (kind, position and fingerprint
only); a download link is built from the transfer id and nothing the user typed; nothing from a file is rendered as
HTML.

**Mock API** (`src/mocks`): a table `failme` fails the export, `blockme` waits, `skewme` is `per-group`, a CSV
`badrow.csv` fails the import with a whitelisted error, `toobig.csv` is above 2 GiB, `outcome.csv` answers `import_outcome_unknown`, an archive named `hostile.kdx.zip` is invalid, `code.kdx.zip`
is refused for its code, `ledger.kdx.zip` needs the acknowledgement, `old.kdx.zip` is incompatible. The mock **behaves like the server** (DXR-21): it serves a
download with `Range` and `Content-Length` and an upload with offsets, `Upload-Offset` conflicts and a 401 in the middle of an upload.

## Tests

Tests are tagged by the phase that needs them; each phase's gate runs its own and everything before it.

### Unit (Go, no database)

**14a**

- The sealing writer and reader: round trip at every segment boundary, a `Range` at every offset, a flipped byte, a
  swapped segment, a truncated file (the last-segment flag), a wrong key, nonce uniqueness over 10^6 segments; the master
  key: two first uses at once, a replaced file, a missing file (`transfer_key_lost`), wrong mode.
- The staging store: quota arithmetic, a reservation that grows, the writer that exceeds the quota fails and its neighbours do
  not, free-space refusal with a fake `statfs`, **the janitor's rules** (a foreign file, a name that does not match the
  pattern, a file younger than an hour, a lower highest id than the files show, a directory that is not empty and has no
  marker, a directory equal to or containing the data directory), a crash between rename and row update, permissions
  (0700, 0600), symbolic links refused, names derived from the id only, a role dropped and re-created under the same name.
- The relay framing: a header larger than 64 KiB, a body that ends early, a status frame after a break, the proof
  checks (replayed, expired, wrong receiver), the member-only and leader-only rules, the header allowlists both ways.
- Upload: offset arithmetic, offsets derived from the file after a crash, a duplicate chunk, a chunk past the end, a chunk
  that is not a multiple of 64 KiB and is not the last, a break at every byte, the checksum mismatch, concurrent chunks on
  one upload (one wins), creator-only access.
- The record scanner: quotes, escapes, a newline inside quotes, an unterminated quote at the end, a CR LF mixture, the
  16 MiB limit, an unquoted `\.` line (refused) and a quoted one (accepted), a BOM, a very long record split across reads at
  every position, `quote` with the client-only encodings.
- Option checking and the literal quoting of delimiter, quote and null string, hostile values (quotes, backslashes,
  newlines, NUL, non-ASCII, a value of 33 bytes), with a differential test against PostgreSQL's parser.
- **`SafeError`** (DXR-03): every SQLSTATE of the table, a fuzz target over arbitrary `PgError` fields, and the canary test: a
  CSV whose failing row carries a canary in every column of every type must leave the canary in no task, problem, audit
  event or log line.
- The spreadsheet guard: the regular expression against a table of cells (each lead character including `|`, `%`, the
  full-width forms, a no-break space before a formula character, a tab, a CR, a lone `-`, a negative number as a numeric
  column that must not change).
- The commit-intent protocol with a fake store: a lease lost before the intent (no commit can be sent), after the intent
  (`import_outcome_unknown`), a cancel at each point, a restart at each point.
- Roles and settings: every operation against every role (`readonly`, `ops`, `admin`), the setting off, a non-TLS
  request, a download by another administrator (404), a delete by another administrator (204), a role that signed out or
  was demoted between two steps.
- The audit details: no value, no statement text and no client-supplied file name in any event (the canary test over every
  field of every request).
- Contract tests (`contracttest`): the operations and responses validated against the schemas and their examples, the
  checker's rule for `x-streams`, the generated operation table.

**14b**

- The lexer: strings, dollar quotes with tags, nested comments, `E''` escapes, `U&''`, a statement of 1 MiB, one of
  1 MiB + 1, a missing terminator, `\restrict` and `\unrestrict`, every other backslash command.
- The normaliser, with the golden files of S3 (a plain table, a hash-distributed table with its bucket partitions and
  `ALTER TABLE ONLY`, a user-partitioned table with leaves and `ALTER INDEX ... ATTACH PARTITION`, a reference table, a
  columnar table with `SET default_table_access_method`, an updatable ledger table with its triggers and history,
  a time-partitioned table, views over them), per major.
- The archive writer against the validator (the shape is exactly the same), the validator on the **corpus**, the version
  rules (a table of (source, target) pairs for the PostgreSQL major, the extension versions, locale, an unknown
  `catalogVersion`, an unknown `requires`).

**14c**

- The shape grammar: a table of statements, one per row, with the verdict, **including every example of DXR-04** (`CREATE
  SCHEMA s CREATE TABLE ... CREATE TRIGGER ...`, `CREATE TABLE ... AS`, `LIKE`, an `ALTER TABLE` with several actions or
  `ENABLE ROW LEVEL SECURITY`, `COMMENT ON ROLE|DATABASE|EXTENSION`, `GRANT r TO s`, `REASSIGN OWNED`, `DO`, `SET ROLE`,
  `COPY ... PROGRAM`, `CREATE FUNCTION`), the function collection, a `GENERATED ... AS (pg_sleep(86400))` column.

### Integration (a cluster of the test's own, on **PostgreSQL 18 and 19**, from one source)

Each runs on both majors with the lane mechanism of the integration runs (`-Lane`), in a private PostgreSQL install per
run.

**14a**

- Table export of a local, a reference, a distributed (3 groups), a user-partitioned and a columnar table: the file
  equals the table (the project's `compare_tables`), the labels are right, writers run during the export and every row of
  the file was committed, a bucket move during the export restarts it, a lost group primary restarts it, a cancel leaves
  no file; a table with row-level security (the statement fails, no shortened file), a table whose virtual generated column
  calls another role's function (`export_untrusted_code`); a columnar export that is 10 times larger than the table
  (the reservation grows, the neighbour is not hurt).
- Table import: every option (delimiter, quote, null string, BOM, `WIN1256` and `LATIN1`, header `none`, `skip`, `match`),
  a bad row at the last line (**nothing stored**, the absolute line number, no value in any output), the unquoted `\.` line
  (refused), a quoted `"\."` row and a single-column file on both majors **through the router and through a local table**,
  a count mismatch forced by a fault (rolled back, nothing stored), the commit intent and a fault that drops the connection
  after the intent (`import_outcome_unknown`) and one before it (nothing stored, a clean retry), a cancel before and after
  the intent, a 2 GiB file with the memory of `kingresd` measured (flat), a file of 2 GiB + 1 (`import_too_large`), a
  reference table (**the batch form of V3 if it passed, the refusal if not**), a ledger table (entries exist, `ledger_verify`
  clean) and one with the policy `required` (refused), a local table with row-level security (refused up front), a table
  with a trigger, a partitioned table, a role demoted between two steps.
- Upload: a break at every chunk and at a random byte, resume, a 20 GiB-declared file refused, the quota, the free-space
  reserve with a small tmpfs (`insufficient_disk` and the cleanup), the janitor over a restarted leader, a session that
  expires in the middle (the console's flow).
- **Leader change** during an export, an upload and an import at each step (the failover tests' fault points): the codes,
  the partial results, no orphan file after the old leader's janitor runs.
- The relay: S1's scenarios as regression tests, through a follower, with a 3 GiB file and a kill in the middle on HTTP/1.1
  and HTTP/2, flat memory on both servers; the download's `Content-Length` makes a truncation visible.
- Hostile CSV: a gzip bomb, a record of 17 MiB, a header with a hostile name, a NUL byte, a CSV that is a different encoding
  from the one declared.

**14b**

- Database export of the round-trip database (below) on 18 and 19: the archive validates; the writer's output is
  accepted by the validator; the fingerprint catches a column added during the export (`schema_changed`); a database with
  large objects (`large_objects_present`), with a policy or a rule (`schema_unrecognised`); a partitioned, a timepart, an
  inheritance and a mixed distributed tree (**no row twice**, DXR-08); a database whose tables are all on one group
  (`archive-single-snapshot`); a database of tables the role cannot read (`export_not_readable`); an export as a role
  that is not a member of `pg_read_all_data` in a database with `kingres_ledger` and `kingres_timepart` (DXR-12); a table
  task refused while an export runs (409) and the reverse.
- Every archive of the **corpus** through the real endpoint: nothing created, the validator's memory and time bounded.

**14c**

- **Round trip of a database with every table kind** (the acceptance test): local, reference, hash (two colocation groups, one
  with a different bucket count), user-partitioned, columnar, an updatable and an append-only ledger table, a time-partition
  policy, sequences used from two nodes, foreign keys between tables of both kinds, views and a materialized view, an enum,
  a domain, comments, a non-default encoding and locale, an extension in a schema other than `public`, a table with every
  column type of the project (including sketch types, `money`, `bytea`, `jsonb`, arrays, ranges, infinity, `-0`, NaN, a
  1 MiB text, a text `\N`, an empty string, a null), is exported on 18, imported on 18 **and** on 19, and compared: the same
  tables, the same distribution (class, column, bucket count, colocation relations), every table row for row, sequences
  above the maximum on every node, the policy of every ledger table back, time-partition policies present **and disabled**,
  the ledgers enabled, the verifier clean, constraints valid, `check_cluster()` clean. A **18 to 19 archive loads** and a **19
  to 18 archive is refused** (`archive_incompatible`).
- Import after the export of a database that was written to during the export (skew): with `fail` the foreign-key step fails
  with the constraint's name and the private database is removed; with `keepNotValid` the constraints stay `NOT VALID` and
  the violations are reported.
- A **leader change at every step** of the import: the private database is dropped, nothing of the final name exists, no orphan
  file; a second import of the same name started meanwhile is not harmed; the rename (or its fallback, V8).
- Hostile dumps through the endpoint: `COPY ... PROGRAM`, `DO`, `SET ROLE`, a function, `ALTER SYSTEM`, `CREATE ROLE`, `GRANT r
  TO s`, `COMMENT ON ROLE`, a schema `kingres`, a table named like a system table, a view that calls `pg_sleep`: each is refused
  at the plan step and **nothing was created** (`pg_database` unchanged, no staging leftovers).

### The corpus of hostile archives [14b]

One generator in the test code produces, from a good archive: a path with `..`, an absolute path, a backslash, a NUL, a
duplicate name, a directory entry, a symbolic link, an unknown name, a missing manifest, an oversized manifest and
catalog, JSON nested 10 000 deep, an unknown member, a header that declares 1 KiB over a 1 GiB stream and the reverse, a
deflate bomb that expands to more than `maxImportBytes`, overlapping entries, hidden bytes between entries and after the
central directory, **a local header whose name differs from the central name, a local `extra` field that hides bytes, flags
that differ, a descriptor that the flag does not announce and a missing one that it does**, a second end record inside a
comment, an archive inside an archive, Zip64 fields that lie, a wrong CRC, a wrong SHA-256, a manifest that disagrees with the
catalog, **a data entry whose `columns` differ from the schema**, an `extensions` list with an unlisted extension, an identifier
with a quote or a newline, a bucket count of 0 and 4097, a gigantic `rows`, 100 000 entries, a `format` of
`kingres-export/2`. The expected result of each is `archive_invalid` or `archive_incompatible` **before any statement or
database**, and the validator's memory and time are bounded (a test measures them).

### Fuzzers

Native Go fuzz targets for the record scanner, `SafeError` (14a), the lexer, the shape grammar, the archive validator (a
byte-level mutation of a good archive), the manifest and catalog decoders and the option literals, with the corpus above as
seeds. They run in CI for a fixed time and the project's cap on fuzzers applies (WSL runs cap them).

### Mutants

One-line edits, each of which must be **killed** by a named test (the same method as the analytics extensions' reviews).

**14a:** the scanner treats a quote inside a quoted field as the end of the field; the scanner ignores a newline inside
quotes; the 16 MiB limit is off by one; the unquoted `\.` check is removed; **the count comparison is moved after the
`COMMIT`**; **the commit intent is written after the `COMMIT`**; the intent write ignores the lease; an import with the
intent recorded is retried; the `Upload-Offset` check is `>=`; the offset is taken from the row and not the file; the
creator check on download is removed; the creator check on delete is added to the admin path (it must not be); the setting
check is removed; the TLS check is removed; the role is not re-checked between steps; **`SafeError` copies `Message`**;
the spreadsheet pattern loses `@` or `|`; the guard changes numeric columns; the consistency label is always
`single-snapshot`; the free-space reserve is zero; the quota reservation is not released on delete; the reservation does not
grow; **the janitor deletes a name that does not match the pattern, or a young file, or runs after a restore**; the
directory marker check is removed; the master key is created without `O_EXCL`; the guard's `RequestTimeout` is not left for
a stream; a download omits `Content-Length`; the audit event includes statement text or a value; the quote is allowed above
0x40 with `SJIS`.

**14b:** the writer omits a descriptor; the validator ignores the local name, the local `extra` or the descriptor flag; the
overlap check or the hidden-byte check is removed; the declared-size limit reader is `+1000`; the SHA-256 comparison is
skipped; the PostgreSQL major compare is reversed; the extension version compare is `>`; a partition leaf is exported
(rows twice); the fingerprint check is removed; large objects are not counted; the normaliser keeps `ONLY`, drops a
removal list entry, or accepts a `PARTITION BY HASH` on another column; a data entry is written to a path built from its
name.

**14c:** the grammar allows `COPY`, `SET`, `GRANT`, `CREATE FUNCTION`, a second `ALTER TABLE` action, `CREATE SCHEMA`
elements, `COMMENT ON ROLE`; the lexer ends a dollar quote at the first `$` or ignores a nested comment; a backslash line
other than `\restrict` is dropped instead of refused; the function check is skipped; the ledger acknowledgement is not
required; `ledger_enable` is called before the load; history rows are loaded; the ledger policy is not set again;
time-partition policies are imported enabled; the `OnFail` drop takes a name other than the private one; step 4 treats an
existing database as success; the final rename does not check the name; views are created before distribution.

## Increments

Each increment builds and passes its own tests; the contract checker passes from A0 on. Sizes: **S** up to 2 days,
**M** 3 to 5 days, **L** 6 to 10 days, **XL** 3 to 4 weeks, of one writer. Phases are released one after the other
(section "Phases"); an increment of a later phase does not start before the gate of the one before, except the spikes.

### Spikes first (they change the design if they fail)

| # | Spike | Size | Answers | Needed by |
|---|---|---|---|---|
| S1 | **The relay**: a follower and a leader, 5 GiB upload and download through the follower, flat memory, a kill of each side mid-stream on HTTP/1.1 and HTTP/2, idle deadlines, the frame limits (section "The streaming relay") | M | F1, F2, DXR-20; the whole of CP3 | CP3 |
| S2 | **The router's `COPY`**: `PgConn().CopyFrom` through the router, the command tag, `HEADER` and `HEADER MATCH`, an unquoted `\.` line and a quoted one and a one-column file on 18 and 19 (V6), the batch `INSERT ... unnest` form for a reference target (V3), one cursor per group for a partitioned tree (V4), the text round trip of every column type of the project with the fixed formats (V5), `COPY` into a `GENERATED ALWAYS` identity column (V7) | S | DXR-02, DXR-25; the shape of CP5 | CP5 |
| S3 | **`pg_dump` for every table kind** on a three-group cluster, PostgreSQL 18 and 19, with the build of this tree, run by the service role: the statements written for a plain, a hash-distributed, a user-partitioned, a reference, a columnar, a ledger and a time-partitioned table, views over them, `\restrict` (V2); `ALTER DATABASE RENAME` across groups and the registry (V8); the functions a read can reach (V9). Its result is the **supported subset**, written into "Normalising the dump" | M | DXR-06, DXR-07, DXR-12; all of 14b and 14c | 14b |

S1 and S2 are done by the control-plane writer in the first week (or by a third person); S3 can run in parallel with
14a and is done at once because the owner asked for it first.

### Phase 14a: two writers, one control plane and one console

The **contract is written first and is its own commit (A0)**. After it, a control-plane writer and a console writer
work **independently**: the contract, its examples and the generated types are the interface, and the console works
against a mock until the server exists.

**A0: contract 0.19.0, a commit of its own (M).** `kingres/api/openapi.yaml` and everything generated from it:

- the ten operations of 14a: `exportTable`, `importTable`, `createDataUpload`, `getDataUpload`, `uploadDataChunk`,
  `deleteDataUpload`, `listDataExports`, `getDataExport`, `downloadDataExport`, `deleteDataExport` (140 to 150), with
  `x-streams` on the last and on `uploadDataChunk`, `x-required-role`, the shared responses, and **an example for every
  request and response** (the console's mock and the server's contract tests both use them);
- the schemas of "REST contract" that 14a needs (`ExportTableRequest`, `ImportTableRequest`, `DataUpload`, `DataExport`,
  `DataExportPage`, `TableConsistency`), the problem codes of 14a, the task kinds `table.export` and `table.import`, the
  audit actions, `ClusterSettings.dataTransfer`, the `Problem` extension `reasons`;
- the checker's new rules (`x-streams`, the headers `Upload-Offset` and `Content-Range`), the README rows, the generated
  operation table of `kingresd`, the contract tests, the regenerated TypeScript types of the console;
- no code of the server and no code of the console beyond what the generator writes.

**Control-plane track** (one writer, or two: CP1 and CP2 do not depend on each other):

| # | Increment | Size | After |
|---|---|---|---|
| CP1 | **Record, setting and error text**: migration `0035_data_transfer.sql` (also checked on a follower: the migration runs on the leader's system database), the store accessors for the record (role **OID and name**, DXR-23), the `dataTransfer` setting and its flags (`transfer-dir`, `transfer-key-file`, `transfer-max-duration`, `maxAtomicBytes`), the audit actions, `transfer.SafeError` with the canary tests and fuzz target, the helper that re-checks the role at a step | M | A0 |
| CP2 | **Staging store and sealing**: the directory (marker file, refuses a non-empty unmarked directory and the data directories, DXR-17), reservations that grow as bytes are written, the quota and reserve, the janitor with its name pattern, minimum age and restored-database guard, the sealing writer and reader (64 KiB segments, `io.ReaderAt`), the master key (`O_EXCL`, fsync, rename, a lost key turns `ready` into `lost`, DXR-24); unit tests and the mutants of the store | M | A0 |
| CP3 | **Relay, uploads, downloads**: `/internal/v1/forward-stream`, the guard flag that leaves `RequestTimeout` and `MaxBytesReader`, the deadlines, the header allowlists, `forwardsToLeader` and `leaderBoundReads`, the TLS and setting guards, the eight operations (`createDataUpload`, `getDataUpload`, `uploadDataChunk`, `deleteDataUpload`, `listDataExports`, `getDataExport`, `downloadDataExport`, `deleteDataExport`), the audit event at the end of a stream; tests with a follower; S1's scenarios as regression tests | L | S1, CP1, CP2 |
| CP4 | **Table export**: `table.export` (statement builder, session formats, `row_security = off`, the untrusted-code check, the spreadsheet guard, consistency labels, the per-group reservation), integration on PostgreSQL 18 and 19 | M | CP1, CP2 (the download is tested with CP3) |
| CP5 | **Table import**: the record scanner (unquoted `\.` rejected), the pipeline, `BEGIN`, `COPY`, the count check, the commit intent, `COMMIT`, the size cap, header, `import_outcome_unknown`, the role checks between steps, local and distributed targets, ledger tables, reference tables **if S2 passed V3**; integration on 18 and 19, the memory test | L | S2, CP1, CP2 (the upload is tested with CP3) |
| CP6 | **Hardening of 14a**: fault-point tests of the leader change at every step, the hostile CSV tests, the canary tests end to end, the mutant run of 14a, `internal-api.md`, the READMEs of the control plane and the contract, the metrics and alerts for staging disk use and transfer counts, an independent review and its fixes | M | CP3, CP4, CP5 |

**Console track** (one writer, starts at A0 with the mock):

| # | Increment | Size | After |
|---|---|---|---|
| CN1 | **API layer and mock**: the generated types and `endpoints.ts`/`queries.ts` for the ten operations, mock handlers and state that **behave like the server** (offsets, `Range`, the task steps and progress, the retention, the problem codes) from the contract's examples, the fixtures `failme`, `blockme`, `skewme`, `badrow.csv`, `toobig.csv` (above 2 GiB), `outcome.csv` (answers `import_outcome_unknown`) | M | A0 |
| CN2 | **Setting, gates and the Transfers panel**: the **Data transfer** group on the settings page, the actions disabled with the reason when the setting is off or the role is too low, the Transfers panel (the caller's files, size, SHA-256, expiry, download, delete) | S | CN1 |
| CN3 | **Export CSV dialog**: options, the consistency sentence **before** the confirmation, the spreadsheet text ("not a security feature"), progress with `TaskProgress`, the result with size, rows, SHA-256, expiry, a download link, and the sentence about running the export twice | M | CN1 |
| CN4 | **Import CSV dialog**: the local preview of the first 64 KiB with a guess of delimiter, header and encoding, the options, the size cap shown before the upload (2 GiB, "split the file"), the **resumable uploader** (64 MiB `Blob.slice` chunks one at a time, resume from `getDataUpload`, pause, a refreshed session between chunks and a prompt for the password on 401, state in `sessionStorage` inside `try/catch`), the import progress, the whitelisted error text, the `import_outcome_unknown` wording | L | CN1 |
| CN5 | **Console tests**: every state, every role, hostile names and file names, the live validation, the browser tests at 1366, 683 and 320 px and the axe tests in both themes, the mock's hostile cases | M | CN2, CN3, CN4 |

**Joining the tracks.** E1 (S): the console against a real `kingresd` on a three-server cluster, a 2 GiB upload through a
follower, an export, a download and an import end to end, on PostgreSQL 18 and 19; after CP3, CP5 and CN4. **Release of
14a** needs CP6, CN5 and E1.

**Time.** The control-plane track is about 36 days of one writer (CP1 4, CP2 4, S1 3, CP3 8, CP4 4, CP5 8, CP6 5) and
about 24 with a second writer taking CP2, CP4 and the parallel parts; the console track is about 22 days; A0 is 3 and S2
is 2. The critical path is A0, S1, CP3, CP5, CP6: about 5 weeks with two control-plane writers.

### Phase 14b: database export and validation only

| # | Increment | Size | After |
|---|---|---|---|
| B0 | Contract 0.20.0 as a commit of its own: `exportDatabase`, `validateDatabaseArchive`, `ArchiveReport`, the codes, the kinds | S | 14a gate |
| B1 | **Archive writer and validator**: the streaming zip writer and the strict validator (local headers, descriptors, extents, names, sizes counted while reading, the sum bound), the manifest, the version rules, the **hostile corpus of about 30 archives** and the fuzz targets | L | B0 |
| B2 | **Catalog document, schema dump and normaliser**: the catalog reader, the `pg_dump` run by the service role, the lexer, the normaliser with the removals and rewrites **of S3's result**, the golden files per major, the refusal of the unsupported subset | XL | S3, B0 |
| B3 | **Database export task**: the steps, the single-snapshot case, the fingerprint and `schema_changed`, partition trees, large objects, the per-group reservation, the exporter's self-check through the validator | L | B1, B2 |
| B4 | **Validate task**: `archive.validate`, the report, the cache by SHA-256 | M | B1 |
| B5 | **Console**: the export dialog (the tables, per-group warnings, the skew warning), the upload of an archive, the validation report page | L | B0 (mock), B3, B4 |
| B6 | **Review and hardening**: the leader-change fault points, the corpus at the endpoint, the mutants of 14b, the independent review | M | B5 |

About 8 to 9 weeks of one writer; B1 and B2 are independent and can be written in parallel.

### Phase 14c: database import

| # | Increment | Size | After |
|---|---|---|---|
| C0 | Contract 0.21.0 as a commit of its own: `importDatabase`, the codes (`archive_statement_refused`, `ledger_restart_not_acknowledged`, `import_too_large`) | S | 14b gate |
| C1 | **The shape-grammar parser**: one parser per allowed kind, the function collection and the check against `pg_proc`, the plan, the fuzz targets and the mutants of the classifier | XL | C0 |
| C2 | **The import task**: the thirteen steps, the strict creation under a private name, **V8's rename or its fallback**, the distribution from the document, views after distribution, the load per table with the intent, sequences on every node, ledger and policies, the `OnFail` drop | XL | C1, S3 |
| C3 | **Console**: the import dialogs and the report page, the typed confirmations | L | C0 (mock), C2 |
| C4 | **Round trip and hostile integration**: the round trip of a database with every table kind on 18, imported on 18 **and** 19, the 19-to-18 refusal, the skew case, a leader change at every step, the hostile dumps through the endpoint with nothing created | L | C2 |
| C5 | Independent review of the classifier and the import, and its fixes | M | C4 |

About 12 weeks of one writer; C1 and the console can overlap.

### Phase 14a+

Batches with the marker (section "Batches"), "continue" from the markers, and reference tables in several transactions:
L, own review, started only when the owner asks.

## Results of the spikes and changes made by the code (phase 14a)

This section is written by the control-plane writer of 14a and says where the code differs from the text above, and why. The text above is left
as the design was reviewed; where the two disagree, this section is right.

### Spike S1: the relay (2026-11-22)

**Result: passed.** `internal/relay/spike_test.go` runs a follower and a leader as two TLS servers with the limits of the real ones (read
timeout 30 s, write timeout 60 s, header timeout 10 s on both; the leader is HTTP/1.1 as the internal listener is), relays an upload and a
download through the frame, and drives them from a client on HTTP/1.1 and on HTTP/2.

| Scenario | Result |
|---|---|
| 5 GiB up (in 64 MiB chunks, the sum of each chunk checked by the leader) and 5 GiB down (SHA-256 of the whole) through the follower | all bytes arrive; HTTP/1.1 73 s (140 MiB/s each way), HTTP/2 48 s (215 MiB/s each way); **heap growth 3 MiB (HTTP/1.1) and 13 MiB (HTTP/2)** over a run that moves 10 GiB |
| deadlines with short limits (read 1 s, write 1.5 s, idle 0.8 s) | a steady stream of 4 s is not ended, on either protocol; a client that stops for the idle time is ended after the idle time and the leader sees the error |
| client, leader and follower each end the connection in the middle of an upload or a download | the other sides see an error, nobody hangs, goroutines return to the starting count; a download cut by the leader reaches the client as a short body with its Content-Length (an error to the client) |
| frames: larger than 64 KiB, cut, empty, not JSON, unknown member, data after the value, wrong type | refused by `relay.ReadFrame`; the sender refuses to send a frame above 64 KiB |
| headers outside the allowlist (`Set-Cookie`) | not relayed |

What the spike changed in the design:

1. **The idle body moves the write deadline too.** The answer of an upload (204) is written when its last byte was read, minutes after the request
   began, and the write timeout of the server counts from the start of the request: without a write deadline that moves, the answer of every upload
   longer than the write timeout fails (on HTTP/1.1 as `tls: bad record MAC` at the client, on HTTP/2 as `INTERNAL_ERROR`). `relay.IdleBody` therefore
   sets both deadlines with every read, and gives the write a time of its own when the read ends in an error, so that the error can be answered.
2. **HTTP/2 needs nothing special.** `http.ResponseController` moves the deadlines of a stream on HTTP/2 as on HTTP/1.1 (tested above), so nothing in the
   design is assumed about it any more.
3. The leader side needs no change of the internal server: its read timeout is moved by the same wrapper and its answer is written through
   `deadlineWriter`, which already reaches the connection through `Unwrap`.

The same scenarios run again as cluster tests through a real follower (CP3).

### Spike S2: the router's COPY (V3 to V7)

The result is `dx/s2-result.md` of the second writer (2026-11-22, PostgreSQL 18.7 and 19.0, identical answers). What it changes, as the code applies it:

1. **V3: reference tables are importable in 14a, with the typed-array form**, not the text-array form with casts: the router refuses any cast of text in
   the select list of an insert into a reference table (`0A000`, the write is replayed on every node), and accepts `INSERT ... SELECT u.c1, u.c2 FROM
   unnest($1::int[], $2::text[]) AS u(c1, c2)` with typed arrays and multi-row `VALUES`. The statement parses the values once on the origin, under the
   fixed session formats. A test checks that the checksum of the table is equal on every node after the import.
2. **V6: `header: match` is done by PostgreSQL on every target.** The exception "the control plane reads the first record" is removed. A mismatch is
   `22P04` whose message echoes the header text, so it is mapped to the sentence "the header of the file does not match the columns" and the task code
   `import_header_mismatch`; the first difference is **not** named (it would be file content). It is a code of the failed task; `importTable` does not answer
   422 `import_header_mismatch` in this version (the code stays in the contract for the day a request-time check exists).
3. **V6: the unquoted `\.` line** is data in CSV on PostgreSQL 18 and 19, routed and local. The rejection (`import_end_marker`) is kept for the first release
   as the conservative choice, and the code says so; it rejects a legal file (a one-column file whose rows can be `\.`) and is to be dropped when the same
   test passes on every supported major.
4. **V7: what errors carry.** The router errors for `23xxx` have **no** table, column or constraint field (or a bucket partition name `dtab_b0000`
   in them) and no line number, and `where` carries the unix socket directory of a node. `SafeError` therefore takes names only from fields that are
   present and do not look like a bucket partition (`_bNNNN`), never from the text, adds the target table from the task (`Error.ForTarget`), and says
   "the row that failed is not known" for a class 23 error without a line. Tests: no `_bNNNN` name and no socket path reaches any output.
5. **V4: the label is `per-group` for a partitioned table too**; `per-group-and-partition` stays in the contract enum and is never written.
6. **V7: identity columns.** A `GENERATED ALWAYS AS IDENTITY` column may be listed in the file; a stored generated column may not (`42P10`, as designed).
   The sequences of a distributed table are node-local (F18): rows loaded without the identity column from different entry nodes can repeat values. The
   README says so; the console shows a warning for such a table (a contract-free text).

### Other changes made by the code

- **Role identity (CP1):** `kingres_cp.data_transfer` keeps the role by **name and OID**. PostgreSQL has no creation time for a role, so "OID with the role
  creation identity" is the OID: a role dropped and made again under the same name has another OID (checked at every step and at every read).
- **The record has two more columns, `reserved_bytes` and `updated_at`**: the reservation of the staging quota that a file holds (the sum is what the
  quota is checked against) and the last time bytes arrived (the janitor rule "nothing received for 48 hours").
- **The setting is read by the leader only.** The guard `dataTransfer.enabled` runs on the server that runs the operation, because the setting is in
  the system database of the leader; a follower checks TLS, the session and the role, and relays. The TLS check is made by the server that received the request.
- **`ClusterSettings.dataTransfer.stagingQuotaBytes`** shows 100 GiB while the administrator has not set it; the effective default, the smaller of 100 GiB and a quarter of
  the file system, is applied where the staging directory is known (the staging store).
- **Contract 0.19.0 lists only the values of this revision** for the enums that 14b extends (`DataUpload.state`, `DataExport.kind` and `format`, `purpose`,
  `TableConsistency.level`), and only `ImportTableRequest` is a closed schema.
- **`maxAtomicBytes` is a flag** and is not in the setting (the console states 2 GiB).
- **Load and commit are two steps that share an open transaction in memory** (the design lists them as two steps and a transaction cannot span a restart). The step
  "Load rows" is not engine-NonIdempotent: a run that finds nothing committed may run again. The commit step writes the intent (`commit sent for N rows`) into the task
  detail under the lease, and a run that finds the marker fails `import_outcome_unknown` with its own code (the engine's `step_interrupted` would have been the code
  otherwise). A connection that died before the intent is found by a `SELECT 1` before the intent is written: nothing stored, `copy_interrupted`, retryable.
  `Task.cancellable` is false from the intent on. Fault hooks: `ops.ImportFault`.
- **Request time and step time.** `exportTable` and `importTable` do the checks of the first step at the request (so that the contract's 404, 409 and 422 answers exist)
  and the steps do the role check and the table check again. The record and the reservation are made at the request; a refused request leaves a record in the state
  failed, which the janitor prunes after seven days.
- **The reservation of an upload is the declared size plus the overhead of the sealed form** (header and a tag per segment), and the export estimate is the local size
  times the groups times 1.5 (10 for a columnar table), at least 64 MiB and at most the largest file.
- **Reference tables refuse array columns**: `unnest` of a typed array flattens an array column. A reference table is always loaded through the temporary table (the
  direct COPY is refused on several groups), with the cap of 256 MiB (a constant, not a flag).
- **Header match** is PostgreSQL's (S2); the header text is never passed on, so the task code is `import_header_mismatch` without the first difference.
- **`maxConcurrent`** is one transfer per role and the setting in the cluster, counted from the unfinished tasks of the two kinds.
- **The cluster test harness serves HTTPS for the data transfer** (`clustertest.FormTLS`, the TLS cluster of `cmd/kingresd`), because the operations refuse plain HTTP.
- **Metrics and an alert were added to contract 0.19.0 before its release:** the metric names transferStagingUsedBytes, transferStagingFreeBytes, transfersRunning and
  transfersStaged, and the alert kind transferStagingLow (90 and 98 percent of the quota); the monitor of the leader samples them from the staging store.
- **The hook `ops.ImportFault`** has the points beforeIntent, intentWrite (an error stands for the refusal of the write of the intent, a lost lease) and afterIntent.
- **Fault tests that were added later:** a record of 15.5 MiB through PostgreSQL, a cancel after the intent, a refused write of the intent, the untrusted-code check with a function of
  another role, a restart of the leader in the middle of an import and of an export, a shard group stopped in the middle of a routed COPY (the import ends copy_interrupted, retryable,
  and the table has none of the rows or all). Still not covered: a real failover of the manager leader (the files of the old leader are on its disk, so every step ends transfer_leader_changed;
  only the codes and the record reads are tested), KS001 from a bucket move during a routed COPY, and a group lost exactly at COMMIT.
- **Not covered by tests in the first increments (some of the items are covered above):** the leader-change fault points of a real failover at every step (the leader that holds the files is gone: the codes
  `transfer_leader_changed` and `transfer_session_lost` are in the code and tested only for the records they read), `KS001` and a bucket move during a routed COPY, a
  group that fails at COMMIT, a record near 16 MiB through PostgreSQL (the scanner is tested at the limit), the untrusted-code check against a role other than the owner
  (the query runs in every export; no table in the tests has a function of another role), and metrics and alerts for the staging disk.

## Known limits

- **No global snapshot.** A distributed table over several groups is exported per group; the labels say so. A database
  archive is one snapshot only when every table is local, reference or on the connection's group. The cure is phase 8.
- **A large file is split by the user** (14a): an import is atomic, at most 2 GiB decompressed. Batches are designed and
  deferred (14a+).
- **Archives are limited by `maxFileBytes` (20 GiB by default).** A larger database needs `pg_dump` and backups. An archive
  of a database that uses what "What the archive does not carry" lists is refused, not shortened.
- **The downloaded file is not encrypted** and the checksums are not signatures. An operator who needs either
  encrypts and signs the file outside, and can bind the import to a SHA-256 (`expectedSha256`). A browser cannot hash a
  large file incrementally, so the SHA-256 flow is "the export shows it, the import dialog asks for the value".
- **Spreadsheet safety is by column type and is not a security feature.** A `json`, an array or another type whose text
  begins with a formula character is not changed.
- **A leader change ends a transfer.** It is not resumed; an upload must be sent again.
- **Roles, owners, privileges and the placement of buckets are not carried.** The importing role owns everything and
  grants are not restored; the target places the buckets. `privileges: restore` and `allowCode` do not exist.
- **Code and some object kinds in a schema** (functions, triggers, policies, rules, event triggers, collations, foreign objects,
  publications, extended statistics, unlogged tables) are refused; such a database is a job for `pg_dump`.
- **Import never replaces and never skips bad rows.**
- **A reference table** is imported only if S2 passed V3, in one transaction of at most 256 MiB.
- **The history of an updatable ledger table is exported and not loaded**, and the ledger of the target is a new chain.
- **TLS must end at `kingresd`.** A transfer behind a proxy that terminates TLS is refused (`insecure_transport`) until a
  setting for a trusted proxy exists.
- **An unknown commit outcome** (`import_outcome_unknown`) needs the user to look at the table. Only batches with the
  marker (14a+) make it decidable.
- **Needs verification**, each by the spike or increment named: V1 (what `pg_dump` writes for every table kind, S3), V2
  (`\restrict`, S3), V3 (reference-table insert form, S2), V4 (one cursor per group for a partitioned table, S2), V5 (text
  round trip of every type of the project by `COPY` in CSV with the fixed formats, S2), V6 (`COPY FROM STDIN` through
  `PgConn().CopyFrom` with the router: `HEADER`, `HEADER MATCH`, `\.`, the command tag, S2), V7 (`COPY` into a `GENERATED
  ALWAYS` identity column, S2), V8 (database rename, S3), V9 (the functions a read can reach, CP4). Each has a fallback
  above and is recorded in this note when it is checked.

## Open questions for the project owner

None open. Answered 2026-11-22: `dataTransfer.enabled` is **off** by default; **no** passphrase for the downloaded archive in
version 1; a database import goes **only into a new database** (now the private database of the task, then renamed).

## Review findings and where they are answered

The independent review ([../reviews/2026-11-22-data-export-import-design.md](../reviews/2026-11-22-data-export-import-design.md))
found 27 points. Each is either a change of the design or a reason why not; "Phase" says which release carries it.

| Finding | Sev | Answer | Where | Phase |
|---|---|---|---|---|
| DXR-01 committed batch forgotten, retry doubles rows | high | Batches are **not in 14a**; the all-or-nothing import writes a **commit intent** under the lease before `COMMIT`, so a lost-lease leader cannot commit and an unknown outcome is reported as unknown. Batches are designed with a **unique marker in the same two-phase transaction** (reference table), "continue" computed from the markers | "The load", "Batches", "Leader change" 2 and 3 | 14a, 14a+ |
| DXR-02 count check after the commit | high | Explicit `BEGIN; COPY; compare; intent; COMMIT`; the comparison is a precondition of the commit; the scanner rejects an unquoted `\.` line; tested on both majors through the router and a local table (S2) | "The pipeline", "The load" | 14a |
| DXR-03 task errors carry row values | high | **`SafeError` whitelist** (SQLSTATE, a sentence per state, constraint, column, table, absolute line); used for the problem, `TaskError`, `Task.detail`, the audit event and the log; canary tests and a fuzz target | "Error text" | 14a |
| DXR-04 classifier on leading keywords | high | Replaced by a **shape grammar**: one parser per kind consumes the whole statement; one `ALTER TABLE` action; no schema elements, `AS`, `LIKE`; a function check for expressions; the claim is restated: the bound is PostgreSQL's privileges | "What may run: the shape grammar" | 14c |
| DXR-05 `privileges: restore` gated by the archive's list | med | **`privileges: restore` removed**; `GRANT`, `REVOKE`, `ALTER DEFAULT PRIVILEGES`, `REASSIGN OWNED` are not in the grammar; no `referencedRoles` in the catalog | "Request (14c)", grammar table, "The catalog document" | 14c |
| DXR-06 normaliser cannot be removal only | high | **Removals and a closed set of rewrites** (`ONLY`, access-method `SET`, `ALTER INDEX ... ATTACH`); **spike S3 first** on a real cluster; the subset is stated; the exporter runs its output through the importer's validator | "Normalising the dump", S3 | 14b |
| DXR-07 views before distribution | med | Views and what depends on a table go to `schema/views.sql`, created **after** distribution; materialized views `WITH NO DATA`, refreshed last | "The archive", import steps 8 and 9 | 14b, 14c |
| DXR-08 partition roots and leaves exported twice | med | Only the **root** of a tree is exported and loaded, leaves are structure; `FROM ONLY` for inheritance; `treeRole` in the catalog; an independent `count(*)` check | "Partition trees and inheritance", export step 5 | 14b |
| DXR-09 large objects and `oid` columns | med | Counted at export; **refused** with `large_objects_present` unless acknowledged; the section "What the archive does not carry" | "What the archive does not carry" | 14b |
| DXR-10 import can load into a database it did not create | med | A **private database name** `kingres_import_<task id>`; creation **fails** if the name exists; rename to the final name last (V8), with a stated fallback | "The task `database.import`" 4 and 13, "Failure" | 14c |
| DXR-11 export runs other roles' code; RLS silently shortens | med | **`export_untrusted_code`** (catalog check, acknowledgement); **`row_security = off`**, so a hiding policy fails the statement; `rlsApplied` recorded | "The task `table.export`" step 1 and 3 | 14a |
| DXR-12 `pg_dump` as the role fails with the project's extensions | med | The schema is dumped by the **service role, read-only, schema only**; the data is read as the role; a test as a non-`pg_read_all_data` admin | "The task `database.export`" step 2 | 14b |
| DXR-13 skew makes large imports fail at the end | med | One **`REPEATABLE READ READ ONLY`** transaction when every table is local, reference or on the connection's group (`archive-single-snapshot`); `onForeignKeyViolation: keepNotValid`; a console warning on recent write activity; the fingerprint | export step 4, import step 10, "The console" | 14b, 14c |
| DXR-14 export subject does not stop DDL or table tasks | med | **Table tasks refused while a database export or import runs, and the reverse**; catalog, schema and column lists taken in **one transaction**; **fingerprint** recomputed at the end (`schema_changed`); columns of each data entry in the manifest | "The task `database.export`" | 14b |
| DXR-15 zip rules incomplete | med | Local header against central, empty `extra`, flags, descriptor exactly when flagged, extent includes header and descriptor; the writer's shape is the only accepted one; **no per-entry ratio**, the bound is `maxImportBytes` and free space; validation **cached by SHA-256** | "Hostile input", "The archive" | 14b |
| DXR-16 disk estimates wrong; quota not enforced while writing | med | Per-group sizes times a factor (columnar 10, others 1.5); the reservation **grows**; the offender fails, not its neighbours | "Where files live" 2 | 14a |
| DXR-17 janitor deletes files in an operator-chosen directory | med | Marker file, refusal of a non-empty unmarked directory and of data directories; **name pattern, one-hour minimum age, restored-database guard** | "Where files live" | 14a |
| DXR-18 ledger and timepart policy restoration | med | The ledger policy is **set again after load and verify**; time-partition policies import **disabled** with the enabling call in the report; both in the audit event | "Ledger tables", import step 12, ADR 0028 | 14c |
| DXR-19 locale, encoding and extension schemas | med | In the manifest; the database is created with them or the import is refused; extensions are installed into their recorded schema | "The archive", import steps 2, 4, 5 | 14b, 14c |
| DXR-20 relay under-specified | med | The frame, the request and response header lists, `forwardsToLeader` and `leaderBoundReads` entries, leaving the guard's context and body limit, the audit event at the end of the stream, `Content-Length` so truncation is visible, SHA-256 in the dialog; **spike S1** with kills on HTTP/1.1 and HTTP/2 | "The streaming relay", S1 | 14a |
| DXR-21 sizing and ordering | med | **Spikes first** (S1, S2, S3); phases; 6 and 8 resized to XL, with separate B and C increments; metrics and alerts in CP6; the migration on a follower in CP1; browser limits in CN4 | "Increments" | all |
| DXR-22 `allowCode` adds exposure for little | low | **`allowCode` removed**: functions and triggers are refused always | "What may run", "Hostile input" | 14c |
| DXR-23 creator-only weaker than stated; proxy map; role identity | low | Stated as **defence in depth**; the role is **re-checked at every step**; `created_by` stores the OID and creation identity; a dropped role's files are deleted | "Who may do what", "The record" | 14a |
| DXR-24 master key and segment details | low | `O_EXCL` creation, mode check, `transfer_key_lost`, no silent new key; chunks aligned to 64 KiB; offset from the file; completion re-reads for the SHA-256 | "Encryption at rest", "Uploads" | 14a |
| DXR-25 CSV details | low | `quote` limited with client-only encodings; `HEADER MATCH` where the router accepts it (S2), else the one-record exception stated; BOM at the start of the decompressed stream only; `columns` default to non-generated; local RLS refused up front; a reference import is atomic or not offered | "Table import" | 14a |
| DXR-26 spreadsheet guard | low | Pattern extended (`\|`, `%`, full-width, no-break space); the dialog says it is **not a security feature** | "Spreadsheet safety" | 14a |
| DXR-27 checksum entered by the user | low | `sha256` on upload is for command-line clients; the console shows the export's SHA-256 and the import dialog takes `expectedSha256` | "Uploads", "Known limits" | 14a, 14c |


### Changes made after the independent review of 14a (2026-11-22)

The review is `docs/reviews/2026-11-22-data-export-import-14a.md`; its section "Fixes" says what was done for each finding. Where the text above and this
section differ, this section is right.

- **The outcome of an import is decided by a marker (DX14-01).** The table `kingres_transfer.import_marker (task_id bigint primary key, state, rows, at)` is made in the
  database of the target by the service role, and belongs to it; no privilege is granted to the importing role (so dropping the role is not blocked), which writes its row
  through the security definer function `kingres_transfer.mark(task_id, rows)` inside the transaction of the load. The row commits with the rows (inside the same
  two-phase commit for a routed table) or not at all. A run that finds the intent asks `INSERT ... 'aborted' ON CONFLICT DO NOTHING` and reads the row back: a row
  `committed` is the proof that the rows are in; a row `aborted` that was inserted makes any later commit of that task fail on the key, so it is the proof that nothing was
  stored (the insert waits up to 60 seconds for a transaction that holds the key). A statement of the text above that is not true as built: the marker table is **local**
  to the database of the server that loaded, not a reference table; after a change of leader the marker cannot be read from the new leader, and the task fails
  `import_outcome_unknown` (the case the design left for "unknown until observed"). The reference-table form belongs to the batch import of 14c.
- **The marker is hardened (DX15-01 to 03).** It is keyed by a random 128-bit nonce kept in the task parameters (not the task id, which a restored or cloned database could repeat),
  old rows are deleted after 30 days, and the write goes through `kingres_transfer.mark(nonce, task, rows, tag)`: the function inserts only when `tag = sha256(secret || nonce:task:rows)`,
  the secret being a service-only table of the database that the control plane reads and uses. A role therefore cannot forge, block or hold a marker. The schema, the tables and the
  function are made by the service role only and checked on every use (owner, no trigger, rule, policy, other column, default or function, the exact body of the function), under an
  advisory lock; anything else fails the import closed (`forbidden`), so a database owner cannot plant the schema and have the service role run it.
- **One step loads and commits (DX14-07).** *Load rows* does `BEGIN`, the load, the checks, the marker, the intent and `COMMIT` and ends with the commit done or an error; the
  open transaction is never kept between steps, so a stand-down cannot leave one without an owner (the connection is closed by the step, and the server ends an idle
  transaction after the time that is left of the task). *Commit* is now *Confirm the commit*: it reads the marker, records the count and writes the audit event.
- **The staged files are adopted at the first use (DX14-02)**, so the sum that the quota checks includes the files that were there when the process started.
- **Roles (DX14-03).** An import is refused at the request (403) and in the step for a role with SUPERUSER, REPLICATION or BYPASSRLS or access to server files, programs or
  logs. The owner test of the export compares the owner of the table with the role (`relowner = the oid of current_user`), not `pg_has_role`, which is true for a superuser.
- **Spreadsheet guard (DX14-04, DX14-11).** The header cells are cells: a risky column name is given an apostrophe in front through the alias of the column (up to 63 bytes),
  and `xml` columns and domains over `xml` are text for the guard.
- **Checks repeated (DX14-05, DX14-09).** The export repeats the checks of the request (the table, the columns, the untrusted code) in its first step; the import compares the
  types of the columns too (for a reference table they build the typed-array cast).
- **Smaller points (DX14-06, 08, 10, 12).** A poll of an upload on a server that is not the one that holds it no longer marks it lost. The cap on a plain file is applied
  after reading the first two bytes of the staged file (a gzip file is not capped by its compressed size). `skipBom` is for UTF-8 only; for another encoding it is refused
  (400) when named and ignored by default. One transfer per role is race free (a lock from the check to the start of the task); a refused request leaves no record; the
  first step of a task reads `dataTransfer.enabled` again (a task that is moving data is cancelled with `cancelTask`).

## Results of the spikes and changes made by the code (phase 14b)

This section is written by the writer of 14b. The text above is left as the design was reviewed; where the two disagree, this section is right. The contract revision
of 14b is **0.29.0** in the integration branch g2 (the sections above name it 0.20.0); the migration is `0043_archive_validation.sql` (0050 on the branch).

### Spike S3: `pg_dump` for every table kind (2026-11-23)

**Result: passed, with the rewrites that the design expected and one it did not (the user's partitioning of a distributed table is synthesised).** The test is
`TestSpikeS3DumpOfEveryTableKind` (`cmd/kingresd/spike_s3_e2e_test.go`): the database of the export fixture (`buildExportFixture`) is built through the API on a real cluster,
and `pg_dump` of the build of this tree is run as the service role with the arguments of the exporter (`--schema-only --no-owner --no-tablespaces --no-publications
--no-subscriptions --no-security-labels --no-privileges`, the two sections, `--exclude-schema=kingres`, `--exclude-schema=kingres_transfer`). It was run on PostgreSQL **18.7 and
19.0**; **the two outputs are identical except for the two version comments and the `\restrict` token** (the golden files are in `internal/dumpnorm/testdata/pg18` and `pg19`,
and a test says that the normalised schema is the same on both). The cluster test of 14b (`TestDatabaseExportOverThreeServers`) repeats the check on three servers, where the
coordinator holds only some of the buckets and the dump holds only those. What the dump looks like for each kind (the answers to V1 and V2):

| Kind | What `pg_dump` writes | What the normaliser does |
|---|---|---|
| Preamble | `\restrict <token>` and `\unrestrict <token>` on both majors (V2: yes); eleven `SET` lines and `SELECT pg_catalog.set_config('search_path', '', false)` | removed (a closed list of `SET` names; any other `SET` is a refusal) |
| Extensions | `CREATE EXTENSION IF NOT EXISTS <name> WITH SCHEMA <schema>` and `COMMENT ON EXTENSION` for each, `kingres_shard` in `pg_catalog` | removed; the extensions and their schemas are in the manifest, from the catalog |
| Hash-distributed table | the parent `CREATE TABLE ... PARTITION BY HASH (<column>)`, then every bucket partition `<table>_bNNNN` as a table of its own with `NOT NULL` constraints named `<table>_<column>_not_null`, and `ALTER TABLE ONLY parent ATTACH PARTITION bucket FOR VALUES WITH (modulus N, remainder R)` | the bucket partitions and their attaches are removed (the set is **read from the catalog as the descendants of every hash table**, so it is exact on a coordinator that holds some of the buckets); `PARTITION BY HASH (<column>)` is removed only when the column is the catalog's |
| User-partitioned distributed table | the parent is `PARTITION BY HASH`; each bucket partition is itself `PARTITION BY RANGE (<key>)`; the user's leaves exist only as `<partition>_bNNNN` under the bucket partitions, with the bound written with explicit time-zone offsets, and `DEFAULT` | the user's partitioning is **synthesised from the catalog**: the parent gets `PARTITION BY <method> (<key>)` (from `pg_get_partkeydef` of a bucket partition) and one `CREATE TABLE <leaf> PARTITION OF <parent> FOR VALUES <bound>` or `DEFAULT` per user partition, the bound taken from the leaf `<partition>_bNNNN` of the first bucket the coordinator holds (`pg_get_expr`), so that a time zone is exact (the text in `kingres.dist_partition.bound` is what the user typed) |
| Constraints and indexes of a distributed table | `ALTER TABLE ONLY parent ADD CONSTRAINT`, `CREATE INDEX ... ON ONLY parent`, the same for every bucket partition with names `<bucket>_pkey` and `<bucket>_<column>_idx`, `ALTER INDEX parent ATTACH PARTITION child` | `ONLY` is removed for a distributed parent; everything about a bucket partition, and every attach of one of their indexes (names **read from the catalog**), is removed |
| Reference table | an ordinary table | kept |
| Columnar table | `SET default_table_access_method = columnar;` before its `CREATE TABLE` (and `= heap` after it; the setting stays on over the partitioned parent that follows) | `USING columnar` is added to the `CREATE TABLE` of a table that is not partitioned while the setting is on; the options are in the catalog document (`kingres.columnar_options`) |
| Updatable and append-only ledger table | the table, the history table `<table>_ledger_history` as an ordinary table, and four trigger names `kingres_ledger_block_insert`, `_block_modify`, `_block_truncate`, `_capture` with `ALTER TABLE ... ENABLE ALWAYS TRIGGER` | the history table (with everything about it) and the triggers of those names on the ledger tables are removed; **a trigger of that name on another table, and every other trigger, is kept and refused by the grammar** |
| Time-partitioned table | an ordinary partitioned table (the policy is in `kingres.timepart_policy_data`, not in the dump) | kept; the policy is in the catalog document and is imported disabled |
| Identity column | `ALTER TABLE ... ALTER COLUMN ... ADD GENERATED ALWAYS AS IDENTITY (SEQUENCE NAME ...)` after the table | kept |
| Local partitioned table | the leaves as tables of their own and `ALTER TABLE ONLY parent ATTACH PARTITION leaf FOR VALUES ...` | kept; the leaves are `leaf` in the catalog and have no data entry |
| Inheritance | `INHERITS (parent)` | kept; the shape grammar accepts it for the tables that the catalog names |
| Views and materialized views | in pre-data, in dependency order among the tables; `CREATE MATERIALIZED VIEW ... WITH NO DATA`; comments on their columns and indexes of a materialized view are in post-data | moved, with their comments and indexes, to `schema/views.sql`, in the order of the dump |
| Sequences | `CREATE SEQUENCE` and `ALTER SEQUENCE ... OWNED BY`, or the identity form above | kept; the state of each is in the catalog document |

What the dump does not show for the tables of the extensions, and is therefore in the catalog document: the access method of a distributed table, the colocation group, the
bucket count, the ledger mode, head and policy, the time-partition policy. **What the importer would refuse** (functions, triggers other than the ledger's, policies, rules,
event triggers, collations, extended statistics, foreign objects, unlogged tables, row-level security) is found twice: by queries on the catalog **at the request** (422
`schema_unrecognised`, naming the kinds), and by the shape grammar on the normalised files in the step *Normalise the schema* (the task code `schema_unrecognised`). V8 (the
rename of a database) belongs to 14c and was not checked. V9, for what 14b does: a read reaches only the expressions of virtual generated columns (checked as in 14a), because
every table with a policy is refused.

### Changes made by the code

- **Contract.** `TaskSubject` has the type `upload` (for `archive.validate`); `export_not_readable` names its objects in the problem's `reasons` (code `no_usage` or
  `no_privilege`, `object` the name, at most 20) and in the detail after a colon; `DataExport.warnings` is new and optional; `DataUpload.state` is `valid` or `invalid` after a
  validation and `report` holds the `ArchiveReport`. An archive that is not valid is a **finished** task whose report says `valid: false` with reasons, not a failed task.
- **Archive writer and validator (package `archive`)** are stricter than the text above in four ways: the end record has no comment and is the last 22 bytes (a comment can
  hide bytes); the flags of every entry are exactly 0x0008 and a descriptor is always present (the writer's shape is the only accepted one); the entries are in a fixed physical
  order (catalog, the three SQL files, the data, the manifest) and the central directory has the same order; an extension outside the closed list of the import (plus
  `plpgsql`) is `archive_invalid`. The catalog document gained `columns` of each table (the header of its data entry) and `historyColumns` of a ledger table; the manifest
  gained `options.excludedTables`, so that "the data entries are the tables that have data" can be decided from the file alone.
- **The progress of the data step is the task's `detail`** (`schema.table (n of m), N bytes so far`); the task engine has no item counters today, so `itemsDone` is not set. A
  task that is done has the detail `transfer <id>`.
- **A failed read restarts the whole data step, not one table** (up to 3 times, as in 14a): the archive writer is sequential and cannot rewind an entry.
- **Single snapshot** is decided by the catalog: when no hash table has buckets in more than one group, every table is read in one `REPEATABLE READ READ ONLY` transaction of the
  role and labelled `archive-single-snapshot`, and the independent count of every table is made in the same transaction (a difference fails the task `row_count_mismatch`).
  Otherwise each table is one `READ COMMITTED` statement with the label of 14a, and the counts are taken at the end and are a **warning** only.
- **A table with row-level security is refused** at the request (`schema_unrecognised`, "row-level security"): policies are not carried, and an archive of such a table would be
  shorter than the table or refused at import.
- **The history of an updatable ledger table** is a data entry with `ledgerHistory: true` whose `table` is the ledger table; it needs `SELECT` on the history table like any table.
- **Table tasks wait for an export:** `StartTask` refuses a table task (`table.distribute`, `.undistribute`, `.alter`, `.partition`, `.setStorage`, `.import`, `partition.maintain`)
  of a database in which a `database.export` is unfinished, and the export refuses to start while one of those is (one query each, 409 `operation_in_progress`).
- **Not covered by tests:** a leader change at every step of the export (the codes come from the helpers of 14a); a database with distributed schemas (`kingres.dist_schema`) is
  read into `distributedSchemas` but no test builds one; a columnar **distributed** table; an upload of an archive above 2 GiB.
- **Not in 14b, by the design's own scope:** the import, `expectedSha256`, the private database; `ArchiveReport.statements` is filled by the shape grammar already, so the
  validation says which statements a 14c importer would refuse.

### Changes made after the security review of 14b (2026-11-23)

- **The service role reads and calls nothing in the schema `kingres` before `extguard` has shown that it is an extension's** (A1). Every read of the catalog of the shard, ledger, timepart and
  columnar extensions, and the call of `kingres.columnar_options`, are gated by `extguard.Guard` for the extension that owns the object; an untrusted schema is 422
  `schema_unrecognised` at the request and the task code `schema_unrecognised` in a step. A database whose schema `kingres` is not owned by a superuser, or that holds objects when no
  extension of KingresDB is installed, and a schema `kingres_transfer` that the service role does not own, are refused the same way (B7). `readDBFacts` runs `SET LOCAL search_path =
  pg_catalog`, so a `search_path` set with `ALTER DATABASE` finds nothing of the tenant's; `pg_dump` gets `PGOPTIONS=-c search_path=pg_catalog`, its password in a file of mode 0600 in the
  staging directory (removed afterwards), a version check (the major of the server, at least 18) and a bounded tail of its error output in the log of the server only (A2, B8). The role's
  connection of an export sets `search_path = pg_catalog` (B9).
- **The lexers agree with PostgreSQL** (B1, B2): a `--` comment ends at a carriage return as well as at a line feed, and a dollar-quote tag may start with a byte above 0x7f, in `sqlshape` and
  in `dumpnorm`. The corpus `internal/sqlshape/testdata/lexcorpus.json` (comments that end at `\r`, tags of high bytes, nested comments, `E''`, `U&''`, dollar quotes in strings and comments)
  holds only harmless `SELECT` scripts; `TestLexersAgreeWithTheServer` sends each to a real server (PostgreSQL 18 and 19) with the simple protocol and counts the results, and the two lexers and
  the corpus must give the same count. The corpus is also the seed of the fuzz targets.
- **A validation is made from a whole read of the bytes or not at all** (B3): a failure that is not a verdict about the archive (the key, the file, a cancel, a read error) fails the task and
  nothing is cached. The cached report carries `ValidatorVersion` and the SHA-256 of the file; a report of another version is stale (the upload is `complete` again). **The cache is per upload
  row and tells what a validation found once. The import of 14c validates again at import time, whatever `archive_state` says, and never trusts it** (B4). The report says that the row counts
  are the manifest's claims (P1). A validation is limited to `min(maxImportBytes, 200 times the staged file)`, an entry to 500 times its compressed size plus 1 MiB (B5).
- **Sequences** (B6): the role needs `SELECT` or `USAGE` on every sequence of the database, or the export is refused with `export_not_readable`.
- **Names are pairs** (B10): a schema or a table with a dot in its name is refused at the export and by the validator; the console keys the tables to leave out as the pair. The number of data
  entries is checked at the request (at most 19 992).
- **The check for an export and the start of a table task are one step** (P2): `StartTask` for a table kind and the start of `database.export` take `dbTaskMu` around check and submit.

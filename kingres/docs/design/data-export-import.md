<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Data export and import from the console: design

Status: proposed, 2026-11-22; not implemented. The phase number (14) is a proposal; the integrator assigns it.
Builds on the control plane of phase 3 ([control-plane-3.md](control-plane-3.md)), the table lifecycle of phase 9
([shard-phase9.md](shard-phase9.md)), the configuration of phase 13 ([config-phase13.md](config-phase13.md)) and the
row router of phase 2b ([shard-phase2b.md](shard-phase2b.md)). It revises the REST contract to **0.19.0**
(additive: no operation, field or enum value of 0.14.0 changes meaning; the integrator renumbers). Decisions that
deserve a record are in [ADR 0027](../adr/0027-transfers-are-staged-encrypted-files-run-as-the-requesting-role.md) and
[ADR 0028](../adr/0028-ledger-tables-are-carried-as-data-and-restart-their-chain.md).

The project owner asked for data export and import in the admin console. This note fixes what that means for a
sharded database: four features (a table to CSV, a CSV into a table, a database to one archive, an archive into a
new database), and everything around them that makes them safe to ship: who may use them, what is recorded, what
they cost, where files live, and what a hostile file can and cannot do.

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
2. **Import a CSV into an existing table** through routed `COPY`, with the options of CSV (header, delimiter, null
   string, quote, encoding), a choice between all-or-nothing and batches, progress, cancel and bounded memory.
3. **Export one database to one archive**: the schema, the KingresDB catalog (distribution, colocation, reference
   tables, access methods, time-partition policies, ledger tables, sequences), the data of every table read through
   the coordinator, and a manifest with versions and checksums.
4. **Import such an archive into a new database**: validate the archive first, recreate the extensions and the
   structure, recreate the distribution **before** the data is loaded, load the data, finish the constraints, and
   refuse archives of incompatible versions.
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

## Decisions in one table

| # | Decision | Why |
|---|---|---|
| 1 | Every export is **staged** on the leader (encrypted, with a retention time) and then downloaded; the task is not the HTTP response. The download streams from the staged file | A COPY that fails with `KS001` after the first byte cannot be retried when the bytes are already in the browser; the audit event should carry the final size and checksum before anyone has the data; a `WriteTimeout` of 60 s and a 32 MiB buffered forward (F1, F2) do not allow a response that is the task; the browser can resume a staged download. Cost: disk on the leader (bounded, see "Where files live") |
| 2 | Export and import need `kingres_admin`, **and** the setting `dataTransfer.enabled` (off by default), **and** the PostgreSQL privileges of the signed-in role; a staged file can be read and downloaded only by the role that made it | The ladder has no separate permission and adding a fifth cluster role changes every client of the ladder. Data runs as the requesting role over `Proxy.Connect`, so a role gets only what its privileges give; the creator-only rule stops a second administrator from taking data the first one could read and the second could not |
| 3 | The archive is a **zip** file with fixed entry names, one CSV per table, a JSON catalog, the schema as two SQL files and a manifest with SHA-256 sums | The central directory gives a table of contents before anything is read, which is what "validated before anything runs" needs; streaming writers can add the manifest last; the standard library reads and writes it |
| 4 | The SQL of the archive is **never run as it is**. It is lexed into statements, each statement is classified against an allowlist, the plan is checked in full, and only then is it executed, one statement at a time, **as the requesting role** (never as the service role), on a connection that passed `refuseDangerousRole`. Distribution, ledger and time-partition policies are recreated from the structured catalog document with the service role, never from SQL in the archive | The dump is attacker-controlled text in the worst case. The allowlist narrows what a non-superuser can attempt; PostgreSQL's privileges bound what is left; the structured parts need superuser functions and so must come from validated data |
| 5 | **Ledger tables are exported and imported as data**; the imported table is a **new chain**: rows are loaded first, then `ledger_enable` enters them. The import needs an explicit acknowledgement, and the verifier says what it says about a new chain. The history of an updatable table is exported for the record and not loaded (ADR 0028) | There is no supported way to load an existing ledger (README of `kingres_ledger`). Refusing would make a database with one ledger table impossible to carry; skipping silently would hide that the proof of the past is not carried |
| 6 | **Distribute first, load second.** The importer creates the tables, calls the same functions an administrator calls to distribute them (while they are empty), then loads each table by routed `COPY` | The user asked for the distribution to be recreated before the data, and a load into an empty distributed table is the path the router is built for; a copy of rows into a table that is distributed later is the slower, blocking path of phase 9 |
| 7 | Data is exported as **CSV in UTF-8 with fixed session formats** (`DateStyle`, `IntervalStyle`, `bytea_output`, `extra_float_digits`, `TimeZone`, `lc_monetary`) and a distinct null marker | Text CSV is portable across PostgreSQL majors; the sessions fix what the router already fixes for transport (README, "The text of a statement sent to another node means the same there") |
| 8 | Table CSV export protects spreadsheets by default (`spreadsheetSafe`), the archive never changes a value | A file for a person opened in a spreadsheet must not run formulas; a file for a restore must be exact |
| 9 | A **leader change ends a transfer** with `transfer_leader_changed` (retryable); nothing is resumed on another server | Files and keys are on the leader's disk; the role connection exists only where the role signed in (F3). The partial results are cleaned up; a batch import reports exactly what was committed |
| 10 | Uploads are **resumable by offset**, in chunks of up to 256 MiB, streamed through a new relay to the leader | Large files must survive a dropped connection; the chunk size bounds the work lost; the relay is needed anyway for downloads (F1) |

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
| `created_by` | The role that made it; the only role that may read, download or use it |
| `database`, `schema_name`, `table_name` | What it is for or from (null for an upload not yet used) |
| `size_bytes`, `declared_bytes` | Size of the file, and for an upload the size the client declared |
| `sha256` | SHA-256 of the plaintext file, 64 hex characters |
| `format` | `csv`, `csv.gz`, `archive/1` |
| `report` | JSON: the result (rows, consistency, options used) or, for an upload, the archive report of a validation |
| `key_id`, `wrapped_key` | The key that encrypts the file (below); the wrapped data key, not the key itself |
| `created_at`, `expires_at`, `downloaded_at` | `expires_at` is the retention time; `downloaded_at` is set when the last byte was served |
| `task_id` | The task that builds or uses it |

The file is the truth about the bytes, the row is the truth about who may have them and for how long. A file
without a row is deleted by the janitor; a row whose file is missing is set to `lost`.

### Where files live

`<transfer-dir>`, default `<DataDir>/transfer`, mode 0700 (a flag, `transfer-dir`; it must be on a file system
with room, and an operator may put it on a volume of its own). Files are created with mode 0600, opened with
`O_NOFOLLOW`, checked to be regular files, and written under the name `transfer-<id>.part` and renamed to `.kdx`
only when complete. The table that an upload or export belongs to is never used in a path.

**Disk exhaustion** is a design input, not an afterthought; four limits apply and each has a code:

1. **Per file**: `maxFileBytes` (default 20 GiB). An export that grows beyond it stops with `export_too_large`; an
   upload that declares more is refused when it is created (422, `validation_failed` on `/sizeBytes`).
2. **Quota of the staging area**: `stagingQuotaBytes` (default the smaller of 100 GiB and 25 percent of the file
   system). An upload reserves its declared size when it is created; an export reserves its estimate (the
   `pg_total_relation_size` of the tables, times 1 for plain CSV, which is an overestimate for compressed output).
   A reservation that does not fit is refused with 409 `transfer_quota_exceeded` and says how much is free and
   which transfers hold the space (the caller's own, by id; other roles' by count only).
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
`kingresd` keeps in its own directory.

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
| `exportDatabase` | `kingres_admin` | `SELECT` on **every** table and `USAGE` on every schema of the database, checked before anything is written; the first 20 objects the role cannot read are named in a 422 `export_not_readable` |
| `importDatabase`, `validateDatabaseArchive` | `kingres_admin` | the role creates the objects in the new database as their owner; it must be able to log in and must not be dangerous (below) |
| `createDataUpload`, `uploadDataChunk`, `getDataUpload`, `deleteDataUpload` | `kingres_admin` | none; creator only |
| `listDataExports`, `getDataExport`, `downloadDataExport` | `kingres_admin` | creator only; another role sees 404, so the existence of a file is not leaked |
| `deleteDataExport` | `kingres_admin` | the creator, **or any `kingres_admin`** (to free the disk of a colleague who left; deleting reveals nothing) |
| `cancelTask` of a transfer | `kingres_ops` (the existing operation) | stopping a transfer is not a leak; an operator may stop an administrator's import |
| `getClusterSettings` (field `dataTransfer`) | `kingres_readonly` | existing; `updateClusterSettings` needs `kingres_admin`, as today |

**Why not a separate permission.** The roles are four fixed levels (`auth/roles.go`) and every operation of the
contract names one of them; a role that may export but not administer would need a new level in the ladder, in the
contract's `x-required-role`, in the generated types of the console, and in `setRoleClusterRoles`. The three guards
above do the same work with what exists: the setting is an explicit decision of an administrator, the privileges
bound the data, and the creator-only rule stops laundering (ADR 0027, alternatives).

**The setting.** `ClusterSettings.dataTransfer` (additive): `enabled` (default `false`), `retentionHours`,
`maxFileBytes`, `stagingQuotaBytes`, `maxConcurrent` (default 2 transfers running in the cluster, 1 per role). While
`enabled` is false every transfer operation answers 403 `data_transfer_disabled`, the console shows the actions
disabled with the reason, and turning it on is an audited `updateClusterSettings`.

**Dangerous roles.** `importDatabase` runs the statements of the archive as the signed-in role, so it applies the
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
| `importTable` (request) | table, options, mode, upload id and SHA-256, task id |
| `dataTransfer.completed` (import) | rows committed, batches, rows skipped by `skipRows`, outcome |
| `validateDatabaseArchive`, `importDatabase` | archive SHA-256, manifest versions, counts of tables by kind, the options `privileges` and `allowCode` as chosen, for a refused statement its **kind and fingerprint** (length and hash prefix, as `StatementFingerprint` does for the workbench) and its position, never its text |
| `updateClusterSettings` | the old and new `dataTransfer` values, as for any setting |

An upload chunk is not an event (an upload of 20 GiB is 80 chunks at the default size); the upload's creation, the
completion check and the use of it are. The audit sink being unready refuses a transfer before it starts (`auditReady`,
as every audited operation), so no transfer happens that cannot be recorded.

## Limits and timeouts

All values are defaults; the ones that are cluster settings are marked, the others are flags of `kingresd`.

| Limit | Default | Where it applies, and what happens at the limit |
|---|---|---|
| `maxFileBytes` (setting) | 20 GiB | Any staged file. Upload refused when created; export stops with `export_too_large` |
| `stagingQuotaBytes` (setting) | min(100 GiB, 25% of the file system) | Sum of reservations, 409 `transfer_quota_exceeded` |
| Free-space reserve (flag) | max(5 GiB, 10%) | 409/failed `insufficient_disk` |
| `retentionHours` (setting) | 24 (1 to 168) | Export files; uploads: 48 h without data, 24 h unused |
| `maxConcurrent` (setting) | 2 per cluster, 1 per role | 409 `operation_in_progress` with `Retry-After` |
| Upload chunk | at most 256 MiB, 64 MiB offered | 413 `chunk_too_large`; the client may send smaller chunks |
| Idle time of a stream | 60 s without a byte (read or write) | The deadline moves with every read and write (`http.ResponseController`); the request ends, the offset stays |
| Duration of one chunk | 15 min | Then the request ends; the client resumes at the offset |
| `RequestTimeout` (30 s), `ReadTimeout` (30 s), `WriteTimeout` (60 s), `MaxBodyBytes` (1 MiB) | unchanged | Do **not** apply to the two streaming operations (F2); they apply to everything else, including the JSON requests of this feature |
| Duration of a transfer task | 6 h per table and 24 h per task (flag `transfer-max-duration`) | The `COPY` carries a `statement_timeout` of the time left; the task fails `transfer_timeout` |
| Record (one CSV row) | 16 MiB | The scanner (below) fails the import with the row number; the workbench's row limit is the same |
| Rows per batch | 50 000, range 1 to 1 000 000, and 64 MiB | Batch mode |
| `maxAtomicBytes` (flag) | 10 GiB | All-or-nothing import refuses a larger file with 422 and says to use batches |
| Archive: entries | 20 000 (10 000 tables) | `archive_invalid` |
| Archive: manifest, catalog, each SQL file | 4 MiB, 16 MiB, 64 MiB | `archive_invalid` |
| Archive: statements, one statement | 200 000, 1 MiB | `archive_statement_refused` |
| Archive: expansion | declared sizes must equal the manifest, the sum must not exceed `maxFileBytes` times 20, one entry may not expand more than 1000 to 1 | `archive_invalid` |
| Workbench-style concurrency | not shared | The transfers have their own limit; they do not use the 2 per role of the workbench |

## Table export

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
   transfer setting, quota and free space (reserves the estimate).
2. *Read the table class*: from the catalog (`kingres.dist_table`, `colocation`, `bucket_map`, `dist_partition`)
   through the service role: class, group count that holds buckets, the `consistency` label (below).
3. *Write the file*: one `COPY (SELECT) TO STDOUT` on a connection of the role (F3), in a read-only `READ COMMITTED`
   transaction, session formats fixed (below), bytes written through the sealing writer (gzip first when asked), the
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
`SELECT` list, in SQL, so that the control plane never parses the CSV it exports: every column whose type is in the
string category (`text`, `varchar`, `bpchar`, `name`, `citext`, a domain over them) or an enum is written as

```
CASE WHEN col ~ '^[\t\r ]*[=+@-]|^[\t\r]' THEN '''' || col ELSE col END
```

so a risky cell is prefixed with an apostrophe, which spreadsheets show as text. Numeric, date and boolean columns
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

## Table import

### Upload, then import

A CSV reaches the server as an **upload** (resumable, below) and is then used by an import. The two are separate
because a 10 GiB file may take an hour to send and the import may be repeated with other options without sending it
again.

`POST /databases/{databaseName}/schemas/{schemaName}/tables/{tableName}/import` (`importTable`, `kingres_admin`), body
`ImportTableRequest`, answer 202 `AcceptedTask` (kind `table.import`, subject `database/schema.table`):

| Field | Default | Meaning |
|---|---|---|
| `uploadId` | required | A complete upload of the caller |
| `header` | `skip` | `none` (no header), `skip` (a header, ignored), `match` (a header whose names must equal `columns`, or the table's columns in order, **checked by the control plane before anything is written**, 422 `import_header_mismatch` with the first difference) |
| `columns` | the table's columns in table order | The columns, in the file's order |
| `delimiter`, `quote`, `nullString`, `encoding` | as for export | `nullString` and `quote` as in export; `forceNotNull`, `forceNull`: lists of columns |
| `skipBom` | `true` | A UTF-8 byte order mark at the start is removed (spreadsheets write one) |
| `compression` | detected from the upload's format | `none` or `gzip` (the expansion is counted, see "Hostile input") |
| `onError` | `allOrNothing` | `allOrNothing` or `batches` |
| `batchRows`, `batchBytes` | 50 000, 64 MiB | Batch mode only |
| `skipRows` | 0 | Data rows to skip at the start (to continue after a failed batch import) |
| `keepUpload` | `false` | Keep the upload after success (until retention) |

Targets: distributed tables (partitioned or not), local tables (also partitioned and columnar), ledger tables
(local; the capture trigger runs; a table with policy `required` is refused up front, 422 `import_ledger_policy`),
and **reference tables** through the batch path of "Reference tables" below. Refused: views, foreign tables, bucket
partitions, `kingres` and system schemas, a table with an unfinished table task (409), and a distributed table
with row-level security (the router refuses; the message is passed on).

### The pipeline, and why memory is bounded

```
staged file (sealed segments)
  -> segment reader (64 KiB)
  -> [gunzip with a count of bytes out and a ratio cap]
  -> [BOM strip, encoding check is left to PostgreSQL]
  -> record scanner (finds record ends and counts rows; copies bytes through)
  -> bounded reader (1 MiB read-ahead) -> PgConn().CopyFrom(COPY ... FROM STDIN)
```

The control plane **never parses values**. PostgreSQL parses the CSV; the scanner only knows where a record ends
(it tracks the quote state, so a newline inside quotes does not end a record), counts records and bytes, enforces the
16 MiB record limit, and makes it possible to cut a batch on a record boundary and to continue after `skipRows`. It
holds one record, and the read-ahead is fixed, so **memory does not depend on the file's size**: a 20 GiB file
takes the same few megabytes as a 20 MiB one. The data goes in with `COPY ... FROM STDIN` only. **No file name or
program ever reaches PostgreSQL**, so nothing needs `pg_read_server_files` or `pg_execute_server_program`, and the
statement is built by the control plane from names that passed `sqlx.NewIdent` and from option values that passed
the checks above; a value of an option never reaches the statement as a string of SQL (the delimiter, quote and
null string are checked and quoted as PostgreSQL literals by one function, with a differential test against
PostgreSQL's own parser).

A line that is only `\.` ends a `COPY FROM STDIN` in PostgreSQL's CSV mode and the scanner does not know that; the
**row count of the scanner is compared with the count in the command tag of every batch**, and a difference fails
the task (`row_count_mismatch`) instead of silently dropping the rest of the file. That check also catches a
difference between the scanner's and the server's idea of a record for any other reason.

### All or nothing and batches

| | `allOrNothing` | `batches` |
|---|---|---|
| Statements | one `COPY` in one transaction (two-phase commit across the groups) | one `COPY` per batch, each its own transaction |
| On an error in the data | nothing is stored; PostgreSQL's message with the line and column, and the line is **absolute** (the header and `skipRows` are added) | the batches before stay; the failed batch is rolled back whole; the task reports `rowsCommitted` and the line range of the failed batch |
| Locks and bloat | row locks and the work of the whole load are held until the end; a failure at the end wastes everything and leaves dead tuples | short transactions; a failure wastes one batch |
| Constraints | unique and foreign keys are checked inside; a violation fails the load | the same, per batch; a batch that violates fails |
| Limits | file up to `maxAtomicBytes` (10 GiB) | any size up to `maxFileBytes` |
| Retry | repeat the import | continue with `skipRows = rowsCommitted` (the console offers it); safe when the table has a unique key, **not** when it has none (a repeated batch is duplicated) |
| Recommended | files up to about 1 GiB | larger files; tables that take other writers |

The console preselects `allOrNothing` below 1 GiB and `batches` above it, and says what each means in the words of
the table. The API default is `allOrNothing`, the conservative one.

**A batch whose outcome is unknown.** If the connection to the coordinator is lost *during the commit* of a batch,
the outcome of that batch is unknown (it may have committed). The step does **not** retry it: the task fails with
`batch_outcome_unknown`, `retryable: false`, and states the row range that is uncertain. For a table with a unique
key the user may continue from the start of that range and read the error. A batch that failed **before** the
commit (an error from `COPY`, a `KS001`, a cancel) is known to have stored nothing and is retried at most 3 times
for `KS001` and connection errors that happen before the commit message, 5 s apart.

### Progress, cancel, timeouts

Steps of `table.import`: *Check the upload* (complete, the caller's, SHA-256 matches the record, the format),
*Check the table* (class, privileges, header against `match`, table task running?, ledger policy), *Load rows*
(`itemsDone` = bytes of the file consumed by `COPY`, `itemsTotal` = file size; `detail` = "batch 12 (rows 550001 to
600000), 600000 rows committed" in batch mode, "row 1 200 000 of about ..." never; **pg_stat_progress_copy is not
updated by routed COPY (F7), so the count is the control plane's own**), *Verify* (the rows the scanner counted equal
the command tags, summed), *Clean up* (delete the upload unless `keepUpload`). The step *Load rows* is
`Interruptible`; cancel cancels the backend (the router ends the statement at once, README "Waits") and, in batch
mode, the batch in flight rolls back and the earlier ones stay: the task ends `failed`, `cancelled`, with
`rowsCommitted` in the failure's message. `Task.cancellable` is true until the commit of an `allOrNothing` load starts.
A failed import never deletes rows that were committed.

### Reference tables

`COPY FROM` into a reference table is refused on a cluster with several groups (F8). The control plane therefore
loads a reference table in two parts: it `COPY`s the file into a **temporary table with the same columns** on the
coordinator's connection (so PostgreSQL still does all the CSV parsing, with the same options), and then reads that
table in batches of `batchRows` as text (`SELECT col::text, ...`) and writes each batch with
`INSERT INTO ref (cols) SELECT u.c1::type1, ... FROM unnest($1::text[], ...) AS u(c1, ...)`, the form the router
itself uses. The temporary table lives in the connection's session, holds the file's rows on the coordinator's
disk for the duration (the free-space check counts it), and is dropped at the end. **[Needs verification] V3**: if
the reference target does not accept the `unnest` form, the batch is a multi-row `INSERT ... VALUES` with typed
parameters, which costs more round trips but needs no new capability. Reference imports are always `batches` (the
write is run on every node, so a very large atomic load is the worst case) and the console says so.

## Uploads: resumable, with limits

| Operation | Method and path | Notes |
|---|---|---|
| `createDataUpload` | `POST /data-uploads` | Body `{purpose, sizeBytes, sha256?, fileName?}`: `purpose` is `tableImport` or `databaseImport`; `fileName` is for display only and is never used in a path. The size must be at most `maxFileBytes`; the quota is reserved. 201 with the upload: `uploadId`, `chunkBytesMax`, `offset` 0, `expiresAt` |
| `getDataUpload` | `GET /data-uploads/{uploadId}` | State, `offset` (bytes received), `sizeBytes`, `sha256` once complete, and the `report` of a validation. This is how a client resumes after a break |
| `uploadDataChunk` | `PATCH /data-uploads/{uploadId}` | Header `Upload-Offset` (must equal the server's offset, else 409 `upload_offset_mismatch` and the header `Upload-Offset` has the right one), `Content-Type: application/octet-stream`, `Content-Length` of at most `chunkBytesMax`. Answer 204 with the new `Upload-Offset`. A chunk is **appended atomically**: a request that breaks off keeps the whole segments received and the offset says which; re-sending the rest is safe |
| `deleteDataUpload` | `DELETE /data-uploads/{uploadId}` | Frees the reservation; 409 while a task uses it |

When the last byte arrives the server compares the total with `sizeBytes` and the SHA-256 (if the client gave one)
with its own and sets the upload `complete`; a mismatch is 422 `upload_checksum_mismatch` and the upload is deleted. A
chunk that would go beyond `sizeBytes` is 413. The relay (below) streams the chunk body to the leader without
buffering it, and the leader seals it into the staged file as it arrives.

**Large uploads.** The chunk size is the unit of loss: a break loses at most one chunk, and the console sends
64 MiB chunks strictly in order with one request in flight, so the offset check is the only ordering rule. A 20 GiB file is 320 requests. No upload exceeds the 16 MiB limit of `forward` because it does not use
it (F1).

### The streaming relay

A follower must not buffer (F1). Two operations (`uploadDataChunk` and `downloadDataExport`)
are marked `x-streams: upload` and `x-streams: download` in the contract and are handled so:

1. The server that received the request runs the normal guard (session or token, CSRF for the cookie, role,
   rate limits, the TLS check, the transfer setting) and **extends the read and write deadlines** with
   `http.NewResponseController` for the stream, with an idle time of 60 s (F2).
2. If it is the leader it runs the handler. Otherwise it opens a request to the leader, `POST
   /internal/v1/forward-stream`, over the mutual-TLS listener: the first frame is the same `ForwardRequest` header
   (operation id, path parameters, query, identity and **proof**, without a body), length-prefixed and at most 64 KiB;
   what follows is the raw stream (the chunk for an upload) and the answer is a status frame and then the raw bytes,
   flushed as they come. The leader checks the proof exactly as for `forward` (CP2-01), and runs the operation
   through the same pipeline, so audit and role checks are the leader's.
3. Nothing is buffered beyond one 64 KiB segment on either server. The internal protocol version stays 1
   (a new call only, as phase 13 did); `internal-api.md` gets the section and a test that the call is member-only
   and answered by the leader only.

The staged file is on the leader, so the relay is what lets the console talk to any server, which is the shape of
every other operation (ADR 0014).

### Download

`GET /data-exports/{exportId}/content` (`downloadDataExport`): 200 or 206 (`Range: bytes=a-b` of the plaintext;
one range; the sealed segments make it a seek), `Content-Length`, `Accept-Ranges: bytes`, `ETag` (the SHA-256),
`Content-Type` `text/csv; charset=<encoding>` for a CSV, `application/zip` for an archive,
`Content-Disposition: attachment; filename="..."` with a name the **server builds** from a safe subset
(`[A-Za-z0-9._-]`, at most 80 characters, from database, table and the UTC time; never from a client string),
`Cache-Control: no-store`, `X-Content-Type-Options: nosniff`. The creator only (404 otherwise). The row's
`downloaded_at` is set when the last byte is served. A file that expired is 410 (the bundle's convention);
`deleteDataExport` removes it earlier. `downloadDataExport` is audited. The cookie is `SameSite=Strict`, so a page of
another site cannot start a download in the user's name (F17).

## Database export

### The task `database.export`

Subject: the database name, so it cannot run during `database.drop` or `database.update`. Steps, each recorded
`done` (a resumed task skips them) and idempotent (a restarted step rewrites its own part):

1. *Check the request*: setting, quota, space, `has_table_privilege` for **every** table and `USAGE` on every
   schema as the role (else 422 `export_not_readable` with the first 20 objects; nothing is written).
2. *Read the catalog* (service role): the **catalog document** (below), the extension list and versions, the table
   list with sizes. The tables to export are the user tables of non-system schemas, minus the `excludeTables` of
   the request, minus the tables that the extensions own.
3. *Dump the schema*: `pg_dump` of the **same PostgreSQL build as the instance** (taken from the instance's
   binary directory, never from `PATH`), as the role, over the unix socket that `Proxy.Connect` uses, with fixed
   arguments and **no value from the client in any of them**: `--schema-only --no-owner --no-tablespaces
   --no-publications --no-subscriptions --no-security-labels` (comments are kept), `--section`
   pre-data and post-data in two runs, `--exclude-schema` for `kingres` and the system schemas, `--exclude-table`
   for the bucket partitions and for the history tables of ledger tables (names from the catalog document), and
   `--no-privileges` unless `privileges: restore` was asked. The output is read through a pipe with a size cap
   (`64 MiB` per run); the subprocess has a timeout, a clean environment (`PGPASSFILE`, `PGSERVICE` unset) and no
   shell. A `pg_dump` error ends the task with its message (no statement text).
4. *Normalise the schema* (below): the output is turned into the **logical schema**, the one a user would have
   written, or the export fails with `schema_unrecognised` naming the statement's kind and position.
5. *Write the data*: for each table, in the order of the catalog (largest last), one `COPY (SELECT) TO STDOUT` as
   in table export, with the **archive's fixed options** (below), into its own entry of the zip, restartable on its
   own (`KS001` or a lost connection restarts that table only, 3 times). `itemsDone` counts tables, `detail` names the table
   and the bytes so far. A per-table `consistency` label is recorded.
6. *Write the manifest and seal*: SHA-256 and sizes of every entry, totals, versions, the labels, then the zip's
   central directory; the SHA-256 of the whole file is the one the row and the audit event carry.

A cancel ends the running `COPY` and the whole task `failed` with `cancelled` and no file. `OnFail` deletes the partial
file.

### The archive

A zip file (`.kdx.zip` for the browser's file name) with these entries and **no others** (the importer refuses an
unknown name):

```
manifest.json            the manifest (written last, first read)
catalog.json             the KingresDB catalog document
schema/pre-data.sql      normalised pg_dump --section=pre-data
schema/post-data.sql     normalised pg_dump --section=post-data
data/00001.csv ...       one entry per table, numbered in the order of the catalog document
```

Entries are compressed with deflate (`archive/zip`); the data entries are CSV in **UTF-8**, written with
`FORMAT csv, HEADER true, NULL '\N', FORCE_QUOTE *, ENCODING 'UTF8'` and the session formats of the table export,
so that an empty string (`""`) and a null (`\N`, unquoted) are different, and a text value `\N` is `"\N"`, which
the CSV reader of PostgreSQL does not take for the null marker. No value is changed.

**The manifest** (JSON, at most 4 MiB, strict decoding, unknown members refused):

```json
{
  "format": "kingres-export/1",
  "createdAt": "2026-11-22T09:30:00Z",
  "source": { "database": "shop", "kingresVersion": "...", "contractVersion": "0.19.0",
              "postgresMajor": 18, "groupCount": 3 },
  "extensions": [ { "name": "kingres_shard", "version": "0.7" }, { "name": "kingres_ledger", "version": "0.3" } ],
  "catalogVersion": 1,
  "entries": [ { "name": "catalog.json", "size": 41233, "sha256": "..." },
               { "name": "data/00001.csv", "size": 90211333, "sha256": "...", "table": "public.orders",
                 "rows": 1200000, "consistency": "per-group", "groups": 3 } ],
  "totals": { "tables": 14, "rows": 5400000, "uncompressedBytes": 411000000 },
  "options": { "privileges": "none", "dataMode": "all" },
  "warnings": [ "table public.orders was read per group, not as one snapshot" ]
}
```

`format` is `kingres-export/<major>`. The checksums find damage and truncation. They do **not** prove who made the
file: anyone who can change the archive can recompute them. The export's result and its audit event carry the
SHA-256 of the whole file, and `importDatabase` accepts `expectedSha256` so that an operator who got the value
from a second channel can bind the import to it.

### The catalog document

`catalog.json` is what the schema SQL does not say: how the tables are distributed. It is read by the control plane
from the extension's catalog (`kingres.dist_table`, `colocation`, `bucket_map`, `dist_partition`, the access
method of each table, `kingres.ledger_table`, `kingres.timepart_policy`, `pg_sequences`) with the service role and
is **structured data**, validated on import, never SQL:

| Part | Content |
|---|---|
| `colocationGroups` | `[{ref, bucketCount, distColumnType, collation}]`. The numeric ids are **not** exported: they are not portable. A table names its group by `ref` |
| `tables` | per table: schema, name, `kind` (`local`, `hash`, `reference`, `schema` when phase 11 exists), `distributionColumn`, `colocationRef`, `userPartitioning` (`method`, `key`, and the user partitions with their bounds from `dist_partition`), `accessMethod` (`heap` or `columnar`) and the `columnar` options that the extension reports, `ledger` (`mode`, `policy`) or null, estimated rows and size |
| `distributedSchemas` | present when `kingres.schemas` exists (extension 0.8): schema and its `colocation` position; absent otherwise. An importer that does not know `distributedSchemas` refuses an archive that has a non-empty one (`archive_incompatible`) |
| `timepartPolicies` | the columns of `kingres.timepart_policy` that `timepart_set_policy` takes (`partition_every`, `timezone`, `create_ahead`, `retention`, `retention_action`, `columnar_after`, `columnar_sort`, `columnar_options`, `run_every`, `enabled`); not `next_run_at`, not the history, not the OIDs |
| `sequences` | for each sequence owned by a column: schema, name, `ownedBy`, `lastValue`, `isCalled` of the coordinator's sequence (sequences are node-local, F18) |
| `referencedRoles` | the role names that the schema's owners and privileges mention (informational unless `privileges: restore`) |

**Placement is not exported.** Which group holds which bucket is a property of this cluster. The importer lets
the extension place the new colocation groups as it places any new group (round-robin over the groups that should
hold shards, README), and phase 10's rebalance moves buckets afterwards. The **bucket count and the colocation
relation** are exported, because tables that are colocated stay colocated and the bucket count decides how the
table is cut.

### Normalising the dump

F12: a dump of a node is the dump of its partitions, not of the table as the user wrote it. The control plane
therefore turns `pg_dump`'s output into the logical schema by a **normaliser that removes, and only removes**, a
closed set of constructs and fails on anything it does not recognise:

1. The bucket partitions `<table>_bNNNN` (exact names from the catalog, which are also `--exclude-table` of the
   dump), and their attach and owner statements.
2. The clause `PARTITION BY HASH (<dist column>)` at the end of the `CREATE TABLE` of a table that the catalog says is
   `hash`, **if and only if** the column in the clause equals the catalog's distribution column. For a
   user-partitioned table the same clause on each user partition, whose leaves are removed under rule 1 (their names
   are in `kingres.partitions`).
3. For a ledger table the capture triggers (their names and the function they execute are in `kingres.ledger_table`
   and in the extension) and its history table (rule 1's list), because the importer will call `ledger_enable`
   itself (ADR 0028).
4. `CREATE EXTENSION` statements (the importer creates the extensions from the manifest's list), and the
   `SET`/`\restrict` preamble (replaced by fixed settings).

A statement that is neither removed nor allowed by the classifier below, a `PARTITION BY HASH` whose column differs
from the catalog's, or a bucket partition that the catalog does not list, fails the **export** (`schema_unrecognised`).
It is a design rule that the exporter refuses rather than guesses: an archive that cannot be imported is found at
export time, by the same classifier the importer uses. The golden files of V1 (increment 6) are the test, on 18 and
19, and the round trip of "Tests" is the proof.

### Roles of the archive

Roles are cluster state, not database state, and are not in the archive. Owners and privileges are the part that
names them. `--no-owner` makes **every object owned by the importing role**; `privileges: none` (default) drops the
grants; with `privileges: restore` the `GRANT` and `REVOKE` statements are kept and the importer **requires every role
in `referencedRoles` to exist** (422 `archive_roles_missing` lists the missing ones), and a grant is allowed only to
a role that is not a predefined `pg_*` role. The importing role is the owner, so it can grant. Ownership by other
roles is not restored (a non-superuser cannot give objects away, and a superuser is refused).

## Database import

### Request

`POST /database-imports` (`importDatabase`, `kingres_admin`), body `ImportDatabaseRequest`, answer 202 `AcceptedTask`
(kind `database.import`, subject the new database's name). The first validation does not need the request:
`POST /data-uploads/{uploadId}/validation` (`validateDatabaseArchive`, kind `archive.validate`, subject
`upload:<id>`) is a task that does steps 1 to 3 below and stores the **archive report** in the upload's row so that
the console can show it before the user decides.

| Field | Default | Meaning |
|---|---|---|
| `uploadId` | required | A complete upload of the caller |
| `database` | the manifest's name | The new database; it must not exist (409 `already_exists`). Import **never** writes into an existing database |
| `expectedSha256` | none | Binds the import to a value the operator got elsewhere |
| `privileges` | `none` | `none` or `restore` (above) |
| `allowCode` | `false` | See "What may run". Needs the text `execute code from the archive` in `confirm` |
| `acknowledgeLedgerRestart` | `false` | **Required `true` when the archive has a ledger table** (ADR 0028) |
| `keepOnFailure` | `false` | Keep the new database if the import fails (to look at it); it is marked and listed as failed |
| `bucketCount` | the source's, per group | Not offered: the bucket counts of the archive are kept |

### The task `database.import`

The steps (a failed step ends the task; the `OnFail` hook drops the new database, below):

1. *Validate the archive*: the whole of "Hostile input", with no database touched (also what `archive.validate`
   runs). Reads the upload end to end once for the SHA-256 (the cost of reading the file; it is the price of
   "validated before anything runs").
2. *Check versions*: the compatibility rules below.
3. *Check the plan*: lex and classify **every statement** of both SQL files and check the catalog document against
   the tables of the schema and the data entries against the manifest; the result is a plan (counts by kind). No
   statement is run, no database exists, and a refusal here names the statement's kind, position and fingerprint.
4. *Create the database*: `database.create`'s own code (the service role, the settings of the archive's source where
   they exist: sharding on, ledger from the extension list), after checking the name is free; the database gets a
   comment `kingres-import:<task id>` that marks it as this task's. The new database has `kingres_shard` (and
   `kingres_ledger` if the archive lists it), because `DatabaseCreateRequest` creates those (F16).
5. *Install extensions*: the rest of the manifest's list (`kingres_columnar`, `kingres_timepart`, `kingres_sketch`, and a
   closed list of contrib extensions: `pgcrypto`, `uuid-ossp`, `citext`, `hstore`, `pg_trgm`, `btree_gin`,
   `btree_gist`, `ltree`, `unaccent`, `fuzzystrmatch`) with the service role, **at the archive's version or newer**;
   any other extension is a refusal at step 3, never an attempt.
6. *Run pre-data*: the statements, one by one, as the role (next section).
7. *Recreate the distribution* (service role, from the catalog document): create the colocation groups in order
   (the first table of a group creates it; the others colocate with it); `create_reference_table` for reference tables;
   `create_distributed_table` for hash tables (user-partitioned ones through the phase 9 call that accepts a
   partitioned table), all **while the tables are empty**, so every call is the cheap one. A call that the extension
   refuses ends the task with the extension's report (`table_not_eligible`, `blockers`), exactly as an administrator
   would see it.
8. *Load the data*: for each table, in the catalog's order, the table import pipeline above with fixed options
   (`header skip`, `nullString \N`, UTF-8, `onError batches` for tables over `maxAtomicBytes`, otherwise one
   `COPY`). `itemsDone` counts tables, `detail` names the table and its percentage by bytes. A ledger table is loaded
   before its ledger is enabled; generated columns are not loaded (they are not in the file's column list).
9. *Run post-data*: indexes, constraints and foreign keys, as the role. A **foreign key that fails because data was
   read at different instants** (no global snapshot) fails the task here with the constraint's name and a count of
   offending rows if PostgreSQL gives it; the database is dropped as for any failure.
10. *Sequences*: for each sequence of the catalog, on **every node** (the service role per server, as the fan-out
    tasks do), `setval(seq, greatest(lastValue, max(column)))` of the global maximum of the owning column, `is_called`
    true when rows exist (F18).
11. *Ledger and policies* (service role): `ledger_enable` for each ledger table with its mode (existing rows are
    entered, F14); `timepart_set_policy` for each policy through the function's own checks (so a policy that the extension
    refuses is a warning with the reason, not a failure, because retention is a property of the new place).
12. *Verify*: per table, the number of rows equals the manifest's, and `ledger_verify()` on a database with ledger
    tables returns no rows; a failure is reported with the table. The task `detail` says "verified rows" never "verified data".

The task is `Cancellable` until step 10 starts; a cancel before that drops the new database as a failure does.

### What may run: the statements of the archive

**Lexer.** A hand-written lexer, not regular expressions, that knows single-quoted strings (also `E''` and `U&''`),
dollar quotes (with tags), double-quoted identifiers, `--` and nested `/* */` comments, and splits at `;` outside
them. It reads **statement by statement** (memory is one statement, 1 MiB at most) and counts. Backslash lines are
refused (`archive_statement_refused`, kind `meta-command`) except exactly `\restrict <token>` and `\unrestrict <token>`
with a token of `[A-Za-z0-9]{1,128}`, which are dropped (F13). The lexer is fuzzed (increment 9).

**Classifier.** Each statement is classified by its leading keywords, after the lexer's normalisation of case,
whitespace and comments, into a **kind**. The allowlist of the first version:

| Kind | Allowed | Notes |
|---|---|---|
| `CREATE SCHEMA` | yes | Not the system schemas, not `kingres` |
| `CREATE TABLE` (also `PARTITION OF`, `USING`, `WITH`, `UNLOGGED` refused) | yes | `UNLOGGED` and `TEMP` are refused (distribution refuses them anyway); `USING` only `heap` or `columnar` of an extension in the archive |
| `CREATE SEQUENCE`, `ALTER SEQUENCE ... OWNED BY` | yes | |
| `CREATE TYPE` (`AS ENUM`, composite, range), `CREATE DOMAIN` | yes | |
| `CREATE INDEX` (not `CONCURRENTLY`) | yes | |
| `ALTER TABLE` | only: `ADD CONSTRAINT`, `ALTER COLUMN ... SET DEFAULT`, `SET NOT NULL`, `ADD GENERATED`, `ATTACH PARTITION`, `OWNER` removed, `REPLICA IDENTITY`; not `SET SCHEMA`, not `SET TABLESPACE`, not `ENABLE/DISABLE TRIGGER`, not row-level security | |
| `CREATE VIEW` / `MATERIALIZED VIEW` | yes, a materialized view `WITH NO DATA` only | A view of a distributed table is created as the phase 9 `dependentViews` rule creates it |
| `COMMENT ON` | yes | |
| `GRANT`, `REVOKE` | only with `privileges: restore` and roles in `referencedRoles` | |
| `SELECT pg_catalog.set_config('search_path', '', false)` | the exact text | Dropped; the importer sets `search_path` itself |
| `CREATE FUNCTION`/`PROCEDURE` | **only with `allowCode`**, language `sql` or `plpgsql`, no `SECURITY DEFINER` unless `allowCode`, no `C`, `internal` or any untrusted language | |
| `CREATE TRIGGER` | **only with `allowCode`**, executing a function created by the archive | |
| `CREATE POLICY`, `ENABLE ROW LEVEL SECURITY` | refused in the first version | Distributed tables refuse them anyway |
| `COPY`, `DO`, `CREATE EVENT TRIGGER`, `CREATE RULE`, `CREATE COLLATION`, `CREATE FOREIGN ...`, `CREATE SERVER`, `CREATE PUBLICATION`/`SUBSCRIPTION`, `CREATE TABLESPACE`, `CREATE ROLE`/`ALTER ROLE`/`CREATE DATABASE`/`ALTER DATABASE`, `ALTER SYSTEM`, `SET ROLE`, `SET SESSION AUTHORIZATION`, `RESET`, `SET` (anything), `CREATE EXTENSION`, `LOAD`, `CALL`, `SELECT` (other than the one above), `INSERT`/`UPDATE`/`DELETE`, transaction control | **never** | The importer sets the session itself and runs the structure in its own transactions |

**Execution.** The statements run **one per round trip, as the signed-in role**, on a connection from `Roles.Connect`
to the **new** database (never the service connection), after `refuseDangerousRole`. The importer sets
`statement_timeout` (a fixed value, 10 min), `lock_timeout`, `idle_in_transaction_session_timeout` and
`search_path = pg_catalog` as `SET LOCAL` itself; a `SET` in the archive is refused, not honoured. The extended
protocol refuses text that holds several statements (the workbench relies on this, `ops/query.go`), which is the
second line behind the lexer. Pre-data runs in one transaction per **table** (the table and its dependent
statements), so a failure rolls back its table and the task then drops the whole database anyway.
**Nothing from the archive is run as the service role**: the distribution, the ledger and the policies are made
from the structured document, with identifiers validated by `sqlx.NewIdent` and quoted by the same package, and
numbers range-checked (bucket count 1 to 4096).

**`allowCode`.** Functions and triggers are code that runs later with someone's privileges (a trigger runs on every
write, a `SECURITY DEFINER` function with its owner's rights, which here is the importer). By default an archive that
contains one is **refused at step 3** with 422 `archive_statement_refused` naming the kind and count, so the user
learns it from the validation report, before anything is created. With `allowCode: true` (and the typed confirmation)
the plan allows `sql` and `plpgsql` functions and the triggers that call them; the audit event records `allowCode`.
The privileges of the importing role still bound them. A schema that depends on functions is exactly the case where
`pg_dump` and `pg_restore` by a database administrator are the right tool, and the dialog says so.

### Versions and refusing an incompatible archive

| Check | Rule | Code |
|---|---|---|
| Format | the archive's `format` major must be one the importer lists (now: 1) | `archive_incompatible` |
| PostgreSQL | the target's major must be **at least** the source's (a dump of 18 loads into 19; a dump of 19 never into 18) | `archive_incompatible` |
| Extensions | each extension of the manifest must be installable on the target at a version at least the archive's, and the importer must know the archive's `catalogVersion` (its list) | `archive_incompatible` |
| Contract | informational only (shown, never refused) | none |
| Dialect | `distributedSchemas` non-empty needs an importer that supports phase 11; an unknown **required** feature in the manifest (`requires: [...]`) is a refusal | `archive_incompatible` |

All reasons are listed in one answer (`reasons: [...]`), as `table_not_eligible` lists blockers.

### Failure: the new database is removed

`database.import` has an `OnFail` hook that **drops the new database** unless `keepOnFailure`. The hook drops only
when the database's comment is `kingres-import:<this task id>` (set in step 4), so a task cannot drop a database it
did not create, and a second task with the same name cannot be fooled. The hook is the compensation the
`database.create` task has for a partial create, and it runs on whichever server is the leader when the task fails.
A database kept with `keepOnFailure` stays marked failed in `listDatabases`' status until dropped.

## Hostile input

### Archives

Done by `archive.Validate` in the import's first step (before a database exists), by `archive/zip` over the
`io.ReaderAt` of the staged file, **without ever writing an entry to a file system**: entries are streamed into
`PgConn().CopyFrom`, parsed in memory (the manifest, the catalog) or lexed from a stream (the SQL). No name of the
archive is ever used as a path, so path traversal has nothing to act on, and it is still checked:

1. **The file as a whole**: the end-of-central-directory record is found at the end and the comment is at most 64 KiB;
   the entries' extents (from their offsets and compressed sizes) do not overlap, are inside the file, and with the
   central directory **account for every byte** of the file (no hidden bytes before, between or after, which stops
   polyglots and an archive inside an archive); Zip64 is accepted, a multi-disk archive is not.
2. **Names**: exactly `manifest.json`, `catalog.json`, `schema/pre-data.sql`, `schema/post-data.sql` and
   `data/NNNNN.csv` (five digits, contiguous from 00001); no other name, no directory entry, no duplicate, no
   backslash, NUL, `..`, leading slash or drive letter, at most 64 bytes. The mode bits must say a regular file (no
   symbolic link). The method is Store or Deflate; anything else is refused.
3. **Sizes**: the manifest is read first and is at most 4 MiB; every entry's declared compressed and uncompressed
   sizes must equal the manifest's; the sum must not exceed `maxFileBytes * 20`; one entry may not expand beyond
   1000 to 1. The **expansion is counted while reading** with a limit reader at the declared size plus one byte, so a
   header that lies is caught by the data (a zip bomb whose header says small fails when the stream is longer; one
   whose stream is as declared is within the limits by construction).
4. **Checksums**: every entry is read once and its SHA-256 compared with the manifest (this is the one pass over the
   whole archive); the CRC-32 of the zip is checked too. The manifest's own coherence is checked: the table list of the
   catalog document equals the data entries (`table` members), the counts add up to `totals`.
5. **JSON** (manifest and catalog): size caps (4 MiB, 16 MiB), `DisallowUnknownFields`, a nesting depth of at most 32
   and at most 100 000 members per array, strings of at most 4 KiB; **every identifier** is checked by the rules of
   `sqlx` before it is used, and used only quoted; numbers are range-checked; `format`, `catalogVersion` and
   `requires` are checked before anything else is read.
6. **SQL**: by the lexer and classifier above, as part of step 3, not run.
7. **CSV**: a data entry is bytes for `COPY FROM STDIN`, never SQL (there is no `\copy`, no psql in the pipeline), and
   its records are bounded by the scanner (16 MiB).

The archive is **also not trusted for who made it** (the checksums are not signatures); that is why the audit
event has the file's SHA-256 and why `expectedSha256` exists.

### CSV files

A CSV is only data for `COPY FROM STDIN` with options that the control plane built from checked values, so the
risks are: a record that is enormous (the scanner's 16 MiB), a gzip bomb (the ratio cap and `maxFileBytes` on the
expanded stream), an encoding that is not what the user said (PostgreSQL's conversion errors fail the load with the
line), a `\.` line (the count check, above), a header that is not the table's (`match`), and values that violate
constraints (PostgreSQL's own errors; the control plane includes the line and column and **never the row** in a
problem or in the audit log: a message of PostgreSQL's `COPY` context contains the line's text, so the control plane
passes it through `safeSQLContext`, which keeps the line number and the column name and drops the data that follows).
CSV **injection into a spreadsheet** is the export's problem, handled above; an imported value that starts with
`=` is data like any other and is stored as it is.

### What a hostile dump can and cannot do

It can ask for: a statement the classifier refuses (the import ends at step 3, nothing created); code
(refused without `allowCode`); resource use (bounded by the caps, the statement timeout, the size limits and the
concurrency limit); a table that the extension refuses to distribute (the extension's report); a name that collides
with something (the database must not exist; schema names of `kingres`, `pg_*` and `information_schema` are
refused). It cannot: run as the service role or as a superuser (the role is not dangerous, the service role runs
structured calls only), write to the server's file system or run a program (no `COPY` to a file, no `COPY ... PROGRAM`,
no `COPY FROM` of a file, and a role with the predefined roles for that is refused), change a role, a setting or
another database, load a library, or change anything outside the new database (statements are on a connection
to the new database, names are schema-qualified by `pg_dump` and `search_path` is `pg_catalog`, and a statement that
names another database is a syntax error: PostgreSQL has no cross-database statement). What it **can** still do
within these limits is create objects, with names and bodies that are bounded text, owned by the importing role,
in a database that the failure of any step removes.

## Ledger tables

[ADR 0028](../adr/0028-ledger-tables-are-carried-as-data-and-restart-their-chain.md) records the decision and the
alternatives. In summary:

- **Table CSV export** of a ledger table is an ordinary export of its current rows. The history table of an
  updatable ledger table is a table of its own and can be exported on its own (the role needs `SELECT`).
- **Table CSV import** into a ledger table is allowed (a local table): the capture trigger enters every row as for any
  insert (F14), and a table with the policy `required` is refused up front because it would fail on the first row
  (`KL010`). The ledger's own tables (`kingres.ledger_*`) are in schema `kingres` and are refused as targets.
- **Database export** carries a ledger table as data plus its ledger `mode` and `policy`; its capture triggers and
  its history table are **not** in the schema section (normaliser rule 3), and the manifest lists it with
  `ledger: {mode, policy, headHeight, headBlockHash}` read **without writing** (no `ledger_generate_digest`, which
  would add a row to the source; the head is read from the extension's block table as the role). The history rows of an updatable
  table are **exported for the record** as a data entry `ledgerHistory: true` and are **not loaded** (the history
  table is created empty by `ledger_enable`, and the guard refuses writes to it by every role).
- **Database import** loads the rows **first**, then calls `ledger_enable(table, mode)`, which enters all existing rows
  as the first entries of a **new chain**. Without `acknowledgeLedgerRestart` the import is refused at step 3 (422
  `ledger_restart_not_acknowledged`) with the list of ledger tables and the sentence below.
- **What the verifier says.** On the new database `ledger_verify()` returns **no rows**: the chain it holds is
  consistent. It says nothing about the source's chain, and cannot: the new database has a new chain identity (README
  of `kingres_ledger`, "Copies of the database"; a digest of the source does not verify against it). The old proofs and
  the digests published for the source stay valid for the source database, which is unchanged by the export. The
  manifest's `headHeight` and `headBlockHash` let an auditor compare the archive with a digest the source published, and
  the import's report and the console say: "The imported ledger starts a new chain at import time. It does not
  carry the history that proves the data was not changed before; keep the digests of the source database."
- **A plain `pg_dump` of the same database** (which the design does not use) would leave the capture triggers of
  unregistered tables in the new database, and the verifier would name each of them; the normaliser removing them is
  what keeps the verifier quiet and honest.

## Leader change and failover

Files, keys and role connections are the leader's (F3, F6, F4). Rules:

1. **Every transfer step starts by checking its inputs**: the row's `server_id` is this server, the file exists, its
   size equals the row's, and (for a step that needs a role connection) the role is connectable here. If not, the
   task fails with `transfer_leader_changed` (`retryable: true`, message: "repeat the operation"), the row is set
   `lost` and the janitor of the server that holds the file deletes it when that server runs again. The new leader
   does not adopt files of others.
2. **A leader that loses its lease mid-step** stops (`Interruptible` steps are cancelled by the lease loss, F4) and its
   `COPY` is cancelled with the context; a distributed `COPY` is atomic, so nothing partial is stored; a batch
   import has committed batches, reported by the task failure as `rowsCommitted` and **recoverable from the failure**
   only if the new leader is asked to continue with `skipRows` and a new upload (the upload was on the old leader; the
   console starts again at the offset 0 of a new upload if the old one is `lost`, and tells the user so).
3. **A batch whose commit was in flight** when the connection or the leader was lost has an unknown outcome:
   `batch_outcome_unknown` (above); the new leader never retries it.
4. **A database import** that fails for this reason runs `OnFail` on the new leader, which drops the database it
   marked (the hook needs no file, only the comment and the service role).
5. **Failover of a shard group during a `COPY`**: the router fails the statement with `KS001` or a connection error
   (README: "a group that changed its primary in the middle of it fails it with `KS001`, nothing stored"). Export
   restarts the step (3 attempts); an `allOrNothing` import fails with nothing stored and the code `copy_interrupted`
   (retryable); a batch import retries the batch (3 attempts) if the failure was before the commit.
6. **The leader restarts** (same disk): the staged files are still there, the role map is empty (F3), so a running
   transfer fails at the next step with `transfer_session_lost`; an upload stays and can be used by a new request,
   which makes the role connectable again.
7. **Uploads** are cheap to lose: `getDataUpload` answers 404 for a `lost` upload and the console starts a new one.

## REST contract, revision 0.19.0

All additions are optional properties, new operations, new enum values and new problem codes (the contract's rule:
clients tolerate codes they do not know). Operations go from 140 to 153.

### New tag and operations

Tag `Data transfer`. `...` in a path is `/databases/{databaseName}/schemas/{schemaName}/tables/{tableName}`.

| operationId | Method and path | Role | Body / query | Response |
|---|---|---|---|---|
| `exportTable` | `POST .../export` | `kingres_admin` | `ExportTableRequest` | 202 `AcceptedTask`; 403 `data_transfer_disabled`, `insecure_transport`; 404; 409 `operation_in_progress`, `transfer_quota_exceeded`; 422 `table_not_eligible`, `validation_failed`, `insufficient_disk` |
| `importTable` | `POST .../import` | `kingres_admin` | `ImportTableRequest` | 202 `AcceptedTask`; 403; 404; 409 `operation_in_progress`, `upload_incomplete`; 422 `import_header_mismatch`, `import_ledger_policy`, `table_not_eligible`, `validation_failed` |
| `exportDatabase` | `POST /databases/{databaseName}/export` | `kingres_admin` | `ExportDatabaseRequest` (`excludeTables`, `dataMode: all\|none`, `privileges`, `retentionHours`) | 202; 403; 404; 409; 422 `export_not_readable`, `schema_unrecognised`, `insufficient_disk` |
| `importDatabase` | `POST /database-imports` | `kingres_admin` | `ImportDatabaseRequest` | 202; 403 `forbidden` (dangerous role), `data_transfer_disabled`; 409 `already_exists`, `upload_incomplete`; 422 `archive_invalid`, `archive_incompatible`, `archive_statement_refused`, `archive_roles_missing`, `ledger_restart_not_acknowledged` |
| `validateDatabaseArchive` | `POST /data-uploads/{uploadId}/validation` | `kingres_admin` | none | 202 (kind `archive.validate`); 404; 409 `upload_incomplete`; 422 |
| `createDataUpload` | `POST /data-uploads` | `kingres_admin` | `CreateDataUploadRequest` | 201 `DataUpload`; 403; 409 `transfer_quota_exceeded`; 422 |
| `getDataUpload` | `GET /data-uploads/{uploadId}` | `kingres_admin` | none | 200 `DataUpload` (with `report` after a validation); 404 |
| `uploadDataChunk` | `PATCH /data-uploads/{uploadId}` | `kingres_admin` | binary body, header `Upload-Offset` | 204 with `Upload-Offset`; 404; 409 `upload_offset_mismatch`; 413 `chunk_too_large`; 422 `upload_checksum_mismatch` |
| `deleteDataUpload` | `DELETE /data-uploads/{uploadId}` | `kingres_admin` | none | 204; 404; 409 `operation_in_progress` |
| `listDataExports` | `GET /data-exports` | `kingres_admin` | query `limit`, `cursor` | 200 `DataExportPage` (the caller's files only) |
| `getDataExport` | `GET /data-exports/{exportId}` | `kingres_admin` | none | 200 `DataExport`; 404 |
| `downloadDataExport` | `GET /data-exports/{exportId}/content` | `kingres_admin` | header `Range` | 200/206 file; 404; 410 expired; 416 |
| `deleteDataExport` | `DELETE /data-exports/{exportId}` | `kingres_admin` | none | 204; 404 |

(`...` is the table path given above.) All list the shared responses
401, 403, 405, 422 and 503, and 400 where a body or query exists, as the checker requires. `uploadDataChunk` and
`downloadDataExport` carry `x-streams: upload` and `x-streams: download`; the checker is extended to know the extension.

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
    retentionHours:  { type: integer, minimum: 1, maximum: 168 }
ImportTableRequest:
  required: [uploadId]
  properties:
    uploadId: { type: string }
    header:   { type: string, enum: [none, skip, match], default: skip }
    columns:  { type: array, items: { type: string } }
    delimiter, quote, nullString, encoding, compression: as in ExportTableRequest (compression: none|gzip, default detected)
    forceNotNull: { type: array, items: { type: string } }
    forceNull:    { type: array, items: { type: string } }
    skipBom:      { type: boolean, default: true }
    onError:      { type: string, enum: [allOrNothing, batches], default: allOrNothing }
    batchRows:    { type: integer, minimum: 1, maximum: 1000000, default: 50000 }
    batchBytes:   { type: integer, minimum: 65536, maximum: 268435456, default: 67108864 }
    skipRows:     { type: integer, minimum: 0, default: 0 }
    keepUpload:   { type: boolean, default: false }
DataUpload:
  required: [uploadId, state, sizeBytes, offset, chunkBytesMax, expiresAt]
  properties:
    uploadId: { type: string }
    state: { type: string, enum: [receiving, complete, validating, valid, invalid, lost, deleted] }
    sizeBytes: { type: integer, format: int64 }
    offset: { type: integer, format: int64 }
    chunkBytesMax: { type: integer, format: int64 }
    sha256: { type: [string, "null"] }
    expiresAt: { type: string, format: date-time }
    report: { oneOf: [ { $ref: ArchiveReport }, { type: "null" } ] }
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
    format: { type: string, enum: [csv, csv.gz, archive/1] }
    taskId: { type: integer, format: int64 }
    downloadedAt: { type: [string, "null"], format: date-time }
TableConsistency: { required: [table, level, groups], table: string, level: { enum: [single-snapshot, per-group, per-group-and-partition] }, groups: integer }
ArchiveReport:   # also the answer of a validation
  required: [valid, format, versions, tables, reasons]
  properties:
    valid: boolean
    format: string
    versions: { postgresMajor, kingresVersion, extensions: [{name, version, installed: version|null}] }
    tables: { local, hash, reference, ledger, columnar: integer, rows: int64, bytes: int64 }
    statements: { byKind: { additionalProperties: integer }, refused: [ { kind, position, fingerprint, reason } ] }
    needs: { allowCode: boolean, ledgerRestart: boolean, rolesMissing: [string] }
    warnings: { type: array, items: string }
    reasons: { type: array, items: { $ref: TableBlocker } }     # the shape of table_not_eligible's blockers
Problem (additive, optional): reasons: array of TableBlocker   # present for archive_incompatible and archive_statement_refused
ClusterSettings (additive): dataTransfer: { enabled, retentionHours, maxFileBytes, stagingQuotaBytes, maxConcurrent }
TaskKind (additive): table.export, table.import, database.export, database.import, archive.validate
AuditAction (additive): the new operation ids, dataTransfer.completed, dataTransfer.purged
```

### Problem codes (added in 0.19.0)

| Status | Code | When |
|---|---|---|
| 403 | `data_transfer_disabled` | the cluster setting is off |
| 403 | `insecure_transport` | the request is not over TLS |
| 409 | `transfer_quota_exceeded` | the staging quota or the concurrency limit; `Retry-After` for the second |
| 409 | `upload_incomplete` | an import names an upload that has not received all its bytes |
| 409 | `upload_offset_mismatch` | `Upload-Offset` is not the server's; the header says the right one |
| 413 | `chunk_too_large` | a chunk beyond `chunkBytesMax`, or beyond the declared size |
| 422 | `upload_checksum_mismatch` | the SHA-256 of the received file is not the declared one |
| 422 | `import_header_mismatch`, `import_ledger_policy` | `header: match` failed (first difference named); the table has policy `required` |
| 422 | `export_not_readable` | the role cannot read every table (first 20 named) |
| 422 | `schema_unrecognised` | the normaliser met a construct it does not know |
| 422 | `archive_invalid` | structure, names, sizes, checksums, JSON, limits |
| 422 | `archive_incompatible` | versions or required features; `reasons` lists all |
| 422 | `archive_statement_refused` | the classifier refused a statement; `reasons` names kind, position and fingerprint |
| 422 | `archive_roles_missing` | `privileges: restore` and roles that do not exist |
| 422 | `ledger_restart_not_acknowledged` | the archive has ledger tables and `acknowledgeLedgerRestart` is false |
| 410 | (code `not_found`, status 410, as the bundle does) | an expired file |

Codes of a **failed task** (`TaskError.code`, not HTTP): `export_too_large`, `insufficient_disk` (existing),
`transfer_timeout`, `transfer_leader_changed` (retryable), `transfer_session_lost` (retryable),
`copy_interrupted` (retryable), `row_count_mismatch`, `batch_outcome_unknown` (not retryable),
`cancelled` (existing), `verification_failed` (existing).

README of the contract: the task-kind table gets `table.export`, `table.import` (subject `table`, `database/schema.table`),
`database.export`, `database.import` (subject `database`, the database name), and `archive.validate` (subject
`upload`, `<id>`); a "Data transfer" section states the consistency labels, the creator-only rule, and the limits.

## The console

**Where.**

| Place | Actions (administrators; others see them disabled with the reason, as `GateButton` does) |
|---|---|
| Tables list of the database page (`DatabaseDetailPage.vue`), a row action next to *Distribute table* and *Enable ledger* | *Export CSV*, *Import CSV* (a table that has an unfinished table task shows them disabled with "waiting for the operation on this table") |
| The table page of phase 9 (`/databases/:db/tables/:schema/:table`) when it exists | The same two actions in its header |
| Header of the database page, next to *Edit settings* and *Drop database* | *Export database* |
| Header of the databases page (`DatabasesPage.vue`), next to *Create database* | *Import database* |
| A **Transfers** panel on the database page and on the databases page | The caller's staged exports and uploads (`listDataExports`): name, size, SHA-256, expiry, state, *Download*, *Delete* |
| The settings page (`SettingsPage.vue`) | A **Data transfer** group for `dataTransfer` (the switch, retention, limits) |

**Dialogs** (all are `TaskActionDialog`s or follow its pattern, so progress, cancel and errors look as they do for
the other tasks; the confirm button is disabled while a blocker shows):

- *Export CSV*: delimiter, null string, encoding (with `WIN1256` and `ISO_8859_6` offered under their common names),
  header, compression, retention, **Spreadsheet-safe values** (on), with the explanation "values that begin with
  `=`, `+`, `-` or `@` are prefixed with an apostrophe so a spreadsheet shows them as text. This changes those
  values." The consistency of the table is shown **before** the user confirms: "This table is read as one snapshot"
  or "This table is spread over 3 groups; each group is read at its own moment, so a transaction that touched two
  groups can appear in one and not in the other." While the task runs the dialog shows `TaskProgress`; when it is
  `done` it shows size, rows, SHA-256, the expiry and a **Download** link (a plain `<a href download>` to
  `downloadDataExport`; the cookie and `SameSite=Strict` make it work and make it safe), and the sentence about
  running it twice to compare the sums.
- *Import CSV*: step 1, choose a file (a drop zone and a file input); the **browser reads the first 64 KiB locally**
  (`File.slice`) and shows the first rows and a guess of delimiter, header and encoding, which the user corrects;
  nothing is sent yet. Step 2, the options of the request, the mode with the table of "All or nothing and batches" in
  words and the recommendation by size, the target's row estimate ("This table already has about N rows. Rows are
  added; nothing is replaced."). Step 3, the upload with a progress bar, **pause and resume** (the uploader keeps
  the `uploadId` and the file's name and size in `sessionStorage` inside `try/catch`, and on resume calls
  `getDataUpload` and continues at its offset; a file that differs in size is refused), then the import task with
  `TaskProgress`; a failed batch import shows the rows committed and a **Continue from row N** button. The size
  limits are shown from the settings before the file is chosen, and a file above them is refused in the browser and
  by the server.
- *Export database*: tables with sizes and a checkbox to leave a table's data out, `dataMode`, `privileges`; the list
  of tables that use `per-group` reads; the warning that the tables are read one after another, so tables that
  refer to each other may be read at different moments, and that the **import checks the foreign keys at the end**;
  progress by table; the download as above.
- *Import database*: choose the archive, upload (as above), then the **validation** (`validateDatabaseArchive`) with
  `TaskProgress`, then the **archive report** as a page of the dialog: source versions and whether the target can take
  them (a refusal shows `reasons` and stops), tables by kind with rows and size, the extensions, the warnings
  (including each `per-group` label), the **statements the plan refuses** (kind, count, position; the text is never
  shown) and, if there is code, an explanation and a typed confirmation for `allowCode`; the ledger tables with the
  required checkbox and the text of ADR 0028; the option `privileges`; the name of the new database; then the import
  task with its steps; on failure the message names the step and says the database was removed (or kept).
  A role that is dangerous sees the dialog explaining that it must sign in as another administrator.

**Safety of the UI.** Names, tables and messages from an archive or a CSV header are shown through `SafeName` and
`safeText` as everywhere else; a statement's text from an archive is **not** shown (kind, position and fingerprint
only); a download link is built from the transfer id and nothing the user typed; nothing from a file is rendered as
HTML.

**Mock API** (`src/mocks`): a table `failme` fails the export, `blockme` waits, `skewme` is `per-group`, a CSV with
a bad row `badrow.csv` fails a batch import at batch 3, an archive named `hostile.kdx.zip` is invalid, `code.kdx.zip`
needs `allowCode`, `ledger.kdx.zip` needs the acknowledgement, `old.kdx.zip` is incompatible. The mock serves a
download and an upload with offsets.

## Tests

### Unit (Go, no database)

- The sealing writer and reader: round trip at every segment boundary, a `Range` at every offset, a flipped byte, a
  swapped segment, a truncated file (the last-segment flag), a wrong key, nonce uniqueness over 10^6 segments.
- The staging store: quota arithmetic and the reservation, free-space refusal with a fake `statfs`, the janitor with a
  table of states (expired, no row, no file, `lost`, unused upload), a crash between rename and row update,
  permissions (0700, 0600), symbolic links refused, names derived from the id only.
- The relay framing: a header larger than 64 KiB, a body that ends early, a status frame after a break, the proof
  checks (replayed, expired, wrong receiver), the member-only and leader-only rules.
- Upload: offset arithmetic, a duplicate chunk, a chunk past the end, a chunk after a break at every byte, the checksum
  mismatch, concurrent chunks on one upload (one wins), creator-only access.
- The record scanner: quotes, escapes, a newline inside quotes, an unterminated quote at the end, a CR LF mixture, the
  16 MiB limit, a `\.` line, a BOM, a very long record split across reads at every position (a differential test with
  PostgreSQL's own `COPY` on the same bytes is in the integration tests).
- Option checking and the literal quoting of delimiter, quote and null string, hostile values (quotes, backslashes,
  newlines, NUL, non-ASCII, a value of 33 bytes), with a differential test against PostgreSQL's parser.
- The spreadsheet guard: the regular expression against a table of cells (each lead character, a tab, a CR, leading
  spaces, a lone `-`, a negative number as a numeric column that must not change).
- The lexer: strings, dollar quotes with tags, nested comments, `E''` escapes, `U&''`, a statement of 1 MiB, one of
  1 MiB + 1, a missing terminator; **the classifier**: a table of statements, one per row, with the expected kind
  and verdict, including every refusal of the table above, `\restrict`, a psql meta-command, `COPY ... PROGRAM`, a
  `DO` block, `SET ROLE`, a `GRANT` with and without `privileges: restore`, a function with and without `allowCode`.
- The normaliser, with golden files of what `pg_dump` writes for a plain table, a hash-distributed table with its
  bucket partitions, a user-partitioned table with leaves, a reference table, a columnar table, an updatable ledger
  table with its triggers and history (V1, kept per major).
- The archive validator on a **corpus of hostile archives**: see below.
- Version rules: a table of (source, target) pairs for the PostgreSQL major, the extension versions, an unknown
  `catalogVersion`, an unknown `requires`.
- Roles and settings: every operation against every role (`readonly`, `ops`, `admin`), the setting off, a non-TLS
  request, a download by another administrator (404), a delete by another administrator (204), a dangerous role for
  `importDatabase`.
- The audit details: no value, no statement text and no client-supplied file name in any event (a test greps the
  event of every operation for a canary string placed in each field of the request).
- Contract tests (`contracttest`): the new operations and responses validated against the schemas, the checker's new
  rule for `x-streams`, the generated operation table.

### Integration (a cluster of the test's own, on **PostgreSQL 18 and 19**, from one source)

Each runs on both majors with the lane mechanism of the integration runs (`-Lane`), in a private PostgreSQL install per
run:

- Table export of a local, a reference, a distributed (3 groups), a user-partitioned and a columnar table: the file
  equals the table (the project's `compare_tables`), the labels are right, writers run during the export and every row of the file was
  committed, a bucket move during the export restarts it, a lost group primary restarts it, a cancel leaves no file.
- Table import: every option (delimiter, quote, null string, BOM, `WIN1256` and `LATIN1`, header `none`, `skip`,
  `match`), all-or-nothing against a bad row at the last line (nothing stored, the absolute line number), batches against a
  bad row at batch 3 (two batches stored, `rowsCommitted` exact, `skipRows` continues), the row-count check on a file
  with a `\.` line, a 2 GiB file with the memory of `kingresd` measured (flat), a reference table (both V3 paths), a
  ledger table (entries exist, `ledger_verify` clean), a table with a trigger, and a partitioned table.
- **Batch outcome**: a fault point that drops the connection after the commit message is sent and one before it
  (`fault`, as the other crash tests do), asserting `batch_outcome_unknown` for the first and a clean retry for the second.
- Upload: a break at every chunk and at a random byte, resume, a 20 GiB-declared file refused, the quota, the free-space
  reserve with a small tmpfs (`insufficient_disk` and the cleanup), the janitor over a restarted leader.
- **Leader change** during an export, an upload, an import in each mode, and a database import at each step
  (the failover tests' fault points): the codes, the partial results, the dropped database, no marker left, no
  orphan file after the old leader's janitor runs.
- Roles: through the HTTP API with real sessions of each role, on a follower and on the leader (the relay), a download
  through a follower with a 3 GiB file (flat memory on both servers).
- **Round trip of a database with every table kind** (the acceptance test): a database with local, reference, hash
  (two colocation groups, one with a different bucket count), user-partitioned, columnar, an updatable and an
  append-only ledger table, a time-partition policy, sequences used from two nodes, foreign keys between tables of
  both kinds, views, an enum, a domain, comments, privileges, a table with every column type of the project (including
  sketch types, `money`, `bytea`, `jsonb`, arrays, ranges, infinity, `-0`, NaN, a 1 MiB text, a text `\N`, an empty
  string, a null), is exported on 18, imported on 18 **and** on 19, and compared: the same tables, the same
  distribution (class, column, bucket count, colocation relations), every table row for row, sequences above the
  maximum on every node, policies set, the ledgers enabled, the verifier clean, constraints valid, `check_cluster()`
  clean. A **18 to 19 archive loads** and a **19 to 18 archive is refused** (`archive_incompatible`).
- Import after the export of a database that was written to during the export (the skew): the foreign-key step fails
  with the constraint's name in a run that is built to produce skew, and the database is removed.
- Hostile inputs at the integration level: every archive of the corpus through the real endpoint; a CSV that is gzip
  bomb, a record of 17 MiB, a header with a hostile name; a dump with a `COPY ... PROGRAM`, a `DO`, a `SET ROLE`, a
  function, `ALTER SYSTEM`, `CREATE ROLE`, a schema `kingres`, a table named like a system table: each is refused at the
  plan step and **nothing was created** (`pg_database` unchanged, no staging leftovers).

### The corpus of hostile archives

One generator in the test code produces, from a good archive: a path with `..`, an absolute path, a backslash, a NUL, a
duplicate name, a directory entry, a symbolic link, an unknown name, a missing manifest, an oversized manifest and
catalog, JSON nested 10 000 deep, an unknown member, a header that declares 1 KiB over a 1 GiB stream and the
reverse, a deflate bomb (1000 to 1 and 10 000 to 1), overlapping entries, hidden bytes between entries and after the
central directory, a second end record inside a comment, an archive inside an archive, Zip64 fields that lie, a wrong
CRC, a wrong SHA-256, a manifest that disagrees with the catalog, an `extensions` list with an unlisted extension, an
identifier with a quote or a newline, a bucket count of 0 and 4097, a gigantic `rows`, 100 000 entries, a `format` of
`kingres-export/2`. The expected result of each is `archive_invalid` or `archive_incompatible` **before any statement
or database**, and the validator's memory and time are bounded (a test measures them).

### Fuzzers

Native Go fuzz targets for the record scanner, the lexer, the classifier, the archive validator (a byte-level mutation of a
good archive), the manifest and catalog decoders and the option literals, with the corpus above as seeds. They run in CI
for a fixed time and the project's cap on fuzzers applies (WSL runs cap them).

### Mutants

One-line edits, each of which must be **killed** by a named test (the same method as the analytics extensions'
reviews): the scanner treats a quote inside a quoted field as the end of the field; the scanner ignores a newline
inside quotes; the 16 MiB limit is off by one; the `Upload-Offset` check is `>=`; the creator check on download is
removed; the creator check on delete is added to the admin path (it must not be); the setting check is removed; the TLS check is removed; the
classifier allows `COPY`; allows `SET`; allows `CREATE FUNCTION` without `allowCode`; treats `ALTER SYSTEM` as `ALTER`;
the lexer ends a dollar quote at the first `$`; the lexer ignores a nested comment; a backslash line other than
`\restrict` is dropped instead of refused; the archive name check allows `..`; the overlap check is removed; the hidden
byte check is removed; the declared-size limit reader is `+1000`; the SHA-256 comparison is skipped; the PostgreSQL
major compare is reversed; the extension version compare is `>`; the ledger acknowledgement is not required; `ledger_enable` is called before the load;
history rows are loaded; the spreadsheet pattern loses `@`; the guard changes numeric columns; the consistency label
is always `single-snapshot`; the free-space reserve is zero; the quota reservation is not released on delete; the
janitor deletes a file of another server; the `OnFail` drop checks no marker; the `row_count_mismatch` check is removed;
a batch with unknown outcome is retried; `skipRows` is not added to the line number; the audit event includes the
statement text; a data entry is written to a path built from its name.

## Increments

Each increment builds and passes its own tests; the contract checker passes from increment 1 on. Sizes: **S** up to
2 days, **M** 3 to 5 days, **L** 6 to 10 days of one writer.

| # | Increment | Size | Depends on |
|---|---|---|---|
| 1 | **Contract 0.19.0 and the record**: the operations, schemas, problem codes, `x-streams` in the checker, the README rows, the generated operation table, the migration `0035_data_transfer.sql`, the `dataTransfer` setting and its console switch; contract tests | M | none |
| 2 | **Staging store and sealing**: the directory, quota, reserve, janitor, the sealing writer and reader, the master key, the audit events `dataTransfer.*`; unit tests and mutants of the store | M | 1 |
| 3 | **Streaming relay, upload and download**: `/internal/v1/forward-stream`, the deadline handling, `createDataUpload`, `uploadDataChunk`, `getDataUpload`, `deleteDataUpload`, `listDataExports`, `getDataExport`, `downloadDataExport` with `Range`, `deleteDataExport`, the TLS and role guards; tests with a follower; V-checks of the streaming limits | L | 2 |
| 4 | **Table export**: `table.export`, the statement builder, the session formats, the spreadsheet guard, the consistency labels (V4), the console dialog and download, the mock; integration on 18 and 19 | M | 3 |
| 5 | **Table import**: the record scanner, `table.import`, header `match`, `allOrNothing` and `batches`, `skipRows`, the unknown-outcome rule, reference tables (V3), cancel and progress, the console dialog with the resumable uploader and the local preview; integration on 18 and 19, the memory test | L | 3 |
| 6 | **Archive, catalog document and the normaliser**: the zip writer and the validator with the hostile corpus, the manifest, the version rules, the catalog reader, the lexer, the classifier, the normaliser with the golden files (V1, V2); fuzzers; no task yet | L | 1 |
| 7 | **Database export**: `database.export`, the `pg_dump` run, the per-table restartable entries, the manifest and seal, the console dialog; integration on 18 and 19 | M | 4, 6 |
| 8 | **Database import**: `archive.validate` and `database.import`, the steps in order, the allowlist run as the role, the distribution from the document, the sequences, ledger and policies, the verify, the `OnFail` drop, the console dialogs and report page; **the round trip test** on 18 and 19, the 19-to-18 refusal | L | 5, 6, 7 |
| 9 | **Hardening**: fault-point tests of the leader change at every step, the hostile-input integration tests, the mutant run, an independent review of the whole feature and the fixes it finds | M | 8 |
| 10 | **Documentation**: the READMEs (control plane, contract, console), `internal-api.md` for the relay, the administrator's text on limits, retention, consistency and the ledger acknowledgement, and the report of what was run | S | 9 |

Increments 4 and 5 can be built in parallel after 3, and increment 6 is independent of 2 to 5 (it needs only the
contract types) and can start at once after 1. The first increment that gives the owner something to use is 4.

## Known limits

- **No global snapshot.** A distributed table over several groups is exported per group; the labels say so. The cure
  is phase 8.
- **Archives are limited by `maxFileBytes` (20 GiB by default).** A larger database needs `pg_dump` and backups.
- **The downloaded file is not encrypted** and the checksums are not signatures. An operator who needs either
  encrypts and signs the file outside, and can bind the import to a SHA-256 (`expectedSha256`).
- **Spreadsheet safety is by column type.** A `json`, an array or another type whose text begins with a formula
  character is not changed; the dialog says that only text and enum columns are protected.
- **A leader change ends a transfer.** It is not resumed; an upload must be sent again.
- **Roles, owners and the placement of buckets are not carried.** The importing role owns everything; privileges come back
  only with `privileges: restore` and roles that exist; the target places the buckets.
- **Code in a schema** (functions, triggers) is refused unless `allowCode`, and policies, rules, event triggers,
  collations, foreign objects and publications are refused always in this version; such a database is a job for `pg_dump`.
- **Import never replaces and never skips bad rows.**
- **A reference table is loaded through the coordinator** in batches, slower than a routed `COPY`.
- **The history of an updatable ledger table is exported and not loaded**, and the ledger of the target is a new chain.
- **TLS must end at `kingresd`.** A transfer behind a proxy that terminates TLS is refused (`insecure_transport`) until a
  setting for a trusted proxy exists.
- **Needs verification**: V1 (what `pg_dump` writes for a distributed table), V2 (`\restrict`), V3 (reference-table
  insert form), V4 (one cursor per group for a partitioned table), V5 (text round trip of every type of the
  project by `COPY` in CSV with the fixed formats, including sketch types and `money`), V6 (`COPY FROM STDIN` through
  `PgConn().CopyFrom` with the router, including `HEADER` as a boolean and the command tag's count), V7 (`COPY` into
  a `GENERATED ALWAYS` identity column, which the importer avoids by not exporting generated columns but not identity
  columns). Each has a fallback above, is checked by the increment that needs it, and is recorded in this note when it
  is.

## Open questions for the project owner (answered 2026-11-22: all three recommended defaults accepted)

1. **Is `dataTransfer.enabled` off by default?** Recommended: **off** (an administrator turns it on once in the
   settings; every export is data leaving). The alternative, on, makes the first run easier and the default less safe.
2. **Should the downloaded archive have an optional passphrase?** Recommended: not in the first version; encrypt the file with
   the operator's own tool. It is a feature of its own (key handling in the browser, a format) and would add
   a decision about lost passphrases.
3. **Should a database import be allowed into an existing empty database?** Recommended: no. Creating the database
   itself gives the import a clean place to drop on failure and a marker that proves the database is its own.

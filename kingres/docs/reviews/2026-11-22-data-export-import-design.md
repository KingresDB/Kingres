# Independent design review: data export and import from the console (phase 14)

Date: 2026-11-22
Reviewed: branch `data-xi` (design commit 2728755821, and the renumbering commit after it, which only changes ADR
numbers and records the owner's answers). Documents: `kingres/docs/design/data-export-import.md`, ADRs now numbered
0027 and 0028. Facts F1 to F19 were checked against the code and the READMEs of the extensions at that commit.
Method: read-only reading of the design and of the cited code (control plane `forward.go`, `guard.go`, `query.go`,
`ops.go`, `databases.go`, `task.go`, `proxy.go`, the shard, ledger and timepart READMEs); no code was run.

## Summary

The design is careful and most of its structure is sound (staged and encrypted files, creator-only access, the control
plane never parsing CSV values, no file or program ever reaching PostgreSQL, validation of an archive before any
database exists, structured catalog instead of SQL for the privileged parts). The facts it cites are accurate: F1,
F2, F3, F6 and F9 to F15 were confirmed in the tree.

The weak points are concentrated in three places:

1. **Exactly-once behaviour of imports** (DXR-01, DXR-02): the bookkeeping of committed batches and the order of the
   row-count check can double or silently truncate rows.
2. **The archive SQL path** (DXR-04, DXR-05, DXR-06, DXR-07, DXR-26): a classifier that works on leading keywords
   cannot give the guarantees the text claims, and the normaliser cannot be "removal only" for what `pg_dump`
   really writes for a sharded database.
3. **Disclosure to lower roles** (DXR-03): task errors carry PostgreSQL message text with row values and are readable
   by `kingres_readonly`.

The database archive (increments 6 to 8) is about two thirds of the risk and more than half of the work, and it
overlaps with what `pg_dump`/`pg_restore` do. A first release that ships table CSV export and import with the staging
store, relay and uploads, and defers the archive, is safe and is the recommended cut (last section).

Counts: 0 critical, 5 high, 16 medium, 6 low (27 findings).

---

## Findings

### DXR-01 (high): a committed batch can be forgotten, so a retry doubles rows

Section: "All or nothing and batches", "A batch whose outcome is unknown", "Leader change and failover" rules 2 and 3.

The design handles one case of uncertainty: the connection is lost during the commit message. Three other cases
produce a committed batch that the task does not know about:

- The step commits batch k, and the process or the lease dies before `Env.SetDetail`/the row range is stored. The
  recorded `rowsCommitted` is behind the truth by one batch. The console then offers "Continue from row N" with the
  stale N. On a table without a unique key (the design itself says retry is unsafe there, but the button is offered
  by the console for every table), batch k is loaded twice.
- A leader that lost its lease while a batch `COPY` was in flight (network partition from the PostgreSQL node, so the
  cancel never arrives and the connection is half open) can still commit after the new leader has failed the task and
  reported `rowsCommitted`. Epoch fencing protects task state, not writes to the database.
- Cancel arriving during the commit of a batch: the task ends `failed/cancelled` with `rowsCommitted`, but the
  batch in flight may have committed.

Scenario: 400 MB CSV into a table with no primary key, batches mode, `kingresd` is restarted between a commit and the
detail write; the user presses "Continue from row N"; 50 000 rows are duplicated and nothing says so.

Suggested change (choose one):

a. Write-ahead the intent: before each batch store "batch k in flight, rows a..b" durably; after the commit store
   "batch k done". On resume or on any failure with an in-flight marker, report the range as **uncertain**
   (`batch_outcome_unknown`), never as committed. Treat every lease loss, cancel, or restart while a batch is in
   flight as unknown, not only a lost connection at commit.
b. Better, make the outcome decidable: in the same distributed transaction that carries the batch, insert
   `(transfer_id, batch_no)` into a marker table of the target database (a local table on the coordinator joins
   the two-phase commit). After an unknown outcome, one query tells the truth and the batch can be retried or
   skipped exactly once. The marker table needs a definer function or a grant for the role.
c. Cheapest: leave `batches` out of the first release (see first cut) and document "split the file".

Also: do not offer "Continue from row N" on a table that has no unique key unless the failure was a known one.

### DXR-02 (high): the row-count check runs after the commit, so "all or nothing" is not

Section: "The pipeline", "Progress, cancel, timeouts" (step *Verify* after *Load rows*).

The scanner counts records and compares with the `COPY` command tag "of every batch", then fails the task. The listed
steps place this comparison in *Verify*, after *Load rows*, i.e. after the commit. If a line `\.` (or any other
disagreement between the scanner and the server about record boundaries) ends the load early, the server commits the
rows before it and returns a smaller tag. The task then fails with `row_count_mismatch`, but half of the file is
committed and `allOrNothing` has been broken. Whether PostgreSQL 18 and 19 treat a lone `\.` line in CSV data from
`CopyData` messages the same way is version dependent and is only listed as V6; the router has its own reader as well.

Suggested change: run the load as an explicit `BEGIN; COPY ...; <compare tag with scanner count>; COMMIT` (or
`ROLLBACK` on mismatch), for both modes and for the reference-table temp-table path. The comparison must be a
precondition of the commit. Test the `\.` line, a quoted `"\."` row and a single-column file on 18 and 19 through
the router and through a local table. Prefer rejecting a file that contains an unquoted `\.` line in the scanner
itself, so the case never reaches the server.

### DXR-03 (high): task errors carry row values and are readable by `kingres_readonly`

Section: "CSV files" (`safeSQLContext`), "Roles, privileges and audit".

`getTask` and `listTasks` are `x-required-role: kingres_readonly` (checked in the contract). `ops.PGProblem` passes
`pe.Message` through for most codes, and `problem.SQL` carries message and position. For `COPY` the data is not only in
the `CONTEXT` line that `safeSQLContext` strips:

- `invalid input syntax for type integer: "abc"`, `value too long for type character varying(5)`,
  `date/time field value out of range: "..."`, `invalid byte sequence for encoding "UTF8": 0xe9 0x20`:
  the value is in the **message**.
- `duplicate key value violates unique constraint "u"` has `DETAIL: Key (email)=(bob@x.example) already exists.`
- `null value in column ... violates not-null constraint` has `DETAIL: Failing row contains (...)`.
- Foreign key and exclusion violations print key values.

Scenario: an administrator imports a file into a table; the failed task's error shows a customer's email and the
entire failing row; a `kingres_readonly` operator, who has no `SELECT` on that table, reads it with `getTask`. The
same applies to the import step of the database import (foreign key and unique violations) and to the 422 problem
body of a failed batch. The mutant list includes "audit event includes statement text" but nothing for task errors.

Suggested change: build the task error from a whitelist, never from `Message` or `Detail`: SQLSTATE, a mapped
text per SQLSTATE class, constraint name, column name, table name, absolute line number. For classes 22 and 23 and
for `XX`, never include the server text. Apply the same function to the problem returned by the HTTP request, to
`TaskError`, to `Task.detail`, to the audit event and to the log. Add the canary test the design already has for
the audit log to task errors: a CSV with a canary value in a failing row must not appear in any task, problem, log
or audit output.

### DXR-04 (high): the classifier works on leading keywords, so whole statement classes bypass it

Section: "What may run", "Classifier".

"Each statement is classified by its leading keywords" cannot enforce the allowlist, because one PostgreSQL
statement can contain other statements or many actions. Examples that pass a leading-keyword rule:

- `CREATE SCHEMA s CREATE TABLE ...  CREATE TRIGGER ... EXECUTE FUNCTION f() GRANT ...`: schema elements may be
  tables, views, indexes, sequences, triggers and grants. This avoids the `allowCode` refusal of `CREATE TRIGGER` and
  the `privileges: none` refusal of `GRANT`.
- `CREATE TABLE t AS SELECT ...`, `CREATE TABLE t (LIKE other INCLUDING ALL)`, `SELECT ... INTO`: kind `CREATE TABLE`,
  but runs a query (the table list says `SELECT` is never allowed).
- `ALTER TABLE t ADD CONSTRAINT c CHECK (...), ENABLE ROW LEVEL SECURITY, SET TABLESPACE x, OWNER TO y`: kind
  `ALTER TABLE` with subcommands the table forbids. The table lists allowed subcommands, but only a parser that
  splits the comma list can apply it.
- `COMMENT ON ROLE`, `COMMENT ON DATABASE`, `COMMENT ON EXTENSION`: allowed by "COMMENT ON yes"; the admin role has
  `CREATEROLE` and `CREATEDB` (`ops/roles.go`), so `COMMENT ON ROLE other` may succeed.
- `CREATE VIEW ... AS SELECT <any expression>`: allowed, with no restriction on the query.
- `CREATE TABLE ... DEFAULT`/`GENERATED ... AS (pg_sleep(86400))`: a stored generated column evaluates on every row
  of the data load (bounded only by the 6 hour statement timeout, which is a way to hold a transfer slot for hours).

The extended-protocol rule (one statement per round trip) is the second line the design cites; it stops the
multi-statement text but not these single statements.

Suggested change: replace "leading keywords" by a **shape grammar of what `pg_dump` emits**: a recursive-descent
parser per allowed statement kind that consumes the whole statement and accepts only the clauses `pg_dump` writes
(for `ALTER TABLE` exactly one subcommand; for `CREATE SCHEMA` no elements; for `CREATE TABLE` no `AS`, no `LIKE`, no
`INHERITS` unless the catalog says so; for `COMMENT` an allowed object kind list: table, column, index, constraint,
sequence, type, domain, schema, view). Expressions (defaults, checks, generated, index, view bodies) are the one
place a full parser is not possible: accept them only after a function-call allowlist check, or cap them at what
`pg_dump` and the catalog say (the catalog document can carry a list of the functions used, checked against
`pg_proc` after creation, with a refusal if any is not a built-in or an allowlisted extension function). State in
the design that the guarantee of "cannot do more than the role could do by hand" rests on PostgreSQL privileges
alone, which is true and enough, and drop the claim that the classifier narrows it.

### DXR-05 (medium): `privileges: restore` is gated by a list that the archive declares

Section: "Roles of the archive", classifier row for `GRANT`.

`referencedRoles` is in `catalog.json`, which the attacker writes. "Allowed only with roles in `referencedRoles`" is
therefore no check. The row also says nothing about the **role-membership form** `GRANT r TO s [WITH ADMIN OPTION]`,
nor `REASSIGN OWNED`, `DROP OWNED`, `ALTER DEFAULT PRIVILEGES`, nor a grant on a table *to* a `pg_*` role versus
`GRANT pg_x TO role`. The design bans grants "to" a predefined role but not grants *of* one. The importing admin has
`CREATEROLE`; membership grants it can make through SQL bypass `refuseDangerousTarget` and the audited
`setRoleClusterRoles` path (`ops/query.go`, `ops/roles.go`).

Suggested change: allow only object privilege `GRANT/REVOKE ... ON <table|sequence|schema|column|type|domain> ...
TO <existing role | PUBLIC>`; refuse role-membership grants outright; build the role list from the statements, not from
the catalog; check existence and the dangerous-role predicate (`dangerousRolesSQL`) on every grantee; refuse
`WITH GRANT OPTION` to PUBLIC. Consider leaving `privileges: restore` out of the first release.

### DXR-06 (high): the normaliser cannot be "removal only" for what `pg_dump` writes

Section: "Normalising the dump" (F12, V1), "Classifier" table.

The design promises a normaliser that "removes, and only removes" a closed set, and that anything else fails the
export. Things `pg_dump` is known to write that fall outside the table and are not removals:

- `SET default_table_access_method = columnar;` before a columnar table (pg_dump uses this form, not a `USING`
  clause, for ordinary tables). The classifier says `SET` is never allowed. Without handling it, **no columnar table
  can be exported**. (`USING` appears for partitioned tables on newer majors.)
- `ALTER TABLE ONLY parent ADD CONSTRAINT ... PRIMARY KEY` and `ALTER INDEX parent_idx ATTACH PARTITION leaf_idx`
  for partitioned tables. `ALTER INDEX` is not in the allowlist, so every user-partitioned or hash-distributed
  table with a primary key fails. For a distributed table, the leaf indexes are the (excluded) bucket partitions'
  indexes; keeping `ONLY` on a parent whose bucket partitions already exist leaves an invalid parent index, so
  `ONLY` has to be **rewritten**, which is not removal.
- `COMMENT ON EXTENSION ...` (classifier: allowed, then fails at run time because the importer is not the extension
  owner), `CREATE STATISTICS`, `ALTER TABLE ... CLUSTER ON`, `ALTER TABLE ... ALTER COLUMN ... SET STATISTICS`,
  `CREATE UNLOGGED TABLE`, `ALTER TABLE ... SET (autovacuum...)`.
- Views and materialized views, see DXR-07.

An export that only fails is acceptable, but the test of what is supported must be known before the design is fixed.
The text depends on V1, which is scheduled inside increment 6, after the lexer and classifier are designed around
the assumption.

Suggested change: run V1 as a spike before increment 1 on a real 3-group cluster on PostgreSQL 18 and 19 and with
the exact pg_dump from the tree; write the list of statements it produces for every table kind; then fix the
classifier and the normaliser around that list, allowing a small set of **rewrites** (`ONLY`, access method
`SET`) in addition to removals. State the supported subset in the design ("schemas that use extended statistics,
unlogged tables, collations, RLS, rules, event triggers, FDW are refused"). Run the exporter's own output through
the importer's validator before sealing, so an archive that cannot be imported fails at export (the design claims
this property, but only for statements, see DXR-14).

### DXR-07 (medium): views created before distribution block the distribution

Section: "The task `database.import`" steps 6 and 7; classifier row for `CREATE VIEW`.

The extension's default is `kingres_shard.dependent_views = refuse`; a view that uses a table makes
`create_distributed_table` fail with `dependent_object` (README, "Dependent views"; materialized views are refused
even with `recreate`). The design runs *all* pre-data (including views) before distribution and says views are created
"as the phase 9 `dependentViews` rule creates it", which is a rule for an existing table, not for ordering.
Scenario: any database with one view over a distributed table fails the import at step 7 and the database is dropped.

Suggested change: take views, materialized views and anything that depends on a table out of pre-data and run them
after step 7 (and before or after the data load, materialized views `WITH NO DATA` last). The normaliser must split
them, which again is not removal-only (DXR-06). Alternatively `SET kingres_shard.dependent_views = 'recreate'` in the
service session, which still refuses materialized views.

### DXR-08 (medium): partition roots and leaves, and inheritance children, are exported twice

Section: "The task `database.export`" step 2 and step 5, "Export request" (`SELECT ... FROM <table>`).

The export list is "the user tables of non-system schemas"; the statement is `SELECT cols FROM table` without
`ONLY`. For a local partitioned table (and for a time-partitioned table of `timepart`, which are native partitions)
`SELECT FROM root` returns the rows of every leaf, and each leaf is also a table in the list. For a plain
inheritance parent the same holds. The manifest counts what the export wrote, and the verification compares with the
manifest, so the doubled rows pass every check. Scenario: a round trip of a timepart table with 10 million rows
yields 20 million rows on the target with `verified rows` in the report.

Suggested change: state the rule: for a partition tree export only the root (`SELECT FROM root`) and load only
through the root, leaves are structure only; for an inheritance parent use `FROM ONLY`. Put "tree root" and
"children" in the catalog document, test with a partitioned, a timepart, an inheritance and a mixed distributed
tree. Compare the row total with `count(*)` of the source tree at the end of the export as an independent check.

### DXR-09 (medium): large objects, `oid` columns and other state are silently not carried

Section: Goals, "Known limits".

`pg_dump --schema-only` writes no large objects, and the data entries are CSV of tables. A database with large
objects (or with `oid`/`lo` columns referencing them, a common pattern for older applications) exports "successfully"
and imports with dangling object ids. The design does not mention large objects at all. The same applies to
subscription/publication state, replication slots, `pg_largeobject` ACLs, security labels, and the contents of
unlisted extension-config tables.

Suggested change: at export, count `pg_largeobject_metadata` and columns of type `oid`/`lo` (and `regclass` family
columns, whose numbers do not carry over); refuse by default with a code such as `large_objects_present`, with an
explicit acknowledgement to proceed without them, recorded in the manifest warnings. Add a section "What the archive
does not carry" that lists these.

### DXR-10 (medium): the import can load into a database it did not create

Section: step 4 "Create the database", "Failure: the new database is removed".

`createDB` in `ops/databases.go` returns without error when the database already exists ("an earlier run of this step
already created it"). The request-time check "database must not exist" is a different moment. Scenario: administrator
A starts an import into `shop`; administrator B creates `shop` before step 4; step 4 returns success, the comment
marker is set on B's database (or the comment call fails because A does not own it), and the import then runs the
service-role distribution calls and the role statements against B's database. The `OnFail` guard (drop only if the
marker matches) then drops B's database if the marker was written. A crash between `CREATE DATABASE` and the
`COMMENT` leaves an unmarked database that a resumed task cannot tell from someone else's, and the failure hook will
not drop it, so garbage accumulates.

Suggested change: create the database under a name that only this task uses (`kingres_import_<taskid>`), do all work
there, and rename it to the final name as the last step (`ALTER DATABASE RENAME`, after checking it is still free);
the hook drops only that private name, with no marker comment. If renaming is not possible in the multi-server
layout, then step 4 must not reuse `createDB` as it is: create with `CREATE DATABASE`, treat "exists" as failure
unless the comment already equals this task's marker, and write the marker in the same service session immediately.

### DXR-11 (medium): an export runs the code of other roles as the administrator, and RLS can silently shorten it

Section: "Roles, privileges and audit" (exports run only statements the control plane writes), "Table export".

`COPY (SELECT ...) TO STDOUT` as the signed-in role evaluates, as that role, code that the table owner chose:
virtual generated columns (PostgreSQL 18), row-level-security policy expressions, and default/ generated expression
functions in views of the same query. A role with `CREATE` on a schema can define a function and attach it to its own
table; when an administrator exports (or the database export iterates every table of the database, which is bulk and
automatic) the function runs with the administrator's privileges (`CREATEROLE`, `CREATEDB`, and everything the
administrator can read). The same risk exists for a hand-written `SELECT` in the workbench, but there it is a
deliberate choice; the export turns it into something a colleague's table can trigger. The extension already has the
predicate for this (`function_owner_not_superuser`, an "untrusted code" rule applied before a distribution copy).

Second effect: for a table with row-level security enabled (local tables), `COPY (SELECT)` applies the policies, so
the file contains only the rows the role may see while the manifest, row counts and the "round trip" all say
complete. `pg_dump` as a non-bypass role would error on such a table instead of silently exporting less.

Suggested change: before reading a table that is owned by another role, refuse (or require an acknowledgement) when
its read path reaches a function owned by a role that is not the exporter or a superuser (reuse the extension's
check); refuse tables with RLS enabled unless the role has `BYPASSRLS` or owns the table with `row_security = off`
and say so; record `rlsApplied` in the report.

### DXR-12 (medium): `pg_dump` as the requesting role may not work for databases with the project's extensions

Section: "The task `database.export`" step 3.

The timepart README states that a dump "needs a role that can read the base tables: a superuser, or any role that can
read every table of the database, such as a member of `pg_read_all_data`", and that a database owner that is not a
member of that role is refused. The ledger extension keeps its tables in `kingres`, readable by few roles. The
design checks `SELECT` on user tables only (`has_table_privilege` for every table) and runs `pg_dump` as the role.
A normal administrator may therefore pass step 1, and the task fails at step 3 after it has started, in exactly the
databases that use ledger or timepart. Schema-only dumps may avoid some of it, but nothing in the design shows that.

Suggested change: include in V1 a run of the exact `pg_dump` command as a non-superuser administrator (database owner,
non-member of `pg_read_all_data`) against a database that has each extension, and a pre-flight at step 1 that tests
the same conditions (for example by `pg_dump --schema-only` to `/dev/null` with a short timeout). If it fails, say
so in a code (`export_needs_read_all`) before any file is staged. Consider catalog introspection by the service role
(read-only) for the schema instead of `pg_dump` as the role, which also solves DXR-11 for schema generation.

### DXR-13 (medium): skew between tables makes large imports fail at the end

Section: "What is consistent", step 9 of the database import, "Known limits".

Tables are read one after another with no snapshot, writers not stopped. For a database with live writers, a foreign
key violation at step 9 is the common outcome, not a corner case, and the user finds out after the whole upload and
load (hours), the database is dropped, and the only advice is to retry in a quiet period. "Export twice and compare
the SHA-256" works for one table, not for an archive (the manifest has creation times).

Suggested change: (a) for a database whose distributed tables all live in one group (the one-group case the design
already identifies) and for local/reference tables, export in a single `REPEATABLE READ READ ONLY` transaction so the
whole archive is one snapshot (the cost, delaying bucket moves, is acknowledged in F11 and acceptable for a bounded
export); label per table; (b) let the import load foreign keys as `NOT VALID`, report the violations by constraint
name and count, and let the user decide; (c) re-read a schema fingerprint (catalog document hash and `pg_dump` object
list) at the end of the export and fail with `schema_changed` if any DDL ran meanwhile (see DXR-14); (d) in the
console, warn before an export of a database with recent write activity (the cluster already has statistics).

### DXR-14 (medium): the database export subject does not stop DDL or table tasks during the run

Section: F5, "The task `database.export`", step 2 versus 3 versus 5.

The subject of `database.export` is the database name; table tasks use `database/schema.table`, so a table
distribution, `ALTER`, undistribute or a user's DDL in the workbench can run while the export runs. The catalog
document (step 2), the schema (step 3) and the data (step 5) are then taken at three moments: a column added between
them gives a CSV header that does not match the schema; a table distributed after the catalog was read is exported as
`local` and imported as local. The claim that "an archive that cannot be imported is found at export time, by the
same classifier" covers statements, not this.

Suggested change: take the catalog document and the schema inside one short repeatable-read transaction where
possible; at the end, recompute a fingerprint (table list, columns, distribution class) and fail on difference;
write the column list of each data entry into the manifest and check it against the schema at validation. Also
hold the table subjects (or a database-level "no table tasks" guard) while an export runs.

### DXR-15 (medium): the zip validation rules are incomplete for local headers, descriptors and ratios

Section: "Hostile input" (Archives) 1 to 4.

- "Extents ... account for every byte" is stated in terms of offsets and compressed sizes. A zip entry also has a local
  file header (30 bytes plus name plus extra) and optionally a data descriptor. Go's `archive/zip` reads names and
  sizes from the central directory and only reads the local header to find the data start; an entry whose local header
  has a large `extra` field, or whose local name differs from the central name, hides bytes without breaking the
  rule as written. The rule must say: local header name equals central name, local extra field empty, flags equal
  the central ones, descriptor present exactly when the flag says so, and the extent includes header and descriptor.
- The design's own writer streams entries and therefore writes data descriptors (flag bit 3, sizes zero in the local
  header), so the validator's strict shape must accept exactly what the exporter's writer produces and nothing else
  (also true for Zip64). Say so, and test the writer's output against the validator in the round-trip test.
- "No entry may expand more than 1000 to 1": deflate cannot exceed about 1032 to 1, so the cap blocks nothing, and it
  would refuse legitimate highly repetitive CSV. The real bound is the sum: `maxFileBytes * 20` is 400 GiB of data
  that the import would load into the database volumes. Tie the limit to the free space of the data nodes and to a
  per-import `maxImportBytes`, and check the sum of the manifest's `rows` and sizes against it before step 4.
- The archive is read and hashed completely at `archive.validate` and again at import step 1 (up to hours for 400 GiB
  of expansion). Cache the validation by file SHA-256 inside the transfer row and re-check only structure and the
  manifest at import.

### DXR-16 (medium): disk estimates are wrong for the tables that matter most, and the quota is not enforced during writing

Section: "Where files live" (limits 2 and 3).

The reservation for an export is "pg_total_relation_size, times 1". For columnar tables (the project's analytics
tables) the stored size is 5 to 10 times smaller than CSV; for TOAST-compressed text the same; integers and
timestamps become 10 to 30 text bytes; `bytea` doubles; `FORCE_QUOTE *` adds two bytes per field. On a coordinator,
`pg_total_relation_size` of a distributed parent covers only the partitions on this node. The text calls the
estimate an "overestimate"; for these tables it is a large underestimate. The quota is checked only at
reservation time; the running export checks `statfs` and `maxFileBytes`, but not the sum of reservations, so five
exports that each underestimated can fill the staging quota several times over before the free-space reserve stops
them (it then fails every concurrent transfer, not only the offender).

Suggested change: reserve using the catalog's per-group sizes times a per-type expansion factor (columnar x10, others
x1.5) capped by `maxFileBytes`; grow the reservation as bytes are written and fail the writer, not its neighbours,
when the quota is exceeded; keep the `statfs` guard as the last line. Test with a columnar table.

### DXR-17 (medium): the janitor deletes "files with no row" in an operator-chosen directory

Section: "Where files live", janitor.

`transfer-dir` is a flag; the text says an operator may put it on a volume of its own. The janitor "deletes files with
no row". If the flag is misconfigured to a directory that holds anything else, or if the leader's view of the system
database is behind (a restored or lagging system database after failover), valid or foreign files are deleted. The
existing precedent (the ledger bundle directory) is a directory the control plane owns exclusively.

Suggested change: the janitor deletes only names that match `^transfer-[0-9]+\.(part|kdx)$` inside the directory,
never anything else, never below a minimum age (for example 1 hour) and never when the system database reports a
lower maximum transfer id than the files show (a sign of a restored database). At first use, refuse a directory that
is not empty and does not carry a marker file written by `kingresd`, and refuse a directory equal to or containing
the data directory, the PostgreSQL data directory or `/`. Add tests for each.

### DXR-18 (medium): the ledger policy and the timepart policy are not restored the way the text suggests

Section: "Ledger tables", ADR 0028 point 3, "The task `database.import`" step 11.

- The catalog document carries the ledger `policy`, ADR 0028 says "the policy `required` of the source is not
  restored", and step 11 mentions only `mode`. The source refused unattested writes; the copy accepts them without a
  word. That is a silent downgrade of a protection, in the one feature whose point is protection. The reason (the
  load would fail on the first row) is a reason to set it after the load, not to drop it.
- `timepart_set_policy` is called with `enabled` as in the source. The README states that the next run drops old
  partitions according to retention; a policy set from an untrusted `catalog.json` with the shortest retention that
  the checks allow drops imported data at the next run. This is not a privilege problem (the policy applies only to
  the new database and the checks hold) but is data loss on a schedule the user did not choose.

Suggested change: after `ledger_enable` and `ledger_verify`, set the source's ledger policy and say so in the report
(or add it to `acknowledgeLedgerRestart` text if it will be left unset); import timepart policies with
`enabled = false` and list them in the report with a button or call to enable. Both items belong in the audit event.

### DXR-19 (medium): locale, encoding and extension schemas are not part of the manifest

Section: "The archive", "The task `database.import`" steps 4 and 5.

`createDB` makes the database from `template0` with the requested encoding and, if given, `LC_COLLATE`/`LC_CTYPE`
(`ops/databases.go`); the import has no input for them, so a source in `de_DE.UTF-8` (or `C`) lands in the default
locale. Unique indexes, range partition bounds on text, `ORDER BY`-dependent bucket checks, and `pg_dump`'s
`COLLATE` clauses (which name libc collations, absent on another host) change or fail. Also, `pg_dump` writes
`CREATE EXTENSION ... WITH SCHEMA x` and objects qualified by that schema (`public.citext`, `extensions.hstore`),
but step 5 installs the closed list "with the service role" with no schema.

Suggested change: write `database: {encoding, lcCollate, lcCtype, localeProvider}` and, per extension, `schema` into
the manifest; create the database with them (or refuse with `archive_incompatible` if the target cannot) and install
each extension into the recorded schema. Add the case to the round trip.

### DXR-20 (medium): the staged-download stream and its relay are under-specified in ways the implementation will hit

Section: "The streaming relay", "Download", F1 and F2.

The relay frame carries "operation id, path parameters, query, identity and proof". Not carried or listed:
`Range`, `Upload-Offset`, `Content-Length`, `Content-Type` of the chunk, source address for the audit record; and on the
way back `Content-Range`, `Content-Length`, `Accept-Ranges`, `ETag`, `Upload-Offset`, `Cache-Control`,
`X-Content-Type-Options` are missing from the `relayedHeaders` allowlist in `forward.go`. `forwardsToLeader` and
`leaderBoundReads` must name `downloadDataExport`, `getDataUpload`, `getDataExport`, `listDataExports` (all read the
leader's system database and disk). `guard.go` wraps every operation in `context.WithTimeout(RequestTimeout)` and
`MaxBytesReader`; the text says to extend deadlines with `ResponseController` but not to leave the guard's context
and body limit for these two operations, and the audit event of a download must be written at the end of the
stream (with `downloaded_at` and whether the last byte was served), after a status line of 200 was already sent.
A relay that fails mid-stream cannot change the status: the browser gets a short file with no error, and nothing in the
text tells the client to check the `ETag`/SHA-256 or the `Content-Length`.
HTTP/2 deadline behaviour of `ResponseController.SetReadDeadline` should be tested rather than assumed.

Suggested change: list the header allowlists and the exact frame in the design; add a "streaming spike" to increment
1 that proves a 5 GiB upload and download through a follower, with a mid-stream kill on HTTP/1.1 and HTTP/2; make the
download set `Content-Length` so a truncation is visible to the browser; add the SHA-256 and size to the dialog so
the user can verify.

### DXR-21 (medium): sizing and ordering of the increments

Section: "Increments".

- Increment 6 (archive writer and validator, hostile corpus of about 30 cases, lexer, classifier, normaliser with
  golden files on two majors, three fuzz targets) and increment 8 (twelve import steps, the `OnFail` drop, console
  dialogs and the report page, the round-trip test on two majors) are each more than the 6 to 10 days of an "L"; I
  would size them XL (three to four weeks each). V1, V2, V3 and V6 are discovered inside increments 5 to 8 but change
  their design (DXR-06, DXR-07).
- Increment 3 (relay, uploads, download, deadlines, guards) is correctly L, but the relay is the part with no
  precedent in the code; it should be proved before the table features depend on it.
- Missing from the list: a spike for V1 (pg_dump output for each table kind on the real extension) and V6 (router
  `COPY` through `PgConn().CopyFrom`, command tag, `HEADER`, `\.`) before increment 1; the metrics and alerts for
  disk use and transfer counts; the upgrade story for `0035_data_transfer.sql` on followers; a contract-version
  release note; the console's browser limits (64 MiB `Blob.slice` bodies, session expiry during a 1 hour upload).

Suggested change: reorder to: spikes (V1, V6, relay) -> 1 -> 2 -> 3 -> 4 and 5 in parallel -> release; then
6, 7 (export and validate only), then 8 behind its own review. Re-estimate 6 and 8 after the spikes.

### DXR-22 (low): `allowCode` adds exposure and little function

Section: "`allowCode`", classifier rows for functions and triggers.

- The extension refuses to distribute a table that has triggers or any default, check, generated column or index that
  calls a function of a non-superuser (`function_owner_not_superuser`, README of `kingres_shard`), and refuses
  views over such tables for distribution. Archive functions are owned by the importing (non-superuser) role, so
  `allowCode` is useful for local tables only; for any table the archive wants distributed it makes step 7 fail.
- The table row "no `SECURITY DEFINER` unless `allowCode`" is vacuous because the whole row needs `allowCode`; say
  "never `SECURITY DEFINER`" or explain the threat.
- Archive code that runs later (triggers on local tables, functions in index expressions) is run when superuser
  processes touch those tables (a restore by an operator, `ANALYZE`/`REINDEX` as the extension owner, background
  workers). The timepart extension isolates the owner's code from its worker (README), the other paths are the
  operator's responsibility; the design should say which service-role steps of the import run after code objects
  exist (steps 7 to 12: all do) and run them with `search_path = pg_catalog` and `session_user` hygiene, or create
  functions and triggers last.

Suggested change: leave `allowCode` out of the first release (refuse functions and triggers always); if kept, move
function and trigger creation after step 12 and run service-role steps with a fixed `search_path`.

### DXR-23 (low): the creator-only rule and the proxy map are weaker than the text says

Section: "Who may do what", ADR 0027 consequence, F3.

- `kingres_admin` PG roles have `CREATEROLE`, so an administrator can often reset another role's password or add
  itself to a role it administers and then sign in as the creator; creator-only protects against accident and
  against administrators without those rights, not against a determined colleague. Present it as defence in depth
  (the audit trail and the setting are the controls).
- The `Proxy` map keeps a role connectable until `kingresd` restarts. A transfer of up to 24 hours therefore keeps
  running with the privileges of a role that has logged out, been demoted from `kingres_admin`, or had its password
  changed. Re-check the cluster role and the role's `rolcanlogin`/`rolvaliduntil` at the start of each step.
- `created_by` is a role name. If a role is dropped and a new role created with the same name, it inherits the files
  and uploads of the old one until they expire. Store the role OID plus name and `rolcreatedat`-like identity (or
  delete a role's staged files when the role is dropped).
- A superuser administrator can export but not import; that is stated. Check that the export path does not rely on
  `refuseDangerousRole` for the `COPY ... TO PROGRAM`-style attack surface (it does not: no file or program name
  is built), which is correct.

### DXR-24 (low): master key and the segment format, small gaps

Section: "Encryption at rest".

The cryptography is sound (per-file key, authenticated segments with a last-segment flag, wrapped key outside the
directory). Gaps in operation: two first uses at once can both create `transfer.key` (create with `O_EXCL`, write,
fsync, rename, re-read); a missing or replaced key file must turn `ready` rows into `lost` with a clear code and
must not create a new key silently while files under the old key exist; the key file is in the data directory and so
in file-level backups of it (the stated limit, fine, but put it in the README); check mode 0600 and ownership at every
start. An upload appends at arbitrary byte offsets while segments are fixed 64 KiB: the writer must hold the tail of
a partial segment across requests (or only accept offsets that are multiples of 64 KiB except the last chunk), and the
upload's offset must be derived from the file, not from the row, after a crash. The incremental SHA-256 state cannot
survive a restart: say that completion re-reads the file once.

### DXR-25 (low): CSV details

Section: "Table import", "CSV files".

- Client-only encodings in the list (`SJIS`, `BIG5`, `GBK`): a byte-oriented record scanner is safe with the default
  quote (0x22 and CR/LF are not trail bytes) but not with an arbitrary one-byte `quote`. Restrict `quote` to ASCII
  below 0x40 for those encodings, or make the scanner encoding-aware.
- `header: match` requires the control plane to parse the first record (CSV quoting, encoding), contradicting "never
  parses values". PostgreSQL 15 and later have `HEADER MATCH` on `COPY FROM`; use it where the router accepts it (V6)
  and keep the control-plane check only for the message.
- A UTF-8 BOM is stripped only at the start of the decompressed stream; say that a BOM before a gzip member is data,
  and that NUL bytes are refused by PostgreSQL (the message must not echo them, DXR-03).
- The default `columns` is "the table's columns in table order" including generated columns; `COPY` refuses a value for
  a generated column. Default to the non-generated columns and document that a table export file has more columns than
  an import takes (`header: match` of a file from `exportTable` will then fail unless the column set agrees).
- `COPY FROM` into a local table with row-level security is refused by PostgreSQL; the design lists only the
  distributed case.
- Reference tables are "always batches" while `onError` defaults to `allOrNothing`: say what the API does (422 or
  silent switch).

### DXR-26 (low): the spreadsheet guard

Section: "Spreadsheet safety".

The pattern covers `= + - @`, tab and CR. Common additions are a leading `|` and `%` for some spreadsheets, full-width
`=`, `+`, `-`, `@`, and a leading no-break space before a formula character. It misses `json`, arrays and intervals
(already listed). It is a guard for the person's convenience, not a boundary; keep it, extend the list, and add the
text "not a security feature" to the dialog.

### DXR-27 (low): archive checksum entered by the user

Section: `expectedSha256`, `createDataUpload` field `sha256?`.

A browser cannot hash a 20 GiB file incrementally with the Web Crypto API (no streaming digest), so the client-side
checksum is impractical for the sizes the feature targets; the useful flow is "export shows the SHA-256, import
dialog asks for the SHA-256 the operator got elsewhere" (already the design's `expectedSha256`). Say that
`sha256?` on upload is for command-line clients, and compare the two values in the console with the displayed text.

---

## What I found sound

- Staging plus encrypted files rather than streaming the `COPY` to the browser (F1, F2 are accurate; the
  `ledger.bundle` precedent exists, `leaderBoundReads` lists `getLedgerBundle`).
- Passing no file name or program to PostgreSQL: the importer uses `COPY FROM STDIN` only, and the exporter only
  `COPY (SELECT) TO STDOUT` on names that passed `sqlx.NewIdent`. COPY TO or FROM PROGRAM and server files cannot be
  reached by the transfer code or by the archive (the classifier refuses `COPY`, and `refuseDangerousRole` is the
  real bound for the role).
- Running imports as the signed-in role, never as the service role, with the same refusal of dangerous roles as the
  workbench (`dangerousRolesSQL` correctly follows inherited and `SET`-able memberships).
- Distribution, ledger and timepart policy from a structured, validated document with the service role
  rather than from SQL in the archive; the extension's own refusal of untrusted code on distribution backs this.
- The version rules (target major at least the source major) and the all-reasons answer.
- Creator-only listing with 404 for others, a server-built `Content-Disposition`, `nosniff`, `no-store`, TLS-only
  transfer operations, audit of downloads and of chunks-as-a-whole, the audit-sink-ready precondition.
- Per-file keys, authenticated segments with a final flag, key outside the staging directory; no new dependency.
- The archive format: central directory for a table of contents, one pass of SHA-256, no entry ever written to a
  file system, fixed names, manifest limits and `DisallowUnknownFields`.
- The explicit statement of consistency (statement snapshot per group, `per-group` label) matches the README
  ("Atomicity is not a global snapshot"); the labels are honest; the "export twice, compare sums" advice is correct
  for one table.
- The ledger decision (ADR 0028): carrying data and starting a new chain with a required acknowledgement is the
  right trade-off; the wording of what the verifier says is accurate. (See DXR-18 for the policy.)
- Leader-change policy "end the transfer, adopt nothing" is simple and safe for exports and uploads.
- Mutation list and the hostile corpus are a good test plan; add the cases named in this review.
- Memory bound: the pipeline holds one record plus fixed buffers; `maxConcurrent` bounds the number of pipelines.
- Identifiers: every name from JSON goes through `sqlx.NewIdent`; bucket count range-checked.

## Recommended first cut (phase 14a), and what to defer

Ship a release with:

1. Contract, setting `dataTransfer.enabled` (off), migration, audit events, staging store with quota, reserve,
   janitor (with the DXR-17 safeguards) and sealed files (increments 1 and 2, plus the relay spike).
2. Streaming relay, resumable upload, download with `Range` (increment 3).
3. **Table CSV export** for local, reference, distributed and partitioned tables (increment 4), with the DXR-03,
   DXR-11 and DXR-16 changes.
4. **Table CSV import**, `allOrNothing` only (explicit `BEGIN`, count check before `COMMIT`, DXR-02), limit
   `maxAtomicBytes` (say 2 GiB at first), targets local and distributed tables, ledger tables through the capture
   trigger; reference tables either excluded or through the batch path restricted to files of at most a few hundred
   MiB with the marker-table approach (DXR-01 b). No `batches` mode, no `skipRows`, no "continue" button until
   DXR-01 is solved; the answer to a large file is "split it".

Defer to later phases, each behind its own review:

- Phase 14b: **database export** and **archive validation only** (read-only, no import), with the V1 spike, the
  supported-subset statement, the exporter self-check through the validator, snapshot options (DXR-13, DXR-14) and
  large-object refusal (DXR-09). The result is useful alone (an auditable logical copy an operator can inspect
  and restore by hand with `psql`, with the schema parts reviewed).
- Phase 14c: **database import** with the shape-grammar classifier (DXR-04, DXR-05), private-name database creation
  (DXR-10), views after distribution (DXR-07), locale and extension schema (DXR-19), ledger and timepart policy
  handling (DXR-18), no `allowCode`, no `privileges: restore`, and the 18-to-19 round trip.
- `batches` mode with marker table as 14a+.

Rationale: 14a is roughly increments 1 to 5 minus the batch features and carries none of the three hardest risks
(untrusted SQL, normalising `pg_dump` for a sharded schema, cross-table consistency). It still answers the owner's
request for export and import in the console for the data people actually move around, and it leaves the archive
design time to meet V1 on a real cluster.

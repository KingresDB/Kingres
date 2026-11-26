<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Review of the fixes of phase 14a (DX14-01 to DX14-12), branch dx-cp, commits 3b87662784, 63e8de8d88, 97976b3c44

## Summary

DX14-01 is fixed as designed: the marker is atomic with the data, the aborted-row protocol gives no wrong answer, and three mutants of it died.
The marker has one new high defect: the schema kingres_transfer is created lazily and is not checked for ownership, so a database owner can
plant the schema first and run code as the service superuser (DX15-01). The SECURITY DEFINER function is callable by every role and can forge
or block markers of other roles' tasks (DX15-02). Both are fixed by creating and checking the marker objects as the service.

Run: unit tests of transfer, staging and ops on Windows (pass); the import, export and staging end-to-end tests on PostgreSQL 18 (pass, lane l4)
and the key ones on PostgreSQL 19.0 (pass); four mutants; one SQL experiment on a private PostgreSQL 18 instance.

| Severity | Count | Ids |
|---|---|---|
| high | 1 | DX15-01 |
| medium | 2 | DX15-02, DX15-03 |
| low | 4 | DX15-04 to DX15-07 |

## Verdicts on the findings

| Finding | Verdict | Note |
|---|---|---|
| DX14-01 | Fixed (with DX15-01, 02, 03 on the marker's surroundings) | see Question 1 |
| DX14-02 | Fixed, one low gap | adoption in openXfer; DX15-05 |
| DX14-03 | Fixed, one low gap, step-time check untested | DX15-04; mutant R4 survived |
| DX14-04 | Fixed | pattern and RiskyCell agree; unit and e2e tests pass; DX15-07 (round trip) |
| DX14-05 | Fixed | export step repeats table, column and untrusted-code checks |
| DX14-06 | Fixed by reading | untested, as the review says |
| DX14-07 | Fixed | no transaction lives between steps |
| DX14-08 | Fixed | isGzip on the staged file; test passes on both majors |
| DX14-09 | Fixed | column types compared in the check-table step |
| DX14-10 | Fixed | BOM only for UTF-8 |
| DX14-11 | Fixed | xml as text; janitor re-reads the highest id |
| DX14-12 | Fixed | lock around limit check and start; record removed on refusal |

## Question 1: the marker

1. Atomic on a distributed table. Yes. `mark` is a plain local statement of the load transaction on the coordinator. kingres_shard commits a
   multi-writer transaction by PREPARE on every remote writer, then a dtx_log row written by the local transaction, then the local commit, then
   COMMIT PREPARED (node_conn.c, header comment and line 3084). The marker gives the local transaction an xid, so the local node counts as a
   writer, and even a one-group load now takes the two-phase path. The local commit is the single decision: the marker is committed exactly
   when the decision is. The marker is not "in" each group's part; it does not need to be.
2. In-doubt prepared transaction containing the marker. Not possible: the coordinator's own transaction is never prepared (PREPARE of the whole
   transaction is refused). Groups hold prepared transactions without the marker. If the coordinator dies before the local commit, the marker is
   absent and recovery rolls the groups back, so "aborted" is correct. Reverse case: marker committed, COMMIT PREPARED pending on a group; the
   task says done a moment before the rows of that group are visible (DX15-06).
3. The 60 s wait. INSERT ... ON CONFLICT DO NOTHING waits on the xid that holds the key (the loader, possibly inside the commit, including the
   sync-replication wait, where the xid still counts as running). After lock_timeout it errors and the code answers import_outcome_unknown, the
   honest answer. The intent is written after `mark` ran, so a transaction that could still commit always holds the key. It cannot end with the
   wrong answer (reading, and mutants R1 to R3). The isolation level is READ COMMITTED, so the SELECT after DO NOTHING sees a concurrent commit.
4. The function. search_path is pg_catalog and every name is qualified: sound. The owner is the service role, so it can write only one
   'committed' row. But EXECUTE is granted to PUBLIC and nothing ties the task id to the caller (DX15-02), and the schema and table are not
   checked for an earlier owner (DX15-01).
5. Forge or delete. Delete: not possible while the service owns the table; possible through DX15-01. Forge: any role that can connect can call
   mark(id, n) (DX15-02).
6. Database dropped and recreated with the same name: the marker table goes with the database. A task re-run after that gets 42P01 in
   decideOutcome and answers import_outcome_unknown, which is acceptable. A database restored or cloned into another cluster keeps its markers
   (DX15-03).

## New findings

### DX15-01 (high): a database owner can plant kingres_transfer first and run code as the service superuser
kingres/control-plane/internal/ops/transfer_import.go:572-588 (ensureMarker), :596-606 (decideOutcome).
ensureMarker uses CREATE SCHEMA IF NOT EXISTS and CREATE TABLE IF NOT EXISTS and never checks who owns what exists. The owner of a database has
CREATE on it, so before the first import into that database (every database that existed before 14a, and every new one until its first import)
the owner can create schema kingres_transfer, a table import_marker with the same name, and a trigger on it. decideOutcome then inserts as the
service role (a superuser) into the owner's table, and the trigger runs as the service role.
Reproduced on PostgreSQL 18 in a private instance: as the owner, schema, table and a BEFORE INSERT trigger that runs CREATE ROLE pwned SUPERUSER;
as the service, the statements of ensureMarker and then the insert of decideOutcome. Result: role pwned exists with rolsuper = t. The same
planting lets the owner delete or pre-insert markers and replace the body of mark (CREATE OR REPLACE keeps the owner), which breaks the proof of
DX14-01.
Fix: create the schema, table and function when the database is created (the service owns them from the start), and make ensureMarker fail
closed unless schema, table and function are owned by the service role (nspowner, relowner, proowner) and the table has no triggers or rules;
no silent IF NOT EXISTS. For old databases, an upgrade step that removes a foreign-owned kingres_transfer after a warning. Add a test that plants
the schema as the owner and expects a refusal.

### DX15-02 (medium): any role can forge or block a marker of any task
transfer_import.go:577-580.
GRANT EXECUTE ON FUNCTION kingres_transfer.mark TO PUBLIC and USAGE on the schema to PUBLIC; the comment says a caller "can only make that task
fail on the key". That is not all. (a) A role commits mark(N, x) for the next task ids: every other role's imports with those ids fail with
"marker could not be written" (cheap cross-role denial of service, ids are sequential). (b) A role commits mark(N, x) for a task of another role
whose commit acknowledgement was lost: decideOutcome reads 'committed' and the task ends done with rows that were never stored (data loss
reported as success). (c) A role holds a transaction that called mark(N) open: decideOutcome waits 60 s and answers unknown.
Fix: make the call carry a proof only the control plane can compute, for example mark(t, n, tag) where the function checks
tag = hmac(secret, t), the secret living in a service-owned table that the function reads as definer; the control plane reads it over a service
connection and sends the tag.

### DX15-03 (medium): task ids are not unique over the life of the marker table
transfer_import.go:567-625, :711-713.
The key is the task id alone, from the sequence of the system database of one cluster. A database dumped and restored or cloned into another
cluster, or a system database restored from an older copy, carries markers with ids that a new task will get. A foreign 'committed' marker makes
a new task whose acknowledgement was lost end as done with nothing stored; a foreign marker of any state makes every load with that id fail on
the key. Markers are also never deleted, so the table grows without bound.
Fix: key by (cluster id, task id) or a random 128-bit nonce kept in the task parameters, and delete markers of finished tasks after the
retention of tasks.

### DX15-04 (low): any error of the role check becomes a permanent 403
transfer_import.go (request: StartImport, the refuseDangerousRole branch; step: stepImportLoad).
refuseDangerousRole returns an error for a failed query too (a dropped connection, a timeout), and both callers answer Forbidden with the text
about the SUPERUSER attribute and retryable false. A transient fault ends the task for good with a wrong reason. Fix: separate the answer
(bad = true) from a query error, and give the latter session_lost, retryable. The step-time check is untested: mutant R4 survived. Add a test
that gives the role BYPASSRLS between the request and the step.

### DX15-05 (low): adoption failure is silent and final for the process
kingres/control-plane/internal/ops/transfer.go:137-145. If OnServer fails when the store opens, the error is dropped without a log line and
xferSt stays set, so the quota stays forgotten until the next restart. Fix: log it, and do not keep xferSt when the adoption read fails.

### DX15-06 (low): done can precede visibility on a group
A committed marker means the local commit happened; COMMIT PREPARED on a group may still be pending (recovery.c finishes it). The task says
"committed N rows" and a SELECT on the distributed table may briefly miss that group's rows. State it in the contract text of the import.

### DX15-07 (low): an exported header does not match its own table on re-import
transfer/csvopts.go (alias with an apostrophe): a column whose name begins with = + - @ | % gets a header cell with a leading apostrophe, so a
file exported with the guard and the header, then imported with header = match, fails with import_header. Fix: say it in the dialog and the
contract, or point to header = skip for such a file.

From reading only: ensureMarker runs CREATE OR REPLACE FUNCTION and GRANT on every import and can fail with "tuple concurrently updated" or a
duplicate key on pg_namespace when two roles import into a fresh database at the same moment (the one-task rule is per role, not per database).
Fix with an advisory lock around it, or do it once at database creation (DX15-01).

## Other fixes checked

- DX14-03: dangerousRolesSQL covers the attributes and the predefined roles reached by SET or INHERIT; it runs at the request and in the load
  step before BEGIN. The export `OwnerIsRole` as `relowner = oid of current_user` is stricter than pg_has_role for members of an owner role: they
  now get the acknowledgement question and the RLS refusal (line 493) instead of an RLS error at run time. Correct; a usability change only.
- DX14-02: adoption runs after the store and keys open and before the first sweep; records in receiving, building and ready are adopted, and the
  janitor frees them when the row is removed or expired. Test passes on both majors.
- DX14-04: the Go RiskyCell and the SQL spreadsheetPattern accept the same set; clipName cuts on a character boundary within 63 bytes.
- DX14-05: the export first step re-reads facts, kind and untrusted-code columns as the owner of the task; the acknowledgement is honoured.
- DX14-08: isGzip reads two bytes of the staged file; a plain file over the cap is refused, a gzip file is bounded by LimitedExpansion on the
  decompressed size; an explicit compression value overrides the magic bytes. Correct.

## Mutants run

| Id | Mutation | Result |
|---|---|---|
| R1 | the load transaction does not call mark | killed (5 tests fail) |
| R2 | decideOutcome does not insert the aborted row | killed (TestImportCommitFaultPoints) |
| R3 | a run that finds the intent loads again instead of asking the marker | killed (TestImportCrashAfterCommit) |
| R4 | refuseDangerousRole removed from the load step only | survived (DX15-04) |

Not mutated, by reading only: the 60 s lock_timeout, the server comparison before decideOutcome, and atomicity of mark with a remote writer (no
multi-group test of a lost acknowledgement exists).

Cleanup: the worktree, the private installs and the test instance were removed; nothing was committed or pushed.

## Fixes

Fixed on `dx-cp` by the writer. The tests ran on PostgreSQL 18 and 19; the mutant column says what was removed to show that the test fails without the fix.

| Finding | Fix | Test | Mutant |
|---|---|---|---|
| DX15-01 (high) | `ensureMarker` (new file `internal/ops/transfer_marker.go`) runs as the service role under an advisory lock: a schema `kingres_transfer` that exists with another owner stops it before anything is created; after the creates it checks owner, tables, triggers, rules, policies, columns, defaults, other functions and the exact body of `mark`, and nothing is granted or written until the check passes; a failed check is `forbidden` (an operator removes the schema). Every statement on the schema runs with `search_path = pg_catalog`. An upgrade step was not written: the feature is unreleased. | `TestMarkerSchemaPlantedByTheDatabaseOwner` (the reviewer's escalation: schema, table and a trigger that creates a superuser; the import fails, no role `kg_pwned`, and works after the schema is dropped) | foreign-owner check off |
| DX15-02 | `mark(nonce, task, rows, tag)` inserts only when `tag = sha256(secret || nonce:task:rows)`; the secret is a service-only table; the control plane computes the tag. A role has USAGE and EXECUTE only (nothing blocks `DROP ROLE`), cannot read the secret or the table, cannot forge or hold a marker. | `TestMarkerCannotBeForgedByARole` | tag check removed from the function |
| DX15-03 | The marker is keyed by a random 128-bit nonce in the task parameters; rows older than 30 days are deleted at each `ensureMarker`. | `TestMarkerKeysAreNoncesAndOldOnesArePruned` (5000 foreign task ids beside, an old row pruned) | prune removed |
| DX15-04 | `dangerousRole` separates the answer from a query error: an error is `transfer_session_lost` (retryable) in the step and the request's error at the request; only a positive answer is `forbidden`. | `TestImportRoleChangedBeforeTheLoad` (kills R4: BYPASSRLS granted after the request) | the check removed from the load step |
| DX15-05 | A failed read of the staged files when the store opens is logged as an error and the store is not kept, so the next use opens it again and adopts; the request gets 503. | not covered by a test (the read of the system database cannot be made to fail in the harness) | |
| DX15-06 | Stated in the contract text of `importTable` and in the README: `done` means the local commit; a group with `COMMIT PREPARED` pending may show its rows a moment later. | text only | |
| DX15-07 | Stated in the contract text of `exportTable` (spreadsheetSafe) and in the README: import such a file with `header: skip`. | text only | |
| ensureMarker race | The advisory lock, and the function is created only when it is absent (no `CREATE OR REPLACE` on every import). | `TestFirstImportsIntoAFreshDatabaseDoNotCollide` (a smoke test: the race is not deterministic) | not shown |

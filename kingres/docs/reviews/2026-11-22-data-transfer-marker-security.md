<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Security review of the marker fixes (DX15-01 to DX15-07), commit a35856d2f6 on dx-cp

## Summary

The design closes the ways a plain role or a database owner can plant or replace the marker objects (DX15-01) and removes the
cross-role forge and block of DX15-02 for every role that cannot read the secret. The tag construction is not broken. Four
weaknesses remain, all reproduced on a private PostgreSQL 18 instance (probe scripts kept by the reviewer):

| Severity | Count | Ids |
|---|---|---|
| high | 1 | DX16-01 |
| medium | 2 | DX16-02, DX16-03 |
| low | 4 | DX16-04 to DX16-07 |

Run: a read of the commit and of the final transfer_marker.go, transfer_import.go and ServiceConn; one probe script on a private
PostgreSQL 18 instance (service superuser, database owner, plain role, pg_read_all_data and pg_write_all_data members). No Go test was
run: the findings are in SQL semantics, and the Go side was read. No mutant of the Go code was run.

## Verdicts

| Finding | Verdict | Note |
|---|---|---|
| DX15-01 (planted schema runs code as the service) | Fixed for the marker objects; a sibling hole remains | the verify step runs before any insert or grant and fails closed on a foreign owner; but the service session itself is open to a database-level search_path (DX16-01) |
| DX15-02 (forge, pre-empt, hold by any role) | Fixed against plain roles; not against members of pg_read_all_data or pg_write_all_data | DX16-02 |
| DX15-03 (ids not unique, table grows) | Fixed | 128-bit nonce, 30 days pruning; DX16-06 on the retention |
| DX15-04 (role check errors) | Fixed | query errors give session_lost, retryable |
| DX15-05 (adoption failure) | Fixed | store not kept, error logged, 503 |
| DX15-06 (done before visible) | Not changed here | documentation only |
| DX15-07 (header round trip) | Not changed here | documentation only |

## Question 1: can a non-superuser still run code as the service role?

Yes, by one path that is not in the marker objects (DX16-01). On the objects of the schema themselves: no.

What I checked and found sound:
- Order in ensureMarker. Nothing runs as the service on an object of the schema before `markerVerifySQL` passes. The foreign-owner
  test, `CREATE ... IF NOT EXISTS` (a no-op on an existing object), the verify query, and only then the grants, the prune and the secret
  insert. If the database owner plants the schema between the owner test and CREATE SCHEMA (the advisory lock binds only the service),
  the verify sees `nspowner <> service` and fails closed. Objects the service made inside that schema are left behind (an owned
  function and tables in a foreign schema) but are never granted, read or written; every later call hits the foreign-owner branch.
- Event triggers need a superuser to create. Inheritance children, partitions, a table in the schema, an index, a statistics object or
  an extension in the schema all need ownership of the parent or CREATE on the schema, which only the service has once it owns the
  schema. relispartition and relkind 'p' are rejected anyway. TOAST relations are allowed by relkind 't' and belong to the table.
- Default ACLs: ALTER DEFAULT PRIVILEGES for the service role needs to be the service. Not reachable.
- Operators and casts inside the marker statements: decideOutcome and ensureMarker set `search_path = pg_catalog` as the first statement and
  use qualified names, and pg_catalog is searched first unless named later in the path. Sound. markerSecret has no operator. stepImportCommit
  does not set the path: DX16-01.
- A function `mark` of another schema reachable through the caller's search_path: the call is `kingres_transfer.mark(...)`, qualified, with
  four typed arguments, and the schema holds exactly one function (verified: `count(*) = 1`).
- The SECURITY DEFINER function: `SET search_path = pg_catalog` in proconfig (compared exactly), qualified names, no dynamic SQL, owner =
  service (compared), body compared byte for byte. Caller GUCs that a non-superuser can set (datestyle, bytea_output, client_encoding,
  plpgsql.*) do not change the result: the message is built from `text` and `bigint::text` and `convert_to(..., 'UTF8')`.
- Time of check to time of use: the verify runs at StartImport; the secret is read and mark is called when the load step runs, later, with no
  new verify. For a plain role this window is harmless because the objects are service-owned and cannot be altered by it. It matters only
  for the roles of DX16-02. Cheap to close: verify again in markerSecret (same transaction, same query).
- Advisory lock key 7355608: a constant that any role of the database can take. DX16-04.

### DX16-01 (high): the service session has no search_path of its own; a database owner can run code as the service superuser

kingres/control-plane/internal/ops/transfer_import.go:828-830 (stepImportCommit); internal/cluster/cluster.go:333-345 (connect: no search_path in
RuntimeParams); the same pattern in every other asService query on a user database (transfer_import.go:90-97 ledgerPolicy, transfer_export.go:202-
distribution, both with `relid = to_regclass($1)`).

Scenario, reproduced (evil.sql, commitq.sql). The owner of the database, a non-superuser, runs
`CREATE SCHEMA evil; CREATE FUNCTION evil.eq(text,text) ...; CREATE OPERATOR evil.= (leftarg=text, rightarg=text, function=evil.eq);
ALTER DATABASE app SET search_path = evil, pg_catalog;`. A database-level setting applies to every connection to the database, including the
service superuser's. Naming pg_catalog after evil makes evil searched first, so `nonce = $1` (text = text) in stepImportCommit resolves to
evil.= and the SQL function runs as the service superuser. In the probe the function recorded `svc/super=true`. With `SET LOCAL
search_path = pg_catalog` first, the same query returned the marker row and ran no foreign code; a startup option (`PGOPTIONS=-c
search_path=pg_catalog`) also overrides the database setting. The import of any role into that database ends in stepImportCommit, so the owner
triggers it by importing once. The same owner can plant it before the first import (before any marker exists), so the DX15-01 verify cannot see
it: it is a property of the session, not of the schema. The effect is superuser, the same as DX15-01 and worse than any marker forge.

Fix: in ServiceConn/connect set `cfg.RuntimeParams["search_path"] = "pg_catalog"` (startup parameters outrank database and role settings), and
also run `SET LOCAL search_path = pg_catalog` as the first statement of asService/inTx for every service transaction on a user database. Add
a test that sets the database path to an evil schema with an operator and runs stepImportCommit, ledgerPolicy and distribution.

## Question 2: the tag

Is sha256(secret || message) safe here? Yes in this exact deployment; I would still change it, because the safety rests on an accident.

Length extension. An attacker who holds the tag for the message m = nonce:task:rows can compute the tag of m || padding || suffix
without the secret. Padding of SHA-256 always contains the byte 0x80 and, for any short message, NUL bytes (the high bytes of the
8-byte length). The message the function hashes is built inside the function from a `text` nonce, `p_task::text` and `p_rows::text` and
converted with convert_to(..., 'UTF8'); a PostgreSQL text value cannot hold a NUL byte, and a lone 0x80 is not valid UTF-8. So no
forged (nonce, task, rows) can produce a padded message, and the extension is impossible by the types. Also the message is unambiguous:
the nonce is exactly 32 characters, then ':' and two decimal integers. It is not a property that survives a change of format (a bytea
or a binary field in the message, a longer or hex-only nonce with an encoding that can carry 0x80 and NUL, a second message type under the
same secret), so it is a trap.

Who can see a tag, and who can read the secret:
- The tag travels as a bind parameter of the loader session. pg_stat_activity and pg_stat_statements show `$4`, not the value; the plan
  cache holds no parameters. Parameters appear in the server log only with log_statement = all, log_min_duration_statement or
  log_parameter_max_length, and the log needs pg_read_server_logs, which the import refuses (the role of the import) but the owner of the
  server has. Error messages of mark say `invalid marker` and leak nothing; a key conflict names the key only after the tag is valid.
  A tag is valid for one (nonce, task, rows) and cannot be used for another nonce.
- The secret: only the service role and superusers read the table. A member of pg_read_all_data reads it directly (reproduced: the
  probe returned the 32 bytes). pg_dump and base backups carry it (anyone with the dump can compute tags for that database, but still needs
  an unknown nonce). The refused predefined roles of the import list do not include pg_read_all_data and pg_write_all_data: DX16-02.
- The secret is also lost on a restore into a database that has no secret row: DX16-03.

Recommendation, the minimal sound construction, with no extension: keep SHA-256 but use two derived keys and an outer hash, which is
HMAC's nested form without the 64-byte pads and so needs no bytea xor:
`tag = sha256(k2 || sha256(k1 || m))`, where `k1 = sha256('1' || secret)` and `k2 = sha256('2' || secret)` (or store two independent 32-byte keys).
Add in the same change: `p_nonce ~ '^[0-9a-f]{32}$'`, `p_tag IS NULL OR length(p_tag) <> 32 OR tag <> p_tag` (see DX16-03), and the
function marked STRICT. pgcrypto's hmac() is clearly better when the extension exists, but it is not guaranteed in the user's database
and the service cannot assume it; if it is present it must be called as `public.hmac` only after checking its owner, which is more
surface than the nested form. md5 or sha224 do not help: sha224 truncates the state (not extendable by the same attacker without guessing
the 32 bits) but is a smaller change in name only.

## Question 3: forge, pre-empt, block, hold, learn

For a role that is neither a superuser nor a member of pg_read_all_data or pg_write_all_data:
- Forge: needs a tag, which needs the secret. No.
- Pre-empt or block a task: needs the nonce, 128 bits random, which lives in the task parameters of the system database and in the
  bind parameters of the loader. No. A row for an invented nonce harms nobody.
- Hold: a transaction that holds a key needs the same tag. No. The advisory lock of ensureMarker can be held: DX16-04.
- Learn another task's outcome: import_marker is not readable; mark errors say the same for every wrong input. No.

For a member of pg_write_all_data or pg_read_all_data (the privilege checks of tables are bypassed; RLS is not):
DX16-02.

### DX16-02 (medium): pg_read_all_data and pg_write_all_data bypass the table privileges of the marker; the DX15-02 protection ends there

transfer_marker.go:55 (the verify rejects relrowsecurity), :115-116 (the revoke), query.go DangerousPredefinedRoles.
Reproduced (probe P1b, P1c): a member of pg_read_all_data reads kingres_transfer.secret; a member of pg_write_all_data runs
`UPDATE kingres_transfer.import_marker SET state = 'aborted'` (no WHERE, no RETURNING; with them PostgreSQL asks for SELECT and refuses),
`UPDATE kingres_transfer.secret SET k = '\x00'` and `DELETE FROM kingres_transfer.secret`. Consequences:
- flipping a committed marker to aborted makes the task answer "not stored, repeat the operation": the retry loads the rows again, that
  is the doubled rows of DX14-01, made by another role at the right moment (after a lost acknowledgement);
- with the secret known, a role forges tags for any (nonce, task, rows) it learns (a pg_read_all_data role also reads every committed
  nonce);
- deleting the secret is DX16-03.
Such roles are administrators of a database, but the marker's claim was that no role can touch it. These predefined roles are grantable by
the API (harmlessPredefined) in the same way as the data roles.

Fix (probe P1d, checked): in ensureMarker, `ALTER TABLE ... ENABLE ROW LEVEL SECURITY` on both tables with no policy (the service and the
SECURITY DEFINER function are superuser and bypass it; pg_read_all_data and pg_write_all_data do not carry BYPASSRLS, so they see 0 rows
and change 0 rows; the mark of a plain role still worked under RLS), and change the verify to require `relrowsecurity` and zero policies
instead of rejecting it. The import should also keep refusing roles with BYPASSRLS, which it already does.

### DX16-03 (medium): mark accepts a NULL tag when the secret row is missing

transfer_marker.go:36 (`... IS DISTINCT FROM p_tag`).
If the secret row is absent, `(SELECT s.k FROM kingres_transfer.secret s)` is NULL, the hash is NULL, and `NULL IS DISTINCT FROM NULL` is
false. Reproduced (probe P2): after `DELETE FROM kingres_transfer.secret`, a plain role ran `SELECT kingres_transfer.mark(<any 32
characters>, 9, 99, NULL)` and a committed row for task 9 appeared. The row is created, for the next ensureMarker only restores the secret
(the insert of the secret is `ON CONFLICT DO NOTHING` after the verify, so a deleted secret is restored by the next import into the
database, not before). The state needs the secret deleted (DX16-02, an operator, or a restore that left an empty table), so the severity is
medium and falls to low once DX16-02 is fixed; but the comparison is wrong in itself.
Fix: write the condition so that NULL means invalid: `IF p_nonce IS NULL OR p_tag IS NULL OR p_nonce !~ '^[0-9a-f]{32}$' OR
coalesce(sha256(...) = p_tag, false) IS NOT TRUE THEN RAISE`, and have the verify require exactly one row in secret, with length(k) = 32.
Add a test: delete the secret and call mark with a NULL tag.

## Question 4: all-or-nothing and no doubled rows

Yes, for the path of the service. The rows, the marker and the commit are one transaction as before; mark is the last statement before the
intent; a failed mark rolls back the load; decideOutcome runs only after the intent and inserts 'aborted' under the same unique key, so a
transaction that could still commit fails on it. The fix does not move any of these. The new code adds three ways of ending without a
marker, all safe: the secret cannot be read (session_lost, retryable, before BEGIN), the tag is wrong (mark raises, the load rolls back), the
verify fails (refused at the request). The doubled-rows outcome is reachable only through DX16-02 (a changed marker) and DX16-06 (pruned marker).

## Other new findings

### DX16-04 (low): the advisory lock key is a constant that any role can hold

transfer_marker.go:77 (`pg_advisory_xact_lock(7355608)`).
Advisory locks are per database and not owned by a role. Any role that can connect holds `pg_advisory_lock(7355608)` in a session (reproduced,
probe P4: the service call waited until it was killed) and every first import into the database hangs in ensureMarker, with no lock_timeout, until the
request's context ends. A collision with an application that uses the same key does the same. Fix: `SET LOCAL lock_timeout = '10s'` in
ensureMarker with a retryable error; better, serialise on an object only the service can lock (for example `LOCK TABLE
kingres_transfer.secret IN SHARE ROW EXCLUSIVE MODE` after a first creation, with creation done by catching the duplicate-object error and
verifying) so no key is shared; or use the two-integer form with a key derived from a hash of 'kingres_transfer' in the first int, which makes a
collision unlikely but does not stop a deliberate holder.

### DX16-05 (low): the verify does not compare constraints, indexes, the schema ACL or the type list

transfer_marker.go:47-67.
A table that the service owns can still carry a CHECK constraint or an expression index that calls a function, and a schema owned by the
service can carry a CREATE grant to a role. They cannot be made by a plain role in a schema that was created by the service. They can arrive in a
database that was cloned or restored by a superuser from a dump or a template (the case DX15-03 describes), where the owner name equals the
service role and the dump carries the objects. Fix: compare `pg_get_constraintdef` of both tables with the expected text, require that the only indexes
are the two primary keys (relkind 'i' count = 2), require `nspacl` to be exactly the owner's rights plus USAGE to PUBLIC, and reject extensions
that own objects of the schema (`pg_depend`, deptype 'e').

### DX16-06 (low): a marker is pruned after 30 days even if its task can still be asked

transfer_marker.go:119.
A task whose intent was written, and whose control plane stayed down for 31 days, finds no committed marker and inserts an 'aborted' row, so it answers
"not stored" for rows that were committed: doubled on the repeat. Whether a task can outlive 30 days depends on the task retention; make the
pruning depend on it (prune `aborted` and `committed` rows only when `at < now() - max(30 days, task retention + margin)`), or have the janitor delete a marker
when the task record is removed.

### DX16-07 (low): the secret is not verified at the step

transfer_import.go:622 and transfer_marker.go:135-141.
markerSecret reads the first row of whatever the table holds with no verify, and a task that waited in the queue loads against a state that was checked at the
request. Run the same verify, in the same transaction, as the read of the secret in markerSecret (cheap: one catalog query) and fail with
session_lost or Forbidden; it also closes the time-of-check window of Question 1.

## Cleanup

The worktree and the private install and test instance were removed; nothing was committed or pushed.

## Fixes

DX16-01 was fixed first on the branch `sp-fix` (commit 5430bb7099, from kingres-18) and merged into `dx-cp`: the three service connection openers send `search_path=pg_catalog` as a startup
parameter, and `asService` repeats it with `SET LOCAL`; `TestServiceSessionIgnoresTheSearchPathOfTheDatabase` reproduces the escalation (a superuser role exists afterwards without the fix).
The rest, on `dx-cp`, all in `internal/ops/transfer_marker.go` and `cmd/kingresd/review16_e2e_test.go`:

| Finding | Fix | Test |
|---|---|---|
| DX16-02 | `ENABLE ROW LEVEL SECURITY` on both tables with no policy; the verify requires it and zero policies. | `TestMarkerRowSecurityStopsAllDataRoles` (a member of pg_read_all_data and pg_write_all_data sees 0 rows and changes none) |
| DX16-03 | `mark` raises for a NULL argument, a nonce that is not `^[0-9a-f]{32}$`, a missing secret or one that is not 32 bytes; the tag is `sha256(k2 \|\| sha256(k1 \|\| m))` with k1, k2 derived from the secret; the verify requires exactly one secret row of 32 bytes; a missing secret is made again by the next import. The function is not STRICT on purpose: a STRICT function answers NULL to a NULL argument without an error, and the load would go on without a marker. | `TestMarkerNullTagAndBadSecret` |
| DX16-04 | No advisory lock. A later run locks the secret table (only the service role can); the first creation retries on the codes of a lost race; `lock_timeout` 10 s gives a retryable error. | `TestMarkerCreationTakesNoLockThatARoleCanHold` (a session holds the old key) |
| DX16-05 | The verify compares the constraints with those of two temporary tables made in the same transaction (as this server prints them), requires the two primary-key indexes only, the grants of the schema, tables and function, and no extension that owns an object. | `TestMarkerVerifyRejectsWhatItDidNotMake` (check constraint, index, create grant, select grant); the extension case is not tested |
| DX16-06 | Pruning (30 days) skips the markers of tasks that are still pending, running or paused. | `TestMarkerPruningKeepsUnfinishedTasks` |
| DX16-07 | `markerSecret` runs the verify in its own transaction before it reads the secret; a failure is `forbidden`. | `TestMarkerVerifiedAgainAtTheLoad` (hook `afterCheck`) |

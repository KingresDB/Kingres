<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Final security review of the marker hardening (DX16-02 to DX16-07), commits 648725ca1b and 91376dc4a6 on dx-cp

Method: read of transfer_marker.go, the import step in transfer_import.go (lines 615-700), the e2e test names and the diff of 91376dc4a6.
No test was run and no probe was built; the findings come from the code and from the PostgreSQL lock and privilege rules.

## Verdicts on the earlier findings

| Finding | Verdict | Note |
|---|---|---|
| DX16-02 (all-data roles touch the marker) | Fixed for data access | RLS is enabled on both tables; the verify requires it and zero policies. Locks are still open: DX17-02 |
| DX16-03 (NULL tag accepted) | Fixed | every NULL, the nonce shape, a missing or wrong-size secret raise; the verify needs exactly one 32-byte secret |
| DX16-04 (advisory lock) | Fixed for the key; replaced by a lock that a few roles can still hold | DX17-02 |
| DX16-05 (verify gaps) | Mostly fixed | constraints, indexes, ACLs and extension ownership are covered; smaller gaps in DX17-03 |
| DX16-06 (pruning) | Fixed | the open states pending, running, paused are all the non-final task states (the others are failed and done) |
| DX16-07 (verify at the step) | Fixed | markerSecret verifies in the same transaction as the read |
| Contract fields (DataExport.spreadsheetSafe, maxAtomicBytes) | No finding | read-only values, no secret, no new input |

## Answers to the questions

1. Read the secret, forge, flip, delete, block a marker, or run code as the service?
   - Read, forge, flip, delete: no, for plain roles, database owners, and members of pg_read_all_data, pg_write_all_data and pg_maintain.
     RLS with no policy protects the rows (the all-data roles bypass privileges, not RLS); pg_maintain has no read or write right.
     A role with BYPASSRLS plus an all-data role could read the secret; only a superuser can give BYPASSRLS, and the import refuses such a role.
   - Block: yes, by a lock (DX17-02). Data stay safe; the effect is a refusal or a hang.
   - Run code as the service: yes, by a race that needs a database owner (DX17-01). This is a regression against the order of the earlier version.
2. Verify gaps: triggers, rules, policies, defaults, constraints, indexes, TOAST (it belongs to the table), partitions and extension ownership are covered.
   Event triggers and FOR ALL TABLES publications need a superuser to exist. The gaps left are in DX17-03.
3. Lock on the secret table: a plain role has no privilege to lock it. Members of pg_write_all_data (write rights allow LOCK TABLE in every mode,
   ACCESS EXCLUSIVE included; even a ROW EXCLUSIVE conflicts with SHARE ROW EXCLUSIVE) and of pg_maintain (MAINTAIN allows every lock mode; pg_maintain is in
   harmlessPredefined, so the API can grant it) can hold it. pg_read_all_data holds only ACCESS SHARE, which does not conflict. See DX17-02.
4. Tag: sound. The inner hash is keyed with k1, the outer with an independent k2, the outer input is a fixed 32 bytes, the message is unambiguous (32 hex digits, ':',
   two decimals), and Go and SQL build the same bytes. Extending the inner hash gets nothing without k2. The comparison is not constant time, but a caller
   learns only "invalid marker" and a forged row needs the real nonce, so I do not count it.
5. Non-STRICT is correct: a STRICT function returns NULL on a NULL argument and lets the load go on with no marker. The IF tests IS NULL for every argument first, so no
   NULL reaches the comparison, which uses IS DISTINCT FROM.

## New findings

### DX17-01 (medium): the secret is inserted before the verify, so a schema planted in the race window runs code as the service

internal/ops/transfer_marker.go, ensureMarkerOnce: the CREATE TABLE IF NOT EXISTS and ALTER TABLE ... ENABLE ROW LEVEL SECURITY statements (about lines 173-181) and the
INSERT INTO kingres_transfer.secret (about line 186) run before verifyMarker (about line 199). The review of the earlier version relied on the verify coming before any insert.

Scenario: the foreign-owner test sees no schema. A database owner (not a superuser) loops `DROP SCHEMA kingres_transfer CASCADE; CREATE SCHEMA kingres_transfer;
CREATE TABLE kingres_transfer.secret (singleton boolean PRIMARY KEY DEFAULT true, k bytea NOT NULL);` plus a BEFORE INSERT trigger calling its own function. If that commit
lands between the foreign test and the service's CREATE statements, the `IF NOT EXISTS` forms do nothing and the service superuser then runs the INSERT into the owner's table,
which fires the owner's trigger as the service. The verify afterwards fails closed, but the code has already run. The window is two round trips and the owner can retry at
will, since every import into the database is another chance.
Fix: verify before the insert. Split the verify: the structural part (all but the last two conditions on the secret rows) runs after the CREATE statements and the function
creation and before the INSERT; after the INSERT a short check (`count(*) = 1 and length(k) = 32`) finishes it. A test can use a failpoint to plant the schema between the
owner test and the creation.

### DX17-02 (low): a member of pg_write_all_data or pg_maintain can hold a lock that blocks the marker for ever

transfer_marker.go: LOCK TABLE kingres_transfer.secret IN SHARE ROW EXCLUSIVE MODE (bounded by lock_timeout 10 s); markerSecret (no lock_timeout; the verify and the SELECT k read the
secret table); the body of mark (reads secret, inserts into import_marker, no lock_timeout); decideOutcome (60 s).
Scenario: such a member runs `BEGIN; LOCK TABLE kingres_transfer.secret IN ACCESS EXCLUSIVE MODE;` and keeps it. Every StartImport then ends with the retryable "marker busy" after
10 s, for ever. An import already at the load step waits in markerSecret with no bound (until the lease or the context ends); one that waits inside mark, after the load, does so
inside the load transaction with the target's locks held, until statement_timeout. No data change and no marker is forged or lost; decideOutcome ends with an error after 60 s, so
the outcome stays unknown, which is safe. No lock mode avoids this, since every mode conflicts with ACCESS EXCLUSIVE.
Fix: `SET LOCAL lock_timeout = '10s'` in markerSecret; add `lock_timeout=10s` to the function's SET clause (and to the compared proconfig array) so mark fails with a retryable
error instead of waiting. Document that all-data and maintenance roles are administrators and can stop imports; consider refusing grants of pg_maintain through the API.

### DX17-03 (low): the verify leaves some properties of a restored or cloned database unchecked

transfer_marker.go, markerVerifySQL.
Not compared: column types, NOT NULL and collation (only names and defaults); column ACLs (pg_attribute.attacl: a dump can carry `GRANT SELECT (k)`, which the table-level ACL
compare does not see); pg_inherits (a parent or child); reloptions; identity and generated columns; publications that name the tables; operators, collations or other objects in
the schema (only classes, procs and types are checked). A plain role cannot make any of them; they need a dump or template made by a superuser, the case DX16-05 named.
Fix: compare atttypid and attnotnull, require attacl IS NULL for both tables, no pg_inherits rows, no pg_publication_rel rows for them, and no pg_operator, pg_collation,
pg_conversion or pg_statistic_ext rows in the schema.

### DX17-04 (info): the literals in mark depend on the caller's standard_conforming_strings

transfer_marker.go, markFuncBody ('\x32'::bytea and '\x31'::bytea). A plpgsql body is lexed at run time with the caller's setting, and a role can turn the setting off. With it off,
'\x32' is read as the string "2", whose bytea value is the same byte, so the tag does not change today; it is safe by accident.
Fix: use `pg_catalog.decode('32', 'hex')` (and '31'), or add `standard_conforming_strings = on` to the function's SET clause and to the compared proconfig.

## Cleanup

The detached worktree was removed; nothing was committed or pushed and no test instance was started.

## Fixes

All in `internal/ops/transfer_marker.go` and `cmd/kingresd/review16_e2e_test.go`.

| Finding | Fix | Test |
|---|---|---|
| DX17-01 | The order is verify first: create what is absent (`IF NOT EXISTS`), run the structural check (owner, tables, triggers, rules, policies, columns, constraints, indexes, grants, objects of the schema, function) with no read or write of the objects, and only then `ENABLE ROW LEVEL SECURITY`, the insert of the secret, and a short final check (row security on both tables, one secret row of 32 bytes). A fault point `beforeMarkerCreate` sits between the owner test and the creation. | `TestMarkerPlantedInTheRaceWindow` (schema, secret table and a superuser-creating trigger planted in the window: the import fails `forbidden` and no role is made) |
| DX17-02 | `lock_timeout = 10s` in `markerSecret` and in the SET clause of `mark` (and in the compared `proconfig`); a holder of a lock now ends the step after 10 s with a retryable code. Documented: members of pg_write_all_data and pg_maintain are administrators of the database and can stop imports, not corrupt them. I did not remove pg_maintain from the grantable roles. | `TestMarkerLockHeldBeforeTheLoadEndsRetryable` |
| DX17-03 | The verify compares columns (name, type, NOT NULL, collation, identity, generated) with the temporary tables, and requires no column ACL, no reloptions, no inheritance, no publication of the tables or the schema, and no operator, collation, conversion, extended statistics, operator class or family, or text search object in the schema. | `TestMarkerVerifyRejectsWhatItDidNotMake` gained a column grant, a storage option and a changed column type |
| DX17-04 | `pg_catalog.decode('31', 'hex')` and `decode('32', 'hex')` in the body of `mark`. | covered by every import (the body is compared with the catalog and the tag is checked) |

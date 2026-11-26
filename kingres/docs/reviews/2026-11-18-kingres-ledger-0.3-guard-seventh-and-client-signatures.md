<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Seventh independent review of the guard of kingres_ledger 0.3 (the fixes for L19) and first review of client signatures (part B)

Reviewer: an independent reviewer. Date: 2026-11-18. Export reviewed: the committed tree `678dd32957`.

## 1. Scope and method

Scope: (1) the fixes for L19-01 to L19-07 (`src/write.c`, `src/kingres_ledger.c`, tests `t/027` to `t/031`, README, corrections 108 to 110);
(2) part B, client signatures: `src/sign.c`, the parts of `src/capture.c`, `src/guard.c`, `src/protect.c`, `src/row_hash.c` that serve it, the
three SQL scripts, `t/032` to `t/035`, README "Client signatures", design 3.6 to 3.8, corrections 111 to 120.

Method. A read-only export was copied into a private area together with one copy of each installation (PostgreSQL 18.7 and 19rc2; both have
assertions on, `SHOW debug_assertions` = on, and both have OpenSSL). The extension was built out of tree with `kingres/dev/ext.sh`
(clean build, `-Werror`), the whole suite was run on each major, then throwaway clusters were attacked with my own TAP files (a private copy of the
TAP framework confined to the ports given), with `psql` in autocommit, in transaction blocks and in pipeline mode, and with Python only to make Ed25519
signatures and to generate load. Ten one-line mutants of the code were built against 19rc2. Everything was removed at the end; no server of mine is left.

What I only read is said as such in each place and in section 9.

## 2. What was run

| Run | 18.7 | 19rc2 |
|---|---|---|
| clean build, `-Werror` | no warning | no warning |
| regression tests (`ledger_format`, `ledger_hash`, ... 19 files) | 19 of 19 | 19 of 19 |
| TAP suite `t/001` to `t/035` (34 files; there is no `t/024`) | 1929 tests, all pass (536 s) | 1929 tests, all pass (490 s) |
| install paths: a catalog fingerprint of 392 lines (functions with ACL, security, volatility, config and source hash; relations with columns, defaults and numbers; constraints; indexes; triggers with enable state; event triggers; ACL and options; extension members; registry rows) of a fresh 0.3, 0.1 then 0.3, 0.2 then 0.3 | identical in the three | identical in the three |
| my TAP files: forgery and replay (rev01), the policy `required` matrix (rev02), install paths and statements as a superuser against the three tables (rev03), roles, revocation, key aliases (rev04) | same results as 19rc2 | 4 files, 79 checks: 72 as expected, 6 finding-related, 1 error of my own test |
| hand attacks on the L19 fixes (sections 3 and 4): 5 statements in autocommit, a block and a DO block, 6 pipelines (`psql` pipeline mode), procedure with `COMMIT`, definer wrapper, the credit loop, the order check by two sessions, the dropper lists | not repeated | all |
| cost: 1500 transactions, three runs, four variants; one transaction with 500 to 4000 attested rows | not run | run |
| mutants (section 7) | none | 10 |

Both suites are green. Everything below is what they do not see.

## 3. Fate of L19-01 to L19-07

| Finding | State | Evidence |
|---|---|---|
| L19-01 timer lost by a second error | fixed | The design changed: a role that is not a superuser takes a lock only in an autocommit statement (`write.c:143-203`). The statements of the review (BEGIN, verify, SAVEPOINT, `1/0`, `SELECT 1`, ROLLBACK TO, sleep): `25001` at the first call, nothing is held (19rc2). DO block that takes the lock and then two EXCEPTION blocks that catch an error, then `pg_sleep(12)`: ended at 5.03 s (limit 5 s). A single statement `SELECT ledger_verify_recorded(), pg_sleep(12)`: ended at 5.03 s. A DO block in autocommit that only calls: fine. DO inside BEGIN, and a definer wrapper (owned by a superuser) inside BEGIN: `25001`. Procedure with `COMMIT` between two lock calls and a sleep: ended at 5.04 s. Pipeline mode: lock, then a statement that sleeps: ended at 5.04 s; lock, then the client idle with flush: lock seen at 2.5 s, gone at 6.5 s, session ended, log line names role and lock; lock, then `1/0`, client idle: lock gone at 2.5 s (the error aborted the implicit transaction); lock, then BEGIN, SAVEPOINT, error: BEGIN refused. Mutants: section 7 (m2 and m9 are redundant defences, not holes). |
| L19-02 credit without bound | fixed, with a default that is large | `enable_credit_limit` 8 s set for the role, the loop of the review (60000-row tables): `54000` after 8.4 s, the whole transaction rolled back, no table registered (0 of 12). Mutant with the cap off: killed (`t/029`, 7 failures). The default of the bound is 30 minutes (L21-06). |
| L19-03 tests depend on the machine | partly fixed | `t/029` no longer measures a table: it uses the developer setting `enable_batch_delay` (a least duration); `t/028` waits for conditions at a gate (an advisory lock), not for a sleep. I could not make a load-dependent failure: one run per major, nothing failed. I did not run the files under artificial load, so "no longer depends on the speed in either direction" is read, not shown. The new setting is a test hook in production code (L21-08). |
| L19-04 order check after the wait | fixed | Session A (superuser): BEGIN, `ledger_verify_recorded`, sleep, `ledger_enable` of `q10`. Session B: BEGIN, `LOCK TABLE q10 IN SHARE ROW EXCLUSIVE`, sleep, `ledger_verify_recorded`. A gets `KL006` at once, B completes, no deadlock (`kingres_ledger.c:262-273`). Mutant with the check before the wait removed: killed by `t/027` (2 failures of 108); `t/025` alone does not see it. |
| L19-05 server file and program roles | fixed as documented | `ledger_status.database_droppers` for a database owned by a superuser: `{grp,prog,viaadm,wall}` with `prog` in `pg_execute_server_program`, `wall` in `pg_write_server_files`, `grp` a group that holds `pg_execute_server_program` with ADMIN OPTION, `viaadm` a member of that group. I did not re-read the README sentence of exception 6. |
| L19-06 order refused in one direction | not re-read | the order check is unchanged in behaviour (`write.c:679-705`); `t/025` passes. |
| L19-07 operator text, quadratic search | fixed by reading | the log line carries a hint (`write.c:322-331`); the per-table credit record that was searched linearly no longer exists (`write.c:583-619`). The client still sees the text of a terminate request. |

## 4. The limit and the guard: what held

- Invariant B against the new rule: no way found to hold a lock of the extension with no timer, or past the limit, for a role that is not a superuser, in any of the 11 forms above, in autocommit and in pipeline mode. Procedures with `COMMIT`, a DO block, a definer wrapper: as above. The new functions of part B take no lock (`sign.c:26-33`, read; `ledger_attest` ran in blocks, in savepoints and in EXCEPTION blocks in all runs).
- Invariant A against part B: as a superuser (the strongest role), outside the script, all of these were refused (rev03): the exact text of the capture trigger statement (`CREATE TRIGGER ... ledger_capture('1')` on `ledger_attestation`), the exact text of `ALTER TABLE ... ENABLE ALWAYS TRIGGER`, `DROP TRIGGER`, `DISABLE TRIGGER`, the same through `EXECUTE` in a DO block, `UPDATE kingres.ledger_table` to `optional` and to `required` with and without `allow_manual_write`, `DELETE` under `session_replication_role = replica`, `INSERT` and `COPY` into the key table, `ledger_register_internal` outside a script, `DROP TABLE ... CASCADE`, `ALTER EXTENSION ... DROP TABLE`, `ALTER TABLE ledger_table DROP COLUMN` and `DROP CONSTRAINT`, `TRUNCATE ledger_table`. Counts unchanged and `ledger_verify` clean afterwards. The exception for the exact text is a fixed string compared while `ledger_register_internal` runs `SPI_execute` and cleared in `PG_FINALLY` (`sign.c:759-776`); the script's own statements are matched by the address of the script text (`protect.c:1401-1424`, read). I could not find a time or a text that matches and does something else; I did not build an event trigger, a second extension script or a `pg_upgrade` state against it (section 9).
- The policy `UPDATE` under the marker: the marker is raised and lowered in `PG_TRY/PG_FINALLY` in the three places (`write.c:109-121`, `sign.c:308-319`, `capture.c:289-297`) and reset at the end of every transaction (`capture.c:168`); the allowed update is only "optional to required, date from NULL, nothing else" (`guard.c:519-566`). I did not force an error while the marker is raised (read only).
- Install paths: one catalog (392 lines) on both majors. The three tables get the next free identifiers; the registry rows and trigger arguments agree in a fresh database. I could not create a 0.2 database with user tables under this library (it refuses to write a 0.2 registry), so the collision case rests on `t/011`, which I did not re-read.
- Hash formats of 0.1 and 0.2: the format tests (`ledger_format`, `ledger_hash`, `t/` files that check preimages) pass on both majors. I cannot say they are unmodified: the export has no history.
- Malleability and encodings (rev01, 19rc2 and 18.7): a signature with S replaced by S plus L is refused (`KL013`); signatures of 0, 63 and 65 bytes are `KL013`; a message with a wrong operation, chain, table, hash, or the signature of another key under a key id is `KL013`; the same nonce twice in one transaction is `KL014`; after a rollback the nonce is free again; an entry of another transaction cannot be attested (`KL011`, with and without an xid); an attestation for a table of the ledger itself is `0A000`. A key restricted to a role: refused to another role (`KL012`), refused to a member without `SET ROLE`, refused through a definer wrapper (the outer user counts), accepted after `SET ROLE` (documented). A revocation committed after the snapshot of a REPEATABLE READ transaction is seen (`KL012`); a revocation not yet committed is not seen (documented).
- Savepoints under `required` (rev02, both majors): unattested commit and prepare refused (`KL010`); attested then commit; attest in a savepoint then release; attest in a savepoint then ROLLBACK TO (owed again); savepoint with insert rolled back then another row attested; re-attest after rollback; DO block with an EXCEPTION block that rolled its row back; a row left by a completed EXCEPTION block (`KL010`); PREPARE refused unattested and accepted attested, then `COMMIT PREPARED` by another session; a deferred constraint trigger that writes at commit (`KL010`); `INSERT ... ON CONFLICT DO NOTHING` (no entry, commit); `MERGE` insert (`KL010`); two identical rows need two calls, a third is `KL011`.
- `ledger_verify`: clean on every healthy database I made (apart from L21-03); the three tables are compared like all ledger tables (the suite deletes, changes and inserts rows of them; I did not repeat it).
- Cost (assertion build, `psql` round trips, `synchronous_commit = off`, 1500 transactions, three runs): a plain table 209 to 254 us a transaction, a ledger table 330 to 386, a table with `required` and an attestation in every transaction 877 to 1093, an attestation on a table with the policy optional 1038 to 1298 (noise; the same cost). So an attestation adds about 550 to 700 us here, with assertions on and one more round trip; the README's 175 us is server time without assertions and is not comparable. One transaction with 500, 1000, 2000 and 4000 attested rows: 566, 560, 555, 526 us a row, linear (no quadratic search of entries). The version before part B cannot be built from the export, so the cost of a write to a table with no policy against the version before part B was not measured.

## 5. New findings

### L21-01 high: a degenerate Ed25519 public key can be registered, and then anyone who may call `ledger_attest` forges attestations with it

File: `src/sign.c:351-397` (`ledger_client_key_register`: the only check is the length of 32 bytes), `src/sign.c:221-251` (`kl_ed25519_verify`: OpenSSL accepts a small-order key and decides only by the equation), design 3.8, README "Client signatures".

What is wrong. Plain Ed25519 verification under a public key of small order is satisfied by a signature that no one made: for the identity point `A = 0x01 00 ... 00` the signature `R = 0x01 00..00, S = 0` verifies for every message. For the all-zero key (a point of order 4) it verifies when `k = H(R, A, M) mod 4 = 0`, which a caller reaches by choosing the nonce, which is part of the message, and trying again (it took 8 and 9 tries). The registration accepts them (and any 32 bytes, also ones that are not a point, and the sign-bit alias `0x01 00..00 80` of the identity point, which is a second key identifier for the same point, so a revocation of one is not a revocation of the other). A client can hand an administrator such a "public key" as its own; the administrator cannot see the difference by eye; from then on a row attested "by" that key proves nothing and the client can deny anything. With a key whose role is NULL any role that holds EXECUTE on `ledger_attest` can sign as it.

Steps (both majors; table `docs` a ledger table with the policy `required`, role `app` with EXECUTE on `ledger_attest`):
```
SELECT kingres.ledger_client_key_register(decode('01' || repeat('00',31), 'hex'), NULL, 'x');   -- ok
BEGIN;
INSERT INTO docs VALUES (100, 'forged');
SELECT kingres.ledger_attest('docs'::regclass, <row hash of that row>, 'I', <the key id>, <any 16 bytes>,
       decode('01' || repeat('00',63), 'hex'));      -- ok, no error
COMMIT;                                              -- ok
```
For the all-zero key the same call with random nonces in savepoints succeeded at the 8th (19rc2) and the 9th (18.7) try. `ledger_verify` reports nothing, because it does not verify signatures (correction 119).

A fix must achieve: the registration refuses a public key that is not a canonical encoding of a point on the curve, or that has small order (the eight points), and refuses the non-canonical aliases of a point it knows, so one point has one key identifier; refuse it again at `ledger_attest` (a key registered by an older version of the script) and in the verifier; add a test with the identity point, the all-zero key, the other small-order encodings and an alias, expected `22023` at registration. The same check belongs in `kingres-verify` for keys read from a proof.

### L21-02 critical: a transaction that is open when a table is switched to `required` commits unattested entries after the switch, and `ledger_verify` cannot see them

File: `src/row_hash.c:173-260` (the policy is read by a SPI query with the snapshot of the transaction and kept for the rest of the transaction), `src/capture.c:386-403` (an entry is owed only when that cached answer was `required` when it was written), `src/sign.c:734-750` (`ledger_set_signature_policy` takes no lock and does not wait for writers), `kingres_ledger--0.3.sql:2170-2185` (`unattested_entry` covers only `ledger_txid > signature_required_from`), README 1135-1156, correction 113.

What is wrong. Three ways the policy is not seen by a writer that is running at the switch:
1. A READ COMMITTED transaction that has written the table before the switch has cached `optional`; every later write of the same transaction, after the switch committed, is unattested and commits. Its transaction id is lower than the switch, so `ledger_verify` never reports it.
2. A REPEATABLE READ transaction whose snapshot predates the switch reads `optional` from its snapshot even after the switch committed; it commits unattested entries. Its id is higher than the switch, so `ledger_verify` reports `unattested_entry` (detected, but committed).
3. The same for a transaction that began after the switcher took its transaction id and before the switcher committed.

So any role that may write the table can keep one transaction open (BEGIN, one write, then wait) across the day on which the owner switches the policy, and then write, unattested and for ever undetected, as many rows as it likes; an honest long job does it by accident. This is exactly the guarantee the policy is for ("a transaction that leaves an entry without an attestation cannot commit").

Steps (rev02, R15; both majors). Table `ir_rc` a ledger table, policy optional:
```
session 1: BEGIN;  INSERT INTO ir_rc VALUES (0);                  -- caches "optional"
session 2: SELECT kingres.ledger_set_signature_policy('ir_rc', 'required');   -- committed
session 1: INSERT INTO ir_rc VALUES (1);  COMMIT;                  -- COMMIT succeeds, no KL010
```
`SELECT * FROM kingres.ledger_verify() WHERE check_name = 'unattested_entry'`: no row for session 1 (its id is below the switch). With `BEGIN ISOLATION LEVEL REPEATABLE READ; SELECT 1 FROM ir_rr;` in place of the first write: the commit succeeds and `ledger_verify` reports `unattested_entry tx 728 seq 0`.

A fix must achieve: after the switch commits, no commit of an entry of that table without an attestation is possible, whatever the isolation level and whatever was cached. For example: `ledger_set_signature_policy` takes `SHARE ROW EXCLUSIVE` (or `SHARE`) on the table before it changes the row, so it waits for the open writers and later writers wait for it; the policy is read at the writer's `PRE_COMMIT` for every table the transaction wrote, with a catalog snapshot that is not the transaction's (not from a cache made at the first write); and the date of the switch is something that is ordered with commits, not a transaction id, so `ledger_verify` has no gap. Tests: the three sequences above, in both isolation levels, with the writer's first statement before and after the switch started.

### L21-03 medium: a healthy database is reported damaged for ever after an ordinary switch

File: `kingres_ledger--0.3.sql:2170-2185`, `src/sign.c:737-740` (`signature_required_from` is a transaction id), README 1153-1155 ("can appear once"), correction 113.

What is wrong. Transaction ids are in the order of their start, not of their commit. A transaction whose id is above the switcher's and that commits before the switch committed (it read `optional`, correctly) is reported by `unattested_entry` in every later `ledger_verify`, and cannot be cleared (the registry and the entries are append-only). "Once" means in every verification from then on, and a recorded verification (`ledger_verify_recorded`) of the control plane fails or is marked damaged for a database that is healthy.

Steps (rev02, R14; both majors): session S: `BEGIN; SELECT kingres.ledger_set_signature_policy('late2','required');` (takes its id), session W: `BEGIN; INSERT INTO late2 VALUES (1,'v'); COMMIT;` (commits, correctly: the policy was not yet in force), session S: `COMMIT;`. `ledger_verify()` returns `unattested_entry tx <W> seq 0`.

A fix must achieve: no finding for an entry that was committed before the switch committed; this follows from the fix of L21-02 (a lock that orders them) plus a date that is a commit order. Test: the sequence above, expecting no finding.

### L21-04 medium: the switching transaction's own earlier writes commit unattested, and the README says they cannot

File: `src/capture.c:386-403` with `src/sign.c:748-750` (`kl_table_cache_reset` after the switch; entries written before it were not tracked), README 1153, design 3.7 and correction 113 ("the switching transaction itself is covered by the check at commit"), `kingres_ledger--0.3.sql:2164-2167`.

Steps (rev02, R11; both majors): `BEGIN; INSERT INTO late VALUES (1); SELECT kingres.ledger_set_signature_policy('late','required'); COMMIT;` commits, no `KL010`; `ledger_verify` is clean (the check is `>` the switching id, so its own entries are not covered either). The owner can do this only to rows of its own transaction, so the effect is small; the statement in README, design and the SQL comment is false.

A fix must achieve: either track entries written before the switch in the same transaction (the fix of L21-02, which reads the policy at `PRE_COMMIT`, does it) or correct the three statements; test the sequence above.

### L21-05 medium: the tests of part B do not kill a mutant that lets an unattested entry commit

File: `t/032_attest_policy.pl`, `src/capture.c:208-215`.

Mutant (one line): in the commit of a subtransaction, do not pass `attested` up to the parent (`if (kl_req[i].attested == mySubid) kl_req[i].attested = parentSubid;` replaced by `;`). `t/032` passes. The sequence that needs the line: `BEGIN; INSERT; SAVEPOINT s; SAVEPOINT t; ledger_attest; RELEASE t; ROLLBACK TO s; COMMIT;` must fail with `KL010` (the attestation row went with `s`); without the line the commit succeeds with an unattested entry. My rev02 (R5) kills the mutant on 19rc2. The same file tests the release of one level and the rollback of one level, not the nesting.

A fix must achieve: add the nested sequence (release of the inner, rollback of the outer, and release of both, with the attestation made in the inner and in the outer, and entry written in the outer and in the inner), and the same in an EXCEPTION block that catches after an inner release.

### L21-06 low: the default of `enable_credit_limit` is 30 minutes, so a role holds the registry's lock for 31 minutes by default

File: `src/kingres_ledger.c:146-157`, README 538.

The fix of L19-02 is a bound, which holds (8.4 s with a cap of 8 s), but the default is 1800000 ms. A role that may create tables and holds EXECUTE on `ledger_enable` stops every other `ledger_enable`, `ledger_remove_stray` and write by hand for up to 31 minutes a transaction, and can start the next one at once. It does not stop the builder or the capture, as the README says. A default of the order of the lock limit, with the large-table path left to a superuser, is what the property statement ("a bounded time") means to an operator.

### L21-07 note: a copy of the database has the same chain identity, so an attestation is valid in each copy

File: `src/sign.c:550-557`, README "The message" ("`chain_id` makes a signature useless in another chain").

A restored physical backup, a promoted standby, `CREATE DATABASE ... TEMPLATE` and a `pg_dump`-less clone carry the same `chain_id`, and each has its own set of used nonces from the moment of the fork. A committed attestation of one copy can be replayed in another copy for an entry with the same table, operation and row hash. I did not run it; it follows from the message and from the unique key being per database. Say it in the README, and give a clone a new chain identity if the tool that makes clones can.

### L21-08 note: a test hook is production code

File: `src/kingres_ledger.c:160-171` (`kingres_ledger.enable_batch_delay`, superuser setting, 0 by default, `GUC_NOT_IN_SAMPLE`). It makes `ledger_enable` wait up to 60 s a batch inside the time it credits; it can only be set by a superuser, who can stop the server anyway. Harmless; name it in the README of tests or build it only for the tests.

## 6. What held under attack

See section 4. In short: nothing found lets a role that is not a superuser, or a superuser outside `allow_manual_write`, write, alter, drop, truncate or re-point the three tables, the new columns or the policy; the timer and the own-statement rule held in every form I built; the nonce, chain, table, operation, hash, key and role of a message are all bound by the signature and by the checks; revocation is read with the latest snapshot; the savepoint logic is right in every nesting I tried except the one a mutant exposed (L21-05); the install paths agree on both majors; and the memory of the owed list is bounded (read: 16 bytes an entry in `TopMemoryContext`, doubled, released after a transaction of more than 65536 owed entries; `repalloc` stops at 1 GB, about 67 million entries, with an error, not a crash; I did not run a transaction of that size).

## 7. Mutants (19rc2, one line each, built clean, the named tests)

| # | Mutant | Tests | Result |
|---|---|---|---|
| m1 | `ledger_lock_table`: the order check before `LockRelationOid` removed (`kingres_ledger.c:269`) | `t/025`, `t/027` | killed by `t/027` (2 failures); `t/025` passes |
| m2 | `GetCurrentTransactionNestLevel() > 1` removed from `kl_require_own_statement` (`write.c:195`) | `t/027` | survived; equivalent, `IsTransactionBlock()` is already true in every subtransaction (the code comment says so) |
| m3 | `if (kl_enable_credit_limit > 0 && ...)` made inert (`write.c:595`) | `t/029` | killed (7 failures) |
| m4 | the reset of `attested` in the abort of a subtransaction removed (`capture.c:228`) | `t/032` | killed |
| m5 | `kl_attest_mark(seq)` removed (`sign.c:674`) | `t/032` | killed |
| m6 | the latest snapshot for the key and revocation replaced by the transaction's snapshot (`sign.c:602`) | `t/032`, `t/033` | killed by `t/033` (1 failure); `t/032` passes |
| m7 | the role check of the key made inert (`sign.c:630-631`) | `t/032` | killed |
| m8 | `case XACT_EVENT_PRE_PREPARE` removed (`capture.c:146`) | `t/032` | killed (2 failures) |
| m9 | `case TRANS_STMT_SAVEPOINT` removed from the utility hook (`write.c:470`) | `t/027`, `t/028` | survived; by hand: a pipeline with the call, SAVEPOINT without BEGIN, an error, ROLLBACK TO and a sleep gives `SAVEPOINT can only be used in transaction blocks`, so the line is a redundant defence behind the refusal of BEGIN, not a hole (same pipeline against the original: BEGIN refused) |
| m10 | attestation not passed to the parent when a subtransaction commits (`capture.c:213-214`) | `t/032` | survived; killed by my rev02 R5 (L21-05) |

## 8. Verdicts

**The guard (fixes for L19): holds.** The fixes for L19-01 to L19-05 do what they say in every nesting, in autocommit and in pipeline mode, on both majors, with mutants killed for the order check, the credit cap and the snapshot-sensitive pieces; the two surviving mutants of the guard are redundant defences, not holes. The new surface of part B does not give a role a way to write, alter or drop the ledger's own tables that I could find, including a superuser outside the manual-write path. What remains is L21-06 (a long default) and L21-08, both small. Another review of the guard alone is not needed; whoever reviews part B again should re-run the invariant-A statements of rev03.

**Client signatures (part B): not ready; another review is needed after the fixes.** Two findings break what the feature is for: a degenerate key lets anyone forge an attestation (L21-01, high) and a transaction open across the switch commits unattested entries that `ledger_verify` cannot see (L21-02, critical). L21-03 and L21-04 are the same root cause (the policy is read from a cache and dated by a transaction id), L21-05 is a gap in the tests. The signature check itself (malleability, wrong field, nonce, role, revocation, savepoints) was sound in everything else I ran.

## 9. Not checked

- Logical replication apply into a table with the policy `required`: by reading, the apply worker's commit reaches the same `PRE_COMMIT` check and fails with `KL010` for rows that carry no attestation, so a subscriber cannot hold such a table; not run. A standby promoted between a write and an attestation, a crash between them (the transaction is not committed until the check passes; `t/032` has a crash test that I did not re-read), `COPY` into a `required` table, partitioned and inherited tables, parallel workers, a write inside a commit callback of another module (callbacks registered after this library run first at pre-commit; one registered before it could write after the check; not built).
- A transaction that owes more than 2 GB of records (argued in section 6, not run).
- The exact-text exception against an event trigger, a second extension script, `CREATE EXTENSION ... FROM`, `ALTER EXTENSION ... UPDATE` by an owner that is not a superuser (an untrusted extension needs a superuser to run a script; argued, not run), `pg_extension_config_dump`, dump and restore of the three tables, and `pg_upgrade`.
- The marker of the guard left raised by an error inside the policy `UPDATE` (a cancel while the row is locked by another session): by reading only.
- A 0.2 database upgraded with user tables whose identifiers collide: not creatable under this library; `t/011` not re-read.
- Whether the format tests are unmodified (no history in the export); the cost against the version before part B.
- The standalone verifier (`kingres-verify`): not run; the checks for the signature, the revocation by block height and a pinned key are not in `ledger_verify` and I did not look at its code. L21-01 applies to it.
- Timing tests under artificial load; a build of PostgreSQL without OpenSSL (the suite's `t/035` builds the extension with the macro and passes on both majors); the README sentences of exception 6 and of the order of the locks.
- Isolation levels other than READ COMMITTED and REPEATABLE READ for the policy (SERIALIZABLE reads the policy as REPEATABLE READ does, by reading).

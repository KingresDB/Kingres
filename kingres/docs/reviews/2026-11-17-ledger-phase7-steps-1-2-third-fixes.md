<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->

# Review of the third fix round for ledger phase 7, steps 1 and 2

Reviewer: independent, adversarial. Date: 2026-11-17.
Reviewed: the two commits of the third fix round, one for the extension
`contrib/kingres_ledger` 0.3 part A (the write guard in `src/guard.c`, the
guard trigger, the break-glass setting, the block time rule, `ledger_remove_stray`
and `ledger_removed`, the fixes for L9-02, L9-03, L9-14) and one for the verifier
`kingres/verifier` (fixes for L9-04 to L9-13 and L9-15). Both were read from the
commits only (exported with `git archive`); nothing in the repository was changed
except this file. The reviews this one follows are
`kingres/docs/reviews/2026-11-17-ledger-phase7-steps-1-2-second-fixes.md`
(findings L9-01 to L9-15 and "The insert guard"),
`kingres/docs/reviews/2026-11-16-ledger-phase7-steps-1-2-fixes.md` and
`kingres/docs/reviews/2026-11-16-ledger-phase7-steps-1-2.md`.

## Summary

The guard does what it was written for against every route I tried that starts
from a role that is not a superuser and has no privilege on the extension's own
objects. A member of `pg_write_all_data`, of `pg_maintain`, of `pg_read_all_data`,
the owner of an application table, a role with `CREATE` on a schema, the database
owner and a subscription owner (superuser, or `pg_create_subscription` with
`run_as_owner` on and off) cannot insert into, update, delete from or truncate any
of the nine ledger tables, through plain statements, views, rules, triggers,
`SECURITY DEFINER` functions, prepared statements, exception blocks, `COPY`,
`MERGE`, `ON CONFLICT`, writable CTEs, the settings, `GRANT SET ON PARAMETER`, or
logical replication (both the table synchronisation worker and the apply worker;
the trigger fires for them). The reproduction of L9-01 no longer works.

What does not hold is the sentence the design rests on: "no role that is not a
superuser can call `ledger_internal_write`". The function is protected by an ACL
only, and an ACL is inherited. A role that is a member of the extension owner's
role with `INHERIT` and without `SET` (a legal grant on PostgreSQL 16 and later),
or an owner that was later made a non-superuser, calls the function directly,
writes a block, and `ledger_verify` and `ledger_catalog_defects` say nothing about
the role (L10-01, high). A superuser calls it without the explicit act the README
promises (L10-02). The fix of L9-14 is incomplete: a role with `pg_maintain` or
`pg_write_all_data` still stops the block builder with a weak lock on
`ledger_chain` (L10-03). The verifier keeps its core property (no input made it
print VERIFIED, exit 0 or move the state for something not proved, except
through a redirected state path, L10-06), but a third dead end of the state file
exists and can be planted by a publisher that holds no key (L10-04, L10-05), the
proof size cap refuses honest proofs of rows older than about six days on a busy
ledger (L10-07), and a failed write of the verdict still exits 0 (L10-08). The
tests of the round kill 95 of 129 mutants; the 34 survivors include real gaps in
the guard (see "The tests of this round").

Count: critical 0, high 1, medium 6, low 8, note 4 (19 findings).

- L10-01 High: a non-superuser calls `ledger_internal_write` through membership of
  the owner role (INHERIT, no SET) or as a demoted owner and forges a block
- L10-02 Low: a superuser writes the ledger through `ledger_internal_write`
  without the break-glass setting
- L10-03 Medium: a weak lock on `kingres.ledger_chain` by `pg_maintain` or
  `pg_write_all_data` stops the block builder (L9-14 is not closed)
- L10-04 Medium: a state is accepted, read back, and refused for ever after a key is
  added to the trust file; a publisher without a key can plant it
- L10-05 Medium: a publisher without a key stops `checkpoint --state` from advancing
- L10-06 Medium: the private directory check looks at one directory; a redirected
  state path makes a rollback pass with status 0
- L10-07 Medium: the 16 MiB cap on a proof makes old rows unprovable on a busy ledger
- L10-08 Low: status 0 when the verdict could not be written
- L10-09 Low: a publisher without a key forces status 4; the note to save is not shown
- L10-10 Low: the text output clips before escaping, so it is not one to one
- L10-11 Low: the README writes the escape of the backslash wrongly
- L10-12 Low: the lock file is outside the private check; a stale lock gives status 2
  for ever
- L10-13 Medium: 34 of 129 mutants of the guard and its scripts survive the tests
- L10-14 Low: tests that simulate honest operations now run with the guard open
- L10-15 Low: the upgrade fingerprint ignores column privileges
- L10-16 Note: exit-site table: rows that can be tested and exits it cannot see
- L10-17 Note: `blocks` exits 0 for a cut or unanchored export
- L10-18 Note: parser and message details
- L10-19 Note: one test fewer on 19 than on 18 (still not explained)

## Scope and method

Ledger: read in full `src/guard.c`, `src/capture.c` (guard and capture parts),
`src/protect.c`, the changes of `kingres_ledger.c` and its header, the 0.3 install
script (tables, triggers, `ledger_enable`, `ledger_build_block`,
`ledger_generate_digest`, `ledger_remove_stray`, `ledger_verify`), the README
sections "Who can write the ledger tables", "A row above the head", "Protection
against being switched off". Not read line by line: the 2,000 lines of the two
update scripts (compared by test, see L10-15), `history.c`, the worker.

Environment (Linux in WSL, Windows 11): PostgreSQL 18.7 and 19rc2 with
assertions, copied into a work area of my own with the prefix fixed; the ledger
built out of tree with the dev script, `EXT_WERROR=1`, the verifier built from the
export and named in `KINGRES_VERIFY` so that the tests that drive it ran; servers
on ports of my own, all stopped. Part of the work was done by two independent
helpers under my brief, and I re-read what they reported: the verifier (the
differential test of the JSON decoder, the state, lock, status 4, escaping and
exit-site items) and the mutation run of the ledger tests. What I ran myself is
marked "(own)", what the helpers ran "(helper)".

Baseline: see the end.

## Fate of L9-01 to L9-15

- L9-01 (member of `pg_write_all_data` forges a block): Fixed against that role
  (own, 18.7; the guard and the hook messages; on 19rc2 by the helper's
  suite and the test t/014). Not closed for the owner-role route, L10-01.
- L9-02 (a digest row stops `ledger_head`, `ledger_verify` clean): Fixed.
  `ledger_verify` reports `digest_block`, `digest_hash`, `digest_json` for every
  digest; the mutants that remove them are killed by t/015 (helper). The two
  clauses of `digest_json` for the chain id and the height survive (L10-13).
- L9-03 (README recovery leaves a finding, deletes evidence, opens a window):
  Fixed. `ledger_remove_stray` copies into `ledger_removed`, requeues, switches no
  trigger off; the recipe is run by t/015 (helper: its mutants killed). Residue
  in "ledger_remove_stray" below.
- L9-04 (state the verifier cannot read back): Fixed for the case of the finding,
  and a third dead end exists (L10-04).
- L9-05 (2 GiB for 16 MiB): Fixed (helper): hostile 16 MiB inputs use 17 to 72 MB.
  Cost: L10-07.
- L9-06 (colon in digest names): Fixed (the unit tests of the commit pass; not
  re-run by me beyond the suite).
- L9-07 (conflict file reported saved): Fixed for what the finding named: content
  is compared byte for byte, eight names (helper). Residue L10-09.
- L9-08 (lock opened through a link): Fixed: a link or a FIFO at the lock name is
  refused (helper). Residue L10-12.
- L9-09 (exit table and `blocks`): Fixed in form; see L10-16.
- L9-10 (escaping ambiguous): Fixed for backslash, newline and the controls: the
  README-only decoder of the helper read back 26 hostile payloads in 10 contexts
  exactly; the exceptions are the clips (L10-10) and the README sentence (L10-11).
- L9-11 (mutants survive): The new tests kill the listed mutants; this round's own
  survivors are in L10-13.
- L9-12 (lock named by the file name): Fixed in the sense of one lock per directory;
  hard link to the state file by another name was not re-tried.
- L9-13 (Windows failure): Not reproduced here either; no Windows run of mine.
- L9-14 (`pg_maintain`, `ledger_pending`): Partly. `DELETE` on `ledger_pending` and
  `LOCK TABLE ... ACCESS EXCLUSIVE` of a ledger table are refused. Weak locks are
  not (L10-03).
- L9-15 (empty state file): Fixed (not re-tried by me; in the commit's tests).

## The guard

Each row: what was tried, result. "18" is 18.7, "19" is 19rc2. Attacks I ran are
marked (own); the others come from the existing tests (helper confirmed them by
mutation) or were reasoned from the code.

### The marker (`src/guard.c`)

| Attack | 18 | 19 |
|---|---|---|
| member of `pg_write_all_data`: `INSERT` into each of the nine tables, `DELETE` and `TRUNCATE` on `ledger_pending`, `UPDATE`, `DELETE`, `TRUNCATE` on blocks (own) | refused, hook text or `KL001` text | same by the suite |
| `MERGE`, `INSERT ... ON CONFLICT DO UPDATE` (new row and existing row), writable CTE, `COPY FROM` stdin and program (own) | refused | same by the suite |
| insert through a view of the member, through a security barrier view, through a rule on another table, through a trigger function of the member, through a `SECURITY DEFINER` function of the member (own) | refused | same by the suite |
| PL/pgSQL exception block then write; prepared statement; cursor then `EXECUTE` (own) | refused | same by the suite |
| user code inside the bracketed statements: triggers, rules, policies, defaults, checks, index expressions of the ledger tables; types of the columns; operators | none exists: the tables carry only the extension's own triggers; the foreign key checks are system triggers and use the `FOR KEY SHARE` skip; the statements name `pg_catalog` or `kingres` and the functions have `search_path = pg_catalog, pg_temp` (read) | same |
| a marker left raised after success (mutants that never lower it) | the code lowers it in `PG_FINALLY`; no test notices if it did not (L10-13) | same |
| parallel workers | the bracketed statements read only; a write is never parallel; not run | not run |
| `ledger_internal_write` as a role with `GRANT EXECUTE`, `ALTER FUNCTION OWNER`, `ALTER DEFAULT PRIVILEGES` (own) | refused by the protection hook | by the suite |
| `ledger_internal_write` as a member of the owner role with INHERIT, SET FALSE (own) | **writes the block: L10-01** | not run, same code |
| `ledger_internal_write` as an owner that was made a non-superuser (own) | **writes the block: L10-01** | not run |
| `ledger_internal_write` as a superuser, no setting (own) | writes a block (L10-02) | not run |
| extension created by a non-superuser, trusted path | the control file has no `trusted`; `CREATE EXTENSION` needs a superuser; `DROP EXTENSION` by the database owner: "must be owner" (own) | same |

### The hook (`ExecutorCheckPerms_hook`)

| Attack | 18 | 19 |
|---|---|---|
| a real `UPDATE` that presents only `ACL_UPDATE` with empty `updatedCols` | none found: every `UPDATE`, `MERGE ... UPDATE`, view `UPDATE` sets columns; the `BEFORE UPDATE` trigger refuses as well (read; `ledger_guard` and mutants G9 killed by the helper) | same |
| `VACUUM FULL`, `CLUSTER`, `REINDEX` by `pg_maintain` (own) | allowed, content preserved (by design) | by the suite |
| `ALTER TABLE ... SET ACCESS METHOD`, `DISABLE TRIGGER ALL`, `OWNER TO` on a catalog table (own) | refused | same |
| `ALTER TABLE` of a user ledger table: `DISABLE TRIGGER`, `SET LOGGED`, `RENAME`, `ENABLE ROW LEVEL SECURITY`, `ADD COLUMN` (own) | trigger and persistence changes refused; rename, nullable add column and enabling row security accepted (as before) | same |
| partitions and inheritance of ledger tables | refused by the protection hook (earlier rounds; not re-run) | same |
| `session_replication_role = replica`: the new guard trigger and the blocking triggers are `ENABLE ALWAYS`; a non-superuser cannot set the parameter without a grant (read; replication run below) | holds | holds |
| logical replication apply and table sync into `kingres.ledger_blocks` and `ledger_pending` and into a user ledger table (own: subscription owned by a superuser, and by a member of `pg_create_subscription` and `pg_write_all_data` with `run_as_owner` false, then true) | ledger tables refused in both workers (log: `cannot write to table "ledger_blocks"`, five times); the user ledger table was filled by the table sync and `ledger_verify` stayed clean | not run: only the code path is shared (L10 "Not covered") |
| library not preloaded | `ledger_enable` refuses; the existing triggers refuse inserts; utility statements are then open to the owner of a user table only (t/006 covers it); I did not check that `ledger_verify` names the missing library | not run |
| a session that connected before the extension was created | the hook cache is invalidated by the syscache callback; mutant "never invalidated" survives (L10-13), behaviour not attacked | not run |

### The break-glass settings

| Attack | 18 | 19 |
|---|---|---|
| `SET`, `SET LOCAL`, `set_config()`, `ALTER ROLE ... SET` by a non-superuser (own) | permission denied; with a grant: sets, then the insert is refused | by the suite |
| a function with `SET kingres_ledger.allow_manual_write = on` in its definition (own) | refused at `CREATE FUNCTION` (the check hook) | same |
| `SET ROLE` to a plain role after turning it on (own) | refused | same |
| `PGOPTIONS` / connection option | the code refuses any source but a session `SET`; the test does not exist, and the mutant that accepts it survives (L10-13) | same |
| `GRANT SET ON PARAMETER kingres_ledger.max_clock_skew` | the grantee sets it to 0 in its session (own); it changes what `ledger_verify` reports to that session only | same |
| record of a manual write | none, except `ledger_removed` for `ledger_remove_stray`: a manual `INSERT` of a block leaves no record in the database (README says "explicit, recorded act"; the recording is the server log only) | same |

### The block time rule

Not run by me at the clock level (no clock manipulation available); read from
the code and the helper's mutants, which show the tests pin the boundary only
with the year 2100 and with a block just under one hour ahead (L10-13). What the
code does for an honest step back of two hours: the builder stops with `KL004`
(the transactions stay pending, nothing is lost), the worker records a failure;
the way out is a superuser setting `max_clock_skew` to 0, after which every block
carries the previous time until the clock catches up. The README says so. Whether
the control plane alert `ledgerPendingTooOld` covers it was not checked (the
control plane code is outside what I may read from the working tree and I did
not read it from the commit). Honest empty blocks: none can be produced
(`ledger_build_block` returns before writing when nothing is pending, and a
pending row cannot lose its entries), so `block_empty` is not a false alarm. A
forged block exactly one hour ahead can be repeated by a superuser only.

### `ledger_remove_stray` and `ledger_removed`

Callable only by a superuser with the setting on (EXECUTE is revoked from PUBLIC;
`wr` got `permission denied`, own). Every table, including `ledger_removed`, has
the guard trigger and the blocking triggers in the 0.3 script; the helper dropped
`ledger_removed` from each list in the scripts one at a time and every case is
detected, with one exception noted in L10-13 (a removal from the 0.1 to 0.3
script of the column privileges). What it removes for each kind of damage is what
the README says; two observations from reading the code:

- the digest rows removed are every row whose block hash differs, so if a
  superuser rewrote the block and the digest row is the honest one, the honest
  digest is what is removed (copied into `ledger_removed`, which no hash covers;
  the README says so for the record as a whole);
- the requeue gives the transactions the order of `ledger_txid` and
  `ledger_build_block` builds them into the next block together with whatever
  is pending then, so the rebuilt block usually differs from the removed one and
  an auditor holding a checkpoint over the removed block sees a fork; the README
  says this.

The mutant that removes the `NOT EXISTS (pending)` guard and the one that removes
the `LOCK` survive (L10-13); the concurrency with the builder was not run.

### Upgrades and other paths

Not run by me: the catalog comparison across the three update paths and the
`pg_upgrade` of a ledger (the suite, t/011 and t/016, passes on both majors).
Read: `IsBinaryUpgrade` cannot be set by a role; `ledger_binary_upgrade()` only
reports it. The library knows the SQL version from `pg_extension.extversion`,
which only the extension script changes. Restore: `pg_dump` does not carry the
ledger tables, so a restored database has a new empty ledger; I did not try to
launder history through it.

## The tests of this round

Mutation run of the ledger tests (helper): 129 mutants of the guard code and the
three SQL scripts, each in a fresh copy built with `-Werror`, run against the 16
regression tests and the 16 TAP files; 43 of them also on 19rc2 (all survivors
and the major-sensitive ones), with the same outcome on both majors. 95 killed,
34 survived, of which 10 are equivalent or only more restrictive. Both verifier
and ledger baselines are at the end.

Survivors that matter (the full list is in the helper's table, summarised here):

| Mutant | Untested behaviour |
|---|---|
| marker never lowered by `kl_execute_guarded`, by `ledger_internal_write`, or by `kl_write_leave` (killed only together with a disabled reset) | a write in the same transaction after an honest capture or after `ledger_build_block` must be refused |
| the outer-user check removed, or replaced by the session user | a role with `GRANT SET ON PARAMETER` calling a superuser-owned definer function; a superuser who did `SET ROLE` plain |
| check hook accepts a connection option (`PGC_S_CLIENT`) | `PGOPTIONS` |
| `LOCK` threshold raised to `SHARE` or `SHARE UPDATE EXCLUSIVE` | only three stronger modes are tried |
| trigger-level `DELETE` checks (`ledger_pending`, `ledger_tx`, `ledger_digest`) allowed without the setting | the hook always refuses first, so without the hook (t/006, apply worker) nothing tests them |
| manual delete allowed on every table named like `ledger_tx` or on `ledger_verification` | `ledger_guard` deletes `WHERE false`, which passes for all eight tables |
| any extension's script may write; the extension oid cache never invalidated | a second extension whose script inserts; `DROP` and `CREATE EXTENSION` in one session |
| `max_clock_skew` unit second instead of millisecond; `>=` for `>`; always one hour in `ledger_verify` | the boundary is pinned only by two data points |
| `digest_json` without the chain id clause or without the height clause | only a document `{}` is tested |
| 0.1 to 0.3 script without the column grant on `ledger_digest` | the fingerprint of t/011 omits `attacl` (L10-15) |

Existing tests that were changed to set `allow_manual_write` (helper review of the
diff): none now proves less about what a non-superuser can do; every tampering that
moved under the setting was already a superuser's. See L10-14 for the honest-path
tests that now run with the guard open. The tests of the verifier added by the
round pass on Linux (210 top-level tests, none skipped; helper); I did not mutate
the verifier.

## Findings

### L10-01 High: a non-superuser calls `ledger_internal_write` and forges a block

Place: `kingres_ledger--0.3.sql` (`REVOKE ALL ... FROM PUBLIC` is the only
protection), `src/guard.c` `kingres_ledger_internal_write` (no check of the
caller), `src/protect.c` (refuses `GRANT` only), README "Who can write the ledger
tables" ("so no role that is not a superuser can call it").

Steps (18.7, own; `own2` is a superuser that created the extension in database
`d2`):

```
create role dba login;
grant own2 to dba with inherit true, set false;   -- no SET: dba cannot become own2
\c d2 dba
select kingres.ledger_internal_write(3, 0::bigint, sha256('a'), sha256('b'), 1, now(), sha256('c'));
 -> 1                       -- a row in kingres.ledger_blocks, written by dba
```

and, second form: `alter role own2 nosuperuser;` then as `own2` the same call
returns 1 (a direct `INSERT` as `own2` is refused, as it should be);
`ledger_catalog_defects()` returns no row for the extension owner not being a
superuser. The values above are garbage, so `ledger_verify` reports them
(`block_prev_hash`, `block_hash`, `block_tx_root`); with the hash computed by the
public functions, as in L9-01, the block is a valid successor and nothing reports
it, and a chosen time is carried by every later block.

Observed: a role that is not a superuser (and cannot become one) writes history.
Expected: by the rule of this review no such role exists. Granting a superuser
role with `INHERIT` and no `SET` is an ordinary way to hand out the privileges of
an owner on PostgreSQL 16 and later; demoting an owner is an ordinary
administrative act; neither is a statement about the ledger.

Direction: do not use an ACL as the guard of an internal write. In the C function
require that the caller is the extension owner acting as itself
(`GetUserId() == extowner`, which is false for an inheriting member) and that a
definer function of the extension is on the call path, or move the seven
statements into the C bodies of `ledger_enable`, `ledger_build_block`,
`ledger_generate_digest` and `ledger_verify_recorded`, so that no SQL-callable
function raises the marker. Add a `ledger_catalog_defects` finding for an extension
owner that is not a superuser and for roles with `EXECUTE` through inheritance.

### L10-02 Low: a superuser writes the ledger without the explicit act

Place: `src/guard.c` (comment: "A superuser who calls it directly writes the
ledger without the setting"), README ("A superuser's direct `INSERT` is refused
too, unless the superuser turned on ... allow_manual_write").

Steps (18.7, own): as a superuser, no setting:
`select kingres.ledger_internal_write(3, 5::bigint, sha256('a'), sha256('b'), 1, now() + interval '2 hours', sha256('c'));`
returns 1; `insert into kingres.ledger_blocks ...` in the same session is refused.
Observed: a superuser forges blocks silently. Expected: only through the explicit
act. Also see "The break-glass settings": a manual write leaves no record.
Direction: as for L10-01; and let `allow_manual_write` write a line to the server
log and into a ledger table that the guard owns.

### L10-03 Medium: a weak lock on `ledger_chain` stops the builder

Place: `src/protect.c` `T_LockStmt` (refuses modes above `RowExclusiveLock`),
`ledger_build_block` (`LOCK TABLE kingres.ledger_chain IN EXCLUSIVE MODE`),
`sql/ledger_guard.sql` ("a lock that blocks nobody is allowed").

Steps (18.7, helper; script kept in the helper's area): as `pg_maintain`
`BEGIN; LOCK kingres.ledger_chain IN ROW EXCLUSIVE MODE;` and keep the
transaction open; the builder waits on its `EXCLUSIVE` lock until `lock_timeout`.
Same with `ROW SHARE`, and with `SELECT 1 FROM kingres.ledger_chain FOR UPDATE` by
a member of `pg_write_all_data`. `EXCLUSIVE` conflicts with `ROW SHARE` and `ROW
EXCLUSIVE`, so the premise of the test is false for this table.
Observed: the availability limit of L9-14 remains for the table the builder locks.
Expected: refused, or the builder uses a lock that only the owner can take.
Direction: also refuse every lock on `ledger_chain` by a non-owner, and refuse
`FOR UPDATE` on it (the hook's skip for `ACL_UPDATE` with no column is what lets it
through), or serialise builders with a lock on an object no other role can reach.

### L10-04 Medium: a state is stored, read back, and refused for ever after a key is added

Place: `internal/cli/cli_state.go` `applyState`, `encodeState`, `verifyHeld`.

The note is stored with every line, including signature lines no key verified;
anybody can append lines of a key id to a genuine note, so a publisher that holds
no key can do it. Steps (helper): the trust file holds key 1; the genuine note of
size 1000 is signed by key 1 and has one more line with the name and key id of
key 2 and a signature of 64 zero bytes. `checkpoint --state st` prints VERIFIED,
"state file was updated", status 0 (the read-back passes because key 2 is not in
the file). The auditor adds key 2 (a normal rotation, `trust show` says 2 of 2
keys usable); the next run, with an honest size-1200 note signed by both keys, and
`consistency --old st`, end with status 2 ("has no signature of a key of the trust
file, or a wrong one") and the advice to start a new state with `--min-size`.
Control: with key 2 in the file at the first run the read-back refuses. Observed
and expected: see above; the state must keep working when a key is added.
Direction: store only the signed text and the lines that verified; or ignore, for
a held note, lines of keys that are not among the keys that verified it.

### L10-05 Medium: a publisher without a key stops the state from advancing

Same place and cause as L10-04, with a retired key. Steps (helper): the trust
file holds key 1 and key 3 (retired, `not_after` in the past, kept as the README
advises); the honest note of size 1200 is signed by key 1; the publisher appends
one line with the id of key 3 and a zero signature. `checkpoint` without `--state`
prints VERIFIED plus "the note also carries a signature of key key-3-retired ...
which was not counted" (a claim of a signature that never verified); with
`--state` the status is 2, "this checkpoint cannot be recorded", the state stays
at 1000, and the advice points to a new state, which is what a hostile publisher
wants. The publisher can repeat this on every checkpoint. Direction: drop
unverified lines before the read-back and before saving; do not call a line a
signature.

### L10-06 Medium: the private directory check looks only at one directory

Place: `internal/statefile/private_unix.go` `CheckPrivate`, `Resolve`.

Steps (helper; a second user was simulated with one uid): `shared/` is mode 0777,
`shared/auditor/` is 0700 and `--state shared/auditor/state` is accepted. The
"other user" renames `shared/auditor` and puts a link `shared/auditor` to an
empty 0700 directory of the auditor; the publisher serves the genuine size-1000
note: VERIFIED, "no earlier checkpoint was recorded", status 0 (the state was at
1200). Second form: `--state shared/st-link` where the link points to a private
state; the other user re-points it to an older state of the auditor (the README
tells auditors to keep old states): VERIFIED, "is the same as the checkpoint in the
state file", status 0, while the real state is at 1200. A dangling link and a
directory created by the attacker are refused. Observed: a rollback ends with 0
through a path another user can change. Expected: every ancestor of the resolved
path and of the link as typed is owned by the auditor or root and not writable by
others (unless sticky), and the file is opened by directory handle after the
check. A real second account and the race between check and open were not run.

### L10-07 Medium: the proof cap refuses honest proofs of old rows

Place: `internal/cli/cli.go` `maxFileSize` (16 MiB for `--proof`), README "Limits".

A proof carries every header after its block up to the latest block when it was
made; jsonb prints a header in about 335 bytes; the default block interval is 10
seconds, so 8,640 blocks a day. Steps (helper): a proof with 50,009 headers
(16,742,904 bytes) is VERIFIED; with 60,009 (20,092,904 bytes) status 2, "larger
than the limit". So a row more than about 50,000 blocks (about 5.8 days) behind
the head cannot be proved, with any digest. The `blocks` cap (1 GiB, about 3.2
million blocks, about a year) refuses honest exports as well. The trust-file caps
(16 keys, 1,024 client keys, 32 TSA roots, 200-byte labels) are not in the README
table. Direction: stream the proof's chain with no cap but the file size, or cut
the chain at a given height, or state the age limit; list every cap.

### L10-08 Low: status 0 when the verdict could not be written

Place: `internal/cli/cli_out.go` (write results ignored). Steps (helper):
`checkpoint ... > /dev/full` and `--json > /dev/full`: status 0; with stdout closed:
0; with `--state` the state is written and the status is 0; `trust init --out - >
/dev/full`: 0 with no trust file produced. A broken pipe gives 141. Expected: the
README says 0 means VERIFIED was printed. Direction: check writes; non-zero when
the verdict could not be written.

### L10-09 Low: a publisher without a key forces status 4

Place: `statefile.SaveConflict`, `cli_state.go`. Steps (helper): with the state at
1200, the genuine size-1000 note served ten times with a different number of
unknown-key lines appended (no key needed): variants 0 to 7 are saved under eight
names, status 1; variants 8 and 9 give status 4, "was NOT saved", although eight
files with the same signed text are on disk. Status 4 says to save the note
yourself, but neither the text nor the JSON contains the note or the path in
`--source`. Held: a read-only directory (4 with the note), `ulimit -f 0` (4), a link
to `/dev/full` at the first name (skipped, saved under the second, 1), content
compared byte for byte, names with digits and hex only. Direction: compare and
store the normalised note; print the note or its path on status 4.

### L10-10 Low: values are clipped before escaping

Place: `clipArg` (100 bytes), the clips of 60, 80 and 120 bytes in `signednote`,
`trust`, `tlog` and `safefile` (each `s[:n] + "..."` and invalid bytes to `?`).
Steps (helper): two `--log` values that differ after byte 100 give identical
stderr; a value ending in `...` prints like a clipped one; a lone byte at the clip
boundary and a real `?` print alike. The README says the only cut is the 2,048
byte cut marker and that two inputs never print the same. Direction: remove the
clips or use the cut marker.

### L10-11 Low: the README writes the backslash escape wrongly

README lines on the escaping rule give a single backslash in a code span ("a
backslash is written `\`"); the output has two. A decoder written from the README
cannot tell a backslash from an escape. With `\\` assumed the helper's decoder
read back 26 payloads in 10 contexts exactly. Fix the README.

### L10-12 Low: the lock file is not part of the private check

Place: `CheckPrivate` (does not look at the lock file), `Acquire`. Steps (helper):
a lock file of mode 0666 in a 0700 directory is accepted and keeps its mode; a
directory with mode 0711 is accepted; a lock held with `flock` gives status 2
after 10.02 seconds and nothing changes. Combined with a lock left from when the
directory was shared, another user can hold it for ever (reasoned, one uid).
Direction: check owner and mode of the lock file, recreate it under the lock with
mode 0600, refuse `o+x` without the sticky bit.

### L10-13 Medium: surviving mutants of the guard

See "The tests of this round". The three that matter most: nothing proves that
the marker is lowered after a successful statement (so a later write in the same
transaction would pass), nothing runs the check of the outer user, and nothing
tests the trigger-level `DELETE` checks that are the only check without the hook.
Suggested tests are in the table. The helper's table with the exact change of
each mutant is available on request.

### L10-14 Low: honest paths no longer run under the guard in several tests

`ledger_head`, `ledger_verify` (the part that builds a block), `ledger_format`,
`ledger_records` and `ledger_updatable` now `SET kingres_ledger.allow_manual_write
= on` for sessions that only run honest operations; `t/007` and `t/011` use it to
run 0.1 and 0.2 SQL, so the refusal with the version message is tested only for
the 0.2 `ledger_build_block`. These tests no longer show that an honest
sequence is accepted by the guard; `ledger_guard`, `t/014` and `t/015` still do.

### L10-15 Low: the update-path fingerprint ignores column privileges

`t/011_upgrade_0_3.pl`: the fingerprint has no `pg_attribute.attacl`; a mutant of
the 0.1 to 0.3 script that drops the column grant on `ledger_digest` passes the
whole suite. Add `attacl` (and, as listed in the task, `pg_init_privs`, default
ACLs).

### L10-16 Note: exit sites

Three of the twelve rows "no portable scenario" can be reached on Unix (helper):
`chmod 000` of the digest directory (status 2), `ulimit -f 0` for `trust init`
(status 2, leaves a 0-byte file that blocks a re-run), `chmod 000` of the
checkpoint file. Outside the derivation: a failed write of stdout (L10-08) and a
Go runtime panic (status 2, as an input error); none reachable was found (30 s of
fuzzing four targets). The trace hook is a nil package variable only tests set;
no environment variable or argument reaches it.

### L10-17 Note: `blocks`

A cut or unanchored export gives `VERIFIED: the 3 block headers read (heights 0 to
2) form an unbroken chain from the genesis hash.`, status 0, JSON `verified: true`,
`complete_export_shown: false` (helper). The text says what was established; a
script that reads only the status concludes a pass, as for `checkpoint` without
`--state`. Direction: a weaker verdict word, or a member a script can test.

### L10-18 Note: parser and message details

`jsonx` accepts 33 nested arrays when the innermost is empty (as `encoding/json`;
the README says 32). The verifier requires `tx_count >= 1` and a `block_hash` in
chain elements, the ledger README allows `tx_count` 0 and no `block_hash`. Digest
timestamps go through RFC 3339 parsing and accept offsets. A refused first state
says "must not be deleted" about a file that does not exist. A state record's
`verified_by` and `verified_at` are not checked against anything. The extension
line cap is reported as "FAILED at step checkpoint", not as a parse failure.

### L10-19 Note: 528 tests on 18, 527 on 19

Both runs pass. I did not find which subtest differs (the TAP output of the
tests does not keep the numbered lines in the per-file logs).

## What held under attack

Ledger: every statement in the first table of "The guard" from a role that is not a
superuser and has no inherited privilege on the owner; the hook messages and the
trigger messages; `GRANT SET ON PARAMETER` of both settings (the grantee sets them
and gets nothing from `allow_manual_write`); `SET ROLE` after the setting; the
setting in a function definition; apply and table-sync workers for a superuser and
a non-superuser subscription owner with `run_as_owner` on and off; `GRANT`,
`ALTER FUNCTION ... OWNER`, `ALTER DEFAULT PRIVILEGES` on the internal function;
`pg_maintain`: `VACUUM FULL`, `CLUSTER`, `REINDEX` allowed and harmless,
`ALTER TABLE` and `OWNER TO` refused, strong locks refused; the database owner
cannot drop the extension.

Verifier (helper): the state moves only with a proof for forks, rollbacks, the
same note, sizes at every bound; the largest honest note (16 extension lines, 31
unknown signature lines, 47,732 bytes) gives a 92,449 byte state that reads back;
17 extension lines and a 1,025 byte line are refused before anything is stored;
`--now` that makes `verified_at` year -1 is caught by the read-back; a key at
`not_after` does not affect a held state; the JSON decoder against
`encoding/json` on 96 inputs: every accepted document has identical values and
every difference is a deliberate refusal (duplicate keys, lone surrogates,
invalid UTF-8, caps); numbers are kept as text and integers parsed with
`ParseInt`, so no precision is lost; the chain of a proof is only collected and
verified after the whole file parses; no input produced a line starting with a
verdict word; the cut marker cannot be produced; `%q` then escape in the same
order in every message tried; hostile 16 MiB inputs use 17 to 72 MB in under 0.05 s;
a link or FIFO at the lock name is refused; status 4 stays loud under a read-only
directory and a file size limit.

## Not reproduced

- A way for a role that is not a superuser, and has no inherited privilege on the
  owner, to write a ledger table or to have a statement of its own executed with
  the marker raised.
- A parallel worker or a cursor that outlives the marker.
- A verifier run that printed VERIFIED, exited 0 or advanced the state for a fork
  or a rollback without the path redirection of L10-06.
- A panic or a hang of the verifier on hostile input; the Windows failure of L9-13.

## Not covered

- The three update paths compared by my own catalog diff, my own run of
  `pg_upgrade` and my own `pg_dump` and restore (the suite's t/011 and t/016 pass).
- The block time rule against a real clock step, the control plane alert, the
  concurrency of `ledger_remove_stray` with the builder, a crash in the middle of
  it, a standby, the honest load with a hot standby and a crash.
- Logical replication on 19rc2; the library not preloaded in detail; a schema
  `kingres` that a non-superuser created before the extension.
- A real second local user for the verifier, the race between check and open,
  ACLs, Windows, macOS, network file systems, `go test -race`.
- A mutation run of the verifier; the helper mutated only the ledger. 86 of the
  129 mutants ran on 18 only.
- The control plane and everything after step 2 of the design.

## Baseline

- Ledger on 18.7 (assertions, `EXT_WERROR=1`, the verifier named in
  `KINGRES_VERIFY`): 16 of 16 regression tests, 16 TAP files, 528 tests, all
  passed (about 3.5 minutes). On 19rc2: 16 of 16, 16 TAP files, 527 tests, all
  passed.
- Verifier, Linux (Go 1.27.3, `CGO_ENABLED=0`; helper): `go vet` and `gofmt -l`
  clean; `go test -count=1 ./...` 14 packages, 210 top-level tests, 1,107 passing
  lines with subtests, 0 failed, 0 skipped.
- Mutants of the ledger: 129 made, 95 killed, 34 survived (10 equivalent or more
  restrictive), 43 also on 19rc2 with the same results.

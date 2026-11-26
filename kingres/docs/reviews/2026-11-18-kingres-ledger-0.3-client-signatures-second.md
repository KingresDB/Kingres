<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->
# Second independent review of client signatures in kingres_ledger 0.3 (the fixes for L21)

Reviewer: an independent reviewer. Date: 2026-11-18. Export reviewed: the committed tree named in `COMMIT` of the export.

## 1. Scope and method

Scope: the fixes for L21-01 to L21-08 of `kingres/docs/reviews/2026-11-18-kingres-ledger-0.3-guard-seventh-and-client-signatures.md`:
`contrib/kingres_ledger/src/sign.c`, `row_hash.c`, `capture.c`, `write.c`, the new `ed25519_check.c`/`.h`, the SQL scripts,
`t/036` to `t/039`, the README, design 3.6 to 3.8 and corrections 108 to 120 and 150 to 158. A focused round: deep on the
fixes, not broad.

Method. The export was copied into a private area together with one copy of each installation (PostgreSQL 18.7 and 19rc2,
both with assertions and OpenSSL). The extension was built out of tree with `kingres/dev/ext.sh` (clean build,
`-Werror`), the whole suite was run on each major, and then throwaway clusters were attacked with my own TAP files (a
private copy of the TAP framework confined to a private port range) and with one independent model of the key check in
Python integers. Seventeen one-line mutants of the code were built against 19rc2 (two of them twice, see section 7).
Everything was removed at the end; no server of mine is left.

My probes (all mine, none are in the export):

- `rev01`: 43 checks. Writers that hold no `ROW EXCLUSIVE` lock when the switch runs and write later, in the three
  isolation levels: the first write rolled back in a savepoint, nested savepoints rolled back to the outer one, an
  `EXCEPTION` block that rolled the write back, a trigger of another table, a rule of another table, `COPY ... FROM PROGRAM`,
  `MERGE`, `INSERT ... ON CONFLICT DO UPDATE`, first write an `UPDATE`, a `DELETE`, a savepoint-rolled-back `UPDATE`
  (updatable table, history path), a prepared statement run before and after the switch, a procedure that commits and writes
  again after the switch (gate by an advisory lock), `PREPARE TRANSACTION` after the switch.
- `rev02`: random interleavings with a gate technique. Per round a fresh table, three writers (`READ COMMITTED` 50 percent,
  `REPEATABLE READ` 25, `SERIALIZABLE` 25) and one switcher (role that is not a superuser) run 26 to 36 random steps (BEGIN,
  INSERT, INSERT plus `ledger_attest`, insert into another table to get an id, SAVEPOINT, ROLLBACK TO, RELEASE, COMMIT,
  ROLLBACK, a rolled-back write in an `EXCEPTION` block, a read) and one switch at a random step. A blocked statement is
  detected from `pg_stat_activity` only after the statement text (with a unique marker) is seen there, so the poll cannot read
  the previous state. At the end each round checks that every unattested entry of the table belongs to a transaction whose
  commit was issued before the switch was seen committed (a lower bound on "after", so it cannot raise a false alarm), that
  `ledger_verify` has no `unattested_entry` finding and no other finding.
- `rev03`: invariant B: what a role that owns a table can block with the function, the bound on the wait, the role's own
  `lock_timeout`, pipeline mode, two switches in opposite order.
- `rev04`: the cost of the per-session key cache. `rev05`: the cost of a write (`pgbench`, rolled-back inserts).
- An independent model of the key check (affine, complete Edwards addition, `[8]P` by double and add, square roots by the
  `(p+3)/8` method; none of the extension's formulas) against `ed25519_check.c` compiled into a small driver with the server
  headers of each major.

What I only read is said as such in each place and in section 9.

## 2. What was run

| Run | 18.7 | 19rc2 |
|---|---|---|
| clean build with `-Werror` | no warning | no warning |
| regression tests (19 files) | 19 of 19 | 19 of 19 |
| TAP suite `t/001` to `t/039` (38 files; no `t/024`) | 2460 tests, all pass (510 s) | 2460 tests, all pass (505 s) |
| `rev01`, 43 checks | all pass | all pass |
| `rev02` random interleavings | 150 rounds, no violation | 806 rounds (seeds 1 to 4), no violation |
| `rev03`, invariant B | 38 checks pass (after fixing my own `psql` pipeline syntax) | 39 checks pass |
| `rev04` key cache cost | run | run |
| key check against the model, special vectors (777) | agree | agree |
| key check against the model, random 32-byte strings | 30000 agree | 120000 agree |
| key check built with `-fsanitize=undefined,address -fno-sanitize-recover=all` (`-O1`) | not repeated | the 777 and 15000 random strings: no report, no difference |
| mutants (section 7) | none | 17 mutants, 19 builds |

`rev02` detail, 19rc2: seed 2 (300 rounds, 30 steps): 440 commits issued after the switch was seen committed, every one with
all entries attested; 430 commits refused with `KL010`; the switch had to wait for a writer in 261 rounds and a writer had to
wait for the switch in 286 steps. Seed 3 (250 rounds, 36 steps): 408 and 415, 211 and 256. Seed 4 (250 rounds, 36 steps):
362 and 432, 217 and 249. 18.7, seed 2 (150 rounds): 221 and 207, 131 and 141. Every round ended with `ledger_verify`
clean. The harness is sensitive: the mutants of section 7 for the lock, the latest snapshot and the reset at subtransaction
abort are killed by it (m1, m2, m4, m5b, m11).

Both suites are green. Everything below is what they do not see.

## 3. Fate of L21-01 to L21-08

| Finding | State | Evidence |
|---|---|---|
| L21-01 degenerate public key | fixed | `kl_ed25519_key_defect` agrees with the model on every vector, section 4. `22023` at registration, `KL012` at `ledger_attest` for a row put in by hand (`t/038`, which passes; the three tests of the key and the cache rule are in the mutants: five mutants of the three tests are killed by `t/038`, the cache rule is not, L22-03). The standalone verifier was not looked at. |
| L21-02 switch ordered with writers (critical) | fixed | The claim is true in everything I built: see section 4 and 5. It rests on one line of `capture.c` that no test pins (L22-01). |
| L21-03 healthy database reported damaged | fixed | `rev02`: no `unattested_entry` finding after any of 806 plus 150 rounds, which include transactions with an id above the switching transaction's that commit before the switch (a transaction that takes an id by writing another table first). `t/036` G passes. |
| L21-04 switching transaction's own earlier writes | fixed | `t/036` H (nine sequences, savepoints included) passes; mutant without the seeding is killed by `t/032` and `t/036`. |
| L21-05 test gap in `t/032` (nesting) | fixed | `t/037` runs two nesting levels against a model and passes; mutant `m4` of the previous review was not rebuilt by me. |
| L21-06 default of `enable_credit_limit` | fixed | default is 60000 ms (`src/kingres_ledger.c:70`). Design 156 says "one minute"; consistent. |
| L21-07 copies of a database | documented | README "Copies of the database" and design 157. Not run. |
| L21-08 test hook | not changed | `kingres_ledger.enable_batch_delay` is still production code; unchanged note. |

## 4. The claim of L21-02: attacks and results

The claim: after the switch has committed, no transaction commits or prepares an entry of the table without an attestation,
at any isolation level, whenever it began.

What the code does (read, and confirmed by the runs): the writer takes `ROW EXCLUSIVE` at the statement that writes, which is
before any trigger runs, so the capture trigger always runs with the lock; the trigger reads the policy through
`kl_query_id(..., latest = true)` with `GetLatestSnapshot()` (`row_hash.c:224`) and keeps the answer in a per-transaction
cache only for that kind of read (`row_hash.c:338`); the cache is dropped at the end of every transaction in every way
(`capture.c:175`) and at the abort of every subtransaction (`capture.c:204`), which is where a lock taken by the aborted
subtransaction goes; a deferred trigger runs before the pre-commit callback (and `t/036` E checks it).

Runs, all in `READ COMMITTED`, `REPEATABLE READ` and `SERIALIZABLE` (`rev01`, 43 checks, both majors), each ending in
`KL010` at `COMMIT`:

- first write rolled back in a savepoint, the switch commits, a second write; the same with nested savepoints rolled back to
  the outer one; the same in an `EXCEPTION` block that caught an error after the write;
- first write by a trigger of another table, by a rule of another table, by `COPY ... FROM PROGRAM`, by `MERGE`, by
  `INSERT ... ON CONFLICT DO UPDATE` (update branch), by an `UPDATE` or a `DELETE` of an updatable table (the history path has
  a cache of its own, `kl_hist_hash`; it follows the same rule), by an `UPDATE` rolled back in a savepoint;
- a prepared statement executed in one transaction before the switch and again after it, in a block and in autocommit;
- a procedure that commits (the commit callback drops the cache) and writes again after the switch;
- `PREPARE TRANSACTION` after the switch with an unattested entry (`KL010`, nothing is prepared);
- the writer that took a transaction id below the date by writing another table first: `KL010`. This is the gap the author
  names. It cannot be made into a commit: the check is a callback at `PRE_COMMIT` and `PRE_PREPARE` that every ending of a
  transaction passes, and `ledger_verify` being blind to such a transaction therefore does not matter in ordinary reach.
  The README says it ("a superuser who bypasses the check could hide an entry of such a transaction").

Random interleavings (`rev02`, 806 rounds on 19rc2 and 150 on 18.7): no unattested entry was committed after the switch was
seen committed, in about 1200 commits issued after it. (SERIALIZABLE writers are in the mix; I did not count `40001`
separately, and none was a violation.)

Two switches: the second waits for the first's lock and finds the policy set (README, `t/036`); two switches of two tables in
opposite order by superusers end in `40P01` for one of them after `deadlock_timeout` (`rev03`, no hang).
A switch inside a prepared transaction: the writers wait, the lock is held across the prepare (`t/036` P, passes).
A hot standby that is promoted: not run (a standby has no writer; a prepared transaction keeps its locks across promotion;
by reading, nothing here depends on the standby).

A switch of a table while its `ledger_enable` runs: the registry row of the table is invisible to the switch until
`ledger_enable` commits, so the switch answers "not under ledger control" and does not wait (by reading, and by `t/036` H
for the same transaction). Not run as two sessions.

The date (`sign.c:819`): `ReadNextFullTransactionId() - 1`, 8 bytes, read after the lock. A cluster started with
`pg_resetwal -e 7 -x 5000` (full transaction ids around 30 billion): the entry's `ledger_txid` and `signature_required_from`
are both `bigint` with the same full id (30064776075 for both: the entry that committed just before the switch is not above
the date), the unattested write after the switch is refused with `KL010`, `ledger_verify` is clean. Subtransaction ids do not
matter (entries carry the top-level id). Transactions that hold an id above the date but have not written are handled:
they wait for the lock and the commit check holds them (random runs).

The policy read in `SERIALIZABLE`: a writer reads the registry with a snapshot that is not its own; the random runs have
`SERIALIZABLE` writers and no violation; I did not look for avoidable `40001`.

So: the claim is true in everything I tried. It holds because of four things together: the lock, the latest snapshot, the
drop of the cache at the abort of a subtransaction, and the drop at transaction end. The third has no test (L22-01).

## 5. The new lock against the guard, and what changed for honest use

Invariant B (`rev03`, 19rc2 and 18.7; limit 4 s for the role):

- The owner (not a superuser) that calls the function while a writer is open waits for the writers for exactly the limit
  (4.00 s) and gets `55P03` with the hint; with `lock_timeout = 0` set by the role: 4.00 s; with a longer own timeout (100 s):
  4.00 s; with a shorter own timeout (200 ms): `55P03` at 0.20 s (the plain server message, without the extension's hint).
- In pipeline mode (`psql \startpipeline`, the call and then `pg_sleep(30)`): the wait ends at 4.0 s with `55P03`; when the
  lock is free, the hold after the grant ends the session at 4.0 s and the switch goes with it.
- While the switch is queued behind an open writer: a later writer of the same table (another role) queues behind it and goes
  on when the switch gives up; a reader (`SELECT count(*)`), `ledger_verify`, `ledger_build_block`, `ledger_verify_recorded`, a
  superuser's `ledger_enable` of another table, a write to another table and a read of the ledger tables all run at once (at
  most 25 ms); `VACUUM` of that table waits (an ordinary conflict with `SHARE ROW EXCLUSIVE`, bounded by the limit).
- The same role can already hold the whole table with `LOCK TABLE ... ACCESS EXCLUSIVE MODE` in a block, with no bound, and
  readers wait for it (ordinary PostgreSQL; the extension does not refuse it). So the function gives a role that owns a table no
  new way to stop other users of that table; it gives a bounded one. It does not touch the builder, the capture of other tables
  or the verification. I found no hold of a lock of the extension with no timer.
- A transaction that holds a lock of the extension and would wait for the table: `KL006` before the wait (`t/036` O passes);
  a deadlock between a switch and a writer that holds an extension lock needs the writer to wait for the extension while the
  switcher holds the table, and the switch takes no extension lock, so I could not build one.

For honest use: a role that is not a superuser cannot call the function in a block (`25001`; README says it, and part B is
new in 0.3). A superuser's call waits with no bound for every open writer of the table and everything that writes the table
queues behind it (README says "the switch is queued, and later writers queue behind it"): see L22-05. Switching in the
transaction that enabled a table with rows owes an attestation per row and fails at `COMMIT`: see L22-07.

Cost (19rc2, assertion build, machine shared with other work, single client, `synchronous_commit = off`, 6000 rolled-back
single-row insert transactions, three runs): plain table 163 to 171 us; ledger table with the policy `optional` 272 to 312;
policy `required` 276 to 288 (the tracking of owed entries is not measurable); two ledger tables in one transaction 435 to 467.
The README's own before and after numbers (about 30 us for the second snapshot) are consistent with that order; I did not
measure against the version before the fix, and I did not measure an attested transaction end to end (see L22-02 for the
attest cost).

## 6. L21-01, the arithmetic

Model against `kl_ed25519_key_defect`, 19rc2 and 18.7 headers, `-O2` and the sanitizer build:

| Vectors | Count | Model | Extension |
|---|---|---|---|
| the eight points of order 1, 2, 4, 8 in both sign encodings (found as `[l]Q` of random points) | 16 | 14 small order, 2 not canonical (sign bit on `x = 0`) | identical |
| points of order l, 2l, 4l, 8l: `[8]Q + T` for 40 random `Q` and the 8 torsion points `T`, both sign bits | 640 | all accepted (mixed order) | identical |
| non-canonical y (p to 2^255 - 1) with both sign bits | 40 | all not canonical | identical |
| edge values: y in 0, 1, 2, 3, 4, 5, 18, 19, 20, p-1, p-2, p-3, p-19, p-20, (p-1)/2, (p+1)/2, 2^254, 2^254-1, 2^255-20, both signs; all-ones; all-zero; `ff..7f` | 41 | as computed | identical |
| real public keys `[k]B` | 40 | accepted | identical |
| random 32-byte strings (60019 on the curve, accepted; 59981 off the curve) | 120000 (19rc2), 30000 (18.7) | as computed | identical |

Mixed order is accepted by design: a key with a small-order component next to a component of order l is accepted, its holder
has the private scalar of the large part; the README says so, and it means one client can hold several encodings of "the same
key plus a torsion point" as different key identifiers (each is a different point; none is a forgery). I state it, I do not
count it as a defect.

Reading the arithmetic: limbs of 8 bits in `int64`, every operation ends in `fe_carry`; `fe_sub` adds 4p so no limb is
negative (the limbs of the subtrahend are at most 255); `fe_carry` terminates (the carry shrinks by a factor of about 2^8 a
round), all values stay non-negative so the signed shifts and the `&` act on non-negative numbers; `fe_reduce` is two
conditional subtractions of p, enough for a value below 2^256 = 2p + 38; `fe_is_zero` accepts 0, p and 2p. The doubling
formula `y(2P) = (y^2 + x^2) / (2 - y^2 + x^2)` is the Edwards doubling with the curve equation used for the denominator; the
three doublings reach `y = 1` for exactly the points whose `[8]P` is the identity (checked by the model on 120000 random keys and the
torsion set). No undefined behaviour in the sanitizer run, no sign-bit alias accepted.

The per-session cache of judged keys (`ed25519_check.c:361-392`): it is keyed by the 32 bytes of the key, and a function of
nothing else, so a refused key cannot become accepted through it as the code is, and a revoked key cannot stay accepted
(the revocation is read from the table with the latest snapshot on every call, `sign.c:603-660`); the cache is
per process, so `ALTER EXTENSION UPDATE` does not touch it (a session that loaded the old library keeps its old code
anyway). Its size is 8 entries with first-in-first-out replacement, and two things are wrong with it (L22-02, L22-03).

## 7. Mutants (19rc2, one line each, built with `-Werror`)

Tests run for each: `t/032`, `t/036`, `t/037`, `t/038`, `t/039` and my `rev01`, `rev02`.

| # | Mutant | Result |
|---|---|---|
| m1 | the call `kl_lock_table_for_policy(relid)` removed (`sign.c:808`) | killed: `t/036` stops at its first wait, `rev02` |
| m11 | the conditional try takes `ROW EXCLUSIVE` instead of `SHARE ROW EXCLUSIVE` (`write.c`) | killed: `t/036` stops at its first wait, `rev02` |
| m2 | no latest snapshot (`pushed = false`, `row_hash.c:224`) | killed: `t/036` 34 of 151, `rev01` 12, `rev02` |
| m3 | a reader's answer is kept as "known" for a writer (`row_hash.c:338`) | killed by `t/036` (10 of 151, test D); `rev01` and `rev02` do not exercise it |
| **m4** | **no reset of the cache at the abort of a subtransaction (`capture.c:204-205`)** | **survives `t/032`, `t/036` to `t/039`** (and was not run against the regression tests); killed by `rev01` (12 of 43) and `rev02` |
| **m5a** | **date later by 1000000 (`sign.c:819`)** | **survives `t/032`, `t/036` to `t/039` and the 19 regression tests** |
| m5b | date earlier by 1000 | killed: `t/032` 5, `t/036` 11, `rev01` 2, `rev02` 2 |
| m6 | no seeding of the switching transaction's entries (`sign.c:874`) | killed: `t/032` 1, `t/036` 6 |
| m14 | no cache reset after the switch in the switching transaction (`sign.c:878`) | killed: `t/036` 2 (115, 116) |
| m7a | canonical test of y removed (`ed25519_check.c`) | killed by `t/038` (11) |
| m7b | sign bit on zero x not refused | killed by `t/038` (2: tests 27, 30) |
| m7c | small order not refused | killed by `t/038` (29) |
| m7d | on-curve test (Euler) removed | killed by `t/038` (11) |
| m7e | two doublings instead of three | killed by `t/038` (14) |
| m8 | the bound on the wait removed (`write.c:757`) | killed: `t/036` hangs in test Q until the harness timeout (I used 45 s) |
| **m9** | **a refused key is remembered as acceptable (`ed25519_check.c:386`)** | **survives everything** |
| m10 | the re-check at `ledger_attest` removed (`sign.c:639`) | killed by `t/038` (1) |

(m7b and m8 first failed to build under `-Werror` because my edit left a variable unused; I rebuilt them with `&& 0`. The tests
named were not the full suite for each mutant.)

Timing dependence of the verdicts: `waits_for_lock` in `t/036` polls `pg_stat_activity` until a backend whose statement text
contains the given text waits on a lock. A poll can only err towards "not yet", so an `ok(waits_for_lock(...))` can only fail
spuriously by timing out (180 s), not pass early; the statements that follow it assert states that the wait itself guarantees.
The wait could be on another lock of the same statement (a `Lock` wait type is not specific), I found none that could. The
timed cases use limits of 2 s against waits of 60 s. I did not run `t/036` under artificial load (the machine was loaded by
other work while I ran it: no failure).

## 8. New findings

### L22-01 medium: nothing pins the drop of the cache at the abort of a subtransaction, and the claim of L21-02 rests on it

File: `contrib/kingres_ledger/src/capture.c:204-205` (`if (event == SUBXACT_EVENT_ABORT_SUB) kl_table_cache_reset();`), tests `t/036`.

What is wrong. A writer that took its first lock on the table in a subtransaction and cached "optional" in it loses the lock
when the subtransaction aborts. Without the reset of the cache at that moment the next write of the table (after the switch has
committed) uses the stale "optional", the entry is not tracked and the transaction commits an unattested entry of a table with
the policy `required`. The code has the line; no test of the export has a sequence that needs it. With the line removed,
`t/032`, `t/036`, `t/037`, `t/038`, `t/039` all pass.

Steps (mutant m4, 19rc2; table `t` a ledger table, policy optional, role `app` the writer):
```
session 1: BEGIN; SAVEPOINT s; INSERT INTO t VALUES (1,'a'); ROLLBACK TO s;
session 2: SELECT kingres.ledger_set_signature_policy('t','required');   -- commits at once: no lock is held
session 1: INSERT INTO t VALUES (2,'b'); COMMIT;                         -- original: KL010; mutant: COMMIT succeeds
```
The same with nested savepoints rolled back to the outer one, in an `EXCEPTION` block, and with an `UPDATE` of an updatable table,
in all three isolation levels: 12 of the 43 checks of `rev01` fail on the mutant and none on the original; one check of `rev02` fails.

A fix must achieve: a test in `t/036` for each of those sequences in the three isolation levels (a write in a savepoint, rolled
back, the switch, a second write, expecting `KL010`; and the same in an `EXCEPTION` block), so that the mutant is killed.

### L22-02 medium: the cache of judged keys has 8 places with first-in-first-out replacement, so a session that signs for more than 8 keys pays the full judgment on every attestation, and the README says it is paid once per key

File: `contrib/kingres_ledger/src/ed25519_check.c:361-392` (`KL_KEY_CACHE 8`, `kl_key_cache_next`), README 1366-1368 ("the judgment of a key costs about three milliseconds, once per key and session").

What is wrong. A cyclic use of more keys than places is the worst case of first-in-first-out replacement: every lookup misses.
A service role that attests on behalf of many clients through one pooled session (the ordinary use of a key that "names a role",
or of a key with a null role) does exactly that.

Steps (`rev04`, both majors; 12 keys registered, `ledger_attest` called 600 times in one session with a signature that ends in `KL013` after
the key check, so the call measures the key check plus the verification, no insert):
```
keys in rotation:  1    4    8     9     12
us per call:      117  124  138  2697  2673     (19rc2; 18.7: 119 124 146 2744 2748)
```
From the ninth key the call is about 20 times slower: 2.6 ms for the judgment, on every attestation, against about 90 us for
the verification.

A fix must achieve: the cost of the judgment is paid once per key per session for as many keys as a session may use (for example
a hash table bounded by a few thousand keys, or least-recently-used replacement with a larger bound), or the judgment is not
repeated at `ledger_attest` for keys that the registry itself accepted (a flag in the row would need a new column; the other way
is to keep the re-check only for rows that the registration did not write, which the registry cannot tell today); and the README
sentence is made true or changed. A test with more keys than places in one session that checks the number of judgments (a counter
visible to the test, or a time bound with the margin of this measurement).

### L22-03 medium: nothing tests that a refused key is judged again on the next call, and a mutant that remembers it re-opens L21-01

File: `contrib/kingres_ledger/src/ed25519_check.c:386` (`if (defect == KL_KEY_OK)` guards the insertion), `t/038` (one `ledger_attest` call per session for the identity key).

What is wrong. The comment says a refused key "is judged again every time (it ends the call)". The line that makes it so has no test: with
`if (defect == KL_KEY_OK)` replaced by `if (1)`, every test of the export passes. With that change the first `ledger_attest` under a
key of small order that was put in by hand is refused (`KL012`) and the second call in the same session is accepted by the cache and
the forged signature (`R` = identity, `S` = 0) is verified by OpenSSL: the forgery of L21-01 is back for any session that tried once.

A fix must achieve: in `t/038`, call `ledger_attest` twice in one session (in two transactions or two savepoints) for the identity
key and for the all-zero key, expecting `KL012` both times.

### L22-04 low: the date the switch writes is never checked from above, and `t/036` calls its own copy of the check `ledger_verify`

File: `t/032_attest_policy.pl:547-551` and `sql/ledger_sign.sql:496-512` (the only tests that expect an `unattested_entry` finding put `signature_required_from = 0` in the registry by hand), `t/036_policy_switch.pl:103-110` and uses at lines 210 and later (`unattested()` is a copy of the SQL of the check, named in the test titles as `ledger_verify`).

What is wrong. A date that is too late (mutant m5a: `+ 1000000`) makes `ledger_verify` blind to every unattested entry above the real date; all tests
and the regression tests pass. The tests for a date that is too early (G and the others) are good (m5b is killed). The author's argument that the
gap does not matter because the commit check holds is right in ordinary reach, but the check of `ledger_verify` is the second line and its date is
what `L21-03` and `L21-02` changed.

A fix must achieve: one test that uses the date the switch wrote (no hand-made date): switch, then as a superuser with the policy bypassed (as `t/032`
does) write an unattested entry in a later transaction and expect exactly one finding; and the same for a transaction whose id is just above the
date. The helper in `t/036` should call `ledger_verify()` where the title says so, or the titles should say what the helper is.

### L22-05 low: a superuser's switch waits with no bound and holds back every later writer of the table, and the operator is not told how to find the blocker

File: `contrib/kingres_ledger/src/write.c:740-800` (the bound is for a role that is not a superuser only), README "The policy `required`" (the lock paragraph), the error hint of the bounded case ("Wait for the open transactions of the table to end (pg_stat_activity), or ask a superuser to run the call").

What is wrong. A call that waits for a long writer (a batch of an hour) queues behind it, and every later writer of the table queues behind the call, so one
`SELECT ledger_set_signature_policy(...)` by an administrator stops all writes to the table until the batch ends or the call is cancelled. The README says
"the switch is queued, and later writers queue behind it"; it does not tell the administrator to set `lock_timeout` for the call, nor how to find the
blocker (`pg_blocking_pids`, `pg_locks`), and the hint with the sentence about `pg_stat_activity` is raised only for the bounded case. For a role that is not a
superuser the stall is bounded by `lock_hold_limit` (60 s by default) and can be repeated by the owner, as the owner can already do with `LOCK TABLE`
(measured, section 5).

A fix must achieve: the README gives the operator's recipe (set a `lock_timeout` for the call, query `pg_blocking_pids` for the pid of the call, cancel it), and the
hint is raised for a superuser's `lock_timeout` as well (or the function says in a `NOTICE` that it waits and for whom).

### L22-06 note: the date is a transaction id of this cluster

File: `kingres_ledger--0.3.sql:2160-2185` (`signature_required_from < e.ledger_txid`).

The date and the `ledger_txid` of the entries are full transaction ids of the cluster that made them. After `pg_upgrade` (ids are kept) nothing changes. After a
dump and restore into a new cluster the ids restart low while the restored registry keeps the old date, so `ledger_verify` would not look for entries above the
date until the new cluster's ids pass it (the commit check is unaffected). I did not run it (it depends on how the entries and ids of a dump are restored, which
is outside this round); I name it so that whoever documents restore knows the date is cluster-local.

### L22-07 note: a switch in the transaction that enabled a table with rows fails only at `COMMIT`

File: `contrib/kingres_ledger/src/sign.c:819-878`, README "What the switching transaction owes".

By design (correction 153) every row that `ledger_enable` entered in the same transaction is owed an attestation, so the transaction fails at `COMMIT` with `KL010`
after `ledger_enable` has hashed every row; for a large table the whole work is then rolled back, and the message asks for one attestation per row. The README says
"switch in a later transaction". I ran only the small cases (`t/036` H). The function could refuse early (the seeding query already counts the owed rows, and it
could raise an error naming the cause when the count is more than zero and the entries came from `ledger_enable`).

## 9. What held under attack

- The lock, the latest snapshot, the seeding of the switching transaction's entries, the reset after the switch, the reset at transaction end, the two caches
  (identity and history) and the commit callbacks: no unattested entry committed after the switch in 43 targeted checks and 956 random rounds on two majors.
- `KL010` at `COMMIT` and at `PREPARE TRANSACTION` for every path I built, through a trigger, a rule, `COPY`, `MERGE`, `ON CONFLICT`, a prepared statement, a
  procedure that commits, a deferred trigger (`t/036` E), a writer with an old transaction id.
- The bound on the wait and on the hold of a role that is not a superuser, in autocommit and in pipeline mode, with the role's own `lock_timeout` unset, 0,
  shorter and longer; the timer after the grant; `25001` in a block; `KL006` instead of a wait.
- What the queued switch does not block: readers, `ledger_verify`, the builder, `ledger_verify_recorded`, a superuser's `ledger_enable` of another table.
- The key check: no difference from an independent model on about 150000 vectors and no sanitizer report; the three tests are each killed by `t/038`.
- The date at epoch 7 (full ids near 3 * 10^10): consistent in both columns.

## 10. Verdict

**May the policy `required` be relied on?** Yes, for what the design claims, with the one caveat that the claim rests on the commit check. In everything I built
(targeted sequences in three isolation levels, random interleavings, prepared transactions, savepoints, triggers, rules, `COPY`, `MERGE`, a high epoch), no
transaction committed or prepared an unattested entry of the table after the switch had committed, `ledger_verify` reported no entry that was committed before
the switch, and the registry no longer accepts a key under which a signature proves nothing. The code of L21-01 to L21-04 is right as far as I could show.
`ledger_verify` is blind by design to an unattested entry of a transaction whose id is below the date; that transaction cannot commit it, so it matters only to
someone who bypasses the extension (a superuser), which the README says.

**What is not good enough yet:** the tests. One line (`capture.c:204`) carries the claim of L21-02 and nothing tests it (L22-01); one line carries L21-01 for a
second call in a session and nothing tests it (L22-03); the date has no test from above (L22-04). L22-02 is a real cost defect with a README sentence that is
not true for more than eight keys.

**Another review?** Not of the code. Before the policy is relied on in a release, the author should add the tests of L22-01, L22-03 and L22-04, show that
mutants m4, m5a and m9 are killed, and fix or document L22-02; a short check of those tests (re-running the three mutants) is enough. A review of the standalone
verifier against L21-01, as the design says it must apply the same three tests, is still due and was not part of this round.

## 11. Not checked

- The standalone verifier (`kingres-verify`) and its handling of small-order, non-canonical and off-curve keys: not looked at (design 155 says it must).
- A hot standby that is promoted; logical replication into a `required` table (the README's `t/039` passes in the suite, I did not rerun it separately);
  parallel workers; a commit callback of another module registered before this library's.
- Dump and restore, `pg_upgrade`, `ALTER EXTENSION UPDATE` from 0.1 and 0.2 beyond what `t/007`, `t/011` and `t/016` do in the suite.
- A PostgreSQL really built without OpenSSL: not available; `t/035` builds the extension with the macro and passes on both majors in the suite. The key
  check needs no OpenSSL and was built and run by me without it (it includes only `postgres.h`).
- `SERIALIZABLE` switchers (a superuser's switch in a serializable block) and avoidable `40001`; the random runs have `SERIALIZABLE` writers only.
- The attested write end to end: the cost was measured only for the key check and the verification (L22-02) and for the write path without commit;
  the README's own numbers were not repeated.
- The text of the error for a role's own shorter `lock_timeout` (I saw `55P03` at 0.2 s; I did not read the message).
- `t/036` under artificial load.
- A transaction that owes more than about 67 million entries (by reading, the bound is `repalloc` at 1 GB with an error).
- Mutants of the commit check itself (`kl_check_attested`) and of the guard: not repeated; the previous review has them.
- The regression tests were run against only one mutant (m5a); the TAP files named in section 7 were run against all.

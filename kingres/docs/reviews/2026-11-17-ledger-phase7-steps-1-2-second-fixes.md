<!-- SPDX-License-Identifier: AGPL-3.0-only -->

# Review of the second fix round for ledger phase 7, steps 1 and 2

Reviewer: independent, adversarial. Date: 2026-11-17.
Reviewed: the two commits of the second fix round, one for the verifier
`kingres/verifier` (the state file, the held notes, the output layer, the
exit statuses, `digest-chain` over a directory, `blocks` as a stream) and one
for the extension `contrib/kingres_ledger` 0.3, part A (`ledger_head`, the new
`ledger_verify` check, the README section on a row above the head, the direct
update script 0.1 to 0.3). Both were read from the commits only (exported
with `git archive`); nothing in the repository was changed except this file.
The reviews this one follows are
`kingres/docs/reviews/2026-11-16-ledger-phase7-steps-1-2-fixes.md` (findings
L8-01 to L8-11) and `kingres/docs/reviews/2026-11-16-ledger-phase7-steps-1-2.md`.

## Summary

The mechanism the round was written for holds. I found no way to make
`checkpoint --state` move to a checkpoint that was not proved, in the same run
and from the same bytes, to extend the stored one: the fork at a larger size,
the same size with another root, two keys signing two histories, a missing or
damaged tile, sizes 0 and 1, a rollback, a state in the earlier form, and
every malformed state file I tried all end with the state untouched and the
status the README gives. The note is read once and the tiles are checked
against the root of that very note, so a source that changes between reads
cannot move the state. The held-note rules agree between `checkpoint --state`
and `consistency --old` answer by answer. The messages never say to delete the
state. The 400 MB export of `blocks` runs in 12 MB. The direct update script
0.1 to 0.3 gives a catalog identical to a fresh 0.3 and to the two-step
updates (310 lines of catalog, on both majors), and no non-superuser can use
the update window.

What does not hold is the extension's stated limit, and it is worse than the
README says. A member of `pg_write_all_data` (and nothing else) can, with plain
`INSERT`s, add a block that `ledger_verify` accepts and that
`kingres-verify blocks` verifies, and every later honest block then carries the
time that member chose (L9-01, critical by the definition of this review: a
documented limit, reproduced end to end, not guarded). The same role can stop
`ledger_head` for good with one `INSERT` of a digest row, with
`ledger_verify` reporting nothing and the README's way out not applying
(L9-02). The README's way out itself leaves a finding that its text says will be
0 (L9-03). In the verifier the new state file can be made too large for the
verifier to read back (L9-04), and a 16 MiB JSON file costs 2 GiB of memory
(L9-05). The tests of the round are good where they were aimed (every mutant of
the proof-before-advance logic was killed) and silent in places the round
claims (the JSON `extends_state`, the strictness of the state record, a read
error in the `blocks` stream, `old size 1`).

Count: critical 1, high 0, medium 4, low 6, note 4 (15 findings).

- L9-01 Critical: a `pg_write_all_data` member forges a block and fixes every
  later block time, with `ledger_verify` and the verifier both clean
- L9-02 Medium: one digest row by the same role stops `ledger_head` for ever;
  `ledger_verify` is clean and the README recovery does not apply
- L9-03 Medium: the README recovery leaves a finding (`entry_orphan`) where the
  text says 0, deletes the evidence without keeping it, and opens a window
- L9-04 Medium: a validly signed note with a long extension line makes a state
  file that the verifier cannot read back (status 0, then status 2 for ever)
- L9-05 Medium: a 16 MiB digest or proof file uses 2.0 to 2.3 GiB of memory;
  four of them 3.1 GiB
- L9-06 Low: a digest directory with the file names of the README's own export
  command (a colon in the name) is refused
- L9-07 Low: a conflict file that is already there, with other content, is
  reported as "saved"
- L9-08 Low: the lock file is opened through a link and created where the link
  points
- L9-09 Low: the README exit-status table and `blocks` disagree, and the test
  that "walks every command" walks two
- L9-10 Low: the escaped text is ambiguous, and `--json` carries raw C1, DEL and
  bidirectional characters
- L9-11 Low: 19 of 60 mutants of the round's own fixes survive their tests (8
  of them matter)
- L9-12 Note: the lock is named by the file name, so two names of one file do
  not exclude each other
- L9-13 Note: the Windows failure was not reproduced; what the code does with
  its handles
- L9-14 Note: `pg_maintain` and `ledger_pending` (availability and a permanent
  finding)
- L9-15 Note: an empty state file is refused with a misleading message

## Scope and method

Read completely: `internal/cli/cli_state.go`, `cli_tlog.go`, `cli_out.go`,
`text.go`, `cli_trust.go`, the first 600 lines of `cli.go`,
`internal/statefile` (all files), `internal/safefile/dir.go` and the file
readers, `internal/chain/chain.go`, the verifier README (exit statuses, state
file, held checkpoints, sources), the tests `cli_state_test.go`,
`cli_state_lock_test.go`, `cli_status_test.go`, `cli_text_test.go`,
`statefile_test.go`, parts of `cli_tlog_test.go`. For the extension: the
commit's changes to `kingres_ledger--0.3.sql` (`ledger_head`,
`ledger_verify`), `ledger_build_block` and `ledger_generate_digest`, the
head of the 0.1 to 0.3 script, the hook statements of `src/protect.c` around
the script trust, and the README section "A row above the head". Not read line
by line: the 2,000 lines of the direct update script (they were compared by
their result), `cli_misc_test.go`, `stream_test.go` (names and what mutants
say), the C code of the block builder other than what the SQL shows.

Environment (Linux in WSL, Windows 11): Go 1.27.3, `CGO_ENABLED=0`, an export
of the committed tree in a work area of my own, PostgreSQL 18.7 and 19rc2
(assertion builds) copied into the work area with the prefix fixed, servers on
ports of my own, all stopped at the end. The verifier was built from the
export and named in `KINGRES_VERIFY` for the extension tests, so the tests that
drive it ran (none skipped).

What I did:

- Baselines (see the end), then my own tests inside a copy of the `cli`
  package (publications with forks, two keys, sizes 0 and 1, held-note
  matrices) and from the command line (the built binary on generated
  publications).
- 64 mutants of the round's fixes (60 applied and built; 3 did not build and 1
  did not apply, and were left out), each applied to a fresh copy of the tree and
  run against `internal/cli`, `statefile`, `chain`, `trust` and `safefile` (the
  table is in "The tests of this round").
- The state file against: a link, a FIFO, a directory, an empty file, a sparse
  file of 1 GiB, a record with duplicate keys, unknown members, wrong types,
  trailing text, a newer format, a record whose own fields lie, a plain note
  (the earlier form), a hard link, a read-only directory, a conflict name that
  is already taken (by a file, a link, a dangling link), a lock file that is a
  dangling link, two keys signing different trees, sizes 0 and 1 as stored
  states, and a validly signed note of 700 KB.
- Output: hostile file names (LF, ESC, U+009B, U+0085, DEL, U+202E, U+2028),
  an input that is itself the escape notation, flag errors, unknown commands,
  `--json` and the text of the same run.
- Directories and streams: 1,025 entries, 1,100 digests, names with a colon,
  a newline and a leading dash, a link out, a FIFO named `x.json`; a block
  export of 400 MB (1.2 million blocks), a line of 3 MiB, a line of 1 MiB with
  100,000 keys, a nesting of 500,000, a valid prefix followed by an oversize or
  a cut line; 16 MiB digest and proof files and a directory of four.
- The extension on both majors: a catalog dump of 310 lines compared across
  four ways to 0.3 (fresh, 0.1 to 0.3 direct, 0.1 to 0.2 to 0.3 in two
  commands, 0.2 to 0.3); the hook after each; the roles `pg_write_all_data`,
  `pg_maintain`, a table owner; a forged block and a forged transaction; a
  digest row; the README recovery run literally; eight kills with SIGKILL of the
  postmaster and its children under load (inserts, two block builders, a digest
  loop, a prepared transaction that holds entries across blocks), each followed
  by `ledger_head` and `ledger_verify`; a base backup taken while the builders
  ran, started as a hot standby with `wal_consistency_checking = all` on both
  sides, read by 60 `ledger_head` calls during replay, then promoted in the
  middle of more load.
- Windows: the whole verifier suite once, the state and held-note tests 50
  times, the `statefile` package 100 times, the `cli` package 8 times.
- Fuzz: four of the `cli` and `chain` targets, 40 s each.

## Fate of L8-01 to L8-11

- L8-01 (state accepts a fork): Fixed. Evidence: a fork at a larger size, a
  fork at the same size, two trusted keys signing two histories (size 300 and
  size 500), a missing tile (status 3), a damaged tile (status 1), a rollback:
  the state file is byte for byte the same afterwards, the conflict note is
  saved, the status is 1 or 3 as the README says. Sizes 0 and 1 as stored
  state advance only with a proof (size 0 needs none, size 1 does). The note is
  read once and the consistency proof is checked against the root of those
  bytes. Test without the fix: yes for the proof, the fork and the rollback
  (mutants S1 to S5, S13 killed); not for the stored size 1 (S6 survives), not
  for the JSON `extends_state` (L9-11)
- L8-02 (publisher text in the output): Fixed for the text output; a residue
  in L9-10. Evidence: every hostile string of the test and mine (LF, CR, ESC,
  C1, DEL, bidirectional marks, U+2028, an overlong value, bytes that are not
  UTF-8, a flag name, a command name) comes out escaped on stdout and stderr; no
  input produced a line that starts with a verdict word. Test without the fix:
  yes for the control characters, the cut, the error line, the flag error and
  the unknown command (T1, T4, T6, T8, T10, T11, T15, T16 killed); no for
  invalid UTF-8 (T5), the step detail (T7), the success headline and the failed
  step name (T13, T14)
- L8-03 (held notes judged alike): Fixed. Evidence: a note signed by two keys
  with one revoked: 0 for `--state`, `--old` and as a new note; a corrupt
  signature of a revoked key: 2 and 2 as held, 0 as new (documented: a held
  note allows no wrong signature of any key of the file); a note of a key past
  `not_after`: 0 and 0 as held, 1 as new; a signer not in the trust file: 2 and
  2. No message tells the auditor to delete the state (S12 killed). Test without
  the fix: yes (S10, S11, S12 killed)
- L8-04 (two runs lose the higher state): Fixed. Evidence: six concurrent
  runs in the test and the lock timeout; a lock held by another run gives
  status 2 after the wait and nothing is changed. Test without the fix: yes
  (S9, L4 killed). Residue: L9-08, L9-12
- L8-05 (`digest-chain` caps and globs): Fixed. Evidence: 1,025 entries and
  1,100 files refused with status 2, a name with glob characters is a name, a
  link and a FIFO named `x.json` refused, 64 MiB total cap. Test without the fix:
  yes (D1, D2 killed). New: L9-06, L9-05
- L8-06 (exit statuses): Partly. Evidence: the table is in the README and the
  42 scenarios of the test follow it. The test covers `checkpoint` and
  `consistency` only; `blocks` ends 1 for a file that cannot be parsed where the
  table says 2 (L9-09). Test without the fix: yes for the three places of the
  review (E1, E3, E5 killed)
- L8-07 (state file details): Fixed. Evidence: a link is followed and stays a
  link (and a dangling one is refused), a 200 byte name works, an existing mode
  is kept. The mode of a new file (0644) is not tested (S19 survives); the link
  tests do not run on Windows (skipped)
- L8-08 (`ledger_head` stops for ever): Partly. The error now names the row
  and `ledger_verify` has the new check. But the recovery text does not give 0
  (L9-03), another row that stops `ledger_head` is not covered (L9-02), and a
  non-superuser can write both (L9-01). No honest path to such a row was found
  (below)
- L8-09 (key windows): Fixed. Evidence: `--now ""` is status 2, the fraction is
  printed, the boundary is inclusive as the README says, a comma is refused.
  The printing of the fraction is not tested (E7 survives)
- L8-10 (error text tells EXECUTE-only roles a block number): unchanged and
  accepted by the README; still true
- L8-11 (memory of `blocks`, two wordings): Fixed. Evidence: a block export of
  1,200,000 blocks (403 MB) verified in 3.7 s with a maximum resident set of 11.8
  MB; a line of 3 MiB or a line of 1 MiB with 100,000 keys is a parse failure
  with 7 MB; a nesting of 500,000 is refused at depth 32; `row-hash --json` says
  `verified: false`. A read error in the stream is not pinned by a test (B2
  survives)

## Findings

### L9-01 Critical: a member of `pg_write_all_data` forges a block and fixes every later block time

Place: `contrib/kingres_ledger/src/protect.c` (the only hook is
`ProcessUtility_hook`; no `INSERT` is looked at), the README "What the
protections cannot stop", and the stated limit of the commit.

The README says that such a member "can insert into every ledger table, a
valid successor block included", and calls it a limit. What a forged block does
is not written there, and it is more than "a valid block that was not
published": the builder takes the time of a block as `greatest(clock, time of
the previous block)`, so one forged block with a time in the year 2100 makes
every later honest block carry that time. Nothing in the database or in the
verifier says so.

Steps (both majors; `wr` is `CREATE ROLE wr IN ROLE pg_write_all_data`, no
other privilege, `app` owns one ledger table; the superuser only computes the
literals with the public functions):

```
-- a chain with block 0 built from one insert by app
select height+1 as nh, block_hash as ph from kingres.ledger_blocks order by height desc limit 1 \gset
select '2100-01-01 00:00:00+00'::timestamptz as nt \gset
select sha256('') as e0 \gset
select kingres.ledger_merkle_root(kingres.ledger_block_tx_leaf(900000001, 0, :'e0', 'app'::name)) as broot \gset
select kingres.ledger_block_hash(1::smallint, :nh, :'ph', :'broot', 1, :'nt') as bh \gset
set role wr;
insert into kingres.ledger_tx values (900000001, :nh, 0, 0, :'e0', 'app');
insert into kingres.ledger_blocks values (:nh, 1, :'ph', :'broot', 1, :'nt', :'bh');
reset role;
select count(*) from kingres.ledger_verify();         -- 0
-- app inserts a row, ledger_build_block() builds block 2
select height, closed_at from kingres.ledger_blocks;   -- block 2 closed_at 2100-01-01
select count(*) from kingres.ledger_verify();         -- 0
```

Observed (19rc2; the variant with a block of `tx_count` 0 and no transaction was
run on both 18.7 and 19rc2 with the same result): two `INSERT 0 1`,
`ledger_verify` returns no row, and after an honest insert and build the honest
block 2 has `closed_at = 2100-01-01`. The export of the headers with the README's recipe and a digest
of block 0 or of block 1 give `kingres-verify blocks`: `VERIFIED ... the chain
reaches the digest at height 1`, status 0. A digest generated afterwards covers
the forged block. Variants I tried: a block with `tx_count` 0 is also accepted
by `ledger_verify` (the verifier's parser refuses a header with `tx_count` 0, so
the verifier does see that one); a transaction with entries for the append-only
table is found (`table_rows`, `entry_op`) unless the entries match a row, so the
undetected forgery is a block without effect on rows and with a chosen time,
and a transaction that claims nothing. `DELETE FROM kingres.ledger_pending` by
the same role works too (the entries it leaves are then reported as
`entry_orphan`, which is detected, but permanent).

Expected: no role that is not a superuser changes the chain, or its times,
without `ledger_verify` reporting it.

Direction: the section "The insert guard" below.

### L9-02 Medium: one digest row stops `ledger_head` for good, `ledger_verify` is clean

Place: `ledger_head` (the check of the newest digest), README "A row above the
head" and the hint of the error.

Steps (the same role, one statement):

```
set role wr;
insert into kingres.ledger_digest(generated_at, generated_by, height, block_hash, digest)
values (now(), 'x', 0, sha256('wrong'), '{}');
reset role;
select * from kingres.ledger_head();
  -- ERROR: block 0 is not the block that digest 2 in kingres.ledger_digest names
select count(*) from kingres.ledger_verify();      -- 0
-- the README recovery for ledger_digest (delete where height > head): DELETE 0
select * from kingres.ledger_head();               -- the same error
```

Observed as shown (19rc2; 18.7 has the same function). `ledger_head` stops, which
stops a publisher that calls it first; `ledger_verify` has no check for a digest
whose hash is not the block's (the code says so in a comment); the hint of the
error sends the operator to the section "A row above the head", whose recipe
removes only rows above the head, so it removes nothing here. The operator has
no way out in the documents.

Expected: `ledger_verify` reports what `ledger_head` stops on, and the README
says how to remove a digest row that names the wrong hash (or that the row must
be removed by its id), or the guard of L9-01 keeps the row out.

Direction: add a `digest_hash` check to `ledger_verify` for the newest digest
(or every digest), extend the recovery text (delete by `digest_id`, keep the row
first), and change the hint.

### L9-03 Medium: the README recovery leaves a finding, deletes the evidence, opens a window

Place: README "How to remove it".

Steps (an honest chain of three blocks and a digest at height 2; the row of
block 2 removed from `ledger_blocks` alone with the trigger off, which is the
case the section is about; then the recipe, literally, for `ledger_tx` and then
for `ledger_digest`):

```
select * from kingres.ledger_head();   -- ERROR: the head is block 1, but the row of transaction 894 ... names block 2
select check_name, object from kingres.ledger_verify();   -- tx_block, digest_block
-- the recipe for ledger_tx, then (head still fails on the digest) for ledger_digest
select * from kingres.ledger_head();                      -- block 1, works
select count(*) from kingres.ledger_verify();             -- 1   (the README says 0)
  -- entry_orphan | tx 894 | 1 entries belong to no block and to no pending transaction
-- the next insert and ledger_build_block() builds a block 2 that is not the removed one
select count(*) from kingres.ledger_verify();             -- still 1
```

Three things.
(1) The text says the result is 0; for the case "blocks were removed", which is
the one with entries, it cannot be: the entries of the removed transactions stay
(`ledger_entry` is append-only for the recipe too) and are reported for ever.
Following the text to the letter an operator concludes that the recovery failed.
(2) The recipe deletes the rows without keeping them. The text says to keep a
record outside the database but gives no statement that does it, and the rows
are the only trace of the removed block. (3) While the trigger of a table is
off, any new session can update or delete that table, and a member of
`pg_write_all_data` needs no table privilege: with the trigger of `ledger_tx` off,
`set role wr; update kingres.ledger_tx set user_name = 'mallory';` gave
`UPDATE 2` (no `WHERE` is needed; with one the role has no `SELECT`). The change
was found afterwards (`block_tx_root`), so this is a window, not a hiding place;
it is not mentioned.

Direction: say that the count is the number of orphan entries of the removed
transactions, and how to list them; add `CREATE TABLE ... AS SELECT` or `\copy`
of the rows to a file before the `DELETE`; say that the sessions of other roles
must be kept out (`ALTER DATABASE ... ALLOW_CONNECTIONS false`, or single user
mode) between the two catalog updates.

### L9-04 Medium: a state file the verifier cannot read back

Place: `cli_state.go` `encodeState` (no size check), `maxStateFile = 3 << 20`,
`signednote.MaxNoteSize = 1 << 20`.

A checkpoint is a signed note of up to 1 MiB, and extension lines may hold any
text without control characters. The state record holds the note as a JSON
string, and the encoder writes `<`, `>`, `&` as six bytes (and U+2028 as six).
A validly signed note whose extension line is 700,000 times `<` gives a state
file of 4,200,728 bytes.

Steps (a test in the `cli` package; the signing key is a key of the trust
file):

```
checkpoint --state st ...   -> status 0, the state is written (4,200,728 bytes)
checkpoint --state st ...   -> status 2:
  --state: ... is larger than the limit (4200728 bytes, at most 3145728)
```

Observed: the first run says VERIFIED and "the state file was updated"; every
later run, and `consistency --old st`, ends with status 2. The state is kept
(nothing is overwritten), but the protection against rollback and forks is off
until the auditor re-anchors, and the message does not say why. The signer has
to hold a key of the trust file, which is the threat the state exists for.

Expected: the verifier never writes a state it cannot read.

Direction: before the replace, refuse to advance (status 2, state kept) when the
record is larger than the limit, or store the note base64 encoded, or lower the
size of a note accepted into a state, or raise the read limit above the worst
encoding (7 MiB) and say so in the README.

### L9-05 Medium: 2 GiB for a 16 MiB JSON file

Place: `internal/jsonx` (a `Value` with a path string for every node),
`chain.ParseDigest`, `proof.Parse`; the size limits `maxFileSize` (16 MiB) and
`maxDigestTotal` (64 MiB).

Steps (Linux; a digest with an unknown member that holds an array of 8 million
zeros, 16,777,205 bytes):

```
kingres-verify digest-chain big.json d1.json    -> CONSISTENT, 2.6 s, 2,118,536 KB resident
a directory of four such files                   -> CONSISTENT, 8.2 s, 3,285,620 KB
kingres-verify proof --proof bigproof.json ...   -> VERIFIED (a 16 MB proof with an extra member), 2,404,428 KB
```

Observed: 126 bytes of memory per byte of input. The limits of the files hold
(an object of 22 MB is refused before it is read), but a digest is a few
hundred bytes and the verifier runs on an auditor's laptop. A hostile digest
directory or proof file (the database server's output) is enough. Not a wrong
verdict; a process killed for memory ends with a signal and never prints
VERIFIED.

Direction: cap the number of values and the total of path bytes in `jsonx`
(a digest has fewer than 20 members), or parse the known members without
building the tree; a digest larger than 64 KiB is not a digest.

### L9-06 Low: a digest directory with colons in the names is refused

Place: `safefile/dir.go` (`CleanRel` refuses `\`, `:` and NUL; `ReadFile` is
used for the names that `RegularNames` returned), `cli.go` `runDigestChain`.

The README's export command writes `digest-$(date -u +%FT%TZ).json`, a name
with colons. Steps: two such files, `kingres-verify digest-chain d1 d2` is
`CONSISTENT`, `kingres-verify digest-chain --digests dir` is status 2 with
`path "digest-2026-11-14T17:21:53Z.json" has a backslash, colon or NUL`. The
message names a path rule where the cause is a directory listing.

Direction: read the names that the listing returned through a function that
takes a name, not a path (no path rule applies to a single name), or change the
export command.

### L9-07 Low: the conflict file is reported saved when another file is there

Place: `statefile.SaveConflict`, `cli_state.go` `conflict`.

`O_EXCL` is right, but a file that exists is taken to be "the same evidence".
Steps: a fork at size 300; the conflict file `st.conflict-300-3870fe03` is
written; replace its content by the text `PLANTED, not the note`; run the same
fork again: the output says `the conflicting signed checkpoint (size 300) was
saved to ...st.conflict-300-3870fe03; keep both files` and the file still holds
the planted text. The same with a link at that name (the link target is not
written, and the message says saved) and when an earlier write was cut short by
a full disk (a partial file stays). The name has 4 bytes of the root, so two
different notes of one size with the same first 4 bytes (a signer can grind one in
2^32 tries) share a name and the second is dropped silently. Hostile input cannot
choose the name beyond decimal digits and hex, cannot overwrite a file and adds
one file per run at most, so this is about the claim, not about damage.

Direction: use the whole root in the name (64 hex fit), and when the name exists
compare the content and say "a different file is there".

### L9-08 Low: the lock file is opened through a link and created where it points

Place: `statefile/sync_unix.go` and `sync_windows.go` `Acquire`
(`os.OpenFile(lockName(path), O_RDWR|O_CREATE, 0644)`), `lockName` (the name is
a hash of the base name of the state, so another user of the directory can
predict it).

Steps: a state directory in which `.kingres-state-<12 hex>.lock` is a link to a
file that does not exist; `checkpoint --state` ends with status 0 and an empty
file has been created at the target of the link (reproduced). By reading the
code, not run: a link to a directory is status 2, and a link to a FIFO is opened
read-write (which does not block on Linux) and locked. A lock file that another user can open for reading can be held with
`flock` by that user for ever: status 2 after 10 s, nothing changed (safe, and
documented by the README as a stuck process). The state directory is meant to be
the auditor's own, so this needs a hostile writer in it; it is the case the task
names.

Direction: open with `O_NOFOLLOW` and refuse anything that is not a regular
file (on Windows `FILE_FLAG_OPEN_REPARSE_POINT` and the same check); create it
`0600`.

### L9-09 Low: the exit-status table and `blocks`; the test walks two commands

Place: README "Exit statuses" and "What is status 2, 3 or 1",
`cli_status_test.go`.

The table says status 2 for "block export when it cannot be parsed as an
export"; `kingres-verify blocks --blocks garbage --chain-id 00..` prints `FAILED
at step parse` and ends with status 1, and the section on `blocks` says "a parse
failure and a chain failure are both status 1". The README also says the rule is
"tested path by path in `TestExitStatusFollowsTheRule`" for "every command"; the
test has 42 scenarios, all of `checkpoint` and `consistency`, and none for
`proof`, `row-hash`, `digest-chain`, `blocks` or `trust` (the commands of
`cli.go` have 28 call sites of the input-error path and 3 of the failure path,
`cli_trust.go` has 14 of the input-error path; the whole package has 57 and 14). Status 3 cannot be produced by an operator to hide a
fork in a way that looks like a pass: it prints `INCOMPLETE`, a first line that
says to treat it as an alert, a `missing` list, never a verdict word, and the
README says that 3 is not a pass (by design an operator can still turn a 1 into
a 3; nothing turns a 1 or a 3 into a 0).

Direction: one of the two sentences about `blocks` is wrong; make the table
true, and either extend the test or say that it covers two commands.

### L9-10 Low: the escaping is ambiguous; `--json` is raw for C1, DEL and bidirectional characters

Place: `text.go` (`untrusted` leaves the backslash alone), `cli_out.go`
(`printJSON`).

(1) Two different inputs print the same: a file name with a real newline and a
file name with a backslash and an `n` both print as `./x\ny.json` (reproduced);
the same for `\x1b` and ESC. The auditor cannot recover the bytes. The README
says the backslash is left alone for the sake of Windows paths; escaping it
would cost a doubled backslash in Windows names.
(2) In `--json` output a file name with U+009B, DEL and U+202E passed through raw
(only U+2028 is escaped by the encoder), and invalid UTF-8 becomes U+FFFD
without a trace. The README says the JSON is ordinary and must not be copied to
a terminal; a CSI introducer in a C1 form is still one on some terminals.
(3) `TestNoPrintOutsideTheOutputLayer` is a substring scan with a whitelist of
substrings (`usageText`, `trust follow`, ...): any line that contains one of
them is allowed whatever it prints.

I did not find a string that starts a line, ends one or moves a terminal in the
text output: ESC, C1 (also as the two bytes of UTF-8), CR, TAB, DEL, the
bidirectional marks, U+2028, U+2029, U+FEFF, tag characters and an overlong value
all come out as `\x..`, `\u....` or cut. The flag package's messages and the
unknown command go through the same function; nothing in the sources panics on
these inputs (fuzz and the runs above).

Direction: escape the backslash as `\\` in the text form (or print file names
with `%q`), and for `--json` escape U+0080 to U+009F, DEL and the bidirectional
characters.

### L9-11 Low: 19 of 60 mutants of the round's fixes survive

Place: the tests of the round; see the table below.

Of the 19, three are equivalent (S21, T2, T3), two cannot be tested (the two
`fsync` calls, S15 and S16) and one is harmless (a cut inside a character, T12).
The eight that matter: `blocks` ignoring a read error (B2: an export cut by the
reader's limit on a later line would be reported as verified up to the cut when
no digest is given; the code is right, nothing pins it); the JSON
`extends_state` always true (L9) and a first use that claims a proof (L10), the
member the round introduced so that a script can tell a proved extension from a
recorded first checkpoint; the state record accepting text after it, unknown
members and any `format` (S23 to S25: the checks exist, no test pins them); a
stored size of 1 treated as the empty tree (S6: a state of size 1 would advance
to any tree without a proof); the time fraction (E7). The others: the alert
sentence of the help (L3), the caveat of the damaged tile on `--state` (L8), the
mode of a new state file (S19), a conflict name for a long state name (S22), and
four output-layer mutants that are defence in depth (T5, T7, T13, T14).

### L9-12 Note: the lock is named by the file name

`lockName` is a hash of the base name of the resolved path. Two names of one
file (a hard link) take two different locks and do not exclude each other;
reproduced: the state `a` and its hard link `b`, a run through `b` leaves `a` at
size 300 and `b` at size 700 (the safe direction, an older state). On Windows
the case of a name that does not exist yet is as typed, so two spellings of a
new state could take two locks (by reading the code; not run). No data is lost;
a file name cannot stand for a file.

### L9-13 Note: the Windows failure was not reproduced

`go test -count=50 -run 'State|HeldNote' ./internal/cli/ ./internal/statefile/`
passed (308 s), `./internal/statefile/` 100 times passed (30 s), the whole `cli`
package 8 times passed (84 s), and the whole suite once (154 passed, 6 skipped,
`go vet` and `gofmt` clean). Two remarks. The suggested command runs no test of
`statefile` at all (no test name in that package contains `State` or `HeldNote`;
the output says `[no tests to run]`), so the crash and lock tests were run by
me separately. Reading the code for a handle left open when a test ends: the
lock file is closed by `Release` (deferred right after `Acquire`), the
temporary file is closed on every path of `Replace` including the stops of the
test hook, `ReadArg` and `Dir` close with `defer`, the source directory is
closed by the `defer in.close()` of each command and on the error path of
`loadLogInputs`. I found none. A directory that is "not empty" at the end of a
test on Windows is the sign of a file that was deleted while a handle was open
(it stays until the handle closes) or of a scanner opening the new file for a
moment; I could not tell which, and a scanner is the likelier cause.
`TestStateThroughALinkAndWithALongName` returns with a log line, not a skip,
when a link cannot be made, so on Windows the long-name part runs and the link
part does not, and counts as passed.

### L9-14 Note: `pg_maintain`, and `ledger_pending`

`pg_maintain` can `VACUUM`, `CLUSTER`, `REINDEX` and `ANALYZE` the ledger tables
(no effect on the chain) and can hold `ACCESS EXCLUSIVE` on `ledger_blocks` inside
a transaction (reproduced), which stops the builder and every reader: an
availability limit that the README does not list. A `pg_write_all_data` member's
`DELETE FROM kingres.ledger_pending` works (the trigger allows delete on that
table); the pending entries are then `entry_orphan` for ever (detected, and with
no way out in the README).

### L9-15 Note: an empty state file

`touch st` and then `checkpoint --state st` ends with status 2 and `not a
checkpoint of this log: note does not end with a newline`. Safe, and a person
who made the file with `touch` gets a message about a note. A file of size 0 could
be taken as "no state" (the first-use path).

## The insert guard

The limit as written: a member of `pg_write_all_data` bypasses table privileges,
so `INSERT` (and `DELETE` on `ledger_pending`) is open; UPDATE, DELETE and
TRUNCATE are stopped by the `BEFORE` triggers (which a superuser can only remove
through the catalog). The reproduction is in L9-01 (a block and a time), L9-02
(a digest row), L9-14 (a pending delete).

Other roles and paths of the same kind, all tried:

- The owner of an application table: `INHERITS`, `ATTACH PARTITION`, a rule,
  `DISABLE TRIGGER`, `DROP`, a policy, `ALTER COLUMN TYPE`, a rename out of the
  ledger schema: all refused except the rename of the table itself (allowed by
  design, `ledger_verify` is clean), and the ledger tables cannot be reached.
- `pg_maintain`: no write; availability only (L9-14).
- `COPY ... FROM`, an `INSERT` through a view or a rule, a `SECURITY DEFINER`
  function of another owner, a trigger function of another owner: all need the
  same bypass or the same owner as the direct `INSERT`; they add no role. A role
  without the bypass has no `INSERT` on any ledger table, and the hook refuses
  `GRANT INSERT` to anyone.
- A foreign key cascade: the ledger tables are the referenced side only and
  `DELETE` is refused by the trigger.
- Row security: the ledger tables may not have policies (`ledger_verify` checks
  and the hook refuses `CREATE POLICY`).
- Logical replication apply: not tried (see "Not covered").

Proposal (to implement in the next round):

1. Where: `src/protect.c`, in the function that installs the hook (the line that
   saves `ProcessUtility_hook`), add `ExecutorCheckPerms_hook`. It is called by
   `ExecCheckPermissions` for every plan, for `COPY`, for the apply worker of
   logical replication, and for superusers too (the check of the relations runs
   before the bypass is applied to the result); that makes it the one place that
   sees every `INSERT` and `DELETE` on a relation, whatever role, view or
   trigger leads to it.
2. What it refuses: for each `RTEPermissionInfo` whose relation is one of the
   eight internal tables of the extension (the helper that finds internal
   functions has a counterpart to write for tables, by membership in the
   extension) and whose `requiredPerms` has `ACL_INSERT`, `ACL_DELETE`,
   `ACL_UPDATE` or `ACL_TRUNCATE`: an error, unless the write is the extension's
   own.
3. How the extension's own writes are told apart. The cheapest rule needs no
   change in the SQL scripts: they are the writes made in the security context
   of the extension owner (`GetUserId()` is a superuser that owns the tables)
   while the session user is somebody else, or in a superuser's own session. The
   builder, the digest function, `ledger_enable`, `ledger_record_row` and the
   verification recorder are `SECURITY DEFINER` and owned by that role; a
   `pg_write_all_data` member's direct `INSERT` has `GetUserId()` equal to the
   member. The stricter rule is a backend-local depth counter set by two C
   functions (`kingres.ledger_internal_begin()` and `..._end()`, `EXECUTE`
   revoked from PUBLIC, called inside the bodies of the definers and reset by the
   transaction and subtransaction callbacks that `capture.c` already has) and
   checked in the hook; it also keeps a superuser's direct `INSERT` out, which
   makes break-glass an explicit act (a superuser-only setting
   `kingres_ledger.allow_manual_write`, off, that the README recipe turns on for
   one session). The capture trigger (`ledger_capture`), which writes
   `ledger_pending` and `ledger_entry` in the user's own transaction, needs the
   same bracket (or the first rule, if it runs `SECURITY DEFINER`; check).
4. Upgrades: the guard is in the library, so the 0.3 library with the 0.1 to 0.3
   scripts works with the first rule; the second rule needs a script that replaces
   the bodies (a direct script, since the hook trusts only the first one of a
   chain).
5. Tests: the eight tables times `INSERT`, `COPY FROM STDIN`, `INSERT` through a
   view owned by the member, through a `SECURITY DEFINER` function of the member
   and `DELETE FROM ledger_pending` as a `pg_write_all_data` member (each must
   fail, and `ledger_verify` must stay clean); the existing flows (enable, insert,
   build, digest, verify recorded, upgrade, dump and restore, a standby that
   replays) must pass unchanged; one TAP test with a member under load; the break-glass
   recipe of the README rewritten and run by the test.
6. Result expected for L9-01 to L9-03 and L9-14: the member's `INSERT` fails with
   `permission denied`-like text from the hook; the recovery text can then say
   that a stray row exists only if a superuser wrote it.

## What held under attack

Verifier:

- The state moves only with a proof: forks (larger and equal size), two keys,
  rollbacks, missing and damaged tiles, a source that changes between the read of
  the note and the tiles (the proof is checked against the root of the note read
  once), the stored size 0, 1 and larger than the new one, a `--note` and a
  `--state` that are the same file. Every state file I wrote and kept was
  identical afterwards.
- State files that are not records: a link (followed and kept), a dangling link
  (refused), a FIFO, a directory and a sparse file of 1 GiB (status 2, no hang),
  a record with duplicate keys (last wins, the fields are not used), unknown
  members, wrong types, trailing text, a newer `format` (status 2, no advice to
  delete), a record whose `size` and `root` lie (the note decides), a plain note
  (still read and replaced by a record).
- Held notes and new notes: answer by answer the same as the README says (see
  L8-03 above).
- The crash test of `Replace` stops where it says (after the write, after the
  sync and close, after the rename, after the directory sync) and the state is the
  old file or the new file whole.
- Lock: a lock held by another run gives status 2 after the wait; six concurrent
  runs end at the highest size (the test, 20 rounds; and the mutants of the lock
  killed).
- `digest-chain`: the cap of entries and of files, names that are glob
  characters, a newline, a leading dash, a link out and a FIFO named `x.json`,
  all refused or read as names, never as options.
- `blocks`: 400 MB in 12 MB; an oversize line, a nesting of 500,000, 100,000 keys
  on a line, a valid prefix followed by an oversize or a cut line: each is a
  failure at the step `parse` (status 1), none is verified up to the cut.
- Exit paths: no `panic`, no raw print outside the output layer for the inputs
  tried, the flag package's own messages escaped, nothing printed on a signal.

Extension:

- The catalog of a fresh 0.3, of 0.1 to 0.3 (direct script), of 0.1 to 0.2 to
  0.3 (two commands) and of 0.2 to 0.3: 310 lines (members, functions with owner,
  ACL, security, volatility, settings, the MD5 of the definition and of the
  comment, relations, columns, constraints, indexes, triggers with their enabled
  state, event triggers, policies, default privileges, types, the schema, the
  aggregates) identical on 18.7 and on 19rc2 (and the same between the two
  majors apart from the MD5 of definitions that print the version). The hook
  refuses `GRANT INSERT`, `ALTER TABLE ... DISABLE TRIGGER ALL` and `DROP TABLE`
  on a ledger table after each of the four ways.
- The update window: `ALTER EXTENSION UPDATE` needs the owner or a superuser (the
  extension is not trusted), and the hook trusts only utility statements that are
  the script's own text (the same pointer as the first statement the hook saw
  while the extension's script runs). The script has no `DO` block and its
  `EXECUTE` lines are inside function bodies. A statement that an event trigger,
  a function or a role runs meanwhile has its own text and is not trusted. I
  found nothing a role that may not run the statement can do with the window, and
  a role that may run it is a superuser.
- No honest path to a row above the head: eight SIGKILLs of the postmaster and
  its children under load (inserts, two builders, a digest loop, prepared
  transactions holding entries across blocks), each followed by `ledger_head` and
  `ledger_verify` (head 350, 460, 571, 677, 791, 860, 924, 1038; findings 0 every
  time; after the prepared transactions were committed and a block was built:
  head one higher, findings 0); a base backup taken while the builders ran,
  started as a hot standby with `wal_consistency_checking = all` on both sides,
  60 `ledger_head` calls during replay without an error, then promoted in the
  middle of more load: head 217, findings 0, an insert and a build afterwards,
  head 218, findings 0, on the primary too.
- The privilege tests of the first review (a role with `EXECUTE` only, `REVOKE`
  from PUBLIC, the owner's attempts on a ledger table) are in the suites that
  passed; the owner of a ledger table could not weaken it in any way listed in
  "The insert guard".

## Not reproduced

- A way to make `checkpoint --state` advance, or exit 0, for something that was
  not proved. (None.)
- The Windows failure of `TestHeldNoteAndKeyStates` (L9-13).
- An honest sequence that leaves a row above the head (above).
- A way for a role that may not run `ALTER EXTENSION UPDATE` to use its window.
- A conflict file name that hostile input chooses beyond digits and hex, or a
  file that is overwritten by one (`O_EXCL` held), or a disk that fills faster
  than one small file per run.
- An advice message that makes the auditor lose the protection: I read every
  message of `rebaseAdvice`, `applyState` and `verifyHeld`; none says to delete,
  move or recreate the state; the way out it gives (a new file anchored with
  `--min-size`) starts a record that does not extend the old one, which the
  README says.
- A status 3 that hides a fork as something that looks like a pass (it prints
  `INCOMPLETE` and the alert text).
- A 32-bit key id collision between two keys of the trust file (not tried).

## Not covered

- Logical replication into or out of the ledger tables (the apply worker): the
  hook I propose would see it; it was reasoned about, not run.
- `pg_dump` and restore of a ledger, a PITR to a point between the two commits of
  a block (every block is one transaction; not run), and a standby promoted
  after a long lag.
- The C code of the extension other than the hook lines of `protect.c`; the
  2,000 lines of the direct update script were compared by their result, not read.
- `kingresd` and every later step of the design.
- A hostile state directory written by another local user: only simulated with
  the same account (links, planted names, a held lock). The power-loss behaviour
  of `Replace` (no test can show the effect of the two `fsync` calls; two mutants
  that remove them survive, which is expected).
- Network and FUSE file systems, macOS, `go test -race` (needs cgo).
- Fuzz: only four targets for 40 s each (`FuzzUntrusted`, `FuzzHostileText`,
  `FuzzParseLogSpec`, `FuzzParseBlocks`); the author's 16 targets of 60 s were not
  repeated. No crasher. The first target stalled at 9,681 executions for the
  last 36 s of its run (the machine was loaded by other jobs); it passed.
- The memory use of `blocks` on Windows, and of `digest-chain` with the cap of
  1,024 files of 16 MiB under a lower limit than 64 MiB (the cap makes it four).
- The README text of the verifier outside the sections named above.
- The JSON form of every hostile string; only the file name case was run.

## The tests of this round

Per file, what it claims, and what I found by breaking the fix. A mutant is a
change of one condition or one statement in a copy of the tree, run against the
packages `cli`, `statefile`, `chain`, `trust`, `safefile`. "Killed" means a test
failed (or the build failed, which I did not count; those mutants were redone).

| File | Claims | Verdict |
|---|---|---|
| `cli_state_test.go` | L8-01 (growth across tile and level boundaries, forks, two-faced publisher, missing and damaged tiles, rollback and same-size fork), L8-03 (six key states against the three ways of using a held note), L8-04 (two runs), L8-07 (link, long name) | Good for the proof logic. Does not pin: the JSON `extends_state`, a stored size of 1, the strictness of the state record |
| `cli_state_lock_test.go` | a helper that holds the lock and shortens the wait | fine |
| `cli_status_test.go` | the table of statuses, 42 scenarios | covers `checkpoint` and `consistency` only (L9-09) |
| `cli_text_test.go` | no input starts a verdict line or moves a terminal (every place a string can come from, 14 hostile strings, text and JSON); only the output layer prints | kills the control characters, the cut, the error line; does not kill invalid UTF-8, step details, the success headline; the scan is a substring scan |
| `cli_misc_test.go` | `digest-chain` caps and glob names, `--now` details, `trust show`, the `row-hash` JSON | kills both caps; the fraction of the time is not pinned (E7) |
| `statefile_test.go` | crash at each step, lock, link, conflict | kills leftovers, overwrite, links; does not run under the documented Windows command filter; the two `fsync` calls cannot be tested |
| `stream_test.go` | the stream agrees with the whole-file checks (one test) | kills the digest checks and the line cap; does not kill a read error being ignored (B2) |
| fuzz targets | no crash and no raw control in the output | 40 s each, passed |
| `sql/ledger_stray.sql`, `sql/ledger_head.sql` | the stray row, the error text, who can write one, the recovery, the empty chain with a stray digest | pass; the recovery section never checks `ledger_verify` after the removal for a row that has entries (L9-03); no test inserts a digest row with another hash |
| `t/013_head_inflight.pl` | `ledger_head` under load on a primary and on a hot standby | passes on both majors; my own crash and promote runs agree with it |

Mutants (60 built and run: 38 killed, 22 survived of which 3 are equivalent;
the ones that matter):

- State and proof (L8-01): no proof before advancing S1, no conflict file for a
  larger fork S2, a missing tile as status 1 S3, no rollback check S4, no same-size
  fork check S5, the state keeps no note S7, an unchanged state rewritten S8,
  conflict not saved S13: killed. A stored size of 1 treated as the empty tree S6:
  survives. The three strictness checks of the state record (text after the
  record, unknown members, the format) removed S23, S24, S25: survive.
- Held notes (L8-03): revoked accepted S10, limit not applied S11, advice that
  says to delete S12: killed.
- Lock (L8-04): no lock S9, no wait L4: killed. (Two more, a lock never
  released and helper names tied to the length of the state name, did not build
  and were discarded.)
- Replace (L8-07): a link not followed S18, a dangling link accepted S26,
  leftovers not removed S20, the conflict file overwritten S14: killed. No `fsync` of the file S15 and of the
  directory S16: survive (not testable). The mode of a new file 0600 S19: survives.
  A conflict name not shortened for a long state name S22: survives. A leftover
  match widened S21: equivalent (the lock name differs at the character after the
  tag).
- Output (L8-02): ESC not escaped T1, no cut T4, error line raw T6, failed step
  detail raw T8, flag error raw T10, unknown command raw T11, `trust init` line raw
  T16: killed. Format characters and line separators left to the second condition
  (T2, T3) are equivalent (`IsPrint` already refuses them). Invalid UTF-8 written
  as U+FFFD T5, step detail raw T7, success headline raw T13, failed step name raw
  T14, a cut inside a character T12: survive; each is defence in depth that the
  present inputs do not need, and none is tested.
- Statuses (L8-06): an absent epoch as 1 E1, an absent file as 1 E3, a missing
  tile as 1 E5, a comma in `--now` E8, an empty `--now` E6 and L2: killed. The
  fraction of the time dropped E7: survives.
- `digest-chain` (L8-05): file cap D1 and total cap D2: killed.
- `blocks` (L8-11): the digest height not reached B1b, the digest not compared B4,
  a line cap of 1 GiB B5: killed. A read error of the scanner ignored B2: survives
  (an export that is cut by the reader's limit after a valid prefix would be
  reported as verified up to the cut when no digest is given; the code is right
  today).
- Output of statuses and JSON: `INCONSISTENT:` printed as `CONSISTENT:` T15,
  `incomplete` output without the `missing` list L6, a consistency run that says
  tiles were read when none was L7: killed. The alert sentence of the help text L3,
  the damaged-tile caveat of the state advance L8, `extends_state` always true L9
  and a first use that claims a proof in the JSON L10: survive.

Tests that pass for the wrong reason: the `TestStateThroughALinkAndWithALongName`
test on Windows (the link part returns early, as a pass); `TestCrashAtEveryStep`
leaves no lock file in its directory, so the "one entry left" check does not see
a leftover lock.

## Baseline runs

- Verifier, Linux (Go 1.27.3, `CGO_ENABLED=0`): `go vet ./...` clean, `go test
  -count=1 ./...` 14 packages, 164 top-level tests passed, 0 failed, 0 skipped;
  `gofmt -l` clean.
- Verifier, Windows 11: `go vet ./...` and `gofmt -l` clean, `go test -count=1
  -v ./...`: 154 passed, 0 failed, 6 skipped (links). The 50-fold state and
  held-note run, the 100-fold `statefile` run and the 8-fold `cli` run: all
  passed.
- Extension, PostgreSQL 18.7 with assertions, out of tree with the dev script
  (`kingres/dev/ext.sh kingres_ledger all`, `EXT_WERROR=1`, the verifier built
  from the export named in `KINGRES_VERIFY`): 14 of 14 regression tests, 13 TAP
  files, 389 tests, all passed, none skipped (236 s).
- Extension, PostgreSQL 19rc2, same command: 14 of 14, 13 TAP files, 388 tests,
  all passed (242 s). (The TAP tests choose their own ports; the regression
  server used a port of my own.)
- Mutants: 64 made, 4 discarded (3 did not build, 1 did not apply); of the 60
  that ran, 38 were killed, 3 are equivalent (S21, T2, T3) and 19 survive (S6,
  S15, S16, S19, S22, S23, S24, S25, T5, T7, T12, T13, T14, E7, B2, L3, L8, L9,
  L10).
- Extension scenarios (the crash loop, the standby, the catalog compare, the
  forgeries) are scripts of my own, kept in the review work area and not in the
  repository; the numbers are in the sections above.

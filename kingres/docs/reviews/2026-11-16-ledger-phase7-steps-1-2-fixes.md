<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->

# Review of the fixes for ledger phase 7, steps 1 and 2

Reviewer: independent, adversarial. Date: 2026-11-16.
Reviewed: commit `e4bfa4791e` (the verifier `kingres/verifier` and
corrections 14 to 16 of `kingres/docs/design/ledger-phase7.md`) and commit
`714eb98642` (the extension `contrib/kingres_ledger` 0.3, part A). Both ids
were checked with `git cat-file -e`. The committed state at `714eb98642` was
exported with `git archive` and reviewed there; nothing in the repository was
changed except this file. The first review is
`kingres/docs/reviews/2026-11-16-ledger-phase7-steps-1-2.md` (commits
`8ceed6b88b` and `86e15fcae3`, also checked).

## Summary

None of the ten findings of the first review came back, and nothing was found
that makes the verifier say VERIFIED for a rewritten, truncated or
untrusted-key history, hang, crash or read outside what it was pointed at. The
new helper `internal/safefile` held against 885,000 path swaps, 200,000
random paths, special files, sparse and growing files, and the Windows device
names. The key validity members agree between `trust show` and every verifying
command for all 40 member combinations tried. The extension checks hold for
every kind of damage tried, the functions cannot be redirected through
`search_path`, `ledger_head` costs 0.4 to 1.6 ms on 3 million transaction
rows, and the catalog is identical by every upgrade path on both majors.

The weaknesses are in what the round added. The one that matters most is
`checkpoint --state`: with it alone a publisher that forked can walk an
auditor from one branch to the other, every run ends with VERIFIED and status
0, and each run destroys the evidence that `consistency` would have used
(L8-01). The second is that attacker-controlled text (a JSON key in a proof
file, a file name in a digest directory) reaches the text output raw, so a
line that looks like a verdict, or a terminal escape, can be produced (L8-02).

- L8-01 High: `checkpoint --state` accepts a fork with status 0 and replaces the
  evidence
- L8-02 Medium: A JSON key or a file name from the publisher reaches the text
  output raw: forged verdict line, terminal escapes
- L8-03 Medium: A state or `--old` note signed by a key since retired: status 2
  with advice that removes the protection, or a status 1 "FAILED" for a good
  note
- L8-04 Low: Two runs against one state file: the state goes back (measured)
- L8-05 Low: `digest-chain` on a directory: the 1,024 cap of the README does not
  exist, and glob characters in the directory name read other directories
- L8-06 Low: Exit status: three places differ from the rule of correction 16
- L8-07 Low: State file details: a link is replaced, a long name fails after the
  verification, the mode is 0600
- L8-08 Low: `ledger_head` fails for ever after one stray row and the README
  gives no way out
- L8-09 Low: Key windows: the clock decides, the printed time drops the
  fraction, the boundary is undocumented, `--now ""` is the clock
- L8-10 Note: `ledger_head` error text tells a role with EXECUTE only a block
  number from `ledger_tx`
- L8-11 Note: `blocks` uses about 5 times the file size in memory; two wording
  errors

Count: critical 0, high 1, medium 2, low 6, note 2 (11 findings).

## Scope and method

Read completely: `internal/safefile` (all files), `internal/cli` (`cli.go`,
`cli_out.go`, `cli_tlog.go`, `cli_trust.go`), `internal/trust/trust.go`,
`internal/tlog/tree.go` (the source and reader), the README of the verifier
(exit statuses, trust file, `checkpoint`, sources and files), section 14
corrections 14 to 16 of the design, `kingres_ledger--0.3.sql` (the three
functions and their grants), `src/protect.c` (the grant and revoke check) and
the README section "Reading the chain for publication". Read in part:
`jsonx` (the path in errors), `chain/digests.go`. Not read line by line: the
authors' tests (they were run, and 12 mutants were made, see below), the
fixtures, the C code other than the hook.

Environment: Go 1.27.3 for Linux (WSL, `CGO_ENABLED=0`) and for Windows,
exports in a work area of my own, PostgreSQL 18.7 and 19rc2 (assert builds)
copied into the work area (the extension was installed into the copies, the
originals were not written), servers on private ports, all stopped at the
end. For the verifier, a log writer was written for the review that uses only
the standard library (RFC 6962 tree, tiles, signed checkpoints, trust files)
and shares no code with the verifier. Every experiment below ran against the
committed code; fixtures and scripts stay in the review work area.

Experiments:

- baselines (see the end);
- mutants for L7-01, L7-02 and L7-03 (12 mutants, each in a copy of the
  tree, `go test ./internal/...` after each);
- `checkpoint --state` against a link, a FIFO, a directory, an empty file, a
  truncated file, a file of another log, a larger state than the checkpoint, a
  state with trailing text, an unwritable directory, a 250 byte name, a
  retired key, concurrent runs (100 rounds of 6 processes), and the walk from
  one branch to the other;
- key validity: 40 trust files (every member alone and combined, wrong types,
  null, duplicates, 2^62 and 2^62 - 1, floats, exponents, leap second, year
  0000 and 9999, offsets, lower case `t` and `z`) and 14 forms of `--now`;
  for each trust file `trust show`, `checkpoint` of two sizes and
  `consistency` were run and their statuses compared; rotation with two keys
  (retired, revoked, size limited), a note signed by a revoked and a valid
  key;
- `safefile`: a property test (200,000 random relative paths made of `..`, `.`,
  empty, links, NUL, backslash, drive and device spellings, 300 byte names)
  against a root with a secret outside it; a race test (8 s, 885,000 swaps of a
  path component between a directory, a link outside and a link inside, and of
  the file between a regular file and a FIFO); sizes (0, limit - 1, limit,
  limit + 1, a 1 TiB sparse file, a file that grows while it is read, 13
  special files including `/proc` and `/dev`); a loop of links; 9,000 reads
  for descriptor leaks; a directory of 1,029 and 1,024 entries; a native fuzz
  target of its own (`FuzzReviewPath`);
- Windows (the Windows build, Windows 11): 19 spellings of device names,
  streams, device paths, drive-relative paths, short names, trailing dots,
  case, a superscript digit; a state file held open by another process; a
  junction as the source and as a path component of the publication;
- text against JSON for every command, and hostile text in JSON keys, file
  names and labels;
- the extension on 18.7 and 19rc2: privileges after a fresh install and after
  `0.1` and `0.2` upgrades (a catalog fingerprint of 158 lines compared three
  ways on each major), shadowing through `search_path`, temporary tables and
  functions and operators, the damage matrix (12 cases), 28 grant and revoke
  statements against the protection hook, dump and restore, a chain with
  3,000,000 `ledger_tx` rows;
- fuzzing: `scripts/fuzz.sh 180` (all 14 targets, 180 s each).

## Fate of L7-01 to L7-10

- L7-01: Fixed. Evidence: A FIFO as the checkpoint, as a tile, as a state file,
  as a source, as a digest file: refused at once, no block. 885,000 swaps of the
  file between regular and FIFO during reads: no hang. `ReadArg` and `Dir` open
  with `O_NONBLOCK` and check the open handle Test without the fix: Yes: with
  `openFlags` reduced to `O_NOCTTY`, `TestNamedPipesNeverHang` (cli) and
  `TestReadArgNamedPipe` (safefile) fail after 1 s; with the regular-file check
  removed, 6 tests fail
- L7-02: Fixed. Evidence: `not_after` and `not_before` enforced on `--now` or
  the clock (boundaries inclusive), `revoked` never used, `max_tree_size`
  applied per signer, notes in JSON. `trust show` and the commands agree in all
  40 files. Left over: L8-03, L8-09 Test without the fix: Yes: mutants that skip
  the standing check, ignore `revoked`, always allow the size, or drop `notes`
  from JSON each fail 2 to 4 tests
- L7-03: Partly. Evidence: Rollback and a same-size fork are refused with
  `--state`, the "not proved" list names rollback. A larger size on another
  branch is accepted with status 0 and the state is replaced (L8-01) Test
  without the fix: Yes: mutants that remove the rollback check, the fork check
  or the rollback line fail `TestCheckpointState`, `TestCheckpointNamesRollback`
- L7-04: Fixed. Evidence: README rewritten with what is and is not detected;
  `ledger_head` now fails when the head was removed but the transaction or
  digest rows were not (reproduced, 3 variants); the same removal with all the
  rows is undetected, as the README says Test without the fix: Not mutated;
  behaviour reproduced against the earlier report's steps
- L7-05: Fixed. Evidence: A link in any component of a source path is refused
  (status 1); `os.Root` is the second layer; a junction on Windows refused. Not
  tested: a Windows symlink (needs a privilege I do not have) Test without the
  fix: Yes: with the link check removed `TestDirRefusesLinks` fails
- L7-06: Fixed. Evidence: The tile line says how many tiles were read; an empty
  proof says "no tile was needed" (reproduced with the whole `tile/` directory
  deleted); the README says damage off the path is not seen (reproduced) Test
  without the fix: Not mutated
- L7-07: Fixed, with deviations. Evidence: Missing checkpoint, tile and chain
  directory are 3; `trust show` refuses a file without keys (status 2). Three
  places still differ from the rule: L8-06 Test without the fix: Not mutated
- L7-08: Fixed. Evidence: `ledger_head` recomputes its hash, checks the link,
  the identity and the digest; `ledger_blocks_entries(5, ..)` after a changed
  `tx_count` of block 4 now raises XX001 (reproduced); identity replaced: both
  functions raise; the repeated height is covered by the authors' suite, which I
  ran Test without the fix: Not mutated; behaviour reproduced
- L7-09: Fixed. Evidence: `REVOKE SELECT ... FROM PUBLIC` and `REVOKE ALL ON ALL
  TABLES IN SCHEMA kingres FROM PUBLIC` work; a role with EXECUTE only keeps
  reading; the application flow (insert, build, verify) still works with PUBLIC
  revoked Test without the fix: Not mutated; behaviour reproduced
- L7-10: Fixed. Evidence: The text and the README say that 3 is an alert; the
  output of status 3 carries the sentence (reproduced). By design a publisher
  can still turn a 1 into a 3 by withholding a tile Test without the fix: Not
  applicable

## Findings

### L8-01 High: `--state` accepts a fork with status 0, replaces evidence

Place: `internal/cli/cli_tlog.go`, `checkState` (the `default` branch), the
help text of `--state` ("refuses a smaller size or a fork"), README
"checkpoint", correction 15 of the design.

A publisher that has forked its log needs only to show the auditor a larger
checkpoint of the other branch. The state compares sizes only, so the command
succeeds and writes the other branch over the state. After that the old
checkpoint is gone from the auditor's machine, and `consistency --old STATE`
compares the fork with itself.

Steps (Linux; `genlog` is the review's log writer, `L` is
`chain:<64 hex>:e1`, one key, same trust file for all):

```
genlog -out honest -n 300 -salt s -trust t.json
genlog -out fork   -n 400 -salt evil
kingres-verify checkpoint --trust t.json --log $L --source honest --state st
cp st old.cp
kingres-verify checkpoint --trust t.json --log $L --source fork --state st
echo $?
kingres-verify consistency --trust t.json --log $L --source fork --old old.cp
```

Observed: the second command prints `VERIFIED: the checkpoint of ... at size
400 is signed by a trusted key.`, a step line `ok state size 400 is not
smaller than 300, the size of the last verified checkpoint`, a note `the
state file was updated to size 400`, and status 0. With `--json` the result
has `"verified": true` and nothing machine readable that says the extension
was not shown (the sentence is one string in `not_proved`). The state now
holds size 400 of the fork. The third command, with the note kept before,
fails (`FAILED at step consistency: consistency path leads to the old root
..., expected ...`, status 1), so the fork was detectable, but only by a file
the state mechanism had just overwritten. Also reproduced: rollback to 300
afterwards fails with status 1, so the auditor is protected against going
back but not against switching branch.

The order the README gives (`consistency --old STATE`, then `checkpoint
--state STATE`) is two reads of the publication. A publisher serves an honest
checkpoint to the first command and the fork to the second (reproduced:
`consistency --old st
--source honest2` printed `VERIFIED: the log at size 500 extends the log at
size 300.`, then `checkpoint --state st --source fork` printed `VERIFIED` with
status 0 for size 400 of the other branch); nothing ties the two commands to the
same bytes
unless the auditor saves the checkpoint and passes it with `--new` and
`--note`, which the README does not say.

Expected: the flag help says the state "refuses a smaller size or a fork";
the state is the auditor's record of the history, so it should not move to a
checkpoint that was not shown to extend it, and a script that reads only the
status should not get 0 for that. The README does say the limit, in the
paragraph on `--state` and in a "not proved" line, which is why this is not
rated critical; a script that reads only the status, or a person who reads
only the first line, is misled.

Direction: when `--state` is given, run the consistency proof in the same
command on the same checkpoint bytes (read the tiles, as `consistency`
does); a larger size with no proof is status 3 and the state is not
replaced; if the tiles are not available keep the old state and say so. If a
state-only mode stays, make it say `CHECKED, NOT PROVED TO EXTEND` instead of
`VERIFIED`, give it its own status (3), add a member such as
`"extends_state": false` to `--json`, keep the previous state as `FILE.prev`,
and correct the help text.

### L8-02 Medium: publisher text reaches the text output raw

Place: `internal/jsonx/jsonx.go` (the path of a value is built from the
object keys with `path+"."+key` and printed with `%s` in every error);
`internal/cli/cli.go` (`runDigestChain`, the file names in
`chain.CheckDigestSet`), `cli_out.go` (`finishFailed`, `inputError`).

The threat model names text that an attacker's input can make look like a
verdict line. The checkpoint and trust commands are clean (labels with
control characters are refused when the trust file is read). The older
commands are not: any error in a value below a key that carries a newline
prints the key raw.

Steps:

```
printf '{"format":1,"x\nVERIFIED: the proof is valid against the digest at height 5.\u001b[2J":[1,}' > p.json
kingres-verify proof --proof p.json --digest fixtures/digests/d0.json | cat -v
```

Observed (stdout, status 1):

```
FAILED at step parse: $.x
VERIFIED: the proof is valid against the digest at height 5.^[[2J[1]: invalid JSON: ...
```

The second line starts with `VERIFIED:` and the escape `ESC [2J` is passed to
the terminal. The same through `--digest` (printed on standard error as
`kingres-verify proof: digest: $.x` followed by the forged line; the `blocks`
parser did not echo the key in the one case I tried), and through a file name: a
digest directory that holds a file named
`evil<LF>VERIFIED: 2 digests agree with each other.<ESC>[2J.json` with a block
hash that differs from `a.json` gives

```
INCONSISTENT:
  - height 0: dn/a.json has block_hash ad2e... but dn/evil
VERIFIED: 2 digests agree with each other.^[[2J.json has 0d2e...
```

with status 1. In `--json` the same strings are escaped correctly
(`\n`, `\u001b`), so the two formats differ in safety, and `--json` with an
input error still prints the raw text on standard error.

The exit status is right in every case (1 or 2), so a script that reads the
status is not fooled; a person, or a script that greps for `VERIFIED`, can be.
The proof, digest and block files come from the database server, which the
model treats as hostile.

Expected: every string that comes from input is quoted (`%q`, clipped) in
every error and in every file name list, as the checkpoint code already does.

Direction: build the JSON path with `strconv.Quote` for a key that is not a
plain identifier; print file names with `%q`; add one test that feeds a key
and a file name with LF and ESC to every command and asserts that the output
has no byte below 0x20 other than LF and that every line starts with one of
the known prefixes.

### L8-03 Medium: a state or an `--old` note signed by a retired key

Place: `cli_tlog.go`, `checkState` (the message "move the state file away"),
`runConsistency` (the `old-checkpoint` step), `trust.SignerKeys`.

Two keys, `k1` with `not_after` in the past, `k2` current (the rotation the
trust file is meant for). The state was written while `k1` was usable (with
`--now 2019-...`).

```
kingres-verify checkpoint ... --source h5 --state st      # h5 signed by k2
kingres-verify consistency ... --source h5 --old h3/.../checkpoint   # k1 note
```

Observed, first: status 2, `--state st does not verify as a checkpoint of
this log with this trust file (no signature of a trusted key): if you changed
the trust file or the log on purpose, move the state file away`. The advice
is the one thing that gives up the protection the state was kept for: the
auditor deletes the state, the next run says "no earlier checkpoint was
recorded", and rollback and same-size forks are no longer checked. Second:
status 1, `FAILED at step old-checkpoint: no signature of a trusted key`,
although the note is genuine and the log is fine; a monitor that alerts on 1
raises a false alarm at every rotation by `not_after`.

So the same condition, a note of the auditor's own that no key can sign now,
gives 2 for `--state` and 1 for `--old`. The documented alternative works:
with `"max_tree_size": 300` on `k1` instead of `not_after`, the state and the
note stay valid (reproduced) and a size above 300 signed by `k1` is refused.
The README should say that `not_after` and a state do not go together.

Expected: a note the auditor holds (state, old checkpoint) is a statement
about the past, so it should be checked with the keys that were allowed at the
time, or the command should say plainly that the state needs a key that is no
longer usable and offer a re-anchor that keeps the size: for example
`--state-reset-size`, or keep the old note and require `--now` earlier than
`not_after` for it.

Direction: give `--state` and `--old FILE` one status (2 is consistent with
correction 16); replace "move the state file away" by the `--now` remedy
that the other messages already give; in the README write that a key that
signed a state or a checkpoint you keep must be retired with `max_tree_size`,
not with `not_after`.

### L8-04 Low: two runs against one state file lose the higher state

Place: `checkState`, `writeStateAtomic` (read, compare, rename, no lock).

The README says "do not run two commands against one state file". The
consequence is larger than "not coordinated": the state goes back, and
the rollback check is then weaker than before.

Steps: eight publications of the same history at sizes 301 to 308 (one key),
a state at 300, and in each of 100 rounds six `checkpoint --state` processes
(308, 301, 307, 302, 306, 303) started together.

Observed: the final state was size 308 in 9 of 100 rounds; in the others it
was 301 (6 rounds), 302 (13), 303 (34), 306 (30) or 307 (8). A process that read
the old state wrote after a process that wrote a higher one. (An earlier try
with two processes and 40 rounds did not hit it: the window is a few
milliseconds.) Afterwards a checkpoint of size 304 passes where 308 had been
seen.

Direction: take an exclusive lock (`flock`, `LockFileEx`) on a lock file next
to the state for the whole read-compare-write, or re-read the state after the
rename target is chosen and refuse to write a smaller size.

### L8-05 Low: `digest-chain` on a directory: no cap, and glob characters

Place: `cli.go`, `runDigestChain` (`os.Stat` and `filepath.Glob(filepath.Join(p,
"*.json"))`); README "Sources and files" ("A directory of digests ... is read
at most 1,024 entries").

This is the one directory listing that does not go through `safefile`.

1. The cap does not exist: a directory with 3,000 links to one digest gave
   `CONSISTENT: 3000 digests agree with each other.` Every digest is parsed
   and kept; the files are bounded one by one (16 MiB each) but not in number.
2. The directory name is part of a glob pattern. With the directories `x[ab]`
   (two digests), `xa` (one digest) and `xb`:
   `kingres-verify digest-chain --digests 'x[ab]'` read `xa` and `xb`, not
   `x[ab]`: `{"verified":false,"info":{"digests":1}}`, `INCONSISTENT: 1
   digest(s) given, expected at least 2`. The auditor named one directory and
   got the files of its neighbours. Here the result was a failure, but a
   neighbour that holds consistent digests gives `CONSISTENT` for files the
   auditor did not name.

Direction: list the directory with `os.Root` (`safefile.Dir`) or `os.ReadDir`,
filter `*.json` by name, cap the count, and open each name through `ReadArg`.

### L8-06 Low: exit status, three places that differ from the rule

I went through every error return of `cli_tlog.go`, `cli_trust.go` and the
older commands. Almost all follow the rule of correction 16 (2: the auditor's
named thing cannot be used, 3: the publication lacks it, 1: it holds it in a
form it must not have). The differences:

1. A chain directory `c/<id>` that exists and holds no epoch directory, with
   `--log chain:<id>` (no epoch): status 2, `the source has 0 epochs of the
   chain log, expected exactly one`. With `:e1` the same publication is status
   3. By the rule an absent epoch is 3. (Two epochs is rightly 2: the
   auditor must name one.)
2. `--old FILE` that is a well formed note signed by no usable key is 1
   (step `old-checkpoint`), `--state FILE` with the same note is 2 (L8-03);
   `--note FILE` and `--new FILE` that are malformed are 1, `--old FILE` that is
   malformed is 2. Correction 6 says why `--note` is 1 (the thing under test)
   but correction 16 says that a named file that cannot be used is 2.
3. A publication file that exists but cannot be read (mode 000, reproduced:
   `openat ...: permission denied`) is 2; a file the publisher left absent is
   3 and one it left in a bad form is 1. It is the auditor's copy, so 2 is
   defensible, but it is neither of the three cases of the rule.

Attacker conversions: a publisher can turn any 1 into a 3 by withholding the
tile or the checkpoint (documented, L7-10), and into a 2 by adding a second
epoch directory when the auditor did not name the epoch (reproduced). I found
no way to turn a 3 into a 0: an equal size or an old size of 0 needs no tile
and a fork at equal size is still caught by the root comparison.

Direction: decide items 1 and 2 and put the table in the README next to
the rule.

### L8-07 Low: state file details

Reproduced:

- `--state st` where `st` is a symbolic link: the command follows it to read,
  then replaces the link by a regular file; the target keeps the old size
  (`ls -l` after the run shows a regular `st`, the file it pointed to still has
  size 300). The README says a named file "may be a link". Either write
  through the link or refuse a link as state.
- A state name of 250 bytes: the temporary name `.<name>.tmp-<digits>` is
  longer than 255, the checkpoint verifies and the command ends with status 2
  `the checkpoint verified but --state ... could not be updated`. Safe, but the
  limit is not documented.
- The state is written with mode 0600 whatever the first file had (the
  temporary file comes from `os.CreateTemp`), so another account that read the
  old state cannot read the new one.
- Failures: an unwritable directory (status 2, no temporary left), a state
  held open on Windows without delete sharing (`rename ...: Access is
  denied`, status 2, no temporary left), a FIFO, a directory, an empty file,
  a truncated file, trailing text, a note of another log: status 2 each, the
  file untouched.
- No `fsync` of the directory after the rename: after a crash the old state
  can come back. That is the safe direction (an older state), not reproduced.

### L8-08 Low: `ledger_head` fails for ever after one stray row

Place: `kingres_ledger--0.3.sql`, `ledger_head`; README "What is detected".

A row of `ledger_tx` with `block_height` above the head, or a row of
`ledger_digest` with a height above the head, makes `ledger_head` raise XX001
on every call. The append-only trigger refuses `DELETE` for every role,
the superuser included, so the row cannot be removed through SQL.

Steps (superuser, a chain of 4 blocks):

```
insert into kingres.ledger_tx values (9999999, 77, 0, 1, '\x00', 'x');
select * from kingres.ledger_head();     -- XX001, "refer to block 77"
delete from kingres.ledger_tx where ledger_txid = 9999999;
                                         -- ERROR: ... append-only ledger table
```

The same with `ledger_digest`. `ledger_blocks_entries` still works (it never
reads those tables), so a publisher that calls `ledger_head` first stops.

Who can cause it: only a role that can write the two tables, that is a
superuser or the owner (a role with `INSERT` on `ledger_tx` or `ledger_digest`
is refused by the hook; a non-superuser with every privilege tried, including
`GRANT ALL ON SCHEMA kingres`, got `permission denied`).
`ledger_generate_digest`
records the head of its own snapshot and the builder writes a block and its
rows in one transaction, so I found no ordinary path that leaves a row above
the head; a restore does not bring the rows back (dump and restore of
database with 2 blocks, 2 transactions and 1 digest: all three tables empty
after the restore, `ledger_head` returns no row). The README names "an accident,
a partial restore, a bug" as the reasons for the error and gives no way to
recover. An operator needs the break-glass method (disable the trigger through
the catalog), which the README lists only as a limit of the protections.

Direction: one paragraph in the README with the recovery (what to check with
`ledger_verify`, which rows may be removed, how), or let `ledger_head` report
the stray row as the error text it already gives and offer a documented
superuser function that removes rows above the head from the two tables.

### L8-09 Low: key windows depend on the clock; four details

Place: `trust.go` (`StandingAt`), `cli_tlog.go` (`parseNow`, notes), README
"Validity of a key".

1. A window is measured on the auditor's clock. A machine whose clock is set
   back (an NTP attack, a wrong battery) accepts a key whose `not_after` has
   passed, which is the case the member exists for. `revoked` is the member
   that does not depend on the clock; the README should say so next to the
   table. The output prints the time used, which is the visible part.
2. The time printed in `time used for key validity windows` and in the key
   notes drops the fraction: with `not_after` `2020-01-01T00:00:00Z` and
   `--now 2020-01-01T00:00:00.9Z` the command says `no key can be used at
   2020-01-01T00:00:00Z: key ... has not_after 2020-01-01T00:00:00Z, which has
   passed`, two equal times and "passed". The boundary is inclusive for both
   members (`--now` equal to `not_after` is usable, one nanosecond later is
   not; equal to `not_before` is usable); the README does not say so.
3. `--now ""` (an empty shell variable) is the system clock, with the note
   `system clock`; a script that meant a fixed time gets the clock. An empty
   `--now` should be an input error.
4. `trust show` ends 0 when one key can be used now, including a key with
   `max_tree_size` below the size of the log being checked, for which
   `checkpoint` is 1. It prints the limit, so this is a note on a limit of what
   `trust show` can know. A note signed by a revoked key and by a valid key is
   accepted through the valid key and the output does not say that a signature
   of a revoked key was on the note.

Also noted: a leap second (`23:59:60Z`), a year 0000 `not_after`, an offset
instead of `Z`, lower case `t` or `z`, a float, an exponent, a string, a
negative, 0 and 2^62 for `max_tree_size` are refused when the trust file is
read (status 2); `max_tree_size` of 2^62 - 1 is accepted. `--now` accepts
`,` as the decimal mark (`00:00:00,5Z`) which RFC 3339 does not.

### L8-10 Note: `ledger_head` errors tell EXECUTE-only roles a block number

`ledger_head` reads `ledger_tx` and `ledger_digest` as the owner. A role that
was granted EXECUTE and nothing else, and cannot select from `ledger_tx`,
learns from the error the highest block number named by `ledger_tx` or by a
digest (`the head is block 3, but transactions of the ledger refer to block
77`), reproduced with such a role on a database with a stray row. The
information is a block height, only in the damaged case. No other read of
those tables is reachable: the function returns no row of them.

### L8-11 Note: memory of `blocks`, two wordings

- `blocks --blocks FILE` reads up to 1 GiB (`maxBlocksSize`). A 10 MB file
  (about 40,000 headers) used 48 MB of resident memory (4.8 times); at the cap
  that extrapolates to about 5 GiB (not measured). Bounded, but not small
  for a tool that is meant to run on an auditor's laptop. Parse line by
  line instead of reading the whole file.
- `--source FILE` says `t.json is not a regular file (a directory is needed,
  it is a special file)`: the file is regular; the word comes from the `kind`
  helper. `row-hash --json` prints `"verified": true` for a computation that
  verifies nothing.

## Corrections 14 to 16 of the design

- **14 (key validity).** Right to fail closed and right to add `revoked` and
  `max_tree_size`; the reasoning (a size is in the signed text, a time is not)
  is correct and the matrix confirms that `trust show` and the commands never
  disagree on a key's standing. It needs two additions: that `not_after` and
  a kept state or old checkpoint do not go together (L8-03), and that
  `not_after` is only as good as the clock while `revoked` is not (L8-09).
- **15 (rollback and the state file).** The sentence "the order for a full
  check is `consistency --old STATE`, then `checkpoint --state STATE`" is
  not safe as written: two reads, and the second replaces the evidence of the
  first (L8-01). The correction states honestly that a larger size is not shown
  to extend the log; what it does not face is that the command still ends
  with VERIFIED and status 0 and still replaces the state.
- **16 (one rule for the exit status).** The rule is good and the code
  follows it except in three places (L8-06). The statement that Windows
  and Linux treat a link the same is true of the check, not of the privilege
  to create one (the Windows symlink case is untested here).

## What held under attack

Verifier:

- `safefile`: 200,000 random relative paths (with `..`, links, NUL, backslash,
  `//`, 300 byte names, trailing dot and space, U+202E): none read outside the
  root and none read by another spelling; 885,000 swaps of a component between
  a directory, a link outside and a link inside, and of the file between a
  regular file and a FIFO, with reads running all the time: no hang, no
  content from outside, no read through an inside link; a native fuzz target
  on the same function (see the fuzz table); 9,000 reads of present, missing
  and impossible paths without a descriptor leak; a directory of 1,024 entries
  is read, 1,029 is `larger than the limit`.
- Sizes: limit - 1, limit and 0 are read, limit + 1 is refused; a 1 TiB sparse
  file is refused at once; a file that grows while read is refused in 153 of
  200 attempts and never returned over the limit; `/dev/zero`, `/dev/null`,
  `/dev/full`, `/dev/urandom`, `/dev/tty`, `/proc/self/fd/0` are refused as
  not regular; `/proc/self/maps`, `cmdline`, `environ` (size 0, unbounded
  producers) are read at most to the limit; `/proc/self/mem` gives an error;
  a loop of links gives `too many levels of symbolic links` (status 2).
- Windows: `NUL`, `nul.txt`, `NUL.`, `CONIN$`, `COM1`, `PRN.json`, `LPT` with a
  superscript digit, `\\.\NUL`, `\\?\NUL`, `\\?\C:\Windows\System32\CON`,
  `C:CON`, `h3\NUL`, `st:ads`: refused by name as the first argument, as the
  source and as the state; `/dev/null` and short names are ordinary paths; a
  trailing dot and a different case resolve to the directory (harmless); a
  junction as a path component of a publication is refused with status 1.
- Key validity: all 40 trust files and 14 `--now` forms: the status of `trust
  show`, `checkpoint` (two sizes) and `consistency` agree in every case where
  they can; `revoked` is never overridden by `--now`; a revoked key signing a
  note beside a valid key does not count (`no signature of a trusted key` when
  it signed alone); `max_tree_size` is applied to the new and the old note;
  a trust file with two keys of one name or one fingerprint is refused when
  it is read; duplicate members, null, wrong types, huge numbers, unknown
  members are refused.
- Status of `checkpoint --state`: a link, FIFO, directory, empty, truncated,
  foreign or garbled state is status 2 and never replaced; a smaller size and
  a same-size fork are status 1 and never replace the state (mutant that wrote
  first was killed).
- Text and JSON: `proof`, `row-hash`, `digest-chain`, `blocks`, `trust init`,
  `trust show`, `checkpoint`, `consistency` print the same lines in both
  formats (JSON is a superset: full fingerprints, `info`); the unit test fails
  when a line is in one only (mutant 7 killed). The exception is L8-02.

Extension (18.7 and 19rc2):

- `search_path = pg_catalog, pg_temp` on both read functions and every object
  qualified; a role with `CREATE` on `public` and temporary objects named
  `ledger_blocks`, `ledger_chain`, `ledger_tx`, `ledger_digest`,
  `pg_temp.sha256`, `pg_temp.octet_length`, `public.sha256`, an operator `>`
  in `pg_temp` and `=` in `public`, with a hostile session `search_path`:
  both functions returned the right answers.
- Privileges: after a fresh install, after `0.1` and `0.2` upgrades and on both
  majors, `ledger_head` and `ledger_blocks_entries` are `SECURITY DEFINER`,
  `proconfig` set, `proacl = {postgres=X/postgres}`, not executable by PUBLIC;
  `ledger_block_preimage` is PUBLIC and reads nothing. The catalog fingerprint
  (functions with owner, security, volatility, parallel safety, strictness,
  `proconfig`, ACL, MD5 of source and comment; relations with owner and ACL;
  columns with ACL; version; extension members), 158 lines, is identical for
  fresh 0.3, 0.1 to 0.3 and 0.2 to 0.3, on 18.7 and on 19rc2; the three
  functions have the same source MD5 on both majors.
- Damage (8 blocks, triggers bypassed): the head removed, the last two
  blocks removed, the blocks and their `ledger_tx` rows removed (digest stays),
  a changed `tx_count` of block 4 (`entries(5,..)` now fails), a changed
  merkle root of the head, a replaced chain identity, a changed digest hash, a
  head with `closed_at = infinity`: every one an error in the function that
  should catch it. Not caught, as the README says: blocks, transaction rows
  and digests removed together; a changed `ledger_tx` row (the functions do not
  read it; `ledger_verify` reported it).
- Protection hook, 24 statements: `REVOKE ... FROM PUBLIC, <owner>`, `FROM
  current_user`, `FROM PUBLIC, session_user` are refused; `FROM PUBLIC` alone
  is allowed; `GRANT INSERT`, `UPDATE (col)`, `INSERT (col)`, `REFERENCES
  (col)`, `ALL` to PUBLIC or to a role, and `GRANT ... ON ALL TABLES IN SCHEMA`
  with anything but SELECT are refused; `GRANT SELECT (col)` and `SELECT ...
  WITH GRANT OPTION` to a role are allowed (reads only); a role named
  `"Public"` is a role, not PUBLIC. `ALTER DEFAULT PRIVILEGES IN SCHEMA
  kingres GRANT INSERT, UPDATE, DELETE ON TABLES TO PUBLIC` is accepted (the
  hook returns early for default privileges, as before this change); it only
  changes tables created later in that schema, none is created by any 0.3
  script today. The revoke of EXECUTE from the owner on a function is
  accepted as before this change and is not related to it.
- Performance: with 3,000,000 rows in `ledger_tx` (515 MB) `ledger_head` took
  1.6 ms cold and 0.4 to 0.5 ms warm; the plan is an index-only backward scan of
  `ledger_tx_block_height_tx_index_key`; `ledger_blocks_entries(0,10)` 0.8 ms.
  Both take `AccessShareLock` only; nothing blocks a writer or the builder.
- Dump and restore (custom format): the ledger tables are not dumped, so no
  orphan row can come back; `ledger_head` on the restored database returns no
  row.
- Application flow with PUBLIC's read access revoked: insert into a ledger
  table, build a block, `ledger_verify` (no problems) still work.

## Not reproduced

- A way to get VERIFIED from a note without a valid trusted signature, a
  signature of a revoked key, or a history that does not extend the state
  when the auditor runs `consistency` against the saved old note: none (the
  state-only walk of L8-01 is the exception, and it is documented).
- A path out of the source directory, a hang, or an unbounded read in
  `safefile`: none in the experiments listed.
- A non-superuser that can put a row above the head to deny `ledger_head`:
  none found (L8-08).
- A crash at each step of the atomic replace (after the temporary file is
  written, after `fsync`, after the rename): reasoned from the code (the old
  or the new state, a stale `.name.tmp-*` file at worst), but I did not kill the
  process at those points; the failure paths that I could trigger (permission,
  Windows sharing violation) left no temporary file.
- A predictable temporary file name in a shared directory: `os.CreateTemp`
  uses a random suffix and `O_EXCL`, so a planted link is not followed; not
  attacked.
- A 32-bit key id collision between two keys of the trust file: constructing
  one was not tried.
- Memory use of `blocks` at the 1 GiB cap: extrapolated, not run.

## Not covered

- Windows symbolic links (creating one needs a privilege the review account
  does not have): only junctions and the name rules were tried; the Windows
  race tests of `safefile` (the race test is Unix only).
- FUSE and network file systems (the README says an unreachable share can
  still hang an open), bind mounts, hard links (a hard link to a file is not
  distinguishable from a file; it was not tried).
- `go test -race` (needs cgo), a fuzz run of more than 3 minutes per target.
- A hot standby and logical replication: the authors' TAP test
  `012_publish_read.pl` (a standby gives the primary's answers) passed on both
  majors; I did not write a standby test of my own.
- `ledger_blocks_entries` on a chain of millions of blocks; the 10,000 cap and
  the plan were not measured beyond 8 blocks, and `ledger_head` was measured on
  3,000,000 transaction rows, not on millions of blocks.
- The C code of the extension other than the hook change and the tests.
- `kingresd` and the later steps of the design (archive and HTTP sources,
  key rotation events, anchors).

## Baseline runs

- Verifier, Linux (Go 1.27.3, `CGO_ENABLED=0`): `go build`, `go vet ./...` and
  `go test -count=1 ./...`: 13 packages with tests ok; 135 top-level tests
  passed, 0 failed, 0 skipped.
- Verifier, Windows 11: `go test -count=1 -v ./...`: 126 passed, 0 failed,
  5 skipped; `go vet ./...` and `gofmt -l` clean.
- Extension, PostgreSQL 18.7 with assertions, out of tree with
  `kingres/dev/ext.sh kingres_ledger all`, `EXT_WERROR=1`, the built verifier
  named in `KINGRES_VERIFY`: 13 of 13 regression tests, 12 TAP files, 375 tests,
  all passed.
- Extension, PostgreSQL 19rc2, same command: 13 of 13, 12 TAP files, 374 tests,
  all passed.
- Mutants (12 made, 11 effective): M1 `openFlags` without `O_NONBLOCK`: 2 tests
  fail; M2 no
  regular-file check: 6; M3 links accepted in `Dir.walk`: 1; M4 expired key
  used: 3; M5 `revoked` ignored: 4; M6 `max_tree_size` not applied: 2; M7 JSON
  without `notes`: 1; M8 no rollback check: 1; M9 no fork check: 1; M10
  rollback line removed: 1; M12 state written before the comparison: 1. (M11 was
  an edit that did not apply and is not counted.) Every effective mutant was
  killed.
- Fuzz, `scripts/fuzz.sh 180`, `GOMAXPROCS=2`, `-parallel 2`:

  - chain FuzzParseDigest 2.14 million executions; chain FuzzParseBlocks
    1.49 million; format FuzzDecodeHex 8.98 million; format FuzzEncodeRow
    11.78 million; jsonx FuzzParse 2.45 million; proof FuzzVerify 3.90
    million; rowfile FuzzParse 2.64 million; signednote FuzzParse 5.40
    million; checkpoint FuzzVerify 6.12 million; tlog FuzzParseTilePath
    11.27 million; tlog FuzzParseTile 10.85 million; trust FuzzParse 2.27
    million; cli FuzzParseLogSpec 6.48 million; safefile FuzzCleanRel 12.55
    million. All 14 targets ran 180 s and passed; no crasher, no new
    corpus entry kept in the repository (the work area only).
  - Own target `FuzzReviewPath` (`Dir.ReadFile`, `SubDirs` and `CleanRel`
    on a fuzzed relative path against a root with links, a link to the
    parent and an absolute link, and a secret outside): 120 s, 7.2 million
    executions, passed: no path outside the root was read and no path other
    than `c/ok/f` returned the file. With the property test (200,000 random
    paths) and the race test (885,000 swaps) in `safefile` this is the
    path-handling check asked for; the three are in the review work area,
    not in the repository.

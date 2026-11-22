<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Portions Copyright (c) 2026, RooyeKhat Media Co -->
# kingres-verify

`kingres-verify` is a standalone command-line verifier for the tamper-evident
ledger of KingresDB (`contrib/kingres_ledger`). It checks ledger digests,
inclusion proofs and exported block headers **without trusting the database**:
an auditor runs it on their own machine with files exported earlier. It is an
independent second implementation of hash format version 1, written from the
documentation (`contrib/kingres_ledger/README.md` and
`kingres/docs/design/ledger-mvp.md`) alone, and it is checked against fixtures
exported from a real server (see "Cross-check against the server").

It is standard library Go only, no third-party modules, so that an auditor can
read all of it (about 14,000 lines outside the tests) and build it with one
command.

## What it can and cannot prove

It verifies **what is in the files you give it**, nothing else.

- The **digest must have been obtained and kept outside the database's
  control** (a different machine, an append-only store, a notary, a printout).
  A digest that was read from the database you are checking proves nothing: an
  attacker who rewrites the whole chain also rewrites the digest. Everything
  `kingres-verify` says is relative to the digest you hold.
- A proof shows that **an entry with this `row_hash` and `table_id` is in the
  ledger**. It cannot know the row. To learn that a row **you hold** is in the
  ledger, **recompute its row hash from data you hold** (`row-hash`) and give it
  to `proof --row-hash` together with `--table-id`. Without those two flags the
  tool says so in its output.
- It does not prove that a row was not later changed *in the table* (the ledger
  is append-only; compare current table contents with the hashes yourself), that
  no other rows exist, or anything about transactions that are not yet in a
  block covered by a digest (the window described in the ledger README).
- Block times are the database server's clock; they are evidence only relative
  to an external anchor of a digest with a trusted timestamp.
- `digest-chain` compares digests only (at least two, from a directory or files you name); `blocks` checks exported headers only.
  Their output says what they do not prove.
- `checkpoint` without `--state` shows that a checkpoint is signed by a key you trust, not
  that it is the latest one (rollback), and reads no tile. With `--state` it also proves,
  from the tiles, that the checkpoint extends the one you recorded, and nothing else about
  the log. `consistency` reads only the tiles on its proof path. Neither shows that the
  entries behind the hashes are what the operator says. Status 3 is an alert: see "Exit
  statuses".

## Installation

```
cd kingres/verifier
go build -o kingres-verify ./cmd/kingres-verify      # Linux, macOS
go build -o kingres-verify.exe ./cmd/kingres-verify  # Windows
```

Releases and how to get a verifier that does not come from the cluster: see "Releases, and how a third party gets the verifier".
Go 1.27.3 or later. `go test ./...` needs no database (the fixtures are
committed). `go.mod` has no `require` line: only the standard library is used.

## Releases, and how a third party gets the verifier (phase 7, step 23)

An auditor must not run a verifier that came from the cluster it checks. Two ways give a
verifier that does not depend on the operator, and both end in the same check.

**From the source, at the release tag (preferred).** Clone the repository on your own machine,
check out the tag, and build with the Go release that `go.mod` names (`go 1.27.3`):

```
cd kingres/verifier
scripts/build-one.sh linux amd64 0.1.0 ./kingres-verify    # GOOS GOARCH VERSION OUTPUT
sha256sum kingres-verify                                   # compare with the line of SHA256SUMS
```

`scripts/build-one.sh` is the one place that holds the flags: `CGO_ENABLED=0 go build -trimpath
-buildvcs=false -ldflags "-s -w -buildid= -X main.version=VERSION" ./cmd/kingres-verify`, with the
micro-architecture levels fixed (`GOAMD64=v1`, `GOARM64=v8.0`), `GOFLAGS=-mod=readonly`, `GOWORK=off`
and `GOTOOLCHAIN=local` (no other Go release is fetched). There is no third-party module, so the
build depends on the Go release only. The same source and Go release give the same bytes on any
machine and at any path.

**From the release files.** A release is these files, made by `scripts/release.sh` at the first
release and at each later one (no tag exists before the first):

| File | What it is |
|---|---|
| `kingres-verify-linux-amd64`, `kingres-verify-linux-arm64`, `kingres-verify-darwin-arm64`, `kingres-verify-windows-amd64.exe` | the binaries |
| `SHA256SUMS` | one line per binary in the form `sha256sum -c` reads: 64 lower case hex characters, two spaces, the file name |
| `RELEASE` | which release this is: the version, the commit, the commit date (UTC) and the Go release that built the files, five lines; it is listed in `SHA256SUMS` like a binary, so the signature covers it (P7R-07) |
| `SHA256SUMS.note` | a signed note (the format of the checkpoints) whose text is exactly `SHA256SUMS`, signed with the release key: Ed25519, key name `kingres-verify-release` |

Check them **before you run anything**, with a verifier you already trust (a build of your own, or
one from an earlier release) and the release key **that you got from the documentation, a printout
or a colleague, not from the place you downloaded the files from**:

```
kingres-verify release verify --sums SHA256SUMS --note SHA256SUMS.note \
    --key <release key> --binary kingres-verify-linux-amd64
```

`--key` is the base64 of the 32 byte public key, or the key string
`kingres-verify-release+<id>+<base64>`. The line you want is `VERIFIED`; it is printed only when
the note is signed by that key, its text is the sums file, and every `--binary` has the SHA-256 of
its line (the line is found by the file's name; `--name` says which line a renamed file is).
Statuses: 0 verified; 1 a check failed, the step is named (`note-parse`, `note-signature`,
`note-text`, `sums-format`, `binary`, `release-info`) and `VERIFIED` is not printed; 2 a flag or file is wrong, or
the key cannot be read. Status 3 is not used: every check that is asked for is made or fails.
With no `--binary` the note alone is checked, and the output says that no binary was.

**The release key.** The project's public key is printed here when the first release is made:

```
release key: (not created yet: the key's custody is a decision of the project owner)
```

Until then there is no release and nothing to check against; do not trust a key that is not in
this file and in the documentation of the same tag. The key is made with
`go run ./cmd/kingres-release-tool keygen --out release.key` (a file of mode 0600 that the tool
will not overwrite and that other users cannot read; no private key is in the repository, and
the tests make theirs). `go run ./cmd/kingres-release-tool pub --key-file release.key` prints the
public key, its key string and its fingerprint.

**Making a release** (the holder of the key, on a machine with the Go release of `go.mod`):

```
scripts/release.sh --version 0.1.0 --out ../../release-0.1.0 --key-file /path/to/release.key
```

`--version` is the product version (what `kingres_version` says, so that the binary the package
installs is the same file as `kingres-verify-linux-amd64`); it is never guessed. The script
refuses a Go release other than the one of `go.mod` (`KINGRES_ALLOW_TOOL_MISMATCH=1` for a trial),
an output directory that is not empty, and a key file that others can read. It builds the four
files, writes `SHA256SUMS`, checks it with `sha256sum -c`, signs it, and verifies the note it just
made with `release verify` before it prints anything. `--unsigned` builds and sums without a note
and says in capitals that the result is not a release.

`release verify` shows what the signed `RELEASE` file says when you give it as a binary (`--binary RELEASE`, step `release-info`: version, commit, commit date, Go release), and it says in
"not proved" that **this is the newest release** cannot be seen from the files: a genuine older release, with a known fault, verifies as well, and the signed files do not expire (a rollback
to an older release is not detectable from them). Compare the version with the project's announcement of the newest release, which you must get from the project and not from the place the
files came from. When the sums list no `RELEASE` file the output says so. When the signed `RELEASE` file says the commit or its date is `unknown` (a set that `release.sh --dry-run` signed from a tree that was not clean or not at the tag), the verdict is `VERIFIED (NOT A RELEASE)` and the exit status is 5, so that a script that tests only the status does not take it for a release; `--accept-dry-run` gives status 0 for whoever wants such a set (the verdict word stays).

`release.sh` makes a **signed** release only from a clean checkout whose `HEAD` is exactly the tag of the version (`--tag NAME`, default `v<version>`, as in `v0.1.0`): it refuses a tree
with any change, tracked or not, a tag that does not exist, a `HEAD` that is not the tag, and a directory that is not a git checkout, and it never creates a tag. `--dry-run` (with `--key-file`) makes and
signs the files anyway for a trial, says in capitals that the result is **not a release**, and records the commit in `RELEASE` as `unknown`. `build-one.sh` sets `GOENV=off` and unsets
`GOEXPERIMENT`, `GOFIPS140` and `GODEBUG`, so that a setting made with `go env -w` on the machine of the releaser cannot change the bytes; `repro-check.sh` builds one more binary under a hostile Go environment (a
Go environment file that sets `GOEXPERIMENT` and `GOFIPS140`, and the same variables exported) and requires the same bytes.

**Checking that the build is reproducible.** `scripts/repro-check.sh` copies the source twice into
directories of different depth and name length, gives each its own empty Go build cache and GOPATH,
builds all four files in each with `release.sh --unsigned`, and compares every byte. It prints
`REPRODUCIBLE` and the sums (exit 0), or `NOT REPRODUCIBLE` and the names that differ (exit 1).
Run it on a clean checkout before a release. On Linux with `go` on the `PATH`:

```
scripts/repro-check.sh [--work DIR] [--targets "linux/amd64 windows/amd64"]
```

**The copy on the server.** The Debian package installs a copy of `kingres-verify` in
`/opt/kingres/bin`, built with `build-one.sh`, and `LimitCORE=0` is in the service unit. The copy
is for convenience; an auditor does not use it. `kingres/packaging/test/test-verifier-package.sh`
checks, without Docker, that the package holds it, that it is the same bytes as the release file
of the same version, and that `release verify` of the packaged copy accepts a release made in the
test.

**What this does not prove.** That the key is the project's (you passed it), that the code is
free of faults, that the source was reviewed. A matching hash shows that your binary is the one
whose hash the key holder signed, and that it can be rebuilt from the source you read.

**Decisions of step 23** (the design is silent or leaves them open):

- The signing side is a separate program, `cmd/kingres-release-tool` (`keygen`, `pub`, `sums`,
  `sign`), not a subcommand of `kingres-verify`: the verifier never signs, and the tool is not part
  of a release. It reads a key only from a file (never from the command line or the environment),
  refuses a key file that others can read on Unix, and refuses to overwrite a key, a sums file or a note.
- `go.mod` is not changed by the release (the design says a `toolchain` line is added by the
  release step): a script that edits a tracked file makes the tree it builds differ from the tag.
  The `go` line already names the exact release (`go 1.27.3`); `release.sh` insists that the
  installed toolchain is that one, and uses a `toolchain` line instead when one is present.
- The key may be given as the raw base64 (44 characters) or as the key string; an input of 44
  characters is always the raw form, because that base64 may itself contain plus signs.
- The sums file allows only plain file names (letters, digits, dot, dash, underscore), no
  path, no duplicate, the two-space form only, and the note's text must be byte for byte the
  file, so the tool never signs sums that this check would refuse.
- `ParseVKey` split a key string on every plus sign, so a key whose base64 holds one (about half
  of all keys) could not be read; it now splits into three parts.
- An unsigned build is possible (`--unsigned`) only so that the reproducibility check and the
  package test can run without a key; it writes no note and says it is not a release.

## Exit statuses

| Status | Meaning |
|---|---|
| 0 | everything that was asked for was verified (`VERIFIED` or `CONSISTENT` is printed) |
| 1 | a verification failed (the failed step is named), or the publication holds a file in a form it must not have (a symbolic link on the way, a directory, device or named pipe, a file over its size limit, a tile of the wrong size), or the thing under test cannot be parsed (a proof, a block export, a checkpoint note); the word `VERIFIED` is never printed |
| 2 | usage or input error: a flag is missing or wrong, a file or directory **you named** cannot be read (it is absent, not a regular file, too large) or is malformed (digest, columns file, trust file, state file), or no key of the trust file can be used at the time given, or **a checkpoint you hold** (`--state`, `--old FILE`) cannot serve as your anchor (no key of the trust file signed it; for a plain note given as `--old FILE` also: only a revoked key, or one beyond its `max_tree_size`, signed it; see "Checkpoints you hold"), or the state file stayed locked by another run for 10 seconds, or the lock file belongs to another user, or **the result could not be written** to the standard output (a full disk, a short write, a closed pipe: the line `could not be written to the standard output` is on the standard error; a closed descriptor: see "The standard output"), in text and in `--json`, on every command: a verdict nobody could read is not status 0. A run whose verdict is not 0 keeps its status when its output cannot be written; nothing was verified |
| 3 | incomplete: nothing failed, but **something that was asked for could not be checked with the inputs and flags given**: the publication does not contain it (a checkpoint, a tile, the directory of a log) or the source cannot be reached; `INCOMPLETE` is printed and the missing items are listed; `VERIFIED` is never printed. **Treat 3 as an alert, never as "probably fine"**: a publisher must serve the tiles of every checkpoint it signed, so one that withholds exactly the tiles that would show a fork gets status 3 and not status 1, and a missing file is at best neglect. The commands that read a publication or a bundle can end with 3 (`checkpoint`, `consistency`, `aggregate`, `anchors`, `attestation`, `bundle` and `monitor`); the commands that check one given proof, block export or digest list do not; a failure always wins over an incomplete check |
| 4 | a fork or a rollback was found, which is what status 1 says for it, **and the conflicting checkpoint could not be saved** (every name for the evidence is taken by a file with other content, or the disk refuses it); `FAILED at step fork` (or `rollback`) is printed with a note that says `was NOT saved`. The finding stands and is as serious as a 1: act on the fork and save the note you were shown yourself. Only `checkpoint --state` can end with 4 |

A malformed **proof** or **block export** is a failed verification (status 1, step
`parse`): it is the thing under test. A malformed **digest** is an input error
(status 2): the trusted anchor is unusable. `--json` prints one JSON object
(`command`, `verified`, `steps`, `failed_step`, `error`, `notes`, `not_proved`, `info`, `problems`,
for status 3 `incomplete` and `missing`, and `invalid_utf8` when a string had bytes that
are not UTF-8) instead of text; the exit status is the same.

**Which command ends with which status.** The statuses of each command are the ones its exit
sites in the source can give; `TestEveryExitSiteHasARowAndAScenario` reads the sites from the
source (every call of a finish function, of the flag parser and of `exit`) and fails when one
has no row or when the scenario of a row does not reach it with its status, and
`TestReadmeTableOfStatusesIsTheTableOfTheCode` fails when this table differs from the rows.
Status 0 of a command that has none for a failure is a success; `trust` alone is a missing or
unknown subcommand.

| Command | Statuses it can end with |
|---|---|
| `proof` | 0, 1, 2, 3 |
| `row-hash` | 0, 2 |
| `digest-chain` | 0, 1, 2 |
| `blocks` | 0, 1, 2 |
| `checkpoint` | 0, 1, 2, 3, 4 |
| `consistency` | 0, 1, 2, 3 |
| `aggregate` | 0, 1, 2, 3 |
| `anchors` | 0, 1, 2, 3 |
| `bundle verify` | 0, 1, 2, 3 |
| `bundle fetch` | 0, 1, 2, 3 |
| `monitor` | 0, 1, 2, 3 |
| `attestation` | 0, 1, 2, 3 |
| `release verify` | 0, 1, 2, 5 (a genuine set made by a dry run: `--accept-dry-run` gives 0) |
| `trust init` | 0, 2 |
| `trust show` | 0, 2 |
| `trust follow` | 0, 1, 2, 3 |
| `trust` | 2 |

**Status 3 is not a pass.** It means that the publication did not contain something that was
asked for. A publisher that wants to hide a fork can withhold exactly the tile or the file
that would have shown it, and the run then ends with 3 and not with 1; it can never end with
0. Treat 3 like 1 when you decide what to do, and ask the publisher for the missing files.

**The standard output** (L12-04, L12-09). A Go program started with its standard output closed
(`>&-`) does not see a closed descriptor: on Unix the runtime opens the null device on every one of the
descriptors 0, 1 and 2 that is closed at start, and a write to it succeeds, so the verdict was lost and the
status was 0. What the program can still see, after the runtime changed it, is how the replacement was
opened: the runtime opens the null device for reading **and** writing, and a shell that is asked for no output
(`>/dev/null`, `>>/dev/null`) opens it for writing only. So, on Linux, macOS and the BSDs
(`internal/stdio`, called first by `cmd/kingres-verify`, for every command including `version` and `--help`):

- a standard output that is the null device opened for reading and writing is taken for a closed
  descriptor: nothing is run, the standard error says `the standard output is closed ... nothing was run`,
  and the status is 2;
- a standard output that is the null device opened for writing only is what a caller who wants the status and
  no text asks for: the program runs as usual, and a verdict that is not 0 keeps its status;
- a file, a pipe and a terminal are normal.

The cost is one refusal that is not a closed descriptor: `1<>/dev/null`, which opens the null device for
reading and writing on purpose, is refused as well; write `>/dev/null`, or say that it is on purpose (the
paragraph after the tests). What is not detected: a descriptor
that was closed and then given by the parent to something that accepts writes and loses them, and any system
other than those named (no check is made there). On Windows a closed standard output is a null or invalid
handle, which the runtime does not replace by the null device: the write fails, and the run ends with status 2
and `could not be written` like any failed write; `> NUL` is the null device and is accepted. A reader that
closes a pipe early kills the program with SIGPIPE (the shell sees 141, also when SIGPIPE is ignored); a
reader that takes only the first bytes (`head -c 5`) leaves status 0 and a cut line, because the whole output
fitted in the pipe: the program cannot know, and it is the caller's cut. Tests: `TestClosedStandardOutputIsStatus2`,
`TestStandardOutputThatTheUserChoseRuns` (Unix) and `TestClosedStandardOutputOnWindowsIsStatus2` start the
program as a child.

**A caller that gives the null device on purpose** (L14-01). Python's `subprocess.DEVNULL`, `daemon(3)` and
the usual double-fork recipes open `/dev/null` for reading and writing, which the program cannot tell from the
runtime's stand-in for a closed descriptor (nothing in the descriptor tells them apart), so by default they
get the refusal above: status 2, nothing run. Such a caller says what it did with the environment variable
**`KINGRES_VERIFY_NO_OUTPUT=1`**: "the standard output I gave you is the null device on purpose, the status is
the only answer I read". The program then runs as usual and writes its verdict to the null device, where it is
lost, and the status is the answer. Only the value `1` counts (`0`, an empty value and anything else keep the
refusal, and the refusal names the variable), and it changes nothing when the standard output is anything but
that null device: a pipe, a file and a terminal show the verdict as ever. The cost: a really closed descriptor
(`>&-`) with the variable set is accepted too, because nothing can tell it from the null device; a caller that
sets the variable has said that it does not read the output. How to call the program:

- Python: `subprocess.run([...], stdout=subprocess.DEVNULL, env={**os.environ, "KINGRES_VERIFY_NO_OUTPUT": "1"})`
  and read `returncode`. The standard error may be a pipe: an input error is still written there.
- C with `daemon(3)`, and any double-fork recipe that opens `/dev/null` with `O_RDWR`: `setenv("KINGRES_VERIFY_NO_OUTPUT", "1", 1)`
  before `execve`, or run `env KINGRES_VERIFY_NO_OUTPUT=1 kingres-verify ...` as the program.
- A shell: `>/dev/null` needs nothing; for `1<>/dev/null` write `KINGRES_VERIFY_NO_OUTPUT=1 kingres-verify ... 1<>/dev/null`.
- systemd, `StandardOutput=null`: not run here, and I believe it opens the null device for writing only, which is accepted
  without the variable; `Environment=KINGRES_VERIFY_NO_OUTPUT=1` in the unit makes it work either way and is harmless.
  cron and a terminal give a mail or a terminal, which is a normal output.
- Windows: nothing is needed. A closed handle fails the write (status 2, `could not be written`), and `NUL` (what
  `subprocess.DEVNULL` opens there) is accepted; the variable is read and changes nothing.

Tests: `TestNullStandardOutputGivenOnPurposeRunsWhenTheCallerSaysSo`, `TestPythonDevnullCallerWithAndWithoutTheVariable`
(the real `subprocess.DEVNULL`; skipped where there is no `python3`), `TestNullStandardOutputOnWindowsRunsWithOrWithoutTheVariable`.

Text and JSON are made from one structure: every `ok` step, `note`, `not proved`,
`missing` and `problems` line of the text is a member of the JSON (`steps`,
`notes`, `not_proved`, `missing`, `problems`) and the other way round; a test fails when a
command prints a line in only one of the two. `notes` is what the reader must know about
how the run was made (the clock that was used, a key that was left out and why, a state
file that was written); `not_proved` is what a success does not show. A failed result
carries its notes too.

**Text from the inputs is escaped, one to one.** A file name, a key of a JSON file, a label, a path
on the command line or an error text that quotes them can come from the publisher or the
database server. In the text output (and on the standard error, in both formats) every
such string is written by one function, and what it writes can be read back exactly:

- a backslash is written as **two** backslashes (`\\`), so that no input can write an escape or the
  cut marker;
- a control character (the C0 range, newline and tab included, and DEL) is written `\xNN`
  (a newline is `\x0a`, escape `\x1b`); a byte that is not part of valid UTF-8 is written `\xNN`
  too, with `NN` of 80 or more, which a valid character never gives;
- any other character that changes how text is drawn or split or that is not printable (the
  C1 range, the bidirectional marks, overrides and isolates, the line and paragraph separators,
  the byte order mark, zero width and other format characters) is written `\uNNNN`, or
  `\UNNNNNNNN` above U+FFFF, in lower case hex;
- a value of more than 2,048 bytes is cut at a character boundary and ends with
  `\...(cut: N of M bytes shown)`: `\.` is never written for any other input, so no input can
  print the marker, and M is the original length. **This is the only cut**: it is made on the raw
  value, **before** escaping, at a character boundary, so it cannot split a multi-byte character, and
  an escape is never cut because it is made after the cut. The escaped text is longer than the cut: a
  control character is four bytes, so 3,000 bytes of `0x01` print as 8,192 bytes and the marker, at
  most four times the 2,048 that are shown (`TestTheCutIsMadeBeforeTheEscape`). The
  older cuts of a value at 60, 80, 100 or 120 bytes before it was escaped (which could split a
  character, made two values print alike and made a value that ends in `...` look cut) are gone;
- everything else is written as it is.

To read it back: two backslashes (`\\`) are one backslash, `\xNN` the byte NN, `\uNNNN` and `\UNNNNNNNN` the
character, and the cut marker ends the text. A message that quotes a value between double
quotes was quoted by Go first (`%q`) and then escaped, so undo the escape first and then Go's
quoting. A real newline and the two characters backslash and `n` print differently
(`\x0a` and `\n`), and `TestUntrustedIsOneToOne` and a fuzz target test that two different
inputs never print the same and that the original comes back when it was not cut.
So no input can start a line (`VERIFIED:`, `FAILED at step`, `  ok  `, `  note:` ...) or
move the terminal, and `TestNoPrintOutsideTheOutputLayer` reads the source and fails when code
outside the output layer prints anything but literals or a value that went through the function.

The JSON output escapes the same characters: everything above that the text writes as an
escape is written in the JSON as a `\uXXXX` escape (a pair of them above U+FFFF), so a JSON
file shown on a terminal cannot move it either; `<`, `>` and `&` are written as they are.
A JSON string cannot carry bytes that are not UTF-8: each becomes U+FFFD and the object has
`"invalid_utf8": true` (a real U+FFFD in the input does not set it). The JSON gives the
strings as they are, without the doubled backslash: a program reads them, a person reads the text.

## Subcommands

### `proof`

```
kingres-verify proof --proof proof.json --digest digest.json [--table-id N] [--row-hash HEX] [--op I|D|any] [--json]
```

Runs the verification steps of the ledger README on the proof against the
trusted digest: the chain id (step `chain-id`; `binding` first when
`--table-id`/`--row-hash` are given), the entry leaf and its audit path to the
transaction root (`entry-path`), the transaction leaf and its audit path to the
block's transaction root (`tx-path`), the block hash recomputed from the header
(`block-hash`, and for block 0 the genesis `prev_hash`), then the chain of later
headers up to the digest height with linkage, recomputed hashes and
non-decreasing block times, ending at the digest's block hash (`chain-walk`). If
the digest carries `block_closed_at` it must equal the block time of the digest's
block (`digest-time`). A digest older than the proof's block fails at
`chain-walk`. Numbers must be plain JSON integers in range; hex must be lower case.

**Insert and removal proofs.** A proof is for one ledger entry, and an entry has an
operation: `I` (a version of a row was inserted) or `D` (a version of a row was
removed, by an `UPDATE` or a `DELETE` of an updatable table). They are different
statements: "this version was inserted" does not say that it is still there, and
"this version was removed" does not say who made the row. The verifier accepts
proofs of either operation, because the entry leaf covers the operation byte and a
proof whose `op` was changed fails at `entry-path`. `--op I` or `--op D` makes the
expected operation the caller's explicit choice: a proof of the other operation
fails at step `binding`. The default, `--op any`, accepts either and **reports
which one was proven** (in the text output as `operation D: ...`, in `--json` as
`info.op` and `info.proves`). Use `--op` whenever you mean one of the two. Any
other value of `op` in the proof is a parse failure.

```
$ kingres-verify proof --proof proofs/acc5.json --digest digests/d5.json \
    --table-id 1 --row-hash 56700cbe26b660e69f01339a5629217dde30b035480f68f9c7873caca086d856 --op I
VERIFIED: the proof is valid against the digest at height 9.
  ok  binding      proof is for the expected table, row and operation
  ...
$ echo $?
0
```

### `row-hash`

```
kingres-verify row-hash --table-id N --columns columns.json [--encoding] [--json]
```

Computes `row_hash = leaf_hash(canonical row encoding)` and prints it as 64
lower case hex characters (`--encoding` also prints the encoding). The table id
is the `table_id` of the proof (or `SELECT table_id FROM kingres.ledger_table WHERE relid = 'app.accounts'::regclass`).

**Columns file.** A JSON array with one object per **non-NULL** column, **in
column order** (attribute order of the table, dropped columns omitted). Each
object has exactly two members:

- `name`: the column name, a JSON string (UTF-8; it is encoded as UTF-8 bytes);
- `send_hex`: the bytes of the value's **binary send form** (the output of the
  type's `typsend` function, without the protocol's length header), as lower
  case hex. A zero-length value (an empty string, an empty `bytea`) is `""`; a
  NULL column is **left out of the array**, it is not an empty string.

```json
[
  {"name": "id",    "send_hex": "00000001"},
  {"name": "owner", "send_hex": "616c696365"},
  {"name": "note",  "send_hex": ""}
]
```

Unknown members, duplicate keys, upper case or odd-length hex are rejected
(status 2). Text-like send functions convert to the client encoding, so
obtain the bytes with `SET client_encoding = 'UTF8'`. For example, for
`app.accounts(id int, owner text, note text)`:

```sql
SET client_encoding = 'UTF8';
SELECT encode(int4send(id), 'hex'), encode(textsend(owner), 'hex'), encode(textsend(note), 'hex')
FROM app.accounts WHERE id = 1;
```

`scripts/gen-fixtures.sh` contains a generic helper (`app.fx_cols`) that builds
the whole file for any table from `pg_type.typsend`. An auditor who does not
hold the database can build the file from their own source data, using the send
format of each type (for example `int4send` is 4 bytes big-endian, `textsend` is
the UTF-8 bytes, `boolsend` is one byte, `timestamptzsend` is the 8-byte
microseconds since 2000-01-01 UTC).

### `digest-chain`

```
kingres-verify digest-chain --digests DIR|FILE [--digests FILE ...] [FILE ...] [--json]
```

Takes several digests of the same chain exported at different times (a
directory stands for its `*.json` files) and checks that they are mutually
consistent as far as digests alone allow: the same `chain_id`; sorted by height,
block times do not go back; equal heights have equal block hashes (and equal
`block_closed_at`); and, when every digest has `generated_at`, heights do not
decrease in generation order (otherwise the database went back to an earlier
state). At least two digests are required. Exit status 1 lists every problem.
A directory is listed by name (a directory called `x[ab]` is that directory, never a
pattern) and is read through the same helper as a source: at most 1,024 entries, no link
followed. At most 1,024 digest files are read in all (named or in a directory), each at most
64 KiB (a digest is a few hundred bytes) and 16 MiB together; more is status 2.

**Names in a digest directory.** A name the directory listing returns is a name, whatever
it holds (a colon, a space, a leading dash, a backslash on Linux): it is never read as a
path, so only what cannot be one entry is refused (nothing, `.`, `..`, a slash, a NUL, more
than 255 bytes). The export command of this README (below) makes names without a colon
(`digest-20261114T172153Z.json`), which every file system holds, and a test runs the very text
of that command, as far as it can run without a database, and reads the directory it fills.

It does **not** prove that the blocks between the digests are intact, that any
row is in the ledger, or that any digest is authentic; `generated_at` is not
authenticated. Use `proof` or `blocks` for that.

### `blocks`

```
kingres-verify blocks --blocks blocks.jsonl (--digest digest.json | --chain-id HEX) [--json]
```

Takes an export of the block headers, one JSON object per line, and recomputes
every block hash from the genesis hash `SHA-256(0x14 || chain_id)`: the file
must start at height 0, heights must be dense, each `prev_hash` must be the
previous block's hash, and block times must not go back. With `--digest` it also
checks that the chain reaches the digest (block at the digest height has the
digest's hash and `block_closed_at`). The chain id comes from the digest or from
`--chain-id` (the export has no chain id; without either, status 2).

The export is read line by line and only one header is kept, so memory does not grow
with the file: a measured run over 1.2 million blocks (a 400 MB file) used 12 MB of
resident memory, where reading the whole file and all headers (the earlier version) used
1.15 GB (2.9 times the file). A line is at most 1 MiB and the file at most 1 GiB. The
first problem in file order is reported, so a later damage is found only if no earlier
one stops the run (a parse failure and a chain failure are both status 1, because the
export is the thing under test). A read error, a line over the limit or a line that is
cut in the middle after a good start is such a failure at the step `parse`, never "verified
up to the cut".

**What `blocks` establishes, and what it does not.** What it proves is that the blocks it
read are a chain: from the genesis hash of this chain id, dense heights, every hash
recomputed, times that do not go back, and with `--digest` a block at the digest's height
that the digest names. The output says how far that goes: `the N block headers read (heights
0 to H) form an unbroken chain from the genesis hash`, with the hash of block H, and in
`--json` `info.blocks`, `info.head_height`, `info.head_hash` and `info.complete_export_shown`
(always `false`). **It cannot tell a complete export from one that was cut**: the hash of a
block does not say whether another follows, so an export that ends at any block verifies the
same way. Without `--digest` the output says that; with one it says that the digest covers up
to its height and that blocks after it, or an end before the head of the ledger, are not
covered. An export that ends before the digest's height is a failure at the step `digest`.
To know that the export is the whole chain, compare block H with a digest you hold of height
H or above, or with the head the ledger reported when you made the export.

Each line has the header fields: `height`, `hash_version`, `prev_hash`,
`tx_merkle_root`, `tx_count`, `closed_at_us` (microseconds since 2000-01-01 UTC,
an integer), `block_hash` (hex lower case). Export:

```
psql -X -At -c "SELECT json_build_object('height', height, 'hash_version', hash_version,
    'prev_hash', encode(prev_hash, 'hex'), 'tx_merkle_root', encode(tx_merkle_root, 'hex'),
    'tx_count', tx_count, 'closed_at_us', kingres.ledger_timestamptz_us(closed_at),
    'block_hash', encode(block_hash, 'hex'))
  FROM kingres.ledger_blocks ORDER BY height" > blocks.jsonl
```

`kingres.ledger_blocks` is readable by PUBLIC. This verifies the headers only: it
says nothing about transactions or rows.

### `checkpoint`, `consistency`, `trust` (phase 7, step 1)

These commands check signed checkpoints of tile logs, the format of
`kingres/docs/design/ledger-phase7.md` (sections 3, 4 and 7). They need a trust
file that **you** made from a key you obtained outside the cluster. They are
built from the public C2SP texts `signed-note`, `tlog-checkpoint` and
`tlog-tiles` and from RFC 6962 and RFC 9162, with Ed25519 only.

```
kingres-verify trust init --origin-prefix P (--public-key B64 | --public-key-file F) --fingerprint HEX [--name LABEL] [--out FILE|-]
kingres-verify trust show --trust trust.json [--now TIME] [--json]
kingres-verify checkpoint --trust trust.json --log chain:<id>[:e<N>]|aggregate:<id> [--source DIR | --note FILE] [--min-size N] [--state FILE [--allow-shared-state-dir]] [--now TIME] [--json]
kingres-verify consistency --trust trust.json --log <as above> --old NOTEFILE|SIZE:ROOT [--new NOTEFILE] [--source DIR] [--now TIME] [--json]
```

**`trust init`** refuses to run without `--fingerprint` (the SHA-256 of the
public key in hex; upper case and colons are accepted) and refuses a key that
does not match it, so a mistyped or swapped key is noticed. It never overwrites
a file. `trust follow` (rotation events of the publication) is described below, with
the commands of step 7.

**`trust show [--now TIME]`** validates a file and prints, for each key, how it
stands at `--now` (default the system clock): `usable`, `not yet valid`,
`expired` or `revoked`, and a line for each validity member the key has that
says that it is enforced and how (below). A file that is well formed but in which
no key can be used at that time is reported as such and ends with status 2,
because every verifying command would refuse it; a file with no key at all is
refused when it is read. The time-stamp authority roots and client keys of the
file are validated and counted, and the output says that no command uses them yet.

**Validity of a key** (design 14, correction 14). A checkpoint carries no time that
can be trusted, so the verifier can enforce what the data proves (a tree size) and
what the auditor says (the time given with `--now`), and nothing else:

| Member | Effect |
|---|---|
| `not_before`, `not_after` | enforced on `--now` (default the system clock, printed in the output with its fraction of a second), inclusive at both ends (a time equal to `not_after` is still inside, one nanosecond later is not): a key outside its window is left out, with a note that says why; if no key is left the command ends with status 2 and lists each key and why. After `not_after` a retired key's checkpoints therefore stop verifying as **new** checkpoints unless you give an earlier `--now`, which is your statement and is printed. **A checkpoint you already hold** (`--state`, `--old FILE`) is not subject to the window: see "Checkpoints you hold". The window is measured on the clock of the machine you run on: a machine whose clock was set back still accepts a key whose `not_after` has passed, so for a key that leaked use `revoked`, which does not depend on the clock. This fails closed: a member that only reported would look like a control and not be one. `--now` must be an RFC 3339 time with a point as the decimal mark; an empty `--now` (an unset shell variable) is an error, never the clock |
| `revoked` (true or false) | true: the key is never used for a new checkpoint, whatever `--now` says. A revocation says that the key must not be trusted **for new notes**, from now on; it does not make a checkpoint you already verified and stored unknown to you: such a state stays your anchor (it only ever makes a run stricter), flagged in the output as resting on a key the file no longer counts (see "Checkpoints you hold"). A note signed by a revoked key and by a valid one is accepted through the valid key, and the output says that a signature of the revoked key was on it and was not counted. There is no `compromised_since` member: a checkpoint carries no time that anyone can trust, so a "since" cannot be applied; `max_tree_size` is the form of it that the data proves |
| `max_tree_size` (an integer from 1) | a checkpoint counts as signed by this key only if its tree size is at most this: the way to retire a key at a known size so that what it signed stays valid and what it signs later (a leaked key) is refused, step `key-size`. It does not stop a leaked key from signing a different history at a smaller size: only a checkpoint or consistency proof you already hold catches that. The limit is applied to a checkpoint you hold too. `trust show` cannot know the size of your log, so a key it reports as usable may still be refused for a larger checkpoint. `revoked` and `max_tree_size` together are refused |

**`checkpoint`** verifies one checkpoint note: the note is well formed, at
least one signature is by a key of the trust file that may be used now, no
signature of such a key is wrong, the origin is the origin of the named log below
the trusted prefix, the tree size is within what the trust file allows for the
signing key, and the size and the root are well formed (`--min-size` fails a
smaller tree). It reads `<source>/c/<id>/e<N>/checkpoint` (aggregate:
`<source>/a/<id>/checkpoint`) or `--note`. Without `:e<N>` the epoch is taken
from the source only when it holds exactly one epoch of that chain; with `--note`
the epoch must be given. It checks no tile.

**Rollback, and what `--state` and `consistency --old` are for.** `checkpoint` shows that a
checkpoint is signed, **not that it is the latest**: an older genuine checkpoint verifies, so a
publisher that serves one hides every later entry. Without `--state` the output says so ("not
proved: that this is the latest checkpoint ... (rollback)"). There are three ways to close it:

- `--min-size N`: fail a tree smaller than the size you already saw.
- `checkpoint --state FILE`: the way to **follow** a log over time. The state file is your
  record of the last checkpoint this command proved. The invariant is that **the state only ever
  moves from a checkpoint to one that this very run proved to be an extension of it, for the same
  bytes whose signature it verified** (the note is read once and used for both), so status 0
  with `--state` means exactly that. A larger checkpoint is therefore proved in the same
  command: it reads the proof tiles from `--source` (RFC 9162 2.1.4.2) and only then replaces
  the state (atomically: a temporary file in the same directory, flushed, renamed over the state,
  the directory flushed; on Windows the rename is written through). The first run creates the
  state and says that nothing earlier was known. The same checkpoint again changes nothing and
  needs no tile. A smaller size, the same size with another root, or a path that does not lead to
  the stored root is **status 1**: the state is left untouched, the conflicting note is saved
  next to it as `<state>.conflict-<size>-<root in hex>` (see "The evidence of a fork"), and the output names both files: two notes signed by the same key for
  trees that are not one history are the proof of misbehaviour, so **keep both**. A smaller size
  is reported as `rollback` and may be an old genuine checkpoint or another history; this run does
  not tell which. If the tiles are missing the result is **status 3** and the state stays; if a tile
  is present and wrong (the tiles do not lead to the signed new root) it is status 1 and the state
  stays. `--source` must therefore be a source that holds the tiles; without them `--state` cannot
  advance.
- `consistency --old FILE|SIZE:ROOT`: a **read-only** proof for a checkpoint you hold by hand (a
  note you saved, a state file, or a size and root from elsewhere). It writes nothing, and it does not
  update a state file: it is the check for an old note that is not your state, or for
  a second opinion. The earlier advice to run `consistency --old STATE` and then `checkpoint --state
  STATE` is withdrawn: they were two reads of the publication, a publisher could show an honest
  checkpoint to the first and a fork to the second, and the second replaced the evidence of the
  first. `checkpoint --state` does both in one run.

**The state file** is a small JSON record (`format`, `log` the origin, `size`, `root`,
`verified_at`, `verified_by` the fingerprints of the keys, `note` the signed note **in its canonical
form**, see "What the state holds"). The facts are
for the reader, and they must agree with the note (see "What a state file must be"); the decision
is the note's: it still has to carry a valid signature of a key of the trust file, of any
standing (a person who can write the file can also delete it). `verified_by` and `verified_at` are
information for a person: no decision uses them, and they are not compared with anything but their
form.
It may be a link (the state is replaced where the link points, the link stays a link; a link to
nothing is refused). A new state file has mode 0600, a replaced one keeps its mode. The temporary file has a short
name that does not depend on the length of the state's name. The state file is locked with `flock`
(Linux, macOS) or `LockFileEx` (Windows) on `.kingres-state.lock` in its directory for the whole read,
check, proof and replace; another run waits up to 10 seconds and then ends with status 2 (nothing
changed), so two runs never move the state backwards. A reader that does not take the lock (`consistency --old STATE`) can on
Windows meet a sharing error while the file is replaced; run it again. A crash leaves the old state or the new
one, whole, and at worst a temporary file. The next run removes it under the lock, together with every other
temporary file of this program in the directory, whatever name it was made under: a crash in the extension of an
evidence file leaves a file named after that evidence file, which no run of the state would look for otherwise
(L16-04). What is removed must be a regular file whose name is exactly one this program makes
(`.kingres-state-` or `.kingres-evidence-`, 12 hex characters, a dash, a decimal number, `.tmp`) and, where the
system says who owns it, one of the user running the command; a link, a directory or any other file with
such a name, and everything else in the directory, is left alone. A state that is itself named like that is refused at
the start, with status 2 and `give the state another name; nothing was changed` (L18-01), before anything is read: a
run of any state in the same directory would remove it as a leftover, and refusing it is the one answer that cannot
surprise an auditor (`TestStateNamedLikeATemporaryFileIsRefusedAndNeverRead`; the cleanup also never removes the file
the run was told to use, `TestRemoveLeftoversNeverRemovesTheFileTheRunWasToldToUse`) (`TestRemoveLeftoversRemovesEveryTemporaryFileOfTheProgramAndNothingElse`,
`TestRunRemovesTheTemporaryFilesOfTheProgramAndNothingElse`, `TestCrashInTheExtensionOfAnEvidenceFileLeavesAFileTheNextRunRemoves`).
A stop after each step of the replace is tested on both platforms. Old state files that are a plain signed note are still read.

**What the state holds (the canonical form).** The invariant, in one sentence: **the identity of a
checkpoint is its signed text (origin, size, root, extension lines) and never its signature lines; the
lines of one text only accumulate, so a state keeps the union of every line that verified for that text,
two notes with the same text are the same checkpoint (no fork, no evidence file of their own, no
status 4), and a state is usable as long as its text verified once under a key of the trust file,
whatever became of that key afterwards.** The last clause is right because a revocation says that
a key must not be trusted for **new** notes from now on; it does not say that a checkpoint you verified
and stored is unknown to you, and a note carries no time that anyone can trust, so a "compromised
since" cannot be applied to it (the trust file has `revoked` and `max_tree_size`, and no
`compromised_since`, for that reason). A held state only ever makes a run stricter (a smaller size, a fork,
and a larger size that is not proved to extend it are failures), so keeping one that rests on a revoked
key cannot make a run pass that the held text does not allow (it can make a run fail that would pass without it, which is the point of
keeping it, and the output of such a failure says whose word it rests on: "A conflict with a state that rests on keys that no longer count"); refusing it would make the choice of lines that a
publisher served decide whether later runs can go on (L12-01). It is not shown to be genuine now, and the
output says so ("Checkpoints you hold"). Nobody who holds no trusted key can put a byte into the state,
choose by serving some of the genuine lines what a later run does, or make the exit status, the name of an
evidence file or the stored state differ by anything that no verified signature covers. A later change
of the trust file (a key added, retired, revoked, limited, a window changed) never makes a stored state
unreadable, except by removing every key whose line the state holds (L14-08, "The one way left to make a held
state unreadable"), and never makes it accept less. The canonical form of a note is

1. the text of the note, which is what the signatures cover (origin, size, root, extension lines);
2. one blank line;
3. the signature lines of **every** key of the trust file, whatever its standing now (usable, retired,
   not yet valid, revoked, beyond its `max_tree_size`), whose signature **verifies** over that text,
   each written again from the parsed signature, in the order of key name and key id. (Until this
   round it was the lines of the keys that counted in the run; which lines a state held then depended on
   the standing of the keys that day and on which of the genuine lines a publisher served.)

When the state already holds the same text, the lines of the note served and of the note held are
merged: the union of the lines that verify. The state is rewritten only when it gains a line
(`info.state.action` is `signatures_added`, and the note says so; otherwise `unchanged`, and the file
is not touched). The evidence of a fork or rollback is merged the same way (below).

Every input of a note, of a state, of a publication, and what it can change. "Covered" means that a
signature that verified covers it:

| Input | Covered | What it can change |
|---|---|---|
| the text (origin, size, root, the extension lines) | yes | it is the identity of the checkpoint; stored as it is |
| the blank line before the signatures | no, structure | written again; nothing |
| a signature line of a key of the trust file that verifies, counted or not (revoked, outside its window, over its `max_tree_size`) | yes | stored, merged with the lines the state already holds; the output names the ones that were not counted |
| a signature line that names a key of the file that is not usable now and does not verify | no | dropped; the output says "carries a line that names key ... and does not verify", it is never called a signature; no effect on the status |
| a signature line that names a usable key and does not verify | no | a **new** note is refused (status 1, step `signature`) and nothing is stored; a **held** note ignores it with a note. A publisher can refuse any note by serving it wrongly or not at all, and this is the same refusal: it never moves or changes the state |
| a signature line of a key the trust file does not know | no | dropped, counted in "N signature lines that were not counted" |
| the order of the signature lines, how many unknown ones there are | no | nothing: the stored note is the same bytes |
| which of the genuine lines are present (a subset of them, one or all) | the lines are, their absence is not | the state keeps the union of the lines served over the runs; the status, the size stored, the evidence name and the verdict of every later run do not depend on which. A note whose served lines include none that counts is refused, which is the refusal above |
| a line repeated (same key name and key id), a line that is not a well formed signature line, a control character, a missing newline | no | the note is refused as malformed (status 1), nothing is stored |
| the tiles | the hashes are checked against the signed roots | only the tiles on the proof path are read, and each is hashed into the signed new root: a changed tile is status 1, a missing one status 3, and neither moves the state |
| the names and number of the files of the publication (a directory listing, a second epoch directory, a bundle) | no | only names built from the log named on the command line are opened; a listing is read only to find the one epoch when `:e<N>` is not given (at most 1,024 entries), and a second epoch makes that a status 2 that asks for `:e<N>`: a refusal, never another state |
| the names of the files beside the state (the evidence names, the lock file, the temporary files) | no | built by the program from the state's name, the size and the whole root; an existing file is looked at by its exact name and a link is never followed |
| the members of the state record other than `note` (`verified_at`, `verified_by`, `log`, `size`, `root`) | no | information for a person; `log`, `size` and `root` must agree with the note, and no decision uses the others |

The place where the lines are chosen is one function (`signednote.Merge`, called through
`canonicalOf` before anything is stored or saved), so the number and the order of the lines a publisher
serves changes nothing that a run writes or decides: the same size and root with ten different sets of
junk lines, or with every subset of the genuine lines, is one evidence file, status 1 and not status 4
(L10-09, L12-03). The canonical note keeps every line that verifies, so it verifies under every trust
file that still holds one of the keys that signed it, whatever its standing. A note you **hold** (`--state`,
`--old FILE`) is judged by the signatures that verify: a line that names a key of the file and does not
verify is ignored, with a note (a state written by an earlier version holds the note as it was served,
including such lines, or only the lines of the keys that counted, and is still read; it is rewritten when
it advances or gains a line, and then it is canonical). A note **under test** keeps the strict rule: a line
that names a usable key and does not verify refuses the note, because that is a note somebody tampered with,
and a refusal never moves the state.

**The one way left to make a held state unreadable, and what to do.** No sequence made of a
publisher that serves some of the genuine lines, a party without a key that drops, reorders or adds
lines, and changes of the trust file in which the keys stay in it (added, retired, revoked, limited,
a window changed) ends in a run that is refused when the same sequence with the genuine notes served whole
would not be: `TestKeylessAdversaryNeverChangesTheOutcome` (150 sequences of 8 runs with 4 keys and 25 with 16 keys, in two worlds side by
side) and `FuzzKeylessAdversary` test exactly that. The one change that can is **deleting from the trust file
every key whose line the state holds**: the auditor then says that they no longer pin any key that vouched
for the state, no run can check the state against a key, and `checkpoint --state` and `consistency --old` end
with status 2, say which it is, and give the one command that restores the protection (`checkpoint --state
NEWFILE --min-size N`, the rebase of "Checkpoints you hold"), which works (`TestRefusedStateNamesTheRebaseAndTheRebaseWorks`).
To retire a key use `not_after`, `max_tree_size` or `revoked`, which keep every state readable; deleting a
key is not a retirement. A run that meets a state whose every line is of a key the file no longer holds says
so (status 2): it names the key ids of the note, says that removing a key did it and that a key is marked
revoked and never deleted, and gives the rebase (`TestLineOfAKeyThatLeftTheFileMakesNothingUsable`). What a record would have to do to survive a deletion (be believed without a
signature) is a decision that was not taken: whoever can write your state file can delete it or put an older
genuine checkpoint in it, but could also put any size in a record that nothing checks, and the signature is
what makes the record more than that.

`TestStateSurvivesEveryChangeOfTheTrustFile` (random sequences of trust files, of signers and of junk
lines), `FuzzStateFromNote` and the two tests of the keyless party above test the invariant: a run that ends
with status 0 leaves a state its own trust file reads back, a run that does not leaves the state as it was,
and a later trust file that holds a key that signed the state reads it.

**What a state file must be, and what the verifier never writes.** The invariant is that the
verifier never writes a state it would not read back, and every limit on what is read is a limit
on what is written, by the same code. A note is bounded where it is parsed (see "Limits"): at most
64 KiB, 32 lines of text, 32 signatures and 1,024 bytes a line, and at most 16 extension lines
in a checkpoint, so a signed checkpoint that exceeds them (a validly signed note with an
extension line of 700,000 characters made a state of 4.2 MB that no later run could read) is
refused **before anything is stored**, as a failed `parse` (status 1), with the state and the
folder untouched. The record built from a note is at most twice its size plus a few hundred bytes
(`<`, `>` and `&` are not rewritten), and the limit on reading a state is that size plus 16 KiB
(144 KiB), the same constant as on writing. Before a state is stored it is **read back** through
the functions the next run uses (the same decoder, the same trust file, the same checks of
"Checkpoints you hold") and must give the checkpoint that was verified, in the canonical form
below. Since the canonical form reads back by construction, this is a guard and not the
mechanism (it used to be the mechanism, and a note whose extra lines a later trust file would
refuse was the third dead end of the state file, L10-04 and L10-05): a file that a later run
would refuse is not stored; the run ends with status 2, says `cannot be recorded`, and the
state is unchanged. The decoder of
a state is strict: one object, every member exactly once (`format`, `log`, `size`, `root`,
`verified_at`, `verified_by`, `note`), no unknown member, no text after the record (white space
is allowed), format 1, the right types, a `verified_at` that is a time, at most 32 fingerprints,
and the `log`, `size` and `root` of the record must be those of the note it holds (the note
decides, a record that says otherwise was damaged or edited). Anything else is status 2 and the
file is never changed or removed. **An empty state file is not "no state"**: a state that lost
its content (a disk that kept the name and not the data after a power loss) must not make the next
run record a first checkpoint without any check against a rollback. It is status 2 and says
so; if you made the file yourself with `touch` it holds nothing to lose (remove it or give
another name); if it ever held a state, restore your copy, or start a new record with
`checkpoint --state NEWFILE --min-size N`.

**Where the state may be.** The state is your protection against a rollback, so it must be in a
directory that only **you** can write and that you own. Another local account that can change
anything the state is reached through can swap the whole record for an older one, or point the name
at an empty directory, and a rollback then ends with VERIFIED and status 0 (L10-06 did exactly that
through a shared directory above a private one). So the check covers everything such an account
could redirect, as OpenSSH does for the files it reads.

On Linux and macOS (`internal/statefile`, `CheckPrivate`), for the path as typed and as it resolves,
walked component by component from the root, links followed:

- every directory the path passes through, the root included, is owned by root or by the user
  running this command and is not writable by group or others, **unless it has the sticky bit**
  (`/tmp`: an entry that is not another user's cannot be renamed or removed by them);
- the directory that holds the state (and every directory that holds a link the path goes through)
  is stricter: owned by the user running this command, and not writable by group or others even with
  the sticky bit, because whoever can create a name there can plant the lock or the evidence;
- every link on the way is owned by root or by the user running this command, and its target is
  checked the same way (at most 40 links); a `..` after a link means the parent of the target, in a relative name too (L12-06);
- the state itself, when it exists, is owned by the user running this command and not writable by
  group or others;
- the lock file is owned by the user running this command, with mode 0600; one of another owner is
  refused (status 2, with the command to remove it by hand) and is never removed by the program; one
  with access for group or others (a file of mode 0666 or 0640 kept from a time the directory was shared)
  is locked as it is and its mode is made 0600 in place; it is never replaced (L12-02, "The rule of the
  lock file"). A directory of mode 0711 is accepted: others can look a name up
  and nothing more, the state is 0600 and holds nothing that is secret;
- after the lock is taken the name is resolved and checked again, and the run stops if it leads
  somewhere else.

When any of that fails, `checkpoint --state` is **refused** with status 2, naming the component, and
nothing is created; `--allow-shared-state-dir` accepts it, with a note in the output, and keeps
meaning exactly that: the path is used as it is (only the second look at the name remains). What is not
checked: access control lists and extended attributes (only the owner and the permission bits are read),
root, a file system that does not keep owners and modes (a network or FAT volume mounted with fixed
permissions), and a change between the check and the open by somebody the check already trusts. A
directory that nobody but its owner and root can write cannot change under a run, which is why this is a
check and not a race; the file is opened by its path, not by a directory handle, which the standard library
does not offer on every system.

On Windows **none of this is checked**: the right to change a folder is an access control list, and
reading one needs a library outside the standard library, which this program does not use. Keep the
state in a folder that only you and the administrators can write, **and in none of whose parents others
can create or rename entries**; a junction or symbolic link on the way is followed, as on Unix, but its owner is
not looked at. A new state file, its lock file and the files of evidence have mode 0600 on Unix (a
replaced state keeps its mode). The lock file and the evidence
are opened **without following a link** and must be regular files (`internal/safefile`, `O_NOFOLLOW`
on Unix, the reparse point flag on Windows): a link at the name of the lock is status 2 and nothing
is created where it points.

**What the lock does and does not protect.** The lock is `.kingres-state.lock` in the directory of
the state (not named after the file), taken with `flock` or `LockFileEx` for the whole read, check,
proof and replace. It excludes every other run of this program that uses a state in that
directory, whatever name it gives the file (a hard link in the same directory, two spellings of
one name on a system that ignores case): the second waits up to 10 seconds and then ends with
status 2. It does **not** cover two names of one file in different directories (a hard link from
another directory: a run replaces the name it was given, which does not change the other link),
a program that does not take it, or a user who can write the directory (see above): use one name
for a state.

**The rule of the lock file: it is never removed and never replaced while it can be held** (L12-02).
A run that replaced a lock file that was "not private" (remove, then create) could hold a different file
from the one another run held, two runs then held "the" lock together, and a late writer moved the state
backwards (four goroutines on a lock file of mode 0644 did it in the second round; processes do it as well).
The lock file is now opened as it is and locked first; then, on Unix, when its mode gives group or others any
access, the mode is made 0600 in place with `fchmod` on the descriptor that holds the lock (a file of mode
0640 is tightened as 0644 is); a lock file that another user owns is **refused** with status 2 and the
command to remove it by hand (it is not removed by the program, which cannot know that nobody holds it); and
after the lock is taken the name is looked at again, and when it no longer leads to the file that was locked
(somebody removed the file by hand) the lock is dropped and the file the name leads to now is locked. Whoever
keeps an old lock file open therefore holds the very file the auditor locks: the auditor waits up to 10
seconds for them (status 2, nothing changed) and does not walk around them. A lock left by a process that died
is released by the system. A lock held by a live process of your own that never ends is status 2 after 10
seconds, with nothing changed: look for that process.

**A write never moves the state over one it did not read** (L12-02). Independently of the lock, the file is
read again immediately before the rename and the run writes nothing, with status 2 and a message that says so,
when it is not the bytes the decision was made on (`statefile.ReplaceIfUnchanged`): a late writer, one that does
not take the lock or holds a lock on another file, cannot replace a newer state with one decided on an older.
What is left is the time between that read and the rename, for a writer that ignores the lock; no call of the
file system closes it without a lock. Since L14-04 the new state is written and flushed in a temporary file
first (`statefile.Prepare`) and the state is read again only then, immediately before the rename
(`Commit`); before, the read came first and the window was the whole write of the temporary file, which this
README called "a few microseconds". The window is measured, not argued: a second writer that ignores the lock
(temporary file and rename, no flush) starts at a random time between 0 and twice the duration of the call, and a
run is lost when the call returned nil and the rival's bytes are gone. Linux (WSL2, ext4, a call takes about
40 ms), 1,200 attempts for each version: before 265 of 1,200 lost, after 1 of 1,200 lost, 169 of 1,200 refused
because the rival was seen. Windows 11 (NTFS, a call takes about 9 ms), 3,000 attempts: before 1,346 of 3,000
lost, after 189 of 3,000 lost, 1,084 of 3,000 refused. These are counts, from one machine, one file system for
each system (WSL2 ext4, NTFS) and one model of the rival (the writer described above, which does not flush and
starts uniformly in 0 to twice the call); they are not rates of anything and no rate is claimed. One of 1,200 is
one event: it shows that the window is not closed, and an interval for it is wide (about 0.002 to 0.5 percent at 95
percent, by the usual interval for one event in 1,200), so "after 1 of 1,200" must not be read as "0.08 percent"
of what a real second writer does. The Windows count is high because the rename asks for a
write through to disk (`MOVEFILE_WRITE_THROUGH`, the only way to ask for the flush of the directory there) and
the rename call takes about as long as the window: on Windows the window is the rename, milliseconds and not
microseconds, and a state that matters must not be written by a program that ignores the lock.
`TestMeasureTheWindowOfAWriterThatIgnoresTheLock` repeats it
(`KINGRES_WINDOW_ATTEMPTS=N go test -run Measure -v ./internal/statefile`, a few minutes for a few thousand).
What `MoveFileEx` answers was measured on Windows 11 (`TestRenameErrorCodes`; its documentation lists no error
codes, and the documentation of `ReplaceFile` names others that this call does not return): a target with the
read-only attribute and a directory at the name both give `ERROR_ACCESS_DENIED` (5) for ever; a target that
another program holds open gives 5 as well, whether that program shares delete or not (L18-03), and ends when the program lets go; a source
that is open exclusively gives `ERROR_SHARING_VIOLATION` (32). Code 5 alone cannot tell a handle from a refusal
that lasts, so what the file itself shows is looked at first (L16-06): a read-only attribute or a directory at the
name is named at once, with status 2 and no wait (`the file is read-only (the read-only attribute is set), so it
cannot be replaced: clear the attribute or name another file; nothing was changed`, and for a directory `the name is a
directory, so a file cannot be put there: move or remove the directory, or name another file; nothing was changed`,
`TestRenameOfAReadOnlyStateIsNamedAtOnce`, which sets a wait of 5 seconds that must not be spent). What is left of
codes 5, 32 and 33 (`ERROR_LOCK_VIOLATION`), which is what another program's open handle gives (a backup, a
scanner, a viewer), is tried again every 20 ms for 3 seconds (L14-05), and then ends the run with status 2 and
the message `another program may have the file open (a backup, a virus scanner, an indexer, a viewer) ...: close it
and run again, nothing was changed`, after the text the system gave. The words claim nothing about how the other
program opened the file: any open handle of another program gives the same code 5, whether it shares delete or not.
So code 5 is the same for an open handle of any kind and for a refusal that the file does not show (an access control
list that denies the rename), which also waits the 3 seconds and is then worded as an open file: the one case in
which that wording can be wrong, and the system's text comes first in the message. The state is unchanged in every case
(`TestRenameWaitsForAProgramThatHoldsTheStateAndGivesUp`, `TestRenameSucceedsWhenTheProgramLetsGoInTime`). The earlier statement that the state "is read again under the
lock" was true and was not enough: the read happened at the start, and the write at the end, with nothing
between them to see another writer. `TestStateAcrossRealProcessesNeverMovesBackwards` runs eight processes at
once against a lock file of mode 0644, eight rounds, with a watcher on Unix, and `TestLockExcludesRealProcesses`
and `TestLockExcludesRealProcessesOnALooseLockFile` run four processes on the lock itself, on Linux and on
Windows.

**The evidence of a fork.** When `--state` finds a fork or a rollback the conflicting note is saved
beside the state as `<state>.conflict-<size>-<the whole root in 64 hex>` (the state's name is
shortened above 100 bytes), created exclusively and never through a link. What is saved is the
note in its canonical form ("What the state holds"), so the same signed note served with any number of
other signature lines is the same bytes. The identity of the evidence is the signed text, as for the
state: when a file is already at that name and holds the **same text**, it is the evidence already saved
(an earlier run saved it) and the lines of both notes are merged into it, atomically, when the note
served now has a genuine line that the file lacks (the note says "with fewer signature lines: the file now
holds N signature line(s)" and what they are, see "Lines of one text only accumulate"); so every subset of the genuine lines of a checkpoint is one file, and a publisher
without a key cannot use up the names (15 subsets of 4 keys gave 8 files and 7 times status 4 before:
L12-03). **Another text is never called saved**: a file with other content (another note, a file somebody
put there, a write that was cut short) takes the next name (`-2`, `-3` and so on, 8 in all), which only a
text that was really signed with the same size and root can reach (extension lines differ). The number of
evidence files for a state that counts is bounded by the history of the log: one for each checkpoint that the log ever
signed and that is served as a rollback. A party without a key chooses which
of those old checkpoints to replay, and so how many of the files the auditor's runs make, up to that bound; it cannot
make a file that is not the signed text of a checkpoint of the log. Each file is a note of a few hundred bytes, and
the runs are the auditor's own. The program does not count them and sets no limit, because a limit would be a way to
force status 4 (a state that does not count has two files at most, see below). If
none can be used or the disk refuses, the output says `was NOT saved`, `--json` has
`info.state.evidence_saved` false and `conflict_file` null, the finding is printed as for a fork,
and the status is **4**, which is not 0 and is not the 1 of the fork itself: act on the fork and
save the note you were shown yourself. **The note is in the output** (L10-09): a note line with the
base64 of its exact bytes (decode it into a file), and `info.state.note_base64` in `--json`. A failed flush of the
directory after the file was linked is not a failed save (L16-05): the file is in place, and the output says `was
saved to ..., but the directory could not be flushed: a power loss may take the file back`, with `evidence_saved`
true, `evidence_flushed` false, the file and the note (printed, since the file is not known to be durable); the
status stays 4. The state is the same: when the rename went through and the flush of the directory then failed,
the run says `was replaced (it holds size N now), but the directory could not be flushed: a power loss may bring
the old file back, and the next run then goes on from that one`, with status 2 as before and `directory_flushed`
false in `info.state`, and not `could not be updated, so later runs do not know it`, which would be false
(`TestFailedDirectoryFlushAfterTheStateWasReplacedIsReportedAsThat`, `TestFailedDirectoryFlushAfterTheEvidenceWasLinkedIsReportedAsThat`,
`TestFailedDirectoryFlushAfterAWeakEvidenceFileIsReportedAsThat`, `TestFailedFlushOfTheDirectoryAfterTheRenameIsNotAFailedWrite`,
`TestFailedFlushOfTheDirectoryAfterTheLinkIsNotAFailedSave`).

A new evidence file is written and flushed to a temporary file in the same directory and then **linked** to its
name (`link`, which fails when the name exists and never follows a link at the name), so that a stop between
the create and the write cannot leave an empty file that takes one of the eight names (L14-09,
`TestEvidenceFileIsNeverEmptyAtItsName`); a stop leaves a temporary file, which the next run removes. The flush of
the temporary file before the link is held by `TestEvidenceTemporaryFileIsFlushedBeforeItIsLinked` (the order of
the calls) and `TestEvidenceIsNotLinkedWhenItCannotBeFlushed` (a flush that fails ends the save before the link). On a file
system that cannot link the older exclusive create is used (`TestEvidenceFileWhereTheFileSystemCannotLink`).

**Lines of one text only accumulate** (L14-03). When an evidence file or a state is rewritten with more lines
of the same text, the lines it holds are kept as they are, also the line of a key that has left the trust file
since (it was verified when it was stored and is bound to the text, and it cannot be verified now), and only the
new lines are verified. The output says what the file holds: `N signature line(s), M verify under a key of the
trust file and K are of keys the trust file does not hold: kept as stored, not verified now, and they make
nothing usable`. Such a line decides nothing: a note is usable when its text verifies under any key of the
trust file, and a line of a key the file does not hold is not one of those lines, so a state with no other line
is refused as before (status 2). Lines of keys the file holds that do not verify are dropped, as before. Under
the 32 lines a note may carry, the lines that do not verify are left out first
(`TestEvidenceKeepsTheLineOfAKeyThatLeftTheTrustFile`, `TestStateKeepsTheLineOfAKeyThatLeftTheTrustFile`,
`TestMergeStoredKeepsWhatIsStoredAndVerifiesWhatIsNew`). A state written by an earlier version holds the note as it
was served, and the lines of keys the file does not hold that it carries are kept with it.

**A conflict with a state that rests on keys that no longer count** (L14-02). When the held checkpoint rests
only on keys the trust file no longer counts (revoked, or beyond their `max_tree_size`: "Checkpoints you
hold"), a fork, a rollback or a failed extension is not an accusation of the log: a thief who signed a forked
log while its key counted leaves exactly such a state, and then meets the honest log. The failure line says
so: that the state rests only on the named keys and why, that it may have been signed by a stolen key, that the
conflict may come from the state and not from the log, and the rebase (`checkpoint --state NEWFILE --min-size N`,
with the size the state holds named and said to be unauthenticated). The sentence "both are signed by a key of
the trust file" is printed only for a state that counts. `consistency --old STATE` words it the same way.
The status is **1**: a verification failed (the held checkpoint is used as the anchor, as decided, and what is
served does not agree with it). It is not 2, which says that the held checkpoint cannot serve as the anchor and
is not the case, and not 4, which is a finding about the log whose evidence was lost, which this is not
established to be. The evidence is **two files at most for the held state**, one for a rollback and one for a
fork (L16-01): `<state>.conflict-weak-rollback-<held size>-<held root in hex>` holds the first conflicting note
served that is smaller than the held one, and `<state>.conflict-weak-fork-<held size>-<held root in hex>` the first
one with the same size and another root, or a larger size that the proof does not connect. (Before, one file
served both, and a replay of an old genuine checkpoint, which a party without a key can always serve once, took
its place and kept a later fork out.) The names depend on the held state and on the kind and on nothing the log
serves, so any number of runs, sizes and notes make two files at most, never one per run or per size. A later
conflict of a kind that has its file is either the same checkpoint (the same signed text: the file is extended
with its new genuine lines, as for a state that counts) or another one. Another one is **not saved and is printed
in full**: a note line with the base64 of its exact bytes and `info.state.note_base64` in `--json`, with
`evidence_saved` false and `conflict_file` null, as the evidence of a state that counts is when it could not be
saved. That holds whatever the reason a note is not saved by this run (the file of its kind holds another
checkpoint, a file or a directory was put at the name, the disk refuses), so the auditor never learns that a
note exists only in the source of the run; the status stays 1. A file at a name that is not a signed note of the
same text is never taken for evidence and never overwritten. Both kinds are kept and not dropped because a log
that really forked after a key was rotated looks the same. What a party without a key can still do is decide
which conflicting checkpoint of a kind is the one saved (the first it serves); it cannot hide another one, which
is in the output of the run that met it (that output is lost only when the caller discards it on purpose with
`KINGRES_VERIFY_NO_OUTPUT=1`, as documented).
In the JSON, `info.state.anchor_weak` is true or false on every exit after the held state was judged (L16-02):
the conflicts, a missing or a wrong tile, a read-back that failed, a state that changed under the run, a failed
write and a success; a run that stops before it has read the state (the lock was not taken, the file is not a
state, not a checkpoint of this log) has judged nothing and has no such member. When no state existed it is false.
`consistency --old STATE` has `info.old_anchor_weak` and `info.old_anchor_weak_keys` on every exit after `--old` was
judged (a plain note has `old_anchor_weak` false, and a size and root given by hand, which are not a held checkpoint, have none). When the run itself writes the state (it extends it, or adds a
genuine line of a key that counts), the member says what the state is **after** the write (L16-03): `anchor_weak`
false, `anchor_weak_repaired` true, no `anchor_weak_keys`, a note that says so, and no non-proof about the
checkpoint the state held, because the run proved it a prefix of a checkpoint signed by a key that counts (or merged
that key's line into it). `anchor_weak_keys` and the non-proof are for a state that is left as it was.
Which lines a state holds is what the publisher served on the run that made it, so a publisher without a key
decides whether a state is of this kind; what it changes is the wording, the JSON members above and the evidence
files of a later conflict (two at most, one for each kind, and not one for each signed text), and neither the status
nor the size nor the held text. `TestKeylessAdversaryNeverChangesTheOutcome` compares those at every step (its
random worlds meet the case in few steps and the test counts them), and
`TestKeylessAdversaryDecidesOnlyTheWordingWhenTheWorldsDifferInWhetherTheStateIsWeak` makes it happen every time:
in world A the state holds the lines of two keys, in world B the adversary served the line of one, and that key is
revoked. Tests: `TestThiefStateMeetsTheHonestLogAndTheOutputSaysWhoMayBeWrong`,
`TestThiefStateInTheJSON`, `TestThiefStateKeepsTwoEvidenceFilesWhateverTheNumberOfRuns`, `TestThiefStateRebaseWorks`,
`TestStateThatCountsKeepsTheOldWordsAndEvidence`, `TestConsistencyAgainstAThiefStateSaysWhoMayBeWrong`,
`TestWeakAnchorIsInTheJSONOfASuccess`, `TestWeakConflictKeepsOneFileForEachKindOfConflict`,
`TestWeakReplayOfAnOldCheckpointDoesNotTakeThePlaceOfAFork`, `TestWeakConflictThatIsNotSavedIsPrintedInFull`,
`TestWeakEvidenceStaysAtTwoFilesWhateverIsServed`, `TestAnchorWeakIsOnEveryExitAfterTheStateWasJudged`,
`TestWeakStateRepairedByTheRunIsReportedAsItIsAfterTheWrite`.

**A key rotation makes a state weak without anyone's act** (L16-10). A publisher without a key decides which
genuine lines a state holds, but a rotation does the same with no publisher at all: the state was made while only
the old key signed (the note had that one line); the auditor then adds the new key to the trust file and marks
the old one revoked. From then on that state rests only on a key that is revoked, so it is weak, and every
conflict against it is worded as for a thief's state ("may come from your state and not from the log", with the
rebase, which adopts the log as served) and its evidence is the two files above, with the note printed when it is not
saved. That is the cost of the decision that a held checkpoint is judged by its text and stays the anchor, and
not a defect; the wording is the soft one on purpose, because the same state would be a thief's. **Before
revoking the old key, run once** `checkpoint --state STATE` with a trust file that holds both keys, while the
publisher serves a checkpoint signed by the new key: the state then gains the new key's line (same checkpoint,
`signatures_added`) or advances to a checkpoint it signed (`advanced`), rests on a key that counts, and the
revocation of the old key does not weaken it. A state that is already weak is repaired the same way by the first
run that meets its checkpoint, or one that extends it, signed by the new key (`anchor_weak_repaired`); a conflict
met before that is the weak conflict above.

**Checkpoints you hold** (`--state`, `--old FILE`; design corrections 17 and 60). A checkpoint you kept is
a statement about the past, so it is judged differently from a checkpoint under test, and the same
way in both commands for a state file:

- at least one signature line must verify against a key of the trust file, **whatever the standing of
  that key now** (a line that names a key of the file and does not verify is ignored, and the output says
  how many: the note is judged by the signatures that verify, "What the state holds");
- the text decides, not the lines that were served with it. A state whose verifying keys are **all revoked
  or beyond their `max_tree_size`** is still your anchor, in `checkpoint --state` and in
  `consistency --old STATE`: the output says `rests only on keys that the trust file no longer counts`, names
  each key and why, and `consistency --old STATE` lists as not proved that the checkpoint it held was genuine
  (`checkpoint --state` does not when it succeeds: it has proved the held checkpoint a prefix of one signed by a key
  that counts, or merged that key's line into it, and reports the state as it is after the write); a smaller size is a
  rollback and a root that does not extend it is a fork as ever, and the first checkpoint that extends it and
  is signed by a key that counts replaces it. This is the answer to the question of what a revocation does to a
  state you already hold: it does not make it unknown. (Before this round it was refused, which let a
  publisher decide it: a note signed by two keys that was served with the line of one of them left a state
  that held only that key, and after that key was revoked every run ended with status 2, where the whole
  note would have left a state that worked: L12-01.) A plain note given as `--old FILE` is not a record of
  what you verified when the key was good, so it keeps the strict reading: a revoked key, or a size beyond a
  key's limit, makes it status 2;
- the **validity window is not applied** (a note signed by a key since retired by `not_after`, or not yet
  valid, stays usable, and a note says so). Retiring a key by `not_after` therefore does not make you discard
  your state.

Anything else (not signed by any key of the file, which includes a key that was deleted from the file,
malformed, another log) is **status 2** in both commands, never 1: the thing you hold cannot serve as your
anchor. The message does **not** say to delete the state (with no state a rollback is no longer noticed).
It says to keep the file, obtain a checkpoint of the log signed by a key you trust from a source you trust,
and run `checkpoint --state NEWFILE --min-size N` with N the size you believe the log had (the old note says
what it claims, which nothing authenticates), and to use NEWFILE from then on. That command is the rebase:
it records the first checkpoint of NEWFILE without a comparison, refuses one smaller than N, and from then on
NEWFILE catches a rollback. What trusting the record costs: whoever can write your state file can also delete
it or put an older genuine checkpoint in it, which weakens the protection the same way. A key that leaked
should be marked `revoked`; `not_after` is only as good as the clock.

**`consistency`** checks that the log at the new checkpoint (`--new`, default
the checkpoint in `--source`) extends the log at the old one (`--old`: a note
file or a state file, verified as "Checkpoints you hold" says, or `SIZE:ROOT` with the root
in base64, taken as you give it and printed as not authenticated). It writes nothing. It reads the
proof hashes from the tiles under
`<source>/c/<id>/e<N>/tile/...` (full and partial tiles; a partial tile that
is missing is read from the full tile), and verifies them against both roots with
RFC 9162 section 2.1.4.2. **Only the tiles on the proof path are read** (the
output says how many): a tile that is damaged but not on that path is not noticed,
and the output lists it as not proved. When the sizes are equal, or the old size is 0,
the proof is empty and no tile is read; the output says "no tile was needed". A new
size smaller than the old one fails (the log went back). A tile that is absent gives
status 3; a tile that is present and wrong gives status 1.

**Sources and files.** A source is a directory (a tar file and an HTTP source are
not built yet). Every file the verifier reads goes through one helper
(`internal/safefile`): it opens without blocking (a named pipe cannot hang it),
requires a regular file after the open, and bounds the size before and while
reading. In a source directory **no symbolic link or Windows reparse point is
followed, in any component of a path below the directory**, and a path cannot leave
the directory (`os.Root`, Go 1.24 and later; the module needs Go 1.27.3 and the
packaging build uses it with `GOTOOLCHAIN=local`). Links were refused instead of
resolved inside the directory because a publication written by the publisher's
tools has no link, so a link is an accident or an attempt to alias one tile as
another or to reach outside, and a rule that allowed some would have to explain
which. A directory you pointed `--source` at may itself be a link. A file you
name on the command line (`--trust`, `--note`, `--old`, `--new`, `--state`, the
files of the older commands) may be a link, since you chose it, but must be a
regular file after resolution, never a pipe, device, socket or directory. On
Windows the device names (`NUL`, `CON`, `COM1`, ...), the device namespace
(`\\.\`) and alternate data streams are refused by name. A directory of digests
or a directory of epochs is read at most 1,024 entries.

**What is status 2, 3 or 1** (one rule for every command, tested site by site in
`TestEveryExitSiteHasARowAndAScenario` and, for `checkpoint` and `consistency`, in
`TestExitStatusFollowsTheRule`):

| Status | When | Examples |
|---|---|---|
| 2 | what **you** named or hold cannot be used | a missing or malformed `--trust`, `--source`, `--note`, `--old`, `--state`; a held checkpoint no key of the trust file can vouch for (also a bad signature or another log); no key usable at `--now`; `--now` empty or malformed; two epochs of the chain in the source and no `:e<N>` in `--log` (you must name one); the lock of the state; a file of the publication that exists but that your account cannot read (permission denied: it is your copy, so it is not the publisher's absence) |
| 3 | what the publication **should hold and does not** | no checkpoint file, no tile, a chain directory that holds no epoch directory (with or without `:e<N>`), no chain directory |
| 1 | what the publication holds in a form it must not have, or the thing **under test** fails | a link, a pipe, a directory, a file over its limit, a tile of the wrong size; `--note`, `--new` or the checkpoint of the source that is malformed, forged, of another log or beyond a key's limit; a wrong root; a fork or a rollback with `--state` (4 when its evidence could not be saved) |

A held checkpoint is status 2 and the thing under test status 1 because the first is your anchor
(a wrong one is your problem) and the second is the claim being checked.

Formats, with the specification they follow:

- **Signed note** (signed-note, "Format"): UTF-8 without control characters
  except newline; the text ends in a newline; the last blank line separates it
  from one or more lines `U+2014 SP name SP base64(4-byte key id || signature)`.
  Key names are non-empty without Unicode spaces or `+`. Base64 must be
  canonical (RFC 4648 3.5), which is checked by re-encoding. Two lines with the
  same name and key id are rejected (tlog-checkpoint). At most 32 signatures,
  32 lines of text, 1,024 bytes a line and 64 KiB in all. Key id = `SHA-256(name || 0x0A || 0x01 || public key)[:4]`.
  A line whose name and key id match no trusted key is ignored; a line that
  matches one must verify, or the whole note is rejected (the specification
  says SHOULD; for a note you hold the line is ignored instead, see "What the state holds");
  no verified line means rejection.
- **Checkpoint** (tlog-checkpoint): at least three non-empty lines, origin,
  size as a canonical decimal below 2^62, root as canonical base64 of 32 bytes;
  further lines are extension lines (at most 16), kept and reported but without meaning here.
  The origin follows tlog-cosignature: at most 255 bytes, no spaces, `+` or
  control characters. A size 0 checkpoint must carry the hash of the empty
  string. The key name of a log's signature is its origin
  (`<prefix>/c/<chain_id>/e<epoch>`, `<prefix>/a/<aggregate_id>`).
- **Tiles** (tlog-tiles): height 8, hashes of 32 bytes, a full tile is 8192
  bytes; path `tile/<L>/<N>[.p/<W>]` with L from 0 to 63 and N in three digit
  groups, every group but the last prefixed `x` (`x001/x234/067`); a partial
  tile has 1 to 255 hashes; the width at level l for size s is
  `floor(s / 256^l) mod 256`. Paths are parsed canonically (no leading zeros,
  no `..`); a tile of any other length is rejected. Entry bundles
  (`tile/entries/<N>`, big-endian `u16` length prefixes) can be parsed and are
  not used yet.
- **Trust file** (design 7.2): strict JSON; `format` 1, `origin_prefix`, `keys`
  (`name` label, `public_key` base64, `fingerprint` that must equal the
  SHA-256 of the key, `not_before`, `not_after` as RFC 3339 UTC or null, `revoked` true or
  false, `max_tree_size` an integer from 1 or null; at least one key is required),
  optional `tsa_roots` (PEM certificates, validated), `client_keys` (`key_id`
  must equal the first 16 bytes of the SHA-256 of the key), and `witnesses`,
  which must be empty. Unknown members are rejected.

Not built yet (later steps of the design): `anchors`, `release verify`. The text of this section is the contract of step 1; where
the design and the specification differ, the specification was followed (see
"Findings about the phase 7 design").

### Phase 7, step 7 and step 18: `aggregate`, `proof --bundle`, `bundle`, `monitor`, `trust follow`, `attestation`

These commands read a **publication** (the layout of `ledger-phase7.md` 4.6) from `--source`, which is a
directory, a `.tar.gz` file or an `https://` address (plain `http://` only for `127.0.0.1`, `::1` and `localhost`).
A directory is read through `os.Root` with no link followed. An archive is read once into memory under limits
(`MaxArchiveFile` 1 GiB, 200,000 entries, 512 MiB after decompression, 32 MiB a file), a name must be a plain
relative path of at most 16 elements, and a link, device, pipe, repeated name, file that is also a directory, second
gzip member or trailing data is refused. An HTTP source follows **no redirect** (status 2, "point the source at the
address that serves the files"), uses **no proxy** from the environment, refuses an address with a user name or
password, has a timeout on the connection (10 s), the headers (15 s) and the whole request (60 s), and reads at most
the limit of the caller after any gzip decoding. A server that cannot be reached, answers 5xx or 429, or times
out is "could not check": status 3, never 1. A 404 is an absent file.

```
kingres-verify aggregate   --trust T --aggregate-id HEX [--source S] [--previous NOTEFILE] [--chains-from S] [--accept-epoch-closed SEQ ...] [--now TIME] [--json]
kingres-verify proof       --proof P --bundle S --trust T --log-leg chain|aggregate [--epoch N] [--aggregate-id HEX] [--accept-epoch-closed SEQ ...] [--table-id N --row-hash HEX --op I|D|any]
kingres-verify bundle verify --trust T BUNDLE [--require-anchors N] [--require-attestation] [--attestation-table-id N]
                           [--accept-epoch-closed SEQ ...] [--worm-url [ID=]URL ...] [--btc-header HEIGHT:HEX80 ...]
kingres-verify bundle fetch  --source URL --out DIR [--log LOG ...] [--trust T] [--since-size N] [--max-files N] [--max-bytes N]
kingres-verify monitor     --trust T --source S --state DIR [--log LOG ...] [--init] [--once] [--interval D] [--polls N]
kingres-verify trust follow --trust T --source S --out FILE
kingres-verify attestation --trust T --proof ROW --attestation ATT --attestation-columns FILE --attestation-table-id N
                           (--digest D | --bundle S --log-leg chain|aggregate) [--client-key HEX]
                           [--revocations DIR --revocation-table-id N]
```

**Key events: what every command does with them (V8-01, V8-02, V8-11).** Every command that judges a signature of a publication
(`checkpoint`, `consistency`, `aggregate`, `proof --bundle`, `bundle verify`, `bundle fetch --trust`, `monitor`, `attestation --bundle`)
reads the `events/` of its source first, verifies them under the keys of **your** trust file (the file as you pinned it), and narrows
that file by what they say, with one rule: a key that a verified `key-revoked` event revoked vouches for **nothing**, whatever the size
or the date of the checkpoint (an anchor could date a checkpoint before the revocation, see "Anchors"; until the design states that rule
precisely this version does not apply it, and `revoked` stays revoked), and a key that a verified `key-retired` event retired vouches only for
checkpoints up to the `max-tree-size` of the event (the smaller of that and the file's own limit; `unbounded` changes nothing and is said).
A checkpoint that only such a key signed is not verified (status 1, the note says which event); the output says which keys vouched
(`verified by key ...`). A key that an event **adds** is not used by these commands: trust moves only through `trust follow`, whose output
file you pin yourself, and the note says so. The events count only when an authorised key signed them (correction 165); an event that
no authorised key signed is **read and not verified**: it changes nothing and every command lists it in its notes (the first 20), and
`bundle verify` ends with status 3 because part of what the bundle holds could not be verified (it never says "verified" for it).
Events are numbered from 0 without a gap: with a source that can list (a directory, a `.tar.gz`) a file with a number above the first
missing one is a failure (status 1, step `events`, or `publication` in `checkpoint` and `consistency`: one was withheld or removed, and what follows could not be applied); an HTTP source
cannot list, so a withheld event cannot be told from the end, as this README has always said. Events that are present but wrong end the
command with status 1 (the same steps), an unreachable source with status 3. A publication without `events/` changes nothing. What
the commands do **not** do: they do not apply a key-added event.

**`aggregate`** verifies the checkpoint of the aggregate log, its **last leaf** as entry `size-1` of the signed tree
(RFC 9162 2.1.3.2, from the tiles), parses it strictly (tag 0x23, format 1, `seq` equal to the index, 1 to 512 members
sorted by (chain_id, epoch) with no repeat, flags only bits 0 to 2, epoch from 1, height below 2^62), and checks
**every member against its chain log**: the signed chain checkpoint, the root the member names for size `height+1` as a
prefix of that log (RFC 9162 2.1.4.2 from the tiles), and the block as the leaf at that height (87 bytes, tag 0x13,
`SHA-256(leaf) = block_hash`). Failures: `member-root`, `member-block`, `member` (a chain checkpoint smaller than the
member), `aggregate-leaf`. The **flags** of a member (bit 0 `stale`, bit 1 `retired`, bit 2 `diverged`; any other bit is refused) are judged **together**,
by one table, and every row of it is a test (`TestV804TheFlagsOfAMemberAreJudgedTogether`, for `aggregate` and for `bundle verify`).
`diverged` is a failure whatever else is set and whatever events are served (the publisher writes 0x06 for a diverged chain that it
retired, correction 176: it must not turn into "could not check" because the retirement event is absent); `stale` is never dropped
(status 3 even when a retirement event confirms the head); a retired member needs its event, and a retired chain whose own signed
log goes past the retirement height is a failure (the same rule as `monitor`, `retired-continues`).

| Flags | Events | Status | Step |
|---|---|---|---|
| 0x00 | (none needed) | 0 | |
| 0x01 stale | any | 3 | |
| 0x02 retired | a `chain-retired` event, authorised, same height and hash, chain log not longer than height+1 | 0 | |
| 0x02 | no event | 3 | |
| 0x02 | an event that no authorised key signed | 1 | `chain-retired` |
| 0x02 | an authorised event with another head | 1 | `chain-retired` |
| 0x02 | an authorised event, and the signed chain log is longer than height+1 | 1 | `chain-retired` |
| 0x03 stale + retired | authorised and matching | 3 (stale is kept) | |
| 0x03 | none | 3 | |
| 0x03 | unauthorised, another head, or the chain log continues | 1 | `chain-retired` |
| 0x04, 0x05 diverged (alone or with stale) | any | 1 | `diverged` |
| 0x06, 0x07 diverged and retired (with or without stale) | any: none, authorised, unauthorised, a continuing chain | 1 | `diverged` |

A failure wins over an incomplete check: a member that is stale and another that is diverged end with status 1. A member's own
checks (the chain checkpoint, the root, the block) come first and fail with their own step. `proof --log-leg aggregate` applies the
same rule to the member of the chain it proves: a stale member ends with status 3, with the block shown.
 With `--previous` (a checkpoint of this aggregate log you hold, a note or a state file) it also proves the
aggregate log extends it, that the leader's times do not go back, that **no chain vanished without a record** (a
chain of the older round must be in the newer one, flagged retired in the older one, or ended by a verified event),
that **no head went back**, that a **retired chain does not return** in the next round, and that **each chain log
extends the log the older round named** (a consistency proof from the chain's tiles). Only the two leaves named are read;
the rounds between them are covered by the consistency of the aggregate log only, which the output says.
The sentence of design 5.6 (no global snapshot) is in the output.

**`proof --bundle`** runs steps 0 to 3 of the ledger README (binding, entry path, transaction path, block hash; the
proof's `chain` of later headers is ignored), forms the 87-byte leaf data of the block and shows its leaf hash in the signed
chain log of the bundle (step `log-inclusion`). `--log-leg aggregate` shows the block through the member of the signed
aggregate's last leaf (step `aggregate-leg`; a round that does not reach the block is a failure). The last line says which root
of trust ended the chain: *signature of key X*. No anchor is checked in this version, and the line says so.

**`bundle verify`** treats `bundle.json` as an index that is not believed: the prefix must be the trust file's, each log it
names must have the canonical origin and checkpoint path and the size the signed checkpoint says, and the layout is searched for
logs the index does not name, which are verified too. For every log: the checkpoint, the binding of **every tile** to the signed root,
every entry (block headers: heights, `prev_hash` links from the genesis hash, times; aggregate leaves: sequence, times), and **every
file of the tile directories** (a partial tile or entry bundle of an older size must be the first hashes or entries of the tile or
bundle of the signed tree; a file the tree does not have is a failure). Then each aggregate against its chains, each proof on the chain leg and
on the aggregate leg when an aggregate names the chain, each attestation (needs `--attestation-table-id`), the events (the line says how many were verified under the trust file and how many were not; one that is not is status 3). **Anchors are verified** (step 21, "Epochs and anchors in `bundle verify`" below). A source that cannot list directories (HTTP) is
told so in a note and the older partial tiles are not checked.
Mutation tests flip every byte of every file of a bundle, **the event files included**, one at a time, and none verifies
(`bundle.json` alone is an index that is not believed, and `logs.json` is not read by `bundle verify`: the layout is searched). A log in the layout that the
index does not name is verified too.

**`bundle fetch`** mirrors logs: `keyring.json`, `events/*`, the checkpoint of each log, the tiles and entry bundles of its size. The logs are the
`--log` options (an HTTP source has no listing) or, when there is none, those of the publication's **`logs.json`**, which is read in its format 1,
`{"format":1,"logs":[{"kind","origin","state","size"}]}` (correction 178), strictly (an unknown format, member, kind or state, a duplicate origin or an
origin that is not `<prefix>/c/<64 hex>/e<N>` or `<prefix>/a/<64 hex>` is status 2: nothing is guessed). `logs.json` is an index and nothing in it is
believed: with `--trust`, an origin under another prefix than the pinned one is status 1 (`logs-json`); a log it lists as `diverged` or `closed` is
fetched and the state is said in a note (the checkpoint and the aggregate say what is true). File names are made from numbers only, written below
the output directory through `os.Root`, through a temporary name and a rename. Without `--trust` the output says `FETCHED` and that **nothing was
verified**; with it every checkpoint is verified (under the keys the events of the copy leave valid, see above) **before** anything is written, and
the tiles are bound to the signed root.

*Bounds (V8-03).* A run writes at most `--max-files` files (default 100,000) and `--max-bytes` bytes (default 2 GiB), whoever signed what it
reads, and says so in a note. The size of a checkpoint decides how many tiles and entry bundles are asked for; it is checked against the file ceiling
**before** the first tile is requested, so an unsigned checkpoint that claims a size of 10^12 makes the run stop with status 2 and no file written
(the check comes before the checkpoint note is stored). A run that reaches a ceiling stops with status 2, names the flag to raise, and leaves what it has
written; nothing is cut silently. The ceilings are the user's: raise them for a log that is really that large.

*What the directory already holds (V8-06).* Nothing is replaced without a comparison. A served checkpoint smaller than the held one is status 1
(`rollback`), one of the same size and another root is status 1 (`fork`); in both the held file is kept and the served note is saved as `checkpoint.conflict`
beside it. A larger one is, with `--trust`, **proved** to extend the held one (RFC 9162 2.1.4.2, from the tiles of the copy) after the tiles were bound to
its root; if the proof fails the held checkpoint is put back and the run is status 1 (`fork`); if a tile of the proof is missing (for example with
`--since-size`) the run says so and ends with status 3. Without `--trust` a larger checkpoint replaces the held one and the note says that the extension was
**not** checked. A held checkpoint that does not read as a checkpoint of the log is status 1 and is not replaced. An event whose text is not the held
one, or that the source no longer serves, is status 1 (`events`) and nothing is replaced. A tile that is already here with the size it must have is
**kept without being read** and counted as kept, not as fetched (with `--trust` the binding reads every tile anyway; without it nothing compares the kept
tile, and the note says so). A held checkpoint of size 0 proves nothing (the empty tree is a prefix of every tree) and the note says so.

**`monitor`** keeps `--state DIR`: one file `<log>.state` per log in the form of `checkpoint --state`, written by the same code (the
state moves only to a checkpoint proved to extend it, under the lock, never over bytes the run did not read; a rollback, a fork or a rewrite is
status 1 and the conflicting note is saved beside it, status 4 when it cannot be), and `monitor.json` with how far the entries of each log were
audited, the digest of the text of each event seen and the chains retired. Each poll, per log: the checkpoint and its consistency with the
state, then the entries not audited yet, then **all events**. Findings (status 1): `rollback`, `fork` (also of the aggregate, also across two
polls: the case of the review P8-01), `events` (an event number whose text changed, or that is gone), `retired-continues` (the log of a chain that a verified
chain-retired event ended has more blocks), `retired-reappears` (a member flagged retired is a member of a later round), `entries`, `tiles`.
Status 3: the publication cannot be reached. Without `--once` it polls every `--interval` until `--polls` (default: until stopped) and stops at the first finding.
**The first run says so (V8-07):** a state directory that holds no `monitor.json` is an error (status 2), because what was seen is what makes a rollback
visible and a record that was deleted would otherwise be taken for "nothing seen yet"; `--init` declares a first run, and is refused when the directory
already holds a record or any `<log>.state` file (move them away to forget them on purpose). A record that is cut or emptied (`{"format":1}`, a member
missing or `null`) is refused as incomplete. The key events are applied as in the section above, and an event gap is a finding (`events`).

It sees one view: a split view needs witnesses (design 4.8).

**`trust follow`** reads `events/000000, 000001, ...` and verifies each one under the keys pinned in the file or added by a verified earlier event:
`key-added` must be signed by an authorised key and by the new key and the key must pass the three tests of the extension (correction 155: canonical encoding,
on the curve, not of small order, which every public key the verifier reads must pass: trust file, client keys, event keys); `key-retired` and
`key-revoked` by an authorised key other than the one they are about. It writes a new file (`--out`, which must not exist and is never the file read): added keys with
the event's `not_before`, retired keys limited to `max_tree_size` (an `unbounded` retirement is said), revoked keys `revoked`. An event signed by no
authorised key stops the run (status 1), and so does a gap in the numbers (a file `events/000004` when `000003` is missing). No event at all is status 3. Start from the file you pinned yourself.

**`attestation`** (step 18): verifies the proof of the row's entry and the proof of the attestation row of `kingres.ledger_attestation` (its row
hash recomputed from the columns file `ledger_txid, seq, key_id, nonce, signature`, its table id the one you give) against one anchor, that the attestation
row names the transaction and entry of the row proof and sits in the same block, and the Ed25519 signature over the **136-byte message** under a key **pinned in
`client_keys`** (a key only in the server's registry fails: step `client-key`). Revocation rows (`NAME.proof.json` and `NAME.columns.json` in `--revocations`)
fail the attestation when the key was revoked in a block at or before the attestation's block (the same block counts as after). The result always says
**"no revocation shown in the evidence given"**, and never that none exists: an inclusion proof shows presence, not absence.

*What the signature covers, and what the verifier checks by other means (V8-08).* The 136 bytes are the 34 characters
`kingres-ledger/client-signature/v1`, a zero byte, the **chain id** (32), the **table id** (4), the **operation** (1), the **row hash** (32), the
**key id** (16) and the **nonce** (16). They do **not** cover the transaction id, the sequence number of the entry, the block, any time, or the
database. So the signature is over content and not over a transaction: two inserts of identical content in one chain share one valid signature.
What ties the signature to **this** entry is checked outside the signed bytes, and each check is a step that can fail: the attestation row is
proved in the chain (its row hash recomputed from the columns file), it is an **insert** (`attestation-row`), its own entry is in the transaction of
the entry it attests (`attestation-row`: ledger_attest runs in that transaction), its columns `ledger_txid` and `seq` are the entry's (`attestation-entry`)
and it is in the **same block** as the entry (`attestation-entry`). An operator who can write rows into the attestation table by a route that bypasses
the extension can still show one valid signature for a second identical insert **in the same transaction and block as the entry it names**, if such a row
exists in the chain; what he cannot do is move the signature to an entry in another transaction or block, and a **forged** publication that repeats a
transaction id is stopped by the block check (test `TestV808TheAttestationRowMustBeInTheBlockOfItsEntry`). What this does not prove: that the client saw
anything but the hash the server returned, and that a copy of the database (which shares the chain identity, see the extension's README) is not the
original: only the signed chain log of a publication you were given by someone other than the operator tells them apart.


**Statuses** of the new commands are in the table of this README. **Findings and decisions of step 7 and step 18** are in "Decisions of steps 7 and 18" below.

### `version`, `--help`

`kingres-verify version`, `kingres-verify --help`, `kingres-verify <command> --help`
(all exit with status 0).

## Limits and memory

The files the verifier reads come from a database server, a publisher or a directory somebody
else filled, and a note can be signed by a key of your trust file and still be hostile. What one
input can make the program allocate, store or print is therefore bounded by constants, never by
the shape of the input. **Memory is a constant plus a small multiple of the largest single value
(one string, one streamed element), never a multiple of the input.**

| What | Limit |
|---|---|
| A proof | **no limit on its size**: it is read in pieces and twice (below), so memory does not depend on it |
| A columns file | 16 MiB |
| A digest file | 64 KiB each, 1,024 files and 16 MiB together for `digest-chain` |
| A trust file | 1 MiB |
| A signed note (a checkpoint, `--old`, `--new`) | 64 KiB, 32 lines of text, 32 signature lines, 1,024 bytes a line; a checkpoint has at most 16 extension lines |
| A state file | 144 KiB (twice the largest note and 16 KiB for the rest) |
| A block export | **no limit on its size** (it is read line by line, one header is kept), 1 MiB a line |
| A trust file's members | 16 keys, 1,024 client keys, 32 time-stamp roots, 200 bytes in a label, 1 MiB in all |
| A source directory, a digest directory | 1,024 entries |
| JSON, every file | nesting 32; 16,384 values in a document (a proof counts the elements of `chain` one by one, not together); 1,024 members in an object; 256 bytes in a key; 64 KiB in a string as written (a `send_hex` of a columns file may be 16 MiB); 64 characters in a number; 65,536 characters of white space in a row |
| One element of a proof's `chain` | **4,096 bytes**, white space included, checked token by token while it is read: a block header is at most about 355 bytes compact and 450 indented (seven members, three hashes of 64 hex characters, three numbers), so the limit is a factor of ten above what any program writes; a larger element is `FAILED at step parse`, refused before it has been held (L12-05) |
| A proof outside its `chain` | 1 MiB (an honest proof outside the chain is under 20 KiB: two paths of at most 64 hashes, one header, a few numbers and names), checked the same way |
| A proof's `chain` | read element by element, **no header is kept** (below): the file is read twice, once to check every element and drop it, once to walk from the block of the row to the digest holding one header, and the SHA-256 of the two readings must be equal |

What the JSON reader does about each: a document that exceeds a limit is refused at the first
value that does (`more than 16384 values`, `a string is longer than 65536 bytes`, `nesting deeper
than 32 levels`), so eight million zeros in a 16 MiB file are not turned into eight million
nodes (the first version held 2.0 to 2.4 GiB for such a file: a tree of values, each with the
text of its path); the path of a value is built only when an error says it. A file is read into one
buffer of its size (not by doubling), except a proof and a block export, which are read in pieces; the
white space between two tokens is bounded because the decoder keeps it until the next token.

**Why a proof has no size limit, and what it costs.** A proof carries one block header after another from
the block of the row to the block of the digest it was made for, about 335 bytes of JSON each; at one
block in 10 seconds that is 8,640 a day, so the cap of 16 MiB that this table used to have (about 50,000
headers) refused every honest proof of a row older than about six days (L10-07; the same for the 1 GiB
cap of a block export, about a year). The cap was there because the file was read into memory whole and
the headers were kept (about 100 bytes each) until the whole file had parsed. Now the proof is read in
pieces, **twice** (`proof.ParseStream`, `proof.VerifyStream`): the first reading checks the whole
file, every element of `chain` is parsed and dropped, and keeps the rest of the proof, the number of
elements and the SHA-256 of the bytes; the steps that need no chain run; the second reading walks the
elements from the block of the row towards the digest and keeps one header. Two readings, because the
member `chain` may come before the member `block` in the file. The hash of the second reading must equal
the first (a file that changed in between is refused, status 1, step `parse`), nothing is reported as
verified before the second reading ended, and the verdict is about the bytes that were parsed. The cost is
time: a proof is read twice, about 22 seconds for the 1.06 GB proof of a row of one year below. There is no
size that is refused; a file that does not end is read until the disk does.

Measured on Linux, peak resident set of a child process (VmHWM; a run of `version` is 6 MiB),
the worst of ten hostile shapes (zeros in an array, a nesting of 16 million, a million members, a
million equal keys, one string of the whole file, a file of white space or of newlines, and a file
of `{}` lines, and one element of a proof's chain made of strings of 65,000 bytes) for each command that reads a file, at the largest size the command accepts and at 16 MiB:

| Command and file | Peak resident set |
|---|---|
| `proof --proof` (16 MiB) | 13 MiB (it was 29 MiB) |
| `proof --proof`, a valid proof with 60,009 headers (20 MB, refused by the old cap) | 11 MiB (refused before) |
| `proof --proof`, a valid proof of **a row one year old at one block in 10 seconds**: 3,153,600 headers, 1,061,653,051 bytes | **12.6 MiB**, 22 seconds (two runs: 12,912 and 12,544 KiB, `/usr/bin/time -v`) |
| `proof --proof`, **one element of the chain that holds 3,000 strings of 65,000 bytes** (a file of 195 MB; the shape of L12-05, a file of 1,040,064,023 bytes with 16,000 of them held 1,036,732 KiB before) | **5 MiB** (5,084 KiB), refused in under 10 ms at the first 4 KiB of the element (before: 198,844 KiB) |
| `proof --proof`, 3,000 members of 65,000 bytes beside the chain (195 MB) | **6 MiB** (6,048 KiB), refused after the first 1 MiB outside the chain (before: 71,932 KiB, refused at the 1,025th member) |
| `proof --digest`, `digest-chain` (64 KiB; 16 MiB is refused unread, 6 MiB) | 14 MiB |
| `row-hash --columns` (16 MiB, the worst case is one string of the whole file) | 72 MiB |
| `blocks --blocks` (16 MiB of lines) | 8 MiB |
| `blocks --digest`, `checkpoint --note`, `--trust`, `consistency --old`, `trust show` | at most 15 MiB |
| `digest-chain` over a directory of four files of 16 MiB (the review's case, 3.1 GiB before) | 6 MiB (the first is refused by its size, unread) |

`TestPeakResidentSetOfACommandOnAHostileFile` (Linux; it runs the test binary as a child and
reads its `VmHWM`) fails above the size of the file times five plus 8 MiB over the baseline, and
`TestAllocationDoesNotGrowWithTheShapeOfTheInput` (every system, in the process of the test)
measures the peak of the heap with the collector made eager and fails above the same bound. The
first one is skipped on Windows, which has no `VmHWM`; the second runs there.
`TestPeakResidentSetOfAProofDoesNotGrowWithItsChain` (Linux) runs proofs of 100,000 and 400,000 headers
(34 and 136 MB) and fails when the peak is more than 16 MiB over a small input or grows by more than 8 MiB when the proof is four times longer.
The measurement of the proof of one year is made with `TestWriteBigProofForMeasurement`, which writes
such a proof into the directory named by `KINGRES_BIGPROOF_DIR` (`KINGRES_BIGPROOF_N` headers) and does
nothing otherwise, and the built program run on it under `/usr/bin/time -v`.

## Exporting from a database

Run these where the database is, then copy the files to the auditor's machine
(the digest must go to a place the database's operators cannot change):

```
# digest (builds a block first if transactions are pending); store it OUTSIDE the database
psql -X -At -c "SELECT kingres.ledger_generate_digest()" > digest-$(date -u +%Y%m%dT%H%M%SZ).json

# inclusion proof for one row (the row hash is computed by the server here;
# the auditor recomputes it with row-hash from data they hold)
psql -X -At -c "SELECT kingres.ledger_inclusion_proof('app.accounts', kingres.ledger_row_hash(a.*)) FROM app.accounts a WHERE id = 1" > proof.json
```

`ledger_generate_digest` and `ledger_inclusion_proof` need `EXECUTE` granted
(see the ledger README, "Privileges"). The server's OS user is called `kingres`,
like the extension's schema, so set `search_path` explicitly or qualify tables.
A proof made at time T contains the headers up to the latest block at T; verify
it against a digest of block T or later.

## Cross-check against the server

`scripts/gen-fixtures.sh` starts a throwaway server from the installed binaries
in WSL2 (new directory under `$KINGRES_TEST_WORK/verifier-test/`, default `~/kw-verifier`, a free port from
57300 to 57399, `shared_preload_libraries = 'kingres_ledger'`), creates two
ledger tables with many column types and a third, updatable table (integers, numeric, text with non-ASCII
characters and a non-ASCII column name, timestamptz, timestamp, date, bytea,
boolean, uuid, float, jsonb, interval, inet, an array, NULL columns, an empty
string, an added nullable column), inserts rows over several transactions (one
by another role, one rolled back) and ten blocks (the updatable table gets inserts, updates of the same row
in two transactions, an update that changes several columns and a delete), and
writes `testdata/fixtures/`: nine digests at different heights, one digest of a
second, separately created database, the proofs of 15 rows and of 4 current rows
of the updatable table (`I`) and of 4 removed versions of rows, each as the `D`
proof of its removal and as the `I` proof of its entry, the column descriptions of
all of them in binary send form (for a removed version, the columns of its row in
the history table), the block headers and a manifest of the server's own answers.
Then it stops and removes the server. Regenerate with:

```
wsl.exe -d Ubuntu-24.04 -u kingres -- bash "/mnt/c/<path to the repository>/kingres/verifier/scripts/gen-fixtures.sh"
```

`go test ./internal/crosscheck/` then shows that every server proof verifies
against its digest and every later digest, fails against earlier digests and
against the other database's digest, that every `D` proof verifies and says that it is a `D` proof, that a `D` proof presented as `I` (by `--op I`, or by changing its `op` member) fails and the reverse, that every recomputed row hash equals the
server's `ledger_row_hash`, that the recomputed block chain equals the server's
digests and the headers inside the proofs, and (in `internal/proof`) that every
single-field and every single-byte mutation of a fixture proof is rejected.

Fuzz targets (kept): `FuzzParse` (jsonx, rowfile), `FuzzDecodeHex`, `FuzzEncodeRow`
(format), `FuzzParseDigest`, `FuzzParseBlocks` (chain), `FuzzVerify` (proof), and for phase 7 `FuzzParse` (signednote, trust), `FuzzVerify` (checkpoint), `FuzzParseTilePath` and `FuzzParseTile` (tlog), `FuzzParseLogSpec` (cli), `FuzzCleanRel` (safefile: the relative paths the source directory accepts). `scripts/fuzz.sh [seconds]` runs every target for a short bounded time; the seeds (the published vectors) also run in `go test ./...`.
Run one with `go test ./internal/proof -run '^$' -fuzz '^FuzzVerify$' -fuzztime 30s`.

## Layout

| Package | Purpose |
|---|---|
| `internal/merkle` | RFC 6962 hashes, tree root, RFC 9162 audit path and consistency proof verification |
| `internal/format` | every hash input (and the 87-byte leaf of a chain log), one function each, with the domain tag as a named constant and the README text quoted; strict hex |
| `internal/jsonx` | strict JSON (duplicate keys, invalid UTF-8, lone surrogates, plain integers in range) with error paths |
| `internal/chain` | digests, block headers, the block chain, digest sets |
| `internal/proof` | proof parsing and the verification steps |
| `internal/signednote` | signed notes (C2SP signed-note), Ed25519, key ids, verification against trusted keys |
| `internal/tlog` | tile paths, tile and entry bundle parsing, tree hashes read from tiles, inclusion and consistency proofs |
| `internal/checkpoint` | tlog-checkpoint parsing and verification of a signed checkpoint |
| `internal/trust` | the trust file: parsing, validation, writing, keys per log origin |
| `internal/rowfile` | the columns file of `row-hash` |
| `internal/safefile` | the one way every file is opened: never blocking, regular files only, bounded size, no link below a source directory |
| `internal/statefile` | the state file: lock, atomic replace with a crash test, link resolution, the place for conflicting evidence |
| `internal/stdio` | tells the null device that the runtime puts in place of a closed standard output from the one the user asked for, and reads `KINGRES_VERIFY_NO_OUTPUT` with which a caller says it gave the null device on purpose |
| `internal/testdir` | the temporary directories of the tests: a removal with a retry of 10 seconds on Windows, where a scanner can keep an entry for a moment (a test helper, never built into the program) |
| `internal/edkey` | the three tests of an Ed25519 public key (canonical, on the curve, not of small order), from the definitions |
| `internal/aggregate` | the leaf of an aggregate log (5.2): strict codec |
| `internal/events` | the signed events of a publication and the walk that decides which key authorises which event |
| `internal/pubsrc` | a publication as a directory, a `.tar.gz` or an HTTP(S) address, bounded and untrusted |
| `internal/bundleidx` | `bundle.json`, read strictly and never believed |
| `internal/attest` | the client attestation: the 136-byte message, the rows of the attestation and revocation tables, the block-height rule |
| `internal/cli` | the command line (`cmd/kingres-verify` is a thin `main`) |

## Decisions where the documentation is silent

These readings were made and are enforced; each is a finding below.

- Required proof members: all of the README example except `relation`
  (informational, optional). Unknown members are ignored in proofs and digests
  (but rejected in the columns file). Duplicate keys are always rejected.
- Required digest members: `chain_id`, `hash_version` (must be 1), `height`,
  `block_hash`. `database`, `block_closed_at`, `generated_at` are optional but
  checked when present (`block_closed_at` must equal the block time of the digest's block).
- Integer ranges: `table_id`, `seq`, `entry_index`, `entry_count`, `tx_index` and
  `tx_count` are PostgreSQL `int` (0 to 2147483647; counts at least 1);
  `ledger_txid` and `height` are `bigint` (0 to 9223372036854775807);
  `closed_at_us` is any `int64`; `hash_version` and `format` must be 1.
- Headers in `chain` after the digest height are parsed (strictly) but not
  verified, because a proof made after the digest legitimately has them.
- `op` must be the string `"I"` (byte `0x49`, an entry that inserted a row version) or `"D"`
  (byte `0x44`, an entry that removed one); any other string is rejected. A proof
  that says `D` but whose entry is an `I` (or the reverse) fails at `entry-path`.

## Findings about the format documentation

The ledger README and the design note were enough to write a working verifier:
the first run of the cross-check against the real server matched on every row
hash, every block hash and every proof, and **no source of the extension was read**
(the cross-check resolved nothing that needed it). The findings below are
ambiguities, omissions and one stale passage, most important first. Each gives
the wording to add.

1. **The block header export is not documented, and a header export does not
   carry the chain id.** The README documents the header fields only inside the
   proof (`block`, `chain`), never as an export of `kingres.ledger_blocks`, and
   the column names appear only in the design note. A verifier that recomputes
   the chain from the genesis hash needs the chain id, which is not in
   `ledger_blocks`. Add to the README, section "Chain identity": *"To check the
   whole chain independently, export `kingres.ledger_chain.chain_id` (it is also
   in every digest and proof) and the headers: `SELECT height, hash_version,
   prev_hash, tx_merkle_root, tx_count, kingres.ledger_timestamptz_us(closed_at)
   AS closed_at_us, block_hash FROM kingres.ledger_blocks ORDER BY height`. The
   chain is valid when block 0 has `prev_hash = SHA256(0x14 || chain_id)`, every
   block hash is recomputed as in "Block hash", every `prev_hash` equals the hash
   of the previous block, heights are 0, 1, 2, ... without gaps and
   `closed_at_us` never decreases."*

2. **Step 4 does not say what to do with `chain` elements beyond the digest
   height, or with a non-empty `chain` when `block.height == H`.** A proof made
   after the digest was taken has headers beyond `H`. Add to step 4: *"Elements of
   `chain` with a height greater than `H` are not used and need not be checked;
   a proof is valid against any digest whose height is at least the proof's block
   height and at most the height of the last element of `chain`. When
   `block.height == H`, `chain` is ignored."* (Consequence, implemented: a
   mutation of an element beyond `H` is not detected by that digest.)

3. **JSON integer ranges are not given.** "A number with a fractional part, or
   outside the range, is rejected" does not say which range, and the hash input
   widths (u32, u64) are wider than the database types. Add: *"`table_id`, `seq`,
   `entry_index`, `entry_count`, `tx_index`, `tx_count` are 32-bit signed
   integers (0 to 2147483647; `entry_count` and `tx_count` at least 1);
   `ledger_txid` and `height` are 64-bit signed integers (0 to
   9223372036854775807); `closed_at_us` is a 64-bit signed integer. A number
   written with a fraction (`1.0`), an exponent (`1e2`), a leading zero, a plus
   sign, or `-0` is rejected."* The same strictness should be stated for digests
   (the README only says it for proofs).

4. **Which proof and digest members are required, and whether unknown or
   duplicate members are allowed, is not stated.** Add: *"All members shown in
   the example are required, except `relation`, which is informational. A digest
   must contain `chain_id`, `hash_version`, `height` and `block_hash`; `database`,
   `block_closed_at` and `generated_at` are informational. A verifier must reject
   an object that has the same key twice. Unknown members are ignored. `format`
   and `hash_version` must be 1."* The README says `op` is `"I"` in the proof JSON
   and `0x49` in the leaf; add *"the JSON value `"I"` is the byte `0x49`"* to
   "Transaction tree".

5. **Digest timestamps: format and meaning.** `block_closed_at` and
   `generated_at` are shown only by example. Add: *"`block_closed_at` is the
   `closed_at` of the digest's block and `generated_at` the server time when the
   digest was made, both RFC 3339 timestamps in UTC with microsecond precision.
   `block_closed_at` must equal `closed_at_us` of block `height`. `generated_at`
   and `database` are not authenticated."* Also say that `ledger_generate_digest()`
   returns NULL for an empty ledger, so there is no digest (and no proof) for it.

6. **Row encoding: what "column order" and "name" mean in edge cases.** The
   README says "attribute order (attnum ascending)" in one place and "column
   order" in the design note. Add: *"Columns are taken in `attnum` order; dropped
   columns are skipped and do not renumber anything; a column added later comes
   last. The name is the column's name as stored, converted to UTF-8 in the same
   way as text values. A value of zero length (an empty string, an empty `bytea`)
   is not NULL and is encoded with `u32 length = 0`. At most 65535 columns and
   names of at most 65535 bytes are encoded."* (Both hold in the cross-check: an
   empty `bytea` and an empty `text` are in the fixtures.)

7. **How to obtain the send bytes of a row outside the server.** The README says
   "compute the row hash from that row" but gives no per-type list or procedure.
   Add a paragraph: *"The bytes are those of `typsend`: for example `int4send`
   is 4 bytes big-endian, `boolsend` one byte, `textsend` the UTF-8 bytes,
   `timestamptzsend` the int64 microseconds since 2000-01-01 UTC, `numeric_send`
   the PostgreSQL binary numeric. To obtain them in SQL: `SET client_encoding =
   'UTF8'; SELECT encode(<type>send(col), 'hex')`. Types whose send form depends
   on the server version are listed in `ledger_type_problem`."*

8. **Verification step list omits some checks that an implementation needs.**
   Add to the numbered steps: *"Every header (the proof's `block` and each
   element of `chain` that is used) must have `hash_version` 1. The tree size
   must be at least 1 and the index smaller than the size. An audit path longer
   than 64 hashes is invalid. `user_name` is at most 255 bytes of valid UTF-8."*
   The README says the sizes are authenticated by the transaction leaf and the
   block hash but never says a verifier must reject an index outside the tree.
   (Observed in this implementation's tests: an audit path built for a tree of
   `n` leaves also verifies as a path in a tree of `n+1` leaves for many indexes,
   which is why the authenticated sizes matter.)

9. **Stale numbers in the design note.** "Review of the hash formats" says "the
   transaction leaf (41 bytes) and the block leaf (44 bytes)". With the tag
   byte the entry leaf data is 42 bytes, and the block transaction leaf is
   `1 + 8 + 4 + 32 + 2 + n = 47 + n` bytes. The section above the "fix round"
   notes still contradicts them. Replace with *"the entry leaf data (42 bytes)
   and the block transaction leaf data (47 bytes plus the user name)"*. The block
   hash input is 87 bytes (`1 + 2 + 8 + 32 + 32 + 4 + 8`); stating it would help
   implementers check their buffer.

10. **`user_name` and database encoding.** The README says the name is UTF-8 but
    `name` values are stored in the database encoding. Add: *"For a database whose
    encoding is not UTF-8 the name is converted to UTF-8 (like text values); for
    `SQL_ASCII` the stored bytes are used."* Also state that names longer than
    255 bytes cannot occur (`name` is at most 63 bytes) so that no truncation rule is needed.

11. **Time ordering across a digest set.** The README defines "never goes back"
    for blocks only. Add: *"Digests of one chain taken at different times satisfy:
    same `chain_id`; if `height` is equal then `block_hash` is equal; if `height`
    is larger then `block_closed_at` is not smaller."* (This is what
    `digest-chain` checks.)

12. **Minor.** The README example shows `"closed_at_us": 816000000000000` for a
    block whose `block_closed_at` would be 2025; use consistent example values so
    that an implementer can test the conversion. The README never says that proof
    JSON is produced by `jsonb`, so key order and spacing (`"op": "I"` with a
    space) are not significant; say so: *"Key order and white space are not
    significant."*

## Findings about the phase 7 design (step 1)

Checked against the public text of C2SP `signed-note`, `tlog-checkpoint`,
`tlog-cosignature` (for the definition of an origin), `tlog-tiles`, RFC 6962
section 2.1 and RFC 9162 sections 2.1.1 to 2.1.4. Where the design and a
specification differ, the specification was followed. The test vector of the
signed-note specification (example key and note) is used; the checkpoint
example of tlog-checkpoint has no published key, so only its text is parsed.

1. **Consistency path length.** Design 4.10 says "at most `2*ceil(log2 n)`
   hashes"; RFC 9162 2.1.4.1 says "bounded above by `ceil(log2(n)) + 1`". The
   verifier accepts at most 65 hashes.
2. **Extension lines.** Design 3.2.4 says a checkpoint has three non-empty
   lines and no extension lines; tlog-checkpoint allows optional non-empty
   extension lines after the third. The verifier accepts and reports them
   (the design's own checkpoints have none).
3. **Origin rules.** The design states key-name rules only. tlog-checkpoint
   takes the origin from tlog-cosignature: at most 255 bytes, no Unicode spaces,
   `+` or control characters. Enforced; the trust file limits `origin_prefix`
   to 160 bytes so that `/c/<64 hex>/e<digits>` fits.
4. **Key names.** Design 7.2 says a key "whose name is `origin` or
   `origin_prefix`" is tried, but the `name` of a trust file key (`"2026"`) is
   a label. Implemented: the note key name of every signature is the log origin
   (3.2.4), which must be the prefix or below it; each pinned public key is
   tried under that name. The cluster-level name `<prefix>` is accepted for the
   origin of a later step (events).
5. **Validity windows against rotation.** Design 7.4 applies `not_before` and
   `not_after` to `--now`, but 3.3 says a retired key's checkpoints stay valid
   for ever, and a checkpoint carries no time. `not_before` is applied to
   `--now`; `not_after` is reported, not enforced.
6. **Exit status 2 for the note.** Design 7.4 lists a malformed "checkpoint
   note given as the anchor of trust" under status 2. For `checkpoint` the note
   is the thing under test, so a malformed note is status 1 (step `parse`), like
   a malformed proof. For `consistency` the `--old` note is the anchor: unreadable
   or not a note is 2, a bad signature or origin is 1.
7. **Output order.** Design 7.4 says the final line is `VERIFIED`, `FAILED` or
   `INCOMPLETE`; the existing commands print the headline first and the steps
   after. The new commands do the same as the existing ones.
8. **Duplicate signature lines.** The signed-note text does not forbid two
   lines with the same name and key id; tlog-checkpoint does. Rejected for every
   note, as design 7.6 (3) lists.
9. **A forged line from a known key.** signed-note says SHOULD reject; the
   verifier always rejects. A line of an unknown key is ignored.
10. **Epoch.** Design 7.4 shows `:e<N>` as optional. Without it the epoch is
    taken from the source only if it holds exactly one epoch; a checkpoint file
    needs it given.
11. **Partial tiles.** Design 4.4 deletes a partial tile 24 hours after its
    full tile; tlog-tiles allows clients to fall back to the full tile, which
    the reader does (the first `W` hashes).
12. **Sizes are not authenticated by a proof.** A path that verifies for size
    `n` can also verify for `n + 1` for the same root (observed in the tests),
    so sizes are only meaningful through a signed checkpoint, which binds size
    and root. Every command takes sizes from checkpoints.
13. **Sources.** Design 4.6 and 7.4 let the verifier read a tar file or an HTTP
    base URL; step 1 reads a directory only.

## Findings of the review of step 1 and what was changed

The independent review of steps 1 and 2 (`kingres/docs/reviews/2026-11-16-ledger-phase7-steps-1-2.md`)
found these in the verifier; each has a test that failed before the change.

- **L7-01, L7-05** (a named pipe hangs the verifier; links are followed): one file helper
  for everything the verifier opens (`internal/safefile`) and the rule above for sources.
- **L7-02** (`not_after` reported only, notes missing from `--json`): the validity members
  of a key are enforced and `trust show` says how; text and JSON are made from one structure.
- **L7-03** (an older genuine checkpoint is VERIFIED): the "not proved" list names rollback
  and the three remedies; `checkpoint --state`.
- **L7-06** (output claimed more than was checked): the tile line says how many tiles were
  read and that no other was; an empty proof says that no tile was needed; the help and this
  README say that only the proof path is read.
- **L7-07** (status 2 for a missing checkpoint, 3 for a missing tile; `trust show` accepted a
  file with no key): one rule for what the publication does not contain (status 3), and a trust
  file with no usable key is refused by every command including `trust show`.
- **L7-10** (status 3 and a hostile publisher): the output and "Exit statuses" say that 3 is
  an alert.
- Found in the pass over the older commands: `digest-chain` over an empty directory printed
  `CONSISTENT: 0 digests agree`; `blocks` without `--digest` said "the block chain is consistent"
  without saying that a chain rewritten from the genesis block verifies too; the text output of
  `proof` and `blocks` called the "not proved" items "note" while `--json` called them
  `not_proved`; a failed `proof` printed none of the steps that had passed (the JSON did); the text of `proof` had two lines (the
  entry and the operation) that were not in `--json` as lines (they are steps now); every
  command opened its files with `os.Open`.

## Findings of the review of the fixes (L8) and what was changed

The second review (`kingres/docs/reviews/2026-11-16-ledger-phase7-steps-1-2-fixes.md`) found
these in the verifier; the design has the decisions as corrections 17 to 19.

- **L8-01** (`--state` accepted a fork of a larger size and replaced the evidence): `checkpoint
  --state` proves the extension from the tiles in the same run and for the same bytes, keeps
  the state and saves the conflicting note on a fork or rollback; `consistency --old` is the
  read-only proof; the two-command order is withdrawn.
- **L8-02** (input text reached the output raw): one function renders every string, see "Text
  from the inputs is escaped".
- **L8-03** (a state or `--old` note signed by a key retired since): a checkpoint you hold is
  judged by signature, revocation and size limit, not by the window; one status (2) in both
  commands; the advice never says to delete the state.
- **L8-04, L8-07** (two runs lose the higher state; a link replaced; a long name failed; mode
  0600; no directory flush): lock, write through a link, short helper names, kept mode, a flush
  of the directory (a write-through rename on Windows), a crash test at every step.
- **L8-05** (`digest-chain` caps and glob characters): listed by name through the safe helper,
  1,024 files and 64 MiB.
- **L8-06** (three exit statuses): a chain directory with no epoch is 3; a held note is 2 in both
  commands; a publication file your account cannot read stays 2, documented in the table above.
- **L8-09** (key windows): clock and `revoked` said in the table, the time is printed with its
  fraction, the boundary is documented, an empty or comma `--now` is an error, a revoked signer
  on a note is named, `trust show` says what it cannot know.
- **L8-11**: `blocks` streams; `row-hash --json` no longer says `"verified": true`; the message
  for a regular file given as a source no longer says "special file".

## Findings of the review of the second fixes (L9) and what was changed

The third review (`kingres/docs/reviews/2026-11-17-ledger-phase7-steps-1-2-second-fixes.md`)
found these in the verifier; each fix has a test that fails without it (shown by removing the
fix in a copy of the tree).

- **L9-04** (a validly signed note with a long extension line made a state of 4.2 MB that no later
  run could read): the note is bounded where it is parsed, the record is bounded by the same
  constant on writing and on reading, and a state is read back through the reader's own functions
  before it is stored ("What a state file must be"); a property test and a fuzz target write
  every state they can and read it back.
- **L9-05** (2 GiB for a 16 MiB JSON file): caps on depth, values, members, key and string
  length, white space, and the elements of a proof's chain streamed ("Limits and memory"), with a
  measurement of the peak memory of every command that reads a file.
- **L9-06**: a directory of digests is read by the names its listing gives; the export command
  makes names without a colon.
- **L9-07**: evidence is claimed saved only when a file with its content is there; status 4 when
  it could not be saved.
- **L9-08, L9-12**: the lock is opened without following a link, is one per directory, the
  directory of the state must be private (Unix), and the README says what the lock does not cover.
- **L9-09**: the table of statuses is derived from the source and tested; `blocks` is status 1 for
  an export that cannot be parsed.
- **L9-10**: the escaping is one to one, the cut is marked in a way no input can print, and the JSON
  escapes what the text escapes.
- **L9-11**: tests for the mutants that survived (the JSON `extends_state`, the strictness of the
  state, a stored size of 1, a read error in `blocks`, the printed time, the order of the flushes
  of the state), and a statement of what `blocks` does and does not show about an export that ends.
- **L9-13**: the failure seen on Windows (a test directory that cannot be removed at the end of
  a test, "The directory is not empty") was reproduced, about once in 150 runs of a state
  command and about once in three runs of the whole suite on a machine with a file scanner, and
  it is not the program: no handle of the program is left open after a run (a test counts them on
  Windows, before and after a collection, over 60 runs of three commands), and the directory can
  be removed some 12 ms later by the same process without anything being closed, so the holder is
  outside it (a scanner opens a new file for a moment). Nothing was added to retry it. The memory
  tests write smaller files on Windows so that they give such a scanner less to read. The tests of
  the state package are named `TestStateFile...`, so the filter `-run 'State|HeldNote'` runs them.
- **L9-15**: an empty state file is status 2 with advice that keeps the protection.

Changes of interface that a caller can see: exit status 4; the flag `--allow-shared-state-dir`;
`blocks` output (the headline, one more "not proved" line and `info.blocks`, `info.head_height`,
`info.head_hash`, `info.complete_export_shown`); `info.extends_state` and
`info.state.extension_proved` are `false` (not null) for a first use; `info.state.evidence_saved`;
`"invalid_utf8"`; the state file name `<state>.conflict-<size>-<64 hex>`; the lock file
`.kingres-state.lock`; a new state file has mode 0600; notes over 64 KiB, with a line over 1,024
bytes or with more than 16 extension lines are refused; digests over 64 KiB are refused;
escapes in the text output (`\\` for a backslash, `\x0a` for a newline, a cut marker).

## Findings of the review of the third fixes (L10) and what was changed

The fourth review (`kingres/docs/reviews/2026-11-17-ledger-phase7-steps-1-2-third-fixes.md`) found
these in the verifier; each fix has a test that fails without it (shown by removing the fix in a copy
of the tree). The design has the decisions as corrections 40 and later.

- **L10-04, L10-05, L10-09** (a publisher without a key plants a dead end in the state, stops it from
  advancing, or turns status 1 into status 4): one cause, the note was stored and saved as it was served.
  The state and the evidence now hold the canonical form of the verified note, and a held note is judged
  by the signatures that verify ("What the state holds"); status 4 prints the note. Tests:
  `TestStateKeepsOnlyTheLinesThatVerified`, `TestRetiredKeyLineDoesNotStopTheState`,
  `TestStateOfTheEarlierVersionIsStillRead`, `TestEvidenceIsOneFileWhateverTheUnverifiedLines`,
  `TestStatus4ShowsTheNoteToKeep`, `TestStateSurvivesEveryChangeOfTheTrustFile`, `FuzzStateFromNote`,
  `TestCanonicalDropsWhatNoKeyVerified`, `TestReadBackRefusesWhatALaterRunWouldNotRead`.
- **L10-06, L10-12** (a rollback printed VERIFIED with status 0 through a path another user can change;
  the lock file was outside the check): the check covers every component of the path, links, the
  holder, the state and the lock file ("Where the state may be"), on Unix; nothing on Windows, and the
  README says so. Tests: `TestRedirectedStateIsRefused` (the reported case), `TestCheckPrivate...`,
  `TestLockFileIsPrivateAndReplaced`, `TestAcquireCheckedLooksAgain`.
- **L10-07** (the proof cap refused every honest proof of a row older than six days): no size limit,
  a proof is read in pieces and twice, memory is constant ("Limits and memory"); the cap of a block
  export is gone too. Tests: `TestHonestProofOfAnOldRowIsNotRefusedForItsSize`,
  `TestProofThatChangesBetweenTheTwoReadingsIsRefused`, `TestStreamedVerification...`,
  `TestStreamingChecksAgreeWithTheBufferedOnes`, `FuzzParseReader`,
  `TestPeakResidentSetOfAProofDoesNotGrowWithItsChain`.
- **L10-08** (status 0 when the verdict could not be written): `Run` wraps the standard output and turns
  status 0 into 2 when a write failed. Tests: `TestFailedWriteOfTheVerdictIsNeverStatus0`,
  `TestVerdictToDevFull`.
- **L10-10** (values clipped before escaping): the clips are gone, the only cut is the output layer's.
  Test: `TestValuesAreNotClippedBeforeTheyAreEscaped`.
- **L10-11** (the README wrote the backslash escape with one backslash): fixed, and a test reads the
  rule back from the README text (`TestReadmeBackslashRuleDecodesTheOutput`).
- **L10-16** (exit sites): three rows that had "no portable scenario" have one on Unix (a directory and a
  file without permission, a write that fails for `trust init`); `trust init` removes the file it made
  when the write failed (a file of 0 bytes blocked the next run).
- **L10-17** (`blocks` exits 0 for a cut or unanchored export): the text said so; the JSON now has
  `info.anchored_to_digest`, false when no digest ties the chain to something kept outside the database.
  The status stays 0: the headers form an unbroken chain from the genesis hash, which is what was asked.
- **L10-18**: the nesting limit is 32 levels exactly (33 arrays were accepted); a refused state that was
  never written no longer says that the file "must not be deleted". Left as they are, and why:
  the verifier requires `tx_count` of at least 1 and a `block_hash` in every header of a chain (the
  ledger never makes a block with no transaction, and the hash is what the walk checks); digest times
  accept an offset and are converted to the instant (they are informational and compared only with
  the block time); `verified_by` and `verified_at` of a state are for a person and no decision
  uses them; the 16 extension line cap fails at step `checkpoint`, after the signature was verified,
  because it is a property of the signed content and not of the note's form.

Changes of interface that a caller can see: status 2 for a result that could not be written (before: 0);
no size limit on `--proof` and `--blocks` (before 16 MiB and 1 GiB, status 2 or 1 for a larger file);
`checkpoint --state` refuses a path with a component another user can change (before: only the
last directory), and the lock file is replaced when it is not private (withdrawn in L12-02: it let two runs hold the lock together); the state and the evidence file
hold the canonical note (before: the note as served), a held note ignores lines that do not verify
(before: status 2); a note line `the note to keep ...` and `info.state.note_base64` on status 4; the
notes `the note as served has N signature line(s) that were not counted ...`, `... has N signature line(s)
that name a key of the trust file and do not verify: ignored ...` and `the note also carries a line that
names key ... and does not verify` (before: a line that never verified was called a signature);
`info.anchored_to_digest` of `blocks`; no clip of a value before escaping (a message is longer, and cut
by the output layer at 2,048 bytes); the nesting limit of 32 is exact; a failed `trust init` leaves no file.
Not changed: the exit statuses of the table, the flags, the file names.

**Mutation run of the verifier (L10).** The review ran none, so one was made: a small program with
`go/ast` (outside the repository) changes one thing at a time in a file (a condition negated, a
comparison, `&&` and `||`, `+` and `-` swapped, a constant changed, a boolean swapped, a statement
removed) and runs the tests of the package against each mutant; a mutant the tests do not fail is a
survivor. First run, then the same after the tests written for the survivors (a mutant that does not
compile is not counted; the files of `internal/cli` were run on every second or fourth mutant to keep
the load of a shared machine down):

| File | Mutants | First run: killed, survived | After the new tests: killed, survived |
|---|---|---|---|
| `signednote/signednote.go` | 175 | 140, 19 | 152, 7 |
| `checkpoint/checkpoint.go` | 91 | 62, 21 | 75, 8 |
| `proof/proof.go`, `proof/stream.go` | 150, 39 | 120, 25 and 37, 1 | 141, 4 and 38, 0 |
| `statefile/statefile.go`, `private_unix.go`, `sync_unix.go` | 109, 78, 36 | 74, 29 and 64, 13 and 22, 14 | 91, 12 and 74, 3 and 23, 13 |
| `trust/trust.go` | 243 | 196, 38 | 221, 13 (and tests for 6 more, not run again) |
| `merkle/consistency.go`, `merkle.go` (with the tests of `tlog`) | 70, 61 | 0, 70 (the tests of the package do not cover it) and 57, 2 | 68, 2 and 57, 2 |
| `cli/cli_state.go` (every second mutant) | 101 of 202 | 136, 47 (all) | 81, 10 |
| `cli/state_record.go` | 58 | 51, 7 | 58, 0 |
| `cli/cli_tlog.go` (every fourth) | 79 of 313 | 201, 48 (all) | 58, 3 |
| `cli/cli_out.go` (every second) | 53 of 105 | 41, 12 | 48, 5 |
| `cli/cli.go` (every fourth) | 52 of 245 | 45, 7 | 51, 1 |

The survivors that are left are equivalent or cannot be told apart by a test: return values of an error
path that no caller reads (`return 0, 0, err`), the second check of a size that `ParseUint` makes anyway,
the cleanup `_ = f.Close()` of a file that is closed by the process end, the sleep between two attempts
of the lock, the constants of the loop that replaces a lock file (an extra attempt), the branch of
`ownerOf` for a system that does not give an owner, the escape of a raw invalid byte that the encoder has
already replaced, the error paths of `writeAndSync` and of the removal of a half written evidence file
(they need a file system that fails a write), the `consistency` error that `applyState` never sees, and
the test-only hook `traceExit`.

## Findings of the review of the fourth fixes (L12) and what was changed

The review (`kingres/docs/reviews/2026-11-17-kingres-verifier-fourth-fixes.md`) confirmed that nothing
unverified was accepted and that the state never moved backwards, and found that a party without a key
still had a choice that mattered: which of the genuine signature lines of a note the auditor saw. The
standard of this round: nobody who holds no trusted key can bring the auditor's state into a condition from
which the protection is lost or from which no later run can go on, or decide the exit status of a run by
something no signature covers.

| Finding | What was changed | Where it is described |
|---|---|---|
| L12-01 the publisher chooses the key a state rests on | the identity of a checkpoint is its signed text; the state keeps the union of every line that verifies under any key of the file; a held state is usable while its text verified under a key of the file, whatever the key became (a state on a revoked key is flagged and used as the stricter anchor) | "What the state holds", "Checkpoints you hold" |
| L12-03 subsets of lines force status 4 | evidence is one file per signed text, extended with the lines of the same text; no limit that a party could use up | "The evidence of a fork" |
| L12-02 two holders of one lock | a lock file is never removed or replaced: locked first, mode tightened in place with `fchmod`, a foreign owner refused; the name is looked at again after the lock; the write refuses to replace a state that is not the one that was read | "What the lock does and does not protect" |
| L12-04 closed standard output | a standard output that is the null device opened for reading and writing is refused with status 2 | "The standard output" |
| L12-05 one element of 1 GiB | one element of a proof's chain is at most 4,096 bytes, what is outside the chain at most 1 MiB, checked while reading | "Limits and memory" |
| L12-06 `..` after a link in a relative `--state` | the name is not cleaned before the link is followed, in the check and in the name used | "Where the state may be" |
| L12-07 the cut is before the escape | the README says so | "Text from the inputs is escaped" |
| L12-08 guards that cannot fail | the nesting limit is a literal 32 in its test; a lock file of mode 0640 is tightened and tested; the read-back guard is reached through a seam and tested | tests |
| L12-09, L12-10 | said in "The standard output" (a reader that cuts the output); the order of the verdict and the state write is left as it was, see below | below |

**Interface changes a caller can see.**

- `checkpoint --state` and `consistency --old STATE` with a state whose verifying keys are all revoked or
  beyond their `max_tree_size`: **status 0 and a note, where it was status 2**. The output says `rests only
  on keys that the trust file no longer counts` and lists as not proved that the held checkpoint was
  genuine; a plain note as `--old FILE` keeps status 2. A state that no key of the file verifies (a key was
  deleted from the file) is status 2 as before, and its message names `checkpoint --state NEWFILE
  --min-size N`.
- A new value of `info.state.action`: `signatures_added` (the same checkpoint, more genuine lines, the state
  was rewritten). `unchanged` is as before and means the file was not touched.
- The state and the evidence files hold the lines that verify under any key of the file, counted or not
  (before: the counted ones). The note `the note as served has N signature line(s) that were not counted`
  now says `(they verify under no key of the trust file)`.
- An evidence file that exists for the same text is extended in place (atomically) with the lines of the
  note served; the note says so. The name is as before.
- A lock file of another owner is refused (status 2, the command to remove it by hand); a lock file with
  group or other access is kept and tightened (before: removed and made again).
- A state that changed between the read and the write is not overwritten: status 2, `wrote nothing`.
- A standard output that is the null device opened for reading and writing is status 2 on every command, with
  a line on the standard error (before: status 0 and no output); `>/dev/null` is unchanged.
- A proof element of more than 4,096 bytes, or more than 1 MiB of a proof outside its chain, is `FAILED at
  step parse` (status 1), where it was held in memory whole.
- A relative `--state sl/../s` with `sl` a link goes to the parent of the target, as the system takes it;
  before it went to `./s`.
- No new flag, no new exit status, no new file name. The table of exit statuses is unchanged and is still
  derived from the source (the sites of `applyState` were renumbered and two reached through a test seam).

**States written by earlier versions** are read: a state that holds the note as served, or only the lines of
the keys that counted, is used as it is; it is rewritten (canonical, with the union) when it advances or when
a note of the same text brings a line it lacks, and is not touched otherwise. A state written by this version
is read by the previous one when a key that counted still counts, and is refused by it, with its advice to
rebase, only when that was already so.

**What is left.** Deleting from the trust file every key whose line a state holds still makes the state
unreadable, with the rebase as the way on (see "The one way left ..."): to be readable without a key a state
would have to be believed without its signature, which was not decided. The time between the last
read of the state and the rename is open for a writer that ignores the lock (measured in L14-04, see "A write never moves the state over one it did not read"). The status of a run that cannot
write its verdict after it moved the state is 2 and the state stays moved (L12-10): the standard error says so;
writing the verdict first would not be better, since then a verdict could be shown for a state that was not
written. The property tests run with a trust file of 4 keys and with one of 16 (the most it may hold), and two epochs of
one chain have a test of their own (`TestTwoEpochsKeepTheirOwnState`). The swap loops against the path check and against the proof file were run with one
account (`TestStateDirectorySwapped...`, `TestProofFileSwapped...`): they show that a state written through
a link that is swapped between private directories stays coherent and that a proof that changes between its
two readings is refused with that reason, and not that a second account cannot do more; the check needs a
swapper that may change a component the walk required to be owned by the auditor or by root, which one
account is.

## Findings of the review of the fifth fixes (L14) and what was changed

The review (`kingres/docs/reviews/2026-11-17-kingres-verifier-fifth-fixes.md`) found no wrong verdict, no
accepted rollback or fork, and no way for a party without a key to move or weaken the state. It accepted the
decision of the fourth round about revoked keys (a held state that rests only on keys the trust file no longer
counts stays the anchor) on the condition that a conflict with such a state is worded and kept as what it is.
The round is small and changes no verdict.

| Finding | What was changed | Where it is described |
|---|---|---|
| L14-02 a fork against a state on a revoked key accuses the log, names no way out, saves a file at every run | the failure line says that the state rests only on the named keys, that it may have been signed by a stolen key and that the conflict may come from the state, and gives the rebase; the sentence "both are signed by a key of the trust file" is printed only when true; one evidence file for the held state, never one per run; status stays 1; the JSON has `anchor_weak` and the keys | "A conflict with a state that rests on keys that no longer count" |
| L14-01 a caller that gives the null device read-write is refused | `KINGRES_VERIFY_NO_OUTPUT=1` says it was on purpose; the refusal stays the default and names the variable; Python, `daemon(3)`, a shell, systemd and Windows are named | "The standard output" |
| L14-04 the window of a writer that ignores the lock is not microseconds | the temporary file is written and flushed first, the state is read and renamed after; the window is measured | "A write never moves the state over one it did not read" |
| L14-03 extending a state or an evidence file drops the line of a key that left the trust file | lines already stored are kept as stored and only new lines are verified; the output says what the file holds | "Lines of one text only accumulate" |
| L14-05 a program that holds the state open fails a verified run on Windows | the rename is tried again for 3 seconds, then the message says that another program may hold the file | "A write never moves the state over one it did not read" |
| L14-06 guards no test catches | tests for the evidence that is not rewritten, for `consistency --old STATE` on a weak state, and for the text check of `Merge`; the two redundant guards (M29: the equal-text test before the merge, M30: the origin compared in `readBack`) are deleted, not tested | tests |
| L14-07 the weak anchor is not in the JSON | `info.state.anchor_weak`, `info.state.anchor_weak_keys`; for `consistency`: `info.old_anchor_weak`, `info.old_anchor_weak_keys` | "A conflict with a state that rests on keys that no longer count" |
| L14-08 the removal dead end | the README sentence is limited to what is true; a state whose every line is of a key the file no longer holds is refused with the key ids and the reason | "The one way left to make a held state unreadable" |
| L14-09 a new evidence file is created, then written | written to a temporary file and linked into place | "The evidence of a fork" |

**Interface changes a caller can see.**

- The environment variable `KINGRES_VERIFY_NO_OUTPUT=1` (new). Without it nothing changes; the refusal of a null
  standard output opened for reading and writing now names it.
- A fork, a rollback or a failed extension against a state that rests only on keys the trust file no longer
  counts: the status is still 1 and the step is still `fork` or `rollback`; the failure line and the notes are
  longer and say what is said above; the evidence file is `<state>.conflict-weak-<held size>-<held root>` (one
  for the state; two files since L16, see below) and not `<state>.conflict-<size>-<root>` for each size served.
- New JSON members: `info.state.anchor_weak` (every run with `--state` that does not end before the state is read),
  `info.state.anchor_weak_keys`, `info.old_anchor_weak`, `info.old_anchor_weak_keys` (`consistency --old`).
- The note of an evidence file or a state that gained a line says how many lines it holds and of what kind; a
  line of a key that left the trust file is kept in it.
- The message for a state none of whose keys the trust file holds names the key ids and says that removing a key
  did it.
- On Windows a rename that another program blocks is tried again for 3 seconds before the run ends with status
  2; the message says that another program may hold the file.
- No new flag, no new exit status. The table of exit statuses is unchanged and is still derived from the source (the new site
  `weakConflict` has a row of its own, status 1; status 4 is stated as unreachable from it).

**States and evidence written by earlier versions** are read as they were.

**What is left.** The window of a writer that ignores the lock is the rename call, measured above (1 of 1,200
attempts on Linux, 189 of 3,000 on Windows: counts for one machine and one rival, not rates). Whether a state counts
as "weak" depends on the lines the publisher served on the run that made it: it is not a choice of the verdict,
only of the wording and of the evidence files of a later conflict. `systemd` was not run.

## Findings of the review of the sixth fixes (L16) and what was changed

The review (`kingres/docs/reviews/2026-11-18-kingres-verifier-sixth-fixes.md`) found no wrong verdict, no accepted
rollback or fork, no way for a party without a key to move or replace a held state or to flood the auditor, and no
torn state in 700 kills. It found one finding that mattered (L16-01) and a set of statements that the code did not
keep. This round changes no verdict and no exit status.

| Finding | What was changed | Where it is described |
|---|---|---|
| L16-01 a replay of an old genuine checkpoint takes the one evidence file of a weak state, and the next conflict is neither saved nor printed | two files at most for a weak state, one for a rollback and one for a fork (`<state>.conflict-weak-rollback-...`, `<state>.conflict-weak-fork-...`); whatever note is not saved by the run is printed in full (text and `note_base64`), for any reason | "A conflict with a state that rests on keys that no longer count" |
| L16-02 `anchor_weak` and `old_anchor_weak` are missing on some exits | the member is on every exit after the held state was judged: tile failures, input errors after the state was read, read-back, changed state, failed write, the size failure of `consistency`, and the other exits of `consistency` after `--old` | the same |
| L16-03 a weak state that the run repaired is still called weak and its non-proof is listed | the JSON and the text report the state as it is after the write (`anchor_weak` false, `anchor_weak_repaired` true); the non-proof is not listed | the same |
| L16-04 a temporary file left by a crash in the extension of an evidence file is not removed | every temporary file of this program in the directory is removed under the lock, with a strict name, regular-file and owner check | "The state file" |
| L16-05 a failed flush of the directory after a successful rename or link is reported as a failed write | it is reported as what it is, with the same status; `directory_flushed` and `evidence_flushed` in the JSON | "The evidence of a fork" |
| L16-06 (Windows) a read-only target waits 3 seconds and is blamed on another program | a read-only attribute and a directory at the name are named at once; the wait stays for codes 5, 32 and 33; the codes were measured | "A write never moves the state over one it did not read" |
| L16-07 the flush of the evidence file before the link is not tested; two tests fail under load; the adversary test rarely meets a weak state | a test of the order and of a failing flush; `TestStateTwoRunsNeverMoveBackwards` counts the designed status 2 and keeps its assertion; the two load-sensitive tests repeat a measurement and wait longer without losing what they check; a deterministic test makes the two worlds differ in whether the state is weak | tests |
| the cleanup failure of the tests on Windows | one helper for the temporary directories of the tests (`internal/testdir`) with a retry of 10 seconds on Windows; the handle test is repeated with the collector off, on Windows and on Linux | tests |
| L16-09 the percentages | the figures are stated as counts, for one machine, one file system and one rival, and no rate is claimed | "A write never moves the state over one it did not read" |
| L16-10 a rotation makes a state weak | stated, with the wording and what to do before revoking | "A key rotation makes a state weak without anyone's act" |

**Interface changes a caller can see.**

- A fork, a rollback or a failed extension against a state that rests only on keys the trust file no longer counts:
  the evidence files are `<state>.conflict-weak-rollback-<held size>-<held root>` and
  `<state>.conflict-weak-fork-<held size>-<held root>` (at most one of each), not
  `<state>.conflict-weak-<held size>-<held root>`. A file of the old name that an earlier version made is not read and
  not removed. A conflict that is not saved by the run says `was NOT saved by this run` and prints the note
  (`the note to keep, as base64 ...`, `info.state.note_base64`), with `evidence_saved` false and `conflict_file` null;
  the status is still 1.
- New JSON members: `info.state.anchor_weak_repaired` (true, only when the run wrote a state that no longer rests only
  on keys the trust file does not count), `info.state.directory_flushed` (false, only when the directory could not be
  flushed after the state was replaced) and `info.state.evidence_flushed` (false, only when the directory could not be
  flushed after an evidence file was linked). `info.state.anchor_weak` is now on every exit after the held state was
  judged (it was on some), and `info.old_anchor_weak` on every exit of `consistency` after `--old` was judged, a plain
  note included (`false`).
- After a run that replaced a weak state with one that counts, `anchor_weak` is false and the weak keys are not
  listed; the text note `the state file rested only on keys ... it does not rest only on such keys any more` says so,
  and the not-proved line about the held checkpoint is not printed. `consistency --old STATE` still prints it.
- The message after a failed flush of the directory (status 2 for the state, 4 or 1 for the evidence, as before) says
  `was replaced ... but the directory could not be flushed: a power loss may bring the old file back` or `was saved to
  ..., but the directory could not be flushed`, and not that the state could not be updated or the evidence was not
  saved.
- A temporary file of this program that an earlier crash left, under any name of the program, is removed by the next
  `checkpoint --state` under the lock (it was removed only when the same name was written again).
- On Windows a state or an evidence file with the read-only attribute, or a directory at the name, is refused at once
  with `the file is read-only (the read-only attribute is set), so it cannot be replaced` (status 2) and not after 3
  seconds with the wording about another program.
- No new flag, no new exit status. The table of exit statuses is unchanged and is still derived from the source (the
  new exit of the state replaced with a failed flush is a site of its own, status 2).

**States and evidence written by earlier versions** are read as they were. A state that rests on a revoked key is
still judged the same way.

**What is left.** A party without a key decides which of two conflicting checkpoints of one kind is saved; it cannot
make a run print less, and it cannot move or replace the state. Windows code 5 cannot tell an open handle from a denial
by an access control list: the second waits 3 seconds and is worded as the first. The figures of the window are
counts from one machine. The cleanup failure of the tests on Windows was not traced to a process (the Restart Manager
named none); the tests now wait for it.

## Findings of the review of the seventh fixes (L18) and what was changed

The review (`kingres/docs/reviews/2026-11-18-kingres-verifier-seventh-fixes.md`) found one code defect, wording and test
gaps. This round changes no verdict and no exit status.

| Finding | What was changed | Where it is described |
|---|---|---|
| L18-01 a state named like a temporary file of the program is removed by the cleanup and then read as a state | such a name is refused with status 2 before anything is read, and the cleanup never removes the file the run was told to use | "The state file" |
| L18-02 a literal backslash-n in a sentence about `consistency --old` | the text | "`checkpoint`, `consistency`, `trust`" |
| L18-03 (Windows) the message says the other program does not share delete | it says another program may have the file open; a directory at the name has its own words; the stated limit names every open handle | "A write never moves the state over one it did not read" |
| L18-04 the handle tests do not walk the weak path, the evidence merge or the removal of a leftover | both handle tests walk them, with the collector off | tests |
| L18-05 the cleanup of a test is not a leak detector | said in the package comment of `internal/testdir` | tests |
| L18-06 the bound on the strong evidence files is the log's history, chosen by a party without a key | worded as it is | "The evidence of a fork" |
| L18-07 the fallback for a file system that cannot link does not flush the directory | it flushes and reports a failed flush as the other paths do | "The evidence of a fork" |

**Interface changes a caller can see.**

- `checkpoint --state FILE` where the name of FILE (after links are resolved) is one the program gives to its own
  temporary files (`.kingres-state-<12 hex>-<digits>.tmp` or `.kingres-evidence-<12 hex>-<digits>.tmp`) ends with
  status 2 and a message that says to give the state another name; nothing is read, removed or written. Before, such a
  state was deleted by the cleanup of leftovers before it was read.
- On Windows the message of a rename that is refused for the whole wait says `another program may have the file open (a
  backup, a virus scanner, an indexer, a viewer)`, without `without sharing delete`; a directory at the name is
  refused with `the name is a directory, so a file cannot be put there: move or remove the directory, or name another
  file`.
- On a file system that cannot make a hard link, an evidence file created in place is followed by a flush of the
  directory; a failed flush gives the same notes, statuses and `evidence_flushed` false as on the other paths.
- No new flag, no new JSON member, no new exit status.
## Decisions of steps 7 and 18 (proposed corrections 160 onwards of the design)

The design is silent or open on these points; each decision takes the stricter behaviour.

- **160 (7.4, attestation): the inputs.** The proof of the attestation row is a format 1 proof like any other, so it is a file of its
  own (`--attestation`), with the columns file of the row (`--attestation-columns`); the verifier cannot know which table is
  `kingres.ledger_attestation` (correction 111: the tables are found by name, no number is assumed), so the table id is a flag,
  `--attestation-table-id`, and so is `--revocation-table-id`. They are the server's claims and the output says so (the
  signature under a key the auditor pinned is what a row of another table cannot have). `--client-key` is the 16-byte key id in hex.
  `bundle.json` gains `attestation_columns` beside `attestation_file`, and `bundle verify` takes `--attestation-table-id`;
  without it an attestation of the bundle is status 3. Revocation evidence is a directory of `NAME.proof.json` and `NAME.columns.json`.
- **161 (3.7): the attestation row is in the block of its entry.** `ledger_attest` runs in the transaction of the entry, and a transaction
  is in one block, so the attestation row must name the transaction and the sequence number of the entry of the row proof and be
  in the same block (same height and hash). A different block fails (`attestation-entry`).
- **162 (5.5, 5.6): flags of an aggregate member.** `diverged` is status 1 (step `diverged`): the publisher says the chain was rolled
  back or rewritten, and an exit status of 0 would hide that from a script. `stale` is status 3 (the head is old news). `retired` needs a
  chain-retired event that the trust chain authorises and that names the same final height and block hash: no such event is status 3,
  an event signed by no authorised key is status 1, another head is status 1. An aggregate member is checked against the chain log it
  names with the chain's own signed checkpoint, whose size may be larger than the member's (the root of the prefix is shown with a
  consistency proof from the tiles).
- **163 (7.5): the state of `monitor` is a directory.** One file per log in the form of `checkpoint --state` (so every property of the
  reviews holds for it: the identity of a checkpoint is its signed text, the lock file, replacement only of bytes the run read, limits, no
  temporary name), and `monitor.json` for what is not a checkpoint (audited size per log, digest of the text of each event, chains retired). An event is identified by the
  text of its note, never by its signature lines. A run stops at the first finding.
- **164 (4.4, 7.4): sources.** An HTTP source follows no redirect and uses no proxy; `http://` is for this machine only; a server that cannot be
  reached is status 3; an HTTP source cannot list directories, so the epoch of a chain log must be given and the tile directories of older sizes are not
  checked (a note says so). An archive is read under the limits above.
- **165 (3.3, 3.4): the authority of an event.** An event counts when a key signed it that is, at its number, a key of the trust file that the file does not call
  revoked, or a key that an earlier verified event added, and that no earlier event retired or revoked. A key the file calls revoked never
  vouches for an event (so `trust follow` starts from the file the auditor pinned, not from its own output when a revocation is in it).
  Validity windows are not applied to events (an event is a statement about the past). `trust follow` stops at an event no authorised key signed;
  the checks of an aggregate skip such an event, list it, and refuse the retired flag that depends on it. A key-added event for a key that is not pinned is
  signed by an authorised key other than the new one and by the new key itself. A key-retired and a key-revoked event are signed by an authorised key other than the one they are about.
- **166 (7.3): `bundle verify` checks every file of the tile directories**, not only those the checkpoint needs: a partial tile or entry bundle of an older size must
  be the prefix of the tile or bundle of the signed tree, and a file the tree does not have is a failure. The mutation test of every byte found that without
  this, a rewritten partial tile of an older size verified.
- **167 (correction 155): the three tests** (canonical encoding, on the curve, not of small order) are applied to every public key the verifier reads: the pinned
  server keys, the client keys, and the key of a key-added event. Written from the definitions with integers (`internal/edkey`) and tested with the vectors of
  `contrib/kingres_ledger/t/038_public_key_check.pl`.
- **168 (3.3): a revoked key is revoked.** The rule that a revoked key's old checkpoints may stay good when an anchor dates them needs anchors (steps 11 to 15). Until
  then `trust follow` writes `revoked`, and a held checkpoint rests on the rules of corrections 55 and 69.
- **169 (4.10, 7.4): the log leg.** A block at a height the signed chain log does not cover is a failure. `--log-leg aggregate` shows the block through the member of the
  last leaf of the signed aggregate; its root of trust is the signature on the aggregate checkpoint and the output says that no anchor was checked.
- **170 (4.6, 7.4): `bundle fetch`** needs the logs named (`--log`): `logs.json` is optional and has no format yet. A partial tile that is gone is not an error (tlog-tiles).
  Without `--trust` nothing is verified, and the word is `FETCHED`, not `VERIFIED`.

### What the verifier relies on in what `kingresd` publishes (today's format)

- The leaf of an aggregate entry: tag 0x23, `u16` format 1, `u64` seq equal to the entry's index, `i64` microseconds, `u16` count 1 to 512, members of 109 bytes
  (flags, chain id, `u32` epoch, `u64` height, block hash, log root) sorted by the bytes of (chain id, epoch), epoch from 1, flags 3 bits, height below 2^62.
- The leaf of a chain log: the 87 bytes of the block hash preimage; its entry bundle prefix is a big-endian `u16`.
- Origins `<prefix>/c/<chain>/e<n>` and `<prefix>/a/<aggregate>`; files `checkpoint`, `tile/<L>/<N>[.p/<W>]`, `tile/entries/<N>[.p/<W>]`, `events/<6 digits>` numbered from 0 without a gap,
  `keyring.json` (copied by `bundle fetch`, not read by any check: the trust file is the one source of keys).
- An event: `kingres-ledger-event/1`, `seq`, `time` (whole seconds, UTC), `type`, then `name: value` lines; `key-added` has `key`, `fingerprint`, `not-before`; `key-retired` has `key`,
  `max-tree-size` (a number from 1 or `unbounded`); `key-revoked` has `key`, `compromised-since`, `reason`; `chain-retired` has `chain`, `epoch`, `final-height`, `final-block-hash`, `reason`
  (one of `dropped`, `group-removed`, `database-missing`, `chain-replaced`). Its note's key name is the origin prefix.
- A chain-retired event is read from the number it has; the verifier assumes that a number is never given to two texts (the case of review P8-01 is what `monitor` reports).

## Findings of the review of the publication and client signatures (V8) and what was changed

The eighth review (`kingres/docs/reviews/2026-11-19-kingres-verifier-publication-and-client-signatures.md`) found no input that verified when it should not.
Its findings were about what a VERIFIED left out. Each fix has a test that fails without it, named in the table.

**State at step 21.** All twelve findings are fixed in the code and have a test; none is left open. The project README listed four of them (V8-01 to V8-04) as open after they were fixed, and now says so. Step 21 ran every test of the table again and mutated each fix by one line (the revocation by a verified event, the count of events that were verified, the diverged flag in the table of the flags, the ceiling of files of `bundle fetch`, the retired chain that goes on): all are killed. One of them was not at first: the ceiling of files at the write itself, behind the check that precedes the requests, had no test that reached it, and `TestV803TheFileCeilingHoldsAtTheWriteAsWell` does (a ceiling equal to the number of tile and entry files passes the check and is reached by the checkpoint, the last file written).

| Finding | Fix | Test |
|---|---|---|
| V8-01 key events changed nothing outside `trust follow` | one rule for every command: the trust file is narrowed by the verified events before any signature is judged (see "Key events"); the output names the event and the keys that vouched | `TestV801AKeyRevokedByAVerifiedEventVouchesForNothingInAnyCommand`, `TestV801TheOutputNamesTheKeysThatVouched`, `TestV801ARetiredKeyVouchesOnlyUpToTheBoundOfTheEvent`, `TestV801AKeyAddedByAnEventIsNotUsedHere` |
| V8-02 unauthorised events counted as "verified" | the line counts verified and not verified events apart; the not verified are listed and `bundle verify` ends with status 3; the byte-mutation test includes the events | `TestV802AnEventNoAuthorisedKeySignedIsNeverCountedAsVerified`, `TestBundleVerifyRejectsEveryByteOfEveryFile` |
| V8-03 `bundle fetch` without bound | `--max-files`, `--max-bytes`, a check of the size against the ceiling before the first tile, stated in the output | `TestV803AnUnsignedCheckpointOfAHugeSizeDoesNotDriveWrites`, `TestV803TheCeilingsAreStatedAndApplyWithATrustFile` |
| V8-04, V8-05, V8-09, V8-10 flags of a member | one table (see `aggregate`); `diverged` first and always; `stale` kept; a retired chain whose log continues fails; the aggregate leg reports a stale member | `TestV804TheFlagsOfAMemberAreJudgedTogether` (21 rows, each also through `bundle verify`), `TestV810TheAggregateLegOfAProofReportsAStaleMember` |
| V8-06 `bundle fetch` replaces what it holds | held checkpoints, events and tiles are compared first (see `bundle fetch`) | `TestV806FetchComparesWhatItHoldsAndNeverReplacesSilently`, `TestV806FetchComparesTheEvents`, `TestV806KeptTilesAreNotCountedAsFetched` |
| V8-07 missing monitor record taken for a first run | `--init`; a missing, cut or emptied record is status 2 | `TestV807AMissingOrEmptiedMonitorRecordIsNotAFirstRun` |
| V8-08 attestation row not tied to the transaction | insert, same transaction, same block; the README says what the 136 bytes cover | `TestV808...` (three tests; the second is the survivor M20 of the review) |
| V8-11 events after a gap | `events.Walk.CheckGap` against the listing of the source | `TestV811EventsAfterAGapAreReported`, `TestCheckGap` |
| V8-12 consistency from size 0 | the step and the "not proved" line say that nothing was compared | `TestV812ConsistencyFromSizeZeroSaysNothingWasCompared` |
| (round) `logs.json`, flag byte 0x06 | `internal/logsjson` reads format 1 strictly; `bundle fetch` finds the logs by it; 0x06 is a row of the flag table | `TestLogsJSONFindsTheLogsOfAPublication`, the `logsjson` tests and fuzz target |
| (round) memory tests under the race detector | the two resident-memory tests skip when the tests are built with `-race` | `race_on_test.go`, `race_off_test.go` |

**Outcomes that changed** (scripts depend on them):

| Case | Before | Now | Why |
|---|---|---|---|
| a checkpoint signed only by a key that a verified key-revoked event revoked, in `checkpoint`, `consistency`, `aggregate`, `proof`, `bundle verify`, `monitor` | 0 | 1 (2 when no pinned key is left usable) | V8-01 |
| a checkpoint above the `max-tree-size` of a verified key-retired event, signed by that key only | 0 | 1, step `key-size` | V8-01 |
| `bundle verify` with an event that no authorised key signed | 0, "read and verified" | 3, the event listed | V8-02 |
| a source whose `events/` has a gap (directory, `.tar.gz`) in any of the commands above and in `trust follow` | 0 | 1, step `events` | V8-11 |
| a source with a malformed event, in `checkpoint` and `consistency` | 0 (events were not read) | 1, step `publication` | the events decide which keys vouch |
| an unreachable `events/` of an HTTP source, in `checkpoint` and `consistency` | 0 | 3 | same |
| `checkpoint --note F --source S` with a `--source` that cannot be opened | 0 | 2 | the events of the source are read |
| member flags 0x06 without an event | 3 | 1, step `diverged` | V8-04 |
| member flags 0x03 with a matching event | 0 | 3 | V8-05 |
| a retired member whose chain log is longer than its final height + 1 | 0 | 1, step `chain-retired` | V8-09 |
| `proof --log-leg aggregate` of a stale member | 0 | 3 | V8-10 |
| `bundle fetch` without `--log` | 2 | reads `logs.json`; 2 only when the publication has none | the publisher writes one |
| `bundle fetch` of a checkpoint that needs more than 100,000 files or more than 2 GiB | wrote without bound | 2, nothing asked for | V8-03 |
| `bundle fetch` over a held smaller or forked checkpoint, a changed event, an event no longer served | replaced | 1, nothing replaced | V8-06 |
| `monitor` on a directory with no `monitor.json` | 0 (a first run) | 2 unless `--init` | V8-07 |
| `attestation` whose attestation row is a delete, or in another transaction | 0 or 1 at another step | 1, step `attestation-row` | V8-08 |

**What an auditor may conclude from a VERIFIED of these commands.** That the checkpoints (and, where the command reads them, the tiles, entries, members
and proofs) are signed by keys of the trust file he pinned that **no verified event of the same publication revoked or limited below the size of the
checkpoint**; that every event counted as verified was signed by a key the trust chain authorises; that every member flag of the last aggregate round was
judged by the table. **Not** that no revocation exists (an event that is withheld, or sits after the point where an HTTP source stops, is invisible), not the
time of anything (no anchor was read by these commands), and not that a key that an event added is trusted (use `trust follow`).

## Epochs and anchors in `bundle verify` (phase 7, step 21)

```
kingres-verify aggregate     ... [--accept-epoch-closed SEQ ...]
kingres-verify proof         ... --bundle S [--accept-epoch-closed SEQ ...]
kingres-verify bundle verify ... [--accept-epoch-closed SEQ ...] [--require-anchors N] [--worm-url [ID=]URL ...] [--btc-header HEIGHT:HEX80 ...]
```

**What an epoch is.** When the chain of a database is restored to an earlier state, or loses its tail at a failover, the publisher stops that
chain's log (the member is flagged `diverged`) until an operator acknowledges it (design 9.4). The acknowledgement publishes a signed event
`epoch-closed`, freezes the log of the old epoch (its checkpoint and tiles stay served for ever), opens the log of epoch *e + 1* (a different log
with another origin, built from block 0 of the chain as it is now), flags the old member `retired` and `diverged` (flag byte 0x06) in the one leaf
that follows, and leaves it out after that. Each epoch verifies **on its own**. What does not hold is a comparison between them: the log of epoch
*e + 1* is not an extension of the log of epoch *e*, and an auditor who holds a head of the old history must be told, not shown VERIFIED.

**The event** (`events/<6 digits>`, the note of every event, key name the origin prefix) has the type `epoch-closed` and exactly these lines after `seq`,
`time` and `type`: `chain` (64 lower case hex), `epoch` (the epoch that was closed, from 1), `final-size` (from 1: the size of the closed log, the last
one published), `final-root` (64 lower case hex, its root at that size), `common-height` (`unknown`, `-1` for no common block, or a height below the
final size), `kind` (`restore`, `failover-loss` or `other`), `reason` (1 to 200 printable ASCII characters) and `acknowledged-by` (1 to 200). A missing or
extra line, a non canonical number, a second event for one epoch, a `kind` outside the three: status 1, step `events`. An event counts only when an
authorised key signed it (the rule of every event, see "Key events"); one that no authorised key signed is skipped and listed, and changes nothing. The
publisher writes `common-height: unknown` today: finding the highest height whose block is the same in both histories needs the old blocks, which the
database no longer has. The text of a finding says "common height not stated" then, and counts the blocks above the common height only when the event gives it.
**The event is what the operator and the publisher say; the verifier checks that it is signed and that it describes the log it closes, not that the
restore happened as described.**

**What each command does with it.**

| Command | Input | Without `--accept-epoch-closed` | With it (for the number of that event) |
|---|---|---|---|
| `aggregate` | the last leaf has a member flagged diverged (0x04 or 0x06) for which a verified event closed the epoch | 1, step `epoch-closed`: the event's statement (epoch, event number, kind, common height, who acknowledged, the reason, the blocks published) and the sentence to run again | 0: the step `finding epoch-closed` says what was accepted; it is printed, never silent; the member is not checked for a chain-retired event (the event confirms it) |
| `aggregate` | the same, and the event's `final-size` or `final-root` is not the member's size and root, or the signed log of the closed epoch is longer than `final-size` | 1, step `epoch-closed` | 1, the same: the flag never covers a disagreement between the event and the log |
| `aggregate` | a member flagged diverged and **no** verified event (none, or one that no authorised key signed: the output says so) | 1, step `diverged` (unchanged, V8-04) | 1, step `diverged` |
| `aggregate` | the last leaf is of the new epoch alone (the old member is left out) | 0 and a note per verified `epoch-closed` event about a chain of the aggregate: nothing was compared with the old epoch; give `--previous` | the same note (the number accepted nothing: said in a note) |
| `aggregate --previous N` | the held round names a chain of epoch *e* that the last round no longer has, and a verified event closed that epoch | 1, step `epoch-closed`: "epoch *e* was closed by event ... N blocks above the common height existed and are not in epoch *e + 1*"; before saying it, the held head is proved a prefix of the frozen log of the closed epoch (else step `continuity`, a fork, never covered by the flag) | 0 and the finding; the continuity of every other chain is checked as before |
| `aggregate --previous N` | the same and no verified event | 1, step `continuity`, "removed without record" (unchanged) | 1 |
| `checkpoint`, `consistency` inside one epoch | the log is of an epoch that a verified event closed | 0 as before, and the note "finding: this log is of an epoch that a verified event closed, and it is frozen" with the statement of the event | the same |
| `consistency --log chain:ID:eN --old NOTE` | the note is a checkpoint of the same chain at **another epoch** that a verified event closed | 1, step `epoch-closed` (the verified event, its statement, and "different logs: the tree of one is not a prefix of the other"), both checkpoints are verified first. When no verified event closed that epoch the note is not a checkpoint of this log, status 2, as before. A held size or root that contradicts the event is also step `epoch-closed` | there is no flag: nothing is proved, so nothing is accepted |
| `proof --bundle --log-leg aggregate` | the chain of the proof is a member in a round where its epoch is closed (flagged 0x06) | as `aggregate` for that member | as `aggregate` |
| `proof --bundle` | the block is shown in an epoch that a verified event closed | 0, with the note "finding: the block is shown in epoch *e*, which a verified event closed ... this run does not show that the block is in the history that goes on"; when the event gives a common height at or above the block the note quotes it and says that this command did not open the log of the next epoch, so it did not test that the block is in the history that goes on (P7R-02) | the same |
| `proof --log-leg aggregate` | the chain of the proof is of a closed epoch that the last round no longer names | 1, step `member`, with the finding and what to do instead (show the block through the signed chain log, and an aggregate checkpoint you hold from before the closing) | the same |
| `bundle verify` | the bundle holds the log of an epoch *e + 1* of a chain and no verified event closed epoch *e* | 1, step `epoch-closed`: the event was withheld, or this is not the publication; "an event that no authorised key signed" is said when there is one | 1, the same |
| `bundle verify` | a verified event closed an epoch of a chain the bundle holds | 1, step `epoch-closed` with the statement; when the closed log is in the bundle, its signed size and root must be the event's `final-size` and `final-root` (a difference is 1 whatever is accepted); a proof whose block is in the closed epoch (found by trying the epochs newest first) is named in the same failure unless the event states a common height at or above the block: that statement is tested, not believed (P7R-02): the block must be leaf *h* of the signed log of epoch *e + 1* in the bundle, and a block that is not there is 1, step `epoch-closed` ("the event and the logs disagree"), whatever is accepted; without that log in the bundle the note says the statement was not tested | 0, with the step `finding epoch-closed` that says what was accepted and what was compared (the frozen log, and whether the log of the next epoch is in the bundle) |
| `monitor` | a verified `epoch-closed` event in the publication | 0 (nothing changes), and a note "finding: ..." on every poll: the closed log stays frozen, the log of the next epoch is another log, and a state kept for one is not a state of the other | the same |

A chain log of the bundle with several epochs: a proof is shown in the epochs newest first, and the first whose log holds the block at its height is the one
(the proof file carries no epoch). A proof that no epoch holds is the failure it always was (step `log-inclusion`). The aggregate leg of a proof is made only against the epoch
the aggregate names in its last round; a proof of a block of a closed epoch says so in a note and has no aggregate leg.

**What `--accept-epoch-closed SEQ` is.** SEQ is the number of the event, as in the file name `events/000003` (a canonical decimal; `007`, `-1`, an empty value
or a number above 2^31 is status 2). It accepts **that event's finding and nothing else**: it does not make a checkpoint verify, does not cover a log that disagrees with the
event, a fork of the held head, an event that no authorised key signed, a diverged member without an event, or a different event. It never hides the finding: the step
`finding epoch-closed` carries the statement, the number is in `--json` as the step and in `info.epochs_closed_accepted` (`aggregate`) or `info.epochs_closed` (`bundle verify`), and a number
that no finding of the run needed is said in a note ("accepted nothing"), so that a typo cannot pass for an acceptance. Use it to verify the new epoch alone, after reading
the finding; do not put it in a script that is meant to notice a restore.

**`bundle verify`: what else is new.**

- *Anchors.* For every aggregate log in the bundle the sizes and targets are the directories `a/<id>/anchors/<size>/` of the source when it can list them, and the paths that
  `bundle.json` names when it cannot (hints: nothing in the index is believed; a path that is not of the layout is not read and is listed as not checked). Each evidence file is
  checked by the code of `anchors`: the digest is recomputed from the signed aggregate log, the kind is verified from **your** inputs (`--worm-url`, `--btc-header`, the `tsa_roots`
  of the trust file). Without `--require-anchors` every anchor the bundle holds must verify, and one that cannot (no means for it, a pending proof, an evidence file that is absent,
  a size above the signed checkpoint) is status 3. With `--require-anchors N` the count decides: status 3 unless at least N anchors verified, and an anchor that could not be
  checked is a note ("not required, and not verified"). A failure of any evidence file is status 1 in both cases. A bundle with no anchor at all and no `--require-anchors` stays
  as it was. The "not proved" lines about time say what an anchor shows and what it does not (the same as `anchors`): every time is an outside party's word.
- *Epochs*, as the table above says.
- The events line, the list of events that were not verified (status 3) and the checks of every byte are as in V8.

**What an auditor may conclude.** VERIFIED from `bundle verify` with `--accept-epoch-closed`: everything it verified without the flag, and that the history of every closed epoch
the bundle holds is a frozen log whose size and root the signed event states, that each later epoch is introduced by such an event, and that you accepted the cut. **Not** that the
history above the common height was lost for the reason the operator gave, how many transactions were in it (the publication keeps hashes, not rows), or that a block of a closed
epoch is in the history that goes on (only the common height of the event, when it states one, says so, and it is the operator's statement; `bundle verify` tests it against the log of the next epoch when the bundle holds that log). A verification of the new epoch alone is
a statement about the new epoch alone.

**The event `restore-acknowledged`** (P7R-09, design 9.4, ADR 0025). When an operator accepts that the system database of the cluster was restored to an older state than
what was published, the publisher signs this event and goes on from the restored database: the aggregate log and the events are cut there. The verifier reads it as it
reads `epoch-closed` (the lines `generation` from 1, `published-events`, `published-logs`, `published-state-sha256`, `database-events`, `database-logs`,
`database-state-sha256`, `reason`, `acknowledged-by`; a missing, extra or malformed line, or the same generation twice, ends the run; an event that no authorised key signed
is skipped and listed, and explains nothing). Where it reads the events it says so in a note, `finding: the publication holds a verified event 000007 (restore-acknowledged, generation 1, ...)`:
`aggregate`, `bundle verify`, `monitor`, and `checkpoint`/`consistency` of the aggregate log (the chain logs are not cut by a restore and say nothing). And the failure of a comparison
of the aggregate log with a checkpoint you hold (`aggregate --previous`: step `rollback`, `fork` or `continuity` after a fork; `consistency` and `checkpoint --state` of the aggregate
log; `monitor`) names the event: "if the checkpoint you hold is from before that event, this explains the failure". The event is the operator's statement signed by an authorised key.
It explains why a fork exists; it does not show that the cut was honest, and a checkpoint you hold from after the event is not explained by it. Nothing is accepted by a flag and the
exit status of a failure does not change.

**Tests and what is not tested here.** The tests build the whole publication of a restore from the documents (`epoch_test.go`: the aggregate rounds 0 to 3, the event, the frozen log of epoch 1
and the log of epoch 2 with another history, a held checkpoint) and not from the code of `kingresd`; every case of the table above is a test, with the sign of the cases that must stay failures.
The cluster test of step 20 (`ledgerepoch_test.go`) was not run for this step (it needs a cluster and the integration mutex), so **no byte of a real closed-epoch publication was read**: a
real one can be added the way `testdata/publication` was made (a cluster test run with `KINGRES_PUBLICATION_FIXTURES`), and the epoch test of step 20 does not write one yet; the line formats above were taken from the
code that writes the event (`ledgerround/epoch.go`, `ledgersign/publish.go`) and from the aggregate rules in `ledgerround/aggregate.go`. Fuzz target: `FuzzEpochClosedEvent`.

**Mutants of step 21** (one line each, the tests named run; `killed` means a test failed): the member's size and root against the event, `--accept-epoch-closed` accepting any number, the held head not proved a prefix of the frozen log (the consistency proof and the equal size), a later epoch that needs no event, the closed log in the bundle not compared with the event, a refused finding dropped from the failure, the order of the epochs in which a proof is shown, the check that a note is of another epoch, the count of `--require-anchors`, an unmet anchor that is not a note, the note of a closed epoch in `checkpoint`, the note of the new epoch alone, the minimum final size, a second event for one epoch, the bound of the common height, an event that no authorised key signed: 22 of 22 (with those of V8 above) killed.

## Anchors: `anchors` (phase 7, steps 11, 13 and 15)

```
kingres-verify anchors --trust T --aggregate-id HEX [--source S] [--size N] [--target ID ...] [--require worm,tsa,ots|any]
                       [--worm-url [ID=]URL ...] [--btc-header HEIGHT:HEX80 ...] [--now TIME] [--json]
```

An anchor is evidence that the digest of an aggregate checkpoint (SHA-256 of its text, correction 179) existed at a time an outside party
vouches for. The command verifies the aggregate checkpoint as `aggregate` does (the key events apply), then **recomputes the digest from the
signed aggregate log** (for the size of the signed checkpoint, the text of the signed note; for an older `--size`, the three lines for the root of
the first N leaves, proved a prefix of the signed log with RFC 9162 2.1.4.2 from the tiles) and reads the evidence files
`a/<id>/anchors/<size>/<target>.json` (listed from a directory or `.tar.gz`; an HTTP source cannot list, so name them with `--target`). The evidence
file is the cluster's claim and nothing in it is believed: its origin, size and `payload_sha256` must be the recomputed ones (else status 1,
`anchor-evidence`), and each kind is then checked from **your** inputs. The files are read as strictly as the publisher writes them (unknown or missing
member, upper case hex, a time that is not whole seconds ending in Z, non canonical base64: status 1).

| Kind | What is verified | From what | Without that input | Wrong input |
|---|---|---|---|---|
| `worm` | the object key names this aggregate, size and generation; **with `--worm-url`**: the fetched bytes have the SHA-256 and length of the evidence, are a note signed by a key of your trust file (key events applied) of this size whose text is the digest; the store's version, lock mode, retain-until and Last-Modified headers equal the evidence when returned (a header not returned is said to be the cluster's word) | a URL to the object version, anonymous or presigned (no redirect, no proxy, 1 MiB, https or loopback http; the URL never appears in an error) | status 3: a claim that nobody re-fetched is not an anchor | different bytes, version, lock or key, another signer: 1; store unreachable: 3 |
| `tsa` | RFC 3161 token, offline: DER, one SignerInfo, signed attributes (content type, message digest, ESS signing certificate), the signature (RSA PKCS#1 v1.5 of 2048 bits or more, or ECDSA; SHA-256, SHA-384, SHA-512), imprint is the digest, the certificate has id-kp-timeStamping alone in a **critical** extended key usage extension and chains to a root of **your** `tsa_roots`, valid **at genTime**, the `tsa` name if present is the subject; the evidence's gen_time, serial, policy and key hash equal the token's | `tsa_roots` of your trust file | status 3 ("pin the authority's certificate yourself") | any check failing: 1 |
| `ots` | every calendar fragment is walked from the digest (append, prepend, SHA-256, SHA-1); a Bitcoin attestation's message must be the merkle root of the header at its height | `--btc-header HEIGHT:HEX` of a block header you obtained yourself; it must meet its own target (a work check, and not easier than difficulty 1) | status 3 ("commitment tree valid; not checked against Bitcoin"); pending only: 3 | a header whose merkle root is not the tree's, or not mined: 1; an operation not computed here (RIPEMD-160, reverse, hexlify, Keccak-256, unknown): 1, by name; the claim of the evidence not present in any fragment: 1 |

Statuses: 0 when everything asked for verified; 1 when anything failed (wins over everything); 3 when nothing failed and something could not be checked, or
no anchor exists for the size (the checkpoint is not anchored or the evidence was withheld), or the anchored size is larger than the signed checkpoint served
(nothing can be recomputed; the publication may show a log older than the anchor); 2 for bad input. Without `--require` every evidence file read must verify.
With `--require worm,tsa,ots` or `any`, each named kind needs one verified anchor and the other files that could not be checked are notes; a failure of any file is 1 in
both cases. **What each time means:** the WORM time is the store's `Last-Modified` and the store's word; the time-stamp's is genTime and the authority's word, within its
accuracy (printed, not used); the OpenTimestamps one is "no later than" the miner's time field of the header, an error of the order of hours. None is a statement that
the checkpoint is the latest, that no other history was anchored, or (tsa) that the certificate was not revoked (no OCSP or CRL is read); a header is not shown to be on
the best chain. `bundle verify` verifies the anchors of every aggregate checkpoint of the bundle with this same code (step 21, below); `--worm-url` and `--btc-header` are its flags too.

**Tests and what is not testable here.** The evidence of the tests is made by `internal/anchortest` from the documented formats (the verifier module cannot import the
control plane, and no dependency may be added): a local test authority with ECDSA and RSA keys, fragments written byte by byte, evidence files in the publisher's JSON. Not made
here, and named: tokens of public authorities, a WORM object of a real MinIO, a `.ots` file of the reference client, and a **mined header of the block of an anchor** (the
real header of the genesis block is used to test the work check, and the tests that need a header that matches a constructed tree replace the work check by one at an easy
target through the unexported `parseHeader`; the real limit is tested to refuse such a header). Fuzz targets: `FuzzParse` (token), `FuzzWalk` (fragment), `FuzzParse` (evidence file).
Every single-byte change of a token is refused except inside the copy of the root that the token carries, which is never trusted.

**Correction 168 and revoked keys.** The rule that an old checkpoint of a revoked key may stay good when an anchor dates it before `compromised-since` is **not** applied: `revoked`
stays revoked in every command. The design must state first: which anchor kinds may date (a WORM store's clock and a Bitcoin time field are not the same kind of word), whether
the comparison with `compromised-since` is strict, what "covered by the anchor" means for a chain checkpoint (the consistency with the member `log_root` of an anchored aggregate whose leaf names the
chain at a height), and that the anchor itself must be verified with the inputs above.

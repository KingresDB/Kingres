<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->

# Fifth independent review of the anchoring libraries of kingresd

Reviewer: an independent reviewer. Date: 2026-11-18.

## Scope and method

Reviewed: `control-plane/internal/anchor/` and its sub-packages `sigv4`, `worm`, `tsa`, `ots` (about 23,500 lines with
tests), and the two files the round added to the verifier module (`verifier/internal/checkpoint/payload_corpus_test.go`
and `testdata/payload_corpus.txt`), after the fourth fix round, against the fourth review (A4-01 to A4-09), design
section 6 and corrections 60 to 65, 81 to 84, 88 to 92 and 95 to 97 of section 14, and every `doc.go`. Everything was
read from a read-only export and run in a copy under a Linux home directory (project toolchain, local mode, no
network, nothing fetched, `testdata/third_party` absent and untouched). The copy and all my files were removed at the
end. Nothing in the repository was changed.

The reading went in this order: the previous review; `worm/submit.go` whole; `record.go`, `target.go`, `evidence.go`
of the root package; `worm/target.go` and `worm/config.go` (key, `Test`); `ots/upgrade.go` and `ots/target.go` whole,
`ots/bitcoin.go` (floor); `tsa/token.go` (signers, pin, stored verifiers); the new tests of the round in each package;
all four `doc.go` files as a specification for the caller.

## What I ran

- The default suite of the five packages and of the verifier's `checkpoint` package before touching anything: all
  pass, no network, no third-party vectors. Verbose run of the five packages: 533 test runs (273 test functions plus
  subtests), 268 top-level passes, 0 failures, 17 skips (all of them the tests that read `testdata/third_party`, as
  stated). The same with the race detector: pass. Static vet of the five packages: clean.
- 42 one-line source mutants of my own, one test binary at a time (after the first runs, mutants that did not compile
  or whose pattern did not match were redone in a corrected form, and are not counted twice). 38 killed, 4 survived
  (M21, M22, M24, M25 below). The list is in "The tests".
- Probes of my own against the repository's fake store and fake calendars, written only in the copy (about 30 cases):
  the Retry after each form of the metadata refusal; the Test button against a store that drops metadata; 20 to 30
  hostile copies of our bytes; two generations of one checkpoint, a lowered, a skipped and a mismatching generation,
  key collisions for 5 sizes by 30 generations; retention repair in the four mode combinations; a prior evidence with
  contradicted and dropped claims passed to `SubmitWithPrior`; a hostile calendar that fills the aside list plus one
  stale header answer for an honest claim.
- Corpus copies: a verdict flipped in one copy (anchor-side test fails: yes; verifier-side test: only for the verifier
  column), in both copies (both sides fail), the verifier tree absent (anchor-side test skips).
- Fuzzing, 25 s each with two workers and a memory cap, 150 s in all: `FuzzParseFragment`, `FuzzHeader`,
  `FuzzVerifyToken`, `FuzzDER` (tsa), `FuzzParseEvidenceFile`, `FuzzNewPayload`: no failure. `FuzzVerifyToken` reached
  only 142 executions in 25 s (see A5-10).
- Not run: the differential of `ParsePayload` against the verifier's parsers with random mutations (done in the
  fourth round; the round changed no parser rule I could find, and the shared corpus now covers the hand cases).

## The earlier findings

| Finding | State | Evidence |
|---|---|---|
| A4-01 medium, clock fault burns the key, wrong text | Fixed in substance: the text now says that waiting and a Retry find the same object (`worm/submit.go:453-462`, `471-475`), and a per-record way out exists (generation, `target.go:128-142`, `worm/config.go:197-204`). Left: the class stays refused for an object whose verdict is final (A5-09), the caller has no column for generation or start (duties 2, 3). | Mutants M01 (key ignores generation), M02 (VerifyNote ignores it), M12, M13, M18, M03, M04, M23 killed. Probe: generations 0, 1 and skipped 5 give distinct keys, 150 size/generation pairs no collision, `.g1` evidence against `.g10` or `.g0` refused. |
| A4-02 medium, narrow window depends on an unchecked header | Fixed for a fresh write: the echo must equal what was sent, else `no_user_metadata` (`submit.go:594-604`); a present malformed value is `attempt_malformed`, never absent (`429-442`, `568-573`); the evidence carries `attempt` (`674`). The exit promised for the refusal is only partly true (A5-01), and the Test button does not look (A5-02). | M05b (no echo check), M06 (several values accepted), M07 (malformed treated as absent), M11b (attempt not in the evidence), M08 (fresh branch of the window, the old W07) killed. A test fails without each fix. |
| A4-03 medium, a later copy of our bytes decides alone | Fixed up to a bound: the latest failing does not decide, the list is walked, the first of ours that passes completes (`submit.go:196-255`). The bound still lets 25 copies hide our version (A5-06). | M09e (latest decides alone), M26 (error becomes a conflict), M27 (bound removed), M19 killed. Probe: 24 hostile copies: record completes; 25: ends. |
| A4-04 low, merged claim lost after an hour of a wrong source | Mostly fixed: the pending attestation of a merged claim is kept (no request without a source, a request with one), the dropped claim is remembered, the error carries the prior evidence. New holes: A5-03, A5-04, A5-05. | O01 to O06, O10, O11 killed. |
| A4-05 low, floor frozen, `WorkSlack` unbounded | Partly: `MaxWorkSlack` 2^20 refused by `New` (`ots/target.go:216`, mutant O07 killed), `NewestCheckpoint` exposed. The yearly extension and the test that fails on an old table were not done, and the table is already older than the documented limit (A5-08). | Arithmetic: 2^177 times 2^20 stays 2^27 below difficulty 1. |
| A4-06 low, stored verifiers cannot pin; no key hash in evidence | Fixed: both stored verifiers take the pin (`tsa/token.go:862-882`), the evidence carries `signer_key_sha256`, `findSigners` returns every matching certificate and `VerifyToken` tries each. | T01 to T05 killed (pin dropped, hash compare dropped, first candidate only, pin check off, hash not written). Reading: a decoy cannot pass where the right certificate would be refused, since every candidate must pass the same ESS binding, signature, key usage and chain checks and only one certificate has the hash that the signed attribute names (A5-11). |
| A4-07 low, old found object with a short lock passes | Fixed: older than its own period means held to now plus the days and repaired (`submit.go:630-639`). | M10b killed; the test `TestOldFoundObjectIsExtendedToNowPlusTheDays` has four cases including the store that refuses. |
| A4-08 note, name profile refuses honest authority | Documented in `tsa/doc.go` (duties). Unchanged. | |
| A4-09 note, tests | The two survivors W06 and W07 are killed (M10b, M08). The shared corpus now lives in both modules and each side holds its column. New survivors: A5-10. | Corpus probes above. |

## The time of the store, after the round

I re-derived the property with the new rules. For a version this call wrote, the evidence time is within the
tolerance of the clock of the call and the store must have echoed the attempt exactly as sent (`submit.go:557-566`,
`594-604`). For a found version with a valid attempt `a`: within `[max(start, a) - tol, min(now, a + 30 s) + tol]`. For
a found version without the header: within `[start - tol, now + tol]`, and the evidence says `attempt: null`. I could
not find an input that moves a version this package wrote outside the first two windows without a lying store.

What the round added to what a bucket writer can do: nothing in the backdating direction (a forged value can only
narrow). A single malformed foreign copy cannot end a record any more (probe: one copy, record completes); 25 can.
Header spelling: Go canonicalises the name; surrounding spaces of the value are removed by the HTTP reader (so
"surrounding space" is harmless and, by reading, cannot reach the comparison); a value in another case, with an
offset or a fraction, or twice, is refused (tests cover each; "twice" is `attemptExtra` in the fake).

## New findings

Counts: 0 critical, 0 high, 3 medium (A5-01 to A5-03), 6 low (A5-04 to A5-09), 3 notes (A5-10 to A5-12).

### A5-01 (medium) worm: the text and the doc promise that a Retry accepts an object after `no_user_metadata`; that is true only when the store dropped the header

File: `worm/submit.go:484-491` (`noMetadata`), `worm/doc.go` (the paragraph "A store that does not keep user metadata ...
a Retry of that refusal accepts the object under the wide window"), `worm_test` `TestRetryAfterNoUserMetadataStatesNullAttempt`.

What is wrong: the refusal fires for a header that is missing (n = 0) and for one that comes back changed or repeated
(n above 0). The text, the doc and the only Retry test cover n = 0. Probe, against the fake store, first Submit then a
second Submit of the same payload:

- store drops the header: first `no_user_metadata`, second ANCHORED, `found` true, `attempt` null (as the text says);
- store rewrites the value (any other string): first `no_user_metadata`, second `attempt_malformed` (permanent);
- store repeats the header: first `no_user_metadata`, second `attempt_malformed` (permanent).

So for a gateway that rewrites or doubles the header the Retry the text tells the operator to press is refused for
good, and the text of that refusal (`attempt_malformed`) says "another principal wrote it ... anchor under a new key
generation". A new generation writes a new object and meets `no_user_metadata` again, so each press leaves one more
object locked for the whole retention (3650 days by default) and the checkpoint is still not anchored. The record
itself does not spin (permanent, fails once, alert), and the operator can leave only by changing the store or the
gateway, which the first text says, but the second text points the other way.

Also by reading: even in the drop case the evidence after the Retry (`found` true, `attempt` null) cannot be told from
a foreign version, although the narrow rule was applied at the write; the duty "warn when attempt is null" then fires
for every checkpoint of such a store.

How shown: probe (three stores, two calls each); reading.

What a fix must achieve: split the text of `noMetadata` by n (dropped: Retry works and what the evidence will say;
changed or repeated: it does not, and only another store or gateway helps); make `attempt_malformed` name this case
(the version was written by this package if the key's first call was the failed fresh write) or say that a new
generation does not help when the cause is the store; add the Retry test for the changed and the repeated forms.

### A5-02 (medium) worm: the Test button does not exercise the metadata echo, the clock of the store, or the retention permission

File: `worm/target.go:213-243` (the probe writes with `attempt` zero, so no header is sent; the probe-head checks only
the status).

What is wrong: the new rule fails every first Submit on a store that does not keep the header, but `Test` writes the
probe without the header and reads back nothing about it. Probe: a store that drops the header: `Test` says OK on
every check (bucket, versioning, object lock, probe put, probe head, if-none-match; "mode" is a warning as always).
The operator sees green, wires the target, and the first checkpoint fails permanently with `no_user_metadata`. The
same probe does not compare the store's Last-Modified with the clock (a store 10 minutes off passes Test and refuses
every fresh write), and does not check that the credential may call PutObjectRetention (the call a repair needs; see
duty 8).

How shown: probe `Test` against the dropping store (all checks OK); reading.

What a fix must achieve: write the probe with the attempt header, require the echo as `verify` does and add a check
named for it; compare Last-Modified of the probe with the clock within `maxSkew` and warn or fail; optionally try
PutObjectRetention on the probe version in the configured mode (a warning when refused: only the cluster's repair
path needs it). Add tests of the three.

### A5-03 (medium, liveness) ots: one hostile calendar defeats the confirmation interval for every other calendar's claim

File: `ots/upgrade.go:306-322` (the `case len(aside) >= anchor.MaxOTSContradictions: drop = append(drop, c)`),
`362-363`, `dropClaims` (`123-136`).

What is wrong: the rule that makes a header source "wrong once" harmless (a claim is dropped only when contradicted
again at least the interval later) holds only while fewer than 32 claims are aside. When the list is full a claim
contradicted for the FIRST time is dropped at once, its pending attestation goes with it, and the calendar is never
asked again. A calendar answer of 10,000 bytes holds about 400 plausible false claims (each is a few operations and
a Bitcoin leaf at a height inside the plausible range), so one hostile calendar fills the list in one round at no
cost, and then any honest claim that the source contradicts even once (a stale node, one wrong answer) is dropped for
good. Probe (two calendars, the first answers 33 false claims, the second the honest claim; the source returns a
wrong header for the honest height in round 1 and the right header from round 2): round 1 `ContradictedClaims` 34,
`ClaimsAside` 32, `ClaimsDropped` 2 (one false claim and the honest one); rounds 2 to 6 (right source): state
submitted, 32 aside, 0 dropped, no error, no request to the honest calendar (asked once in all), never anchored. The
record then sits until expiry (7 days) or, after the 32 false claims are dropped at the second contradiction an hour
later, fails with `claim_contradicted`; its prior evidence no longer holds the honest fragment (it was removed in
round 1), so Retry with it re-submits that calendar from scratch. No safety property is lost (a false claim is never
anchored without a header that matches it), but the protection added for A3-03 and A4-04 is off for a record whose
calendars include one hostile calendar, and the receipt gives only counts to notice it.

How shown: probe `TestProbeA5AsideFullDefeatsConfirm` (output above); reading.

What a fix must achieve: a claim contradicted for the first time is never dropped on a full list; either keep the
list larger than a calendar's answer can fill, or evict the oldest aside claim of the calendar that has the most
entries (the hostile one), or count the claims per calendar and cap each calendar's share (a calendar over its share
loses its own surplus claims, not another calendar's claim). Test: one hostile calendar full, one honest claim
contradicted once, source right afterwards: the record anchors.

### A5-04 (low) ots: the memory of dropped claims is as large as the aside list, so one answer evicts it

File: `ots/upgrade.go:428-441` (`rememberDropped`), `anchor/evidence.go:36-38` (`MaxOTSDropped` 32).

What is wrong: eviction is oldest first and the bound is 32, the same as the aside list; an answer with more than 64
plausible claims drops the surplus at once (A5-03) and the 32-entry memory keeps only the last 32 of them, so
the first dropped claims are forgotten in the round that dropped them. A calendar with a spare pending attestation
on a branch not above the dropped claims (the cycle of A4-04) then gets the forgotten claims merged again, aside,
dropped, re-merged, for every round until expiry. The probe of the fourth round's author tests the bound (the 33rd
entry evicts the first) but not this consequence. No state shows the cycle except `ClaimsRepeated` going down and
`ContradictedClaims` staying above zero.

How shown: reading of `rememberDropped` and `lookups`; the bound test; the 400-claim answer size.

What a fix must achieve: remember by calendar and bound per calendar (a calendar that exhausts its share is itself
the signal), or refuse the answer of a calendar that returns more than a stated number of Bitcoin leaves; alert on
`ClaimsDropped` above a few per round.

### A5-05 (low) ots: `SubmitWithPrior` throws away what the prior evidence says about contradictions and drops; the doc says the record keeps no evidence, but it does

File: `ots/target.go:319-377` (`SubmitWithPrior` builds a new evidence from the prior's `Digest`, `SubmittedAt` and
`Calendars` only), `ots/doc.go` ("anchor.Record keeps no evidence of its own"), `anchor/record.go:182` (`Evidence`).

What is wrong:

1. Probe: a prior with one contradicted entry and one dropped entry goes in; the new submission's evidence has 0
   contradicted and 0 dropped, and the next `Upgrade` without a header source returns state `claimed` for the claim
   the source had contradicted. A claim that a source contradicted (twice, for a `ContradictedError`) is a plain
   plausible claim again, shown as pending/claimed, with no trace. With a source that is wrong again the cycle simply
   restarts (another hour at least). Judging the claims again is what the doc says; losing the memory, including the
   remembered drops of earlier rounds, is not said.
2. `AfterError` leaves `Record.Evidence` as it was and `Retry` keeps it, so the failed record already holds the
   evidence that `ContradictedError.Evidence` carries (it is the evidence of the call's input). The doc tells the
   caller to store `PriorEvidence(err)` "with the failed record" because the record keeps none: wrong, and a caller
   that clears `Evidence` on failure (a natural thing to do) creates the loss the text warns about. For a
   `PartialError` the record has no evidence (Submit gave none), so storing it is a real duty.
3. The generic `anchor.Target.Submit` has no prior argument; a caller that holds the interface and calls it for a
   retried OTS record starts again from the calendars without noticing. Reaching `SubmitWithPrior` needs a type
   assertion to `*ots.Target`.

How shown: probe (state claimed, 0/0 lists); reading.

What a fix must achieve: carry `Contradicted` and `Dropped` of the prior into the new evidence (or refuse a prior that
has them unless the caller says the source was wrong); correct the doc sentence about `Record`; give the interface a
way to pass the prior (a field of the payload is wrong since the other targets must see the same payload; a separate
optional interface that the scheduler tests for is enough) and say which duty is on the caller.

### A5-06 (low; medium if others can write the bucket) worm: 25 versions with our bytes still end the record, and the error that comes back is the foreign copy's

File: `worm/submit.go:323-379` (`ourVersions`, bound 25, the latest is read twice and counts twice), `214-254`.

What is wrong: the walk is bounded by `MaxVersionsChecked` versions of the key read (newest first), so our version is
read only when fewer than 25 later versions exist. Probe: our version written, then n copies of our bytes with a
malformed attempt placed later: n = 24 completes, n = 25 ends the record with `attempt_malformed` (and 26, 30 the
same). The A4-03 fix and the doc ("a foreign copy ... cannot end the record") are true for one copy and up to 23 more,
not for a principal who writes 25 times. The error is that of the latest, a foreign version, whose text says "another
principal wrote it ... anchor under a new key generation", which sends the operator to spend a generation although
our own valid object exists and is only hidden. `worm/doc.go` states the 25 limit for foreign bytes but not for copies
of ours. Nothing is wrong in a bucket that nobody else writes, which the duties require.

How shown: probe, n from 20 to 30.

What a fix must achieve: say it in the doc and in the text of the error returned after a walk that reached the bound
("the bound was reached, our version may be hidden"); optionally list only the key's versions oldest first for the
search, or raise the bound for the version search separately from the cost of reading (listing is cheap, a GET each
is not: the id of our version is in the evidence of the first attempt if the caller kept it).

### A5-07 (low) worm: the package comment is cut in two; the rendered documentation of the package starts at "The retention floor"

File: `worm/doc.go` (the line after "At most anchor.MaxGeneration." is an empty line outside the comment, before
"// # The retention floor"). The generation section also has the typo "operator.s".

What is wrong: a blank line between two comment lines ends the comment group; only the last group before `package` is
the package documentation. The rendered documentation of `worm` therefore shows only the sections from "The retention
floor" to the end (checked); "What Submit does", "The time of the store" and "The generation of the key" (about 160
lines, the text this round's corrections are written into) are not in it. The wiring notes at the end survive.

How shown: rendered package documentation of `worm` (starts with "# The retention floor"); reading.

What a fix must achieve: join the groups with a `//` line; add a test that the rendered package comment contains the
four headings (or read the source file in a test); fix "operator.s".

### A5-08 (low) ots: the checkpoint table is older than the documented limit, and nothing makes a release fail on it

File: `ots/testdata/headers.txt` (newest row block 912095, August 2025), `ots/bitcoin.go:135-160`, `ots/doc.go` ("What
it does not do": warn when more than a year behind; "a release must not be cut with an old table").

What is wrong: on the date of this review the newest row is about 15 months old, so the stated release rule is already
broken by the tree that is being wired; `TestNewestCheckpoint` pins the function to the table and "does not look at
the clock" (its own comment). A4-05 asked for a test that fails when the table is older than a stated age; that was not
done, nor the extension. Effect: the floor of every height after that block is the difficulty of that block divided by
16 (about 2^75 hash evaluations); it is a fixed number, and the margin between it and the real difficulty grows each
period. A forged header at the floor is still a large cost, and the evidence proves no inclusion (stated), so this is
a weakness to close before a beta and not a defect of the wiring.

How shown: reading of the table and the doc; the test.

What a fix must achieve: add a row from a recent header; add a release-time check (a test that fails when the newest
row is more than 12 months older than the build date, with an override in the release script, not in the tree); the
caller's duty to warn stays (duty 14).

### A5-09 (low) worm and anchor: a final refusal still costs three waits and the way out is easy to misuse

File: `worm/submit.go:453-462` (class refused for `store_time_skew` of an object that carries its attempt),
`target.go:128-142` (`WithGeneration`), `record.go:159-187` (no column for start or generation).

What is wrong:

1. The verdict of an object that carries its attempt is final, the text now says so, but the class stays refused, so
   `AfterError` spends refusals 1, 5 and 15 minutes and fails on the fourth: the alert comes 21 minutes after the
   fault, after three repeated requests that cannot change the answer.
2. `WithGeneration` takes any generation up to 1000 and any non-zero start. It does not require the generation to be
   the caller's current one plus one, nor a start later than the old start. Probes: a lowered generation with a new
   start fails `store_time_early` (the older object is older than the new start); with the old start it silently finds
   the same object again; a skipped generation (0 to 5) anchors a fresh object under `.g5.note`. None of it is unsafe,
   but the rule "by one, with a new start, only by an operator" is only prose.
3. `anchor.Record` has no field for `SubmitStart` or `Generation`. A caller that builds the next payload from the
   record row and forgets the new column sends generation 0 again after a restart and meets the dead key for ever, with
   a text that says to raise the generation.

What a fix must achieve: give `store_time_skew` for a final object its own class (permanent) or let `AfterError` fail
at once on a code the library marks final; add `Generation` and `SubmitStart` to `Record` (or a documented companion
struct) so that the shape of the row is in the library; make `WithGeneration` refuse a lower or equal generation if
the caller passes the old one (`NextGeneration(p, start)`).

### A5-10 (note) tests

- Survivors of my mutants: M24 (the error returned when every version fails is the latest's, not the last version's:
  `if firstErr == nil` removed; no test pins which error comes back), M25 (the context check in the walk), M21 (the
  text of `no_user_metadata` for n above 0), M22 (`HasSuffix` as `Contains` in `VerifyNote`). M24 matters because the
  doc says "the error is that of the latest" and the operator reads it (A5-06).
- Every new test of the round can fail: for each fix of A4-01 to A4-07 I removed or reversed the line and the test
  named above failed (list below). The author's statement that the new tests were not run before the fixes does not
  show in what I removed: no test passed against a mutant of its own subject, except the four survivors, which are not
  tests of the round's headline claims.
- `TestFreshWriteRequiresTheAttemptEchoedAsSent` has a case named "leading-space" that is a trailing value
  ("a x"), not a space around the value; spaces around the value never reach the comparison (the HTTP reader trims
  them), so no case is missing, but the name is wrong.
- `FuzzVerifyToken` made 142 executions in 25 s with two workers: each worker rebuilds an authority with a 2048-bit
  key in setup, so a short fuzz run exercises the verifier almost not at all. Build the seeds once from embedded
  fixtures.
- `TestCorpusCopiesAreIdentical` skips when the verifier tree is absent (seen, with the message). In the repository
  it runs; a control-plane-only checkout compares nothing.
- The corpus is held on both sides (flipped verdicts fail in the verifier module for the verifier column and in the
  control plane for the anchor column); the "stricter on purpose" rule is enforced by name.

The mutants, one line each (K killed, S survived): M01 key ignores generation K; M02 VerifyNote ignores generation K;
M03 `WithGeneration` bound off K; M04 `Check` bound off K; M05b echo check off K; M06 several header values accepted K;
M07 malformed treated as absent K; M08 fresh lower bound off K; M09e latest decides alone K; M10b old object floor
off K; M11b attempt not recorded K; M12 generation not recorded K; M13 generation not passed K; M14 skipped version
not removed K; M15 version bound 25 to 2500 K (no unbounded allocation: the fake lists at most a few hundred
versions); M16b upper attempt bound off K; M17b lower attempt bound off K; M18 key without generation K; M19 error of
the latest not returned K; M21 text S; M22 suffix to contains S; M23 evidence generation bound off K; M24 latest's
error not kept S; M25 context break removed S; M26 conflict instead of the error K; M27 version bound removed K; O01
nothing is remembered K; O02 memory unbounded K; O03 memory not encoded K; O04 kept pending not asked with a source
K; O05 asked without a source K; O06 prior evidence empty K; O07 `WorkSlack` bound off K; O09 no confirmation
interval K; O10 pending above a dropped claim kept K; O11 remembered claims not filtered K; O12 aside bound off K;
T01 stored token verifier ignores the pin K; T02 evidence key hash not compared K; T03 first candidate only K; T04
pin check off K; T05 evidence key hash wrong K.

### A5-11 (note) tsa: what `signer_key_sha256` in the evidence does and does not give

File: `tsa/token.go:870-882`.

The evidence field is checked against the signer key of the token inside the same evidence, so it adds no outside
fixed point: with no pin, anyone who holds a token from any authority under the auditor's roots can write evidence
that passes `VerifyEvidence`, with the matching hash. Only the pin argument is an outside value. The field is useful
to an indexer that does not parse the token, and as the value an operator copies into the configuration. When no
candidate passes, the error comes from the first candidate, which may be a decoy's (its ESS or signature diagnostic),
not the right certificate's. Doc fix only: say that the evidence hash is not a pin.

### A5-12 (note) the repair path needs a permission that nothing checks

An object found older than its own retention period (by design now) and every found object with a short lock needs
PutObjectRetention; a store or a credential that lacks it gives permanent `not_locked` for an object that was written
by this library and is otherwise fine. `Test` does not try the call (A5-02) and the doc lists the permission only
indirectly. See duty 8.

## The duties of the caller

Read from the four `doc.go` files and the code. "Easy to forget" means the API gives no sign.

1. Persist `SubmitStart` before the first Submit call for each (target, aggregate, size, key prefix, generation), as
   one durable value, and pass exactly it to `NewPayload` on every later call, restart, leader change and Retry.
   Wrong if forgotten (recomputed from the creation of the record or the attempt time): a later value makes the object
   the first call wrote refuse for good (`store_time_early`, the key is dead until a generation or a new prefix); an
   earlier value only widens the window for a foreign version. API: `NewPayload` refuses only a zero time; `Record`
   has no field for it. Easy to forget or to misuse.
2. Persist the generation with the record (0 for a new one) and pass it on every call. Wrong if forgotten: the record
   returns to the dead key after a restart (`store_time_early`/`store_time_skew` for ever, text says to raise the
   generation). API: no field, `WithGeneration` does not check "by one". Easy.
3. Raise the generation only by an operator's explicit choice, once, after a permanent refusal of the time
   (`store_time_early`, `store_time_skew` of an object that carries its attempt, `attempt_malformed` of a version this
   package wrote), with a new start from a clock that is right, and persist generation and start in one transaction.
   Never automatically, never because a Retry failed. Wrong if automatic: one more locked object per loop (3650 days
   by default), the bucket fills; wrong if only one of the two values is saved: new start with the old generation
   makes the old object older than the start (dead key), new generation with the old start only widens a foreign
   window. API: nothing stops a scheduler from calling it. Easy.
4. Never call `Record.Retry` automatically (failed and expired records are the operator's), and keep the first
   evidence per (target, log, size); apply every result with `ApplyReceipt` or `AfterError` and treat a refused
   transition as a stale job; run jobs on the leader with the term check; one `Upgrade` at a time per record; two
   leaders on a worm key with different notes make a fork nobody reports. Wrong if forgotten: stale jobs overwrite
   state, double Upgrade, fork. API: the library has no lock and no term; `Retry` is a plain method. Easy.
5. Alerts from the code and the text of the error (`CodeOf`, `Error()`), never from the class. `refused` covers
   credentials and also clocks (`store_time_skew`, `time_skew`, `chain_not_trusted_now`); `submit_start_ahead` is a
   clock or the persisted value; class conflict is used by OTS `claim_contradicted` (a calendar lied or the source is
   wrong) and by `worm_conflict` (other bytes under the key), and `AfterError` names the same alert
   (`ledgerAnchorConflict`) for both. Wrong if from the class: the operator is sent to the credentials for a clock.
   API: `AfterError` returns only the alert kind. Easy.
6. Store the evidence as canonical bytes (text, or jsonb read back through `CanonicalEvidence`; jsonb reorders
   members and the strict parsers refuse that). Wrong: every later `Upgrade`, `VerifyNote`, `CheckEvidence` fails with
   "evidence is not readable" (permanent). Easy with a jsonb column.
7. Build the HTTP client with `NewHTTPClient` from the CA bundle and the credentials as `Secret`; any other client is
   used as given except the proxy, which is removed. Wrong: TLS to nothing (fails safe); a custom RoundTripper that is
   not an HTTP transport keeps its own proxy rules (the doc says so). Hard to get wrong silently.
8. WORM bucket and credential: versioning and Object Lock on, used for nothing else, written by nobody else; the
   credential may Put, Get, GetObjectVersion, ListBucketVersions and PutObjectRetention (the repair); the store must
   keep user metadata (A5-02 means `Test` will not tell you). Wrong: a foreign writer can end a record with 25
   versions, be adopted when it copies our public bytes, or place junk under a future key; a credential without
   PutObjectRetention turns every repair into permanent `not_locked`; a store without metadata fails every first
   Submit. Easy to miss all three; the API shows none.
9. WORM clocks: every server that calls Submit agrees with the store within the tolerance (default 300 s); an NTP step
   or a resumed virtual machine during one call costs that checkpoint its key on that prefix (duty 3). Wrong: dead
   keys and generations, delayed alert (A5-09). Monitor offsets; alert on `store_time_*`.
10. WORM evidence hygiene: warn when `retain_until` approaches (nothing renews it; 3650 days by default, then the
    anchor is unprotected), warn when the evidence says `attempt: null` or `found: true`, never delete versions. Wrong:
    a silent loss of protection years later; adoption of a foreign copy unnoticed. API: the evidence says it, nothing
    pushes it. Easy to forget.
11. TSA: set `signerKeySha256` when one authority is meant, and pass the same pin to `VerifyStoredToken` and
    `VerifyEvidence` in any exporter or auditor tool (nil means any authority under the roots; the evidence hash is not
    a pin); load the roots yourself with `ParseBundle`; know that a name or DER refusal is permanent; raise
    `ledgerAnchorBehind` from the age of the newest anchor; make Submit idempotent per record (every call gets a new
    token, the first stored wins). Wrong: any authority under the roots accepted; an honest authority refused for good
    with a support case. API: the pin is an optional slice in two places. Easy to forget.
12. OTS retry: after a `PartialError` or a `claim_contradicted` failure, retry with `SubmitWithPrior` and the prior
    evidence (the failed record's own evidence is that evidence for a contradicted record; for a partial one the caller
    must have stored `PriorEvidence(err)`), not with the generic `Submit`. Wrong: the proof, the calendars already
    answered and the first submission time are lost. API: the interface has no prior; the retry path must type-assert.
    Easy (A5-05). Know that the contradiction and drop memory is not carried over.
13. OTS scheduling: call `Upgrade` at `NextCheck` for submitted and claimed records (`UpgradeInterval` stretches it);
    a claimed record without a `NextCheck` is never due (`Record.Due`), so enqueue an explicit `Upgrade` for every
    claimed record when a header source is first configured and at each start; decide expiry with `ExpireIfDue` (with
    `ots.Timeout`, `ots.ClaimedTimeout`) for submitted and claimed records alike; give `Upgrade` a deadline of a few
    minutes of its own; never concurrent for one record. Wrong: claims sit as claims for ever after a source is added;
    records never expire. API: easy.
14. OTS alerts and presentation: alert on `ContradictedClaims`, `ClaimsAside`, `ClaimsDropped`, `ClaimsRepeated` above
    zero round after round (a lying calendar or a wrong source, A5-03, A5-04); show a claimed record as claimed and
    never count it toward the minimum of anchors (`State.ContractStatus` does map it to "pending"); write the
    evidence file for a claimed record with status "claimed"; implement `HeaderSource` over a node you trust or leave
    it out; call `NewestCheckpoint` at start and warn when its block time is over a year old (it is already, A5-08).
    Wrong: a false claim is shown as an anchor (only if the caller maps states itself); a hostile calendar goes
    unseen. API: the counts are on the receipt only, not in the state.
15. Build payloads only from notes this cluster signed: `ParsePayload` checks the shape, not a signature. Wrong: an
    outside note is anchored and the evidence looks like ours. API: easy.
16. Fixed-cost duties of the counters: store `Attempts`, `Refusals`, `Retryables`, `FailingSince` with the record and
    pass `Limits` from the settings; reset by the library's methods only. Wrong: refusals never reach the fourth, a
    record retries for ever. Moderate.

Never automatic: generation raise, `Retry`, change of `SubmitStart`, deletion of anything in the bucket, `Submit` of an
OTS record that holds a prior evidence.

## Verdict

- sigv4: fit to be wired (not changed this round; its tests pass).
- tsa: fit to be wired. Set the pin if one authority is meant, and pass it in the auditor path (duty 11). Nothing
  found that needs a library change.
- worm: fit to be wired in a bucket that nobody else writes, after A5-01 and A5-02 are done: both are a text and a
  probe in `Test`, small and local, and without them the first symptom of a store that does not keep metadata is a
  failed first checkpoint. A5-07 (the cut package comment) should go with them. A5-06 and A5-09 can travel with the
  wiring as documented duties.
- ots: fit to be wired without a header source (it makes no request for a merged claim and ends in "claimed"). Fit
  with a source only with A5-03 fixed or with the alert on `ClaimsDropped` and `ClaimsAside` in place and a calendar
  list that the operator trusts (the defect needs one hostile configured calendar and one stale header answer; it
  costs liveness, not safety). A5-04, A5-05 and A5-08 can travel with the wiring.

Another review round of the libraries is not needed before the wiring if A5-01, A5-02, A5-03 and A5-07 are fixed with
tests that fail without the fix (mutants as above); the next reviewer of the wiring should re-check those four, the
retry paths of duties 3 and 12, and the state transitions of a record through them. If the wiring starts before
those four, the three medium findings must be on the wiring's list as known.

## What I did not check

- A real S3, MinIO or Ceph (whether they return the user metadata of a HEAD exactly as written, and in which case),
  a real TSA, real calendars, a real node; the network behaviour of the standard HTTP client against slow-loris
  responses beyond reading the limits.
- `sigv4` beyond running its tests; the OTS proof serialiser and the CMS and DER reader of `tsa` beyond running the
  tests and 25 s of fuzzing each; the name profile; the Redactor beyond reading the format strings that carry store
  or calendar text (two uses of `%v`, both on library errors).
- The differential run of `ParsePayload` with random mutations against the verifier's parsers (done in the fourth
  round, not repeated); only the corpus and its two copies.
- Concurrency of two leaders on one key beyond reading; a zombie put; a chain with two certificates of one key in a
  real token (the test builds one with a decoy).
- The console, scheduler and alert wiring, which do not exist yet; the real size of a calendar answer and how often a
  public calendar sends more than a few Bitcoin leaves (the 400 in A5-03 is the arithmetic of the 10,000-byte limit).

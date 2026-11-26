<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->

# Third independent review of the anchoring libraries of kingresd

Reviewer: an independent reviewer. Date: 2026-11-18.

## Scope and method

Reviewed: `control-plane/internal/anchor/` and its sub-packages `sigv4`, `worm`, `tsa`, `ots` (about 19,500 lines
with tests) after the second fix round, against the second review
(`docs/reviews/2026-11-18-kingresd-anchoring-libraries-second.md`, A2-01 to A2-12), the design
`docs/design/ledger-phase7.md` (section 6, corrections 60 to 65 and 81 to 84), each package's `doc.go`, and the
verifier module (`verifier/internal/signednote`, `verifier/internal/checkpoint`). Everything was read from a
read-only export; all runs were in a copy under a Linux home directory (toolchain of the project, local mode, no
network, nothing fetched, the third-party vector directories are absent and were not touched). The copy and all
my files were removed at the end. Nothing in the repository was changed.

What I ran:

- The whole suite of the five packages in the copy before touching anything: all five packages pass (219 test
  functions in the tree; the default run needs no network and no third-party vectors).
- 54 source mutants of my own (one-line edits, listed by effect in the table and in "The tests"): 48 killed,
  4 survived, 2 did not compile (both redone in another form in the second batch).
- Probe tests of my own, written against the real code and the fake store and scripted calendars of the
  repository's tests: 10 worm probes (clock of the store against the first attempt, adoption of a foreign object,
  wrong first attempts, retention, mode, printing of a target), 2 ots probes (a header source that is wrong once;
  a self-replenishing hostile calendar with a flapping header source), 1 tsa probe with 22 pairs of names.
- A differential run of `NewPayload`'s parser against the verifier's real `signednote.Parse` and
  `checkpoint.Parse`: 24 hand-made notes and 400,000 random mutations of a good note.
- Fuzzing, about 160 seconds in total at parallelism 2: my own `Upgrade` fuzzer (arbitrary calendar answer bytes
  and header bytes, with and without a header source; about 48,000 executions; invariants: ends in time, never
  anchored without a header source, evidence parses, the state machine accepts every receipt returned),
  `FuzzSameName` (about 630,000), `FuzzVerifyToken` (about 2,900), `FuzzParseS3Error` (about 366,000),
  `FuzzNewPayload` (about 186,000), `FuzzParseFragment`. No crash, no hang, no invariant broken.

## The earlier findings

| Finding | State | Evidence |
|---|---|---|
| A2-01 high, retry defeats the store-time bound | partly fixed; the window is wider than documented (A3-01), and the bound can be switched off by the caller (A3-02) | `worm/submit.go:84-97` (first attempt, floor), `433-445` (window), tests `worm/second_test.go:35-76`. Mutants "lower = now-8d", "early check off", "ahead check off", "first_attempt_ahead off", "tolerance doubled": killed. Mutant "fresh bound max(now-skew) removed": SURVIVED (no test of the fresh rule being tighter than the existing one). |
| A2-02 medium, first claim ends collection | fixed | `ots/upgrade.go:205-313` (claimed keeps asking, header contradiction dropped, fallback to submitted), `anchor/record.go:127-137`, `ots/target.go:329-347`. Mutants "claimed does not ask", "contradiction is conflict always", "claimed timeout factor 1", "no expire for claimed", "no claimed to submitted", "claimed due without next check", "transient header taken as contradiction", "fallback off", "pending kept after merge": all killed. New problems in A3-03, A3-06, A3-07. |
| A2-03 medium, recovery stops after 8 days | fixed | no age limit any more (`worm/submit.go:433-445`); `TestRecoveryPathWorksAfterAnyDelay` kills "existing judged as fresh" and "wide window". Old age problems have their own codes (`store_time_early`, `store_time_skew`). Depends on a sound first attempt (A3-02). |
| A2-04 low, payload limits | partly fixed: the constants are the verifier's, the shape is not | `anchor/target.go:45-60,115-225`. Numbers: mutants for origin 256, 33 lines, line 1025, size 2^63, size check off, line check off, count check off, note 64 KiB: all killed. Shape differences remain in both directions (A3-05). |
| A2-05 note, verifier has no reader | unchanged | a search of `verifier/` for `object_sha256`, `token_der_b64`, `fragment_b64`, `header_checked` finds nothing |
| A2-06 low, tests blind to limits | fixed for the limits; two items not re-checked | mutants `PerFragmentLookups` 1000, `MaxParallel` 1000, `MaxHeaderChecks` 1000, `MaxListBody` 2^30, `MaxListPages` 500, `MaxVersionsChecked` 2500, `badOwner` removed: all killed (tests now use literals, e.g. `worm/second_test.go:241`). Not re-checked: the HEAD-answers-another-version test beyond reading it, the Redactor second loop. |
| A2-07 low, wrong citations | fixed with a remainder | no "70 to 76" left; section 6 carries "(Replaced by correction N)" marks. Remainder: package docs cite "60 to 65" (`anchor/doc.go:5`) and not 81 to 84, which are what this round implemented; the 6.5 step 3 mark points to 60 only, not on to 82 (A3-13). |
| A2-08 low, one permanent round ends a record | fixed | `ots/upgrade.go:88-98`; mutant killed (`TestRoundOfPermanentFailuresIsRetryable`). |
| A2-09 low, strict TSA name check | fixed in intent; the new rule is too loose in places (A3-09) | `tsa/names.go:23-236`, `tsa/token.go:735`. Mutants "bytes only", "no case fold", "no space fold", "name check off", "SAN directoryName off", "subject off": killed. Survivors: "string type mismatch allowed", "odd-length BMP allowed", "Teletex decoding removed". |
| A2-10 low, short retention accepted | fixed, with the floor taken from the caller's first attempt (A3-02) | `worm/submit.go:462-476`, `second_test.go:182-226`; mutants "repair never", "tolerance 8 days", "asked = retain": killed. |
| A2-11 low, class "refused" for time problems | fixed as designed (class stays refused, code and text say clocks) | `worm/submit.go:366-381`, `tsa/token.go` (`failRefused(CodeTime ...)`); mutants flipping the classes: killed. The alert text still depends on the caller using the code (documented). |
| A2-12 note, global state | fixed | `anchor/globals_test.go` (list of allowed globals; mutant "add a global" killed); `headerWork` and `fragmentLimit` are unexported options of `Target` (`ots/target.go:135-142`). Remainder in A3-12. |

## New findings

Counts: 0 critical, 1 high, 3 medium, 6 low, 3 notes (A3-01 to A3-13; A3-01 is the only high).

### A3-01 (high) worm: the "first attempt" is the creation of the record, so the residual window is the age of the record, and a second attempt is judged by a wider rule than the first

File: `worm/submit.go:84-97`, `433-445`, `worm/doc.go:34-37`, design correction 81 (last sentences).

What is wrong: for an existing object (a second attempt) the earliest Last-Modified accepted is
`first - tolerance`, where `first` is the creation time of the record (`Payload.FirstAttempt`); for a fresh write
it is `max(first, now) - tolerance` (`submit.go:433-439`). The documented residual says "a store behind by more
than the tolerance and less than the duration of the first submit". The real window is the age of the record at
the time the object was first written (plus the tolerance), and the operator can widen it at will:

1. Any transient failure after the put: the put succeeds, the HEAD of the read-back fails (5xx, cut
   connection), the error is retryable, the next Submit finds the object by 412 and judges it by the existing
   rule only. Probe: first attempt 30 minutes ago, store 20 minutes behind, one HEAD answers 500: first Submit
   fails retryable, second Submit is anchored with `last_modified` 20 minutes in the past. No error ever named
   the clock.
2. Refused for time, then Retry: probe with first attempt 2 hours ago and the store 1 hour behind: the first
   Submit is refused (`store_time_early`, permanent), the record fails, the operator presses Retry (which keeps
   the first attempt, design 81), the second Submit is anchored with the evidence time one hour in the past. The
   error text (`submit.go:379`) tells the operator to change the key prefix; a plain Retry works silently. A record
   that sat failed for five days (credentials, a holiday) then gets exactly the A2-01 hole back for a store five
   days behind: the object is written fresh and refused, the Retry accepts it.
3. `worm/doc.go:34-37` says "the second attempt does not judge the object by a wider rule than the first". That is
   false by the code above; the existing-object rule is wider by `now - first`.

The evidence time is then a time the store stated but the cluster should not vouch for (the store was behind the
true time of the write by up to the age of the record). The ahead direction is harmless in effect: a store that
is ahead by up to about 26 minutes is accepted after the refusals at 1, 5 and 15 minutes because the clock
catches up (probe: store 20 minutes ahead, accepted 16 minutes later with an evidence time 4 minutes ahead of the
clock); the stated time is then later than the truth, which only weakens the bound.

How shown: three probes against the repository's fake store (cases 1 and 2 and the ahead case above), and a
mutant (`lower` for an existing object computed as for a fresh one) that is killed only because it breaks
crash-recovery tests; mutant "fresh bound max(now-skew) removed" survives, so no test pins the difference.

What a fix must achieve: the lower bound of an existing object must not depend on how long the record has
existed. Options: define the first attempt as the start of the first Submit call that reached the store (persisted
before the call) instead of the creation of the record; or have Submit return, with every refusal that happened
after a write, a marker that the caller must store and pass back (the design already says so) and make the
library refuse an existing object whose time is earlier than the earlier attempt's marker; or make a refusal for
`store_time_early` terminal for that key (Retry must change the key prefix). Correct `worm/doc.go` and design
81. Add tests: an old record with a store behind by less than its age, once through a transient read-back
failure and once through refusal plus Retry, both of which must be refused; and a test that a fresh write is judged
by `now` and an existing one by the first attempt (kills the surviving mutant).

### A3-02 (medium) worm: a wrong first attempt switches the time and retention checks off, or burns the key

File: `worm/submit.go:84-97`, `433-439`, `462-476`; `anchor/target.go:131-141`, `231-233`.

What is wrong: the library trusts `FirstAttempt` completely and checks only that it is not zero and not later than
the clock plus the tolerance.

- Far in the past (probe, `time.Unix(1,0)`): an object written in 2020 is accepted; its time is the evidence
  time. The same value also removes the retention check: the floor is `first + days`, which is in the past, so
  the minimum becomes "now" (`submit.go:466-469`): probe with a configured retention of 3,650 days and an existing
  locked version that expires in 2 minutes: accepted, no repair, `retain_until` two minutes ahead.
- Different between retries: later than before makes the check stricter (the same object is then refused as
  `store_time_early`, permanently); earlier loosens it.
- Later than the real one (probe, first = now plus 4 minutes, object written 3 minutes ago): refused permanently
  with `store_time_early`. A caller that passes the time of the current attempt instead of the record's creation
  turns every crash recovery older than 5 minutes into a permanent failure and burns the key.
- A party that can write the bucket can pre-write our key with our bytes (the note is public once published) after
  the record exists: the object is adopted if its time is after `first - tolerance` and, because the retention floor
  is derived from `first`, with a retention up to the age of the record shorter than configured.

How shown: probes above; reading.

What a fix must achieve: derive the retention floor from the store's own timeline (the object's Last-Modified plus
the retention days, less the tolerance and the time tolerance), not from a caller-supplied time; bound `first` by
the clock (for example not before `now - Limits.MaxAge - retry margin` unless the caller states a recovery) or
refuse a first that differs between calls for the same key if the caller keeps it; test with first attempts at
the edges (zero, 1970, a year ago, now+4m, now+6m).

### A3-03 (medium) ots: a contradicted claim destroys the calendar's proof for good; a header source that is wrong once costs evidence, with no trace

File: `ots/upgrade.go:245-265` (loop), `153-173` (`dropClaims`), `427` (`removePending`), `270-273`; `ots/claims.go:222-245`.

What is wrong: when a claim is merged, the pending attestation it replaces is removed (`upgrade.go:427`); when a
header contradicts a claim, the claim is dropped and a fragment with nothing left is removed
(`dropClaims`). After that nothing stored can make that calendar be asked again. A single wrong answer of the
header source (a header from a stale fork or an orphaned block during a reorganisation of the last blocks, a node
that is still synchronising, a node of another network such as testnet whose difficulty-1 headers pass the work
check, a mix-up of heights) therefore deletes a good proof. Probe: two calendars, calendar H has a good claim,
the source returns a stale-fork header for H's height once: the record falls back to `submitted` and the
calendar H is gone from the evidence (1 calendar left). The next round, with the source right again, cannot bring
it back (probe round 2: still `submitted`, 1 calendar). With one calendar the result is class conflict and a
failed record. The fallback to `submitted` produces no alert and leaves no note of the contradiction; the
contradiction text is used only for the conflict error. There is no depth (a claimed block is accepted
as contradicted on one answer for a block that may be 1 block deep).

How shown: probe `one bad header` (above) and reading.

What a fix must achieve: a contradiction must not be irreversible on one answer. Keep the dropped attestation
(quarantined, with the header that contradicted it) and ask the calendar again at the next round, or keep the
pending attestation until two rounds, separated by time, agree; record the contradiction in the receipt so that
the caller can alert (a calendar that lied or a source that is wrong are both operator matters); consider a
confirmation depth when the header source can say how deep the block is.

### A3-04 (medium) ots: the proof-of-work check accepts difficulty 1, so "header_checked" is not evidence of a block

File: `ots/bitcoin.go:19`, `75-100`; `ots/claims.go:222-245`.

What is wrong: `CheckHeader` requires only that the header hashes below its own target and that the target is not
easier than the main chain's limit (difficulty 1). A header of difficulty 1 costs about 2^32 hash evaluations, which
is minutes on a CPU and nothing on a GPU, against about 2^77 for a block of today. A header source that is wrong or
hostile, a rogue operator writing evidence, or a calendar that can reach the source can make a fabricated
header with any merkle root and any time field in the accepted window (submission minus 3 hours to now plus 2
hours; `submitted_at` is the cluster's own assertion). `CheckEvidence` then reports `header_checked` from the
evidence alone. The docs say whether the header is on the best chain is for the auditor, but the status name,
the contract status "anchored" and the value of the stored header suggest more than the check delivers.

How shown: reading (`bitcoin.go:75-100`: no floor above the limit); nothing in the tests mines a header of
difficulty 1 to show that it passes, which is the point.

What a fix must achieve: a work floor tied to the height (the checkpoint table already gives heights and times;
add the bits of each checkpoint and require a target no easier than a stated fraction of the nearest earlier
checkpoint's, with a margin for difficulty falls), or, at least, report the difficulty in `HeaderResult` and say in
the docs of `anchor` and `ots` that `header_checked` means "a header of at least difficulty X that matches, not
yet shown to be on the chain", and require the verifier to fetch the header itself. Add a test with a mined
difficulty-1 header at a height of 950000 that must be refused.

### A3-05 (low) anchor: the payload shape still differs from the verifier's, in both directions

File: `anchor/target.go:115-225`, against `verifier/internal/signednote/signednote.go:79-89,155-215` and
`verifier/internal/checkpoint/checkpoint.go:60-130`.

What is wrong: the numbers are now the verifier's, the rules are not. Accepted by `NewPayload`, refused by the
verifier (each proved in a differential probe against the verifier's real parsers):

- a signature name with a plus sign or a Unicode space (`ParsePayload` only requires a non-empty name,
  `target.go:194-198`; the verifier's `ValidName`);
- a signature of fewer than 4 decoded bytes (the verifier needs the 4-byte key id);
- non-canonical base64 of a signature or of the root (non-zero trailing bits; `base64.StdEncoding.DecodeString` is
  not strict, `target.go:199,214`; the verifier's `DecodeBase64` is strict and re-encodes);
- two signature lines with the same name and key id;
- a note of size 0 with any root (the verifier requires the root of the empty tree).

Refused by `NewPayload`, accepted by the verifier: an origin or a signature name with a DEL byte (0x7f)
(`target.go:120,171`; the verifier refuses only below 0x20); extension lines after the root (the verifier allows up to
16, `target.go:204` demands exactly three lines); a note over 16 KiB up to the verifier's 64 KiB. These are
stricter on the anchor side, which is safe, but the docs call the rules "the same".

Over 400,000 random mutations of a good note the two parsers disagreed 353 times in the second direction (a
class of one: DEL) and 311 times in the first (the classes above). UTF-8 edge cases of the origin agree: a
C1 control other than U+0085 is accepted by both, U+0085 and U+00A0 are refused by both, invalid UTF-8 is refused by both.

How shown: differential probe.

What a fix must achieve: one parser's rules. Reuse the verifier's checks for the signature lines (name, length,
strict canonical base64, duplicates) and for the root and the empty tree; decide about DEL and extension lines
on purpose and say so in the doc; put a shared corpus of accepted and refused notes in both modules and test each
against it (the current test compares only the constants, `anchor/payload_limits_test.go:132-188`, and skips when the
verifier source is not beside it).

### A3-06 (low) ots: a hostile calendar can keep a record flapping without any alert, at a cost of one request per round

File: `ots/upgrade.go:226-313`, `anchor/record.go:127-137`.

What is wrong: a calendar that answers every lookup with a false plausible claim and a new pending attestation
for itself, together with a header source that is unavailable in one round and answers in the next, makes the
record alternate between `claimed` and `submitted` every round until it expires (probe: six rounds, states
claimed, submitted, claimed, submitted, claimed, submitted; one request to the calendar per round; every
`Upgrade` returned a valid receipt that the state machine accepts). The expiry from the creation of the record
(`ExpireIfDue`) ends it; nothing in between alerts, because the conflict error is returned only when nothing is
left that could deliver (`upgrade.go:270-273`). The contract status is "pending" in both states. A calendar can
also steer the lookups of its own fragment (up to 4 per round) at another configured calendar by naming that
calendar in pending attestations; the amplification is bounded by the per-fragment budget.

No unbounded loop: the loop at `upgrade.go:245-265` removes at least the attestation of each bad claim in every
iteration (a bad claim has no problem of its own, so its leaf is matched by calendar, height and message), the header
cache is kept across iterations, and the fuzzer found no hang. The worst round is bounded (16 fragments times 4
lookups, 8 in parallel, 20 s each, plus 8 serial header requests of 20 s: about 5 minutes) but `Upgrade` has no
deadline of its own; it takes the caller's context.

What a fix must achieve: count contradictions and return them (A3-03); say in the wiring notes that `Upgrade` needs
a caller deadline of a few minutes.

### A3-07 (low) anchor: a state with no exit that the contract calls "failed", and a claimed record that fails before it can expire

File: `anchor/record.go:65`, `109-141`, `435`; `ots/upgrade.go:303-307`.

What is wrong: (a) `expired` has no event that leads out, while `ContractStatus` shows it as "failed" and the console
will offer Retry for "failed"; `Record.Retry` refuses an expired record (`record.go:138-140`). The operator cannot
retry an expired OpenTimestamps record, and the design is silent. (b) A claimed record whose calendars all fail
(and no header source answers) is returned by `Upgrade` as an error (`upgrade.go:303-307`), the record counts
failures, and after 200 attempts or 7 days of failure it is `failed` with an alert, long before the claimed
timeout of 28 days that the docs advertise ("expires after its own, longer timeout"). This is defensible but
contradicts the doc. The table itself is total: every state and event pair is either in it or refused, the
terminal states accept nothing, and the golden-table and mutant tests kill the removal of any claimed entry.

What a fix must achieve: state in the doc and the design what the operator can do with an expired record
(Retry from expired, or a new record), and which of the two limits ends a claimed record first.

### A3-08 (low) worm: what the evidence says about an object that someone else wrote; and the parties that can write the bucket

File: `worm/submit.go:165-218`, `303-327`, `254-256`, `450-452`.

What is wrong or worth stating:

- Adoption: an object with our key and our bytes that someone else wrote is adopted when its time and retention
  pass the rules; the evidence says `recorded_by: cluster` and does not say that the version was found rather than
  written (probe: a foreign unlocked version of the note is adopted, the retention is applied by the cluster,
  evidence indistinguishable from a fresh write). The time is the store's, so the foreign writer gains nothing in
  time terms that a wrong store clock would not give (A3-01, A3-02), but a verifier cannot tell the cases.
- A party with write access can make the lookup fail by adding 25 versions of other bytes after ours: the search
  reads the newest 25 only (`MaxVersionsChecked`), so a record that must go through `existing` ends in class
  conflict with the critical alert. The same party can write junk under the key of a future size and cause a
  conflict. Inherent to a bucket that others can write; worth a line in the wiring notes (give the cluster a
  bucket of its own).
- Retention repair asks for one permission, `s3:PutObjectRetention`, on the found version, never the bypass
  permission; the repaired date is always later than the existing one (the asked date is `now + days`, the
  existing one is below `max(floor - tolerance, now)`), so the store is never asked to shorten. A store that
  refuses gets the text "exists without a lock" (`submit.go:254`) even when the object has a lock that is only
  too short or has expired; the advice stays right, the first words do not.
- Mode: an existing COMPLIANCE version with the configuration at GOVERNANCE is refused permanently (probe),
  although it is the stronger lock; an existing GOVERNANCE version with the configuration at COMPLIANCE is also
  refused and not upgraded, although the store would accept compliance on top of governance. Safe, but a changed
  configuration then needs a new key prefix.

What a fix must achieve: a field in the evidence (or the doc) that a version may have been written by another
principal; word the not-locked text for a short lock; accept a stronger mode than configured, or say it is
refused on purpose.

### A3-09 (low) tsa: the name comparison treats unequal names as equal

File: `tsa/names.go:149-203`.

What is wrong: probes of `sameName` over encoded names: the following pairs are reported the same.

- Two PrintableString, IA5String, NumericString or VisibleString values that differ only in invalid bytes
  (`a\xff` and `a\xfe`), because those string types are not validated and `strings.EqualFold` compares invalid
  bytes as U+FFFD; the same for BMPStrings with different lone surrogates; and such a value equals a UTF8String of
  U+FFFD.
- Case folding by `EqualFold` (full Unicode simple folding): the Kelvin sign (U+212A) equals "K", the long s
  (U+017F) equals "s".
- `strings.Fields` folds every Unicode space: "My TSA" equals "My" NBSP "TSA" and "My" U+3000 U+2003 "TSA"; an empty
  value equals a value of blanks.
- TeletexString is read as Latin-1: the byte 0xE9 (which is not e-acute in T.61) equals UTF8 e-acute.
- The check is about whether the token's `tsa` name corresponds to the signer's own certificate, and the
  name is neither recorded in the evidence nor compared with the configuration. Not equal, as intended: a
  different attribute type, a different RDN order, a multi-valued RDN against two RDNs, a sharp s, a dotless i,
  a Cyrillic letter, a zero-width space, a NUL.

Can the looser rule make a token of another authority under the same root pass where the configuration pins one
authority? No, because nothing pins one: `Config` has the CA bundle and the policy OID only; the verification of the
name is a consistency check between a token and its own certificate, and a token of any authority under the
configured roots with a matching own name is accepted before and after this round. If one authority is
intended, the configuration needs a pin (signer certificate fingerprint or subject) that this package does not
offer. A token of another authority passes the same as it did.

How shown: probe of 22 pairs; the survivors of the mutants "string type mismatch allowed", "odd-length BMP
allowed" and "Teletex decoding removed" show that the string types and their edge cases are untested.

What a fix must achieve: refuse (not equal) a value that is not valid in its string type (invalid UTF-8, lone
surrogates, odd lengths), fold case for ASCII only or by a stated profile and collapse only ASCII white space (or
use the stringprep profile that RFC 5280 names), compare TeletexString by bytes unless it is ASCII, and add the
tests for each string type; add a signer pin if one authority is meant.

### A3-10 (low) tests: what cannot fail or is missing

- `TestSkewedStoreIsRefusedOnEveryAttempt` (`worm/second_test.go:35-76`) builds its payload with `first = now`
  (`worm/worm_test.go:31-40`), so it cannot show A3-01. No test has an old first attempt with a store behind by
  less than its age; no test fixes the fresh rule against the existing rule (surviving mutant).
- No test for a first attempt at the edges (zero is tested; 1970, a year ago, later than the real one are not).
- `tsa`: no test for TeletexString, an odd-length BMPString, or a string-type mismatch (three surviving mutants);
  `FuzzSameName` checks no property about folding.
- `anchor/payload_limits_test.go` compares the numbers only; no negative case for the shape differences in A3-05; both
  it and `globals_test.go` skip when the sibling source is missing, so a checkout without `verifier/` runs
  green with the check absent (acceptable, but say so in the test names).
- Nothing mines a difficulty-1 header (A3-04), nothing tests a header source that is wrong once (A3-03).
- The default run needs no network and no third-party vectors: confirmed (the vector tests skip).

### A3-11 (note) network, secrets, proxy, requests

- Everything read from the network has a limit and the fuzzers found no hang or panic (above). A worst `Upgrade`
  round is about 5 minutes without a caller deadline (A3-06).
- No secret in an error or in `%v`, `%+v` and `%#v` of a worm target (probe: 16 bytes, no key); credentials are
  `anchor.Secret` and the types that hold them have `Format`; the Redactor second loop is not re-checked by me
  (open from A2-06).
- No proxy: `anchor/httpx.go:78,95-108` clears `Proxy` on every transport it clones, including a nil one; a
  caller's own non-`*http.Transport` round tripper is kept, as documented. The HTTP client is built once per target,
  not per request (no leaked pools).
- Paths that should make no request make none: a bad payload, a first attempt ahead (repository test: 0 requests),
  `Upgrade` on an anchored receipt, a worm `Upgrade`, and an `ots` `Upgrade` of a claimed receipt with nothing pending
  and no header source (test and reading).

### A3-12 (note) global state

`TestNoPackageLevelState` holds. What remains: `ots.TagPending` and `ots.TagBitcoin` are exported arrays and the
`ots.Err...` sentinels are exported variables, so any caller can assign them (`ots/proof.go:12-15`,
`ots/op.go:159-172`); nothing in this code does, and the test lists the arrays by name. Make the tags functions
or unexported if callers never need to write; the sentinels are the usual Go pattern.

### A3-13 (note) design and doc pointers

`anchor/doc.go:5` cites "corrections 60 to 65"; the corrections this round implemented are 81 to 84. The 6.5 step 3
mark says "(Replaced by correction 60)" and the reader must then find 82 by chance. Correction 81 still describes
the residual window as "less than the duration of the first submit" (A3-01).

## What the round got right

Worth saying because I tried to break each and could not: the state table is total and every claimed entry is pinned
by a mutant; `Upgrade` ends (the drop loop removes a leaf per bad claim; the fuzzer found no hang); a contradicted
claim cannot end as anchored (the summary requires a plausible claim, no header problem and, in conflict, a header
match, `ots/claims.go:131-136,262-281`); a block time ahead of the clock contradicts nothing; a round of permanent
failures is retryable; the claimed timeout is four times the submitted one and measured from creation, so the
flapping of A3-06 ends; the verifier limits are checked by constants read from the verifier's source; the globals test
is a list; every limit has a test with a literal at its edge.

## Verdict

Not yet fit to be wired as a whole. sigv4 and the TSA target (A3-09 is low and has no way to pass another authority
that the configuration did not already allow) are fit. The WORM target should not be wired until A3-01 and A3-02 are fixed:
the second review's high finding is closed only for a record that is young; the evidence time of an older record can
still be a clock of the store that the cluster should not vouch for, by a path the documentation says is closed. The
OpenTimestamps target can be wired without a header source (it then only claims), but not with one until A3-03
(a wrong answer of the source deletes a proof, silently) and A3-04 (the work check proves almost nothing) are
fixed; the verifier's note rules (A3-05) should be unified before the evidence files of this module are read by
`kingres-verify`. If the wiring has to go ahead first, the minimum compensation is: persist the start of the first
Submit that reached the store and pass that as the first attempt; never let Retry reuse the first attempt after a
`store_time_early`; give the cluster a bucket nobody else writes; run OpenTimestamps without a header source.

## What I did not check

- Real S3, MinIO, a real time-stamp authority, real calendars and a real Bitcoin node: nothing outside the fakes and
  the repository's own fixtures (no network was used or allowed).
- The code of `sigv4` beyond running its tests and reading the type that prints credentials (it did not change this
  round); the OpenTimestamps proof parser and serializer beyond the paths `Upgrade` uses; the CMS and DER reader of
  `tsa` beyond its fuzzers and the name code.
- The Redactor second loop, the HEAD-answers-another-version test and the bloat test with a skip (open from A2-06),
  beyond reading.
- Behaviour under the race detector for the new code (the earlier review ran it clean; I ran the suites without it).
- Concurrency of two leaders writing the same key, the console and alert wiring, and the scheduler: nothing calls
  these packages yet.
- Fuzzing was brief (about 160 seconds in total); `FuzzVerifyToken` ran only about 2,900 executions in its slice.

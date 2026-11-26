<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->

# Second independent review of the anchoring libraries of kingresd

Reviewer: an independent reviewer. Date: 2026-11-18.

## Scope and method

Reviewed: `control-plane/internal/anchor/` and its sub-packages `sigv4`, `worm`, `tsa`, `ots` (about 17,300
lines with tests) in the state after the first fix round, against the first review
(`docs/reviews/2026-11-17-kingresd-anchoring-libraries.md`, findings A1-01 to A1-19), the design
`docs/design/ledger-phase7.md` (sections 6.2 to 6.6 and the corrections of section 14), each package's
`doc.go`, and the verifier module `verifier/`. Everything was read from a read-only export; all runs were in a
copy under a Linux home directory (Go toolchain of the project, local mode, no network use, nothing fetched).
The copy was removed at the end. Nothing in the repository was changed except this file.

What I did:

- Read all non-test code of the five packages (record.go, target.go, evidence.go, errors.go, httpx.go,
  secret.go; worm: submit.go, target.go, s3.go, config.go; tsa: target.go and token.go in full, request.go
  in part; ots: target.go, upgrade.go, claims.go, calendar.go, bitcoin.go; sigv4.go) and the doc.go of
  anchor, worm and ots.
- Ran the whole suite, `go vet`, `gofmt -l` and the suite under the race detector (all clean).
- Wrote about 80 source mutants of my own (single-line edits, named in the table below where they prove a
  fix) and ran the package tests against each; about 65 compiled and ran, the others did not compile and some
  were redone in another form. Survivors are discussed in "The tests".
- Wrote five probe tests of my own (named `zz_*_test.go` in the findings; they are not in the repository)
  that drive the real code against the fake store, a scripted calendar and the payload parser.
- Fuzzed `FuzzVerifyToken`, `FuzzParseFragment`, `FuzzParseS3Error` and `FuzzParseEvidenceFile` for 25
  seconds each with `-parallel=2` (100 seconds in total, as the brief allows); no failure. (This is far short
  of the first review's runs; the first review's longer runs are the real fuzz evidence, and the code they
  ran on has changed.)

Counts: default run on Linux, without the third-party vectors: 190 top-level tests pass, 17 skip, 0 fail
(plus 190 passing subtests). The skips are exactly
the tests that read `testdata/third_party` (`ots`: 13, `sigv4`: 4) and every one skips with a clear
message. No test reached the network: every URL in a test is a local listener (`httptest`), and the one
dialer that connects elsewhere is a function that connects to the local listener. The one test group that
runs an external certificate tool (`tsa/openssl_test.go`) skips when the tool is not on the path.

## Summary

New counts: 1 high, 2 medium, 7 low, 2 notes (A2-01 to A2-12).

The fix round is real and good. Of the 19 earlier findings, 18 are fixed with a test that fails without the
fix (checked by mutants; for two of them, A1-03 and A1-08, a related gap is in A2-02 and A2-03), and 1 is only
partly fixed (A1-07: the bound is bypassed by a repeated Submit, A2-01). Nothing here writes or reads a file, so the
points about a half-written state file and file replace on Windows do not apply to this scope (the records
and the evidence are values; storage is the wiring's).

What is not right yet:

- The bound on the store's time (A1-07) can be defeated by the normal retry: a store whose clock is days behind
  is refused once and accepted on the next attempt, and the evidence then states the store's time (A2-01).
- One OpenTimestamps calendar that answers first with a plausible but false claim moves the record to
  `claimed`, after which no calendar is asked again; with a header source the record then fails with a
  "conflict" although an honest calendar would deliver (A2-02).
- A recovery path that the code itself recommends (apply the lock with the store's tools, then Retry) stops
  working after 8 days (A2-03).
- The payload check accepts notes that `kingres-verify` refuses (A2-04), and the verifier does not read the
  anchor evidence at all yet (A2-05), so "what the packages write is what the verifier reads" cannot be
  confirmed, only compared by limits.

Verdicts: `sigv4`: no defect found. `tsa`: sound; the time and chain-now bounds work, with notes. `ots`: the
format and the header check are sound; the target is safe against a bogus claim being called anchored, not
yet against a hostile calendar's effect on availability (A2-02). `worm`: sound for lock and read-back; the time
bound needs A2-01 before the evidence time can be trusted as a bound. `anchor`: the state machine is now
total and tested.

## Earlier findings

Evidence is file:line in the export. "Mutant" means a one-line edit of mine that removes the fix; "killed"
means the package tests fail on it.

| Id | State | Evidence |
|---|---|---|
| A1-01 critical, bogus Bitcoin claim anchors | fixed | `ots/claims.go:165-181` `checkLeaf` (two SHA-256 operations at the end, height window from `HeightBounds`), `ots/claims.go:188-212` conflicting roots at one height, `ots/upgrade.go:306-319` implausible claims dropped, `anchor/record.go:19-24` new state `claimed` that the contract shows as pending, anchored only with a checked header (`ots/claims.go:217-238`). Tests `TestImplausibleClaimsAreNotRecorded`, `TestClaimBoundsFollowTheClock`, `TestContradictingClaimsAreNotPlausible`, `TestHeaderSourceAnchors`, `TestHeaderSourceRootNotReversed`. Mutants: drop the two-SHA-256 check, drop the upper bound, drop the lower bound, drop the conflict rule, drop the header root compare, drop the header time checks: all killed. (Dropping the 32-byte length check survives, but it is implied by the SHA-256 check: an equivalent mutant.) Side effect: A2-02. |
| A1-02 high, genTime unbounded, warning dropped | fixed | `tsa/token.go:816-847` skew in both directions (class refused, code `time_skew`) and chain valid at the clock as well as at genTime; `tsa/target.go:230-244` `Submit` passes `Now` and `MaxSkew`, so the interface method carries the bound; the warning type is gone. Tests `TestSubmitBoundsGenTime`, `TestSubmitRefusesExpiredAuthority`. Mutants: skew check removed, one direction removed, chain-now removed: all killed. |
| A1-03 high, hostile calendar breaks or starves upgrade | fixed (see A2-02 for a related hole) | `ots/target.go:39` and `ots/upgrade.go:253-275` a lookup budget per fragment (4 distinct commitments), `ots/upgrade.go:291-335` an oversized, unmergeable or implausible answer drops only that calendar's answer. Tests `TestNoStarvationByAHostileFirstCalendar`, the bloat test at `ots/claims_test.go:530-605`. Mutant: lookup budget raised to 1000 survives (A2-06: the test compares with the constant it mutates). |
| A1-04 medium, refusal counting | fixed | `anchor/record.go:157-163,418-425` separate `Refusals` counter. Test `TestRefusalCountsRefusalsOnly`. Mutant (count attempts instead): killed. |
| A1-05 medium, late error leaves a final state | fixed | `anchor/record.go:401-403`, and `step` refuses again. Test `TestAfterErrorRefusesFinalStates` (also nil error). The guard is redundant with the table (an equivalent mutant survives), which is fine. |
| A1-06 medium, retryable failures unbounded | fixed | `anchor/record.go:309-339,426-432` `Limits` (200 attempts or 7 days from `FailingSince`). Test `TestRetryableFailuresAreBounded`. Mutants (age removed, count removed): killed. |
| A1-07 medium, store time unbounded | partly | `worm/submit.go:400-413` bounded for a fresh write (5 minutes by default, configurable); but the object that a failed first attempt left is judged by the wide rule for "existing" objects (8 days before) on the next attempt, which the retry always reaches. Probe: A2-01. Test `TestStoreTimeIsBounded` covers the fresh path and a 20-year-old object only. |
| A1-08 medium, unlocked object can never be completed | fixed (with A2-03) | `worm/submit.go:187-235` `complete` applies `PutObjectRetention` (with Content-MD5) to our version and verifies again; a refusal gives an error that says what to do. Tests `TestUnlockedObjectIsCompletedByLaterSubmit`, `TestUnlockedObjectStoreRefusesRetention`, `TestExistingShortRetentionAndOtherMode`. Mutant (retention never applied): killed. |
| A1-09 medium, evidence has no digest of the object | fixed | `anchor/evidence.go:59-64,164-166` `object_sha256` and `content_length`; `worm/submit.go:434-440`; `worm/submit.go:459-478` `VerifyNote`, `483-501` `VerifyHead`. Tests `TestEvidenceNamesTheObject`, `TestVerifyNoteAndHead`. Mutant (digest of the wrong bytes): killed. The verifier does not use it yet (A2-05). |
| A1-10 medium, later foreign version means conflict | fixed | `worm/submit.go:143-182,248-306` versions are listed (bounded) and ours is found; conflict only when no version holds the note. Test `TestLaterForeignVersionIsNotAConflict` (both paged and unpaged listings, 12 foreign versions). Mutant (no listing): killed. |
| A1-11 medium, `too_few_calendars` class | fixed | `ots/target.go:283-300`: retryable, refused or permanent by what the failures were. Test `TestTooFewCalendarsClass`. |
| A1-12 low, read-back failure retried for ever | fixed | `worm/submit.go:344-355` permanent-class read failures become class refused (code `verify_read_failed`). Test `TestReadBackFailureEndsInRefused`, driven through `AfterError` to the fourth refusal. Mutant: killed. |
| A1-13 low, height limits disagree | fixed | one constant `anchor.MaxBitcoinHeight` (`anchor/evidence.go:99-101`), used by `ots/op.go:28`, `ots/proof.go:48,381` and `OTSEvidence.Validate`. Tests `ots/claims_test.go:803-812`, `evidence_test.go:155`. |
| A1-14 low, evidence parser loose | fixed | `anchor/evidence.go:314-325,462-492`: the parsers re-encode and compare bytes (canonical only); `CanonicalEvidence` is the one explicit way to read another spelling. Tests `TestParseIsCanonicalOnly`, `TestIndependentReaderOfEvidenceFiles`, `TestCanonicalEvidenceFromAnotherSpelling`. Mutants (compare removed in the member parsers and in the file parser): killed. |
| A1-15 low, TSA URL query, Basic over http | fixed | `tsa/target.go:67-71` no query; `185-187` Basic over http needs `allowInsecure`. Test `TestConfigRefusesQueryAndBasicOverHTTP`. Mutants: killed. |
| A1-16 low, TSA extra checks | fixed | RSA floor 2048 `tsa/token.go:797`, issuer of the ESS issuerSerial `647-651`, TSTInfo `tsa` name `789-791` (`101-150`), key usage `813-815`. Test `TestVerifyExtraChecks`. Mutants for each of the four: killed. (Note A2-09 on the name check.) |
| A1-17 low, clock skew reported as refusal | fixed | `worm/s3.go:205-209` `RequestTimeTooSkewed` is retryable with code `clock_skew`. Test `TestRequestTimeTooSkewedIsRetryable`. Mutant: killed. |
| A1-18 low, smaller points | fixed | `CheckPayload` in every `Submit` (`worm/submit.go:67`, `tsa/target.go:231`, `ots/target.go:209`), tests `TestSubmitRefusesInconsistentPayload` (three packages), mutants for tsa and ots: killed; `AfterError(nil)` returns an error `anchor/record.go:398-400`; `RefusedRetryDelays` is now a function `RefusedRetryDelay`; `ots.Magic` is a function returning a copy. The caller-side `Secret` in an unexported field is inherent to `fmt` and unchanged (documented). |
| A1-19 low, BER and non-reply answers | fixed | `tsa/target.go:299-305` a wrong content type is retryable (test `TestContentTypeMismatchIsRetryable`, mutant killed); strict DER stays a documented choice. |

## New findings

Test files named below (`zz_*_test.go`) were written by the reviewer in a scratch copy; they are not in the
repository.

### A2-01 (high) worm: the store-time bound is bypassed by the next attempt

File: `worm/submit.go:339-345` (`tol`), `400-413`, constants `28-31`.

What is wrong: for a fresh write, `Last-Modified` must be within `maxSkew` (5 minutes) of the clock
(`submit.go:407-410`). When it is not, the call returns class refused (`store_time_skew`) but the object is
already written and locked. The next Submit (the record retries a refusal after 1 minute, automatically)
gets 412, goes through `existing` and `verify(..., fresh=false)`, whose rule is only "not later than the
tolerance after now and not older than `MaxExistingAge` (8 days)" (`submit.go:411`). So a store whose clock is
up to 8 days behind is refused once and then accepted, and `EvidenceTime` and `last_modified` state the
store's earlier time as the time of the anchor. The doc claims (`worm/doc.go:23-28`) that a store with a wrong
clock "is refused rather than believed"; the wide window is meant for a crash recovery, but a refused fresh
write lands in it too.

How shown: probe `TestZZSkewBypassedByRepeat` (worm, fake store with `skew = -5 days`): first Submit:
`store_time_skew ... differs from the clock of this server by -120h0m1s`; second Submit of the same payload:
state anchored, evidence time 2026-11-12 when the clock said 2026-11-17. Mutant "fresh bound widened" is
killed, but no test calls Submit twice on a skewed store. The existing test only checks the old-object case
with an object 20 years old.

Fix must achieve: an object that this cluster wrote in an attempt that this code refused for time must never
be accepted later on the wide window. Options: give `Submit` the time of the first attempt of the record (the
`Payload` or an option; the caller has `CreatedAt`) and require `Last-Modified` to be no earlier than that
time minus the skew and no later than now plus the skew; or keep a skew refusal permanent (class conflict-like,
needs the operator); or at least make the wide window apply only when the caller states that the earlier
attempt crashed. Add a test that submits twice against a store that is days off, and a test that an old valid
object from a crash still completes.

### A2-02 (medium) ots: the first plausible claim ends the collection of proofs; one hostile calendar can freeze or fail the record

File: `ots/upgrade.go:104-113,123-139,177-185`, `anchor/record.go:127-133`.

What is wrong: as soon as any calendar's fragment holds a claim that passes the header-free checks, `Upgrade`
returns state `claimed`. For a `claimed` receipt `Upgrade` asks no calendar again (it returns the receipt
unchanged without a header source, and only re-checks headers with one). The table gives `claimed` no
`EventExpire` and `Due` does not select it without a `NextCheck`. So:

1. Without a header source (the only configuration the library can run in today): a hostile calendar that
   answers first with a plausible false claim (a 32-byte message after two SHA-256 operations at a height
   inside the window, which nobody can distinguish from a real one without a header) fixes the record at
   `claimed` for ever with its claim; the honest calendars' proofs are never collected, and the record never
   expires and never alerts by itself.
2. With a header source: the claimed record is re-checked against headers only; the hostile claim is
   contradicted, and `Upgrade` returns class conflict `claim_contradicted` (`upgrade.go:177-184`), which
   `AfterError` turns into `failed` with the critical conflict alert, although the honest calendar would
   have delivered a valid proof on its next poll.

This is the A1-03 property ("a calendar that misbehaves loses its own fragment, not the others'") broken one
level up: dropping is done inside one lookup round but not across rounds once the state is `claimed`.

How shown: probe `TestZZHostileFirstClaimFreezesRecord` (ots, two scripted calendars, one hostile answering a
plausible claim, one honest that answers later): round 1: `claimed`; round 2 (honest calendar now has its proof):
state `claimed`, the honest calendar was not asked again, evidence byte-identical; with a header source holding
the honest block header: `class=conflict code=claim_contradicted`. The existing test at
`ots/claims_test.go:448-451` asserts that a claimed receipt does not ask the calendar again, so the behaviour
is deliberate for one calendar, not for several.

Fix must achieve: a claimed record must keep asking the calendars whose fragments still hold pending
attestations until it is anchored (header checked), expires, or the pending attestations are used up; and a
claim that a header contradicts must be dropped (as an implausible claim is), the record falling back to
`submitted` if no other plausible claim is left, not to failed. Add `EventExpire` for `claimed` (with a
longer timeout) so that a stuck claim ends. Test: two calendars, hostile first, with and without a header
source; the honest proof must win.

### A2-03 (medium) worm: the recovery path the code recommends stops working after 8 days

File: `worm/submit.go:28-31,411-412`, `worm/submit.go:232-234` (the error text), `worm/doc.go:39-45`.

What is wrong: the error for a store that refuses `PutObjectRetention` says to apply the lock with the store's
tools and then Retry. `Retry` makes a new pending record and the next Submit finds the object by 412; but an
object whose `Last-Modified` is more than 8 days old is refused as `store_time_skew` (class refused, fails after
the fourth try with a "credentials" alert), whatever its lock. An operator who takes more than 8 days (a
change process, a holiday) to fix the bucket can no longer complete that key; the same happens to a record
that sat `failed` for a week. The 8 days is a constant, not configuration, and is unrelated to the record age
limit (7 days) except by the comment. Class refused also makes the alert text about credentials, which
misleads for a clock or age problem.

How shown: probe `TestZZOldObjectBlocksRetry`: our note, fully locked in compliance mode, `Last-Modified` 9 days
ago: `store_time_skew ... differs from the clock of this server by -216h0m1s`, class refused. Reading of
`submit.go:411`.

Fix must achieve: no age limit on an existing, complete, locked version of our own bytes (its time is the
store's time and is stated as such), or an operator-set limit; with A2-01's fix the age rule is replaced by a
comparison with the first attempt, which also solves this. Give age problems their own code and class
(permanent with a text that says what to do), not "refused".

### A2-04 (low) anchor: a payload that `kingres-verify` would refuse is anchored

File: `anchor/target.go:99-163` (`NewPayload`), against `verifier/internal/signednote/signednote.go:42-48,
508-526` and `verifier/internal/checkpoint/checkpoint.go:21-23,60-67,130-135`.

What is wrong: `NewPayload` accepts what the verifier refuses: an origin with a plus sign or a non-ASCII
space (verifier `ValidOrigin`), an origin of 256 bytes (verifier 255), a size of 2^62 or more (verifier below
2^62), more than 32 signature lines (verifier `MaxSignatures`), a line longer than 1024 bytes
(verifier `MaxLineLength`). The package would anchor such a note and publish evidence for a checkpoint that
the offline verifier then calls malformed. In practice the server only signs well-formed notes, so this is
a robustness mismatch, not an attack; but the package says it checks "the shape the anchoring depends on" and
the shape the verifier depends on is narrower.

How shown: probe `TestZZVerifierLimits`: all six notes accepted by `NewPayload` (no error). Reading of the
verifier constants.

Fix must achieve: the same limits in `NewPayload` as in the verifier (or a shared constant file checked by a
test in both modules).

### A2-05 (note) the verifier does not read anchor evidence yet

`verifier/` has no reader for the evidence files (a search for `object_sha256`, `token_der_b64`,
`fragment_b64`, `header_checked`, `payload_sha256` finds nothing outside the anchor packages); the plan puts it
at steps 11, 13 and 15 of the implementation plan of the design. So the agreement of formats, member names and
limits between writer and reader could be checked only by their specifications: the limits of the note
(A2-04), the 4 MiB file limit (`anchor.MaxEvidenceFile`) and the canonical-bytes-only rule are written in
`anchor/doc.go`, and `worm.VerifyNote` and `ots.CheckEvidence` exist in the library as the reference for the
verifier's second implementation. Make sure the verifier's implementation is written against `doc.go`, with a
shared test vector for each kind (the design says so), and that the verifier's note limits are used by
`NewPayload` (A2-04).

### A2-06 (low) tests that cannot fail, or are blind to a limit

- `ots/claims_test.go:633-634` `TestNoStarvationByAHostileFirstCalendar` compares the number of requests with
  `PerFragmentLookups` and `MaxUpgradeLookups`, the constants under test: raising the constant to 1000 makes
  the hostile calendar cost 40 requests and the test still passes (mutant survived). Compare with the literal 4
  and 64, or with the bound of the design.
- No test reaches `MaxListBody`, `MaxListPages`, `MaxVersionsChecked` (`worm/submit.go:33-37`, `255,285`) or
  `MaxParallel` (`ots`): raising each constant by orders of magnitude did not fail any test. These are bounds
  of what comes from the network; the doc says a boundary test exists for every limit
  (`anchor/doc.go:21-22`).
- The HEAD answer with another version id than the one asked for (`worm/submit.go:365-367`) is not tested
  (mutant survived).
- `Redactor.Clean`'s second loop (trim a secret cut by the length limit, `anchor/secret.go:73-81`) is not
  tested (mutant survived): a secret cut at a boundary leaks up to its first characters. Only a value cut in the
  middle by `max` is affected, and the first three bytes always show; low.
- `badOwner` in `ots/upgrade.go:292-295` (skip further answers of an owner whose merge failed) is not needed
  by any test (mutant survived); either test it or remove it.
- The bloat test with `t.Skip("test fragment is not canonical")`
  (`ots/claims_test.go:573`) can pass by skipping: the skip is at the one place that proves the oversize path;
  today it does not skip, but a change in canonical ordering would make it skip silently.

### A2-07 (low) comments cite corrections that are not in the design

`anchor/doc.go:5`, `anchor/evidence.go:52,121`, `ots/claims.go:17` cite "corrections 70 to 76" and "correction
70"; the design (section 14) ends at 68, and the matching text is corrections 60 to 65. The body of the design
still says the old things: 6.3 "default governance" and the evidence without the digest, 6.5 step 3 "any one
fragment with a Bitcoin attestation makes the record anchored", 6.4 "accepted but the alert carries the skew",
6.6 "failed after 3 attempts". The corrections say "replaces", but a reader of section 6 meets the old rule
first. Renumber the references and fold the corrections into section 6 (or mark each replaced paragraph).

### A2-08 (low) ots: a permanent answer of every calendar ends a submitted record on one poll

`ots/upgrade.go:127-133`, class by `bestClass`. If every lookup of a round fails and none is retryable or
refused (a gateway that answers 200 with HTML once, a 410, a 400 during a deployment), `Upgrade` returns a
permanent error and the record fails (documented, `ots/doc.go:174-176`). A record that has waited days and
holds valid fragments is lost by a single bad round. Direction: treat a permanent failure of every lookup as
retryable up to the expiry date, since the evidence is kept anyway and the cost of another poll is one request
per calendar; keep permanent only for a fragment that cannot be parsed from the store (our own evidence).

### A2-09 (low) tsa: strict name check may refuse real authorities (not tested against any)

`tsa/token.go:101-150`, `789-791`. The TSTInfo `tsa` name must be byte-equal to the signer's subject or to a
name in the subject alternative name (directoryName) or equal in text for DNS, URI and e-mail; a name form
that is not listed (IP address, otherName), or a directoryName that is the same name in a different encoding
(for instance a different string type) is refused as `tsa_name_mismatch`, permanent. RFC 3161 says the name
"shall correspond to" the certificate's subject, which a comparison of encoded bytes may miss. I could not test
public authorities (no network); the author's tests use their own authority and the external tool's tokens.
Direction: compare directory names by their decoded, case-folded form, and make the check refuse only when no
name of the certificate corresponds; test with a token whose name uses another string type.

### A2-10 (low) worm: an existing object may have a shorter retention than asked

`worm/submit.go:339-345,389-399`. For an object written by an earlier attempt, the retention date must be no
earlier than `retain - 8 days` (and never earlier than now). With the default 3650 days, an existing version
whose retention is 8 days shorter than the configured one is accepted without repair; with a configured
retention of 8 days or less, a version that expires in a second is accepted. The evidence states the true
`retain_until` so a verifier sees it; but the doc says "a retention date not earlier than requested".
Probe `TestZZShortRetentionAccepted`: configured 30 days, existing 23 days: accepted, no retention call.
Direction: apply the retention (repair path) whenever the date is earlier than requested minus the five-minute
write tolerance, for existing objects too.

### A2-11 (low) misleading class for time problems

`tsa/token.go:822,845` and `worm/submit.go:326-330` use class refused for "the clock differs" and "the
certificate is not valid now". Refused means 1, 5 and 15 minutes and then `failed` with the alert
`ledgerAnchorFailed`; the alert text is about credentials in the design (6.6). A skewed clock is retried for 21
minutes and then the record fails. That is defensible, but the alert must carry the code; otherwise the operator
looks at credentials. Say in the wiring notes that the alert text comes from the error code and text.

### A2-12 (note) global state and claims in doc.go

`anchor/doc.go:15-16` says "no global state". `ots.headerWork` (`ots/bitcoin.go:76`) and `ots.fragmentLimit`
(`ots/upgrade.go:364`) are package variables that tests replace; a test that sets `headerWork` and runs with
`t.Parallel` would race, and the race detector run (clean today) only holds because no test is parallel.
Production code never writes them. Make them fields of `Target` (an unexported option) or state the exception
in the doc.

## Other checks (no finding)

- Evidence encoders and parsers: member order, canonical bytes, strict times, size and hex rules read and
  covered; the OTS `header_checked` rule ties the header's bytes 36 to 68 to `merkle_root` (hex offsets
  72 to 136, `anchor/evidence.go:238`). A test of the offset exists through `TestHeaderSourceAnchors`.
- Error text: every constructor uses static text, statuses and redacted store messages
  (`worm/s3.go:189-227`, `tsa/target.go:341-347`); the underlying error with the URL is kept only in `Err`
  and is never printed by `Error()`. No credential reached an error in my reading or in the author's
  `secret_test.go` files (mutant that disables the redaction: killed). A signature or the derived signing key is
  not a "known secret" for the redactor, so a store that echoes the request headers would show a signature
  that is valid for 15 minutes for that one request; not a realistic leak, and the redactor cannot know it.
- Bounded reads: every network read goes through `ReadLimited` (`worm/s3.go:167-178`, `tsa/target.go:306`,
  `ots/calendar.go:127`); listings are bounded (1 MiB, 5 pages, 25 versions); XML entity expansion is off
  (`Entity = nil`). The TSA response bound is tested at the boundary (`tsa/target_test.go:341-354`).
- Redirects: `anchor.Client` follows none (worm) or same host and scheme only (tsa, ots); proxy variables are
  never read (`TestClientNeverUsesTheProxyOfTheEnvironment`; mutant killed). The Basic header of the TSA
  stays on the same host over the same scheme.
- Idempotence and repeat after a crash: worm (412 path, tested, with the issue A2-01), tsa (a repeat gets
  another token with a new nonce; the first stored token wins in the wiring, as the design says), ots (a repeat
  asks the calendars again; a partial result is kept by `SubmitWithPrior`).
- Concurrency: targets hold no mutable state; `ots` goroutines write only their own slot of a result slice;
  `go test -race` is clean on all five packages.
- Windows: no file system access exists in the five packages (searched: no `os`, no `filepath`), so file
  replace and permissions are not in scope; the tests use `t.TempDir` and local listeners and the external
  tool test skips without the tool. I did not run on Windows.

## The tests

Good: the state machine has a golden table and a totality test; the TSA and OTS fixes have negative cases;
the worm fake verifies SigV4 with code of its own. Mutants that survived (equivalent ones left out): lookup
budget constant (A2-06), list and version limits, the HEAD version-id mismatch, the redactor's truncation trim,
`badOwner`. No default test needs the third-party vectors, and none uses the network.

The first review noted that the "merkle root reversed" mutant needed the third-party vectors. A test without vectors now
exists (`TestHeaderSourceRootNotReversed`, `TestRealHeadersPassCheck` on embedded headers); I ran the mutant that
removes the root comparison (killed) but not the reversal mutant itself.

## Not checked

- No run against a real store, authority or calendar, and no public token; no Bitcoin node.
- The reference OpenTimestamps client was not run on `Export`.
- Fuzzing was 25 seconds per target (four targets); the other seven targets were not fuzzed.
- I did not re-run the first review's checks of the SigV4 signer against a second implementation; `sigv4` has
  no changes in this round that I could see by reading, and the signer was read again without a finding.
- The wiring, storage, REST layer and jobs do not exist and were not reviewed; neither was the console.
- Windows was not used to run anything.
- The third-party vectors were not fetched; tests that need them were seen to skip.

## Verdict

The first round fixed what the first review found, and most of it with tests that fail without the fix. The
library is not yet safe to wire until A2-01 (a retry defeats the store-time bound) is fixed, and A2-02 and
A2-03 should be fixed in the same round because they are about what a hostile or slow party can do to a record
that is already half done. The rest are low and can follow with the wiring. Do not count a `claimed` OpenTimestamps
record toward the minimum of anchors (the library already says so), and make the verifier read the evidence
with the limits of `anchor/doc.go`.

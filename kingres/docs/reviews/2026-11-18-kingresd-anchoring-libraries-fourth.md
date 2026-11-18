<!-- SPDX-License-Identifier: AGPL-3.0-only -->

# Fourth independent review of the anchoring libraries of kingresd

Reviewer: an independent reviewer. Date: 2026-11-18.

## Scope and method

Reviewed: `control-plane/internal/anchor/` and `sigv4`, `worm`, `tsa`, `ots` (about 21,700 lines with tests) after the
third fix round, against the third review (A3-01 to A3-13), design section 6 and corrections 60 to 65, 81 to 84 and
88 to 92 of section 14, each package's `doc.go`, and the verifier parsers (`verifier/internal/signednote`,
`verifier/internal/checkpoint`). Everything was read from a read-only export and run in a copy under a Linux home
directory (project toolchain, local mode, no network, nothing fetched; `testdata/third_party` absent and untouched).
The copy and all my files were removed at the end. Nothing in the repository was changed.

What I ran:

- The whole suite of the five packages before touching anything: all pass (251 test functions in the tree; the
  default run needs no network and no third-party vectors).
- 39 source mutants of my own, one line each (listed in "The tests"): 37 killed, 2 survived (W06, W07), 1 not
  applied (W12, my pattern did not match; not run). A first batch was contaminated by two overlapping runs and was
  thrown away; the numbers are from one clean serial run.
- 8 groups of worm probes against the repository's fake store (about 60 individual cases): metadata matrix,
  foreign latest version, cluster clock fault, store that drops metadata, near-expired lock, shortened retention.
- OpenTimestamps: a probe of a good claim lost to a wrong header source; a compact-target comparison with an
  independent implementation of Bitcoin Core's SetCompact (3,000,000 random values, all 256 exponents against ten
  mantissas, 17 known vectors: 0 differences); floor facts for 12 heights; an invariant fuzz of `Upgrade` of my own
  (1,500 scenarios, 9,818 rounds, random calendar answers, random header sources, random clock steps, with the
  stub work check and with the real one; checks: evidence parses, `CheckEvidence` accepts it, never anchored without
  a source or with the real work check, the summary header matches its root, the state machine accepts every
  receipt, at most 32 claims aside): 0 violations.
- A differential run of `ParsePayload` against the verifier's real `signednote.Parse` plus `checkpoint.Parse` (the
  anchor sources compiled inside a copy of the verifier module): the 39 cases of the corpus (verifier column
  re-derived: 0 wrong), 38 hand-made notes, 400,000 random mutations of 4 seeds.
- Not done: the 3 minutes of coverage-guided fuzzing. I used my own random loops above instead (no timing limit
  was hit, and the machine stayed light); the repository's fuzz targets were not run by me this round.

## The earlier findings

| Finding | State | Evidence |
|---|---|---|
| A3-01 high, window = age of record | Fixed for a version this package wrote, if the store keeps the metadata; with new holes A4-01, A4-02 | `worm/target.go:107,127-130` (put sets the attempt), `worm/submit.go:475-484` (window). Mutants W01 (no lower narrowing), W02 (no upper), W03 (attempt never read), W04 (not sent), W15, W16: killed. Probe: store 24 h behind, record 48 h old, one read-back failure: refused permanent; same with the metadata removed from the stored version: accepted 1 day early (A4-02). |
| A3-02 medium, wrong first attempt | Fixed for the retention (floor from the store's own Last-Modified, `submit.go:515-519`; mutant W05 killed). For the time: a wrong start now only matters for a version without the metadata (earlier start widens only that case; later start can burn a key, documented). Not detectable by the library. |
| A3-03 medium, contradicted claim destroyed | Mostly fixed: kept aside, asked again, dropped only after a second contradiction at least the interval later (`upgrade.go:266-292`). Mutants O03, O04, O05, O06, O07, O13: killed. Remainder A4-04. |
| A3-04 medium, difficulty 1 | Fixed: floor 1/16 of the nearest earlier checkpoint, applied in `Upgrade` (`target.go:213-219`), in `CheckEvidence` (`claims.go:356`). Mutants O01, O11, O14, O15: killed. Fuzz: no header below the floor passes. Remainder A4-05. |
| A3-05 low, payload shape | Fixed. Differential: 0 cases where the anchor parser accepts and the verifier refuses (400,000 mutations, 38 hand cases); the only difference left is DEL (0x7f), stricter on the anchor side, stated in the doc (425 random cases, all that class). |
| A3-06 low, flapping hostile calendar | Reduced: the confirm interval and the counts on the receipt (`ContradictedClaims`, `ClaimsAside`, `ClaimsDropped`) let a caller alert; `Upgrade` has its own deadline (5 min). The spare-pending loop (A4-04) is bounded by expiry and not probed by me. |
| A3-07 low, expired without exit, claimed fails early | Fixed: `StateExpired` leaves by `EventRetry`, `Retry` resets `CreatedAt` (`record.go:127-140,233-246`); mutants A01, A02 killed; the doc now says which limit ends a claimed record first (`ots/doc.go`, retry limit, with the defaults). |
| A3-08 low, adoption and the bucket | Partly: the "not locked" text now covers a short lock (`submit.go:264-266`); a stronger mode is accepted (mutant W08 killed; GOVERNANCE where COMPLIANCE is configured refused on purpose, W18 killed). The evidence still cannot say whether the metadata was present (A4-02); a foreign later version with our bytes can end the record (A4-03). |
| A3-09 low, name comparison | Fixed in the profile (`tsa/names.go`: ASCII fold only, invalid strings by bytes, Teletex ASCII only) and a signer pin was added (`token.go:126-135,722-727`). Mutants T01 to T04 killed. Remainders A4-06, A4-08. |
| A3-10 low, tests | Mostly fixed; two mutants of this round survive (A4-09). |
| A3-11 note | Unchanged and still true as far as I re-read (limits, no proxy, no request on a bad payload). No secret in any error text I printed (about 60 worm errors, OTS and TSA errors in the probes). |
| A3-12 note | Fixed (tags are functions; `TestTagsAreFunctions`). |
| A3-13 note | Fixed: `worm/doc.go:3-6` cites 81 to 84 and 88 to 92. |

## The time of the store: what holds and what does not

The property as I found it, stated in the cluster's clock domain (the tolerance is measured against the clock of the
server that made the call, so a cluster clock wrong by more than the tolerance is a fault, not an attack):

1. A version written by this call: Last-Modified within `[max(start, now) - tol, now + tol]` (`submit.go:468-474`).
2. A version found, carrying a well-formed `x-amz-meta-kingres-attempt` `a`: within `[max(start, a) - tol, min(now,
   a + 30 s) + tol]` (`submit.go:468,475-484`). This bound does not depend on the age of the record, the number of
   attempts, Retries or crashes. For a version this package wrote, with the metadata kept, the evidence time is at
   most `tol` before and `tol + 30 s` after the true write. I could not break this part by any input I could
   construct without a lying store AND a lost metadata.
3. A version found with no usable metadata: within `[start - tol, now + tol]`: bounded only by the start of the
   first Submit, as the doc says. But the evidence does NOT say so (A4-02): `found` is true in both cases and
   `submit_start` is the same.

What a forged value buys (probes on a foreign version of our bytes with a chosen header and a chosen
Last-Modified; Last-Modified is the store's, an attacker cannot set it):

- The value can only narrow the window. `lower = max(start - tol, a - tol)` is never below the start window and the
  upper end is never above `now + tol`. A future `a` (probe, +1 h) gives permanent `store_time_early`; a past `a`
  (probe, -1 h or -30 min against a Last-Modified now) gives `store_time_skew`; `a` equal to the real Last-Modified
  is accepted, with a time that is the store's real one. So a forged value buys nothing in the backdating direction
  and a refusal of that version in the other.
- Malformed values (garbage, an offset time zone, fractional seconds, lower case `z`, two values joined by a comma:
  what a store or proxy that joins duplicate headers would produce) are NOT refused: `attemptOf` returns false
  and the version is judged as one without metadata (`submit.go:383-386,475`). Probe: all five accepted with the
  start window. Several header lines: Go's `Header.Get` takes the first; again only narrowing.
- A store or proxy that drops, rewrites to another format, or truncates the metadata silently turns the narrow
  window into the wide one (A4-02). Lower-casing of the header name is harmless (canonicalised).

Paths that reach "found": the 412 or 409 on the conditional put, the check-then-put path, the latest version equal to
our bytes, the version list. All go through `complete` and `verify` with the same rules. A transient failure of any
single request (HEAD of the read-back, GET, put with a cut connection) leaves a version that the next call judges by
rule 2; second leaders at the same moment judge the other's version by rule 2 (their clocks must agree within the
tolerance, documented). The retention floor (`submit.go:515-529`) comes from the store's time:
a store that lies earlier by up to the tolerance lowers the floor by the same; later raises it and so only causes a
repair (an extension, never a shortening: probe, retention cut to 2 h, repaired to 30 days with one
`PutObjectRetention`); COMPLIANCE found where GOVERNANCE is configured is accepted and repaired in its own mode;
GOVERNANCE found where COMPLIANCE is configured is refused permanently (probe). Repair needs only
`s3:PutObjectRetention`.

What does not hold, or holds at a price:

- A4-01: after a short fault of any one clock, the key is dead for good on an honest store, and one of the two
  error texts tells the operator to do something that cannot work.
- A4-02: the narrow window depends on the store keeping a header that nothing checks it kept.
- A4-03: a foreign version of our bytes placed after ours can end the record although ours is fine.
- A4-07: an old found object whose lock is about to end is accepted.
- A request that the client abandoned at its 30 s timeout but that a proxy or store executes later (a zombie put)
  gives an honest Last-Modified later than `a + 30 s + tol` and is refused for good: safe, but it is the same dead
  key as A4-01 (not probed; reading).
- `MaxSkewSeconds` goes up to 86,400 (`worm/config.go:73,152`): an operator can make the tolerance a day; it is
  the operator's own, and the doc says the evidence time is bounded by it.

## New findings

Counts: 0 critical, 0 high, 3 medium, 3 low, 3 notes (A4-01 to A4-09).

### A4-01 (medium) worm: a clock fault of one Submit call burns the key for good, and the "ahead" text promises a Retry that cannot work

File: `worm/submit.go:393-397` (text), `481-487` (the upper end), `406-410` (the early text).

What is wrong: a version that carries its attempt is judged against `a + CallTimeout + tol` for ever (the clock
catching up does not help, as designed). Probes with an honest store and the cluster's own clock wrong for one
call:

- cluster clock 10 min behind, then fixed: first call `store_time_skew` (class refused), and the same refusal at
  +1 min, +20 min and +48 h ("9m30s ahead of the end of the Submit call that wrote the object"). The text ends
  "correct the clock of the store or of this server, then Retry": a Retry is refused identically for ever. The
  class refused also makes the record spend its refusals first (1, 5, 15 minutes) and fail.
- cluster clock 10 min ahead, then fixed: first call permanent `store_time_early`; later calls: first
  `submit_start_ahead` (retryable, the persisted start is the wrong-clock one), then `store_time_early`. The text
  there does say "change the keyPrefix", which is a global configuration change of the target for one
  checkpoint.
- 2 minutes wrong: fine in both directions.

So one NTP step or a resumed virtual machine during one Submit costs that checkpoint its anchor on that target until
the prefix is changed, and for the ahead class the operator is told the wrong cure. The library has no per-record way
out (the key is derived from prefix, aggregate and size only).

How shown: probe `TestProbeClusterClockFault` (cases above); reading.

What a fix must achieve: (a) make the text of `store_time_skew` for an object that carries its attempt say that it
will not heal and what to do (the same as `store_time_early`); (b) give a per-record way out that does not change the
configuration, for example a caller-persisted key generation in the key, or an explicit, evidenced operator choice to
accept a time that is ahead (ahead only weakens the bound; consider accepting it with a flag in the evidence, the way
the old rule accepted it when the clock caught up); (c) test a wrong cluster clock in both directions followed by
Retry.

### A4-02 (medium) worm: the narrow window depends on a header that is never checked to have been kept; malformed is treated as absent; the evidence cannot say which window applied

File: `worm/submit.go:380-386`, `475-484`, `550-558`; `worm/target.go:127-130`; `anchor/evidence.go:57-83`.

What is wrong:

1. Nothing at write time checks that the store kept `kingres-attempt`. Probe: a store that drops user metadata:
   fresh write accepted, `found: false`, no sign of anything wrong.
2. Probe `TestProbeStoreDropsMetadata`: store 24 h behind, record 48 h old, put succeeds, one HEAD fails, then
   recovery. With the metadata kept: refused permanent (`store_time_early`). With the metadata gone from the
   stored version: ANCHORED with `last_modified` one day before the true write. The documented property (tolerance
   plus one call) is broken by a store, a gateway or a copy tool that does not keep the header.
3. A malformed value (garbage, offset zone, fraction, lower case `z`, "a,a") is the same as no value. A joined
   duplicate header from a proxy is therefore enough.
4. The doc (`worm/doc.go:98-111`) and the task statement say the evidence says "found, bounded only by the start".
   It does not: `found: true` is written for a version found WITH a valid attempt and for one found without. No
   member records whether the narrow rule applied or the attempt that was used.
5. Mutant W07 (the fresh branch of the window removed) survives: with the metadata kept the fresh rule is implied by
   the attempt rule, so no test pins what the fresh rule does when the metadata is lost.

How shown: probes above; mutant W07 survived.

What a fix must achieve: at the read-back of a fresh write require the echoed attempt equal to what was sent, else
fail with a clear code ("this store does not keep user metadata"); treat a present but malformed value as an error
for that version, not as absent; add `attempt` (or a boolean `attempt_checked`) to `WORMEvidence` so the evidence
states which window was applied; tests: metadata dropped on a fresh write, malformed values, and a test that kills
W07.

### A4-03 (medium; low if the bucket is only ours) worm: a later version with our bytes decides alone; a foreign copy placed after ours ends the record

File: `worm/submit.go:184-190` against `doc.go:38-39`.

What is wrong: when the latest version of the key has our bytes, `existing` verifies only that version and returns
its error; the other versions are listed only when the latest does not have our bytes. A principal that can write
the bucket adds one version with our bytes (the note is public) after ours: unlocked and the store refuses the
retention call, or a later or wrong-mode lock, or a forged future attempt. Probes (our version written and valid,
then a foreign copy added 10 minutes later): unlocked with `retentionStatus` 403: permanent `not_locked`; forged
future attempt: permanent `store_time_early`; GOVERNANCE copy with COMPLIANCE configured: permanent `not_locked`.
In every case our own valid version was never tried. The doc says a foreign later version is not a conflict as long
as ours is there.

How shown: `TestProbeForeignLatestWithOurBytes` (4 cases).

What a fix must achieve: if `complete` on the latest fails, continue with the list (skipping that version id) and
return the first success; add the three probes as tests.

### A4-04 (low) ots: a good claim merged before a header source existed is lost for good after one hour of a wrong source, and Retry then discards the evidence

File: `ots/upgrade.go:112-135` (`dropClaims`), `266-292`, `397-405` (`removePending` only without a source), `ots/doc.go` section "kept aside".

What is wrong: the author says a claim merged before a source was configured cannot be asked again. Judged: the
price is real but the case needs a source that gives a header of another root for the height in two rounds at
least an hour apart (a node on a stale fork or still synchronising for over an hour, a node of another network
that passes the floor). Probe: a claim merged without a source (state claimed), then a source that returns a wrong
header every 10 minutes: rounds 1 to 6 give state submitted with 1 claim aside and 0 requests to the calendar
(nothing pending to ask); round 7 (70 min): error class conflict, code `claim_contradicted`; with the source right
again the same input receipt anchors at once (the proof was fine). The error leaves the record failed; the only
operator step is Retry, which goes to pending and a new Submit, whose receipt replaces the evidence, so the
original submission time and claim are gone unless the caller kept them. The same happens at once when the
32-entry bound is full (a hostile calendar fills it, then a wrong source drops a good claim). Also by reading: a
hostile calendar that returns a spare pending attestation on a branch that is not above the dropped claim gets
the same false claim re-merged after the drop (the drop is not remembered), so a record cycles aside, dropped,
re-merged every confirm interval until expiry; bounded, alerts available through the counts, but no state change
shows it.

How shown: probe `TestProbeProofLostByASourceWrongForAnHour`; reading for the cycle.

What a fix must achieve: do not remove the pending attestation of a merged claim (keep it, a source may come later),
or record a dropped claim (calendar, height, root) so that a re-merge is quarantined at once; make a
`claim_contradicted` failure keep the claimed evidence available to `SubmitWithPrior`/Upgrade on Retry; say in the
wiring notes that Retry of such a record must keep the old evidence.

### A4-05 (low) ots: the work floor is frozen at one checkpoint and `WorkSlack` has no upper bound

File: `ots/bitcoin.go:128-166`, `ots/target.go:162-167,210-216`.

Facts (probe): the last checkpoint is block 912095, bits 0x17022b91, difficulty 1.297e14. The floor for every later
height is 16 times that target: difficulty 8.1e12, about 2^74.9 hash evaluations per header (the doc's "about 2^75"
is right). The default therefore tolerates a fall of difficulty to 6.25 percent of the August 2025 level, whatever
the difficulty is at the time; if difficulty is X today, the tolerated fall is `1 - 8.1e12 / X` (about 95 percent
at 1.8e14). Consensus changes difficulty by at most 4 times per period of 2016 blocks, so reaching the floor takes
at least two periods. A real header below the floor is "transient": the claim stays a claim (never contradicted,
never dropped), and the operator has `WorkSlack` (a Go option, not in the configuration file). The floor does not
move with the chain: a table that is not extended makes the floor weaker every year as difficulty rises, and a
header of 2^75 work is a large but not an out-of-reach forgery for a state-size attacker; the doc states it proves
no inclusion. `WorkSlack` is any `uint`: `^uint(0)` gives a floor below difficulty 1, and only the separate
difficulty-1 limit remains.

Arithmetic at the edges is right: `CompactToTarget` agrees with an independent SetCompact on 3,000,000 random
values, all exponents against ten mantissas and 17 known vectors (negative, overflow, zero mantissa, exponents 0
to 3, 33, 34, 35); `MaxTargetAt` is safe at height 0 and at the largest height (the table starts at height 0). No
path lets a header below the floor reach "header_checked": `Upgrade` and `checkEvidence` use the same work function
(the only unfloored exported functions are `CheckHeader` and `CheckAgainstHeader`, which the doc names and nothing
in the package calls on a stored header; the fuzz targets call them on purpose).

What a fix must achieve: cap `WorkSlack` (for example 2^20) or refuse a value that would admit difficulty 1; add a
yearly (or release-time) extension of the checkpoint table and a test that fails when the newest checkpoint is
older than a stated age.

### A4-06 (low) tsa: the stored-evidence verifiers cannot apply the pin, and the evidence does not carry the signer key hash

File: `tsa/token.go:126-135` (`SignerKeyHash`), `829-871` (`VerifyStoredToken`, `VerifyEvidence`).

What is wrong: `Expect.SignerKeySHA256` pins a signer in `VerifyToken` and in `Submit`, but `VerifyStoredToken` and
`VerifyEvidence` build `Expect` with only the digest, so an auditor using them accepts any authority under the
roots. The hashed value is right (SHA-256 of the DER SubjectPublicKeyInfo, equal to the usual `openssl` pipeline;
mutants T01 and T02 killed): a renewed certificate with the same key keeps the pin; a certificate with another key
does not. A pin of the wrong length or an empty non-nil slice fails closed (compared with constant time to 32
bytes; probe by reading and the repository test with 5 bytes). Upper case or spaces in the configured pin are
refused at configuration (`target.go:99-103`; mutant T03 killed). Two certificates with the same key in a chain are
not a problem for the pin; `findSigner` takes the first certificate whose subject key id (or issuer and serial)
matches and `checkESS` then binds it, so a token that lists an older certificate with the same key id before the
one that signed fails with an ESS mismatch (permanent): a rare liveness case (not probed).

What a fix must achieve: give `VerifyStoredToken` and `VerifyEvidence` a pin argument (or an options struct), write
the signer key hash into the evidence, and let `findSigner` try every matching certificate.

### A4-07 (low) worm: an old found object whose lock is nearly over passes

File: `worm/submit.go:515-522`; mutant W06 survives.

What is wrong: the floor is `max(Last-Modified + days - tol - 5 min, now)`. For an object older than the configured
days the floor is `now`, so any lock that ends after now passes, and nothing repairs it. Probe: `retentionDays` 1,
object 24 h old, lock ending in 1 minute: ANCHORED, `retain_until` one minute ahead, 0 repair calls. Reachable only
for a record found after its own retention period (a long failure then Retry); the evidence states the date and the
doc asks the caller to warn when it approaches, but the stored evidence is then an anchor that expires in a minute.

What a fix must achieve: decide on purpose (repair to `now + days`, or refuse) and kill W06 with a test.

### A4-08 (note) tsa: the name profile can refuse an honest authority; the refusal is permanent

File: `tsa/names.go:156-198`, `token.go:~755`. A PrintableString that holds a character outside the
PrintableString set (some real certificates contain `&` or `@`) is not decoded, so it is compared by bytes and is
not equal to the same text as a UTF8String; no Unicode normalisation is done (composed and decomposed letters
differ). Both fail safe (`tsa_name` is permanent). Worth a line in the wiring notes; a name check that fails for an
authority that works elsewhere is a support case.

### A4-09 (note) tests

- Survivors: W06 (A4-07) and W07 (A4-02). W12 (repair never offered) was not run (my pattern was wrong).
- The mutants killed are killed by the tests that name them; I did not find a test that cannot fail, or a constant
  compared with itself, in the parts I read (the limit tests use literals; the checkpoint test compares the table
  with the real headers; the corpus verifier column is correct against the real parsers: 0 of 39 wrong).
- The payload corpus lives only in the control-plane module. A shared corpus run in the verifier module (the way
  my differential did) would make the claim "the rules are the verifier's" hold by test; today it holds by a column
  that was written by hand (I checked it; it was right).
- `ots` tests of the path to a header use `headerWork`; the real floor is tested by arithmetic and on real headers
  (and never on a mined header at the floor, which is not possible); acceptable.

## The payload rules (summary)

`ParsePayload` against the verifier's parsers: 400,000 random mutations, 38 hand cases (size 0 with another root,
leading zero, plus sign, 2^62 and 2^62-1, 2^63, 3-byte signature, no-break space, U+0085, zero-width space and DEL
in a name, origin of 255 and 256 bytes, extension lines of 1,024 and 1,025 bytes, tab, CRLF, non-canonical
base64, two blank lines, a blank line inside the signature block): the anchor side accepts nothing the verifier
refuses; the verifier accepts and the anchor refuses only for DEL (0x7f) and, by size, notes over 16 KiB (stated).

## Verdict

- sigv4: fit to be wired (not changed this round; I ran its tests only).
- TSA (`tsa`): fit to be wired. Set the pin if one authority is meant (A4-06 for the auditor side).
- WORM (`worm`): fit to be wired in a bucket nobody else writes, once A4-02 (require the echoed attempt, refuse a
  malformed one, record it in the evidence) is done; A4-01 needs at least the corrected text and a documented
  operator route before it is wired, since one clock fault of the cluster makes a checkpoint unanchorable on that
  prefix. A4-03 is a medium only if others can write the bucket. With a store that keeps user metadata the stated
  property holds under everything I could construct.
- OpenTimestamps (`ots`): fit to be wired, with or without a header source. With a source: keep the old evidence on
  Retry (A4-04) and say in the wiring notes that the floor is frozen at block 912095 and must be refreshed
  (A4-05).

## What I did not check

- Real S3, MinIO, a real TSA, real calendars, a real Bitcoin node (no network).
- `sigv4` beyond running its tests; the OTS proof parser and serialiser beyond the paths `Upgrade` uses; the CMS
  and DER reader of `tsa` beyond running its tests and reading the name and pin code; the console, scheduler and
  alert wiring.
- Coverage-guided fuzzing of the repository's targets (my own random loops only); the race detector; concurrency of
  two leaders on one key beyond reading.
- A zombie put, a chain with two certificates of one key in a token, and the spare-pending cycle of a hostile
  calendar (A4-04): argued from the code, not run.
- Mutant W12.

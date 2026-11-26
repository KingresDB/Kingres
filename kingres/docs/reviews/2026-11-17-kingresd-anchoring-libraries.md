<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (c) 2026, RooyeKhat Media Co -->

# Review of the anchoring libraries of kingresd

Reviewer: an independent reviewer. Date: 2026-11-17.

## Scope and method

Reviewed: the commit that added the packages `anchor`, `anchor/sigv4`, `anchor/worm`, `anchor/tsa` and
`anchor/ots` under `kingres/control-plane/internal/anchor` (about 12,400 lines with tests), against
`kingres/docs/design/ledger-phase7.md` sections 6, 7.4 and 7.6 and corrections 35 to 39 of section 14 (read
from the branch head, since the commit does not hold them), RFC 3161, RFC 5652, RFC 5035, the AWS Signature
Version 4 and S3 Object Lock documentation, and the OpenTimestamps reference library as the design cites
it. The code was exported with `git archive` from the commit, into a scratch directory on Windows and into a
Linux home directory (Ubuntu under WSL, Go 1.27.3); nothing in the repository was changed except this file.
The third-party vectors were fetched with the two scripts (manifests verified: 172 and 34 files); no test
and no check of mine contacted a store, an authority or a calendar.

What I did: read all non-test code; wrote my own tests in a scratch copy (named `zz_*_test.go` in the
findings) that drive the real code with hostile servers, hand-edited DER and bit flips; built tokens with
`openssl ts` (OpenSSL 3.0.13); compared SigV4 signatures with `curl 8.5.0 --aws-sigv4`; parsed and
re-serialised the reference client's example proofs; ran ten mutants of my own; ran every fuzz target.
Fuzz: `FuzzVerifyToken`, `FuzzParseOTS`, `FuzzParseFragment` and `FuzzParseEvidenceFile` for five minutes
each, `FuzzParseResponse`, `FuzzDER`, `FuzzParseS3Error` and `FuzzNewPayload` for two minutes,
`FuzzParseConfig`, `FuzzHeader` and `FuzzCompact` for one minute, all with `-parallel=2`; all ended with
exit status 0 and no crashing input (the seven shorter runs are below the five minutes the brief asks for,
to keep within the load limits).

## Summary

Counts: 1 critical, 2 high, 8 medium, 8 low, 5 notes.

The code is careful where the brief asked for most care. The RFC 3161 verification binds the token to the
digest, the nonce and the roots as the RFCs say (nothing from the system roots, signed attributes as
received, critical exclusive timeStamping usage, strict DER); no token I could construct, by hand or from
OpenSSL, was accepted that should not have been. The SigV4 signer agrees with a second implementation on the
requests `worm` sends. The OpenTimestamps format round-trips the reference client's real files byte for
byte. All fuzz targets ran clean. The credentials never appeared in an error, a format verb or JSON.

What fails is at the edges where a remote party chooses a value that the code then trusts: an OpenTimestamps
calendar chooses the Bitcoin attestation that makes a record "anchored" (A1-01, critical: even a claim that
cannot be a block's merkle root is accepted) and can starve or break the upgrade of the honest calendars
(A1-03); a time-stamp authority and an object store choose the time the evidence states, and the TSA warning
is lost through the interface method (A1-02, A1-07); and the retry arithmetic gives up early or never
(A1-04, A1-05, A1-06). The WORM evidence does not carry a digest of the object (A1-09).

Verdicts: `tsa` verification: sound, with the time problem A1-02 in `Submit`. `worm`: sound for the lock
and read-back, with A1-07 to A1-10 open. `sigv4`: no defect found. `ots` format and header check: sound;
`ots` target: not safe against a hostile calendar until A1-01 and A1-03 are fixed. `anchor`: the state
machine needs A1-04 and A1-05 before the wiring relies on it.

## The packages

### tsa (RFC 3161)

What it claims: a token is accepted only after strict DER parsing, a check of the TSTInfo against the request
(imprint, nonce, policy), the messageDigest attribute, the signature over the signed attributes as received,
the signing-certificate binding, a critical and exclusive timeStamping usage, and a chain to configured roots
with validity checked at genTime.

Read against RFC 3161 section 2.4.2 and 2.3, RFC 5652 sections 5.3, 5.4 and 5.6, and RFC 5035:

- Held: the bytes that are signed are the received `[0]` signed attributes with the tag byte replaced by
  `0x31` (RFC 5652 5.4); a reordered attribute list failed the signature (reproduced). The messageDigest is
  taken over the eContent octets as received. The contentType attribute must be id-ct-TSTInfo. The imprint
  must equal the digest sent, its hash algorithm must be SHA-256 with absent or NULL parameters, and the nonce
  must be present and equal when one was sent. TSTInfo version must be 1; a critical extension is refused;
  ordering FALSE spelled out is refused as non-DER.
- Held: certificate selection. The signer is chosen by the sid (issuer and serial, or the subject key
  identifier), the first certificate that matches. An extra certificate placed by an attacker can only
  become the signer if it is signed by a key that chains to a configured root, because the signature is
  checked with the chosen certificate's key, the ESSCertID hash must be the hash of that certificate, and
  `x509.Verify` runs on it. The other certificates of the token are used only as intermediates. System roots
  are never used: a nil root pool is an error, and an empty non-nil pool gives "unknown authority"
  (reproduced).
- Held: extended key usage. Missing, non-critical, or with any second purpose (including anyExtendedKeyUsage)
  is refused; `openssl ts -reply` itself refuses to sign with such a certificate, so I could not make an
  OpenSSL token with them and relied on the author's authority plus the mutant results below.
- Held: signature algorithms. RSA PSS, Ed25519, SHA-1 and MD5 are refused by OID before any key is touched;
  a digest/signature OID disagreement is refused; an RSA signer with an ECDSA OID (and the reverse) is
  refused. Key size is not enforced by the package: with the default Go runtime settings the standard library
  refuses RSA keys below 1024 bits (reproduced: 512 bit key cannot even be generated; with
  `GODEBUG=rsa1024min=0` a 512, 768 and 1024 bit signer all verify). So the floor is whatever the toolchain
  default is (A1-16).
- Held: DER strictness. Indefinite length, non-minimal length, high tag numbers, trailing bytes inside and
  after the token, constructed OCTET STRING, deep nesting (32), duplicate contentType, messageDigest and
  signingCertificate attributes, two SignerInfos, unsigned attributes are all refused (the author's tests
  and my byte-flip run). Flipping any single bit of an OpenSSL-made token (1,303 bytes, two flips per byte)
  was refused everywhere except inside the extra certificate that OpenSSL added (the root, bytes 950 to
  about 1,106): 314 flips accepted, all in a certificate that is only an intermediate candidate. So the
  certificates carried by a token are not covered by anything and the stored evidence can differ from what
  the TSA sent in those bytes without being noticed (note, not a defect: the signer certificate itself is
  bound by the ESSCertID hash).
- Not held: the time (A1-02). genTime is unbounded against the clock; the one warning the design asks for is
  returned only by `SubmitDetailed` and is dropped by `Submit`, the method of the interface. A TSA whose
  certificate expired an hour ago can hand out a token dated inside the old validity window and it is
  recorded as an anchor made now.
- Other points: v1 (SHA-1) signing-certificate attributes are accepted. They only bind the identity of a
  certificate that is then verified by chain and signature, so no collision on the SHA-1 hash helps an
  attacker who has no trusted key (note). The `issuerSerial` inside an ESSCertID is compared by serial only,
  the issuer is not compared (low). The TSTInfo `tsa` name is not compared with the certificate (RFC 3161
  2.4.2 says it must be the subject name when present) (low). The keyUsage extension of the signer is not
  looked at (low).
- Behaviour of `VerifyStoredToken` with other roots: the same code with the roots given; validity at
  genTime, so a token stays valid after the certificate expired, which is the right rule for evidence, and
  also what makes the unbounded genTime of A1-02 matter at Submit.

### worm (S3 Object Lock)

What it claims: put with `If-None-Match: *`, lock headers, read back (HEAD of the version, then GET of the
version) and compare; a store that accepts without locking is a failure.

- Held: every answer path of Submit was exercised with the author's fake store plus my own cases: store
  without lock, with the other mode, with a retain-until date shorter than asked, without version id, with
  a wrong checksum header, a wrong length, wrong bytes on read-back, 412 with equal and with different
  bytes, 409 ConditionalRequestConflict, 501 and 400 for If-None-Match under `auto`, `on` and `off`,
  redirect (never followed), cut connection, huge error and huge GET bodies, cancelled context (the call
  returns a retryable error and sends nothing).
- Held: the request the package signs. I compared the Authorization header of three real request shapes
  built by `worm` (the PUT with all lock headers and a body, the HEAD with `versionId` containing `+ / = &
  ~` and a space, a GET of a key with a space and a non-ASCII byte) with the output of `curl 8.5.0
  --aws-sigv4` (a second implementation, run against a local listener): SignedHeaders and Signature are
  identical in all three. The fake store's own SigV4 check does not import package `sigv4` (it is a
  separate implementation of the algorithm, checked by reading the file).
- Not held: the time the evidence states is the store's `Last-Modified` and is not compared with the clock
  (A1-07); the evidence does not carry a digest of the object (A1-09); a key whose first write was not locked
  can never be completed (A1-08); a later foreign version makes a repeated Submit report a conflict
  (A1-10); a store that answers the HEAD of the version with 404 for ever is retried for ever (A1-12).
- Governance against compliance: the package accepts whichever of the two the store reports as long as it
  equals the configured one; with the default (governance) the evidence says GOVERNANCE and any principal
  with `s3:BypassGovernanceRetention` can still delete the version. The package cannot tell and the design
  chose it; the wiring and the console must show it (For the wiring). Legal hold is never read.
- The fallback without `If-None-Match` is HEAD then PUT, which races; the package says so. Two writers (two
  leaders in a term change) with different notes of the same size both succeed and the evidence of each
  names its own version id; the key then has two locked versions and nobody notices the fork from this
  package (note).

### sigv4

- Held: the AWS suite as kept in botocore (34 case directories, fetched by the script; all
  pass: canonical request, string to sign and Authorization), the four S3 documentation examples, and my
  comparison with curl above. S3 rules are right for the requests `worm` sends: the path is encoded once
  (the `RawPath` that `worm` sets equals what `Escape` gives, so the wire path and the signed path are the
  same bytes), no normalisation, query parameters re-encoded and sorted, headers lower-cased, trimmed and
  folded, `X-Amz-Content-Sha256` set and signed, the session token set as a header and signed.
- Points to know: `foldSpaces` uses `strings.Fields`, which also folds non-ASCII white space (U+00A0 and
  others) where AWS folds only sequential spaces; no header `worm` sends can contain one. There is no handling
  of clock skew (the caller's clock decides; a `RequestTimeTooSkewed` answer is a 403 and so "refused", A1-17).
  `unescape` treats `+` as a space in a query, as S3 does.

### ots (OpenTimestamps)

- Held: the format against the reference library's `Timestamp.deserialize` and `serialize` as I know them
  (and against the real files): same item loop (`0xff` introduces every item but the last), same recursion
  limit (256 levels), attestations sorted by tag then payload, operations by tag then argument, binary
  operations with a non-empty argument of at most 4,096 bytes, attestation payload at most 8,192 bytes and
  pending URI at most 1,000 bytes of `A-Za-z0-9-._/:`, payload must end exactly after the URI or the height.
  Of the 17 `.ots` files of the reference client's examples, 13 parse and every one of them re-serialises to
  the identical bytes (my own run); the other 4 are refused for the stated reasons (keccak256, bad major
  version, hexlify, a file hash operation that is not a hash). `keccak256` (`different-blockchains.txt.ots`), `reverse` and `hexlify` are refused by name,
  unknown tags as unknown; an unknown attestation is kept opaque and written back unchanged.
- Held: the block header check. The merkle root is bytes 36 to 68 of the header compared as they stand (no
  reversal), the time is the little-endian word at 68, `bits` at 72; target decoding follows Bitcoin Core's
  `SetCompact` (negative, zero and overflow refused); the double SHA-256, read as a little-endian number,
  must not exceed the target and the target must not be easier than 0x1d00ffff. The real headers in
  `testdata/headers.txt` (genesis, 129405, 358391, 465751, 466906, 912095) pass, and the real example proofs
  match their real headers (the author's tests, which pass with the vectors present). Note that the
  height is not tied to the header (a header carries none): the work check is a difficulty-1 check, so a
  header mined by anyone with a few billion hashes passes; this is the stated limit of the design.
- Held: bounds. 1 MiB, 8,192 nodes, 4,096-byte messages, depth 256, 9-byte varuint, response 10,000 bytes
  (the reference client's limit), fragment 64 KiB, upgrade lookups 32, calendars 16, parallel requests 8.
  The worst memory for a parsed proof is bounded by 8,192 nodes times 4,096 bytes, about 32 MiB.
- Not held: A1-01 (a calendar's Bitcoin attestation is accepted unchecked), A1-03 (a hostile calendar makes
  the upgrade fail for good or starves it), A1-11 (the failure class of a submit with fewer calendars than
  the minimum), A1-13 (height limits disagree).
- A user whose calendar returns `keccak256` (the Ethereum calendars of the reference ecosystem; none of the
  public Bitcoin calendars does): `ParseFragment` fails with `UnsupportedOpError`, the calendar client turns
  that into a permanent `bad_fragment` error, and `Submit` with `minCalendars` equal to the number of
  calendars returns `too_few_calendars` of class permanent; the record fails with the text "unsupported
  operation keccak256". That is the right outcome for an unsupported primitive and the text is clear; the
  alternative is to keep the calendar's fragment opaque (it cannot be upgraded or verified by this code).
- `Export`: I could not run the reference client (it is not installed, its core library is a separate
  repository that the fetch script does not download, and I did not install packages from the network).
  What I did: parse and re-serialise real example files byte for byte (above), compare the writer code with
  the reference serialiser line by line, and check the magic, the version byte, the file hash operation tag
  (0x08), the digest length and the tree order. Not run against `ots verify` or `ots info` (Not covered).
  One difference with the reference reader: the major version is a varuint in the reference and one byte
  here; the same for the value 1.

### anchor

- Held: the transition table (pending to pending, submitted, anchored, failed, superseded; submitted to
  submitted, anchored, failed, expired; nothing else), `Retry` (failed to pending only), `Due`, `Supersede`,
  `ApplyReceipt` (resets attempts and error, sets `AnchoredAt` only for anchored), backoff (30 s doubled, cap
  one hour, jitter plus or minus 20 percent, `u` clamped, `Retry-After` wins when larger, capped at 24 hours,
  negative and absurd values give 0 or 24 h, reproduced), `ExpireIfDue` and `Expired` agree at the boundary,
  `UpgradeInterval`.
- Not held: A1-04 (what counts as the fourth refusal), A1-05 (a late error turns any final state into
  failed), A1-06 (no bound on retryable attempts, a server can keep a record waiting for ever).
- Evidence encoding: deterministic, parse of encode equal (my fuzz run of 5 minutes and the author's tests);
  times strict (whole seconds, Z, no offset); integers: `size` as `uint64`, a larger value or `1.0` is
  refused; `height` above 2^31 refused. Not strict: member names match without regard to case, a duplicate
  member is accepted (the last wins), white space and `\u` escapes in names are accepted (A1-14).
- `Secret`: `%v %+v %#v %s %q %x %d`, JSON, text, map keys, and an exported struct field all print
  `[redacted]` (reproduced); a `Secret` in an unexported field of a struct of another package prints in
  full with every verb (reproduced: fmt does not call methods on unexported fields); the types of this
  module that hold secrets have their own `Format`, closures, or exported fields of type `Secret` and are
  safe (the author's tests plus mine); a caller's struct is not (note, A1-18).

## Evidence formats

Is `doc.go` plus the design enough to write a verifier?

- tsa: yes. The token is the DER ContentInfo as sent, base64 padded; gen_time, serial and policy are copies
  that a verifier must compare with the token; the digest to compare is SHA-256 of the checkpoint text and
  is not in the evidence. The exact rules (what is signed, the EKU rule, validity at genTime, the digest
  algorithms, SHA-1 ESS accepted) are in `tsa/doc.go`. Ambiguous: whether a verifier must reproduce the
  strict DER rule (a verifier that accepts BER reads tokens this package refuses); the evidence `tsa_url`
  is informational. Open: the time bound of A1-02.
- ots: yes for the tree (the format is the reference format and `Export` gives the reference file), the
  commitment (SHA-256 of the note text, then the operations) and the header check (bytes 36 to 68 against the
  message, no reversal, time at 68, work at 72). Ambiguous: `bitcoin.height` is "the lowest height in the
  fragment" while the fragment may hold several Bitcoin attestations of different heights; the verifier
  should check each, and say which one it used. What "anchored" means without a header is only "claimed"
  (A1-01).
- worm: no, not offline. The evidence names the object and states retention and time as the cluster's
  claims; the design says an evidence that cannot be re-fetched is "not independently checked". With a
  fetch of the version the verifier can compare bytes, but nothing in the evidence says what bytes to expect
  (A1-09), and the time is the store's own (A1-07).
- The evidence file: the canonical encoding is exact and deterministic, but the parser accepts more than it
  writes (A1-14), which a stand-alone verifier written from the text will not.

## The tests

The suite is good: 124 top-level tests pass and 17 skip without the vectors, 140 pass and 1 skip with them
(both reproduced). `go vet` is clean and `gofmt` lists none of the author's files.

Mutants of my own, run on the packages' tests (without my `zz_` files):

| Mutant | Result |
|---|---|
| tsa: nonce check removed | killed (`TestVerifyRejects`, `TestOpenSSLTokenVerifiedByUs`, `TestWrongTokensThroughHTTP`) |
| tsa: EKU accepted when not critical | killed (`TestVerifyRejects`) |
| tsa: validity checked at the clock instead of genTime | killed (`TestVerifyAcceptsVariants`) |
| tsa: messageDigest attribute not compared | killed (`TestVerifyRejects`) |
| worm: read-back byte comparison skipped | killed (`TestReadBackMismatch`) |
| worm: `If-None-Match` never sent | killed (several tests) |
| ots: merkle root compared reversed | killed only by `TestRealProofsAgainstRealHeaders`, which needs the third-party vectors; without them this mutant very likely survives (not run in that configuration) |
| sigv4: path encoded twice | killed (`TestAWSSuite` needs vectors; `TestS3DocumentationExamples` and `TestCanonicalAwkward` do not) |
| anchor: `Secret` printed by `%v` | killed (`TestSecretAndRedactor`) |
| anchor: refusal handled as retryable | killed (`TestAfterError`) |

Two further mutants I planned were not run (the "missing lock mode accepted" pattern occurs twice in the
file and was not applied; the placeholder for "refusal counted as retryable" was replaced by the last row).
The tests do not catch the behaviours of A1-01 to A1-13: no test feeds a bogus Bitcoin claim, a bloating
calendar, a refusal after retryable failures, a late error on a final state, or a skewed `Last-Modified`;
`TestVerifyAcceptsVariants` asserts the acceptance of an expired certificate at genTime for
`VerifyToken`, which is right for stored evidence and is what A1-02 is about at `Submit`. None of the tests
reaches the network (the only host names are in strings and a dialer that connects to a local listener).
The fake store verifies SigV4 with code of its own (read, not mutated).

## Findings

Test files named below (`zz_*_test.go`) were written by the reviewer in a scratch copy; they are not in the
repository. Each finding says which test shows it.

### A1-01 (critical) ots: a Bitcoin attestation from a calendar is recorded as anchored with no plausibility check

Steps: a calendar that answers `POST /digest` normally and answers `GET /timestamp/<commitment>` with
`append("12345678")` followed by a Bitcoin attestation of height 1 (`TestRevBogusBitcoinAccepted`,
`zz_hostile_test.go`). `Target.Upgrade` on the submitted receipt.

Observed: `Upgrade` returns state `anchored`, evidence `bitcoin: {height: 1, calendar_uri: ...}`, no error. The
message the attestation sits on is 40 bytes long, so it can never be the merkle root of a block (32 bytes);
height 1 is a real block that cannot contain it. A height in 2^31 to 2^32-1 is accepted by the parser and
fails later in evidence encoding (A1-13). Expected: such a record is not `anchored`. Design 6.5 step 3 says
that any fragment holding a Bitcoin attestation makes the record anchored because the server fetches no
Bitcoin data; that rule makes the label "anchored" mean "a calendar says so". The brief of this review
counts that as critical for the part that can be checked without Bitcoin data (message length, height
range), and as a limit the wiring must know for the rest.

Place: `ots/target.go` `Upgrade` (`len(BitcoinClaims(got)) == 0`, and the claims loop that sets `btc`);
`ots/bitcoin.go` `BitcoinClaims`. Direction: accept a Bitcoin claim only when the node message is 32 bytes and
the height is at most an upper bound computed from the clock (genesis time and one block per ten minutes with
a margin), refuse a lower height than the submit time allows (a block older than the submit is impossible
for a commitment made now, given a lower bound from the clock), require that the claim of one calendar is
not contradicted by another, and give the receipt a state or a flag that says "claimed, not checked against
a header" until the auditor's verifier or an optional header source confirms it.

### A1-02 (high) tsa: genTime is not bounded, the skew warning is dropped by `Submit`, an expired certificate still anchors

Steps: `TestRevGenTimeFarAway` (genTime ten years ahead and three years behind: both accepted by
`VerifyToken`), `TestRevSubmitDropsWarning` (genTime 48 h ahead through the interface method `Submit`),
`TestRevExpiredTSAAcceptedBySubmit` (`zz_expired_test.go`: the TSA certificate expired an hour ago, the token
is dated three hours ago).

Observed: all accepted and recorded as `anchored` with `EvidenceTime` equal to the TSA's claim. Only
`SubmitDetailed` returns the skew `Warning`; `Submit`, the method of `anchor.Target` that the wiring will
call, discards it, so `ledgerAnchorBehind` cannot be raised. Expected (design 6.4 step 4): accepted with the
alert carrying the skew; at least the warning must reach the caller through the interface. Beyond the
design: because validity is checked at genTime and genTime is free, a TSA key whose certificate has expired
(or a TSA operator) can issue tokens dated inside the old validity window now; a token dated far in the
past is the evidence of an existence that never was.

Place: `tsa/target.go` `Submit` and `SubmitDetailed`; `tsa/token.go` `VerifyToken` (`CurrentTime:
info.genTime`). Direction: in `Submit` (not in `VerifyStoredToken`) also require that the chain is valid at the
clock and that genTime is within a bound of the clock (the design's 5 minutes, or a larger configured one,
class refused or permanent), and put the warning into the `Receipt` so the interface carries it.

### A1-03 (high) ots: one hostile calendar can make an upgrade fail for good or keep it from finding the honest proof

Steps: `TestRevBloatedUpgradeFailsHonestToo` (`zz_hostile_test.go`): calendars A (honest, answers a Bitcoin
attestation) and H (answers the submit with 40 pending attestations naming itself, and each lookup with
about 10,000 bytes holding three Bitcoin attestations). `TestRevUpgradeStarvation` (`zz_starve_test.go`):
H is listed first and names A in 40 pending attestations with commitments A does not know.

Observed: the upgrade returns `ots upgrade: the upgraded fragment is empty or larger than 65536 bytes`, class
permanent, state unchanged; the honest calendar's valid proof is lost with it, and since the same inputs
give the same error the wiring marks the record failed. Starvation: with H listed first, three consecutive `Upgrade` calls made 32, 64 and 96 GET requests (each call
spends its 32 lookups on H's commitments), none for A's own commitment, and the state stayed `submitted`
with no error; the record would run to `expired` after the timeout although A had the proof.
Expected: a calendar that misbehaves loses its own fragment, not the others'; a permanent error only when
nothing can be saved.

Place: `ots/target.go` `Upgrade` (the `MaxFragment` check returns for the whole call; `MaxUpgradeLookups`
takes the first 32 pending attestations in tree order of the first calendars). Direction: size-check and
merge per calendar, keep the fragments that are within bounds, drop the offender; take lookups round-robin
over calendars and by distinct commitment.

### A1-04 (medium) anchor: the "fourth consecutive refusal" counts every failure

Steps: `TestRevAfterErrorCounting` (`zz_review_test.go` in package anchor). Three retryable failures, then one
refusal (a 403).

Observed: `state=failed attempts=4 alert=ledgerAnchorFailed` at the first refusal. The reverse: three
refusals then one retryable failure waits 4 minutes (the backoff of attempt 3), not 30 s. Expected (design
6.6, doc.go): the 1st, 2nd and 3rd refusal wait 1, 5 and 15 minutes and the 4th consecutive refusal fails;
network trouble before a propagation delay of credentials must not use up the refusal budget.
Place: `anchor/record.go` `AfterError` (`prev := r.Attempts` is used for both classes). Direction: keep a
separate refusal counter (reset by any success or by a non-refusal), or derive the refusal count from the
last error class.

### A1-05 (medium) anchor: `AfterError` moves a record out of a final state

Steps: `TestRevAfterErrorOnFinalState`. A record in `anchored`, `superseded` or `expired` gets a permanent
error. Observed: all three become `failed` with alert `ledgerAnchorFailed`. Expected: the transition table
says these states are final; a late failure of a stale job (leader change, a retry that overlaps) must not
turn an anchored record into a failed one and raise a critical alert. Place: `anchor/record.go` `AfterError`
(`fail` sets the state without `CanTransition`). Direction: return the record unchanged with no alert when the
state is not pending or submitted.

### A1-06 (medium) anchor: retryable failures have no bound

`Backoff` is capped at one hour and `Retry-After` at 24 hours (`RetryAfter` in `errors.go`, reproduced:
`99999999999` gives 24 h, a negative value 0), but `AfterError` never fails a record for retryable errors.
A target that answers 503 or 429 with `Retry-After: 86400` for ever keeps a record `pending` for ever, and
a hostile store makes the cluster call it once a day for ever. Design 6.6 says only that the
`ledgerAnchorBehind` alert fires. Direction: an age limit on a pending record (config) after which it
fails with its last error; or state it as intended and make the alert the only guard (For the wiring).

### A1-07 (medium) worm: the evidence time is the store's `Last-Modified`, unbounded against the clock

Steps: `TestRevLastModifiedUnbounded` (`worm/zz_review_test.go`): after a good Submit, the store's version is
given a `Last-Modified` 20 years ago, then 5 years ahead; Submit again (the 412 path). Observed: accepted, the
receipt's `EvidenceTime` is that date (fresh path: same code). A store with a wrong or hostile clock thus
states any time as the time of the anchor. Expected: the design calls it the store's own clock; evidence of
"existed at" should at least be refused when it is more than a bound from the clock, as the TSA design does
(warning). Place: `worm/target.go` `verify` (`http.ParseTime(lm)`). Direction: compare with `t.s.clock()`
and refuse or warn beyond a configured skew; for a fresh write require `Last-Modified` within a few minutes
of the put.

### A1-08 (medium) worm: an object that was written without a lock can never be completed

Steps: `TestRevStuckAfterNotLocked`. The store accepts the put and stores no lock; Submit fails with
`not_locked` (permanent, correct). The operator fixes the bucket; Submit again: the put is answered 412, the
object exists with the right bytes, `verify` finds no lock mode and fails with `not_locked` again, for ever.
The package never calls PutObjectRetention. Expected: a way out. Direction: a documented procedure (apply
the retention with the store's tools, then Retry) or, better, after 412 with equal bytes and no lock, put the
retention on that version (`PutObjectRetention`) and verify; at least tell the wiring (For the wiring). The
unlocked object is also a hole until someone acts: it can be deleted by the credentials the cluster holds.

### A1-09 (medium) worm: the evidence has no digest of the object

The evidence names bucket, key, version id, the ETag, the lock fields and the store's time (design 6.3 step
5, and `doc.go`). Nothing in it says what the object holds. The ETag of a single-part, non-KMS object is its
MD5, which a verifier could compare with the note it has, but that is store-dependent and the package
does not record the checksum it compared (`x-amz-checksum-sha256`). An independent verifier without access
to the store can tie the evidence to the payload only through the enclosing file's `payload_sha256`, which is
the cluster's claim. Direction: add `checksum_sha256` (base64 of what was written and read back) and
`content_length` to the evidence; the design must say how the verifier uses them.

### A1-10 (medium) worm: a later version by someone else turns a repeat into a "conflict"

Steps: `TestRevLaterVersionByOthers`. After a good Submit another principal puts a different, unlocked
version under the key. A second Submit of the same payload (the normal repeat after a crash, or a retry) returns
`class=conflict code=worm_conflict`. `existing` reads the latest version only and compares bytes;
the locked version that is ours is not looked for, so a repeat reports
`worm_conflict` (critical alert, no retry) although our locked version is intact and a different payload was
never written by the cluster. Direction: on a mismatch list the versions of the key and look for ours before
calling it a conflict, or read by the version id the record already holds.

### A1-11 (medium) ots: `too_few_calendars` is permanent when the failures were refusals or one bad answer

`Submit` counts only retryable failures as hope: with `minCalendars` equal to the number of calendars
(the default for two), one calendar answering 401, 403, 400, a fragment that does not parse (keccak) or
more than 10,000 bytes makes the call permanent on the first attempt; a 403 is class refused for every other
target, which gets 1, 5 and 15 minutes. Place: `ots/target.go` `SubmitWithPrior` (`answered+retryable <
MinCalendars`). Direction: count refused as refused (class refused when enough calendars are only refused or
retryable), keep permanent for calendars whose answer is wrong.

### A1-12 (low) worm: a read-back that always fails with a permanent-class answer is retried for ever

`verify` turns every permanent error of the read-back (other than a redirect) into a retryable
`verify_read_failed` (`TestRevHeadAlways404Retryable`: the HEAD of the version answers 404). With A1-06 there
is no end. Direction: only transport failures and 5xx are "not a verdict"; a 404 for a version that was just
written, 4xx and the like count as refused or permanent after a few tries.

### A1-13 (low) ots: the height limits disagree

`MaxHeight` is 2^32-1 in the parser and `OTSEvidence.Validate` refuses above 2^31, so a calendar that returns
a height in between makes `Upgrade` fail permanently with the code `evidence` (reproduced with 4294967295:
`ots upgrade: cannot encode the evidence`). Use one limit, and reject the height at the parser with a
message that names it.

### A1-14 (low) anchor: the evidence parser is not as strict as the documents say

`doc.go` says a parser rejects unknown members and values that do not follow the rules. Reproduced
(`TestRevEvidenceFileLoose`): a member name in upper case (`"FORMAT"`), spelled with a `\u` escape, a
duplicate member (last wins) and white space between tokens are accepted by `ParseEvidenceFile` and by the
three `Parse*Evidence`, because `encoding/json` matches names without case and `Decode` accepts them. A
verifier written from the text (case-sensitive, no duplicates, compact) would reject what this accepts, and
a stored file can be altered without changing what it parses to. `ParseEvidenceFile` does not require its
input to be canonical. Direction: re-encode and compare bytes (canonical form must equal the input), which
also removes the other differences.

### A1-15 (low) tsa: the URL goes into public evidence as configured, and Basic credentials may travel over http

`TSAURL` is `Config.URL` with user information refused but a query allowed; the evidence file is public. A TSA
that takes a key in the query puts it in the publication. Basic authentication is sent over `http://` too
(documented in `doc.go`). Direction: refuse a query in the URL or keep it out of the evidence; refuse Basic
over http unless an explicit flag.

### A1-16 (low) tsa: checks the RFCs ask for or that depend on the toolchain

(a) The RSA minimum is the Go runtime default (1024 bits; reproduced with `GODEBUG=rsa1024min=0`: 512 bits
accepted); the module's `go` line fixes the default but a GODEBUG setting in the binary changes it; add an
own floor (2048). (b) `issuerSerial.issuer` of the ESSCertID is not compared. (c) TSTInfo `tsa` is not
compared with the signer's subject. (d) The signer's keyUsage is not read. (e) ESS v1 with SHA-1 is accepted
(documented; the binding is only an identity check). None reaches an attacker without a trusted key.

### A1-17 (low) worm: a clock skew is reported as a refusal

A 403 `RequestTimeTooSkewed` is class refused (4 attempts over 21 minutes, then failed with a credentials
alert) when the cause is the server's clock. Give the code its own class (retryable with an alert text that
names the clock).

### A1-18 (low) anchor: smaller points

`tsa` and `ots` `Submit` do not check that `Payload.Digest` is `SHA-256(NoteText)` (only `worm` does):
a zero `Payload{}` is stamped as it stands. `Record.AfterError(nil, ...)` panics. `RefusedRetryDelays` and
`ots.Magic` are exported mutable variables. A `Secret` in an unexported field of a struct of the caller prints
in full with every verb (reproduced; inherent to `fmt`); types of this module are safe.

### A1-19 (low) tsa: only strict DER is read, and a non-reply answer is permanent

The strictness is deliberate and documented. The cost is availability: a TSA whose valid token uses BER
(indefinite length or a constructed OCTET STRING in the CMS, which some authorities emit) is refused for
ever as `bad_der` class permanent, and so is a captive portal or proxy that answers 200 with HTML
(`content_type`, permanent). I could not test public authorities (no network). Direction: a configuration
note, and `content_type` mismatch as retryable.

### Notes

- N1: `anchor.Client(nil, ...)` uses `http.DefaultTransport`, which reads `HTTP_PROXY`, `HTTPS_PROXY` and
  `NO_PROXY` from the environment; the Authorization header of an http endpoint (allowInsecure) is then
  visible to the proxy. The caller supplies the client in production; say so.
- N2: Governance is the default mode; the evidence then says GOVERNANCE and the version can be deleted by a
  principal with the bypass permission. An "anchored" WORM record is not the same assurance in both modes.
- N3: the fallback without `If-None-Match` is HEAD then PUT, which races; two leaders can leave two locked
  versions with different notes of the same size and nobody is told.
- N4: the certificates inside a token are not covered by the signature or by any check of this package
  beyond parsing; stored evidence can differ from what the TSA sent in those bytes (see "tsa").
- N5: `Export` was not run through the reference client.

## For the wiring

- "Anchored" for `ots` means that a calendar claims a Bitcoin attestation; nothing the server holds proves
  it (A1-01). Show it so, and do not count it toward `anchor.minimumAnchored` as if verified.
- Call `tsa` `SubmitDetailed`, not `Submit`, until the warning is part of the receipt (A1-02), and bound the
  time yourself.
- Do not call `Record.AfterError` on a record that is not `pending` or `submitted` (A1-05), and keep your
  own count of refusals (A1-04). Put an age limit on retryable records (A1-06).
- Pass the partial evidence of an `ots` `PartialError` back with `SubmitWithPrior`; keep the first stored
  evidence per record; remember `Upgrade` returns an error with the old receipt untouched, so a permanent
  upgrade error is the record's end (A1-03).
- A `worm` `not_locked` leaves an object under the key that no later Submit can complete (A1-08): the
  operator must apply retention with the store's tools before "Retry", or use another prefix. A `conflict` may
  be a foreign later version, not a different note (A1-10). Show the lock mode (governance is the default).
- Build the `http.Client` yourself with the CA bundle and, where wanted, without proxy (N1); the packages do not
  read the environment otherwise.
- Check `Payload.Digest` yourself or build payloads only with `NewPayload`.

## Not reproduced

- A SHA-1 collision on the ESSCertID hash used to substitute a certificate: not attempted, and the signature
  and the chain still bind the signer.
- An RSA-PSS, Ed25519, SHA-1 and MD5 token built by hand: refused by reading the OID switch and by the
  author's test for SHA-1; I did not build the other three.
- A signer that is a CA certificate, a certificate chaining to a root that is not configured, and a token with
  two SignerInfos: the second and third are covered by the author's tests (`TestVerifyUntrustedRoot`,
  `TestVerifyRejects`); the first would be refused by the EKU rule for the usual CA certificates, I did not
  build one with a critical timeStamping usage.
- Whether a real public TSA or calendar returns BER or keccak256 in practice (no network).
- Goroutine and file-descriptor leaks: read only (every response body is closed by `defer`, the reads are under
  the call's context); not measured with a leak detector.
- A redirect from the TSA or a calendar to another host: the author's tests cover it; I did not repeat them.

## Not covered

The reference OpenTimestamps client (`ots verify`, `ots info`) was not run (not installed; its core library is
a separate repository); the `Export` file is checked only by parse and re-serialisation of real files. No run
against a real S3, MinIO or Ceph store, a public time-stamp authority or a public calendar. No `-race` run (no
C toolchain was checked). The stand-alone verifier of `kingres/verifier` was not read; the question whether it
will accept these formats rests on the documents. The REST operations, tables and jobs do not exist yet. Windows
was not used to run the tests (the author reports it).

## Baseline numbers

Linux, Go 1.27.3: without the third-party vectors 124 top-level tests pass, 17 skip, 0 fail; with them 140 pass,
1 skips (`TestExportForReferenceClient`, which needs a directory to write to), 0 fail. Fetch manifests: 172 files
for the AWS suite directory and 34 for the OpenTimestamps examples verified.

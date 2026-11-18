// SPDX-License-Identifier: AGPL-3.0-only

// Package anchor is the library half of anchoring a signed checkpoint of the
// ledger at an outside party (design: kingres/docs/design/ledger-phase7.md,
// section 6 and corrections 60 to 65, 81 to 84 and 88 to 92 of section 14; the
// code follows 61 to 65, 81 to 84 and 88 to 92: 88 the state table (Retry from
// expired), 89 the time of the store, 90 claims kept aside, 91 the work of a
// header, 92 the payload rules, the tags and the names of the TSA; and 95 to 97 of the fourth round: the
// time of the store and the generation of the key, claims dropped and kept, the
// pin and the corpus). It defines the Target
// interface, the payload, the receipt, the anchor record with its states and
// its state machine, the classification of errors, the retry arithmetic, the
// HTTP client the targets are meant to be given, and the canonical encoding of
// evidence. The targets themselves are in the subpackages worm (S3 Object
// Lock), tsa (RFC 3161) and ots (OpenTimestamps); sigv4 is the request
// signing that worm uses.
//
// # What it guarantees
//
//   - No storage, no scheduler, no goroutine that outlives a call, and no
//     package-level state that a call reads or writes: the package-level
//     variables are sentinel errors and read-only tables (a test of the
//     package lists them, so a new one must be added on purpose); what a test
//     needs to replace (the work check of a header, the limit of a fragment)
//     is an unexported option of the target. Every network call takes a
//     context (its deadline is the timeout) and an *http.Client supplied by
//     the caller; time comes from a Clock
//     argument and nothing is read from the environment. The functions on
//     Record are pure: they return a changed copy, or refuse and return the
//     record unchanged.
//   - Every response read has a size limit (constants in each package, with a
//     test at the boundary). No redirect is followed to another host (Client).
//   - No proxy is ever used: Client replaces the transport of whatever client
//     it is given by a copy whose Proxy is nil, so HTTP_PROXY, HTTPS_PROXY and
//     NO_PROXY are never read. NewHTTPClient builds the client a caller should
//     pass: no proxy, TLS 1.2 or later against exactly the roots given (an
//     empty pool when none are, never the system roots, which on Unix are
//     read from the environment), and timeouts for the dial, the handshake, the
//     response header and the whole request. A caller's own RoundTripper that
//     is not an *http.Transport is kept as it is: it is the caller's code.
//   - No credential appears in any error, String, GoString, Format, JSON or
//     text encoding: credentials are of type Secret, errors are built from
//     static text, statuses and sanitised store messages (Redactor), and the
//     types that hold credentials print a placeholder as a whole.
//   - Evidence is deterministic: the same value always encodes to the same
//     bytes (below), what a target produces parses back with this package, and
//     the parsers accept the canonical encoding and nothing else.
//   - Every target refuses a payload that was not built consistently
//     (Payload.Check, CheckPayload): the digest is SHA-256 of the note text,
//     the text is the head of the signed note, the id and the size agree.
//
// # Payload
//
// A Payload is one signed aggregate checkpoint (a signed note, C2SP
// signed-note) and SubmitStart, the start of the first Submit call for its key.
// NewPayload(aggregateID, signedNote, submitStart) checks the shape the
// anchoring depends on and computes Digest = SHA-256(NoteText), where NoteText
// is the lines of the text (origin, size in ASCII decimal, root as standard
// base64 of 32 bytes, and up to MaxExtensionLines extension lines), each ending
// in a newline, without the blank line and the signature lines. ID is
// "<aggregate id hex>/<size>". SubmitStart is required (a zero one is refused by
// NewPayload and by every Submit): the caller persists it durably before the
// first Submit call for the key, never changes it afterwards, and passes the same
// value on every later call, Retry and restart; it is neither the creation of
// the record nor the time of the current attempt, and a caller that wants a new
// start uses a new key. The WORM target bounds the time of the store by it (see
// package worm); the others do not use it.
//
// A Payload also carries Generation (correction 95): 0 for every record that
// exists, persisted by the caller with the record, and the same for every target;
// only the WORM target puts it in the object key. Payload.WithGeneration(n,
// newStart) is how an operator's explicit choice to anchor a checkpoint under a
// new key, after a permanent refusal of the time of the store, is made: never
// automatically (package worm says when).
//
// The shape rules are the verifier's (correction 92): ParsePayload applies the
// rules of kingres-verify's signednote.Parse and checkpoint.Parse. The note:
// valid UTF-8, no control character but newline, no line over MaxNoteLineLen
// (1024) bytes, at most MaxSignatureLines (32) signature lines, each U+2014, a
// space, a key name (no Unicode white space, no plus sign, no control
// character), a space and strict canonical base64 of at least 4 bytes, no two
// lines with the same name and key id. The text: an origin of 1 to MaxOriginLen
// (255) bytes of UTF-8 without white space, plus signs or control characters; a
// size below TreeSizeLimit (2^62) in canonical decimal; a root in strict
// canonical base64 of 32 bytes (the root of the empty tree when the size is 0);
// at most 16 non-empty extension lines. The numbers are compared by a test with
// the verifier's source when it is beside this package, and a corpus in testdata
// lists accepted and refused notes with the verdict of each rule set. The corpus has a
// byte-identical copy in the verifier module, where a test holds the verifier's
// real parsers to the verifier column, and a test here fails when the two
// copies differ (correction 97). This
// package is stricter on purpose in two places only: it refuses the byte 0x7f
// (DEL) anywhere, and a note over MaxSignedNote (16 KiB; the verifier takes 64
// KiB). ParsePayload applies the same rules without a SubmitStart, for a reader
// of stored notes. Neither verifies a signature.
//
// # The states of a record, and what each means
//
//	pending     to be submitted (or to be submitted again after a failure)
//	submitted   accepted by the target, not final (OpenTimestamps: no Bitcoin
//	            attestation yet)
//	claimed     OpenTimestamps only: the calendars claim a Bitcoin attestation
//	            that passed every check that needs no block header; nothing
//	            checked it against the chain. NOT anchored: it must never be
//	            shown or counted as anchored (contract status "pending"). It
//	            keeps asking the calendars whose fragments are still pending,
//	            falls back to submitted when a header contradicts the claim and
//	            another calendar can still deliver, and expires after its own,
//	            longer timeout (ExpireIfDue)
//	anchored    final and verified as far as the kind allows (below)
//	failed      a person must act (Record.Retry)
//	superseded  a newer checkpoint replaced it before it was submitted
//	expired     OpenTimestamps never got a Bitcoin attestation in time (the
//	            operator may Retry it, like a failed one, correction 88)
//
// What "anchored" means for each kind:
//
//   - worm: the object was written with a lock, read back by its version id
//     (HEAD and GET), and the lock mode, the retention date, the length, the
//     bytes and the store's Last-Modified (within the tolerance of the clock)
//     are those the evidence states; the store's time is bounded by the start of
//     the first Submit call and by the call that wrote the version (package
//     worm, correction 89), and "found" says whether this call wrote the
//     version. The lock is the store's word: in
//     governance mode a principal with the bypass permission can delete the
//     version, and the evidence says GOVERNANCE.
//   - tsa: the token was verified in full against the configured roots, bound
//     to the digest and the nonce, its genTime is within the tolerance of the
//     clock, and the signing certificate chains to the roots both at genTime
//     and now. No revocation check.
//   - ots: only when an operator-configured source of block headers returned
//     the header of the block and the claim matched it (merkle root, proof of
//     work, a block time that fits the submission and the clock). Without such
//     a source the record ends in "claimed". The header is in the evidence.
//
// The state machine is one table (Next, States, Events): every state and every
// event has a next state or a refusal, every method of Record that changes
// the state consults that table and nothing else, and a refused event returns
// an error and the record unchanged. anchored, superseded and expired accept
// no event; failed and expired accept only the operator's Retry (Retry of an
// expired record starts its waiting period again). A claimed record ends at the
// first of two limits: its own timeout (ExpireIfDue, 28 days with the defaults)
// and the limits of the retries when its calendars all fail (200 attempts or 7
// days of failure with the defaults, then failed with the alert): with the
// defaults, the second.
//
// # Evidence: the formats
//
// Evidence is JSON, UTF-8, compact (no white space), the members in the order
// below, no trailing newline, no member omitted (an absent value is "" or
// null as stated), no member not listed. Times are RFC 3339 in UTC with whole
// seconds and the letter Z ("2026-12-01T10:00:00Z", TimeLayout); hexadecimal
// is lower case; base64 is the standard alphabet with padding and no line
// breaks; integers are written without sign, fraction, exponent or leading
// zero. The writer's output is the only accepted input: a parser (ParseEvidenceFile,
// ParseWORMEvidence, ParseTSAEvidence, ParseOTSEvidence) rejects unknown
// members, a member name in another case or spelled with an escape, a
// repeated member, white space, a trailing byte, an escape that the writer
// would not write, and any value that does not follow these rules; the test is
// that parsing and writing again gives the same bytes. A verifier written from
// this text must be as strict.
//
// The evidence file of the publication, a/<aggregate id>/anchors/<size>/<target id>.json,
// is (EncodeEvidenceFile):
//
//	{"format":1,"kind":"worm|tsa|ots","origin":"<origin>","size":<n>,
//	 "payload_sha256":"<64 hex>","created_at":"<time>","evidence":{...}}
//
// origin and size are those of the note, payload_sha256 is SHA-256 of the note
// text (the digest the targets stamped), created_at is when the file was
// written. "evidence" is one of:
//
//	worm: {"endpoint":"","bucket":"","key":"","version_id":"","etag":"",
//	       "object_sha256":"<64 hex>","content_length":<n>,
//	       "lock_mode":"GOVERNANCE|COMPLIANCE","retain_until":"<time>",
//	       "last_modified":"<time>","submit_start":"<time>",
//	       "attempt":null|"<time>","generation":<n>,"found":false,
//	       "recorded_by":"cluster"}
//	tsa:  {"tsa_url":"","token_der_b64":"","gen_time":"<time>","serial":"<decimal>",
//	       "policy":"<dotted OID>","signer_key_sha256":"<64 hex>",
//	       "recorded_by":"cluster"}
//	ots:  {"digest":"<64 hex>","submitted_at":"<time>",
//	       "calendars":[{"uri":"","fragment_b64":""}],
//	       "bitcoin":null | {"height":<n>,"calendar_uri":"","merkle_root":"<64 hex>",
//	                         "status":"claimed|header_checked","header":""|"<160 hex>"},
//	       "contradicted":[{"calendar_uri":"","height":<n>,"merkle_root":"<64 hex>",
//	                        "header":"<160 hex>","at":"<time>","reason":""}],
//	       "dropped":[{"calendar_uri":"","height":<n>,"merkle_root":"<64 hex>",
//	                   "at":"<time>"}]}
//
// What each member means, and what a verifier checks, is in the documentation
// of the three subpackages. Choices where the design is silent:
//
//   - The evidence object of a record (Receipt.Evidence) is the "evidence"
//     member alone; the caller wraps it in the file with EncodeEvidenceFile.
//   - "endpoint" of worm evidence is the configured endpoint, "" for AWS.
//   - worm: the evidence is the cluster's claim ("recorded_by") until a
//     verifier fetches the version itself. It carries SHA-256 and length of
//     the object so that what the verifier fetches or holds can be compared
//     byte for byte (worm.VerifyNote, worm.VerifyHead).
//   - ots "bitcoin" names ONE attestation, the one with the lowest height among
//     those that passed the checks, by calendar, height and merkle_root (the
//     32-byte message the attestation sits on, in header byte order). The
//     fragments may hold more attestations; a verifier checks every Bitcoin
//     attestation of every fragment, not only the named one, and says which
//     one it used. status "claimed" means nothing but a calendar's word;
//     "header_checked" means that "header" (80 bytes, 160 hex digits) passed
//     the checks in the evidence. Whether that header is on the best chain is
//     for the auditor, who supplies their own.
//   - A WORM anchor has no "submitted" state: a failed read-back after a good
//     write is an error, and the retry finds the object and completes the
//     verification.
//
// # Errors and retries
//
// Every target returns *Error. ClassOf says what to do (design 6.6):
//
//	retryable  unreachable, timeout, 5xx, 429, TSA "waiting", a clock that the
//	           store finds wrong, a round of calendar answers that were all
//	           permanent failures: Backoff, and RetryAfterOf when larger;
//	           bounded by Limits (an attempt limit and an age limit)
//	refused    401, 403, bucket missing, lock not enabled, TSA rejection, a time
//	           that is not accepted (a store or a TSA ahead of the clock, a TSA
//	           certificate not valid now), a read-back that fails with a 4xx:
//	           retried after 1, 5 and 15 minutes (RefusedRetryDelay); the fourth
//	           consecutive refusal fails the record
//	permanent  malformed answer, unlocked object, bad token, an object whose
//	           time is earlier than the start of the first Submit call: failed, no retry
//	conflict   the store holds other content under the key: failed, no retry,
//	           alert ledgerAnchorConflict
//
// The class says what to do with the record; it does not say what is wrong.
// "Refused" covers credentials and also clocks, and the alert text must come
// from the code and the text of the error (CodeOf, Error()), which carry the
// cause ("store_time_skew", "time_skew", "chain_not_trusted_now" say clock or
// certificate validity and say what to do), never from the class: an alert that
// reads "credentials" for a skewed clock sends the operator to the wrong place.
//
// Record.AfterError applies this to a record and names the alert to raise. It
// counts consecutive refusals apart from the failures of any other kind
// (Record.Refusals), the failures since the last success (Record.Attempts) and
// the consecutive retryable ones (Record.Retryables, the exponent of the
// backoff); a retryable failure past Limits.MaxAttempts or Limits.MaxAge
// (from Record.FailingSince) fails the record with ledgerAnchorFailed.
//
// # What the caller must do
//
// Persist the start of the first Submit call for a key before that call, build
// every payload with it (and never with the creation of the record or the time
// of the current attempt), keep it across retries, and use the code and the
// text of an error for the alert.
//
// Store the records (kingres_cp.ledger_anchor, with the columns refusals,
// retryables and failing_since besides attempts; the evidence as text, or as
// jsonb read back through CanonicalEvidence, because the strict parsers accept
// the canonical bytes only and jsonb reorders members), run the jobs (enqueue,
// submit, upgrade) on the leader with the term check, call Submit when a
// record is due and apply the result with Record.ApplyReceipt or
// Record.AfterError (check the error: a refused event means the job is stale),
// keep the first stored evidence per (target, log, size), write the evidence
// file with EncodeEvidenceFile when a record is anchored (and, for ots, when
// it is claimed, with status "claimed"), build the *http.Client with
// NewHTTPClient from the CA bundle, load credentials, raise the alerts, show a
// "claimed" record as claimed and never count it toward anchor.minimumAnchored,
// and run ExpireIfDue (with ots.Timeout and ots.ClaimedTimeout) and
// UpgradeInterval for OpenTimestamps, for submitted and claimed records
// alike. This package does none of these.
package anchor

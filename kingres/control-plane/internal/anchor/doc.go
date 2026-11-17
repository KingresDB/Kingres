// SPDX-License-Identifier: AGPL-3.0-only

// Package anchor is the library half of anchoring a signed checkpoint of the
// ledger at an outside party (design: kingres/docs/design/ledger-phase7.md,
// section 6 and corrections 70 to 76 of section 14). It defines the Target
// interface, the payload, the receipt, the anchor record with its states and
// its state machine, the classification of errors, the retry arithmetic, the
// HTTP client the targets are meant to be given, and the canonical encoding of
// evidence. The targets themselves are in the subpackages worm (S3 Object
// Lock), tsa (RFC 3161) and ots (OpenTimestamps); sigv4 is the request
// signing that worm uses.
//
// # What it guarantees
//
//   - No storage, no scheduler, no goroutine that outlives a call, no global
//     state. Every network call takes a context (its deadline is the timeout)
//     and an *http.Client supplied by the caller; time comes from a Clock
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
// signed-note). NewPayload checks the shape the anchoring depends on and
// computes Digest = SHA-256(NoteText), where NoteText is the three lines
// (origin, size in ASCII decimal, root as standard base64 of 32 bytes), each
// ending in a newline, without the blank line and the signature lines. ID is
// "<aggregate id hex>/<size>". NewPayload does not verify a signature.
//
// # The states of a record, and what each means
//
//	pending     to be submitted (or to be submitted again after a failure)
//	submitted   accepted by the target, not final (OpenTimestamps: no Bitcoin
//	            attestation yet)
//	claimed     OpenTimestamps only: the calendars claim a Bitcoin attestation
//	            that passed every check that needs no block header; nothing
//	            checked it against the chain. NOT anchored: it must never be
//	            shown or counted as anchored (contract status "pending")
//	anchored    final and verified as far as the kind allows (below)
//	failed      a person must act (Record.Retry)
//	superseded  a newer checkpoint replaced it before it was submitted
//	expired     OpenTimestamps never got a Bitcoin attestation in time
//
// What "anchored" means for each kind:
//
//   - worm: the object was written with a lock, read back by its version id
//     (HEAD and GET), and the lock mode, the retention date, the length, the
//     bytes and the store's Last-Modified (within the tolerance of the clock)
//     are those the evidence states. The lock is the store's word: in
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
// no event; failed accepts only the operator's Retry.
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
//	       "last_modified":"<time>","recorded_by":"cluster"}
//	tsa:  {"tsa_url":"","token_der_b64":"","gen_time":"<time>","serial":"<decimal>",
//	       "policy":"<dotted OID>","recorded_by":"cluster"}
//	ots:  {"digest":"<64 hex>","submitted_at":"<time>",
//	       "calendars":[{"uri":"","fragment_b64":""}],
//	       "bitcoin":null | {"height":<n>,"calendar_uri":"","merkle_root":"<64 hex>",
//	                         "status":"claimed|header_checked","header":""|"<160 hex>"}}
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
//	           store finds wrong: Backoff, and RetryAfterOf when larger; bounded
//	           by Limits (an attempt limit and an age limit)
//	refused    401, 403, bucket missing, lock not enabled, TSA rejection, a time
//	           that is not accepted, a read-back that fails with a 4xx: retried
//	           after 1, 5 and 15 minutes (RefusedRetryDelay); the fourth
//	           consecutive refusal fails the record
//	permanent  malformed answer, unlocked object, bad token: failed, no retry
//	conflict   the store holds other content under the key: failed, no retry,
//	           alert ledgerAnchorConflict
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
// and run ExpireIfDue and UpgradeInterval for OpenTimestamps. This package
// does none of these.
package anchor

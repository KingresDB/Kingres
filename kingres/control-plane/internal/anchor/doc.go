// SPDX-License-Identifier: AGPL-3.0-only

// Package anchor is the library half of anchoring a signed checkpoint of the
// ledger at an outside party (design: kingres/docs/design/ledger-phase7.md,
// section 6). It defines the Target interface, the payload, the receipt, the
// anchor record with its states, the classification of errors, the retry
// arithmetic, and the canonical encoding of evidence. The targets themselves
// are in the subpackages worm (S3 Object Lock), tsa (RFC 3161) and ots
// (OpenTimestamps); sigv4 is the request signing that worm uses.
//
// # What it guarantees
//
//   - No storage, no scheduler, no goroutine that outlives a call, no global
//     state. Every network call takes a context (its deadline is the timeout)
//     and an *http.Client supplied by the caller; time comes from a Clock
//     argument. The functions on Record are pure: they return a changed copy.
//   - Every response read has a size limit (constants in each package, with a
//     test at the boundary). No redirect is followed to another host (Client).
//   - No credential appears in any error, String, GoString, Format, JSON or
//     text encoding: credentials are of type Secret, errors are built from
//     static text, statuses and sanitised store messages (Redactor), and the
//     types that hold credentials print a placeholder as a whole.
//   - Evidence is deterministic: the same value always encodes to the same
//     bytes (below), and what a target produces parses back with this package.
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
// # Evidence: the formats
//
// Evidence is JSON, UTF-8, compact (no white space), the members in the order
// below, no trailing newline, no member omitted (an absent value is "" or
// null as stated), no member not listed. Times are RFC 3339 in UTC with whole
// seconds and the letter Z ("2026-12-01T10:00:00Z", TimeLayout); hexadecimal
// is lower case; base64 is the standard alphabet with padding. A parser
// rejects unknown members, trailing data and values that do not follow these
// rules. EncodeEvidence and the Parse functions are the reference for them.
//
// The evidence file of the publication, a/<aggregate id>/anchors/<size>/<target id>.json,
// is (EncodeEvidenceFile):
//
//	{"format":1,"kind":"worm|tsa|ots","origin":"<origin>","size":<n>,
//	 "payload_sha256":"<64 hex>","created_at":"<time>","evidence":{...}}
//
// where "evidence" is one of:
//
//	worm: {"endpoint":"","bucket":"","key":"","version_id":"","etag":"",
//	       "lock_mode":"GOVERNANCE|COMPLIANCE","retain_until":"<time>",
//	       "last_modified":"<time>","recorded_by":"cluster"}
//	tsa:  {"tsa_url":"","token_der_b64":"","gen_time":"<time>","serial":"<decimal>",
//	       "policy":"<dotted OID>","recorded_by":"cluster"}
//	ots:  {"digest":"<64 hex>","calendars":[{"uri":"","fragment_b64":""}],
//	       "bitcoin":null | {"height":<n>,"calendar_uri":""}}
//
// What each member means, and what a verifier checks, is in the documentation
// of the three subpackages. Choices where the design is silent:
//
//   - The evidence object of a record (Receipt.Evidence) is the "evidence"
//     member alone; the caller wraps it in the file with EncodeEvidenceFile.
//   - "endpoint" of worm evidence is the configured endpoint, "" for AWS.
//   - The ots "bitcoin" member names the calendar whose fragment carries the
//     attestation and the lowest height in it.
//   - A WORM anchor has no "submitted" state: a failed read-back after a good
//     write is a retryable error, and the retry finds the object (412) and
//     completes the verification (the design's "submitted" for a failed
//     verification read is not used).
//
// # Errors and retries
//
// Every target returns *Error. ClassOf says what to do (design 6.6):
//
//	retryable  unreachable, timeout, 5xx, 429, TSA "waiting": Backoff, and
//	           RetryAfterOf when larger
//	refused    401, 403, bucket missing, lock not enabled, TSA rejection:
//	           retried after 1, 5 and 15 minutes (RefusedRetryDelays); the
//	           fourth consecutive refusal fails the record
//	permanent  malformed answer, unlocked object, bad token: failed, no retry
//	conflict   the store holds other content under the key: failed, no retry,
//	           alert ledgerAnchorConflict
//
// Record.AfterError applies this to a record and names the alert to raise.
//
// # What the caller must do
//
// Store the records (kingres_cp.ledger_anchor), run the jobs (enqueue, submit,
// upgrade) on the leader with the term check, call Submit when a record is due
// and apply the result with Record.ApplyReceipt or Record.AfterError, keep the
// first stored evidence per (target, log, size), write the evidence file with
// EncodeEvidenceFile when a record is anchored, load credentials and CA files
// and build the *http.Client, raise the alerts, and run ExpireIfDue and
// UpgradeInterval for OpenTimestamps. This package does none of these.
package anchor

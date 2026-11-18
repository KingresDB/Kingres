// SPDX-License-Identifier: AGPL-3.0-only

// Package worm anchors a signed checkpoint by writing it to an S3 compatible
// store under Object Lock (design 6.3, with corrections 60 to 65, 81 to 84 and
// 88 to 92 of section 14; the ones that describe this code are 81, 82, 89 and 95).
// It implements anchor.Target with kind "worm".
//
// # What Submit does
//
//  1. The key is <keyPrefix>/<aggregate id hex>/<size as 20 decimal digits,
//     zero padded>.note (ObjectKey) for generation 0 of the payload, and
//     <size>.g<n>.note for generation n above 0 (ObjectKeyGen; correction 95);
//     the body is the signed note.
//  2. PutObject, signed with SigV4 (package sigv4), with Content-Type
//     "text/plain; charset=utf-8", x-amz-checksum-sha256 (base64 SHA-256 of
//     the body), If-None-Match: *, x-amz-object-lock-mode (GOVERNANCE or
//     COMPLIANCE), x-amz-object-lock-retain-until-date (the clock plus
//     retentionDays, RFC 3339 UTC, whole seconds) and the user metadata
//     x-amz-meta-kingres-attempt: the clock of this server at the start of
//     this call (TimeLayout).
//  3. On 200 the version is read back: HEAD with versionId and
//     x-amz-checksum-mode ENABLED, then GET of that version. The object must
//     carry the configured lock mode (or COMPLIANCE when GOVERNANCE is
//     configured, the stronger lock) and a retain-until date not earlier than
//     required (below) and in the future; Content-Length, the version id
//     (which must exist: no version id is no anchor, Code "no_version_id")
//     and, when the store returns it, the sha256 checksum must match; the
//     bytes of THAT version must equal the note; Last-Modified must be
//     present and inside the window of the store's time (next section). The user metadata
//     of the attempt must come back on the read-back of a version this call wrote
//     exactly as it was sent (one value, the same text); a store that drops it,
//     changes it or repeats it is refused with Code "no_user_metadata" (permanent:
//     the object is written and locked and stays, the record does not spin; the
//     text says what the operator does; correction 95). A
//     store that accepts the put without locking is reported as a permanent
//     error with Code "not_locked". A failed read-back is retryable for a
//     transport failure or a 5xx and refused (a few tries, then failed) for
//     any other answer. A start of the first Submit call later than the clock
//     plus the tolerance is Code "submit_start_ahead", retryable, and nothing
//     is sent.
//  4. On 412, or 409 ConditionalRequestConflict, the key is read: the latest
//     version when its bytes equal the note, otherwise the versions of the key
//     are listed (newest first, at most MaxVersionsChecked read) and every
//     version with the bytes of the note is verified as in 3, as a version that
//     was found (an existing one). A foreign later version over the key is
//     therefore not a conflict as long as our version is there, and it does not
//     decide alone: when the latest version has our bytes and fails its checks
//     (a copy another principal placed after ours, unlocked, in a weaker mode or
//     with a forged attempt), the other versions are looked at, bounded as above,
//     and the first of ours that passes completes the record; when none passes,
//     the error is that of the latest (correction 95). Only when no
//     version of the key holds the note is it an error of class conflict (Code
//     "worm_conflict"); nothing is ever overwritten. A version of ours that has
//     no lock, or a retention shorter than the floor below, gets the retention
//     applied (PutObjectRetention on that version, with Content-MD5, in the mode
//     the version has, until the retention asked now) and is verified again: an
//     object that an earlier Submit left unlocked or locked too short, or whose
//     lock has expired, is completed by the next one. When the store refuses,
//     the error (Code "not_locked") names the key, the version, the mode and the
//     date, says that the lock is missing, too short or expired, and says to
//     apply them with the store's tools and to Retry (which works after any
//     delay). Nothing is repaired on an object whose time is refused.
//  5. With ifNoneMatch "auto" a store that answers the put with 501, or 400
//     that does not mention the lock, is handled by a check before the write
//     (HEAD of the key, put without the header when absent). "on" refuses such
//     a store (class refused); "off" always checks first. The check is not
//     atomic: object lock is what protects an existing version.
//
// # The time of the store (correction 89)
//
// The time the evidence states, and the cluster vouches for, is the store's
// Last-Modified. The property this package keeps: for a version that this
// package wrote, the time stated is never further from the true time of the
// write than the tolerance (Config.MaxSkewSeconds, default 300 s) plus the
// duration of the call that wrote it (at most CallTimeout, 30 s), whatever the
// age of the record, the number of attempts, the Retries and the crashes; and
// for any version it is never earlier than SubmitStart less the tolerance. The
// rules:
//
//   - SubmitStart (Payload.SubmitStart) is the start of the FIRST Submit call
//     that the checkpoint had for this key. The caller persists it before that
//     call and never changes it; a Retry keeps it; a caller that wants a new
//     start uses a new key (another keyPrefix). It is not the creation of the
//     record.
//   - A version that this call has just written must have a time within the
//     tolerance of now (not earlier than now less it, not later than now plus
//     it). A store whose clock is wrong by more is refused at once.
//   - A version that is found (a later call, a crash recovery, a Retry, a
//     version that another principal wrote) must have a time not earlier than
//     SubmitStart less the tolerance and not later than now plus the tolerance.
//     This is a wider rule than the fresh one, on purpose: a version found after
//     a crash is old by any amount. When the version carries the user metadata
//     kingres-attempt (every put of this package sets it, and the store keeps
//     it with the version), the window is narrowed to that start less the
//     tolerance up to that start plus CallTimeout plus the tolerance, so that a
//     Retry or a crash recovery days after SubmitStart does not widen it: the
//     object is judged by the call that wrote it, as the fresh rule judged it.
//     A value that is present and is not exactly one value in the format this
//     package writes (another time format, an offset, a fraction, a lower case z,
//     a joined list, several header lines, an empty value) is an error for that
//     version, Code "attempt_malformed" (permanent), and is never judged as
//     absent; the evidence states which window was applied (member "attempt").
//   - So the three cases of the third review are refused: a read-back that fails
//     after the put (the next call finds an object whose store is behind or
//     ahead), a refusal followed by a Retry, and a record that sat failed for
//     days. Refused for a store behind: Code "store_time_early", class
//     permanent (the object is locked and keeps its time; a Retry finds the same
//     object). Refused for a store ahead: Code "store_time_skew", class refused,
//     and it stays refused when the clock catches up, for an object that carries
//     the start of its call (for a found object without it, the clock catching
//     up can make a time that is ahead acceptable: it is then later than the
//     truth, which only weakens the bound the evidence gives). The text of both
//     says that it is a problem of clocks, not of credentials. For an object that
//     carries its attempt (and for one this call wrote) the text of store_time_skew
//     also says that waiting and a Retry find the same object and are refused in
//     the same way, and what the operator can do: a new key generation (below) or
//     another keyPrefix; for a found object without the attempt it says Retry,
//     since a clock that has caught up can make its time acceptable.
//
// What is left, stated so that it can be tested: a version found WITHOUT the
// metadata (written by another principal, or by a writer that is not this
// package) may have a time as early as SubmitStart less the tolerance, so for
// such a version the window is the time since SubmitStart; the evidence says
// "found": true, states the SubmitStart that was applied and "attempt": null, so
// an auditor sees which window it was. The metadata is required at the write (a
// store that drops it is refused, "no_user_metadata"), so null appears only for
// a version another principal wrote, for a version whose read-back failed and
// whose metadata was lost afterwards, or after the operator chose to Retry a
// "no_user_metadata" refusal. For a version this package wrote with the
// metadata kept, the residual window is the tolerance plus the duration of one
// Submit call. What a wrong SubmitStart does: later
// than the true start by more than the tolerance, the object that the first call
// wrote is refused for good ("store_time_early", change the keyPrefix); a start
// later than the clock plus the tolerance is retryable and sends nothing; earlier
// than the true start (the creation of the record, 1970), it widens the window of
// a version found without the metadata only, to the stated start, and the
// evidence says which start was applied; different between calls, the verdict
// follows the start of the call.
//
// # The generation of the key: a way out per record (correction 95)
//
// A clock fault of the cluster during one Submit call (an NTP step, a resumed
// virtual machine) leaves an object whose time is refused on every later call
// (it carries the start of that call), and on an honest store nothing heals it:
// the key is dead for that checkpoint. The way out that does not change the
// configuration of the target is the generation of the key: anchor.Payload
// carries Generation (0 for every record that exists; the key of generation 0 is
// exactly the key of ObjectKey), the caller persists it with the record, and
// Payload.WithGeneration(n, newStart) gives the payload of generation n, which is
// a new key, so it has a new first Submit call and a new start. The evidence
// records the generation (member "generation") and VerifyNote checks it against
// the key. Every target receives the same payload; only this one uses the value.
// The caller raises it only on an operator.s explicit choice, after a permanent
// refusal of the time of the store (store_time_early, store_time_skew of an
// object that carries its attempt, attempt_malformed), once the clocks are
// right, and never automatically: each generation is one more object in the
// bucket that claims to be the anchor of that checkpoint, locked until its
// retention ends, and a loop that raised it would fill the bucket. The earlier
// generations stay as they are; nothing is deleted. At most anchor.MaxGeneration.

// # The retention floor
//
// The floor of the retain-until date comes from the store's own timeline and not
// from a time the caller states: for a version this call wrote, the date asked
// now less RetentionTolerance; for a found version, its own Last-Modified plus
// the configured days, less the tolerance of the time and RetentionTolerance. A
// found version that is older than its own retention period (so that this floor
// would be in the past, where a lock that ends in a minute would pass) is held to
// what a fresh anchor gets: its retention is extended to now plus the configured
// days (less RetentionTolerance) and it is verified again (correction 95). A
// found version with a date below the floor is repaired, as in step 4. A version that is found in the other mode: COMPLIANCE where
// GOVERNANCE is configured is accepted (the stronger lock; the evidence names
// the mode the version has, and a short lock is repaired in that mode, never
// weakened); GOVERNANCE where COMPLIANCE is configured is refused (Code
// "not_locked", permanent, nothing touched), because a principal with the bypass
// permission could delete it and the configuration promised that nobody can; a
// changed configuration then needs a lock applied with the store's tools or
// another keyPrefix.
//
// # Evidence
//
// anchor.WORMEvidence, encoded by anchor.EncodeEvidence (see package anchor):
//
//	{"endpoint":"","bucket":"","key":"","version_id":"","etag":"",
//	 "object_sha256":"<64 hex>","content_length":<n>,
//	 "lock_mode":"GOVERNANCE|COMPLIANCE","retain_until":"<time>",
//	 "last_modified":"<time>","submit_start":"<time>","attempt":null|"<time>",
//	 "generation":0,"found":false,"recorded_by":"cluster"}
//
// endpoint is the configured endpoint ("" for AWS), key the object key,
// version_id the version read back (never empty), etag the ETag of the
// read-back as received (with its quotes), object_sha256 and content_length the
// SHA-256 (lower case hex) and the length of the object's bytes that were
// written and read back from that version (the signed note), lock_mode,
// retain_until and last_modified the values the store returned (whole
// seconds, UTC), submit_start the SubmitStart that was applied, attempt the
// attempt time that was used for the window (the value of the user metadata
// of the version, which for a version this call wrote was read back equal to the
// value sent) or null when the version carried none, generation the generation
// of the key, found false
// when this call wrote the version and true when it found it (an earlier call
// of this package that did not finish, or another principal), recorded_by
// always "cluster". Receipt.State is "anchored" and Receipt.EvidenceTime is
// last_modified, the store's clock, bounded as above. What a verifier does:
// VerifyNote (the note in hand, from the publication or fetched, is exactly the
// object that the evidence describes by SHA-256 and length, is a well-formed
// signed note whose text hashes to payload_sha256 of the file, and sits at the
// key of the evidence), then, if it can reach the store, VerifyHead on the
// headers of its own HEAD of the version (version id, lock mode, retention date,
// length), then the signature under its pinned key. The lock, the retention date
// and the time are claims of the cluster ("recorded_by") until the verifier's own
// fetch returns them; an evidence that cannot be re-fetched is "not
// independently checked". In governance mode a principal with
// s3:BypassGovernanceRetention can still delete the version; the evidence says
// so with GOVERNANCE, and the console must show it. Two writers (two leaders in
// a term change) with different notes of one size both succeed under the
// fallback without If-None-Match, or leave two versions: the evidence of each
// names its own version and nobody is told of the fork by this package.
//
// # Configuration
//
// Config (JSON, unknown members rejected): endpoint, region, bucket, keyPrefix
// (default kingres-anchors), pathStyle (default true when endpoint is set),
// mode (compliance by default, governance as an explicit choice that Warnings
// and Test report: only compliance cannot be bypassed), retentionDays (3650; 1
// to 36500), ifNoneMatch (auto, on, off), caBundlePath (kept for the caller,
// which builds the client; this package reads no file), allowInsecure,
// maxSkewSeconds (the tolerance of the store's time, default 300).
//
// Choices where the design is silent: allowInsecure (not in the design's list)
// permits an http endpoint, since without it every URL must be https and
// there is no other way to say "TLS is not required"; redirects are never
// followed, not even to the same host (a 3xx is a permanent error "redirect",
// the usual cause being a wrong endpoint or region); virtual-host addressing
// uses <bucket>.<host>, and for AWS the host s3.<region>.amazonaws.com
// (s3.amazonaws.com for us-east-1); a failed read-back after a good put is a
// retryable error (the next Submit finds the key by 412), not a "submitted"
// receipt; the probe of Test is written twice with If-None-Match to see
// whether the store honours it (a store that ignores the header gets a second
// version of the probe key).
//
// # Limits
//
// Responses are read up to MaxErrorBody (64 KiB) for errors and the small XML
// answers of Test, and anchor.MaxSignedNote for a note being read back; a
// longer body is an error (a different, larger object under the key is a
// conflict). Every call is limited to CallTimeout (30 s) or the caller's
// earlier deadline. Store messages are reduced to printable ASCII, cut to
// MaxMessage bytes, and stripped of the credentials.
//
// # What the caller must do (the notes for the wiring)
//
// Load the credentials and the CA bundle, build the *http.Client with
// anchor.NewHTTPClient (no proxy from the environment, the roots of the bundle,
// timeouts; whatever client is passed, this package uses no proxy).
//
// Persist, BEFORE the first Submit call for a key, the time at which that call
// starts (the clock of the server that makes it), as one durable value per (target,
// aggregate, size) and key prefix; pass exactly that value to anchor.NewPayload
// on every later call for that key, after a crash, a restart, a leader change
// and every Retry, and never replace it. If the process dies between persisting
// it and making the call, keep the value: it is then earlier than the true
// start, which is safe (the objects this package writes carry the start of their
// own call). Do not pass the creation time of the record, and do not pass the
// time of the current attempt. Call Submit for due records and keep the first
// evidence for a key, raise the alerts that anchor.Record.AfterError names, and
// warn when retain_until approaches. The text of an alert must come from the
// code and the text of the error (anchor.CodeOf, Error()), not from its class:
// "store_time_skew" and "store_time_early" are about clocks, "submit_start_ahead"
// about the clock of this server or of the value persisted, and none of them is
// about credentials although the first is of class refused. The clocks of the
// servers that make calls must agree within the tolerance.
//
// Persist the generation of the key (Payload.Generation, 0 for a new record) with
// the record and pass it on every call. Raise it with Payload.WithGeneration(n,
// newStart), by one and with a new start taken from a clock that is right, only
// when an operator chooses to anchor the checkpoint under a new key after a
// permanent refusal of the time of the store (the codes store_time_early,
// store_time_skew of an object that carries its attempt, attempt_malformed; the
// text of the error says so); never automatically, and never because a Retry
// failed: each generation is one more object that claims to be the anchor of that
// checkpoint. A store that does not keep user metadata ("no_user_metadata") needs
// another store, or a gateway that keeps it; a Retry of that refusal accepts the
// object under the wide window and the evidence states "attempt": null. Warn when
// the evidence of a found object states "attempt": null: its time is bounded by
// SubmitStart only.
//
// Give the cluster a bucket that nobody else writes, with versioning and Object
// Lock, used for nothing else. Another principal that can write the bucket can
// write our key with our bytes (the note is public once published) and be
// adopted (the evidence says found), can write versions of other bytes after
// ours until the newest MaxVersionsChecked hide ours and the record ends in
// class conflict with the critical alert, and can write junk under the key of a
// future size; none of this can be told from the cluster's own writes except by
// the "found" member and the metadata. This package does not store anything,
// retry, or delete objects.
package worm

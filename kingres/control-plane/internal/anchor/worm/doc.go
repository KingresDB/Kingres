// SPDX-License-Identifier: AGPL-3.0-only

// Package worm anchors a signed checkpoint by writing it to an S3 compatible
// store under Object Lock (design 6.3). It implements anchor.Target with kind
// "worm".
//
// # What Submit does
//
//  1. The key is <keyPrefix>/<aggregate id hex>/<size as 20 decimal digits,
//     zero padded>.note (ObjectKey); the body is the signed note.
//  2. PutObject, signed with SigV4 (package sigv4), with Content-Type
//     "text/plain; charset=utf-8", x-amz-checksum-sha256 (base64 SHA-256 of
//     the body), If-None-Match: *, x-amz-object-lock-mode (GOVERNANCE or
//     COMPLIANCE) and x-amz-object-lock-retain-until-date (the clock plus
//     retentionDays, RFC 3339 UTC, whole seconds).
//  3. On 200 the version is read back: HEAD with versionId and
//     x-amz-checksum-mode ENABLED, then GET of that version. The object must
//     carry the configured lock mode and a retain-until date not earlier than
//     requested (5 minutes of tolerance when it was just written, 8 days when an
//     earlier attempt wrote it) and in the future; Content-Length, the version
//     id (which must exist: no version id is no anchor, Code "no_version_id")
//     and, when the store returns it, the sha256 checksum must match; the
//     bytes of THAT version must equal the note; Last-Modified must be present
//     and within Config.MaxSkewSeconds (default 300) of the clock for a fresh
//     write, or within 8 days before and the tolerance after the clock for
//     an object an earlier attempt wrote (Code "store_time_skew", class refused:
//     the time the evidence states is the store's clock, and a store with a
//     wrong clock is refused rather than believed). A store that accepts the
//     put without locking is reported as a permanent error with Code
//     "not_locked". A failed read-back is retryable for a transport failure or
//     a 5xx and refused (a few tries, then failed) for any other answer.
//  4. On 412, or 409 ConditionalRequestConflict, the key is read: the latest
//     version when its bytes equal the note, otherwise the versions of the key
//     are listed (newest first, at most MaxVersionsChecked read) and every
//     version with the bytes of the note is verified as in 3. A foreign later
//     version over the key is therefore not a conflict as long as our version is
//     there. Only when no version of the key holds the note is it an error of
//     class conflict (Code "worm_conflict"); nothing is ever overwritten.
//     A version of ours that has no lock, or a retention shorter than asked in
//     the configured mode, gets the retention applied (PutObjectRetention on
//     that version, with Content-MD5) and is verified again: an object that an
//     earlier Submit left unlocked is completed by the next one. When the store
//     refuses, the error (Code "not_locked") names the key, the version, the
//     mode and the date, and says to apply them with the store's tools and to
//     Retry; a version locked in another mode is never touched.
//  5. With ifNoneMatch "auto" a store that answers the put with 501, or 400
//     that does not mention the lock, is handled by a check before the write
//     (HEAD of the key, put without the header when absent). "on" refuses such
//     a store (class refused); "off" always checks first. The check is not
//     atomic: object lock is what protects an existing version.
//
// # Evidence
//
// anchor.WORMEvidence, encoded by anchor.EncodeEvidence (see package anchor):
//
//	{"endpoint":"","bucket":"","key":"","version_id":"","etag":"",
//	 "object_sha256":"<64 hex>","content_length":<n>,
//	 "lock_mode":"GOVERNANCE|COMPLIANCE","retain_until":"<time>",
//	 "last_modified":"<time>","recorded_by":"cluster"}
//
// endpoint is the configured endpoint ("" for AWS), key the object key,
// version_id the version read back (never empty), etag the ETag of the
// read-back as received (with its quotes), object_sha256 and content_length the
// SHA-256 (lower case hex) and the length of the object's bytes that were
// written and read back from that version (the signed note), lock_mode,
// retain_until and last_modified the values the store returned (whole
// seconds, UTC), recorded_by always "cluster". Receipt.State is "anchored"
// and Receipt.EvidenceTime is last_modified, the store's clock, which was
// bounded against the cluster's clock. What a verifier does: VerifyNote (the
// note in hand, from the publication or fetched, is exactly the object that
// the evidence describes by SHA-256 and length, is a well-formed signed note
// whose text hashes to payload_sha256 of the file, and sits at the key of the
// evidence), then, if it can reach the store, VerifyHead on the headers of its
// own HEAD of the version (version id, lock mode, retention date, length),
// then the signature under its pinned key. The lock, the retention date and
// the time are claims of the cluster ("recorded_by") until the verifier's own
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
// # What the caller must do
//
// Load the credentials and the CA bundle, build the *http.Client with
// anchor.NewHTTPClient (no proxy from the environment, the roots of the bundle,
// timeouts; whatever client is passed, this package uses no proxy), call Submit
// for due records and keep the first evidence for a key, raise the alerts that
// anchor.Record.AfterError names, and warn when retain_until approaches. This
// package does not store anything, retry, or delete objects.
package worm

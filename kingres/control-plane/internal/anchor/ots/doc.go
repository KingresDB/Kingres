// SPDX-License-Identifier: AGPL-3.0-only

// Package ots anchors the digest of a signed checkpoint with OpenTimestamps
// calendars (section 6.5 of the phase 7 design): it submits the digest to
// several calendars, keeps the fragments they return, asks the calendars
// again until a fragment holds a Bitcoin attestation, and assembles the
// detached ".ots" proof file. It also parses and writes the proof format and
// computes what a proof commits to, so that a verifier can check it against a
// Bitcoin block header that an auditor supplies.
//
// # What it guarantees
//
//   - Every parser is bounded: MaxProofSize bytes, MaxDepth levels, MaxNodes
//     nodes, MaxMessageLen bytes per message, MaxPayloadLen per attestation
//     payload, MaxURILen per pending URI, MaxCalendarResponse bytes per
//     calendar answer, MaxFragment bytes per stored fragment.
//   - Parsing then serialising a proof gives the same bytes as the reference
//     library writes (attestations sorted, then operations sorted, every
//     item but the last introduced by 0xff). All sample files of the
//     reference client round-trip byte for byte (see the tests).
//   - No request goes to a host other than the configured calendars: only
//     same-host, same-scheme redirects are followed (at most 3). A pending
//     attestation whose URI is not one of the configured calendars is never
//     asked. No message contains a URL with user information (the
//     configuration refuses it), a response body, or a credential.
//   - The package stores nothing and starts no goroutine that outlives a call.
//     The caller's context bounds every call; each calendar request is also
//     capped at CallTimeout (20 s).
//
// # Formats
//
// Operations. Implemented: append (0xf0), prepend (0xf1), sha1 (0x02),
// ripemd160 (0x03, an implementation of this package), sha256 (0x08).
// Refused by name with *UnsupportedOpError: reverse (0xf2), hexlify (0xf3),
// keccak256 (0x67); any other tag is refused as unknown. A detached file's
// file hash operation must be sha1, ripemd160 or sha256.
//
// Attestations. Pending (tag 83dfe30d2ef90c8e): payload = varbytes(URI),
// URI of at most 1000 bytes from the letters, digits and "-._/:". Bitcoin
// (tag 0588960d73d71901): payload = varuint(height), height below 2^32. Any
// other 8-byte tag is kept opaque with its payload (at most 8192 bytes) and
// written back unchanged; it is never "completed".
//
// Detached file: the 31 magic bytes "\x00OpenTimestamps\x00\x00Proof\x00" and
// bf89e2e884e89294, the major version byte 1, the file hash operation tag,
// the digest (20 or 32 bytes), then the tree. The kingresd export always
// uses sha256 and the 32-byte checkpoint digest.
//
// Tree: a node is the message it stands for; its serialisation is, in order,
// each attestation as 0x00 tag(8) varbytes(payload) and each edge as
// op-tag [varbytes(arg)] then the child's serialisation; every item except
// the last of a node is preceded by 0xff. Varuint is little-endian base 128
// with the continuation bit in 0x80, at most 9 bytes.
//
// # What a Bitcoin attestation makes of a record
//
// kingresd holds no block headers, so it cannot verify a Bitcoin attestation
// by itself; a calendar's attestation is a CLAIM. Upgrade accepts a claim as a
// claim (receipt state "claimed", evidence status "claimed") only if every
// check that needs no header passes, each recomputed from the digest and never
// taken from the tree shape the calendar sent:
//
//   - the message the attestation sits on is 32 bytes (a merkle root);
//   - the operations from the digest to it end in two SHA-256 operations (a
//     Bitcoin merkle node is a double SHA-256), so the digest is committed
//     to by the path, and an attestation on the digest itself or on a path
//     with fewer operations is refused;
//   - the height is above the highest block that was mined before the digest
//     was made (HeightBounds: the table of checkpoints is the real headers of
//     testdata/headers.txt, with a slack of BlockTimeSlack) and not above what
//     the chain can have reached by the clock (one block per two minutes since
//     the last checkpoint, plus a difficulty period);
//   - no other claim at the same height names another merkle root (a block
//     has one): such claims are not plausible until a header settles them.
//
// An answer that holds a claim that fails these checks loses that claim; the
// rest of the answer, and the other calendars', are kept. A receipt is
// "anchored" only when a HeaderSource (Options.Headers, the operator's) returned
// the 80-byte header of the block at the height and the claim matched it: the
// proof-of-work check of CheckHeader, the merkle root as it stands in bytes 36
// to 68, and a block time not older than the submission (less BlockTimeSlack)
// and not more than FutureSlack ahead of the clock. The header is kept in the
// evidence and EvidenceTime is its time. A source that cannot be reached leaves
// the claim a claim, to be checked again (Receipt.NextCheck); a header that
// contradicts a claim makes the claim false, and when nothing else is plausible
// Upgrade returns an error of class conflict (code "claim_contradicted").
//
// # Evidence (stored by the caller, see package anchor)
//
// The evidence of an OTS record is anchor.OTSEvidence, encoded by
// anchor.EncodeEvidence: {"digest": hex(32 bytes), "submitted_at": time,
// "calendars": [{"uri": base URL of the calendar, "fragment_b64": base64 of the
// serialised tree rooted at digest}], "bitcoin": null or {"height": H,
// "calendar_uri": URI of the calendar whose fragment holds the attestation,
// "merkle_root": hex of the message it sits on, "status": "claimed" or
// "header_checked", "header": "" or the 160 hex digits of the header}}.
// Calendars are in the order of the configuration; each fragment is a tree
// serialised as above without the magic, the version, the hash operation and
// the digest. The digest is SHA-256 of the checkpoint text, the payload_sha256
// of the evidence file. submitted_at is the clock of the cluster when the
// digest was first accepted by a calendar; it is what the lower bound of the
// height is computed from. "bitcoin" names ONE attestation: the one with the
// lowest height among those that passed the checks (among the header-checked
// ones when there are any). The fragments may hold others; a verifier checks
// every Bitcoin attestation of every fragment (CheckEvidence does) and says
// which one it used. Evidence time is the time of the header when status is
// "header_checked" and unknown otherwise (EvidenceTime is nil): "included in
// Bitcoin block H, as a calendar claims".
//
// The detached file of a record is Export(evidence): the fragments merged
// into one tree (identical subtrees merge, attestations deduplicate), the
// sha256 file hash operation, the digest. The reference client reads it.
//
// # What a verifier does with stored evidence
//
// CheckEvidence is the whole check, with nothing but the evidence and what
// the verifier supplies: parse every fragment from the digest (so that every
// message is computed, never read), list every Bitcoin attestation with the
// result of the checks above, and, for the heights for which the verifier has a
// header of its own (HeaderFunc), check the claim against it. Status is
// "header_checked" when a claim passed everything including a header,
// "claimed" when a claim passed the checks that need no header, and "" when none
// did. A "claimed" result must be presented as "a calendar claims block H; not
// checked against Bitcoin", never as an anchor. The evidence's own summary is
// checked against the fragments (and, for "header_checked", against the
// header it carries) and refused when it does not match.
//
// # Merkle root and the block header
//
// The message of a node that carries a Bitcoin attestation (the value the
// operations lead to from the digest) is compared with bytes 36 to 68 of the
// 80-byte header as they stand in the header (internal byte order, which is
// the reverse of the order a block explorer shows). This is what the
// reference library does: python-bitcoinlib's hashMerkleRoot is the raw
// field and verify_against_blockheader compares it with the digest. The
// header time is the uint32 at offset 68 (little-endian); the header's
// double SHA-256 (read as a little-endian number) must not exceed the target
// of its own bits field, which in turn must not be easier than 0x1d00ffff.
// That is a work check; it does not show that the header is on the best chain.
//
// # Choices where the design is silent
//
// A calendar URL is joined with "/digest" and "/timestamp/<hex>" after
// trimming trailing slashes (the reference client uses urljoin, which drops
// the last path segment of a URL without a trailing slash; the two agree for
// the public calendars, which have no path). Configuration: "calendars",
// "minCalendars", "upgradeTimeoutDays" and "allowInsecure" (http allowed only
// when true); unknown members are refused. An upgrade merges a calendar's
// answer only if it holds an acceptable Bitcoin attestation, and then removes
// the pending attestation it asked about; an answer of 404 or one with only
// pending attestations changes nothing. Every fragment has a budget of
// PerFragmentLookups requests per call (distinct commitments, in tree order),
// so every calendar is asked in every round and a calendar that names others
// in many pending attestations spends only its own share; the requests of a
// call run in parallel (MaxParallel). A fragment that would grow past
// MaxFragment loses the answer that made it grow. Submit with fewer calendars
// than minCalendars is retryable when the shortfall is calendars that were
// unreachable or slow, refused (a 401 or 403) when it rests on a calendar that
// refused, and permanent only when calendars that gave a wrong answer make the
// minimum out of reach. The first upgrade check is due 10 minutes after
// Submit; the caller's scheduler owns the later schedule and the expiry (see
// Expired). Litecoin and Ethereum attestations are kept opaque. The height
// limit is one: anchor.MaxBitcoinHeight (2^31 - 1), in the parser and in the
// evidence. Major version of the file is one byte here and a varuint in the
// reference reader; the two agree for the value 1.
//
// # What the caller must do
//
// Keep the evidence of a PartialError and pass it to SubmitWithPrior at the
// next attempt (its submitted_at is kept); call Upgrade for every "submitted"
// record when its NextCheck is due and for a "claimed" one when a header
// source is configured (the receipt's NextCheck says when); decide expiry with
// Expired; keep the credentials of nothing (there are none). It must not call
// Upgrade concurrently for one record. A permanent error of Upgrade ends the
// record: the old receipt is untouched, and the error is only returned when
// every lookup failed. Implement HeaderSource over a node you trust, or leave
// it out and present the result as a claim.
//
// # What it does not do
//
// It does not fetch Bitcoin data itself, does not verify a Bitcoin attestation
// against anything without a header from the operator's source or the
// verifier's, does not store or schedule, does not add a privacy nonce (the
// digest is derived from a public checkpoint), and does not implement the
// operations reverse, hexlify and keccak256. It does not say that a header is
// on the best chain.
//
// # Third-party test data
//
// The real .ots files used as vectors are the examples directory of the
// OpenTimestamps client (LGPL-3.0-or-later). They are third-party data and not
// part of the tree: testdata/fetch-third-party.sh fetches them from a pinned
// commit into testdata/third_party and verifies them against
// testdata/third_party.sha256. They are test data only, read at run time by the
// tests, never embedded, never linked and never part of a binary. Without them
// the tests that need them skip and every other test runs. The block headers
// (testdata/headers.txt, embedded in the test binary) and the other vectors are
// our own, and the properties that the examples test (round trip, truncation,
// magic and version, refused operations, merge, the merkle root against a
// header) are also tested with proofs that the tests write byte by byte
// (own_test.go); what only the examples test is compatibility with the real
// files of the reference client. See testdata/SOURCES.txt.
package ots

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
// work check of CheckHeaderAt (below), the merkle root as it stands in bytes 36
// to 68, and a block time not older than the submission (less BlockTimeSlack)
// and not more than FutureSlack ahead of the clock. The header is kept in the
// evidence and EvidenceTime is its time. A source that cannot be reached, a
// header that fails the work check (the floor included) and a block time ahead
// of the clock leave the claim a claim, to be checked again (Receipt.NextCheck):
// the fault may be the source's or the clock's.
//
// # A contradicted claim is kept aside (correction 90)
//
// A header that contradicts a claim (another merkle root, a block older than the
// digest) does not make the claim false on that one answer: the source may be
// wrong once (a header of an orphaned block, a node that is still synchronising,
// a node of another network, a mix-up of heights). The claim is kept aside:
// OTSEvidence.Contradicted gets an entry (calendar, height, merkle root, the
// header that contradicted it, the time, the reason), the claim stays in the
// fragment of its calendar, is neither plausible nor anchored (ClaimCheck.
// Quarantined; CheckEvidence says so too and refuses a summary that names it),
// and the calendar is asked about it again in the later rounds: every target keeps the pending
// attestation of a calendar whose answer it merged (correction 96; it used to be
// removed by a target without a source, so a claim merged before a source was
// configured could not be asked again), and its entry waits for the second
// contradiction. A target without a source makes no request for such a kept
// pending attestation (nothing can contradict the claim, and the answer is
// merged); a source configured later makes the calendars be asked again. The claim is
// dropped for good, with that pending attestation and with the fragment when
// nothing else is left of it, only when the header source contradicts it again in
// a round at least Config.ContradictionConfirmMinutes (default 60, range 1 to
// 10080) after the first contradiction; a header that checks it in a later round
// puts it back (the entry is removed) and anchors the record. The receipt
// carries ContradictedClaims (contradicted in this call), ClaimsAside (entries
// after it) and ClaimsDropped (dropped in this call), so that the caller can
// alert: a calendar that lied and a source that is wrong are both operator
// matters. What is kept aside is bounded PER CALENDAR (correction 106): each
// calendar has a share of anchor.MaxOTSContradictionsPerCalendar entries, and the
// whole list is the shares together (anchor.MaxOTSContradictions, so that the
// evidence stays within its size limit). A calendar over its own share loses its
// own surplus claims (they are dropped at once and remembered); a claim of
// another calendar that a header contradicts for the first time is never dropped
// because the list is full, so one hostile calendar cannot use another's share and
// switch off the confirmation interval for the honest claims. An answer of one
// calendar that carries more than MaxAnswerClaims Bitcoin attestations is refused
// as a whole, as an implausible answer of that calendar (Code "implausible_answer",
// permanent; the other calendars' answers are kept): an honest answer holds a few,
// and 10,000 bytes hold about 400 false ones. Every round ends (the budgets of
// lookups and header requests hold, and each loop of a round adds, removes or
// drops a claim), and a
// hostile calendar cannot keep a record alive beyond its expiry, which counts
// from the creation of the record. The record is not failed for a contradiction
// while another calendar or a claim kept aside can still deliver: when no
// plausible claim is left the receipt is "submitted" again (a claimed record
// falls back to submitted). Only when claims were dropped for good and nothing
// is left that could deliver or is kept aside does Upgrade return an error of
// class conflict (code "claim_contradicted": a calendar lied, or the source is
// wrong twice), with the receipt untouched. That error is a *ContradictedError:
// it carries the evidence as it was BEFORE the call, which still holds the
// claims, and PriorEvidence reads it. A claim dropped for good is remembered in
// OTSEvidence.Dropped (calendar, height, merkle root, time; bounded per calendar:
// anchor.MaxOTSDroppedPerCalendar entries for each, the oldest of a calendar
// forgotten when its share is full, and anchor.MaxOTSDropped in all, so that one
// calendar's answers cannot evict what is remembered of another, and one answer of
// at most MaxAnswerClaims claims cannot evict what is remembered of its own
// calendar while its share has room), and the same claim answered again by a
// calendar is put aside at once, before it enters a fragment, and counted only in
// Receipt.ClaimsRepeated, not as contradicted, aside or dropped again (correction
// 96). A SubmitWithPrior (a Retry) carries the claims kept aside and the claims
// dropped of the prior evidence into the new evidence (the claims kept aside only
// when they name an attestation of a fragment that is kept; they are judged again
// by the rules above, so a source that is right now restores them at once), so
// that the memory is not lost (correction 106).
//
// # What header_checked proves, and what the auditor still does (correction 91)
//
// "header_checked" means: an 80-byte header, kept in the evidence, whose double
// SHA-256 meets its own target; whose target is no easier than 1/16 of the
// target of the nearest real header of the table of checkpoints at or below the
// height of the claim (CheckHeaderAt, DefaultWorkSlack; for every height after
// block 912095 that is a work of about 2^75 hash evaluations at the least,
// against about 2^32 for difficulty 1, and the work of the accepted header,
// HeaderResult.Work, is reported); whose merkle root is the message that the
// claim sits on, recomputed from the digest; and whose time fits the submission
// and the clock. Options.WorkSlack changes the factor, up to MaxWorkSlack (2^20; New refuses more, since 2^47 and above would admit
// difficulty 1 again). It does NOT prove that
// the header is in the best chain: a real header of an orphaned block, or a header
// of any chain of that much work, passes, and nothing here counts confirmations.
// The auditor takes the height and the merkle root of the evidence (or of the
// .ots file), fetches the header at that height from a node of their own, and
// compares; a header_checked evidence is "a claim that matched a header the
// operator's source gave", not a verified anchor.
//
// A claimed receipt is not final: Upgrade asks the calendars whose fragments
// still hold pending attestations again, so that a calendar that answered first
// with a plausible false claim cannot keep the proofs of the honest calendars
// from being collected; with a HeaderSource it also checks the claims against
// the headers, and the header of the honest calendar's block anchors the
// record. Without a source, a claimed receipt with nothing pending is returned
// as it is and no request is made. A claimed record expires after its own
// timeout, ClaimedTimeout (4 times Timeout, the timeout of a submitted one),
// counted from the creation of the record; but a claimed record whose calendars
// all fail (an Upgrade that returns an error each time) is ended earlier by the
// limits of the retries (anchor.Limits: 200 attempts or 7 days of failure by
// default, with the alert), not by that timeout: the limit that is reached first
// ends it, and with the defaults it is the retry limit.
//
// # Evidence (stored by the caller, see package anchor)
//
// The evidence of an OTS record is anchor.OTSEvidence, encoded by
// anchor.EncodeEvidence: {"digest": hex(32 bytes), "submitted_at": time,
// "calendars": [{"uri": base URL of the calendar, "fragment_b64": base64 of the
// serialised tree rooted at digest}], "bitcoin": null or {"height": H,
// "calendar_uri": URI of the calendar whose fragment holds the attestation,
// "merkle_root": hex of the message it sits on, "status": "claimed" or
// "header_checked", "header": "" or the 160 hex digits of the header},
// "contradicted": [] or entries {"calendar_uri","height","merkle_root","header":
// the 160 hex digits of the header that contradicted the claim,"at": time,
// "reason": text} of the claims kept aside, "dropped": [] or entries
// {"calendar_uri","height","merkle_root","at": time} of the claims dropped for
// good that are remembered (at most anchor.MaxOTSDroppedPerCalendar for one
// calendar and anchor.MaxOTSDropped in all, the oldest of a calendar forgotten
// first; corrections 96 and 106)}. At most anchor.MaxOTSContradictionsPerCalendar
// entries of "contradicted" name one calendar.
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
// of its own bits field, which in turn must not be easier than 0x1d00ffff
// (CheckHeader) and, in Upgrade and CheckEvidence, no easier than the floor of
// CheckHeaderAt. That is a work check; it does not show that the header is on
// the best chain.
//
// # Choices where the design is silent
//
// A calendar URL is joined with "/digest" and "/timestamp/<hex>" after
// trimming trailing slashes (the reference client uses urljoin, which drops
// the last path segment of a URL without a trailing slash; the two agree for
// the public calendars, which have no path). Configuration: "calendars",
// "minCalendars", "upgradeTimeoutDays", "contradictionConfirmMinutes" and
// "allowInsecure" (http allowed only
// when true); unknown members are refused. An upgrade merges a calendar's
// answer only if it holds an acceptable Bitcoin attestation, and then the
// pending attestation it asked about is kept (see "A contradicted claim is kept
// aside");
// an answer of 404 or one with only pending attestations changes nothing. Every
// fragment has a budget of PerFragmentLookups requests per call (distinct commitments, in tree order),
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
// or "claimed" record when its NextCheck is due (a claimed receipt has one
// while a calendar can still deliver or a header could not be checked; one
// without waits for an explicit Upgrade, for instance after a header source
// was configured; the receipt's NextCheck is the first-check delay, and the
// scheduler may stretch it with anchor.UpgradeInterval); decide expiry with
// anchor.Record.ExpireIfDue, Timeout and ClaimedTimeout (or Expired); keep the
// credentials of nothing (there are none). Upgrade is bounded by
// Options.UpgradeTimeout (default DefaultUpgradeTimeout, five minutes: the worst
// round, 16 fragments of 4 lookups in parallel by 8 and 8 header requests of 20 s,
// is about that) besides the caller's context, which should have a deadline of its
// own of a few minutes; act on the counts of the receipt (ContradictedClaims,
// ClaimsAside, ClaimsDropped) with an alert. A record of a record that was retried
// from expired is a new submission (anchor.Record.Retry starts its waiting period
// again). It must not call Upgrade concurrently for one record. A permanent error of Upgrade ends the record:
// the old receipt is untouched. It is returned only for a stored fragment that
// cannot be read; a round in which every lookup failed is retryable even when
// every failure was permanent (a gateway that answered with HTML once, a 410):
// the record keeps waiting until it expires or the limits of the retries end
// it. A record that failed with "claim_contradicted" keeps its own Evidence:
// AfterError and Retry leave Record.Evidence as it was, and the evidence of the
// call's input is the evidence that the *ContradictedError carries (a caller that
// clears Evidence when a record fails creates the loss that the text warns of; do
// not). A *PartialError is different: Submit gave no receipt, so the record has no
// evidence, and the evidence of the calendars that did answer is only in the error:
// store PriorEvidenceRaw(err) (the canonical encoding, as the evidence column
// holds it) with the failed record. When the operator retries a record that holds
// evidence, the Retry goes through anchor.Resumer (Target.Resume, or
// SubmitWithPrior on a *Target): the scheduler holds anchor.Target and asks for
// the interface, it needs no type assertion to *ots.Target. The proof, the
// calendars already answered, the first submission time and the claims kept aside
// and dropped are kept, and the claims are judged again; a Submit starts again
// from the calendars and the old proof is gone (corrections 96 and 106). Keep the
// evidence of a claimed record until it is anchored or expires. At start, warn
// when NewestCheckpointAge(now) exceeds CheckpointWarnAge (a year: see "What it
// does not do"). Implement HeaderSource over a node you trust, or leave it out and
// present the result as a claim.
//
// # What it does not do
//
// The floor is frozen at the newest checkpoint of the table (NewestCheckpoint
// returns its height and block time: block 912095 of August 2025 in this
// release): it is the difficulty of that block divided by the slack whatever the
// difficulty is when the claim is checked, so it weakens every year as difficulty
// rises.
//
// Release duty (correction 106; stated here once): the table must be extended
// before a release (a recent header and its row in testdata/headers.txt), and a
// release must not be cut with an old table. The table of this tree was not
// extended in the round that added NewestCheckpointAge (no chain data is invented
// and nothing is fetched here), and it is older than a year at the date of that
// round; it is a task of the release. The daemon warns when
// NewestCheckpointAge(now) is above CheckpointWarnAge (a year); no test of this
// package fails because time passes.
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

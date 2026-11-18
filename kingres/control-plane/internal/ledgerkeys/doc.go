// SPDX-License-Identifier: AGPL-3.0-only

// Package ledgerkeys holds the Ed25519 server keys that sign the checkpoints of
// the ledger logs of KingresDB (design ledger-phase7.md, sections 3.2 and 3.3,
// and the corrections of section 14).
//
// # What it is
//
//   - The key file: a canonical text form, plain or sealed under a passphrase
//     (PBKDF2-HMAC-SHA256, 600,000 iterations, AES-256-GCM with the header as
//     additional authenticated data), read and written with every malformed
//     input refused, written atomically and durably with mode 0600 (Parse,
//     Encode, ReadFile, ReadNamed, WriteFile, Decode, FileName, File,
//     Passphrase, NewPassphrase, ReadPassphraseFile).
//   - Signer: the interface (public key, sign a message) with FileSigner as
//     its implementation (GenerateKey, NewFileSigner), and VerifyingSigner,
//     which checks every signature an external signer returns, so that a
//     program, an HSM or a KMS can be added later behind the same interface.
//     KeyID, Fingerprint, ValidKeyName and SignatureLineName give the
//     signed-note key id and key name rules. CheckKeyIDs, CheckSetKeyIDs and
//     KeyIDCollision find two different keys with the same key id under one
//     name; CheckSetKeyIDs applies the verifier's rule to a key set.
//   - Set, Key, State: the key state machine of design 3.3 (next, active,
//     retired, revoked; at most two active keys; overlap of a rotation;
//     what a key in each state may sign) as pure logic. It has no storage.
//     Add (for tests and for a set under no origin yet), AddChecked, Activate,
//     Retire (with a TreeBound: AtMost or Unbounded), Revoke, Signers,
//     DueForRetirement, CanSign, MaySign, RecordLimit, Validate, Summary, and
//     the constants MaxKeys, MaxActive, MaxActivationLeadDays; the errors
//     ErrInvalidTime, ErrNoActiveKey and ErrNotInForce (with NotInForceError,
//     which says when the key becomes usable).
//   - TrustFile and TrustEntries: the keys member of trust.json, in the bytes
//     that kingres-verify trust writes and reads (TrustFile also refuses a set
//     with a key id collision under the prefix or under the origins it is given).
//     ValidPrefix checks an origin prefix.
//
// # What it guarantees
//
// A secret (the seed, the private key, a passphrase) is never part of an error
// message, a log line, or the String, GoString, %v, %+v, %#v, JSON, text or
// slog output of any type of the package; the types that hold one implement
// those methods to print a marker. Secrets are held in buffers that Destroy
// and Close zero, and are compared in constant time. A key file that is not
// byte for byte in the canonical form is refused, so parse then marshal gives
// the same bytes. Every size read from a file or argument has a limit (the
// constants of the package) with a test at the limit.
//
// # Where a secret cannot be zeroed
//
// The package zeroes every buffer it makes: the plain key file is parsed from
// and written into byte slices (no string ever holds the base64 text of the
// seed, and a test scans the memory of the process for it), the passphrase and
// the key derived from it are byte slices, and the files are read through
// buffers that are cleared. It cannot zero what it does not own:
//
//   - the expanded private key and the scalars that crypto/ed25519 makes inside
//     NewKeyFromSeed and Sign, which live in the library's own variables and on
//     the stack, and the raw 32 byte seed itself, which the library copies:
//     NewKeyFromSeed copies it into the key it builds, and Sign builds a signing
//     key out of the private key and copies the seed again. The package clears
//     the private key that NewKeyFromSeed returns, the seed buffers of Parse,
//     Open, Encode and GenerateKey, and the secret it holds (Close and
//     Destroy), so the copies that remain after a key is closed are the
//     library's. How many there are depends on the Go version and on whether
//     the stack and heap memory has been reused: a scan of the memory of the
//     process for the 32 raw bytes after a key was made, used for up to four
//     signatures and closed found none to three in the runs made for this
//     package (a test scans the one path that is the package's own, parsePlain,
//     and finds none);
//   - the key schedule of AES and the state of GCM that crypto/aes and
//     crypto/cipher build from the key derived from the passphrase, and the
//     inner and outer pads and hash states that crypto/hmac and crypto/sha256
//     build from the passphrase while PBKDF2 runs;
//   - copies that the Go runtime leaves behind when it moves a stack or a
//     value, and what the operating system keeps of the process (swap, a
//     hibernation file, a core dump);
//   - a seed or passphrase that the caller holds in a string or keeps in its
//     own slice: NewFileSigner and NewPassphrase copy their argument, and the
//     caller zeroes its copy.
//
// The plain key file is clear text on disk by design (the same as ca.key), so
// its protection is the file mode and the host, not this package. The package
// does not protect against an attacker who can read the memory of the process
// or the passphrase file.
//
// # What the caller must do
//
// Persist the Set after every transition (kingres_cp.ledger_signing_key): the
// package returns the new Set and keeps nothing. Serialise transitions (one
// task at a time). Re-sign the latest checkpoint of every log with the new key
// set at a rotation (ledgerlog.ResignCheckpoint, which takes every entry of the
// trust file, TrustEntries, with the standing of each key, and refuses a note
// that rests only on a revoked key or on a retired key beyond its bound): a log
// that grows no more keeps the signatures it has, and a retired key's signature
// alone is fine for the verifier only while the trust file still lists that
// key. The full list is under "Duties of the caller" below. Publish the trust
// file entries to the operator; the verifier never takes first trust from the
// publication. Delete the file of a retired or revoked key on every holder.
//
// Check the set after loading it: Set.Validate says why a set cannot be used.
// Two of its answers are conditions to test with errors.Is. ErrNoActiveKey
// means a set that has been in service (some key has left the state next) holds
// no active key, which no sequence of transitions can produce, so the stored
// set was damaged: the daemon must not sign and must raise an alarm. A set
// whose keys are all next (before its first activation) and the empty set are
// legal and do not give ErrNoActiveKey. A set where a key is active in the
// record but its activation time has not come is legal too; Signers(now) then
// returns ErrNotInForce (a *NotInForceError that says when the key becomes
// usable; it is NOT ErrNoActiveKey, because the set is not damaged: a clock that
// stepped back by a second must not raise the alarm of a damaged set), and a
// caller that signs must treat either error as "do not sign", never as an
// empty list. Validate also refuses the damage it can see: a time that is zero
// or outside the years 1 to 9999 (ErrInvalidTime), fields that do not belong to
// the state (a Reason or CompromisedSince on a key that is not revoked, a
// MaxTreeSize or an OverlapEnds where none belongs), times in the wrong order
// (see "The order of the times of a key" below), and two active keys that do
// not have exactly one OverlapEnds, equal to the activation of the other key
// plus a whole number of days from 1 to 365. What it cannot tell apart: a set
// in which every key was flipped back to next with its times cleared looks like
// a new set that was never used; and a marker moved to the key that was
// activated last looks like the set that Activate makes after a future or
// backward activation time (see Activate), so only the whole-day rule catches
// it, and only when the move does not happen to keep it. Nor can it see a
// changed value that has no second copy: the activation time (not_before), the
// bound of a retired key (max_tree_size; set to 0 it becomes unbounded), the
// label, the public key and the creation time all change what the trust file
// says and are all valid whatever they are. The daemon therefore keeps the
// trust file it published (or its hash) with each transition and compares it
// with TrustFile(...) after every load (K3-10).
//
// Times. The package has no clock; the caller passes the time of each call. Add,
// Activate, Retire and Revoke refuse the zero time and a time outside the years
// 1 to 9999 (ErrInvalidTime), which no later call could repair: the zero time
// makes Validate refuse the whole set for ever, and a year of five digits makes
// the verifier refuse the trust file. They also ALL refuse a time later than
// RecordLimit, the latest time the set records (the creation, activation,
// retirement or revocation of any key) plus MaxActivationLeadDays (366), the
// same tolerance for every operation, so that no operation can record a far
// time that would move the limit of the next one (the limit of Activate alone
// was removed by one far time recorded by Add, Retire or Revoke; correction
// 107). An empty set has no limit.
//
// What this does not see, and the daemon holds: a clock that is wrong by less
// than the tolerance, a clock that is wrong in the same way for every call, and
// a time that is earlier than the records. Before any transition the daemon
// compares its clock with a second source (another host, the database, a
// signed time) and passes no time that it did not read itself. A key that is
// active but not yet in force does not sign and is not the "other key" of
// Retire or Revoke; DueForRetirement, Retire, Revoke and CanSign agree about
// which keys are in force, and such a key can be retired without a revocation
// (it is recorded as retired at the time it would have come into force). A
// clock that steps back is tolerated: Retire and Revoke record the latest of
// the time given and the creation, activation and retirement of the key, so a
// revocation is never refused for its time.
//
// An idle set. The limit is relative to the records and not to a clock, so a
// set whose latest record is more than 366 days old refuses the real time. This
// is the one exception that keeps it usable: it may record a time up to
// RecordLimit, again and again. The caller passes min(now, RecordLimit()) to
// the operation that records, and each operation moves the limit up to a year
// ahead of the last record, so the set is brought forward by ordinary operations
// in steps and no key place is spent on it. A rotation after up to two years of
// idleness: Add at RecordLimit (the creation is recorded up to a year early,
// which only orders the keys), then Activate at the real time. After longer:
// Activate at RecordLimit too, and Retire and Revoke likewise. The record then
// lags the real time, so the overlap runs from the recorded activation and may
// already be over (DueForRetirement says so; the daemon decides when it
// retires, the old key signs until then). A revocation is never delayed for
// this: Revoke at t = min(now, RecordLimit()) with compromised-since
// min(since, t); an earlier compromised-since is the safe direction, and the
// trust file carries no revocation time. A set restored from a backup has the
// time of the backup as its latest record, so a real time within a year of it
// is accepted as it is; a set whose first key is added has no limit, but that
// key, like every next key, cannot be activated more than a year after its
// creation. A next key that waited longer (a rotation announced more than 366
// days ahead, or a backup older than the key) is replaced in this order, which
// works: Add a fresh key at min(now, RecordLimit()); Activate the fresh key at
// the real time; Revoke the stale key (the revocation needs the fresh key in
// force, so it cannot come first: ErrNoOtherSigner). The stale key stays in
// the trust file as revoked and takes one of the 16 places. Auditors who pinned
// it (the trust file with IncludeNext) must be given the new trust file, which
// pins the fresh key, before they can read what the fresh key signs; until then
// they hold a key that never signs.
//
// The order of the times of a key (K3-05). A key has its times in the order a
// key can have: creation, then activation, then retirement or revocation. Activate
// refuses a time before the creation (a clock that stepped back: try again when
// it has passed it); Revoke refuses a compromised-since before the creation (a
// key cannot be compromised before it exists), except when the time of the
// call itself is before the creation, when it is recorded as the creation, so a
// revocation is not refused for the clock; the time recorded as the revocation
// is never before the creation, activation or retirement; and Validate refuses
// a stored key whose times are out of that order. CompromisedSince, RetiredAt,
// RevokedAt, OverlapEnds and Reason are not written to the trust file (it has
// only revoked, not_before, not_after and max_tree_size): for the verifier a
// revocation has no date, and a revoked key's signature is never accepted at
// any --now, which is why compromised-since is kept in the record and in the
// key-revoked statement the daemon logs (design 3.2.4) and not in the file.
//
// Key id collisions: the key id of a signature line is 32 bits of a hash of the
// key name (the origin of a log) and the key. Two keys of a set can have the
// same id under the origin of some log. A signed note holds one line for a
// name and key id, and the verifier refuses a note with two such lines. It also
// refuses by two rules, which the tests run against the built verifier (see
// ledgerlog): a fresh checkpoint is checked against the usable keys of the
// trust file (revoked keys, and next keys that the file does not list, are left
// out), so two usable keys with one id make it refuse every checkpoint of that
// name; a note the auditor already holds (a state file, --old) is checked
// against every key of the file, whatever its standing, so one revoked key with
// the id of another listed key makes it refuse that log for every auditor who
// keeps a note of it, with status 2, for ever. A key is never removed from the
// trust file, so revoking a key or retiring it is NO cure: no later transition
// removes the collision. The only place to stop it is where a key enters the
// set: Set.AddChecked refuses a new key whose id under any of the origins the
// caller names equals that of ANY key of the set, whatever its state, and
// CheckSetKeyIDs applies the same rule to a key set as the trust file would list
// it (with and without the next keys, as TrustOptions.IncludeNext says) and an
// origin, and says which of the two paths refuse (KeyIDCollision.Fresh and
// Held). AddChecked looks at the pairs that contain the new key and at nothing
// else, so an old colliding pair already in the set does not stop a new key that
// collides with neither, and its error names the new key; CheckSetKeyIDs reports
// EVERY colliding pair (the error is the first pair that stops fresh checkpoints,
// KeyIDCollision.Fresh, and KeyIDCollision.Also lists the others), so a pair that
// stops fresh checkpoints is never hidden behind one that only affects notes
// already held. Add does not check: it is for tests and for a set under no origin
// yet, and TrustFile refuses to write a trust file in which two listed keys
// collide under the prefix or under the origins it is given (ErrKeyIDCollision),
// so a collision that Add let in is not published without a word. The caller
// names every origin the key will sign: one per log and
// epoch, so that is every log of the cluster that exists or will. The chance is
// about 10^-4 for a set of 16 keys and 10^4 logs. ledgerlog reports a collision
// of a note as ErrKeyIDCollision with both fingerprints. A key that has not
// signed anything (state next) that collides is dropped and another generated
// (AddChecked does not add it, so there is nothing to drop). When a collision
// is found after the fact (a key added with Add, or an origin that did not
// exist at the time, such as a new epoch or a new chain), this package cannot
// cure it: the colliding pair stays in the trust file. If the colliding key
// never signed (next), revoking it removes it from the fresh path only; for the
// held path the operator's options are to publish that log under another origin
// (another epoch or prefix, whose key ids differ), to tell the auditors of the
// log to start a new state file without a held note, or to start a new trust
// generation (see "When the set is full"). The choice belongs to the operator,
// and it is why the check must run before the key is added.
//
// # Retiring, revoking and bounds
//
// Retire and Revoke need another key that is active and in force at the time
// given (its activation time is not later). They leave a consistent set: when a
// key leaves the active state and fewer than two keys remain active, no key
// keeps an OverlapEnds.
//
// Retire takes a decision about the largest tree the retired key may still
// sign: AtMost(size), written to the trust file as max_tree_size (1 to
// MaxTrustTreeSize, 2^62-1, the range the verifier reads; Retire and Validate
// refuse any other value because the verifier refuses the whole file for it),
// or Unbounded(). The zero TreeBound is refused, so a bound is never left out
// by passing a bare 0. A key signs many logs, so the size must be at least the
// size of the largest log the key ever signed, which the caller knows and this
// package does not. Unbounded leaves the key without max_tree_size and, as
// correction 14 requires, without not_after: the verifier accepts a checkpoint
// of any size signed by it for ever. If such a retired key leaks, whoever holds
// it can sign a larger tree that every verifier accepts as genuine, and the
// only remedy is Revoke, which makes the checkpoints that rest only on that key
// unreadable. Choose Unbounded only when no bound can be stated.
//
// # When the set is full
//
// A set holds at most MaxKeys, 16, keys, which is the most the trust file that
// the verifier reads allows, and retired keys count: a retired key stays in the
// trust file for good, because what it signed must stay verifiable, and taking
// it out makes every state that rests only on it unreadable. When the set is
// full Add returns ErrTooManyKeys, and nothing in this package removes a key.
// The operator then starts a new trust generation: a new origin prefix with its
// own trust file and its own set, new logs signed by new keys, while the old
// publication stays verifiable only with the old trust file, which must be kept
// and published. Re-signing the logs of the old prefix does not help, because
// the trust file would still have to list every key the old checkpoints rest
// on. Plan for it: rotate rarely, and treat 14 keys as the time to prepare the
// next generation.
//
// # Duties of the caller
//
// What the daemon that wires this package must do. The marks say what the API
// does for it: "the API refuses" where forgetting the duty gives an error,
// "nothing in the API" where it can be forgotten without a sign. The duties of
// the log package (ledgerlog) are in its documentation.
//
//   - Persist the Set after every transition (kingres_cp.ledger_signing_key).
//     Forgotten: the key states roll back after a restart (a retired key signs
//     again, a revoked key is active), and a rolled back set is valid, so
//     Validate cannot see it. Nothing in the API: transitions are pure functions
//     and the old Set stays usable.
//   - Serialise transitions: one task at a time, from the set just stored.
//     Forgotten: two tasks start from one stored set and the second write erases
//     the first transition, for instance a revocation. Nothing in the API (no
//     sequence number).
//   - Read the clock from a trusted source and compare it with a second one
//     before every transition; never pass a stored or a user-given time. The
//     API refuses the zero time, years outside 1 to 9999 and any time later than
//     RecordLimit, so no operation records a far time; it does not see a clock
//     that is wrong by less than 366 days or consistently wrong. Forgotten: a
//     key activated at a wrong time is not in force for up to a year (a single
//     key: the daemon cannot sign), or not_before in the trust file is ahead of
//     the clocks of the auditors. After a long idle, pass min(now,
//     RecordLimit()) as described under "Times".
//   - Add a key with AddChecked and every origin that exists or will exist (every
//     log of every epoch), never with Add; when a new origin appears (a new epoch
//     or prefix) check the whole set with CheckSetKeyIDs; write the trust file
//     with TrustFile and the same origins. Forgotten: two keys with one key id
//     stop every auditor's state file for that log for ever, also when one of
//     them is revoked, and nothing cures it. The API now refuses what it can see:
//     AddChecked is not closed by an old pair, and TrustFile refuses a collision
//     under the origins it is given; Add and an origin that is never named are
//     still unchecked.
//   - Validate after every load. An error is an alarm and no signing. Tell the
//     two errors of Signers apart: ErrNoActiveKey is a damaged set (alarm),
//     ErrNotInForce is a clock behind an activation (wait until the time it
//     names, check the clock). Forgotten: a damaged row is used as it is
//     (Signers, Retire and the others do not call Validate). The API returns an
//     error and never an empty list of signers.
//   - Keep the trust file you published (or its hash) with each transition and
//     compare it with TrustFile(...) after every load. Forgotten: a changed
//     bound, activation time or label is invisible, because Validate has no
//     second copy to compare with. Nothing in the API.
//   - Choose the TreeBound at Retire: at least the largest size of any log the
//     key ever signed (all logs, all epochs), or Unbounded. Too low: the verifier
//     refuses the newest checkpoints the key signed, for everybody. Unbounded: a
//     key that leaks later can sign larger trees for ever. The API refuses the
//     zero bound, and cannot know the right one.
//   - At each rotation re-sign the latest checkpoint of every log that no longer
//     grows (ledgerlog.ResignCheckpoint, with TrustEntries). Forgotten: such a
//     log keeps only the old key's signature, fine while that key is retired and
//     bounded, unreadable if it is revoked. The API now refuses a note that rests
//     only on a revoked key or a retired key beyond its bound; nothing lists the
//     logs to re-sign.
//   - During an overlap sign every checkpoint with every key Signers returns
//     (two lines). Forgotten: auditors who hold the old trust file, or only the
//     new one, cannot verify it. Nothing in the API: the signers are a variadic
//     list.
//   - Build, sign and log the key-added, key-retired and key-revoked statements
//     (design 3.2.4) with CanSign; the package builds none. Forgotten: transitions
//     have no audit trail and auditors who pinned one key never learn the new
//     one. Nothing in the API.
//   - Poll DueForRetirement and retire when you decide to. Forgotten: two keys
//     sign for ever and a retired key is never retired. Nothing is automatic.
//   - Publish the trust file entries, delete the key file of a retired or
//     revoked key on every holder, and back up the key files and the passphrase.
//     Forgotten: auditors on an old trust file; a retired key that leaks later; a
//     lost active key with no second active key means no signing.
//   - Alarm on: a Validate failure, ErrNoActiveKey, a due retirement,
//     ErrTooManyKeys and 14 or more keys, ErrKeyIDCollision, every revocation,
//     ErrUntrustedLine, ErrNoVouchingSignature, ErrCheckpointBusy for longer than
//     a few tries, ErrConflict, ErrTilesAhead outside a restart,
//     ErrHardLinksRequired. Forgotten: silent stops. The errors are typed; there
//     is no alert hook.
//   - Never, automatically: revoke, choose Unbounded, add a key with Add,
//     activate a stale next key, re-sign a note that no vouching key signed,
//     take a time from outside. Each is irreversible or hides a divergence, and
//     each is a plain call.
//
// # What it does not do
//
// It does not store anything, distribute keys to servers, build or sign the
// events of design 3.2.4 (key-added, key-retired, key-revoked), or run an
// external signer program. On Windows a key file is written without any change
// of its access control list (mode 0600 is not applied there and is not
// checked on reading); the directory that holds the keys must be protected by
// the host.
//
// # Choices where the design is silent
//
//   - The key file JSON is compact (no spaces), members in the order of the
//     design, and the file ends with a newline; the first line is the magic.
//   - The additional authenticated data of the sealed seed is the magic line
//     with its newline, the KDF name, a zero byte, the iteration count as a
//     big endian u32 and the salt.
//   - A file with fewer than 600,000 or more than 6,000,000 iterations is
//     refused.
//   - A next key may be revoked (it leaked before it was used); revoking or
//     retiring any key needs another active key in force.
//   - A retired key is written to the trust file without not_after, because
//     not_after makes the verifier refuse its old checkpoints as new ones
//     (correction 14); max_tree_size is written when the caller gives a bound.
package ledgerkeys

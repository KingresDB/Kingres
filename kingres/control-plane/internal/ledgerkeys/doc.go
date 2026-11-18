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
//     Add, AddChecked, Activate, Retire (with a TreeBound: AtMost or
//     Unbounded), Revoke, Signers, DueForRetirement, CanSign, MaySign,
//     Validate, Summary, and the constants MaxKeys, MaxActive,
//     MaxActivationLeadDays and ErrInvalidTime.
//   - TrustFile and TrustEntries: the keys member of trust.json, in the bytes
//     that kingres-verify trust writes and reads. ValidPrefix checks an origin
//     prefix.
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
// set at a rotation (ledgerlog.ResignCheckpoint, which takes every key of the
// trust file as the trusted keys): a log that grows no more keeps
// the signatures it has, and a retired key's signature alone is fine for the
// verifier only while the trust file still lists that key. Publish the trust
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
// returns ErrNoActiveKey until that time, and a caller that signs must treat
// that error as "do not sign", never as an empty list. Validate also refuses
// the damage it can see: a time that is zero or outside the years 1 to 9999
// (ErrInvalidTime), fields that do not belong to the state (a Reason or
// CompromisedSince on a key that is not revoked, a MaxTreeSize or an
// OverlapEnds where none belongs), times in the wrong order (retired before
// activated, revoked before created), and two active keys that do not have
// exactly one OverlapEnds, equal to the activation of the other key plus a whole
// number of days from 1 to 365. What it cannot tell apart: a set in which every
// key was flipped back to next with its times cleared looks like a new set that
// was never used; and a marker moved to the key that was activated last looks
// like the set that Activate makes after a future or backward activation time
// (see Activate), so only the whole-day rule catches it, and only when the move
// does not happen to keep it.
//
// Times. The package has no clock; the caller passes the time of each call. Add,
// Activate, Retire and Revoke refuse the zero time and a time outside the years
// 1 to 9999 (ErrInvalidTime), which no later call could repair: the zero time
// makes Validate refuse the whole set for ever, and a year of five digits makes
// the verifier refuse the trust file. Activate also refuses a time more than
// MaxActivationLeadDays (366) after the latest time the set records, which
// catches a clock that is wrong by years at the moment it is wrong and nothing
// else (it cannot see a time that is wrong within that tolerance, or one that is
// earlier than the records; a next key that waited longer than the tolerance
// cannot be activated either: add a fresh key and revoke the stale one). A key
// that is active but not yet in force does not sign and is not the "other key"
// of Retire or Revoke; DueForRetirement, Retire, Revoke and CanSign agree about
// which keys are in force, and such a key can be retired without a revocation
// (it is recorded as retired at the time it would have come into force). A
// clock that steps back is tolerated: Retire and Revoke record the later of
// the time given and the activation (or creation) of the key, so a revocation
// is never refused for its time.
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
// Held). The caller names every origin the key will sign: one per log and
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

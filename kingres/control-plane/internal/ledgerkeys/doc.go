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
//     signed-note key id and key name rules. CheckKeyIDs and KeyIDCollision
//     find two different keys with the same key id under one name.
//   - Set, Key, State: the key state machine of design 3.3 (next, active,
//     retired, revoked; at most two active keys; overlap of a rotation;
//     what a key in each state may sign) as pure logic. It has no storage.
//     Add, Activate, Retire (with a TreeBound: AtMost or Unbounded), Revoke,
//     Signers, DueForRetirement, CanSign, MaySign, Validate, Summary.
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
//     the stack;
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
// set at a rotation (ledgerlog.ResignCheckpoint): a log that grows no more keeps
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
// that error as "do not sign", never as an empty list.
//
// Key id collisions: the key id of a signature line is 32 bits of a hash of the
// key name (the origin of a log) and the key. Two keys of a set can have the
// same id under the origin of some log. A signed note holds one line for a
// name and key id, and the verifier refuses a note with two such lines, and
// refuses every checkpoint of that name while its trust file lists both keys as
// usable, so such a pair can never sign together or be trusted together for
// that log. Verified with the built verifier (see ledgerlog). The chance is
// about 10^-4 for a set of 16 keys and 10^4 logs. ledgerlog reports it as
// ErrKeyIDCollision with both fingerprints. CheckKeyIDs finds it before it
// matters: call it, per log origin, with the keys that may sign at once and
// the retired keys the trust file lists, before a next key is activated. A key
// that has not signed anything (state next) is then dropped and another
// generated. A collision between an active key and a retired one cannot be
// cured by this package: the verifier refuses that log until the trust file no
// longer lists both as usable, which is a decision of the operator (revoking
// the retired key makes every checkpoint that rests only on it unreadable).
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

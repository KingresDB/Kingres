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
//     input refused, written atomically and durably with mode 0600.
//   - Signer: the interface (public key, sign a message) with FileSigner as
//     its implementation, and VerifyingSigner, which checks every signature an
//     external signer returns, so that a program, an HSM or a KMS can be added
//     later behind the same interface. KeyID and ValidKeyName give the
//     signed-note key id and key name rules.
//   - Set, Key, State: the key state machine of design 3.3 (next, active,
//     retired, revoked; at most two active keys; overlap of a rotation;
//     what a key in each state may sign) as pure logic. It has no storage.
//   - TrustFile and TrustEntries: the keys member of trust.json, in the bytes
//     that kingres-verify trust writes and reads.
//
// # What it guarantees
//
// A secret (the seed, the private key, a passphrase) is never part of an error
// message, a log line, or the String, GoString, %v, %+v, %#v, JSON, text or
// slog output of any type of the package; the types that hold one implement
// those methods to print a marker. Secrets are held in buffers that Destroy
// and Close zero, and are compared in constant time. The one place a secret
// cannot be zeroed is the Go runtime's own copy inside crypto/ed25519 while it
// signs. A key file that is not byte for byte in the canonical form is refused,
// so parse then marshal gives the same bytes. Every size read from a file or
// argument has a limit (the constants of the package) with a test at the limit.
//
// # What the caller must do
//
// Persist the Set after every transition (kingres_cp.ledger_signing_key): the
// package returns the new Set and keeps nothing. Serialise transitions (one
// task at a time). Re-sign the latest checkpoint of every log with the new key
// set at a rotation: a log that grows no more keeps the signatures it has, and a
// retired key's signature alone is fine for the verifier only while the trust
// file still lists that key. Publish the trust file entries to the operator;
// the verifier never takes first trust from the publication. Delete the file
// of a retired or revoked key on every holder.
//
// # What it does not do
//
// It does not store anything, distribute keys to servers, build or sign the
// events of design 3.2.4 (key-added, key-retired, key-revoked), or run an
// external signer program. It does not protect against an attacker who can read
// the memory of the process or the passphrase file.
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
//     retiring any key needs another active key.
//   - A retired key is written to the trust file without not_after, because
//     not_after makes the verifier refuse its old checkpoints as new ones
//     (correction 14); max_tree_size is written when the caller gives one.
package ledgerkeys

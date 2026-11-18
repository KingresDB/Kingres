// SPDX-License-Identifier: AGPL-3.0-only

// Package ledgerlog builds the logs that KingresDB publishes for its ledger
// (design ledger-phase7.md, sections 4.1 to 4.4, 4.6, 4.10 and 5.2, with the
// corrections of section 14): a Merkle tree over a sequence of leaves in the
// tile layout of C2SP tlog-tiles, the checkpoint of the tree as a signed note,
// and the leaf formats of the two kinds of log (a block of a chain, and an
// aggregation round).
//
// # What it is
//
//   - The tree of RFC 9162 with the domain separation of the ledger (leaf hash
//     SHA-256(0x00 || data), node hash SHA-256(0x01 || left || right)): LeafHash,
//     NodeHash, RootOf, EmptyRoot, VerifyInclusion, VerifyConsistency,
//     RootFromInclusion.
//   - Frontier: the compact range (at most 62 hashes) from which the root is
//     computed and new leaves are appended without reading an old leaf; it is
//     serialised in one canonical form and verified against the stored size and
//     root when it is loaded (LoadFrontier, SaveFrontierFile, LoadFrontierFile).
//   - Log: appends entries, writes the tiles and the entry bundles (tile/<L>/<N>
//     with the x123/... index form, full and partial tiles, tile/entries/<N>) to a
//     Store, and keeps the right edge of the tiles in memory (Create, Open,
//     Append). Open checks the frontier against the root and the right edge of
//     the tiles, and says which is ahead after a crash.
//   - Store, CheckpointStore, MemStore, DirStore: tiles are written once (an
//     identical put is not an error, other content is ErrConflict). DirStore is
//     the layout of a publication directory that kingres-verify reads with
//     --source, written with a temporary file, a flush and a hard link, and
//     refuses links, files that are not regular and content that is not a tile.
//     Required lists the keys a reader of a size may ask for (up to
//     MaxRequiredSize leaves).
//   - Reader: hashes, entries, inclusion proofs and consistency proofs computed
//     from tiles; Audit checks every tile and bundle of a size against the root.
//     A Reader takes any TileGetter and does not trust it: every tile it is
//     handed is checked for its length (a partial tile, a full tile cut to its
//     first hashes, an entry bundle) before it is used, so a short, long, wrong
//     or failing answer is an error and never a panic.
//   - Checkpoint, Note, SignCheckpoint, ResignCheckpoint, VerifyCheckpoint,
//     SignNote, ParseNote, VerifyNote: the checkpoint text (origin, size, root,
//     optional extension lines) and signed notes with several signature lines,
//     signed through ledgerkeys.Signer under the key name that is the origin.
//   - BlockEntry (the 87 bytes of design 4.3) and AggregateEntry (5.2: canonical
//     and total, up to 512 members), and the origins of the logs (ChainOrigin,
//     AggregateOrigin).
//
// # What it guarantees
//
// Everything it writes is accepted by kingres-verify as it is (the tests run the
// verifier on publications with many sizes and a key rotation, and damage each
// kind of file). Every parser refuses what is not in the canonical form, so
// serialise(parse(b)) = b, and every input has a limit (the constants of the
// package) with a test at the boundary. Checkpoint.Text refuses everything
// ParseCheckpoint refuses (a newline or control byte inside an extension line,
// more than MaxExtensionLines lines), so ParseCheckpoint(Text(c)) is c. Appending
// leaves in any batching gives the same tiles that a reader of the final size may
// ask for, the same frontier and the same root. A failed Append changes nothing
// in memory, and repeating it is safe. A published tile is never replaced, and
// never published by a rename (see DirStore).
//
// # DirStore: what is on disk, and what is not guaranteed
//
// A tile is made visible by a hard link of a flushed temporary file, which fails
// when the name exists. When the link cannot be made for another reason (a file
// system without hard links, a permission error on the link call) PutTile
// returns ErrHardLinksRequired and publishes nothing: there is no fallback to a
// rename, which could replace a tile that another writer published in the
// meantime. The publication directory therefore needs a file system with hard
// links (ext4, XFS, btrfs and NTFS have them; FAT, exFAT and some network and
// container file systems do not).
//
// When NoSync is false and the system and the disk honour a flush, a put that
// returned nil is whole on disk with every directory it created: the file is
// flushed, then the directory that holds it, then the parent of each directory
// that the put created, deepest first, up to the root of the store. A put that
// failed or was cut by a power loss left the tile whole or absent and at worst a
// temporary file (RemoveTemporaryFiles); the checkpoint is the old note or the
// new one. Not guaranteed: the order in which two different puts reach the disk
// if the first had not returned (the order of a round, Append then frontier then
// checkpoint, rests on each call returning before the next starts); anything on
// a disk that lies about flushes; and, on Windows, the flush of a directory,
// which the system does not offer (only the file is flushed there). The hook of
// DirStore names every flush ("sync-dir <path>") so that a test sees which
// directories are flushed and in which order.
//
// On Windows a program that holds the checkpoint open without delete sharing (a
// file server, a verifier reading it) blocks the replacement. PutCheckpoint
// retries the rename for DirStore.CheckpointRetry (DefaultCheckpointRetry, 3
// seconds, when zero) and then returns an error that is ErrCheckpointBusy: the
// old checkpoint is whole, no temporary file is left, and a retry later may
// succeed. Other systems replace an open file at once. The check that a file is
// the one that was looked at (os.SameFile between the Lstat and the open) is
// weaker on Windows, where Go finds the identity of an Lstat result by its path
// at the time of the call, so a swap of one regular file for another cannot be
// seen there; a link on the way is refused on every system.
//
// # What the caller must do
//
//   - Serialise rounds: one writer per log. A round is: Append, then persist the
//     frontier (Appended.Frontier, with the size and root, in one atomic write),
//     then sign the checkpoint and store it. In that order, so that after a crash
//     the checkpoint is never ahead of the frontier and the frontier is never ahead
//     of the tiles; the tiles can be ahead of the frontier, which Open reports as
//     ErrTilesAhead, and which is repaired by opening with Options.AllowTilesAhead
//     and appending the same leaves again (other leaves are ErrConflict, which is a
//     divergence, never a repair).
//   - Hold the lock that makes it one writer. This package takes none (a lock
//     file is not part of it yet). With two writers on one directory, a second
//     Log that appends different leaves of the same width gets ErrConflict; one
//     that appends at another width succeeds silently, and the first writer then
//     gets ErrConflict at its next append of that width; the checkpoint file is
//     last writer wins. The detection is late but certain for the tiles, and does
//     not exist for the checkpoint, so the daemon must not start two.
//   - Append once per checkpoint. The partial tile of a size is written by the
//     Append that ends at that size, so a log that is appended in one batch has no
//     partial tile for the sizes in between, and a reader of an earlier size can be
//     served only once the full tile exists.
//   - Open a log with the frontier and the size and root stored with it. For a
//     crash in the very first round use the empty frontier (Frontier{}.Marshal())
//     with size 0 and EmptyRoot.
//   - Know that a log can stop for good with ErrConflict. A partial tile left by a
//     crashed Append that is never appended again with the same leaves, or a
//     database restored from a backup older than the tiles whose leaves are then
//     made again with other content (another closing time, another order), makes
//     every later Append that reaches that tile fail with ErrConflict. That is the
//     divergence rule of design 4.7: the log is not extended and not repaired by
//     this package, and the operator decides (restore the same leaves, or start a
//     new log).
//   - At a key rotation, add the new key's signature to the latest checkpoint of
//     every log with ResignCheckpoint, giving it the public keys the caller
//     trusts for the log: the note is verified under them first and refused when
//     no line of it verifies, so a new signature is never attached to text that
//     nobody verified. A log that stops growing keeps the signatures it has.
//   - Watch for key id collisions: two different keys that have the same 32 bit
//     key id under the origin of a log cannot sign the same checkpoint (a note
//     holds one line for a name and key id, and the verifier refuses two). SignNote,
//     AddSignature, ResignCheckpoint and VerifyNote report it as
//     ledgerkeys.ErrKeyIDCollision with both fingerprints, and the test
//     TestVerifierAndKeyIDCollision records what the built verifier does:
//     it refuses a note with two such lines, and refuses a note while its trust
//     file lists both keys as usable. Check new keys with ledgerkeys.CheckKeyIDs
//     before they are activated (see the package ledgerkeys for what the operator
//     does).
//   - Keep a partial tile for every size that has a checkpoint until clients can no
//     longer ask for it; DirStore has no pruning.
//   - Hold the divergence rules of design 4.7 itself: compare the leaves it reads
//     with the published ones (CheckLink, ErrConflict) and stop the log.
//
// # What it does not do
//
// It does not read the database, make leaves (BlockEntry only encodes them), keep
// the frontier or the checkpoints in the system database, serve HTTP, anchor, or
// choose when to sign. Audit and the proofs cost time linear in the size or in the
// proof, and Required lists keys in proportion to size/256 and refuses a size
// above MaxRequiredSize (2^28 leaves, some two million keys). The verifier of
// step 1 reads tiles on the path of a proof and no entry bundle, so a damaged
// bundle is found only by Audit (or by a later verifier). Audit(size) reads only
// the tiles and bundles of that size: the older partial tiles that are kept for
// older checkpoints are not looked at, and are checked only by an Audit at their
// own size.
//
// # Tests
//
// The default run (under 30 s) covers logs of up to 1,000 leaves, every
// parser, the property tests, the crash points of a 90-leaf log, the flushes
// and failures of DirStore through its hooks and seams, and, when
// KINGRES_VERIFY names a built kingres-verify, a publication of 8 logs. With
// KINGRES_LONG_TESTS=1 it also runs the 65,539 and 66,000-leaf cases, the long
// random sizes, the crash points of a 520-leaf log that spans a full tile, the
// 64 MiB batch and the publication of thirteen logs with more byte flips. The
// tests that need a symbolic link skip where one cannot be made.
//
// # Choices where the design is silent
//
//   - A checkpoint of size 0 is signed and written (the root is that of the empty
//     tree), because kingres-verify accepts it; no tile exists for it.
//   - Store has a third method, Widths, which lists the widths stored for a tile
//     position: Open uses it to find tiles beyond the frontier. The latest
//     checkpoint is kept by CheckpointStore, which replaces it.
//   - Entry level in a TileKey is -1, as the design's ledger_tile table says.
//   - An entry has 1 to 65,535 bytes; one Append takes 1 to 10,000 entries and at
//     most 64 MiB; an empty entry is refused.
//   - The partial tile of the current size is written by every Append (so one is
//     written for each size that has a checkpoint if the caller appends once per
//     checkpoint), and older partial tiles are left as they are.
//   - The aggregate member must have an epoch of at least 1 and flags within the
//     three defined bits; members are sorted by the bytes of chain_id then the
//     big endian epoch, which is the order the verifier will read.
//   - Signature lines are written in the order of the signers given; the
//     verifier does not care.
package ledgerlog

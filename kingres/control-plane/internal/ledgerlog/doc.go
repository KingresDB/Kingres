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
//     NodeHash, RootOf, VerifyInclusion, VerifyConsistency.
//   - Frontier: the compact range (at most 62 hashes) from which the root is
//     computed and new leaves are appended without reading an old leaf; it is
//     serialised in one canonical form and verified against the stored size and
//     root when it is loaded.
//   - Log: appends entries, writes the tiles and the entry bundles (tile/<L>/<N>
//     with the x123/... index form, full and partial tiles, tile/entries/<N>) to a
//     Store, and keeps the right edge of the tiles in memory. Open checks the
//     frontier against the root and the right edge of the tiles, and says which
//     is ahead after a crash.
//   - Store, MemStore, DirStore: tiles are written once (an identical put is not an
//     error, other content is ErrConflict). DirStore is the layout of a
//     publication directory that kingres-verify reads with --source, written with
//     a temporary file, a flush and a hard link, and refuses links, files that are
//     not regular and content that is not a tile.
//   - Reader: hashes, entries, inclusion proofs and consistency proofs computed
//     from tiles; Audit checks every tile and bundle of a size against the root.
//   - Checkpoint, Note, SignCheckpoint, ResignCheckpoint, VerifyCheckpoint: the
//     checkpoint text (origin, size, root, optional extension lines) and signed
//     notes with several signature lines, signed through ledgerkeys.Signer under
//     the key name that is the origin.
//   - BlockEntry (the 87 bytes of design 4.3) and AggregateEntry (5.2: canonical
//     and total, up to 512 members), and the origins of the logs.
//
// # What it guarantees
//
// Everything it writes is accepted by kingres-verify as it is (the tests run the
// verifier on publications with many sizes and a key rotation, and damage each
// kind of file). Every parser refuses what is not in the canonical form, so
// serialise(parse(b)) = b, and every input has a limit (the constants of the
// package) with a test at the boundary. Appending leaves in any batching gives the
// same tiles that a reader of the final size may ask for, the same frontier and the
// same root. A failed Append changes nothing in memory, and repeating it is safe.
// A published tile is never replaced.
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
//   - Open a log with the frontier and the size and root stored with it. For a
//     crash in the very first round use the empty frontier (Frontier{}.Marshal())
//     with size 0 and EmptyRoot.
//   - At a key rotation, add the new key's signature to the latest checkpoint of
//     every log with ResignCheckpoint: a log that stops growing keeps the
//     signatures it has.
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
// proof, and Required lists keys in proportion to size/256. The verifier of step 1
// reads tiles on the path of a proof and no entry bundle, so a damaged bundle is
// found only by Audit (or by a later verifier).
//
// # Tests
//
// The default run (under 30 s) covers logs of up to 1,000 leaves, every
// parser, the property tests, the crash points of a 90-leaf log, and, when
// KINGRES_VERIFY names a built kingres-verify, a publication of 8 logs. With
// KINGRES_LONG_TESTS=1 it also runs the 65,539 and 66,000-leaf cases, the long
// random sizes, the crash points of a 520-leaf log that spans a full tile, the
// 64 MiB batch and the publication of thirteen logs with more byte flips.
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

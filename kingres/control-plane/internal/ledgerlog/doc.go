// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

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
//     A Reader keeps every tile it has parsed for as long as it lives (there is
//     no eviction: it holds at most one tile of 8 KiB for each tile position it
//     was asked about, so a Reader that answers proofs over a log of 2^28 leaves
//     can hold a million tiles). Use a Reader for a request or a batch of
//     requests and drop it; a long-lived caller makes a new Reader (NewReader is
//     cheap) when it has served many proofs, or after the log has grown.
//     A Reader takes any TileGetter and does not trust it: every tile it is
//     handed is checked for its length (a partial tile, a full tile cut to its
//     first hashes, an entry bundle) before it is used, so a short, long, wrong
//     or failing answer is an error and never a panic.
//   - Checkpoint, Note, SignCheckpoint, ResignCheckpoint, VerifyCheckpoint,
//     SignNote, ParseNote, VerifyNote: the checkpoint text (origin, size, root,
//     optional extension lines) and signed notes with several signature lines,
//     signed through ledgerkeys.Signer under the key name that is the origin.
//     ResignCheckpoint takes the entries of the trust file with the standing of
//     each key (ledgerkeys.TrustEntries), refuses a note that rests only on a
//     revoked key or on a retired key beyond its bound (ErrNoVouchingSignature),
//     a note that carries a line it cannot account for (ErrUntrustedLine) and
//     every key id collision among the keys it is given.
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
// when the name exists. When the link cannot be made because the file system has
// no hard links PutTile returns ErrHardLinksRequired and publishes nothing:
// there is no fallback to a rename, which could replace a tile that another
// writer published in the meantime. Only the errors that mean "no hard links
// here" are classed so (on Linux and the other Unix systems EPERM, which
// link(2) gives for a file system without hard links, EOPNOTSUPP and ENOTSUP,
// which some file systems return although link(2) does not list them, EXDEV and
// ENOSYS; on Windows ERROR_INVALID_FUNCTION, ERROR_NOT_SUPPORTED,
// ERROR_CALL_NOT_IMPLEMENTED and ERROR_NOT_SAME_DEVICE). Every other failure
// (a full disk, a quota, an I/O error, too many links, a refused permission) is
// returned as it is, wrapped so that errors.Is finds the cause: a full disk
// looks like a full disk. The classification follows the documentation of
// link(2) and CreateHardLink and is tested by injecting each error; it was
// not run against a real file system without hard links. EPERM is wider than
// "no hard links": link(2) also gives it for a directory that is immutable or
// append-only (chattr +i or +a) and for a rule that refuses new links, so the
// text of ErrHardLinksRequired names both and the operator looks at the
// directory (lsattr -d); nothing is published in either case (K3-09). The
// publication directory therefore needs a file system with hard
// links (ext4, XFS, btrfs and NTFS have them; FAT, exFAT and some network and
// container file systems do not).
//
// When NoSync is false and the system and the disk honour a flush, a put that
// returned nil is whole on disk with every directory it created: the file is
// flushed, then the directory that holds it, then the parent of each directory
// that the put created, deepest first, up to the root of the store. This holds
// also when the tile was there already and equal (for instance the repeat of a
// put that was cut between the link and its flushes, which is exactly what the
// crash procedure prescribes): the put then flushes the file, the directory of
// the tile and every ancestor up to the root again, since it cannot know which
// of them reached the disk. A put that failed or was cut by a power loss left
// the tile whole or absent and at worst a temporary file (RemoveTemporaryFiles);
// the checkpoint is the old note or the new one. Not guaranteed: the order in which two different puts reach the disk
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
// succeed. The retry is for the codes that an open handle gives; Windows gives
// the same "access denied" for a directory in the place of the checkpoint and
// for a checkpoint with the read-only attribute, which waiting does not cure,
// so PutCheckpoint looks at the target after the first denial and names those
// two at once (no wait, no ErrCheckpointBusy). Other systems replace an open
// file at once. The check that a file is
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
//     every log with ResignCheckpoint, giving it as the trusted keys EVERY entry
//     of the trust file for the log (ledgerkeys.TrustEntries, with IncludeNext
//     when the file lists the next keys), whatever its state: the note is
//     verified under them first and refused when no line of it verifies, so a new
//     signature is never attached to text that nobody verified. It is also
//     refused (ErrNoVouchingSignature) when no line that verifies is of a key the
//     verifier counts for a fresh checkpoint of that size (a line of a revoked key,
//     or of a retired key whose max_tree_size is below the size, vouches for
//     nothing: the verifier refuses such a note, and the signature of a good key
//     would make it genuine; correction 107). A line of such a key beside a line
//     of a key that counts is kept. The window of a key (not_before, not_after)
//     is not applied, since the package has no clock. Before it signs anything
//     ResignCheckpoint refuses a new signer, or a trusted key, whose key id
//     equals that of another key among them, also one that has no line in the
//     note (ErrKeyIDCollision), and a note that has a line under the name of the
//     log with the key id of no trusted key (ErrUntrustedLine: it is a forgery,
//     or trusted is not the whole trust file; the line is not dropped silently,
//     because a note must not change without the caller deciding), so the
//     result is never a note that the verifier refuses for a reason the function
//     could see. A log that stops growing keeps the signatures it has.
//   - Watch for key id collisions: two different keys that have the same 32 bit
//     key id under the origin of a log cannot sign the same checkpoint (a note
//     holds one line for a name and key id, and the verifier refuses two). SignNote,
//     AddSignature, ResignCheckpoint and VerifyNote report it as
//     ledgerkeys.ErrKeyIDCollision with both fingerprints. The built verifier
//     refuses by two rules, which TestKeyIDCollisionMatrixAgainstTheVerifier
//     runs for the rows both active, retired and active, revoked and active (with
//     a note by each) and next excluded, included and revoked: a fresh
//     checkpoint is checked against the usable keys of the trust file, and a note
//     the auditor already holds against every key of the file, revoked ones
//     too, so a collision with a revoked or retired key is never cured by a later
//     transition. Check a new key before it enters the key set, against every log
//     origin, with ledgerkeys.Set.AddChecked (or ledgerkeys.CheckSetKeyIDs on the
//     set as it will be); the package ledgerkeys says what the operator does when
//     a collision is found after the fact.
//   - Sign only the checkpoint that Log.Checkpoint() returns. SignCheckpoint
//     signs whatever Checkpoint it is given (any size, any root) and Open does not
//     look at the stored checkpoint, so nothing in the package stops a caller from
//     signing a smaller tree than the one published (a rollback) or a second root
//     for a size already signed (a fork) (K3-11).
//   - Keep a partial tile for every size that has a checkpoint until clients can no
//     longer ask for it; DirStore has no pruning.
//   - Hold the divergence rules of design 4.7 itself: compare the leaves it reads
//     with the published ones (CheckLink, ErrConflict) and stop the log.
//
// # Duties of the caller
//
// What the daemon that wires this package must do, in one place. "Nothing in the
// API" means the duty can be forgotten without an error; "the API refuses" means
// forgetting it gives one. The duties of the key package (ledgerkeys) are in its
// documentation; the two that touch this package are repeated here.
//
//   - One writer per log. The package takes no lock. Forgotten: a second writer
//     at another width succeeds silently, the checkpoint is last writer wins and
//     the fork is found late. Nothing in the API.
//   - The order of a round: Append, persist the frontier (with size and root, in
//     one atomic write), sign, PutCheckpoint. Forgotten: the checkpoint is ahead
//     of the frontier after a crash. The frontier ahead of the tiles cannot
//     happen and tiles ahead are repaired, but nothing makes the three calls a
//     unit.
//   - Open with the stored frontier, size and root; for the first round the empty
//     frontier. Forgotten: ErrFrontierRoot, or a log that starts from nothing over
//     existing tiles. The API checks the frontier against the root.
//   - Append once per checkpoint. Forgotten: no partial tile for the sizes in
//     between, so a reader of an earlier size cannot be served. Nothing in the
//     API.
//   - Crash repair: Open with AllowTilesAhead and append the same leaves again;
//     other leaves give ErrConflict, which is a stop for the operator. The leaves
//     must be reproducible. Forgotten: a round rebuilt with another closing time
//     or order leaves a log that cannot be extended. Never use AllowTilesAhead on
//     a normal start.
//   - On ErrConflict stop the log and alarm; never repair or delete tiles.
//     Forgotten: a bug or an attacker makes the log diverge and the daemon goes
//     on.
//   - Sign only Log.Checkpoint(): never a smaller tree than the one published,
//     never two roots for one size. Forgotten: a rollback or a fork signed by the
//     server itself. Nothing in the API: SignCheckpoint takes any Checkpoint
//     (K3-11).
//   - During a key overlap give SignCheckpoint every signer that
//     ledgerkeys.Set.Signers returns. Forgotten: auditors that hold only one of
//     the trust files cannot verify the checkpoint. Nothing in the API.
//   - At a rotation re-sign the latest checkpoint of every log that no longer
//     grows, with ResignCheckpoint and the entries of the trust file. The API now
//     refuses a note that rests only on a revoked key or on a retired key beyond
//     its bound, a collision and a line it cannot account for; it does not know
//     which logs have stopped growing.
//   - Run Audit from time to time. The verifier reads no entry bundle.
//     Forgotten: a damaged bundle is found by nobody. Its cost is linear in the
//     size.
//   - Keep every partial tile of a size that has a checkpoint; there is no
//     pruning. Forgotten: a reader of an old checkpoint finds no tile (status 3
//     for the auditors). Nothing in the API says "keep" to a backup or cleaning
//     layer.
//   - Use a file system with hard links and honoured flushes, NoSync false in
//     production (the zero value is right); on Windows the flush of a directory
//     is missing. Forgotten: ErrHardLinksRequired at the first put (also for a
//     directory that is immutable or append-only), or lost tiles after a power
//     loss on a disk that lies.
//   - Use a Reader for a request or a batch and drop it (no eviction; one that
//     lives long holds up to a million tiles at 2^28 leaves). Forgotten: slow
//     memory growth.
//   - Call RemoveTemporaryFiles only at start, with no write running. Forgotten:
//     it deletes the temporary file of a running put.
//   - Pass a context and expect it to be honoured at the start of a store call, at
//     each tile of Audit and in the checkpoint retry; a hung call of the operating
//     system is not interrupted.
//   - Check divergence with CheckLink and the leaves it reads (design 4.7).
//     Forgotten: the publisher does not monitor itself.
//   - Alarm on: ErrNoVouchingSignature, ErrUntrustedLine, ErrKeyIDCollision,
//     ErrCheckpointBusy for longer than a few tries, ErrConflict, ErrTilesAhead
//     outside a restart, ErrHardLinksRequired. Forgotten: silent stops.
//
// # What it does not do
//
// It does not read the database, make leaves (BlockEntry only encodes them), keep
// the frontier or the checkpoints in the system database, serve HTTP, anchor, or
// choose when to sign. Audit and the proofs cost time linear in the size or in the
// proof, and Required lists keys in proportion to size/256 and refuses a size
// above MaxRequiredSize (2^28 leaves): at that size it lists 2,101,265 keys of 24
// bytes, which it counts first and allocates in one slice, so the list is about
// 50 MB and no more than that is allocated (a slice that grew by doubling would
// have allocated about 257 MB in all). The verifier of
// step 1 reads tiles on the path of a proof and no entry bundle, so a damaged
// bundle is found only by Audit (or by a later verifier). Audit(size) reads only
// the tiles and bundles of that size: the older partial tiles that are kept for
// older checkpoints are not looked at, and are checked only by an Audit at their
// own size.
//
// # The context
//
// The stores look at the context before they start: PutTile, GetTile, Widths,
// PutCheckpoint and GetCheckpoint of DirStore and MemStore return its error
// without doing anything when it is done, and the wait between the tries of
// PutCheckpoint ends with it (the old checkpoint whole, no temporary file).
// Audit looks at it at the start and at least once per tile, also when the
// getter does not; Log.Append looks at it before each tile it stores. It is not
// looked at inside one call of the operating system (a read, a write, a flush),
// inside the hashing of one batch, or by a Reader, which only passes it to its
// TileGetter: a getter that ignores it is not interrupted.
//
// # Tests
//
// The default run covers logs of up to 1,000 leaves, every parser, the
// property tests, the crash points of a 90-leaf log, the flushes and failures of
// DirStore through its hooks and seams, and, when KINGRES_VERIFY names a built
// kingres-verify, a publication of 7 chain logs (sizes 0, 1, 2, 255, 256, 257 and
// a few hundred) and one aggregate log, each accepted by the verifier's
// checkpoint and consistency commands, the random-size chain log and the
// aggregate log with every other check (size and root, a wrong root, a state
// file, damaged files), and the matrix of key id collisions. A verifier process
// costs from a tenth of a second to half a second on Windows, so the default run
// starts about a third of the processes of a run with KINGRES_LONG_TESTS=1, which
// gives every log every check and also runs the 65,539 and 66,000-leaf cases,
// the long random sizes, the crash points of a 520-leaf log that spans a full
// tile, the 64 MiB batch, the 3 second give-up of PutCheckpoint and the
// publication of twelve chain logs and the aggregate log with more byte flips.
// The tests that need a symbolic link skip where one cannot be made, and two
// that need os.SameFile to see a swap of one file for another skip on Windows.
//
// The tests that run the verifier are skipped when KINGRES_VERIFY is not set,
// and a skip is invisible in a plain go test. TestVerifierTestsRan always runs:
// without a verifier it writes a notice that they did not run to the standard
// error (shown by go test in the package directory and with -v) and passes, so
// that an ordinary developer run does not fail; with KINGRES_REQUIRE_VERIFY=1
// it fails, and so does each verifier test (the setting for a run that must
// not pass without them). In a go test of several packages a passing package
// shows no output, so there only the failure of KINGRES_REQUIRE_VERIFY=1 is
// visible.
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

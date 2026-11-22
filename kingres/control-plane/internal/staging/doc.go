// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

// Package staging is the staging store of the data transfers of KingresDB (design data-export-import.md, section "The transfer
// record and the staging store", increment CP2; ADR 0027). It holds no database row and no HTTP: it is the part that decides what may
// be on the disk, how much, how it is sealed and what may be deleted.
//
// # What it is
//
//   - Open checks the staging directory: a directory that is not empty and carries no marker written here is refused, so is the root,
//     a directory that is or contains a forbidden directory (the data directory of kingresd, a PostgreSQL data directory), a PostgreSQL
//     data directory itself, a symbolic link and a directory open to group or others (DXR-17).
//   - Reserve, Adopt, Reservation: a quota over the sum of the reservations and a reserve of free space on the file system. A
//     reservation grows as bytes are sealed; the writer that exceeds the quota or the reserve fails and its neighbours do not (DXR-16).
//   - Create, OpenAppend, Writer: a file of 64 KiB segments, each sealed with AES-256-GCM under a key of its own, which is wrapped by
//     the master key (Keys). The nonce is a random 32-bit prefix per file and the number of the segment; the additional data are the
//     transfer id, the number of the segment and a flag on the last segment, so a reordered, swapped, extended or truncated file does
//     not open. The file is written as transfer-<id>.part and renamed to transfer-<id>.kdx when it is complete (DXR-24).
//   - OpenReader, Reader: io.ReaderAt, io.Reader and io.Seeker over the plaintext, which is what a Range download and archive/zip
//     need. Nothing is held beyond one segment.
//   - LoadOrCreateKeys, Keys: the master key file, created with O_EXCL and a hard link so that two first uses at once cannot make two
//     keys, never created while files under an old key exist (ErrKeyLost), checked for mode and owner at every start.
//   - Sweep: the janitor. It deletes only names of the form transfer-<digits>.part or .kdx, only regular files, only files older than
//     the minimum age, only files without a row or whose row says remove, and nothing at all when the system database reports a lower
//     highest transfer id than the files show (ErrRestoredDatabase).
//
// # Guarantees
//
//   - A path is built from the transfer id only. Nothing a client sends reaches a path.
//   - Files are created with mode 0600 and opened without following a symbolic link, and a file that is not regular is refused.
//   - The plaintext is never written to the disk, and a data key is never written unwrapped.
//   - The package does not log and its errors carry no key, no plaintext and no file content.
//
// # Limits
//
// The master key is a file next to the data. A process that reads both reads the plaintext. File modes and owners are enforced where
// the operating system has them (not on Windows). The file system is reached through the FS interface, so the tests can make a disk
// full and a free space of their choosing.
package staging

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package staging

import (
	"errors"
	"fmt"
)

// The sentinel errors. Callers compare with errors.Is; the typed errors below carry the numbers.
var (
	ErrDirNotMarked     = errors.New("staging: the directory is not empty and carries no marker written by this program")
	ErrBadMarker        = errors.New("staging: the marker file of the directory is not the one this program writes")
	ErrDirForbidden     = errors.New("staging: the directory is, or contains, a directory that must never be staging space")
	ErrDirMode          = errors.New("staging: the directory or the key file is open to group or others, or is not owned by the user of this process")
	ErrQuotaExceeded    = errors.New("staging: the quota of the staging area would be exceeded")
	ErrInsufficientDisk = errors.New("staging: the free space of the file system would fall below the reserve")
	ErrFileTooLarge     = errors.New("staging: the file would be larger than the limit of one file")
	ErrKeyLost          = errors.New("staging: the master key is missing or is not the one that wrapped the file key")
	ErrKeyFile          = errors.New("staging: the key file is malformed, is not a regular file, or is in the staging directory")
	ErrNotRegular       = errors.New("staging: the path is not a regular file")
	ErrCorrupt          = errors.New("staging: the sealed file does not open (wrong key, damaged, reordered, extended or truncated)")
	ErrRestoredDatabase = errors.New("staging: the files show a higher transfer id than the system database knows; the janitor stops")
	ErrUnaligned        = errors.New("staging: the chunk does not end on a segment boundary and is not the last")
	ErrEmptyTail        = errors.New("staging: the last segment would be empty although earlier segments are sealed")
	ErrExists           = errors.New("staging: the file or reservation exists already")
	ErrNotFound         = errors.New("staging: no such file")
	ErrBusy             = errors.New("staging: the file is being written by another request")
	ErrClosed           = errors.New("staging: the writer is closed")
)

// Hold is one reservation as the error text of a refused request names it.
type Hold struct {
	ID    int64
	Bytes int64
}

// QuotaError is the refusal of a reservation or of a growth. Own lists the reservations of the same owner by id; the others are only
// counted, so that the existence of another role's files is not named.
type QuotaError struct {
	Quota     int64
	Reserved  int64
	Requested int64
	Own       []Hold
	Others    int
}

func (e *QuotaError) Error() string {
	return fmt.Sprintf("staging: quota exceeded: %d of %d bytes reserved, %d more requested", e.Reserved, e.Quota, e.Requested)
}

// Is makes errors.Is(err, ErrQuotaExceeded) true.
func (e *QuotaError) Is(target error) bool { return target == ErrQuotaExceeded }

// DiskError is the refusal for lack of free space: the reserve would be crossed, or the file system said no space left.
type DiskError struct {
	Free    uint64
	Needed  int64
	Reserve uint64
	Cause   error
}

func (e *DiskError) Error() string {
	if e.Cause != nil {
		return "staging: no space left on the file system"
	}
	return fmt.Sprintf("staging: %d bytes free, %d needed, reserve %d", e.Free, e.Needed, e.Reserve)
}

// Is makes errors.Is(err, ErrInsufficientDisk) true.
func (e *DiskError) Is(target error) bool { return target == ErrInsufficientDisk }

// Unwrap gives the operating system's error, when there is one.
func (e *DiskError) Unwrap() error { return e.Cause }

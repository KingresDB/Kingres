// SPDX-License-Identifier: AGPL-3.0-only

package ledgerlog

import (
	"errors"
	"syscall"
)

// platformRenameBusy reports whether a failed rename is the kind that another
// program's open handle causes: access denied, a sharing violation or a lock
// violation. Waiting can cure it (except for a directory or a read-only file
// in the place of the checkpoint, which PutCheckpoint tells apart by looking at
// the target).
func platformRenameBusy(err error) bool {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case 5, 32, 33: // ERROR_ACCESS_DENIED, ERROR_SHARING_VIOLATION, ERROR_LOCK_VIOLATION
			return true
		}
	}
	return false
}

// platformNoHardLinks reports whether a failed CreateHardLink means that the
// file system cannot make hard links, as opposed to a failure of this call. The
// codes: ERROR_INVALID_FUNCTION (1), what FAT and exFAT volumes give;
// ERROR_NOT_SUPPORTED (50), what a file system or a remote share that has no
// link operation gives; ERROR_CALL_NOT_IMPLEMENTED (120); ERROR_NOT_SAME_DEVICE
// (17), because a hard link cannot cross volumes (the documentation of
// CreateHardLink: both names must be on the same volume), which the temporary
// file in the directory of the tile never does unless a mount point splits it.
// Not among them, so returned as they are: ERROR_DISK_FULL, ERROR_HANDLE_DISK_FULL,
// ERROR_TOO_MANY_LINKS (1142, a limit), ERROR_ACCESS_DENIED, ERROR_PATH_NOT_FOUND,
// ERROR_WRITE_FAULT, and the I/O errors.
func platformNoHardLinks(err error) bool {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case 1, 17, 50, 120:
			return true
		}
	}
	return false
}

// SPDX-License-Identifier: AGPL-3.0-only

//go:build unix

package ledgerlog

import (
	"errors"
	"syscall"
)

// platformNoHardLinks reports whether a failed link call means that the file
// system cannot make hard links, as opposed to a failure of this call (a full
// disk, a quota, an I/O error, a refused permission). The errors, as link(2)
// documents or file systems return them:
//
//   - EPERM: "the file system containing oldpath and newpath does not support
//     the creation of hard links" (link(2)); it is also the answer of the
//     protected_hardlinks rule, which cannot apply to a temporary file the
//     process has just made.
//   - EOPNOTSUPP and ENOTSUP: not in the list of link(2), but returned by file
//     systems that have no link operation (some FUSE, network and container
//     file systems). They are one value on Linux and two on the BSDs.
//   - EXDEV: link(2) says the two names are not on the same mounted file
//     system. The temporary file is made in the directory of the tile, so it
//     happens only when a mount point splits that directory in a way that no
//     hard link crosses (an overlay or a union file system).
//   - ENOSYS: the call is not implemented.
//
// Not among them, so returned as they are: ENOSPC, EDQUOT, EIO, EMLINK (the
// file has the most links the file system allows: a limit, not a missing
// function), EACCES, EROFS, ENOENT, ENOTDIR, ENAMETOOLONG, ELOOP, ENOMEM.
func platformNoHardLinks(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	return errno == syscall.EPERM || errno == syscall.EOPNOTSUPP || errno == syscall.ENOTSUP ||
		errno == syscall.EXDEV || errno == syscall.ENOSYS
}

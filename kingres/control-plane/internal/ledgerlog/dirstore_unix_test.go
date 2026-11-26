// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

//go:build unix

package ledgerlog

import "syscall"

// The errors that mean "this file system has no hard links" and the ones that
// are about this one call (see platformNoHardLinks for each).
var (
	noLinkErrnos    = []error{syscall.EPERM, syscall.EOPNOTSUPP, syscall.ENOTSUP, syscall.EXDEV, syscall.ENOSYS}
	otherLinkErrnos = []error{syscall.ENOSPC, syscall.EDQUOT, syscall.EIO, syscall.EMLINK, syscall.EACCES, syscall.EROFS}
)

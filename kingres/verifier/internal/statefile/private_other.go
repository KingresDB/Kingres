// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

//go:build !unix

package statefile

import "io/fs"

// CheckPrivate does nothing here. On Windows the right to change a folder is an
// access control list, and reading one (GetNamedSecurityInfo and the security
// descriptor calls) needs a library outside the standard library, which this
// program does not use; owners and permission bits do not exist as Unix has
// them. So nothing of what the Unix check covers (every component of the path,
// links on the way, the owner and mode of the folder, the state and the lock
// file) is checked, and the README says what the folder of the state must be
// there: only the auditor and the administrators may write it and every folder
// above it.
func CheckPrivate(string) error { return nil }

// The owner is not read here. These two exist so that the lock code of the
// systems that are not Windows and not Unix builds; nothing on Windows uses them.
var ownerOf = func(string, fs.FileInfo) (int, bool) { return 0, false }

var selfUID = func() int { return 0 }

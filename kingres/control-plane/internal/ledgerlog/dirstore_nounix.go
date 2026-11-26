// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

//go:build !unix && !windows

package ledgerlog

// platformNoHardLinks reports whether a failed link call means that the file
// system cannot make hard links. On a system that is neither Unix nor Windows
// no error is known to mean that, so every link failure is returned as it is.
func platformNoHardLinks(error) bool { return false }

// SPDX-License-Identifier: AGPL-3.0-only

//go:build !windows

package ledgerlog

// platformRenameBusy reports whether a failed rename is the kind that another
// program's open handle causes. Only Windows has that kind: elsewhere a rename
// replaces a file that is open, so no failure is worth waiting for.
func platformRenameBusy(error) bool { return false }

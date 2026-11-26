// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ledgerstate

// SetMaxFile lowers the size a file may hold, for a test; it returns the function that puts it back.
func SetMaxFile(n int) func() {
	old := maxFile
	maxFile = n
	return func() { maxFile = old }
}

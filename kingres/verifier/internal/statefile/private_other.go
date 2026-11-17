// SPDX-License-Identifier: AGPL-3.0-only

//go:build !unix

package statefile

// CheckPrivate does nothing here: the permissions of a Windows folder are access
// control lists, which this program does not read. The README says what the
// folder of the state must be.
func CheckPrivate(string) error { return nil }

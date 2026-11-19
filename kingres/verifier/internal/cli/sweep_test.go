// SPDX-License-Identifier: AGPL-3.0-only

package cli

import "os"

// The byte-mutation sweeps (every byte of every file of a bundle is flipped, one at a time, and the bundle must not
// verify) cost more than ten minutes on Windows. By default they flip a deterministic sample: every byte of a file of
// at most 2,048 bytes (the checkpoints, the events, the small tiles: the files whose bytes decide the most), the first
// and last 64 bytes of every file, and every 16th byte of the rest, so every file and every kind of file is covered.
// KINGRES_VERIFY_FULL_SWEEP=1 flips every byte of every file; the Linux check of a release runs it:
//
//	KINGRES_VERIFY_FULL_SWEEP=1 go test -count=1 -timeout 60m -run 'RejectsEveryByte' ./internal/cli ./internal/crosscheck
func sweepPosition(i, size int) bool {
	if os.Getenv("KINGRES_VERIFY_FULL_SWEEP") == "1" {
		return true
	}
	return size <= 2048 || i < 64 || i >= size-64 || i%16 == 0
}

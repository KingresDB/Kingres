// SPDX-License-Identifier: AGPL-3.0-only

//go:build !unix && !windows

package safefile

import (
	"fmt"
	"os"
)

// openNoFollow on a system with neither O_NOFOLLOW nor reparse points: the
// last component is looked at before the open, which leaves a gap that the
// two systems the verifier is built for do not have.
func openNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	if st, err := os.Lstat(path); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s %w", clipPath(path), ErrLink)
	}
	return os.OpenFile(path, flag, perm) // #nosec G304 -- a file of the verifier
}

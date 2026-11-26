// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

//go:build unix

package safefile

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// openNoFollow adds O_NOFOLLOW (a link at the last component fails with ELOOP)
// to the flags that keep an open from blocking.
func openNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	f, err := os.OpenFile(path, flag|syscall.O_NOFOLLOW|openFlags, perm) // #nosec G304 -- a file of the verifier, next to the state the auditor named
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("%s %w", path, ErrLink)
		}
		return nil, err
	}
	return f, nil
}

// SPDX-License-Identifier: AGPL-3.0-only

package safefile

import (
	"fmt"
	"os"
)

// OpenNoFollow opens a file the verifier itself keeps (the lock of a state, the
// evidence of a fork) and does not follow a link at the last component: a
// symbolic link or other reparse point there is refused with ErrLink, and so is
// anything that is not a regular file once it is open (a named pipe, a
// directory, a device) with ErrNotRegular. A name that does not exist is
// created when flag has O_CREATE, with perm; with O_EXCL it must not exist.
//
// The auditor's own files are opened like this because the directory they sit
// in may be one that another local user can write: a link planted at the name
// of the lock would otherwise make the verifier create or open a file where the
// link points. The links of the path above the last component are the ones the
// auditor chose (the state path is resolved before it is used).
func OpenNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	if err := checkArgPath(path); err != nil {
		return nil, err
	}
	f, err := openNoFollow(path, flag, perm)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if !st.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%s %w (it is %s)", path, ErrNotRegular, kind(st.Mode()))
	}
	return f, nil
}

// ReadNoFollow reads a file the verifier keeps, at most limit bytes, through
// OpenNoFollow.
func ReadNoFollow(path string, limit int64) ([]byte, error) {
	f, err := OpenNoFollow(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readAll(f, path, limit)
}

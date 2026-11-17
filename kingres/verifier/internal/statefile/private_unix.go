// SPDX-License-Identifier: AGPL-3.0-only

//go:build unix

package statefile

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// CheckPrivate says whether the directory of the state, and the state itself
// when it exists, belong to the user running the verifier and cannot be written
// by group or others. The state is the auditor's protection against a rollback:
// a user who can write the directory can replace or delete the state, plant a
// link where the lock or the evidence of a fork is to be written, or hold the
// lock for ever. The error says which and what to do.
func CheckPrivate(path string) error {
	dir := filepath.Dir(path)
	if err := checkOne(dir, "the directory"); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return checkOne(path, "the file")
	}
	return nil
}

func checkOne(p, what string) error {
	st, err := os.Stat(p)
	if err != nil {
		return err
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) != os.Geteuid() {
		return fmt.Errorf("%s %s is owned by user %d, not by the user running this command (%d)", what, p, sys.Uid, os.Geteuid())
	}
	if m := st.Mode().Perm(); m&0o022 != 0 {
		return fmt.Errorf("%s %s can be written by group or others (mode %04o)", what, p, m)
	}
	return nil
}

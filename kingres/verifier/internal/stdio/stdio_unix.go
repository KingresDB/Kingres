// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package stdio

import (
	"os"
	"syscall"
)

func substituted(f *os.File) bool {
	if f == nil {
		return false
	}
	st, err := f.Stat()
	if err != nil || st.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	null, err := os.Stat(os.DevNull)
	if err != nil || !os.SameFile(st, null) {
		return false
	}
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), syscall.F_GETFL, 0) // #nosec G103 -- a descriptor
	if errno != 0 {
		return false
	}
	return flags&syscall.O_ACCMODE == syscall.O_RDWR
}

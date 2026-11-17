// SPDX-License-Identifier: AGPL-3.0-only

//go:build windows

package safefile

import (
	"fmt"
	"os"
	"syscall"
)

const (
	fileFlagOpenReparsePoint = 0x00200000
	fileAttributeReparse     = 0x400
)

// openNoFollow opens the name itself: with FILE_FLAG_OPEN_REPARSE_POINT a link
// or junction at the last component is opened as the link and not followed, and
// the attributes of the open handle say so, so there is no gap between a check
// and the open.
func openNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	var access uint32
	switch flag & (os.O_RDONLY | os.O_WRONLY | os.O_RDWR) {
	case os.O_WRONLY:
		access = syscall.GENERIC_WRITE
	case os.O_RDWR:
		access = syscall.GENERIC_READ | syscall.GENERIC_WRITE
	default:
		access = syscall.GENERIC_READ
	}
	var create uint32 = syscall.OPEN_EXISTING
	switch {
	case flag&os.O_CREATE != 0 && flag&os.O_EXCL != 0:
		create = syscall.CREATE_NEW
	case flag&os.O_CREATE != 0:
		create = syscall.OPEN_ALWAYS
	}
	h, err := syscall.CreateFile(name, access, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, nil, create,
		syscall.FILE_ATTRIBUTE_NORMAL|fileFlagOpenReparsePoint, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(h, &info); err != nil {
		_ = syscall.CloseHandle(h)
		return nil, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	if info.FileAttributes&fileAttributeReparse != 0 {
		_ = syscall.CloseHandle(h)
		return nil, fmt.Errorf("%s %w", path, ErrLink)
	}
	return os.NewFile(uintptr(h), path), nil
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package staging

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// Space is the size and the free space of a file system.
type Space struct {
	Total uint64
	Free  uint64
}

// File is the part of *os.File that the store uses.
type File interface {
	io.Reader
	io.Writer
	io.ReaderAt
	io.Seeker
	io.Closer
	Sync() error
	Truncate(size int64) error
	Stat() (fs.FileInfo, error)
}

// FS is the file system as the store sees it. OSFS is the real one; the tests wrap it to fill the disk.
type FS interface {
	OpenFile(name string, flag int, perm fs.FileMode) (File, error)
	Lstat(name string) (fs.FileInfo, error)
	ReadDir(name string) ([]fs.DirEntry, error)
	Remove(name string) error
	Rename(oldpath, newpath string) error
	// Link must fail if newpath exists; it is what makes the creation of the master key exclusive.
	Link(oldpath, newpath string) error
	Mkdir(name string, perm fs.FileMode) error
	Chmod(name string, mode fs.FileMode) error
	EvalSymlinks(path string) (string, error)
	Space(dir string) (Space, error)
}

// OSFS is the file system of the operating system.
type OSFS struct{}

// OpenFile opens without following a symbolic link where the system has the flag.
func (OSFS) OpenFile(name string, flag int, perm fs.FileMode) (File, error) {
	f, err := os.OpenFile(name, flag|noFollowFlag, perm)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Lstat does not follow a symbolic link.
func (OSFS) Lstat(name string) (fs.FileInfo, error) { return os.Lstat(name) }

// ReadDir lists a directory.
func (OSFS) ReadDir(name string) ([]fs.DirEntry, error) { return os.ReadDir(name) }

// Remove deletes one file or empty directory.
func (OSFS) Remove(name string) error { return os.Remove(name) }

// Rename renames.
func (OSFS) Rename(a, b string) error { return os.Rename(a, b) }

// Link makes a hard link and fails when the target exists.
func (OSFS) Link(a, b string) error { return os.Link(a, b) }

// Mkdir makes a directory with exactly the given permission bits (the umask is undone).
func (OSFS) Mkdir(name string, perm fs.FileMode) error {
	if err := os.Mkdir(name, perm); err != nil {
		return err
	}
	return os.Chmod(name, perm)
}

// Chmod changes the mode.
func (OSFS) Chmod(name string, mode fs.FileMode) error { return os.Chmod(name, mode) }

// EvalSymlinks resolves symbolic links.
func (OSFS) EvalSymlinks(p string) (string, error) { return filepath.EvalSymlinks(p) }

// Space reports the file system that holds dir.
func (OSFS) Space(dir string) (Space, error) { return spaceOf(dir) }

// isDiskFull tells whether err is the operating system's "no space left".
func isDiskFull(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || isDiskFullOS(err)
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

//go:build unix

package staging

import (
	"io/fs"
	"os"
	"syscall"
)

const noFollowFlag = syscall.O_NOFOLLOW

// modesEnforced says whether file modes and owners mean something on this system.
const modesEnforced = true

func spaceOf(dir string) (Space, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return Space{}, err
	}
	bs := uint64(st.Bsize)                                                         //nolint:gosec // the block size is positive
	return Space{Total: uint64(st.Blocks) * bs, Free: uint64(st.Bavail) * bs}, nil //nolint:gosec
}

func isDiskFullOS(error) bool { return false }

// ownedByUs tells whether the file belongs to the user that runs the process.
func ownedByUs(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return int(st.Uid) == os.Geteuid()
}

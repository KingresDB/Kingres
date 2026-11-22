// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package main

import (
	"context"
	"syscall"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ops"
)

// diskFreeOf tells the free space of the disk of this server's data directory (the table lifecycle checks it before it copies); another server's
// is not known to this one, so it is not checked from here.
func diskFreeOf(serverHost, dataDir string) ops.DiskFree {
	return func(_ context.Context, server string) (int64, bool) {
		if server != serverHost {
			return 0, false
		}
		var st syscall.Statfs_t
		if err := syscall.Statfs(dataDir, &st); err != nil {
			return 0, false
		}
		return int64(st.Bavail) * int64(st.Bsize), true //nolint:gosec // file system sizes fit
	}
}

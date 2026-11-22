// SPDX-License-Identifier: AGPL-3.0-only

//go:build !linux

package main

import "github.com/KingresDB/Kingres/kingres/control-plane/internal/ops"

// diskFreeOf is not available off Linux: the free space is not known and the check is skipped.
func diskFreeOf(string, string) ops.DiskFree { return nil }

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package stdio

import "os"

func substituted(*os.File) bool { return false }

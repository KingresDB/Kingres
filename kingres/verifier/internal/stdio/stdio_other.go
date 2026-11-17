// SPDX-License-Identifier: AGPL-3.0-only

//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package stdio

import "os"

func substituted(*os.File) bool { return false }

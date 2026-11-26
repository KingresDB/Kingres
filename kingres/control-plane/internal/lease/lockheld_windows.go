// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

//go:build windows

package lease

// LockHeld is not available on Windows (the watchdog does not lock there): it says "not held".
func LockHeld(string) bool { return false }

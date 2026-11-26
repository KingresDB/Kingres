// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

//go:build windows

package main

// takeLock: kingresd manages instances on Linux only; there is nothing to lock.
func takeLock(string) bool { return true }

func alive(int) bool { return true }

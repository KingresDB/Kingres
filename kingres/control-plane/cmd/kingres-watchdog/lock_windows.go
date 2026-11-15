// SPDX-License-Identifier: PostgreSQL

//go:build windows

package main

// takeLock: kingresd manages instances on Linux only; there is nothing to lock.
func takeLock(string) bool { return true }

func alive(int) bool { return true }

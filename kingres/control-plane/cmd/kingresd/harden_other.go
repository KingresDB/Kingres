// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

//go:build !linux

package main

// hardenProcess does nothing where there is no prctl (the daemon runs on Linux; other systems are for development).
func hardenProcess() {}

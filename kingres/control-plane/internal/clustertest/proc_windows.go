// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

//go:build windows

package clustertest

import "os/exec"

func setProc(*exec.Cmd) {}

func terminate(cmd *exec.Cmd, _ bool) { _ = cmd.Process.Kill() }

func pause(*exec.Cmd, bool) {}

// quit is not available on Windows.
func quit(*exec.Cmd) {}

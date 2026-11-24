// SPDX-License-Identifier: AGPL-3.0-only

//go:build windows

package clustertest

import "os/exec"

func setProc(*exec.Cmd) {}

func terminate(cmd *exec.Cmd, _ bool) { _ = cmd.Process.Kill() }

func pause(*exec.Cmd, bool) {}

// quit is not available on Windows.
func quit(*exec.Cmd) {}

// SPDX-License-Identifier: PostgreSQL

//go:build windows

package clustertest

import "os/exec"

func setProc(*exec.Cmd) {}

func terminate(cmd *exec.Cmd, _ bool) { _ = cmd.Process.Kill() }

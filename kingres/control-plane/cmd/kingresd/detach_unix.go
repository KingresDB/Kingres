// SPDX-License-Identifier: AGPL-3.0-only

//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// detach puts the child in its own session so that it survives kingresd and the
// signals sent to kingresd's process group.
func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

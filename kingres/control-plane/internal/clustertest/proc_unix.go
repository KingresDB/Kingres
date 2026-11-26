// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

//go:build !windows

package clustertest

import (
	"os/exec"
	"syscall"
)

// setProc puts the process in its own group so that a signal reaches it only.
func setProc(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

// terminate signals the process group: SIGTERM, or SIGKILL when hard.
func terminate(cmd *exec.Cmd, hard bool) {
	sig := syscall.SIGTERM
	if hard {
		sig = syscall.SIGKILL
	}
	_ = syscall.Kill(-cmd.Process.Pid, sig)
}

// pause stops (SIGSTOP) or continues (SIGCONT) the kingresd process itself, not
// its group: the instance it started keeps running, as it would if kingresd hung.
func pause(cmd *exec.Cmd, stop bool) {
	sig := syscall.SIGCONT
	if stop {
		sig = syscall.SIGSTOP
	}
	_ = syscall.Kill(cmd.Process.Pid, sig)
}

// quit makes the Go runtime of the process print every goroutine and end (SIGQUIT), for a test that wants to see where a step is stuck.
func quit(cmd *exec.Cmd) { _ = syscall.Kill(cmd.Process.Pid, syscall.SIGQUIT) }

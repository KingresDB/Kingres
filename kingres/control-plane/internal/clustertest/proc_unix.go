// SPDX-License-Identifier: PostgreSQL

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

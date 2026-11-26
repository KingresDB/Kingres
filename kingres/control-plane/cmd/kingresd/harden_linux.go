// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package main

import "syscall"

// hardenProcess makes the process refuse a core dump and refuse to be traced by another user: the signing keys of the ledger are in its memory (the seed
// of a plain key file, as bytes and as text on the install path) and a core file would be a copy of them (review P7-07). The unit file the package ships
// sets LimitCORE=0 too; this does not depend on it. It is best effort: a failure is not fatal.
func hardenProcess() {
	_ = syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{Cur: 0, Max: 0})
	const prSetDumpable = 4
	_, _, _ = syscall.RawSyscall(syscall.SYS_PRCTL, prSetDumpable, 0, 0)
}

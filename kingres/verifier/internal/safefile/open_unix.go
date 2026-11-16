// SPDX-License-Identifier: AGPL-3.0-only

//go:build unix

package safefile

import "syscall"

// openFlags: O_NONBLOCK so that opening a named pipe or a device returns at
// once (the open handle is then found not to be a regular file), O_NOCTTY so
// that a terminal device never becomes the controlling terminal.
const openFlags = syscall.O_NONBLOCK | syscall.O_NOCTTY

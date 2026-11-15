// SPDX-License-Identifier: AGPL-3.0-only

//go:build windows

package main

import "os/exec"

func detach(*exec.Cmd) {}

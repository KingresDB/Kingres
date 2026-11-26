// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

//go:build windows

package main

import "os/exec"

func detach(*exec.Cmd) {}

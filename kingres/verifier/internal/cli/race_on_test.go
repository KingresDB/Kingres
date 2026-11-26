// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

//go:build race

package cli

// raceDetector is true when the tests are built with the race detector, which multiplies the memory of a process
// and makes a bound on its resident set meaningless.
const raceDetector = true

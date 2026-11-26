// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

//go:build !unix

package safefile

// openFlags: Windows has no open flag for this. Named pipes and devices are
// reached only through names that checkArgPath refuses, and the open handle
// is checked to be a disk file anyway.
const openFlags = 0

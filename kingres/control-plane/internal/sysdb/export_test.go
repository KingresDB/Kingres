// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package sysdb

import "io/fs"

// LoadForTest exposes load to the external tests.
func LoadForTest(fsys fs.FS, dir string) ([]Migration, error) { return load(fsys, dir) }

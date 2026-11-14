// SPDX-License-Identifier: PostgreSQL

package sysdb

import "io/fs"

// LoadForTest exposes load to the external tests.
func LoadForTest(fsys fs.FS, dir string) ([]Migration, error) { return load(fsys, dir) }

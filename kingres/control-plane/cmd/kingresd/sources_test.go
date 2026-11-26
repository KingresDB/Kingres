// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// readSource returns a source file of this package for the tests that check the wiring of main.
// The file is looked for beside this test file (the path the compiler recorded), then in the
// working directory, then in the directory KINGRES_TEST_BIN, where the WSL runner stages a copy
// for a test binary that runs outside the source tree. A prebuilt test binary started from any
// other directory therefore still finds it.
func readSource(t *testing.T, name string) []byte {
	t.Helper()
	var dirs []string
	if _, self, _, ok := runtime.Caller(0); ok {
		dirs = append(dirs, filepath.Dir(self))
	}
	dirs = append(dirs, ".")
	if bin := os.Getenv("KINGRES_TEST_BIN"); bin != "" {
		dirs = append(dirs, filepath.Join(bin, "src"))
	}
	for _, d := range dirs {
		if b, err := os.ReadFile(filepath.Join(d, name)); err == nil {
			return b
		}
	}
	t.Fatalf("the source %s is not beside the test file, in the working directory or in the staged sources", name)
	return nil
}

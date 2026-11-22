// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"os"
	"testing"
	"time"
)

// tempDir is t.TempDir() for tests that start goroutines or processes that write in the directory: when the test ends the
// directory is removed again until it can be (a writer that is still finishing, or an indexer of the machine that holds a file
// for a moment, makes the first try fail with "directory not empty" on Windows); a directory that cannot be removed within a
// minute still fails the test, so a goroutine that never stops is not hidden.
func tempDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "kg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		var last error
		for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if last = os.RemoveAll(d); last == nil {
				return
			}
		}
		t.Errorf("the temporary directory cannot be removed: %v", last)
	})
	return d
}

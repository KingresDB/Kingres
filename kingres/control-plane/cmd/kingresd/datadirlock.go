// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"errors"
	"os"
	"path/filepath"
	"time"
)

// dataDirLockName is the file below the data directory whose lock says "a process of kingresd owns this data directory".
const dataDirLockName = "kingresd.lock"

// errDataDirInUse is returned when another kingresd (or a local command of it) holds the lock of the data directory.
var errDataDirInUse = errors.New("another kingresd, or a command of kingresd, is using this data directory")

// lockDataDir takes the lock of the data directory for the life of the process. It waits up to wait for a process that is just ending (a restart by the service
// manager starts the new process before the old one has gone).
func lockDataDir(dir string, wait time.Duration) (func(), error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		unlock, err := lockFile(filepath.Join(dir, dataDirLockName))
		if err == nil || !errors.Is(err, errDataDirInUse) || time.Now().After(deadline) {
			return unlock, err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

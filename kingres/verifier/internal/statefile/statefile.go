// SPDX-License-Identifier: AGPL-3.0-only

// Package statefile keeps the auditor's own record of the last checkpoint
// they verified: a lock so that two runs cannot move it backwards, an atomic
// replace that survives a crash at every step, and a place to keep the
// evidence when a fork is found.
//
// The invariant of the callers is that the file only moves from a checkpoint
// to one proved to extend it. This package supplies the mechanics that keep
// that true when two processes run at once or the machine stops half way.
package statefile

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// LockWait is how long Acquire waits for another run to finish. The holder
// reads a few tiles and writes one small file, so this is generous; a wait
// that long means a stuck process, and the caller says so.
var LockWait = 10 * time.Second

// ErrLockTimeout is returned when the lock could not be had within the wait.
var ErrLockTimeout = errors.New("the state file is locked by another run")

// Resolve returns the path the state is really stored at: the name with every
// link followed. A state named through a link is replaced where the link
// points (the link stays a link). A name that does not exist yet is resolved
// through its directory.
func Resolve(path string) (string, error) {
	p, err := filepath.EvalSymlinks(path)
	if err == nil {
		return filepath.Abs(p)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	// Not there, or a dangling link: a dangling link is an error because
	// writing would create a file the auditor did not name.
	if st, lerr := os.Lstat(path); lerr == nil && st.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%s is a link to a file that does not exist", path)
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return "", err
	}
	return filepath.Join(dir, filepath.Base(path)), nil
}

// tag is a short name that depends only on the base name of the state, so
// that the names of the helper files stay short whatever the state is called
// (a state with a 250 byte name must still work).
func tag(path string) string {
	sum := sha256.Sum256([]byte(filepath.Base(path)))
	return hex.EncodeToString(sum[:6])
}

func lockName(path string) string {
	return filepath.Join(filepath.Dir(path), ".kingres-state-"+tag(path)+".lock")
}

func tmpPrefix(path string) string { return ".kingres-state-" + tag(path) + "-" }

// step is the fault hook of the tests: it is called after each stage of the
// replace, and an error from it stops the replace there without any cleanup,
// as a crash would. Production code never sets it.
var step = func(string) error { return nil }

// errStop is what a test hook returns to simulate the end of the process.
var errStop = errors.New("stopped by the test")

// removeLeftovers deletes temporary files of this state that an earlier
// crashed run left. The caller holds the lock, so nobody else is writing.
func removeLeftovers(path string) {
	ents, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return
	}
	pre := tmpPrefix(path)
	for _, e := range ents {
		n := e.Name()
		if len(n) > len(pre) && n[:len(pre)] == pre && filepath.Ext(n) == ".tmp" {
			_ = os.Remove(filepath.Join(filepath.Dir(path), n))
		}
	}
}

// Replace stores data as the state file: a temporary file in the same
// directory, flushed, renamed over the state, and the directory flushed, so
// that after a crash at any point the state is the old file or the new one,
// whole. The caller holds the lock.
func Replace(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	removeLeftovers(path)
	f, err := os.CreateTemp(dir, tmpPrefix(path)+"*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	stopped := false
	defer func() {
		if err != nil && !stopped {
			_ = os.Remove(tmp)
		}
	}()
	fail := func(e error) error {
		if errors.Is(e, errStop) {
			stopped = true
		}
		return e
	}
	// The state is a record of what the log said, and the file it replaces
	// keeps its mode.
	mode := os.FileMode(0o644)
	if st, serr := os.Stat(path); serr == nil {
		mode = st.Mode().Perm()
	}
	_ = f.Chmod(mode)
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err = step("written"); err != nil {
		_ = f.Close()
		return fail(err)
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = step("synced"); err != nil {
		return fail(err)
	}
	if err = rename(tmp, path); err != nil {
		return err
	}
	if err = step("renamed"); err != nil {
		return fail(err)
	}
	if err = syncDir(dir); err != nil {
		return err
	}
	return step("dirsynced")
}

// ConflictPath is where the evidence of a fork or rollback is kept: next to
// the state, with the size and the start of the root in the name so that two
// different conflicting checkpoints never overwrite each other.
func ConflictPath(path string, size uint64, root []byte) string {
	base := filepath.Base(path)
	if len(base) > 100 {
		base = "state-" + tag(path)
	}
	return filepath.Join(filepath.Dir(path), fmt.Sprintf("%s.conflict-%d-%x", base, size, root[:4]))
}

// SaveConflict writes the conflicting signed note beside the state and never
// replaces an existing file of that name (it is the same evidence).
func SaveConflict(path string, size uint64, root []byte, note []byte) (string, error) {
	p := ConflictPath(path, size, root)
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) // #nosec G304 -- next to the state the auditor named
	if errors.Is(err, os.ErrExist) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	if _, err = f.Write(note); err != nil {
		_ = f.Close()
		return p, err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return p, err
	}
	return p, f.Close()
}

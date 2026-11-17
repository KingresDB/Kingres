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
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/safefile"
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

// lockName is the lock of the directory the state is in, not of the state's
// name. A lock named after the file is two locks for one file when the file has
// two names in one directory (a hard link, two spellings of one name on a file
// system that ignores case), and the run through one name then replaces the
// state while the run through the other holds a different lock. One lock per
// directory costs only that two states in one directory wait for each other.
//
// What the lock does not cover: names of one file in different directories (a
// hard link from another directory), and a writer that does not take it. A
// run replaces the state by renaming a new file over the name it was given,
// which does not change the other name of a hard link: use one name for a state.
func lockName(path string) string {
	return filepath.Join(filepath.Dir(path), ".kingres-state.lock")
}

func tmpPrefix(path string) string { return ".kingres-state-" + tag(path) + "-" }

// tempFile and fileOps are the operations Replace makes on the file system, so
// that a test can see the order in which they happen: the flush of the file
// before the rename and the flush of the directory after it are what make the
// state survive a power loss, and nothing but the order can show that they are
// there.
type tempFile interface {
	Name() string
	Chmod(os.FileMode) error
	Write([]byte) (int, error)
	Sync() error
	Close() error
}

type fileOps interface {
	CreateTemp(dir, pattern string) (tempFile, error)
	Rename(from, to string) error
	SyncDir(dir string) error
}

type osOps struct{}

func (osOps) CreateTemp(dir, pattern string) (tempFile, error) { return os.CreateTemp(dir, pattern) }
func (osOps) Rename(from, to string) error                     { return rename(from, to) }
func (osOps) SyncDir(dir string) error                         { return syncDir(dir) }

// ops is what Replace uses; production code never changes it.
var ops fileOps = osOps{}

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
	f, err := ops.CreateTemp(dir, tmpPrefix(path)+"*.tmp")
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
	// A new state is private to its owner (it is a record the auditor keeps; the
	// folder it is in must not be writable by others, see the README), and a file
	// it replaces keeps the mode it has.
	mode := os.FileMode(0o600)
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
	if err = ops.Rename(tmp, path); err != nil {
		return err
	}
	if err = step("renamed"); err != nil {
		return fail(err)
	}
	if err = ops.SyncDir(dir); err != nil {
		return err
	}
	return step("dirsynced")
}

// ConflictPath is where the evidence of a fork or rollback is kept: next to
// the state, with the size and the whole root in the name (64 hex characters) so that two
// different conflicting checkpoints never overwrite each other.
func ConflictPath(path string, size uint64, root []byte) string {
	base := filepath.Base(path)
	if len(base) > 100 {
		base = "state-" + tag(path)
	}
	return filepath.Join(filepath.Dir(path), fmt.Sprintf("%s.conflict-%d-%x", base, size, root))
}

// maxConflictNames is how many names SaveConflict tries for one checkpoint.
const maxConflictNames = 8

// ErrConflictNames is returned when every name for the evidence is taken by a
// file with other content.
var ErrConflictNames = errors.New("every name for the evidence is already used by a file with other content")

// Saved says where the evidence of a fork is and whether it was written now.
type Saved struct {
	Path string
	// Existing is true when a file with exactly this content was already
	// there: the evidence is saved, by an earlier run.
	Existing bool
}

// SaveConflict writes the conflicting signed note beside the state and claims
// it saved only when a file with exactly this content is there. The file is
// created exclusively and not through a link. A name that is taken is looked at:
// the same content is the evidence already saved; other content (another file
// that someone put there, a note signed differently for the same size and root,
// a write that was cut short) takes the next name, up to maxConflictNames
// names, and then it is an error: the caller must say that the evidence is not
// saved.
func SaveConflict(path string, size uint64, root []byte, note []byte) (Saved, error) {
	base := ConflictPath(path, size, root)
	var last error
	for i := 0; i < maxConflictNames; i++ {
		p := base
		if i > 0 {
			p = fmt.Sprintf("%s-%d", base, i+1)
		}
		f, err := safefile.OpenNoFollow(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			if err = writeAndSync(f, note); err != nil {
				_ = os.Remove(p)
				return Saved{Path: p}, err
			}
			return Saved{Path: p}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return Saved{Path: p}, err
		}
		have, rerr := safefile.ReadNoFollow(p, int64(len(note))+1)
		if rerr == nil && bytes.Equal(have, note) {
			return Saved{Path: p, Existing: true}, nil
		}
		last = fmt.Errorf("%s exists and is not this note", p)
	}
	return Saved{Path: base}, fmt.Errorf("%w (%d names tried, the last: %v)", ErrConflictNames, maxConflictNames, last)
}

func writeAndSync(f *os.File, data []byte) error {
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

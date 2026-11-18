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
	"strings"
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
	// The directory is the text before the last separator as typed: filepath.Dir
	// would clean it first, and "sl/../s" would then lose the ".." that follows a
	// link (the parent of the link's target, as the system takes it) and name
	// "./s" (L12-06).
	dirText, base := splitLast(path)
	dir, err := filepath.EvalSymlinks(dirText)
	if err != nil {
		return "", err
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return "", err
	}
	return filepath.Join(dir, base), nil
}

// splitLast splits a path at its last separator without cleaning either half:
// the directory text keeps every "." and ".." as typed, and is "." for a bare
// name. A trailing separator is dropped first, so "d/" is the name "d".
func splitLast(path string) (dir, base string) {
	for len(path) > 1 && os.IsPathSeparator(path[len(path)-1]) {
		path = path[:len(path)-1]
	}
	i := len(path) - 1
	for i >= 0 && !os.IsPathSeparator(path[i]) {
		i--
	}
	if i < 0 {
		return ".", path
	}
	if i == 0 || i == len(filepath.VolumeName(path)) {
		return path[:i+1], path[i+1:] // the root of the volume keeps its separator
	}
	return path[:i], path[i+1:]
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

// isTempName reports whether a file name is one this program gives a temporary
// file: ".kingres-state-" or ".kingres-evidence-", the 12 hex characters of
// tag, a dash, the decimal number os.CreateTemp puts in place of the star, and
// ".tmp" (L16-04). Nothing else matches.
func isTempName(n string) bool {
	rest, ok := strings.CutPrefix(n, ".kingres-state-")
	if !ok {
		rest, ok = strings.CutPrefix(n, ".kingres-evidence-")
	}
	rest, ok2 := strings.CutSuffix(rest, ".tmp")
	if !ok || !ok2 || len(rest) < 14 || rest[12] != '-' {
		return false
	}
	for i := 0; i < 12; i++ {
		if c := rest[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	for _, c := range rest[13:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// CheckName refuses a state whose name is one that this program gives to its own
// temporary files (L18-01). A run of any state in the same directory removes such
// files as the leftovers of a crash, so a state kept under that name could be
// deleted by the run of another state beside it; refusing it up front, whatever
// the directory holds, is the only answer that cannot surprise an auditor.
func CheckName(path string) error {
	if base := filepath.Base(path); isTempName(base) {
		return fmt.Errorf("the name %s is one this program gives to its own temporary files (.kingres-state-<12 hex characters>-<digits>.tmp or .kingres-evidence-<12 hex characters>-<digits>.tmp), and a run removes such files from the directory as the leftovers of a crash, so a state kept under it could be deleted: give the state another name; nothing was changed", base)
	}
	return nil
}

// RemoveLeftovers deletes the temporary files of this program in the directory of
// the state that an earlier crashed run left: those of the state and of the
// evidence files, under any name of the program (L16-04: a crash in the extension
// of an evidence file leaves a file whose name comes from the evidence file's
// name, which no run of that state would have looked for). The caller holds the
// lock, which is one per directory, so nobody of this program is writing. What is
// removed must be a regular file (never a link or a directory) with a name
// isTempName accepts, and, where the system tells, owned by the user running this
// command; everything else in the directory is left as it is.
func RemoveLeftovers(path string) {
	dir := filepath.Dir(path)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	self := filepath.Base(path)
	for _, e := range ents {
		// Never the file the run was told to use, whatever its name (L18-01); CheckName
		// refuses such a state before a run gets here, this is the second line.
		if !isTempName(e.Name()) || e.Name() == self || e.Name() == filepath.Base(lockName(path)) {
			continue
		}
		full := filepath.Join(dir, e.Name())
		st, err := os.Lstat(full)
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		if uid, known := ownerOf(full, st); known && uid != selfUID() {
			continue
		}
		_ = os.Remove(full)
	}
}

// DirSyncError says that a file is in place (renamed or linked, and not undone)
// and that the flush of its directory failed afterwards: a power loss may bring
// the old state of the directory back. It is not a failure to write (L16-05).
type DirSyncError struct{ Err error }

func (e *DirSyncError) Error() string {
	return "the file is in place, but the directory could not be flushed, so a power loss may bring the old directory entry back: " + e.Err.Error()
}

func (e *DirSyncError) Unwrap() error { return e.Err }

// Replace stores data as the state file: a temporary file in the same
// directory, flushed, renamed over the state, and the directory flushed, so
// that after a crash at any point the state is the old file or the new one,
// whole. The caller holds the lock.
func Replace(path string, data []byte) error {
	p, err := Prepare(path, data)
	if err != nil {
		return err
	}
	return p.Commit()
}

// Prepared is a new state that is written and flushed in a temporary file and
// not yet in place (L14-04). Replace is Prepare and Commit; a caller that must
// look at the state once more before it moves it does so between the two, so that
// the time between that look and the rename is only the rename.
type Prepared struct {
	path, tmp string
	stopped   bool
}

// Prepare creates the temporary file beside the state, writes data to it and
// flushes it. Nothing of the state changes. The caller holds the lock. It must
// be followed by Commit or by Abort.
func Prepare(path string, data []byte) (p *Prepared, err error) {
	dir := filepath.Dir(path)
	RemoveLeftovers(path)
	f, err := ops.CreateTemp(dir, tmpPrefix(path)+"*.tmp")
	if err != nil {
		return nil, err
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
		return nil, err
	}
	if err = step("written"); err != nil {
		_ = f.Close()
		return nil, fail(err)
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	if err = step("synced"); err != nil {
		return nil, fail(err)
	}
	return &Prepared{path: path, tmp: tmp}, nil
}

// Abort removes the temporary file of a state that will not be put in place.
func (p *Prepared) Abort() { _ = os.Remove(p.tmp) }

// Commit renames the temporary file over the state and flushes the directory.
// After an error the temporary file is removed, unless the error is a test's
// stand-in for the end of the process.
func (p *Prepared) Commit() (err error) {
	defer func() {
		if err != nil && !p.stopped {
			_ = os.Remove(p.tmp)
		}
	}()
	fail := func(e error) error {
		if errors.Is(e, errStop) {
			p.stopped = true
		}
		return e
	}
	if err = ops.Rename(p.tmp, p.path); err != nil {
		return err
	}
	if err = step("renamed"); err != nil {
		return fail(err)
	}
	if serr := ops.SyncDir(filepath.Dir(p.path)); serr != nil {
		// The rename happened: the new file is in place and the temporary name is gone.
		err = &DirSyncError{serr}
		return err
	}
	return fail(step("dirsynced"))
}

// ErrStateChanged is returned by ReplaceIfUnchanged when the file on disk is not
// the one the caller decided on.
var ErrStateChanged = errors.New("the state file changed while this run was working on it")

// ReplaceIfUnchanged is Replace for a caller that decided on the content it read
// earlier (was; existed says whether there was a file): the new state is first
// written and flushed in a temporary file (Prepare), then the state is read again
// and the run refuses, writing nothing, when it is not what was read, and only
// then is the temporary file renamed over it (Commit). The lock already excludes
// every run of this program; this is the second line, so that a writer that does
// not take the lock, or one that holds a lock on another file, can not be
// overwritten by a decision made on an older state (L12-02: a late writer moved
// the state backwards). What is left is the time between that read and the
// rename, for a writer that ignores the lock (L14-04: the README gives the
// measured figure); no call of the file system closes it without a lock.
func ReplaceIfUnchanged(path string, data, was []byte, existed bool, limit int64) error {
	p, err := Prepare(path, data)
	if err != nil {
		return err
	}
	now, err := safefile.ReadArg(path, limit)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if existed {
			err = fmt.Errorf("%w: it is gone", ErrStateChanged)
		} else {
			err = nil
		}
	case err != nil:
		err = fmt.Errorf("%w: %v", ErrStateChanged, err)
	case !existed:
		err = fmt.Errorf("%w: it did not exist and does now", ErrStateChanged)
	case !bytes.Equal(now, was):
		err = fmt.Errorf("%w: its content is not what was read", ErrStateChanged)
	}
	if err != nil {
		p.Abort()
		return err
	}
	return p.Commit()
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
	// Extended is true when the file was there for the same checkpoint and was
	// rewritten with more genuine signature lines (SaveConflictMerging).
	Extended bool
}

// MaxEvidence is the most bytes of an evidence file that are read back: a note
// is at most 64 KiB.
const MaxEvidence = 64 << 10

// SaveConflict writes the conflicting signed note beside the state and claims
// it saved only when a file with exactly this content is there. The file is
// created exclusively and not through a link. A name that is taken is looked at:
// the same content is the evidence already saved; other content (another file
// that someone put there, a note signed differently for the same size and root,
// a write that was cut short) takes the next name, up to maxConflictNames
// names, and then it is an error: the caller must say that the evidence is not
// saved.
func SaveConflict(path string, size uint64, root []byte, note []byte) (Saved, error) {
	return SaveConflictMerging(path, size, root, note, nil)
}

// SaveConflictMerging is SaveConflict for notes whose identity is not their
// bytes. A file that is already at a name is shown to merge (when it is not nil)
// together with the note: merge returns the bytes the file should hold and
// whether the file is the same checkpoint as the note (same). A file that is the
// same checkpoint with the same bytes is the evidence already saved; one that is
// the same checkpoint with other bytes (it holds fewer genuine signature lines
// than the note, or more) is replaced, in the same directory and atomically, by
// the merged bytes, and Saved.Extended says so; a file that is not the same
// checkpoint takes the next name. The signature lines of one signed text are
// therefore one file, whichever of them were served on which run (L12-03).
func SaveConflictMerging(path string, size uint64, root []byte, note []byte, merge func(have []byte) (merged []byte, same bool)) (Saved, error) {
	base := ConflictPath(path, size, root)
	RemoveLeftovers(path)
	var last error
	for i := 0; i < maxConflictNames; i++ {
		p := base
		if i > 0 {
			p = fmt.Sprintf("%s-%d", base, i+1)
		}
		err := createEvidence(path, p, note)
		if err == nil {
			return Saved{Path: p}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return Saved{Path: p}, err
		}
		if merge != nil {
			have, rerr := safefile.ReadNoFollow(p, 2*MaxEvidence)
			if rerr == nil {
				if sv, same, merr := mergeInto(p, have, merge); same {
					return sv, merr
				}
			}
			last = fmt.Errorf("%s exists and is not this checkpoint", p)
			continue
		}
		have, rerr := safefile.ReadNoFollow(p, int64(len(note))+1)
		if rerr == nil && bytes.Equal(have, note) {
			return Saved{Path: p, Existing: true}, nil
		}
		last = fmt.Errorf("%s exists and is not this note", p)
	}
	return Saved{Path: base}, fmt.Errorf("%w (%d names tried, the last: %v)", ErrConflictNames, maxConflictNames, last)
}

// evidencePrefix is the start of the temporary files of the evidence beside one
// state.
func evidencePrefix(state string) string { return ".kingres-evidence-" + tag(state) + "-" }

// linkFn is how a finished temporary file is given its final name; a test
// replaces it. Production code never sets it.
var linkFn = os.Link

// createEvidence creates the file p with data, and never a file that is empty or
// half written (L14-09): the data is written and flushed to a temporary file in
// the same directory and the file is then linked to its name, which fails when
// the name exists (os.ErrExist, a link at the name included: the link call does not
// follow one). A stop at any point leaves a temporary file, which the next run
// removes, and no file at the name. A file system that cannot link gets the
// older way, an exclusive create and a write, which can leave an empty file after a
// stop; the next run then takes another name for the evidence.
func createEvidence(state, p string, data []byte) error {
	dir := filepath.Dir(p)
	f, err := ops.CreateTemp(dir, evidencePrefix(state)+"*.tmp")
	if err != nil {
		return createEvidenceInPlace(p, data)
	}
	tmp := f.Name()
	stopped := false
	defer func() {
		if !stopped {
			_ = os.Remove(tmp)
		}
	}()
	_ = f.Chmod(0o600)
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err = step("evidence-written"); err != nil {
		_ = f.Close()
		stopped = errors.Is(err, errStop)
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = linkFn(tmp, p); err != nil {
		if errors.Is(err, os.ErrExist) {
			return err
		}
		return createEvidenceInPlace(p, data)
	}
	if err = step("evidence-linked"); err != nil {
		stopped = errors.Is(err, errStop)
		return err
	}
	if serr := ops.SyncDir(dir); serr != nil {
		return &DirSyncError{serr}
	}
	return nil
}

// createEvidenceInPlace is the exclusive create and write, for a file system
// that cannot link.
func createEvidenceInPlace(p string, data []byte) error {
	f, err := safefile.OpenNoFollow(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err = writeAndSync(f, data); err != nil {
		_ = os.Remove(p)
		return err
	}
	// The file is in place: the directory is flushed and a failure is reported as
	// the other paths do (L18-07).
	if serr := ops.SyncDir(filepath.Dir(p)); serr != nil {
		return &DirSyncError{serr}
	}
	return nil
}

// mergeInto handles a file that is already at a name of the evidence: merge says
// whether it is the same checkpoint as the note (same) and the bytes the file
// should hold. The same checkpoint with the same bytes is the evidence already
// saved; with other bytes (more genuine lines) the file is replaced, in the same
// directory and atomically. When same is false nothing was done and nothing is
// returned. An error after the file is in place (a failed flush of the directory)
// comes with the Saved that says where the file is.
func mergeInto(p string, have []byte, merge func(have []byte) ([]byte, bool)) (sv Saved, same bool, err error) {
	merged, same := merge(have)
	if !same {
		return Saved{}, false, nil
	}
	if bytes.Equal(merged, have) {
		return Saved{Path: p, Existing: true}, true, nil
	}
	if rerr := Replace(p, merged); rerr != nil {
		var dse *DirSyncError
		if errors.As(rerr, &dse) {
			return Saved{Path: p, Existing: true, Extended: true}, true, rerr
		}
		return Saved{Path: p}, true, rerr
	}
	return Saved{Path: p, Existing: true, Extended: true}, true, nil
}

// The two kinds of conflict with a held checkpoint that rests only on keys the
// trust file no longer counts (L16-01). A rollback is a served checkpoint smaller
// than the held one: a party without a key can always serve an old genuine one. A
// fork is the same size with another root, or a larger one that the proof does not
// connect to the held one.
const (
	WeakRollback = "rollback"
	WeakFork     = "fork"
)

// ErrSlotTaken is returned by SaveWeakConflict when the file for the kind is
// there and holds another checkpoint: the note is not saved.
var ErrSlotTaken = errors.New("the file kept for this kind of conflict holds another checkpoint")

// WeakConflictPath is the name of the evidence of a conflict of one kind with a
// held checkpoint that rests only on keys the trust file no longer counts: it
// depends on the held checkpoint and on the kind (two values) and on nothing that
// the log serves, so that the conflicts of any number of runs and sizes are two
// files at most.
func WeakConflictPath(path string, kind string, heldSize uint64, heldRoot []byte) string {
	base := filepath.Base(path)
	if len(base) > 100 {
		base = "state-" + tag(path)
	}
	return filepath.Join(filepath.Dir(path), fmt.Sprintf("%s.conflict-weak-%s-%d-%x", base, kind, heldSize, heldRoot))
}

// SaveWeakConflict keeps the first note of each kind that conflicted with a held
// checkpoint that rests only on keys the trust file no longer counts, and no
// later one of that kind: a log that grows would otherwise make a file at each
// run. The two kinds have a file each so that a replay of an old genuine
// checkpoint (a rollback) cannot take the place of a fork. A note of the same
// checkpoint as the one kept (same signed text; merge as in SaveConflictMerging)
// is the one already saved. A note of another checkpoint is not saved: the error
// is ErrSlotTaken, and the caller must say so and print the note (L16-01).
func SaveWeakConflict(path string, kind string, heldSize uint64, heldRoot []byte, note []byte, merge func(have []byte) ([]byte, bool)) (Saved, error) {
	if kind != WeakRollback && kind != WeakFork {
		return Saved{}, fmt.Errorf("unknown kind of conflict %q", kind)
	}
	p := WeakConflictPath(path, kind, heldSize, heldRoot)
	RemoveLeftovers(path)
	err := createEvidence(path, p, note)
	if err == nil {
		return Saved{Path: p}, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return Saved{Path: p}, err
	}
	if have, rerr := safefile.ReadNoFollow(p, 2*MaxEvidence); rerr == nil {
		if sv, same, merr := mergeInto(p, have, merge); same {
			return sv, merr
		}
	}
	return Saved{Path: p}, fmt.Errorf("%w: %s", ErrSlotTaken, p)
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

// AcquireChecked takes the lock of the state named by typed, whose resolved
// path is resolved, and then looks again: the name must still lead to the same
// path, and when enforce is set the private check passes again. The first check
// was made before the lock, possibly while another run held it for seconds; this
// one is what the run relies on.
func AcquireChecked(typed, resolved string, wait time.Duration, enforce bool) (*Lock, error) {
	lock, err := Acquire(resolved, wait)
	if err != nil {
		return nil, err
	}
	again, rerr := Resolve(typed)
	if rerr == nil && again != resolved {
		rerr = fmt.Errorf("the name now leads to %s, not to %s", again, resolved)
	}
	if rerr == nil && enforce {
		rerr = CheckPrivate(typed)
	}
	if rerr != nil {
		lock.Release()
		return nil, fmt.Errorf("the state path changed while the lock was taken, so nothing was changed: %w", rerr)
	}
	return lock, nil
}

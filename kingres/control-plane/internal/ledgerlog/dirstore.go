// SPDX-License-Identifier: AGPL-3.0-only

package ledgerlog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Modes of the files of a publication directory: public data.
const (
	dirMode  os.FileMode = 0o755
	fileMode os.FileMode = 0o644
	// tempPrefix starts the name of every temporary file of the store.
	tempPrefix = ".tmp-"
)

// DirStore is a Store and CheckpointStore over a directory in the layout of
// the publication (design 4.6): below the directory of one log,
//
//	checkpoint
//	tile/<L>/<N>[.p/<W>]
//	tile/entries/<N>[.p/<W>]
//
// so that a publication directory built from DirStores is what
// kingres-verify reads with --source. Every file is written to a temporary file
// in its own directory, flushed, and made visible by a hard link (a tile, which
// is never replaced) or a rename (the checkpoint), and the directory is
// flushed, so a crash leaves a tile whole or absent and at worst a temporary
// file (RemoveTemporaryFiles). Reads refuse a link on the way, a file that is
// not regular, a file over the size its key allows, and content that is not a
// valid tile.
//
// A tile is never published by a rename, which would replace a tile that
// another writer published in the meantime. When a hard link cannot be made
// because the file system has none (a failure of another kind is returned as it
// is, with its cause), PutTile fails with
// ErrHardLinksRequired: the publication directory must be on a file system
// with hard links (ext4, XFS, btrfs, NTFS; not FAT or exFAT, and not every
// network or container file system).
//
// What is on disk after a power loss, when NoSync is false and the operating
// system and the disk honour a flush: a put that returned nil has its file
// whole, and every directory that the put created has its entry in its parent,
// up to the root of the store (the parent of each new directory is flushed,
// deepest first, after the directory that holds the file; a put that finds the
// tile there and equal flushes the file, its directory and every ancestor up to
// the root, so that the repeat after a crash makes the tile durable). A put that returned
// an error, or that was cut by the loss, left the tile absent or whole, never
// part of one, and at worst a temporary file. A checkpoint is the old note or
// the new one. Not guaranteed: the order in which two different puts reach the
// disk (the caller's order of rounds, Append then frontier then checkpoint,
// rests on each put returning before the next starts); anything on a disk that
// lies about flushes; and, on Windows, the flush of a directory, which the
// system does not offer (the file is flushed, the directory entry rests on the
// journal of the file system).
type DirStore struct {
	root string
	// NoSync skips the flush of files and directories. It is for tests and for
	// rebuilding a publication that can be rebuilt again; with it a crash of
	// the machine can lose files that were reported as written.
	NoSync bool
	// Hook, when set, is called at the steps of a write and may return an error
	// that stops the write there, as a crash would (a hook for tests). The
	// steps are "temp-written", "before-publish", "after-publish", and then,
	// when NoSync is false, "sync-dir <path>" before each flush of a
	// directory, <path> being the directory relative to the root with forward
	// slashes ("." for the root, ".." for the parent of a root that had to be
	// created). The directory that holds the file is flushed first, then the
	// parent of each directory the write created, deepest first, up to the
	// root.
	Hook func(step string) error
	// CheckpointRetry is how long PutCheckpoint keeps retrying a rename that
	// another program's open handle blocks. It matters on Windows only (a file
	// that is open without delete sharing cannot be replaced there); other
	// systems replace it at once. Zero is DefaultCheckpointRetry, a negative
	// value is a single attempt. When the time is up PutCheckpoint returns
	// ErrCheckpointBusy.
	CheckpointRetry time.Duration

	// Seams for tests: the link and rename calls, the test that tells a
	// blocked rename, and a call between the Lstat and the open of a read.
	link       func(oldname, newname string) error
	rename     func(oldname, newname string) error
	renameBusy func(err error) bool
	afterStat  func(path string)
	now        func() time.Time                                 // the clock of the retry, for tests
	pause      func(ctx context.Context, d time.Duration) error // the wait of the retry, for tests
}

// DefaultCheckpointRetry is the time PutCheckpoint retries a blocked rename
// when CheckpointRetry is zero.
const DefaultCheckpointRetry = 3 * time.Second

// Errors of DirStore, tested with errors.Is.
var (
	// ErrHardLinksRequired is returned by PutTile when a hard link cannot be
	// made because the file system has no hard links (the errors that mean
	// that, and only those: see platformNoHardLinks; any other failure is
	// returned as it is, with its cause): the publication directory needs a
	// file system with hard links. Nothing was published. The error that
	// means "no hard links" on Unix (EPERM) is also what link(2) gives for a
	// directory that is immutable or append-only, so the text names both
	// (K3-09); nothing is published in either case.
	ErrHardLinksRequired = errors.New("ledgerlog: the publication directory needs a file system with hard links (the same error also means that the directory is immutable or append-only, or that the system refuses new links there)")
	// ErrCheckpointBusy is returned by PutCheckpoint when the checkpoint file is
	// held open by another program (Windows) for the whole of CheckpointRetry.
	// The old checkpoint is whole, no temporary file is left, and trying again
	// later may succeed.
	ErrCheckpointBusy = errors.New("ledgerlog: the checkpoint file is held open by another program; a retry may succeed")
)

// NewDirStore returns a store over dir, creating it (mode 0755) when needed;
// the parents of the directories it creates are flushed.
func NewDirStore(dir string) (*DirStore, error) {
	if dir == "" {
		return nil, errors.New("ledgerlog: the directory is empty")
	}
	created := missingDirs(dir, "")
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, fmt.Errorf("ledgerlog: %w", err)
	}
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("ledgerlog: %s is not a directory", dir)
	}
	for i := len(created) - 1; i >= 0; i-- {
		if err := syncDir(filepath.Dir(created[i])); err != nil {
			return nil, err
		}
	}
	return &DirStore{root: filepath.Clean(dir)}, nil
}

// missingDirs returns the directories on the way to dir that do not exist yet,
// shallowest first, looking up no further than stop.
func missingDirs(dir, stop string) []string {
	var miss []string
	for cur := dir; ; cur = filepath.Dir(cur) {
		if _, err := os.Lstat(cur); err == nil {
			break
		}
		miss = append([]string{cur}, miss...)
		if cur == stop || filepath.Dir(cur) == cur {
			break
		}
	}
	return miss
}

// Root returns the directory.
func (d *DirStore) Root() string { return d.root }

func (d *DirStore) hook(step string) error {
	if d.Hook != nil {
		return d.Hook(step)
	}
	return nil
}

func (d *DirStore) path(slash string) string {
	return filepath.Join(d.root, filepath.FromSlash(slash))
}

// noLinks refuses a link, or a file that is not a directory, on the way from the
// root to path (the last element may be absent).
func (d *DirStore) noLinks(path string) error {
	rel, err := filepath.Rel(d.root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("ledgerlog: %s is outside the store", path)
	}
	cur := d.root
	parts := strings.Split(rel, string(filepath.Separator))
	for i, p := range parts {
		if p == "." || p == "" {
			continue
		}
		cur = filepath.Join(cur, p)
		fi, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("ledgerlog: %w", err)
		}
		if fi.Mode()&os.ModeSymlink != 0 || (runtime.GOOS == "windows" && fi.Mode()&os.ModeIrregular != 0) {
			return fmt.Errorf("ledgerlog: %s is a link, which a publication directory must not hold", cur)
		}
		if i < len(parts)-1 && !fi.IsDir() {
			return fmt.Errorf("ledgerlog: %s is not a directory", cur)
		}
	}
	return nil
}

// readRegular reads a regular file of at most limit bytes below the root.
func (d *DirStore) readRegular(path string, limit int64) ([]byte, error) {
	if err := d.noLinks(path); err != nil {
		return nil, err
	}
	fi, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, filepath.Base(path))
		}
		return nil, fmt.Errorf("ledgerlog: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("ledgerlog: %s is not a regular file", path)
	}
	if fi.Size() > limit {
		return nil, fmt.Errorf("ledgerlog: %s has %d bytes, expected at most %d", path, fi.Size(), limit)
	}
	if d.afterStat != nil {
		d.afterStat(path)
	}
	f, err := os.Open(path) // #nosec G304 -- built from numbers below the root
	if err != nil {
		return nil, fmt.Errorf("ledgerlog: %w", err)
	}
	defer f.Close()
	// The file opened must be the one that was checked: a name that was
	// swapped for a link or another file between the Lstat and the open is
	// refused.
	if of, err := f.Stat(); err != nil || !of.Mode().IsRegular() || !os.SameFile(fi, of) {
		return nil, fmt.Errorf("ledgerlog: %s changed while it was opened", path)
	}
	return readAtMost(f, path, limit)
}

// readHook, when set, is called by readAtMost with the number of bytes it read
// (a hook for tests that see how much a file that grows was read).
var readHook func(n int64)

// readAtMost reads r to its end but never more than limit+1 bytes, and refuses
// what is longer than limit: a file that grows between the Lstat that checked
// its size and the read is read only up to the limit, whatever it grows to.
func readAtMost(r io.Reader, name string, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if readHook != nil {
		readHook(int64(len(data)))
	}
	if err != nil {
		return nil, fmt.Errorf("ledgerlog: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("ledgerlog: %s has more than %d bytes", name, limit)
	}
	return data, nil
}

func tileLimit(k TileKey) int64 {
	if k.Level == EntryLevel {
		return MaxBundleSize
	}
	return int64(k.Width * HashSize)
}

// GetTile implements Store.
func (d *DirStore) GetTile(ctx context.Context, k TileKey) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !k.Valid() {
		return nil, fmt.Errorf("%w: the key %+v is not valid", ErrTileMalformed, k)
	}
	data, err := d.readRegular(d.path(k.Path()), tileLimit(k))
	if err != nil {
		return nil, err
	}
	if err := checkTileData(k, data); err != nil {
		return nil, fmt.Errorf("%s: %w", k.Path(), err)
	}
	return data, nil
}

// PutTile implements Store. A hard link publishes the file, which fails when
// the name exists, so a tile is never replaced. When the link cannot be made
// because the file system has no hard links PutTile returns
// ErrHardLinksRequired and publishes nothing: there is no fallback to a
// rename, which would replace a tile that another writer published in the
// meantime. Any other link failure (a full disk, a quota, a refused
// permission) is returned as it is, wrapping its cause. A tile that is there
// and equal is not written again but is flushed, with its directories (see
// DirStore). The context is checked before the work.
func (d *DirStore) PutTile(ctx context.Context, k TileKey, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkTileData(k, data); err != nil {
		return err
	}
	target := d.path(k.Path())
	same := func() (bool, error) {
		old, err := d.readRegular(target, tileLimit(k))
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !bytes.Equal(old, data) {
			return false, fmt.Errorf("%w: %s", ErrConflict, k.Path())
		}
		return true, nil
	}
	if done, err := same(); err != nil {
		return err
	} else if done {
		// The tile is there and equal, and a put that returns nil is durable:
		// this may be the repeat after a crash between the link and the
		// flushes, whose page cache the first put never flushed.
		return d.flushExisting(target, nil)
	}
	created, err := d.mkdirs(filepath.Dir(target))
	if err != nil {
		return err
	}
	tmp, err := d.writeTemp(filepath.Dir(target), data)
	if err != nil {
		return err
	}
	crashed := false // a hook error stands for a crash, which leaves the temporary file
	defer func() {
		if !crashed {
			_ = os.Remove(tmp)
		}
	}()
	if err := d.hook("before-publish"); err != nil {
		crashed = true
		return err
	}
	link := os.Link
	if d.link != nil {
		link = d.link
	}
	if err := link(tmp, target); err != nil {
		if errors.Is(err, fs.ErrExist) {
			done, serr := same()
			if serr != nil {
				return serr
			}
			if !done {
				return fmt.Errorf("ledgerlog: %s disappeared while it was being published", k.Path())
			}
			return d.flushExisting(target, created)
		}
		if platformNoHardLinks(err) {
			return fmt.Errorf("%w: %s was not published: %w", ErrHardLinksRequired, k.Path(), err)
		}
		// Anything else (a full disk, a quota, an I/O error, a refused
		// permission) is not a property of the file system: say what it is.
		return fmt.Errorf("ledgerlog: %s was not published: %w", k.Path(), err)
	}
	if err := d.hook("after-publish"); err != nil {
		crashed = true
		return err
	}
	return d.flush(filepath.Dir(target), created)
}

// mkdirs creates dir (and the directories above it that are missing) and
// returns the ones it had to create, shallowest first.
func (d *DirStore) mkdirs(dir string) ([]string, error) {
	if err := d.noLinks(dir); err != nil {
		return nil, err
	}
	created := missingDirs(dir, d.root)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, fmt.Errorf("ledgerlog: %w", err)
	}
	return created, d.noLinks(dir)
}

// flush makes a put durable (unless NoSync): the directory that holds the
// file, then the parent of each directory the put created, deepest first, up
// to the root, so that no new directory depends on an entry that was not
// flushed. The hook sees each step before it runs.
func (d *DirStore) flush(dir string, created []string) error {
	if d.NoSync {
		return nil
	}
	step := func(p string) error {
		rel, err := filepath.Rel(d.root, p)
		if err != nil {
			return fmt.Errorf("ledgerlog: %w", err)
		}
		if err := d.hook("sync-dir " + filepath.ToSlash(rel)); err != nil {
			return err
		}
		return syncDir(p)
	}
	if err := step(dir); err != nil {
		return err
	}
	for i := len(created) - 1; i >= 0; i-- {
		if err := step(filepath.Dir(created[i])); err != nil {
			return err
		}
	}
	return nil
}

// flushExisting makes durable a tile that was found there and equal (it was
// published by an earlier put, possibly one that crashed before its flushes):
// the file, then the directory that holds it and every ancestor up to the root
// (the depth is at most five), plus the parent of the root when this put had to
// create it. created are the directories this put made (nil when it made
// none).
func (d *DirStore) flushExisting(target string, created []string) error {
	if d.NoSync {
		return nil
	}
	if err := syncFile(target); err != nil {
		return err
	}
	for p := filepath.Dir(target); ; p = filepath.Dir(p) {
		if err := d.syncStep(p); err != nil {
			return err
		}
		if p == d.root || filepath.Dir(p) == p {
			break
		}
	}
	if len(created) > 0 && created[0] == d.root {
		return d.syncStep(filepath.Dir(d.root))
	}
	return nil
}

// syncStep flushes the directory p after the hook has seen it.
func (d *DirStore) syncStep(p string) error {
	rel, err := filepath.Rel(d.root, p)
	if err != nil {
		return fmt.Errorf("ledgerlog: %w", err)
	}
	if err := d.hook("sync-dir " + filepath.ToSlash(rel)); err != nil {
		return err
	}
	return syncDir(p)
}

// syncFile flushes a file that already exists (on Windows only a handle that
// is open for writing can be flushed, and the file was flushed when it was
// written, so nothing is done there).
func syncFile(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	f, err := os.Open(path) // #nosec G304 -- a tile of the store
	if err != nil {
		return fmt.Errorf("ledgerlog: %w", err)
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("ledgerlog: flushing %s: %w", path, err)
	}
	return nil
}

// writeTemp writes data to a new temporary file in dir and flushes it.
func (d *DirStore) writeTemp(dir string, data []byte) (string, error) {
	f, err := os.CreateTemp(dir, tempPrefix+"*")
	if err != nil {
		return "", fmt.Errorf("ledgerlog: %w", err)
	}
	name := f.Name()
	fail := func(err error) (string, error) {
		_ = f.Close()
		_ = os.Remove(name)
		return "", fmt.Errorf("ledgerlog: %w", err)
	}
	if runtime.GOOS != "windows" {
		if err := f.Chmod(fileMode); err != nil {
			return fail(err)
		}
	}
	if _, err := f.Write(data); err != nil {
		return fail(err)
	}
	if d.NoSync {
	} else if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", fmt.Errorf("ledgerlog: %w", err)
	}
	if err := d.hook("temp-written"); err != nil {
		return name, err // the temporary file stays, as after a crash
	}
	return name, nil
}

// syncDirHook, when set, is called with every directory syncDir is asked to
// flush, before anything else (a hook for tests).
var syncDirHook func(dir string)

func syncDir(dir string) error {
	if syncDirHook != nil {
		syncDirHook(dir)
	}
	if runtime.GOOS == "windows" {
		return nil // a directory cannot be opened to flush it
	}
	f, err := os.Open(dir) // #nosec G304 -- a directory of the store
	if err != nil {
		return fmt.Errorf("ledgerlog: %w", err)
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("ledgerlog: flushing %s: %w", dir, err)
	}
	return nil
}

// Widths implements Store.
func (d *DirStore) Widths(ctx context.Context, level int, index uint64) ([]int, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if level < EntryLevel || level > MaxLevel {
		return nil, fmt.Errorf("%w: level %d", ErrBadPath, level)
	}
	var out []int
	if fi, err := os.Lstat(d.path(TileKey{level, index, TileWidth}.Path())); err == nil && fi.Mode().IsRegular() {
		out = append(out, TileWidth)
	}
	partial := TileKey{level, index, 1}.Path()
	dir := d.path(strings.TrimSuffix(partial, "/1"))
	if err := d.noLinks(dir); err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("ledgerlog: %w", err)
	}
	for _, e := range ents {
		w, ok := canonicalDecimal(e.Name(), TileWidth-1)
		if !ok || w == 0 || !e.Type().IsRegular() {
			continue
		}
		out = append(out, int(w))
	}
	sort.Ints(out)
	return out, nil
}

// GetCheckpoint implements CheckpointStore.
func (d *DirStore) GetCheckpoint(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return d.readRegular(d.path("checkpoint"), MaxNoteSize)
}

// PutCheckpoint implements CheckpointStore: the note replaces the file
// atomically (a reader sees the old note or the new one).
//
// On Windows a program that holds the checkpoint open without delete sharing
// (a file server, a verifier reading it) blocks the replacement. PutCheckpoint
// retries the rename for CheckpointRetry (DefaultCheckpointRetry when zero);
// if the file is still held it returns an error that is ErrCheckpointBusy, the
// old checkpoint is whole and no temporary file is left, and the caller may
// try again later. The same access-denied failure is also what a directory in
// the place of the checkpoint, or a checkpoint with the read-only attribute,
// gives: those do not go away by waiting, so they are named as what they are at
// once, without the wait and without ErrCheckpointBusy. The context is checked
// before the work and in the wait between two tries (the wait ends with its
// error, the old checkpoint whole and no temporary file left). Any other
// failure is returned as it is.
func (d *DirStore) PutCheckpoint(ctx context.Context, note []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(note) == 0 || len(note) > MaxNoteSize {
		return fmt.Errorf("ledgerlog: a checkpoint note has 1 to %d bytes", MaxNoteSize)
	}
	target := d.path("checkpoint")
	created, err := d.mkdirs(d.root)
	if err != nil {
		return err
	}
	if err := d.noLinks(target); err != nil {
		return err
	}
	tmp, err := d.writeTemp(d.root, note)
	if err != nil {
		return err
	}
	if err := d.hook("before-publish"); err != nil {
		return err
	}
	rename, busy := os.Rename, platformRenameBusy
	if d.rename != nil {
		rename = d.rename
	}
	if d.renameBusy != nil {
		busy = d.renameBusy
	}
	budget := d.retryBudget()
	now, pause := time.Now, sleepContext
	if d.now != nil {
		now = d.now
	}
	if d.pause != nil {
		pause = d.pause
	}
	deadline := now().Add(budget)
	wait := 5 * time.Millisecond
	var rerr, stop error
	for {
		if rerr = rename(tmp, target); rerr == nil {
			break
		}
		if !busy(rerr) {
			break
		}
		if stop = checkpointTargetProblem(target); stop != nil {
			break // waiting does not cure it
		}
		if !now().Before(deadline) {
			break
		}
		// a reader holds the old file open
		if err := pause(ctx, min(wait, deadline.Sub(now()))); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		wait = min(wait*2, 100*time.Millisecond)
	}
	if rerr != nil {
		_ = os.Remove(tmp)
		if stop != nil {
			return fmt.Errorf("ledgerlog: the checkpoint was not replaced: %w (%v)", stop, rerr)
		}
		if busy(rerr) {
			return fmt.Errorf("%w (%v)", ErrCheckpointBusy, rerr)
		}
		return fmt.Errorf("ledgerlog: %w", rerr)
	}
	if err := d.hook("after-publish"); err != nil {
		return err
	}
	return d.flush(d.root, created)
}

// RemoveTemporaryFiles deletes the temporary files a crash left below the
// root, and returns how many. It must not run while a write is in progress.
func (d *DirStore) RemoveTemporaryFiles() (int, error) {
	n := 0
	err := filepath.WalkDir(d.root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !e.IsDir() && strings.HasPrefix(e.Name(), tempPrefix) {
			if rerr := os.Remove(p); rerr != nil {
				return rerr
			}
			n++
		}
		return nil
	})
	return n, err
}

// SaveFrontierFile writes the serialised frontier to path atomically and
// durably (a temporary file in the same directory, flushed, renamed over the
// file, the directory flushed). It is the file form of the row that the
// system database keeps for a log; the caller writes it after Append has
// stored the tiles.
func SaveFrontierFile(path string, f Frontier) error {
	data := f.Marshal()
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, tempPrefix+"*")
	if err != nil {
		return fmt.Errorf("ledgerlog: %w", err)
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return fmt.Errorf("ledgerlog: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return fmt.Errorf("ledgerlog: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("ledgerlog: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("ledgerlog: %w", err)
	}
	return syncDir(dir)
}

// frontierAfterStat, when set, is called between the Lstat and the open of
// LoadFrontierFile (a hook for tests of a file that changes in between).
var frontierAfterStat func(path string)

// LoadFrontierFile reads a file written by SaveFrontierFile and checks it
// against the size and root stored with it (see LoadFrontier).
func LoadFrontierFile(path string, wantSize uint64, wantRoot Hash) (Frontier, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return Frontier{}, fmt.Errorf("ledgerlog: %w", err)
	}
	if !fi.Mode().IsRegular() || fi.Size() > MaxFrontierSize {
		return Frontier{}, fmt.Errorf("ledgerlog: %s is not a regular file of at most %d bytes", path, MaxFrontierSize)
	}
	if frontierAfterStat != nil {
		frontierAfterStat(path)
	}
	f, err := os.Open(path) // #nosec G304 -- the caller names the file
	if err != nil {
		return Frontier{}, fmt.Errorf("ledgerlog: %w", err)
	}
	defer f.Close()
	// The file opened is the one that was checked, and no more than the limit
	// is read from it, whatever the file grows to.
	if of, err := f.Stat(); err != nil || !of.Mode().IsRegular() || !os.SameFile(fi, of) {
		return Frontier{}, fmt.Errorf("ledgerlog: %s changed while it was opened", path)
	}
	data, err := readAtMost(f, path, MaxFrontierSize)
	if err != nil {
		return Frontier{}, err
	}
	return LoadFrontier(data, wantSize, wantRoot)
}

// checkpointTargetProblem says why a failed rename onto target will not
// succeed by waiting: the target is a directory, or (on Windows, where the
// attribute blocks a replacement) a file with the read-only attribute. It
// returns nil when the target is a file that another program may be holding.
func checkpointTargetProblem(target string) error {
	fi, err := os.Lstat(target)
	if err != nil {
		return nil
	}
	if fi.IsDir() {
		return errors.New("a directory is in the place of the checkpoint file")
	}
	if runtime.GOOS == "windows" && fi.Mode().Perm()&0o200 == 0 {
		return errors.New("the checkpoint file has the read-only attribute")
	}
	return nil
}

// retryBudget is how long PutCheckpoint keeps retrying a blocked rename:
// CheckpointRetry, DefaultCheckpointRetry when it is zero. A negative value
// leaves no time, so the rename is tried once.
func (d *DirStore) retryBudget() time.Duration {
	if d.CheckpointRetry == 0 {
		return DefaultCheckpointRetry
	}
	return d.CheckpointRetry
}

// sleepContext waits for d or until ctx is done, and returns the error of ctx in
// that case.
func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

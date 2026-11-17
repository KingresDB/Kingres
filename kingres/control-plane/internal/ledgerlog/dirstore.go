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
type DirStore struct {
	root string
	// NoSync skips the flush of files and directories. It is for tests and for
	// rebuilding a publication that can be rebuilt again; with it a crash of
	// the machine can lose files that were reported as written.
	NoSync bool
	// Hook, when set, is called at the steps of a write ("temp-written",
	// "before-publish", "after-publish") and may return an error that stops the
	// write there, as a crash would (a hook for tests).
	Hook func(step string) error
}

// NewDirStore returns a store over dir, creating it (mode 0755) when needed.
func NewDirStore(dir string) (*DirStore, error) {
	if dir == "" {
		return nil, errors.New("ledgerlog: the directory is empty")
	}
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, fmt.Errorf("ledgerlog: %w", err)
	}
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("ledgerlog: %s is not a directory", dir)
	}
	return &DirStore{root: dir}, nil
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
	f, err := os.Open(path) // #nosec G304 -- built from numbers below the root
	if err != nil {
		return nil, fmt.Errorf("ledgerlog: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, fmt.Errorf("ledgerlog: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("ledgerlog: %s has more than %d bytes", path, limit)
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
func (d *DirStore) GetTile(_ context.Context, k TileKey) ([]byte, error) {
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
// the name exists, so a tile is never replaced; a store on a file system that
// has no hard links falls back to a check and a rename.
func (d *DirStore) PutTile(ctx context.Context, k TileKey, data []byte) error {
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
	if done, err := same(); done || err != nil {
		return err
	}
	if err := d.mkdirs(filepath.Dir(target)); err != nil {
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
	if err := os.Link(tmp, target); err != nil {
		if errors.Is(err, fs.ErrExist) {
			_, serr := same()
			return serr
		}
		// No hard links here: check again and rename.
		if done, serr := same(); done || serr != nil {
			return serr
		}
		if rerr := os.Rename(tmp, target); rerr != nil {
			return fmt.Errorf("ledgerlog: %w", rerr)
		}
	}
	if err := d.hook("after-publish"); err != nil {
		crashed = true
		return err
	}
	if d.NoSync {
		return nil
	}
	return syncDir(filepath.Dir(target))
}

func (d *DirStore) mkdirs(dir string) error {
	if err := d.noLinks(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("ledgerlog: %w", err)
	}
	return d.noLinks(dir)
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

func syncDir(dir string) error {
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
func (d *DirStore) Widths(_ context.Context, level int, index uint64) ([]int, error) {
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
func (d *DirStore) GetCheckpoint(context.Context) ([]byte, error) {
	return d.readRegular(d.path("checkpoint"), MaxNoteSize)
}

// PutCheckpoint implements CheckpointStore: the note replaces the file
// atomically (a reader sees the old note or the new one).
func (d *DirStore) PutCheckpoint(_ context.Context, note []byte) error {
	if len(note) == 0 || len(note) > MaxNoteSize {
		return fmt.Errorf("ledgerlog: a checkpoint note has 1 to %d bytes", MaxNoteSize)
	}
	target := d.path("checkpoint")
	if err := d.mkdirs(d.root); err != nil {
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
	var rerr error
	for i := 0; i < 8; i++ {
		if rerr = os.Rename(tmp, target); rerr == nil {
			break
		}
		if runtime.GOOS != "windows" {
			break
		}
		time.Sleep(20 * time.Millisecond) // a reader holds the old file open
	}
	if rerr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("ledgerlog: %w", rerr)
	}
	if err := d.hook("after-publish"); err != nil {
		return err
	}
	if d.NoSync {
		return nil
	}
	return syncDir(d.root)
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
	data, err := os.ReadFile(path) // #nosec G304 -- the caller names the file
	if err != nil {
		return Frontier{}, fmt.Errorf("ledgerlog: %w", err)
	}
	return LoadFrontier(data, wantSize, wantRoot)
}

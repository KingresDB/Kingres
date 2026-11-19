// SPDX-License-Identifier: AGPL-3.0-only

package safefile

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"
)

// Dir is a source directory: a publication the auditor was pointed at. Every
// read goes through os.Root, so a path cannot leave the directory, and through
// a stricter rule of our own: no component of the path below the root may be a
// symbolic link.
//
// Why refuse links instead of resolving them inside the root: a publication
// written by the publisher's own tools has no reason for a link (tiles and the
// checkpoint are plain files), so a link is either an accident or an attempt to
// alias one tile as another or to reach outside. Resolving inside the root would
// need the verifier to explain which alias is legal, for no benefit. A link
// that is refused is reported as a malformed publication. os.Root stays as
// the second layer in case a link is swapped in after our check.
type Dir struct {
	root *os.Root
	name string
}

// OpenDir opens the directory the auditor named. The path itself may be a
// symbolic link (the auditor chose it); it must resolve to a directory.
func OpenDir(path string) (*Dir, error) {
	if err := checkArgPath(path); err != nil {
		return nil, err
	}
	// os.OpenRoot opens the name itself and blocks on a named pipe, so look at
	// it first (Stat opens nothing and follows the links the auditor chose).
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%s %w (a directory is needed, it is %s)", path, ErrNotRegular, kind(st.Mode()))
	}
	r, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	return &Dir{root: r, name: path}, nil
}

// Close releases the directory.
func (d *Dir) Close() error { return d.root.Close() }

// Name is the path the directory was opened with.
func (d *Dir) Name() string { return d.name }

// CleanRel validates a slash separated relative path of the form the verifier
// builds from numbers and hex: non-empty components that are not "." or "..",
// no backslash, colon or NUL, each at most 255 bytes, at most 16 components.
// It exists so that a path that was built from attacker data (it never is
// today) still cannot reach beyond the directory.
func CleanRel(rel string) ([]string, error) {
	if rel == "" {
		return nil, errors.New("empty path")
	}
	parts := strings.Split(rel, "/")
	if len(parts) > 16 {
		return nil, fmt.Errorf("path %q has too many components", rel)
	}
	for _, p := range parts {
		switch {
		case p == "" || p == "." || p == "..":
			return nil, fmt.Errorf("path %q has an empty, . or .. component", rel)
		case len(p) > 255:
			return nil, fmt.Errorf("path %q has a component longer than 255 bytes", rel)
		case strings.ContainsAny(p, `\:`+"\x00"):
			return nil, fmt.Errorf("path %q has a backslash, colon or NUL", rel)
		}
	}
	return parts, nil
}

// walk checks every component below the root with Lstat: the intermediate
// ones must be directories, the last must be a regular file (wantDir false) or
// a directory (wantDir true). A component that does not exist gives an error
// that wraps fs.ErrNotExist.
func (d *Dir) walk(rel string, wantDir bool) error {
	parts, err := CleanRel(rel)
	if err != nil {
		return err
	}
	return d.walkParts(parts, rel, wantDir)
}

// walkParts is walk for a path that is already split and checked.
func (d *Dir) walkParts(parts []string, rel string, wantDir bool) error {
	for i := range parts {
		sub := strings.Join(parts[:i+1], "/")
		st, err := d.root.Lstat(sub)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("%s: %w", sub, fs.ErrNotExist)
			}
			return fmt.Errorf("%s: %w", sub, err)
		}
		m := st.Mode()
		// A symbolic link has ModeSymlink; a Windows junction or other
		// reparse point has ModeIrregular. Neither is a plain file or
		// directory, so they are refused as links.
		if m&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
			return fmt.Errorf("%s %w", sub, ErrLink)
		}
		last := i == len(parts)-1
		switch {
		case !last && !m.IsDir():
			return fmt.Errorf("%s %w (a directory is needed on the way to %s, it is %s)", sub, ErrNotRegular, rel, kind(m))
		case last && wantDir && !m.IsDir():
			return fmt.Errorf("%s %w (a directory is needed, it is %s)", sub, ErrNotRegular, kind(m))
		case last && !wantDir && !m.IsRegular():
			return fmt.Errorf("%s %w (it is %s)", sub, ErrNotRegular, kind(m))
		}
	}
	return nil
}

// ReadFile reads the regular file rel (slash separated) of the directory.
// A missing file wraps fs.ErrNotExist; a link, a special file or a file larger
// than limit wraps ErrLink, ErrNotRegular or ErrTooLarge (see Unusable).
func (d *Dir) ReadFile(rel string, limit int64) ([]byte, error) {
	if err := d.walk(rel, false); err != nil {
		return nil, err
	}
	return d.readOpen(rel, limit)
}

// readOpen opens a path that walk has checked and reads it.
func (d *Dir) readOpen(rel string, limit int64) ([]byte, error) {
	f, err := d.root.OpenFile(rel, os.O_RDONLY|openFlags, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%s: %w", rel, fs.ErrNotExist)
		}
		return nil, fmt.Errorf("%s: %w", rel, err)
	}
	defer f.Close()
	return readAll(f, rel, limit)
}

// CleanName validates a name taken from a listing of the directory. It is not
// a path: whatever the system lists as one entry is a name (a colon, a space,
// a leading dash), and only what could not be one entry is refused: nothing,
// a dot name, a separator, a NUL, more than 255 bytes. CleanRel is the rule for
// paths the program builds; the two are different questions.
func CleanName(name string) error {
	switch {
	case name == "" || name == "." || name == "..":
		return fmt.Errorf("name %q is not a file name", name)
	case len(name) > 255:
		return fmt.Errorf("name %q is longer than 255 bytes", name)
	case strings.ContainsAny(name, "/") || strings.IndexByte(name, 0) >= 0:
		return fmt.Errorf("name %q has a slash or a NUL, so it is not one entry of the directory", name)
	}
	return nil
}

// ReadEntry reads the regular file called name in the root of the directory
// (a name the listing returned, see CleanName), with the checks of ReadFile.
func (d *Dir) ReadEntry(name string, limit int64) ([]byte, error) {
	if err := CleanName(name); err != nil {
		return nil, err
	}
	if err := d.walkParts([]string{name}, name, false); err != nil {
		return nil, err
	}
	return d.readOpen(name, limit)
}

// SubDirs lists the directory rel: the names of its subdirectories, and the
// names of entries that are neither directories nor regular files (links and
// special files), which the caller may report. At most MaxDirEntries entries are
// read; a larger directory wraps ErrTooLarge.
func (d *Dir) SubDirs(rel string) (dirs, others []string, err error) {
	if err := d.walk(rel, true); err != nil {
		return nil, nil, err
	}
	f, err := d.root.OpenFile(rel, os.O_RDONLY|openFlags, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", rel, err)
	}
	defer f.Close()
	ents, err := f.ReadDir(MaxDirEntries + 1)
	if err != nil && len(ents) == 0 && !errors.Is(err, io.EOF) {
		return nil, nil, fmt.Errorf("listing %s: %w", rel, err)
	}
	if len(ents) > MaxDirEntries {
		return nil, nil, fmt.Errorf("%s %w (more than %d entries)", rel, ErrTooLarge, MaxDirEntries)
	}
	for _, en := range ents {
		switch {
		case en.IsDir():
			dirs = append(dirs, en.Name())
		case !en.Type().IsRegular():
			others = append(others, en.Name())
		}
	}
	return dirs, others, nil
}

// RegularNames lists the names of the regular files directly in the root of the
// directory, sorted, and separately the names of the entries that are neither
// regular files nor directories (links, pipes, devices), which the caller may
// refuse by name: a pipe called x.json must not be skipped silently. At most MaxDirEntries entries are read; a larger directory wraps
// ErrTooLarge. The names are returned as names, never as a pattern: a directory
// whose name has glob characters is just a directory.
func (d *Dir) RegularNames() (names, others []string, err error) {
	f, err := d.root.Open(".")
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	ents, err := f.ReadDir(MaxDirEntries + 1)
	if err != nil && len(ents) == 0 && !errors.Is(err, io.EOF) {
		return nil, nil, fmt.Errorf("listing %s: %w", d.name, err)
	}
	if len(ents) > MaxDirEntries {
		return nil, nil, fmt.Errorf("%s %w (more than %d entries)", d.name, ErrTooLarge, MaxDirEntries)
	}
	for _, en := range ents {
		switch {
		case en.Type().IsRegular():
			names = append(names, en.Name())
		case !en.IsDir():
			others = append(others, en.Name())
		}
	}
	sort.Strings(names)
	sort.Strings(others)
	return names, others, nil
}

// RegularFiles lists the names of the regular files directly in the directory rel, sorted. Links
// and special files are refused as in the other reads (an error that Unusable recognises); at most
// MaxDirEntries entries are read.
func (d *Dir) RegularFiles(rel string) ([]string, error) {
	if err := d.walk(rel, true); err != nil {
		return nil, err
	}
	f, err := d.root.OpenFile(rel, os.O_RDONLY|openFlags, 0)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", rel, err)
	}
	defer f.Close()
	ents, err := f.ReadDir(MaxDirEntries + 1)
	if err != nil && len(ents) == 0 && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("listing %s: %w", rel, err)
	}
	if len(ents) > MaxDirEntries {
		return nil, fmt.Errorf("%s %w (more than %d entries)", rel, ErrTooLarge, MaxDirEntries)
	}
	var out []string
	for _, en := range ents {
		switch {
		case en.IsDir():
		case en.Type().IsRegular():
			out = append(out, en.Name())
		default:
			return nil, fmt.Errorf("%s/%s %w", rel, en.Name(), ErrNotRegular)
		}
	}
	sort.Strings(out)
	return out, nil
}

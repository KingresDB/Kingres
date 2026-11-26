// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package pubsrc

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/safefile"
)

// Limits of an archive, applied while it is read: a hostile archive must not use the memory of
// the machine (the tests show them at small values through the variables).
var (
	// MaxArchiveFile is the size of the .tar.gz file itself.
	MaxArchiveFile int64 = 1 << 30
	// MaxArchiveFiles is the number of entries.
	MaxArchiveFiles = 200000
	// MaxArchiveTotal is the size of everything the archive holds, after decompression.
	MaxArchiveTotal int64 = 512 << 20
	// MaxArchiveMember is the size of one file of the archive.
	MaxArchiveMember int64 = 32 << 20
)

type tarSource struct {
	files map[string][]byte
	dirs  map[string]map[string]bool // directory -> names of its subdirectories
}

// OpenTar reads a .tar.gz into memory. Only regular files and directories are accepted; a name
// must be a plain relative path (CleanRel, with one leading "./" allowed), so it can never name a
// place outside the archive; the same name twice, a name that is both a file and a directory, a
// second gzip member and bytes after the end of the archive are refused.
func OpenTar(path string) (Source, error) {
	rc, err := safefile.OpenArg(path, MaxArchiveFile)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	br := bufio.NewReader(rc) // a ByteReader: the gzip reader takes no more than its member
	zr, err := gzip.NewReader(br)
	if err != nil {
		return nil, fmt.Errorf("%s is not a gzip file: %w", path, err)
	}
	zr.Multistream(false)
	s := &tarSource{files: map[string][]byte{}, dirs: map[string]map[string]bool{"": {}}}
	tr := tar.NewReader(io.LimitReader(zr, MaxArchiveTotal+1<<20))
	var total int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: reading the archive: %w", path, err)
		}
		if len(s.files)+len(s.dirs) > MaxArchiveFiles {
			return nil, fmt.Errorf("%s holds more than %d entries: %w", path, MaxArchiveFiles, safefile.ErrTooLarge)
		}
		name := strings.TrimPrefix(h.Name, "./")
		name = strings.TrimSuffix(name, "/")
		switch h.Typeflag {
		case tar.TypeDir:
			if name == "" || name == "." {
				continue
			}
			if _, err := safefile.CleanRel(name); err != nil {
				return nil, fmt.Errorf("%s: an entry named %q: %w", path, h.Name, err)
			}
			if _, isFile := s.files[name]; isFile {
				return nil, fmt.Errorf("%s: %q is both a file and a directory", path, name)
			}
			s.addDir(name)
		case tar.TypeReg:
			if _, err := safefile.CleanRel(name); err != nil {
				return nil, fmt.Errorf("%s: an entry named %q: %w", path, h.Name, err)
			}
			if h.Size < 0 || h.Size > MaxArchiveMember {
				return nil, fmt.Errorf("%s: %q is %d bytes: %w", path, name, h.Size, safefile.ErrTooLarge)
			}
			if total += h.Size; total > MaxArchiveTotal {
				return nil, fmt.Errorf("%s holds more than %d bytes: %w", path, MaxArchiveTotal, safefile.ErrTooLarge)
			}
			if _, dup := s.files[name]; dup {
				return nil, fmt.Errorf("%s holds %q twice", path, name)
			}
			if _, isDir := s.dirs[name]; isDir {
				return nil, fmt.Errorf("%s: %q is both a file and a directory", path, name)
			}
			data, err := io.ReadAll(io.LimitReader(tr, h.Size+1))
			if err != nil || int64(len(data)) != h.Size {
				return nil, fmt.Errorf("%s: reading %q: the entry is cut off or longer than it says", path, name)
			}
			s.files[name] = data
			s.addParents(name)
		default:
			// links, devices, named pipes, sockets, global headers: nothing a publication has
			return nil, fmt.Errorf("%s: %q is not a regular file or a directory (type %q): %w", path, h.Name, string(h.Typeflag), safefile.ErrNotRegular)
		}
	}
	// nothing may follow the archive: not bytes, not a second gzip member
	rest, err := io.ReadAll(io.LimitReader(zr, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%s: the end of the archive: %w", path, err)
	}
	for _, b := range rest {
		if b != 0 {
			return nil, fmt.Errorf("%s holds bytes after the end of the archive", path)
		}
	}
	if _, perr := br.Peek(1); perr == nil {
		return nil, fmt.Errorf("%s holds data after the gzip stream (a second member is refused)", path)
	}
	return s, nil
}

func (s *tarSource) addDir(name string) {
	parts := strings.Split(name, "/")
	for i := range parts {
		parent := strings.Join(parts[:i], "/")
		cur := strings.Join(parts[:i+1], "/")
		if s.dirs[parent] == nil {
			s.dirs[parent] = map[string]bool{}
		}
		s.dirs[parent][parts[i]] = true
		if s.dirs[cur] == nil {
			s.dirs[cur] = map[string]bool{}
		}
	}
}

func (s *tarSource) addParents(file string) {
	if i := strings.LastIndexByte(file, '/'); i >= 0 {
		s.addDir(file[:i])
	}
}

func (s *tarSource) Kind() string { return "tar.gz" }
func (s *tarSource) Close() error { return nil }

func (s *tarSource) ReadFile(rel string, limit int64) ([]byte, error) {
	if _, err := safefile.CleanRel(rel); err != nil {
		return nil, err
	}
	if _, isDir := s.dirs[rel]; isDir {
		return nil, fmt.Errorf("%s %w (it is a directory)", rel, safefile.ErrNotRegular)
	}
	data, ok := s.files[rel]
	if !ok {
		return nil, fmt.Errorf("%s: %w", rel, fs.ErrNotExist)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s %w (%d bytes, at most %d)", rel, safefile.ErrTooLarge, len(data), limit)
	}
	return data, nil
}

func (s *tarSource) SubDirs(rel string) ([]string, []string, error) {
	if rel == "." {
		rel = ""
	} else if _, err := safefile.CleanRel(rel); err != nil {
		return nil, nil, err
	}
	subs, ok := s.dirs[rel]
	if !ok {
		return nil, nil, fmt.Errorf("%s: %w", rel, fs.ErrNotExist)
	}
	var dirs []string
	for n := range subs {
		dirs = append(dirs, n)
	}
	sort.Strings(dirs)
	return dirs, nil, nil
}

func (s *tarSource) Files(rel string) ([]string, error) {
	if rel != "" {
		if _, err := safefile.CleanRel(rel); err != nil {
			return nil, err
		}
	}
	if _, ok := s.dirs[rel]; !ok {
		return nil, fmt.Errorf("%s: %w", rel, fs.ErrNotExist)
	}
	var out []string
	prefix := rel + "/"
	if rel == "" {
		prefix = ""
	}
	for name := range s.files {
		if strings.HasPrefix(name, prefix) && !strings.Contains(name[len(prefix):], "/") {
			out = append(out, name[len(prefix):])
		}
	}
	sort.Strings(out)
	return out, nil
}

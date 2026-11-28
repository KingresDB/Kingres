// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

// Package safefile reads the files the verifier is given, and nothing else.
//
// The verifier runs on an auditor's machine over material from a publisher
// that may be hostile. A named pipe in a directory the publisher prepared
// blocks open(2) for ever, a symbolic link can point at any file the auditor
// can read, and a device such as /dev/zero never ends. Every file the verifier
// opens therefore goes through this package, which
//
//   - never blocks on open (O_NONBLOCK on Unix, no device or pipe namespace on
//     Windows),
//   - requires a regular file after the open (a check on the open handle, so
//     there is no gap between the check and the use),
//   - bounds the size before reading and again while reading, because a file
//     can grow between the two,
//   - for a file in a source directory, refuses every symbolic link or other
//     reparse point in any component of the path below the root and, as a
//     second layer, lets os.Root refuse any path that would leave the root.
package safefile

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"runtime"
	"strings"
)

// Errors that say the thing exists and cannot be used. The publisher or the
// directory is at fault, not the auditor's command line, so the commands map
// them to a failed verification, unlike an absent file.
var (
	// ErrNotRegular is a directory, device, named pipe or socket where a
	// regular file is required.
	ErrNotRegular = errors.New("is not a regular file")
	// ErrLink is a symbolic link or other reparse point in a source directory.
	ErrLink = errors.New("is or passes through a symbolic link")
	// ErrTooLarge is a file larger than the limit of the caller.
	ErrTooLarge = errors.New("is larger than the limit")
)

// Unusable reports whether err says that a file exists and must not be used:
// not regular, a link, or too large.
func Unusable(err error) bool {
	return errors.Is(err, ErrNotRegular) || errors.Is(err, ErrLink) || errors.Is(err, ErrTooLarge)
}

// MaxDirEntries bounds a directory listing: a hostile directory must not make
// the verifier hold millions of names.
const MaxDirEntries = 1024

// statReader is what readAll needs from an open file.
type statReader interface {
	io.Reader
	Stat() (fs.FileInfo, error)
}

// readAll reads a regular file of at most limit bytes from an open handle.
func readAll(f statReader, name string, limit int64) ([]byte, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s %w (it is %s)", name, ErrNotRegular, kind(st.Mode()))
	}
	if st.Size() > limit {
		return nil, fmt.Errorf("%s %w (%d bytes, at most %d)", name, ErrTooLarge, st.Size(), limit)
	}
	// The size can change after the Stat: read at most one byte more than the
	// limit and refuse if it is there.
	// The buffer is made once, of the size the file has now plus the one byte
	// that shows it grew: io.ReadAll would grow it by doubling and hold up to
	// three times the file while it does.
	data := make([]byte, st.Size()+1)
	n, err := io.ReadFull(io.LimitReader(f, limit+1), data)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}
	data = data[:n]
	if err == nil {
		// The file is larger than it was: read what may still be there.
		rest, rerr := io.ReadAll(io.LimitReader(f, limit+1-int64(n)))
		if rerr != nil {
			return nil, fmt.Errorf("reading %s: %w", name, rerr)
		}
		data = append(data, rest...)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s %w (more than %d bytes)", name, ErrTooLarge, limit)
	}
	return data, nil
}

func kind(m fs.FileMode) string {
	switch {
	case m.IsRegular():
		return "a regular file"
	case m.IsDir():
		return "a directory"
	case m&fs.ModeSymlink != 0:
		return "a symbolic link"
	case m&fs.ModeNamedPipe != 0:
		return "a named pipe"
	case m&fs.ModeSocket != 0:
		return "a socket"
	case m&fs.ModeDevice != 0:
		return "a device"
	}
	return "a special file"
}

// ReadArg reads a file the auditor named on the command line. It may be a
// symbolic link (the auditor chose it) but it must be a regular file after
// the link is resolved, it is opened without blocking, and at most limit
// bytes are read.
func ReadArg(path string, limit int64) ([]byte, error) {
	if err := checkArgPath(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|openFlags, 0) // #nosec G304 -- the path is a command line argument
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readAll(f, path, limit)
}

// checkArgPath refuses, before any open, the names the Windows file system
// treats as devices, pipes or alternate data streams. On other systems a path
// is only a path.
func checkArgPath(path string) error {
	if runtime.GOOS != "windows" {
		return nil
	}
	return checkWindowsPath(path)
}

// checkWindowsPath is the Windows rule, separate so that it is tested on
// every platform.
func checkWindowsPath(path string) error {
	p := strings.ReplaceAll(path, "/", `\`)
	if strings.HasPrefix(p, `\\.\`) || strings.HasPrefix(p, `\\?\GLOBALROOT`) {
		return fmt.Errorf("%q %w (the device namespace is not a file)", path, ErrNotRegular)
	}
	rest := strings.TrimPrefix(p, `\\?\`)
	for i, comp := range strings.Split(rest, `\`) {
		if i == 0 && len(comp) == 2 && comp[1] == ':' {
			continue // a drive letter
		}
		if strings.Contains(comp, ":") {
			return fmt.Errorf("%q %w (a colon names an alternate data stream)", path, ErrNotRegular)
		}
		if reservedWindowsName(comp) {
			return fmt.Errorf("%q %w (a reserved device name)", path, ErrNotRegular)
		}
	}
	return nil
}

// reservedWindowsName reports whether a path component names a DOS device:
// CON, PRN, AUX, NUL, COM1-9, LPT1-9, CONIN$, CONOUT$, with or without an
// extension, in any case, and with trailing spaces or dots.
func reservedWindowsName(comp string) bool {
	base := comp
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	base = strings.ToUpper(strings.TrimRight(base, " "))
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) {
		c := base[3]
		return (c >= '1' && c <= '9')
	}
	// COM and LPT followed by a superscript digit (U+00B9, U+00B2, U+00B3).
	if strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT") {
		switch base[3:] {
		case "\u00b9", "\u00b2", "\u00b3":
			return true
		}
	}
	return false
}

// limitedFile is an open regular file that fails with ErrTooLarge when more than
// limit bytes are read from it, whatever its size was when it was opened.
type limitedFile struct {
	f    *os.File
	name string
	left int64
}

func (l *limitedFile) Read(p []byte) (int, error) {
	if l.left < 0 {
		return 0, fmt.Errorf("%s %w", l.name, ErrTooLarge)
	}
	if int64(len(p)) > l.left+1 {
		p = p[:l.left+1]
	}
	n, err := l.f.Read(p)
	l.left -= int64(n)
	if l.left < 0 {
		return n, fmt.Errorf("%s %w (more than the limit)", l.name, ErrTooLarge)
	}
	return n, err
}

func (l *limitedFile) Close() error { return l.f.Close() }

// OpenArg opens a file named on the command line for reading in pieces, with
// the checks of ReadArg: never blocking, regular after links are resolved,
// at most limit bytes now and while it is read.
func OpenArg(path string, limit int64) (io.ReadCloser, error) {
	if err := checkArgPath(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|openFlags, 0) // #nosec G304 -- the path is a command line argument
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if !st.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%s %w (it is %s)", path, ErrNotRegular, kind(st.Mode()))
	}
	if st.Size() > limit {
		_ = f.Close()
		return nil, fmt.Errorf("%s %w (%d bytes, at most %d)", path, ErrTooLarge, st.Size(), limit)
	}
	return &limitedFile{f: f, name: path, left: limit}, nil
}

// hashAll hashes a regular file of at most limit bytes from an open handle
// without holding its bytes: they go through a fixed buffer into the hash. The
// size is bounded before reading and again while reading.
func hashAll(f statReader, name string, limit int64) ([sha256.Size]byte, int64, error) {
	var sum [sha256.Size]byte
	st, err := f.Stat()
	if err != nil {
		return sum, 0, fmt.Errorf("%s: %w", name, err)
	}
	if !st.Mode().IsRegular() {
		return sum, 0, fmt.Errorf("%s %w (it is %s)", name, ErrNotRegular, kind(st.Mode()))
	}
	if st.Size() > limit {
		return sum, 0, fmt.Errorf("%s %w (%d bytes, at most %d)", name, ErrTooLarge, st.Size(), limit)
	}
	h := sha256.New()
	// One byte more than the limit shows that the file grew after the Stat.
	n, err := io.Copy(h, io.LimitReader(f, limit+1))
	if err != nil {
		return sum, 0, fmt.Errorf("reading %s: %w", name, err)
	}
	if n > limit {
		return sum, 0, fmt.Errorf("%s %w (more than %d bytes)", name, ErrTooLarge, limit)
	}
	copy(sum[:], h.Sum(nil))
	return sum, n, nil
}

// HashArg is ReadArg for a file that is only hashed: the SHA-256 and the size
// of a file the auditor named on the command line, with the same rules as
// ReadArg (a regular file after links, opened without blocking, at most limit
// bytes), and memory that does not depend on the size of the file.
func HashArg(path string, limit int64) ([sha256.Size]byte, int64, error) {
	var zero [sha256.Size]byte
	if err := checkArgPath(path); err != nil {
		return zero, 0, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|openFlags, 0) // #nosec G304 -- the path is a command line argument
	if err != nil {
		return zero, 0, err
	}
	defer f.Close()
	return hashAll(f, path, limit)
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package staging

import (
	"errors"
	"io"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"
)

// faultFS is the real file system with a fake size and free space, a budget of bytes that can be written before "no space left", and a
// hook before a hard link. It is the test filesystem of the disk-full tests.
type faultFS struct {
	OSFS
	mu         sync.Mutex
	fakeSpace  bool
	space      Space
	budget     int64 // bytes that may still be written; negative means no limit
	beforeLink func()
	written    int64
	files      []File
}

func newFaultFS() *faultFS {
	return &faultFS{fakeSpace: true, space: Space{Total: 1 << 40, Free: 1 << 40}, budget: -1}
}

func (f *faultFS) setSpace(total, free uint64) {
	f.mu.Lock()
	f.space = Space{Total: total, Free: free}
	f.mu.Unlock()
}

func (f *faultFS) setBudget(n int64) {
	f.mu.Lock()
	f.budget = n
	f.mu.Unlock()
}

func (f *faultFS) Space(dir string) (Space, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fakeSpace {
		return f.space, nil
	}
	return spaceOf(dir)
}

func (f *faultFS) Link(a, b string) error {
	if f.beforeLink != nil {
		f.beforeLink()
	}
	return f.OSFS.Link(a, b)
}

func (f *faultFS) OpenFile(name string, flag int, perm fs.FileMode) (File, error) {
	file, err := f.OSFS.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}
	ff := &faultFile{File: file, f: f}
	f.mu.Lock()
	f.files = append(f.files, ff)
	f.mu.Unlock()
	return ff, nil
}

type faultFile struct {
	File
	f *faultFS
}

func (w *faultFile) Write(p []byte) (int, error) {
	w.f.mu.Lock()
	n := len(p)
	full := false
	if w.f.budget >= 0 {
		if int64(n) > w.f.budget {
			n = int(w.f.budget)
			full = true
		}
		w.f.budget -= int64(n)
	}
	w.f.written += int64(n)
	w.f.mu.Unlock()
	if n > 0 {
		if _, err := w.File.Write(p[:n]); err != nil {
			return 0, err
		}
	}
	if full {
		return n, &fs.PathError{Op: "write", Path: "staging", Err: syscall.ENOSPC}
	}
	return n, nil
}

var testKeys = NewKeysForTest(7)

// testStore opens a store in a fresh directory on a fake file system with plenty of space.
func testStore(t *testing.T, mut func(*Config)) (*Store, *faultFS) {
	t.Helper()
	ffs := newFaultFS()
	cfg := Config{
		FS:      ffs,
		Dir:     filepath.Join(t.TempDir(), "stage"),
		Quota:   1 << 30,
		Reserve: func(uint64) uint64 { return 1 << 20 },
	}
	if mut != nil {
		mut(&cfg)
	}
	t.Cleanup(func() {
		ffs.mu.Lock()
		defer ffs.mu.Unlock()
		for _, f := range ffs.files {
			_ = f.Close()
		}
	})
	s, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s, ffs
}

func payload(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b) //nolint:gosec // test data
	return b
}

// sealFile writes data as transfer id and returns the meta and the data key.
func sealFile(t *testing.T, s *Store, id int64, data []byte) (FileMeta, []byte) {
	t.Helper()
	res, err := s.Reserve(id, "owner", int64(len(data))+int64(len(data)/SegmentSize+2)*tagSize+HeaderSize)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	w, meta, err := s.Create(id, res, testKeys)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := w.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	dk, err := testKeys.Unwrap(id, meta.KeyID, meta.WrappedKey)
	if err != nil {
		t.Fatalf("Unwrap: %v", err)
	}
	return meta, dk
}

func readAll(t *testing.T, r *Reader) []byte {
	t.Helper()
	b := make([]byte, r.Size())
	if _, err := r.ReadAt(b, 0); err != nil && r.Size() > 0 {
		if !isEOF(err) {
			t.Fatalf("ReadAt: %v", err)
		}
	}
	return b
}

func isEOF(err error) bool { return errors.Is(err, io.EOF) }

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustWrite(t *testing.T, p string, b []byte) {
	t.Helper()
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func unixOnly(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("file modes and owners are not enforced on Windows")
	}
}

func age(t *testing.T, p string, d time.Duration) {
	t.Helper()
	old := time.Now().Add(-d)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
}

// memFile is a File over bytes, for the fuzz tests of the reader.
type memFile struct{ b []byte }

func (m *memFile) Read([]byte) (int, error)       { return 0, fs.ErrInvalid }
func (m *memFile) Write([]byte) (int, error)      { return 0, fs.ErrInvalid }
func (m *memFile) Seek(int64, int) (int64, error) { return 0, fs.ErrInvalid }
func (m *memFile) Close() error                   { return nil }
func (m *memFile) Sync() error                    { return nil }
func (m *memFile) Truncate(int64) error           { return fs.ErrInvalid }
func (m *memFile) Stat() (fs.FileInfo, error)     { return memInfo(len(m.b)), nil }
func (m *memFile) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off > int64(len(m.b)) {
		return 0, fs.ErrInvalid
	}
	n := copy(p, m.b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

type memInfo int

func (m memInfo) Name() string       { return "mem" }
func (m memInfo) Size() int64        { return int64(m) }
func (m memInfo) Mode() fs.FileMode  { return 0o600 }
func (m memInfo) ModTime() time.Time { return time.Time{} }
func (m memInfo) IsDir() bool        { return false }
func (m memInfo) Sys() any           { return nil }

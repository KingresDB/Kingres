// SPDX-License-Identifier: AGPL-3.0-only

package safefile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// within fails the test when f takes a second or more: a blocked open is the
// bug these tests exist for, so it must show as a failure, not as a hang.
func within(t *testing.T, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the call did not return within a second")
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// symlink skips the test where the platform will not make links (Windows
// without the privilege).
func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot make a symbolic link here: %v", err)
	}
}

func TestReadArg(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "ok"), "hello")
	got, err := ReadArg(filepath.Join(dir, "ok"), 5)
	if err != nil || string(got) != "hello" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := ReadArg(filepath.Join(dir, "ok"), 4); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("a file larger than the limit: %v", err)
	}
	if _, err := ReadArg(dir, 100); !errors.Is(err, ErrNotRegular) || !Unusable(err) {
		t.Fatalf("a directory: %v", err)
	}
	if _, err := ReadArg(filepath.Join(dir, "absent"), 100); !errors.Is(err, fs.ErrNotExist) || Unusable(err) {
		t.Fatalf("an absent file: %v", err)
	}
}

func TestReadArgFollowsLinkToRegularFile(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "real"), "x")
	symlink(t, filepath.Join(dir, "real"), filepath.Join(dir, "link"))
	// The auditor chose the link, so it is read.
	if got, err := ReadArg(filepath.Join(dir, "link"), 10); err != nil || string(got) != "x" {
		t.Fatalf("link to a regular file: %q, %v", got, err)
	}
	// A link to a directory is still not a regular file after resolution.
	symlink(t, dir, filepath.Join(dir, "dirlink"))
	if _, err := ReadArg(filepath.Join(dir, "dirlink"), 10); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("link to a directory: %v", err)
	}
}

// grower is a regular file whose size grows between the Stat and the read.
type grower struct{ read int64 }

func (g *grower) Stat() (fs.FileInfo, error) { return growerInfo{}, nil }
func (g *grower) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	g.read += int64(len(p))
	return len(p), nil
}

type growerInfo struct{}

func (growerInfo) Name() string       { return "grower" }
func (growerInfo) Size() int64        { return 3 }
func (growerInfo) Mode() fs.FileMode  { return 0o644 }
func (growerInfo) ModTime() time.Time { return time.Time{} }
func (growerInfo) IsDir() bool        { return false }
func (growerInfo) Sys() any           { return nil }

func TestReadAllBoundsAFileThatGrows(t *testing.T) {
	g := &grower{}
	_, err := readAll(g, "grower", 100)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("a file that grows past the bound: %v", err)
	}
	if g.read > 100+4096 {
		t.Fatalf("read %d bytes for a limit of 100", g.read)
	}
}

func TestWindowsNames(t *testing.T) {
	bad := []string{`NUL`, `nul.txt`, `C:\x\CON`, `COM1`, `lpt9.log`, `\\.\pipe\x`, `//./PhysicalDrive0`, `C:\a\file:stream`, `CONIN$`, `x\AUX.`, `\\?\GLOBALROOT\Device\x`, "COM\u00b9"}
	for _, p := range bad {
		if err := checkWindowsPath(p); err == nil || !errors.Is(err, ErrNotRegular) {
			t.Errorf("%q must be refused, got %v", p, err)
		}
	}
	good := []string{`C:\Users\a\trust.json`, `trust.json`, `..\x\y`, `\\server\share\f`, `\\?\C:\long\path`, `COM10`, `console.txt`, `a/b/c`}
	for _, p := range good {
		if err := checkWindowsPath(p); err != nil {
			t.Errorf("%q must be accepted, got %v", p, err)
		}
	}
}

func TestCleanRel(t *testing.T) {
	for _, p := range []string{"", "/a", "a//b", "a/../b", "./a", "a/.", "a\\b", "a:b", "a\x00b", strings.Repeat("x", 256), strings.Repeat("a/", 17) + "a"} {
		if _, err := CleanRel(p); err == nil {
			t.Errorf("%q must be refused", p)
		}
	}
	if parts, err := CleanRel("c/ab/e1/tile/0/x001/234.p/7"); err != nil || len(parts) != 8 {
		t.Errorf("a normal path: %v %v", parts, err)
	}
}

func FuzzCleanRel(f *testing.F) {
	for _, s := range []string{"a/b", "", "..", "a/../b", "x/./y", "c/ab/e1/checkpoint", "a\\b", "a:b"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		parts, err := CleanRel(s)
		if err != nil {
			return
		}
		// An accepted path rebuilds exactly and has no component that can
		// climb, name the current directory or switch to another volume.
		if strings.Join(parts, "/") != s {
			t.Fatalf("accepted %q but rebuilt it as %q", s, strings.Join(parts, "/"))
		}
		for _, p := range parts {
			if p == "" || p == "." || p == ".." || strings.ContainsAny(p, "\\:\x00") {
				t.Fatalf("accepted %q with component %q", s, p)
			}
		}
	})
}

func TestDirReadFile(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	write(t, filepath.Join(root, "a", "b", "f"), "data")
	write(t, filepath.Join(outside, "secret"), "secret")
	d, err := OpenDir(root)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if got, err := d.ReadFile("a/b/f", 10); err != nil || string(got) != "data" {
		t.Fatalf("plain file: %q %v", got, err)
	}
	if _, err := d.ReadFile("a/b/f", 3); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("too large: %v", err)
	}
	if _, err := d.ReadFile("a/b/none", 10); !errors.Is(err, fs.ErrNotExist) || Unusable(err) {
		t.Fatalf("absent: %v", err)
	}
	if _, err := d.ReadFile("nothing/at/all", 10); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("absent directory: %v", err)
	}
	// A directory where a file is wanted, and a file where a directory is.
	if _, err := d.ReadFile("a/b", 10); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("directory in place of a file: %v", err)
	}
	if _, err := d.ReadFile("a/b/f/g", 10); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("file in place of a directory: %v", err)
	}
	if _, err := d.ReadFile("../x", 10); err == nil {
		t.Fatal("a path with .. must be refused")
	}
}

func TestDirRefusesLinks(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	write(t, filepath.Join(root, "real", "f"), "inside")
	write(t, filepath.Join(outside, "secret"), "secret")
	symlink(t, filepath.Join(outside, "secret"), filepath.Join(root, "outfile"))
	symlink(t, outside, filepath.Join(root, "outdir"))
	symlink(t, filepath.Join(root, "real", "f"), filepath.Join(root, "infile"))
	symlink(t, filepath.Join(root, "real"), filepath.Join(root, "indir"))
	d, err := OpenDir(root)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, p := range []string{"outfile", "outdir/secret", "infile", "indir/f"} {
		_, err := d.ReadFile(p, 100)
		if !errors.Is(err, ErrLink) || !Unusable(err) {
			t.Errorf("%s: want a refused link, got %v", p, err)
		}
	}
	// A dangling link is a link too, not an absent file.
	symlink(t, filepath.Join(outside, "gone"), filepath.Join(root, "dangling"))
	if _, err := d.ReadFile("dangling", 100); !errors.Is(err, ErrLink) {
		t.Errorf("dangling link: %v", err)
	}
}

func TestOpenDirOfLinkAndOfFile(t *testing.T) {
	base := t.TempDir()
	write(t, filepath.Join(base, "real", "f"), "x")
	write(t, filepath.Join(base, "plain"), "x")
	symlink(t, filepath.Join(base, "real"), filepath.Join(base, "link"))
	// The auditor may point at a link to the directory.
	d, err := OpenDir(filepath.Join(base, "link"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := d.ReadFile("f", 10); err != nil || string(got) != "x" {
		t.Fatalf("%q %v", got, err)
	}
	d.Close()
	if _, err := OpenDir(filepath.Join(base, "plain")); err == nil {
		t.Fatal("a file is not a source directory")
	}
	if _, err := OpenDir(filepath.Join(base, "absent")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("absent source: %v", err)
	}
}

func TestSubDirs(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "c", "id", "e1", "x"), "x")
	write(t, filepath.Join(root, "c", "id", "afile"), "x")
	symlink(t, root, filepath.Join(root, "c", "id", "e2"))
	d, err := OpenDir(root)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	dirs, others, err := d.SubDirs("c/id")
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 1 || dirs[0] != "e1" || len(others) != 1 || others[0] != "e2" {
		t.Fatalf("dirs %v others %v", dirs, others)
	}
	if _, _, err := d.SubDirs("c/none"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("absent directory: %v", err)
	}
	within(t, func() {})
}

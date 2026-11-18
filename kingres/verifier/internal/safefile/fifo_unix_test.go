// SPDX-License-Identifier: AGPL-3.0-only

//go:build unix

package safefile

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
)

func mkfifo(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Skipf("cannot make a named pipe here: %v", err)
	}
}

func TestReadArgNamedPipe(t *testing.T) {
	p := filepath.Join(testdir.New(t), "pipe")
	mkfifo(t, p)
	within(t, func() {
		if _, err := ReadArg(p, 100); !errors.Is(err, ErrNotRegular) {
			t.Errorf("named pipe: %v", err)
		}
	})
}

func TestDirNamedPipe(t *testing.T) {
	root := testdir.New(t)
	mkfifo(t, filepath.Join(root, "c", "id", "e1", "checkpoint"))
	mkfifo(t, filepath.Join(root, "tile"))
	d, err := OpenDir(root)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	within(t, func() {
		for _, p := range []string{"c/id/e1/checkpoint", "tile"} {
			if _, err := d.ReadFile(p, 100); !errors.Is(err, ErrNotRegular) {
				t.Errorf("%s: %v", p, err)
			}
		}
	})
}

func TestNamedPipeAsSourceDirectory(t *testing.T) {
	p := filepath.Join(testdir.New(t), "pipe")
	mkfifo(t, p)
	within(t, func() {
		if d, err := OpenDir(p); err == nil {
			d.Close()
			t.Error("a named pipe is not a source directory")
		}
	})
}

func TestDevicesAreRefused(t *testing.T) {
	if _, err := os.Stat("/dev/zero"); err != nil {
		t.Skip("no /dev/zero")
	}
	within(t, func() {
		if _, err := ReadArg("/dev/zero", 100); !errors.Is(err, ErrNotRegular) {
			t.Errorf("/dev/zero: %v", err)
		}
	})
}

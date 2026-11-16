// SPDX-License-Identifier: AGPL-3.0-only

//go:build windows

package safefile

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A junction needs no privilege, unlike a symbolic link, so this is the link
// test that runs on every Windows machine.
func TestJunctionInASourceIsRefused(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	write(t, filepath.Join(outside, "secret"), "secret")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(root, "jct"), outside).CombinedOutput(); err != nil {
		t.Skipf("cannot make a junction: %v %s", err, out)
	}
	d, err := OpenDir(root)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	within(t, func() {
		if _, err := d.ReadFile("jct/secret", 100); !errors.Is(err, ErrLink) {
			t.Errorf("junction out of the root: %v", err)
		}
	})
}

func TestReservedNamesAreNotOpened(t *testing.T) {
	within(t, func() {
		for _, p := range []string{"NUL", "CON", `C:\Windows\System32\drivers\etc\hosts:stream`, `\\.\pipe\none`, `\\.\C:`} {
			if _, err := ReadArg(p, 100); !errors.Is(err, ErrNotRegular) {
				t.Errorf("%s: %v", p, err)
			}
		}
	})
	// A normal path still works.
	f := filepath.Join(t.TempDir(), "ok.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadArg(f, 10); err != nil || string(got) != "x" {
		t.Fatalf("%q %v", got, err)
	}
}

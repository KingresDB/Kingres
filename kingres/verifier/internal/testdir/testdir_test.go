// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package testdir

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewMakesAnEmptyDirectoryThatIsRemovedWithItsContent(t *testing.T) {
	var dir string
	t.Run("inner", func(t *testing.T) {
		dir = New(t)
		if ents, err := os.ReadDir(dir); err != nil || len(ents) != 0 {
			t.Fatalf("the directory is not empty: %v %v", ents, err)
		}
		if err := os.MkdirAll(filepath.Join(dir, "a", "b"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "a", "b", "f"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the directory is still there after its test: %v", err)
	}
}

func TestTwoDirectoriesAreTwoDirectories(t *testing.T) {
	if a, b := New(t), New(t); a == b {
		t.Fatal("the same directory twice")
	}
}

// The wait that New uses on Windows is the one the review asked for.
func TestTheDefaultWaitIsAtLeastTenSeconds(t *testing.T) {
	if Wait < 10*time.Second {
		t.Fatalf("Wait is %v", Wait)
	}
}

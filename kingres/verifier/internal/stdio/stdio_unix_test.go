// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package stdio

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
)

func TestSubstitutedIsTheNullDeviceOpenedForReadingAndWriting(t *testing.T) {
	open := func(flag int) *os.File {
		f, err := os.OpenFile(os.DevNull, flag, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		return f
	}
	if !Substituted(open(os.O_RDWR)) {
		t.Error("the null device opened for reading and writing is what the runtime puts in place of a closed descriptor")
	}
	if Substituted(open(os.O_WRONLY)) {
		t.Error("the null device opened for writing is the user's request for no output")
	}
	if Substituted(open(os.O_WRONLY | os.O_APPEND)) {
		t.Error("the null device opened for appending is the user's request for no output")
	}
	if Substituted(open(os.O_RDONLY)) {
		t.Error("a descriptor opened for reading only is not a standard output the runtime made")
	}
	f, err := os.OpenFile(filepath.Join(testdir.New(t), "out"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if Substituted(f) {
		t.Error("a regular file is a real output")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if Substituted(w) || Substituted(r) {
		t.Error("a pipe is a real output")
	}
	if Substituted(nil) {
		t.Error("no file is not the null device")
	}
}

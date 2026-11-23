// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package staging

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func keyDir(t *testing.T) (dir, keyPath, staging string) {
	t.Helper()
	dir = t.TempDir()
	return dir, filepath.Join(dir, "transfer.key"), filepath.Join(dir, "transfer")
}

func TestKeyCreatedOnceAndReloaded(t *testing.T) {
	_, kp, sd := keyDir(t)
	none := func() (bool, error) { return false, nil }
	k1, err := LoadOrCreateKeys(OSFS{}, kp, sd, none)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(kp)
	if err != nil || info.Size() != int64(keyFileSize) {
		t.Fatalf("key file: %v %v", info, err)
	}
	if modesEnforced && info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}
	k2, err := LoadOrCreateKeys(OSFS{}, kp, sd, func() (bool, error) { t.Fatal("asked although the key exists"); return false, nil })
	if err != nil || k1.ID() != k2.ID() || len(k1.ID()) != 16 {
		t.Fatalf("reload: %v %q %q", err, k1.ID(), k2.ID())
	}
	_, wrapped, _ := k1.NewFileKey(5)
	if _, err := k2.Unwrap(5, k1.ID(), wrapped); err != nil {
		t.Fatalf("a key read back must open what the first one wrapped: %v", err)
	}
	if left := tmpLeftovers(t, filepath.Dir(kp)); len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
}

func tmpLeftovers(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestTwoFirstUsesAtOnceMakeOneKey(t *testing.T) {
	for round := 0; round < 20; round++ {
		_, kp, sd := keyDir(t)
		const n = 16
		ids := make([]string, n)
		errs := make([]error, n)
		var start, wg sync.WaitGroup
		start.Add(1)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				start.Wait()
				k, err := LoadOrCreateKeys(OSFS{}, kp, sd, func() (bool, error) { return false, nil })
				errs[i] = err
				if err == nil {
					ids[i] = k.ID()
				}
			}(i)
		}
		start.Done()
		wg.Wait()
		for i := range ids {
			if errs[i] != nil {
				t.Fatalf("round %d: use %d failed: %v", round, i, errs[i])
			}
			if ids[i] != ids[0] {
				t.Fatalf("round %d: two keys were made: %s and %s", round, ids[0], ids[i])
			}
		}
		if left := tmpLeftovers(t, filepath.Dir(kp)); len(left) != 0 {
			t.Fatalf("temporary files left: %v", left)
		}
	}
}

// The loser of the race is forced: the winner's file appears between the check and the link.
func TestLoserOfTheKeyRaceReadsTheWinnersKey(t *testing.T) {
	_, kp, sd := keyDir(t)
	ffs := newFaultFS()
	var winner *Keys
	ffs.beforeLink = func() {
		ffs.beforeLink = nil
		var err error
		winner, err = LoadOrCreateKeys(OSFS{}, kp, sd, nil) // another process finishes first
		if err != nil {
			t.Error(err)
		}
	}
	k, err := LoadOrCreateKeys(ffs, kp, sd, func() (bool, error) { return false, nil })
	if err != nil {
		t.Fatalf("the loser must succeed with the winner's key: %v", err)
	}
	if winner == nil || k.ID() != winner.ID() {
		t.Fatalf("the loser returned its own key %s instead of %v", k.ID(), winner)
	}
	if left := tmpLeftovers(t, filepath.Dir(kp)); len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
}

func TestMissingKeyWithFilesUnderItIsLostAndNothingIsCreated(t *testing.T) {
	_, kp, sd := keyDir(t)
	_, err := LoadOrCreateKeys(OSFS{}, kp, sd, func() (bool, error) { return true, nil })
	if !errors.Is(err, ErrKeyLost) {
		t.Fatalf("err = %v, want ErrKeyLost", err)
	}
	if _, err := os.Stat(kp); err == nil {
		t.Fatal("a new key was created while files under the old one exist")
	}
	if left := tmpLeftovers(t, filepath.Dir(kp)); len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
	boom := errors.New("system database down")
	if _, err := LoadOrCreateKeys(OSFS{}, kp, sd, func() (bool, error) { return false, boom }); !errors.Is(err, boom) {
		t.Fatalf("an error of the question must stop the creation: %v", err)
	}
	if _, err := os.Stat(kp); err == nil {
		t.Fatal("a key was created although the question failed")
	}
}

func TestReplacedKeyFileLosesTheOldFiles(t *testing.T) {
	_, kp, sd := keyDir(t)
	none := func() (bool, error) { return false, nil }
	old, _ := LoadOrCreateKeys(OSFS{}, kp, sd, none)
	_, wrapped, _ := old.NewFileKey(7)
	if err := os.Remove(kp); err != nil {
		t.Fatal(err)
	}
	replacement, err := LoadOrCreateKeys(OSFS{}, kp, sd, none) // an operator who deleted it and the rows were not asked
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replacement.Unwrap(7, old.ID(), wrapped); !errors.Is(err, ErrKeyLost) {
		t.Fatalf("a replaced key must report the files as lost: %v", err)
	}
}

func TestKeyFileMustBeRegularStrictAndOutsideStaging(t *testing.T) {
	none := func() (bool, error) { return false, nil }
	_, kp, sd := keyDir(t)
	good, err := LoadOrCreateKeys(OSFS{}, kp, sd, none)
	if err != nil {
		t.Fatal(err)
	}
	raw := mustRead(t, kp)
	_ = good

	// Damage: each of these is refused, and the file is not replaced.
	cases := map[string][]byte{
		"checksum":  append(append([]byte(nil), raw[:len(raw)-1]...), raw[len(raw)-1]^1),
		"key byte":  func() []byte { b := append([]byte(nil), raw...); b[20] ^= 1; return b }(),
		"magic":     func() []byte { b := append([]byte(nil), raw...); b[0] = 'X'; return b }(),
		"truncated": raw[:len(raw)-1],
		"extra":     append(append([]byte(nil), raw...), 0),
		"empty":     {},
	}
	for name, content := range cases {
		mustWrite(t, kp, content)
		if _, err := LoadOrCreateKeys(OSFS{}, kp, sd, none); !errors.Is(err, ErrKeyFile) {
			t.Errorf("%s: err = %v, want ErrKeyFile", name, err)
		}
		if got := mustRead(t, kp); string(got) != string(content) {
			t.Errorf("%s: a damaged key file was replaced", name)
		}
	}
	mustWrite(t, kp, raw)

	// A directory in place of the file.
	_, kp2, sd2 := keyDir(t)
	if err := os.Mkdir(kp2, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateKeys(OSFS{}, kp2, sd2, none); !errors.Is(err, ErrKeyFile) {
		t.Errorf("a directory: %v", err)
	}

	// Inside the staging directory, however it is spelled.
	for _, p := range []string{filepath.Join(sd, "transfer.key"), filepath.Join(sd, "sub", "..", "k"), filepath.Join(sd, "deep", "er", "k")} {
		if _, err := LoadOrCreateKeys(OSFS{}, p, sd, none); !errors.Is(err, ErrKeyFile) {
			t.Errorf("%s: err = %v, want ErrKeyFile", p, err)
		}
	}
	if _, err := LoadOrCreateKeys(OSFS{}, "relative.key", sd, none); !errors.Is(err, ErrKeyFile) {
		t.Errorf("relative path: %v", err)
	}
}

func TestKeyFileModeAndOwnerAreCheckedAtEveryStart(t *testing.T) {
	unixOnly(t)
	none := func() (bool, error) { return false, nil }
	_, kp, sd := keyDir(t)
	if _, err := LoadOrCreateKeys(OSFS{}, kp, sd, none); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644, 0o660, 0o666, 0o700 | 0o070} {
		if err := os.Chmod(kp, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadOrCreateKeys(OSFS{}, kp, sd, none); !errors.Is(err, ErrDirMode) {
			t.Errorf("mode %v: err = %v, want ErrDirMode", mode, err)
		}
	}
	if err := os.Chmod(kp, 0o400); err != nil { // tighter is fine
		t.Fatal(err)
	}
	if _, err := LoadOrCreateKeys(OSFS{}, kp, sd, none); err != nil {
		t.Errorf("mode 0400: %v", err)
	}
}

func TestKeyFileThroughASymbolicLinkIsRefused(t *testing.T) {
	none := func() (bool, error) { return false, nil }
	dir, kp, sd := keyDir(t)
	real := filepath.Join(dir, "real.key")
	if _, err := LoadOrCreateKeys(OSFS{}, real, sd, none); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, kp); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
	if _, err := LoadOrCreateKeys(OSFS{}, kp, sd, none); !errors.Is(err, ErrKeyFile) {
		t.Fatalf("a link in place of the key: %v", err)
	}
}

func TestDataKeysAreRandomAndNeverStoredPlain(t *testing.T) {
	k := NewKeysForTest(3)
	d1, w1, _ := k.NewFileKey(1)
	d2, w2, _ := k.NewFileKey(1)
	if string(d1) == string(d2) || string(w1) == string(w2) {
		t.Fatal("two file keys are equal")
	}
	if strings.Contains(string(w1), string(d1)) {
		t.Fatal("the wrapped key holds the data key in the clear")
	}
	got, err := k.Unwrap(1, k.ID(), w1)
	if err != nil || string(got) != string(d1) {
		t.Fatalf("unwrap: %v", err)
	}
	if _, err := k.Unwrap(2, k.ID(), w1); !errors.Is(err, ErrKeyLost) {
		t.Fatalf("a wrapped key moved to another transfer: %v", err)
	}
}

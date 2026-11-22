// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package staging

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A growth that lands exactly on the quota is allowed; one byte more is not.
func TestGrowthExactlyToTheQuota(t *testing.T) {
	s, _ := testStore(t, func(c *Config) { c.Quota = 1000 })
	a, _ := s.Reserve(1, "a", 100)
	if _, err := s.Reserve(2, "b", 100); err != nil {
		t.Fatal(err)
	}
	if err := a.Use(100); err != nil {
		t.Fatal(err)
	}
	if err := a.Use(800); err != nil { // reserved becomes 900, the sum 1000
		t.Fatalf("growth to exactly the quota: %v", err)
	}
	if err := a.Use(1); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("one byte past the quota: %v", err)
	}
}

// The additional data and the nonce carry what the design says: transfer id, segment number, last flag; prefix and counter.
func TestAdditionalDataAndNonceFields(t *testing.T) {
	base := aadFor(7, 3, false)
	if len(base) != 17 {
		t.Fatalf("aad length %d", len(base))
	}
	for name, other := range map[string][]byte{"id": aadFor(8, 3, false), "segment": aadFor(7, 4, false), "last flag": aadFor(7, 3, true)} {
		if bytes.Equal(base, other) {
			t.Errorf("the additional data does not depend on the %s", name)
		}
	}
	n := nonceFor([4]byte{1, 2, 3, 4}, 5)
	if len(n) != nonceSize || !bytes.Equal(n[:4], []byte{1, 2, 3, 4}) || bytes.Equal(n, nonceFor([4]byte{1, 2, 3, 4}, 6)) {
		t.Fatalf("nonce = %x", n)
	}
}

// Create uses O_EXCL: a .part file that appears between the check and the creation is not overwritten.
func TestCreateDoesNotOverwriteAFileThatAppearsFirst(t *testing.T) {
	s, ffs := testStore(t, nil)
	p := filepath.Join(s.Dir(), PartName(1))
	ffs.beforeOpen = func(name string, flag int) {
		if name == p && flag&os.O_CREATE != 0 {
			ffs.beforeOpen = nil
			mustWrite(t, p, []byte("somebody else's file"))
		}
	}
	res := mustRes(t, s, 1)
	if _, _, err := s.Create(1, res, testKeys); err == nil {
		t.Fatal("Create over a file that appeared first succeeded")
	}
	if string(mustRead(t, p)) != "somebody else's file" {
		t.Fatal("the other file was overwritten")
	}
	// The failed Create must not leave the id marked as being written.
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Create(1, res, testKeys); err != nil {
		t.Fatalf("Create after the failure: %v", err)
	}
}

// If the key file is replaced after our link, the creation must not return a key that is not on the disk any more.
func TestKeyReplacedRightAfterTheLinkIsAnError(t *testing.T) {
	_, kp, sd := keyDir(t)
	ffs := newFaultFS()
	ffs.afterLink = func() {
		ffs.afterLink = nil
		_ = os.Remove(kp)
		if _, err := LoadOrCreateKeys(OSFS{}, kp, sd, nil); err != nil {
			t.Error(err)
		}
	}
	if _, err := LoadOrCreateKeys(ffs, kp, sd, func() (bool, error) { return false, nil }); !errors.Is(err, ErrKeyFile) {
		t.Fatalf("err = %v, want ErrKeyFile", err)
	}
	if left := tmpLeftovers(t, filepath.Dir(kp)); len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
}

// Nothing a caller supplies as a name can reach a path: the only inputs are the ids.
func TestPathsStayInsideTheDirectory(t *testing.T) {
	s, _ := testStore(t, nil)
	for _, id := range []int64{0, 1, 1 << 40, 999999999999999999} {
		for _, n := range []string{PartName(id), FinalName(id)} {
			if p := s.path(n); filepath.Dir(p) != s.Dir() || strings.Contains(n, string(filepath.Separator)) {
				t.Errorf("%s leaves the directory: %s", n, p)
			}
		}
	}
}

func TestSetQuotaAppliesToTheNextReservation(t *testing.T) {
	s, _ := testStore(t, nil)
	r1, err := s.Reserve(1, "a", 1000)
	if err != nil {
		t.Fatal(err)
	}
	s.SetQuota(1500)
	if _, err := s.Reserve(2, "a", 600); !errors.Is(err, ErrQuotaExceeded) {
		t.Errorf("a reservation above the new quota: %v", err)
	}
	if _, err := s.Reserve(3, "a", 500); err != nil {
		t.Errorf("a reservation within the new quota: %v", err)
	}
	r1.Release()
}

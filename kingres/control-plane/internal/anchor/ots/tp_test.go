// SPDX-License-Identifier: AGPL-3.0-only

package ots

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// thirdPartyDir holds files fetched from another project (see
// testdata/SOURCES.txt). It is absent in a plain checkout; the tests that
// need it skip, every other test runs without it.
const thirdPartyDir = "testdata/third_party"

func needThirdParty(t testing.TB) {
	t.Helper()
	if st, err := os.Stat(thirdPartyDir); err != nil || !st.IsDir() {
		t.Skipf("%s is absent: run testdata/fetch-third-party.sh to fetch it", thirdPartyDir)
	}
}

// TestThirdPartyManifest checks the fetched files against
// testdata/third_party.sha256: every file listed must have its sum, and no
// file may be present that is not listed.
func TestThirdPartyManifest(t *testing.T) {
	needThirdParty(t)
	f, err := os.Open("testdata/third_party.sha256")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	want := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		sum, path, ok := strings.Cut(sc.Text(), "  ")
		if !ok || len(sum) != 64 {
			t.Fatalf("bad manifest line %q", sc.Text())
		}
		want[path] = sum
	}
	seen := map[string]bool{}
	err = filepath.WalkDir(thirdPartyDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(thirdPartyDir, p)
		rel = filepath.ToSlash(rel)
		seen[rel] = true
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		s := sha256.Sum256(b)
		if w, ok := want[rel]; !ok {
			t.Errorf("%s is not in the manifest", rel)
		} else if w != hex.EncodeToString(s[:]) {
			t.Errorf("%s has another sum than the manifest", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for p := range want {
		if !seen[p] {
			t.Errorf("%s is in the manifest but missing", p)
		}
	}
}

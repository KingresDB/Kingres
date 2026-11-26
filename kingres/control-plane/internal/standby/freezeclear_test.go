// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package standby

import (
	"os"
	"path/filepath"
	"testing"
)

// The local release of the hold of the map group lifts the freeze of its list on disk, and only that one.
func TestClearFreezeOnDiskLiftsOnlyTheFreezeOfTheGroup(t *testing.T) {
	dir := t.TempDir()
	if cleared, err := ClearFreezeOnDisk(dir, 0); err != nil || cleared {
		t.Fatalf("no file: %v %v", cleared, err)
	}
	body := `{"frozen":{"0":99999999999999,"3":99999999999999},"gone":{"s3":99999999999999},"owners":{"0":{"Task":16,"Epoch":3},"3":{"Task":9,"Epoch":1}}}`
	if err := os.WriteFile(filepath.Join(dir, freezeFileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if cleared, err := ClearFreezeOnDisk(dir, 0); err != nil || !cleared {
		t.Fatalf("a freeze of group 0: %v %v", cleared, err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, freezeFileName))
	got := string(b)
	if contains(got, `"0":99999999999999`) || !contains(got, `"3":99999999999999`) || !contains(got, `"s3"`) {
		t.Fatalf("the file after the release: %s", got)
	}
	if cleared, err := ClearFreezeOnDisk(dir, 0); err != nil || cleared {
		t.Fatalf("a second release: %v %v", cleared, err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

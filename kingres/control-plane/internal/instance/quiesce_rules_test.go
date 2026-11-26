// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package instance

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func heldDir(t *testing.T, real string) string {
	t.Helper()
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "pg_hba.conf"), []byte(HoldRules(real)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, hbaBackupName), []byte(real), 0o600); err != nil {
		t.Fatal(err)
	}
	return d
}

// F4-10. The rules to copy to the standbys of the same server are the rules that the held instance will have again, not the hold rules.
func TestTheRulesOfAHeldDirectoryAreTheBackup(t *testing.T) {
	real := "hostssl all app 0.0.0.0/0 scram-sha-256\nlocal all all peer\n"
	d := heldDir(t, real)
	got, err := rulesAt(d)
	if err != nil || string(got) != real {
		t.Fatalf("%q %v", got, err)
	}
	free := t.TempDir()
	if err := os.WriteFile(filepath.Join(free, "pg_hba.conf"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := rulesAt(free); string(got) != "x\n" {
		t.Fatalf("a directory that is not held: %q", got)
	}
}

// F4-10. Rules written while the directory is held go behind the hold: the backup is the new rules and the file still holds.
func TestRulesWrittenWhileHeldAreWrittenBehindTheHold(t *testing.T) {
	d := heldDir(t, "hostssl all app 0.0.0.0/0 scram-sha-256\nlocal all all peer\n")
	fresh := "hostssl all other 10.0.0.0/8 scram-sha-256\nlocal all all peer\n"
	if err := writeRulesAt(d, fresh); err != nil {
		t.Fatal(err)
	}
	backup, _ := os.ReadFile(filepath.Join(d, hbaBackupName))
	file, _ := os.ReadFile(filepath.Join(d, "pg_hba.conf"))
	if string(backup) != fresh || string(file) != HoldRules(fresh) || strings.Contains(string(file), "other") {
		t.Fatalf("backup %q file %q", backup, file)
	}
	free := t.TempDir()
	if err := writeRulesAt(free, fresh); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(free, "pg_hba.conf")); string(got) != fresh {
		t.Fatalf("a directory that is not held: %q", got)
	}
	if _, err := os.Stat(filepath.Join(free, hbaBackupName)); err == nil {
		t.Fatal("a backup was made for a directory that is not held")
	}
}

// F4-10 and F4-15. The hold of a directory that is not a primary any more is dropped.
func TestTheHoldOfAStandbyIsDropped(t *testing.T) {
	d := heldDir(t, "x\n")
	dropHold(d)
	if _, err := os.Stat(filepath.Join(d, hbaBackupName)); err == nil {
		t.Fatal("the backup stayed")
	}
}

// F4-11. The replication keyword is matched as a word, not as a part of a database name.
func TestHoldRulesMatchReplicationAsAWord(t *testing.T) {
	out := HoldRules(strings.Join([]string{
		"hostssl xreplicationx all 10.0.0.2/32 cert",
		"hostssl replicationdb all 10.0.0.3/32 cert",
		"hostssl app,replication all 10.0.0.4/32 cert",
		"hostssl replication all 10.0.0.5/32 cert",
	}, "\n"))
	for _, bad := range []string{"xreplicationx", "replicationdb"} {
		if strings.Contains(out, bad) {
			t.Errorf("the rule for %q was kept:\n%s", bad, out)
		}
	}
	for _, good := range []string{"10.0.0.4/32", "10.0.0.5/32"} {
		if !strings.Contains(out, good) {
			t.Errorf("the rule for %s was dropped:\n%s", good, out)
		}
	}
}

// F4-15. Unquiesce of an instance that is a standby now (the old primary of a switchover, rebuilt) does not put the rules of the primary that
// was held over the rules of the rebuild; it only drops the backup. A primary gets its rules back.
func TestUnquiesceDoesNotRestoreOverAStandby(t *testing.T) {
	m := accessRig(t, 1, 101)
	ctx := context.Background()
	for _, id := range []int{1, 101} {
		data, _ := m.DataDir(id)
		if err := os.WriteFile(filepath.Join(data, "pg_hba.conf"), []byte("current rules\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(data, hbaBackupName), []byte("rules of the held primary\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Unquiesce(ctx, 101); err != nil { // 101 is a standby by the default of its id
		t.Fatal(err)
	}
	d101, _ := m.DataDir(101)
	if got, _ := os.ReadFile(filepath.Join(d101, "pg_hba.conf")); string(got) != "current rules\n" || m.Quiesced(101) {
		t.Fatalf("a standby: %q, held %v", got, m.Quiesced(101))
	}
	if err := m.Unquiesce(ctx, 1); err != nil {
		t.Fatal(err)
	}
	d1, _ := m.DataDir(1)
	if got, _ := os.ReadFile(filepath.Join(d1, "pg_hba.conf")); string(got) != "rules of the held primary\n" || m.Quiesced(1) {
		t.Fatalf("a primary: %q, held %v", got, m.Quiesced(1))
	}
}

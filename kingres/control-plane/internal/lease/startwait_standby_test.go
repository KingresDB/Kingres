// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package lease

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func prepDir(t *testing.T, groups []int32, enforced []int32) string {
	t.Helper()
	dir := tempDir(t)
	b, _ := json.Marshal(Topology{Groups: groups, Members: []string{"a", "b", "c"}})
	if err := os.WriteFile(filepath.Join(dir, topologyFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if enforced != nil {
		b, _ = json.Marshal(enforcedRecord{Groups: enforced})
		if err := os.WriteFile(filepath.Join(dir, enforcedFile), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func waits(dir string, g int32) bool { return waitMarkerExists(dir, g) }

// The record of the lease manager is written only after a round that left a lease, so it can lack a group whose standby became ready since. The standby list
// on disk then makes the group wait for its lease at the start: the primary is not started without one (it would serve until the first round ended, while its
// standby may have been promoted), and the wait shows as the wait marker and the return value that makes kingresd log it.
func TestAReadyStandbyOnDiskMakesAnUnrecordedGroupWaitForItsLease(t *testing.T) {
	dir := prepDir(t, []int32{1, 2}, []int32{})
	standby := func(g int32) bool { return g == 1 }
	w, err := PrepareStart(dir, func(int32) bool { return false }, standby)
	if err != nil || !w {
		t.Fatalf("wait %v err %v: the group with a ready standby must wait", w, err)
	}
	if !waits(dir, 1) || waits(dir, 2) {
		t.Fatalf("markers: group 1 %v (want true), group 2 %v (want false)", waits(dir, 1), waits(dir, 2))
	}
	if err := Gate(dir, testPrimary)(1); err == nil {
		t.Fatal("the start gate must refuse the primary of group 1 while it waits for its lease")
	}
}

// No standby anywhere (the record lacks the group and the list names none, or there is no list): nothing is promotable, nothing waits, as before.
func TestNoStandbyOnDiskAndNoRecordMeansNoWait(t *testing.T) {
	for name, fn := range map[string]func(int32) bool{"no list function": nil, "an empty list": func(int32) bool { return false }} {
		dir := prepDir(t, []int32{1}, []int32{})
		w, err := PrepareStart(dir, func(int32) bool { return false }, fn)
		if err != nil || w || waits(dir, 1) {
			t.Errorf("%s: wait %v marker %v err %v", name, w, waits(dir, 1), err)
		}
	}
}

// The list only adds a wait: a group the record names still waits when the list says nothing (a standby removed while the server was down is judged by the
// record, as before), and an instance that runs is left alone whatever the list says.
func TestTheListNeverRemovesAWait(t *testing.T) {
	dir := prepDir(t, []int32{1}, []int32{1})
	w, err := PrepareStart(dir, func(int32) bool { return false }, func(int32) bool { return false })
	if err != nil || !w || !waits(dir, 1) {
		t.Fatalf("a recorded group: wait %v marker %v err %v", w, waits(dir, 1), err)
	}
	dir = prepDir(t, []int32{1}, []int32{})
	w, err = PrepareStart(dir, func(int32) bool { return true }, func(int32) bool { return true })
	if err != nil || w || waits(dir, 1) {
		t.Fatalf("a running instance: wait %v marker %v err %v", w, waits(dir, 1), err)
	}
}

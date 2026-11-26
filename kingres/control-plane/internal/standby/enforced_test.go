// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package standby

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// The cold start judges a group by the list on disk with the very predicate the run time fence uses: for every shape of an entry the two answers are the
// same, so that "this primary waits for its lease" and "this primary is fenced without one" cannot disagree.
func TestTheColdStartAndTheFenceJudgeTheListAlike(t *testing.T) {
	dir := t.TempDir()
	a, err := NewAgent(&Agent{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	e := func(id int64, g int32, ready bool, gen int64) Entry {
		return Entry{ID: id, Group: g, Server: "b", PrimaryServer: "a", PrimaryPort: 5000, Ready: ready, Generation: gen}
	}
	plan := []Entry{e(1, 1, true, 0), e(2, 2, false, 0), e(3, 3, false, 1), e(4, 4, true, 2)}
	if err := a.Plan(context.Background(), PlanRequest{Version: 1, Standbys: plan}); err != nil {
		t.Fatal(err)
	}
	disk, err := EnforcedOnDisk(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int32]bool{0: false, 1: true, 2: false, 3: true, 4: true, 5: false}
	for g, w := range want {
		if got := disk(g); got != w || a.HasStandby(g) != w {
			t.Errorf("group %d: disk %v, agent %v, want %v", g, got, a.HasStandby(g), w)
		}
	}
}

// A list that was never written is no standby; one that cannot be read or parsed is judged as enforced for every group and reported.
func TestEnforcedOnDiskWithoutAReadableList(t *testing.T) {
	dir := t.TempDir()
	fn, err := EnforcedOnDisk(dir)
	if err != nil || fn(1) || fn(0) {
		t.Fatalf("no file: err %v, enforced %v %v (want none)", err, fn(0), fn(1))
	}
	if err := os.MkdirAll(filepath.Join(dir, "standby"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "standby", stateFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	fn, err = EnforcedOnDisk(dir)
	if err == nil || !fn(1) || !fn(7) {
		t.Fatalf("a corrupt file: err %v, enforced %v %v (want an error and every group enforced)", err, fn(1), fn(7))
	}
}

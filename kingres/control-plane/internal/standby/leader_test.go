// SPDX-License-Identifier: AGPL-3.0-only

package standby

import (
	"context"
	"reflect"
	"testing"
)

func testTopology() topology {
	return topology{
		ring:   []Server{{ID: "a"}, {ID: "b"}, {ID: "c"}},
		groups: []groupInfo{{ID: 0, Primary: "a", Port: 5000}, {ID: 1, Primary: "b", Port: 5001}, {ID: 2, Primary: "c", Port: 5002}},
	}
}

func servers(es []Entry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Server)
	}
	return out
}

func TestDiffCreatesOneStandbyPerGroupOnTheNextServer(t *testing.T) {
	l := NewLeader(&Leader{})
	w := l.diff(testTopology(), nil, 1)
	if len(w.Add) != 3 || len(w.Remove)+len(w.Resume)+len(w.Rebuild) != 0 {
		t.Fatalf("%+v", w)
	}
	got := map[int32]string{}
	for _, e := range w.Add {
		got[e.Group] = e.Server
		if e.PrimaryPort == 0 || e.PrimaryServer == "" {
			t.Errorf("an entry without its primary: %+v", e)
		}
	}
	if !reflect.DeepEqual(got, map[int32]string{0: "b", 1: "c", 2: "a"}) {
		t.Fatalf("placement %v", got)
	}
}

func TestDiffIsQuietWhenTheClusterMatches(t *testing.T) {
	l := NewLeader(&Leader{})
	recs := []Record{
		{ID: 1, Group: 0, Server: "b", PrimaryServer: "a", State: stateReady},
		{ID: 2, Group: 1, Server: "c", PrimaryServer: "b", State: stateReady},
		{ID: 3, Group: 2, Server: "a", PrimaryServer: "c", State: stateReady},
	}
	if w := l.diff(testTopology(), recs, 1); !w.empty() {
		t.Fatalf("nothing to do, got %+v", w)
	}
}

func TestDiffResumesRebuildsAndRemoves(t *testing.T) {
	l := NewLeader(&Leader{})
	recs := []Record{
		{ID: 1, Group: 0, Server: "b", PrimaryServer: "a", State: stateCreating},
		{ID: 2, Group: 1, Server: "c", PrimaryServer: "b", State: stateReady, NeedsRebuild: true},
		{ID: 3, Group: 2, Server: "a", PrimaryServer: "c", State: stateRemoving},
		{ID: 4, Group: 9, Server: "a", PrimaryServer: "z", State: stateReady}, // its group left the catalog
	}
	w := l.diff(testTopology(), recs, 1)
	if len(w.Resume) != 1 || w.Resume[0].ID != 1 {
		t.Errorf("resume: %+v", w.Resume)
	}
	if len(w.Rebuild) != 1 || w.Rebuild[0].ID != 2 {
		t.Errorf("rebuild: %+v", w.Rebuild)
	}
	ids := map[int64]bool{}
	for _, e := range w.Remove {
		ids[e.ID] = true
	}
	if !ids[3] || !ids[4] || len(ids) != 2 {
		t.Errorf("remove: %+v", w.Remove)
	}
	// Group 2 lost its standby on a: it is created again on a (the record was being removed).
	if !reflect.DeepEqual(servers(w.Add), []string{"a"}) {
		t.Errorf("add: %+v", w.Add)
	}
}

func TestDiffLowersTheReplicaCount(t *testing.T) {
	l := NewLeader(&Leader{})
	recs := []Record{
		{ID: 1, Group: 0, Server: "b", PrimaryServer: "a", State: stateReady},
		{ID: 2, Group: 0, Server: "c", PrimaryServer: "a", State: stateReady},
	}
	w := l.diff(topology{ring: testTopology().ring, groups: testTopology().groups[:1]}, recs, 1)
	if len(w.Remove) != 1 || w.Remove[0].Server != "c" || len(w.Add) != 0 {
		t.Fatalf("the least preferred standby goes: %+v", w)
	}
	w = l.diff(topology{ring: testTopology().ring, groups: testTopology().groups[:1]}, recs, 0)
	if len(w.Remove) != 2 {
		t.Fatalf("zero removes all: %+v", w)
	}
}

func TestLocalStatePlanVersionsAndLostSlots(t *testing.T) {
	dir := t.TempDir()
	l, err := openLocalState(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := Entry{ID: 7, Group: 1, Server: "b", PrimaryServer: "a", PrimaryPort: 5000, Ready: true}
	if ok, err := l.replace(PlanRequest{Version: 5, Replicas: 1, Standbys: []Entry{e}}); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	if ok, _ := l.replace(PlanRequest{Version: 4}); ok {
		t.Fatal("an older plan must not replace a newer one")
	}
	if err := l.markDropped(e); err != nil {
		t.Fatal(err)
	}
	// Reopened from disk (a restart of kingresd): the list and the mark are still there.
	l2, err := openLocalState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s := l2.snapshot(); s.Version != 5 || len(s.Standbys) != 1 || s.Dropped[7] != 0 {
		t.Fatalf("%+v", s)
	}
	// The same generation keeps the mark; a rebuilt standby (higher generation) clears it.
	e2 := e
	if _, err := l2.replace(PlanRequest{Version: 6, Standbys: []Entry{e2}}); err != nil {
		t.Fatal(err)
	}
	if _, kept := l2.snapshot().Dropped[7]; !kept {
		t.Fatal("the mark must stay while the generation is the same")
	}
	e2.Generation = 1
	if _, err := l2.replace(PlanRequest{Version: 7, Standbys: []Entry{e2}}); err != nil {
		t.Fatal(err)
	}
	if _, kept := l2.snapshot().Dropped[7]; kept {
		t.Fatal("a rebuilt standby clears the mark")
	}
}

func TestAgentAnswersFromItsList(t *testing.T) {
	a, err := NewAgent(&Agent{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if a.HasStandby(1) {
		t.Fatal("no standby before a plan")
	}
	if err := a.Plan(context.Background(), PlanRequest{Version: 1, Standbys: []Entry{{ID: 3, Group: 1, Server: "b", PrimaryServer: "a", PrimaryPort: 5000}}}); err != nil {
		t.Fatal(err)
	}
	if !a.HasStandby(1) || a.HasStandby(0) {
		t.Fatal("HasStandby follows the list")
	}
	list, _ := a.List(context.Background())
	if len(list) != 1 || list[0].State != "starting" || list[0].ServerID != "b" {
		t.Fatalf("%+v", list)
	}
	if err := a.Plan(context.Background(), PlanRequest{Version: 2, Standbys: []Entry{{ID: 0}}}); err == nil {
		t.Fatal("an invalid entry must be refused")
	}
}

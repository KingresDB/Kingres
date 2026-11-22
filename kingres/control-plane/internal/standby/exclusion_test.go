// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package standby

import (
	"reflect"
	"testing"
)

func ids(r []Server) []string {
	out := []string{}
	for _, s := range r {
		out = append(out, s.ID)
	}
	return out
}

func TestWithoutServersKeepsTheOrder(t *testing.T) {
	ring := []Server{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}}
	if got := ids(WithoutServers(ring, map[string]bool{"b": true})); !reflect.DeepEqual(got, []string{"a", "c", "d"}) {
		t.Fatalf("%v", got)
	}
	if got := WithoutServers(ring, nil); len(got) != 4 {
		t.Fatal("nothing excluded")
	}
	if got := WithoutServers(ring, map[string]bool{"zzz": true}); len(got) != 4 {
		t.Fatal("an unknown server changes nothing")
	}
}

// A drained server is no candidate for any group, and the standbys it hosts are removed and replaced elsewhere: the plan of the
// reconcile with the ring without it.
func TestADrainedServerLosesItsStandbysAndGetsNone(t *testing.T) {
	ring := []Server{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}}
	drained := WithoutServers(ring, map[string]bool{"b": true})
	// the group whose primary is on a has its standby on b (the next server of the ring); with b out it goes to c
	cand := Candidates(drained, "a")
	if len(cand) == 0 || cand[0] != "c" {
		t.Fatalf("candidates of the group on a: %v", cand)
	}
	add, remove := Plan(cand, []string{"b"}, 1)
	if !reflect.DeepEqual(add, []string{"c"}) || !reflect.DeepEqual(remove, []string{"b"}) {
		t.Fatalf("add %v remove %v", add, remove)
	}
	// the group whose primary is on the drained server itself still has standbys elsewhere
	if c := Candidates(drained, "b"); len(c) != 3 {
		t.Fatalf("candidates of the group on b: %v", c)
	}
	// with too few servers left a group is under-replicated, never two instances on one server
	two := WithoutServers([]Server{{ID: "a"}, {ID: "b"}}, map[string]bool{"b": true})
	if c := Candidates(two, "a"); len(c) != 0 {
		t.Fatalf("a lone server has no candidate: %v", c)
	}
}

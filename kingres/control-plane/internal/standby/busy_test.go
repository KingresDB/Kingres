// SPDX-License-Identifier: AGPL-3.0-only

package standby

import "testing"

// The reconcile loop leaves the groups of an unfinished failover alone.
func TestWithoutGroupsDropsTheRecordsOfBusyGroups(t *testing.T) {
	recs := []Record{{ID: 1, Group: 1}, {ID: 2, Group: 2}, {ID: 3, Group: 1}}
	got := withoutGroups(recs, map[int32]bool{1: true})
	if len(got) != 1 || got[0].ID != 2 {
		t.Fatalf("%+v", got)
	}
	if len(withoutGroups(recs, nil)) != 3 {
		t.Fatal("nothing busy must keep every record")
	}
}

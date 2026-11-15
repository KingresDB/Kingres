// SPDX-License-Identifier: PostgreSQL

package instance

import (
	"strings"
	"testing"
)

func TestNodeRulesCoverReplicationConnections(t *testing.T) {
	hba, err := RenderHBAWithNodes(nil, false, NodeTrust{Peers: []NodePeer{{ServerID: "b", CIDRs: []string{"10.0.0.2/32"}}}})
	if err != nil {
		t.Fatal(err)
	}
	// "all" never matches a replication connection: the certificate rule is needed twice.
	for _, want := range []string{"hostssl   all all 10.0.0.2/32 cert map=kingres_nodes", "hostssl   replication all 10.0.0.2/32 cert map=kingres_nodes"} {
		if !strings.Contains(hba, want) {
			t.Errorf("pg_hba.conf lacks %q:\n%s", want, hba)
		}
	}
}

func TestStandbyDirIDs(t *testing.T) {
	if StandbyDirID(0) != 100 || StandbyDirID(7) != 107 {
		t.Fatal("standby directory ids are 100 plus the group")
	}
	if IsStandbyDirID(1) || !IsStandbyDirID(100) {
		t.Fatal("instance 1 is a primary, 100 and up are standbys")
	}
}

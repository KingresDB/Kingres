// SPDX-License-Identifier: AGPL-3.0-only

package instance

import (
	"strings"
	"testing"
)

// A promoted standby keeps the rule that lets kingresd in by peer authentication when the rules of a primary are written again
// (a restart of kingresd rewrites them): without it the service role asks for a password that the copy does not have.
func TestACopyKeepsTheStandbyAccessWhenTheRulesAreRewritten(t *testing.T) {
	hba, err := RenderHBAWithNodes(nil, false, NodeTrust{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(hba, "peer map="+StandbyIdentMap) {
		t.Fatal("the rules of a primary carry the standby rule")
	}
	if got := withStandbyAccessRule(hba, true); !strings.HasPrefix(got, "local   all  "+ServiceRole+"  peer map="+StandbyIdentMap+"\n") {
		t.Fatalf("a copy lost the rule:\n%s", got)
	}
	if withStandbyAccessRule(hba, false) != hba {
		t.Fatal("a primary got the rule")
	}
	if got := withStandbyAccessRule(withStandbyAccessRule(hba, true), true); strings.Count(got, "peer map="+StandbyIdentMap) != 1 {
		t.Fatal("the rule was added twice")
	}
	id, err := RenderIdentWithNodes("kingres", nil, NodeTrust{})
	if err != nil {
		t.Fatal(err)
	}
	got := withStandbyIdentLine(id, "kingres", true)
	if !strings.Contains(got, StandbyIdentMap+` "kingres" `+ServiceRole+"\n") || withStandbyIdentLine(got, "kingres", true) != got || withStandbyIdentLine(id, "kingres", false) != id {
		t.Fatalf("ident:\n%s", got)
	}
}

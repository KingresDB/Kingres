// SPDX-License-Identifier: PostgreSQL

package monitor

import "testing"

func TestLeaseLostRaisesAnAlertOnlyForALostLease(t *testing.T) {
	r := Readings{Host: "h1", InstanceUp: true, LeasesChecked: true, Leases: []LeaseReading{
		{Group: 0}, {Group: 2, Lost: true, Enforced: false}, {Group: 3, Lost: true, Enforced: true}}}
	conds, evaluated := Evaluate(r)
	if !evaluated["leaseLost"] {
		t.Fatal("leaseLost must be evaluated when the leases were read")
	}
	var got []string
	for _, c := range conds {
		if c.Kind == "leaseLost" {
			got = append(got, c.Subject)
		}
	}
	if len(got) != 2 || got[0] != "server:h1|group:2" || got[1] != "server:h1|group:3" {
		t.Fatalf("subjects: %v", got)
	}
	_, evaluated = Evaluate(Readings{Host: "h1", InstanceUp: true})
	if evaluated["leaseLost"] {
		t.Fatal("an alert must not be cleared when the leases were not read")
	}
}

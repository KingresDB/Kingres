// SPDX-License-Identifier: AGPL-3.0-only

package lease

import (
	"context"
	"testing"
)

// F3-01: the release that OnFail of a failed failover sends must not forget the adoption of a promoted primary
// (attached): the lease keeps being renewed, so that the watchdog does not stop the only promoted copy.
func TestUnadoptRefusesAnAttachedAdoption(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, []string{"a", "b", "c"}, true)
	r.m.c.Source = fakeSource{Topology{Groups: nil, Members: []string{"a", "b", "c"}}}
	if err := r.m.Adopt(6, AdoptHolder); err != nil {
		t.Fatal(err)
	}
	r.m.Renew(ctx)
	if !r.m.Unadopt(6) {
		t.Fatal("an adoption that is not attached must be released")
	}
	if _, _, ok := r.m.Adopted(6); ok {
		t.Fatal("the adoption of a released group is still there")
	}
	if err := r.m.Adopt(6, AdoptHolder); err != nil {
		t.Fatal(err)
	}
	r.m.Renew(ctx)
	if err := r.m.Attach(6); err != nil {
		t.Fatal(err)
	}
	if r.m.Unadopt(6) {
		t.Fatal("an attached adoption was released (F3-01)")
	}
	if _, attached, ok := r.m.Adopted(6); !ok || !attached {
		t.Fatal("the attached adoption is gone after a refused release")
	}
	r.m.Renew(ctx)
	if !r.m.Holds(6) {
		t.Fatal("the lease of the promoted group is not renewed after a refused release")
	}
}

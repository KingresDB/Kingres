// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"strings"
	"testing"
)

// kingres_timepart is brought to the version of the package on every server: the leader's own copy, the other servers through the update path of the
// extension steps, a server that is at the version already is left, a server that is down is named and the others are still updated, and no other
// extension is touched.
func TestUpdateExtensionEverywhere(t *testing.T) {
	c := newFakeCluster("a", "b", "c")
	for _, id := range c.ids {
		c.ext[id].available[ExtTimepart] = "0.3"
		c.ext[id].available[ExtColumnar] = "0.2"
	}
	c.ext["a"].installed[ExtTimepart] = "0.2"
	c.ext["b"].installed[ExtTimepart] = "0.2"
	c.ext["c"].installed[ExtTimepart] = "0.3"
	c.ext["a"].installed[ExtColumnar] = "0.1"
	ctx := context.Background()
	if _, err := c.ops["a"].updateExtensionEverywhere(ctx, "shop", ExtTimepart); err != nil {
		t.Fatal(err)
	}
	for _, id := range c.ids {
		if v := c.ext[id].installed[ExtTimepart]; v != "0.3" {
			t.Errorf("server %s runs kingres_timepart %q", id, v)
		}
	}
	if v := c.ext["a"].installed[ExtColumnar]; v != "0.1" {
		t.Errorf("another extension was changed: %q", v)
	}
	// a server that is down: the error names it, the others are updated
	c.ext["a"].installed[ExtTimepart] = "0.2"
	c.ext["b"].installed[ExtTimepart] = "0.2"
	c.ext["c"].installed[ExtTimepart] = "0.2"
	c.down["b"] = true
	_, err := c.ops["a"].updateExtensionEverywhere(ctx, "shop", ExtTimepart)
	if err == nil || !strings.Contains(err.Error(), "b:") {
		t.Fatalf("the error should name server b: %v", err)
	}
	if c.ext["a"].installed[ExtTimepart] != "0.3" || c.ext["c"].installed[ExtTimepart] != "0.3" || c.ext["b"].installed[ExtTimepart] != "0.2" {
		t.Errorf("versions after: a %s, b %s, c %s", c.ext["a"].installed[ExtTimepart], c.ext["b"].installed[ExtTimepart], c.ext["c"].installed[ExtTimepart])
	}
	// the extension is not installed here: nothing is created on this server (the other servers are asked, as the extension steps do)
	c.down["b"] = false
	c.ext["a"].installed = map[string]string{}
	if _, err := c.ops["a"].updateExtensionEverywhere(ctx, "shop", ExtTimepart); err != nil {
		t.Fatal(err)
	}
	if v := c.ext["a"].installed[ExtTimepart]; v != "" {
		t.Errorf("created on the leader's server: %q", v)
	}
}

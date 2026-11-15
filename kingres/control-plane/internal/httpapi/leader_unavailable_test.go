// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"reflect"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
)

// leader_unavailable names the servers that can be opened instead: the leader first, never this server.
func TestLeaderUnavailableNamesOtherServers(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.LeaderTrusted = func() bool { return false } })
	e.initialize()
	self := "a"
	e.mem.LocalServer = &self
	e.mem.ServerList = []api.Server{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	e.mem.Leader = "c"
	tok := e.token(auth.RoleAdmin)
	m := wantProblem(t, e.do(req{method: "GET", path: "/api/v1/cluster", bearer: tok}), 503, "leader_unavailable")
	var got []string
	for _, v := range m["serverIds"].([]any) {
		got = append(got, v.(string))
	}
	if !reflect.DeepEqual(got, []string{"c", "b"}) {
		t.Errorf("serverIds = %v", got)
	}
}

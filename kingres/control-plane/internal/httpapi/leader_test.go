// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"net/http"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
)

// The manager leader is one fact of the catalog; ClusterOverview.managerLeader,
// Server.isManagerLeader and LocalServerInfo.isManagerLeader must agree.
func TestManagerLeaderAgreesEverywhere(t *testing.T) {
	for _, tc := range []struct{ name, leader, local string }{
		{"this server leads", "h1", "h1"},
		{"another server leads", "h2", "h1"},
		{"nobody leads", "", "h1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.initialize()
			mk := func(id string) api.Server {
				return api.Server{ID: id, Address: id + ":8091", Hostname: id, Status: "live", Instances: []api.Instance{}}
			}
			e.mem.ServerList = []api.Server{mk("h1"), mk("h2")}
			e.mem.Leader = tc.leader
			e.mem.LocalServer = &tc.local
			tok := e.token(auth.RoleReadonly)

			cluster := decode(t, e.do(req{method: "GET", path: "/api/v1/cluster", bearer: tok}))
			this := decode(t, e.do(req{method: "GET", path: "/api/v1/this-server", bearer: tok}))
			list := decode(t, e.do(req{method: "GET", path: "/api/v1/servers", bearer: tok}))

			leaders := map[string]bool{}
			for _, it := range list["items"].([]any) {
				sv := it.(map[string]any)
				leaders[sv["id"].(string)] = sv["isManagerLeader"].(bool)
			}
			one := e.do(req{method: "GET", path: "/api/v1/servers/h2", bearer: tok})
			if one.Code != http.StatusOK {
				t.Fatalf("getServer = %d", one.Code)
			}
			if got := decode(t, one)["isManagerLeader"]; got != leaders["h2"] {
				t.Errorf("getServer h2 = %v, listServers = %v", got, leaders["h2"])
			}
			ml, _ := cluster["managerLeader"].(map[string]any)
			if tc.leader == "" {
				if cluster["managerLeader"] != nil || leaders["h1"] || leaders["h2"] || this["isManagerLeader"] != false {
					t.Errorf("no leader, but cluster=%v servers=%v this=%v", cluster["managerLeader"], leaders, this["isManagerLeader"])
				}
				return
			}
			if ml == nil || ml["serverId"] != tc.leader {
				t.Fatalf("managerLeader = %v, want %s", cluster["managerLeader"], tc.leader)
			}
			for id, is := range leaders {
				if is != (id == tc.leader) {
					t.Errorf("server %s isManagerLeader = %v with leader %s", id, is, tc.leader)
				}
			}
			if want := tc.leader == tc.local; this["isManagerLeader"] != want {
				t.Errorf("this-server isManagerLeader = %v, want %v", this["isManagerLeader"], want)
			}
		})
	}
}

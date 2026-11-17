// SPDX-License-Identifier: AGPL-3.0-only

package clustertest_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/lease"
)

// readRole reads the role file of an instance directory of a node.
func readRole(t *testing.T, n *clustertest.Node, id int) (instance.Role, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(n.DataDir, "instances", strconv.Itoa(id), instance.RoleFileName))
	if err != nil {
		return instance.Role{}, false
	}
	var r instance.Role
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("node %d instance %d: %v", n.Index, id, err)
	}
	return r, true
}

// Every instance directory has a role file naming its group, within a few seconds
// of the start: the home instance of a server is the primary of its own group, the
// directory of a standby is a standby of its group and a copy.
func TestRoleFilesFollowTheCatalog(t *testing.T) {
	c := formStandbys(t, 3, 1)
	top := topology(t, c.Leader())
	primaries, _ := primaryOf(t, c.Leader())
	for g := 0; g < 3; g++ {
		home := nodeOf(c, primaries[g])
		until(t, time.Minute, "the role file of the home instance of group "+strconv.Itoa(g), func() bool {
			r, ok := readRole(t, home, 1)
			return ok && r.Group == int32(g) && r.Role == instance.RolePrimary && r.Origin == instance.OriginOwn
		})
		sb := nodeOf(c, top[g][0].server)
		until(t, time.Minute, "the role file of the standby of group "+strconv.Itoa(g), func() bool {
			r, ok := readRole(t, sb, 100+g)
			return ok && r.Group == int32(g) && r.Role == instance.RoleStandby && r.Origin == instance.OriginCopy
		})
	}
}

// A cluster made before the role files existed (no file in any instance directory,
// and a fence marker without a group suffix) comes up under the new code by itself:
// the files are made from the catalog and the old marker is renamed to the groups
// of the server.
func TestAnOldLayoutConvergesByItself(t *testing.T) {
	c := formStandbys(t, 3, 1)
	n := c.Nodes[1]
	until(t, time.Minute, "role files", func() bool { _, ok := readRole(t, n, 1); return ok })
	n.Stop()
	entries, _ := os.ReadDir(filepath.Join(n.DataDir, "instances"))
	for _, e := range entries {
		_ = os.Remove(filepath.Join(n.DataDir, "instances", e.Name(), instance.RoleFileName))
	}
	// A leftover marker of the old layout from a lease that was lost and is held again by now.
	leaseDir := filepath.Join(n.DataDir, "lease")
	if err := os.WriteFile(filepath.Join(leaseDir, lease.FencedMarker), []byte("fenced\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	n.Start()
	until(t, 2*time.Minute, "the role files made again", func() bool {
		r, ok := readRole(t, n, 1)
		return ok && r.Group == 1 && r.Role == instance.RolePrimary
	})
	// The marker held the primary back until its lease was held; it is gone afterwards (renamed to the group's,
	// then removed by the start), and the instance runs.
	until(t, 2*time.Minute, "the old marker gone and the instance up", func() bool {
		_, err := os.Stat(filepath.Join(leaseDir, lease.FencedMarker))
		return os.IsNotExist(err) && !lease.AnyFenced(leaseDir) && instanceAccepts(n)
	})
	// The cluster works: the server's primary is held by its lease and the standbys stream.
	waitLease(t, c.Leader(), n, 1, "held", 2*time.Minute)
	waitStandbys(t, c, 1)
}

// The group of a server's standby is failed over by hand (the extension's own call
// sequence and the files kingresd reads, as part 2 of the design will do it): after
// it the server runs two primaries, instance 1 for its own group and instance 101
// for the promoted standby, and what kingresd does per server reaches both: the
// lease, the role files, the fan-out of a database, the orderly restart.
func TestInstanceMappingAfterAHandFailover(t *testing.T) {
	c := formStandbys(t, 3, 1)
	leader := c.Leader()
	until(t, 2*time.Minute, "every standby registered", func() bool {
		return allRegistered(recordNodes(t, leader), 3) && len(catalogStandbys(t, leader)) == 3
	})
	top := topology(t, leader)
	primaries, _ := primaryOf(t, leader)
	// A group whose primary is away from the leader (which runs the map group and is not killed). Where its standby
	// is depends on the placement of the run: on the leader's server or on the third.
	var g int32 = -1
	var old, nw *clustertest.Node
	for cand := 1; cand < 3; cand++ {
		o, n := nodeOf(c, primaries[cand]), nodeOf(c, top[cand][0].server)
		if o != nil && n != nil && o != leader && o != n {
			g, old, nw = int32(cand), o, n
			break
		}
	}
	if g < 0 {
		t.Fatalf("no group has its primary away from the leader: %v %v", primaries, top)
	}
	homeGroup := int32(0)
	for cand, h := range primaries {
		if h == nw.Host {
			homeGroup = int32(cand)
		}
	}
	var newNode catalogNode
	for _, n := range catalogStandbys(t, leader) {
		if n.group == g {
			newNode = n
		}
	}
	// The old primary's server goes away for good (its standby of the leader's group goes with it).
	old.Kill()
	old.StopInstance()
	old.StopStandbys()
	// promote_node commits on every primary: the lists that name a standby of the dead server must be given up first.
	sc0 := leader.ServicePG("kingres_system")
	rows, err := sc0.Query(context.Background(), `SELECT standby_id FROM kingres_cp.standby WHERE server_id = $1`, old.Host)
	if err != nil {
		t.Fatal(err)
	}
	var gone []string
	for rows.Next() {
		var id int64
		_ = rows.Scan(&id)
		gone = append(gone, "kingres_standby_"+strconv.FormatInt(id, 10))
	}
	rows.Close()
	_ = sc0.Close(context.Background())
	for _, n := range c.Nodes {
		if n == old {
			continue
		}
		until(t, 2*time.Minute, "the list of node "+n.Host+" giving up the standbys of the dead server", func() bool {
			l := syncNames(t, n)
			for _, name := range gone {
				if strings.Contains(l, name) {
					return false
				}
			}
			return true
		})
	}

	// Promote in PostgreSQL, then record what the instance is, then let the extension move the catalog.
	sc := standbyConn(t, nw, int(g), newNode.port, "postgres")
	ctx := context.Background()
	if _, err := sc.Exec(ctx, `SELECT pg_promote(true, 60)`); err != nil {
		t.Fatal(err)
	}
	until(t, time.Minute, "the promoted instance out of recovery", func() bool {
		var rec bool
		return sc.QueryRow(ctx, `SELECT pg_is_in_recovery()`).Scan(&rec) == nil && !rec
	})
	if _, err := sc.Exec(ctx, `ALTER SYSTEM SET synchronous_standby_names = ''`); err != nil {
		t.Fatal(err)
	}
	if _, err := sc.Exec(ctx, `SELECT pg_reload_conf()`); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(instance.Role{Group: g, Role: instance.RolePrimary, Origin: instance.OriginCopy})
	if err := os.WriteFile(filepath.Join(nw.DataDir, "instances", strconv.Itoa(100+int(g)), instance.RoleFileName), b, 0o600); err != nil {
		t.Fatal(err)
	}
	mc := leader.ServicePG("kingres_system")
	defer func() { _ = mc.Close(ctx) }()
	until(t, 3*time.Minute, "promote_node answering already", func() bool {
		var res string
		if err := mc.QueryRow(ctx, `SELECT kingres.promote_node($1)::text`, newNode.id).Scan(&res); err != nil {
			t.Logf("promote_node: %v", err)
			return false
		}
		return strings.Contains(res, "already")
	})

	// The lease of group 1 is renewed by the server that runs the promoted instance, next to its own group's.
	waitLease(t, leader, nw, g, "held", 2*time.Minute)
	waitLease(t, leader, nw, homeGroup, "held", time.Minute)
	until(t, time.Minute, "the replication view with the promoted primary", func() bool {
		g := replicationGroup(t, leader, g)
		p, _ := g["primary"].(map[string]any)
		return p != nil && p["serverId"] == nw.Host && int32(p["instanceId"].(float64)) == newNode.id
	})
	// The role files are what they were made to be, and the home instance still is group 2's primary.
	if r, ok := readRole(t, nw, 1); !ok || r.Group != homeGroup || r.Role != instance.RolePrimary {
		t.Errorf("home instance of the new server: %+v %v", r, ok)
	}

	// A fan-out reaches every primary of the server: a role made through the API exists on both instances.
	if st, body := leader.Do("POST", "/roles", map[string]any{"name": "mapping_probe", "password": "Pr0be-Passw0rd-xyz", "canLogin": true}, nil); st != 201 && st != 202 && st != 200 {
		t.Logf("creating a role: %d %v", st, body)
	}
	until(t, time.Minute, "the role on both primaries of the server", func() bool {
		home := nw.ServicePG("postgres")
		defer func() { _ = home.Close(ctx) }()
		var a, b int
		_ = home.QueryRow(ctx, `SELECT count(*) FROM pg_roles WHERE rolname = 'mapping_probe'`).Scan(&a)
		_ = sc.QueryRow(ctx, `SELECT count(*) FROM pg_roles WHERE rolname = 'mapping_probe'`).Scan(&b)
		return a == 1 && b == 1
	})

	// Not done here: a restart of kingresd of the server that runs both primaries. With the old primary's server dead only
	// two of the three servers are left, and a restart of one of them takes the majority of the leases away: the leader
	// then fences its own map primary, which is the protocol working, not a defect to test. The restart of a server with
	// two primaries is covered by the unit tests of PrepareStart and the markers per group.
}

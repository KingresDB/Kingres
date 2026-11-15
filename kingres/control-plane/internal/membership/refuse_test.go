// SPDX-License-Identifier: AGPL-3.0-only

package membership

import (
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

// ok is a server that may be removed: known, alive, one of three, no map
// group instance, no buckets, nothing running.
func ok() removalFacts { return removalFacts{Known: true, Servers: 3} }

func TestDecideRemovalEachRefusal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mod    func(*removalFacts)
		status int
		code   problem.Code
	}{
		{"allowed", func(*removalFacts) {}, 0, ""},
		{"unknown", func(f *removalFacts) { f.Known = false }, 404, problem.NotFound},
		{"dead", func(f *removalFacts) { f.Dead = true }, 422, problem.NodeStateInvalid},
		{"last server", func(f *removalFacts) { f.Servers = 1 }, 409, problem.LastServer},
		{"map group", func(f *removalFacts) { f.HostsMapGroup = true }, 409, problem.ServerHostsMapGroup},
		{"buckets", func(f *removalFacts) { f.Holds = []bucketHold{{"shop", 12}} }, 409, problem.ServerOwnsBuckets},
		{"rebalance", func(f *removalFacts) { f.RebalanceRunning = true }, 409, problem.RebalanceInProgress},
		{"operation", func(f *removalFacts) { f.OperationRunning = true }, 409, problem.OperationInProgress},
	} {
		f := ok()
		tc.mod(&f)
		for _, self := range []bool{false, true} {
			p := decideRemoval("db2", self, f)
			switch {
			case tc.code == "" && p != nil:
				t.Errorf("%s (self=%v): refused with %v", tc.name, self, p)
			case tc.code != "" && (p == nil || p.Code != tc.code || p.Status != tc.status):
				t.Errorf("%s (self=%v): got %v, want %d %s", tc.name, self, p, tc.status, tc.code)
			}
		}
	}
}

// The order of the contract: 404, dead (422), last_server, map group, buckets,
// rebalance, operation. Each pair is checked: when both conditions hold the
// earlier answer wins.
func TestDecideRemovalOrder(t *testing.T) {
	set := map[problem.Code]func(*removalFacts){
		problem.NotFound:            func(f *removalFacts) { f.Known = false },
		problem.NodeStateInvalid:    func(f *removalFacts) { f.Dead = true },
		problem.LastServer:          func(f *removalFacts) { f.Servers = 1 },
		problem.ServerHostsMapGroup: func(f *removalFacts) { f.HostsMapGroup = true },
		problem.ServerOwnsBuckets:   func(f *removalFacts) { f.Holds = []bucketHold{{"a", 1}} },
		problem.RebalanceInProgress: func(f *removalFacts) { f.RebalanceRunning = true },
		problem.OperationInProgress: func(f *removalFacts) { f.OperationRunning = true },
	}
	order := []problem.Code{problem.NotFound, problem.NodeStateInvalid, problem.LastServer, problem.ServerHostsMapGroup,
		problem.ServerOwnsBuckets, problem.RebalanceInProgress, problem.OperationInProgress}
	for i := range order {
		for j := i + 1; j < len(order); j++ {
			f := ok()
			set[order[i]](&f)
			set[order[j]](&f)
			if p := decideRemoval("s", false, f); p == nil || p.Code != order[i] {
				t.Errorf("%s together with %s: got %v, want %s", order[i], order[j], p, order[i])
			}
		}
	}
}

func TestBucketRefusalNamesDatabasesAndCounts(t *testing.T) {
	f := ok()
	f.Holds = []bucketHold{{"orders", 12}, {"shop", 3}}
	p := decideRemoval("db2", false, f)
	if p == nil {
		t.Fatal("not refused")
	}
	for _, want := range []string{`"orders"`, "12", `"shop"`, "3", "Rebalance", "db2"} {
		if !strings.Contains(p.Detail, want) {
			t.Errorf("detail %q lacks %q", p.Detail, want)
		}
	}
}

// SPDX-License-Identifier: AGPL-3.0-only

package clustertest_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// Removing a server must cost no other group its lease and no other instance its availability: the voters of a
// lease are the servers of the catalog, and the catalog loses one while the others hold their leases (with
// standbys, a lost lease stops the instance). The leases of the two servers that stay are sampled every
// 100 ms from the start of the removal until well after it, and so is a connection to each of their instances: not
// one sample may show a lease that is not held, or an instance that refuses a connection.
func TestRemovingAServerCostsNoOtherGroupItsLease(t *testing.T) {
	// ST-01: the groups are enforced because they have real standbys, not because the fault hook says so: the
	// removal takes the standbys of the removed server away, and the groups that stay must not be fenced by their watchdogs.
	c, groups := leaseCluster(t, false, standbyArgs...)
	waitStandbys(t, c, 1)
	leader, victim := c.Nodes[0], c.Nodes[2]
	stay := c.Nodes[:2]
	var mu sync.Mutex
	var bad []string
	stop := make(chan struct{})
	var wg sync.WaitGroup
	start := time.Now()
	for i, n := range stay {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				case <-time.After(100 * time.Millisecond):
				}
				at := time.Since(start).Round(10 * time.Millisecond)
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				if conn, err := n.PG(ctx, "kingresd", servicePassword(t, n), "postgres"); err != nil {
					mu.Lock()
					bad = append(bad, fmt.Sprintf("+%s node %d: its instance refused a connection: %v", at, n.Index, err))
					mu.Unlock()
				} else {
					_ = conn.Close(ctx)
				}
				cancel()
				s, err := leaseState(c.Nodes[(n.Index+1)%2], n)
				if err != nil {
					continue // the answer of the other server is not what is measured
				}
				for _, l := range s.Leases {
					if l.Group == groups[i] && l.Holder == n.Host && l.State != "held" {
						mu.Lock()
						bad = append(bad, fmt.Sprintf("+%s node %d: the lease of group %d is %q (%d ms left)", at, n.Index, l.Group, l.State, l.RemainingMillis))
						mu.Unlock()
					}
				}
			}
		}()
	}
	body := leader.Expect(202, "DELETE", "/servers/"+victim.Host, nil)
	leader.WaitTaskDone(clustertest.TaskID(body), 5*time.Minute)
	time.Sleep(20 * time.Second) // longer than a lease: whatever the removal changed has had its effect
	close(stop)
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	for _, b := range bad {
		t.Error(b)
	}
	if len(bad) > 0 {
		t.Logf("the removal started at +0s and took %s\n%s\n%s", time.Since(start), leader.LogTail(3000), c.Nodes[1].LogTail(3000))
	}
}

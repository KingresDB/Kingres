// SPDX-License-Identifier: AGPL-3.0-only

package clustertest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// instanceSampler connects to the instance of every node every 100 ms and keeps what failed.
type instanceSampler struct {
	t     *testing.T
	mu    sync.Mutex
	bad   []string
	stop  chan struct{}
	once  sync.Once
	wg    sync.WaitGroup
	start time.Time
}

func sampleInstances(t *testing.T, nodes []*clustertest.Node) *instanceSampler {
	s := &instanceSampler{t: t, stop: make(chan struct{}), start: time.Now()}
	for _, n := range nodes {
		pw := servicePassword(t, n) // read here: a goroutine must not call t.Fatal, and the files go when the test ends
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			for {
				select {
				case <-s.stop:
					return
				case <-time.After(100 * time.Millisecond):
				}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				conn, err := n.PG(ctx, "kingresd", pw, "postgres")
				if err == nil {
					var one int
					if err = conn.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
						err = fmt.Errorf("query: %w", err)
					}
					_ = conn.Close(ctx)
				}
				cancel()
				if err != nil {
					s.mu.Lock()
					s.bad = append(s.bad, fmt.Sprintf("+%s node %d: %v", time.Since(s.start).Round(10*time.Millisecond), n.Index, err))
					s.mu.Unlock()
				}
			}
		}()
	}
	// The sampler ends with the test even when the test fails early.
	t.Cleanup(s.halt)
	return s
}

// finish stops the sampling and fails the test for every failed sample.
func (s *instanceSampler) halt() {
	s.once.Do(func() { close(s.stop) })
	s.wg.Wait()
}

func (s *instanceSampler) finish(nodes []*clustertest.Node) {
	s.halt()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.bad {
		s.t.Error(b)
	}
	if len(s.bad) > 0 {
		for _, n := range nodes {
			s.t.Logf("node %d watchdog log:\n%s", n.Index, readTail(filepath.Join(n.DataDir, "lease", "watchdog.log")))
		}
	}
}

func readTail(p string) string {
	b, _ := os.ReadFile(p)
	if len(b) > 2000 {
		b = b[len(b)-2000:]
	}
	return string(b)
}

// watchdogOff reports whether the lease file of the primary instance of a node says that nothing is enforced.
func watchdogFile(n *clustertest.Node) (off, ok bool) {
	b, err := os.ReadFile(filepath.Join(n.DataDir, "instances", "1", "lease.json"))
	if err != nil {
		return false, false
	}
	var f struct {
		Off     bool  `json:"off"`
		ValidMs int64 `json:"validMs"`
	}
	if json.Unmarshal(b, &f) != nil {
		return false, false
	}
	return f.Off, true
}

func setReplicas(t *testing.T, leader *clustertest.Node, n int) {
	t.Helper()
	if st, body := leader.Do("PATCH", "/settings/cluster", map[string]any{"defaultReplicaCount": n}, nil); st != 200 && st != 202 {
		t.Fatalf("setting the replica count to %d: %d %v", n, st, body)
	}
}

// ST-01 (the reviewer's deterministic case, and the loop): the replica count goes to 0 and back, again and again,
// without the fault hook that forces enforcement. Every group stops being enforced and is enforced again; the
// watchdog is told each time. Not one primary may be stopped: every sample of every instance connects.
func TestSwitchingEnforcementOnAndOffNeverFencesAHealthyPrimary(t *testing.T) {
	// The default lease (10 s, margin 3 s): a watchdog that was not told fences 7 s after the last file, and the
	// observation window below is longer than that. A short lease would make a loaded machine lose leases for real.
	c := clustertest.FormWithExtra(t, 3, standbyArgs, nil)
	waitStandbys(t, c, 1)
	leader := c.Leader()
	for _, n := range c.Nodes {
		until(t, time.Minute, "an armed watchdog file", func() bool { off, ok := watchdogFile(n); return ok && !off })
	}
	s := sampleInstances(t, c.Nodes)
	for round := 0; round < 3; round++ {
		setReplicas(t, leader, 0)
		for _, n := range c.Nodes {
			until(t, 3*time.Minute, fmt.Sprintf("round %d: the watchdog of node %d told that nothing is enforced", round, n.Index), func() bool { off, ok := watchdogFile(n); return ok && off })
		}
		// The negative observation: with the stale file of the bug the watchdog fences within one lease (6 s);
		// look for two.
		time.Sleep(12 * time.Second)
		setReplicas(t, leader, 1)
		waitStandbys(t, c, 1)
		for _, n := range c.Nodes {
			until(t, time.Minute, fmt.Sprintf("round %d: the watchdog of node %d armed again", round, n.Index), func() bool { off, ok := watchdogFile(n); return ok && !off })
		}
	}
	s.finish(c.Nodes)
}

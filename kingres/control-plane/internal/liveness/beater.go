// SPDX-License-Identifier: PostgreSQL

package liveness

import (
	"context"
	"sync"
	"time"
)

// Beater sends a heartbeat to every expected server once per interval and
// records the answers. A slow or silent server never delays the others (each
// has its own goroutine) and never piles calls up (a server with a call in
// flight is skipped until it ends).
type Beater struct {
	Tracker  *Tracker
	Interval time.Duration
	// Timeout bounds one heartbeat call.
	Timeout time.Duration
	// Peers lists the other servers (the directory of the cluster).
	Peers func() []string
	// Send makes one heartbeat call to a server; a nil error is an answer.
	Send func(ctx context.Context, server string) error
}

// Run beats until ctx ends and waits for the calls in flight.
func (b *Beater) Run(ctx context.Context) {
	var wg sync.WaitGroup
	defer wg.Wait()
	inflight := map[string]bool{}
	var mu sync.Mutex
	t := time.NewTicker(b.Interval)
	defer t.Stop()
	for {
		b.Tracker.Expect(b.Peers())
		b.Tracker.Tick()
		for _, p := range b.Tracker.Expected() {
			mu.Lock()
			busy := inflight[p]
			inflight[p] = true
			mu.Unlock()
			if busy {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				cctx, cancel := context.WithTimeout(ctx, b.Timeout)
				err := b.Send(cctx, p)
				cancel()
				if err == nil {
					b.Tracker.Heard(p)
				}
				mu.Lock()
				delete(inflight, p)
				mu.Unlock()
			}()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

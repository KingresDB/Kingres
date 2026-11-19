// SPDX-License-Identifier: AGPL-3.0-only

package lease

import "sync"

// flights counts the requests of the renewal rounds that were sent and have not been answered, per group. A round ends when a
// majority answered; the request to the last grantor may still be on its way. Whatever must not be followed by a renewal at a
// grantor (a release, a transfer, the end of a test that removes the directory the grantor writes to) waits for them (FO-13).
type flights struct {
	mu   sync.Mutex
	cond *sync.Cond
	n    map[int32]int
}

func (f *flights) add(g int32) {
	f.mu.Lock()
	if f.n == nil {
		f.n = map[int32]int{}
		f.cond = sync.NewCond(&f.mu)
	}
	f.n[g]++
	f.mu.Unlock()
}

func (f *flights) done(g int32) {
	f.mu.Lock()
	f.n[g]--
	if f.n[g] <= 0 {
		delete(f.n, g)
	}
	f.cond.Broadcast()
	f.mu.Unlock()
}

// wait returns when no request of the group is in flight. New rounds of the group must have been stopped by then (the group is
// marked relinquished), or the wait can only end between two rounds.
func (f *flights) wait(g int32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for f.n[g] > 0 {
		f.cond.Wait()
	}
}

// waitAll returns when no request of any group is in flight (the end of a test).
func (f *flights) waitAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for len(f.n) > 0 {
		f.cond.Wait()
	}
}

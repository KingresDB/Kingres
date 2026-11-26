// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package lease

import (
	"context"
	"sync"
)

// fairSem is a counting semaphore that hands its places to the waiters in the order they asked (a channel semaphore wakes
// them in no order, so with more rounds than places a group could wait through several slow rounds, phase 5 review M-C).
type fairSem struct {
	mu    sync.Mutex
	free  int
	queue []chan struct{}
}

func newFairSem(n int) *fairSem { return &fairSem{free: n} }

// acquire waits for a place; it returns false when ctx ended first.
func (s *fairSem) acquire(ctx context.Context) bool {
	s.mu.Lock()
	if s.free > 0 && len(s.queue) == 0 {
		s.free--
		s.mu.Unlock()
		return true
	}
	ch := make(chan struct{})
	s.queue = append(s.queue, ch)
	s.mu.Unlock()
	select {
	case <-ch:
		return true
	case <-ctx.Done():
		s.mu.Lock()
		for i, c := range s.queue {
			if c == ch {
				s.queue = append(s.queue[:i], s.queue[i+1:]...)
				s.mu.Unlock()
				return false
			}
		}
		s.mu.Unlock()
		// The place was handed over at the same moment: give it on.
		s.release()
		return false
	}
}

// release gives a place back, to the first waiter when there is one.
func (s *fairSem) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) > 0 {
		ch := s.queue[0]
		s.queue = s.queue[1:]
		close(ch)
		return
	}
	s.free++
}

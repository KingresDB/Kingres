// SPDX-License-Identifier: PostgreSQL

package auth

import (
	"sync"
	"time"
)

// Limiter is a sliding-window rate limiter keyed by an arbitrary string (a
// client address or a user name). It is safe for concurrent use and bounds
// its memory: when more than maxKeys keys are tracked, expired keys are
// dropped first and, if the table is still full, new keys are refused.
type Limiter struct {
	limit   int
	window  time.Duration
	maxKeys int
	now     func() time.Time

	mu   sync.Mutex
	hits map[string][]time.Time
}

// NewLimiter allows limit events per key in any window. now is the clock.
func NewLimiter(limit int, window time.Duration, now func() time.Time) *Limiter {
	return &Limiter{
		limit: limit, window: window, maxKeys: 50000, now: now,
		hits: map[string][]time.Time{},
	}
}

// Allow records an event for key and reports whether it is within the limit.
// When it is not, retryAfter is the time until the oldest event leaves the
// window. A refused event is not recorded, so a client that backs off is
// admitted again after the window.
func (l *Limiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	cutoff := now.Add(-l.window)
	h := l.hits[key]
	i := 0
	for i < len(h) && !h[i].After(cutoff) {
		i++
	}
	h = h[i:]
	if len(h) >= l.limit {
		l.hits[key] = h
		return false, h[0].Add(l.window).Sub(now)
	}
	if _, exists := l.hits[key]; !exists && len(l.hits) >= l.maxKeys {
		l.evictLocked(cutoff)
		if len(l.hits) >= l.maxKeys {
			return false, l.window
		}
	}
	l.hits[key] = append(h, now)
	return true, 0
}

// Reset forgets the events of key, for example after a successful login.
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.hits, key)
}

func (l *Limiter) evictLocked(cutoff time.Time) {
	for k, h := range l.hits {
		if len(h) == 0 || !h[len(h)-1].After(cutoff) {
			delete(l.hits, k)
		}
	}
}

// Blocked reports, without recording an event, whether key has used up its
// limit, and if so how long until it may try again.
func (l *Limiter) Blocked(key string) (blocked bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	cutoff := now.Add(-l.window)
	h := l.hits[key]
	i := 0
	for i < len(h) && !h[i].After(cutoff) {
		i++
	}
	h = h[i:]
	if len(h) >= l.limit {
		return true, h[0].Add(l.window).Sub(now)
	}
	return false, 0
}

// Record adds an event for key without checking the limit; it is used with
// Blocked to count only failed attempts. Unlike Allow it also refuses to grow
// the table beyond its bound, silently dropping the event.
func (l *Limiter) Record(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	cutoff := now.Add(-l.window)
	if _, exists := l.hits[key]; !exists && len(l.hits) >= l.maxKeys {
		l.evictLocked(cutoff)
		if len(l.hits) >= l.maxKeys {
			return
		}
	}
	h := l.hits[key]
	i := 0
	for i < len(h) && !h[i].After(cutoff) {
		i++
	}
	l.hits[key] = append(h[i:], now)
}

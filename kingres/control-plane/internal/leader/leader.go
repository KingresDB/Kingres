// SPDX-License-Identifier: PostgreSQL

// Package leader tells which server is the manager leader: the server that
// holds the primary of shard group 0, read from the catalog (design
// control-plane-3.md, D6, and control-plane-2.md, D1). The answer is cached
// for a short time; an error or an unknown leader is never answered from a
// stale value, because leader-only calls are authorized with it.
package leader

import (
	"context"
	"sync"
	"time"
)

// Source reads the leader from the catalog: "" when there is none.
type Source func(ctx context.Context) (string, error)

// Resolver caches a Source.
type Resolver struct {
	src Source
	ttl time.Duration
	now func() time.Time

	mu  sync.Mutex
	val string
	at  time.Time
	ok  bool
}

// New builds a resolver. now may be nil.
func New(src Source, ttl time.Duration, now func() time.Time) *Resolver {
	if now == nil {
		now = time.Now
	}
	return &Resolver{src: src, ttl: ttl, now: now}
}

// Get returns the leader's server id, "" when unknown. A failing source gives
// the error and no value.
func (r *Resolver) Get(ctx context.Context) (string, error) {
	r.mu.Lock()
	if r.ok && r.now().Sub(r.at) < r.ttl {
		v := r.val
		r.mu.Unlock()
		return v, nil
	}
	r.mu.Unlock()
	v, err := r.src(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		r.ok = false
		return "", err
	}
	r.val, r.at, r.ok = v, r.now(), true
	return v, nil
}

// Current is Get for callers without a context (the internal API's
// authorization): it waits at most three seconds and answers "" on failure.
func (r *Resolver) Current() string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	v, err := r.Get(ctx)
	if err != nil {
		return ""
	}
	return v
}

// Invalidate drops the cached value.
func (r *Resolver) Invalidate() {
	r.mu.Lock()
	r.ok = false
	r.mu.Unlock()
}

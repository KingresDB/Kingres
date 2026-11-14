// SPDX-License-Identifier: PostgreSQL

package auth

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Revalidator makes authorization follow the current state of PostgreSQL
// instead of the state at sign-in: every request asks it for the roles of the
// session's role. The answer is cached for a few seconds per role (that is the
// longest a change made directly in SQL, outside the API, can go unnoticed);
// a change through the API calls Changed, which ends the role's sessions and
// forgets the cached answers at once.
type Revalidator struct {
	authn    Authenticator
	sessions *Store
	ttl      time.Duration
	now      func() time.Time

	mu    sync.Mutex
	cache map[string]revalEntry
	epoch uint64
	// good is the last successful answer per role, kept past the cache time for
	// LastGood (never used to authorize anything but the start of a stopped
	// instance).
	good map[string]goodEntry
}

type goodEntry struct {
	roles []string
	at    time.Time
}

type revalEntry struct {
	roles []string
	err   error
	at    time.Time
	epoch uint64
}

// DefaultRevalidateTTL is the staleness bound for changes made outside the API.
const DefaultRevalidateTTL = 3 * time.Second

// NewRevalidator builds one. ttl <= 0 uses DefaultRevalidateTTL.
func NewRevalidator(a Authenticator, sessions *Store, ttl time.Duration, now func() time.Time) *Revalidator {
	if ttl <= 0 {
		ttl = DefaultRevalidateTTL
	}
	if now == nil {
		now = time.Now
	}
	return &Revalidator{authn: a, sessions: sessions, ttl: ttl, now: now, cache: map[string]revalEntry{}, good: map[string]goodEntry{}}
}

// Roles returns the current cluster roles of the role. ErrInvalidCredentials
// means the role may not sign in any more: its sessions are ended. Any other
// error (the backend is unavailable) is returned unchanged and is never
// cached: the request is refused rather than answered from stale data.
func (v *Revalidator) Roles(ctx context.Context, username string) ([]string, error) {
	v.mu.Lock()
	e, ok := v.cache[username]
	epoch := v.epoch
	v.mu.Unlock()
	if ok && e.epoch == epoch && v.now().Sub(e.at) < v.ttl {
		return e.roles, e.err
	}
	roles, err := v.authn.CurrentRoles(ctx, username)
	if err != nil && !errors.Is(err, ErrInvalidCredentials) {
		return nil, err
	}
	if err != nil {
		v.sessions.RevokeUser(username)
	}
	v.mu.Lock()
	if err == nil {
		if len(v.good) > 4096 {
			v.good = map[string]goodEntry{}
		}
		v.good[username] = goodEntry{roles: roles, at: v.now()}
	} else {
		delete(v.good, username)
	}
	if len(v.cache) > 4096 {
		v.cache = map[string]revalEntry{}
	}
	v.cache[username] = revalEntry{roles: roles, err: err, at: v.now(), epoch: epoch}
	v.mu.Unlock()
	return roles, err
}

// Changed ends every session of the role and forgets all cached answers (a
// membership change of a group role affects other roles too).
func (v *Revalidator) Changed(username string) {
	if username != "" {
		v.sessions.RevokeUser(username)
	}
	v.mu.Lock()
	v.epoch++
	v.cache = map[string]revalEntry{}
	v.good = map[string]goodEntry{}
	v.mu.Unlock()
}

// LastGood returns the cluster roles of the role's last successful check, if
// it is not older than maxAge. It is for the one operation that must work while
// PostgreSQL cannot be asked (starting the stopped instance, see the README);
// every other caller uses Roles, which fails closed.
func (v *Revalidator) LastGood(username string, maxAge time.Duration) ([]string, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	g, ok := v.good[username]
	if !ok || v.now().Sub(g.at) > maxAge || len(g.roles) == 0 {
		return nil, false
	}
	return append([]string(nil), g.roles...), true
}

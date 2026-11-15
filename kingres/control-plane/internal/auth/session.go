// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Kind tells how a session was established.
type Kind int

const (
	// KindCookie is a browser session: it carries a CSRF token.
	KindCookie Kind = iota
	// KindToken is a bearer token session for the CLI: it needs no CSRF token.
	KindToken
)

// Session is an authenticated principal. The raw credential (cookie value or
// bearer token) is not part of it and is never stored: the store keys
// sessions by the SHA-256 digest of the credential.
type Session struct {
	// Username is the PostgreSQL role that signed in.
	Username string
	// ClusterRoles are the cluster roles at sign-in time, normalized.
	ClusterRoles []string
	// Kind is KindCookie or KindToken.
	Kind Kind
	// CSRFToken is set for cookie sessions.
	CSRFToken string
	// ExpiresAt is the absolute expiry.
	ExpiresAt time.Time
	// CreatedAt is the sign-in time.
	CreatedAt time.Time
	// ProofKey is what the session proves itself to other servers with when a
	// request is forwarded (package fwdproof); empty when the role has no SCRAM
	// verifier. It is kept in memory only and never leaves this server.
	ProofKey []byte
}

// Errors returned by the session store.
var (
	// ErrNoSession is returned for an unknown or revoked credential.
	ErrNoSession = errors.New("no such session")
	// ErrExpired is returned for a credential whose session has expired.
	ErrExpired = errors.New("session expired")
	// ErrTooManySessions is returned when the store is full.
	ErrTooManySessions = errors.New("too many sessions")
)

// Store is the server-side session store. It is held in memory: sessions do
// not survive a restart, which signs everybody out (documented behavior).
type Store struct {
	ttl time.Duration
	max int
	// maxPerUser bounds the sessions of one role; the oldest one of the role is
	// ended when a new one would exceed it. Without it one account could use up
	// the global cap and lock every other sign-in out (CP2-09).
	maxPerUser int
	now        func() time.Time

	mu       sync.Mutex
	sessions map[[sha256.Size]byte]*Session
	expired  map[[sha256.Size]byte]time.Time // recently expired, to report session_expired
}

// DefaultMaxSessionsPerUser is the number of live sessions one role may hold.
const DefaultMaxSessionsPerUser = 20

// NewStore creates a store. ttl is the absolute session lifetime, max the
// maximum number of live sessions (a backstop: the per-role cap is what stops
// one account from filling the store), now the clock.
func NewStore(ttl time.Duration, max int, now func() time.Time) *Store {
	return &Store{
		ttl: ttl, max: max, maxPerUser: DefaultMaxSessionsPerUser, now: now,
		sessions: map[[sha256.Size]byte]*Session{},
		expired:  map[[sha256.Size]byte]time.Time{},
	}
}

// newSecret returns 32 random bytes encoded as unpadded base64url.
func newSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("reading random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func digest(secret string) [sha256.Size]byte { return sha256.Sum256([]byte(secret)) }

// Create opens a session of the given kind and returns the credential, which
// is shown to the client once and not stored. Cookie sessions get a CSRF
// token.
func (s *Store) Create(username string, roles []string, kind Kind) (credential string, sess Session, err error) {
	credential, err = newSecret()
	if err != nil {
		return "", Session{}, err
	}
	sess = Session{Username: username, ClusterRoles: Normalize(roles), Kind: kind}
	if kind == KindCookie {
		if sess.CSRFToken, err = newSecret(); err != nil {
			return "", Session{}, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.sweepLocked(now)
	s.evictOldestOfLocked(username)
	if len(s.sessions) >= s.max {
		return "", Session{}, ErrTooManySessions
	}
	sess.CreatedAt = now
	sess.ExpiresAt = now.Add(s.ttl)
	cp := sess
	s.sessions[digest(credential)] = &cp
	return credential, sess, nil
}

// evictOldestOfLocked ends the oldest sessions of the role until one more fits
// under the per-role cap.
func (s *Store) evictOldestOfLocked(username string) {
	for {
		n := 0
		var oldestKey [sha256.Size]byte
		var oldest time.Time
		for k, sess := range s.sessions {
			if sess.Username != username {
				continue
			}
			if n == 0 || sess.CreatedAt.Before(oldest) {
				oldestKey, oldest = k, sess.CreatedAt
			}
			n++
		}
		if n < s.maxPerUser {
			return
		}
		delete(s.sessions, oldestKey)
	}
}

// Lookup returns the session of a credential. ErrExpired is returned (once
// per credential, until the tombstone is swept) for an expired session, and
// ErrNoSession for an unknown one.
func (s *Store) Lookup(credential string) (Session, error) {
	key := digest(credential)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if sess, ok := s.sessions[key]; ok {
		if !now.Before(sess.ExpiresAt) {
			delete(s.sessions, key)
			s.expired[key] = now.Add(s.ttl)
			return Session{}, ErrExpired
		}
		return *sess, nil
	}
	if _, ok := s.expired[key]; ok {
		return Session{}, ErrExpired
	}
	return Session{}, ErrNoSession
}

// Revoke deletes the session of a credential. Revoking an unknown credential
// is not an error.
func (s *Store) Revoke(credential string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, digest(credential))
}

// Len returns the number of live sessions (expired ones may be included until
// the next sweep).
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// Sweep removes expired sessions and old expiry tombstones.
func (s *Store) Sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(s.now())
}

func (s *Store) sweepLocked(now time.Time) {
	for k, sess := range s.sessions {
		if !now.Before(sess.ExpiresAt) {
			delete(s.sessions, k)
			s.expired[k] = now.Add(s.ttl)
		}
	}
	for k, until := range s.expired {
		if !now.Before(until) {
			delete(s.expired, k)
		}
	}
}

// Run sweeps the store every interval until ctx is done. The caller owns the
// goroutine and waits for Run to return.
func (s *Store) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Sweep()
		}
	}
}

// CSRFValid compares a presented CSRF token with the session's token in
// constant time. A session without a CSRF token (bearer) never matches.
func CSRFValid(sess Session, presented string) bool {
	if sess.CSRFToken == "" || presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(sess.CSRFToken), []byte(presented)) == 1
}

// RevokeUser deletes every session and token of a role and returns how many
// there were. It is how a change to a role ends its sessions at once.
func (s *Store) RevokeUser(username string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k, sess := range s.sessions {
		if sess.Username == username {
			delete(s.sessions, k)
			n++
		}
	}
	return n
}

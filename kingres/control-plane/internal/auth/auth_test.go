// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package auth

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func TestNormalizeAndIsClusterRole(t *testing.T) {
	got := Normalize([]string{RoleStats, "postgres", RoleAdmin, RoleStats, RoleNone})
	if len(got) != 2 || got[0] != RoleAdmin || got[1] != RoleStats {
		t.Errorf("Normalize = %v", got)
	}
	if IsClusterRole(RoleNone) || IsClusterRole("x") || !IsClusterRole(RoleOps) {
		t.Error("IsClusterRole is wrong")
	}
}

func TestStoreLifecycle(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	s := NewStore(time.Hour, 2, c.now)
	cred, sess, err := s.Create("alice", []string{RoleOps, RoleAdmin}, KindCookie)
	if err != nil {
		t.Fatal(err)
	}
	if sess.CSRFToken == "" || len(cred) < 40 || cred == sess.CSRFToken {
		t.Errorf("weak credential or csrf: %q %q", cred, sess.CSRFToken)
	}
	if !sess.ExpiresAt.Equal(c.t.Add(time.Hour)) {
		t.Errorf("expiry %v", sess.ExpiresAt)
	}
	got, err := s.Lookup(cred)
	if err != nil || got.Username != "alice" || got.ClusterRoles[0] != RoleAdmin {
		t.Fatalf("Lookup = %+v %v", got, err)
	}
	tokCred, tokSess, err := s.Create("alice", nil, KindToken)
	if err != nil || tokSess.CSRFToken != "" {
		t.Fatalf("token session: %+v %v", tokSess, err)
	}
	if _, _, err := s.Create("bob", nil, KindToken); !errors.Is(err, ErrTooManySessions) {
		t.Errorf("full store: %v", err)
	}
	s.Revoke(tokCred)
	if _, err := s.Lookup(tokCred); !errors.Is(err, ErrNoSession) {
		t.Errorf("revoked: %v", err)
	}
	if _, err := s.Lookup("unknown"); !errors.Is(err, ErrNoSession) {
		t.Errorf("unknown: %v", err)
	}
	c.t = c.t.Add(time.Hour)
	if _, err := s.Lookup(cred); !errors.Is(err, ErrExpired) {
		t.Errorf("expired: %v", err)
	}
	c.t = c.t.Add(2 * time.Hour)
	s.Sweep()
	if _, err := s.Lookup(cred); !errors.Is(err, ErrNoSession) {
		t.Errorf("after the tombstone is swept: %v", err)
	}
	if s.Len() != 0 {
		t.Errorf("Len = %d", s.Len())
	}
}

func TestCredentialsAreUnique(t *testing.T) {
	s := NewStore(time.Hour, 1000, time.Now)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		cred, sess, err := s.Create("u", nil, KindCookie)
		if err != nil {
			t.Fatal(err)
		}
		if seen[cred] || seen[sess.CSRFToken] {
			t.Fatal("duplicate secret")
		}
		seen[cred], seen[sess.CSRFToken] = true, true
	}
}

func TestCSRFValid(t *testing.T) {
	sess := Session{CSRFToken: "abc"}
	tests := []struct {
		sess      Session
		presented string
		want      bool
	}{
		{sess, "abc", true}, {sess, "abd", false}, {sess, "", false}, {sess, "abcd", false},
		{Session{}, "", false}, {Session{}, "abc", false},
	}
	for _, tc := range tests {
		if got := CSRFValid(tc.sess, tc.presented); got != tc.want {
			t.Errorf("CSRFValid(%q, %q) = %v", tc.sess.CSRFToken, tc.presented, got)
		}
	}
}

func TestLimiter(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	l := NewLimiter(3, time.Minute, c.now)
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("attempt %d refused", i)
		}
	}
	ok, wait := l.Allow("a")
	if ok || wait <= 0 || wait > time.Minute {
		t.Fatalf("4th attempt: ok=%v wait=%v", ok, wait)
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Error("keys must be independent")
	}
	c.t = c.t.Add(30 * time.Second)
	if ok, _ := l.Allow("a"); ok {
		t.Error("window has not passed yet")
	}
	c.t = c.t.Add(31 * time.Second)
	if ok, _ := l.Allow("a"); !ok {
		t.Error("window passed, must be admitted")
	}
	l.Reset("a")
	if b, _ := l.Blocked("a"); b {
		t.Error("Reset must clear the key")
	}
}

func TestLimiterBlockedAndRecord(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	l := NewLimiter(2, time.Minute, c.now)
	for i := 0; i < 5; i++ {
		if b, _ := l.Blocked("k"); b {
			t.Fatal("Blocked must not count by itself")
		}
	}
	l.Record("k")
	l.Record("k")
	if b, wait := l.Blocked("k"); !b || wait <= 0 {
		t.Fatalf("after two failures: blocked=%v wait=%v", b, wait)
	}
}

func TestLimiterBoundsMemory(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	l := NewLimiter(1, time.Minute, c.now)
	l.maxKeys = 3
	for _, k := range []string{"a", "b", "c"} {
		if ok, _ := l.Allow(k); !ok {
			t.Fatal(k)
		}
	}
	if ok, _ := l.Allow("d"); !ok {
		t.Error("a full table must make room, not refuse a new key (CP2-10)")
	}
	if len(l.hits) > l.maxKeys {
		t.Errorf("table has %d keys, bound is %d", len(l.hits), l.maxKeys)
	}
	c.t = c.t.Add(2 * time.Minute)
	if ok, _ := l.Allow("e"); !ok {
		t.Error("expired keys must be evicted to make room")
	}
}

// The reviewer's reproduction of CP2-10: after 50000 distinct keys within the
// window a new address or user name was refused. Neither is now, and the
// table stays bounded.
func TestLimiterSaturationDoesNotLockNewKeysOut(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	l := NewLimiter(10, time.Minute, c.now)
	for i := 0; i < l.maxKeys; i++ {
		c.t = c.t.Add(time.Millisecond)
		if ok, _ := l.Allow(fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255)); !ok {
			t.Fatalf("key %d refused", i)
		}
	}
	for _, k := range []string{"203.0.113.9", "root_admin"} {
		if ok, wait := l.Allow(k); !ok {
			t.Fatalf("new key %q refused with a wait of %v when the table is full", k, wait)
		}
	}
	for i := 0; i < 2*l.maxKeys; i++ {
		l.Record(fmt.Sprintf("flood-%d", i))
	}
	if len(l.hits) > l.maxKeys {
		t.Errorf("table grew to %d keys, bound %d", len(l.hits), l.maxKeys)
	}
}

func TestAddressKeyGroupsIPv6By64(t *testing.T) {
	a := AddressKey("2001:db8:1:2:aaaa:bbbb:cccc:dddd")
	b := AddressKey("2001:db8:1:2:1111:2222:3333:4444")
	if a != b || a == AddressKey("2001:db8:1:3::1") {
		t.Errorf("same /64 must share a key and another /64 must not: %q %q", a, b)
	}
	if AddressKey("192.0.2.1") != "192.0.2.1" || AddressKey("forwarded") != "forwarded" {
		t.Error("IPv4 and non-IP text stay as they are")
	}
}

// CP2-09: one account cannot use up the store; its oldest session goes first
// and other accounts can still sign in.
func TestStorePerUserCap(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	s := NewStore(time.Hour, 100, c.now)
	var first string
	for i := 0; i < 500; i++ {
		c.t = c.t.Add(time.Second)
		cred, _, err := s.Create("mallory", nil, KindToken)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = cred
		}
	}
	if n := s.Len(); n != DefaultMaxSessionsPerUser {
		t.Errorf("mallory holds %d sessions, cap is %d", n, DefaultMaxSessionsPerUser)
	}
	if _, err := s.Lookup(first); !errors.Is(err, ErrNoSession) {
		t.Errorf("the oldest session must be gone: %v", err)
	}
	if _, _, err := s.Create("root_admin", nil, KindToken); err != nil {
		t.Errorf("another account must still sign in: %v", err)
	}
}

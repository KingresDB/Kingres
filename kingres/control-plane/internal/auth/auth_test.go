// SPDX-License-Identifier: PostgreSQL

package auth

import (
	"errors"
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
	if ok, _ := l.Allow("d"); ok {
		t.Error("a full table must refuse new keys (fail closed)")
	}
	c.t = c.t.Add(2 * time.Minute)
	if ok, _ := l.Allow("d"); !ok {
		t.Error("expired keys must be evicted to make room")
	}
}

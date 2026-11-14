// SPDX-License-Identifier: PostgreSQL

package auth

import (
	"context"
	"testing"
	"time"
)

type scriptedAuthn struct {
	roles []string
	err   error
}

func (s *scriptedAuthn) Authenticate(context.Context, string, string) ([]string, error) {
	return s.roles, s.err
}

func (s *scriptedAuthn) CurrentRoles(context.Context, string) ([]string, error) {
	return s.roles, s.err
}

// LastGood remembers the last successful check beyond the cache time, never an
// unavailable backend, and forgets it when the role changes or is refused.
func TestRevalidatorLastGood(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	a := &scriptedAuthn{roles: []string{RoleOps}}
	v := NewRevalidator(a, NewStore(time.Hour, 10, c.now), time.Second, c.now)
	if _, ok := v.LastGood("alice", time.Hour); ok {
		t.Fatal("nothing was checked yet")
	}
	if _, err := v.Roles(context.Background(), "alice"); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(10 * time.Minute) // far past the cache time
	a.err = ErrUnavailable
	if _, err := v.Roles(context.Background(), "alice"); err == nil {
		t.Fatal("an unavailable backend must fail closed in Roles")
	}
	got, ok := v.LastGood("alice", time.Hour)
	if !ok || len(got) != 1 || got[0] != RoleOps {
		t.Fatalf("LastGood = %v %v", got, ok)
	}
	got[0] = "tampered"
	if again, _ := v.LastGood("alice", time.Hour); again[0] != RoleOps {
		t.Error("the caller can change the remembered roles")
	}
	if _, ok := v.LastGood("alice", time.Minute); ok {
		t.Error("an answer older than the bound was returned")
	}
	if _, ok := v.LastGood("bob", time.Hour); ok {
		t.Error("another role's answer")
	}
	// A change through the API forgets it (the role must not be revived).
	v.Changed("alice")
	if _, ok := v.LastGood("alice", time.Hour); ok {
		t.Error("LastGood survived Changed")
	}
	// A refusal forgets it too.
	a.err = nil
	if _, err := v.Roles(context.Background(), "alice"); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(time.Minute)
	a.err = ErrInvalidCredentials
	_, _ = v.Roles(context.Background(), "alice")
	if _, ok := v.LastGood("alice", time.Hour); ok {
		t.Error("LastGood survived the role being refused")
	}
}

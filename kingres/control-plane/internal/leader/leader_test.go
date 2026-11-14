// SPDX-License-Identifier: PostgreSQL

package leader

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCacheAndExpiry(t *testing.T) {
	now := time.Unix(1000, 0)
	calls, val := 0, "s1"
	var fail error
	r := New(func(context.Context) (string, error) { calls++; return val, fail }, 2*time.Second, func() time.Time { return now })
	for range 3 {
		if v, err := r.Get(context.Background()); v != "s1" || err != nil {
			t.Fatalf("Get = %q, %v", v, err)
		}
	}
	if calls != 1 {
		t.Fatalf("source called %d times within the ttl", calls)
	}
	val, now = "s2", now.Add(3*time.Second)
	if v, _ := r.Get(context.Background()); v != "s2" {
		t.Fatalf("after expiry = %q", v)
	}
	r.Invalidate()
	fail = errors.New("catalog down")
	if v, err := r.Get(context.Background()); v != "" || err == nil {
		t.Fatalf("failing source answered %q, %v", v, err)
	}
	if r.Current() != "" {
		t.Fatal("Current answered from a failing source")
	}
	// A failure is never answered from the stale value.
	fail = nil
	_, _ = r.Get(context.Background())
	now = now.Add(10 * time.Second)
	fail = errors.New("down")
	if v, err := r.Get(context.Background()); v != "" || err == nil {
		t.Fatalf("stale value served: %q", v)
	}
}

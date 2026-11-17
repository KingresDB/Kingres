// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type flaky struct {
	failures int32
	calls    atomic.Int32
	err      error
}

func (f *flaky) Authenticate(context.Context, string, string) ([]string, error) {
	if f.calls.Add(1) <= f.failures {
		return nil, f.err
	}
	return []string{"kingres_admin"}, nil
}

func (f *flaky) CurrentRoles(ctx context.Context, u string) ([]string, error) {
	return f.Authenticate(ctx, u, "")
}

func TestRetryingWaitsForAnInstanceThatComesBack(t *testing.T) {
	f := &flaky{failures: 3, err: ErrUnavailable}
	roles, err := Retrying(f, 5*time.Second).CurrentRoles(context.Background(), "u")
	if err != nil || len(roles) != 1 || f.calls.Load() != 4 {
		t.Fatalf("roles %v, err %v, calls %d", roles, err, f.calls.Load())
	}
}

func TestRetryingFailsClosedAfterTheWait(t *testing.T) {
	f := &flaky{failures: 1 << 20, err: ErrUnavailable}
	start := time.Now()
	_, err := Retrying(f, 600*time.Millisecond).CurrentRoles(context.Background(), "u")
	if !errors.Is(err, ErrUnavailable) || time.Since(start) > 3*time.Second {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
}

func TestRetryingDoesNotRetryAnAnswer(t *testing.T) {
	f := &flaky{failures: 1 << 20, err: ErrInvalidCredentials}
	_, err := Retrying(f, 5*time.Second).Authenticate(context.Background(), "u", "p")
	if !errors.Is(err, ErrInvalidCredentials) || f.calls.Load() != 1 {
		t.Fatalf("err %v, calls %d", err, f.calls.Load())
	}
}

type keyer struct{ flaky }

func (*keyer) ProofKey(context.Context, string, string) ([]byte, error) { return []byte{1}, nil }

// The wrappers must keep the proof key capability: without it every session is
// unforwardable to the leader.
func TestWrappersKeepTheProofKeyer(t *testing.T) {
	var a Authenticator = Retrying(Gated(&keyer{}, func() error { return nil }), time.Second)
	k, ok := a.(ProofKeyer)
	if !ok {
		t.Fatal("the wrapped authenticator lost ProofKeyer")
	}
	if key, err := k.ProofKey(context.Background(), "u", "p"); err != nil || len(key) != 1 {
		t.Fatalf("key %v err %v", key, err)
	}
}

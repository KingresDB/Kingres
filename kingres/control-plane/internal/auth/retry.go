// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// DefaultUnavailableWait is how long a check waits for an instance that cannot be
// reached before it gives up: long enough for the restart of an instance that
// was fenced or stopped for a moment (the lease manager starts it again within
// a few rounds), shorter than the request timeout so that the caller still gets
// a 503 with Retry-After instead of a timeout.
const DefaultUnavailableWait = 10 * time.Second

// Retrying wraps an Authenticator so that ErrUnavailable is retried with a short,
// growing pause until wait has passed or the context ends. Any other answer,
// ErrInvalidCredentials included, is returned at once; and after the wait the
// last ErrUnavailable is returned, so a role that cannot be checked still fails
// closed: only how long it waits changes.
func Retrying(a Authenticator, wait time.Duration) Authenticator {
	if wait <= 0 {
		wait = DefaultUnavailableWait
	}
	return &retrying{a: a, wait: wait}
}

type retrying struct {
	a    Authenticator
	wait time.Duration
}

func (r *retrying) Authenticate(ctx context.Context, username, password string) ([]string, error) {
	var roles []string
	err := r.do(ctx, func() (err error) { roles, err = r.a.Authenticate(ctx, username, password); return })
	return roles, err
}

func (r *retrying) CurrentRoles(ctx context.Context, username string) ([]string, error) {
	var roles []string
	err := r.do(ctx, func() (err error) { roles, err = r.a.CurrentRoles(ctx, username); return })
	return roles, err
}

func (r *retrying) do(ctx context.Context, f func() error) error {
	deadline := time.Now().Add(r.wait)
	pause := 200 * time.Millisecond
	for {
		err := f()
		if err == nil || !errors.Is(err, ErrUnavailable) || ctx.Err() != nil {
			return err
		}
		if time.Now().Add(pause).After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(pause):
		}
		pause = min(pause*2, time.Second)
	}
}

// Gated wraps an Authenticator so that, while hold returns an error, nothing is
// checked and the answer is ErrUnavailable wrapping that error. It keeps a
// server from signing a session in (or confirming one) against an instance that
// is about to be stopped: a restarted kingresd has not yet renewed the lease of
// its group, and the watchdog may still fence the instance it found running.
// Put it inside Retrying, so that a request waits for the first round instead
// of failing at once.
func Gated(a Authenticator, hold func() error) Authenticator {
	return &gated{a: a, hold: hold}
}

type gated struct {
	a    Authenticator
	hold func() error
}

func (g *gated) Authenticate(ctx context.Context, username, password string) ([]string, error) {
	if err := g.hold(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return g.a.Authenticate(ctx, username, password)
}

func (g *gated) CurrentRoles(ctx context.Context, username string) ([]string, error) {
	if err := g.hold(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return g.a.CurrentRoles(ctx, username)
}

// ProofKey passes through to the wrapped authenticator when it derives proof keys
// (a wrapper that hid the interface would make every session unforwardable).
func (r *retrying) ProofKey(ctx context.Context, username, password string) ([]byte, error) {
	k, ok := r.a.(ProofKeyer)
	if !ok {
		return nil, errors.New("auth: the authenticator derives no proof keys")
	}
	var key []byte
	err := r.do(ctx, func() (err error) { key, err = k.ProofKey(ctx, username, password); return })
	return key, err
}

// ProofKey passes through, held back like every other check.
func (g *gated) ProofKey(ctx context.Context, username, password string) ([]byte, error) {
	k, ok := g.a.(ProofKeyer)
	if !ok {
		return nil, errors.New("auth: the authenticator derives no proof keys")
	}
	if err := g.hold(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return k.ProofKey(ctx, username, password)
}

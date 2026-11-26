// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package peerclient

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fence"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peers"
)

// flaky serves a JSON answer but ends the connection of the first `bad` requests without one (what a
// caller sees when a connection fails under a call). calls counts the requests that reached it.
func flaky(t *testing.T, cert tls.Certificate, bad int32) (addr string, calls *atomic.Int32) {
	t.Helper()
	calls = new(atomic.Int32)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= bad {
			c, _, err := http.NewResponseController(w).Hijack()
			if err == nil {
				_ = c.Close()
			}
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	srv.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "https://"), calls
}

func clientFor(t *testing.T, f *fixture, addr string) *Client {
	t.Helper()
	pd, _ := peers.Open(t.TempDir())
	_ = pd.Put(peers.Peer{ServerID: "srv", InternalAddr: addr})
	return New(Config{PKIDir: f.dir, Peers: pd, Revoked: f.ca.Revoked, Timeout: 5e9})
}

// A connection that fails under a call is no reason to fail a task step: the call is made again, because
// a step is run again from its start after any interruption and so makes calls that give the same result
// when repeated. A call outside a step is repeated only when it reads (GET); anything else could have
// been done once already.
func TestAConnectionThatFailsUnderACallIsRetriedWhereTheCallMayBeRepeated(t *testing.T) {
	f := newFixture(t, "127.0.0.1:1")
	ctx := context.Background()
	step := fence.With(ctx, fence.Token{Task: 1, Epoch: 1})

	addr, calls := flaky(t, f.cert, 2)
	if err := clientFor(t, f, addr).Do(step, "srv", "POST", "/config/restart", nil, nil); err != nil {
		t.Fatalf("a step's call failed for two broken connections: %v", err)
	}
	if calls.Load() != 3 {
		t.Errorf("the server saw %d calls, want 3", calls.Load())
	}

	addr, calls = flaky(t, f.cert, 1)
	if err := clientFor(t, f, addr).Do(ctx, "srv", "POST", "/forward", nil, nil); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("a call outside a step that changes something was repeated or hid the failure: %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("a call outside a step was made %d times, want 1", calls.Load())
	}

	addr, _ = flaky(t, f.cert, 1)
	if err := clientFor(t, f, addr).Do(ctx, "srv", "GET", "/state", nil, nil); err != nil {
		t.Fatalf("a reading was not repeated: %v", err)
	}

	// Not for ever: three tries, then the failure is returned.
	addr, calls = flaky(t, f.cert, 100)
	if err := clientFor(t, f, addr).Do(step, "srv", "POST", "/config/restart", nil, nil); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("a server that always fails: %v", err)
	}
	if calls.Load() != transportTries {
		t.Errorf("the server saw %d calls, want %d", calls.Load(), transportTries)
	}
}

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
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
)

func TestBootstrapCallPinsOrRecordsTheFingerprint(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.TLS != nil && len(r.TLS.PeerCertificates) != 0 {
			t.Error("the bootstrap call must not present a client certificate")
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	srv.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	srv.StartTLS()
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "https://")
	real := pki.Fingerprint(srv.Certificate().Raw)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Trust on first use: nothing pinned, the seen fingerprint comes back.
	var out struct{ OK bool }
	seen, err := BootstrapCall(ctx, addr, "", "POST", "/state", nil, map[string]string{}, &out)
	if err != nil || seen != real || !out.OK {
		t.Fatalf("first use: %v %q %v", err, seen, out)
	}
	// Pinned and right.
	if _, err := BootstrapCall(ctx, addr, real, "POST", "/state", nil, nil, nil); err != nil {
		t.Fatalf("pinned: %v", err)
	}
	// Pinned and wrong: refused before any request is sent.
	before := hits.Load()
	seen, err = BootstrapCall(ctx, addr, strings.Repeat("0", 64), "POST", "/state", map[string]string{"X-Setup-Token": "secret"}, nil, nil)
	if !errors.Is(err, ErrFingerprintMismatch) || seen != real {
		t.Fatalf("wrong pin: %v %q", err, seen)
	}
	if hits.Load() != before {
		t.Error("a request (and the setup token in it) reached a server whose certificate did not match the pin")
	}
}

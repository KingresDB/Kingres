// SPDX-License-Identifier: AGPL-3.0-only

package peerclient

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peers"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
)

// fixture: a CA, a server certificate it signed for "srv", and a client that
// trusts the CA; the revocation list names serial when revoked is true.
type fixture struct {
	dir    string
	ca     *pki.CA
	cert   tls.Certificate
	serial string
	client *Client
}

func newFixture(t *testing.T, addr string) *fixture {
	t.Helper()
	dir := t.TempDir()
	ca, err := pki.CreateCA(dir, "t", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	names := pki.Names{CommonName: "srv", DNS: []string{"localhost"}}
	key, csr, err := pki.NewKeyAndCSR(names)
	if err != nil {
		t.Fatal(err)
	}
	crt, serial, err := ca.Sign(csr, names, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(crt, key)
	if err != nil {
		t.Fatal(err)
	}
	// This server's own certificate (the client certificate of the calls).
	ckey, ccsr, _ := pki.NewKeyAndCSR(pki.Names{CommonName: "me", DNS: []string{"localhost"}})
	ccrt, _, _ := ca.Sign(ccsr, pki.Names{CommonName: "me", DNS: []string{"localhost"}}, time.Now())
	if err := os.WriteFile(filepath.Join(dir, "server.crt"), ccrt, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "server.key"), ckey, 0o600); err != nil {
		t.Fatal(err)
	}
	pd, err := peers.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := pd.Put(peers.Peer{ServerID: "srv", InternalAddr: addr}); err != nil {
		t.Fatal(err)
	}
	f := &fixture{dir: dir, ca: ca, cert: pair, serial: serial}
	f.client = New(Config{PKIDir: dir, Peers: pd, Revoked: func() []string { return ca.Revoked() }, Timeout: 5 * time.Second})
	return f
}

func serve(t *testing.T, cert tls.Certificate) string {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	srv.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "https://")
}

// A server whose certificate was revoked is not called by Do, but DoUnchecked
// still reaches it: the revocation list must be given to it and it must be told
// to reset itself.
func TestDoRefusesARevokedServerAndDoUncheckedReachesIt(t *testing.T) {
	// The address is known only after the server started, and the server needs the certificate.
	f := newFixture(t, "127.0.0.1:1")
	addr := serve(t, f.cert)
	pd, _ := peers.Open(t.TempDir())
	_ = pd.Put(peers.Peer{ServerID: "srv", InternalAddr: addr})
	f.client = New(Config{PKIDir: f.dir, Peers: pd, Revoked: f.ca.Revoked, Timeout: 5 * time.Second})
	ctx := context.Background()
	if err := f.client.Do(ctx, "srv", "GET", "/state", nil, nil); err != nil {
		t.Fatalf("before the revocation: %v", err)
	}
	if err := f.ca.Revoke(f.serial, time.Now()); err != nil {
		t.Fatal(err)
	}
	f.client.Forget("srv")
	err := f.client.Do(ctx, "srv", "GET", "/state", nil, nil)
	if err == nil || !errors.Is(err, ErrUnreachable) || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("a revoked server was called: %v", err)
	}
	if err := f.client.DoUnchecked(ctx, "srv", "GET", "/state", nil, nil); err != nil {
		t.Fatalf("DoUnchecked: %v", err)
	}
	// Forget drops both kinds of connection.
	f.client.Forget("srv")
	f.client.mu.Lock()
	n := len(f.client.http)
	f.client.mu.Unlock()
	if n != 0 {
		t.Errorf("%d clients left after Forget", n)
	}
}

// A server that presents a certificate of no known authority is not a member
// (any more): Do says so and does not call it unreachable, so that a task does
// not wait for it for ever.
func TestDoReportsAServerThatIsNoMemberNotAsUnreachable(t *testing.T) {
	// What a server that reset itself presents: its bootstrap certificate, self-signed, for its own name.
	crt, key, err := pki.NewBootstrapCert(pki.Names{CommonName: "srv"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(crt, key)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, serve(t, pair))
	err = f.client.DoUnchecked(context.Background(), "srv", "GET", "/state", nil, nil)
	if !errors.Is(err, ErrNotMember) || errors.Is(err, ErrUnreachable) {
		t.Fatalf("a server of another authority: %v", err)
	}
	// A server that is simply not there is unreachable.
	closed := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := strings.TrimPrefix(closed.URL, "https://")
	closed.Close()
	g := newFixture(t, addr)
	if err := g.client.Do(context.Background(), "srv", "GET", "/state", nil, nil); !errors.Is(err, ErrUnreachable) {
		t.Errorf("a server that is gone: %v", err)
	}
	// ForgetAll leaves nothing behind either.
	g.client.ForgetAll()
}

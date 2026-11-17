// SPDX-License-Identifier: AGPL-3.0-only

package anchor

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// N1: the packages never read the proxy settings of the environment,
// whatever client the caller passes. A client whose transport asks the
// environment (the default transport does) is replaced by a copy that does not.
func TestClientNeverUsesTheProxyOfTheEnvironment(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://proxy.example:3128")
	t.Setenv("http_proxy", "http://proxy.example:3128")
	t.Setenv("HTTPS_PROXY", "http://proxy.example:3128")
	t.Setenv("NO_PROXY", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	var mu sync.Mutex
	var dialed []string
	recording := func() *http.Transport {
		return &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				mu.Lock()
				dialed = append(dialed, addr)
				mu.Unlock()
				var d net.Dialer
				return d.DialContext(ctx, network, srv.Listener.Addr().String())
			},
		}
	}
	for name, c := range map[string]*http.Client{
		"nil":                nil,
		"default-transport":  {Transport: http.DefaultTransport},
		"nil-transport":      {},
		"from-environment":   {Transport: recording()},
		"timeout-and-others": {Transport: recording(), Timeout: time.Minute},
	} {
		got := Client(c, 0)
		tr, ok := got.Transport.(*http.Transport)
		if !ok || tr.Proxy != nil {
			t.Errorf("%s: the transport can use a proxy: %#v", name, got.Transport)
		}
		if tr != nil && tr == http.DefaultTransport.(*http.Transport) {
			t.Errorf("%s: the default transport itself was kept", name)
		}
	}
	// and in effect: the address that is dialed is the target, not the proxy
	got := Client(&http.Client{Transport: recording()}, 0)
	resp, err := got.Get("http://kingres-target.example/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(dialed) != 1 || dialed[0] != "kingres-target.example:80" {
		t.Fatalf("dialed %v: the proxy of the environment was used", dialed)
	}
	// the client of the caller is not changed
	orig := &http.Client{Transport: recording()}
	_ = Client(orig, 0)
	if orig.Transport.(*http.Transport).Proxy == nil {
		t.Fatal("the transport of the caller was modified")
	}
	// a transport that is not an *http.Transport is the caller's own code and is kept
	rt := roundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, nil })
	if _, ok := Client(&http.Client{Transport: rt}, 0).Transport.(roundTripperFunc); !ok {
		t.Fatal("a custom round tripper was replaced")
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNewHTTPClient(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://proxy.example:3128")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer srv.Close()
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())

	c := NewHTTPClient(HTTPOptions{RootCAs: roots})
	tr := c.Transport.(*http.Transport)
	if tr.Proxy != nil {
		t.Fatal("a proxy function")
	}
	if tr.TLSClientConfig.RootCAs != roots || tr.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Fatal("tls configuration")
	}
	if tr.TLSHandshakeTimeout != DefaultTLSHandshakeTimeout || tr.ResponseHeaderTimeout != DefaultResponseHeaderTimeout || c.Timeout != DefaultClientTimeout || tr.DialContext == nil {
		t.Fatal("timeouts")
	}
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// through Client the roots and the timeouts survive
	cc := Client(c, 0)
	if cc.Timeout != DefaultClientTimeout || cc.Transport.(*http.Transport).TLSClientConfig.RootCAs != roots {
		t.Fatal("Client lost the configuration")
	}
	if resp, err := cc.Get(srv.URL); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
	}
	// no roots given: an empty pool, so nothing is trusted, and the system roots are never used
	if _, err := NewHTTPClient(HTTPOptions{}).Get(srv.URL); err == nil {
		t.Fatal("a server was trusted without roots")
	}
	// the given durations are used
	c = NewHTTPClient(HTTPOptions{RootCAs: roots, DialTimeout: time.Second, TLSHandshakeTimeout: 2 * time.Second, ResponseHeaderTimeout: 3 * time.Second, Timeout: 4 * time.Second})
	tr = c.Transport.(*http.Transport)
	if tr.TLSHandshakeTimeout != 2*time.Second || tr.ResponseHeaderTimeout != 3*time.Second || c.Timeout != 4*time.Second {
		t.Fatal("durations")
	}
}

func TestPayloadCheck(t *testing.T) {
	good, err := NewPayload("ab12", note("17", root32(), sigLine))
	if err != nil {
		t.Fatal(err)
	}
	if err := good.Check(); err != nil {
		t.Fatal(err)
	}
	if CheckPayload("tsa", good) != nil {
		t.Fatal("CheckPayload")
	}
	mut := map[string]func(*Payload){
		"digest":      func(p *Payload) { p.Digest[0] ^= 1 },
		"size":        func(p *Payload) { p.Size++ },
		"origin":      func(p *Payload) { p.Origin = "other" },
		"id":          func(p *Payload) { p.ID = "ab12/18" },
		"text":        func(p *Payload) { p.NoteText = append([]byte(nil), p.NoteText[1:]...) },
		"signed-note": func(p *Payload) { p.SignedNote = append([]byte(nil), p.SignedNote[1:]...) },
		"zero":        func(p *Payload) { *p = Payload{} },
	}
	for name, f := range mut {
		p := good
		f(&p)
		if p.Check() == nil {
			t.Errorf("%s: accepted", name)
		}
		err := CheckPayload("ots", p)
		if err == nil || CodeOf(err) != "bad_payload" || ClassOf(err) != ClassPermanent {
			t.Errorf("%s: %v", name, err)
		}
	}
}

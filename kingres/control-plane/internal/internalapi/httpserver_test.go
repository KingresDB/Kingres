// SPDX-License-Identifier: AGPL-3.0-only

package internalapi_test

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
)

// A call that takes longer than the time allowed for writing the answer must still be answered: the
// answer is what is bounded, not the call. With a write timeout counted from the request, the connection was
// ended under the call and the caller saw "tls: bad record MAC" (a close alert on a connection whose record
// counter had moved on) instead of the answer: the symptom of a task step that failed on a healthy server.
func TestACallLongerThanTheResponseTimeoutIsAnswered(t *testing.T) {
	certPEM, keyPEM, err := pki.NewBootstrapCert(pki.Names{CommonName: "127.0.0.1"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	const slow = 1500 * time.Millisecond
	be := internalapi.Backend{Bootstrap: func(context.Context, string, *http.Request, []byte) (any, error) {
		time.Sleep(slow)
		return map[string]string{"ok": "yes"}, nil
	}}
	call := func(srv *http.Server) (*http.Response, error) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go func() { _ = srv.ServeTLS(ln, "", "") }()
		t.Cleanup(func() { _ = srv.Close() })
		c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13}}} //nolint:gosec // the test's own certificate
		req, _ := http.NewRequest(http.MethodPost, "https://"+ln.Addr().String()+internalapi.Prefix+"/bootstrap/prepare", strings.NewReader("{}"))
		req.Header.Set(internalapi.HeaderVersion, internalapi.Version)
		return c.Do(req)
	}
	handler := internalapi.New(nil, be, nil, nil)

	// The control: a write timeout counted from the request, as the listener had (60 s), against a call
	// that takes longer. This is the failure; the test below it is the fix.
	old := &http.Server{Handler: handler, TLSConfig: internalapi.TLSConfig(pair), WriteTimeout: time.Second}
	if resp, err := call(old); err == nil {
		_ = resp.Body.Close()
		t.Fatal("the control did not fail: the write timeout of net/http no longer ends a connection under a slow call")
	} else if !strings.Contains(err.Error(), "bad record MAC") {
		t.Logf("the control failed with %v (the text depends on the Go version)", err)
	}

	// The response timeout is shorter than the call, as the 60 s write timeout was for a restart under load.
	resp, err := call(internalapi.NewHTTPServer("", handler, internalapi.TLSConfig(pair), nil, time.Second))
	if err != nil {
		t.Fatalf("a call of %s was not answered: %v", slow, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "yes") {
		t.Fatalf("answer: %d %s", resp.StatusCode, b)
	}
}

// The deadline set at the first byte of the answer ends a peer that does not read it: without it the handler
// would block on the write for as long as the peer holds its end open (a goroutine and a socket per peer).
func TestAPeerThatDoesNotReadTheAnswerIsEnded(t *testing.T) {
	certPEM, keyPEM, err := pki.NewBootstrapCert(pki.Names{CommonName: "127.0.0.1"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("x", 48<<20) // far more than the socket buffers hold
	be := internalapi.Backend{Bootstrap: func(context.Context, string, *http.Request, []byte) (any, error) {
		return map[string]string{"big": big}, nil
	}}
	srv := internalapi.NewHTTPServer("", internalapi.New(nil, be, nil, nil), internalapi.TLSConfig(pair), nil, time.Second)
	closed := make(chan struct{})
	srv.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateClosed {
			select {
			case closed <- struct{}{}:
			default:
			}
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })
	conn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13}) //nolint:gosec // the test's own certificate
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	req := "POST " + internalapi.Prefix + "/bootstrap/prepare HTTP/1.1\r\nHost: x\r\n" + internalapi.HeaderVersion + ": " + internalapi.Version +
		"\r\nContent-Length: 2\r\n\r\n{}"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	// the peer reads nothing; the server must end the connection by itself
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("the server still holds the connection of a peer that does not read the answer")
	}
}

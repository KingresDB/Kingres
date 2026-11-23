// SPDX-License-Identifier: AGPL-3.0-only

package internalapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fence"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fwdproof"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
)

type env struct {
	srv       *httptest.Server
	ca        *pki.CA
	t         *testing.T
	leader    string
	execd     []ExecRequest
	forwarded []string
	// pending are the servers that hold a certificate but are not members;
	// unproven is the error VerifyIdentity answers (nil: the proof is fine).
	pending  map[string]bool
	unproven error
	// gather counts the gather calls that reach the backend; noAssertion makes
	// VerifyAssertion refuse.
	gather      func()
	noAssertion bool
	// stream is the streaming operation of the backend (nil: it counts the body and answers 204).
	stream func(ctx context.Context, peer string, r ForwardRequest, body io.Reader, w http.ResponseWriter) error
}

// cert issues a certificate for cn from ca and returns it as a TLS pair.
func cert(t *testing.T, ca *pki.CA, cn string) tls.Certificate {
	t.Helper()
	n := pki.Names{CommonName: cn}
	key, csr, err := pki.NewKeyAndCSR(n)
	if err != nil {
		t.Fatal(err)
	}
	crt, _, err := ca.Sign(csr, n, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(crt, key)
	if err != nil {
		t.Fatal(err)
	}
	return pair
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ca, err := pki.CreateCA(filepath.Join(t.TempDir(), "pki"), "t", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e := &env{ca: ca, t: t, leader: "leader"}
	be := Backend{
		State: func(context.Context) State {
			return State{ServerID: "leader", NodeState: "clustered", InternalVersion: Version}
		},
		Leader: func() string { return e.leader },
		Self:   "leader",
		Forward: func(_ context.Context, peer string, r ForwardRequest) (ForwardResponse, error) {
			e.forwarded = append(e.forwarded, peer+":"+r.OperationID+":"+r.Identity.Username)
			return ForwardResponse{Status: 201, Header: map[string]string{"Content-Type": "application/json"}, Body: []byte(`{"ok":true}`)}, nil
		},
		ForwardStream: func(ctx context.Context, peer string, r ForwardRequest, body io.Reader, w http.ResponseWriter) error {
			if e.stream != nil {
				return e.stream(ctx, peer, r, body, w)
			}
			n, _ := io.Copy(io.Discard, body)
			w.Header().Set("Upload-Offset", strconv.FormatInt(n, 10))
			w.WriteHeader(http.StatusNoContent)
			return nil
		},
		Member:         func(_ context.Context, peer string) (bool, error) { return !e.pending[peer], nil },
		VerifyIdentity: func(context.Context, fwdproof.Kind, string, ForwardRequest) error { return e.unproven },
		Gather: func(context.Context, string, ForwardRequest) (ForwardResponse, error) {
			if e.gather != nil {
				e.gather()
			}
			return ForwardResponse{Status: 200}, nil
		},
		Authorize: func(context.Context, string, ForwardRequest) (fwdproof.Assertion, error) {
			return fwdproof.Assertion{}, nil
		},
		VerifyAssertion: func(context.Context, string, ForwardRequest) error {
			if e.noAssertion {
				return fwdproof.ErrInvalid
			}
			return nil
		},
		Revoked:   ca.Revoked,
		Fence:     fence.NewRegistry(),
		Resources: func(context.Context) (any, error) { return map[string]int{"cores": 4}, nil },
		Logs:      func(context.Context, map[string]string) (any, error) { return map[string]any{"items": []any{}}, nil },
		Metrics:   func(context.Context) (any, error) { return map[string]any{}, nil },
		Exec: func(_ context.Context, r ExecRequest) (ExecResult, error) {
			e.execd = append(e.execd, r)
			return ExecResult{Columns: []string{"x"}, Rows: [][]any{{1}}, Tag: "SELECT 1"}, nil
		},
	}
	var logs bytes.Buffer
	s := New(ca, be, nil, nil)
	_ = logs
	e.srv = httptest.NewUnstartedServer(s)
	e.srv.TLS = TLSConfig(cert(t, ca, "leader"))
	e.srv.StartTLS()
	t.Cleanup(e.srv.Close)
	return e
}

func (e *env) client(c *tls.Certificate) *http.Client {
	tc := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: e.ca.Pool(), ServerName: "leader"}
	if c != nil {
		tc.Certificates = []tls.Certificate{*c}
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: tc}, Timeout: 10 * time.Second}
}

func (e *env) do(c *http.Client, method, path, version string, body any) (int, map[string]any, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+Prefix+path, rd)
	if version != "" {
		req.Header.Set(HeaderVersion, version)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out, nil
}

func TestAuthorizationTable(t *testing.T) {
	e := newEnv(t)
	leaderC := cert(t, e.ca, "leader")
	peerC := cert(t, e.ca, "server2")
	lc, pc := e.client(&leaderC), e.client(&peerC)
	// Any peer may ask for the state and the revocation list.
	for _, p := range []string{"/state", "/leader", "/revoked"} {
		if st, _, err := e.do(pc, "GET", p, Version, nil); err != nil || st != 200 {
			t.Errorf("peer %s: %d %v", p, st, err)
		}
	}
	// Only the leader may ask for resources, logs, metrics and exec.
	for _, c := range []struct{ m, p string }{{"GET", "/resources"}, {"GET", "/logs"}, {"GET", "/metrics"}, {"POST", "/exec"}} {
		st, out, err := e.do(pc, c.m, c.p, Version, ExecRequest{Database: "d", SQL: "select 1"})
		if err != nil || st != 403 || out["error"] != "not_leader" {
			t.Errorf("non-leader %s %s: %d %v %v", c.m, c.p, st, out, err)
		}
		if st, _, err := e.do(lc, c.m, c.p, Version, ExecRequest{Database: "d", SQL: "select 1"}); err != nil || st != 200 {
			t.Errorf("leader %s %s: %d %v", c.m, c.p, st, err)
		}
	}
	if len(e.execd) != 1 {
		t.Errorf("exec ran %d times: only the leader's call may run", len(e.execd))
	}
	// While the leader is unknown no leader-only call is accepted, not even from "leader".
	e.leader = ""
	if st, _, _ := e.do(lc, "GET", "/resources", Version, nil); st != 403 {
		t.Errorf("unknown leader: %d", st)
	}
	e.leader = "leader"
	// Unknown calls and wrong methods do not exist.
	for _, c := range []struct{ m, p string }{{"GET", "/nope"}, {"DELETE", "/state"}, {"GET", "/exec"}, {"GET", "/state/../state"}, {"GET", "/%2e%2e/etc"}} {
		if st, _, err := e.do(lc, c.m, c.p, Version, nil); err != nil || st != 404 {
			t.Errorf("%s %s: %d %v", c.m, c.p, st, err)
		}
	}
	// Exec input is validated.
	for _, b := range []any{ExecRequest{}, ExecRequest{Database: "d"}, ExecRequest{Database: "d", SQL: "x", RowLimit: 99999}, map[string]any{"database": "d", "sql": "x", "extra": 1}} {
		if st, _, _ := e.do(lc, "POST", "/exec", Version, b); st != 400 {
			t.Errorf("exec body %v: %d", b, st)
		}
	}
}

func TestCertificateAndVersionRefusals(t *testing.T) {
	e := newEnv(t)
	// No client certificate: the handshake succeeds (the bootstrap calls need that) and the call is refused.
	if st, out, err := e.do(e.client(nil), "GET", "/state", Version, nil); err == nil && (st != 401 || out["error"] != "no_certificate") {
		t.Errorf("a call without a certificate was answered: %d %v", st, out)
	}
	// A certificate of another CA.
	other, _ := pki.CreateCA(filepath.Join(t.TempDir(), "pki"), "other", time.Now())
	oc := cert(t, other, "server2")
	if st, out, err := e.do(e.client(&oc), "GET", "/state", Version, nil); err == nil && st == 200 {
		t.Errorf("a certificate of another CA was accepted: %v", out)
	}
	// A revoked certificate.
	rev := cert(t, e.ca, "server3")
	leaf := rev.Leaf
	if leaf == nil {
		leaf, _ = parseLeaf(rev)
	}
	if err := e.ca.Revoke(pki.SerialHex(leaf.SerialNumber), time.Now()); err != nil {
		t.Fatal(err)
	}
	st, out, err := e.do(e.client(&rev), "GET", "/state", Version, nil)
	if err != nil || st != 401 || out["error"] != "certificate_refused" {
		t.Errorf("revoked certificate: %d %v %v", st, out, err)
	}
	// Another internal version, named on both sides; a missing header too.
	good := cert(t, e.ca, "server2")
	for _, v := range []string{"0", "2", ""} {
		st, out, err := e.do(e.client(&good), "GET", "/state", v, nil)
		msg, _ := out["message"].(string)
		if err != nil || st != 409 || out["error"] != "version_mismatch" || !strings.Contains(msg, Version) {
			t.Errorf("version %q: %d %v %v", v, st, out, err)
		}
	}
	// A session or a setup token is not an authentication here.
	req, _ := http.NewRequest("GET", e.srv.URL+Prefix+"/state", nil)
	req.Header.Set("Authorization", "Bearer abc")
	req.Header.Set("X-Setup-Token", "abc")
	req.Header.Set(HeaderVersion, Version)
	if resp, err := e.client(nil).Do(req); err == nil {
		defer resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Errorf("a call with only a bearer token was answered: %d", resp.StatusCode)
		}
	}
}

func TestExecIsLoggedWithLengthAndHashOnly(t *testing.T) {
	ca, _ := pki.CreateCA(filepath.Join(t.TempDir(), "pki"), "t", time.Now())
	var logs bytes.Buffer
	log := newLogger(&logs)
	be := Backend{Leader: func() string { return "leader" }, Exec: func(context.Context, ExecRequest) (ExecResult, error) { return ExecResult{}, nil }}
	s := New(ca, be, log, nil)
	srv := httptest.NewUnstartedServer(s)
	srv.TLS = TLSConfig(cert(t, ca, "leader"))
	srv.StartTLS()
	defer srv.Close()
	lc := cert(t, ca, "leader")
	tc := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: ca.Pool(), ServerName: "leader", Certificates: []tls.Certificate{lc}}
	cl := &http.Client{Transport: &http.Transport{TLSClientConfig: tc}}
	const secret = "select 'SECRET-MARKER-TEXT'"
	b, _ := json.Marshal(ExecRequest{Database: "d", SQL: secret})
	req, _ := http.NewRequest("POST", srv.URL+Prefix+"/exec", bytes.NewReader(b))
	req.Header.Set(HeaderVersion, Version)
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if strings.Contains(logs.String(), "SECRET-MARKER-TEXT") || !strings.Contains(logs.String(), "sql_hash") || !strings.Contains(logs.String(), "sql_length") {
		t.Errorf("log: %s", logs.String())
	}
}

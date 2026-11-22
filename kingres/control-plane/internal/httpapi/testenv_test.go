// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/testutil"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/setup"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store/memstore"
)

// fakeClock is a settable clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type fakeUser struct {
	password string
	roles    []string
}

// fakeAuth is an auth.Authenticator with a fixed user table.
type fakeAuth struct {
	mu    sync.Mutex
	users map[string]fakeUser
	err   error
	calls int
}

func (f *fakeAuth) Authenticate(_ context.Context, user, pass string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	u, ok := f.users[user]
	if !ok || u.password != pass {
		return nil, auth.ErrInvalidCredentials
	}
	return u.roles, nil
}

// env is a fully wired server with fakes.
type env struct {
	t        *testing.T
	srv      *Server
	mem      *memstore.Store
	sessions *auth.Store
	audit    *audit.Memory
	node     *setup.Manager
	clock    *fakeClock
	fa       *fakeAuth
	logs     *bytes.Buffer
}

func newEnv(t *testing.T, mods ...func(*Deps)) *env {
	t.Helper()
	return newEnvWith(t, (*Server).handlers, mods...)
}

// newEnvWith is newEnv with the handler registry given, so that a test can put its own handler where the server has none yet.
func newEnvWith(t *testing.T, registry func(*Server) map[string]handlerFunc, mods ...func(*Deps)) *env {
	t.Helper()
	clock := &fakeClock{t: time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)}
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(&lockedWriter{w: logs}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	node, err := setup.Open(testutil.TempDir(t), clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{
		t: t, mem: memstore.New(), audit: audit.NewMemory(clock.Now), node: node, clock: clock, logs: logs,
		sessions: auth.NewStore(time.Hour, 100, clock.Now),
		fa: &fakeAuth{users: map[string]fakeUser{
			"alice":  {"s3cret", []string{auth.RoleAdmin}},
			"bob":    {"hunter2", []string{auth.RoleReadonly}},
			"nobody": {"pw", nil},
		}},
	}
	d := Deps{
		Logger: logger, Store: e.mem, Setup: node, Sessions: e.sessions, Authenticator: e.fa, Audit: e.audit,
		Version: "test", Hostname: "host1", ListenAddr: "127.0.0.1:8091", LoopbackOnly: true,
		MaxBodyBytes: 1 << 16, RequestTimeout: 5 * time.Second, LoginRateLimit: 5,
		LoginRateWindow: time.Minute, SessionTTL: time.Hour, Now: clock.Now,
	}
	for _, m := range mods {
		m(&d)
	}
	e.srv, err = build(d, registry)
	if err != nil {
		t.Fatal(err)
	}
	e.srv.SetReady(true)
	return e
}

// initialize moves the node out of the uninitialized state.
func (e *env) initialize() {
	e.t.Helper()
	if err := e.node.Transition(setup.Standalone, "7d1f2c0e-3b2a-4d58-9c51-2f6f0a1b9e11", "prod", nil); err != nil {
		e.t.Fatal(err)
	}
}

// lockedWriter makes a bytes.Buffer safe for the logger.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// req describes a test request.
type req struct {
	method, path string
	body         any
	header       map[string]string
	cookie       string
	bearer       string
	remote       string
	// tls makes the request look as if it arrived over TLS.
	tls bool
}

func (e *env) do(r req) *httptest.ResponseRecorder {
	e.t.Helper()
	var rdr io.Reader
	if r.body != nil {
		switch b := r.body.(type) {
		case string:
			rdr = strings.NewReader(b)
		case []byte:
			rdr = bytes.NewReader(b)
		default:
			data, err := json.Marshal(b)
			if err != nil {
				e.t.Fatal(err)
			}
			rdr = bytes.NewReader(data)
		}
	}
	hr := httptest.NewRequest(r.method, r.path, rdr)
	if r.body != nil {
		hr.Header.Set("Content-Type", "application/json")
	}
	for k, v := range r.header {
		hr.Header.Set(k, v)
	}
	if r.cookie != "" {
		hr.AddCookie(&http.Cookie{Name: CookieName, Value: r.cookie})
	}
	if r.bearer != "" {
		hr.Header.Set("Authorization", "Bearer "+r.bearer)
	}
	if r.remote != "" {
		hr.RemoteAddr = r.remote
	} else {
		hr.RemoteAddr = "127.0.0.1:4000"
	}
	if r.tls {
		hr.TLS = &tls.ConnectionState{Version: tls.VersionTLS13}
	}
	w := httptest.NewRecorder()
	e.srv.ServeHTTP(w, hr)
	return w
}

// token opens a bearer session with the given cluster roles and returns the
// token.
func (e *env) token(roles ...string) string {
	e.t.Helper()
	cred, _, err := e.sessions.Create(e.testUser(roles), roles, auth.KindToken)
	if err != nil {
		e.t.Fatal(err)
	}
	return cred
}

// cookieSession opens a cookie session and returns the cookie value and the
// CSRF token.
func (e *env) cookieSession(roles ...string) (cookie, csrf string) {
	e.t.Helper()
	cred, sess, err := e.sessions.Create(e.testUser(roles), roles, auth.KindCookie)
	if err != nil {
		e.t.Fatal(err)
	}
	return cred, sess.CSRFToken
}

// decode parses a JSON response body into a map.
func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("response is not a JSON object: %v\n%s", err, w.Body.String())
	}
	return m
}

// wantProblem asserts status and problem code of a response.
func wantProblem(t *testing.T, w *httptest.ResponseRecorder, status int, code string) map[string]any {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, status, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("content type = %q, want application/problem+json", ct)
	}
	m := decode(t, w)
	if m["code"] != code {
		t.Fatalf("code = %v, want %s; body: %s", m["code"], code, w.Body.String())
	}
	if int(m["status"].(float64)) != status {
		t.Fatalf("body status = %v, want %d", m["status"], status)
	}
	return m
}

// CurrentRoles reads the (mutable) user table: a user that was removed or
// whose roles were changed is seen at once.
func (f *fakeAuth) CurrentRoles(_ context.Context, user string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	u, ok := f.users[user]
	if !ok {
		return nil, auth.ErrInvalidCredentials
	}
	return auth.Normalize(u.roles), nil
}

// testUser registers (in the fake role table) a user for a test session with
// the given cluster roles and returns its name: "tester" for one role,
// "tester-<roles>" otherwise, so that sessions with different roles are
// different users.
func (e *env) testUser(roles []string) string {
	name := "tester"
	if len(roles) != 1 || roles[0] != auth.RoleAdmin {
		name += "-" + strings.Join(roles, "+")
	}
	e.fa.mu.Lock()
	defer e.fa.mu.Unlock()
	if e.fa.users == nil {
		e.fa.users = map[string]fakeUser{}
	}
	e.fa.users[name] = fakeUser{password: "-", roles: roles}
	return name
}

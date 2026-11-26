// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/webui"
)

const (
	wantCSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; " +
		"font-src 'none'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
	wantPermissions = "camera=(), microphone=(), geolocation=()"
	wantHSTS        = "max-age=63072000; includeSubDomains"
)

func panelFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":               {Data: []byte("<html>panel</html>")},
		"favicon.svg":              {Data: []byte("<svg/>")},
		"assets/index-27aDquRx.js": {Data: []byte("console.log(1)")},
	}
}

// TestSecurityHeadersEveryResponseClass checks the exact header values of
// every class of response, with TLS off and on.
func TestSecurityHeadersEveryResponseClass(t *testing.T) {
	classes := []struct {
		name         string
		method, path string
		auth         string // bearer for /metrics
		panics       bool
		status       int
		cache        string
	}{
		{"api success", "GET", "/api/v1/healthz", "", false, 200, "no-store"},
		{"api error", "GET", "/api/v1/auth/session", "", false, 401, "no-store"},
		{"index", "GET", "/", "", false, 200, "no-cache"},
		{"index head", "HEAD", "/", "", false, 200, "no-cache"},
		{"spa fallback", "GET", "/databases/shop/tables", "", false, 200, "no-cache"},
		{"hashed asset", "GET", "/assets/index-27aDquRx.js", "", false, 200, "public, max-age=31536000, immutable"},
		{"non-hashed asset", "GET", "/favicon.svg", "", false, 200, "no-cache"},
		{"missing asset 404", "GET", "/assets/gone-27aDquRx.js", "", false, 404, "no-store"},
		{"unknown api route 404", "GET", "/api/v1/nope", "", false, 404, "no-store"},
		{"method not allowed 405", "POST", "/api/v1/healthz", "", false, 405, "no-store"},
		{"static method 405", "POST", "/databases", "", false, 405, "no-store"},
		{"panic 500", "GET", "/boom", "", true, 500, "no-store"},
		{"metrics 401", "GET", "/metrics", "", false, 401, "no-store"},
		{"metrics 405", "POST", "/metrics", "", false, 405, "no-store"},
		{"metrics token", "GET", "/metrics", "scrape", false, 503, "no-store"},
	}
	for _, tls := range []bool{false, true} {
		for _, tc := range classes {
			name := tc.name + "/tls=off"
			if tls {
				name = tc.name + "/tls=on"
			}
			t.Run(name, func(t *testing.T) {
				e := newEnv(t, func(d *Deps) {
					d.Web = webui.NewHandler(panelFS())
					d.TLS = tls
					d.MetricsToken = "scrape"
				})
				e.initialize()
				var h http.Handler = e.srv
				if tc.panics {
					h = e.srv.chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
				}
				r := httptest.NewRequest(tc.method, tc.path, nil)
				if tc.auth != "" {
					r.Header.Set("Authorization", "Bearer "+tc.auth)
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != tc.status {
					t.Fatalf("status = %d, want %d: %s", w.Code, tc.status, w.Body.String())
				}
				want := map[string]string{
					"Content-Security-Policy":      wantCSP,
					"X-Frame-Options":              "DENY",
					"X-Content-Type-Options":       "nosniff",
					"Referrer-Policy":              "no-referrer",
					"Cross-Origin-Opener-Policy":   "same-origin",
					"Cross-Origin-Resource-Policy": "same-origin",
					"Permissions-Policy":           wantPermissions,
					"Cache-Control":                tc.cache,
				}
				if tls {
					want["Strict-Transport-Security"] = wantHSTS
				} else {
					want["Strict-Transport-Security"] = ""
				}
				for k, v := range want {
					if vals := w.Header().Values(k); v == "" && len(vals) != 0 || v != "" && (len(vals) != 1 || vals[0] != v) {
						t.Errorf("%s = %q, want %q", k, vals, v)
					}
				}
			})
		}
	}
}

// X-Forwarded-Proto must never switch HSTS on over plain HTTP.
func TestHSTSIgnoresForwardedProto(t *testing.T) {
	e := newEnv(t)
	r := httptest.NewRequest("GET", "/api/v1/healthz", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	e.srv.ServeHTTP(w, r)
	if got := w.Header().Get("Strict-Transport-Security"); got != "" {
		t.Fatalf("HSTS = %q over plain HTTP", got)
	}
}

// A handler that sets its own Cache-Control keeps it; one that does not gets
// no-store.
func TestCacheControlDefaultDoesNotOverride(t *testing.T) {
	e := newEnv(t)
	for _, tc := range []struct{ set, want string }{{"", "no-store"}, {"no-cache", "no-cache"}} {
		h := e.srv.chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if tc.set != "" {
				w.Header().Set("Cache-Control", tc.set)
			}
			_, _ = w.Write([]byte("x"))
		}))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))
		if got := w.Header().Get("Cache-Control"); got != tc.want {
			t.Errorf("set %q: Cache-Control = %q, want %q", tc.set, got, tc.want)
		}
	}
}

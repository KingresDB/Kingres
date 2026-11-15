// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func sessionCookieOf(t *testing.T, w *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no cookie %q in %v", name, w.Result().Header["Set-Cookie"])
	return nil
}

func TestSessionCookieAttributes(t *testing.T) {
	for _, tc := range []struct {
		tls  bool
		name string
		path string
	}{
		{false, "kingres_session", "/"},
		{true, "kingres_session", "/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, func(d *Deps) { d.TLS = tc.tls })
			e.initialize()
			do := func(method, path, body string, c *http.Cookie, hdr map[string]string) *httptest.ResponseRecorder {
				r := httptest.NewRequest(method, path, strings.NewReader(body))
				r.Header.Set("Content-Type", "application/json")
				for k, v := range hdr {
					r.Header.Set(k, v)
				}
				if c != nil {
					r.AddCookie(c)
				}
				w := httptest.NewRecorder()
				e.srv.ServeHTTP(w, r)
				return w
			}
			w := do("POST", "/api/v1/auth/login", `{"username":"alice","password":"s3cret"}`, nil, nil)
			if w.Code != 200 {
				t.Fatalf("login: %d %s", w.Code, w.Body.String())
			}
			ck := sessionCookieOf(t, w, tc.name)
			if !ck.HttpOnly || !ck.Secure || ck.SameSite != http.SameSiteStrictMode || ck.Path != tc.path || ck.Domain != "" || ck.Value == "" {
				t.Fatalf("cookie attributes: %+v", ck)
			}
			if len(w.Result().Header["Set-Cookie"]) != 1 {
				t.Errorf("more than one Set-Cookie: %v", w.Result().Header["Set-Cookie"])
			}
			csrf := decode(t, w)["csrfToken"].(string)

			// The server accepts the cookie name it sets.
			if w := do("GET", "/api/v1/auth/session", "", &http.Cookie{Name: tc.name, Value: ck.Value}, nil); w.Code != 200 {
				t.Fatalf("session with %s: %d", tc.name, w.Code)
			}
			// Logout clears the cookie with the same name, Path and no Domain.
			w = do("POST", "/api/v1/auth/logout", "", &http.Cookie{Name: tc.name, Value: ck.Value}, map[string]string{"X-CSRF-Token": csrf})
			if w.Code != 204 {
				t.Fatalf("logout: %d %s", w.Code, w.Body.String())
			}
			cl := sessionCookieOf(t, w, tc.name)
			if cl.MaxAge >= 0 || cl.Value != "" || cl.Path != tc.path || cl.Domain != "" || !cl.Secure || !cl.HttpOnly || cl.SameSite != http.SameSiteStrictMode {
				t.Fatalf("clearing cookie: %+v", cl)
			}
		})
	}
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":             {Data: []byte("<html>panel</html>")},
		"assets/app-Ab3dEf9x.js": {Data: []byte("console.log(1)")},
		"assets/app.js":          {Data: []byte("console.log(1)")},
		"favicon.ico":            {Data: []byte("ico")},
		"deep/page/x.html":       {Data: []byte("x")},
	}
}

func get(h http.Handler, method, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w
}

func TestSPAFallbackAndFiles(t *testing.T) {
	h := NewHandler(testFS())
	tests := []struct {
		path   string
		status int
		body   string
		cache  string
	}{
		{"/", 200, "panel", "no-cache"},
		{"/databases", 200, "panel", "no-cache"},
		{"/databases/shop/tables", 200, "panel", "no-cache"},
		{"/assets/app-Ab3dEf9x.js", 200, "console.log", "immutable"},
		{"/assets/app.js", 200, "console.log", "no-cache"},
		{"/favicon.ico", 200, "ico", "no-cache"},
		{"/assets/missing.js", 404, "not_found", ""},
		{"/missing.css", 404, "not_found", ""},
	}
	for _, tc := range tests {
		w := get(h, "GET", tc.path)
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.body) {
			t.Errorf("GET %s = %d %q", tc.path, w.Code, w.Body.String())
		}
		if tc.cache != "" && !strings.Contains(w.Header().Get("Cache-Control"), tc.cache) {
			t.Errorf("GET %s Cache-Control = %q", tc.path, w.Header().Get("Cache-Control"))
		}
	}
	if w := get(h, "HEAD", "/databases"); w.Code != 200 || w.Body.Len() != 0 {
		t.Errorf("HEAD fallback = %d len %d", w.Code, w.Body.Len())
	}
	if w := get(h, "POST", "/"); w.Code != 405 || w.Header().Get("Allow") == "" {
		t.Errorf("POST = %d", w.Code)
	}
}

// TestAPIPathsAreNeverShadowed checks that the SPA fallback does not answer
// for anything under /api, even if it reaches the handler directly.
func TestAPIPathsAreNeverShadowed(t *testing.T) {
	h := NewHandler(testFS())
	for _, p := range []string{"/api", "/api/", "/api/v1/unknown", "/api/v1/servers/", "/api/../api/x"} {
		w := get(h, "GET", p)
		if w.Code != 404 || strings.Contains(w.Body.String(), "panel") {
			t.Errorf("GET %s = %d %q", p, w.Code, w.Body.String())
		}
		if w.Header().Get("Content-Type") != "application/problem+json" {
			t.Errorf("GET %s content type %q", p, w.Header().Get("Content-Type"))
		}
	}
}

func TestEmbeddedPlaceholder(t *testing.T) {
	h, err := Handler()
	if err != nil {
		t.Fatal(err)
	}
	if w := get(h, "GET", "/some/route"); w.Code != 200 || !strings.Contains(w.Body.String(), "KingresDB") {
		t.Errorf("embedded fallback = %d %q", w.Code, w.Body.String())
	}
}

// SPDX-License-Identifier: PostgreSQL

package httpapi

import "net/http"

// The security headers of every response (see "Headers the server sends" in
// the README). The panel is served from the same origin as the API, loads no
// third-party resource, and has no inline script or style.
const (
	// contentSecurityPolicy lets the panel load scripts, styles, images and
	// fetch targets from its own origin only. There is no 'unsafe-inline',
	// 'unsafe-eval', data: or blob: source, and no font (the panel uses system
	// fonts). frame-ancestors cannot be set from a meta tag, so it is sent here.
	contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; " +
		"font-src 'none'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

	// permissionsPolicy switches off the browser features the panel never uses.
	permissionsPolicy = "camera=(), microphone=(), geolocation=()"

	// hstsValue is sent over TLS only. No "preload": preload submission is
	// effectively irreversible and covers every subdomain of the host name,
	// which an operator of an internal installation cannot promise.
	hstsValue = "max-age=63072000; includeSubDomains"
)

// securityHeaders sets the security headers on every response, whatever
// writes it (a handler, the problem writer, the static file server, the panic
// recoverer), and gives a response that has no Cache-Control the default
// "no-store". It sits outside the access log and the recoverer so that the
// recoverer's 500 is covered too. TLS comes from the server configuration
// (Deps.TLS), never from a request header such as X-Forwarded-Proto.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", permissionsPolicy)
		// COOP isolates the browsing context group; CORP stops other origins
		// from embedding any response (the API, the panel files, /metrics).
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		if s.deps.TLS {
			h.Set("Strict-Transport-Security", hstsValue)
		}
		next.ServeHTTP(&cacheDefaultWriter{ResponseWriter: w}, r)
	})
}

// cacheDefaultWriter adds "Cache-Control: no-store" when the response is
// about to be sent without a Cache-Control header, so that no response class
// is cacheable by accident. A handler that sets its own value keeps it.
type cacheDefaultWriter struct {
	http.ResponseWriter
	sent bool
}

func (w *cacheDefaultWriter) fill() {
	if w.sent {
		return
	}
	w.sent = true
	if h := w.ResponseWriter.Header(); h.Get("Cache-Control") == "" {
		h.Set("Cache-Control", "no-store")
	}
}

func (w *cacheDefaultWriter) WriteHeader(code int) {
	if code >= 200 || code == http.StatusSwitchingProtocols {
		w.fill()
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *cacheDefaultWriter) Write(b []byte) (int, error) {
	w.fill()
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *cacheDefaultWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

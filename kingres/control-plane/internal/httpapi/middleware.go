// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"runtime/debug"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

// reqState is the per-request record shared between the middleware chain and
// the guard: the request id, and what the guard learns (actor, operation) so
// that the access log can include it.
type reqState struct {
	id        string
	operation string
	actor     string
}

type ctxKey int

const keyState ctxKey = iota

func stateOf(r *http.Request) *reqState {
	if st, ok := r.Context().Value(keyState).(*reqState); ok {
		return st
	}
	return &reqState{}
}

// statusWriter records the status and size of a response.
type statusWriter struct {
	http.ResponseWriter
	status  int
	bytes   int64
	written bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.written {
		w.status = code
		w.written = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.written {
		w.status = http.StatusOK
		w.written = true
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// chain wraps the mux with the outer middleware: request id,
// security headers (headers.go), access log and panic recovery (outermost first). The
// recoverer sits inside the access log so that a panicking request is still
// logged, with status 500.
func (s *Server) chain(next http.Handler) http.Handler {
	return s.requestID(s.securityHeaders(s.accessLog(s.recoverer(next))))
}

// requestID assigns the request id, taken from a well-formed incoming
// X-Request-Id header or generated, and echoes it in the response.
func (s *Server) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if !validRequestID(id) {
			id = newRequestID()
		}
		w.Header().Set("X-Request-Id", id)
		st := &reqState{id: id}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), keyState, st)))
	})
}

func validRequestID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}

func newRequestID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b)
}

// accessLog writes one structured line per request after it completes. It
// logs the path without the query string and never headers or bodies.
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		st := stateOf(r)
		attrs := []any{
			"request_id", st.id, "method", r.Method, "path", r.URL.Path,
			"status", sw.status, "bytes", sw.bytes,
			"duration_ms", s.now().Sub(start).Milliseconds(), "remote", clientIP(r),
		}
		if st.operation != "" {
			attrs = append(attrs, "operation", st.operation)
		}
		if st.actor != "" {
			attrs = append(attrs, "actor", st.actor)
		}
		// 503 is an expected answer (uninitialized node, PostgreSQL down), not a
		// server fault.
		level := s.log.Info
		switch {
		case sw.status == http.StatusServiceUnavailable:
			level = s.log.Warn
		case sw.status >= 500:
			level = s.log.Error
		}
		level("request", attrs...)
	})
}

// recoverer turns a panic in a handler into a 500 problem and logs the stack.
// http.ErrAbortHandler is re-raised, as the net/http contract requires.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(rec)
			}
			s.log.Error("panic in handler", "request_id", w.Header().Get("X-Request-Id"),
				"method", r.Method, "path", r.URL.Path, "panic", rec, "stack", string(debug.Stack()))
			if sw, ok := w.(*statusWriter); ok && sw.written {
				return // the response is already partially sent
			}
			writeProblem(w, r, problem.New(problem.InternalError, "An unexpected error occurred."))
		}()
		next.ServeHTTP(w, r)
	})
}

// clientIP is the host part of the peer address. Forwarded-for headers are
// deliberately not trusted: there is no trusted-proxy configuration yet.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

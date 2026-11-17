// SPDX-License-Identifier: AGPL-3.0-only

package internalapi

import (
	"crypto/tls"
	"log"
	"net/http"
	"time"
)

// ResponseTimeout bounds the write of an answer to a call, not the call itself.
//
// The calls of this API are of any length: a statement that runs for minutes, an instance that is
// started or restarted, a standby that is created. A net/http WriteTimeout runs from the moment
// the request was read, so it would end the connection under a call that takes longer, and a
// connection that ends that way is not closed cleanly: the answer was never written, the record
// counter of the TLS connection has moved on, and the close alert that follows makes the caller fail
// with "tls: bad record MAC" instead of a timeout. So the server has no WriteTimeout, and the deadline
// of the write is set when the answer is about to be written (see deadlineWriter), which still ends
// a peer that does not read it.
const ResponseTimeout = 30 * time.Second

// NewHTTPServer is the server of the internal listener: h behind the mutual TLS of tc, with limits on
// reading the request (header 10 s, whole request 30 s), on writing the answer (responseTimeout, counted
// from the first byte of the answer) and on an idle connection (2 min), and no limit on the time a call takes.
func NewHTTPServer(addr string, h http.Handler, tc *tls.Config, errorLog *log.Logger, responseTimeout time.Duration) *http.Server {
	return &http.Server{
		Addr: addr, Handler: lateDeadline{next: h, d: responseTimeout}, TLSConfig: tc,
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second,
		MaxHeaderBytes: 64 << 10, ErrorLog: errorLog,
	}
}

type lateDeadline struct {
	next http.Handler
	d    time.Duration
}

func (l lateDeadline) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	l.next.ServeHTTP(&deadlineWriter{ResponseWriter: w, rc: http.NewResponseController(w), d: l.d}, r)
}

// deadlineWriter sets the write deadline of the connection at the first byte of the answer.
type deadlineWriter struct {
	http.ResponseWriter
	rc    *http.ResponseController
	d     time.Duration
	armed bool
}

func (w *deadlineWriter) arm() {
	if !w.armed {
		w.armed = true
		_ = w.rc.SetWriteDeadline(time.Now().Add(w.d))
	}
}

func (w *deadlineWriter) WriteHeader(code int) { w.arm(); w.ResponseWriter.WriteHeader(code) }

func (w *deadlineWriter) Write(b []byte) (int, error) { w.arm(); return w.ResponseWriter.Write(b) }

// Unwrap lets http.ResponseController reach the connection (hijack, flush, deadlines).
func (w *deadlineWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

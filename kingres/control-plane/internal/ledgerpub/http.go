// SPDX-License-Identifier: AGPL-3.0-only

package ledgerpub

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"
)

// Cache-Control values of design 4.5: tiles, bundles and events never change; the checkpoint, the key ring and the listings do.
const (
	CacheImmutable = "public, max-age=31536000, immutable"
	CacheMutable   = "public, max-age=5"
	// CacheMiss is for a 404: a partial tile or a checkpoint may appear a moment later, so a miss is not kept.
	CacheMiss = "public, max-age=5"
)

// Defaults of the limits of the listener (design 4.5).
const (
	DefaultMaxConcurrent = 200
	DefaultRatePerSecond = 50
)

// Objects reads one file of the publication: the path is parsed already. ErrNotFound for a file that is not there.
type Objects func(ctx context.Context, p LogPath) (Object, error)

// Handler serves the files of a publication on GET and HEAD, and nothing else: no listing, no redirect, no cookie, no
// authentication (the files are public by design), and no path that is not the layout of 4.6. It never touches a user database.
type Handler struct {
	Objects Objects
	// MaxConcurrent requests are served at once (DefaultMaxConcurrent when zero); more get 503 with Retry-After.
	MaxConcurrent int
	// RatePerSecond is the rate per client address (DefaultRatePerSecond when zero); a burst of the same size is allowed.
	RatePerSecond int
	// Now is the clock of the rate limit (default time.Now).
	Now func() time.Time
	Log *slog.Logger

	once sync.Once
	sem  chan struct{}
	mu   sync.Mutex
	rate map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func (h *Handler) init() {
	h.once.Do(func() {
		n := h.MaxConcurrent
		if n <= 0 {
			n = DefaultMaxConcurrent
		}
		h.sem = make(chan struct{}, n)
		h.rate = map[string]*bucket{}
		if h.Now == nil {
			h.Now = time.Now
		}
	})
}

// allow takes one token of the address; false when the address is over its rate.
func (h *Handler) allow(addr string) bool {
	per := float64(h.RatePerSecond)
	if per <= 0 {
		per = DefaultRatePerSecond
	}
	now := h.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	b := h.rate[addr]
	if b == nil {
		if len(h.rate) >= 4096 {
			// bound the table: forget the addresses that have not asked for a while
			for k, v := range h.rate {
				if now.Sub(v.last) > 10*time.Second {
					delete(h.rate, k)
				}
			}
			// still full: the entry that asked longest ago makes room, so that a flood of addresses cannot keep a new auditor out (P8-05)
			if len(h.rate) >= 4096 {
				var oldest string
				var oldestAt time.Time
				for k, v := range h.rate {
					if oldest == "" || v.last.Before(oldestAt) {
						oldest, oldestAt = k, v.last
					}
				}
				delete(h.rate, oldest)
			}
		}
		b = &bucket{tokens: per, last: now}
		h.rate[addr] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * per
	if b.tokens > per {
		b.tokens = per
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func plain(w http.ResponseWriter, status int, cache, msg string) {
	hd := w.Header()
	hd.Set("Content-Type", "text/plain; charset=utf-8")
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Cache-Control", cache)
	w.WriteHeader(status)
	_, _ = w.Write([]byte(msg + "\n"))
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.init()
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		plain(w, http.StatusMethodNotAllowed, "no-store", "method not allowed")
		return
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if !h.allow(rateKey(host)) {
		w.Header().Set("Retry-After", "1")
		plain(w, http.StatusTooManyRequests, "no-store", "too many requests")
		return
	}
	select {
	case h.sem <- struct{}{}:
		defer func() { <-h.sem }()
	default:
		w.Header().Set("Retry-After", "1")
		plain(w, http.StatusServiceUnavailable, "no-store", "busy")
		return
	}
	// Only the canonical path of the layout is served: a request that needed decoding (RawPath is set when the escaped form
	// differs from the decoded one), an empty or dot element, or a path that does not start with a slash is a 404, never
	// a redirect to a cleaned path.
	if r.URL.RawPath != "" || len(r.URL.Path) < 2 || r.URL.Path[0] != '/' {
		plain(w, http.StatusNotFound, CacheMiss, "not found")
		return
	}
	lp, err := ParsePath(r.URL.Path[1:])
	if err != nil {
		plain(w, http.StatusNotFound, CacheMiss, "not found")
		return
	}
	obj, err := h.Objects(r.Context(), lp)
	switch {
	case errors.Is(err, ErrNotFound):
		plain(w, http.StatusNotFound, CacheMiss, "not found")
		return
	case err != nil:
		if h.Log != nil && r.Context().Err() == nil {
			h.Log.Warn("ledger publication: reading an object", "error", err)
		}
		w.Header().Set("Retry-After", "5")
		plain(w, http.StatusServiceUnavailable, "no-store", "unavailable")
		return
	}
	hd := w.Header()
	ct := obj.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	hd.Set("Content-Type", ct)
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Access-Control-Allow-Origin", "*")
	if obj.Mutable {
		hd.Set("Cache-Control", CacheMutable)
	} else {
		hd.Set("Cache-Control", CacheImmutable)
	}
	hd.Set("Content-Length", strconv.Itoa(len(obj.Data)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(obj.Data)
	}
}

// Listener runs a Handler on an address, with TLS when a certificate is configured. It reports what the API shows
// (address, tls, running).
type Listener struct {
	Addr    string
	TLSCert string
	TLSKey  string
	Handler http.Handler
	Log     *slog.Logger

	mu      sync.Mutex
	srv     *http.Server
	bound   string
	running bool
	done    chan struct{}
}

// Status is what getLedgerPublication reports about the listener: the configured address ("" for none), whether it uses TLS and
// whether it runs.
func (l *Listener) Status() (address string, tlsOn, running bool) {
	if l == nil {
		return "", false, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.Addr, l.TLSCert != "" && l.TLSKey != "", l.running
}

// BoundAddr is the address the listener is bound to (after Start), "" before.
func (l *Listener) BoundAddr() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.bound
}

// Start binds the address and serves in the background until Stop or until ctx ends. It returns the bind error at once.
func (l *Listener) Start(ctx context.Context) error {
	var cfg *tls.Config
	if l.TLSCert != "" || l.TLSKey != "" {
		cert, err := tls.LoadX509KeyPair(l.TLSCert, l.TLSKey)
		if err != nil {
			return err
		}
		cfg = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", l.Addr)
	if err != nil {
		return err
	}
	if cfg != nil {
		ln = tls.NewListener(ln, cfg)
	}
	srv := &http.Server{
		Handler:           l.Handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	if l.Log != nil {
		srv.ErrorLog = slog.NewLogLogger(l.Log.Handler(), slog.LevelWarn)
	}
	l.mu.Lock()
	l.srv, l.bound, l.running, l.done = srv, ln.Addr().String(), true, make(chan struct{})
	done := l.done
	l.mu.Unlock()
	go func() {
		defer close(done)
		err := srv.Serve(ln)
		l.mu.Lock()
		l.running = false
		l.mu.Unlock()
		if err != nil && !errors.Is(err, http.ErrServerClosed) && l.Log != nil {
			l.Log.Error("ledger publication listener stopped", "error", err)
		}
	}()
	go func() {
		select {
		case <-ctx.Done():
			l.Stop()
		case <-done:
		}
	}()
	return nil
}

// Stop shuts the listener down and waits for it.
func (l *Listener) Stop() {
	l.mu.Lock()
	srv, done := l.srv, l.done
	l.mu.Unlock()
	if srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	<-done
}

// rateKey is the key of the rate table for a client address: an IPv4 address as it is, an IPv6 address by its /64 network (one customer
// holds a whole /64, so an address each is no new client).
func rateKey(host string) string {
	a, err := netip.ParseAddr(host)
	if err != nil || a.Is4() || a.Is4In6() {
		return host
	}
	p, err := a.Prefix(64)
	if err != nil {
		return host
	}
	return p.String()
}

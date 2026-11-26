// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

// Package clientproxy is the client entry point of a server (design ha-phase5.md,
// part D): it accepts PostgreSQL connections on one port and forwards each of
// them, byte for byte from the first packet, to a PostgreSQL instance that it
// chooses. It chooses a destination and nothing else.
//
// What it never does: read, parse, buffer or log what the client sends (the
// startup packet, an SSLRequest, SCRAM messages and SQL are all just bytes, so
// TLS negotiation, authentication and client certificates are PostgreSQL's own,
// end to end between client and instance); keep a connection to a destination
// that is no longer the right one (Sweep closes it); or accept more connections
// than its cap.
//
// Everything that depends on the cluster (which instance is the primary, whether
// this server holds the lease) is behind Resolver, so the package knows nothing
// of the catalog or of leases and is tested with fakes.
package clientproxy

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Destination is where new connections go. Key identifies the destination for
// change detection (two destinations with the same Key are the same place).
type Destination struct {
	Addr string
	Key  string
}

// ErrNoDestination means there is no instance to forward to right now; new
// connections are closed and open ones are closed at the next Sweep.
var ErrNoDestination = errors.New("clientproxy: no destination")

// Resolver chooses the destination. It is called for every new connection and
// at every Sweep, so it must be cheap (cached or a local read).
type Resolver func(ctx context.Context) (Destination, error)

// Clock gives the time that idle connections are measured with (a monotonic
// reading); tests inject one.
type Clock func() time.Duration

// Config configures a Proxy.
type Config struct {
	// Resolve chooses the destination.
	Resolve Resolver
	// Dial opens the connection to a destination (default: TCP).
	Dial func(ctx context.Context, addr string) (net.Conn, error)
	// MaxConns caps the connections forwarded at once; a connection over the cap
	// is closed at once. 0 means 1000. Shared makes several proxies count against
	// one cap.
	MaxConns int
	Shared   *Limit
	// ConnectTimeout bounds resolving and connecting (default 5s).
	ConnectTimeout time.Duration
	// IdleTimeout closes a connection with no byte in either direction for that
	// long (default 1h).
	IdleTimeout time.Duration
	// Recheck is how often the destination of the open connections is looked at
	// again and idle ones are closed (default 1s).
	Recheck time.Duration
	// Now is the monotonic clock (default the process clock).
	Now Clock
	// OnEvent receives a short reason for each refused, failed or moved connection
	// (never an address of a client, never data).
	OnEvent func(event string)
}

// Limit is a connection cap that several proxies can share.
type Limit struct {
	max int
	n   atomic.Int64
}

// NewLimit returns a cap of max connections.
func NewLimit(max int) *Limit { return &Limit{max: max} }

func (l *Limit) acquire() bool {
	for {
		n := l.n.Load()
		if int(n) >= l.max {
			return false
		}
		if l.n.CompareAndSwap(n, n+1) {
			return true
		}
	}
}

func (l *Limit) release() { l.n.Add(-1) }

// Stats are the counters of a proxy; all but Active only grow.
type Stats struct {
	Accepted, Active, RefusedOverCap, ConnectFailures, ClosedOnMove, ClosedIdle, BytesIn, BytesOut int64
}

// Proxy is one listening entry point.
type Proxy struct {
	cfg   Config
	limit *Limit

	accepted, refused, connectFail, moved, idled, bytesIn, bytesOut, active atomic.Int64

	mu    sync.Mutex
	conns map[*conn]struct{}
	ln    net.Listener
	wg    sync.WaitGroup
}

// New returns a proxy; it listens when Serve is called.
func New(cfg Config) *Proxy {
	if cfg.MaxConns <= 0 {
		cfg.MaxConns = 1000
	}
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = 5 * time.Second
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = time.Hour
	}
	if cfg.Recheck <= 0 {
		cfg.Recheck = time.Second
	}
	if cfg.Now == nil {
		start := time.Now()
		cfg.Now = func() time.Duration { return time.Since(start) }
	}
	if cfg.Dial == nil {
		cfg.Dial = func(ctx context.Context, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		}
	}
	p := &Proxy{cfg: cfg, limit: cfg.Shared, conns: map[*conn]struct{}{}}
	if p.limit == nil {
		p.limit = NewLimit(cfg.MaxConns)
	}
	return p
}

type conn struct {
	client, dest net.Conn
	key          string
	last         atomic.Int64 // monotonic nanoseconds of the last byte in either direction
	once         sync.Once
	why          atomic.Value // string: why the proxy closed it
}

func (c *conn) close(why string) {
	c.once.Do(func() {
		c.why.Store(why)
		_ = c.client.Close()
		_ = c.dest.Close()
	})
}

// Stats returns the counters.
func (p *Proxy) Stats() Stats {
	return Stats{Accepted: p.accepted.Load(), Active: p.active.Load(), RefusedOverCap: p.refused.Load(),
		ConnectFailures: p.connectFail.Load(), ClosedOnMove: p.moved.Load(), ClosedIdle: p.idled.Load(),
		BytesIn: p.bytesIn.Load(), BytesOut: p.bytesOut.Load()}
}

func (p *Proxy) event(s string) {
	if p.cfg.OnEvent != nil {
		p.cfg.OnEvent(s)
	}
}

// Serve accepts connections on ln until ctx ends, then closes the listener and
// every forwarded connection and waits for them.
func (p *Proxy) Serve(ctx context.Context, ln net.Listener) error {
	p.wg.Add(1)
	go func() { defer p.wg.Done(); p.sweepLoop(ctx) }()
	go func() { <-ctx.Done(); _ = ln.Close() }()
	var err error
	for {
		var c net.Conn
		c, err = ln.Accept()
		if err != nil {
			break
		}
		p.accepted.Add(1)
		if !p.limit.acquire() {
			p.refused.Add(1)
			p.event("refused: connection cap reached")
			_ = c.Close()
			continue
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			defer p.limit.release()
			p.handle(ctx, c)
		}()
	}
	p.closeAll("shutdown")
	p.wg.Wait()
	if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func (p *Proxy) handle(ctx context.Context, client net.Conn) {
	cctx, cancel := context.WithTimeout(context.WithValue(ctx, remoteKey{}, client.RemoteAddr()), p.cfg.ConnectTimeout)
	d, err := p.cfg.Resolve(cctx)
	var dest net.Conn
	if err == nil {
		dest, err = p.cfg.Dial(cctx, d.Addr)
	}
	cancel()
	if err != nil {
		p.connectFail.Add(1)
		p.event("connect failed")
		_ = client.Close()
		return
	}
	c := &conn{client: client, dest: dest, key: d.Key}
	c.last.Store(int64(p.cfg.Now()))
	p.mu.Lock()
	if ctx.Err() != nil {
		p.mu.Unlock()
		c.close("shutdown")
		return
	}
	p.conns[c] = struct{}{}
	p.mu.Unlock()
	p.active.Add(1)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); p.pipe(c, client, dest, &p.bytesOut) }() // instance to client
	go func() { defer wg.Done(); p.pipe(c, dest, client, &p.bytesIn) }()  // client to instance
	wg.Wait()
	c.close("closed")
	p.active.Add(-1)
	p.mu.Lock()
	delete(p.conns, c)
	p.mu.Unlock()
	switch c.why.Load() {
	case "moved":
		p.moved.Add(1)
		p.event("closed: the destination changed")
	case "idle":
		p.idled.Add(1)
		p.event("closed: idle")
	}
}

// pipe copies src to dst and counts the bytes. When src ends, both sides are
// closed: PostgreSQL has no half-closed state worth keeping (the instance ends a
// session by closing it, a client by sending Terminate and closing).
func (p *Proxy) pipe(c *conn, dst, src net.Conn, counter *atomic.Int64) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			c.last.Store(int64(p.cfg.Now()))
			counter.Add(int64(n))
			if _, werr := dst.Write(buf[:n]); werr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	c.close("closed")
}

func (p *Proxy) closeAll(why string) {
	p.mu.Lock()
	cs := make([]*conn, 0, len(p.conns))
	for c := range p.conns {
		cs = append(cs, c)
	}
	p.mu.Unlock()
	for _, c := range cs {
		c.close(why)
	}
}

func (p *Proxy) sweepLoop(ctx context.Context) {
	t := time.NewTicker(p.cfg.Recheck)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.Sweep(ctx)
		}
	}
}

// Sweep closes the connections that went idle and those whose destination is no
// longer the one Resolve gives now. A resolver that fails with anything but
// ErrNoDestination closes nothing: not knowing is not a reason to cut users off
// (a lost lease makes the resolver name another destination, which is).
func (p *Proxy) Sweep(ctx context.Context) {
	now := int64(p.cfg.Now())
	idle := int64(p.cfg.IdleTimeout)
	cctx, cancel := context.WithTimeout(ctx, p.cfg.ConnectTimeout)
	cur, err := p.cfg.Resolve(cctx)
	cancel()
	gone := errors.Is(err, ErrNoDestination)
	p.mu.Lock()
	var move, stale []*conn
	for c := range p.conns {
		switch {
		case now-c.last.Load() > idle:
			stale = append(stale, c)
		case gone, err == nil && c.key != cur.Key:
			move = append(move, c)
		}
	}
	p.mu.Unlock()
	for _, c := range stale {
		c.close("idle")
	}
	for _, c := range move {
		c.close("moved")
	}
}

type remoteKey struct{}

// RemoteAddr is the address of the client whose connection a Resolver is called
// for; nil when it is called for a sweep. A resolver may use it only to decide
// where to forward (never to look at what the client sends).
func RemoteAddr(ctx context.Context) net.Addr {
	a, _ := ctx.Value(remoteKey{}).(net.Addr)
	return a
}

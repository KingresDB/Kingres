// SPDX-License-Identifier: AGPL-3.0-only

package clientproxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeInstance accepts connections, records the bytes it receives from each and
// answers with whatever the test wrote to it.
type fakeInstance struct {
	ln    net.Listener
	mu    sync.Mutex
	conns []net.Conn
	got   chan []byte
}

func newInstance(t *testing.T, echo bool) *fakeInstance {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeInstance{ln: ln, got: make(chan []byte, 16)}
	t.Cleanup(func() { _ = ln.Close(); f.closeAll() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.conns = append(f.conns, c)
			f.mu.Unlock()
			if echo {
				go func() { _, _ = io.Copy(c, c) }()
			}
		}
	}()
	return f
}

func (f *fakeInstance) closeAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.conns {
		_ = c.Close()
	}
}

func (f *fakeInstance) addr() string { return f.ln.Addr().String() }

// start runs a proxy on a loopback port with the resolver and returns its address.
func start(t *testing.T, cfg Config) (*Proxy, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = p.Serve(ctx, ln); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return p, ln.Addr().String()
}

func fixed(a *fakeInstance, key string) Resolver {
	return func(context.Context) (Destination, error) { return Destination{Addr: a.addr(), Key: key}, nil }
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// readClosed reports whether the connection is closed by the other side.
func readClosed(c net.Conn) bool {
	_ = c.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	_, err := c.Read(make([]byte, 1))
	var ne net.Error
	return err != nil && !(errors.As(err, &ne) && ne.Timeout())
}

// TestBytesPassUnchangedBothWays sends random bytes, among them the eight bytes of
// an SSLRequest as the very first thing, and expects exactly the same bytes back
// from an echoing instance: the proxy neither reads nor changes anything, and it
// never speaks first.
func TestBytesPassUnchangedBothWays(t *testing.T) {
	inst := newInstance(t, true)
	p, addr := start(t, Config{Resolve: fixed(inst, "a")})
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Nothing arrives that the instance did not send.
	_ = c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if n, _ := c.Read(make([]byte, 1)); n != 0 {
		t.Fatal("the proxy sent a byte of its own")
	}
	msg := append([]byte{0, 0, 0, 8, 4, 210, 22, 47}, make([]byte, 1<<20)...) // an SSLRequest, then noise
	if _, err := rand.Read(msg[8:]); err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = c.Write(msg) }()
	got := make([]byte, len(msg))
	_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatal("the bytes changed on the way")
	}
	if s := p.Stats(); s.Accepted != 1 {
		t.Fatalf("stats %+v", s)
	}
	eventually(t, "byte counters", func() bool { s := p.Stats(); return s.BytesIn >= int64(len(msg)) && s.BytesOut >= int64(len(msg)) })
}

func TestConnectionCapClosesTheExcessAtOnce(t *testing.T) {
	inst := newInstance(t, true)
	p, addr := start(t, Config{Resolve: fixed(inst, "a"), MaxConns: 2})
	open := func() net.Conn {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	a, b := open(), open()
	eventually(t, "two active", func() bool { return p.Stats().Active == 2 })
	c := open()
	eventually(t, "the third is closed", func() bool { return readClosed(c) })
	if s := p.Stats(); s.RefusedOverCap != 1 || s.Active != 2 {
		t.Fatalf("stats %+v", s)
	}
	_ = a.Close()
	eventually(t, "a slot is free again", func() bool { return p.Stats().Active == 1 })
	d := open()
	_, _ = d.Write([]byte("x"))
	_ = d.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(d, make([]byte, 1)); err != nil {
		t.Fatalf("a connection under the cap must work: %v", err)
	}
	_ = b
}

func TestSharedCapCountsBothProxies(t *testing.T) {
	inst := newInstance(t, true)
	lim := NewLimit(1)
	_, a1 := start(t, Config{Resolve: fixed(inst, "a"), Shared: lim})
	p2, a2 := start(t, Config{Resolve: fixed(inst, "a"), Shared: lim})
	c1, _ := net.Dial("tcp", a1)
	defer c1.Close()
	_, _ = c1.Write([]byte("x"))
	_ = c1.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(c1, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	c2, _ := net.Dial("tcp", a2)
	defer c2.Close()
	eventually(t, "the second proxy refuses", func() bool { return readClosed(c2) && p2.Stats().RefusedOverCap == 1 })
}

func TestUnreachableDestinationClosesTheClient(t *testing.T) {
	p, addr := start(t, Config{Resolve: func(context.Context) (Destination, error) { return Destination{}, ErrNoDestination }})
	c, _ := net.Dial("tcp", addr)
	defer c.Close()
	eventually(t, "closed", func() bool { return readClosed(c) })
	if s := p.Stats(); s.ConnectFailures != 1 || s.Active != 0 {
		t.Fatalf("stats %+v", s)
	}
}

func TestConnectTimeoutBoundsTheDial(t *testing.T) {
	dial := func(ctx context.Context, _ string) (net.Conn, error) { <-ctx.Done(); return nil, ctx.Err() }
	inst := newInstance(t, false)
	p, addr := start(t, Config{Resolve: fixed(inst, "a"), Dial: dial, ConnectTimeout: 100 * time.Millisecond})
	c, _ := net.Dial("tcp", addr)
	defer c.Close()
	eventually(t, "closed after the connect timeout", func() bool { return readClosed(c) })
	if p.Stats().ConnectFailures != 1 {
		t.Fatalf("stats %+v", p.Stats())
	}
}

// TestSweepClosesOnlyWhatMoved changes the destination and expects exactly the
// connections to the old one to be closed, new ones to go to the new one.
func TestSweepClosesOnlyWhatMoved(t *testing.T) {
	oldI, newI := newInstance(t, true), newInstance(t, true)
	var cur atomic.Pointer[Destination]
	cur.Store(&Destination{Addr: oldI.addr(), Key: "old"})
	p, addr := start(t, Config{Resolve: func(context.Context) (Destination, error) {
		if d := cur.Load(); d != nil {
			return *d, nil
		}
		return Destination{}, ErrNoDestination
	}, Recheck: time.Hour})
	dial := func() net.Conn {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = c.Write([]byte("x"))
		_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
		if _, err := io.ReadFull(c, make([]byte, 1)); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	a, b := dial(), dial()
	p.Sweep(context.Background())
	if readClosed(a) || readClosed(b) {
		t.Fatal("nothing moved, nothing may be closed")
	}
	cur.Store(&Destination{Addr: newI.addr(), Key: "new"})
	c := dial() // goes to the new destination
	p.Sweep(context.Background())
	eventually(t, "old connections closed", func() bool { return readClosed(a) && readClosed(b) })
	if readClosed(c) {
		t.Fatal("a connection to the new destination was closed")
	}
	eventually(t, "counted", func() bool { return p.Stats().ClosedOnMove == 2 })
	// No destination at all: everything is closed.
	cur.Store(nil)
	p.Sweep(context.Background())
	eventually(t, "all closed", func() bool { return readClosed(c) })
}

// TestIdleConnectionsAreClosedByTheInjectedClock needs no sleeping: the clock is
// advanced by hand and Sweep run explicitly.
func TestIdleConnectionsAreClosedByTheInjectedClock(t *testing.T) {
	inst := newInstance(t, true)
	var now atomic.Int64
	p, addr := start(t, Config{Resolve: fixed(inst, "a"), IdleTimeout: time.Minute, Recheck: time.Hour, Now: func() time.Duration { return time.Duration(now.Load()) }})
	c, _ := net.Dial("tcp", addr)
	defer c.Close()
	roundTrip := func() {
		_, _ = c.Write([]byte("x"))
		_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
		if _, err := io.ReadFull(c, make([]byte, 1)); err != nil {
			t.Fatal(err)
		}
	}
	roundTrip()
	now.Add(int64(50 * time.Second))
	p.Sweep(context.Background())
	roundTrip() // traffic resets the idle time
	now.Add(int64(50 * time.Second))
	p.Sweep(context.Background())
	if readClosed(c) {
		t.Fatal("a connection that carried traffic 50 s ago must stay")
	}
	now.Add(int64(11 * time.Second))
	p.Sweep(context.Background())
	eventually(t, "idle connection closed", func() bool { return readClosed(c) })
	eventually(t, "counted", func() bool { return p.Stats().ClosedIdle == 1 })
}

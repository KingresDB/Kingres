// SPDX-License-Identifier: AGPL-3.0-only

package clientproxy

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func nodes3() []Node {
	return []Node{
		{ID: 1, Group: 0, Host: "a", Port: 5001, Primary: true, Active: true},
		{ID: 2, Group: 1, Host: "b", Port: 5002, Primary: true, Active: true},
		{ID: 3, Group: 2, Host: "c", Port: 5003, Primary: true, Active: true},
		{ID: 4, Group: 1, Host: "c", Port: 5099, Primary: false, Active: true}, // a standby is never a destination
	}
}

func env(self string, nodes []Node) Env {
	return Env{Self: self, LocalPort: 6000, Nodes: func(context.Context) ([]Node, error) { return nodes, nil },
		LeaseKnown: func() bool { return true }, HoldsLease: func(int32) bool { return true }, CacheFor: time.Nanosecond, PeerEntryPort: 5432}
}

func resolve(t *testing.T, e Env) (Destination, error) {
	t.Helper()
	return PrimaryResolver(e)(context.Background())
}

func TestLocalPrimaryWhenTheLeaseIsHeld(t *testing.T) {
	d, err := resolve(t, env("b", nodes3()))
	if err != nil || d.Addr != "127.0.0.1:6000" || d.Key != "local:1" {
		t.Fatalf("%+v %v", d, err)
	}
}

func TestOtherServersPrimaryWhenTheLeaseIsNotHeld(t *testing.T) {
	e := env("b", nodes3())
	e.HoldsLease = func(int32) bool { return false }
	d, err := resolve(t, e)
	if err != nil || d.Addr != "c:5432" || d.Key != "node:3" {
		t.Fatalf("next in the ring after group 1 is group 2: %+v %v", d, err)
	}
	// The ring wraps and skips dead servers.
	e = env("c", nodes3())
	e.HoldsLease = func(int32) bool { return false }
	e.Dead = func(h string) bool { return h == "a" }
	d, _ = resolve(t, e)
	if d.Addr != "b:5432" {
		t.Fatalf("a is dead, so b: %+v", d)
	}
}

func TestWithoutALeaseManagerTheLocalPrimaryIsUsed(t *testing.T) {
	e := env("a", nodes3())
	e.LeaseKnown = func() bool { return false }
	e.HoldsLease = func(int32) bool { return false }
	if d, _ := resolve(t, e); d.Key != "local:0" {
		t.Fatalf("%+v", d)
	}
}

func TestInactiveLocalPrimaryOrNothingElse(t *testing.T) {
	n := nodes3()
	n[1].Active = false // b's own primary is not active in the catalog
	if d, _ := resolve(t, env("b", n)); d.Key != "node:3" {
		t.Fatalf("an inactive local primary must not be used: %+v", d)
	}
	alone := []Node{{ID: 1, Group: 0, Host: "a", Port: 5001, Primary: true, Active: true}}
	e := env("a", alone)
	e.HoldsLease = func(int32) bool { return false }
	if _, err := resolve(t, e); !errors.Is(err, ErrNoDestination) {
		t.Fatalf("a single server without its lease has nowhere to forward: %v", err)
	}
}

func TestACatalogThatCannotBeReadIsAnErrorNotADestination(t *testing.T) {
	e := env("a", nil)
	e.Nodes = func(context.Context) ([]Node, error) { return nil, errors.New("down") }
	if _, err := resolve(t, e); err == nil || errors.Is(err, ErrNoDestination) {
		t.Fatalf("%v", err)
	}
}

func TestStandbyResolverPrefersStreamingAndIsStable(t *testing.T) {
	r := StandbyResolver(func(context.Context) ([]StandbyInfo, error) {
		return []StandbyInfo{{ID: 7, Port: 7007, Running: true}, {ID: 9, Port: 7009, Running: true, Streaming: true}, {ID: 3, Port: 7003}}, nil
	})
	d, err := r(context.Background())
	if err != nil || d.Key != "standby:9" || d.Addr != "127.0.0.1:7009" {
		t.Fatalf("%+v %v", d, err)
	}
	none := StandbyResolver(func(context.Context) ([]StandbyInfo, error) { return nil, nil })
	if _, err := none(context.Background()); !errors.Is(err, ErrNoDestination) {
		t.Fatal(err)
	}
}

// TestAConnectionFromAnotherServerIsNeverForwardedAgain is the loop rule: a server that
// forwarded a client here must find the lease held here, or the connection ends.
func TestAConnectionFromAnotherServerIsNeverForwardedAgain(t *testing.T) {
	e := env("b", nodes3())
	e.HoldsLease = func(int32) bool { return false }
	e.IsPeer = func(a net.Addr) bool { return a != nil && a.String() == "10.0.0.1:1" }
	r := PrimaryResolver(e)
	peer := context.WithValue(context.Background(), remoteKey{}, net.Addr(&net.TCPAddr{IP: net.ParseIP("10.0.0.1"), Port: 1}))
	if _, err := r(peer); !errors.Is(err, ErrNoDestination) {
		t.Fatalf("a peer's connection without our lease must end: %v", err)
	}
	client := context.WithValue(context.Background(), remoteKey{}, net.Addr(&net.TCPAddr{IP: net.ParseIP("192.0.2.9"), Port: 1}))
	if d, err := r(client); err != nil || d.Key != "node:3" {
		t.Fatalf("a client's is forwarded: %+v %v", d, err)
	}
	e.HoldsLease = func(int32) bool { return true }
	if d, err := PrimaryResolver(e)(peer); err != nil || d.Key != "local:1" {
		t.Fatalf("a peer's connection is served locally when the lease is held: %+v %v", d, err)
	}
}

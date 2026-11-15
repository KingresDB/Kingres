// SPDX-License-Identifier: PostgreSQL

package clientproxy

import (
	"context"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Node is one instance of the catalog as the choice of a destination needs it.
type Node struct {
	ID      int32
	Group   int32
	Host    string
	Port    int
	Primary bool
	Active  bool
}

// Env is what the choice of a destination reads. Every function is cheap and
// local; none of them calls another server.
type Env struct {
	// Self is the server id (the host) of this server and LocalPort the port of its
	// primary instance, which is reached on the loopback address.
	Self      string
	LocalPort int
	// Nodes reads the instances of the catalog (this server's own copy).
	Nodes func(ctx context.Context) ([]Node, error)
	// LeaseKnown tells whether a lease manager is running and knows leases at all;
	// HoldsLease whether this server holds a valid lease for the group. Without a
	// lease manager (LeaseKnown false) the local primary is used when the catalog
	// calls it active: there is nothing that could say otherwise.
	LeaseKnown func() bool
	HoldsLease func(group int32) bool
	// Dead tells whether a server is dead according to the heartbeats.
	Dead func(host string) bool
	// PeerEntryPort is the port of the client entry point of the other servers (the
	// same as this server's: one port for the whole cluster); 0 when this server
	// has none, which leaves nowhere to forward to but the local primary. IsPeer
	// tells whether a client address belongs to another server of the cluster.
	PeerEntryPort int
	IsPeer        func(net.Addr) bool
	// CacheFor is how long the catalog read is reused (default 500 ms); a catalog
	// that cannot be read is not cached.
	CacheFor time.Duration
	// Now is a clock for the cache (default time.Now).
	Now func() time.Time
}

// cached wraps Nodes with a short-lived cache, because a resolver runs for every
// connection and at every sweep.
type cached struct {
	env Env
	mu  sync.Mutex
	at  time.Time
	val []Node
}

func (c *cached) nodes(ctx context.Context) ([]Node, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.env.Now()
	if c.val != nil && now.Sub(c.at) < c.env.CacheFor {
		return c.val, nil
	}
	n, err := c.env.Nodes(ctx)
	if err != nil {
		c.val = nil
		return nil, err
	}
	c.val, c.at = n, now
	return n, nil
}

// PrimaryResolver chooses the destination of the primary entry point:
//
//  1. the primary instance of this server, on the loopback address, when the
//     catalog says it is active and this server holds the lease of its group;
//  2. otherwise the client entry point (same port) of the server that runs the
//     next active primary after this server's group in group order (a ring),
//     skipping servers that the heartbeats say are dead. Not the instance itself:
//     an instance accepts connections from another server's address only with
//     that server's certificate (the node rules of pg_hba.conf), so a client
//     forwarded to it directly would be refused; the entry point of that server
//     connects to its own instance from its loopback address. A connection that
//     comes from another server of the cluster is never forwarded again (no
//     loops): it goes to the local primary or nowhere.
//
// With nothing to forward to the answer is ErrNoDestination.
func PrimaryResolver(e Env) Resolver {
	if e.CacheFor <= 0 {
		e.CacheFor = 500 * time.Millisecond
	}
	if e.Now == nil {
		e.Now = time.Now
	}
	c := &cached{env: e}
	return func(ctx context.Context) (Destination, error) {
		nodes, err := c.nodes(ctx)
		if err != nil {
			return Destination{}, err
		}
		var primaries []Node
		var own *Node // this server's primary, active or not: it fixes where the ring starts
		for i, n := range nodes {
			if n.Primary && n.Host == e.Self && own == nil {
				own = &nodes[i]
			}
			if n.Primary && n.Active {
				primaries = append(primaries, n)
			}
		}
		sort.Slice(primaries, func(i, j int) bool { return primaries[i].Group < primaries[j].Group })
		if own != nil && own.Active && (e.LeaseKnown == nil || !e.LeaseKnown() || e.HoldsLease(own.Group)) {
			return Destination{Addr: net.JoinHostPort("127.0.0.1", strconv.Itoa(e.LocalPort)), Key: "local:" + strconv.Itoa(int(own.Group))}, nil
		}
		if e.PeerEntryPort == 0 || (e.IsPeer != nil && e.IsPeer(RemoteAddr(ctx))) {
			return Destination{}, ErrNoDestination
		}
		// Start after this server's own group, or at the first group when it has none.
		after := int32(-1)
		if own != nil {
			after = own.Group
		}
		ordered := make([]Node, 0, len(primaries))
		for _, n := range primaries {
			if n.Group > after {
				ordered = append(ordered, n)
			}
		}
		for _, n := range primaries {
			if n.Group <= after {
				ordered = append(ordered, n)
			}
		}
		for _, n := range ordered {
			if n.Host == e.Self || (e.Dead != nil && e.Dead(n.Host)) {
				continue
			}
			return Destination{Addr: net.JoinHostPort(n.Host, strconv.Itoa(e.PeerEntryPort)), Key: "node:" + strconv.Itoa(int(n.ID))}, nil
		}
		return Destination{}, ErrNoDestination
	}
}

// StandbyInfo is a standby instance of this server as the standby entry point
// needs it.
type StandbyInfo struct {
	ID        int64
	Port      int
	Running   bool
	Streaming bool
}

// StandbyResolver chooses a local standby for reads that tolerate lag: a running
// one that is streaming, else a running one that is not (it may lag; the client
// asked for lag-tolerant reads), else ErrNoDestination. The choice is stable
// (lowest id first) so that connections are not moved for no reason.
func StandbyResolver(list func(ctx context.Context) ([]StandbyInfo, error)) Resolver {
	return func(ctx context.Context) (Destination, error) {
		all, err := list(ctx)
		if err != nil {
			return Destination{}, err
		}
		sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
		var fallback *StandbyInfo
		for i := range all {
			s := all[i]
			if !s.Running {
				continue
			}
			if s.Streaming {
				return Destination{Addr: net.JoinHostPort("127.0.0.1", strconv.Itoa(s.Port)), Key: "standby:" + strconv.FormatInt(s.ID, 10)}, nil
			}
			if fallback == nil {
				fallback = &all[i]
			}
		}
		if fallback != nil {
			return Destination{Addr: net.JoinHostPort("127.0.0.1", strconv.Itoa(fallback.Port)), Key: "standby:" + strconv.FormatInt(fallback.ID, 10)}, nil
		}
		return Destination{}, ErrNoDestination
	}
}

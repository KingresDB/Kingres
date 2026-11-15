// SPDX-License-Identifier: PostgreSQL

package main

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clientproxy"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ha"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/monitor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// startClientProxy starts the client entry points (design part D, package
// clientproxy) when client-listen or client-standby-listen is set; with neither
// it does nothing. A port that cannot be bound stops the start: an entry point
// that was asked for and is not there must not go unnoticed.
func startClientProxy(d *haDeps) error {
	h := d.Cfg.HA
	if (h.ClientListen == "" && h.ClientStandbyListen == "") || !d.Svc.Enabled() {
		return nil
	}
	shared := clientproxy.NewLimit(h.ClientMaxConnections)
	base := clientproxy.Config{
		Shared: shared, ConnectTimeout: h.ClientConnectTimeout, IdleTimeout: h.ClientIdleTimeout, Dial: localDial(d.ServerHost),
	}
	type entry struct {
		prefix string
		proxy  *clientproxy.Proxy
	}
	var entries []entry
	listen := func(name, addr string, resolve clientproxy.Resolver) error {
		cfg := base
		cfg.Resolve = resolve
		cfg.OnEvent = func(ev string) { d.Log.Debug("client entry point", "entry", name, "event", ev) }
		p := clientproxy.New(cfg)
		var lc net.ListenConfig
		ln, err := lc.Listen(d.BG, "tcp", addr)
		if err != nil {
			return fmt.Errorf("client entry point %s: listening on %s: %w", name, addr, err)
		}
		d.Log.Info("client entry point listening", "entry", name, "address", ln.Addr().String())
		d.WG.Add(1)
		go func() {
			defer d.WG.Done()
			if err := p.Serve(d.BG, ln); err != nil {
				d.Log.Error("client entry point", "entry", name, "error", err)
			}
		}()
		prefix := "client"
		if name == "standby" {
			prefix = "clientStandby"
		}
		entries = append(entries, entry{prefix, p})
		return nil
	}
	if h.ClientListen != "" {
		if err := listen("primary", h.ClientListen, primaryResolver(d)); err != nil {
			return err
		}
	}
	if h.ClientStandbyListen != "" {
		if err := listen("standby", h.ClientStandbyListen, standbyResolver(d)); err != nil {
			return err
		}
	}
	if d.Ring != nil {
		d.WG.Add(1)
		go func() {
			defer d.WG.Done()
			t := time.NewTicker(5 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-d.BG.Done():
					return
				case now := <-t.C:
					for _, e := range entries {
						addProxyMetrics(d.Ring, d.ServerHost, e.prefix, e.proxy.Stats(), now)
					}
				}
			}
		}()
	}
	return nil
}

// addProxyMetrics records the counters of one entry point. They are totals since
// the start of kingresd (they only grow), except the active connections.
func addProxyMetrics(r *monitor.Ring, host, prefix string, s clientproxy.Stats, now time.Time) {
	add := func(name string, v int64) {
		f := float64(v)
		r.Add(monitor.Series{Metric: prefix + name, ServerID: host}, now, &f)
	}
	add("ConnectionsAccepted", s.Accepted)
	add("ConnectionsActive", s.Active)
	add("ConnectionsRefused", s.RefusedOverCap)
	add("ConnectFailures", s.ConnectFailures)
	add("ConnectionsMoved", s.ClosedOnMove)
	add("ConnectionsIdleClosed", s.ClosedIdle)
	add("BytesIn", s.BytesIn)
	add("BytesOut", s.BytesOut)
}

// primaryResolver reads this server's own copy of the shard catalog and the
// lease and heartbeat state through the registry; see clientproxy.PrimaryResolver
// for the rule.
func primaryResolver(d *haDeps) clientproxy.Resolver {
	return clientproxy.PrimaryResolver(clientproxy.Env{
		Self: d.ServerHost, LocalPort: d.Cfg.PGPort,
		Nodes: func(ctx context.Context) ([]clientproxy.Node, error) {
			c, err := d.Svc.ServiceConn(ctx, sysdb.Database)
			if err != nil {
				return nil, fmt.Errorf("reading the shard catalog: %w", err)
			}
			defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
			rows, err := c.Query(ctx, `SELECT node_id, group_id, host, port, role = 'primary', is_active FROM kingres.node`)
			if err != nil {
				return nil, fmt.Errorf("reading the shard catalog: %w", err)
			}
			defer rows.Close()
			out := []clientproxy.Node{}
			for rows.Next() {
				var n clientproxy.Node
				if err := rows.Scan(&n.ID, &n.Group, &n.Host, &n.Port, &n.Primary, &n.Active); err != nil {
					return nil, fmt.Errorf("reading the shard catalog: %w", err)
				}
				out = append(out, n)
			}
			return out, rows.Err()
		},
		LeaseKnown:    func() bool { return len(d.Registry.Leases().Info()) > 0 },
		HoldsLease:    func(g int32) bool { return d.Registry.Leases().Holds(g) },
		Dead:          func(host string) bool { return d.Registry.Liveness().State(host) == ha.PeerDead },
		PeerEntryPort: entryPort(d.Cfg.HA.ClientListen),
		IsPeer:        newPeerMatcher(d).isPeer,
	})
}

// standbyResolver offers the standbys of this server that the standby manager
// reports; with none (or no manager) new connections are closed.
func standbyResolver(d *haDeps) clientproxy.Resolver {
	return clientproxy.StandbyResolver(func(ctx context.Context) ([]clientproxy.StandbyInfo, error) {
		all, err := d.Registry.Standbys().List(ctx)
		if err != nil {
			return nil, err
		}
		var out []clientproxy.StandbyInfo
		for _, s := range all {
			if s.ServerID == d.ServerHost {
				out = append(out, clientproxy.StandbyInfo{ID: s.ID, Port: s.Port, Running: s.State == "running", Streaming: s.Streaming})
			}
		}
		return out, nil
	})
}

// entryPort is the port of the client entry point (0 when there is none).
func entryPort(listen string) int {
	_, p, err := net.SplitHostPort(listen)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(p)
	return n
}

// peerMatcher tells whether a client address is one of the other servers of the
// cluster: the addresses of the servers in the peer directory, looked up again
// every half minute. It only decides where a connection may be forwarded to; a
// wrong "no" costs a forwarding hop that the loop rule would otherwise save.
type peerMatcher struct {
	d  *haDeps
	mu sync.Mutex
	at time.Time
	ip map[string]bool
}

func newPeerMatcher(d *haDeps) *peerMatcher { return &peerMatcher{d: d} }

func (m *peerMatcher) isPeer(a net.Addr) bool {
	ta, ok := a.(*net.TCPAddr)
	if !ok || ta == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ip == nil || time.Since(m.at) > 30*time.Second {
		m.ip = map[string]bool{}
		for _, p := range m.d.Peers.All() {
			if p.ServerID == m.d.ServerHost {
				continue
			}
			if ips, err := net.LookupIP(p.ServerID); err == nil {
				for _, ip := range ips {
					m.ip[ip.String()] = true
				}
			}
		}
		m.at = time.Now()
	}
	return m.ip[ta.IP.String()]
}

// localDial opens the connection to an instance of this server. It comes from
// 127.0.0.1, which the default network rules admit. The exception is a cluster on
// one machine, where every server's host is a loopback address: the instances'
// node rules (certificate required) already list 127.0.0.1 as a source of the other
// servers (see cluster.resolveCIDRs), and so is every server's own address, so a
// client arriving from any of them would be asked for a node certificate. The
// connection then comes from 127.255.255.254, an address on the loopback network
// that no instance lists and that the default rules (127.0.0.0/8) admit.
func localDial(serverHost string) func(ctx context.Context, addr string) (net.Conn, error) {
	var d net.Dialer
	if ip := net.ParseIP(serverHost); ip != nil && ip.IsLoopback() {
		d.LocalAddr = &net.TCPAddr{IP: net.IPv4(127, 255, 255, 254)}
	}
	return func(ctx context.Context, addr string) (net.Conn, error) {
		if h, _, err := net.SplitHostPort(addr); err == nil && h == "127.0.0.1" {
			return d.DialContext(ctx, "tcp", addr)
		}
		var plain net.Dialer
		return plain.DialContext(ctx, "tcp", addr)
	}
}

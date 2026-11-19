// SPDX-License-Identifier: AGPL-3.0-only

package ledgeranchor

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
)

// The rule for outgoing requests (correction 180). A target may reach exactly the host and port that its
// configuration names (the endpoint of a store, the URL of an authority, the URL of each calendar or source of
// headers) and no other: the libraries follow no redirect to another host, and the dialer of the client given to
// them refuses every address that is not one of those. A host that is written as an IP address is connected to as
// written, whatever the address (the operator named it: a store on 127.0.0.1 or on a link-local address is a
// configuration, not an accident). A host that is a name is resolved here, and a name that resolves to a loopback,
// link-local, unspecified or multicast address is refused: a name that leads to the metadata service of a cloud
// host, or to a service of this server, is how a hostile or hijacked name turns an anchor target into a request
// to something the operator did not name. Private (RFC 1918 and unique local) addresses are allowed for names:
// a store on a private network is the usual case. The connection is made to the address that was checked, so a
// second lookup cannot change it.

// Egress is the set of "host:port" that a client may dial.
type Egress struct {
	allowed map[string]bool
	// Resolve is the lookup of a host name (net.DefaultResolver when nil); tests replace it.
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	// Dial connects to an address that has been checked (a net.Dialer when nil); tests replace it.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// NewEgress allows the hosts of the given URLs (each with its port, the default of its scheme when it has none)
// and the extra "host:port" values given.
func NewEgress(urls []string, extra ...string) (*Egress, error) {
	e := &Egress{allowed: map[string]bool{}}
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return nil, errors.New("a URL of the target is not an http or https URL with a host")
		}
		port := u.Port()
		if port == "" {
			port = "443"
			if u.Scheme == "http" {
				port = "80"
			}
		}
		e.allowed[net.JoinHostPort(strings.ToLower(u.Hostname()), port)] = true
	}
	for _, x := range extra {
		e.allowed[strings.ToLower(x)] = true
	}
	return e, nil
}

// Allows reports whether host:port may be dialed.
func (e *Egress) Allows(hostport string) bool { return e.allowed[strings.ToLower(hostport)] }

func refused(code, text string) error {
	return anchor.Errorf(anchor.ClassPermanent, "egress", "dial", code, "%s", text)
}

// badAddr is an address that a host name may not resolve to.
func badAddr(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsUnspecified() || a.IsMulticast()
}

// DialContext is the dialer of the client of a target.
func (e *Egress) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, refused("egress_refused", "the address of the request is not host:port")
	}
	if !e.Allows(net.JoinHostPort(host, port)) {
		return nil, refused("egress_refused", "a request to a host that the configuration of the target does not name was refused")
	}
	dial := e.Dial
	if dial == nil {
		d := &net.Dialer{Timeout: anchor.DefaultDialTimeout}
		dial = d.DialContext
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return dial(ctx, network, net.JoinHostPort(ip.String(), port))
	}
	resolve := e.Resolve
	if resolve == nil {
		resolve = func(ctx context.Context, h string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", h)
		}
	}
	addrs, err := resolve(ctx, host)
	if err != nil || len(addrs) == 0 {
		return nil, anchor.Errorf(anchor.ClassRetryable, "egress", "dial", "resolve", "the host name of the target did not resolve")
	}
	for _, a := range addrs {
		if badAddr(a) {
			return nil, refused("egress_address", "the host name of the target resolves to a loopback, link-local, unspecified or multicast address; if that is meant, configure the address itself")
		}
	}
	var last error
	for _, a := range addrs {
		c, err := dial(ctx, network, net.JoinHostPort(a.Unmap().String(), port))
		if err == nil {
			return c, nil
		}
		last = err
		if ctx.Err() != nil {
			break
		}
	}
	return nil, last
}

// Client builds the *http.Client of a target: no proxy, TLS 1.2 or later against the given roots, the timeouts of
// anchor.NewHTTPClient, and this dialer. A nil pool means the roots of the system (the caller chose it: see
// SystemRoots) and never an environment-dependent default of the library.
func (e *Egress) Client(roots *x509.CertPool) *http.Client {
	c := anchor.NewHTTPClient(anchor.HTTPOptions{RootCAs: roots})
	if tr, ok := c.Transport.(*http.Transport); ok {
		tr.DialContext = e.DialContext
	}
	return c
}

// SystemRoots is the pool of the roots of this server's system, used for a store that names no CA bundle (a public
// store such as AWS S3); it is read once.
func SystemRoots() (*x509.CertPool, error) { return x509.SystemCertPool() }

// LoadPool reads a PEM bundle of trust anchors (a regular file of at most 1 MiB) into a pool. An empty or
// unparsable bundle is an error: a bundle that holds no certificate would silently trust nothing.
func LoadPool(path string) (*x509.CertPool, error) {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() > 1<<20 {
		return nil, errors.New("the CA bundle cannot be read (it must be a regular file of at most 1 MiB)")
	}
	b, err := os.ReadFile(path) // #nosec G304 -- the path is configured by the administrator
	if err != nil {
		return nil, errors.New("the CA bundle cannot be read")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b) {
		return nil, errors.New("the CA bundle holds no certificate in PEM form")
	}
	return pool, nil
}

// ReadBundle reads a PEM bundle (a regular file of at most 1 MiB).
func ReadBundle(path string) ([]byte, error) {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() > 1<<20 {
		return nil, errors.New("the CA bundle cannot be read (it must be a regular file of at most 1 MiB)")
	}
	b, err := os.ReadFile(path) // #nosec G304 -- the path is configured by the administrator
	if err != nil {
		return nil, errors.New("the CA bundle cannot be read")
	}
	return b, nil
}

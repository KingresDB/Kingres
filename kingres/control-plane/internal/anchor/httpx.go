// SPDX-License-Identifier: AGPL-3.0-only

package anchor

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// ErrTooLarge is returned when a response body exceeds its limit.
var ErrTooLarge = errors.New("anchor: response larger than the limit")

// ErrRedirectRefused is returned when a redirect is refused.
var ErrRedirectRefused = errors.New("anchor: redirect refused")

// ReadLimited reads at most max bytes from r and returns ErrTooLarge when
// there is more. It never allocates more than max+1 bytes.
func ReadLimited(r io.Reader, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, ErrTooLarge
	}
	return b, nil
}

// Defaults of NewHTTPClient.
const (
	DefaultDialTimeout           = 10 * time.Second
	DefaultTLSHandshakeTimeout   = 10 * time.Second
	DefaultResponseHeaderTimeout = 30 * time.Second
	DefaultClientTimeout         = 60 * time.Second
)

// HTTPOptions are the parts of the client that NewHTTPClient builds. Zero
// durations mean the defaults above.
type HTTPOptions struct {
	// RootCAs are the trust anchors of every https connection. Nil means an
	// empty pool: every https connection then fails with an unknown
	// authority error, because the package never falls back to the system
	// roots (on Unix the system roots are read from the environment:
	// SSL_CERT_FILE and SSL_CERT_DIR). A caller that wants the system roots
	// passes x509.SystemCertPool() itself.
	RootCAs               *x509.CertPool
	DialTimeout           time.Duration
	TLSHandshakeTimeout   time.Duration
	ResponseHeaderTimeout time.Duration
	// Timeout bounds one whole request (dial to the end of the body).
	Timeout time.Duration
}

func pick(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// NewHTTPClient builds the *http.Client that the targets are meant to be
// given: no proxy (the proxy variables of the environment are never read),
// TLS 1.2 or later against exactly the given roots, and timeouts for the
// dial, the handshake, the response header and the whole request. Redirects
// are limited by the targets themselves (Client).
func NewHTTPClient(o HTTPOptions) *http.Client {
	roots := o.RootCAs
	if roots == nil {
		roots = x509.NewCertPool()
	}
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: pick(o.DialTimeout, DefaultDialTimeout)}).DialContext,
		TLSClientConfig:       &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   pick(o.TLSHandshakeTimeout, DefaultTLSHandshakeTimeout),
		ResponseHeaderTimeout: pick(o.ResponseHeaderTimeout, DefaultResponseHeaderTimeout),
		MaxIdleConns:          8,
		IdleConnTimeout:       30 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{Transport: tr, Timeout: pick(o.Timeout, DefaultClientTimeout)}
}

// noProxy returns a round tripper that never uses a proxy: a nil transport
// and an *http.Transport (the default one included) are replaced by a clone
// whose Proxy is nil, so HTTP_PROXY, HTTPS_PROXY and NO_PROXY are never read,
// whatever client the caller passed. Any other RoundTripper is the caller's
// own code and is kept as it is.
func noProxy(rt http.RoundTripper) http.RoundTripper {
	var base *http.Transport
	switch t := rt.(type) {
	case nil:
		base = http.DefaultTransport.(*http.Transport)
	case *http.Transport:
		base = t
	default:
		return rt
	}
	c := base.Clone()
	c.Proxy = nil
	return c
}

// Client returns a copy of c (nil means a client with the default transport
// and no timeout of its own: the context is the timeout) that uses no proxy
// (see noProxy) and never follows a redirect to another host. With
// sameHostHops 0 it follows no redirect at all: the 3xx response is returned
// to the caller. With sameHostHops n > 0 it follows at most n redirects to
// the same host and port, and only over the same scheme.
func Client(c *http.Client, sameHostHops int) *http.Client {
	var cc http.Client
	if c != nil {
		cc = *c
	}
	cc.Transport = noProxy(cc.Transport)
	cc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if sameHostHops <= 0 {
			return http.ErrUseLastResponse
		}
		if len(via) > sameHostHops {
			return ErrRedirectRefused
		}
		first := via[0]
		if !strings.EqualFold(req.URL.Host, first.URL.Host) || req.URL.Scheme != first.URL.Scheme {
			return ErrRedirectRefused
		}
		return nil
	}
	return &cc
}

func isCertError(err error) bool {
	var (
		a x509.UnknownAuthorityError
		h x509.HostnameError
		c x509.CertificateInvalidError
		r *tls.CertificateVerificationError
	)
	return errors.As(err, &a) || errors.As(err, &h) || errors.As(err, &c) || errors.As(err, &r)
}

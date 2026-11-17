// SPDX-License-Identifier: AGPL-3.0-only

package anchor

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"strings"
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

// Client returns a copy of c (nil means a client with the default transport
// and no timeout of its own: the context is the timeout) that never follows a
// redirect to another host. With sameHostHops 0 it follows no redirect at
// all: the 3xx response is returned to the caller. With sameHostHops n > 0 it
// follows at most n redirects to the same host and port, and only over the
// same scheme.
func Client(c *http.Client, sameHostHops int) *http.Client {
	var cc http.Client
	if c != nil {
		cc = *c
	}
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

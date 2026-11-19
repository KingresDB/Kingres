// SPDX-License-Identifier: AGPL-3.0-only

package pubsrc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/safefile"
)

// HTTPOptions are the timeouts of an HTTP source. A zero field takes its default.
type HTTPOptions struct {
	// Dial is the limit on making a connection (default 10 s), Header the wait for the headers of
	// the answer (15 s), Request the limit on the whole of one request, body included (60 s).
	Dial, Header, Request time.Duration
}

func (o HTTPOptions) withDefaults() HTTPOptions {
	if o.Dial <= 0 {
		o.Dial = 10 * time.Second
	}
	if o.Header <= 0 {
		o.Header = 15 * time.Second
	}
	if o.Request <= 0 {
		o.Request = 60 * time.Second
	}
	return o
}

type httpSource struct {
	base   *url.URL
	client *http.Client
	opt    HTTPOptions
}

// OpenHTTP opens an HTTP(S) source. Nothing is fetched yet.
func OpenHTTP(spec string, opt HTTPOptions) (Source, error) {
	u, err := parseBase(spec)
	if err != nil {
		return nil, err
	}
	opt = opt.withDefaults()
	tr := &http.Transport{
		Proxy:                  nil, // never a proxy from the environment: the auditor said where to go
		DialContext:            (&net.Dialer{Timeout: opt.Dial}).DialContext,
		TLSHandshakeTimeout:    opt.Dial,
		ResponseHeaderTimeout:  opt.Header,
		MaxResponseHeaderBytes: 64 << 10,
		MaxIdleConns:           4,
		IdleConnTimeout:        30 * time.Second,
		ExpectContinueTimeout:  time.Second,
	}
	c := &http.Client{
		Transport: tr,
		Timeout:   opt.Request,
		// A redirect is never followed, to the same host or another: the answer is returned and refused.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return &httpSource{base: u, client: c, opt: opt}, nil
}

func (s *httpSource) Kind() string { return "http" }

func (s *httpSource) Close() error {
	s.client.CloseIdleConnections()
	return nil
}

func (s *httpSource) SubDirs(rel string) ([]string, []string, error) {
	return nil, nil, fmt.Errorf("%w: %s", ErrNoListing, rel)
}

// URLFor is the address of rel, for messages.
func (s *httpSource) URLFor(rel string) string { return s.base.String() + "/" + rel }

func (s *httpSource) ReadFile(rel string, limit int64) ([]byte, error) {
	if _, err := safefile.CleanRel(rel); err != nil {
		return nil, err
	}
	u := s.URLFor(rel)
	ctx, cancel := context.WithTimeout(context.Background(), s.opt.Request)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "kingres-verify")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: GET %s: %v", ErrUnreachable, u, trimURLError(err))
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%s: %w", rel, fs.ErrNotExist)
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return nil, fmt.Errorf("GET %s: %w (status %d)", u, ErrRedirect, resp.StatusCode)
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return nil, fmt.Errorf("%w: GET %s answered %d", ErrUnreachable, u, resp.StatusCode)
	default:
		return nil, fmt.Errorf("GET %s answered %d, expected 200 or 404", u, resp.StatusCode)
	}
	if resp.ContentLength > limit {
		return nil, fmt.Errorf("%s %w (the server announces %d bytes, at most %d)", rel, safefile.ErrTooLarge, resp.ContentLength, limit)
	}
	// The body is read through the limit, whatever the headers said, and after any gzip decoding.
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%w: reading %s: %v", ErrUnreachable, u, trimURLError(err))
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s %w (more than %d bytes)", rel, safefile.ErrTooLarge, limit)
	}
	return data, nil
}

// trimURLError drops the URL that net/http repeats in its errors, which the caller already names.
func trimURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

func (s *httpSource) Files(rel string) ([]string, error) {
	return nil, fmt.Errorf("%w: %s", ErrNoListing, rel)
}

// SPDX-License-Identifier: AGPL-3.0-only

package ots

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
)

// MaxCalendarResponse is the largest calendar response read (the reference
// client's MAX_RESPONSE_LENGTH).
const MaxCalendarResponse = 10000

// AcceptHeader is the media type of the calendar protocol.
const AcceptHeader = "application/vnd.opentimestamps.v1"

// maxRedirects is the number of same-host redirects a calendar request follows.
const maxRedirects = 3

// CalendarClient talks to calendar servers. The HTTP client is the caller's;
// redirects to another host or another scheme are refused.
type CalendarClient struct {
	http      *http.Client
	userAgent string
	now       anchor.Clock
}

// NewCalendarClient returns a client; c may be nil (default transport; the
// context is then the only timeout).
func NewCalendarClient(c *http.Client, userAgent string, now anchor.Clock) *CalendarClient {
	if now == nil {
		now = time.Now
	}
	if userAgent == "" {
		userAgent = "kingresd-anchor"
	}
	return &CalendarClient{http: anchor.Client(c, maxRedirects), userAgent: userAgent, now: now}
}

// NormalizeCalendarURL checks and normalizes a calendar base URL: http or
// https (http only when allowInsecure), a host, no user information, no
// query, no fragment; the host in lower case; no trailing slash.
func NormalizeCalendarURL(raw string, allowInsecure bool) (string, error) {
	if len(raw) == 0 || len(raw) > MaxURILen {
		return "", errors.New("calendar url is empty or too long")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("calendar url is not a valid url")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !allowInsecure {
			return "", errors.New("calendar url must be https (allowInsecure is false)")
		}
	default:
		return "", errors.New("calendar url must be http or https")
	}
	if u.User != nil {
		return "", errors.New("calendar url must not carry user information")
	}
	if u.Host == "" || u.Hostname() == "" {
		return "", errors.New("calendar url has no host")
	}
	if u.RawQuery != "" || u.Fragment != "" || u.ForceQuery || strings.Contains(raw, "#") {
		return "", errors.New("calendar url must not have a query or a fragment")
	}
	if u.Opaque != "" {
		return "", errors.New("calendar url is opaque")
	}
	path := strings.TrimRight(u.EscapedPath(), "/")
	if strings.ContainsAny(path, "%?# \\") {
		return "", errors.New("calendar url path has characters that are not allowed")
	}
	n := u.Scheme + "://" + strings.ToLower(u.Host) + path
	if err := checkURI([]byte(n)); err != nil {
		return "", errors.New("calendar url has characters outside letters, digits and -._/:")
	}
	return n, nil
}

// host returns the host part for messages (never user information).
func hostOf(base string) string {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return "?"
	}
	return strings.ToLower(u.Host)
}

func (c *CalendarClient) do(ctx context.Context, op, method, base, path string, body []byte) ([]byte, int, http.Header, error) {
	var rdr *bytes.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	var req *http.Request
	var err error
	if rdr != nil {
		req, err = http.NewRequestWithContext(ctx, method, base+path, rdr)
	} else {
		req, err = http.NewRequestWithContext(ctx, method, base+path, nil)
	}
	if err != nil {
		return nil, 0, nil, anchor.Errorf(anchor.ClassPermanent, anchor.KindOTS, op, "request", "cannot build the request")
	}
	req.Header.Set("Accept", AcceptHeader)
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, nil, wrapTransport(op, base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, resp.StatusCode, resp.Header, &anchor.Error{Class: anchor.ClassPermanent, Target: anchor.KindOTS, Op: op, Code: "redirect",
			Text: "calendar " + hostOf(base) + " answered with a redirect", Status: resp.StatusCode}
	}
	data, err := anchor.ReadLimited(resp.Body, MaxCalendarResponse)
	if err != nil {
		if errors.Is(err, anchor.ErrTooLarge) {
			return nil, resp.StatusCode, resp.Header, &anchor.Error{Class: anchor.ClassPermanent, Target: anchor.KindOTS, Op: op, Code: "too_large",
				Text: fmt.Sprintf("calendar %s answered with more than %d bytes", hostOf(base), MaxCalendarResponse), Status: resp.StatusCode, Err: err}
		}
		return nil, resp.StatusCode, resp.Header, wrapTransport(op, base, err)
	}
	return data, resp.StatusCode, resp.Header, nil
}

func wrapTransport(op, base string, err error) *anchor.Error {
	e := anchor.TransportError(anchor.KindOTS, op, err)
	e.Text = "calendar " + hostOf(base) + ": " + e.Text
	return e
}

func (c *CalendarClient) statusError(op, base string, status int, h http.Header) *anchor.Error {
	return &anchor.Error{Class: anchor.ClassOfStatus(status), Target: anchor.KindOTS, Op: op, Code: "status",
		Text: "calendar " + hostOf(base) + " answered unexpectedly", Status: status,
		RetryAfter: anchor.RetryAfter(h, c.now())}
}

// Submit posts digest to the calendar at base (a normalized URL) and returns
// the fragment, rooted at digest, that the calendar answered with.
func (c *CalendarClient) Submit(ctx context.Context, base string, digest []byte) (*Node, error) {
	const op = "submit"
	data, status, hdr, err := c.do(ctx, op, http.MethodPost, base, "/digest", digest)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, c.statusError(op, base, status, hdr)
	}
	n, err := ParseFragment(data, digest)
	if err != nil {
		return nil, &anchor.Error{Class: anchor.ClassPermanent, Target: anchor.KindOTS, Op: op, Code: "bad_fragment",
			Text: "calendar " + hostOf(base) + " answered with a malformed timestamp: " + shortErr(err), Err: err}
	}
	useful := false
	for _, l := range n.Leaves() {
		if l.Att.Kind == AttPending || l.Att.Kind == AttBitcoin {
			useful = true
		}
	}
	if !useful {
		return nil, anchor.Errorf(anchor.ClassPermanent, anchor.KindOTS, op, "bad_fragment", "calendar %s answered with no pending or bitcoin attestation", hostOf(base))
	}
	return n, nil
}

// GetTimestamp asks the calendar at base for the timestamp of a commitment.
// found is false when the calendar answers 404 (not yet, or unknown).
func (c *CalendarClient) GetTimestamp(ctx context.Context, base string, commitment []byte) (n *Node, found bool, err error) {
	const op = "upgrade"
	data, status, hdr, err := c.do(ctx, op, http.MethodGet, base, "/timestamp/"+hex.EncodeToString(commitment), nil)
	if err != nil {
		return nil, false, err
	}
	switch status {
	case http.StatusOK:
		n, err := ParseFragment(data, commitment)
		if err != nil {
			return nil, false, &anchor.Error{Class: anchor.ClassPermanent, Target: anchor.KindOTS, Op: op, Code: "bad_fragment",
				Text: "calendar " + hostOf(base) + " answered with a malformed timestamp: " + shortErr(err), Err: err}
		}
		return n, true, nil
	case http.StatusNotFound:
		return nil, false, nil
	}
	return nil, false, c.statusError(op, base, status, hdr)
}

// shortErr is the text of a parse error (they contain no input bytes).
func shortErr(err error) string {
	s := err.Error()
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

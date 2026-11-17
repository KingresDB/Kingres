// SPDX-License-Identifier: AGPL-3.0-only

package worm

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor/sigv4"
)

// Limits on what is read from the store.
const (
	// MaxErrorBody is the most read from an error response (and from the
	// small XML answers of Test).
	MaxErrorBody = 64 << 10
	// CallTimeout is the longest one call may take when the caller's
	// context has no earlier deadline.
	CallTimeout = 30 * time.Second
	// MaxMessage is the longest store message kept in an error.
	MaxMessage = 200
)

var codeRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// ParseS3Error extracts the Code and Message of an S3 XML error body
// (<Error><Code>..</Code><Message>..</Message>..</Error>). It reads at most
// MaxErrorBody bytes, never fails, and returns "" for what it cannot find.
// The code must be 1 to 64 characters of A-Z a-z 0-9 . _ - ; the message is
// cut to MaxMessage bytes and reduced to printable ASCII.
func ParseS3Error(body []byte) (code, message string) {
	if len(body) > MaxErrorBody {
		body = body[:MaxErrorBody]
	}
	dec := xml.NewDecoder(bytes.NewReader(body))
	dec.Strict = false
	dec.Entity = nil
	depth, inErr := 0, false
	var field string
	for n := 0; n < 4096; n++ {
		tok, err := dec.RawToken()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth == 1 {
				inErr = t.Name.Local == "Error"
				if !inErr {
					return "", ""
				}
			} else if depth == 2 && inErr {
				field = t.Name.Local
			}
		case xml.EndElement:
			depth--
			if depth < 2 {
				field = ""
			}
			if depth <= 0 {
				return clean(code, message)
			}
		case xml.CharData:
			if depth == 2 && inErr {
				switch field {
				case "Code":
					if code == "" {
						code = strings.TrimSpace(string(t))
					}
				case "Message":
					if message == "" {
						message = strings.TrimSpace(string(t))
					}
				}
			}
		}
	}
	return clean(code, message)
}

func clean(code, message string) (string, string) {
	if !codeRE.MatchString(code) {
		code = ""
	}
	return code, (anchor.Redactor{}).Clean(message, MaxMessage)
}

type response struct {
	status int
	header http.Header
	body   []byte
}

type s3 struct {
	cfg    Config
	signer sigv4.Signer
	hc     *http.Client
	clock  anchor.Clock
	red    anchor.Redactor
}

func (s *s3) endpoint() (scheme, host string) {
	if s.cfg.Endpoint != "" {
		u, _ := url.Parse(s.cfg.Endpoint)
		return u.Scheme, u.Host
	}
	if s.cfg.Region == "us-east-1" {
		return "https", "s3.amazonaws.com"
	}
	return "https", "s3." + s.cfg.Region + ".amazonaws.com"
}

// request builds the URL for key ("" for a bucket operation) and the
// raw query ("" for none).
func (s *s3) request(ctx context.Context, method, key, rawQuery string, body []byte) (*http.Request, error) {
	scheme, host := s.endpoint()
	var path string
	if *s.cfg.PathStyle {
		path = "/" + s.cfg.Bucket
		if key != "" {
			path += "/" + key
		}
	} else {
		host = s.cfg.Bucket + "." + host
		path = "/" + key
	}
	u := &url.URL{Scheme: scheme, Host: host, Path: path, RawPath: sigv4.Escape(path, true), RawQuery: rawQuery}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	return req, nil
}

// do signs and sends the request; the response body is read up to okMax for
// a 2xx answer and up to MaxErrorBody otherwise. A transport failure, an
// oversized body or a refused redirect comes back as an *anchor.Error.
func (s *s3) do(ctx context.Context, op string, req *http.Request, body []byte, okMax int64) (*response, error) {
	ph := sigv4.EmptyPayloadHash
	if body != nil {
		ph = sigv4.HashPayload(body)
	}
	if err := s.signer.Sign(req, ph, s.clock()); err != nil {
		return nil, anchor.Errorf(anchor.ClassPermanent, anchor.KindWORM, op, "sign", "request could not be signed")
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, anchor.TransportError(anchor.KindWORM, op, err)
	}
	defer resp.Body.Close()
	max := int64(MaxErrorBody)
	if resp.StatusCode/100 == 2 {
		max = okMax
	}
	var b []byte
	if req.Method != http.MethodHead {
		b, err = anchor.ReadLimited(resp.Body, max)
		if err != nil {
			e := anchor.TransportError(anchor.KindWORM, op, err)
			e.Status = resp.StatusCode
			return nil, e
		}
	}
	return &response{status: resp.StatusCode, header: resp.Header, body: b}, nil
}

func isLockComplaint(code, msg string) bool {
	l := strings.ToLower(code + " " + msg)
	return strings.Contains(l, "lock") || strings.Contains(l, "retention") || strings.Contains(l, "versioning")
}

// statusError classifies an unexpected status (design 6.6).
func (s *s3) statusError(op string, r *response) *anchor.Error {
	code, msg := ParseS3Error(r.body)
	msg = s.red.Clean(msg, MaxMessage)
	e := &anchor.Error{Target: anchor.KindWORM, Op: op, Status: r.status, Code: "s3_" + strings.ToLower(code)}
	if code == "" {
		e.Code = "http_status"
	}
	text := "unexpected answer"
	if code != "" {
		text += ", code " + code
	}
	if msg != "" {
		text += ", " + strconvQuote(msg)
	}
	e.Text = text
	switch {
	case code == "RequestTimeTooSkewed":
		// the cause is a clock (ours or the store's), not the credentials: it
		// is retried with backoff and the text names the clock
		e.Class, e.Code = anchor.ClassRetryable, "clock_skew"
		e.Text = "the store says the clock of this server and its own differ too much (RequestTimeTooSkewed): check the time of both"
	case r.status >= 300 && r.status < 400:
		e.Class, e.Code, e.Text = anchor.ClassPermanent, "redirect", "the store answered with a redirect (wrong endpoint or region?); redirects are not followed"
	case r.status == http.StatusNotFound && (code == "NoSuchBucket" || code == "ObjectLockConfigurationNotFoundError"):
		e.Class = anchor.ClassRefused
	case r.status == http.StatusBadRequest && isLockComplaint(code, msg):
		e.Class = anchor.ClassRefused
		e.Code = "object_lock_unavailable"
	case code == "InvalidBucketState" || code == "ObjectLockConfigurationNotFoundError":
		e.Class = anchor.ClassRefused
	case code == "SlowDown" || code == "RequestTimeout" || code == "OperationAborted" || code == "InternalError" || code == "ServiceUnavailable":
		e.Class = anchor.ClassRetryable
	default:
		e.Class = anchor.ClassOfStatus(r.status)
	}
	if e.Class == anchor.ClassRetryable {
		e.RetryAfter = anchor.RetryAfter(r.header, s.clock())
	}
	return e
}

func strconvQuote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `'`) + `"` }

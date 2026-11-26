// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package sigv4

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
)

// Algorithm is the SigV4 algorithm name.
const Algorithm = "AWS4-HMAC-SHA256"

// EmptyPayloadHash is the SHA-256 of the empty body, in hex.
const EmptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// AmzDateLayout is the layout of X-Amz-Date.
const AmzDateLayout = "20060102T150405Z"

// Credentials of the requester. Every value prints as redacted.
type Credentials struct {
	AccessKeyID     anchor.Secret
	SecretAccessKey anchor.Secret
	SessionToken    anchor.Secret // optional
}

// Signer signs requests for one region and service.
type Signer struct {
	Credentials Credentials
	Region      string
	Service     string // "s3"
	// NormalizePath applies the path normalisation of the generic services
	// (dot segments and repeated slashes removed). S3 does not normalise:
	// leave it false for S3.
	NormalizePath bool
	// OmitContentSHA256Header does not set X-Amz-Content-Sha256 (S3 requires
	// it; the generic test suite does not carry it).
	OmitContentSHA256Header bool
}

// HashPayload returns the lower case hex SHA-256 of b.
func HashPayload(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Header is one header line, in the order it was given.
type Header struct{ Name, Value string }

// Input is what the canonical request is built from.
type Input struct {
	Method string
	// Path is the decoded path ("" means "/").
	Path string
	// RawQuery is the query as sent, without '?'; it is decoded, re-encoded,
	// and sorted here.
	RawQuery    string
	Headers     []Header // all headers to be signed (Host included)
	PayloadHash string   // hex, or "UNSIGNED-PAYLOAD"
}

const upperhex = "0123456789ABCDEF"

func isUnreserved(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
		c == '-' || c == '_' || c == '.' || c == '~'
}

// Escape percent-encodes every byte except A-Z a-z 0-9 - _ . ~ ; with
// keepSlash '/' is also kept. Hex digits are upper case.
func Escape(s string, keepSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isUnreserved(c) || keepSlash && c == '/' {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(upperhex[c>>4])
			b.WriteByte(upperhex[c&15])
		}
	}
	return b.String()
}

func normalize(p string) string {
	if p == "" {
		return "/"
	}
	trailing := strings.HasSuffix(p, "/")
	var out []string
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "", ".":
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, seg)
		}
	}
	r := "/" + strings.Join(out, "/")
	if trailing && len(out) > 0 {
		r += "/"
	}
	return r
}

func unescape(s string) string {
	// decode %XX; '+' means space (as servers decode form-style queries);
	// a malformed escape is kept literally
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '+':
			b = append(b, ' ')
		case c == '%' && i+2 < len(s)+0 && ishex(s[i+1]) && ishex(s[i+2]):
			b = append(b, unhex(s[i+1])<<4|unhex(s[i+2]))
			i += 2
		default:
			b = append(b, c)
		}
	}
	return string(b)
}

func ishex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c >= 'a':
		return c - 'a' + 10
	case c >= 'A':
		return c - 'A' + 10
	}
	return c - '0'
}

func canonicalQuery(raw string) string {
	if raw == "" {
		return ""
	}
	type kv struct{ k, v string }
	var kvs []kv
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		kvs = append(kvs, kv{Escape(unescape(k), false), Escape(unescape(v), false)})
	}
	sort.Slice(kvs, func(i, j int) bool {
		if kvs[i].k != kvs[j].k {
			return kvs[i].k < kvs[j].k
		}
		return kvs[i].v < kvs[j].v
	})
	parts := make([]string, len(kvs))
	for i, p := range kvs {
		parts[i] = p.k + "=" + p.v
	}
	return strings.Join(parts, "&")
}

func foldSpaces(v string) string { return strings.Join(strings.Fields(v), " ") }

// Canonical builds the canonical request (without its trailing newline
// beyond the payload hash, as the AWS suite's .creq files) and the
// SignedHeaders value. normalizePath selects the generic-service path rule.
func Canonical(in Input, normalizePath bool) (creq, signedHeaders string) {
	path := in.Path
	if normalizePath {
		path = normalize(path)
	} else if path == "" {
		path = "/"
	}
	// group headers by lower-case name, values joined by ',' in given order
	vals := map[string][]string{}
	var names []string
	for _, h := range in.Headers {
		n := strings.ToLower(strings.TrimSpace(h.Name))
		if _, ok := vals[n]; !ok {
			names = append(names, n)
		}
		vals[n] = append(vals[n], foldSpaces(h.Value))
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString(in.Method)
	b.WriteByte('\n')
	b.WriteString(Escape(path, true))
	b.WriteByte('\n')
	b.WriteString(canonicalQuery(in.RawQuery))
	b.WriteByte('\n')
	for _, n := range names {
		b.WriteString(n)
		b.WriteByte(':')
		b.WriteString(strings.Join(vals[n], ","))
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	signedHeaders = strings.Join(names, ";")
	b.WriteString(signedHeaders)
	b.WriteByte('\n')
	b.WriteString(in.PayloadHash)
	return b.String(), signedHeaders
}

// Scope returns "<yyyymmdd>/<region>/<service>/aws4_request".
func Scope(t time.Time, region, service string) string {
	return t.UTC().Format("20060102") + "/" + region + "/" + service + "/aws4_request"
}

// StringToSign returns the string to sign for a canonical request.
func StringToSign(t time.Time, scope, creq string) string {
	h := sha256.Sum256([]byte(creq))
	return Algorithm + "\n" + t.UTC().Format(AmzDateLayout) + "\n" + scope + "\n" + hex.EncodeToString(h[:])
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// SigningKey derives the signing key: HMAC chain over date, region, service
// and "aws4_request" starting from "AWS4" + secret.
func SigningKey(secret string, t time.Time, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), t.UTC().Format("20060102"))
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	return hmacSHA256(k, "aws4_request")
}

// Signature returns the lower case hex signature of the string to sign.
func Signature(key []byte, stringToSign string) string {
	return hex.EncodeToString(hmacSHA256(key, stringToSign))
}

// Authorization returns the value of the Authorization header.
func Authorization(accessKeyID, scope, signedHeaders, signature string) string {
	return Algorithm + " Credential=" + accessKeyID + "/" + scope + ", SignedHeaders=" + signedHeaders + ", Signature=" + signature
}

// headers the HTTP stack may add or change after signing; never signed
var unsigned = map[string]bool{
	"authorization": true, "user-agent": true, "accept-encoding": true, "connection": true,
	"expect": true, "transfer-encoding": true, "content-length": true, "x-amzn-trace-id": true,
	"te": true, "upgrade": true, "keep-alive": true, "proxy-connection": true,
}

// Sign signs req with header signing. payloadHash is the lower case hex
// SHA-256 of the body, or "UNSIGNED-PAYLOAD". It sets X-Amz-Date,
// X-Amz-Security-Token (when a token is set), X-Amz-Content-Sha256 (unless
// omitted) and Authorization. Every other header present on req at the time,
// except those the HTTP stack manages, is signed, and so is Host (req.Host,
// else the URL host). Headers added after Sign are not covered.
func (s Signer) Sign(req *http.Request, payloadHash string, t time.Time) error {
	if s.Credentials.AccessKeyID == "" || s.Credentials.SecretAccessKey == "" {
		return errors.New("sigv4: credentials are empty")
	}
	if s.Region == "" || s.Service == "" {
		return errors.New("sigv4: region and service are required")
	}
	if len(payloadHash) == 0 {
		return errors.New("sigv4: payload hash is required")
	}
	if req.URL == nil {
		return errors.New("sigv4: request has no URL")
	}
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	if host == "" {
		return errors.New("sigv4: request has no host")
	}
	req.Header.Del("Authorization")
	req.Header.Set("X-Amz-Date", t.UTC().Format(AmzDateLayout))
	if s.Credentials.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", s.Credentials.SessionToken.Reveal())
	}
	if !s.OmitContentSHA256Header {
		req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	}
	in := Input{Method: req.Method, Path: req.URL.Path, RawQuery: req.URL.RawQuery, PayloadHash: payloadHash}
	if in.Method == "" {
		in.Method = http.MethodGet
	}
	in.Headers = append(in.Headers, Header{"host", host})
	keys := make([]string, 0, len(req.Header))
	for k := range req.Header {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if unsigned[strings.ToLower(k)] || strings.EqualFold(k, "host") {
			continue
		}
		for _, v := range req.Header[k] {
			in.Headers = append(in.Headers, Header{k, v})
		}
	}
	creq, signed := Canonical(in, s.NormalizePath)
	scope := Scope(t, s.Region, s.Service)
	sts := StringToSign(t, scope, creq)
	sig := Signature(SigningKey(s.Credentials.SecretAccessKey.Reveal(), t, s.Region, s.Service), sts)
	req.Header.Set("Authorization", Authorization(s.Credentials.AccessKeyID.Reveal(), scope, signed, sig))
	return nil
}

// Format implements fmt.Formatter: credentials never print, even when the
// value sits in an unexported field of another type.
func (c Credentials) Format(f fmt.State, verb rune) { _, _ = f.Write([]byte("sigv4.Credentials{...}")) }

// MarshalJSON implements json.Marshaler.
func (c Credentials) MarshalJSON() ([]byte, error) { return []byte(`"sigv4.Credentials"`), nil }

// Format implements fmt.Formatter: a Signer holds credentials.
func (s Signer) Format(f fmt.State, verb rune) {
	_, _ = fmt.Fprintf(f, "sigv4.Signer{Region:%s Service:%s}", s.Region, s.Service)
}

// MarshalJSON implements json.Marshaler.
func (s Signer) MarshalJSON() ([]byte, error) { return []byte(`"sigv4.Signer"`), nil }

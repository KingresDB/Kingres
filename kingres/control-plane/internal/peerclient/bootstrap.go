// SPDX-License-Identifier: PostgreSQL

package peerclient

import (
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
)

// ErrFingerprintMismatch means the certificate presented at first contact is
// not the one the administrator pinned; nothing was sent.
var ErrFingerprintMismatch = errors.New("peerclient: the certificate of the server does not match the pinned fingerprint")

// BootstrapCall makes one call to addr (host:port of an internal API) before
// the two sides share a CA (design control-plane-3.md, D4 step 1). The
// connection is TLS 1.3 without chain verification, because there is no common
// CA yet; instead the SHA-256 fingerprint of the certificate the server
// presents is checked against expected (hex, already normalized) before a
// single byte of the request is sent, and returned. An empty expected is
// trust on first use: the fingerprint that was seen is the caller's to record
// and show. No client certificate is presented.
func BootstrapCall(ctx context.Context, addr, expected, method, path string, header map[string]string, in, out any) (seen string, err error) {
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS13,
		// There is no CA to verify against; VerifyConnection below is the check.
		InsecureSkipVerify: true, //nolint:gosec // pinned or recorded fingerprint instead of a chain, see the function comment
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("the server presented no certificate")
			}
			seen = pki.Fingerprint(cs.PeerCertificates[0].Raw)
			if expected != "" && subtle.ConstantTimeCompare([]byte(seen), []byte(expected)) != 1 {
				return ErrFingerprintMismatch
			}
			return nil
		},
	}
	hc := &http.Client{
		Timeout:       2 * time.Minute,
		Transport:     &http.Transport{TLSClientConfig: cfg, DisableKeepAlives: true, TLSHandshakeTimeout: 10 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	var body io.Reader
	if in != nil {
		b, merr := json.Marshal(in)
		if merr != nil {
			return "", merr
		}
		body = bytes.NewReader(b)
	}
	target, err := internalapi.URL(addr, path)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return "", err
	}
	req.Header.Set(internalapi.HeaderVersion, internalapi.Version)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := hc.Do(req)
	if err != nil {
		if errors.Is(err, ErrFingerprintMismatch) {
			return seen, ErrFingerprintMismatch
		}
		return seen, fmt.Errorf("%w: %s: %w", ErrUnreachable, addr, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return seen, fmt.Errorf("%w: %s: %w", ErrUnreachable, addr, err)
	}
	if resp.StatusCode/100 != 2 {
		var pe struct{ Error, Message string }
		_ = json.Unmarshal(data, &pe)
		return seen, &APIError{Status: resp.StatusCode, Code: peerText(pe.Error, 64), Message: peerText(pe.Message, MaxPeerText)}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return seen, fmt.Errorf("peerclient: the answer of %s is not valid: %w", addr, err)
		}
	}
	return seen, nil
}

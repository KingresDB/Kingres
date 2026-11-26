// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package pki

import (
	"crypto/x509"
	"fmt"
	"sync"
	"time"
)

// Verifier checks peer certificates on a server that holds only the CA
// certificate (every server) and a revocation list the leader distributes.
type Verifier struct {
	pool *x509.CertPool

	mu      sync.RWMutex
	revoked map[string]bool
}

// NewVerifier returns a verifier trusting the CA certificate in PEM.
func NewVerifier(caPEM []byte) (*Verifier, error) {
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("pki: no certificate in the CA PEM")
	}
	return &Verifier{pool: p, revoked: map[string]bool{}}, nil
}

// Pool returns the trusted CA pool.
func (v *Verifier) Pool() *x509.CertPool { return v.pool }

// SetRevoked replaces the revoked serial numbers (hex).
func (v *Verifier) SetRevoked(serials []string) {
	m := make(map[string]bool, len(serials))
	for _, s := range serials {
		m[s] = true
	}
	v.mu.Lock()
	v.revoked = m
	v.mu.Unlock()
}

// VerifyPeer accepts a certificate signed by the CA, valid now, usable as a
// client certificate and not revoked, and returns its common name.
func (v *Verifier) VerifyPeer(cert *x509.Certificate, now time.Time) (string, error) {
	if _, err := cert.Verify(x509.VerifyOptions{Roots: v.pool, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return "", fmt.Errorf("pki: certificate not accepted: %w", err)
	}
	v.mu.RLock()
	r := v.revoked[SerialHex(cert.SerialNumber)]
	v.mu.RUnlock()
	if r {
		return "", ErrRevoked
	}
	return cert.Subject.CommonName, nil
}

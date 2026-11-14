// SPDX-License-Identifier: PostgreSQL

package cluster

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
)

// pkiState caches what a server knows of the cluster's PKI: the CA with its
// key on the server that created the cluster, only the CA certificate and the
// distributed revocation list on every other server.
type pkiState struct {
	mu       sync.Mutex
	ca       *pki.CA
	verifier *pki.Verifier
	caPEM    []byte
}

// CA returns the certificate authority held by this server (os.ErrNotExist
// when this server does not hold the CA key). One instance is shared by every
// user so that a revocation is seen by all of them.
func (s *Service) CA() (*pki.CA, error) {
	s.pki.mu.Lock()
	defer s.pki.mu.Unlock()
	if s.pki.ca != nil {
		return s.pki.ca, nil
	}
	ca, err := pki.LoadCA(s.PKIDir())
	if err != nil {
		return nil, err
	}
	s.pki.ca = ca
	return ca, nil
}

// HoldsCA reports whether this server holds the CA key.
func (s *Service) HoldsCA() bool {
	_, err := os.Stat(filepath.Join(s.PKIDir(), pki.CAKeyFile))
	return err == nil
}

// VerifyPeer implements internalapi.PeerVerifier for every server: the CA
// holder checks against its own revocation list, the others against the CA
// certificate and the revocation list the leader distributed.
func (s *Service) VerifyPeer(cert *x509.Certificate, now time.Time) (string, error) {
	if ca, err := s.CA(); err == nil {
		return ca.VerifyPeer(cert, now)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	caPEM, err := os.ReadFile(filepath.Join(s.PKIDir(), pki.CACertFile)) //nolint:gosec // fixed name below the pki directory
	if err != nil {
		return "", errors.New("this server has no cluster certificate authority yet")
	}
	s.pki.mu.Lock()
	if s.pki.verifier == nil || string(s.pki.caPEM) != string(caPEM) {
		v, verr := pki.NewVerifier(caPEM)
		if verr != nil {
			s.pki.mu.Unlock()
			return "", verr
		}
		s.pki.verifier, s.pki.caPEM = v, caPEM
	}
	v := s.pki.verifier
	s.pki.mu.Unlock()
	if serials, lerr := pki.LoadRevokedList(s.PKIDir()); lerr == nil {
		v.SetRevoked(serials)
	}
	return v.VerifyPeer(cert, now)
}

// RevokedSerials returns the revoked serial numbers this server knows.
func (s *Service) RevokedSerials() []string {
	if ca, err := s.CA(); err == nil {
		return ca.Revoked()
	}
	list, _ := pki.LoadRevokedList(s.PKIDir())
	return list
}

// NodeTLSConfig is the TLS configuration for a connection to the PostgreSQL
// instance of another server (or of this one through its registered address):
// this server's certificate is the client certificate, and the instance's
// certificate must be signed by the cluster CA and name host.
func (s *Service) NodeTLSConfig(host string) (*tls.Config, error) {
	caPEM, err := os.ReadFile(filepath.Join(s.PKIDir(), pki.CACertFile)) //nolint:gosec // fixed name below the pki directory
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("the CA certificate is not PEM")
	}
	pair, err := tls.LoadX509KeyPair(filepath.Join(s.PKIDir(), pki.ServerCertFile), filepath.Join(s.PKIDir(), pki.ServerKeyFile))
	if err != nil {
		return nil, err
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, ServerName: host, Certificates: []tls.Certificate{pair}}, nil
}

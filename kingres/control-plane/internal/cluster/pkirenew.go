// SPDX-License-Identifier: AGPL-3.0-only

package cluster

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// ErrNoCertificate means this server holds no certificate of a cluster.
var ErrNoCertificate = errors.New("this server holds no cluster certificate")

// CertificateLifetime is the validity of the certificates the CA of this server
// signs.
func (s *Service) CertificateLifetime() time.Duration { return s.cfg.CertificateLifetime }

// ServerCertificate reads this server's own certificate from its file.
func (s *Service) ServerCertificate() (*x509.Certificate, error) {
	b, err := os.ReadFile(filepath.Join(s.PKIDir(), pki.ServerCertFile)) //nolint:gosec // fixed name below the pki directory
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNoCertificate
		}
		return nil, err
	}
	return pki.ParseCertPEM(b)
}

// CACertificate reads the cluster CA certificate this server trusts.
func (s *Service) CACertificate() (*x509.Certificate, error) {
	b, err := os.ReadFile(filepath.Join(s.PKIDir(), pki.CACertFile)) //nolint:gosec // fixed name below the pki directory
	if err != nil {
		return nil, err
	}
	return pki.ParseCertPEM(b)
}

// RenewalCSR makes a certificate request for this server's names from the key
// it holds. A renewed certificate keeps the key: the certificate files, the
// key file and the configuration of the instance stay valid as they are, so a
// reload is enough (see "Certificates" in the README).
func (s *Service) RenewalCSR() ([]byte, error) {
	s.memberMu.Lock()
	defer s.memberMu.Unlock()
	key, err := os.ReadFile(filepath.Join(s.PKIDir(), pki.ServerKeyFile)) //nolint:gosec // fixed name below the pki directory
	if err != nil {
		return nil, ErrNoCertificate
	}
	return pki.CSRFromKey(key, s.ServerNames())
}

// InstallRenewed replaces this server's certificate by a renewed one after
// checking that it is signed by the CA this server trusts, names this server,
// belongs to its key and is valid, and stores the revocation list that came
// with it. The running instance re-reads the files with a reload (PostgreSQL
// reads ssl_cert_file, ssl_ca_file and ssl_crl_file again on SIGHUP); nothing
// is restarted: the internal API and the connections to other servers read
// the certificate file at every handshake.
func (s *Service) InstallRenewed(ctx context.Context, caPEM, certPEM, crlPEM string, revoked []string) error {
	s.memberMu.Lock()
	defer s.memberMu.Unlock()
	dir := s.PKIDir()
	have, err := os.ReadFile(filepath.Join(dir, pki.CACertFile)) //nolint:gosec // fixed name below the pki directory
	if err != nil || string(have) != caPEM {
		return errors.New("the CA certificate is not the one this server trusts")
	}
	key, err := os.ReadFile(filepath.Join(dir, pki.ServerKeyFile)) //nolint:gosec // fixed name below the pki directory
	if err != nil {
		return ErrNoCertificate
	}
	if err := pki.CheckServerCertificate(have, []byte(certPEM), key, s.cfg.ServerHost, time.Now()); err != nil {
		return fmt.Errorf("the renewed certificate was refused: %w", err)
	}
	if crlPEM != "" {
		ca, err := pki.ParseCertPEM(have)
		if err != nil {
			return err
		}
		if err := checkCRL([]byte(crlPEM), ca); err != nil {
			return err
		}
		if err := pki.WriteFile(filepath.Join(dir, pki.CRLFile), []byte(crlPEM), 0o644); err != nil { //nolint:gosec // a revocation list is public
			return err
		}
		if err := pki.SaveRevokedList(dir, revoked); err != nil {
			return err
		}
	}
	if err := pki.WriteFile(filepath.Join(dir, pki.ServerCertFile), []byte(certPEM), 0o644); err != nil { //nolint:gosec // a certificate is public
		return err
	}
	if s.mgr != nil && s.mgr.Exists(NodeID) {
		return s.reloadAll(ctx)
	}
	return nil
}

// MigrateSystem brings the schema kingres_cp of this server's own system
// database up to the version of this binary. The leader's copy holds the
// cluster's state; on every other server the schema holds what that server
// keeps for itself (its alerts). It runs at every start of a clustered server,
// after the instance is up, so that a new binary upgrades an existing cluster,
// and when a server has just joined. It is idempotent.
func (s *Service) MigrateSystem(ctx context.Context) error {
	if !s.Enabled() || !s.mgr.Exists(NodeID) {
		return nil
	}
	c, err := s.SystemConn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	// A server whose system database is a standby (it ran its group's primary before a failover and
	// runs no map primary) gets the schema by replication: there is nothing to write here.
	var recovery bool
	if err := c.QueryRow(ctx, `SELECT pg_is_in_recovery()`).Scan(&recovery); err == nil && recovery {
		return nil
	}
	if _, err := sysdb.Migrate(ctx, c); err != nil {
		return fmt.Errorf("upgrading the system database: %w", err)
	}
	// Idempotent: a cluster made before the hardening gets it at its next start.
	if err := HardenSystemDatabase(ctx, c); err != nil {
		return err
	}
	return nil
}

// CRLThisUpdate returns when the revocation list file of this server was issued.
// ok is false when this server does not hold the CA key (it only receives the
// list); a missing or unreadable file counts as issued at the zero time, so that
// the CA holder issues it.
func (s *Service) CRLThisUpdate() (time.Time, bool) {
	if !s.HoldsCA() {
		return time.Time{}, false
	}
	b, err := os.ReadFile(filepath.Join(s.PKIDir(), pki.CRLFile)) //nolint:gosec // fixed name below the pki directory
	if err != nil {
		return time.Time{}, true
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return time.Time{}, true
	}
	crl, err := x509.ParseRevocationList(blk.Bytes)
	if err != nil {
		return time.Time{}, true
	}
	return crl.ThisUpdate, true
}

// SPDX-License-Identifier: PostgreSQL

package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// More file names below the pki directory.
const (
	// ServerCertFile and ServerKeyFile are this server's own certificate and
	// key (the key never leaves the server that generated it).
	ServerCertFile = "server.crt"
	ServerKeyFile  = "server.key"
	// BootstrapCertFile and BootstrapKeyFile are the self-signed certificate
	// that a server without a cluster presents on its internal listener; it
	// exists only until the server holds a certificate of the cluster.
	BootstrapCertFile = "bootstrap.crt"
	BootstrapKeyFile  = "bootstrap.key"
	// CRLFile is the revocation list in the format PostgreSQL reads
	// (ssl_crl_file); RevokedListFile holds the same serial numbers for the
	// internal API of servers that do not hold the CA.
	CRLFile         = "revoked.crl"
	RevokedListFile = "revoked-serials.json"
)

// WriteFile writes a file below the pki directory atomically with the mode
// (private key files 0600).
func WriteFile(path string, b []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".pki-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if err := firstErr(werr, cerr, os.Chmod(name, mode)); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// ParseCertPEM parses one certificate in PEM.
func ParseCertPEM(b []byte) (*x509.Certificate, error) {
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "CERTIFICATE" {
		return nil, errors.New("pki: not a PEM certificate")
	}
	return x509.ParseCertificate(blk.Bytes)
}

// CSRPublicKeyDER returns the DER of the public key in a certificate request;
// two requests with the same key give the same value.
func CSRPublicKeyDER(csrPEM []byte) ([]byte, error) {
	blk, _ := pem.Decode(csrPEM)
	if blk == nil || blk.Type != "CERTIFICATE REQUEST" {
		return nil, fmt.Errorf("%w: not a PEM certificate request", ErrInvalidRequest)
	}
	csr, err := x509.ParseCertificateRequest(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	return x509.MarshalPKIXPublicKey(csr.PublicKey)
}

// CertMatchesKey reports whether cert was issued for the private key in keyPEM.
func CertMatchesKey(cert *x509.Certificate, keyPEM []byte) bool {
	b, _ := pem.Decode(keyPEM)
	if b == nil {
		return false
	}
	key, err := x509.ParseECPrivateKey(b.Bytes)
	if err != nil {
		return false
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	return ok && pub.Equal(&key.PublicKey)
}

// CheckServerCertificate verifies, before a server installs it, that certPEM
// is a certificate of the CA in caPEM, is valid now, names serverID, and was
// issued for the key in keyPEM.
func CheckServerCertificate(caPEM, certPEM, keyPEM []byte, serverID string, now time.Time) error {
	v, err := NewVerifier(caPEM)
	if err != nil {
		return err
	}
	cert, err := ParseCertPEM(certPEM)
	if err != nil {
		return err
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: v.Pool(), CurrentTime: now,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}); err != nil {
		return fmt.Errorf("pki: the certificate is not signed by the cluster CA: %w", err)
	}
	if cert.Subject.CommonName != serverID {
		return fmt.Errorf("pki: the certificate is for %q, not for this server", trunc(cert.Subject.CommonName))
	}
	if !CertMatchesKey(cert, keyPEM) {
		return errors.New("pki: the certificate does not belong to this server's key")
	}
	return nil
}

// NewBootstrapCert creates a self-signed certificate and key for a server that
// has no cluster yet. Peers pin its fingerprint (or record it on first use).
func NewBootstrapCert(n Names, now time.Time) (certPEM, keyPEM []byte, err error) {
	if err := n.Validate(); err != nil {
		return nil, nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	sn, err := randSerial()
	if err != nil {
		return nil, nil, err
	}
	dns := append([]string(nil), n.DNS...)
	ips := append([]net.IP(nil), n.IPs...)
	if ip := net.ParseIP(n.CommonName); ip != nil {
		ips = append(ips, ip)
	} else {
		dns = append(dns, n.CommonName)
	}
	tpl := &x509.Certificate{
		SerialNumber: sn, Subject: pkix.Name{CommonName: n.CommonName, Organization: []string{"KingresDB bootstrap"}},
		DNSNames: dns, IPAddresses: ips,
		NotBefore: now.Add(-5 * time.Minute).UTC(), NotAfter: now.Add(365 * 24 * time.Hour).UTC(),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), nil
}

// EnsureBootstrapCert returns the bootstrap certificate of dir, creating it
// when missing, and its fingerprint.
func EnsureBootstrapCert(dir string, n Names, now time.Time) (tls.Certificate, string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, "", err
	}
	cp, kp := filepath.Join(dir, BootstrapCertFile), filepath.Join(dir, BootstrapKeyFile)
	if _, err := os.Stat(kp); err != nil {
		c, k, err := NewBootstrapCert(n, now)
		if err != nil {
			return tls.Certificate{}, "", err
		}
		if err := WriteFile(kp, k, 0o600); err != nil {
			return tls.Certificate{}, "", err
		}
		if err := WriteFile(cp, c, 0o644); err != nil { //nolint:gosec // a certificate is public
			return tls.Certificate{}, "", err
		}
	}
	pair, err := tls.LoadX509KeyPair(cp, kp)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	return pair, Fingerprint(pair.Certificate[0]), nil
}

// RevokedListDoc is the content of RevokedListFile.
type RevokedListDoc struct {
	Serials []string `json:"serials"`
}

// SaveRevokedList writes the serial numbers a follower's internal API refuses.
func SaveRevokedList(dir string, serials []string) error {
	b, err := json.Marshal(RevokedListDoc{Serials: serials})
	if err != nil {
		return err
	}
	return WriteFile(filepath.Join(dir, RevokedListFile), b, 0o600)
}

// LoadRevokedList reads RevokedListFile; a missing file is an empty list.
func LoadRevokedList(dir string) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(dir, RevokedListFile)) //nolint:gosec // fixed name below the pki directory
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f RevokedListDoc
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("pki: %s: %w", RevokedListFile, err)
	}
	return f.Serials, nil
}

// NormalizeFingerprint turns a user-supplied SHA-256 fingerprint (hex with or
// without colons, any case) into lower-case hex, or "" when it is not one.
func NormalizeFingerprint(s string) string {
	s = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), ":", ""))
	if len(s) != 64 {
		return ""
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return ""
		}
	}
	return s
}

// FormatFingerprint renders hex as upper-case pairs separated by colons.
func FormatFingerprint(hexFP string) string {
	var sb strings.Builder
	for i := 0; i+1 < len(hexFP); i += 2 {
		if i > 0 {
			sb.WriteByte(':')
		}
		sb.WriteString(strings.ToUpper(hexFP[i : i+2]))
	}
	return sb.String()
}

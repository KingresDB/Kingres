// SPDX-License-Identifier: PostgreSQL

// Package pki is the certificate authority of a cluster (design
// control-plane-3.md, D1). The CA signs only server certificates of its own
// cluster, only from a certificate request whose key the server generated
// itself (ECDSA P-256), and only for the names the caller is allowed to
// vouch for. Private keys of servers never reach the CA; the CA key never
// leaves the manager leader's directory. Revoked serial numbers are kept in
// a file next to the CA and published as a certificate revocation list.
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Lifetimes (D1).
const (
	CALifetime     = 10 * 365 * 24 * time.Hour
	ServerLifetime = 365 * 24 * time.Hour
)

// File names below the pki directory.
const (
	CACertFile  = "ca.crt"
	CAKeyFile   = "ca.key"
	RevokedFile = "revoked.json"
)

// Errors.
var (
	// ErrInvalidRequest means a certificate request was refused.
	ErrInvalidRequest = errors.New("certificate request refused")
	// ErrRevoked means a certificate was revoked.
	ErrRevoked = errors.New("certificate revoked")
)

// Names are the identities a server certificate carries.
type Names struct {
	// CommonName is the server id (its host name as registered in the cluster).
	CommonName string
	// DNS and IPs are the subject alternative names. The common name is
	// always among the DNS names (or IPs when it is an address).
	DNS []string
	IPs []net.IP
}

var hostRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)

// Validate checks the names: a CommonName that is a host name or an IP
// address, DNS names that are host names, and at most 16 names in all.
func (n Names) Validate() error {
	if !hostRE.MatchString(n.CommonName) {
		return fmt.Errorf("%w: %q is not a host name", ErrInvalidRequest, trunc(n.CommonName))
	}
	if len(n.DNS)+len(n.IPs) > 16 {
		return fmt.Errorf("%w: too many names", ErrInvalidRequest)
	}
	for _, d := range n.DNS {
		if !hostRE.MatchString(d) {
			return fmt.Errorf("%w: %q is not a host name", ErrInvalidRequest, trunc(d))
		}
	}
	return nil
}

func trunc(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}

// CA is a loaded certificate authority.
type CA struct {
	dir  string
	cert *x509.Certificate
	pem  []byte
	key  *ecdsa.PrivateKey

	mu      sync.Mutex
	revoked map[string]time.Time // serial (hex) -> revocation time
	crlNum  int64
}

func randSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 126))
}

// CreateCA creates a new CA in dir (created with mode 0700). It fails when a
// CA already exists there.
func CreateCA(dir, clusterName string, now time.Time) (*CA, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(dir, CAKeyFile)); err == nil {
		return nil, errors.New("pki: a CA already exists in " + dir)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randSerial()
	if err != nil {
		return nil, err
	}
	tpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "KingresDB cluster CA " + clusterName, Organization: []string{"KingresDB"}},
		NotBefore: now.Add(-5 * time.Minute).UTC(), NotAfter: now.Add(CALifetime).UTC(),
		IsCA: true, BasicConstraintsValid: true, MaxPathLen: 0, MaxPathLenZero: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	if err := writeNew(filepath.Join(dir, CAKeyFile), keyPEM, 0o600); err != nil {
		return nil, err
	}
	if err := writeNew(filepath.Join(dir, CACertFile), certPEM, 0o644); err != nil { //nolint:gosec // a certificate is public
		return nil, err
	}
	return LoadCA(dir)
}

// writeNew writes a file that must not exist yet.
func writeNew(path string, b []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode) //nolint:gosec // path below the pki directory
	if err != nil {
		return err
	}
	_, werr := f.Write(b)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// LoadCA loads the CA of dir.
func LoadCA(dir string) (*CA, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, CACertFile)) //nolint:gosec // path below the pki directory
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, CAKeyFile)) //nolint:gosec // path below the pki directory
	if err != nil {
		return nil, err
	}
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return nil, errors.New("pki: the CA files are not PEM")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, err
	}
	ca := &CA{dir: dir, cert: cert, pem: certPEM, key: key, revoked: map[string]time.Time{}}
	if b, err := os.ReadFile(filepath.Join(dir, RevokedFile)); err == nil { //nolint:gosec // path below the pki directory
		var m struct {
			Serials map[string]time.Time `json:"serials"`
			CRL     int64                `json:"crlNumber"`
		}
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("pki: reading the revocation list: %w", err)
		}
		if m.Serials != nil {
			ca.revoked = m.Serials
		}
		ca.crlNum = m.CRL
	}
	return ca, nil
}

// CertPEM returns the CA certificate.
func (ca *CA) CertPEM() []byte { return append([]byte(nil), ca.pem...) }

// Cert returns the CA certificate.
func (ca *CA) Cert() *x509.Certificate { return ca.cert }

// Pool returns a pool holding only the CA certificate.
func (ca *CA) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.cert)
	return p
}

// NewKeyAndCSR generates an ECDSA P-256 key and a certificate request for
// names. The key stays with the caller (the server that will use it).
func NewKeyAndCSR(n Names) (keyPEM, csrPEM []byte, err error) {
	if err := n.Validate(); err != nil {
		return nil, nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	csrPEM, err = csrFor(key, n)
	return keyPEM, csrPEM, err
}

// CSRFromKey creates a certificate request for names from an existing key (a
// server that resumes a join keeps the key it generated).
func CSRFromKey(keyPEM []byte, n Names) ([]byte, error) {
	if err := n.Validate(); err != nil {
		return nil, err
	}
	b, _ := pem.Decode(keyPEM)
	if b == nil {
		return nil, errors.New("pki: the key is not PEM")
	}
	key, err := x509.ParseECPrivateKey(b.Bytes)
	if err != nil {
		return nil, err
	}
	return csrFor(key, n)
}

func csrFor(key *ecdsa.PrivateKey, n Names) ([]byte, error) {
	dns := append([]string(nil), n.DNS...)
	ips := append([]net.IP(nil), n.IPs...)
	if ip := net.ParseIP(n.CommonName); ip != nil {
		// An address as the server id is a name of the certificate too: peers
		// verify the certificate against the address they dial.
		ips = append(ips, ip)
	} else if !contains(dns, n.CommonName) {
		dns = append([]string{n.CommonName}, dns...)
	}
	tpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: n.CommonName}, DNSNames: dns, IPAddresses: ips}
	der, err := x509.CreateCertificateRequest(rand.Reader, tpl, key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// Sign verifies a certificate request and signs a server certificate. The
// request must be self-signed correctly, carry an ECDSA P-256 key, have the
// allowed common name, and ask for no name outside allowed (names are what the
// caller vouches for, never what the request claims). Extensions of the
// request other than the names are ignored: the certificate always has the
// key usages of a server and client of this cluster and is never a CA.
func (ca *CA) Sign(csrPEM []byte, allowed Names, now time.Time) (certPEM []byte, serial string, err error) {
	if err := allowed.Validate(); err != nil {
		return nil, "", err
	}
	blk, _ := pem.Decode(csrPEM)
	if blk == nil || blk.Type != "CERTIFICATE REQUEST" {
		return nil, "", fmt.Errorf("%w: not a PEM certificate request", ErrInvalidRequest)
	}
	csr, err := x509.ParseCertificateRequest(blk.Bytes)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, "", fmt.Errorf("%w: the request signature is wrong", ErrInvalidRequest)
	}
	pub, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return nil, "", fmt.Errorf("%w: the key must be ECDSA P-256", ErrInvalidRequest)
	}
	if csr.Subject.CommonName != allowed.CommonName {
		return nil, "", fmt.Errorf("%w: the common name %q is not the one expected for this server", ErrInvalidRequest, trunc(csr.Subject.CommonName))
	}
	okDNS := map[string]bool{allowed.CommonName: true}
	for _, d := range allowed.DNS {
		okDNS[d] = true
	}
	for _, d := range csr.DNSNames {
		if !okDNS[d] {
			return nil, "", fmt.Errorf("%w: the name %q is not allowed", ErrInvalidRequest, trunc(d))
		}
	}
	okIP := map[string]bool{}
	for _, ip := range allowed.IPs {
		okIP[ip.String()] = true
	}
	if ip := net.ParseIP(allowed.CommonName); ip != nil {
		okIP[ip.String()] = true
	}
	for _, ip := range csr.IPAddresses {
		if !okIP[ip.String()] {
			return nil, "", fmt.Errorf("%w: the address %s is not allowed", ErrInvalidRequest, ip)
		}
	}
	if len(csr.EmailAddresses)+len(csr.URIs) > 0 {
		return nil, "", fmt.Errorf("%w: only host names and addresses are allowed", ErrInvalidRequest)
	}
	sn, err := randSerial()
	if err != nil {
		return nil, "", err
	}
	tpl := &x509.Certificate{
		SerialNumber: sn, Subject: pkix.Name{CommonName: allowed.CommonName, Organization: []string{"KingresDB"}},
		DNSNames: csr.DNSNames, IPAddresses: csr.IPAddresses,
		NotBefore: now.Add(-5 * time.Minute).UTC(), NotAfter: now.Add(ServerLifetime).UTC(),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true, IsCA: false,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.cert, pub, ca.key)
	if err != nil {
		return nil, "", err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), SerialHex(sn), nil
}

// SerialHex formats a serial number.
func SerialHex(sn *big.Int) string { return hex.EncodeToString(sn.Bytes()) }

// NeedsRenewal reports whether less than a third of the lifetime of cert is
// left at now (D1).
func NeedsRenewal(cert *x509.Certificate, now time.Time) bool {
	life := cert.NotAfter.Sub(cert.NotBefore)
	return cert.NotAfter.Sub(now) < life/3
}

// Fingerprint is the SHA-256 of a DER certificate as lower-case hex.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// Revoke records a serial number as revoked and publishes the list.
func (ca *CA) Revoke(serial string, now time.Time) error {
	serial = strings.ToLower(serial)
	if _, err := hex.DecodeString(serial); err != nil || serial == "" {
		return errors.New("pki: invalid serial number")
	}
	ca.mu.Lock()
	defer ca.mu.Unlock()
	if _, ok := ca.revoked[serial]; ok {
		return nil
	}
	ca.revoked[serial] = now.UTC()
	return ca.saveLocked()
}

func (ca *CA) saveLocked() error {
	b, err := json.Marshal(struct {
		Serials map[string]time.Time `json:"serials"`
		CRL     int64                `json:"crlNumber"`
	}{ca.revoked, ca.crlNum})
	if err != nil {
		return err
	}
	tmp := filepath.Join(ca.dir, RevokedFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(ca.dir, RevokedFile))
}

// IsRevoked reports whether a serial number (hex) is revoked.
func (ca *CA) IsRevoked(serial string) bool {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	_, ok := ca.revoked[strings.ToLower(serial)]
	return ok
}

// Revoked returns the revoked serial numbers, sorted.
func (ca *CA) Revoked() []string {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	out := make([]string, 0, len(ca.revoked))
	for s := range ca.revoked {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// CRL returns the certificate revocation list in PEM (the file PostgreSQL
// reads as ssl_crl_file).
func (ca *CA) CRL(now time.Time) ([]byte, error) {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	var entries []x509.RevocationListEntry
	for s, t := range ca.revoked {
		b, err := hex.DecodeString(s)
		if err != nil {
			continue
		}
		entries = append(entries, x509.RevocationListEntry{SerialNumber: new(big.Int).SetBytes(b), RevocationTime: t})
	}
	ca.crlNum++
	if err := ca.saveLocked(); err != nil {
		return nil, err
	}
	der, err := x509.CreateRevocationList(rand.Reader, &x509.RevocationList{
		Number: big.NewInt(ca.crlNum), ThisUpdate: now.UTC(), NextUpdate: now.Add(30 * 24 * time.Hour).UTC(),
		RevokedCertificateEntries: entries,
	}, ca.cert, ca.key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "X509 CRL", Bytes: der}), nil
}

// VerifyPeer checks that cert was signed by the CA, is valid now, is usable as
// a client and server certificate, and is not revoked. It returns the common
// name (the server id).
func (ca *CA) VerifyPeer(cert *x509.Certificate, now time.Time) (string, error) {
	opts := x509.VerifyOptions{Roots: ca.Pool(), CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	if _, err := cert.Verify(opts); err != nil {
		return "", fmt.Errorf("pki: certificate not accepted: %w", err)
	}
	if ca.IsRevoked(SerialHex(cert.SerialNumber)) {
		return "", ErrRevoked
	}
	return cert.Subject.CommonName, nil
}

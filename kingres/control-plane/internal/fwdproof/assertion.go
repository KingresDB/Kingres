// SPDX-License-Identifier: AGPL-3.0-only

package fwdproof

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"
)

// A gather call goes to every member of the cluster. The proof of a session
// reveals the user's ClientKey to whoever checks it (see the package comment),
// so it must go to the leader only: the asking server sends its proof to the
// leader (KindGatherAuthorize), and the leader, which is trusted with the
// verifiers anyway, answers with an Assertion that it signed with the key of its
// own server certificate. The members that answer the gather call accept that
// assertion and never see a proof.

// KindGatherAuthorize is the kind of the proof a server sends to the leader to
// get an assertion for a gather call.
const KindGatherAuthorize Kind = "gather-authorize"

// Assertion is the leader's statement that the asking server holds a session of
// the user a gather call names.
type Assertion struct {
	// Leader is the certificate (DER) of the leader that signed it.
	Leader []byte `json:"leader"`
	// IssuedAt is the leader's clock, in Unix seconds; Nonce is 16 random bytes.
	IssuedAt int64  `json:"issuedAt"`
	Nonce    []byte `json:"nonce"`
	// Signature is the signature of the key of Leader over assertionDigest.
	Signature []byte `json:"signature"`
}

// assertionMessage turns the proof message of the asking server into what the
// assertion is about: the same request, for any receiver. The ordinal is a fact
// about the receiver, not about the user.
func assertionMessage(m Message) Message {
	m.Kind, m.To, m.Ordinal, m.Pin = KindGather, "", 0, ""
	return m
}

func assertionDigest(m Message, issuedAt int64, nonce []byte) []byte {
	h := sha256.New()
	h.Write([]byte("kingres gather assertion v1"))
	h.Write(assertionMessage(m).digest())
	var t [8]byte
	binary.BigEndian.PutUint64(t[:], uint64(issuedAt))
	h.Write(t[:])
	h.Write(nonce)
	return h.Sum(nil)
}

// Signer makes assertions with the key of this server's certificate. It is the
// leader's.
type Signer struct {
	// Load returns the server's current certificate and key.
	Load func() (tls.Certificate, error)
	Now  func() time.Time
}

// Sign signs an assertion for the gather request m (m.From is the asking server).
func (s *Signer) Sign(m Message) (Assertion, error) {
	pair, err := s.Load()
	if err != nil {
		return Assertion{}, fmt.Errorf("fwdproof: loading the server certificate: %w", err)
	}
	signer, ok := pair.PrivateKey.(crypto.Signer)
	if !ok || len(pair.Certificate) == 0 {
		return Assertion{}, errors.New("fwdproof: the server certificate has no usable key")
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return Assertion{}, fmt.Errorf("fwdproof: random nonce: %w", err)
	}
	issued := now().Unix()
	sig, err := signer.Sign(rand.Reader, assertionDigest(m, issued, nonce), crypto.SHA256)
	if err != nil {
		return Assertion{}, fmt.Errorf("fwdproof: signing: %w", err)
	}
	return Assertion{Leader: pair.Certificate[0], IssuedAt: issued, Nonce: nonce, Signature: sig}, nil
}

// PeerVerifier accepts a certificate of the cluster and returns the server id in
// it (*pki.CA, *pki.Verifier and cluster.Service implement it); it checks the
// chain, the usage, the validity and the revocation list.
type PeerVerifier interface {
	VerifyPeer(cert *x509.Certificate, now time.Time) (string, error)
}

// AssertionVerifier checks the assertions of gather calls at a member.
type AssertionVerifier struct {
	// Certs verifies the leader's certificate.
	Certs PeerVerifier
	// Leader is this server's own view of the manager leader.
	Leader func() string
	Window time.Duration
	Now    func() time.Time

	mu        sync.Mutex
	seen      map[[16]byte]time.Time
	lastSweep time.Time
}

// Check verifies assertion a for the gather request m sent by the server m.From
// (the caller of the internal API, verified by its certificate): the leader's
// certificate is accepted by the cluster, its server id is the leader this server
// knows, the signature is valid, the time is within the window, and the
// assertion was not used before.
func (v *AssertionVerifier) Check(_ context.Context, m Message, a Assertion) error {
	return v.check(a, func(issued int64, nonce []byte) []byte { return assertionDigest(m, issued, nonce) }, func(id string) error {
		if l := v.Leader(); l == "" || l != id {
			return fmt.Errorf("%w: the signer is not the manager leader", ErrInvalid)
		}
		return nil
	})
}

func (v *AssertionVerifier) check(a Assertion, digest func(issued int64, nonce []byte) []byte, signerOK func(id string) error) error {
	now := time.Now
	if v.Now != nil {
		now = v.Now
	}
	window := v.Window
	if window <= 0 {
		window = DefaultWindow
	}
	if len(a.Nonce) != 16 || len(a.Signature) == 0 || len(a.Leader) == 0 || a.IssuedAt == 0 {
		return ErrInvalid
	}
	if d := now().Sub(time.Unix(a.IssuedAt, 0)); d > window || d < -window {
		return ErrExpired
	}
	cert, err := x509.ParseCertificate(a.Leader)
	if err != nil {
		return ErrInvalid
	}
	id, err := v.Certs.VerifyPeer(cert, now())
	if err != nil {
		return fmt.Errorf("%w: the signer's certificate is not accepted: %w", ErrInvalid, err)
	}
	if err := signerOK(id); err != nil {
		return err
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !ecdsa.VerifyASN1(pub, digest(a.IssuedAt, a.Nonce), a.Signature) {
		return ErrInvalid
	}
	var key [16]byte
	copy(key[:], a.Nonce)
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.seen == nil {
		v.seen = map[[16]byte]time.Time{}
	}
	if t := now(); t.Sub(v.lastSweep) > time.Second {
		for k, at := range v.seen {
			if t.Sub(at) > 2*window {
				delete(v.seen, k)
			}
		}
		v.lastSweep = t
	}
	if _, dup := v.seen[key]; dup {
		return ErrReplay
	}
	if len(v.seen) >= maxSeen {
		return fmt.Errorf("%w: too many assertions in the window", ErrUnavailable)
	}
	v.seen[key] = now()
	return nil
}

// A member's attestation (phase 5 review H1 and M-D). The declaration that a server is lost goes to the old leader of the map group, whose instance (the system
// database) is stopped while it is held back, so it cannot read a verifier and cannot check the proof of the operator's session, and the proof must not be sent to a
// member that is not the leader anyway (the receiver learns the password-equivalent ClientKey). The member that received the operator's request has a live store and
// has authenticated the session; it signs the declaration with the key of its server certificate and sends that, never the proof. The receiver checks that the
// signer is the server it is talking to, that its certificate is accepted by the cluster, the freshness and that it was not used before.

func attestationDigest(m Message, issuedAt int64, nonce []byte) []byte {
	h := sha256.New()
	h.Write([]byte("kingres member attestation v1"))
	h.Write(m.digest())
	var t [8]byte
	binary.BigEndian.PutUint64(t[:], uint64(issuedAt))
	h.Write(t[:])
	h.Write(nonce)
	return h.Sum(nil)
}

// Attest signs the exact message m (m.From is this server): an attestation is for one receiver and one request.
func (s *Signer) Attest(m Message) (Assertion, error) {
	pair, err := s.Load()
	if err != nil {
		return Assertion{}, fmt.Errorf("fwdproof: loading the server certificate: %w", err)
	}
	signer, ok := pair.PrivateKey.(crypto.Signer)
	if !ok || len(pair.Certificate) == 0 {
		return Assertion{}, errors.New("fwdproof: the server certificate has no usable key")
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return Assertion{}, fmt.Errorf("fwdproof: random nonce: %w", err)
	}
	issued := now().Unix()
	sig, err := signer.Sign(rand.Reader, attestationDigest(m, issued, nonce), crypto.SHA256)
	if err != nil {
		return Assertion{}, fmt.Errorf("fwdproof: signing: %w", err)
	}
	return Assertion{Leader: pair.Certificate[0], IssuedAt: issued, Nonce: nonce, Signature: sig}, nil
}

// CheckAttestation verifies the attestation a of the message m: the signing certificate is accepted by the cluster and names the server m.From (the caller of the
// internal API, verified by its certificate), the signature is valid, the time is within the window, and it was not used before. Leader is not used.
func (v *AssertionVerifier) CheckAttestation(_ context.Context, m Message, a Assertion) error {
	if m.Kind != KindMapLost {
		return fmt.Errorf("%w: an attestation is made for the declaration that a server is lost only", ErrInvalid)
	}
	return v.check(a, func(issued int64, nonce []byte) []byte { return attestationDigest(m, issued, nonce) }, func(id string) error {
		if m.From == "" || id != m.From {
			return fmt.Errorf("%w: the signer is not the server that sent the request", ErrInvalid)
		}
		return nil
	})
}

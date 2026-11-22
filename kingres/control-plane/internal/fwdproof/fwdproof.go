// SPDX-License-Identifier: AGPL-3.0-only

// Package fwdproof proves, to the server that runs a forwarded or gathered
// operation, that the forwarding server really holds a session of the user it
// names (review finding CP2-01).
//
// A certificate of the cluster only says "this is a server". It must not be
// enough to act as any role by name, so the forwarding server adds a proof
// that only somebody who has seen the user's password can make: at sign-in
// it derives the SCRAM ClientKey of the password (from the salt and iteration
// count of the role's stored verifier) and keeps it in the session. Per
// request it sends
//
//	ClientProof = ClientKey XOR HMAC(StoredKey, digest(request) || issuedAt || nonce)
//
// which is the proof of RFC 5802 with the request in place of the SCRAM
// authentication message. The receiver holds the verifier (StoredKey,
// ServerKey), recovers ClientKey' = ClientProof XOR HMAC(...) and accepts when
// SHA-256(ClientKey') equals StoredKey.
//
// **The receiver learns the user's ClientKey**, exactly as a SCRAM server does,
// and the ClientKey is password-equivalent (it makes proofs for that user toward
// any server). A proof is therefore sent to the manager leader only, which holds
// every verifier and is trusted with everything anyway (a compromised leader is a
// compromised cluster); the calls that go to every member (gather) carry an
// assertion signed by the leader instead (assertion.go). A proof is bound to one
// request, one sender, one receiver and a short time window, and a changed
// password stops it working.
package fwdproof

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scram"
)

// Errors of Check.
var (
	// ErrInvalid: the proof is missing, malformed or does not match the verifier.
	ErrInvalid = errors.New("fwdproof: the proof of the session is not valid")
	// ErrExpired: issuedAt is outside the accepted window.
	ErrExpired = errors.New("fwdproof: the proof is outside the accepted time window")
	// ErrReplay: this proof was already used.
	ErrReplay = errors.New("fwdproof: the proof was already used")
	// ErrUnavailable: the verifier of the role could not be read.
	ErrUnavailable = errors.New("fwdproof: the verifier of the role cannot be read")
)

// Kind separates the calls a proof may be used for.
type Kind string

// The two calls that carry an identity.
const (
	KindForward Kind = "forward"
	KindGather  Kind = "gather"
	// KindForwardStream is the proof of the streaming relay (POST /forward-stream, contract 0.19.0): the frame carries the request
	// headers the two streaming operations use, and the body is not part of the proof (it is raw bytes of any size, on the mutual TLS
	// connection between two servers of the cluster).
	KindForwardStream Kind = "forward-stream"
)

// Proof is what travels with a request.
type Proof struct {
	// IssuedAt is the sender's clock, in Unix seconds.
	IssuedAt int64 `json:"issuedAt"`
	// Nonce is 16 random bytes, never used twice.
	Nonce []byte `json:"nonce"`
	// Value is ClientProof, 32 bytes.
	Value []byte `json:"value"`
}

// Message is everything a proof is bound to. A field that the receiver acts
// on must be in it: a proof for one request cannot be turned into another.
type Message struct {
	Kind          Kind
	From, To      string
	Username      string
	Roles         []string
	Operation     string
	PathParams    map[string]string
	Query         string
	ContentType   string
	Body          []byte
	RequestID     string
	Source        string
	SourceAddress string
	Pin           string
	Ordinal       int
	// Headers are the request headers that travel with a streaming call (Range, Upload-Offset, Content-Length, Content-Type).
	Headers map[string]string
}

// digest is SHA-256 over the fields, each with a label and a length, so that
// no two different messages have the same encoding.
func (m Message) digest() []byte {
	h := sha256.New()
	put := func(label string, v []byte) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(label)))
		h.Write(n[:])
		h.Write([]byte(label))
		binary.BigEndian.PutUint64(n[:], uint64(len(v)))
		h.Write(n[:])
		h.Write(v)
	}
	str := func(label, v string) { put(label, []byte(v)) }
	str("kind", string(m.Kind))
	str("from", m.From)
	str("to", m.To)
	str("user", m.Username)
	roles := append([]string(nil), m.Roles...)
	sort.Strings(roles)
	for _, r := range roles {
		str("role", r)
	}
	str("operation", m.Operation)
	keys := make([]string, 0, len(m.PathParams))
	for k := range m.PathParams {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		str("param-name", k)
		str("param-value", m.PathParams[k])
	}
	str("query", m.Query)
	str("content-type", m.ContentType)
	bodySum := sha256.Sum256(m.Body)
	put("body", bodySum[:])
	str("request-id", m.RequestID)
	str("source", m.Source)
	str("source-address", m.SourceAddress)
	str("pin", m.Pin)
	str("ordinal", fmt.Sprint(m.Ordinal))
	// Only a message that has headers adds to the digest, so that the proofs of the other calls are what they were.
	if len(m.Headers) > 0 {
		hk := make([]string, 0, len(m.Headers))
		for k := range m.Headers {
			hk = append(hk, k)
		}
		sort.Strings(hk)
		for _, k := range hk {
			str("header-name", k)
			str("header-value", m.Headers[k])
		}
	}
	return h.Sum(nil)
}

func mask(storedKey, digest []byte, issuedAt int64, nonce []byte) []byte {
	mac := hmac.New(sha256.New, storedKey)
	mac.Write(digest)
	var t [8]byte
	binary.BigEndian.PutUint64(t[:], uint64(issuedAt))
	mac.Write(t[:])
	mac.Write(nonce)
	return mac.Sum(nil)
}

// Sign makes the proof of a message from the ClientKey of the session.
func Sign(clientKey []byte, m Message, now time.Time) (Proof, error) {
	if len(clientKey) != sha256.Size {
		return Proof{}, errors.New("fwdproof: the session has no key to prove itself with")
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return Proof{}, fmt.Errorf("fwdproof: random nonce: %w", err)
	}
	issued := now.Unix()
	mk := mask(scram.Stored(clientKey), m.digest(), issued, nonce)
	value := make([]byte, sha256.Size)
	subtle.XORBytes(value, clientKey, mk)
	return Proof{IssuedAt: issued, Nonce: nonce, Value: value}, nil
}

// KeySource gives the StoredKey of a role's current verifier.
type KeySource interface {
	// StoredKey returns scram.ErrNotSCRAM (wrapped) for a role without a
	// SCRAM-SHA-256 password and an error for a role that does not exist.
	StoredKey(ctx context.Context, username string) ([]byte, error)
}

// DefaultWindow is how far a proof's issuedAt may differ from the receiver's clock.
const DefaultWindow = 2 * time.Minute

// maxSeen bounds the replay cache. A proof is only remembered after it
// verified, so it cannot be filled by a caller that has no valid session.
const maxSeen = 100000

// Checker verifies proofs and remembers the nonces it accepted.
type Checker struct {
	keys   KeySource
	window time.Duration
	now    func() time.Time

	mu        sync.Mutex
	seen      map[[16]byte]time.Time
	lastSweep time.Time
}

// NewChecker builds a checker. window <= 0 uses DefaultWindow, now may be nil.
func NewChecker(keys KeySource, window time.Duration, now func() time.Time) *Checker {
	if window <= 0 {
		window = DefaultWindow
	}
	if now == nil {
		now = time.Now
	}
	return &Checker{keys: keys, window: window, now: now, seen: map[[16]byte]time.Time{}}
}

// Check verifies p for m against the current verifier of m.Username.
func (c *Checker) Check(ctx context.Context, m Message, p Proof) error {
	if len(p.Nonce) != 16 || len(p.Value) != sha256.Size || p.IssuedAt == 0 {
		return ErrInvalid
	}
	now := c.now()
	if d := now.Sub(time.Unix(p.IssuedAt, 0)); d > c.window || d < -c.window {
		return ErrExpired
	}
	stored, err := c.keys.StoredKey(ctx, m.Username)
	switch {
	case errors.Is(err, scram.ErrNotSCRAM):
		return fmt.Errorf("%w: the role has no SCRAM-SHA-256 password", ErrInvalid)
	case err != nil:
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	case len(stored) != sha256.Size:
		return ErrInvalid
	}
	key := make([]byte, sha256.Size)
	subtle.XORBytes(key, p.Value, mask(stored, m.digest(), p.IssuedAt, p.Nonce))
	if subtle.ConstantTimeCompare(scram.Stored(key), stored) != 1 {
		return ErrInvalid
	}
	var id [16]byte
	copy(id[:], p.Nonce)
	c.mu.Lock()
	defer c.mu.Unlock()
	if now.Sub(c.lastSweep) > time.Second {
		for k, t := range c.seen {
			if now.Sub(t) > 2*c.window {
				delete(c.seen, k)
			}
		}
		c.lastSweep = now
	}
	if _, dup := c.seen[id]; dup {
		return ErrReplay
	}
	if len(c.seen) >= maxSeen {
		return fmt.Errorf("%w: too many proofs in the window", ErrUnavailable)
	}
	c.seen[id] = now
	return nil
}

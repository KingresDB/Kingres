// SPDX-License-Identifier: AGPL-3.0-only

package ledgerkeys

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Limits of signing.
const (
	// MaxMessageSize is the largest message a Signer is asked to sign: the
	// largest signed note (64 KiB, the limit of the verifier).
	MaxMessageSize = 64 << 10
	// MaxKeyNameSize is the longest key name: the longest origin.
	MaxKeyNameSize = 255
	// SigTypeEd25519 is the signature type byte of signed-note.
	SigTypeEd25519 byte = 0x01
)

// Signer signs messages with an Ed25519 key. The file signer of this package
// is one implementation; an external signer (a program, a KMS) can be added
// behind the same interface. Sign returns the 64 byte signature of msg, and an
// error rather than a wrong signature. Implementations are safe for concurrent
// use.
type Signer interface {
	Public() ed25519.PublicKey
	Sign(ctx context.Context, msg []byte) ([]byte, error)
}

// ValidKeyName reports whether s is a legal signed-note key name: 1 to
// MaxKeyNameSize bytes of UTF-8, no Unicode space, no plus sign, no control
// character. The log origins of the design satisfy it.
func ValidKeyName(s string) bool {
	if s == "" || len(s) > MaxKeyNameSize || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsSpace(r) || r == '+' || r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// KeyID is the 32 bit key id of a signed-note signature line for an Ed25519
// key under a key name: SHA-256(name || 0x0A || 0x01 || public key)[:4], big
// endian (design 3.2.1). The name of a log's signature is its origin.
func KeyID(name string, pub ed25519.PublicKey) uint32 {
	h := sha256.New()
	h.Write([]byte(name))
	h.Write([]byte{0x0A, SigTypeEd25519})
	h.Write(pub)
	return binary.BigEndian.Uint32(h.Sum(nil)[:4])
}

// Fingerprint is the lower case hex SHA-256 of the 32 byte public key: the
// value of the key file name and of the trust file.
func Fingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])
}

// Errors of signing.
var (
	// ErrBadSignature is returned by VerifyingSigner when the wrapped signer
	// returned a signature that does not verify.
	ErrBadSignature = errors.New("ledgerkeys: the signer returned a signature that does not verify")
	// ErrMessageTooLarge is returned for a message over MaxMessageSize.
	ErrMessageTooLarge = errors.New("ledgerkeys: the message is too large to sign")
)

// FileSigner is the Signer of a key held in memory, loaded from a key file or
// generated. It exposes Sign and Public only; nothing prints, encodes or
// returns the seed. Copies of a FileSigner share the key; Close zeroes it for
// all of them.
type FileSigner struct{ k *fileKey }

type fileKey struct {
	priv *secretBytes // ed25519 private key, 64 bytes (seed || public)
	pub  ed25519.PublicKey
}

// GenerateKey creates a key from r (crypto/rand when r is nil).
func GenerateKey(r io.Reader) (*FileSigner, error) {
	if r == nil {
		r = rand.Reader
	}
	seed := make([]byte, ed25519.SeedSize)
	defer clear(seed)
	if _, err := io.ReadFull(r, seed); err != nil {
		return nil, errors.New("ledgerkeys: cannot read random bytes for a key")
	}
	return NewFileSigner(seed)
}

// NewFileSigner builds a signer from a 32 byte seed. The seed is copied; the
// caller should zero its own copy.
func NewFileSigner(seed []byte) (*FileSigner, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("ledgerkeys: a seed has %d bytes, expected %d", len(seed), ed25519.SeedSize)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := append(ed25519.PublicKey(nil), priv.Public().(ed25519.PublicKey)...)
	s := &FileSigner{k: &fileKey{priv: newSecretBytes(priv), pub: pub}}
	clear(priv)
	return s, nil
}

// Public returns a copy of the public key.
func (s FileSigner) Public() ed25519.PublicKey {
	if s.k == nil {
		return nil
	}
	return append(ed25519.PublicKey(nil), s.k.pub...)
}

// Fingerprint is the fingerprint of the public key.
func (s FileSigner) Fingerprint() string { return Fingerprint(s.Public()) }

// Sign signs msg (at most MaxMessageSize bytes) and checks the signature with
// the public key before it returns it, so a fault cannot produce a bad one.
func (s FileSigner) Sign(ctx context.Context, msg []byte) ([]byte, error) {
	if s.k == nil {
		return nil, ErrDestroyed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(msg) > MaxMessageSize {
		return nil, fmt.Errorf("%w: %d bytes, expected at most %d", ErrMessageTooLarge, len(msg), MaxMessageSize)
	}
	var sig []byte
	err := s.k.priv.use(func(b []byte) error {
		sig = ed25519.Sign(ed25519.PrivateKey(b), msg)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !ed25519.Verify(s.k.pub, msg, sig) {
		return nil, ErrBadSignature
	}
	return sig, nil
}

// Close zeroes the private key. Sign returns ErrDestroyed afterwards.
func (s FileSigner) Close() error {
	if s.k != nil {
		s.k.priv.destroy()
	}
	return nil
}

// seedBytes copies the seed out for sealing into a key file. The caller must
// zero the result.
func (s FileSigner) seedBytes() ([]byte, error) {
	if s.k == nil {
		return nil, ErrDestroyed
	}
	var seed []byte
	err := s.k.priv.use(func(b []byte) error {
		seed = append([]byte(nil), b[:ed25519.SeedSize]...)
		return nil
	})
	return seed, err
}

// SameKey reports whether two signers hold the same key, comparing the
// private keys in constant time.
func (s FileSigner) SameKey(o FileSigner) bool {
	if s.k == nil || o.k == nil {
		return false
	}
	ok := false
	_ = o.k.priv.use(func(ob []byte) error {
		ok = s.k.priv.equal(ob)
		return nil
	})
	return ok
}

func (s FileSigner) label() string {
	if s.k == nil {
		return "FileSigner(destroyed)"
	}
	return "FileSigner(" + Fingerprint(s.k.pub)[:16] + ", " + redacted + ")"
}

// Format implements fmt.Formatter: every verb prints the fingerprint prefix
// and a marker, never the key.
func (s FileSigner) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(s.label())) }

// String implements fmt.Stringer.
func (s FileSigner) String() string { return s.label() }

// GoString implements fmt.GoStringer.
func (s FileSigner) GoString() string { return "ledgerkeys." + s.label() }

// MarshalJSON implements json.Marshaler.
func (s FileSigner) MarshalJSON() ([]byte, error) { return []byte(`"` + s.label() + `"`), nil }

// MarshalText implements encoding.TextMarshaler.
func (s FileSigner) MarshalText() ([]byte, error) { return []byte(s.label()), nil }

// LogValue implements slog.LogValuer.
func (s FileSigner) LogValue() slog.Value { return slog.StringValue(s.label()) }

// VerifyingSigner wraps a Signer so that every signature it returns is
// verified with its public key before it is used (design 3.2.3: a broken or
// hostile external signer must not be able to make the cluster publish a bad
// checkpoint). The wrapped signer's public key is read once.
func VerifyingSigner(inner Signer) (Signer, error) {
	pub := inner.Public()
	if len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("ledgerkeys: the signer's public key has %d bytes, expected %d", len(pub), ed25519.PublicKeySize)
	}
	return &verifyingSigner{inner: inner, pub: append(ed25519.PublicKey(nil), pub...)}, nil
}

type verifyingSigner struct {
	inner Signer
	pub   ed25519.PublicKey
}

func (v *verifyingSigner) Public() ed25519.PublicKey { return append(ed25519.PublicKey(nil), v.pub...) }

func (v *verifyingSigner) Sign(ctx context.Context, msg []byte) ([]byte, error) {
	if len(msg) > MaxMessageSize {
		return nil, fmt.Errorf("%w: %d bytes, expected at most %d", ErrMessageTooLarge, len(msg), MaxMessageSize)
	}
	sig, err := v.inner.Sign(ctx, msg)
	if err != nil {
		return nil, err
	}
	if len(sig) != ed25519.SignatureSize || !ed25519.Verify(v.pub, msg, sig) {
		return nil, ErrBadSignature
	}
	return sig, nil
}

// SignatureLineName reports the key name and id a signature of s carries on
// the signature line of a note about the log with the given origin.
func SignatureLineName(s Signer, origin string) (name string, id uint32, err error) {
	if !ValidKeyName(origin) {
		return "", 0, fmt.Errorf("ledgerkeys: %q is not a valid key name", clip(origin))
	}
	return origin, KeyID(origin, s.Public()), nil
}

func clip(s string) string {
	if len(s) > 60 {
		return strings.ToValidUTF8(s[:60], "?") + "..."
	}
	return strings.ToValidUTF8(s, "?")
}

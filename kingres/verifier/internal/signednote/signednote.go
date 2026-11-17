// SPDX-License-Identifier: AGPL-3.0-only

// Package signednote parses and verifies signed notes as C2SP "signed-note"
// defines them (https://c2sp.org/signed-note), for Ed25519 signatures (type
// 0x01) only. The design (ledger-phase7.md section 3) needs nothing else, and
// a verifier that knows one algorithm cannot be talked into another.
//
// The text of a note is untrusted until a signature of a key the caller
// pinned has been verified, so Parse returns the text but callers must only
// use it after Verify succeeded.
package signednote

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// SigTypeEd25519 is the signature type byte of Ed25519 signatures
// (signed-note, "Signature types").
const SigTypeEd25519 byte = 0x01

// Limits of a note. The specification says verifiers SHOULD cap the number of
// signatures but MUST accept at least 16. The other caps keep a hostile note,
// even one signed by a key of the trust file, from using memory and from
// growing the state file that stores it (the verifier never writes a state it
// would not read back, so what a note may hold is bounded here, at the one
// place every note passes through): a checkpoint is an origin of at most 255
// bytes, a size, a root and a few short extension lines, and a signature line
// is at most about 350 bytes, so 32 signatures and 32 lines of text fit in
// well under 64 KiB.
const (
	MaxSignatures = 32
	MaxNoteSize   = 64 << 10
	// MaxLineLength is the longest line of a note, text or signature, without
	// its newline.
	MaxLineLength = 1024
	// MaxTextLines is the most lines the text of a note may have.
	MaxTextLines = 32
)

// emDashPrefix starts every signature line: U+2014 then a space.
const emDashPrefix = "\u2014 "

// Sentinel errors, so callers can tell a note that is bad from a note that is
// merely not signed by anyone they trust.
var (
	ErrBadSignature        = errors.New("a signature of a trusted key does not verify")
	ErrNoTrustedSignature  = errors.New("no signature of a trusted key")
	ErrAmbiguousTrustedKey = errors.New("two trusted keys have the same name and key id")
)

// Signature is one signature line of a note.
type Signature struct {
	Name string
	ID   uint32
	Sig  []byte
}

// Note is a parsed, not yet verified, signed note. Text includes its final
// newline but not the blank separator line.
type Note struct {
	Text       []byte
	Signatures []Signature
}

// ValidName reports whether s is a legal key name: non-empty, no Unicode
// space and no plus sign (signed-note, "Format"). It also refuses invalid
// UTF-8 and control characters, which a note cannot carry anyway.
func ValidName(s string) bool {
	if s == "" || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsSpace(r) || r == '+' || r < 0x20 {
			return false
		}
	}
	return true
}

// KeyID computes the 32-bit key id for an Ed25519 key as signed-note
// recommends: SHA-256(name || 0x0A || 0x01 || public key)[:4], big endian.
func KeyID(name string, pub ed25519.PublicKey) uint32 {
	h := sha256.New()
	h.Write([]byte(name))
	h.Write([]byte{0x0A, SigTypeEd25519})
	h.Write(pub)
	return binary.BigEndian.Uint32(h.Sum(nil)[:4])
}

// Key is a trusted Ed25519 verification key under a note key name.
type Key struct {
	Name   string
	Public ed25519.PublicKey
}

// NewKey validates and builds a Key.
func NewKey(name string, pub []byte) (Key, error) {
	if !ValidName(name) {
		return Key{}, fmt.Errorf("key name %q is not valid, expected a non-empty name without spaces, plus signs or control characters", name)
	}
	if len(pub) != ed25519.PublicKeySize {
		return Key{}, fmt.Errorf("public key has %d bytes, expected %d", len(pub), ed25519.PublicKeySize)
	}
	return Key{Name: name, Public: append(ed25519.PublicKey(nil), pub...)}, nil
}

// ID returns the key id of the key.
func (k Key) ID() uint32 { return KeyID(k.Name, k.Public) }

// ParseVKey parses a verifier key string
// "<name>+<hex key id>+<base64(type || public key)>" (signed-note,
// "Verifier keys"). The key id must match the one computed from the name and
// the key: the specification says they MUST.
func ParseVKey(s string) (Key, error) {
	parts := strings.Split(s, "+")
	if len(parts) != 3 {
		return Key{}, errors.New("verifier key must have three parts separated by plus signs")
	}
	idb, err := hex.DecodeString(parts[1])
	if err != nil || len(idb) != 4 || hex.EncodeToString(idb) != parts[1] {
		return Key{}, errors.New("verifier key id must be 8 lower case hex characters")
	}
	raw, err := DecodeBase64(parts[2])
	if err != nil {
		return Key{}, fmt.Errorf("verifier key material: %w", err)
	}
	if len(raw) < 1 || raw[0] != SigTypeEd25519 {
		return Key{}, errors.New("verifier key is not of signature type 0x01 (Ed25519)")
	}
	k, err := NewKey(parts[0], raw[1:])
	if err != nil {
		return Key{}, err
	}
	if k.ID() != binary.BigEndian.Uint32(idb) {
		return Key{}, errors.New("verifier key id does not match its name and key")
	}
	return k, nil
}

// DecodeBase64 decodes standard base64 with padding and rejects every
// non-canonical form (RFC 4648 section 3.5, which signed-note and
// tlog-checkpoint require): Go's decoder alone skips newlines and accepts
// non-zero trailing bits, so the result is re-encoded and compared.
func DecodeBase64(s string) ([]byte, error) {
	b, err := base64.StdEncoding.Strict().DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("invalid base64: %w", err)
	}
	if base64.StdEncoding.EncodeToString(b) != s {
		return nil, errors.New("base64 is not canonical")
	}
	return b, nil
}

// Parse parses the wire form of a signed note and rejects everything the
// specification forbids: invalid UTF-8, control characters other than
// newline, a missing final newline, a missing blank separator line, no
// signature, a signature line that is not "em dash, space, name, space,
// base64", an invalid name, non-canonical base64, a signature shorter than
// the four byte key id, and (checkpoint specification) two lines with the
// same name and key id. More than MaxSignatures lines are refused, and so are a note of more than
// MaxNoteSize bytes, a line of more than MaxLineLength bytes and a text of more
// than MaxTextLines lines (see the constants for why they exist).
func Parse(data []byte) (*Note, error) {
	if len(data) > MaxNoteSize {
		return nil, fmt.Errorf("note has %d bytes, expected at most %d", len(data), MaxNoteSize)
	}
	if !utf8.Valid(data) {
		return nil, errors.New("note is not valid UTF-8")
	}
	for i, b := range data {
		if b < 0x20 && b != '\n' {
			return nil, fmt.Errorf("note has the control character 0x%02x at byte %d, expected none except newline", b, i)
		}
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		return nil, errors.New("note does not end with a newline")
	}
	i := bytes.LastIndex(data, []byte("\n\n"))
	if i < 0 {
		return nil, errors.New("note has no blank line between the text and the signatures")
	}
	text, sigPart := data[:i+1], data[i+2:]
	if err := checkLines(data, text); err != nil {
		return nil, err
	}
	if len(sigPart) == 0 {
		return nil, errors.New("note has no signature line")
	}
	n := &Note{Text: text}
	seen := map[[2]string]bool{}
	for _, line := range strings.Split(strings.TrimSuffix(string(sigPart), "\n"), "\n") {
		if len(n.Signatures) == MaxSignatures {
			return nil, fmt.Errorf("note has more than %d signatures", MaxSignatures)
		}
		sig, err := parseSigLine(line)
		if err != nil {
			return nil, err
		}
		id := [2]string{sig.Name, fmt.Sprint(sig.ID)}
		if seen[id] {
			return nil, fmt.Errorf("two signature lines have the same key name %q and key id %08x", sig.Name, sig.ID)
		}
		seen[id] = true
		n.Signatures = append(n.Signatures, sig)
	}
	return n, nil
}

func parseSigLine(line string) (Signature, error) {
	rest, ok := strings.CutPrefix(line, emDashPrefix)
	if !ok {
		return Signature{}, fmt.Errorf("signature line %q does not start with an em dash and a space", clip(line))
	}
	name, b64, ok := strings.Cut(rest, " ")
	if !ok || strings.Contains(b64, " ") {
		return Signature{}, fmt.Errorf("signature line %q does not have the form em dash, name, space, base64", clip(line))
	}
	if !ValidName(name) {
		return Signature{}, fmt.Errorf("signature line has the invalid key name %q", clip(name))
	}
	raw, err := DecodeBase64(b64)
	if err != nil {
		return Signature{}, fmt.Errorf("signature of %q: %w", name, err)
	}
	if len(raw) < 4 {
		return Signature{}, fmt.Errorf("signature of %q has %d bytes, expected at least 4 for the key id", name, len(raw))
	}
	return Signature{Name: name, ID: binary.BigEndian.Uint32(raw[:4]), Sig: raw[4:]}, nil
}

func clip(s string) string {
	if len(s) > 60 {
		return strings.ToValidUTF8(s[:60], "?") + "..."
	}
	return s
}

// Verify checks the note against the trusted keys. A signature whose name
// and key id match no trusted key is ignored (the specification: "MUST ignore
// signatures from unknown keys, even if they share a name or ID (but not
// both)"). A signature that matches a trusted key must verify, otherwise the
// whole note is rejected (the specification only says SHOULD; a verifier gains
// nothing by accepting a note that carries a forged line). With no verified
// signature the note is rejected too. It returns the keys whose signatures
// verified.
func Verify(n *Note, keys []Key) ([]Key, error) {
	index := map[[2]string]Key{}
	for _, k := range keys {
		id := [2]string{k.Name, fmt.Sprint(k.ID())}
		if prev, dup := index[id]; dup && !bytes.Equal(prev.Public, k.Public) {
			return nil, fmt.Errorf("%w: %q %08x", ErrAmbiguousTrustedKey, k.Name, k.ID())
		}
		index[id] = k
	}
	var ok []Key
	for _, s := range n.Signatures {
		k, known := index[[2]string{s.Name, fmt.Sprint(s.ID)}]
		if !known {
			continue
		}
		if len(s.Sig) != ed25519.SignatureSize || !ed25519.Verify(k.Public, n.Text, s.Sig) {
			return nil, fmt.Errorf("%w: key %q id %08x", ErrBadSignature, k.Name, s.ID)
		}
		ok = append(ok, k)
	}
	if len(ok) == 0 {
		return nil, ErrNoTrustedSignature
	}
	return ok, nil
}

// checkLines enforces MaxLineLength on every line of the note and MaxTextLines
// on the text. The checks run before anything is converted or stored.
func checkLines(data, text []byte) error {
	if n := bytes.Count(text, []byte("\n")); n > MaxTextLines {
		return fmt.Errorf("note text has %d lines, expected at most %d", n, MaxTextLines)
	}
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			i = len(data)
		}
		if i > MaxLineLength {
			return fmt.Errorf("note has a line of %d bytes, expected at most %d", i, MaxLineLength)
		}
		if i == len(data) {
			break
		}
		data = data[i+1:]
	}
	return nil
}

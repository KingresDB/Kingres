// SPDX-License-Identifier: AGPL-3.0-only

package ledgerlog

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerkeys"
)

// Limits of a signed note (design 14: the bounds the verifier applies). A note
// that exceeds one is never written and never parsed.
const (
	// MaxNoteSize is the most bytes a note has.
	MaxNoteSize = 64 << 10
	// MaxSignatures is the most signature lines.
	MaxSignatures = 32
	// MaxLineLength is the longest line, text or signature, without its newline.
	MaxLineLength = 1024
	// MaxTextLines is the most lines of the text.
	MaxTextLines = 32
)

// emDashPrefix starts every signature line: U+2014 and a space.
const emDashPrefix = "\u2014 "

// ErrNote marks a signed note that is not valid.
var ErrNote = errors.New("ledgerlog: the signed note is not valid")

func noteErr(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrNote}, args...)...)
}

// NoteSignature is one signature line of a note: the key name, the key id and
// the 64 byte Ed25519 signature.
type NoteSignature struct {
	Name string
	ID   uint32
	Sig  []byte
}

// Note is a signed note (C2SP signed-note): a text that ends in a newline and
// signature lines. Text does not include the blank separator line.
type Note struct {
	Text       []byte
	Signatures []NoteSignature
}

// checkText checks a note text as the verifier does when it parses a note: UTF-8
// without a control character other than newline, ends in a newline, no blank
// line (the separator is the last blank line), at most MaxTextLines lines of
// MaxLineLength bytes.
func checkText(text []byte) error {
	if len(text) == 0 || text[len(text)-1] != '\n' {
		return noteErr("the text does not end with a newline")
	}
	if !utf8.Valid(text) {
		return noteErr("the text is not valid UTF-8")
	}
	for i, b := range text {
		if b < 0x20 && b != '\n' {
			return noteErr("the text has the control character 0x%02x at byte %d", b, i)
		}
	}
	if bytes.Contains(text, []byte("\n\n")) {
		return noteErr("the text has a blank line")
	}
	if n := bytes.Count(text, []byte("\n")); n > MaxTextLines {
		return noteErr("the text has %d lines, expected at most %d", n, MaxTextLines)
	}
	for _, line := range bytes.Split(text[:len(text)-1], []byte("\n")) {
		if len(line) > MaxLineLength {
			return noteErr("the text has a line of %d bytes, expected at most %d", len(line), MaxLineLength)
		}
	}
	return nil
}

func sigLine(s NoteSignature) string {
	raw := binary.BigEndian.AppendUint32(nil, s.ID)
	raw = append(raw, s.Sig...)
	return emDashPrefix + s.Name + " " + base64.StdEncoding.EncodeToString(raw) + "\n"
}

// Marshal writes the note: the text, a blank line, the signature lines. It
// refuses a note that Parse would refuse.
func (n *Note) Marshal() ([]byte, error) {
	if err := checkText(n.Text); err != nil {
		return nil, err
	}
	if len(n.Signatures) == 0 {
		return nil, noteErr("no signature")
	}
	if len(n.Signatures) > MaxSignatures {
		return nil, noteErr("%d signatures, expected at most %d", len(n.Signatures), MaxSignatures)
	}
	var b bytes.Buffer
	b.Write(n.Text)
	b.WriteByte('\n')
	seen := map[string]bool{}
	for _, s := range n.Signatures {
		if !ledgerkeys.ValidKeyName(s.Name) {
			return nil, noteErr("the key name %q is not valid", clipStr(s.Name))
		}
		if len(s.Sig) != ed25519.SignatureSize {
			return nil, noteErr("a signature has %d bytes, expected %d", len(s.Sig), ed25519.SignatureSize)
		}
		id := fmt.Sprintf("%d %s", s.ID, s.Name)
		if seen[id] {
			return nil, noteErr("two signature lines have the same key name and key id")
		}
		seen[id] = true
		line := sigLine(s)
		if len(line)-1 > MaxLineLength {
			return nil, noteErr("a signature line has %d bytes, expected at most %d", len(line)-1, MaxLineLength)
		}
		b.WriteString(line)
	}
	if b.Len() > MaxNoteSize {
		return nil, noteErr("%d bytes, expected at most %d", b.Len(), MaxNoteSize)
	}
	return b.Bytes(), nil
}

// ParseNote parses a signed note strictly and refuses everything the
// specification and the verifier refuse: invalid UTF-8, control characters
// other than newline, no final newline, no blank separator, no signature, a
// line over MaxLineLength, more than MaxTextLines text lines or MaxSignatures
// signatures, more than MaxNoteSize bytes, a signature line that is not the
// em dash, a space, the name, a space and canonical base64, an invalid name, a
// signature that is not 4 + 64 bytes, and two lines with the same name and key
// id. Marshal of the result gives the input back.
func ParseNote(data []byte) (*Note, error) {
	if len(data) > MaxNoteSize {
		return nil, noteErr("%d bytes, expected at most %d", len(data), MaxNoteSize)
	}
	if !utf8.Valid(data) {
		return nil, noteErr("not valid UTF-8")
	}
	for i, b := range data {
		if b < 0x20 && b != '\n' {
			return nil, noteErr("the control character 0x%02x at byte %d", b, i)
		}
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		return nil, noteErr("it does not end with a newline")
	}
	i := bytes.LastIndex(data, []byte("\n\n"))
	if i < 0 {
		return nil, noteErr("no blank line between the text and the signatures")
	}
	text, sigPart := data[:i+1], data[i+2:]
	if err := checkText(text); err != nil {
		return nil, err
	}
	if len(sigPart) == 0 {
		return nil, noteErr("no signature line")
	}
	n := &Note{Text: append([]byte(nil), text...)}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSuffix(string(sigPart), "\n"), "\n") {
		if len(n.Signatures) == MaxSignatures {
			return nil, noteErr("more than %d signatures", MaxSignatures)
		}
		if len(line) > MaxLineLength {
			return nil, noteErr("a signature line has %d bytes, expected at most %d", len(line), MaxLineLength)
		}
		s, err := parseSigLine(line)
		if err != nil {
			return nil, err
		}
		id := fmt.Sprintf("%d %s", s.ID, s.Name)
		if seen[id] {
			return nil, noteErr("two signature lines have the same key name and key id")
		}
		seen[id] = true
		n.Signatures = append(n.Signatures, s)
	}
	return n, nil
}

func parseSigLine(line string) (NoteSignature, error) {
	rest, ok := strings.CutPrefix(line, emDashPrefix)
	if !ok {
		return NoteSignature{}, noteErr("a signature line does not start with an em dash and a space")
	}
	name, b64, ok := strings.Cut(rest, " ")
	if !ok || strings.Contains(b64, " ") {
		return NoteSignature{}, noteErr("a signature line does not have the form em dash, name, space, base64")
	}
	if !ledgerkeys.ValidKeyName(name) {
		return NoteSignature{}, noteErr("a signature line has the invalid key name %q", clipStr(name))
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(b64)
	if err != nil || base64.StdEncoding.EncodeToString(raw) != b64 {
		return NoteSignature{}, noteErr("the signature of %q is not canonical base64", clipStr(name))
	}
	if len(raw) != 4+ed25519.SignatureSize {
		return NoteSignature{}, noteErr("the signature of %q has %d bytes, expected %d", clipStr(name), len(raw), 4+ed25519.SignatureSize)
	}
	return NoteSignature{Name: name, ID: binary.BigEndian.Uint32(raw[:4]), Sig: raw[4:]}, nil
}

// SignNote signs text with each signer under the key name name and returns the
// note. The signature lines are in the order of the signers; the same key
// twice is refused, and so are two different keys with the same key id under
// the name (ledgerkeys.ErrKeyIDCollision), because a note holds one line for a
// name and key id. For a checkpoint the name is the origin (design 14,
// correction 4).
func SignNote(ctx context.Context, text []byte, name string, signers ...ledgerkeys.Signer) (*Note, error) {
	if err := checkText(text); err != nil {
		return nil, err
	}
	if len(signers) == 0 {
		return nil, noteErr("no signer")
	}
	n := &Note{Text: append([]byte(nil), text...)}
	var pubs []ed25519.PublicKey
	for _, s := range signers {
		if err := n.addSignature(ctx, name, s, pubs); err != nil {
			return nil, err
		}
		pubs = append(pubs, s.Public())
	}
	if _, err := n.Marshal(); err != nil {
		return nil, err
	}
	return n, nil
}

// AddSignature adds the signature of s under the key name to the note (a
// second key during a rotation, or the re-signing of a checkpoint). A key
// that has signed under this name already is refused; so is a key whose key id
// is that of a line of another key (ledgerkeys.ErrKeyIDCollision, which the
// verifier would refuse as two lines of one name and id), and so is a note
// that has MaxSignatures lines. A line with the same name and id is taken for
// the same key only when its signature verifies under the public key of s.
func (n *Note) AddSignature(ctx context.Context, name string, s ledgerkeys.Signer) error {
	return n.addSignature(ctx, name, s, nil)
}

// addSignature is AddSignature; known are public keys that may own the lines of
// the note, so that a collision can name both keys.
func (n *Note) addSignature(ctx context.Context, name string, s ledgerkeys.Signer, known []ed25519.PublicKey) error {
	if !ledgerkeys.ValidKeyName(name) {
		return noteErr("the key name %q is not valid", clipStr(name))
	}
	if len(n.Signatures) >= MaxSignatures {
		return noteErr("the note has %d signatures already", len(n.Signatures))
	}
	pub := s.Public()
	if len(pub) != ed25519.PublicKeySize {
		return noteErr("the signer's public key has %d bytes", len(pub))
	}
	id := ledgerkeys.KeyID(name, pub)
	for _, o := range n.Signatures {
		if o.Name != name || o.ID != id {
			continue
		}
		if len(o.Sig) == ed25519.SignatureSize && ed25519.Verify(pub, n.Text, o.Sig) {
			return noteErr("the note is already signed by this key")
		}
		// The line has this name and key id but does not verify under this
		// key: another key with the same id (or a damaged line, which the
		// verifier refuses as well).
		c := &ledgerkeys.KeyIDCollision{Name: name, ID: id, A: ledgerkeys.Fingerprint(pub)}
		for _, p := range known {
			if len(p) == ed25519.PublicKeySize && !bytes.Equal(p, pub) && ledgerkeys.KeyID(name, p) == id {
				c.B = ledgerkeys.Fingerprint(p)
			}
		}
		return c
	}
	sig, err := s.Sign(ctx, n.Text)
	if err != nil {
		return err
	}
	if len(sig) != ed25519.SignatureSize || !ed25519.Verify(pub, n.Text, sig) {
		return ledgerkeys.ErrBadSignature
	}
	n.Signatures = append(n.Signatures, NoteSignature{Name: name, ID: id, Sig: sig})
	return nil
}

// NoteKey is a public key trusted under a key name.
type NoteKey struct {
	Name   string
	Public ed25519.PublicKey
}

// Errors of VerifyNote.
var (
	ErrBadSignature       = errors.New("ledgerlog: a signature of a trusted key does not verify")
	ErrNoTrustedSignature = errors.New("ledgerlog: no signature of a trusted key")
)

// VerifyNote checks the note against the keys as the verifier does: a line
// whose name and key id match no key is ignored, a line that matches a key
// must verify (else the note is refused), and at least one line must verify.
// Two different keys with the same name and key id among keys are refused with
// ledgerkeys.ErrKeyIDCollision, as the verifier refuses them (it cannot tell
// which of the two a line belongs to). It returns the keys whose signatures
// verified.
func VerifyNote(n *Note, keys []NoteKey) ([]NoteKey, error) {
	type nameID struct {
		name string
		id   uint32
	}
	seen := map[nameID]ed25519.PublicKey{}
	for _, k := range keys {
		if len(k.Public) != ed25519.PublicKeySize {
			continue
		}
		key := nameID{k.Name, ledgerkeys.KeyID(k.Name, k.Public)}
		if q, dup := seen[key]; dup && !bytes.Equal(q, k.Public) {
			return nil, &ledgerkeys.KeyIDCollision{Name: k.Name, ID: key.id, A: ledgerkeys.Fingerprint(q), B: ledgerkeys.Fingerprint(k.Public)}
		}
		seen[key] = k.Public
	}
	var ok []NoteKey
	for _, s := range n.Signatures {
		for _, k := range keys {
			if k.Name != s.Name || len(k.Public) != ed25519.PublicKeySize || ledgerkeys.KeyID(k.Name, k.Public) != s.ID {
				continue
			}
			if len(s.Sig) != ed25519.SignatureSize || !ed25519.Verify(k.Public, n.Text, s.Sig) {
				return nil, ErrBadSignature
			}
			ok = append(ok, k)
			break
		}
	}
	if len(ok) == 0 {
		return nil, ErrNoTrustedSignature
	}
	return ok, nil
}

// SPDX-License-Identifier: AGPL-3.0-only

// Package release holds what the release of the verifier is made of and what
// "kingres-verify release verify" checks: the sums text (the format of
// sha256sum), the signed note over it, and the release key.
//
// The sums note is a signed note (package signednote) whose text is exactly the
// bytes of the SHA256SUMS file, signed under the key name KeyName. The signing
// side (Sign) is used by the release tool and by tests only; the verifier
// binary never signs.
package release

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

	"github.com/KingresDB/Kingres/kingres/verifier/internal/signednote"
)

const (
	// KeyName is the note key name the release key signs under.
	KeyName = "kingres-verify-release"
	// SumsName and NoteName are the names of the two files of a release.
	SumsName = "SHA256SUMS"
	NoteName = "SHA256SUMS.note"
	// MaxSums is the largest sums file accepted (it must fit in a signed note).
	MaxSums = signednote.MaxNoteSize
)

// Entry is one line of the sums file.
type Entry struct {
	Hash [sha256.Size]byte
	Name string
}

// validFileName reports whether name may be a file of a release: letters,
// digits, dot, dash and underscore only, not starting with a dash or a dot, so
// that it is never a path, an option or a hidden file.
func validFileName(name string) bool {
	if name == "" || len(name) > 255 || name[0] == '-' || name[0] == '.' {
		return false
	}
	for _, c := range []byte(name) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// FormatSums makes the sums text from entries, in the form "sha256sum" writes
// and "sha256sum -c" reads: 64 lower case hex characters, two spaces, the name.
// The lines are in the order given.
func FormatSums(entries []Entry) ([]byte, error) {
	var b bytes.Buffer
	for _, e := range entries {
		if !validFileName(e.Name) {
			return nil, fmt.Errorf("file name %q is not allowed in the sums", e.Name)
		}
		fmt.Fprintf(&b, "%s  %s\n", hex.EncodeToString(e.Hash[:]), e.Name)
	}
	if b.Len() == 0 {
		return nil, errors.New("no file to sum")
	}
	return b.Bytes(), nil
}

// ParseSums reads the sums text strictly: at least one line, every line
// "<64 lower case hex>  <name>" and a newline, a name that is a plain file name
// (validFileName), no name twice, no blank line.
func ParseSums(text []byte) ([]Entry, error) {
	if len(text) == 0 {
		return nil, errors.New("the sums are empty")
	}
	if len(text) > MaxSums {
		return nil, fmt.Errorf("the sums have %d bytes, expected at most %d", len(text), MaxSums)
	}
	if text[len(text)-1] != '\n' {
		return nil, errors.New("the sums do not end with a newline")
	}
	var out []Entry
	seen := map[string]bool{}
	for i, line := range strings.Split(string(text[:len(text)-1]), "\n") {
		n := i + 1
		if len(line) < 67 || line[64:66] != "  " {
			return nil, fmt.Errorf("line %d is not \"<64 hex characters>, two spaces, a file name\"", n)
		}
		raw, err := hex.DecodeString(line[:64])
		if err != nil || hex.EncodeToString(raw) != line[:64] {
			return nil, fmt.Errorf("line %d: the hash is not 64 lower case hex characters", n)
		}
		name := line[66:]
		if !validFileName(name) {
			return nil, fmt.Errorf("line %d: %q is not a plain file name (letters, digits, dot, dash, underscore)", n, name)
		}
		if seen[name] {
			return nil, fmt.Errorf("line %d: %q is listed twice", n, name)
		}
		seen[name] = true
		var e Entry
		copy(e.Hash[:], raw)
		e.Name = name
		out = append(out, e)
	}
	return out, nil
}

// ParseKey reads the release key as the user passes it: the base64 of the 32
// byte Ed25519 public key, or the full verifier key string
// "kingres-verify-release+<id>+<base64>" of the signed-note specification.
// Either way the key belongs to KeyName.
func ParseKey(s string) (signednote.Key, error) {
	s = strings.TrimSpace(s)
	// 44 characters is the base64 of 32 bytes, which may hold plus signs; a key string is longer.
	if len(s) != base64.StdEncoding.EncodedLen(ed25519.PublicKeySize) && strings.Contains(s, "+") {
		k, err := signednote.ParseVKey(s)
		if err != nil {
			return signednote.Key{}, err
		}
		if k.Name != KeyName {
			return signednote.Key{}, fmt.Errorf("the key is named %q, the release key is named %q", k.Name, KeyName)
		}
		return k, nil
	}
	raw, err := signednote.DecodeBase64(s)
	if err != nil {
		return signednote.Key{}, fmt.Errorf("release key: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return signednote.Key{}, fmt.Errorf("release key has %d bytes, expected %d (the base64 of an Ed25519 public key)", len(raw), ed25519.PublicKeySize)
	}
	return signednote.NewKey(KeyName, raw)
}

// VKey returns the verifier key string of pub under KeyName.
func VKey(pub ed25519.PublicKey) string {
	id := signednote.KeyID(KeyName, pub)
	return fmt.Sprintf("%s+%08x+%s", KeyName, id, base64.StdEncoding.EncodeToString(append([]byte{signednote.SigTypeEd25519}, pub...)))
}

// Sign makes the signed note of text under KeyName: the text, a blank line and
// one signature line. text must be what ParseSums accepts.
func Sign(priv ed25519.PrivateKey, text []byte) ([]byte, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, errors.New("the private key is not an Ed25519 key")
	}
	if _, err := ParseSums(text); err != nil {
		return nil, err
	}
	pub := priv.Public().(ed25519.PublicKey)
	sig := ed25519.Sign(priv, text)
	line := make([]byte, 4, 4+len(sig))
	binary.BigEndian.PutUint32(line, signednote.KeyID(KeyName, pub))
	line = append(line, sig...)
	var b bytes.Buffer
	b.Write(text)
	b.WriteString("\n— " + KeyName + " " + base64.StdEncoding.EncodeToString(line) + "\n")
	return b.Bytes(), nil
}

// Errors of VerifyNote that tell what is wrong with the note.
var (
	ErrNoteMalformed = errors.New("the sums note is not a well formed signed note")
	ErrNoteSignature = errors.New("the sums note is not signed by the release key you gave")
	ErrNoteText      = errors.New("the text of the sums note is not the sums file")
)

// VerifyNote checks that note is a signed note, that the release key signed it,
// and that its text is exactly the bytes of sums. The sums are believed only
// after this succeeded.
func VerifyNote(sums, note []byte, key signednote.Key) error {
	n, err := signednote.Parse(note)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNoteMalformed, err)
	}
	if _, err := signednote.Verify(n, []signednote.Key{key}); err != nil {
		return fmt.Errorf("%w: %v", ErrNoteSignature, err)
	}
	if !bytes.Equal(n.Text, sums) {
		return ErrNoteText
	}
	return nil
}

// Find returns the entry named name.
func Find(entries []Entry, name string) (Entry, bool) {
	for _, e := range entries {
		if e.Name == name {
			return e, true
		}
	}
	return Entry{}, false
}

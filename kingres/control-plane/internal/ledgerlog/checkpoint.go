// SPDX-License-Identifier: AGPL-3.0-only

package ledgerlog

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerkeys"
)

// Limits of a checkpoint.
const (
	// MaxOriginSize is the longest origin (tlog-cosignature: a name of at most
	// 255 bytes).
	MaxOriginSize = 255
	// MaxExtensionLines is the most extension lines (design 14, correction 2:
	// the verifier accepts 16, kingresd writes none).
	MaxExtensionLines = 16
)

// ErrCheckpoint marks a checkpoint text that is not valid.
var ErrCheckpoint = errors.New("ledgerlog: the checkpoint is not valid")

func cpErr(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrCheckpoint}, args...)...)
}

// Checkpoint is the text of a checkpoint (C2SP tlog-checkpoint): the origin,
// the tree size, the root, and optional extension lines.
type Checkpoint struct {
	Origin     string
	Size       uint64
	Root       Hash
	Extensions []string
}

// ValidOrigin reports whether s can be an origin: the rules of a key name, at
// most MaxOriginSize bytes (design 14, correction 3).
func ValidOrigin(s string) bool { return len(s) <= MaxOriginSize && ledgerkeys.ValidKeyName(s) }

// Text returns the three lines (and the extension lines), each ended by a
// newline. It refuses a checkpoint that ParseCheckpoint would refuse.
func (c Checkpoint) Text() ([]byte, error) {
	if !ValidOrigin(c.Origin) {
		return nil, cpErr("the origin %q is not valid, expected 1 to %d bytes without spaces, plus signs or control characters", clipStr(c.Origin), MaxOriginSize)
	}
	if c.Size >= MaxTreeSize {
		return nil, cpErr("the size %d is too large", c.Size)
	}
	if c.Size == 0 && c.Root != EmptyRoot() {
		return nil, cpErr("a tree of size 0 has the root of the empty tree")
	}
	if len(c.Extensions) > MaxExtensionLines {
		return nil, cpErr("%d extension lines, expected at most %d", len(c.Extensions), MaxExtensionLines)
	}
	var b strings.Builder
	b.WriteString(c.Origin + "\n" + strconv.FormatUint(c.Size, 10) + "\n" + base64.StdEncoding.EncodeToString(c.Root[:]) + "\n")
	for _, e := range c.Extensions {
		if e == "" {
			return nil, cpErr("an extension line is empty")
		}
		b.WriteString(e + "\n")
	}
	text := []byte(b.String())
	if err := checkText(text); err != nil {
		return nil, cpErr("%v", err)
	}
	return text, nil
}

// ParseCheckpoint parses the text of a checkpoint strictly: valid UTF-8, at
// least three non-empty lines, a valid origin, a size as a canonical decimal
// (no sign, no leading zero) below 2^62, a root of 32 bytes in canonical
// base64, the empty-tree root for size 0, and at most MaxExtensionLines
// non-empty extension lines. Text of the result gives the input back.
func ParseCheckpoint(text []byte) (Checkpoint, error) {
	if !utf8.Valid(text) {
		return Checkpoint{}, cpErr("not valid UTF-8")
	}
	if err := checkText(text); err != nil {
		return Checkpoint{}, cpErr("%v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(text), "\n"), "\n")
	if len(lines) < 3 {
		return Checkpoint{}, cpErr("%d lines, expected at least 3", len(lines))
	}
	for i, l := range lines {
		if l == "" {
			return Checkpoint{}, cpErr("line %d is empty", i+1)
		}
	}
	c := Checkpoint{Origin: lines[0]}
	if !ValidOrigin(c.Origin) {
		return Checkpoint{}, cpErr("the origin %q is not valid", clipStr(c.Origin))
	}
	s := lines[1]
	if len(s) > 19 || (len(s) > 1 && s[0] == '0') {
		return Checkpoint{}, cpErr("the size %q is not a canonical decimal", clipStr(s))
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return Checkpoint{}, cpErr("the size %q is not a canonical decimal", clipStr(s))
		}
	}
	size, err := strconv.ParseUint(s, 10, 64)
	if err != nil || size >= MaxTreeSize {
		return Checkpoint{}, cpErr("the size %q is out of range", clipStr(s))
	}
	c.Size = size
	raw, err := base64.StdEncoding.Strict().DecodeString(lines[2])
	if err != nil || len(raw) != len(c.Root) || base64.StdEncoding.EncodeToString(raw) != lines[2] {
		return Checkpoint{}, cpErr("the root is not canonical base64 of %d bytes", len(c.Root))
	}
	copy(c.Root[:], raw)
	if c.Size == 0 && c.Root != EmptyRoot() {
		return Checkpoint{}, cpErr("a tree of size 0 has the root of the empty tree")
	}
	if len(lines)-3 > MaxExtensionLines {
		return Checkpoint{}, cpErr("%d extension lines, expected at most %d", len(lines)-3, MaxExtensionLines)
	}
	if len(lines) > 3 {
		c.Extensions = lines[3:]
	}
	return c, nil
}

// SignCheckpoint returns the signed note of the checkpoint, signed by each
// signer under the key name c.Origin (one signature line per signer, in the
// order given: two during a key overlap). The note is what a publication
// serves as <log>/checkpoint.
func SignCheckpoint(ctx context.Context, c Checkpoint, signers ...ledgerkeys.Signer) ([]byte, error) {
	text, err := c.Text()
	if err != nil {
		return nil, err
	}
	n, err := SignNote(ctx, text, c.Origin, signers...)
	if err != nil {
		return nil, err
	}
	return n.Marshal()
}

// ResignCheckpoint adds the signatures of more signers to a signed checkpoint
// (a rotation: the latest checkpoint of a log that no longer grows gets the
// new key's signature). A signer that signed already is refused.
func ResignCheckpoint(ctx context.Context, note []byte, more ...ledgerkeys.Signer) ([]byte, error) {
	n, err := ParseNote(note)
	if err != nil {
		return nil, err
	}
	c, err := ParseCheckpoint(n.Text)
	if err != nil {
		return nil, err
	}
	for _, s := range more {
		if err := n.AddSignature(ctx, c.Origin, s); err != nil {
			return nil, err
		}
	}
	return n.Marshal()
}

// VerifyCheckpoint parses a signed checkpoint, verifies it against the public
// keys (all trusted under the key name of the origin), checks the origin is
// wantOrigin, and returns the checkpoint with the keys whose signatures
// verified. The text is parsed only after a signature verified.
func VerifyCheckpoint(note []byte, wantOrigin string, pubs ...ed25519.PublicKey) (Checkpoint, []ed25519.PublicKey, error) {
	n, err := ParseNote(note)
	if err != nil {
		return Checkpoint{}, nil, err
	}
	keys := make([]NoteKey, 0, len(pubs))
	for _, p := range pubs {
		keys = append(keys, NoteKey{Name: wantOrigin, Public: p})
	}
	ok, err := VerifyNote(n, keys)
	if err != nil {
		return Checkpoint{}, nil, err
	}
	c, err := ParseCheckpoint(n.Text)
	if err != nil {
		return Checkpoint{}, nil, err
	}
	if c.Origin != wantOrigin {
		return Checkpoint{}, nil, cpErr("the origin is %q, expected %q", clipStr(c.Origin), wantOrigin)
	}
	var signers []ed25519.PublicKey
	for _, k := range ok {
		signers = append(signers, k.Public)
	}
	return c, signers, nil
}

// Origins of the logs (design 4.2 with corrections 3 and 4).

// ChainOrigin returns "<prefix>/c/<chain id hex>/e<epoch>".
func ChainOrigin(prefix string, chainID [32]byte, epoch uint32) (string, error) {
	if err := ledgerkeys.ValidPrefix(prefix); err != nil {
		return "", fmt.Errorf("ledgerlog: origin prefix: %v", err)
	}
	if epoch == 0 {
		return "", errors.New("ledgerlog: an epoch starts at 1")
	}
	return fmt.Sprintf("%s/c/%x/e%d", prefix, chainID[:], epoch), nil
}

// AggregateOrigin returns "<prefix>/a/<aggregate id hex>".
func AggregateOrigin(prefix string, aggregateID [32]byte) (string, error) {
	if err := ledgerkeys.ValidPrefix(prefix); err != nil {
		return "", fmt.Errorf("ledgerlog: origin prefix: %v", err)
	}
	return fmt.Sprintf("%s/a/%x", prefix, aggregateID[:]), nil
}

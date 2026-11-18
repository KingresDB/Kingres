// SPDX-License-Identifier: AGPL-3.0-only

package ledgerlog

import (
	"bytes"
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
// newline. It refuses everything ParseCheckpoint refuses (an extension line
// holds no newline or other control byte, and there are at most
// MaxExtensionLines of them), so that ParseCheckpoint(Text(c)) is c.
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
		if strings.Contains(e, "\n") {
			// One string is one line: a newline inside it would be read back as
			// two lines, and as more lines than the limit allows.
			return nil, cpErr("an extension line holds a newline")
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

// ErrUntrustedLine is returned by ResignCheckpoint for a note that has a
// signature line under the name of the log (its origin) whose key id belongs to
// none of the trusted keys and none of the new signers. The verifier ignores a
// line that matches no key of its trust file, but refuses the whole note when
// the line matches a key that its trust file lists as usable and does not
// verify; so such a line is either a forgery, or the sign that trusted does not
// hold every key of the trust file. ResignCheckpoint cannot tell which and
// refuses to carry the line along (it would not drop it silently: the caller
// decides, by completing trusted or by making the checkpoint again).
var ErrUntrustedLine = errors.New("ledgerlog: the note has a signature line under the key name of the log whose key id is that of no trusted key")

// ErrNoVouchingSignature is returned by ResignCheckpoint for a note that
// verifies under the keys of the trust file but that no key which counts for a
// fresh checkpoint of its size has signed: every line that verifies is of a
// revoked key, or of a retired key whose max_tree_size is below the size of the
// checkpoint (correction 107). The verifier refuses such a note as a fresh
// checkpoint, so the signature of a good key is not lent to it.
var ErrNoVouchingSignature = errors.New("ledgerlog: no key that counts for a checkpoint of this size has signed the note (every line that verifies is of a revoked key or of a retired key whose bound is below the size)")

// vouches reports whether the trust file entry counts for a fresh checkpoint of
// the size, as the verifier counts it (its StandingAt and LimitFor): not
// revoked, and not limited to a smaller tree. The window (not_before,
// not_after) needs a clock, which this package does not have; the keys of
// ledgerkeys carry no not_after, and a not_before is the activation of a key
// that signs, so it is not applied here (correction 107).
func vouches(k ledgerkeys.TrustKey, size uint64) bool {
	if k.Revoked {
		return false
	}
	return k.MaxTreeSize == nil || size <= *k.MaxTreeSize
}

// ResignCheckpoint adds the signatures of more signers to a signed checkpoint
// (a rotation: the latest checkpoint of a log that no longer grows gets the
// new key's signature). trusted is every entry of the trust file for the log,
// as ledgerkeys.TrustEntries lists them (with IncludeNext when the file lists
// the next keys), so that each key comes with its standing: the note is
// verified under all of them first, as VerifyNote does, and is refused when
// none of its lines verifies or a line of a listed key does not, so a new
// signature is never attached to a text that nobody verified.
//
// The note must also rest on a key that counts: at least one line that verifies
// must be of a key that the verifier counts for a fresh checkpoint of this size
// (not revoked, and not a retired key whose max_tree_size is below the size).
// A note that rests only on a revoked key, or on a retired key beyond its bound,
// is refused with ErrNoVouchingSignature, because the verifier refuses it as a
// fresh checkpoint and the new signature would make it genuine (K3-01,
// correction 107). A line of a key that does not count, beside a line of a
// key that does, is kept.
//
// The result is never a note that the verifier refuses for a reason this
// function can see. It is refused (before anything is signed) when: a new
// signer signed already; the key id of any new signer, or of any trusted key,
// equals that of a different key among them under the origin, whether or not
// that key has a line in the note (ledgerkeys.ErrKeyIDCollision, with both
// fingerprints); or the note holds a line under the origin whose key id is that
// of no trusted key and no new signer (ErrUntrustedLine). Lines under other
// names (cosignatures of a witness) are kept as they are.
func ResignCheckpoint(ctx context.Context, note []byte, trusted []ledgerkeys.TrustKey, more ...ledgerkeys.Signer) ([]byte, error) {
	n, err := ParseNote(note)
	if err != nil {
		return nil, err
	}
	c, err := ParseCheckpoint(n.Text)
	if err != nil {
		return nil, err
	}
	keys := make([]NoteKey, 0, len(trusted))
	for _, k := range trusted {
		keys = append(keys, NoteKey{Name: c.Origin, Public: k.Public})
	}
	verified, err := VerifyNote(n, keys)
	if err != nil {
		return nil, fmt.Errorf("ledgerlog: the checkpoint to re-sign does not verify under the trusted keys: %w", err)
	}
	counts := false
	for _, v := range verified {
		for _, k := range trusted {
			if bytes.Equal(k.Public, v.Public) && vouches(k, c.Size) {
				counts = true
			}
		}
	}
	if !counts {
		return nil, fmt.Errorf("%w (size %d): re-sign only a note that a usable key signed, or make the checkpoint again", ErrNoVouchingSignature, c.Size)
	}
	known := make([]ed25519.PublicKey, 0, len(trusted)+len(more))
	for _, k := range trusted {
		known = append(known, k.Public)
	}
	for _, s := range more {
		known = append(known, s.Public())
	}
	// Every key that can have a line, trusted or new, against every other: a
	// collision with a key that has no line in the note counts, because the
	// verifier lists it too.
	if err := ledgerkeys.CheckKeyIDs(c.Origin, known...); err != nil {
		return nil, err
	}
	ids := map[uint32]bool{}
	for _, p := range known {
		ids[ledgerkeys.KeyID(c.Origin, p)] = true
	}
	for _, s := range n.Signatures {
		if s.Name == c.Origin && !ids[s.ID] {
			return nil, fmt.Errorf("%w: id %08x; pass every key of the trust file in trusted, or make the checkpoint again", ErrUntrustedLine, s.ID)
		}
	}
	for _, s := range more {
		// known is nil: every collision among the keys that are named was reported
		// above with both fingerprints; what addSignature can still meet is a line
		// of a new signer that is already in the note, or one forged under its id.
		if err := n.addSignature(ctx, c.Origin, s, nil); err != nil {
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

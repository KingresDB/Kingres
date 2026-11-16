// SPDX-License-Identifier: AGPL-3.0-only

// Package trust reads, validates and writes the trust file (trust.json,
// ledger-phase7.md section 7.2): the keys and the name prefix an auditor
// pinned from a channel other than the cluster. It is trusted input, so it is
// read strictly: an unknown member, a duplicate key or a wrong fingerprint
// is an error, never ignored.
package trust

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/format"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/jsonx"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/signednote"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/tlog"
)

// Format is the only trust file format version.
const Format = 1

// Limits.
const (
	MaxFileSize        = 1 << 20
	MaxKeys            = 16
	MaxTSARoots        = 32
	MaxClientKeys      = 1024
	MaxLabelLength     = 200
	MaxPrefixLength    = 160 // leaves room in 255 for "/c/<64 hex>/e<digits>"
	ClientKeyIDLength  = 16
	clientKeyIDHexSize = 2 * ClientKeyIDLength
)

// Key is a pinned server signing key. Name is a label for people; the name
// under which the key signs a note is the log origin (see SignerKeys).
type Key struct {
	Name        string
	Public      ed25519.PublicKey
	Fingerprint string // hex SHA-256 of the public key
	NotBefore   *time.Time
	NotAfter    *time.Time
	// Revoked distrusts the key outright: its signatures are never accepted.
	Revoked bool
	// MaxTreeSize, when set, limits the key to checkpoints of at most this
	// tree size. A tree size is in the signed text of a checkpoint, a time is
	// not, so this is how a retired key is limited in a way the data proves.
	MaxTreeSize *uint64
}

// ClientKey is a pinned client attestation key (used by a later step).
type ClientKey struct {
	KeyID  string // hex, 16 bytes
	Public ed25519.PublicKey
	Label  string
}

// File is a validated trust file.
type File struct {
	OriginPrefix string
	Keys         []Key
	TSARoots     []string
	ClientKeys   []ClientKey
}

// Fingerprint returns the hex SHA-256 of a public key, the value the console
// shows and a user compares.
func Fingerprint(pub []byte) string {
	sum := sha256.Sum256(pub)
	return format.EncodeHex(sum[:])
}

// NormalizeFingerprint accepts the fingerprint as a person types or pastes
// it: upper or lower case hex, optionally with colons or spaces between
// bytes, and returns the lower case hex or an error. It must be 32 bytes.
func NormalizeFingerprint(s string) (string, error) {
	t := strings.ToLower(strings.NewReplacer(":", "", " ", "").Replace(s))
	if _, err := format.DecodeHash(t); err != nil {
		return "", fmt.Errorf("fingerprint %q is not 64 hex characters: %w", clip(s), err)
	}
	return t, nil
}

func clip(s string) string {
	if len(s) > 80 {
		return strings.ToValidUTF8(s[:80], "?") + "..."
	}
	return s
}

// ValidPrefix checks an origin prefix: a name (no spaces, no plus, no control
// characters), at most MaxPrefixLength bytes, no scheme, and no leading,
// trailing or doubled slash (ledger-phase7.md 4.2).
func ValidPrefix(p string) error {
	switch {
	case !signednote.ValidName(p):
		return errors.New("must be a non-empty name without spaces, plus signs or control characters")
	case len(p) > MaxPrefixLength:
		return fmt.Errorf("has %d bytes, expected at most %d", len(p), MaxPrefixLength)
	case strings.Contains(p, "://"):
		return errors.New("must not contain a scheme such as https://")
	case strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") || strings.Contains(p, "//"):
		return errors.New("must not start or end with a slash or contain two in a row")
	}
	return nil
}

// New builds a trust file with one key from a public key the user supplied
// and the fingerprint the user typed. It refuses a fingerprint that does not
// match: that is the whole point of the check.
func New(prefix, keyName string, pub []byte, typedFingerprint string) (*File, error) {
	if err := ValidPrefix(prefix); err != nil {
		return nil, fmt.Errorf("origin prefix %q: %w", clip(prefix), err)
	}
	if len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public key has %d bytes, expected %d", len(pub), ed25519.PublicKeySize)
	}
	if err := validLabel(keyName); err != nil {
		return nil, fmt.Errorf("key name: %w", err)
	}
	want, err := NormalizeFingerprint(typedFingerprint)
	if err != nil {
		return nil, err
	}
	got := Fingerprint(pub)
	if want != got {
		return nil, fmt.Errorf("the fingerprint you gave is %s but the public key has the fingerprint %s: the key is not the one you expected", want, got)
	}
	return &File{OriginPrefix: prefix, Keys: []Key{{Name: keyName, Public: append(ed25519.PublicKey(nil), pub...), Fingerprint: got}}}, nil
}

func validLabel(s string) error {
	if s == "" || len(s) > MaxLabelLength || !utf8.ValidString(s) {
		return fmt.Errorf("must be 1 to %d bytes of valid UTF-8", MaxLabelLength)
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return errors.New("must not contain control characters")
		}
	}
	return nil
}

// Parse reads and validates a trust file.
func Parse(data []byte) (*File, error) {
	if len(data) > MaxFileSize {
		return nil, fmt.Errorf("trust file has %d bytes, expected at most %d", len(data), MaxFileSize)
	}
	v, err := jsonx.Parse(data)
	if err != nil {
		return nil, err
	}
	obj, err := v.Object()
	if err != nil {
		return nil, err
	}
	if err := obj.OnlyKeys("format", "origin_prefix", "keys", "tsa_roots", "client_keys", "witnesses"); err != nil {
		return nil, err
	}
	if f, err := obj.Int64("format", 0, 1<<31); err != nil || f != Format {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%s.format is %d, expected %d", obj.Path(), f, Format)
	}
	out := &File{}
	if out.OriginPrefix, err = obj.String("origin_prefix"); err != nil {
		return nil, err
	}
	if err := ValidPrefix(out.OriginPrefix); err != nil {
		return nil, fmt.Errorf("%s.origin_prefix: %w", obj.Path(), err)
	}
	keys, err := obj.Array("keys")
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%s.keys is empty: a trust file with no key can verify nothing", obj.Path())
	}
	if len(keys) > MaxKeys {
		return nil, fmt.Errorf("%s.keys has %d entries, expected at most %d", obj.Path(), len(keys), MaxKeys)
	}
	seenFP, seenName := map[string]bool{}, map[string]bool{}
	for _, kv := range keys {
		k, err := parseKey(kv)
		if err != nil {
			return nil, err
		}
		if seenFP[k.Fingerprint] || seenName[k.Name] {
			return nil, fmt.Errorf("%s: key %q or its fingerprint appears twice", kv.Path(), k.Name)
		}
		seenFP[k.Fingerprint], seenName[k.Name] = true, true
		out.Keys = append(out.Keys, k)
	}
	if obj.Has("tsa_roots") {
		if out.TSARoots, err = parseRoots(obj); err != nil {
			return nil, err
		}
	}
	if obj.Has("client_keys") {
		if out.ClientKeys, err = parseClientKeys(obj); err != nil {
			return nil, err
		}
	}
	if obj.Has("witnesses") {
		w, err := obj.Array("witnesses")
		if err != nil {
			return nil, err
		}
		if len(w) != 0 {
			return nil, fmt.Errorf("%s.witnesses must be empty: witness keys are reserved and not used in this version", obj.Path())
		}
	}
	return out, nil
}

func parseKey(kv jsonx.Value) (Key, error) {
	o, err := kv.Object()
	if err != nil {
		return Key{}, err
	}
	if err := o.OnlyKeys("name", "public_key", "fingerprint", "not_before", "not_after", "revoked", "max_tree_size"); err != nil {
		return Key{}, err
	}
	var k Key
	if k.Name, err = o.String("name"); err != nil {
		return Key{}, err
	}
	if err := validLabel(k.Name); err != nil {
		return Key{}, fmt.Errorf("%s.name: %w", o.Path(), err)
	}
	b64, err := o.String("public_key")
	if err != nil {
		return Key{}, err
	}
	pub, err := signednote.DecodeBase64(b64)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		if err == nil {
			err = fmt.Errorf("has %d bytes, expected %d", len(pub), ed25519.PublicKeySize)
		}
		return Key{}, fmt.Errorf("%s.public_key: %w", o.Path(), err)
	}
	k.Public = pub
	fp, err := o.String("fingerprint")
	if err != nil {
		return Key{}, err
	}
	if fp != Fingerprint(pub) {
		return Key{}, fmt.Errorf("%s.fingerprint is %q, expected %s, the SHA-256 of public_key (a typo in the key or the fingerprint)", o.Path(), clip(fp), Fingerprint(pub))
	}
	k.Fingerprint = fp
	if k.NotBefore, err = optionalTime(o, "not_before"); err != nil {
		return Key{}, err
	}
	if k.NotAfter, err = optionalTime(o, "not_after"); err != nil {
		return Key{}, err
	}
	if k.NotBefore != nil && k.NotAfter != nil && !k.NotAfter.After(*k.NotBefore) {
		return Key{}, fmt.Errorf("%s: not_after is not later than not_before", o.Path())
	}
	if o.Has("revoked") {
		v, err := o.Field("revoked")
		if err != nil {
			return Key{}, err
		}
		if k.Revoked, err = v.Bool(); err != nil {
			return Key{}, err
		}
	}
	if o.Has("max_tree_size") {
		v, err := o.Field("max_tree_size")
		if err != nil {
			return Key{}, err
		}
		if v.Kind() != jsonx.KindNull {
			n, err := v.Int64(1, int64(tlog.MaxTreeSize)-1)
			if err != nil {
				return Key{}, err
			}
			u := uint64(n) // #nosec G115 -- range checked
			k.MaxTreeSize = &u
		}
	}
	if k.Revoked && k.MaxTreeSize != nil {
		return Key{}, fmt.Errorf("%s: a revoked key is not trusted for any size, so max_tree_size makes no sense with revoked", o.Path())
	}
	return k, nil
}

// optionalTime reads a member that is absent, null or an RFC 3339 UTC time
// ending in Z.
func optionalTime(o jsonx.Object, name string) (*time.Time, error) {
	if !o.Has(name) {
		return nil, nil
	}
	v, err := o.Field(name)
	if err != nil {
		return nil, err
	}
	if v.Kind() == jsonx.KindNull {
		return nil, nil
	}
	s, err := v.String()
	if err != nil {
		return nil, err
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil || !strings.HasSuffix(s, "Z") {
		return nil, fmt.Errorf("%s: %q is not an RFC 3339 time in UTC ending in Z", v.Path(), clip(s))
	}
	return &t, nil
}

func parseRoots(obj jsonx.Object) ([]string, error) {
	arr, err := obj.Array("tsa_roots")
	if err != nil {
		return nil, err
	}
	if len(arr) > MaxTSARoots {
		return nil, fmt.Errorf("%s.tsa_roots has %d entries, expected at most %d", obj.Path(), len(arr), MaxTSARoots)
	}
	var out []string
	for _, rv := range arr {
		s, err := rv.String()
		if err != nil {
			return nil, err
		}
		rest, certs := []byte(s), 0
		for {
			var blk *pem.Block
			blk, rest = pem.Decode(rest)
			if blk == nil {
				break
			}
			if blk.Type != "CERTIFICATE" {
				return nil, fmt.Errorf("%s: PEM block of type %q, expected CERTIFICATE", rv.Path(), blk.Type)
			}
			if _, err := x509.ParseCertificate(blk.Bytes); err != nil {
				return nil, fmt.Errorf("%s: certificate does not parse: %w", rv.Path(), err)
			}
			certs++
		}
		if certs == 0 || len(bytes.TrimSpace(rest)) != 0 {
			return nil, fmt.Errorf("%s: expected PEM text holding certificates and nothing else", rv.Path())
		}
		out = append(out, s)
	}
	return out, nil
}

func parseClientKeys(obj jsonx.Object) ([]ClientKey, error) {
	arr, err := obj.Array("client_keys")
	if err != nil {
		return nil, err
	}
	if len(arr) > MaxClientKeys {
		return nil, fmt.Errorf("%s.client_keys has %d entries, expected at most %d", obj.Path(), len(arr), MaxClientKeys)
	}
	var out []ClientKey
	seen := map[string]bool{}
	for _, cv := range arr {
		o, err := cv.Object()
		if err != nil {
			return nil, err
		}
		if err := o.OnlyKeys("key_id", "public_key", "label"); err != nil {
			return nil, err
		}
		var c ClientKey
		if c.KeyID, err = o.String("key_id"); err != nil {
			return nil, err
		}
		idb, err := format.DecodeHex(c.KeyID)
		if err != nil || len(idb) != ClientKeyIDLength {
			return nil, fmt.Errorf("%s.key_id must be %d lower case hex characters", o.Path(), clientKeyIDHexSize)
		}
		b64, err := o.String("public_key")
		if err != nil {
			return nil, err
		}
		pub, err := signednote.DecodeBase64(b64)
		if err != nil || len(pub) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("%s.public_key must be a canonical base64 Ed25519 public key of 32 bytes", o.Path())
		}
		c.Public = pub
		// ledger-phase7.md 3.7: key_id = SHA-256(public_key)[:16].
		if want := Fingerprint(pub)[:clientKeyIDHexSize]; c.KeyID != want {
			return nil, fmt.Errorf("%s.key_id is %s, expected %s, the first 16 bytes of the SHA-256 of public_key", o.Path(), c.KeyID, want)
		}
		if c.Label, err = o.String("label"); err != nil {
			return nil, err
		}
		if err := validLabel(c.Label); err != nil {
			return nil, fmt.Errorf("%s.label: %w", o.Path(), err)
		}
		if seen[c.KeyID] {
			return nil, fmt.Errorf("%s: client key %s appears twice", o.Path(), c.KeyID)
		}
		seen[c.KeyID] = true
		out = append(out, c)
	}
	return out, nil
}

// Standing is how a key stands at one moment, for every command and for
// trust show. Each state other than Usable has a reason that is printed.
type Standing int

// The standings of a key.
const (
	// Usable: the key may be used (a size limit may still apply).
	Usable Standing = iota
	// NotYetValid: now is before not_before.
	NotYetValid
	// Expired: now is after not_after.
	Expired
	// Revoked: the key is distrusted outright.
	Revoked
)

func (s Standing) String() string {
	return [...]string{"usable", "not yet valid", "expired", "revoked"}[s]
}

// StandingAt says how the key stands at now. A revoked key is revoked at any
// time, so --now cannot bring it back.
func (k Key) StandingAt(now time.Time) Standing {
	switch {
	case k.Revoked:
		return Revoked
	case k.NotBefore != nil && now.Before(*k.NotBefore):
		return NotYetValid
	case k.NotAfter != nil && now.After(*k.NotAfter):
		return Expired
	}
	return Usable
}

// Why says in one sentence why the key is not used at now (or that it is
// limited), for notes and for errors.
func (k Key) Why(now time.Time) string {
	id := fmt.Sprintf("key %q (%s)", k.Name, k.Fingerprint[:16])
	switch k.StandingAt(now) {
	case Revoked:
		return id + " is revoked in the trust file and was not used"
	case NotYetValid:
		return fmt.Sprintf("%s is not valid before %s and was not used", id, k.NotBefore.Format(time.RFC3339))
	case Expired:
		return fmt.Sprintf("%s has not_after %s, which has passed, and was not used (to check older checkpoints give --now a time before it, or limit the key with max_tree_size instead)", id, k.NotAfter.Format(time.RFC3339))
	}
	if k.MaxTreeSize != nil {
		return fmt.Sprintf("%s counts only for checkpoints of tree size %d or less", id, *k.MaxTreeSize)
	}
	return id + " is usable"
}

// KeyFor returns the pinned key with this public key.
func (f *File) KeyFor(pub []byte) (Key, bool) {
	for _, k := range f.Keys {
		if bytes.Equal(k.Public, pub) {
			return k, true
		}
	}
	return Key{}, false
}

// SignerKeys returns the keys under which a note about the log with this
// origin may be signed. The origin must be the prefix itself or lie below it
// ("<prefix>/..."); the key name of every signature is the origin
// (ledger-phase7.md 3.2.4), so each pinned key is turned into a note key of
// that name.
//
// What is enforced (correction 14 of the design): a revoked key is never used;
// a key is used only between not_before and not_after, measured on now, which
// is the system clock or --now; a size limit (max_tree_size) is applied to the
// checkpoint by the caller with LimitFor, because the size is in the note. A
// checkpoint carries no time, so a window can only be measured on the
// auditor's clock, not on the time of the signature: this is why a window
// fails closed (the key is left out) instead of being reported.
//
// The notes say, for every key that was left out or is limited, why.
func (f *File) SignerKeys(origin string, now time.Time) ([]signednote.Key, []string, error) {
	if origin != f.OriginPrefix && !strings.HasPrefix(origin, f.OriginPrefix+"/") {
		return nil, nil, fmt.Errorf("origin %q is not below the trusted prefix %q", clip(origin), f.OriginPrefix)
	}
	var keys []signednote.Key
	var notes []string
	for _, k := range f.Keys {
		if k.StandingAt(now) != Usable {
			notes = append(notes, k.Why(now))
			continue
		}
		sk, err := signednote.NewKey(origin, k.Public)
		if err != nil {
			return nil, nil, err
		}
		keys = append(keys, sk)
		if k.MaxTreeSize != nil {
			notes = append(notes, k.Why(now))
		}
	}
	if len(keys) == 0 {
		return nil, notes, fmt.Errorf("the trust file has no key that can be used at %s: %s", now.Format(time.RFC3339), strings.Join(notes, "; "))
	}
	return keys, notes, nil
}

// LimitFor reports whether a checkpoint of this size may count as signed by
// the key with this public key: false when the key is limited to smaller trees.
func (f *File) LimitFor(pub []byte, size uint64) bool {
	k, ok := f.KeyFor(pub)
	return ok && (k.MaxTreeSize == nil || size <= *k.MaxTreeSize)
}

// fileJSON is the written form; field order is the order of the design.
type fileJSON struct {
	Format       int             `json:"format"`
	OriginPrefix string          `json:"origin_prefix"`
	Keys         []keyJSON       `json:"keys"`
	TSARoots     []string        `json:"tsa_roots"`
	ClientKeys   []clientKeyJSON `json:"client_keys"`
	Witnesses    []string        `json:"witnesses"`
}

type keyJSON struct {
	Name        string  `json:"name"`
	PublicKey   string  `json:"public_key"`
	Fingerprint string  `json:"fingerprint"`
	NotBefore   *string `json:"not_before"`
	NotAfter    *string `json:"not_after"`
	Revoked     bool    `json:"revoked"`
	MaxTreeSize *uint64 `json:"max_tree_size"`
}

type clientKeyJSON struct {
	KeyID     string `json:"key_id"`
	PublicKey string `json:"public_key"`
	Label     string `json:"label"`
}

// Marshal writes the file as indented JSON ending in a newline.
func (f *File) Marshal() ([]byte, error) {
	j := fileJSON{Format: Format, OriginPrefix: f.OriginPrefix, Keys: []keyJSON{}, TSARoots: append([]string{}, f.TSARoots...), ClientKeys: []clientKeyJSON{}, Witnesses: []string{}}
	ts := func(t *time.Time) *string {
		if t == nil {
			return nil
		}
		s := t.UTC().Format(time.RFC3339)
		return &s
	}
	for _, k := range f.Keys {
		j.Keys = append(j.Keys, keyJSON{k.Name, base64.StdEncoding.EncodeToString(k.Public), k.Fingerprint, ts(k.NotBefore), ts(k.NotAfter), k.Revoked, k.MaxTreeSize})
	}
	for _, c := range f.ClientKeys {
		j.ClientKeys = append(j.ClientKeys, clientKeyJSON{c.KeyID, base64.StdEncoding.EncodeToString(c.Public), c.Label})
	}
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

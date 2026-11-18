// SPDX-License-Identifier: AGPL-3.0-only

package ledgerkeys

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// MaxPrefixSize is the longest origin prefix of a cluster: 160 bytes, so that
// the longest log origin ("<prefix>/c/<64 hex>/e<digits>") fits in the 255
// bytes of an origin (design 14, correction 3).
const MaxPrefixSize = 160

// ValidPrefix checks an origin prefix as the trust file reader does: a name
// (no space, no plus sign, no control character), at most MaxPrefixSize
// bytes, no scheme, and no leading, trailing or doubled slash.
func ValidPrefix(p string) error {
	switch {
	case !ValidKeyName(p):
		return errors.New("must be a non-empty name without spaces, plus signs or control characters")
	case len(p) > MaxPrefixSize:
		return fmt.Errorf("has %d bytes, expected at most %d", len(p), MaxPrefixSize)
	case strings.Contains(p, "://"):
		return errors.New("must not contain a scheme such as https://")
	case strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") || strings.Contains(p, "//"):
		return errors.New("must not start or end with a slash or contain two in a row")
	}
	return nil
}

// TrustOptions changes what TrustFile writes.
type TrustOptions struct {
	// IncludeNext writes keys in state next too (no validity window): an
	// auditor who pins the next key before its activation can then verify the
	// first checkpoints it signs without a new trust file.
	IncludeNext bool
}

// TrustKey is one entry of the keys member of trust.json (design 7.2 with the
// members of correction 14). Times are nil when not set.
type TrustKey struct {
	Name        string
	Public      ed25519.PublicKey
	Fingerprint string
	NotBefore   *time.Time
	NotAfter    *time.Time
	Revoked     bool
	MaxTreeSize *uint64
}

// TrustEntries turns the key set into the entries of the trust file, in the
// order of creation (then fingerprint). What each state says:
//
//   - next: left out (unless IncludeNext), no window.
//   - active: not_before is the activation time; no not_after.
//   - retired: not_before is the activation time. No not_after: what a
//     retired key signed stays valid for ever (design 3.3), and not_after
//     would make the verifier refuse those checkpoints as new ones after it
//     (correction 14). max_tree_size is written when the key has one.
//   - revoked: revoked true, which the verifier honours at any --now.
//
// The set must validate, and at least one key must be written.
func TrustEntries(set Set, opt TrustOptions) ([]TrustKey, error) {
	if err := set.Validate(); err != nil {
		return nil, err
	}
	keys := append(Set(nil), set...)
	sort.SliceStable(keys, func(i, j int) bool {
		if !keys[i].CreatedAt.Equal(keys[j].CreatedAt) {
			return keys[i].CreatedAt.Before(keys[j].CreatedAt)
		}
		return keys[i].Fingerprint() < keys[j].Fingerprint()
	})
	var out []TrustKey
	for _, k := range keys {
		if k.State == StateNext && !opt.IncludeNext {
			continue
		}
		e := TrustKey{Name: k.TrustLabel(), Public: append(ed25519.PublicKey(nil), k.Public...), Fingerprint: k.Fingerprint()}
		if !k.ActivatedAt.IsZero() {
			t := k.ActivatedAt.UTC()
			e.NotBefore = &t
		}
		switch k.State {
		case StateRevoked:
			e.Revoked = true
		case StateRetired:
			if k.MaxTreeSize != 0 {
				u := k.MaxTreeSize
				e.MaxTreeSize = &u
			}
		}
		out = append(out, e)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: no key to write, a trust file with no key verifies nothing", ErrInvalidArgument)
	}
	return out, nil
}

type trustJSON struct {
	Format       int       `json:"format"`
	OriginPrefix string    `json:"origin_prefix"`
	Keys         []keyJSON `json:"keys"`
	TSARoots     []string  `json:"tsa_roots"`
	ClientKeys   []string  `json:"client_keys"`
	Witnesses    []string  `json:"witnesses"`
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

// TrustFile writes trust.json for the origin prefix and the key set, in
// the bytes kingres-verify writes for the same content (2-space indented JSON
// in the member order of the design, times as RFC 3339 UTC, a final
// newline), with empty tsa_roots, client_keys and witnesses.
//
// It refuses to write a file in which two listed keys have the same key id
// under the prefix itself (a valid origin of the verifier) or under any of the
// origins given (the origins of the logs, which lie below the prefix), with the
// same typed error as AddChecked and CheckSetKeyIDs (ErrKeyIDCollision, every
// pair in the error), so that a set built with the unchecked Add does not get
// its collision published without a word (K3-08, correction 107). The
// prefix alone is not enough for the logs: pass their origins. A caller that
// has no origin to give and wrote no key with Add has nothing to fear.
func TrustFile(prefix string, set Set, opt TrustOptions, origins ...string) ([]byte, error) {
	if err := ValidPrefix(prefix); err != nil {
		return nil, fmt.Errorf("%w: origin prefix %q: %v", ErrInvalidArgument, clip(prefix), err)
	}
	entries, err := TrustEntries(set, opt)
	if err != nil {
		return nil, err
	}
	for _, o := range append([]string{prefix}, origins...) {
		if err := CheckSetKeyIDs(set, opt, o); err != nil {
			return nil, err
		}
	}
	return marshalTrust(prefix, entries)
}

func marshalTrust(prefix string, entries []TrustKey) ([]byte, error) {
	j := trustJSON{Format: 1, OriginPrefix: prefix, Keys: []keyJSON{}, TSARoots: []string{}, ClientKeys: []string{}, Witnesses: []string{}}
	ts := func(t *time.Time) *string {
		if t == nil {
			return nil
		}
		s := t.UTC().Format(time.RFC3339Nano)
		return &s
	}
	for _, e := range entries {
		j.Keys = append(j.Keys, keyJSON{e.Name, base64.StdEncoding.EncodeToString(e.Public), e.Fingerprint, ts(e.NotBefore), ts(e.NotAfter), e.Revoked, e.MaxTreeSize})
	}
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package anchor

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// RedactedText is what a Secret prints as.
const RedactedText = "[redacted]"

// Secret is a credential value (an access key, a session token, a password).
// It prints as RedactedText in every formatting verb, in JSON and in text
// encodings, so a value that was put in a log line or an error by mistake does
// not leak. The only way to read it is the Reveal method.
type Secret string

// Reveal returns the value. Call it only where the value is used.
func (s Secret) Reveal() string { return string(s) }

// String implements fmt.Stringer.
func (s Secret) String() string { return RedactedText }

// GoString implements fmt.GoStringer.
func (s Secret) GoString() string { return RedactedText }

// Format implements fmt.Formatter; every verb prints RedactedText.
func (s Secret) Format(f fmt.State, verb rune) { _, _ = f.Write([]byte(RedactedText)) }

// MarshalJSON implements json.Marshaler.
func (s Secret) MarshalJSON() ([]byte, error) { return json.Marshal(RedactedText) }

// MarshalText implements encoding.TextMarshaler.
func (s Secret) MarshalText() ([]byte, error) { return []byte(RedactedText), nil }

// Redactor removes known secret values from text that came from outside
// (a server's error message may echo a key). The zero value redacts nothing.
type Redactor struct{ values []string }

// MinSecretLength is the length below which a value is not redacted (it would match unrelated text); the
// validation of credentials refuses values shorter than this, so that no secret escapes the redactor (P7A-10).
const MinSecretLength = 4

// NewRedactor returns a Redactor for the given values; empty values and values
// shorter than MinSecretLength bytes are ignored (they would match unrelated
// text). Each value is also removed in the spellings that a server may echo: the
// URL-escaped form, the hexadecimal forms and the four base64 forms (a Basic
// header that is repeated in a status text holds the base64 of "user:password",
// which the caller gives as a value of its own).
func NewRedactor(secrets ...Secret) Redactor {
	var r Redactor
	seen := map[string]bool{}
	add := func(v string) {
		if len(v) >= MinSecretLength && !seen[v] {
			seen[v] = true
			r.values = append(r.values, v)
		}
	}
	for _, s := range secrets {
		v := string(s)
		if len(v) < MinSecretLength {
			continue
		}
		add(v)
		add(url.QueryEscape(v))
		add(url.PathEscape(v))
		add(hex.EncodeToString([]byte(v)))
		add(strings.ToUpper(hex.EncodeToString([]byte(v))))
		b := []byte(v)
		add(base64.StdEncoding.EncodeToString(b))
		add(base64.RawStdEncoding.EncodeToString(b))
		add(base64.URLEncoding.EncodeToString(b))
		add(base64.RawURLEncoding.EncodeToString(b))
	}
	// longest first, so that a form that holds another is removed whole
	sort.SliceStable(r.values, func(i, j int) bool { return len(r.values[i]) > len(r.values[j]) })
	return r
}

// Clean returns text with every secret value replaced by RedactedText, with
// every byte outside printable ASCII replaced by '?', cut to max bytes
// (max <= 0 means 300).
func (r Redactor) Clean(text string, max int) string {
	if max <= 0 {
		max = 300
	}
	for _, v := range r.values {
		text = strings.ReplaceAll(text, v, RedactedText)
	}
	b := make([]byte, 0, len(text))
	for i := 0; i < len(text) && len(b) < max; i++ {
		c := text[i]
		if c < 0x20 || c > 0x7e {
			c = '?'
		}
		b = append(b, c)
	}
	out := string(b)
	// a value cut in the middle by max is not matched above; scan again
	for _, v := range r.values {
		if len(v) > 4 {
			for n := len(v) - 1; n >= 4; n-- {
				out = strings.TrimSuffix(out, v[:n])
			}
		}
	}
	return out
}

// Format implements fmt.Formatter so that printing a Redactor never shows the
// values it holds.
func (r Redactor) Format(f fmt.State, verb rune) { _, _ = f.Write([]byte("[redactor]")) }

// MarshalJSON implements json.Marshaler.
func (r Redactor) MarshalJSON() ([]byte, error) { return json.Marshal("[redactor]") }

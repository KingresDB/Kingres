// SPDX-License-Identifier: AGPL-3.0-only

package anchor

import (
	"encoding/json"
	"fmt"
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

// NewRedactor returns a Redactor for the given values; empty values and values
// shorter than 4 bytes are ignored (they would match unrelated text).
func NewRedactor(secrets ...Secret) Redactor {
	var r Redactor
	for _, s := range secrets {
		if len(s) >= 4 {
			r.values = append(r.values, string(s))
		}
	}
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

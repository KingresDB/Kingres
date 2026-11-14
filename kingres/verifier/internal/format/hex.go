// SPDX-License-Identifier: PostgreSQL

package format

import (
	"fmt"
)

const hexDigits = "0123456789abcdef"

// EncodeHex returns the lower case hex of b, the form the ledger JSON uses
// ("hex is lower case").
func EncodeHex(b []byte) string {
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, hexDigits[c>>4], hexDigits[c&15])
	}
	return string(out)
}

// DecodeHex decodes strict lower case hex: an even number of characters from
// [0-9a-f], nothing else (no prefix, no spaces, no upper case).
func DecodeHex(s string) ([]byte, error) {
	if len(s)%2 != 0 {
		return nil, fmt.Errorf("hex string has %d characters, expected an even number", len(s))
	}
	out := make([]byte, len(s)/2)
	for i := 0; i < len(s); i += 2 {
		hi, ok1 := nibble(s[i])
		lo, ok2 := nibble(s[i+1])
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("hex string has an invalid character at position %d, expected lower case 0-9a-f", i)
		}
		out[i/2] = hi<<4 | lo
	}
	return out, nil
}

func nibble(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	}
	return 0, false
}

// DecodeHash decodes exactly 32 bytes of strict lower case hex.
func DecodeHash(s string) ([32]byte, error) {
	var out [32]byte
	if len(s) != 64 {
		return out, fmt.Errorf("hash has %d hex characters, expected 64", len(s))
	}
	b, err := DecodeHex(s)
	if err != nil {
		return out, err
	}
	copy(out[:], b)
	return out, nil
}

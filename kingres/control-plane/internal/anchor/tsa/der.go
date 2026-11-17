// SPDX-License-Identifier: AGPL-3.0-only

package tsa

import (
	"errors"
	"math/big"
	"time"
)

// MaxDERDepth is the deepest nesting of constructed elements accepted.
const MaxDERDepth = 32

// Universal tags used by the parsers.
const (
	tagBoolean   = 0x01
	tagInteger   = 0x02
	tagOctet     = 0x04
	tagNull      = 0x05
	tagOID       = 0x06
	tagEnum      = 0x0a
	tagGenTime   = 0x18
	tagSequence  = 0x30
	tagSet       = 0x31
	tagCtx0      = 0xa0
	tagCtx1      = 0xa1
	tagCtx0Prim  = 0x80
	tagCtx1Prim  = 0x81
	tagBitString = 0x03
)

var errDER = errors.New("malformed or non-canonical DER")

// tlv is one element: its tag byte, its content and its whole encoding.
type tlv struct {
	tag     byte
	content []byte
	raw     []byte
}

// readTLV reads one element from b. It accepts only what DER allows for
// the encoding of tag and length: a single tag byte (no tag number of 31 or
// more), a definite length in the shortest form.
func readTLV(b []byte) (t tlv, rest []byte, err error) {
	if len(b) < 2 {
		return t, nil, errDER
	}
	tag := b[0]
	if tag&0x1f == 0x1f {
		return t, nil, errDER
	}
	l := int(b[1])
	hdr := 2
	if l == 0x80 {
		return t, nil, errDER // indefinite length
	}
	if l > 0x80 {
		n := l & 0x7f
		if n > 4 || len(b) < 2+n {
			return t, nil, errDER
		}
		if b[2] == 0 {
			return t, nil, errDER // leading zero: not the shortest form
		}
		l = 0
		for i := 0; i < n; i++ {
			l = l<<8 | int(b[2+i])
		}
		if l < 0x80 {
			return t, nil, errDER // fits the short form
		}
		hdr = 2 + n
	}
	if l < 0 || l > len(b)-hdr {
		return t, nil, errDER
	}
	return tlv{tag: tag, content: b[hdr : hdr+l], raw: b[:hdr+l]}, b[hdr+l:], nil
}

// children splits the content of a constructed element.
func children(content []byte) ([]tlv, error) {
	var out []tlv
	for len(content) > 0 {
		t, rest, err := readTLV(content)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
		content = rest
		if len(out) > 1024 {
			return nil, errDER
		}
	}
	return out, nil
}

// checkDER verifies that b is a sequence of elements in strict DER form, all
// the way down: definite minimal lengths, no high tag numbers, SEQUENCE and
// SET constructed, no other universal type constructed, BOOLEAN 00 or ff,
// INTEGER and ENUMERATED minimal and non-empty, NULL empty, at most
// MaxDERDepth levels. The content of OCTET STRING and BIT STRING is not
// entered.
func checkDER(b []byte, depth int) error {
	if depth > MaxDERDepth {
		return errDER
	}
	for len(b) > 0 {
		t, rest, err := readTLV(b)
		if err != nil {
			return err
		}
		b = rest
		constructed := t.tag&0x20 != 0
		if t.tag&0xc0 == 0 { // universal
			switch t.tag & 0x1f {
			case 0x10, 0x11:
				if !constructed {
					return errDER
				}
			default:
				if constructed {
					return errDER
				}
			}
			switch t.tag {
			case tagBoolean:
				if len(t.content) != 1 || (t.content[0] != 0 && t.content[0] != 0xff) {
					return errDER
				}
			case tagInteger, tagEnum:
				c := t.content
				if len(c) == 0 || (len(c) > 1 && (c[0] == 0 && c[1] < 0x80 || c[0] == 0xff && c[1] >= 0x80)) {
					return errDER
				}
			case tagNull:
				if len(t.content) != 0 {
					return errDER
				}
			}
		}
		if constructed {
			if err := checkDER(t.content, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// oneTLV reads exactly one element and nothing after it.
func oneTLV(b []byte) (tlv, error) {
	t, rest, err := readTLV(b)
	if err != nil || len(rest) != 0 {
		return t, errDER
	}
	return t, nil
}

func parseInt(t tlv) (*big.Int, error) {
	if t.tag != tagInteger || len(t.content) == 0 {
		return nil, errDER
	}
	n := new(big.Int)
	c := t.content
	if c[0]&0x80 != 0 {
		inv := make([]byte, len(c))
		for i := range c {
			inv[i] = ^c[i]
		}
		n.SetBytes(inv)
		n.Add(n, big.NewInt(1))
		n.Neg(n)
	} else {
		n.SetBytes(c)
	}
	return n, nil
}

// parseOID returns the dotted form of an OBJECT IDENTIFIER element.
func parseOID(t tlv) (string, error) {
	if t.tag != tagOID || len(t.content) == 0 || len(t.content) > 64 {
		return "", errDER
	}
	var out []byte
	var v uint64
	first := true
	for i, c := range t.content {
		if v == 0 && c == 0x80 {
			return "", errDER // non-minimal
		}
		if v > 1<<55 {
			return "", errDER
		}
		v = v<<7 | uint64(c&0x7f)
		if c&0x80 != 0 {
			if i == len(t.content)-1 {
				return "", errDER
			}
			continue
		}
		if first {
			a := v / 40
			if a > 2 {
				a = 2
			}
			out = appendUint(out, a)
			out = append(out, '.')
			out = appendUint(out, v-a*40)
			first = false
		} else {
			out = append(out, '.')
			out = appendUint(out, v)
		}
		v = 0
	}
	return string(out), nil
}

func appendUint(b []byte, v uint64) []byte {
	var tmp [20]byte
	i := len(tmp)
	for {
		i--
		tmp[i] = byte('0' + v%10)
		v /= 10
		if v == 0 {
			break
		}
	}
	return append(b, tmp[i:]...)
}

// parseGenTime parses a DER GeneralizedTime: YYYYMMDDHHMMSS, optionally a
// fraction without trailing zero, and Z.
func parseGenTime(t tlv) (time.Time, error) {
	if t.tag != tagGenTime {
		return time.Time{}, errDER
	}
	s := t.content
	if len(s) < 15 || s[len(s)-1] != 'Z' {
		return time.Time{}, errDER
	}
	num := func(i, n int) (int, bool) {
		v := 0
		for ; n > 0; n-- {
			if s[i] < '0' || s[i] > '9' {
				return 0, false
			}
			v = v*10 + int(s[i]-'0')
			i++
		}
		return v, true
	}
	var f [6]int
	pos := 0
	for k, n := range []int{4, 2, 2, 2, 2, 2} {
		v, ok := num(pos, n)
		if !ok {
			return time.Time{}, errDER
		}
		f[k] = v
		pos += n
	}
	nsec := 0
	rest := s[14 : len(s)-1]
	if len(rest) > 0 {
		if rest[0] != '.' || len(rest) < 2 || len(rest) > 10 || rest[len(rest)-1] == '0' {
			return time.Time{}, errDER
		}
		scale := 100000000
		for _, c := range rest[1:] {
			if c < '0' || c > '9' {
				return time.Time{}, errDER
			}
			nsec += int(c-'0') * scale
			scale /= 10
		}
	}
	if f[1] < 1 || f[1] > 12 || f[2] < 1 || f[2] > 31 || f[3] > 23 || f[4] > 59 || f[5] > 59 {
		return time.Time{}, errDER
	}
	tm := time.Date(f[0], time.Month(f[1]), f[2], f[3], f[4], f[5], nsec, time.UTC)
	if tm.Day() != f[2] {
		return time.Time{}, errDER
	}
	return tm, nil
}

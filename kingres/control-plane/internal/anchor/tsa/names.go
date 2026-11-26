// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package tsa

import (
	"bytes"
	"crypto/x509"
	"encoding/asn1"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// tsaNameMatches reports whether the GeneralName of the TSTInfo tsa field
// (RFC 3161 2.4.2: it must correspond to one of the subject names of the
// certificate) corresponds to a name of the signer's certificate: its
// subject or a directoryName of the subject alternative name, compared by the
// decoded attribute values and not by the bytes (see sameName); a DNS name
// (case does not matter), URI, e-mail address or IP address that is one of
// the alternative names; or an otherName that is one of them byte for byte.
// The check refuses a token only when no name of the certificate
// corresponds.
func tsaNameMatches(b []byte, c *x509.Certificate) bool {
	g, err := oneTLV(b)
	if err != nil {
		return false
	}
	switch g.tag {
	case 0xa4: // directoryName
		if sameName(g.content, c.RawSubject) {
			return true
		}
		for _, n := range sanNames(c) {
			if n.tag == 0xa4 && sameName(g.content, n.content) {
				return true
			}
		}
	case 0xa0: // otherName: only an identical one corresponds
		for _, n := range sanNames(c) {
			if n.tag == 0xa0 && bytes.Equal(n.raw, g.raw) {
				return true
			}
		}
	case 0x82:
		for _, n := range c.DNSNames {
			if asciiEqualFold(n, string(g.content)) {
				return true
			}
		}
	case 0x86:
		for _, u := range c.URIs {
			if u.String() == string(g.content) {
				return true
			}
		}
	case 0x81:
		for _, m := range c.EmailAddresses {
			if m == string(g.content) {
				return true
			}
		}
	case 0x87:
		for _, ip := range c.IPAddresses {
			if (len(g.content) == 4 || len(g.content) == 16) && ip.Equal(g.content) {
				return true
			}
		}
	}
	return false
}

// sanNames returns the general names of the subject alternative name
// extension of c (none when it has none or it cannot be read).
func sanNames(c *x509.Certificate) []tlv {
	for _, e := range c.Extensions {
		if e.Id.String() != "2.5.29.17" {
			continue
		}
		top, err := oneTLV(e.Value)
		if err != nil {
			return nil
		}
		names, err := children(top.content)
		if err != nil {
			return nil
		}
		return names
	}
	return nil
}

// attr is one AttributeTypeAndValue of a distinguished name.
type attr struct {
	oid string
	// text is the decoded value of a string type; raw is the whole encoding
	// of any other value, which is compared as it is
	text  string
	isStr bool
	raw   []byte
}

// parseName reads a distinguished name (the encoding of a Name, a SEQUENCE
// of SETs of AttributeTypeAndValue).
func parseName(b []byte) ([][]attr, bool) {
	top, err := oneTLV(b)
	if err != nil || top.tag != tagSequence {
		return nil, false
	}
	rdns, err := children(top.content)
	if err != nil {
		return nil, false
	}
	out := make([][]attr, 0, len(rdns))
	for _, rdn := range rdns {
		if rdn.tag != tagSet {
			return nil, false
		}
		atvs, err := children(rdn.content)
		if err != nil || len(atvs) == 0 {
			return nil, false
		}
		var set []attr
		for _, atv := range atvs {
			if atv.tag != tagSequence {
				return nil, false
			}
			items, err := children(atv.content)
			if err != nil || len(items) != 2 {
				return nil, false
			}
			var oid asn1.ObjectIdentifier
			if rest, err := asn1.Unmarshal(items[0].raw, &oid); err != nil || len(rest) != 0 {
				return nil, false
			}
			a := attr{oid: oid.String(), raw: items[1].raw}
			if s, ok := decodeString(items[1]); ok {
				a.text, a.isStr = s, true
			}
			set = append(set, a)
		}
		out = append(out, set)
	}
	return out, true
}

// decodeString decodes the string types a name attribute may use, and refuses
// (false) a value that is not valid in its type, so that a malformed value is
// compared by its bytes and never by what a lenient reader makes of it:
// UTF8String (valid UTF-8), PrintableString, IA5String (ASCII), NumericString
// (digits and space), VisibleString (printable ASCII), TeletexString (only when
// every byte is ASCII: T.61 is not Latin-1, so a Teletex value with a byte above
// 0x7f is not decoded and is compared by its bytes), BMPString (UTF-16 big
// endian, even length, no lone surrogate) and UniversalString (UTF-32 big
// endian, length a multiple of four, every value a Unicode scalar value).
func decodeString(t tlv) (string, bool) {
	c := t.content
	allBytes := func(ok func(b byte) bool) bool {
		for _, b := range c {
			if !ok(b) {
				return false
			}
		}
		return true
	}
	switch t.tag {
	case 0x0c: // UTF8String
		if !utf8.Valid(c) {
			return "", false
		}
		return string(c), true
	case 0x13: // PrintableString
		if !allBytes(func(b byte) bool {
			return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || strings.IndexByte(" '()+,-./:=?", b) >= 0
		}) {
			return "", false
		}
		return string(c), true
	case 0x16: // IA5String
		if !allBytes(func(b byte) bool { return b < 0x80 }) {
			return "", false
		}
		return string(c), true
	case 0x12: // NumericString
		if !allBytes(func(b byte) bool { return b >= '0' && b <= '9' || b == ' ' }) {
			return "", false
		}
		return string(c), true
	case 0x1a: // VisibleString
		if !allBytes(func(b byte) bool { return b >= 0x20 && b <= 0x7e }) {
			return "", false
		}
		return string(c), true
	case 0x14: // TeletexString: ASCII only
		if !allBytes(func(b byte) bool { return b < 0x80 }) {
			return "", false
		}
		return string(c), true
	case 0x1e: // BMPString, UTF-16 big endian
		if len(c)%2 != 0 {
			return "", false
		}
		u := make([]uint16, len(c)/2)
		for i := range u {
			u[i] = uint16(c[2*i])<<8 | uint16(c[2*i+1])
		}
		for i := 0; i < len(u); i++ {
			switch {
			case u[i] >= 0xd800 && u[i] < 0xdc00:
				if i+1 >= len(u) || u[i+1] < 0xdc00 || u[i+1] > 0xdfff {
					return "", false
				}
				i++
			case u[i] >= 0xdc00 && u[i] <= 0xdfff:
				return "", false
			}
		}
		return string(utf16.Decode(u)), true
	case 0x1c: // UniversalString, UTF-32 big endian
		if len(c)%4 != 0 {
			return "", false
		}
		r := make([]rune, len(c)/4)
		for i := range r {
			v := uint32(c[4*i])<<24 | uint32(c[4*i+1])<<16 | uint32(c[4*i+2])<<8 | uint32(c[4*i+3])
			if v > 0x10ffff || (v >= 0xd800 && v <= 0xdfff) {
				return "", false
			}
			r[i] = rune(v)
		}
		return string(r), true
	}
	return "", false
}

// foldAttrValue is the comparison form of a string attribute value: ASCII
// letters in lower case (no other case folding: the Kelvin sign is not a k and
// the long s is not an s), leading and trailing ASCII spaces removed and inner
// runs of ASCII spaces made one (no other character is white space here: a
// no-break space, a tab and an ideographic space stay what they are). A value
// of only spaces is one space, so that it is not an empty value.
func foldAttrValue(s string) string {
	var b strings.Builder
	space := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b.WriteByte(c)
	}
	if b.Len() == 0 && len(s) > 0 {
		return " "
	}
	return b.String()
}

// sameAttr compares two attributes the way RFC 5280 section 7.1 describes for
// names, with the narrow profile of foldAttrValue: the same attribute type, and
// for strings valid in their type the same decoded value whatever the string
// type, ignoring ASCII case and with ASCII white space folded. Every other
// value (another type of value, or a string that is not valid in its type, or a
// TeletexString that is not ASCII) is compared as it is encoded.
func sameAttr(a, b attr) bool {
	if a.oid != b.oid {
		return false
	}
	if a.isStr != b.isStr {
		return false
	}
	if !a.isStr {
		return bytes.Equal(a.raw, b.raw)
	}
	return foldAttrValue(a.text) == foldAttrValue(b.text)
}

// sameName reports whether two encoded distinguished names are the same name:
// the same relative distinguished names in the same order, each one the same
// set of attributes.
func sameName(a, b []byte) bool {
	if bytes.Equal(a, b) {
		return true
	}
	x, ok1 := parseName(a)
	y, ok2 := parseName(b)
	if !ok1 || !ok2 || len(x) != len(y) {
		return false
	}
	for i := range x {
		if len(x[i]) != len(y[i]) {
			return false
		}
		used := make([]bool, len(y[i]))
		for _, p := range x[i] {
			found := false
			for j, q := range y[i] {
				if !used[j] && sameAttr(p, q) {
					used[j], found = true, true
					break
				}
			}
			if !found {
				return false
			}
		}
	}
	return true
}

// asciiEqualFold reports whether a and b are equal when the ASCII letters are
// folded (a DNS name is ASCII; no Unicode folding applies).
func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if x >= 'A' && x <= 'Z' {
			x += 'a' - 'A'
		}
		if y >= 'A' && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

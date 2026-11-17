// SPDX-License-Identifier: AGPL-3.0-only

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
			if strings.EqualFold(n, string(g.content)) {
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

// decodeString decodes the string types a name attribute may use: UTF8String,
// PrintableString, IA5String, NumericString, VisibleString, TeletexString
// (read as Latin-1, as it is used in practice), BMPString and UniversalString.
func decodeString(t tlv) (string, bool) {
	switch t.tag {
	case 0x0c: // UTF8String
		if !utf8.Valid(t.content) {
			return "", false
		}
		return string(t.content), true
	case 0x13, 0x16, 0x12, 0x1a: // Printable, IA5, Numeric, Visible
		return string(t.content), true
	case 0x14: // Teletex as Latin-1
		r := make([]rune, len(t.content))
		for i, c := range t.content {
			r[i] = rune(c)
		}
		return string(r), true
	case 0x1e: // BMPString, UTF-16 big endian
		if len(t.content)%2 != 0 {
			return "", false
		}
		u := make([]uint16, len(t.content)/2)
		for i := range u {
			u[i] = uint16(t.content[2*i])<<8 | uint16(t.content[2*i+1])
		}
		return string(utf16.Decode(u)), true
	case 0x1c: // UniversalString, UTF-32 big endian
		if len(t.content)%4 != 0 {
			return "", false
		}
		r := make([]rune, len(t.content)/4)
		for i := range r {
			r[i] = rune(uint32(t.content[4*i])<<24 | uint32(t.content[4*i+1])<<16 | uint32(t.content[4*i+2])<<8 | uint32(t.content[4*i+3]))
		}
		return string(r), true
	}
	return "", false
}

// sameAttr compares two attributes the way RFC 5280 section 7.1 describes for
// names: the same attribute type, and for strings the same decoded value
// whatever the string type, ignoring case and with white space folded (leading
// and trailing white space removed, inner runs made one space). Other values
// are compared as they are encoded.
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
	fold := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	return strings.EqualFold(fold(a.text), fold(b.text))
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

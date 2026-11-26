// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package tsa

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"net"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
)

// nameAs encodes the distinguished name of subject with every attribute value
// written in the string type tag (after transform), as a Name. It is how
// another authority, or another tool, may write the same name.
func nameAs(t testing.TB, subject pkix.Name, tag int, transform func(string) string) []byte {
	t.Helper()
	seq := subject.ToRDNSequence()
	for i := range seq {
		for j := range seq[i] {
			s, ok := seq[i][j].Value.(string)
			if !ok {
				t.Fatal("not a string attribute")
			}
			s = transform(s)
			var b []byte
			switch tag {
			case asn1.TagBMPString:
				for _, u := range utf16.Encode([]rune(s)) {
					b = append(b, byte(u>>8), byte(u))
				}
			default:
				b = []byte(s)
			}
			seq[i][j].Value = asn1.RawValue{Class: asn1.ClassUniversal, Tag: tag, Bytes: b}
		}
	}
	der, err := asn1.Marshal(seq)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func same(s string) string { return s }

// A2-09: the name of the TSTInfo is compared with the certificate by the
// decoded values of its attributes (any string type, case and white space
// folded), not by the bytes. A token whose name is written in another string
// type than the certificate's is accepted; one that names something else is
// refused.
func TestTSANameCorrespondsByDecodedValue(t *testing.T) {
	a := newAuthority(t, authOpts{})
	subject := a.tsa.Subject
	for name, c := range map[string]struct {
		dn []byte
		ok bool
	}{
		"printable (as the certificate)": {a.tsa.RawSubject, true},
		"utf8":                           {nameAs(t, subject, asn1.TagUTF8String, same), true},
		"ia5":                            {nameAs(t, subject, asn1.TagIA5String, same), true},
		"bmp":                            {nameAs(t, subject, asn1.TagBMPString, same), true},
		"upper case":                     {nameAs(t, subject, asn1.TagUTF8String, strings.ToUpper), true},
		"white space folded":             {nameAs(t, subject, asn1.TagUTF8String, func(s string) string { return "  " + strings.ReplaceAll(s, " ", "   ") + " " }), true},
		"another value":                  {nameAs(t, subject, asn1.TagUTF8String, func(s string) string { return s + "x" }), false},
		"the root's name":                {a.root.RawSubject, false},
	} {
		tok := a.token(testReq(), issueOpts{tsaName: tl(0xa4, c.dn)})
		_, err := VerifyToken(tok, Expect{Digest: testDigest}, a.rootPool, a.interPool)
		if (err == nil) != c.ok || (err != nil && anchor.CodeOf(err) != CodeTSAName) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The comparison itself, on the forms of name that a certificate can carry.
func TestTSANameForms(t *testing.T) {
	subject := pkix.Name{CommonName: "Time Stamp", Organization: []string{"Example"}}
	other := pkix.Name{CommonName: "Else", Organization: []string{"Example"}}
	reversed, _ := asn1.Marshal(pkix.RDNSequence{subject.ToRDNSequence()[1], subject.ToRDNSequence()[0]})
	san := func(general ...[]byte) []pkix.Extension {
		var content []byte
		for _, g := range general {
			content = append(content, g...)
		}
		v := append([]byte{0x30, byte(len(content))}, content...)
		return []pkix.Extension{{Id: asn1.ObjectIdentifier{2, 5, 29, 17}, Value: v}}
	}
	other1 := tl(0xa4, nameAs(t, other, asn1.TagPrintableString, same))
	otherName := tl(0xa0, append(tl(0x06, []byte{0x2a, 0x03}), tl(0xa0, tl(0x0c, []byte("x")))...))
	cert := &x509.Certificate{
		RawSubject:     nameAs(t, subject, asn1.TagPrintableString, same),
		DNSNames:       []string{"TSA.Example.COM"},
		EmailAddresses: []string{"ts@example.com"},
		IPAddresses:    []net.IP{net.ParseIP("192.0.2.7"), net.ParseIP("2001:db8::7")},
		Extensions:     san(other1, otherName),
	}
	for name, c := range map[string]struct {
		g  []byte
		ok bool
	}{
		"subject as utf8":        {tl(0xa4, nameAs(t, subject, asn1.TagUTF8String, same)), true},
		"directoryName of SAN":   {tl(0xa4, nameAs(t, other, asn1.TagUTF8String, same)), true},
		"other directory name":   {tl(0xa4, nameAs(t, pkix.Name{CommonName: "Nobody"}, asn1.TagUTF8String, same)), false},
		"rdns in another order":  {tl(0xa4, reversed), false},
		"not a name":             {tl(0xa4, []byte{1, 2, 3}), false},
		"dns, other case":        {tl(0x82, []byte("tsa.example.com")), true},
		"dns, other name":        {tl(0x82, []byte("tsa.example.org")), false},
		"email":                  {tl(0x81, []byte("ts@example.com")), true},
		"email, other":           {tl(0x81, []byte("x@example.com")), false},
		"ipv4":                   {tl(0x87, net.ParseIP("192.0.2.7").To4()), true},
		"ipv6":                   {tl(0x87, net.ParseIP("2001:db8::7").To16()), true},
		"ip, other":              {tl(0x87, net.ParseIP("192.0.2.8").To4()), false},
		"ip, bad length":         {tl(0x87, []byte{192, 0, 2}), false},
		"otherName, the same":    {otherName, true},
		"otherName, another one": {tl(0xa0, append(tl(0x06, []byte{0x2a, 0x04}), tl(0xa0, tl(0x0c, []byte("x")))...)), false},
	} {
		if got := tsaNameMatches(c.g, cert); got != c.ok {
			t.Errorf("%s: %v", name, got)
		}
	}
	// a certificate whose SAN cannot be read does not make the subject fail
	broken := &x509.Certificate{RawSubject: cert.RawSubject, Extensions: []pkix.Extension{{Id: asn1.ObjectIdentifier{2, 5, 29, 17}, Value: []byte{0x30, 0x05, 1}}}}
	if !tsaNameMatches(tl(0xa4, nameAs(t, subject, asn1.TagUTF8String, same)), broken) {
		t.Error("the subject was refused because of an unreadable SAN")
	}
}

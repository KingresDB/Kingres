// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

// Package tsatoken verifies an RFC 3161 time-stamp token. It is written from RFC 3161 (the time-stamp protocol),
// RFC 5652 (the Cryptographic Message Syntax, section 5 for SignedData and section 5.4 for the signed attributes),
// RFC 5035 (the ESS signing certificate attributes) and RFC 5280 (for the chain), with encoding/asn1 for the
// structures and crypto/x509 for the certificates. It shares no code with the library the publisher uses to
// make the evidence: an auditor's check of a token must not be the publisher's check run again.
//
// A token is the DER of the CMS ContentInfo (correction 184): SEQUENCE { id-signedData, [0] SignedData }. What is
// verified, from the token and the auditor's own roots alone:
//
//   - the encoding is DER (no BER, no indefinite length, nothing after the ContentInfo); SignedData has version
//     3, the content type id-ct-TSTInfo, and exactly one SignerInfo (RFC 3161 2.4.2: a token holds no other signature);
//   - TSTInfo is version 1, its message imprint is SHA-256 of the digest the auditor expects, its genTime is a
//     GeneralizedTime in UTC;
//   - the SignerInfo has signed attributes that carry the content type id-ct-TSTInfo, the message digest of the
//     TSTInfo and the ESS signing-certificate attribute (v1 or v2), whose hash names the signer certificate;
//   - the signature over the DER of the signed attributes verifies under the public key of that certificate
//     (RSA PKCS#1 v1.5 or ECDSA, with SHA-256, SHA-384 or SHA-512, the same hash as the SignerInfo's digest
//     algorithm; RSA keys of at least 2048 bits; SHA-1 and anything else is refused);
//   - the certificate carries the extended key usage extension, marked critical, with id-kp-timeStamping as its only
//     purpose (RFC 3161 2.3), and chains to one of the auditor's roots with its validity checked at genTime, not at
//     the auditor's clock (a token must stay good after its certificate expires);
//   - when TSTInfo names the authority (the tsa field), the name is the subject of the signer certificate or one of
//     its directory-name alternative names, byte for byte; any other form is refused.
//
// Not checked: the revocation status of any certificate (no OCSP, no CRL, no long-term validation: the evidence keeps
// the whole token so that a tool that does can be run on it), the accuracy and the ordering (reported, not used),
// and the nonce (it is not kept with the evidence). The time is the authority's word: it proves the digest existed
// before genTime if the authority was honest at genTime, and that is all.
package tsatoken

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha1" // #nosec G505 -- only for the ESS signing certificate v1 hash, which RFC 3161 prescribes
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/asn1"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// Limits.
const (
	MaxToken = 64 << 10
	maxCerts = 16
)

var (
	oidSignedData    = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 2}
	oidTSTInfo       = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 16, 1, 4}
	oidContentType   = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 3}
	oidMessageDigest = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 4}
	oidSigningCertV1 = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 16, 2, 12}
	oidSigningCertV2 = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 16, 2, 47}
	oidSHA1          = asn1.ObjectIdentifier{1, 3, 14, 3, 2, 26}
	oidSHA256        = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}
	oidSHA384        = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 2}
	oidSHA512        = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 3}
	oidRSA           = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 1}
	oidRSASHA256     = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 11}
	oidRSASHA384     = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 12}
	oidRSASHA512     = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 13}
	oidECDSASHA256   = asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 3, 2}
	oidECDSASHA384   = asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 3, 3}
	oidECDSASHA512   = asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 3, 4}
	oidExtKeyUsage   = asn1.ObjectIdentifier{2, 5, 29, 37}
	oidSubjectAlt    = asn1.ObjectIdentifier{2, 5, 29, 17}
)

// Token is a token parsed and checked for structure; nothing about it is trusted yet.
type Token struct {
	TSTInfo   TSTInfo
	Certs     []*x509.Certificate
	signers   []*x509.Certificate // the certificates that the SignerInfo's identifier selects
	eContent  []byte
	signed    []byte // the DER of the signed attributes as the signature covers it (tag SET)
	sig       []byte
	sigAlg    asn1.ObjectIdentifier
	digestAlg asn1.ObjectIdentifier
	essHash   crypto.Hash
	essCertID []byte
}

// TSTInfo is the content of a token (RFC 3161 2.4.2).
type TSTInfo struct {
	Policy     string // dotted
	ImprintAlg asn1.ObjectIdentifier
	Imprint    []byte
	Serial     *big.Int
	GenTime    time.Time
	Accuracy   string // as the token writes it, "" when absent
	Ordering   bool
	Nonce      *big.Int // nil when absent
	TSAName    []byte   // the DER of the tsa GeneralName, nil when absent
}

// Result is what Verify established.
type Result struct {
	GenTime         time.Time
	Policy          string
	Serial          string
	Accuracy        string
	Signer          *x509.Certificate
	SignerKeySHA256 [32]byte // SHA-256 of the DER SubjectPublicKeyInfo of the signer certificate
	Chain           []*x509.Certificate
}

func fail(format string, a ...any) error { return fmt.Errorf("time-stamp token: "+format, a...) }

// next reads one DER element from b and returns it with the rest.
func next(b []byte) (asn1.RawValue, []byte, error) {
	var rv asn1.RawValue
	rest, err := asn1.Unmarshal(b, &rv)
	if err != nil {
		return rv, nil, err
	}
	return rv, rest, nil
}

// elems splits the content of a constructed element into its elements.
func elems(content []byte) ([]asn1.RawValue, error) {
	var out []asn1.RawValue
	for len(content) > 0 {
		rv, rest, err := next(content)
		if err != nil {
			return nil, err
		}
		out = append(out, rv)
		content = rest
		if len(out) > 1<<12 {
			return nil, errors.New("too many elements")
		}
	}
	return out, nil
}

func isSeq(rv asn1.RawValue) bool {
	return rv.Class == asn1.ClassUniversal && rv.Tag == asn1.TagSequence && rv.IsCompound
}

func isSet(rv asn1.RawValue) bool {
	return rv.Class == asn1.ClassUniversal && rv.Tag == asn1.TagSet && rv.IsCompound
}

func isCtx(rv asn1.RawValue, tag int, compound bool) bool {
	return rv.Class == asn1.ClassContextSpecific && rv.Tag == tag && rv.IsCompound == compound
}

func oidOf(rv asn1.RawValue) (asn1.ObjectIdentifier, error) {
	if rv.Class != asn1.ClassUniversal || rv.Tag != asn1.TagOID || rv.IsCompound {
		return nil, errors.New("not an object identifier")
	}
	var o asn1.ObjectIdentifier
	if _, err := asn1.Unmarshal(rv.FullBytes, &o); err != nil {
		return nil, err
	}
	return o, nil
}

func intOf(rv asn1.RawValue) (*big.Int, error) {
	if rv.Class != asn1.ClassUniversal || rv.Tag != asn1.TagInteger || rv.IsCompound {
		return nil, errors.New("not an integer")
	}
	var n *big.Int
	if _, err := asn1.Unmarshal(rv.FullBytes, &n); err != nil {
		return nil, err
	}
	return n, nil
}

func octetsOf(rv asn1.RawValue) ([]byte, error) {
	if rv.Class != asn1.ClassUniversal || rv.Tag != asn1.TagOctetString || rv.IsCompound {
		return nil, errors.New("not an octet string")
	}
	return rv.Bytes, nil
}

// algID reads AlgorithmIdentifier { algorithm OID, parameters ANY OPTIONAL } and returns the OID. The parameters of
// the hash algorithms must be absent or NULL; of an ECDSA signature absent; of RSA NULL or absent.
func algID(rv asn1.RawValue) (asn1.ObjectIdentifier, error) {
	if !isSeq(rv) {
		return nil, errors.New("an algorithm identifier is not a sequence")
	}
	parts, err := elems(rv.Bytes)
	if err != nil || len(parts) < 1 || len(parts) > 2 {
		return nil, errors.New("an algorithm identifier has 1 or 2 elements")
	}
	oid, err := oidOf(parts[0])
	if err != nil {
		return nil, err
	}
	if len(parts) == 2 && !(parts[1].Class == asn1.ClassUniversal && parts[1].Tag == asn1.TagNull && len(parts[1].Bytes) == 0) {
		return nil, errors.New("the parameters of an algorithm identifier are neither absent nor NULL")
	}
	return oid, nil
}

func hashOf(oid asn1.ObjectIdentifier) (crypto.Hash, bool) {
	switch {
	case oid.Equal(oidSHA256):
		return crypto.SHA256, true
	case oid.Equal(oidSHA384):
		return crypto.SHA384, true
	case oid.Equal(oidSHA512):
		return crypto.SHA512, true
	}
	return 0, false
}

func sum(h crypto.Hash, b []byte) []byte {
	switch h {
	case crypto.SHA256:
		s := sha256.Sum256(b)
		return s[:]
	case crypto.SHA384:
		s := sha512.Sum384(b)
		return s[:]
	case crypto.SHA512:
		s := sha512.Sum512(b)
		return s[:]
	}
	return nil
}

// generalizedTime reads a DER GeneralizedTime in UTC: YYYYMMDDHHMMSS[.fraction]Z, no trailing zero in the fraction.
func generalizedTime(rv asn1.RawValue) (time.Time, error) {
	if rv.Class != asn1.ClassUniversal || rv.Tag != asn1.TagGeneralizedTime || rv.IsCompound {
		return time.Time{}, errors.New("genTime is not a GeneralizedTime")
	}
	s := string(rv.Bytes)
	if len(s) < 15 || s[len(s)-1] != 'Z' {
		return time.Time{}, fmt.Errorf("genTime %q is not in UTC (it must end in Z)", s)
	}
	body := s[:len(s)-1]
	for i := 0; i < len(body); i++ {
		c := body[i]
		if i == 14 {
			if c != '.' {
				return time.Time{}, fmt.Errorf("genTime %q is malformed", s)
			}
			continue
		}
		if c < '0' || c > '9' {
			return time.Time{}, fmt.Errorf("genTime %q is malformed", s)
		}
	}
	if len(body) > 14 {
		frac := body[15:]
		if len(frac) == 0 || len(frac) > 9 || frac[len(frac)-1] == '0' {
			return time.Time{}, fmt.Errorf("genTime %q has a fraction that DER does not allow", s)
		}
	}
	t, err := time.Parse("20060102150405", body[:14])
	if err != nil {
		return time.Time{}, fmt.Errorf("genTime %q: %w", s, err)
	}
	if len(body) > 14 {
		f, err := time.Parse("20060102150405.999999999", body)
		if err != nil {
			return time.Time{}, fmt.Errorf("genTime %q: %w", s, err)
		}
		t = f
	}
	return t.UTC(), nil
}

// Parse reads the structure of a token. It checks the encoding and the shape and nothing about trust.
func Parse(der []byte) (*Token, error) {
	if len(der) == 0 || len(der) > MaxToken {
		return nil, fail("%d bytes, expected 1 to %d", len(der), MaxToken)
	}
	ci, rest, err := next(der)
	if err != nil {
		return nil, fail("the ContentInfo is not DER: %v", err)
	}
	if len(rest) != 0 {
		return nil, fail("%d byte(s) follow the ContentInfo", len(rest))
	}
	if !isSeq(ci) {
		return nil, fail("the ContentInfo is not a sequence")
	}
	parts, err := elems(ci.Bytes)
	if err != nil || len(parts) != 2 {
		return nil, fail("the ContentInfo has %d elements, expected 2", len(parts))
	}
	if oid, err := oidOf(parts[0]); err != nil || !oid.Equal(oidSignedData) {
		return nil, fail("the content type is not id-signedData")
	}
	if !isCtx(parts[1], 0, true) {
		return nil, fail("the content is not [0] EXPLICIT")
	}
	sdv, rest, err := next(parts[1].Bytes)
	if err != nil || len(rest) != 0 || !isSeq(sdv) {
		return nil, fail("SignedData is not a single sequence")
	}
	sd, err := elems(sdv.Bytes)
	if err != nil || len(sd) < 4 {
		return nil, fail("SignedData has too few elements")
	}
	ver, err := intOf(sd[0])
	if err != nil || !ver.IsInt64() || ver.Int64() != 3 {
		return nil, fail("SignedData version is not 3 (the content is not id-data)")
	}
	if !isSet(sd[1]) {
		return nil, fail("digestAlgorithms is not a set")
	}
	digestAlgs, err := elems(sd[1].Bytes)
	if err != nil || len(digestAlgs) != 1 {
		return nil, fail("digestAlgorithms does not hold exactly one algorithm")
	}
	digestSetAlg, err := algID(digestAlgs[0])
	if err != nil {
		return nil, fail("digestAlgorithms: %v", err)
	}
	if !isSeq(sd[2]) {
		return nil, fail("encapContentInfo is not a sequence")
	}
	enc, err := elems(sd[2].Bytes)
	if err != nil || len(enc) != 2 {
		return nil, fail("encapContentInfo has %d elements, expected 2", len(enc))
	}
	if oid, err := oidOf(enc[0]); err != nil || !oid.Equal(oidTSTInfo) {
		return nil, fail("the content type is not id-ct-TSTInfo")
	}
	if !isCtx(enc[1], 0, true) {
		return nil, fail("eContent is not [0] EXPLICIT")
	}
	oct, rest, err := next(enc[1].Bytes)
	if err != nil || len(rest) != 0 {
		return nil, fail("eContent is not a single octet string")
	}
	econtent, err := octetsOf(oct)
	if err != nil {
		return nil, fail("eContent: %v", err)
	}
	t := &Token{eContent: econtent}
	if err := t.parseTSTInfo(econtent); err != nil {
		return nil, err
	}
	idx := 3
	if isCtx(sd[idx], 0, true) { // certificates [0] IMPLICIT SET OF CertificateChoices
		cs, err := elems(sd[idx].Bytes)
		if err != nil || len(cs) > maxCerts {
			return nil, fail("certificates: too many or malformed")
		}
		for _, c := range cs {
			if !isSeq(c) {
				return nil, fail("a certificate choice that is not a Certificate (attribute certificates and other formats are not read)")
			}
			cert, err := x509.ParseCertificate(c.FullBytes)
			if err != nil {
				return nil, fail("a certificate does not parse: %v", err)
			}
			t.Certs = append(t.Certs, cert)
		}
		idx++
	}
	if idx < len(sd) && isCtx(sd[idx], 1, true) { // crls: not read
		idx++
	}
	if idx != len(sd)-1 || !isSet(sd[idx]) {
		return nil, fail("SignedData does not end with exactly the set of SignerInfos")
	}
	sis, err := elems(sd[idx].Bytes)
	if err != nil || len(sis) != 1 {
		return nil, fail("the token holds %d SignerInfo(s), expected exactly 1 (RFC 3161 2.4.2)", len(sis))
	}
	if err := t.parseSignerInfo(sis[0]); err != nil {
		return nil, err
	}
	if !t.digestAlg.Equal(digestSetAlg) {
		return nil, fail("the digest algorithm of the SignerInfo is not the one SignedData lists")
	}
	return t, nil
}

func (t *Token) parseTSTInfo(b []byte) error {
	v, rest, err := next(b)
	if err != nil || len(rest) != 0 || !isSeq(v) {
		return fail("TSTInfo is not a single sequence")
	}
	f, err := elems(v.Bytes)
	if err != nil || len(f) < 5 {
		return fail("TSTInfo has too few elements")
	}
	ver, err := intOf(f[0])
	if err != nil || !ver.IsInt64() || ver.Int64() != 1 {
		return fail("TSTInfo version is not 1")
	}
	pol, err := oidOf(f[1])
	if err != nil {
		return fail("TSTInfo policy: %v", err)
	}
	t.TSTInfo.Policy = pol.String()
	if !isSeq(f[2]) {
		return fail("the message imprint is not a sequence")
	}
	mi, err := elems(f[2].Bytes)
	if err != nil || len(mi) != 2 {
		return fail("the message imprint has %d elements, expected 2", len(mi))
	}
	if t.TSTInfo.ImprintAlg, err = algID(mi[0]); err != nil {
		return fail("the imprint algorithm: %v", err)
	}
	if t.TSTInfo.Imprint, err = octetsOf(mi[1]); err != nil {
		return fail("the imprint: %v", err)
	}
	if t.TSTInfo.Serial, err = intOf(f[3]); err != nil {
		return fail("the serial number: %v", err)
	}
	if t.TSTInfo.Serial.Sign() < 0 {
		return fail("the serial number is negative")
	}
	if t.TSTInfo.GenTime, err = generalizedTime(f[4]); err != nil {
		return fail("%v", err)
	}
	rest2 := f[5:]
	if len(rest2) > 0 && isSeq(rest2[0]) { // accuracy
		t.TSTInfo.Accuracy = accuracyText(rest2[0])
		rest2 = rest2[1:]
	}
	if len(rest2) > 0 && rest2[0].Class == asn1.ClassUniversal && rest2[0].Tag == asn1.TagBoolean {
		var b bool
		if _, err := asn1.Unmarshal(rest2[0].FullBytes, &b); err != nil {
			return fail("ordering: %v", err)
		}
		t.TSTInfo.Ordering = b
		rest2 = rest2[1:]
	}
	if len(rest2) > 0 && rest2[0].Class == asn1.ClassUniversal && rest2[0].Tag == asn1.TagInteger {
		n, err := intOf(rest2[0])
		if err != nil {
			return fail("nonce: %v", err)
		}
		t.TSTInfo.Nonce = n
		rest2 = rest2[1:]
	}
	if len(rest2) > 0 && isCtx(rest2[0], 0, true) { // tsa [0] EXPLICIT GeneralName
		gn, r, err := next(rest2[0].Bytes)
		if err != nil || len(r) != 0 {
			return fail("the tsa name is not one GeneralName")
		}
		t.TSTInfo.TSAName = gn.FullBytes
		rest2 = rest2[1:]
	}
	if len(rest2) > 0 && isCtx(rest2[0], 1, true) { // extensions [1] IMPLICIT: none is understood
		// a TSTInfo extension that is marked critical would have to be refused; their content is not read here, so
		// any extension at all is refused rather than guessed at
		return fail("TSTInfo carries extensions, which this verifier does not read")
	}
	if len(rest2) != 0 {
		return fail("TSTInfo has an element it does not know")
	}
	return nil
}

// accuracyText renders Accuracy { seconds, millis [0], micros [1] } as the token writes it.
func accuracyText(rv asn1.RawValue) string {
	parts, err := elems(rv.Bytes)
	if err != nil {
		return "unreadable"
	}
	s, ms, us := "0", "0", "0"
	for _, p := range parts {
		switch {
		case p.Class == asn1.ClassUniversal && p.Tag == asn1.TagInteger:
			if n, err := intOf(p); err == nil {
				s = n.String()
			}
		case isCtx(p, 0, false):
			ms = new(big.Int).SetBytes(p.Bytes).String()
		case isCtx(p, 1, false):
			us = new(big.Int).SetBytes(p.Bytes).String()
		}
	}
	return fmt.Sprintf("%s s, %s ms, %s us", s, ms, us)
}

func (t *Token) parseSignerInfo(rv asn1.RawValue) error {
	if !isSeq(rv) {
		return fail("the SignerInfo is not a sequence")
	}
	f, err := elems(rv.Bytes)
	if err != nil || len(f) < 5 {
		return fail("the SignerInfo has too few elements")
	}
	ver, err := intOf(f[0])
	if err != nil || !ver.IsInt64() || (ver.Int64() != 1 && ver.Int64() != 3) {
		return fail("the SignerInfo version is neither 1 nor 3")
	}
	var issuer, ski []byte
	var serial *big.Int
	switch {
	case isSeq(f[1]):
		p, err := elems(f[1].Bytes)
		if err != nil || len(p) != 2 || !isSeq(p[0]) {
			return fail("the signer's issuer and serial number are malformed")
		}
		issuer = p[0].FullBytes
		if serial, err = intOf(p[1]); err != nil {
			return fail("the signer's serial number: %v", err)
		}
		if ver.Int64() != 1 {
			return fail("the SignerInfo names the signer by issuer and serial number and has version %d", ver.Int64())
		}
	case isCtx(f[1], 0, false):
		ski = f[1].Bytes
		if ver.Int64() != 3 {
			return fail("the SignerInfo names the signer by key identifier and has version %d", ver.Int64())
		}
	default:
		return fail("the signer identifier is neither issuer and serial number nor a subject key identifier")
	}
	if t.digestAlg, err = algID(f[2]); err != nil {
		return fail("the digest algorithm: %v", err)
	}
	hash, ok := hashOf(t.digestAlg)
	if !ok {
		return fail("the digest algorithm %s is not SHA-256, SHA-384 or SHA-512", t.digestAlg)
	}
	if !isCtx(f[3], 0, true) {
		return fail("the SignerInfo has no signed attributes: a time-stamp token must carry them (RFC 3161 2.4.2 and RFC 5652 5.3)")
	}
	attrs, err := elems(f[3].Bytes)
	if err != nil {
		return fail("the signed attributes: %v", err)
	}
	// the DER of the signed attributes as the signature covers it: the same bytes with the tag of a SET (RFC 5652 5.4)
	t.signed = append([]byte(nil), f[3].FullBytes...)
	t.signed[0] = 0x31
	seen := map[string]bool{}
	var haveCT, haveMD, haveESS bool
	var md []byte
	for _, a := range attrs {
		if !isSeq(a) {
			return fail("an attribute is not a sequence")
		}
		ap, err := elems(a.Bytes)
		if err != nil || len(ap) != 2 || !isSet(ap[1]) {
			return fail("an attribute is malformed")
		}
		ao, err := oidOf(ap[0])
		if err != nil {
			return fail("an attribute type: %v", err)
		}
		if seen[ao.String()] {
			return fail("the attribute %s appears twice", ao)
		}
		seen[ao.String()] = true
		vals, err := elems(ap[1].Bytes)
		if err != nil || len(vals) != 1 {
			return fail("the attribute %s does not have exactly one value", ao)
		}
		switch {
		case ao.Equal(oidContentType):
			ct, err := oidOf(vals[0])
			if err != nil || !ct.Equal(oidTSTInfo) {
				return fail("the content-type attribute is not id-ct-TSTInfo")
			}
			haveCT = true
		case ao.Equal(oidMessageDigest):
			if md, err = octetsOf(vals[0]); err != nil {
				return fail("the message-digest attribute: %v", err)
			}
			haveMD = true
		case ao.Equal(oidSigningCertV1):
			if err := t.parseESS(vals[0], false); err != nil {
				return err
			}
			haveESS = true
		case ao.Equal(oidSigningCertV2):
			if err := t.parseESS(vals[0], true); err != nil {
				return err
			}
			haveESS = true
		}
	}
	if seen[oidSigningCertV1.String()] && seen[oidSigningCertV2.String()] {
		return fail("both ESS signing-certificate attributes are present")
	}
	if !haveCT || !haveMD || !haveESS {
		return fail("the signed attributes lack the content type, the message digest or the ESS signing certificate (RFC 3161 2.4.2)")
	}
	if !bytes.Equal(md, sum(hash, t.eContent)) {
		return fail("the message-digest attribute is not the digest of the TSTInfo")
	}
	if t.sigAlg, err = algID(f[4]); err != nil {
		return fail("the signature algorithm: %v", err)
	}
	if len(f) < 6 {
		return fail("the SignerInfo has no signature")
	}
	if t.sig, err = octetsOf(f[5]); err != nil || len(t.sig) == 0 {
		return fail("the signature: not an octet string")
	}
	if len(f) > 6 && !(len(f) == 7 && isCtx(f[6], 1, true)) {
		return fail("the SignerInfo has an element it does not know")
	}
	for _, c := range t.Certs {
		switch {
		case issuer != nil && bytes.Equal(c.RawIssuer, issuer) && c.SerialNumber.Cmp(serial) == 0:
			t.signers = append(t.signers, c)
		case ski != nil && bytes.Equal(c.SubjectKeyId, ski):
			t.signers = append(t.signers, c)
		}
	}
	if len(t.signers) == 0 {
		return fail("none of the certificates in the token is the signer's")
	}
	return nil
}

// parseESS reads the first ESSCertID(v2) of a signing-certificate attribute: the hash of the signer certificate.
func (t *Token) parseESS(v asn1.RawValue, v2 bool) error {
	if !isSeq(v) {
		return fail("the signing-certificate attribute is not a sequence")
	}
	f, err := elems(v.Bytes)
	if err != nil || len(f) < 1 || len(f) > 2 || !isSeq(f[0]) {
		return fail("the signing-certificate attribute is malformed")
	}
	ids, err := elems(f[0].Bytes)
	if err != nil || len(ids) < 1 {
		return fail("the signing-certificate attribute names no certificate")
	}
	if !isSeq(ids[0]) {
		return fail("an ESSCertID is not a sequence")
	}
	c, err := elems(ids[0].Bytes)
	if err != nil || len(c) < 1 {
		return fail("an ESSCertID is malformed")
	}
	h := crypto.SHA1
	if !v2 {
		if t.essCertID, err = octetsOf(c[0]); err != nil || len(t.essCertID) != 20 {
			return fail("the ESSCertID hash is not a SHA-1 of 20 bytes")
		}
		t.essHash = h
		return nil
	}
	h = crypto.SHA256 // the default of ESSCertIDv2
	rest := c
	if isSeq(c[0]) {
		oid, err := algID(c[0])
		if err != nil {
			return fail("the ESSCertIDv2 hash algorithm: %v", err)
		}
		var ok bool
		if h, ok = hashOf(oid); !ok {
			return fail("the ESSCertIDv2 hash algorithm %s is not SHA-256, SHA-384 or SHA-512", oid)
		}
		rest = c[1:]
	}
	if len(rest) < 1 {
		return fail("the ESSCertIDv2 has no certificate hash")
	}
	if t.essCertID, err = octetsOf(rest[0]); err != nil || len(t.essCertID) != h.Size() {
		return fail("the ESSCertIDv2 hash has the wrong length")
	}
	t.essHash = h
	return nil
}

func (t *Token) essMatches(c *x509.Certificate) bool {
	switch t.essHash {
	case crypto.SHA1:
		s := sha1.Sum(c.Raw) // #nosec G401 -- RFC 3161 prescribes the hash of an ESSCertID v1
		return bytes.Equal(s[:], t.essCertID)
	default:
		return bytes.Equal(sum(t.essHash, c.Raw), t.essCertID)
	}
}

// Options of Verify.
type Options struct {
	// Digest is what the token must stamp: the SHA-256 of the checkpoint text.
	Digest [32]byte
	// Roots are the auditor's own trust anchors (the pinned TSA certificates). Without one nothing is verified.
	Roots *x509.CertPool
	// KeySHA256 optionally pins the signer: SHA-256 of the DER SubjectPublicKeyInfo of the signing certificate.
	KeySHA256 *[32]byte
}

// Verify checks a token as the package comment says.
func Verify(der []byte, o Options) (*Result, error) {
	t, err := Parse(der)
	if err != nil {
		return nil, err
	}
	return t.Verify(o)
}

// Verify applies the checks that need the auditor's inputs to a parsed token.
func (t *Token) Verify(o Options) (*Result, error) {
	if o.Roots == nil {
		return nil, fail("no root of the auditor was given: nothing can be verified")
	}
	if !t.TSTInfo.ImprintAlg.Equal(oidSHA256) {
		return nil, fail("the message imprint uses %s, expected SHA-256", t.TSTInfo.ImprintAlg)
	}
	if !bytes.Equal(t.TSTInfo.Imprint, o.Digest[:]) {
		return nil, fail("the message imprint is %x and the digest of the checkpoint is %x: the token stamps something else", t.TSTInfo.Imprint, o.Digest[:])
	}
	var first error
	for _, c := range t.signers {
		res, err := t.verifySigner(c, o)
		if err == nil {
			return res, nil
		}
		if first == nil {
			first = err
		}
	}
	return nil, first
}

func (t *Token) verifySigner(c *x509.Certificate, o Options) (*Result, error) {
	if !t.essMatches(c) {
		return nil, fail("the signing-certificate attribute does not name the certificate that the signer identifier selects")
	}
	hash, _ := hashOf(t.digestAlg)
	if err := checkSignature(c.PublicKey, t.sigAlg, hash, t.signed, t.sig); err != nil {
		return nil, fail("the signature does not verify: %v", err)
	}
	if err := checkEKU(c); err != nil {
		return nil, err
	}
	if err := t.checkName(c); err != nil {
		return nil, err
	}
	spki := sha256.Sum256(c.RawSubjectPublicKeyInfo)
	if o.KeySHA256 != nil && *o.KeySHA256 != spki {
		return nil, fail("the signer's public key (SHA-256 %x) is not the one you pinned (%x)", spki, o.KeySHA256[:])
	}
	inter := x509.NewCertPool()
	for _, ic := range t.Certs {
		if ic != c {
			inter.AddCert(ic)
		}
	}
	chains, err := c.Verify(x509.VerifyOptions{Roots: o.Roots, Intermediates: inter, CurrentTime: t.TSTInfo.GenTime, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageTimeStamping}})
	if err != nil {
		return nil, fail("the signer's certificate does not chain to a root of yours at genTime %s: %v", t.TSTInfo.GenTime.Format(time.RFC3339), err)
	}
	return &Result{GenTime: t.TSTInfo.GenTime, Policy: t.TSTInfo.Policy, Serial: t.TSTInfo.Serial.String(), Accuracy: t.TSTInfo.Accuracy, Signer: c, SignerKeySHA256: spki, Chain: chains[0]}, nil
}

func checkSignature(pub any, alg asn1.ObjectIdentifier, hash crypto.Hash, signed, sig []byte) error {
	var want crypto.Hash
	ecdsaAlg, rsaAlg := false, false
	switch {
	case alg.Equal(oidRSA):
		rsaAlg, want = true, hash
	case alg.Equal(oidRSASHA256):
		rsaAlg, want = true, crypto.SHA256
	case alg.Equal(oidRSASHA384):
		rsaAlg, want = true, crypto.SHA384
	case alg.Equal(oidRSASHA512):
		rsaAlg, want = true, crypto.SHA512
	case alg.Equal(oidECDSASHA256):
		ecdsaAlg, want = true, crypto.SHA256
	case alg.Equal(oidECDSASHA384):
		ecdsaAlg, want = true, crypto.SHA384
	case alg.Equal(oidECDSASHA512):
		ecdsaAlg, want = true, crypto.SHA512
	default:
		return fmt.Errorf("the signature algorithm %s is not RSA PKCS#1 v1.5 or ECDSA with SHA-256, SHA-384 or SHA-512", alg)
	}
	if want != hash {
		return errors.New("the signature algorithm and the digest algorithm of the SignerInfo name different hashes")
	}
	digest := sum(hash, signed)
	switch k := pub.(type) {
	case *rsa.PublicKey:
		if !rsaAlg {
			return errors.New("an ECDSA signature under an RSA key")
		}
		if k.N.BitLen() < 2048 {
			return fmt.Errorf("the RSA key has %d bits, expected at least 2048", k.N.BitLen())
		}
		return rsa.VerifyPKCS1v15(k, hash, digest, sig)
	case *ecdsa.PublicKey:
		if !ecdsaAlg {
			return errors.New("an RSA signature under an ECDSA key")
		}
		if !ecdsa.VerifyASN1(k, digest, sig) {
			return errors.New("ECDSA verification failed")
		}
		return nil
	}
	return errors.New("the signer's key is neither RSA nor ECDSA")
}

// checkEKU: the extended key usage extension is present, critical, and names id-kp-timeStamping alone (RFC 3161 2.3).
func checkEKU(c *x509.Certificate) error {
	var ext *struct{ critical bool }
	for _, e := range c.Extensions {
		if e.Id.Equal(oidExtKeyUsage) {
			if ext != nil {
				return fail("the signer's certificate has two extended key usage extensions")
			}
			ext = &struct{ critical bool }{e.Critical}
		}
	}
	if ext == nil {
		return fail("the signer's certificate has no extended key usage extension")
	}
	if !ext.critical {
		return fail("the signer's extended key usage extension is not critical (RFC 3161 2.3)")
	}
	if len(c.ExtKeyUsage) != 1 || c.ExtKeyUsage[0] != x509.ExtKeyUsageTimeStamping || len(c.UnknownExtKeyUsage) != 0 {
		return fail("the signer's extended key usage is not id-kp-timeStamping alone")
	}
	return nil
}

// checkName: when TSTInfo names the authority, the name is the subject of the signer certificate or one of its directory-name
// alternative names, byte for byte. Any other form of name is refused.
func (t *Token) checkName(c *x509.Certificate) error {
	if t.TSTInfo.TSAName == nil {
		return nil
	}
	gn, _, err := next(t.TSTInfo.TSAName)
	if err != nil || !isCtx(gn, 4, true) { // directoryName [4] EXPLICIT Name
		return fail("the tsa name in TSTInfo is not a directory name, the only form this verifier compares")
	}
	name := gn.Bytes // the DER of the Name
	if bytes.Equal(name, c.RawSubject) {
		return nil
	}
	for _, e := range c.Extensions {
		if !e.Id.Equal(oidSubjectAlt) {
			continue
		}
		seq, _, err := next(e.Value)
		if err != nil || !isSeq(seq) {
			break
		}
		names, err := elems(seq.Bytes)
		if err != nil {
			break
		}
		for _, n := range names {
			if isCtx(n, 4, true) && bytes.Equal(n.Bytes, name) {
				return nil
			}
		}
	}
	return fail("the tsa name in TSTInfo is not a subject name of the signer's certificate")
}

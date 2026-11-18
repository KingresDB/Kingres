// SPDX-License-Identifier: AGPL-3.0-only

package tsa

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"fmt"
	"math/big"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
)

// Limits.
const (
	// MaxCertificates is the most certificates accepted in a token.
	MaxCertificates = 16
	// DefaultMaxSkew is the distance between genTime and the clock, in either
	// direction, beyond which a token is refused (code time_skew, class
	// refused; correction 62 of the design).
	DefaultMaxSkew = 5 * time.Minute
	// MinRSABits is the smallest RSA signer key accepted, whatever the
	// toolchain's own floor is.
	MinRSABits = 2048
)

// Object identifiers (dotted).
const (
	oidSignedData   = "1.2.840.113549.1.7.2"
	oidTSTInfo      = "1.2.840.113549.1.9.16.1.4"
	oidContentType  = "1.2.840.113549.1.9.3"
	oidMessageDig   = "1.2.840.113549.1.9.4"
	oidSigningCert  = "1.2.840.113549.1.9.16.2.12"
	oidSigningCertV = "1.2.840.113549.1.9.16.2.47"
	oidSHA1         = "1.3.14.3.2.26"
	oidSHA256       = "2.16.840.1.101.3.4.2.1"
	oidSHA384       = "2.16.840.1.101.3.4.2.2"
	oidSHA512       = "2.16.840.1.101.3.4.2.3"
	oidRSA          = "1.2.840.113549.1.1.1"
	oidRSASHA256    = "1.2.840.113549.1.1.11"
	oidRSASHA384    = "1.2.840.113549.1.1.12"
	oidRSASHA512    = "1.2.840.113549.1.1.13"
	oidECPublicKey  = "1.2.840.10045.2.1"
	oidECDSASHA256  = "1.2.840.10045.4.3.2"
	oidECDSASHA384  = "1.2.840.10045.4.3.3"
	oidECDSASHA512  = "1.2.840.10045.4.3.4"
	oidEKU          = "2.5.29.37"
	oidTimeStamping = "1.3.6.1.5.5.7.3.8"
)

// Stable error codes of verification failures (anchor.CodeOf).
const (
	CodeTooLarge         = "token_too_large"
	CodeBadDER           = "bad_der"
	CodeStructure        = "bad_structure"
	CodeUnsignedAttrs    = "unsigned_attrs"
	CodeAlgorithm        = "unsupported_algorithm"
	CodeNoSignerCert     = "no_signer_cert"
	CodeCertBinding      = "cert_binding"
	CodeTSTInfo          = "bad_tstinfo"
	CodeImprint          = "imprint_mismatch"
	CodeNonce            = "nonce_mismatch"
	CodePolicy           = "policy_mismatch"
	CodeSignedAttrs      = "bad_signed_attrs"
	CodeMessageDigest    = "message_digest_mismatch"
	CodeSignature        = "bad_signature"
	CodeEKU              = "bad_extended_key_usage"
	CodeChain            = "chain_not_trusted"
	CodeNoRoots          = "no_roots"
	CodeTime             = "time_skew"
	CodeChainNow         = "chain_not_trusted_now"
	CodeTSAName          = "tsa_name_mismatch"
	CodeKeyUsage         = "bad_key_usage"
	CodeSignerPin        = "signer_not_pinned"
	CodeEvidenceMismatch = "evidence_mismatch"
)

func fail(code, text string) *anchor.Error {
	return anchor.Errorf(anchor.ClassPermanent, anchor.KindTSA, "verify-token", code, "%s", text)
}

// failRefused is a failure that an operator or the authority can fix (the
// clock, the authority's certificate): retried after 1, 5 and 15 minutes,
// then failed with the alert.
func failRefused(code, text string) *anchor.Error {
	return anchor.Errorf(anchor.ClassRefused, anchor.KindTSA, "verify-token", code, "%s", text)
}

// Expect is what a token must match.
type Expect struct {
	// Digest is the SHA-256 digest the token must stamp.
	Digest [32]byte
	// Nonce, when not nil, must be the token's nonce; when nil the nonce is
	// not checked (a stored token, whose request is gone).
	Nonce *big.Int
	// Policy, when not empty, must be the token's policy (dotted OID).
	Policy string
	// Now, when not zero, is the clock of a Submit: genTime must be within
	// MaxSkew of it, in both directions (code "time_skew"), and the signing
	// certificate must also chain to the roots at Now, not only at genTime
	// (code "chain_not_trusted_now"), so that an authority whose certificate
	// has expired cannot hand out tokens dated inside its old validity
	// window. A stored token is checked with Now zero: its certificate is
	// judged at genTime alone.
	Now time.Time
	// MaxSkew is the tolerance for Now; zero means DefaultMaxSkew.
	MaxSkew time.Duration
	// SignerKeySHA256, when not nil (32 bytes), pins the signer: the SHA-256 of
	// the DER SubjectPublicKeyInfo of the signing certificate (see
	// SignerKeyHash) must equal it, else the token is refused (code
	// "signer_not_pinned"). It is what an operator who means one authority says;
	// without it any authority that chains to the configured roots passes.
	SignerKeySHA256 []byte
}

// SignerKeyHash is the pin of a signing certificate: the SHA-256 of the DER
// encoding of its SubjectPublicKeyInfo (the same value as the "sha256//" pin of
// HTTP public key pinning, and as
// `openssl x509 -pubkey -noout | openssl pkey -pubin -outform der | openssl dgst -sha256`).
// The public key is pinned and not the certificate because a time-stamp
// authority renews its certificate more often than its key; the pin of the
// certificate itself would have to be changed at every renewal, and a pin that
// is changed carelessly is no pin.
func SignerKeyHash(c *x509.Certificate) [32]byte { return sha256.Sum256(c.RawSubjectPublicKeyInfo) }

// TokenInfo is what a verified token says.
type TokenInfo struct {
	// GenTime is the time of the TSA, with the sub-seconds the token has.
	GenTime time.Time
	// Accuracy is the sum of the accuracy fields (0 when absent). It is
	// reported, not used.
	Accuracy time.Duration
	// Ordering is the ordering flag, reported, not used.
	Ordering bool
	Policy   string
	Serial   *big.Int
	Nonce    *big.Int // nil when the token has none
	// Signer is the certificate that signed the token.
	Signer *x509.Certificate
	// SignatureAlgorithm names the digest and key type, e.g. "ECDSA-SHA256".
	SignatureAlgorithm string
}

var hashByOID = map[string]crypto.Hash{
	oidSHA256: crypto.SHA256, oidSHA384: crypto.SHA384, oidSHA512: crypto.SHA512,
}

func sum(h crypto.Hash, data []byte) []byte {
	switch h {
	case crypto.SHA256:
		s := sha256.Sum256(data)
		return s[:]
	case crypto.SHA384:
		s := sha512.Sum384(data)
		return s[:]
	case crypto.SHA512:
		s := sha512.Sum512(data)
		return s[:]
	}
	return nil
}

// algID parses an AlgorithmIdentifier: the OID and whether parameters other
// than NULL are present.
func algID(t tlv) (oid string, extraParams bool, err error) {
	if t.tag != tagSequence {
		return "", false, errDER
	}
	ch, err := children(t.content)
	if err != nil || len(ch) < 1 || len(ch) > 2 {
		return "", false, errDER
	}
	oid, err = parseOID(ch[0])
	if err != nil {
		return "", false, err
	}
	if len(ch) == 2 && ch[1].tag != tagNull {
		extraParams = true
	}
	return oid, extraParams, nil
}

type signerInfo struct {
	sidIssuer, sidSerial []byte // issuerAndSerialNumber
	sidSKI               []byte
	digestOID            string
	attrsRaw             []byte // the [0] element as received
	sigOID               string
	signature            []byte
}

type parsedToken struct {
	eContent []byte
	certs    []*x509.Certificate
	si       signerInfo
}

func parseToken(der []byte) (*parsedToken, *anchor.Error) {
	bad := func(format string) *anchor.Error { return fail(CodeStructure, format) }
	if len(der) > anchor.MaxTokenDER {
		return nil, fail(CodeTooLarge, "token is larger than the limit")
	}
	top, err := oneTLV(der)
	if err != nil || checkDER(der, 0) != nil {
		return nil, fail(CodeBadDER, "token is not strict DER")
	}
	if top.tag != tagSequence {
		return nil, bad("token is not a ContentInfo")
	}
	ci, _ := children(top.content)
	if len(ci) != 2 || ci[1].tag != tagCtx0 {
		return nil, bad("token is not a ContentInfo")
	}
	if o, err := parseOID(ci[0]); err != nil || o != oidSignedData {
		return nil, bad("token content type is not SignedData")
	}
	inner, _ := children(ci[1].content)
	if len(inner) != 1 || inner[0].tag != tagSequence {
		return nil, bad("token has no SignedData")
	}
	sd, _ := children(inner[0].content)
	if len(sd) < 4 {
		return nil, bad("SignedData is too short")
	}
	if v, err := parseInt(sd[0]); err != nil || (v.Int64() != 1 && v.Int64() != 3) || !v.IsInt64() {
		return nil, bad("SignedData version is not 1 or 3")
	}
	if sd[1].tag != tagSet || sd[2].tag != tagSequence {
		return nil, bad("SignedData layout is wrong")
	}
	digAlgs, _ := children(sd[1].content)
	pt := &parsedToken{}
	// encapContentInfo
	enc, _ := children(sd[2].content)
	if len(enc) != 2 || enc[1].tag != tagCtx0 {
		return nil, bad("token carries no encapsulated content")
	}
	if o, err := parseOID(enc[0]); err != nil || o != oidTSTInfo {
		return nil, bad("encapsulated content type is not id-ct-TSTInfo")
	}
	oc, _ := children(enc[1].content)
	if len(oc) != 1 || oc[0].tag != tagOctet {
		return nil, bad("encapsulated content is not an OCTET STRING")
	}
	pt.eContent = oc[0].content
	i := 3
	for ; i < len(sd) && (sd[i].tag == tagCtx0 || sd[i].tag == tagCtx1); i++ {
		if sd[i].tag == tagCtx0 {
			cs, _ := children(sd[i].content)
			if len(cs) > MaxCertificates {
				return nil, bad("too many certificates")
			}
			for _, c := range cs {
				cert, err := x509.ParseCertificate(c.raw)
				if err != nil {
					return nil, bad("a certificate in the token does not parse")
				}
				pt.certs = append(pt.certs, cert)
			}
		}
	}
	if i != len(sd)-1 || sd[i].tag != tagSet {
		return nil, bad("SignedData layout is wrong")
	}
	sis, _ := children(sd[i].content)
	if len(sis) != 1 || sis[0].tag != tagSequence {
		return nil, bad("token must have exactly one SignerInfo")
	}
	f, _ := children(sis[0].content)
	k := 0
	if len(f) < 5 {
		return nil, bad("SignerInfo is too short")
	}
	if v, err := parseInt(f[k]); err != nil || !v.IsInt64() || (v.Int64() != 1 && v.Int64() != 3) {
		return nil, bad("SignerInfo version is not 1 or 3")
	}
	k++
	switch f[k].tag {
	case tagSequence:
		ias, _ := children(f[k].content)
		if len(ias) != 2 || ias[0].tag != tagSequence || ias[1].tag != tagInteger {
			return nil, bad("sid is malformed")
		}
		pt.si.sidIssuer, pt.si.sidSerial = ias[0].raw, ias[1].raw
	case tagCtx0Prim:
		pt.si.sidSKI = f[k].content
	default:
		return nil, bad("sid is malformed")
	}
	k++
	dOID, extra, err := algID(f[k])
	if err != nil || extra {
		return nil, bad("SignerInfo digest algorithm is malformed")
	}
	pt.si.digestOID = dOID
	k++
	if f[k].tag != tagCtx0 {
		return nil, fail(CodeSignedAttrs, "SignerInfo has no signed attributes")
	}
	pt.si.attrsRaw = f[k].raw
	k++
	if k >= len(f) {
		return nil, bad("SignerInfo is too short")
	}
	sOID, _, err := algID(f[k])
	if err != nil {
		return nil, bad("SignerInfo signature algorithm is malformed")
	}
	pt.si.sigOID = sOID
	k++
	if k >= len(f) || f[k].tag != tagOctet {
		return nil, bad("SignerInfo signature is missing")
	}
	pt.si.signature = f[k].content
	k++
	if k != len(f) {
		if f[k].tag == tagCtx1 {
			return nil, fail(CodeUnsignedAttrs, "SignerInfo carries unsigned attributes")
		}
		return nil, bad("SignerInfo has unexpected content")
	}
	found := false
	for _, d := range digAlgs {
		if o, _, err := algID(d); err == nil && o == pt.si.digestOID {
			found = true
		}
	}
	if !found {
		return nil, bad("SignerInfo digest algorithm is not in the SignedData digest algorithms")
	}
	return pt, nil
}

type tstInfo struct {
	policy   string
	hashOID  string
	imprint  []byte
	serial   *big.Int
	genTime  time.Time
	accuracy time.Duration
	ordering bool
	nonce    *big.Int
	tsaName  []byte // the GeneralName of the tsa field, nil when absent
}

func parseTSTInfo(b []byte) (*tstInfo, *anchor.Error) {
	bad := func(s string) *anchor.Error { return fail(CodeTSTInfo, s) }
	top, err := oneTLV(b)
	if err != nil || checkDER(b, 0) != nil {
		return nil, fail(CodeBadDER, "TSTInfo is not strict DER")
	}
	if top.tag != tagSequence {
		return nil, bad("TSTInfo is not a SEQUENCE")
	}
	f, _ := children(top.content)
	if len(f) < 5 {
		return nil, bad("TSTInfo is too short")
	}
	if v, err := parseInt(f[0]); err != nil || !v.IsInt64() || v.Int64() != 1 {
		return nil, bad("TSTInfo version is not 1")
	}
	t := &tstInfo{}
	if t.policy, err = parseOID(f[1]); err != nil {
		return nil, bad("TSTInfo policy is malformed")
	}
	if f[2].tag != tagSequence {
		return nil, bad("messageImprint is malformed")
	}
	mi, _ := children(f[2].content)
	if len(mi) != 2 || mi[1].tag != tagOctet {
		return nil, bad("messageImprint is malformed")
	}
	var extra bool
	if t.hashOID, extra, err = algID(mi[0]); err != nil || extra {
		return nil, bad("messageImprint algorithm is malformed")
	}
	t.imprint = mi[1].content
	if t.serial, err = parseInt(f[3]); err != nil || t.serial.Sign() < 0 {
		return nil, bad("serialNumber is malformed")
	}
	if t.genTime, err = parseGenTime(f[4]); err != nil {
		return nil, bad("genTime is malformed")
	}
	i := 5
	if i < len(f) && f[i].tag == tagSequence { // accuracy
		a, _ := children(f[i].content)
		j := 0
		var d time.Duration
		if j < len(a) && a[j].tag == tagInteger {
			v, err := parseInt(a[j])
			if err != nil || !v.IsInt64() || v.Sign() < 0 || v.Int64() > 1<<31 {
				return nil, bad("accuracy seconds are malformed")
			}
			d += time.Duration(v.Int64()) * time.Second
			j++
		}
		if j < len(a) && a[j].tag == tagCtx0Prim {
			n, ok := smallUint(a[j].content, 999)
			if !ok {
				return nil, bad("accuracy millis are malformed")
			}
			d += time.Duration(n) * time.Millisecond
			j++
		}
		if j < len(a) && a[j].tag == tagCtx1Prim {
			n, ok := smallUint(a[j].content, 999)
			if !ok {
				return nil, bad("accuracy micros are malformed")
			}
			d += time.Duration(n) * time.Microsecond
			j++
		}
		if j != len(a) {
			return nil, bad("accuracy is malformed")
		}
		t.accuracy = d
		i++
	}
	if i < len(f) && f[i].tag == tagBoolean {
		t.ordering = f[i].content[0] == 0xff
		if !t.ordering {
			return nil, bad("ordering FALSE must be omitted in DER")
		}
		i++
	}
	if i < len(f) && f[i].tag == tagInteger {
		if t.nonce, err = parseInt(f[i]); err != nil || t.nonce.Sign() < 0 {
			return nil, bad("nonce is malformed")
		}
		i++
	}
	if i < len(f) && f[i].tag == 0xa0 { // tsa [0] GeneralName: compared with the signer's names
		t.tsaName = f[i].content
		i++
	}
	if i < len(f) && f[i].tag == 0xa1 { // extensions: any critical one is refused
		exts, _ := children(f[i].content)
		for _, e := range exts {
			ef, err := children(e.content)
			if err != nil || len(ef) < 2 || e.tag != tagSequence {
				return nil, bad("TSTInfo extension is malformed")
			}
			if ef[1].tag == tagBoolean && ef[1].content[0] == 0xff {
				return nil, bad("TSTInfo has a critical extension")
			}
		}
		i++
	}
	if i != len(f) {
		return nil, bad("TSTInfo has unexpected content")
	}
	return t, nil
}

func smallUint(c []byte, max int) (int, bool) {
	if len(c) == 0 || len(c) > 2 {
		return 0, false
	}
	v := 0
	for _, b := range c {
		v = v<<8 | int(b)
	}
	if c[0]&0x80 != 0 || v > max {
		return 0, false
	}
	return v, true
}

type attrs struct {
	contentType, messageDigest string
	digest                     []byte
	essRaw                     []byte
	essV2                      bool
}

func parseSignedAttrs(raw []byte) (*attrs, *anchor.Error) {
	bad := func(s string) *anchor.Error { return fail(CodeSignedAttrs, s) }
	t, _, err := readTLV(raw)
	if err != nil {
		return nil, bad("signed attributes are malformed")
	}
	list, err := children(t.content)
	if err != nil || len(list) == 0 {
		return nil, bad("signed attributes are malformed")
	}
	a := &attrs{}
	var haveCT, haveMD, haveESS bool
	for _, at := range list {
		f, err := children(at.content)
		if at.tag != tagSequence || err != nil || len(f) != 2 || f[1].tag != tagSet {
			return nil, bad("an attribute is malformed")
		}
		oid, err := parseOID(f[0])
		if err != nil {
			return nil, bad("an attribute is malformed")
		}
		vals, _ := children(f[1].content)
		one := func() (tlv, bool) {
			if len(vals) != 1 {
				return tlv{}, false
			}
			return vals[0], true
		}
		switch oid {
		case oidContentType:
			v, ok := one()
			if !ok || haveCT {
				return nil, bad("contentType attribute is malformed or repeated")
			}
			if a.contentType, err = parseOID(v); err != nil {
				return nil, bad("contentType attribute is malformed")
			}
			haveCT = true
		case oidMessageDig:
			v, ok := one()
			if !ok || haveMD || v.tag != tagOctet {
				return nil, bad("messageDigest attribute is malformed or repeated")
			}
			a.digest = v.content
			haveMD = true
		case oidSigningCert, oidSigningCertV:
			v, ok := one()
			if !ok || haveESS || v.tag != tagSequence {
				return nil, bad("signingCertificate attribute is malformed or repeated")
			}
			a.essRaw, a.essV2, haveESS = v.content, oid == oidSigningCertV, true
		}
	}
	if !haveCT || !haveMD {
		return nil, bad("contentType and messageDigest signed attributes are required")
	}
	if !haveESS {
		return nil, fail(CodeCertBinding, "the signingCertificate or signingCertificateV2 signed attribute is required")
	}
	return a, nil
}

// checkESS verifies that the first ESSCertID of the attribute names cert.
func checkESS(a *attrs, cert *x509.Certificate) *anchor.Error {
	bad := func(s string) *anchor.Error { return fail(CodeCertBinding, s) }
	ids, err := children(a.essRaw)
	if err != nil || len(ids) < 1 || ids[0].tag != tagSequence {
		return bad("signingCertificate attribute is malformed")
	}
	certs, err := children(ids[0].content)
	if err != nil || len(certs) < 1 || certs[0].tag != tagSequence {
		return bad("signingCertificate attribute is malformed")
	}
	f, err := children(certs[0].content)
	if err != nil || len(f) == 0 {
		return bad("ESSCertID is malformed")
	}
	var hashOID string = oidSHA1
	if a.essV2 {
		hashOID = oidSHA256
		if f[0].tag == tagSequence {
			var extra bool
			if hashOID, extra, err = algID(f[0]); err != nil || extra {
				return bad("ESSCertIDv2 hash algorithm is malformed")
			}
			f = f[1:]
		}
	}
	if len(f) < 1 || f[0].tag != tagOctet {
		return bad("ESSCertID is malformed")
	}
	var want []byte
	if hashOID == oidSHA1 && !a.essV2 {
		s := sha1.Sum(cert.Raw)
		want = s[:]
	} else if h, ok := hashByOID[hashOID]; ok {
		want = sum(h, cert.Raw)
	} else {
		return bad("ESSCertIDv2 hash algorithm is not supported")
	}
	if subtle.ConstantTimeCompare(want, f[0].content) != 1 {
		return bad("the signing certificate does not match the ESSCertID hash")
	}
	if len(f) > 1 { // issuerSerial
		if f[1].tag != tagSequence {
			return bad("ESSCertID issuerSerial is malformed")
		}
		is, err := children(f[1].content)
		if err != nil || len(is) != 2 || is[1].tag != tagInteger {
			return bad("ESSCertID issuerSerial is malformed")
		}
		n, err := parseInt(is[1])
		if err != nil || n.Cmp(cert.SerialNumber) != 0 {
			return bad("ESSCertID serial number does not match the certificate")
		}
		// the issuer: GeneralNames with a directoryName that must be the
		// certificate's issuer (a GeneralName of another kind is not compared)
		if is[0].tag != tagSequence {
			return bad("ESSCertID issuerSerial is malformed")
		}
		names, err := children(is[0].content)
		if err != nil {
			return bad("ESSCertID issuerSerial is malformed")
		}
		for _, g := range names {
			if g.tag == 0xa4 && !bytes.Equal(g.content, cert.RawIssuer) {
				return bad("ESSCertID issuer does not match the certificate")
			}
		}
	}
	return nil
}

type sigAlg struct {
	name string
	hash crypto.Hash
	rsa  bool
}

func resolveSigAlg(si signerInfo) (sigAlg, *anchor.Error) {
	dh, ok := hashByOID[si.digestOID]
	if !ok {
		return sigAlg{}, fail(CodeAlgorithm, "digest algorithm is not SHA-256, SHA-384 or SHA-512")
	}
	var h crypto.Hash
	var isRSA bool
	switch si.sigOID {
	case oidRSA:
		isRSA = true
	case oidRSASHA256:
		isRSA, h = true, crypto.SHA256
	case oidRSASHA384:
		isRSA, h = true, crypto.SHA384
	case oidRSASHA512:
		isRSA, h = true, crypto.SHA512
	case oidECPublicKey:
	case oidECDSASHA256:
		h = crypto.SHA256
	case oidECDSASHA384:
		h = crypto.SHA384
	case oidECDSASHA512:
		h = crypto.SHA512
	default:
		return sigAlg{}, fail(CodeAlgorithm, "signature algorithm is not RSA PKCS#1 v1.5 or ECDSA")
	}
	if h != 0 && h != dh {
		return sigAlg{}, fail(CodeAlgorithm, "signature algorithm and digest algorithm disagree")
	}
	name := "ECDSA-"
	if isRSA {
		name = "RSA-"
	}
	return sigAlg{name: name + dh.String(), hash: dh, rsa: isRSA}, nil
}

func findSigner(pt *parsedToken) *x509.Certificate {
	for _, c := range pt.certs {
		if pt.si.sidSKI != nil {
			if len(c.SubjectKeyId) > 0 && bytes.Equal(c.SubjectKeyId, pt.si.sidSKI) {
				return c
			}
			continue
		}
		serial, _ := asn1.Marshal(c.SerialNumber)
		if bytes.Equal(c.RawIssuer, pt.si.sidIssuer) && bytes.Equal(serial, pt.si.sidSerial) {
			return c
		}
	}
	return nil
}

func checkEKU(c *x509.Certificate) *anchor.Error {
	var ext *bool
	for _, e := range c.Extensions {
		if e.Id.String() == oidEKU {
			cr := e.Critical
			ext = &cr
		}
	}
	if ext == nil {
		return fail(CodeEKU, "signing certificate has no extended key usage")
	}
	if !*ext {
		return fail(CodeEKU, "extended key usage of the signing certificate is not critical")
	}
	if len(c.ExtKeyUsage) != 1 || c.ExtKeyUsage[0] != x509.ExtKeyUsageTimeStamping || len(c.UnknownExtKeyUsage) != 0 {
		return fail(CodeEKU, "extended key usage of the signing certificate is not only id-kp-timeStamping")
	}
	return nil
}

// VerifyToken verifies a TimeStampToken (the DER of the ContentInfo) in full,
// as design 6.4 step 4 says, and returns what it states. The token is
// accepted only if: it is strict DER (see doc.go); it is SignedData with
// content type id-ct-TSTInfo and exactly one SignerInfo with signed
// attributes (contentType, messageDigest, signingCertificate or
// signingCertificateV2) and no unsigned attributes; the TSTInfo has version
// 1, a SHA-256 imprint equal to e.Digest, the nonce and policy of e when
// given; the message digest attribute equals the digest of the content; the
// signature (RSA PKCS#1 v1.5 or ECDSA, SHA-256/384/512) verifies over the
// signed attributes as received; the signing certificate is the one the sid
// and the ESSCertID name; its extended key usage is critical and only
// id-kp-timeStamping; and it chains to roots (with intermediates, and the
// certificates of the token) with validity checked at genTime.
func VerifyToken(tokenDER []byte, e Expect, roots, intermediates *x509.CertPool) (*TokenInfo, error) {
	pt, aerr := parseToken(tokenDER)
	if aerr != nil {
		return nil, aerr
	}
	alg, aerr := resolveSigAlg(pt.si)
	if aerr != nil {
		return nil, aerr
	}
	signer := findSigner(pt)
	if signer == nil {
		return nil, fail(CodeNoSignerCert, "the token carries no certificate named by its SignerInfo")
	}
	if e.SignerKeySHA256 != nil {
		if h := SignerKeyHash(signer); subtle.ConstantTimeCompare(h[:], e.SignerKeySHA256) != 1 {
			return nil, fail(CodeSignerPin, "the signing certificate is not the one that the configuration pins (the SHA-256 of its public key differs from signerKeySha256)")
		}
	}
	at, aerr := parseSignedAttrs(pt.si.attrsRaw)
	if aerr != nil {
		return nil, aerr
	}
	if at.contentType != oidTSTInfo {
		return nil, fail(CodeSignedAttrs, "contentType signed attribute is not id-ct-TSTInfo")
	}
	if aerr := checkESS(at, signer); aerr != nil {
		return nil, aerr
	}
	info, aerr := parseTSTInfo(pt.eContent)
	if aerr != nil {
		return nil, aerr
	}
	if info.hashOID != oidSHA256 {
		return nil, fail(CodeImprint, "the token stamps a hash algorithm other than SHA-256")
	}
	if subtle.ConstantTimeCompare(info.imprint, e.Digest[:]) != 1 {
		return nil, fail(CodeImprint, "the message imprint of the token is not the digest that was submitted")
	}
	if e.Nonce != nil && (info.nonce == nil || info.nonce.Cmp(e.Nonce) != 0) {
		return nil, fail(CodeNonce, "the nonce of the token is not the nonce of the request")
	}
	if e.Policy != "" && info.policy != e.Policy {
		return nil, fail(CodePolicy, "the policy of the token is not the requested policy")
	}
	if subtle.ConstantTimeCompare(at.digest, sum(alg.hash, pt.eContent)) != 1 {
		return nil, fail(CodeMessageDigest, "the messageDigest attribute is not the digest of the TSTInfo")
	}
	if info.tsaName != nil && !tsaNameMatches(info.tsaName, signer) {
		return nil, fail(CodeTSAName, "the tsa name of the TSTInfo is not a name of the signing certificate")
	}
	signed := append([]byte(nil), pt.si.attrsRaw...)
	signed[0] = tagSet
	hs := sum(alg.hash, signed)
	switch pk := signer.PublicKey.(type) {
	case *rsa.PublicKey:
		if pk.N.BitLen() < MinRSABits {
			return nil, fail(CodeAlgorithm, "the signing certificate has an RSA key shorter than 2048 bits")
		}
		if !alg.rsa || rsa.VerifyPKCS1v15(pk, alg.hash, hs, pt.si.signature) != nil {
			return nil, fail(CodeSignature, "the signature does not verify under the signing certificate")
		}
	case *ecdsa.PublicKey:
		if alg.rsa || !ecdsa.VerifyASN1(pk, hs, pt.si.signature) {
			return nil, fail(CodeSignature, "the signature does not verify under the signing certificate")
		}
	default:
		return nil, fail(CodeAlgorithm, "the signing certificate key is not RSA or ECDSA")
	}
	if aerr := checkEKU(signer); aerr != nil {
		return nil, aerr
	}
	if signer.KeyUsage != 0 && signer.KeyUsage&(x509.KeyUsageDigitalSignature|x509.KeyUsageContentCommitment) == 0 {
		return nil, fail(CodeKeyUsage, "the key usage of the signing certificate allows neither digitalSignature nor contentCommitment")
	}
	if !e.Now.IsZero() {
		max := e.MaxSkew
		if max <= 0 {
			max = DefaultMaxSkew
		}
		if d := SkewOf(info.genTime, e.Now); d > max || d < -max {
			return nil, failRefused(CodeTime, fmt.Sprintf("genTime differs from the clock by %s, more than the tolerance of %s: a problem of clocks (the authority's or this server's), not of credentials; correct the clock, then Retry", d.Round(time.Second), max))
		}
	}
	if roots == nil {
		return nil, fail(CodeNoRoots, "no trust anchors are configured")
	}
	inter := x509.NewCertPool()
	if intermediates != nil {
		inter = intermediates.Clone()
	}
	for _, c := range pt.certs {
		if c != signer {
			inter.AddCert(c)
		}
	}
	opts := x509.VerifyOptions{Roots: roots, Intermediates: inter, CurrentTime: info.genTime,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageTimeStamping}}
	if _, err := signer.Verify(opts); err != nil {
		return nil, fail(CodeChain, "the signing certificate does not chain to a trusted root at genTime: "+shortErr(err))
	}
	if !e.Now.IsZero() {
		opts.CurrentTime = e.Now
		if _, err := signer.Verify(opts); err != nil {
			return nil, failRefused(CodeChainNow, "the signing certificate does not chain to a trusted root at the clock (an expired or not yet valid certificate of the authority, or a clock that is wrong): not a problem of credentials; renew the roots and intermediates of the CA bundle or ask the authority, or correct the clock, then Retry: "+shortErr(err))
		}
	}
	return &TokenInfo{GenTime: info.genTime, Accuracy: info.accuracy, Ordering: info.ordering, Policy: info.policy,
		Serial: info.serial, Nonce: info.nonce, Signer: signer, SignatureAlgorithm: alg.name}, nil
}

func shortErr(err error) string {
	s := err.Error()
	if len(s) > 120 {
		s = s[:120]
	}
	b := []byte(s)
	for i, c := range b {
		if c < 0x20 || c > 0x7e {
			b[i] = '?'
		}
	}
	return string(b)
}

// VerifyStoredToken verifies a token kept as evidence against the digest it
// must stamp; the nonce is not checked.
func VerifyStoredToken(tokenDER []byte, digest [32]byte, roots, intermediates *x509.CertPool) (*TokenInfo, error) {
	return VerifyToken(tokenDER, Expect{Digest: digest}, roots, intermediates)
}

// VerifyEvidence verifies TSA evidence (anchor.TSAEvidence) against the
// digest: the token as VerifyStoredToken, then that gen_time, serial and
// policy of the evidence are those of the token.
func VerifyEvidence(ev anchor.TSAEvidence, digest [32]byte, roots, intermediates *x509.CertPool) (*TokenInfo, error) {
	if err := ev.Validate(); err != nil {
		return nil, fail(CodeEvidenceMismatch, "evidence is malformed")
	}
	der, _ := base64.StdEncoding.DecodeString(ev.TokenDERB64)
	info, err := VerifyStoredToken(der, digest, roots, intermediates)
	if err != nil {
		return nil, err
	}
	if anchor.FormatTime(info.GenTime) != ev.GenTime || info.Serial.String() != ev.Serial || info.Policy != ev.Policy {
		return nil, fail(CodeEvidenceMismatch, "gen_time, serial or policy of the evidence differ from the token")
	}
	return info, nil
}

// EvidenceFromToken builds the evidence of a verified token. url is the
// address of the TSA (without credentials).
func EvidenceFromToken(url string, info *TokenInfo, tokenDER []byte) anchor.TSAEvidence {
	return anchor.TSAEvidence{
		TSAURL:      url,
		TokenDERB64: base64.StdEncoding.EncodeToString(tokenDER),
		GenTime:     anchor.FormatTime(info.GenTime),
		Serial:      info.Serial.String(),
		Policy:      info.Policy,
		RecordedBy:  anchor.RecordedByCluster,
	}
}

// SkewOf returns genTime minus now (positive: the TSA is ahead).
func SkewOf(genTime, now time.Time) time.Duration { return genTime.Sub(now) }

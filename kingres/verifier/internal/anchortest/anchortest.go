// SPDX-License-Identifier: AGPL-3.0-only

// Package anchortest builds anchor evidence for the tests of the verifier: RFC 3161 tokens made by a local test authority,
// OpenTimestamps fragments written byte by byte, and evidence files in the formats of the publisher's anchor library
// (doc.go of kingres/control-plane/internal/anchor, "Evidence: the formats"). It is written from those formats and from the
// RFCs; the verifier module cannot import the control plane (separate modules, and no dependency may be added), so the
// agreement of the two sides is held by the documented bytes. Nothing but tests imports it.
package anchortest

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"testing"
	"time"
)

var (
	oidTST    = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 16, 1, 4}
	oidSD     = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 2}
	oidCT     = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 3}
	oidMD     = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 4}
	oidESS2   = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 16, 2, 47}
	oidSHA256 = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}
	oidECDSA  = asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 3, 2}
	oidRSA256 = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 11}
	oidEKU    = asn1.ObjectIdentifier{2, 5, 29, 37}
	oidTS     = asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 8}
	oidSrv    = asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 1}
)

// TSAOpts says how a token is made, and how it is made wrong.
type TSAOpts struct {
	Digest         [32]byte
	GenTime        time.Time // zero: 2026-11-19T11:00:00Z
	RSA            bool
	NotCriticalEKU bool
	ExtraEKU       bool
	NoEKU          bool
	BadSignature   bool
	WrongDigest    bool // the message-digest attribute is not that of the TSTInfo
	NoESS          bool
	Imprint        *[32]byte // another imprint than Digest
	WithName       bool      // the tsa field names the subject of the certificate
	WrongName      bool
	CertNotBefore  time.Time // zero: a year before GenTime
	CertNotAfter   time.Time // zero: a year after GenTime
	TwoSigners     bool
	WrongESS       bool // the signing-certificate attribute names another certificate
	Nonce          bool
}

// TSA is a made token with what an auditor pins.
type TSA struct {
	Token   []byte
	RootPEM string
	KeySHA  [32]byte
	Serial  string
	Policy  string
	GenTime time.Time
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func marshal(t testing.TB, v any, params string) []byte {
	t.Helper()
	b, err := asn1.MarshalWithParams(v, params)
	must(t, err)
	return b
}

func raw(tag int, class int, compound bool, content []byte) []byte {
	b, _ := asn1.Marshal(asn1.RawValue{Class: class, Tag: tag, IsCompound: compound, Bytes: content})
	return b
}

func seq(parts ...[]byte) []byte {
	var c []byte
	for _, p := range parts {
		c = append(c, p...)
	}
	return raw(asn1.TagSequence, asn1.ClassUniversal, true, c)
}

func set(parts ...[]byte) []byte {
	var c []byte
	for _, p := range parts {
		c = append(c, p...)
	}
	return raw(asn1.TagSet, asn1.ClassUniversal, true, c)
}

func oid(t testing.TB, o asn1.ObjectIdentifier) []byte { return marshal(t, o, "") }

func octets(b []byte) []byte { return raw(asn1.TagOctetString, asn1.ClassUniversal, false, b) }

func ctx(tag int, compound bool, content []byte) []byte {
	return raw(tag, asn1.ClassContextSpecific, compound, content)
}

func algSHA256(t testing.TB) []byte {
	return seq(oid(t, oidSHA256), raw(asn1.TagNull, asn1.ClassUniversal, false, nil))
}

// NewTSA makes a test authority and a token.
func NewTSA(t testing.TB, o TSAOpts) *TSA {
	t.Helper()
	gen := o.GenTime
	if gen.IsZero() {
		gen = time.Date(2026, 11, 19, 11, 0, 0, 0, time.UTC)
	}
	nb, na := o.CertNotBefore, o.CertNotAfter
	if nb.IsZero() {
		nb = gen.AddDate(-1, 0, 0)
	}
	if na.IsZero() {
		na = gen.AddDate(1, 0, 0)
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Test TSA Root"}, NotBefore: gen.AddDate(-5, 0, 0), NotAfter: gen.AddDate(20, 0, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	must(t, err)
	ca, err := x509.ParseCertificate(caDER)
	must(t, err)
	var signer crypto.Signer
	var pub any
	if o.RSA {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		must(t, err)
		signer, pub = k, &k.PublicKey
	} else {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		must(t, err)
		signer, pub = k, &k.PublicKey
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(77), Subject: pkix.Name{CommonName: "Test Time Stamping Unit", Organization: []string{"Test"}}, NotBefore: nb, NotAfter: na, KeyUsage: x509.KeyUsageDigitalSignature}
	if !o.NoEKU {
		purposes := oid(t, oidTS)
		if o.ExtraEKU {
			purposes = append(purposes, oid(t, oidSrv)...)
		}
		tmpl.ExtraExtensions = []pkix.Extension{{Id: oidEKU, Critical: !o.NotCriticalEKU, Value: seq(purposes)}}
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, tmpl, ca, pub, caKey)
	must(t, err)
	leaf, err := x509.ParseCertificate(leafDER)
	must(t, err)

	imprint := o.Digest
	if o.Imprint != nil {
		imprint = *o.Imprint
	}
	serial := big.NewInt(424242)
	genText := gen.UTC().Format("20060102150405") + "Z"
	tst := [][]byte{
		marshal(t, 1, ""),
		oid(t, asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 99999, 1}),
		seq(algSHA256(t), octets(imprint[:])),
		marshal(t, serial, ""),
		raw(asn1.TagGeneralizedTime, asn1.ClassUniversal, false, []byte(genText)),
	}
	if o.Nonce {
		tst = append(tst, marshal(t, big.NewInt(99), ""))
	}
	if o.WithName || o.WrongName {
		name := leaf.RawSubject
		if o.WrongName {
			nm, _ := asn1.Marshal(pkix.Name{CommonName: "Somebody Else"}.ToRDNSequence())
			name = nm
		}
		tst = append(tst, ctx(0, true, ctx(4, true, name)))
	}
	tstDER := seq(tst...)

	mdSum := sha256.Sum256(tstDER)
	md := mdSum[:]
	if o.WrongDigest {
		md = append([]byte(nil), md...)
		md[0] ^= 1
	}
	certHash := sha256.Sum256(leafDER)
	if o.WrongESS {
		certHash = sha256.Sum256(caDER)
	}
	attrs := [][]byte{
		seq(oid(t, oidCT), set(oid(t, oidTST))),
		seq(oid(t, oidMD), set(octets(md))),
	}
	if !o.NoESS {
		attrs = append(attrs, seq(oid(t, oidESS2), set(seq(seq(seq(octets(certHash[:])))))))
	}
	attrBody := []byte{}
	for _, a := range attrs {
		attrBody = append(attrBody, a...)
	}
	toSign := set(attrs...)
	sum := sha256.Sum256(toSign)
	var sig []byte
	var sigAlg []byte
	if o.RSA {
		sig, err = rsa.SignPKCS1v15(rand.Reader, signer.(*rsa.PrivateKey), crypto.SHA256, sum[:])
		sigAlg = seq(oid(t, oidRSA256), raw(asn1.TagNull, asn1.ClassUniversal, false, nil))
	} else {
		sig, err = ecdsa.SignASN1(rand.Reader, signer.(*ecdsa.PrivateKey), sum[:])
		sigAlg = seq(oid(t, oidECDSA))
	}
	must(t, err)
	if o.BadSignature {
		sig[len(sig)-1] ^= 1
	}
	signerInfo := func() []byte {
		return seq(marshal(t, 1, ""), seq(leaf.RawIssuer, marshal(t, leaf.SerialNumber, "")), algSHA256(t),
			ctx(0, true, attrBody), sigAlg, octets(sig))
	}
	sis := signerInfo()
	if o.TwoSigners {
		sis = append(sis, signerInfo()...)
	}
	certs := append(append([]byte{}, leafDER...), caDER...)
	sd := seq(marshal(t, 3, ""), set(algSHA256(t)), seq(oid(t, oidTST), ctx(0, true, octets(tstDER))), ctx(0, true, certs), raw(asn1.TagSet, asn1.ClassUniversal, true, sis))
	token := seq(oid(t, oidSD), ctx(0, true, sd))
	root := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	return &TSA{Token: token, RootPEM: string(root), KeySHA: sha256.Sum256(leaf.RawSubjectPublicKeyInfo), Serial: serial.String(), Policy: "1.3.6.1.4.1.99999.1", GenTime: gen}
}

// ---- OpenTimestamps ----

// Varuint writes a little-endian base 128 number.
func Varuint(n uint64) []byte {
	var b []byte
	for {
		c := byte(n & 0x7f)
		n >>= 7
		if n == 0 {
			return append(b, c)
		}
		b = append(b, c|0x80)
	}
}

func varbytes(b []byte) []byte { return append(Varuint(uint64(len(b))), b...) }

// BitcoinAttestation is the bytes of an attestation item at a height.
func BitcoinAttestation(height uint64) []byte {
	return append([]byte{0x00, 0x05, 0x88, 0x96, 0x0d, 0x73, 0xd7, 0x19, 0x01}, varbytes(Varuint(height))...)
}

// PendingAttestation is the bytes of a pending attestation item.
func PendingAttestation(uri string) []byte {
	return append([]byte{0x00, 0x83, 0xdf, 0xe3, 0x0d, 0x2e, 0xf9, 0x0c, 0x8e}, varbytes(varbytes([]byte(uri)))...)
}

// Fragment makes the path a calendar returns for a Bitcoin block: prepend(nonce), sha256, append(sibling), sha256, then the attestation.
// It returns the fragment and the merkle root it reaches from the digest.
func Fragment(digest [32]byte, nonce, sibling []byte, att []byte) ([]byte, [32]byte) {
	m := append(append([]byte{}, nonce...), digest[:]...)
	a := sha256.Sum256(m)
	b := sha256.Sum256(append(append([]byte{}, a[:]...), sibling...))
	c := sha256.Sum256(b[:])
	var f []byte
	f = append(f, 0xf1)
	f = append(f, varbytes(nonce)...)
	f = append(f, 0x08, 0xf0)
	f = append(f, varbytes(sibling)...)
	f = append(f, 0x08, 0x08)
	f = append(f, att...)
	_ = c
	return f, c
}

// Header builds an 80-byte header with a merkle root, a time and bits, with a nonce that makes the double SHA-256 meet
// the target of bits (bits must be very easy, as 0x207fffff, so that a nonce is found at once).
func Header(root [32]byte, tm time.Time, bits uint32) []byte {
	h := make([]byte, 80)
	h[0] = 1
	copy(h[36:68], root[:])
	put32(h[68:], uint32(tm.Unix()))
	put32(h[72:], bits)
	for nonce := uint32(0); ; nonce++ {
		put32(h[76:], nonce)
		a := sha256.Sum256(h)
		b := sha256.Sum256(a[:])
		var be [32]byte
		for i := range b {
			be[i] = b[31-i]
		}
		if be[0] < 0x7f { // meets the target 0x7fffff << 232 of bits 0x207fffff
			return h
		}
		if nonce > 1<<20 {
			panic("no nonce")
		}
	}
}

func put32(b []byte, v uint32) {
	b[0], b[1], b[2], b[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
}

// ---- evidence files ----

func tz(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

func file(kind, origin string, size uint64, digest [32]byte, evidence string) []byte {
	return []byte(fmt.Sprintf(`{"format":1,"kind":%q,"origin":%q,"size":%d,"payload_sha256":%q,"created_at":%q,"evidence":%s}`, kind, origin, size, hex.EncodeToString(digest[:]), "2026-11-19T11:05:00Z", evidence))
}

// TSAFile is the evidence file of a TSA anchor.
func TSAFile(origin string, size uint64, digest [32]byte, a *TSA) []byte {
	ev := fmt.Sprintf(`{"tsa_url":"https://tsa.example.com/","token_der_b64":%q,"gen_time":%q,"serial":%q,"policy":%q,"signer_key_sha256":%q,"recorded_by":"cluster"}`,
		base64.StdEncoding.EncodeToString(a.Token), tz(a.GenTime), a.Serial, a.Policy, hex.EncodeToString(a.KeySHA[:]))
	return file("tsa", origin, size, digest, ev)
}

// WORMEvidence is what a test says about the object.
type WORMEvidence struct {
	Key, VersionID string
	Body           []byte
	LockMode       string
	RetainUntil    time.Time
	LastModified   time.Time
	Generation     int
}

// WORMFile is the evidence file of a WORM anchor.
func WORMFile(origin string, size uint64, digest [32]byte, w WORMEvidence) []byte {
	sum := sha256.Sum256(w.Body)
	ev := fmt.Sprintf(`{"endpoint":"","bucket":"anchors","key":%q,"version_id":%q,"etag":"\"abc\"","object_sha256":%q,"content_length":%d,"lock_mode":%q,"retain_until":%q,"last_modified":%q,"submit_start":%q,"attempt":%q,"generation":%d,"found":false,"recorded_by":"cluster"}`,
		w.Key, w.VersionID, hex.EncodeToString(sum[:]), len(w.Body), w.LockMode, tz(w.RetainUntil), tz(w.LastModified), tz(w.LastModified.Add(-time.Second)), tz(w.LastModified.Add(-time.Second)), w.Generation)
	return file("worm", origin, size, digest, ev)
}

// OTSFile is the evidence file of an OpenTimestamps anchor with the given fragments (one per calendar) and an optional claim.
func OTSFile(origin string, size uint64, digest [32]byte, fragments [][]byte, claimHeight uint64, claimRoot *[32]byte) []byte {
	cals := ""
	for i, f := range fragments {
		if i > 0 {
			cals += ","
		}
		cals += fmt.Sprintf(`{"uri":"https://cal%d.example.com","fragment_b64":%q}`, i, base64.StdEncoding.EncodeToString(f))
	}
	btc := "null"
	if claimRoot != nil {
		btc = fmt.Sprintf(`{"height":%d,"calendar_uri":"https://cal0.example.com","merkle_root":%q,"status":"claimed","header":""}`, claimHeight, hex.EncodeToString(claimRoot[:]))
	}
	ev := fmt.Sprintf(`{"digest":%q,"submitted_at":"2026-11-19T10:00:00Z","calendars":[%s],"bitcoin":%s,"contradicted":[],"dropped":[]}`, hex.EncodeToString(digest[:]), cals, btc)
	return file("ots", origin, size, digest, ev)
}

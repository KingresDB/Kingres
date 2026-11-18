// SPDX-License-Identifier: AGPL-3.0-only

package tsa

// A test time-stamp authority built from nothing: a root, an intermediate and
// a TSA certificate, and an issuer of tokens that can be told to produce each
// kind of wrong token. It shares no code with the verifier under test: all DER
// is built here with its own small encoder.

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func tl(tag byte, content []byte) []byte {
	n := len(content)
	var hdr []byte
	switch {
	case n < 0x80:
		hdr = []byte{tag, byte(n)}
	case n < 0x100:
		hdr = []byte{tag, 0x81, byte(n)}
	case n < 0x10000:
		hdr = []byte{tag, 0x82, byte(n >> 8), byte(n)}
	default:
		hdr = []byte{tag, 0x83, byte(n >> 16), byte(n >> 8), byte(n)}
	}
	return append(hdr, content...)
}

func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
func seq(parts ...[]byte) []byte { return tl(0x30, cat(parts...)) }
func set(parts ...[]byte) []byte { return tl(0x31, cat(parts...)) }
func octet(b []byte) []byte      { return tl(0x04, b) }

func oidB(s string) []byte {
	var o asn1.ObjectIdentifier
	for _, p := range strings.Split(s, ".") {
		n, _ := strconv.Atoi(p)
		o = append(o, n)
	}
	b, err := asn1.Marshal(o)
	if err != nil {
		panic(err)
	}
	return b
}

func intB(n *big.Int) []byte {
	b, err := asn1.Marshal(n)
	if err != nil {
		panic(err)
	}
	return b
}
func intI(n int64) []byte { return intB(big.NewInt(n)) }

func algB(oid string) []byte { return seq(oidB(oid), []byte{5, 0}) }

func hashBytes(alg string, data []byte) []byte {
	switch alg {
	case "sha1":
		s := sha1.Sum(data)
		return s[:]
	case "sha256":
		s := sha256.Sum256(data)
		return s[:]
	case "sha384":
		s := sha512.Sum384(data)
		return s[:]
	case "sha512":
		s := sha512.Sum512(data)
		return s[:]
	}
	panic("alg " + alg)
}

var hashOIDs = map[string]string{
	"sha1": "1.3.14.3.2.26", "sha256": "2.16.840.1.101.3.4.2.1",
	"sha384": "2.16.840.1.101.3.4.2.2", "sha512": "2.16.840.1.101.3.4.2.3",
}
var cryptoHash = map[string]crypto.Hash{"sha1": crypto.SHA1, "sha256": crypto.SHA256, "sha384": crypto.SHA384, "sha512": crypto.SHA512}

// ---- keys and certificates ----

var (
	rsaKeyOnce sync.Once
	rsaKeys    [3]*rsa.PrivateKey
)

func rsaKey(i int) *rsa.PrivateKey {
	rsaKeyOnce.Do(func() {
		for j := range rsaKeys {
			k, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				panic(err)
			}
			rsaKeys[j] = k
		}
	})
	return rsaKeys[i]
}

func newKey(alg string, i int) crypto.Signer {
	switch alg {
	case "rsa":
		return rsaKey(i)
	case "p256fixed": // fixed keys: the fuzz seeds must verify in every worker process
		d := bytes.Repeat([]byte{byte(0x31 + i)}, 32)
		k, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), d)
		if err != nil {
			panic(err)
		}
		return k
	case "p256":
		k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		return k
	case "p384":
		k, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
		return k
	}
	panic(alg)
}

type ekuMode int

const (
	ekuCritical ekuMode = iota
	ekuNonCritical
	ekuNone
	ekuExtraCritical
)

type authOpts struct {
	keyAlg    string // "p256" (default), "p384", "rsa"
	eku       ekuMode
	notBefore time.Time // of the TSA certificate; zero: an hour ago
	notAfter  time.Time // zero: ten years
	ski       bool
	// tsaKeyBits, when not 0, gives the TSA certificate a fresh RSA key of
	// that size (the signature algorithm of the token follows); the chain
	// above it keeps the keyAlg
	tsaKeyBits int
	// tsaSAN adds a subject alternative name (a DNS name) to the TSA certificate
	tsaSAN string
	// tsaKeyUsage replaces the digitalSignature key usage of the TSA certificate
	tsaKeyUsage x509.KeyUsage
}

type authority struct {
	rootKey, interKey, tsaKey crypto.Signer
	root, inter, tsa          *x509.Certificate
	rootPool, interPool       *x509.CertPool
	keyAlg                    string
}

var serialCounter int64 = 100

func nextSerial() *big.Int { serialCounter++; return big.NewInt(serialCounter) }

func newAuthority(t testing.TB, o authOpts) *authority {
	t.Helper()
	if o.keyAlg == "" {
		o.keyAlg = "p256"
	}
	now := time.Now().UTC()
	a := &authority{keyAlg: o.keyAlg}
	a.rootKey, a.interKey, a.tsaKey = newKey(o.keyAlg, 0), newKey(o.keyAlg, 1), newKey(o.keyAlg, 2)
	if o.tsaKeyBits != 0 {
		k, err := rsa.GenerateKey(rand.Reader, o.tsaKeyBits)
		if err != nil {
			t.Fatal(err)
		}
		a.tsaKey = k
	}
	mk := func(tmpl, parent *x509.Certificate, pub crypto.PublicKey, signer crypto.Signer) *x509.Certificate {
		der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, signer)
		if err != nil {
			t.Fatal(err)
		}
		c, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	rootT := &x509.Certificate{SerialNumber: nextSerial(), Subject: pkix.Name{CommonName: "Test Root"},
		NotBefore: now.AddDate(-5, 0, 0), NotAfter: now.AddDate(20, 0, 0), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	a.root = mk(rootT, rootT, a.rootKey.Public(), a.rootKey)
	interT := &x509.Certificate{SerialNumber: nextSerial(), Subject: pkix.Name{CommonName: "Test Intermediate"},
		NotBefore: now.AddDate(-4, 0, 0), NotAfter: now.AddDate(15, 0, 0), IsCA: true, BasicConstraintsValid: true, MaxPathLen: 0, MaxPathLenZero: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	a.inter = mk(interT, a.root, a.interKey.Public(), a.rootKey)
	nb, na := o.notBefore, o.notAfter
	if nb.IsZero() {
		nb = now.Add(-time.Hour)
	}
	if na.IsZero() {
		na = now.AddDate(10, 0, 0)
	}
	tsaT := &x509.Certificate{SerialNumber: nextSerial(), Subject: pkix.Name{CommonName: "Test TSA", Organization: []string{"Test"}},
		NotBefore: nb, NotAfter: na, KeyUsage: x509.KeyUsageDigitalSignature}
	if o.tsaKeyUsage != 0 {
		tsaT.KeyUsage = o.tsaKeyUsage
	}
	if o.tsaSAN != "" {
		tsaT.DNSNames = []string{o.tsaSAN}
	}
	if o.ski {
		tsaT.SubjectKeyId = []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}
	}
	ts := []asn1.ObjectIdentifier{{1, 3, 6, 1, 5, 5, 7, 3, 8}}
	switch o.eku {
	case ekuCritical:
		v, _ := asn1.Marshal(ts)
		tsaT.ExtraExtensions = []pkix.Extension{{Id: asn1.ObjectIdentifier{2, 5, 29, 37}, Critical: true, Value: v}}
	case ekuNonCritical:
		tsaT.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageTimeStamping}
	case ekuExtraCritical:
		v, _ := asn1.Marshal([]asn1.ObjectIdentifier{{1, 3, 6, 1, 5, 5, 7, 3, 8}, {1, 3, 6, 1, 5, 5, 7, 3, 1}})
		tsaT.ExtraExtensions = []pkix.Extension{{Id: asn1.ObjectIdentifier{2, 5, 29, 37}, Critical: true, Value: v}}
	}
	a.tsa = mk(tsaT, a.inter, a.tsaKey.Public(), a.interKey)
	a.rootPool, a.interPool = x509.NewCertPool(), x509.NewCertPool()
	a.rootPool.AddCert(a.root)
	a.interPool.AddCert(a.inter)
	return a
}

func (a *authority) bundlePEM() []byte {
	var b []byte
	for _, c := range []*x509.Certificate{a.root, a.inter} {
		b = append(b, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})...)
	}
	return b
}

// ---- tokens ----

// tsReq is a time-stamp request as the test server reads it (own structs).
type tsReq struct {
	Version int
	MI      struct {
		Alg  pkix.AlgorithmIdentifier
		Hash []byte
	}
	Policy  asn1.ObjectIdentifier `asn1:"optional"`
	Nonce   *big.Int              `asn1:"optional"`
	CertReq bool                  `asn1:"optional"`
}

type issueOpts struct {
	genTime             time.Time
	policy              string
	serial              int64
	imprintAlg          string // "sha256" default
	imprint             []byte // override
	nonce               *big.Int
	nonceOverride       *big.Int
	dropNonce           bool
	tsaName             []byte // GeneralName for the tsa field of the TSTInfo
	accuracy            bool
	genTimeText         string // override
	digestAlg           string // "sha256" default
	sigGeneric          bool   // rsaEncryption / id-ecPublicKey instead of sha*WithRSA
	tstVersion          int64
	ess                 string // "v2" default, "v1", "none", "badhash", "v2sha384"
	noCerts             bool
	wrongMD             bool
	wrongCT             bool
	eContentOID         string
	badSigData          bool
	unsigned            bool
	twoSigners          bool
	useSKI              bool
	wrongSID            bool
	critExt             bool
	noSignedAttrs       bool
	versionSD           int64
	extraTSTByte        bool
	omitContentTypeAttr bool
	dupMD               bool
	badTSTDER           bool
	extraCerts          int
	// certsPrefix is put before the certificates of the token (a decoy that the
	// sid also names)
	certsPrefix []byte
}

func (a *authority) token(req *tsReq, o issueOpts) []byte {
	if o.genTime.IsZero() {
		o.genTime = time.Now().UTC().Truncate(time.Second)
	}
	if o.policy == "" {
		o.policy = "1.2.3.4.5"
	}
	if o.serial == 0 {
		o.serial = 4242
	}
	if o.imprintAlg == "" {
		o.imprintAlg = "sha256"
	}
	if o.digestAlg == "" {
		o.digestAlg = "sha256"
	}
	if o.tstVersion == 0 {
		o.tstVersion = 1
	}
	if o.ess == "" {
		o.ess = "v2"
	}
	if o.versionSD == 0 {
		o.versionSD = 3
	}
	imprint := req.MI.Hash
	if o.imprint != nil {
		imprint = o.imprint
	}
	gt := o.genTimeText
	if gt == "" {
		gt = o.genTime.Format("20060102150405") + "Z"
	}
	parts := [][]byte{intI(o.tstVersion), oidB(o.policy), seq(algB(hashOIDs[o.imprintAlg]), octet(imprint)),
		intI(o.serial), tl(0x18, []byte(gt))}
	if o.accuracy {
		parts = append(parts, seq(intI(1), tl(0x80, []byte{0x01, 0xf4}), tl(0x81, []byte{0x64})))
	}
	n := req.Nonce
	if o.nonceOverride != nil {
		n = o.nonceOverride
	}
	if n != nil && !o.dropNonce {
		parts = append(parts, intB(n))
	}
	if o.tsaName != nil {
		parts = append(parts, tl(0xa0, o.tsaName))
	}
	if o.critExt {
		parts = append(parts, tl(0xa1, seq(seq(oidB("1.2.3.99"), []byte{1, 1, 0xff}, octet([]byte{5, 0})))))
	}
	tst := seq(parts...)
	if o.extraTSTByte {
		tst = append(tst, 0)
	}
	if o.badTSTDER {
		tst = tst[:len(tst)-1]
	}
	ect := oidB("1.2.840.113549.1.9.16.1.4")
	if o.eContentOID != "" {
		ect = oidB(o.eContentOID)
	}
	build := func(tstBytes []byte) []byte { return a.signerInfo(o, tstBytes) }
	var signers [][]byte
	signers = append(signers, build(tst))
	if o.twoSigners {
		signers = append(signers, build(tst))
	}
	certs := cat(o.certsPrefix, a.tsa.Raw, a.inter.Raw)
	for i := 0; i < o.extraCerts; i++ {
		certs = cat(certs, a.root.Raw)
	}
	sd := []byte{}
	sd = append(sd, intI(o.versionSD)...)
	sd = append(sd, set(algB(hashOIDs[o.digestAlg]))...)
	sd = append(sd, seq(ect, tl(0xa0, octet(tst)))...)
	if !o.noCerts {
		sd = append(sd, tl(0xa0, certs)...)
	}
	sd = append(sd, set(signers...)...)
	return seq(oidB("1.2.840.113549.1.7.2"), tl(0xa0, seq(sd)))
}

func (a *authority) signerInfo(o issueOpts, tst []byte) []byte {
	sigOID, sigGeneric := "", o.sigGeneric
	_, isRSA := a.tsaKey.(*rsa.PrivateKey)
	switch {
	case isRSA && sigGeneric:
		sigOID = "1.2.840.113549.1.1.1"
	case isRSA:
		sigOID = map[string]string{"sha256": "1.2.840.113549.1.1.11", "sha384": "1.2.840.113549.1.1.12", "sha512": "1.2.840.113549.1.1.13"}[o.digestAlg]
	case sigGeneric:
		sigOID = "1.2.840.10045.2.1"
	default:
		sigOID = map[string]string{"sha256": "1.2.840.10045.4.3.2", "sha384": "1.2.840.10045.4.3.3", "sha512": "1.2.840.10045.4.3.4"}[o.digestAlg]
	}
	md := hashBytes(o.digestAlg, tst)
	if o.wrongMD {
		md = hashBytes(o.digestAlg, append([]byte("x"), tst...))
	}
	ctOID := "1.2.840.113549.1.9.16.1.4"
	if o.wrongCT {
		ctOID = "1.2.840.113549.1.7.1"
	}
	var attrs [][]byte
	if !o.omitContentTypeAttr {
		attrs = append(attrs, seq(oidB("1.2.840.113549.1.9.3"), set(oidB(ctOID))))
	}
	attrs = append(attrs, seq(oidB("1.2.840.113549.1.9.4"), set(octet(md))))
	if o.dupMD {
		attrs = append(attrs, seq(oidB("1.2.840.113549.1.9.4"), set(octet(md))))
	}
	certHash := func(alg string) []byte { return hashBytes(alg, a.tsa.Raw) }
	switch o.ess {
	case "v2":
		attrs = append(attrs, seq(oidB("1.2.840.113549.1.9.16.2.47"), set(seq(seq(seq(octet(certHash("sha256"))))))))
	case "v2sha384":
		attrs = append(attrs, seq(oidB("1.2.840.113549.1.9.16.2.47"), set(seq(seq(seq(algB(hashOIDs["sha384"]), octet(certHash("sha384"))))))))
	case "badhash":
		h := certHash("sha256")
		h[0] ^= 1
		attrs = append(attrs, seq(oidB("1.2.840.113549.1.9.16.2.47"), set(seq(seq(seq(octet(h)))))))
	case "v1":
		attrs = append(attrs, seq(oidB("1.2.840.113549.1.9.16.2.12"), set(seq(seq(seq(octet(certHash("sha1"))))))))
	case "v2serial":
		is := seq(seq(tl(0xa4, a.tsa.RawIssuer)), intB(a.tsa.SerialNumber))
		attrs = append(attrs, seq(oidB("1.2.840.113549.1.9.16.2.47"), set(seq(seq(seq(octet(certHash("sha256")), is))))))
	case "v2badserial":
		is := seq(seq(tl(0xa4, a.tsa.RawIssuer)), intI(7))
		attrs = append(attrs, seq(oidB("1.2.840.113549.1.9.16.2.47"), set(seq(seq(seq(octet(certHash("sha256")), is))))))
	case "v2badissuer":
		is := seq(seq(tl(0xa4, a.root.RawSubject)), intB(a.tsa.SerialNumber))
		attrs = append(attrs, seq(oidB("1.2.840.113549.1.9.16.2.47"), set(seq(seq(seq(octet(certHash("sha256")), is))))))
	case "none":
	}
	sort.Slice(attrs, func(i, j int) bool { return bytes.Compare(attrs[i], attrs[j]) < 0 })
	attrsCat := cat(attrs...)
	toSign := set(attrsCat)
	if o.badSigData {
		toSign = set(append([]byte{0}, attrsCat...))
	}
	ch := cryptoHash[o.digestAlg]
	hs := hashBytes(o.digestAlg, toSign)
	var sig []byte
	var err error
	switch k := a.tsaKey.(type) {
	case *rsa.PrivateKey:
		sig, err = rsa.SignPKCS1v15(rand.Reader, k, ch, hs)
	case *ecdsa.PrivateKey:
		sig, err = ecdsa.SignASN1(rand.Reader, k, hs)
	}
	if err != nil {
		panic(err)
	}
	var sid []byte
	if o.useSKI {
		sid = tl(0x80, a.tsa.SubjectKeyId)
	} else {
		serial := a.tsa.SerialNumber
		if o.wrongSID {
			serial = big.NewInt(999999)
		}
		sid = seq(a.tsa.RawIssuer, intB(serial))
	}
	ver := int64(1)
	if o.useSKI {
		ver = 3
	}
	si := [][]byte{intI(ver), sid, algB(hashOIDs[o.digestAlg])}
	if !o.noSignedAttrs {
		si = append(si, tl(0xa0, attrsCat))
	}
	si = append(si, algB(sigOID), octet(sig))
	if o.unsigned {
		si = append(si, tl(0xa1, seq(oidB("1.2.840.113549.1.9.6"), set(octet([]byte("x"))))))
	}
	return seq(si...)
}

func (a *authority) response(status int64, token []byte) []byte {
	if token == nil {
		return seq(seq(intI(status), seq(tl(0x0c, []byte("no token")))))
	}
	return seq(seq(intI(status)), token)
}

// reindefinite rewrites the outermost definite SEQUENCE as indefinite BER.
func reindefinite(der []byte) []byte {
	t, _, _ := readTLV(der)
	return cat([]byte{der[0], 0x80}, t.content, []byte{0, 0})
}

// nonMinimalLength re-encodes the outermost length with a leading zero byte.
func nonMinimalLength(der []byte) []byte {
	t, _, _ := readTLV(der)
	n := len(t.content)
	return cat([]byte{der[0], 0x83, byte(n >> 16), byte(n >> 8), byte(n)}, t.content)
}

func parseTSReq(t testing.TB, der []byte) *tsReq {
	var r tsReq
	rest, err := asn1.Unmarshal(der, &r)
	if err != nil || len(rest) != 0 {
		t.Fatalf("test server: bad request: %v", err)
	}
	return &r
}

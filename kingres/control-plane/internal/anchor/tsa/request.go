// SPDX-License-Identifier: AGPL-3.0-only

package tsa

import (
	"encoding/asn1"
	"errors"
	"io"
	"math/big"
	"strings"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
)

// MaxResponseBytes is the largest HTTP response body read from a TSA.
const MaxResponseBytes = 512 << 10

// PKIStatus values (RFC 3161 section 2.4.2).
const (
	StatusGranted        = 0
	StatusGrantedWithMod = 1
	StatusRejection      = 2
	StatusWaiting        = 3
	StatusRevocationWarn = 4
	StatusRevocationNote = 5
)

type algorithmIdentifier struct {
	Algorithm  asn1.ObjectIdentifier
	Parameters asn1.RawValue `asn1:"optional"`
}

type messageImprint struct {
	HashAlgorithm algorithmIdentifier
	HashedMessage []byte
}

type timeStampReq struct {
	Version        int
	MessageImprint messageImprint
	ReqPolicy      asn1.ObjectIdentifier `asn1:"optional"`
	Nonce          *big.Int              `asn1:"optional"`
	CertReq        bool                  `asn1:"optional"`
}

// Request is a TimeStampReq to build.
type Request struct {
	Digest    [32]byte
	PolicyOID string   // dotted; empty for none
	Nonce     *big.Int // positive
}

// BuildRequest returns the DER of the TimeStampReq of design 6.4 step 1:
// version 1, SHA-256 imprint (parameters NULL), reqPolicy if set, the nonce,
// certReq TRUE.
func BuildRequest(r Request) ([]byte, error) {
	if r.Nonce == nil || r.Nonce.Sign() <= 0 {
		return nil, errors.New("tsa: nonce must be positive")
	}
	req := timeStampReq{
		Version: 1,
		MessageImprint: messageImprint{
			HashAlgorithm: algorithmIdentifier{Algorithm: asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}, Parameters: asn1.NullRawValue},
			HashedMessage: r.Digest[:],
		},
		Nonce:   r.Nonce,
		CertReq: true,
	}
	if r.PolicyOID != "" {
		oid, err := ParseOID(r.PolicyOID)
		if err != nil {
			return nil, err
		}
		req.ReqPolicy = oid
	}
	return asn1.Marshal(req)
}

// ParseOID parses a dotted object identifier (at least two arcs, each a
// decimal number without leading zeros, first arc 0 to 2).
func ParseOID(s string) (asn1.ObjectIdentifier, error) {
	parts := strings.Split(s, ".")
	if len(parts) < 2 || len(s) > 128 {
		return nil, errors.New("tsa: malformed OID")
	}
	var oid asn1.ObjectIdentifier
	for _, p := range parts {
		if p == "" || len(p) > 9 || (len(p) > 1 && p[0] == '0') {
			return nil, errors.New("tsa: malformed OID")
		}
		n := 0
		for _, c := range p {
			if c < '0' || c > '9' {
				return nil, errors.New("tsa: malformed OID")
			}
			n = n*10 + int(c-'0')
		}
		oid = append(oid, n)
	}
	if oid[0] > 2 || (oid[0] < 2 && oid[1] >= 40) {
		return nil, errors.New("tsa: malformed OID")
	}
	return oid, nil
}

// Response is a parsed TimeStampResp.
type Response struct {
	Status     int
	StatusText string // first status string, ASCII-sanitised, at most 100 bytes
	FailInfo   uint32 // bit i set: failInfo bit i (0 badAlg, 2 badRequest, 5 badDataFormat, 14 timeNotAvailable, 15 unacceptedPolicy, 16 unacceptedExtension, 17 addInfoNotAvailable, 25 systemFailure)
	HasToken   bool
	// Token is the DER of the TimeStampToken (a sub-slice of the input).
	Token []byte
}

// ParseResponse parses a TimeStampResp in strict DER. It does not look at
// the token beyond cutting it out.
func ParseResponse(der []byte) (*Response, error) {
	bad := func(code, s string) error {
		return anchor.Errorf(anchor.ClassPermanent, anchor.KindTSA, "parse-response", code, "%s", s)
	}
	if len(der) > MaxResponseBytes {
		return nil, bad(CodeTooLarge, "response is larger than the limit")
	}
	top, err := oneTLV(der)
	if err != nil || checkDER(der, 0) != nil {
		return nil, bad(CodeBadDER, "response is not strict DER")
	}
	if top.tag != tagSequence {
		return nil, bad(CodeStructure, "response is not a TimeStampResp")
	}
	f, _ := children(top.content)
	if len(f) < 1 || len(f) > 2 || f[0].tag != tagSequence {
		return nil, bad(CodeStructure, "response is not a TimeStampResp")
	}
	psi, _ := children(f[0].content)
	if len(psi) < 1 {
		return nil, bad(CodeStructure, "PKIStatusInfo is malformed")
	}
	st, err := parseInt(psi[0])
	if err != nil || !st.IsInt64() {
		return nil, bad(CodeStructure, "PKIStatus is malformed")
	}
	r := &Response{Status: -1}
	if st.Int64() >= 0 && st.Int64() <= 5 {
		r.Status = int(st.Int64())
	}
	i := 1
	if i < len(psi) && psi[i].tag == tagSequence { // PKIFreeText
		txt, err := children(psi[i].content)
		if err != nil || len(txt) == 0 {
			return nil, bad(CodeStructure, "PKIFreeText is malformed")
		}
		if len(txt[0].content) > 0 {
			r.StatusText = anchor.NewRedactor().Clean(string(txt[0].content), 100)
		}
		i++
	}
	if i < len(psi) && psi[i].tag == tagBitString {
		c := psi[i].content
		if len(c) < 1 || len(c) > 5 || c[0] > 7 {
			return nil, bad(CodeStructure, "PKIFailureInfo is malformed")
		}
		for bit := 0; bit < (len(c)-1)*8 && bit < 32; bit++ {
			if c[1+bit/8]&(0x80>>(bit%8)) != 0 {
				r.FailInfo |= 1 << bit
			}
		}
		i++
	}
	if i != len(psi) {
		return nil, bad(CodeStructure, "PKIStatusInfo has unexpected content")
	}
	if len(f) == 2 {
		if f[1].tag != tagSequence {
			return nil, bad(CodeStructure, "timeStampToken is malformed")
		}
		r.HasToken, r.Token = true, f[1].raw
	}
	if r.Status < 0 {
		return nil, bad(CodeStructure, "PKIStatus is not a defined value")
	}
	return r, nil
}

// randNonce returns 64 random bits as a positive integer.
func randNonce(rd io.Reader) (*big.Int, error) {
	var b [8]byte
	if _, err := io.ReadFull(rd, b[:]); err != nil {
		return nil, err
	}
	n := new(big.Int).SetBytes(b[:])
	if n.Sign() == 0 {
		n.SetInt64(1)
	}
	return n, nil
}

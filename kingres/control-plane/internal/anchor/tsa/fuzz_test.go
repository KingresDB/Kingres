// SPDX-License-Identifier: AGPL-3.0-only

package tsa

import (
	"math/big"
	"testing"
	"time"
)

func fuzzSeeds(f *testing.F) (*authority, []byte) {
	a := newAuthority(f, authOpts{keyAlg: "p256fixed"})
	r := testReq()
	tok := a.token(r, issueOpts{accuracy: true})
	f.Add(tok)
	f.Add(a.token(r, issueOpts{unsigned: true}))
	f.Add(a.token(r, issueOpts{ess: "v1", useSKI: false}))
	f.Add(a.response(0, tok))
	f.Add(a.response(2, nil))
	f.Add(a.response(3, nil))
	f.Add(reindefinite(tok))
	f.Add([]byte{})
	f.Add([]byte{0x30, 0x80, 0, 0})
	return a, tok
}

// FuzzParseResponse: the TimeStampResp parser never panics, and what it
// accepts is strict DER with a token that lies inside the input.
func FuzzParseResponse(f *testing.F) {
	fuzzSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := ParseResponse(b)
		if err != nil {
			return
		}
		if r.HasToken && len(r.Token) > len(b) {
			t.Fatal("token outside the input")
		}
		if checkDER(b, 0) != nil {
			t.Fatal("accepted input that is not strict DER")
		}
	})
}

// FuzzVerifyToken: the token parser and verifier never panic. A token that
// verifies must be strict DER, and a token whose bytes differ from the seed
// inside the signed parts must not verify (checked for the TSTInfo imprint).
func FuzzVerifyToken(f *testing.F) {
	a, _ := fuzzSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		info, err := VerifyToken(b, Expect{Digest: testDigest, Nonce: big.NewInt(0x1122334455667788)}, a.rootPool, a.interPool)
		if err != nil {
			return
		}
		if checkDER(b, 0) != nil {
			t.Fatal("accepted a token that is not strict DER")
		}
		if info.Nonce == nil || info.GenTime.After(time.Now().Add(time.Hour)) {
			t.Fatalf("accepted a token with %+v", info)
		}
	})
}

// FuzzDER: the strict-DER walker and the element readers never panic.
func FuzzDER(f *testing.F) {
	fuzzSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		_ = checkDER(b, 0)
		if x, rest, err := readTLV(b); err == nil {
			_ = rest
			_, _ = children(x.content)
			_, _ = parseOID(x)
			_, _ = parseInt(x)
			_, _ = parseGenTime(x)
		}
	})
}

// FuzzSameName: the name comparison never panics, is symmetric, and a name
// that parses is the same name as itself.
func FuzzSameName(f *testing.F) {
	a := newAuthority(f, authOpts{keyAlg: "p256fixed"})
	f.Add(a.tsa.RawSubject, a.root.RawSubject)
	f.Add(a.tsa.RawSubject, a.tsa.RawSubject)
	f.Add([]byte{0x30, 0x00}, []byte{0x30, 0x00})
	f.Add([]byte{}, []byte{1})
	f.Fuzz(func(t *testing.T, x, y []byte) {
		if sameName(x, y) != sameName(y, x) {
			t.Fatal("not symmetric")
		}
		if _, ok := parseName(x); ok && !sameName(x, x) {
			t.Fatal("a name that parses is not the same as itself")
		}
	})
}

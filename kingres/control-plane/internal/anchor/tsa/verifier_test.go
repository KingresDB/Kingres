// SPDX-License-Identifier: AGPL-3.0-only

package tsa_test

// A verifier that has nothing but the bytes of an evidence file, the note it
// belongs to and the auditor's own roots: only exported functions are used. The
// files in testdata were made once by the test authority of this package (a
// root, an intermediate and a time-stamping certificate with fixed keys) and
// are embedded in the test binary.

import (
	"bytes"
	"crypto/x509"
	_ "embed"
	"encoding/base64"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor/tsa"
)

//go:embed testdata/own_evidence_file.json
var ownEvidenceFile []byte

//go:embed testdata/own_roots.pem
var ownRootsPEM []byte

func ownNote() []byte {
	root := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))
	sig := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 68))
	return []byte("kingres.example/agg\n17\n" + root + "\n\n" + string(rune(0x2014)) + " kingres.example/agg " + sig + "\n")
}

func TestIndependentVerifierOfStoredEvidence(t *testing.T) {
	file, err := anchor.ParseEvidenceFile(ownEvidenceFile)
	if err != nil {
		t.Fatal(err)
	}
	if file.Kind != anchor.KindTSA || file.Size != 17 || file.Origin != "kingres.example/agg" {
		t.Fatalf("%+v", file)
	}
	// 1. the note: its text hashes to payload_sha256 (the digest the token must stamp)
	p, err := anchor.ParsePayload("ab12", ownNote())
	if err != nil {
		t.Fatal(err)
	}
	if p.DigestHex() != file.PayloadSHA256 || p.Origin != file.Origin || p.Size != file.Size {
		t.Fatal("the note is not the one the evidence file belongs to")
	}
	// 2. the evidence, in its canonical form, and the token against the auditor's roots
	ev, err := anchor.ParseTSAEvidence(file.Evidence)
	if err != nil {
		t.Fatal(err)
	}
	roots, inter, err := tsa.ParseBundle(ownRootsPEM)
	if err != nil {
		t.Fatal(err)
	}
	info, err := tsa.VerifyEvidence(ev, p.Digest, roots, inter)
	if err != nil {
		t.Fatal(err)
	}
	// 3. the time that the verifier prints is genTime, as the evidence says
	if anchor.FormatTime(info.GenTime) != ev.GenTime || info.Serial.String() != ev.Serial || info.Policy != ev.Policy {
		t.Fatal("the copies in the evidence differ from the token")
	}
	// the evidence file was written when the record was anchored: the same second
	if d := file.CreatedAt.Sub(info.GenTime); d < -time.Minute || d > time.Minute {
		t.Errorf("created_at %s, genTime %s", file.CreatedAt, info.GenTime)
	}
	// and it stays valid long after: the certificate is judged at genTime
	if _, err := tsa.VerifyStoredToken(mustToken(t, ev), p.Digest, roots, inter); err != nil {
		t.Fatal(err)
	}
	// a different digest is not stamped by this token
	other := p.Digest
	other[0] ^= 1
	if _, err := tsa.VerifyEvidence(ev, other, roots, inter); err == nil {
		t.Fatal("the token verified for another digest")
	}
	// the auditor's roots decide: an empty pool trusts nothing
	if _, err := tsa.VerifyEvidence(ev, p.Digest, emptyPool(t), inter); err == nil {
		t.Fatal("verified against no roots")
	}
	// a copy in the evidence that differs from the token is refused
	bad := ev
	bad.Serial = "1"
	if _, err := tsa.VerifyEvidence(bad, p.Digest, roots, inter); err == nil {
		t.Fatal("a wrong serial in the evidence accepted")
	}
}

func mustToken(t *testing.T, ev anchor.TSAEvidence) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(ev.TokenDERB64)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func emptyPool(t *testing.T) *x509.CertPool { return x509.NewCertPool() }

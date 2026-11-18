// SPDX-License-Identifier: AGPL-3.0-only

package ots_test

// A verifier that has nothing but the bytes of an evidence file written from
// the text of doc.go and the note: only exported functions are used. The
// fragments are encoded here with a few lines of their own.

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor/ots"
)

func sha(b []byte) []byte { s := sha256.Sum256(b); return s[:] }

func vb(b []byte) []byte { return append([]byte{byte(len(b))}, b...) } // lengths below 128

func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

func TestIndependentVerifierOfStoredEvidence(t *testing.T) {
	root := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))
	sig := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 68))
	note := []byte("kingres.example/agg\n17\n" + root + "\n\n" + string(rune(0x2014)) + " kingres.example/agg " + sig + "\n")
	p, err := anchor.ParsePayload("ab12", note)
	if err != nil {
		t.Fatal(err)
	}
	digest := p.Digest[:]

	// the fragment of calendar a: digest, append(N), sha256, pending; and a Bitcoin claim from calendar b:
	// digest, append(N2), sha256, prepend(S), sha256, sha256, bitcoin(960000)
	n1 := bytes.Repeat([]byte{1}, 8)
	pend := cat([]byte{0x00}, []byte{0x83, 0xdf, 0xe3, 0x0d, 0x2e, 0xf9, 0x0c, 0x8e}, vb(vb([]byte("https://a.example"))))
	fragA := cat([]byte{0xf0}, vb(n1), []byte{0x08}, pend)
	n2 := bytes.Repeat([]byte{2}, 8)
	s := bytes.Repeat([]byte{0xab}, 32)
	btc := cat([]byte{0x00}, []byte{0x05, 0x88, 0x96, 0x0d, 0x73, 0xd7, 0x19, 0x01}, vb([]byte{0x80, 0xcc, 0x3a})) // 960000 as a varuint
	fragB := cat([]byte{0xf0}, vb(n2), []byte{0x08, 0xf1}, vb(s), []byte{0x08, 0x08}, btc)
	merkle := sha(sha(sha(cat(s, sha(cat(digest, n2))))))
	_ = merkle
	m1 := sha(cat(digest, n2))
	merkle = sha(sha(cat(s, m1)))
	ev := fmt.Sprintf(`{"digest":"%s","submitted_at":"2026-12-01T10:00:00Z","calendars":[`+
		`{"uri":"https://a.example","fragment_b64":"%s"},{"uri":"https://b.example","fragment_b64":"%s"}],`+
		`"bitcoin":{"height":960000,"calendar_uri":"https://b.example","merkle_root":"%s","status":"claimed","header":""},"contradicted":[]}`,
		p.DigestHex(), base64.StdEncoding.EncodeToString(fragA), base64.StdEncoding.EncodeToString(fragB), hex.EncodeToString(merkle))
	file := fmt.Sprintf(`{"format":1,"kind":"ots","origin":"kingres.example/agg","size":17,"payload_sha256":"%s","created_at":"2026-12-01T10:30:00Z","evidence":%s}`, p.DigestHex(), ev)

	f, err := anchor.ParseEvidenceFile([]byte(file))
	if err != nil {
		t.Fatal(err)
	}
	oe, err := anchor.ParseOTSEvidence(f.Evidence)
	if err != nil {
		t.Fatal(err)
	}
	// the commitment of the file: the digest of the evidence is the digest of the note text
	if oe.Digest != f.PayloadSHA256 || oe.Digest != p.DigestHex() {
		t.Fatal("the digest of the evidence is not the digest of the note")
	}
	now := time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)
	chk, err := ots.CheckEvidence(oe, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	// every Bitcoin attestation of every fragment is reported: here one, and it is only a claim
	if len(chk.Claims) != 1 || chk.Status != anchor.BitcoinClaimed || chk.Claims[0].Height != 960000 ||
		hex.EncodeToString(chk.Claims[0].Root[:]) != hex.EncodeToString(merkle) || chk.Claims[0].CalendarURI != "https://b.example" {
		t.Fatalf("%+v", chk)
	}
	if chk.Claims[0].HeaderChecked {
		t.Fatal("a claim was reported as checked against a header that nobody supplied")
	}
	// a header that the verifier holds and that does not hold this root makes the claim false
	other := make([]byte, 80)
	copy(other[36:68], bytes.Repeat([]byte{7}, 32))
	chk, err = ots.CheckEvidence(oe, now, func(h uint64) ([]byte, bool) { return other, true })
	if err != nil || chk.Status == anchor.BitcoinHeaderChecked || chk.Claims[0].HeaderChecked {
		t.Fatalf("%+v %v", chk, err)
	}
	// the export is the reference file: it parses and holds both attestations
	exp, err := ots.Export(oe)
	if err != nil {
		t.Fatal(err)
	}
	pf, err := ots.ParseFile(exp)
	if err != nil || !bytes.Equal(pf.Root.Msg, digest) {
		t.Fatalf("%v", err)
	}
	var kinds []string
	for _, l := range pf.Root.Leaves() {
		switch l.Att.Kind {
		case ots.AttPending:
			kinds = append(kinds, "pending")
		case ots.AttBitcoin:
			kinds = append(kinds, "bitcoin")
		}
	}
	if strings.Join(kinds, ",") != "pending,bitcoin" && strings.Join(kinds, ",") != "bitcoin,pending" {
		t.Fatalf("%v", kinds)
	}
	// the same fragment with a height that no block could have (the digest is made in 2026): refused
	lowFrag := cat([]byte{0xf0}, vb(n2), []byte{0x08, 0xf1}, vb(s), []byte{0x08, 0x08}, cat([]byte{0x00}, []byte{0x05, 0x88, 0x96, 0x0d, 0x73, 0xd7, 0x19, 0x01}, vb([]byte{0x05})))
	low := oe
	low.Calendars = []anchor.OTSCalendar{oe.Calendars[0], {URI: "https://b.example", FragmentB64: base64.StdEncoding.EncodeToString(lowFrag)}}
	lb := *oe.Bitcoin
	lb.Height = 5
	low.Bitcoin = &lb
	if c, err := ots.CheckEvidence(low, now, nil); err == nil {
		t.Errorf("a claim at height 5 accepted: %+v", c)
	}
	low.Bitcoin = nil
	if c, err := ots.CheckEvidence(low, now, nil); err != nil || c.Status != "" || len(c.Claims) != 1 || c.Claims[0].Problem == "" {
		t.Errorf("the problem of the claim at height 5 is not reported: %+v %v", c, err)
	}
	// a summary that matches no attestation of the fragments is refused
	bad := oe
	bb := *oe.Bitcoin
	bb.Height = 960001
	bad.Bitcoin = &bb
	if _, err := ots.CheckEvidence(bad, now, nil); err == nil {
		t.Error("a summary that names no attestation of the fragments accepted")
	}
	// a file edited without changing what it parses to is refused
	for name, s := range map[string]string{
		"upper-case":   strings.Replace(file, `"calendars"`, `"Calendars"`, 1),
		"space":        strings.Replace(file, `"height":960000,`, `"height": 960000,`, 1),
		"status":       strings.Replace(file, `"status":"claimed"`, `"status":"anchored"`, 1),
		"header":       strings.Replace(file, `"header":""`, `"header":"00"`, 1),
		"no-merkle":    strings.Replace(file, `"merkle_root":"`+hex.EncodeToString(merkle)+`",`, ``, 1),
		"no-submitted": strings.Replace(file, `"submitted_at":"2026-12-01T10:00:00Z",`, ``, 1),
	} {
		if _, err := anchor.ParseEvidenceFile([]byte(s)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

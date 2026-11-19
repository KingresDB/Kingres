// SPDX-License-Identifier: AGPL-3.0-only

package attest

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/edkey"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/format"
)

func TestMessageIsTheDocumented136Bytes(t *testing.T) {
	var chain, row [32]byte
	var key, nonce [16]byte
	for i := range chain {
		chain[i], row[i] = 0xc0+byte(i%16), 0x30+byte(i%16)
	}
	for i := range key {
		key[i], nonce[i] = 0x10+byte(i), 0x70+byte(i)
	}
	m := Message(chain, 0x01020304, format.OpInsert, row, key, nonce)
	if len(m) != 136 || MessageSize != 136 {
		t.Fatalf("%d bytes", len(m))
	}
	want := []byte("kingres-ledger/client-signature/v1")
	want = append(want, 0)
	want = append(want, chain[:]...)
	want = append(want, 1, 2, 3, 4, 'I')
	want = append(want, row[:]...)
	want = append(want, key[:]...)
	want = append(want, nonce[:]...)
	if !bytes.Equal(m, want) {
		t.Fatalf("%x\n%x", m, want)
	}
	if Message(chain, 1, format.OpDelete, row, key, nonce)[71] != 'D' {
		t.Fatal("the operation byte")
	}
}

func TestVerifyRefusesKeysOfSmallOrderAndWrongSignatures(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(bytes.NewReader(bytes.Repeat([]byte{3}, 64)))
	msg := []byte("a message")
	sig := ed25519.Sign(priv, msg)
	if err := Verify(pub, msg, sig); err != nil {
		t.Fatal(err)
	}
	bad := append([]byte{}, sig...)
	bad[3] ^= 1
	if err := Verify(pub, msg, bad); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("%v", err)
	}
	if err := Verify(pub, msg, sig[:10]); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("%v", err)
	}
	identity, _ := hex.DecodeString("01" + "00000000000000000000000000000000000000000000000000000000000000")
	if err := Verify(identity, msg, append([]byte{1}, make([]byte, 63)...)); !errors.Is(err, edkey.ErrSmallOrder) {
		t.Fatalf("a key of small order: %v", err)
	}
}

func col(n string, v []byte) format.Column { return format.Column{Name: n, Value: v} }

func TestParseAttestationAndRevocationRefuseWhatIsNotTheTable(t *testing.T) {
	good := []format.Column{col("ledger_txid", make([]byte, 8)), col("seq", make([]byte, 4)), col("key_id", make([]byte, 16)), col("nonce", make([]byte, 16)), col("signature", make([]byte, 64))}
	if _, err := ParseAttestation(good); err != nil {
		t.Fatal(err)
	}
	mut := func(i int, c format.Column) []format.Column {
		x := append([]format.Column{}, good...)
		x[i] = c
		return x
	}
	for name, cols := range map[string][]format.Column{
		"missing column":  good[:4],
		"extra column":    append(append([]format.Column{}, good...), col("x", nil)),
		"wrong order":     append([]format.Column{good[1], good[0]}, good[2:]...),
		"short txid":      mut(0, col("ledger_txid", make([]byte, 4))),
		"negative txid":   mut(0, col("ledger_txid", bytes.Repeat([]byte{0xff}, 8))),
		"negative seq":    mut(1, col("seq", []byte{0xff, 0xff, 0xff, 0xff})),
		"short key id":    mut(2, col("key_id", make([]byte, 15))),
		"long nonce":      mut(3, col("nonce", make([]byte, 17))),
		"short signature": mut(4, col("signature", make([]byte, 63))),
		"wrong name":      mut(2, col("keyid", make([]byte, 16))),
	} {
		if _, err := ParseAttestation(cols); err == nil {
			t.Errorf("attestation: %s accepted", name)
		}
	}
	rev := []format.Column{col("key_id", make([]byte, 16)), col("reason", []byte("stolen")), col("revoked_by", []byte("admin")), col("revoked_at", make([]byte, 8))}
	r, err := ParseRevocation(rev)
	if err != nil || r.Reason != "stolen" || r.RevokedBy != "admin" {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := ParseRevocation(rev[:3]); err == nil {
		t.Error("revocation with a column missing")
	}
	bad := append([]format.Column{}, rev...)
	bad[1] = col("reason", []byte{0xff, 0xfe})
	if _, err := ParseRevocation(bad); err == nil {
		t.Error("revocation with a reason that is not UTF-8")
	}
}

func TestTheRevocationRuleIsByBlockHeightAndTheSameBlockCountsAsAfter(t *testing.T) {
	for _, c := range []struct {
		att, rev uint64
		revoked  bool
	}{{5, 6, false}, {5, 5, true}, {5, 4, true}, {0, 0, true}, {0, 1, false}} {
		if got := RevokedBefore(c.att, c.rev); got != c.revoked {
			t.Errorf("attestation in block %d, revocation in block %d: %v", c.att, c.rev, got)
		}
	}
}

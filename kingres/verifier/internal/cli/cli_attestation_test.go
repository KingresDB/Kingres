// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package cli

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/format"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/rowfile"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/trust"
)

const (
	tableAtt = 8
	tableRev = 9
)

// message is the 136 bytes of design 3.7, written here by hand (not by the verifier's function).
func message(chain [32]byte, table uint32, op byte, row [32]byte, keyID [16]byte, nonce [16]byte) []byte {
	var b bytes.Buffer
	b.WriteString("kingres-ledger/client-signature/v1")
	b.WriteByte(0)
	b.Write(chain[:])
	var t [4]byte
	binary.BigEndian.PutUint32(t[:], table)
	b.Write(t[:])
	b.WriteByte(op)
	b.Write(row[:])
	b.Write(keyID[:])
	b.Write(nonce[:])
	return b.Bytes()
}

func colsJSON(cols ...[2]string) []byte {
	var parts []string
	for _, c := range cols {
		parts = append(parts, fmt.Sprintf(`{"name":%q,"send_hex":%q}`, c[0], c[1]))
	}
	return []byte("[" + strings.Join(parts, ",") + "]")
}

func rowHashOfCols(t *testing.T, table uint32, cols []byte) h32 {
	t.Helper()
	cs, err := rowfile.Parse(cols)
	if err != nil {
		t.Fatal(err)
	}
	h, err := format.RowHash(table, cs)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

type attFix struct {
	w                          *world
	chain                      *chainFix
	key                        ed25519.PrivateKey
	keyID                      [16]byte
	nonce                      [16]byte
	attCols                    []byte
	proofDir, row, att, cols   string
	trustPinned, trustUnpinned string
}

func attCols(txid uint64, seq uint32, keyID, nonce [16]byte, sig []byte) []byte {
	var tx [8]byte
	binary.BigEndian.PutUint64(tx[:], txid)
	var sq [4]byte
	binary.BigEndian.PutUint32(sq[:], seq)
	return colsJSON([2]string{"ledger_txid", hex.EncodeToString(tx[:])}, [2]string{"seq", hex.EncodeToString(sq[:])}, [2]string{"key_id", hex.EncodeToString(keyID[:])},
		[2]string{"nonce", hex.EncodeToString(nonce[:])}, [2]string{"signature", hex.EncodeToString(sig)})
}

// newAtt makes a chain of 5 blocks in which block 2 holds the row (entry 0, table 7) and its attestation (entry 1, table 8),
// signed under chainIDForSig (a test of another chain's id passes another one) and the given op.
func newAtt(t *testing.T, mutate func(*attFix, *[]byte, *[32]byte)) *attFix {
	t.Helper()
	f := &attFix{w: newWorld(t)}
	_, f.key, _ = ed25519.GenerateKey(bytes.NewReader(bytes.Repeat([]byte{7}, 64)))
	pub := f.key.Public().(ed25519.PublicKey)
	sum := sha256.Sum256(pub)
	copy(f.keyID[:], sum[:16])
	copy(f.nonce[:], bytes.Repeat([]byte{0x5a}, 16))
	chainID := makeChain(0xc3, 1, nil).id
	row := rowHashOf(0xc3, 2)
	sig := ed25519.Sign(f.key, message(chainID, tableUser, format.OpInsert, row, f.keyID, f.nonce))
	cols := attCols(102, 0, f.keyID, f.nonce, sig)
	if mutate != nil {
		mutate(f, &cols, &chainID)
	}
	f.attCols = cols
	attRow := rowHashOfCols(t, tableAtt, cols)
	f.chain = makeChain(0xc3, 5, map[int][]extraEntry{2: {{table: tableAtt, op: format.OpInsert, row: attRow}}})
	f.w.addChain(f.chain, 5)
	f.w.appendRound(1_000, member(f.chain, 5, 0))
	f.w.put("p/row.json", f.chain.proofJSON(2, []extraEntry{{table: tableAtt, op: format.OpInsert, row: attRow}}, 0))
	f.w.put("p/att.json", f.chain.proofJSON(2, []extraEntry{{table: tableAtt, op: format.OpInsert, row: attRow}}, 1))
	f.w.put("p/att.columns.json", cols)
	f.trustPinned = f.pinTrust(true)
	f.trustUnpinned = f.pinTrust(false)
	return f
}

func (f *attFix) pinTrust(pin bool) string {
	f.w.t.Helper()
	data, err := os.ReadFile(f.w.trust)
	if err != nil {
		f.w.t.Fatal(err)
	}
	tf, err := trust.Parse(data)
	if err != nil {
		f.w.t.Fatal(err)
	}
	if pin {
		pub := f.key.Public().(ed25519.PublicKey)
		tf.ClientKeys = []trust.ClientKey{{KeyID: hex.EncodeToString(f.keyID[:]), Public: pub, Label: "teller-7"}}
	}
	out, err := tf.Marshal()
	if err != nil {
		f.w.t.Fatal(err)
	}
	p := filepath.Join(testdir.New(f.w.t), "trust.json")
	if err := os.WriteFile(p, out, 0o600); err != nil {
		f.w.t.Fatal(err)
	}
	return p
}

func (f *attFix) args(trustFile string, more ...string) []string {
	p := func(n string) string { return filepath.Join(f.w.dir, "p", n) }
	return append([]string{"attestation", "--trust", trustFile, "--proof", p("row.json"), "--attestation", p("att.json"), "--attestation-columns", p("att.columns.json"),
		"--attestation-table-id", fmt.Sprint(tableAtt), "--bundle", f.w.dir, "--log-leg", "chain", "--now", "2026-11-19T12:00:00Z"}, more...)
}

func TestAttestationVerifiedWithThePinnedKeyAndSaysNoRevocationShown(t *testing.T) {
	f := newAtt(t, nil)
	out := wantRun(t, 0, f.args(f.trustPinned), "VERIFIED:", "no revocation shown in the evidence given", "ok  signature", "teller-7", "presence, not absence", "ok  row log-inclusion", "ok  attestation log-inclusion")
	if strings.Contains(out, "no revocation exists") {
		t.Fatal("the output says that no revocation exists")
	}
	wantRun(t, 0, f.args(f.trustPinned, "--json"), `"revocation_shown": false`)
	wantRun(t, 0, f.args(f.trustPinned, "--client-key", hex.EncodeToString(f.keyID[:])), "VERIFIED:")
}

func TestAttestationRefusesAKeyThatIsNotPinnedAndAWrongExpectedKey(t *testing.T) {
	f := newAtt(t, nil)
	wantRun(t, 1, f.args(f.trustUnpinned), "FAILED at step client-key", "not pinned", "registry of the server is not trusted")
	wantRun(t, 1, f.args(f.trustPinned, "--client-key", strings.Repeat("ab", 16)), "FAILED at step client-key", "you expected")
}

func TestAttestationRefusesWhatTheSignatureDoesNotCover(t *testing.T) {
	// signed for another chain
	f := newAtt(t, func(f *attFix, cols *[]byte, chain *[32]byte) {
		other := makeChain(0xd4, 1, nil).id
		sig := ed25519.Sign(f.key, message(other, tableUser, format.OpInsert, rowHashOf(0xc3, 2), f.keyID, f.nonce))
		*cols = attCols(102, 0, f.keyID, f.nonce, sig)
	})
	wantRun(t, 1, f.args(f.trustPinned), "FAILED at step signature")
	// signed for another table
	f = newAtt(t, func(f *attFix, cols *[]byte, chain *[32]byte) {
		sig := ed25519.Sign(f.key, message(*chain, 99, format.OpInsert, rowHashOf(0xc3, 2), f.keyID, f.nonce))
		*cols = attCols(102, 0, f.keyID, f.nonce, sig)
	})
	wantRun(t, 1, f.args(f.trustPinned), "FAILED at step signature")
	// signed for another operation
	f = newAtt(t, func(f *attFix, cols *[]byte, chain *[32]byte) {
		sig := ed25519.Sign(f.key, message(*chain, tableUser, format.OpDelete, rowHashOf(0xc3, 2), f.keyID, f.nonce))
		*cols = attCols(102, 0, f.keyID, f.nonce, sig)
	})
	wantRun(t, 1, f.args(f.trustPinned), "FAILED at step signature")
	// a replay: the signature that was made for another row of the same client
	f = newAtt(t, func(f *attFix, cols *[]byte, chain *[32]byte) {
		sig := ed25519.Sign(f.key, message(*chain, tableUser, format.OpInsert, rowHashOf(0xc3, 3), f.keyID, f.nonce))
		*cols = attCols(102, 0, f.keyID, f.nonce, sig)
	})
	wantRun(t, 1, f.args(f.trustPinned), "FAILED at step signature")
	// a signature byte changed
	f = newAtt(t, func(f *attFix, cols *[]byte, chain *[32]byte) {
		sig := ed25519.Sign(f.key, message(*chain, tableUser, format.OpInsert, rowHashOf(0xc3, 2), f.keyID, f.nonce))
		sig[10] ^= 1
		*cols = attCols(102, 0, f.keyID, f.nonce, sig)
	})
	wantRun(t, 1, f.args(f.trustPinned), "FAILED at step signature")
	// the attestation names another transaction or entry than the row proof
	f = newAtt(t, func(f *attFix, cols *[]byte, chain *[32]byte) {
		sig := ed25519.Sign(f.key, message(*chain, tableUser, format.OpInsert, rowHashOf(0xc3, 2), f.keyID, f.nonce))
		*cols = attCols(103, 0, f.keyID, f.nonce, sig)
	})
	wantRun(t, 1, f.args(f.trustPinned), "FAILED at step attestation-entry", "transaction 103")
	// columns of the wrong shape
	f = newAtt(t, nil)
	f.w.put("p/att.columns.json", colsJSON([2]string{"ledger_txid", "00"}))
	wantRun(t, 1, f.args(f.trustPinned), "attestation-columns")
	// columns that do not hash to the proof's row
	f = newAtt(t, nil)
	b := append([]byte{}, f.attCols...)
	b = bytes.Replace(b, []byte(`"seq","send_hex":"00000000"`), []byte(`"seq","send_hex":"00000001"`), 1)
	f.w.put("p/att.columns.json", b)
	wantRun(t, 1, f.args(f.trustPinned), "FAILED at step attestation: binding")
}

func TestAttestationRevocationByBlockHeight(t *testing.T) {
	// the revocation row is entry 1 of block b of a chain; the attestation is in block 2
	for _, tc := range []struct {
		block int
		want  int
		text  string
	}{{1, 1, "FAILED at step revoked"}, {2, 1, "same block counts as after"}, {3, 0, "after block 2 of the attestation"}} {
		rev := colsJSON([2]string{"key_id", ""}, [2]string{"reason", hex.EncodeToString([]byte("stolen"))}, [2]string{"revoked_by", hex.EncodeToString([]byte("admin"))}, [2]string{"revoked_at", "0000000000000000"})
		var f *attFix
		f = newAtt(t, nil)
		rev = bytes.Replace(rev, []byte(`"send_hex":""`), []byte(`"send_hex":"`+hex.EncodeToString(f.keyID[:])+`"`), 1)
		revRow := rowHashOfCols(t, tableRev, rev)
		// rebuild the chain with the revocation row in the block of the case
		attRow := rowHashOfCols(t, tableAtt, f.attCols)
		extra := map[int][]extraEntry{2: {{table: tableAtt, op: format.OpInsert, row: attRow}}}
		if tc.block == 2 {
			extra[2] = append(extra[2], extraEntry{table: tableRev, op: format.OpInsert, row: revRow})
		} else {
			extra[tc.block] = []extraEntry{{table: tableRev, op: format.OpInsert, row: revRow}}
		}
		f.chain = makeChain(0xc3, 5, extra)
		f.w.chains = nil
		f.w.leaves, f.w.aggEnc = nil, nil
		f.w.addChain(f.chain, 5)
		f.w.appendRound(1_000, member(f.chain, 5, 0))
		f.w.put("p/row.json", f.chain.proofJSON(2, extra[2], 0))
		f.w.put("p/att.json", f.chain.proofJSON(2, extra[2], 1))
		revIdx := 1
		if tc.block == 2 {
			revIdx = 2
		}
		f.w.put("rev/k.proof.json", f.chain.proofJSON(tc.block, extra[tc.block], revIdx))
		f.w.put("rev/k.columns.json", rev)
		args := f.args(f.trustPinned, "--revocations", filepath.Join(f.w.dir, "rev"), "--revocation-table-id", fmt.Sprint(tableRev))
		out := wantRun(t, tc.want, args, tc.text)
		if tc.want == 0 && !strings.Contains(out, "no revocation shown in the evidence given") {
			t.Fatal(out)
		}
	}
}

func TestAttestationUsageErrors(t *testing.T) {
	f := newAtt(t, nil)
	wantRun(t, 2, []string{"attestation", "--trust", f.trustPinned}, "missing --proof")
	args := f.args(f.trustPinned)
	wantRun(t, 2, append(args, "--digest", fx("digests/d0.json")), "exactly one anchor")
	wantRun(t, 2, f.args(f.trustPinned, "--client-key", "zz"), "--client-key")
	wantRun(t, 2, f.args(f.trustPinned, "--revocations", filepath.Join(f.w.dir, "p")), "--revocation-table-id")
	_ = base64.StdEncoding
}

func TestAKeyOfSmallOrderInTheTrustFileIsRefused(t *testing.T) {
	f := newAtt(t, nil)
	data, _ := os.ReadFile(f.trustPinned)
	small := strings.Repeat("00", 31) + "01" // not the identity encoding; use the identity: 01 00..
	small = "01" + strings.Repeat("00", 31)
	pub, _ := hex.DecodeString(small)
	id := sha256.Sum256(pub)
	bad := strings.Replace(string(data), hex.EncodeToString(f.keyID[:]), hex.EncodeToString(id[:16]), 1)
	bad = strings.Replace(bad, base64.StdEncoding.EncodeToString(f.key.Public().(ed25519.PublicKey)), base64.StdEncoding.EncodeToString(pub), 1)
	p := filepath.Join(testdir.New(t), "t.json")
	_ = os.WriteFile(p, []byte(bad), 0o600)
	wantRun(t, 2, f.args(p), "small order")
}

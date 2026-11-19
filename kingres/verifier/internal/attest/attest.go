// SPDX-License-Identifier: AGPL-3.0-only

// Package attest is the client attestation of ledger-phase7.md 3.6 to 3.8, as the verifier reads it:
// the message a client signs, the three tables of the extension that hold what the verifier needs
// (an attestation, a key revocation) and how their columns are read from the columns file that
// `row-hash` also takes. It is written from the design and the README of the extension, not from
// the extension's code.
package attest

import (
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/edkey"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/format"
)

// Sizes of the fields of the message and of the rows.
const (
	Domain      = "kingres-ledger/client-signature/v1"
	ChainIDSize = 32
	HashSize    = 32
	KeyIDSize   = 16
	NonceSize   = 16
	SigSize     = ed25519.SignatureSize
	// MessageSize is the 136 signed bytes: the domain tag and a zero byte, the chain id, the table id,
	// the operation, the row hash, the key id and the nonce.
	MessageSize = len(Domain) + 1 + ChainIDSize + 4 + 1 + HashSize + KeyIDSize + NonceSize
)

// Message builds the bytes a client signs (3.7): "kingres-ledger/client-signature/v1" || 0x00 ||
// chain_id (32) || u32 table_id || u8 op ('I' = 0x49, 'D' = 0x44) || row_hash (32) || key_id (16) || nonce (16),
// integers big endian. It is the result of the SQL function ledger_attestation_message.
func Message(chainID [ChainIDSize]byte, tableID uint32, op byte, rowHash [HashSize]byte, keyID [KeyIDSize]byte, nonce [NonceSize]byte) []byte {
	b := make([]byte, 0, MessageSize)
	b = append(b, Domain...)
	b = append(b, 0)
	b = append(b, chainID[:]...)
	b = binary.BigEndian.AppendUint32(b, tableID)
	b = append(b, op)
	b = append(b, rowHash[:]...)
	b = append(b, keyID[:]...)
	return append(b, nonce[:]...)
}

// Errors.
var (
	// ErrBadSignature: the signature does not verify under the key.
	ErrBadSignature = errors.New("the signature does not verify under the pinned key for this message")
	// ErrBadOp: the operation byte is neither I nor D.
	ErrBadOp = errors.New("the operation is neither I nor D")
)

// Verify checks the Ed25519 signature of a message under a public key that passes the three tests of
// correction 155 (the standard library accepts signatures under keys that it should not).
func Verify(pub, msg, sig []byte) error {
	if err := edkey.Check(pub); err != nil {
		return fmt.Errorf("the public key is refused: %w", err)
	}
	if len(sig) != SigSize || !ed25519.Verify(pub, msg, sig) {
		return ErrBadSignature
	}
	return nil
}

// Row is a row of kingres.ledger_attestation.
type Row struct {
	LedgerTxID uint64
	Seq        uint32
	KeyID      [KeyIDSize]byte
	Nonce      [NonceSize]byte
	Signature  [SigSize]byte
}

// Revocation is a row of kingres.ledger_client_key_revocation.
type Revocation struct {
	KeyID       [KeyIDSize]byte
	Reason      string
	RevokedBy   string
	RevokedAtUS int64 // microseconds since 2000-01-01 00:00:00 UTC
}

func expectColumns(cols []format.Column, names ...string) error {
	if len(cols) != len(names) {
		return fmt.Errorf("the columns file has %d column(s), expected the %d non-NULL columns %v in this order (a NULL column is left out, and none of these may be NULL)", len(cols), len(names), names)
	}
	for i, n := range names {
		if cols[i].Name != n {
			return fmt.Errorf("column %d is %q, expected %q: the columns are those of the table in the order of its attributes", i+1, cols[i].Name, n)
		}
	}
	return nil
}

func exactly(c format.Column, n int) ([]byte, error) {
	if len(c.Value) != n {
		return nil, fmt.Errorf("column %q has %d byte(s) in its binary send form, expected %d", c.Name, len(c.Value), n)
	}
	return c.Value, nil
}

// ParseAttestation reads the columns ledger_txid (int8), seq (int4), key_id (bytea, 16), nonce
// (bytea, 16) and signature (bytea, 64), each as the bytes of its binary send function.
func ParseAttestation(cols []format.Column) (Row, error) {
	var r Row
	if err := expectColumns(cols, "ledger_txid", "seq", "key_id", "nonce", "signature"); err != nil {
		return r, err
	}
	b, err := exactly(cols[0], 8)
	if err != nil {
		return r, err
	}
	tx := binary.BigEndian.Uint64(b)
	if tx > 1<<63-1 {
		return r, errors.New("column \"ledger_txid\" is negative: it is a bigint of a transaction number")
	}
	r.LedgerTxID = tx
	if b, err = exactly(cols[1], 4); err != nil {
		return r, err
	}
	seq := binary.BigEndian.Uint32(b)
	if seq > 1<<31-1 {
		return r, errors.New("column \"seq\" is negative")
	}
	r.Seq = seq
	if b, err = exactly(cols[2], KeyIDSize); err != nil {
		return r, err
	}
	copy(r.KeyID[:], b)
	if b, err = exactly(cols[3], NonceSize); err != nil {
		return r, err
	}
	copy(r.Nonce[:], b)
	if b, err = exactly(cols[4], SigSize); err != nil {
		return r, err
	}
	copy(r.Signature[:], b)
	return r, nil
}

// ParseRevocation reads key_id (bytea, 16), reason (text), revoked_by (name) and revoked_at
// (timestamptz, int8 microseconds since 2000-01-01).
func ParseRevocation(cols []format.Column) (Revocation, error) {
	var r Revocation
	if err := expectColumns(cols, "key_id", "reason", "revoked_by", "revoked_at"); err != nil {
		return r, err
	}
	b, err := exactly(cols[0], KeyIDSize)
	if err != nil {
		return r, err
	}
	copy(r.KeyID[:], b)
	if !utf8.Valid(cols[1].Value) || !utf8.Valid(cols[2].Value) {
		return r, errors.New("the reason and revoked_by are text: valid UTF-8")
	}
	r.Reason, r.RevokedBy = string(cols[1].Value), string(cols[2].Value)
	if b, err = exactly(cols[3], 8); err != nil {
		return r, err
	}
	r.RevokedAtUS = int64(binary.BigEndian.Uint64(b)) // #nosec G115 -- two's complement i64
	return r, nil
}

// RevokedBefore applies the rule of 3.3 and 3.7: an attestation in the block at height attBlock is
// valid iff that height is LOWER than the height of the block of the revocation. The same block counts
// as after, because the order inside a block is by transaction id, not by commit order.
func RevokedBefore(attBlock, revocationBlock uint64) bool { return revocationBlock <= attBlock }

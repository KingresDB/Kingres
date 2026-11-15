// SPDX-License-Identifier: PostgreSQL

// Package format builds the byte strings that the KingresDB ledger hashes
// (hash format version 1). Each kind of hash input is built in exactly one
// function of this package; the layouts are quoted from
// contrib/kingres_ledger/README.md, section "Hash format version 1".
package format

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/merkle"
)

// HashVersion is the only hash format version this package implements.
const HashVersion = 1

// Domain tags. README: "Every kind of hashed data starts with its own
// one-byte tag": row 0x10, entry 0x11, block transaction 0x12, block 0x13,
// chain 0x14.
const (
	// TagRow starts the canonical row encoding.
	TagRow byte = 0x10
	// TagEntry starts the leaf data of an entry in a transaction tree.
	TagEntry byte = 0x11
	// TagBlockTx starts the leaf data of a transaction in a block tree.
	TagBlockTx byte = 0x12
	// TagBlock starts the input of the block hash.
	TagBlock byte = 0x13
	// TagChain starts the input of the hash that is the prev_hash of block 0.
	TagChain byte = 0x14
)

// OpInsert is the operation byte of an insert entry: README "op = 0x49 ('I')".
const OpInsert byte = 0x49

// OpDelete is the operation byte of a removal entry (version 0.2 of the
// ledger, updatable tables): the version of a row with this hash left the
// table, by an UPDATE or a DELETE. README "op = 0x44 ('D')".
const OpDelete byte = 0x44

// Limits that follow from the widths of the length fields.
const (
	maxColumns   = 1<<16 - 1
	maxNameLen   = 1<<16 - 1
	maxValueLen  = 1<<32 - 1
	maxUserName  = 255
	chainIDBytes = 32
)

// Column is one non-NULL column of a row: its name and the bytes of the
// value's binary send form.
type Column struct {
	Name  string
	Value []byte
}

// ErrEncoding is wrapped by every error about an input that cannot be
// encoded.
var ErrEncoding = errors.New("cannot encode")

// EncodeRow returns the canonical row encoding. README:
//
//	u8   tag 0x10
//	u16  hash_version (1)
//	u32  table_id
//	u16  number of encoded columns (non-dropped columns whose value is not NULL)
//	for each such column in attribute order (attnum ascending):
//	    u16  length in bytes of the column name, then the name in UTF-8
//	    u32  length in bytes of the value, then the value
//
// cols must already be in attribute order and must not contain NULL columns.
func EncodeRow(tableID uint32, cols []Column) ([]byte, error) {
	if len(cols) > maxColumns {
		return nil, fmt.Errorf("%w: %d columns, expected at most %d", ErrEncoding, len(cols), maxColumns)
	}
	b := make([]byte, 0, 9+len(cols)*16)
	b = append(b, TagRow)
	b = binary.BigEndian.AppendUint16(b, HashVersion)
	b = binary.BigEndian.AppendUint32(b, tableID)
	b = binary.BigEndian.AppendUint16(b, uint16(len(cols))) // #nosec G115 -- bounded above
	for i, c := range cols {
		if !utf8.ValidString(c.Name) {
			return nil, fmt.Errorf("%w: column %d name is not valid UTF-8", ErrEncoding, i)
		}
		if len(c.Name) > maxNameLen {
			return nil, fmt.Errorf("%w: column %d name has %d bytes, expected at most %d", ErrEncoding, i, len(c.Name), maxNameLen)
		}
		if uint64(len(c.Value)) > maxValueLen {
			return nil, fmt.Errorf("%w: column %d value has %d bytes, expected at most %d", ErrEncoding, i, len(c.Value), uint64(maxValueLen))
		}
		b = binary.BigEndian.AppendUint16(b, uint16(len(c.Name))) // #nosec G115 -- bounded above
		b = append(b, c.Name...)
		b = binary.BigEndian.AppendUint32(b, uint32(len(c.Value))) // #nosec G115 -- bounded above
		b = append(b, c.Value...)
	}
	return b, nil
}

// RowHash is leaf_hash of the canonical row encoding ("row_hash =
// leaf_hash(encoding)").
func RowHash(tableID uint32, cols []Column) (merkle.Hash, error) {
	enc, err := EncodeRow(tableID, cols)
	if err != nil {
		return merkle.Hash{}, err
	}
	return merkle.LeafHash(enc), nil
}

// EntryLeafData returns the leaf data of an entry in a transaction tree.
// README: "leaf_hash(0x11 || u32 seq || u32 table_id || u8 op || row_hash)
// with op = 0x49 ('I')".
func EntryLeafData(seq, tableID uint32, op byte, rowHash merkle.Hash) []byte {
	b := make([]byte, 0, 42)
	b = append(b, TagEntry)
	b = binary.BigEndian.AppendUint32(b, seq)
	b = binary.BigEndian.AppendUint32(b, tableID)
	b = append(b, op)
	return append(b, rowHash[:]...)
}

// EntryLeafHash is leaf_hash of EntryLeafData.
func EntryLeafHash(seq, tableID uint32, op byte, rowHash merkle.Hash) merkle.Hash {
	return merkle.LeafHash(EntryLeafData(seq, tableID, op, rowHash))
}

// BlockTxLeafData returns the leaf data of a transaction in a block tree.
// README: "leaf_hash(0x12 || u64 ledger_txid || u32 entry_count ||
// merkle_root || u16 n || user_name) where user_name is the name of the user
// who wrote the transaction in UTF-8 and n is its length in bytes (at most
// 255)".
func BlockTxLeafData(ledgerTxID uint64, entryCount uint32, txRoot merkle.Hash, userName string) ([]byte, error) {
	if len(userName) > maxUserName {
		return nil, fmt.Errorf("%w: user name has %d bytes, expected at most %d", ErrEncoding, len(userName), maxUserName)
	}
	if !utf8.ValidString(userName) {
		return nil, fmt.Errorf("%w: user name is not valid UTF-8", ErrEncoding)
	}
	b := make([]byte, 0, 47+len(userName))
	b = append(b, TagBlockTx)
	b = binary.BigEndian.AppendUint64(b, ledgerTxID)
	b = binary.BigEndian.AppendUint32(b, entryCount)
	b = append(b, txRoot[:]...)
	b = binary.BigEndian.AppendUint16(b, uint16(len(userName))) // #nosec G115 -- bounded above
	return append(b, userName...), nil
}

// BlockTxLeafHash is leaf_hash of BlockTxLeafData.
func BlockTxLeafHash(ledgerTxID uint64, entryCount uint32, txRoot merkle.Hash, userName string) (merkle.Hash, error) {
	d, err := BlockTxLeafData(ledgerTxID, entryCount, txRoot, userName)
	if err != nil {
		return merkle.Hash{}, err
	}
	return merkle.LeafHash(d), nil
}

// Header is a block header: the fields the block hash covers, plus the hash.
type Header struct {
	Height       uint64
	HashVersion  uint16
	PrevHash     merkle.Hash
	TxMerkleRoot merkle.Hash
	TxCount      uint32
	ClosedAtUS   int64 // microseconds since 2000-01-01 00:00:00 UTC
	BlockHash    merkle.Hash
}

// BlockHashInput returns the input of the block hash. README:
//
//	block_hash = SHA256(0x13 || u16 hash_version || u64 height || prev_hash
//	                    || tx_merkle_root || u32 tx_count || i64 closed_at)
//
// where closed_at is the timestamp in microseconds since 2000-01-01
// 00:00:00 UTC.
func (h Header) BlockHashInput() []byte {
	b := make([]byte, 0, 87)
	b = append(b, TagBlock)
	b = binary.BigEndian.AppendUint16(b, h.HashVersion)
	b = binary.BigEndian.AppendUint64(b, h.Height)
	b = append(b, h.PrevHash[:]...)
	b = append(b, h.TxMerkleRoot[:]...)
	b = binary.BigEndian.AppendUint32(b, h.TxCount)
	return binary.BigEndian.AppendUint64(b, uint64(h.ClosedAtUS)) // #nosec G115 -- two's complement i64
}

// ComputeHash returns SHA-256 of BlockHashInput. It does not look at
// h.BlockHash.
func (h Header) ComputeHash() merkle.Hash {
	return sha256.Sum256(h.BlockHashInput())
}

// GenesisHash is the prev_hash of block 0: README "SHA256(0x14 || chain_id)".
func GenesisHash(chainID [chainIDBytes]byte) merkle.Hash {
	b := make([]byte, 0, 1+chainIDBytes)
	b = append(b, TagChain)
	b = append(b, chainID[:]...)
	return sha256.Sum256(b)
}

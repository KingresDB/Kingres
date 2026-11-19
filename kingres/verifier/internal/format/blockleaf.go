// SPDX-License-Identifier: AGPL-3.0-only

package format

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/merkle"
)

// BlockLeafSize is the length of the leaf data of a chain log: the 87 bytes the block hash
// covers (ledger-phase7.md 4.3).
const BlockLeafSize = 87

// ParseBlockLeaf reads the leaf data of the block entry of a chain log: tag 0x13, u16
// hash_version, u64 height, prev_hash, tx_merkle_root, u32 tx_count, i64 closed_at. The data must
// be exactly 87 bytes with the tag and the version of this format: a leaf that is not that
// is refused, never read as far as it goes. BlockHash of the result is SHA-256 of the data,
// which is what the block hash is by definition.
func ParseBlockLeaf(data []byte) (Header, error) {
	if len(data) != BlockLeafSize {
		return Header{}, fmt.Errorf("a block entry has %d bytes, expected %d", len(data), BlockLeafSize)
	}
	if data[0] != TagBlock {
		return Header{}, fmt.Errorf("a block entry starts with 0x%02x, expected the block tag 0x%02x", data[0], TagBlock)
	}
	h := Header{
		HashVersion: binary.BigEndian.Uint16(data[1:3]),
		Height:      binary.BigEndian.Uint64(data[3:11]),
		TxCount:     binary.BigEndian.Uint32(data[75:79]),
		ClosedAtUS:  int64(binary.BigEndian.Uint64(data[79:87])), // #nosec G115 -- two's complement i64
	}
	if h.HashVersion != HashVersion {
		return Header{}, fmt.Errorf("a block entry has hash_version %d, expected %d", h.HashVersion, HashVersion)
	}
	copy(h.PrevHash[:], data[11:43])
	copy(h.TxMerkleRoot[:], data[43:75])
	h.BlockHash = merkle.Hash(sha256.Sum256(data))
	return h, nil
}

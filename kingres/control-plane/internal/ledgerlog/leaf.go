// SPDX-License-Identifier: AGPL-3.0-only

package ledgerlog

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
)

// Tags of leaf data (design 4.3 and 5.2). 0x13 is the tag the extension gives a
// block header, and 0x23 avoids 0x00, 0x01 and 0x10 to 0x14.
const (
	TagBlock     byte = 0x13
	TagAggregate byte = 0x23
)

// ErrLeaf marks leaf data that is not valid.
var ErrLeaf = errors.New("ledgerlog: the leaf data is not valid")

func leafErr(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrLeaf}, args...)...)
}

// ---- the block entry of a chain log (design 4.3) ----

// BlockEntrySize is the size of the leaf data of a block: the 87 bytes that the
// block hash covers.
const BlockEntrySize = 87

// BlockEntry is the leaf data of the block of one height of a chain:
//
//	u8   0x13
//	u16  hash_version (1)
//	u64  height
//	[32] prev_hash
//	[32] tx_merkle_root
//	u32  tx_count
//	i64  closed_at, microseconds since 2000-01-01 00:00:00 UTC
//
// Integers are big endian. The block hash is SHA-256 of these bytes and the
// leaf hash is SHA-256(0x00 || these bytes).
type BlockEntry struct {
	HashVersion  uint16
	Height       uint64
	PrevHash     [32]byte
	TxMerkleRoot [32]byte
	TxCount      uint32
	ClosedAtUS   int64
}

// Marshal writes the 87 bytes. It refuses a hash version other than 1 and a
// height that is not below 2^62 (it is a leaf index).
func (b BlockEntry) Marshal() ([]byte, error) {
	if b.HashVersion != 1 {
		return nil, leafErr("hash version %d, expected 1", b.HashVersion)
	}
	if b.Height >= MaxTreeSize {
		return nil, leafErr("height %d is too large", b.Height)
	}
	out := make([]byte, 0, BlockEntrySize)
	out = append(out, TagBlock)
	out = binary.BigEndian.AppendUint16(out, b.HashVersion)
	out = binary.BigEndian.AppendUint64(out, b.Height)
	out = append(out, b.PrevHash[:]...)
	out = append(out, b.TxMerkleRoot[:]...)
	out = binary.BigEndian.AppendUint32(out, b.TxCount)
	out = binary.BigEndian.AppendUint64(out, uint64(b.ClosedAtUS))
	return out, nil
}

// ParseBlockEntry parses the leaf data of a block: exactly 87 bytes, tag 0x13,
// hash version 1, a height below 2^62. Marshal of the result gives the input
// back.
func ParseBlockEntry(data []byte) (BlockEntry, error) {
	if len(data) != BlockEntrySize {
		return BlockEntry{}, leafErr("%d bytes, expected %d", len(data), BlockEntrySize)
	}
	if data[0] != TagBlock {
		return BlockEntry{}, leafErr("tag 0x%02x, expected 0x%02x", data[0], TagBlock)
	}
	var b BlockEntry
	b.HashVersion = binary.BigEndian.Uint16(data[1:3])
	if b.HashVersion != 1 {
		return BlockEntry{}, leafErr("hash version %d, expected 1", b.HashVersion)
	}
	b.Height = binary.BigEndian.Uint64(data[3:11])
	if b.Height >= MaxTreeSize {
		return BlockEntry{}, leafErr("height %d is too large", b.Height)
	}
	copy(b.PrevHash[:], data[11:43])
	copy(b.TxMerkleRoot[:], data[43:75])
	b.TxCount = binary.BigEndian.Uint32(data[75:79])
	b.ClosedAtUS = int64(binary.BigEndian.Uint64(data[79:87]))
	return b, nil
}

// BlockHash is SHA-256 of the leaf data: the block hash of the chain.
func (b BlockEntry) BlockHash() ([32]byte, error) {
	d, err := b.Marshal()
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(d), nil
}

// ErrChainLink marks a block that does not follow the previous one.
var ErrChainLink = errors.New("ledgerlog: the block does not follow the previous block")

// CheckLink checks that next follows prev in a chain: its height is one more and
// its prev_hash is the block hash of prev (design 4.5 step 2, 4.7).
func CheckLink(prev, next BlockEntry) error {
	if next.Height != prev.Height+1 {
		return fmt.Errorf("%w: height %d after height %d", ErrChainLink, next.Height, prev.Height)
	}
	h, err := prev.BlockHash()
	if err != nil {
		return err
	}
	if next.PrevHash != h {
		return fmt.Errorf("%w: prev_hash of height %d is not the hash of height %d", ErrChainLink, next.Height, prev.Height)
	}
	return nil
}

// ---- the aggregate entry (design 5.2) ----

// Sizes and limits of the aggregate leaf.
const (
	// AggregateFormat is the only format of the aggregate leaf.
	AggregateFormat = 1
	// AggregateHeaderSize is the size of the header of the leaf.
	AggregateHeaderSize = 1 + 2 + 8 + 8 + 2
	// AggregateMemberSize is the size of one member.
	AggregateMemberSize = 1 + 32 + 4 + 8 + 32 + 32
	// MaxMembers is the most members of one leaf.
	MaxMembers = 512
	// MaxAggregateSize is the size of the largest leaf, 21 + 512 * 109 bytes,
	// which fits the u16 length of an entry bundle.
	MaxAggregateSize = AggregateHeaderSize + MaxMembers*AggregateMemberSize
)

// Flags of an aggregate member.
const (
	FlagStale    byte = 1 << 0
	FlagRetired  byte = 1 << 1
	FlagDiverged byte = 1 << 2
	flagsMask    byte = FlagStale | FlagRetired | FlagDiverged
)

// AggregateMember is the head of one chain log in one round.
type AggregateMember struct {
	Flags     byte
	ChainID   [32]byte
	Epoch     uint32
	Height    uint64
	BlockHash [32]byte
	LogRoot   [32]byte
}

// AggregateEntry is the leaf data of an aggregation round:
//
//	u8   0x23
//	u16  format (1)
//	u64  seq                  the leaf index
//	i64  generated_at_us      microseconds since 2000-01-01 UTC
//	u16  member_count         1 to 512
//	members, sorted by (chain_id, epoch) ascending as byte strings:
//	  u8 flags, [32] chain_id, u32 epoch, u64 height, [32] block_hash, [32] log_root
//
// Integers are big endian. The encoding is canonical: Marshal sorts a copy of
// the members, and ParseAggregateEntry accepts only members in ascending order
// with no (chain_id, epoch) twice, flags within the three defined bits, an
// epoch of at least 1, a height below 2^62 and exactly the length the member
// count gives. Every input has one outcome (a leaf or an error), and Marshal
// of a parsed leaf gives the input back.
type AggregateEntry struct {
	Seq           uint64
	GeneratedAtUS int64
	Members       []AggregateMember
}

func memberKey(m AggregateMember) [36]byte {
	var k [36]byte
	copy(k[:32], m.ChainID[:])
	binary.BigEndian.PutUint32(k[32:], m.Epoch)
	return k
}

func checkMember(m AggregateMember) error {
	if m.Flags&^flagsMask != 0 {
		return leafErr("flags 0x%02x use bits that are not defined", m.Flags)
	}
	if m.Epoch == 0 {
		return leafErr("an epoch starts at 1")
	}
	if m.Height >= MaxTreeSize {
		return leafErr("height %d is too large", m.Height)
	}
	return nil
}

// Marshal writes the leaf data. It sorts the members, and refuses no member,
// more than MaxMembers, a repeated (chain_id, epoch), an invalid member, and a
// seq that is not a leaf index.
func (a AggregateEntry) Marshal() ([]byte, error) {
	if len(a.Members) < 1 || len(a.Members) > MaxMembers {
		return nil, leafErr("%d members, expected 1 to %d", len(a.Members), MaxMembers)
	}
	if a.Seq >= MaxTreeSize {
		return nil, leafErr("seq %d is too large", a.Seq)
	}
	ms := append([]AggregateMember(nil), a.Members...)
	sort.Slice(ms, func(i, j int) bool {
		ki, kj := memberKey(ms[i]), memberKey(ms[j])
		return bytes.Compare(ki[:], kj[:]) < 0
	})
	out := make([]byte, 0, AggregateHeaderSize+len(ms)*AggregateMemberSize)
	out = append(out, TagAggregate)
	out = binary.BigEndian.AppendUint16(out, AggregateFormat)
	out = binary.BigEndian.AppendUint64(out, a.Seq)
	out = binary.BigEndian.AppendUint64(out, uint64(a.GeneratedAtUS))
	out = binary.BigEndian.AppendUint16(out, uint16(len(ms)))
	for i, m := range ms {
		if err := checkMember(m); err != nil {
			return nil, err
		}
		if i > 0 && memberKey(ms[i-1]) == memberKey(m) {
			return nil, leafErr("the chain %x epoch %d appears twice", m.ChainID[:8], m.Epoch)
		}
		out = append(out, m.Flags)
		out = append(out, m.ChainID[:]...)
		out = binary.BigEndian.AppendUint32(out, m.Epoch)
		out = binary.BigEndian.AppendUint64(out, m.Height)
		out = append(out, m.BlockHash[:]...)
		out = append(out, m.LogRoot[:]...)
	}
	return out, nil
}

// ParseAggregateEntry parses the leaf data of an aggregation round.
func ParseAggregateEntry(data []byte) (AggregateEntry, error) {
	if len(data) < AggregateHeaderSize {
		return AggregateEntry{}, leafErr("%d bytes, expected at least %d", len(data), AggregateHeaderSize)
	}
	if data[0] != TagAggregate {
		return AggregateEntry{}, leafErr("tag 0x%02x, expected 0x%02x", data[0], TagAggregate)
	}
	if f := binary.BigEndian.Uint16(data[1:3]); f != AggregateFormat {
		return AggregateEntry{}, leafErr("format %d, expected %d", f, AggregateFormat)
	}
	a := AggregateEntry{
		Seq:           binary.BigEndian.Uint64(data[3:11]),
		GeneratedAtUS: int64(binary.BigEndian.Uint64(data[11:19])),
	}
	if a.Seq >= MaxTreeSize {
		return AggregateEntry{}, leafErr("seq %d is too large", a.Seq)
	}
	n := int(binary.BigEndian.Uint16(data[19:21]))
	if n < 1 || n > MaxMembers {
		return AggregateEntry{}, leafErr("%d members, expected 1 to %d", n, MaxMembers)
	}
	if len(data) != AggregateHeaderSize+n*AggregateMemberSize {
		return AggregateEntry{}, leafErr("%d bytes for %d members, expected %d", len(data), n, AggregateHeaderSize+n*AggregateMemberSize)
	}
	a.Members = make([]AggregateMember, n)
	rest := data[AggregateHeaderSize:]
	for i := range a.Members {
		m := &a.Members[i]
		m.Flags = rest[0]
		copy(m.ChainID[:], rest[1:33])
		m.Epoch = binary.BigEndian.Uint32(rest[33:37])
		m.Height = binary.BigEndian.Uint64(rest[37:45])
		copy(m.BlockHash[:], rest[45:77])
		copy(m.LogRoot[:], rest[77:109])
		rest = rest[AggregateMemberSize:]
		if err := checkMember(*m); err != nil {
			return AggregateEntry{}, err
		}
		if i > 0 {
			p, c := memberKey(a.Members[i-1]), memberKey(*m)
			switch cmp := bytes.Compare(p[:], c[:]); {
			case cmp == 0:
				return AggregateEntry{}, leafErr("the chain %x epoch %d appears twice", m.ChainID[:8], m.Epoch)
			case cmp > 0:
				return AggregateEntry{}, leafErr("the members are not sorted by chain id and epoch")
			}
		}
	}
	return a, nil
}

// AggregateID returns the identifier of the aggregate log of a database:
// SHA-256("kingres-aggregate-id" || 0x00 || cluster_id || database name in
// UTF-8) (design 4.2).
func AggregateID(clusterID [32]byte, database string) [32]byte {
	h := sha256.New()
	h.Write([]byte("kingres-aggregate-id"))
	h.Write([]byte{0})
	h.Write(clusterID[:])
	h.Write([]byte(database))
	var out [32]byte
	h.Sum(out[:0])
	return out
}

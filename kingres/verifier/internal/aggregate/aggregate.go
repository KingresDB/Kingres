// SPDX-License-Identifier: AGPL-3.0-only

// Package aggregate reads the leaf of an aggregate log (ledger-phase7.md 5.2). It is written from
// that text: integers big endian, a tag 0x23, a u16 format, a u64 seq that equals the leaf
// index, the leader's clock, 1 to 512 members sorted by (chain_id, epoch) as byte strings,
// each member 109 bytes. A leaf that is not exactly that, with a member out of order, a duplicate,
// a flag the format does not define or an epoch of 0, is refused.
package aggregate

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// Layout constants of the leaf.
const (
	Tag          byte = 0x23
	Format            = 1
	HeaderSize        = 21 // tag, format, seq, generated_at, member_count
	MemberSize        = 109
	MaxMembers        = 512
	MaxLeafSize       = HeaderSize + MaxMembers*MemberSize // 55 829: inside the u16 limit of an entry
	FlagStale    byte = 1 << 0
	FlagRetired  byte = 1 << 1
	FlagDiverged byte = 1 << 2
	flagsDefined      = FlagStale | FlagRetired | FlagDiverged
)

// Member is one chain of the database in one aggregation round.
type Member struct {
	Flags     byte
	ChainID   [32]byte
	Epoch     uint32
	Height    uint64 // latest block height at the time (the chain has Height + 1 blocks)
	BlockHash [32]byte
	LogRoot   [32]byte // root of the chain log at size Height + 1, under that epoch
}

// Stale reports whether the group could not be read in the round (the member repeats the last good head).
func (m Member) Stale() bool { return m.Flags&FlagStale != 0 }

// Retired reports whether this is the last leaf in which the chain appears.
func (m Member) Retired() bool { return m.Flags&FlagRetired != 0 }

// Diverged reports whether the chain log is stopped (the member repeats the last good head).
func (m Member) Diverged() bool { return m.Flags&FlagDiverged != 0 }

// Size is the size of the chain log the member names.
func (m Member) Size() uint64 { return m.Height + 1 }

// Leaf is one leaf of the aggregate log.
type Leaf struct {
	Seq           uint64
	GeneratedAtUS int64 // microseconds since 2000-01-01 UTC, the leader's clock
	Members       []Member
}

// ErrFormat is wrapped by every refusal of Parse.
var ErrFormat = errors.New("not an aggregate leaf")

func bad(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrFormat, fmt.Sprintf(format, args...))
}

// Parse reads the leaf data of an aggregate entry.
func Parse(data []byte) (*Leaf, error) {
	if len(data) < HeaderSize {
		return nil, bad("%d bytes, expected at least %d", len(data), HeaderSize)
	}
	if data[0] != Tag {
		return nil, bad("starts with 0x%02x, expected the aggregate tag 0x%02x", data[0], Tag)
	}
	if f := binary.BigEndian.Uint16(data[1:3]); f != Format {
		return nil, bad("format %d, expected %d", f, Format)
	}
	l := &Leaf{
		Seq:           binary.BigEndian.Uint64(data[3:11]),
		GeneratedAtUS: int64(binary.BigEndian.Uint64(data[11:19])), // #nosec G115 -- two's complement i64
	}
	n := int(binary.BigEndian.Uint16(data[19:21]))
	if n < 1 || n > MaxMembers {
		return nil, bad("member_count is %d, expected 1 to %d", n, MaxMembers)
	}
	if want := HeaderSize + n*MemberSize; len(data) != want {
		return nil, bad("%d bytes for %d members, expected exactly %d", len(data), n, want)
	}
	rest := data[HeaderSize:]
	var prev []byte
	for i := 0; i < n; i++ {
		b := rest[i*MemberSize : (i+1)*MemberSize]
		m := Member{Flags: b[0]}
		if m.Flags&^flagsDefined != 0 {
			return nil, bad("member %d has flags 0x%02x, expected only bits 0 to 2", i, m.Flags)
		}
		copy(m.ChainID[:], b[1:33])
		m.Epoch = binary.BigEndian.Uint32(b[33:37])
		m.Height = binary.BigEndian.Uint64(b[37:45])
		copy(m.BlockHash[:], b[45:77])
		copy(m.LogRoot[:], b[77:109])
		if m.Epoch == 0 {
			return nil, bad("member %d has epoch 0, expected epochs from 1", i)
		}
		if m.Height >= 1<<62-1 {
			return nil, bad("member %d has height %d, expected a height below 2^62 - 1 (the tree size height+1 must stay below 2^62)", i, m.Height)
		}
		key := b[1:37] // chain_id then epoch, big endian: the order of byte strings
		if prev != nil {
			switch c := bytes.Compare(prev, key); {
			case c == 0:
				return nil, bad("member %d repeats the chain and epoch of the member before it", i)
			case c > 0:
				return nil, bad("member %d is out of order: the members are sorted by (chain_id, epoch)", i)
			}
		}
		prev = key
		l.Members = append(l.Members, m)
	}
	return l, nil
}

// Encode writes the leaf data. It is for tests and for the vectors: the members must be in order.
func (l *Leaf) Encode() []byte {
	b := []byte{Tag}
	b = binary.BigEndian.AppendUint16(b, Format)
	b = binary.BigEndian.AppendUint64(b, l.Seq)
	b = binary.BigEndian.AppendUint64(b, uint64(l.GeneratedAtUS)) // #nosec G115 -- two's complement i64
	b = binary.BigEndian.AppendUint16(b, uint16(len(l.Members)))  // #nosec G115 -- a test passes at most 65535
	for _, m := range l.Members {
		b = append(b, m.Flags)
		b = append(b, m.ChainID[:]...)
		b = binary.BigEndian.AppendUint32(b, m.Epoch)
		b = binary.BigEndian.AppendUint64(b, m.Height)
		b = append(b, m.BlockHash[:]...)
		b = append(b, m.LogRoot[:]...)
	}
	return b
}

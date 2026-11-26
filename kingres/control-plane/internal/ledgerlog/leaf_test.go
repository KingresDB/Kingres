// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ledgerlog

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math/rand"
	"reflect"
	"sort"
	"testing"
)

func someBlock(h uint64) BlockEntry {
	var b BlockEntry
	b.HashVersion, b.Height, b.TxCount, b.ClosedAtUS = 1, h, uint32(h*3+1), int64(h)*1_000_000-5
	for i := range b.PrevHash {
		b.PrevHash[i] = byte(h) + byte(i)
		b.TxMerkleRoot[i] = byte(h) ^ byte(i*7)
	}
	return b
}

func TestBlockEntryLayout(t *testing.T) {
	b := someBlock(0x0102030405060708 >> 8)
	b.ClosedAtUS = -2
	d, err := b.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if len(d) != 87 || d[0] != 0x13 || d[1] != 0 || d[2] != 1 {
		t.Fatalf("header %x", d[:3])
	}
	if binary.BigEndian.Uint64(d[3:11]) != b.Height {
		t.Fatal("height")
	}
	if !bytes.Equal(d[11:43], b.PrevHash[:]) || !bytes.Equal(d[43:75], b.TxMerkleRoot[:]) {
		t.Fatal("hashes")
	}
	if binary.BigEndian.Uint32(d[75:79]) != b.TxCount {
		t.Fatal("tx count")
	}
	if int64(binary.BigEndian.Uint64(d[79:87])) != -2 {
		t.Fatal("closed_at is a signed 64 bit integer")
	}
	// The block hash is SHA-256 of the bytes; the leaf hash has the 0x00 prefix.
	bh, _ := b.BlockHash()
	if bh != sha256.Sum256(d) {
		t.Fatal("block hash")
	}
	if LeafHash(d) != sha256.Sum256(append([]byte{0}, d...)) {
		t.Fatal("leaf hash")
	}
	back, err := ParseBlockEntry(d)
	if err != nil || back != b {
		t.Fatalf("%+v %v", back, err)
	}
	// A known vector written out by hand: all fields zero but the version.
	var z BlockEntry
	z.HashVersion = 1
	zd, _ := z.Marshal()
	want := "13" + "0001" + "0000000000000000" + hex.EncodeToString(make([]byte, 64)) + "00000000" + "0000000000000000"
	if hex.EncodeToString(zd) != want {
		t.Fatalf("%x", zd)
	}
}

func TestBlockEntryRefusals(t *testing.T) {
	good, _ := someBlock(5).Marshal()
	mut := func(f func(b []byte) []byte) []byte { return f(bytes.Clone(good)) }
	for name, in := range map[string][]byte{
		"empty":       nil,
		"86 bytes":    good[:86],
		"88 bytes":    append(bytes.Clone(good), 0),
		"wrong tag":   mut(func(b []byte) []byte { b[0] = 0x12; return b }),
		"version 0":   mut(func(b []byte) []byte { b[2] = 0; return b }),
		"version 2":   mut(func(b []byte) []byte { b[2] = 2; return b }),
		"version 257": mut(func(b []byte) []byte { b[1] = 1; return b }),
		"height 2^62": mut(func(b []byte) []byte { binary.BigEndian.PutUint64(b[3:], 1<<62); return b }),
		"height max":  mut(func(b []byte) []byte { binary.BigEndian.PutUint64(b[3:], ^uint64(0)); return b }),
	} {
		if _, err := ParseBlockEntry(in); !errors.Is(err, ErrLeaf) {
			t.Errorf("%s: %v", name, err)
		}
	}
	bad := someBlock(5)
	bad.HashVersion = 2
	if _, err := bad.Marshal(); err == nil {
		t.Error("marshal of version 2")
	}
	bad = someBlock(5)
	bad.Height = MaxTreeSize
	if _, err := bad.Marshal(); err == nil {
		t.Error("marshal of height 2^62")
	}
	// The largest height passes.
	ok := someBlock(5)
	ok.Height = MaxTreeSize - 1
	d, err := ok.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if back, err := ParseBlockEntry(d); err != nil || back != ok {
		t.Fatal(err)
	}
}

func TestChainLink(t *testing.T) {
	a := someBlock(0)
	h, _ := a.BlockHash()
	b := someBlock(1)
	b.PrevHash = h
	if err := CheckLink(a, b); err != nil {
		t.Fatal(err)
	}
	b2 := b
	b2.PrevHash[0] ^= 1
	if err := CheckLink(a, b2); !errors.Is(err, ErrChainLink) {
		t.Fatalf("%v", err)
	}
	b3 := b
	b3.Height = 2
	if err := CheckLink(a, b3); !errors.Is(err, ErrChainLink) {
		t.Fatalf("%v", err)
	}
	if err := CheckLink(b, a); !errors.Is(err, ErrChainLink) {
		t.Fatalf("%v", err)
	}
}

func member(n int, flags byte) AggregateMember {
	var m AggregateMember
	m.Flags = flags
	binary.BigEndian.PutUint64(m.ChainID[:8], uint64(n)*0x9e3779b97f4a7c15)
	binary.BigEndian.PutUint64(m.ChainID[24:], uint64(n))
	m.Epoch = uint32(1 + n%3)
	m.Height = uint64(n) * 11
	m.BlockHash[0], m.LogRoot[1] = byte(n), byte(n>>8)
	return m
}

func TestAggregateLayout(t *testing.T) {
	a := AggregateEntry{Seq: 5, GeneratedAtUS: -7, Members: []AggregateMember{member(2, FlagStale|FlagDiverged), member(1, 0)}}
	d, err := a.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if len(d) != 21+2*109 || d[0] != 0x23 || d[1] != 0 || d[2] != 1 {
		t.Fatalf("size %d header %x", len(d), d[:3])
	}
	if binary.BigEndian.Uint64(d[3:11]) != 5 || int64(binary.BigEndian.Uint64(d[11:19])) != -7 || binary.BigEndian.Uint16(d[19:21]) != 2 {
		t.Fatal("header fields")
	}
	// Members are sorted by chain id then epoch, and a member is 109 bytes.
	m0 := d[21 : 21+109]
	m1 := d[21+109:]
	if bytes.Compare(m0[1:33], m1[1:33]) >= 0 {
		t.Fatal("members are not sorted by chain id")
	}
	if m0[0]|m1[0] != byte(FlagStale|FlagDiverged) || m0[0]&m1[0] != 0 {
		t.Fatalf("flags %x", m1[0])
	}
	back, err := ParseAggregateEntry(d)
	if err != nil {
		t.Fatal(err)
	}
	if back.Seq != 5 || back.GeneratedAtUS != -7 || len(back.Members) != 2 || !slicesContain(back.Members, member(1, 0)) {
		t.Fatalf("%+v", back)
	}
	again, _ := back.Marshal()
	if !bytes.Equal(again, d) {
		t.Fatal("serialise(parse(b)) != b")
	}
	// The sort is by the bytes of chain id, then epoch: the same chain in two
	// epochs sorts by epoch.
	x, y := member(1, 0), member(1, 0)
	x.Epoch, y.Epoch = 9, 2
	d2, err := AggregateEntry{Members: []AggregateMember{x, y}}.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	p, _ := ParseAggregateEntry(d2)
	if p.Members[0].Epoch != 2 || p.Members[1].Epoch != 9 {
		t.Fatal("epochs are not sorted")
	}
}

func TestAggregateMemberLimits(t *testing.T) {
	var ms []AggregateMember
	for i := 0; i < MaxMembers; i++ {
		ms = append(ms, member(i, byte(i)&flagsMask))
	}
	d, err := AggregateEntry{Seq: 1, Members: ms}.Marshal()
	if err != nil {
		t.Fatalf("512 members: %v", err)
	}
	if len(d) != 55829 || MaxAggregateSize != 55829 || len(d) > MaxEntrySize {
		t.Fatalf("the largest leaf has %d bytes", len(d))
	}
	back, err := ParseAggregateEntry(d)
	if err != nil || len(back.Members) != 512 {
		t.Fatalf("%v", err)
	}
	// The largest leaf fits an entry of a log, with 255 others in a bundle, and
	// the tile of the leaf hashes verifies.
	store := NewMemStore()
	l, _ := Create(bg, testOrigin(9), store)
	if _, err := l.Append(bg, [][]byte{d}); err != nil {
		t.Fatalf("append of the largest leaf: %v", err)
	}
	r, _ := NewReader(bg, store, 1)
	if e, err := r.Entry(0); err != nil || !bytes.Equal(e, d) {
		t.Fatalf("read back: %v", err)
	}
	// 513 members, none, a repeat, bad flags, epoch 0 are refused.
	if _, err := (AggregateEntry{Members: append(ms, member(1000, 0))}).Marshal(); !errors.Is(err, ErrLeaf) {
		t.Errorf("513 members: %v", err)
	}
	if _, err := (AggregateEntry{}).Marshal(); !errors.Is(err, ErrLeaf) {
		t.Errorf("0 members: %v", err)
	}
	if _, err := (AggregateEntry{Members: []AggregateMember{member(1, 0), member(1, FlagStale)}}).Marshal(); !errors.Is(err, ErrLeaf) {
		t.Errorf("a repeated member: %v", err)
	}
	if _, err := (AggregateEntry{Members: []AggregateMember{member(1, 8)}}).Marshal(); !errors.Is(err, ErrLeaf) {
		t.Errorf("an undefined flag: %v", err)
	}
	z := member(1, 0)
	z.Epoch = 0
	if _, err := (AggregateEntry{Members: []AggregateMember{z}}).Marshal(); !errors.Is(err, ErrLeaf) {
		t.Errorf("epoch 0: %v", err)
	}
	z = member(1, 0)
	z.Height = MaxTreeSize
	if _, err := (AggregateEntry{Members: []AggregateMember{z}}).Marshal(); !errors.Is(err, ErrLeaf) {
		t.Errorf("height 2^62: %v", err)
	}
	if _, err := (AggregateEntry{Seq: MaxTreeSize, Members: ms[:1]}).Marshal(); !errors.Is(err, ErrLeaf) {
		t.Errorf("seq 2^62: %v", err)
	}
}

func TestParseAggregateRefusals(t *testing.T) {
	good, _ := AggregateEntry{Seq: 2, Members: []AggregateMember{member(1, 0), member(2, 0), member(3, 0)}}.Marshal()
	mut := func(f func(b []byte) []byte) []byte { return f(bytes.Clone(good)) }
	cases := map[string][]byte{
		"empty":       nil,
		"header only": good[:21],
		"cut member":  good[:len(good)-1],
		"extra byte":  append(bytes.Clone(good), 0),
		"wrong tag":   mut(func(b []byte) []byte { b[0] = 0x13; return b }),
		"format 0":    mut(func(b []byte) []byte { b[2] = 0; return b }),
		"format 2":    mut(func(b []byte) []byte { b[2] = 2; return b }),
		"count 0":     mut(func(b []byte) []byte { b[20] = 0; return b[:21] }),
		"count 513":   mut(func(b []byte) []byte { binary.BigEndian.PutUint16(b[19:], 513); return b }),
		"count 4":     mut(func(b []byte) []byte { b[20] = 4; return b }),
		"count 2":     mut(func(b []byte) []byte { b[20] = 2; return b }),
		"seq 2^62":    mut(func(b []byte) []byte { binary.BigEndian.PutUint64(b[3:], 1<<62); return b }),
		"flag bit 3":  mut(func(b []byte) []byte { b[21] = 8; return b }),
		"flag bit 7":  mut(func(b []byte) []byte { b[21] = 0x80; return b }),
		"epoch 0":     mut(func(b []byte) []byte { binary.BigEndian.PutUint32(b[21+33:], 0); return b }),
		"height 2^62": mut(func(b []byte) []byte { binary.BigEndian.PutUint64(b[21+37:], 1<<62); return b }),
		"unsorted": mut(func(b []byte) []byte {
			x := bytes.Clone(b[21 : 21+109])
			copy(b[21:], b[21+109:21+218])
			copy(b[21+109:], x)
			return b
		}),
		"repeat": mut(func(b []byte) []byte { copy(b[21+109:], b[21:21+109]); return b }),
	}
	for name, in := range cases {
		if _, err := ParseAggregateEntry(in); !errors.Is(err, ErrLeaf) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := ParseAggregateEntry(good); err != nil {
		t.Fatal(err)
	}
}

// TestAggregateIsTotalAndCanonical: every random byte string has one outcome,
// and what parses marshals back to the same bytes; what is built marshals and
// parses to the sorted equivalent.
func TestAggregateIsTotalAndCanonical(t *testing.T) {
	rng := newRand(t)
	for i := 0; i < 300; i++ {
		n := 1 + rng.Intn(40)
		var ms []AggregateMember
		seen := map[[36]byte]bool{}
		for len(ms) < n {
			m := member(rng.Intn(60), byte(rng.Intn(8)))
			m.Epoch = uint32(1 + rng.Intn(3))
			if seen[memberKey(m)] {
				continue
			}
			seen[memberKey(m)] = true
			ms = append(ms, m)
		}
		a := AggregateEntry{Seq: uint64(rng.Intn(1000)), GeneratedAtUS: rng.Int63() - rng.Int63(), Members: ms}
		d, err := a.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		back, err := ParseAggregateEntry(d)
		if err != nil {
			t.Fatal(err)
		}
		sorted := append([]AggregateMember(nil), ms...)
		sort.Slice(sorted, func(i, j int) bool {
			ki, kj := memberKey(sorted[i]), memberKey(sorted[j])
			return bytes.Compare(ki[:], kj[:]) < 0
		})
		a.Members = sorted
		if !reflect.DeepEqual(back, a) {
			t.Fatal("parse(serialise(x)) != sorted x")
		}
		again, _ := back.Marshal()
		if !bytes.Equal(again, d) {
			t.Fatal("serialise(parse(b)) != b")
		}
		// Any shuffle of the members gives the same bytes.
		rng.Shuffle(len(ms), func(i, j int) { ms[i], ms[j] = ms[j], ms[i] })
		d2, _ := AggregateEntry{Seq: a.Seq, GeneratedAtUS: a.GeneratedAtUS, Members: ms}.Marshal()
		if !bytes.Equal(d, d2) {
			t.Fatal("the encoding depends on the order of the input")
		}
		// A random cut or flip never panics, and has a definite outcome.
		junk := bytes.Clone(d)
		junk[rng.Intn(len(junk))] ^= byte(1 + rng.Intn(255))
		if p, err := ParseAggregateEntry(junk); err == nil {
			if again, _ := p.Marshal(); !bytes.Equal(again, junk) {
				t.Fatal("an accepted mutation is not canonical")
			}
		}
	}
	for i := 0; i < 2000; i++ {
		junk := make([]byte, rng.Intn(300))
		rng.Read(junk)
		if len(junk) > 0 && rng.Intn(2) == 0 {
			junk[0] = TagAggregate
		}
		_, _ = ParseAggregateEntry(junk)
	}
	_ = rand.New
}

func TestAggregateID(t *testing.T) {
	var c [32]byte
	c[0] = 1
	a := AggregateID(c, "db")
	h := sha256.New()
	h.Write([]byte("kingres-aggregate-id\x00"))
	h.Write(c[:])
	h.Write([]byte("db"))
	if !bytes.Equal(a[:], h.Sum(nil)) {
		t.Fatal("AggregateID is not SHA-256(\"kingres-aggregate-id\" || 0x00 || cluster_id || name)")
	}
	if AggregateID(c, "db") == AggregateID(c, "db2") || AggregateID(c, "db") == AggregateID([32]byte{}, "db") {
		t.Fatal("collision")
	}
}

func slicesContain(ms []AggregateMember, m AggregateMember) bool {
	for _, x := range ms {
		if x == m {
			return true
		}
	}
	return false
}

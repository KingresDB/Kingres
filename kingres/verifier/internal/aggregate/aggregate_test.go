// SPDX-License-Identifier: AGPL-3.0-only

package aggregate

import (
	"bytes"
	"errors"
	"testing"
)

func mem(chain byte, epoch uint32, flags byte) Member {
	var m Member
	m.Flags, m.Epoch, m.Height = flags, epoch, 41
	for i := range m.ChainID {
		m.ChainID[i] = chain
		m.BlockHash[i] = 0xb0
		m.LogRoot[i] = 0xc0
	}
	return m
}

func TestRoundTripOfLeavesOfOneTwoAndManyMembers(t *testing.T) {
	for _, n := range []int{1, 2, 3, 512} {
		l := &Leaf{Seq: 9, GeneratedAtUS: -5}
		for i := 0; i < n; i++ {
			m := mem(0, uint32(i%255)+1, 0)
			m.ChainID[1], m.ChainID[2] = byte(i>>8), byte(i)
			l.Members = append(l.Members, m)
		}
		enc := l.Encode()
		if len(enc) != HeaderSize+n*MemberSize {
			t.Fatalf("%d members: %d bytes", n, len(enc))
		}
		got, err := Parse(enc)
		if err != nil {
			t.Fatalf("%d members: %v", n, err)
		}
		if got.Seq != 9 || got.GeneratedAtUS != -5 || len(got.Members) != n || !bytes.Equal(got.Encode(), enc) {
			t.Fatalf("%d members: %+v", n, got)
		}
	}
	if MaxLeafSize != 55829 || MaxLeafSize > 65535 {
		t.Fatal(MaxLeafSize)
	}
}

func TestRefusals(t *testing.T) {
	ok := (&Leaf{Members: []Member{mem(1, 1, 0), mem(2, 1, 0)}}).Encode()
	mut := func(f func(b []byte) []byte) []byte { return f(append([]byte{}, ok...)) }
	cases := map[string][]byte{
		"empty":         nil,
		"short header":  ok[:10],
		"wrong tag":     mut(func(b []byte) []byte { b[0] = 0x22; return b }),
		"wrong format":  mut(func(b []byte) []byte { b[2] = 2; return b }),
		"zero members":  mut(func(b []byte) []byte { b[19], b[20] = 0, 0; return b }),
		"513 members":   mut(func(b []byte) []byte { b[19], b[20] = 0x02, 0x01; return b }),
		"count too big": mut(func(b []byte) []byte { b[20] = 3; return b }),
		"count too low": mut(func(b []byte) []byte { b[20] = 1; return b }),
		"trailing byte": append(append([]byte{}, ok...), 0),
		"truncated":     ok[:len(ok)-1],
		"unknown flag":  mut(func(b []byte) []byte { b[HeaderSize] = 0x08; return b }),
		"epoch zero": mut(func(b []byte) []byte {
			b[HeaderSize+33], b[HeaderSize+34], b[HeaderSize+35], b[HeaderSize+36] = 0, 0, 0, 0
			return b
		}),
		"duplicate":      (&Leaf{Members: []Member{mem(1, 1, 0), mem(1, 1, 0)}}).Encode(),
		"unsorted":       (&Leaf{Members: []Member{mem(2, 1, 0), mem(1, 1, 0)}}).Encode(),
		"unsorted epoch": (&Leaf{Members: []Member{mem(1, 2, 0), mem(1, 1, 0)}}).Encode(),
		"huge height":    mut(func(b []byte) []byte { b[HeaderSize+37] = 0x7f; return b }),
	}
	for name, b := range cases {
		if _, err := Parse(b); !errors.Is(err, ErrFormat) {
			t.Errorf("%s: %v", name, err)
		}
	}
	over := &Leaf{}
	for i := 0; i < 513; i++ {
		m := mem(0, 1, 0)
		m.ChainID[0], m.ChainID[1] = byte(i>>8), byte(i)
		over.Members = append(over.Members, m)
	}
	if _, err := Parse(over.Encode()); !errors.Is(err, ErrFormat) {
		t.Errorf("513 members: %v", err)
	}
}

func TestFlagsAndSizes(t *testing.T) {
	m := mem(1, 1, FlagStale|FlagDiverged)
	if !m.Stale() || m.Retired() || !m.Diverged() || m.Size() != 42 {
		t.Fatal(m)
	}
	if !mem(1, 1, FlagRetired).Retired() {
		t.Fatal("retired")
	}
}

func FuzzParseAggregate(f *testing.F) {
	f.Add((&Leaf{Members: []Member{mem(1, 1, 0), mem(2, 3, FlagRetired)}}).Encode())
	f.Add([]byte{0x23})
	f.Fuzz(func(t *testing.T, b []byte) {
		l, err := Parse(b)
		if err != nil {
			return
		}
		if !bytes.Equal(l.Encode(), b) {
			t.Fatalf("a leaf that parses is not written back as it was: %x", b)
		}
	})
}

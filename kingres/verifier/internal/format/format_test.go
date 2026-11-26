// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package format

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestEncodeRow(t *testing.T) {
	tests := []struct {
		name    string
		tableID uint32
		cols    []Column
		want    string
	}{
		{"no columns", 7, nil, "10 0001 00000007 0000"},
		{
			"one int column", 1,
			[]Column{{"id", []byte{0, 0, 0, 1}}},
			"10 0001 00000001 0001 0002 6964 00000004 00000001",
		},
		{
			"empty value", 258,
			[]Column{{"s", nil}},
			"10 0001 00000102 0001 0001 73 00000000",
		},
		{
			"non ASCII name, two columns", 0xffffffff,
			[]Column{{"größe", []byte{0xde, 0xad}}, {"b", []byte{0}}},
			"10 0001 ffffffff 0002 0007 6772c3b6c39f65 00000002 dead 0001 62 00000001 00",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := EncodeRow(tt.tableID, tt.cols)
			if err != nil {
				t.Fatal(err)
			}
			if want := mustHex(t, tt.want); !bytes.Equal(got, want) {
				t.Errorf("encoding\n got %x\nwant %x", got, want)
			}
			h, err := RowHash(tt.tableID, tt.cols)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(append([]byte{0}, mustHex(t, tt.want)...))
			if h != sum {
				t.Errorf("RowHash = %x, want leaf hash %x", h, sum)
			}
		})
	}
}

func TestEncodeRowRejects(t *testing.T) {
	tests := []struct {
		name string
		cols []Column
	}{
		{"invalid UTF-8 name", []Column{{"\xff", nil}}},
		{"name too long", []Column{{strings.Repeat("a", 1<<16), nil}}},
		{"too many columns", make([]Column, 1<<16)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := EncodeRow(1, tt.cols); err == nil {
				t.Error("want an error")
			}
		})
	}
}

func TestEntryLeafData(t *testing.T) {
	var rh [32]byte
	for i := range rh {
		rh[i] = byte(i)
	}
	got := EntryLeafData(0x01020304, 0x0a0b0c0d, OpInsert, rh)
	want := mustHex(t, "11 01020304 0a0b0c0d 49 000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	if !bytes.Equal(got, want) {
		t.Errorf("got %x, want %x", got, want)
	}
	if len(got) != 42 {
		t.Errorf("length %d, want 42", len(got))
	}
}

func TestBlockTxLeafData(t *testing.T) {
	var root [32]byte
	for i := range root {
		root[i] = 0xa0 + byte(i%16)
	}
	rootHex := hex.EncodeToString(root[:])
	tests := []struct {
		name string
		txid uint64
		cnt  uint32
		user string
		want string
	}{
		{"ascii", 1234, 5, "alice", "12 00000000000004d2 00000005 " + rootHex + " 0005 616c696365"},
		{"empty name", 1, 1, "", "12 0000000000000001 00000001 " + rootHex + " 0000"},
		{"UTF-8 name", 1<<63 - 1, 1, "zoé", "12 7fffffffffffffff 00000001 " + rootHex + " 0004 7a6fc3a9"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := BlockTxLeafData(tt.txid, tt.cnt, root, tt.user)
			if err != nil {
				t.Fatal(err)
			}
			if want := mustHex(t, tt.want); !bytes.Equal(got, want) {
				t.Errorf("got %x, want %x", got, want)
			}
		})
	}
	if _, err := BlockTxLeafData(1, 1, root, strings.Repeat("a", 256)); err == nil {
		t.Error("256 byte user name: want an error")
	}
	if _, err := BlockTxLeafData(1, 1, root, strings.Repeat("a", 255)); err != nil {
		t.Errorf("255 byte user name: %v", err)
	}
	if _, err := BlockTxLeafData(1, 1, root, "\xc3"); err == nil {
		t.Error("invalid UTF-8 user name: want an error")
	}
}

func TestBlockHashInput(t *testing.T) {
	var prev, root [32]byte
	for i := range prev {
		prev[i] = 0x01
		root[i] = 0x02
	}
	h := Header{Height: 258, HashVersion: 1, PrevHash: prev, TxMerkleRoot: root, TxCount: 3, ClosedAtUS: -2}
	want := mustHex(t, "13 0001 0000000000000102 "+strings.Repeat("01", 32)+" "+strings.Repeat("02", 32)+" 00000003 fffffffffffffffe")
	if got := h.BlockHashInput(); !bytes.Equal(got, want) {
		t.Errorf("got %x\nwant %x", got, want)
	}
	if got, want := h.ComputeHash(), sha256.Sum256(want); got != want {
		t.Errorf("ComputeHash = %x, want %x", got, want)
	}
}

func TestGenesisHash(t *testing.T) {
	var id [32]byte
	for i := range id {
		id[i] = byte(i)
	}
	want := sha256.Sum256(append([]byte{0x14}, id[:]...))
	if got := GenesisHash(id); got != want {
		t.Errorf("got %x, want %x", got, want)
	}
}

func TestDecodeHex(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{"00ff", "00ff", false},
		{"abcdef0123456789", "abcdef0123456789", false},
		{"0", "", true},
		{"0G", "", true},
		{"AB", "", true},
		{"0x00", "", true},
		{"00 ", "", true},
		{"+0", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := DecodeHex(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && hex.EncodeToString(got) != tt.want {
				t.Errorf("got %x, want %s", got, tt.want)
			}
		})
	}
	if _, err := DecodeHash(strings.Repeat("0", 62)); err == nil {
		t.Error("31 byte hash: want an error")
	}
	if _, err := DecodeHash(strings.Repeat("0", 66)); err == nil {
		t.Error("33 byte hash: want an error")
	}
	if _, err := DecodeHash(strings.Repeat("a", 64)); err != nil {
		t.Errorf("32 byte hash: %v", err)
	}
	if EncodeHex([]byte{0, 0xab, 0xff}) != "00abff" {
		t.Error("EncodeHex")
	}
}

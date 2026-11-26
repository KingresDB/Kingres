// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package format

import (
	"bytes"
	"testing"
)

func TestBlockLeafIsTheBlockHashPreimage(t *testing.T) {
	h := Header{Height: 7, HashVersion: 1, TxCount: 3, ClosedAtUS: -12}
	h.PrevHash[0], h.TxMerkleRoot[31] = 9, 8
	data := h.BlockHashInput()
	if len(data) != BlockLeafSize {
		t.Fatal(len(data))
	}
	got, err := ParseBlockLeaf(data)
	if err != nil {
		t.Fatal(err)
	}
	h.BlockHash = h.ComputeHash()
	if got != h {
		t.Fatalf("%+v != %+v", got, h)
	}
}

func TestBlockLeafRefusals(t *testing.T) {
	good := Header{HashVersion: 1}.BlockHashInput()
	for name, b := range map[string][]byte{
		"short":   good[:86],
		"long":    append(append([]byte{}, good...), 0),
		"tag":     append([]byte{0x14}, good[1:]...),
		"version": append(append(append([]byte{}, good[:1]...), 0, 2), good[3:]...),
		"nil":     nil,
	} {
		if _, err := ParseBlockLeaf(b); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func FuzzParseBlockLeaf(f *testing.F) {
	f.Add(Header{HashVersion: 1}.BlockHashInput())
	f.Fuzz(func(t *testing.T, b []byte) {
		h, err := ParseBlockLeaf(b)
		if err == nil && !bytes.Equal(h.BlockHashInput(), b) {
			t.Fatalf("not written back: %x", b)
		}
	})
}

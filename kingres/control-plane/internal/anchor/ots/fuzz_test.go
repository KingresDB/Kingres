// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ots

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func seedFiles(f *testing.F) {
	a, _ := filepath.Glob("testdata/third_party/opentimestamps-client-examples/*.ots")
	b, _ := filepath.Glob("testdata/third_party/opentimestamps-client-examples/*/*.ots")
	for _, p := range append(a, b...) {
		if data, err := os.ReadFile(p); err == nil {
			f.Add(data)
		}
	}
}

// FuzzParseOTS: the parser never panics, and whatever it accepts it writes
// back in a form that parses to the same bytes again.
func FuzzParseOTS(f *testing.F) {
	seedFiles(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		file, err := ParseFile(data)
		if err != nil {
			return
		}
		out, err := file.Serialize()
		if err != nil {
			t.Fatalf("accepted input does not serialise: %v", err)
		}
		again, err := ParseFile(out)
		if err != nil {
			t.Fatalf("serialised form does not parse: %v", err)
		}
		out2, err := again.Serialize()
		if err != nil || !bytes.Equal(out, out2) {
			t.Fatal("serialisation is not stable")
		}
		for _, l := range file.Root.Leaves() {
			_ = l.Att.payload()
		}
		if file.Root.Count() > MaxNodes {
			t.Fatal("node budget exceeded")
		}
	})
}

// FuzzParseFragment: fragments as calendars send them, rooted at a given message.
func FuzzParseFragment(f *testing.F) {
	for _, p := range []string{"hello-world.txt.ots", "two-calendars.txt.ots", "known-and-unknown-notary.txt.ots"} {
		data, _ := os.ReadFile("testdata/third_party/opentimestamps-client-examples/" + p)
		if file, err := ParseFile(data); err == nil {
			body, _ := file.Root.Serialize()
			f.Add(body, file.Root.Msg)
		}
	}
	f.Add([]byte{0x00}, []byte{1})
	f.Fuzz(func(t *testing.T, data, msg []byte) {
		n, err := ParseFragment(data, msg)
		if err != nil {
			return
		}
		out, err := n.Serialize()
		if err != nil {
			t.Fatalf("accepted fragment does not serialise: %v", err)
		}
		n2, err := ParseFragment(out, msg)
		if err != nil {
			t.Fatalf("serialised fragment does not parse: %v", err)
		}
		if err := n2.Merge(n); err != nil {
			t.Fatalf("a fragment does not merge with itself: %v", err)
		}
		out2, _ := n2.Serialize()
		if !bytes.Equal(out, out2) {
			t.Fatal("merge with itself changed the fragment")
		}
	})
}

// FuzzCompact: the compact target decoder never panics.
func FuzzCompact(f *testing.F) {
	f.Add(uint32(0x1d00ffff))
	f.Fuzz(func(t *testing.T, bits uint32) { _, _ = CompactToTarget(bits) })
}

// FuzzHeader: header checks never panic on any 80 byte input.
func FuzzHeader(f *testing.F) {
	f.Add(make([]byte, 80))
	f.Fuzz(func(t *testing.T, h []byte) {
		_, _ = CheckHeader(h)
		root := NewNode(make([]byte, 32))
		a, _ := NewBitcoin(1)
		root.AddAttestation(a)
		_, _ = CheckAgainstHeader(root, 1, h)
	})
}

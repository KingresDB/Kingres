// SPDX-License-Identifier: AGPL-3.0-only

package ledgerlog

import (
	"bytes"
	"testing"
)

// Each fuzz target checks that its parser does not panic and that what it
// accepts has the canonical form: serialise(parse(b)) = b.

func FuzzParseNote(f *testing.F) {
	k := testKey(f, 1)
	n1, _ := SignCheckpoint(bg, goodCheckpoint(), k)
	f.Add(n1)
	f.Add([]byte("a\n\n\u2014 n AAAA\n"))
	f.Add([]byte(""))
	f.Fuzz(func(t *testing.T, data []byte) {
		n, err := ParseNote(data)
		if err != nil {
			return
		}
		out, err := n.Marshal()
		if err != nil || !bytes.Equal(out, data) {
			t.Fatalf("serialise(parse(b)) != b: %v", err)
		}
	})
}

func FuzzParseCheckpoint(f *testing.F) {
	c := goodCheckpoint()
	text, _ := c.Text()
	f.Add(text)
	c.Extensions = []string{"x", "y z"}
	text, _ = c.Text()
	f.Add(text)
	f.Add([]byte("o\n0\n47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		c, err := ParseCheckpoint(data)
		if err != nil {
			return
		}
		out, err := c.Text()
		if err != nil || !bytes.Equal(out, data) {
			t.Fatalf("serialise(parse(b)) != b: %v", err)
		}
	})
}

func FuzzParseTile(f *testing.F) {
	d, _ := EncodeTile([]Hash{{1}, {2}})
	f.Add(d, 2)
	f.Add([]byte{}, 1)
	f.Fuzz(func(t *testing.T, data []byte, width int) {
		hs, err := ParseTile(data, width)
		if err != nil {
			return
		}
		out, err := EncodeTile(hs)
		if err != nil || !bytes.Equal(out, data) {
			t.Fatalf("serialise(parse(b)) != b: %v", err)
		}
	})
}

func FuzzParseBundle(f *testing.F) {
	d, _ := EncodeBundle([][]byte{{1, 2, 3}, {}, {4}})
	f.Add(d)
	f.Add([]byte{0, 1})
	f.Fuzz(func(t *testing.T, data []byte) {
		es, err := ParseBundle(data, 0)
		if err != nil {
			return
		}
		out, err := EncodeBundle(es)
		if err != nil || !bytes.Equal(out, data) {
			t.Fatalf("serialise(parse(b)) != b: %v", err)
		}
	})
}

func FuzzParseBlockEntry(f *testing.F) {
	d, _ := someBlock(3).Marshal()
	f.Add(d)
	f.Add(make([]byte, 87))
	f.Fuzz(func(t *testing.T, data []byte) {
		b, err := ParseBlockEntry(data)
		if err != nil {
			return
		}
		out, err := b.Marshal()
		if err != nil || !bytes.Equal(out, data) {
			t.Fatalf("serialise(parse(b)) != b: %v", err)
		}
	})
}

func FuzzParseAggregateEntry(f *testing.F) {
	d, _ := AggregateEntry{Seq: 1, Members: []AggregateMember{member(1, 0), member(2, 5)}}.Marshal()
	f.Add(d)
	f.Add([]byte{0x23, 0, 1})
	f.Fuzz(func(t *testing.T, data []byte) {
		a, err := ParseAggregateEntry(data)
		if err != nil {
			return
		}
		out, err := a.Marshal()
		if err != nil || !bytes.Equal(out, data) {
			t.Fatalf("serialise(parse(b)) != b: %v", err)
		}
	})
}

func FuzzParseFrontier(f *testing.F) {
	var fr Frontier
	for i := 0; i < 11; i++ {
		_ = fr.Append(Hash{byte(i)})
	}
	f.Add(fr.Marshal())
	f.Add([]byte{1, 0, 0, 0, 0, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		fr, err := ParseFrontier(data)
		if err != nil {
			return
		}
		if !bytes.Equal(fr.Marshal(), data) {
			t.Fatal("serialise(parse(b)) != b")
		}
		// An accepted frontier can be appended to (unless it is at the limit).
		c := fr.Clone()
		if err := c.Append(Hash{1}); err != nil && c.Size() != fr.Size() {
			t.Fatal("a failed append changed the frontier")
		}
	})
}

func FuzzParseTilePath(f *testing.F) {
	f.Add("tile/0/x001/x234/067")
	f.Add("tile/entries/000.p/255")
	f.Add("tile/63/000")
	f.Fuzz(func(t *testing.T, p string) {
		k, err := ParseTilePath(p)
		if err != nil {
			return
		}
		if k.Path() != p {
			t.Fatalf("Path(Parse(%q)) = %q", p, k.Path())
		}
	})
}

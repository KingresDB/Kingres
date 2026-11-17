// SPDX-License-Identifier: AGPL-3.0-only

package ots

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func exampleFiles(t testing.TB) []string {
	t.Helper()
	needThirdParty(t)
	a, _ := filepath.Glob("testdata/third_party/opentimestamps-client-examples/*.ots")
	b, _ := filepath.Glob("testdata/third_party/opentimestamps-client-examples/sha1/*.ots")
	all := append(a, b...)
	// the unsupported ones are tested by name elsewhere
	var out []string
	for _, f := range all {
		if filepath.Base(f) == "different-blockchains.txt.ots" {
			continue
		}
		out = append(out, f)
	}
	if len(out) < 10 {
		t.Fatalf("vectors missing: %d files", len(out))
	}
	return out
}

func mustRead(t testing.TB, p string) []byte {
	t.Helper()
	if strings.Contains(p, "third_party") {
		needThirdParty(t)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Every real file written by the reference client parses and is written back
// byte for byte.
func TestReferenceVectorsRoundTrip(t *testing.T) {
	for _, p := range exampleFiles(t) {
		b := mustRead(t, p)
		f, err := ParseFile(b)
		if err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		out, err := f.Serialize()
		if err != nil || !bytes.Equal(out, b) {
			t.Errorf("%s: re-serialisation differs (err %v)", p, err)
		}
	}
}

// The root digest of a vector is the digest of the sample file next to it.
func TestReferenceVectorsDigest(t *testing.T) {
	cases := map[string]string{
		"hello-world.txt.ots":    "hello-world.txt",
		"incomplete.txt.ots":     "incomplete.txt",
		"two-calendars.txt.ots":  "two-calendars.txt",
		"merkle1.txt.ots":        "merkle1.txt",
		"unknown-notary.txt.ots": "unknown-notary.txt",
		"empty.ots":              "empty",
	}
	for ots, src := range cases {
		f, err := ParseFile(mustRead(t, "testdata/third_party/opentimestamps-client-examples/"+ots))
		if err != nil {
			t.Fatal(ots, err)
		}
		data := mustRead(t, "testdata/third_party/opentimestamps-client-examples/"+src)
		var want []byte
		switch f.HashOp {
		case TagSHA256:
			s := sha256.Sum256(data)
			want = s[:]
		default:
			t.Fatalf("%s: unexpected hash op %d", ots, f.HashOp)
		}
		if !bytes.Equal(f.Root.Msg, want) {
			t.Errorf("%s: digest %x, want %x", ots, f.Root.Msg, want)
		}
	}
}

func TestReferenceVectorContent(t *testing.T) {
	heights := map[string]uint64{
		"hello-world.txt.ots": 358391, "bitcoin.pdf.ots": 465751, "empty.ots": 129405,
		"gdp2q25-2nd.pdf.ots": 912095, "sha1/a-or-b.ots": 466906,
	}
	for name, h := range heights {
		f, err := ParseFile(mustRead(t, "testdata/third_party/opentimestamps-client-examples/"+name))
		if err != nil {
			t.Fatal(err)
		}
		cl := BitcoinClaims(f.Root)
		if len(cl) != 1 || cl[0].Att.Height != h {
			t.Errorf("%s: claims %+v, want height %d", name, cl, h)
		}
	}
	// two calendars, pending
	f, _ := ParseFile(mustRead(t, "testdata/third_party/opentimestamps-client-examples/two-calendars.txt.ots"))
	var uris []string
	for _, l := range f.Root.Leaves() {
		uris = append(uris, l.Att.URI)
	}
	if strings.Join(uris, ",") != "https://alice.btc.calendar.opentimestamps.org,https://bob.btc.calendar.opentimestamps.org" {
		t.Errorf("uris %v", uris)
	}
	// sha1 example uses a SHA-1 file hash and ripemd/sha chains
	f, _ = ParseFile(mustRead(t, "testdata/third_party/opentimestamps-client-examples/sha1/a-or-b.ots"))
	if f.HashOp != TagSHA1 || len(f.Root.Msg) != 20 {
		t.Errorf("a-or-b: hash op %d", f.HashOp)
	}
	// the unknown notary is kept opaque
	f, _ = ParseFile(mustRead(t, "testdata/third_party/opentimestamps-client-examples/unknown-notary.txt.ots"))
	ls := f.Root.Leaves()
	if len(ls) != 1 || ls[0].Att.Kind != AttUnknown || len(ls[0].Att.Payload) == 0 {
		t.Errorf("unknown notary: %+v", ls)
	}
}

func TestRefusedByName(t *testing.T) {
	cases := []struct{ file, name string }{
		{"different-blockchains.txt.ots", "keccak256"},
		{"invalid/exceeds-max-msg-length.ots", "hexlify"},
	}
	for _, c := range cases {
		_, err := ParseFile(mustRead(t, "testdata/third_party/opentimestamps-client-examples/"+c.file))
		var ue *UnsupportedOpError
		if !errors.As(err, &ue) || ue.Name != c.name || !strings.Contains(err.Error(), c.name) {
			t.Errorf("%s: err %v, want unsupported %s", c.file, err, c.name)
		}
	}
	// unknown tag
	f, _ := ParseFile(mustRead(t, "testdata/third_party/opentimestamps-client-examples/incomplete.txt.ots"))
	b, _ := f.Serialize()
	pos := len(Magic) + 2 + 32
	b2 := append([]byte(nil), b...)
	b2[pos] = 0x42
	_, err := ParseFile(b2)
	var ue *UnsupportedOpError
	if !errors.As(err, &ue) || ue.Name != "" || ue.Tag != 0x42 {
		t.Errorf("unknown tag: %v", err)
	}
	// reverse
	b2[pos] = 0xf2
	if _, err := ParseFile(b2); !errors.As(err, &ue) || ue.Name != "reverse" {
		t.Errorf("reverse: %v", err)
	}
}

func TestInvalidExamples(t *testing.T) {
	_, err := ParseFile(mustRead(t, "testdata/third_party/opentimestamps-client-examples/invalid/bad-major-version.ots"))
	if !errors.Is(err, ErrVersion) {
		t.Errorf("bad major version: %v", err)
	}
	_, err = ParseFile(mustRead(t, "testdata/third_party/opentimestamps-client-examples/invalid/invalid-file-digest-type.ots"))
	if !errors.Is(err, ErrBadHashOp) {
		t.Errorf("bad file digest type: %v", err)
	}
}

// Every proper prefix of a real file fails, none panics; trailing bytes fail.
func TestTruncationsAndTrailing(t *testing.T) {
	for _, p := range exampleFiles(t) {
		b := mustRead(t, p)
		for i := 0; i < len(b); i++ {
			if _, err := ParseFile(b[:i]); err == nil {
				t.Fatalf("%s: prefix of %d bytes parsed", p, i)
			}
		}
		if _, err := ParseFile(append(append([]byte(nil), b...), 0)); !errors.Is(err, ErrTrailing) {
			t.Errorf("%s: trailing byte: %v", p, err)
		}
	}
}

func TestBadMagicAndVersion(t *testing.T) {
	b := mustRead(t, "testdata/third_party/opentimestamps-client-examples/hello-world.txt.ots")
	for _, i := range []int{0, 1, 10, len(Magic) - 1} {
		c := append([]byte(nil), b...)
		c[i] ^= 1
		if _, err := ParseFile(c); !errors.Is(err, ErrBadMagic) {
			t.Errorf("magic byte %d: %v", i, err)
		}
	}
	c := append([]byte(nil), b...)
	c[len(Magic)] = 2
	if _, err := ParseFile(c); !errors.Is(err, ErrVersion) {
		t.Errorf("version 2: %v", err)
	}
}

func newRoot(t *testing.T) *Node {
	t.Helper()
	d := sha256.Sum256([]byte("kingres"))
	return NewNode(d[:])
}

func pending(t *testing.T, uri string) Attestation {
	t.Helper()
	a, err := NewPending(uri)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func roundTrip(t *testing.T, root *Node) (*File, error) {
	t.Helper()
	f := &File{HashOp: TagSHA256, Root: root}
	b, err := f.Serialize()
	if err != nil {
		return nil, err
	}
	return ParseFile(b)
}

func TestDepthBoundary(t *testing.T) {
	build := func(ops int) *Node {
		root := newRoot(t)
		n := root
		for i := 0; i < ops; i++ {
			var err error
			n, err = n.Child(Op{Tag: TagAppend, Arg: []byte{1}})
			if err != nil {
				t.Fatal(err)
			}
		}
		n.AddAttestation(pending(t, "https://a.example"))
		return root
	}
	if _, err := roundTrip(t, build(MaxDepth-1)); err != nil {
		t.Errorf("%d levels: %v", MaxDepth, err)
	}
	if _, err := roundTrip(t, build(MaxDepth)); !errors.Is(err, ErrDepth) {
		t.Errorf("%d levels: %v, want ErrDepth", MaxDepth+1, err)
	}
	// parse side: serialise 256 ops by hand
	var w writer
	for i := 0; i < MaxDepth; i++ {
		w.byte(TagAppend)
		w.varbytes([]byte{1})
	}
	w.attestation(pending(t, "https://a.example"))
	d := sha256.Sum256([]byte("x"))
	if _, err := ParseFragment(w.b, d[:]); !errors.Is(err, ErrDepth) {
		t.Errorf("parse of 256 ops: %v", err)
	}
}

func TestNodeBudgetBoundary(t *testing.T) {
	build := func(n int) *Node {
		root := newRoot(t)
		for i := 0; i < n-1; i++ {
			c, err := root.Child(Op{Tag: TagAppend, Arg: []byte{byte(i), byte(i >> 8)}})
			if err != nil {
				t.Fatal(err)
			}
			c.AddAttestation(pending(t, "https://a.example"))
		}
		return root
	}
	if _, err := roundTrip(t, build(MaxNodes)); err != nil {
		t.Errorf("%d nodes: %v", MaxNodes, err)
	}
	if _, err := roundTrip(t, build(MaxNodes+1)); !errors.Is(err, ErrNodeBudget) {
		t.Errorf("%d nodes: %v", MaxNodes+1, err)
	}
	a, b := build(MaxNodes/2+10), build(MaxNodes/2+10)
	for i := range b.Edges { // make b different
		b.Edges[i].Op.Arg = []byte{0xee, byte(i), byte(i >> 8)}
	}
	// recompute is not needed for budget: messages differ but merge keys on op
	if err := a.Merge(b); !errors.Is(err, ErrNodeBudget) {
		t.Errorf("merge over budget: %v", err)
	}
}

func TestMessageLengthBoundary(t *testing.T) {
	msg := make([]byte, 32)
	if _, err := (Op{Tag: TagAppend, Arg: make([]byte, MaxMessageLen-32)}).Apply(msg); err != nil {
		t.Errorf("result of exactly %d: %v", MaxMessageLen, err)
	}
	if _, err := (Op{Tag: TagAppend, Arg: make([]byte, MaxMessageLen-31)}).Apply(msg); err == nil {
		t.Error("result of 4097 accepted")
	}
	if _, err := (Op{Tag: TagSHA256}).Apply(make([]byte, MaxMessageLen+1)); err == nil {
		t.Error("message of 4097 accepted")
	}
	if _, err := (Op{Tag: TagSHA256}).Apply(make([]byte, MaxMessageLen)); err != nil {
		t.Errorf("message of 4096: %v", err)
	}
	// parser: argument of 4096 accepted by the reader, 4097 refused
	for _, c := range []struct {
		n  int
		ok bool
	}{{MaxMessageLen - 20, true}, {MaxMessageLen + 1, false}} {
		var w writer
		w.byte(TagAppend)
		w.varbytes(make([]byte, c.n))
		w.byte(TagSHA1)
		w.attestation(pending(t, "https://a.example"))
		d := make([]byte, 20)
		_, err := ParseFragment(w.b, d)
		if (err == nil) != c.ok {
			t.Errorf("append arg %d: %v", c.n, err)
		}
	}
	// empty argument
	var w writer
	w.byte(TagAppend)
	w.varbytes(nil)
	w.attestation(pending(t, "https://a.example"))
	if _, err := ParseFragment(w.b, make([]byte, 20)); !errors.Is(err, ErrMalformed) {
		t.Errorf("empty arg: %v", err)
	}
}

func TestURIBoundary(t *testing.T) {
	ok := "https://" + strings.Repeat("a", MaxURILen-8)
	if _, err := NewPending(ok); err != nil {
		t.Errorf("uri of %d: %v", len(ok), err)
	}
	if _, err := NewPending(ok + "a"); !errors.Is(err, ErrURI) {
		t.Errorf("uri of %d: %v", len(ok)+1, err)
	}
	for _, bad := range []string{"https://a.example/?x=1", "https://u@a.example", "https://a b", "https://a.example/" + string(rune(0xe9)), "a#b", "a%20b"} {
		if _, err := NewPending(bad); !errors.Is(err, ErrURI) {
			t.Errorf("%q accepted", bad)
		}
	}
	// parser enforces the same
	var w writer
	w.byte(0)
	w.raw(TagPending[:])
	var p writer
	p.varbytes([]byte("https://a.example?x"))
	w.varbytes(p.b)
	if _, err := ParseFragment(w.b, make([]byte, 32)); !errors.Is(err, ErrURI) {
		t.Errorf("parser accepted a bad uri: %v", err)
	}
}

func TestPayloadAndHeightBoundary(t *testing.T) {
	mk := func(tag [8]byte, payload []byte) []byte {
		var w writer
		w.byte(0)
		w.raw(tag[:])
		w.varbytes(payload)
		return w.b
	}
	unk := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	if _, err := ParseFragment(mk(unk, make([]byte, MaxPayloadLen)), make([]byte, 32)); err != nil {
		t.Errorf("payload %d: %v", MaxPayloadLen, err)
	}
	if _, err := ParseFragment(mk(unk, make([]byte, MaxPayloadLen+1)), make([]byte, 32)); !errors.Is(err, ErrMalformed) {
		t.Errorf("payload %d: %v", MaxPayloadLen+1, err)
	}
	var h writer
	h.varuint(MaxHeight)
	if _, err := ParseFragment(mk(TagBitcoin, h.b), make([]byte, 32)); err != nil {
		t.Errorf("height 2^32-1: %v", err)
	}
	h = writer{}
	h.varuint(MaxHeight + 1)
	if _, err := ParseFragment(mk(TagBitcoin, h.b), make([]byte, 32)); !errors.Is(err, ErrMalformed) {
		t.Errorf("height 2^32: %v", err)
	}
	// data after the height
	h = writer{}
	h.varuint(5)
	h.byte(0)
	if _, err := ParseFragment(mk(TagBitcoin, h.b), make([]byte, 32)); !errors.Is(err, ErrMalformed) {
		t.Errorf("bitcoin payload with extra byte: %v", err)
	}
	// varuint of 10 bytes
	long := append(bytes.Repeat([]byte{0x80}, 9), 0x01)
	if _, err := ParseFragment(mk(TagBitcoin, long), make([]byte, 32)); !errors.Is(err, ErrMalformed) {
		t.Errorf("long varuint: %v", err)
	}
	if _, err := NewBitcoin(MaxHeight + 1); err == nil {
		t.Error("NewBitcoin accepted 2^32")
	}
}

func TestProofSizeBoundary(t *testing.T) {
	f, _ := ParseFile(mustRead(t, "testdata/third_party/opentimestamps-client-examples/hello-world.txt.ots"))
	b, _ := f.Serialize()
	pad := func(n int) []byte { return append(append([]byte(nil), b...), make([]byte, n-len(b))...) }
	if _, err := ParseFile(pad(MaxProofSize)); !errors.Is(err, ErrTrailing) {
		t.Errorf("size limit exactly: %v (size is checked, then the content)", err)
	}
	if _, err := ParseFile(pad(MaxProofSize + 1)); !errors.Is(err, ErrTooLarge) {
		t.Errorf("size limit plus one: %v", err)
	}
	if _, err := ParseFragment(make([]byte, MaxProofSize+1), make([]byte, 32)); !errors.Is(err, ErrTooLarge) {
		t.Errorf("fragment: %v", err)
	}
}

func TestRepeatedOpAndEmptyNode(t *testing.T) {
	var w writer
	w.byte(0xff)
	w.byte(TagSHA256)
	w.attestation(pending(t, "https://a.example"))
	w.byte(TagSHA256)
	w.attestation(pending(t, "https://b.example"))
	if _, err := ParseFragment(w.b, make([]byte, 32)); !errors.Is(err, ErrMalformed) {
		t.Errorf("repeated op: %v", err)
	}
	if _, err := newRoot(t).Serialize(); !errors.Is(err, ErrEmptyNode) {
		t.Errorf("empty node: %v", err)
	}
	if _, err := ParseFragment(nil, make([]byte, 32)); err == nil {
		t.Error("empty input parsed")
	}
}

func TestMergeCanonical(t *testing.T) {
	d := sha256.Sum256([]byte("m"))
	mk := func(uri string, extra bool) *Node {
		r := NewNode(d[:])
		c, _ := r.Child(Op{Tag: TagAppend, Arg: []byte("nonce")})
		c, _ = c.Child(Op{Tag: TagSHA256})
		c.AddAttestation(pending(t, uri))
		if extra {
			e, _ := r.Child(Op{Tag: TagPrepend, Arg: []byte("p")})
			e.AddAttestation(pending(t, "https://c.example"))
		}
		return r
	}
	a, b := mk("https://a.example", false), mk("https://b.example", true)
	ab, ba := a.Clone(), b.Clone()
	if err := ab.Merge(b); err != nil {
		t.Fatal(err)
	}
	if err := ba.Merge(a); err != nil {
		t.Fatal(err)
	}
	s1, _ := ab.Serialize()
	s2, _ := ba.Serialize()
	if !bytes.Equal(s1, s2) {
		t.Error("merge is not commutative on the serialised form")
	}
	// identical subtrees are shared: the shared append/sha256 chain exists once
	if got := ab.Count(); got != 4 {
		t.Errorf("nodes %d, want 4 (root, append, sha256 with both attestations, prepend)", got)
	}
	if err := ab.Merge(ab.Clone()); err != nil {
		t.Fatal(err)
	}
	s3, _ := ab.Serialize()
	if !bytes.Equal(s1, s3) {
		t.Error("merging a tree with itself changed it")
	}
	back, err := ParseFragment(s1, d[:])
	if err != nil {
		t.Fatal(err)
	}
	s4, _ := back.Serialize()
	if !bytes.Equal(s1, s4) {
		t.Error("parse then serialise changed a merged tree")
	}
	other := NewNode([]byte("different message of 32 bytes!!!"))
	if err := ab.Merge(other); err == nil {
		t.Error("merge of another message accepted")
	}
}

// Merging the real two single-calendar halves gives the real two-calendar file.
func TestMergeRealVectorHalves(t *testing.T) {
	f, _ := ParseFile(mustRead(t, "testdata/third_party/opentimestamps-client-examples/two-calendars.txt.ots"))
	full, _ := f.Serialize()
	halves := make([]*Node, 2)
	for i, keep := range []string{"alice", "bob"} {
		n := f.Root.Clone()
		n.Walk(func(x *Node) {
			var keepAtts []Attestation
			for _, a := range x.Attestations {
				if strings.Contains(a.URI, keep) {
					keepAtts = append(keepAtts, a)
				}
			}
			x.Attestations = keepAtts
		})
		halves[i] = prune(n)
	}
	if err := halves[0].Merge(halves[1]); err != nil {
		t.Fatal(err)
	}
	got, err := (&File{HashOp: TagSHA256, Root: halves[0]}).Serialize()
	if err != nil || !bytes.Equal(got, full) {
		t.Errorf("merged halves differ from the reference file (err %v)", err)
	}
}

func prune(n *Node) *Node {
	var edges []Edge
	for _, e := range n.Edges {
		c := prune(e.Next)
		if c != nil {
			edges = append(edges, Edge{Op: e.Op, Next: c})
		}
	}
	n.Edges = edges
	if len(n.Attestations) == 0 && len(n.Edges) == 0 {
		return nil
	}
	return n
}

func TestOpsKnownAnswers(t *testing.T) {
	r, _ := (Op{Tag: TagSHA256}).Apply([]byte("abc"))
	if hex.EncodeToString(r) != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Error("sha256")
	}
	r, _ = (Op{Tag: TagSHA1}).Apply([]byte("abc"))
	if hex.EncodeToString(r) != "a9993e364706816aba3e25717850c26c9cd0d89d" {
		t.Error("sha1")
	}
	r, _ = (Op{Tag: TagRIPEMD160}).Apply([]byte("abc"))
	if hex.EncodeToString(r) != "8eb208f7e05d987a9b044a8e98c6b087f15a0bfc" {
		t.Error("ripemd160")
	}
	r, _ = (Op{Tag: TagAppend, Arg: []byte("y")}).Apply([]byte("x"))
	if string(r) != "xy" {
		t.Error("append")
	}
	r, _ = (Op{Tag: TagPrepend, Arg: []byte("y")}).Apply([]byte("x"))
	if string(r) != "yx" {
		t.Error("prepend")
	}
	if _, err := (Op{Tag: 0xf3}).Apply([]byte("x")); err == nil {
		t.Error("hexlify applied")
	}
}

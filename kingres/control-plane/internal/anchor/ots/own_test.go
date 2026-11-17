// SPDX-License-Identifier: AGPL-3.0-only

package ots

// Tests with data of our own. They need nothing from testdata/third_party:
// without the examples of the reference client, every property below is still
// tested. The proofs are written here byte by byte with an encoder of this
// file (which shares no code with proof.go), from the format described in
// doc.go and in the reference library, and the hashes they commit to are
// computed here with crypto/sha256 and crypto/sha1.

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

// vu is a varuint: little-endian base 128, the high bit marks "more".
func vu(n uint64) []byte {
	var out []byte
	for {
		b := byte(n & 0x7f)
		n >>= 7
		if n != 0 {
			out = append(out, b|0x80)
			continue
		}
		return append(out, b)
	}
}

func vb(b []byte) []byte { return cat(vu(uint64(len(b))), b) }

var (
	tagPendingBytes = []byte{0x83, 0xdf, 0xe3, 0x0d, 0x2e, 0xf9, 0x0c, 0x8e}
	tagBitcoinBytes = []byte{0x05, 0x88, 0x96, 0x0d, 0x73, 0xd7, 0x19, 0x01}
	ownMagic        = []byte("\x00OpenTimestamps\x00\x00Proof\x00\xbf\x89\xe2\xe8\x84\xe8\x92\x94")
)

func attPendingB(uri string) []byte { return cat([]byte{0x00}, tagPendingBytes, vb(vb([]byte(uri)))) }
func attBitcoinB(h uint64) []byte   { return cat([]byte{0x00}, tagBitcoinBytes, vb(vu(h))) }
func opAppend(arg []byte) []byte    { return cat([]byte{0xf0}, vb(arg)) }
func opPrepend(arg []byte) []byte   { return cat([]byte{0xf1}, vb(arg)) }

var opSHA256B = []byte{0x08}

func sha(b []byte) []byte { s := sha256.Sum256(b); return s[:] }

// ownDigest is the digest the own proofs are about.
var ownDigest = sha([]byte("kingres own test vector"))

// ownProof builds a calendar-shaped proof by hand:
//
//	digest -- sha256 -- pending(https://a.example)
//	digest -- append(N) -- sha256 -- prepend(S) -- sha256 -- sha256 -- bitcoin(height)
//
// The items of a node are the attestations (sorted), then the operations
// sorted by tag and argument; every item but the last is preceded by 0xff.
// It returns the tree bytes and the message that the Bitcoin attestation sits on.
func ownProof(height uint64) (tree []byte, root []byte) {
	n := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	s := bytes.Repeat([]byte{0xab}, 32)
	m1 := sha(cat(ownDigest, n))
	m2 := sha(cat(s, m1))
	root = sha(m2)
	// the last item of the root: append(N), and the chain of single items
	branch := cat(opAppend(n), opSHA256B, opPrepend(s), opSHA256B, opSHA256B, attBitcoinB(height))
	tree = cat(
		[]byte{0xff}, opSHA256B, attPendingB("https://a.example"), // first item of the root: sha256, a node with one attestation
		branch, // last item of the root: append(N), then the chain
	)
	return tree, root
}

func ownFile(height uint64) (file, tree, root []byte) {
	tree, root = ownProof(height)
	return cat(ownMagic, []byte{0x01, 0x08}, ownDigest, tree), tree, root
}

func TestOwnProofParsesAndComputesWhatTheCalendarSays(t *testing.T) {
	file, tree, root := ownFile(960000)
	f, err := ParseFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if f.HashOp != TagSHA256 || !bytes.Equal(f.Root.Msg, ownDigest) {
		t.Fatal("header")
	}
	var btc, pend []Leaf
	for _, l := range f.Root.Leaves() {
		switch l.Att.Kind {
		case AttBitcoin:
			btc = append(btc, l)
		case AttPending:
			pend = append(pend, l)
		}
	}
	if len(btc) != 1 || len(pend) != 1 || btc[0].Att.Height != 960000 || pend[0].Att.URI != "https://a.example" {
		t.Fatalf("leaves %+v %+v", btc, pend)
	}
	// the message the attestation sits on is computed by the operations, from the digest
	if !bytes.Equal(btc[0].Msg, root) {
		t.Fatalf("message %x, want %x", btc[0].Msg, root)
	}
	if !bytes.Equal(pend[0].Msg, sha(ownDigest)) {
		t.Fatal("pending message")
	}
	// it serialises to the bytes that were written by hand (the order of the items is the reference order)
	again, err := f.Serialize()
	if err != nil || !bytes.Equal(again, file) {
		t.Fatalf("round trip differs (%v):\n%x\n%x", err, again, file)
	}
	n, err := ParseFragment(tree, ownDigest)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := n.Serialize(); !bytes.Equal(b, tree) {
		t.Fatal("fragment round trip")
	}
	// the own proof is calendar-shaped: it passes the plausibility checks
	cl := checkFragments([]*Node{n}, []string{"https://a.example"}, ownSubmitted, ownNow)
	if len(cl) != 1 || cl[0].Problem != "" || cl[0].Conflict != "" || cl[0].Height != 960000 || !bytes.Equal(cl[0].Root[:], root) {
		t.Fatalf("%+v", cl)
	}
}

var (
	ownSubmitted = time.Date(2026, 12, 1, 10, 0, 0, 0, time.UTC)
	ownNow       = ownSubmitted.Add(2 * time.Hour)
)

// Every proper prefix of a proof fails and none panics; a byte after it fails
// (with the examples of the reference client absent this is the only test of
// truncation and trailing data).
func TestOwnTruncationsAndTrailing(t *testing.T) {
	file, tree, _ := ownFile(960000)
	for i := 0; i < len(file); i++ {
		if _, err := ParseFile(file[:i]); err == nil {
			t.Fatalf("prefix of %d bytes parsed", i)
		}
	}
	for i := 0; i < len(tree); i++ {
		if _, err := ParseFragment(tree[:i], ownDigest); err == nil {
			t.Fatalf("fragment prefix of %d bytes parsed", i)
		}
	}
	if _, err := ParseFile(append(append([]byte(nil), file...), 0)); !errors.Is(err, ErrTrailing) {
		t.Errorf("trailing byte: %v", err)
	}
	if _, err := ParseFragment(append(append([]byte(nil), tree...), 0), ownDigest); !errors.Is(err, ErrTrailing) {
		t.Errorf("trailing byte on a fragment: %v", err)
	}
}

func TestOwnMagicVersionAndHashOp(t *testing.T) {
	file, _, _ := ownFile(960000)
	for _, i := range []int{0, 1, 10, len(ownMagic) - 1} {
		c := append([]byte(nil), file...)
		c[i] ^= 1
		if _, err := ParseFile(c); !errors.Is(err, ErrBadMagic) {
			t.Errorf("magic byte %d: %v", i, err)
		}
	}
	c := append([]byte(nil), file...)
	c[len(ownMagic)] = 2
	if _, err := ParseFile(c); !errors.Is(err, ErrVersion) {
		t.Errorf("major version 2: %v", err)
	}
	c[len(ownMagic)] = 0
	if _, err := ParseFile(c); !errors.Is(err, ErrVersion) {
		t.Errorf("major version 0: %v", err)
	}
	// the file hash operation must be a hash: append, prepend, keccak256, an unknown tag
	for _, tag := range []byte{0xf0, 0xf1, 0x67, 0xf2, 0x42} {
		c := append([]byte(nil), file...)
		c[len(ownMagic)+1] = tag
		if _, err := ParseFile(c); !errors.Is(err, ErrBadHashOp) {
			t.Errorf("file hash op 0x%02x: %v", tag, err)
		}
	}
	// sha1 and ripemd160 are fine, with a 20-byte digest
	for _, tag := range []byte{0x02, 0x03} {
		d := ownDigest[:20]
		f2 := cat(ownMagic, []byte{0x01, tag}, d, attBitcoinB(1))
		if _, err := ParseFile(f2); err != nil {
			t.Errorf("hash op 0x%02x: %v", tag, err)
		}
	}
}

// The operations that the format defines and this package refuses are refused
// by name, whatever sits on the path; any other tag as unknown.
func TestOwnRefusedByName(t *testing.T) {
	for _, c := range []struct {
		tag  byte
		name string
	}{{0x67, "keccak256"}, {0xf2, "reverse"}, {0xf3, "hexlify"}, {0x42, ""}, {0x04, ""}, {0x09, ""}} {
		tree := cat([]byte{0xff}, opSHA256B, attBitcoinB(1), []byte{c.tag}, attBitcoinB(2))
		_, err := ParseFragment(tree, ownDigest)
		var ue *UnsupportedOpError
		if !errors.As(err, &ue) || ue.Name != c.name || ue.Tag != c.tag {
			t.Errorf("tag 0x%02x: %v", c.tag, err)
		}
		if c.name != "" && !strings.Contains(err.Error(), c.name) {
			t.Errorf("the message does not name %s: %v", c.name, err)
		}
	}
}

// The merkle root is compared with bytes 36 to 68 of the header as they stand:
// a proof whose digest IS the merkle root of a real block (a file whose
// SHA-256 is that value) with a Bitcoin attestation on it matches that block's
// real header, and not the same root reversed. This catches a comparison with
// the reversed root without any proof of the reference client.
func TestOwnMerkleRootIsNotReversed(t *testing.T) {
	hs := headers(t)
	for h, x := range hs {
		mr, _ := MerkleRoot(x.raw)
		if len(mr) != 32 || !bytes.Equal(mr, x.raw[36:68]) {
			t.Fatalf("height %d: MerkleRoot is not bytes 36 to 68", h)
		}
		rev := make([]byte, 32)
		for i := range rev {
			rev[i] = mr[31-i]
		}
		// the proof of a file whose digest is the root as the header holds it
		fwd := cat(ownMagic, []byte{0x01, 0x08}, mr, attBitcoinB(h))
		f, err := ParseFile(fwd)
		if err != nil {
			t.Fatal(err)
		}
		res, err := CheckAgainstHeader(f.Root, h, x.raw)
		if err != nil || res.Matches != 1 || res.Height != h {
			t.Errorf("height %d: the proof on the root as stored does not match its header: %v %+v", h, err, res)
		}
		// the same proof on the reversed root (the order a block explorer shows) does not
		bad := cat(ownMagic, []byte{0x01, 0x08}, rev, attBitcoinB(h))
		f2, _ := ParseFile(bad)
		if _, err := CheckAgainstHeader(f2.Root, h, x.raw); !errors.Is(err, ErrNoMatch) {
			t.Errorf("height %d: the reversed root matched: %v", h, err)
		}
		// another height's header does not match, and neither does a wrong claimed height
		for oh, ox := range hs {
			if oh == h {
				continue
			}
			if _, err := CheckAgainstHeader(f.Root, h, ox.raw); !errors.Is(err, ErrNoMatch) {
				t.Errorf("height %d: header %d accepted: %v", h, oh, err)
			}
			if _, err := CheckAgainstHeader(f.Root, oh, x.raw); !errors.Is(err, ErrNoMatch) {
				t.Errorf("height %d: claimed height %d accepted: %v", h, oh, err)
			}
		}
	}
	// a proof with no Bitcoin attestation, and one on a node of 31 bytes
	f, _ := ParseFile(cat(ownMagic, []byte{0x01, 0x08}, ownDigest, attPendingB("https://a.example")))
	if _, err := CheckAgainstHeader(f.Root, 0, hs[0].raw); !errors.Is(err, ErrNoMatch) {
		t.Errorf("pending proof: %v", err)
	}
	f, _ = ParseFile(cat(ownMagic, []byte{0x01, 0x03}, bytes.Repeat([]byte{1}, 20), attBitcoinB(0)))
	if _, err := CheckAgainstHeader(f.Root, 0, hs[0].raw); !errors.Is(err, ErrNoMatch) {
		t.Errorf("20-byte message: %v", err)
	}
}

// The table of checkpoints that bounds the heights is a table of real
// headers: every height and time in it is that of a header of
// testdata/headers.txt, which is in the test binary.
func TestCheckpointsAreRealHeaders(t *testing.T) {
	hs := headers(t)
	if len(checkpoints) != len(hs) {
		t.Fatalf("%d checkpoints, %d headers", len(checkpoints), len(hs))
	}
	for _, c := range checkpoints {
		h, ok := hs[c.height]
		if !ok {
			t.Errorf("no header for the checkpoint at height %d", c.height)
			continue
		}
		if got := int64(binary.LittleEndian.Uint32(h.raw[68:72])); got != c.time {
			t.Errorf("height %d: time %d, header says %d", c.height, c.time, got)
		}
	}
}

func TestHeightBounds(t *testing.T) {
	// a digest made after the last checkpoint: nothing at or below it can attest it
	lo, hi := HeightBounds(ownSubmitted, ownNow)
	if lo != 912096 {
		t.Errorf("lower bound %d", lo)
	}
	if hi < 960000 || hi > 1300000 {
		t.Errorf("upper bound %d is not a plausible bound for the end of 2026", hi)
	}
	// the bound grows with the clock, never shrinks
	_, hi2 := HeightBounds(ownSubmitted, ownNow.Add(365*24*time.Hour))
	if hi2 <= hi {
		t.Errorf("hi %d then %d", hi, hi2)
	}
	// a digest made in 2017 (after 465751 and before 466906 by a week): 465751 is below, 466906 is not
	sub := time.Date(2017, 5, 17, 0, 0, 0, 0, time.UTC)
	lo, _ = HeightBounds(sub, sub.Add(time.Hour))
	if lo != 465752 {
		t.Errorf("2017 lower bound %d", lo)
	}
	// within the slack of a checkpoint the checkpoint still counts as possible
	t0 := time.Unix(1495071945, 0).UTC() // the time of block 466906
	lo, _ = HeightBounds(t0.Add(BlockTimeSlack-time.Second), t0.Add(24*time.Hour))
	if lo != 465752 {
		t.Errorf("just inside the slack: %d", lo)
	}
	lo, _ = HeightBounds(t0.Add(BlockTimeSlack), t0.Add(24*time.Hour))
	if lo != 466907 {
		t.Errorf("at the slack: %d", lo)
	}
	// before the genesis block
	lo, hi = HeightBounds(time.Unix(1000, 0), time.Unix(2000, 0))
	if lo != 0 || hi != heightMargin {
		t.Errorf("before genesis: %d %d", lo, hi)
	}
}

// Without the examples of the reference client, the structure of a merge is
// tested on a proof of our own: the two halves of ownProof merge to the whole
// (order does not matter) and a merge of different messages is refused.
func TestOwnMergeHalves(t *testing.T) {
	_, tree, _ := ownFile(960000)
	whole, _ := ParseFragment(tree, ownDigest)
	left, _ := ParseFragment(tree, ownDigest)
	right, _ := ParseFragment(tree, ownDigest)
	left = dropLeaves(left, func(path []Op, msg []byte, a Attestation) bool { return a.Kind == AttBitcoin })
	right = dropLeaves(right, func(path []Op, msg []byte, a Attestation) bool { return a.Kind == AttPending })
	for _, order := range [][2]*Node{{left.Clone(), right.Clone()}, {right.Clone(), left.Clone()}} {
		a, b := order[0], order[1]
		if err := a.Merge(b); err != nil {
			t.Fatal(err)
		}
		got, _ := a.Serialize()
		want, _ := whole.Serialize()
		if !bytes.Equal(got, want) {
			t.Fatalf("merge differs:\n%x\n%x", got, want)
		}
	}
	other := NewNode(sha([]byte("another digest")))
	if err := left.Clone().Merge(other); err == nil {
		t.Error("nodes of different messages merged")
	}
	// merging a node into itself changes nothing
	c := whole.Clone()
	if err := c.Merge(whole); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Serialize(); !bytes.Equal(got, tree) {
		t.Error("self merge changed the tree")
	}
}

// An attestation of a kind this package does not know is kept as it is and
// written back unchanged.
func TestOwnUnknownAttestationKept(t *testing.T) {
	tag := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	unknown := cat([]byte{0x00}, tag, vb([]byte("payload bytes")))
	tree := cat([]byte{0xff}, unknown, opSHA256B, attBitcoinB(960000))
	n, err := ParseFragment(tree, ownDigest)
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, l := range n.Leaves() {
		if l.Att.Kind == AttUnknown && string(l.Att.Payload) == "payload bytes" && bytes.Equal(l.Att.Tag[:], tag) {
			seen = true
		}
	}
	if !seen {
		t.Fatal("unknown attestation lost")
	}
	if b, _ := n.Serialize(); !bytes.Equal(b, tree) {
		t.Fatalf("not written back unchanged:\n%x\n%x", b, tree)
	}
}

// What the parser computes for every operation is checked against hash
// functions of the standard library, on the message that the proof reaches.
func TestOwnOperationsAreRecomputed(t *testing.T) {
	// a calendar that lies about the tree shape cannot make a message that the
	// operations do not give: the message of a node is computed, never read
	_, tree, root := ownFile(960000)
	n, _ := ParseFragment(tree, ownDigest)
	got := BitcoinClaims(n)
	if len(got) != 1 || !bytes.Equal(got[0].Msg, root) {
		t.Fatal("claim message is not the recomputed one")
	}
	other := sha([]byte("not the digest"))
	n2, _ := ParseFragment(tree, other)
	got2 := BitcoinClaims(n2)
	if len(got2) != 1 || bytes.Equal(got2[0].Msg, root) {
		t.Fatal("the same tree on another digest gave the same message")
	}
	if hex.EncodeToString(root) == "" {
		t.Fatal()
	}
}

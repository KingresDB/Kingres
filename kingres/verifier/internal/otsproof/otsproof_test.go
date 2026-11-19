// SPDX-License-Identifier: AGPL-3.0-only

package otsproof

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/anchortest"
)

var dig = sha256.Sum256([]byte("an aggregate checkpoint text"))

// The genesis block of Bitcoin: a real mined header, the one real vector available without a network (design 7.6, 7: a header
// of the block of an anchor is not testable here).
const genesis = "0100000000000000000000000000000000000000000000000000000000000000000000003ba3edfd7a7b12b27ac72c3e67768f617fc81bc3888a51323a9fb8aa4b1e5e4a29ab5f49ffff001d1dac2b7c"

func TestTheGenesisHeaderPassesTheWorkCheck(t *testing.T) {
	raw, _ := hex.DecodeString(genesis)
	h, err := ParseHeader(raw)
	if err != nil {
		t.Fatal(err)
	}
	if h.Time.Unix() != 1231006505 {
		t.Fatal(h.Time)
	}
	if hex.EncodeToString(h.MerkleRoot[:]) != "3ba3edfd7a7b12b27ac72c3e67768f617fc81bc3888a51323a9fb8aa4b1e5e4a" {
		t.Fatal("merkle root")
	}
	// every single bit matters: a changed header is not a mined header (the bits of the merkle root and of the time too)
	for i := 0; i < 80*8; i++ {
		m := append([]byte{}, raw...)
		m[i/8] ^= 1 << (i % 8)
		if _, err := ParseHeader(m); err == nil {
			t.Fatalf("bit %d flipped and the header still passes", i)
		}
	}
	if _, err := ParseHeader(raw[:79]); err == nil {
		t.Fatal("79 bytes accepted")
	}
}

func TestAnEasyTargetIsRefusedAtTheRealLimit(t *testing.T) {
	easy := anchortest.Header([32]byte{1}, time.Unix(1_700_000_000, 0), 0x207fffff)
	if _, err := ParseHeader(easy); err == nil || !strings.Contains(err.Error(), "easier than difficulty 1") {
		t.Fatalf("%v", err)
	}
	limit := new(big.Int).Lsh(big.NewInt(1), 255)
	if _, err := ParseHeaderLimit(easy, limit); err != nil {
		t.Fatal(err)
	}
	// a negative, a zero and an overflowing target
	for _, bits := range []uint32{0x20ffffff, 0x1d000000, 0x22ffffff} {
		h := append([]byte{}, easy...)
		h[72], h[73], h[74], h[75] = byte(bits), byte(bits>>8), byte(bits>>16), byte(bits>>24)
		if _, err := ParseHeaderLimit(h, limit); err == nil {
			t.Errorf("bits %08x accepted", bits)
		}
	}
}

func TestWalkComputesTheMessageOfAnAttestation(t *testing.T) {
	nonce := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	sib := []byte("0123456789abcdef0123456789abcdef")
	frag, root := anchortest.Fragment(dig, nonce, sib, anchortest.BitcoinAttestation(912345))
	as, err := Walk(frag, dig[:])
	if err != nil {
		t.Fatal(err)
	}
	if len(as) != 1 || as[0].Kind != Bitcoin || as[0].Height != 912345 || string(as[0].Message) != string(root[:]) || !as[0].EndsInDoubleSHA256 || as[0].Hashes != 3 {
		t.Fatalf("%+v", as)
	}
	other := sha256.Sum256([]byte("another"))
	as2, err := Walk(frag, other[:])
	if err != nil || string(as2[0].Message) == string(root[:]) {
		t.Fatal("the message does not depend on the digest")
	}
	both := append([]byte{0xff}, anchortest.PendingAttestation("https://cal.example.com/x")...)
	both = append(both, anchortest.BitcoinAttestation(7)...)
	as3, err := Walk(both, dig[:])
	if err != nil || len(as3) != 2 || as3[0].Kind != Pending || as3[0].URI != "https://cal.example.com/x" || as3[1].Kind != Bitcoin {
		t.Fatalf("%+v %v", as3, err)
	}
}

func TestWalkRefusesWhatItCannotCompute(t *testing.T) {
	att := anchortest.BitcoinAttestation(5)
	for name, c := range map[string]struct {
		frag []byte
		want string
	}{
		"ripemd160":      {append([]byte{0x03}, att...), "RIPEMD-160"},
		"reverse":        {append([]byte{0xf2}, att...), "reverse"},
		"hexlify":        {append([]byte{0xf3}, att...), "hexlify"},
		"keccak":         {append([]byte{0x67}, att...), "Keccak-256"},
		"unknown":        {append([]byte{0x77}, att...), "unknown operation"},
		"trailing bytes": {append(append([]byte{}, att...), 0), "follow"},
		"truncated":      {att[:len(att)-1], "ends"},
		"empty append":   {append([]byte{0xf0, 0x00}, att...), "empty argument"},
		"bad height":     {append([]byte{0x00, 0x05, 0x88, 0x96, 0x0d, 0x73, 0xd7, 0x19, 0x01}, 0x05, 0x80, 0x80, 0x80, 0x80, 0x10), "varuint height"},
		"bad URI char":   {anchortest.PendingAttestation("https://x y"), "outside letters"},
		"non minimal":    {[]byte{0xf0, 0x81, 0x00}, "not minimal"},
	} {
		_, err := Walk(c.frag, dig[:])
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	var ue *UnsupportedOpError
	if _, err := Walk(append([]byte{0x03}, att...), dig[:]); !errors.As(err, &ue) {
		t.Error("not an UnsupportedOpError")
	}
	var deep []byte
	for i := 0; i < MaxDepth+2; i++ {
		deep = append(deep, 0x08)
	}
	deep = append(deep, att...)
	if _, err := Walk(deep, dig[:]); err == nil || !strings.Contains(err.Error(), "deeper") {
		t.Errorf("deep: %v", err)
	}
	if _, err := Walk(make([]byte, MaxFragment+1), dig[:]); err == nil {
		t.Error("over-long")
	}
	if _, err := Walk(nil, dig[:]); err == nil {
		t.Error("empty")
	}
}

func TestAHeaderMatchesOnlyItsOwnMerkleRoot(t *testing.T) {
	frag, root := anchortest.Fragment(dig, []byte{9}, []byte("sibling-sibling-sibling-sibling!"), anchortest.BitcoinAttestation(100))
	as, _ := Walk(frag, dig[:])
	limit := new(big.Int).Lsh(big.NewInt(1), 255)
	h, err := ParseHeaderLimit(anchortest.Header(root, time.Unix(1_800_000_000, 0), 0x207fffff), limit)
	if err != nil || !h.Matches(as[0].Message) {
		t.Fatalf("%v", err)
	}
	other, _ := ParseHeaderLimit(anchortest.Header([32]byte{5}, time.Unix(1_800_000_000, 0), 0x207fffff), limit)
	if other.Matches(as[0].Message) || h.Matches(as[0].Message[:31]) {
		t.Fatal("matched")
	}
}

func FuzzWalk(f *testing.F) {
	frag, _ := anchortest.Fragment(dig, []byte{1, 2}, []byte("0123456789abcdef0123456789abcdef"), anchortest.BitcoinAttestation(1234))
	f.Add(frag)
	f.Add(append([]byte{0xff}, anchortest.PendingAttestation("https://a.example.com")...))
	f.Add([]byte{0x03, 0x00})
	f.Fuzz(func(t *testing.T, b []byte) {
		as, err := Walk(b, dig[:])
		if err != nil {
			return
		}
		for _, a := range as {
			if len(a.Message) > MaxMessageLen {
				t.Fatal("a message over the bound")
			}
		}
	})
}

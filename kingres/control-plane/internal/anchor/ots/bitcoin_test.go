// SPDX-License-Identifier: AGPL-3.0-only

package ots

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

type hdr struct {
	height uint64
	id     string
	raw    []byte
}

// testdata/headers.txt: "<height> <block id> <80-byte header hex>" lines
// (real headers of the Bitcoin main chain, see testdata/SOURCES.txt).
func headers(t testing.TB) map[uint64]hdr {
	t.Helper()
	out := map[uint64]hdr{}
	for _, l := range strings.Split(strings.TrimSpace(string(mustRead(t, "testdata/headers.txt"))), "\n") {
		f := strings.Fields(l)
		if len(f) != 3 {
			t.Fatalf("bad line %q", l)
		}
		h, _ := strconv.ParseUint(f[0], 10, 64)
		raw, err := hex.DecodeString(f[2])
		if err != nil || len(raw) != 80 {
			t.Fatalf("bad header %q", l)
		}
		out[h] = hdr{h, f[1], raw}
	}
	return out
}

func TestRealHeadersPassCheck(t *testing.T) {
	hs := headers(t)
	if len(hs) < 6 {
		t.Fatal("headers missing")
	}
	for h, x := range hs {
		r, err := CheckHeader(x.raw)
		if err != nil {
			t.Errorf("height %d: %v", h, err)
			continue
		}
		// the block id is the reversed double hash
		var id [32]byte
		for i := range id {
			id[i] = r.Hash[31-i]
		}
		if hex.EncodeToString(id[:]) != x.id {
			t.Errorf("height %d: id %x, want %s", h, id, x.id)
		}
	}
	g := hs[0]
	r, _ := CheckHeader(g.raw)
	if r.Time.Unix() != 1231006505 {
		t.Errorf("genesis time %v", r.Time)
	}
	mr, _ := MerkleRoot(g.raw)
	if hex.EncodeToString(mr) != "3ba3edfd7a7b12b27ac72c3e67768f617fc81bc3888a51323a9fb8aa4b1e5e4a" {
		t.Errorf("genesis merkle root %x", mr)
	}
}

// The reference client's real proofs commit to the merkle roots of real blocks.
func TestRealProofsAgainstRealHeaders(t *testing.T) {
	hs := headers(t)
	cases := map[string]uint64{
		"hello-world.txt.ots": 358391, "bitcoin.pdf.ots": 465751, "empty.ots": 129405,
		"gdp2q25-2nd.pdf.ots": 912095, "sha1/a-or-b.ots": 466906,
	}
	for name, h := range cases {
		f, err := ParseFile(mustRead(t, "testdata/third_party/opentimestamps-client-examples/"+name))
		if err != nil {
			t.Fatal(err)
		}
		res, err := CheckAgainstHeader(f.Root, h, hs[h].raw)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if res.Matches != 1 || res.Height != h {
			t.Errorf("%s: %+v", name, res)
		}
		t.Logf("%s: block %d time %s", name, h, res.Time.Format(time.RFC3339))
		// a header of another height does not match
		for other, x := range hs {
			if other == h || other == 0 {
				continue
			}
			if _, err := CheckAgainstHeader(f.Root, h, x.raw); !errors.Is(err, ErrNoMatch) {
				t.Errorf("%s: header %d accepted for height %d: %v", name, other, h, err)
			}
			if _, err := CheckAgainstHeader(f.Root, other, x.raw); !errors.Is(err, ErrNoMatch) {
				t.Errorf("%s: claimed height %d accepted: %v", name, other, err)
			}
		}
	}
	// the proof whose file was altered (bad-stamp) carries the same height but another root
	f, _ := ParseFile(mustRead(t, "testdata/third_party/opentimestamps-client-examples/bad-stamp.txt.ots"))
	if _, err := CheckAgainstHeader(f.Root, 358391, hs[358391].raw); !errors.Is(err, ErrNoMatch) {
		t.Errorf("bad-stamp: %v", err)
	}
	// a pending proof has nothing to check
	f, _ = ParseFile(mustRead(t, "testdata/third_party/opentimestamps-client-examples/incomplete.txt.ots"))
	if _, err := CheckAgainstHeader(f.Root, 358391, hs[358391].raw); !errors.Is(err, ErrNoMatch) {
		t.Errorf("incomplete: %v", err)
	}
}

func TestHeaderRejections(t *testing.T) {
	hs := headers(t)
	good := hs[358391].raw
	f, _ := ParseFile(mustRead(t, "testdata/third_party/opentimestamps-client-examples/hello-world.txt.ots"))

	if _, err := CheckAgainstHeader(f.Root, 358391, good[:79]); !errors.Is(err, ErrBadHeader) {
		t.Errorf("79 bytes: %v", err)
	}
	if _, err := CheckAgainstHeader(f.Root, 358391, append(append([]byte(nil), good...), 0)); !errors.Is(err, ErrBadHeader) {
		t.Errorf("81 bytes: %v", err)
	}
	// nonce changed: the hash no longer meets the target
	bad := append([]byte(nil), good...)
	bad[76]++
	if _, err := CheckHeader(bad); !errors.Is(err, ErrBadHeader) {
		t.Errorf("changed nonce: %v", err)
	}
	// merkle root changed: the work check fails first
	bad = append([]byte(nil), good...)
	bad[40] ^= 1
	if _, err := CheckAgainstHeader(f.Root, 358391, bad); err == nil {
		t.Error("changed merkle root accepted")
	}
	// bits easier than the limit: refused whatever the hash is
	bad = append([]byte(nil), good...)
	binary.LittleEndian.PutUint32(bad[72:], 0x207fffff)
	if _, err := CheckHeader(bad); !errors.Is(err, ErrBadHeader) || !strings.Contains(err.Error(), "limit") {
		t.Errorf("easy bits: %v", err)
	}
}

func TestCompactToTarget(t *testing.T) {
	ok := map[uint32]string{
		0x1d00ffff: "ffff0000000000000000000000000000000000000000000000000000",
		0x01120000: "12",
		0x02008000: "80",
		0x05009234: "92340000",
		0x03000001: "1",
		0x04123456: "12345600",
	}
	for bits, want := range ok {
		got, err := CompactToTarget(bits)
		if err != nil {
			t.Errorf("%08x: %v", bits, err)
			continue
		}
		if got.Text(16) != strings.TrimLeft(want, "0") {
			t.Errorf("%08x: %s, want %s", bits, got.Text(16), want)
		}
	}
	for _, bits := range []uint32{0x00000000, 0x01003456 /* 0 after shift */, 0x04923456 /* negative */, 0xff123456, 0x23000001 /* exponent 35 */, 0x22000100 /* 34, mantissa > 0xff */, 0x21010000 /* 33 with mantissa > 0xffff */} {
		if _, err := CompactToTarget(bits); err == nil {
			t.Errorf("%08x accepted", bits)
		}
	}
	// the largest accepted exponents
	if _, err := CompactToTarget(0x22000001); err != nil {
		t.Errorf("exp 34 mantissa 1: %v", err)
	}
}

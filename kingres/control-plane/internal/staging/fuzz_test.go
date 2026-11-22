// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package staging

import (
	"bytes"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func fuzzFixture(f *testing.F, size int) (raw, plain, dk []byte) {
	s, _ := testStoreF(f)
	plain = payload(size, 31)
	res, err := s.Reserve(1, "o", 1<<30)
	if err != nil {
		f.Fatal(err)
	}
	w, meta, err := s.Create(1, res, testKeys)
	if err != nil {
		f.Fatal(err)
	}
	_, _ = w.Write(plain)
	if _, err := w.Finish(); err != nil {
		f.Fatal(err)
	}
	dk, _ = testKeys.Unwrap(1, meta.KeyID, meta.WrappedKey)
	raw, err = os.ReadFile(filepath.Join(s.Dir(), FinalName(1)))
	if err != nil {
		f.Fatal(err)
	}
	return raw, plain, dk
}

func testStoreF(f *testing.F) (*Store, *faultFS) {
	ffs := newFaultFS()
	s, err := Open(Config{FS: ffs, Dir: filepath.Join(f.TempDir(), "stage"), Quota: 1 << 30, Reserve: func(uint64) uint64 { return 0 }})
	if err != nil {
		f.Fatal(err)
	}
	return s, ffs
}

// FuzzReader mutates a sealed file. The property is authenticity: the reader may refuse (ErrCorrupt) or fail in no other way, it never
// panics, and if it opens the file and reads every byte then the bytes are the original plaintext and the file is the original file.
func FuzzReader(f *testing.F) {
	raw, plain, dk := fuzzFixture(f, 2*SegmentSize+321)
	f.Add(raw, uint16(0), byte(0), uint32(0))
	f.Add(raw, uint16(5), byte(1), uint32(0))
	f.Add(raw, uint16(100), byte(0xff), uint32(100))
	f.Add(raw[:HeaderSize+SegmentDisk], uint16(0), byte(0), uint32(0))
	f.Add(raw[:len(raw)-1], uint16(0), byte(0), uint32(0))
	f.Add(append(append([]byte(nil), raw...), raw[HeaderSize:HeaderSize+SegmentDisk]...), uint16(0), byte(0), uint32(0))
	f.Fuzz(func(t *testing.T, data []byte, pos uint16, x byte, cut uint32) {
		if len(data) > 1<<20 {
			return
		}
		m := append([]byte(nil), data...)
		if len(m) > 0 && x != 0 {
			m[int(pos)%len(m)] ^= x
		}
		if c := int(cut); c > 0 && c < len(m) {
			m = m[:len(m)-c]
		}
		r, err := NewReader(&memFile{b: m}, 1, dk)
		if err != nil {
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("NewReader failed with something other than ErrCorrupt: %v", err)
			}
			return
		}
		got := make([]byte, r.Size())
		_, err = r.ReadAt(got, 0)
		if err != nil && !errors.Is(err, io.EOF) {
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("ReadAt failed with something other than ErrCorrupt: %v", err)
			}
			return
		}
		if !bytes.Equal(got, plain) || !bytes.Equal(m, raw) {
			t.Fatalf("a changed file was accepted: %d bytes read, original file %v", len(got), bytes.Equal(m, raw))
		}
	})
}

// FuzzReaderRange reads ranges of a good file and of a damaged one; a range read must equal the slice of the plaintext or fail ErrCorrupt.
func FuzzReaderRange(f *testing.F) {
	raw, plain, dk := fuzzFixture(f, 3*SegmentSize+7)
	f.Add(int64(0), 10, uint32(0), byte(0))
	f.Add(int64(SegmentSize-1), 3, uint32(0), byte(0))
	f.Add(int64(len(plain)-1), 100, uint32(0), byte(0))
	f.Add(int64(SegmentSize), SegmentSize, uint32(HeaderSize+SegmentDisk+9), byte(1))
	f.Fuzz(func(t *testing.T, off int64, n int, pos uint32, x byte) {
		if n < 0 || n > 1<<20 {
			return
		}
		m := append([]byte(nil), raw...)
		damaged := false
		if x != 0 {
			m[int(pos)%len(m)] ^= x
			damaged = true
		}
		r, err := NewReader(&memFile{b: m}, 1, dk)
		if err != nil {
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("open: %v", err)
			}
			return
		}
		buf := make([]byte, n)
		got, err := r.ReadAt(buf, off)
		if off < 0 {
			if err == nil {
				t.Fatal("a negative offset was accepted")
			}
			return
		}
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, ErrCorrupt) {
			t.Fatalf("ReadAt: %v", err)
		}
		if err == nil || errors.Is(err, io.EOF) {
			want := 0
			if off < int64(len(plain)) {
				want = len(plain) - int(off)
				if want > n {
					want = n
				}
			}
			if got != want || (off < int64(len(plain)) && !bytes.Equal(buf[:got], plain[off:int(off)+got])) {
				t.Fatalf("range [%d,+%d): got %d bytes, want %d, damaged=%v", off, n, got, want, damaged)
			}
		}
	})
}

// FuzzUnwrap feeds arbitrary wrapped keys: never a panic, and only ErrKeyLost.
func FuzzUnwrap(f *testing.F) {
	_, w, _ := testKeys.NewFileKey(1)
	f.Add(w, "0707070707070707", int64(1))
	f.Add([]byte{}, "", int64(0))
	f.Fuzz(func(t *testing.T, wrapped []byte, keyID string, id int64) {
		dk, err := testKeys.Unwrap(id, keyID, wrapped)
		if err != nil {
			if !errors.Is(err, ErrKeyLost) {
				t.Fatalf("err = %v", err)
			}
			return
		}
		if len(dk) != keyBytes || keyID != testKeys.ID() {
			t.Fatalf("accepted a key of %d bytes under id %q", len(dk), keyID)
		}
	})
}

// FuzzLayout: the size of a file gives its layout, and the layout gives the size back.
func FuzzLayout(f *testing.F) {
	for _, n := range []int64{0, 15, 16, 32, 33, HeaderSize + SegmentDisk, HeaderSize + SegmentDisk + 16, math.MaxInt64} {
		f.Add(n)
	}
	f.Fuzz(func(t *testing.T, size int64) {
		nseg, last, plain, ok := layout(size)
		if !ok {
			return
		}
		if nseg < 1 || last < 0 || last > SegmentSize || plain < 0 || (nseg > 1 && last == 0) {
			t.Fatalf("layout(%d) = %d %d %d", size, nseg, last, plain)
		}
		if back := HeaderSize + plain + nseg*tagSize; back != size {
			t.Fatalf("layout(%d) does not round trip: %d", size, back)
		}
	})
}

// FuzzParseName: a name parses only in the one form, and a parsed name is the one the id gives back.
func FuzzParseName(f *testing.F) {
	for _, n := range []string{"transfer-1.kdx", "transfer-1.part", "transfer-007.kdx", "../transfer-1.kdx", "transfer-1.kdx\x00", MarkerName} {
		f.Add(n)
	}
	f.Fuzz(func(t *testing.T, name string) {
		id, final, ok := ParseName(name)
		if !ok {
			return
		}
		want := PartName(id)
		if final {
			want = FinalName(id)
		}
		// Leading zeros are the only allowed difference.
		if want != name && !bytes.Contains([]byte(name), []byte("-0")) {
			t.Fatalf("%q parsed as %d final=%v, which is %q", name, id, final, want)
		}
		if bytes.ContainsAny([]byte(name), "/\\\x00") || id < 0 {
			t.Fatalf("%q must not parse", name)
		}
	})
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package staging

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// hostileFixture seals a file of three segments (the last one short) and returns the sealed bytes and what is needed to open them.
func hostileFixture(t *testing.T) (s *Store, raw []byte, plain []byte, dk []byte) {
	t.Helper()
	s, _ = testStore(t, nil)
	plain = payload(2*SegmentSize+1000, 77)
	_, dk = sealFile(t, s, 1, plain)
	return s, mustRead(t, filepath.Join(s.Dir(), FinalName(1))), plain, dk
}

// openAndReadAll opens raw as transfer 1 with dk and reads all of it. It returns nil only if every byte was read and authenticated.
func openAndReadAll(raw []byte, id int64, dk []byte) ([]byte, error) {
	r, err := NewReader(&memFile{b: raw}, id, dk)
	if err != nil {
		return nil, err
	}
	b := make([]byte, r.Size())
	if _, err := r.ReadAt(b, 0); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return b, nil
}

func TestTamperedByteIsAlwaysDetected(t *testing.T) {
	_, raw, plain, dk := hostileFixture(t)
	if got, err := openAndReadAll(raw, 1, dk); err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("the untouched file must open: %v", err)
	}
	// Every byte of the header, the edges and the middle of every segment and its tag, and 300 random positions.
	pos := map[int]bool{}
	for i := 0; i < HeaderSize; i++ {
		pos[i] = true
	}
	for _, seg := range []int{0, 1, 2} {
		base := HeaderSize + seg*SegmentDisk
		for _, d := range []int{0, 1, 100, SegmentDisk/2 - 1} {
			pos[base+d] = true
		}
	}
	for _, p := range []int{len(raw) - 1, len(raw) - 16, len(raw) - 17, HeaderSize + SegmentDisk - 1, HeaderSize + SegmentDisk - 16, HeaderSize + SegmentDisk - 17} {
		pos[p] = true
	}
	rnd := rand.New(rand.NewSource(5)) //nolint:gosec // test
	for i := 0; i < 300; i++ {
		pos[rnd.Intn(len(raw))] = true
	}
	for p := range pos {
		if p >= len(raw) {
			continue
		}
		for _, bit := range []byte{1, 0x80} {
			m := append([]byte(nil), raw...)
			m[p] ^= bit
			if _, err := openAndReadAll(m, 1, dk); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("flipping bit %x of byte %d: err = %v, want ErrCorrupt", bit, p, err)
			}
		}
	}
}

func TestReorderedSegmentsAreRefused(t *testing.T) {
	_, raw, _, dk := hostileFixture(t)
	seg := func(i int) []byte { return raw[HeaderSize+i*SegmentDisk : HeaderSize+(i+1)*SegmentDisk] }
	// Swap the two full segments: the last is untouched, so the file opens, and the first read of a swapped segment fails.
	swapped := append([]byte(nil), raw[:HeaderSize]...)
	swapped = append(swapped, seg(1)...)
	swapped = append(swapped, seg(0)...)
	swapped = append(swapped, raw[HeaderSize+2*SegmentDisk:]...)
	if _, err := openAndReadAll(swapped, 1, dk); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("swapped full segments: %v", err)
	}
	// The same segment twice.
	dup := append([]byte(nil), raw[:HeaderSize]...)
	dup = append(dup, seg(0)...)
	dup = append(dup, seg(0)...)
	dup = append(dup, raw[HeaderSize+2*SegmentDisk:]...)
	if _, err := openAndReadAll(dup, 1, dk); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("a repeated segment: %v", err)
	}
	// A full segment moved to the end where the last one belongs (size changes, flag differs).
	moved := append([]byte(nil), raw[:HeaderSize]...)
	moved = append(moved, seg(1)...)
	moved = append(moved, seg(0)...)
	if _, err := openAndReadAll(moved, 1, dk); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("the last segment replaced by an earlier one: %v", err)
	}
	// Segments of another file with the same key and id would be a replay; the nonce prefix of the header stops it.
	_, rawB, _, _ := hostileFixture(t)
	mixed := append([]byte(nil), raw...)
	copy(mixed[HeaderSize:HeaderSize+SegmentDisk], rawB[HeaderSize:HeaderSize+SegmentDisk])
	if _, err := openAndReadAll(mixed, 1, dk); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("a segment of another file: %v", err)
	}
}

func TestTruncationIsDetectedAtEverySize(t *testing.T) {
	_, raw, _, dk := hostileFixture(t)
	sizes := []int{0, 1, HeaderSize - 1, HeaderSize, HeaderSize + 1, HeaderSize + tagSize - 1, HeaderSize + tagSize, HeaderSize + SegmentDisk - 1, HeaderSize + SegmentDisk,
		HeaderSize + SegmentDisk + 1, HeaderSize + 2*SegmentDisk - 1, HeaderSize + 2*SegmentDisk, HeaderSize + 2*SegmentDisk + 1, HeaderSize + 2*SegmentDisk + tagSize,
		len(raw) - 1, len(raw) - tagSize, len(raw) - tagSize - 1}
	for _, n := range sizes {
		if n >= len(raw) {
			continue
		}
		if _, err := openAndReadAll(raw[:n], 1, dk); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("truncated to %d: err = %v, want ErrCorrupt", n, err)
		}
	}
	// Dropping whole segments from the end is the dangerous case: every remaining segment is valid.
	for _, n := range []int{HeaderSize + SegmentDisk, HeaderSize + 2*SegmentDisk} {
		if _, err := NewReader(&memFile{b: raw[:n]}, 1, dk); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("dropped the last segment(s), %d bytes left: the open itself must fail, got %v", n, err)
		}
	}
}

func TestExtensionIsDetected(t *testing.T) {
	_, raw, _, dk := hostileFixture(t)
	seg0 := raw[HeaderSize : HeaderSize+SegmentDisk]
	// A valid full segment added after the real last one.
	if _, err := openAndReadAll(append(append([]byte(nil), raw...), seg0...), 1, dk); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("a segment appended: %v", err)
	}
	for _, extra := range []int{1, 15, 16, 17, 1000} {
		if _, err := openAndReadAll(append(append([]byte(nil), raw...), make([]byte, extra)...), 1, dk); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("%d bytes appended: %v", extra, err)
		}
	}
}

func TestWrongKeyAndWrongTransferAreRefused(t *testing.T) {
	_, raw, _, dk := hostileFixture(t)
	other := append([]byte(nil), dk...)
	other[0] ^= 1
	if _, err := openAndReadAll(raw, 1, other); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("wrong data key: %v", err)
	}
	if _, err := openAndReadAll(raw, 2, dk); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("the file read as another transfer: %v", err)
	}
	if _, err := openAndReadAll(raw, 1, dk[:16]); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("a 16-byte key is a valid AES size and must fail on authentication: %v", err)
	}
	if _, err := NewReader(&memFile{b: raw}, 1, dk[:5]); err == nil {
		t.Fatal("a key of 5 bytes was accepted")
	}
	// The same file renamed to another id on the disk does not open for that id.
	s, _ := testStore(t, nil)
	_, dk2 := sealFile(t, s, 1, []byte("hello"))
	if err := os.Rename(filepath.Join(s.Dir(), FinalName(1)), filepath.Join(s.Dir(), FinalName(2))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenReader(2, dk2); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("a file under another id: %v", err)
	}
}

func TestHeaderPaddingAndVersionAreChecked(t *testing.T) {
	_, raw, _, dk := hostileFixture(t)
	for _, i := range []int{4, 5, 6, 7, 12, 13, 14, 15, 0, 3} {
		m := append([]byte(nil), raw...)
		m[i] ^= 0x01
		if _, err := openAndReadAll(m, 1, dk); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("header byte %d: %v", i, err)
		}
	}
}

func TestEmptyFileAndLayoutRules(t *testing.T) {
	s, _ := testStore(t, nil)
	_, dk := sealFile(t, s, 1, nil)
	r, err := s.OpenReader(1, dk)
	if err != nil || r.Size() != 0 {
		t.Fatalf("empty file: %v %v", r, err)
	}
	if n, err := r.Read(make([]byte, 4)); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("Read of an empty file = %d %v", n, err)
	}
	for _, size := range []int64{-5, 0, 15, HeaderSize, HeaderSize + 15, HeaderSize + SegmentDisk + 16, HeaderSize + SegmentDisk + 3} {
		if _, _, _, ok := layout(size); ok {
			t.Errorf("layout(%d) accepted", size)
		}
	}
	if _, last, plain, ok := layout(HeaderSize + SegmentDisk); !ok || last != SegmentSize || plain != SegmentSize {
		t.Errorf("one full segment: %d %d %v", last, plain, ok)
	}
}

func TestReaderRefusesNotRegular(t *testing.T) {
	s, _ := testStore(t, nil)
	if err := os.Mkdir(filepath.Join(s.Dir(), FinalName(3)), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenReader(3, make([]byte, 32)); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("a directory named like a file: %v", err)
	}
	if _, err := s.OpenReader(4, make([]byte, 32)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a missing file: %v", err)
	}
}

func TestFileOfAnotherMasterKeyIsLostNotCorrupt(t *testing.T) {
	s, _ := testStore(t, nil)
	meta, _ := sealFile(t, s, 1, []byte("hello"))
	replaced := NewKeysForTest(9) // the key file was replaced
	if _, err := replaced.Unwrap(1, meta.KeyID, meta.WrappedKey); !errors.Is(err, ErrKeyLost) {
		t.Fatalf("a replaced master key: %v", err)
	}
	// The same master key, but the wrapped key of another file or a damaged one.
	meta2, _ := sealFile(t, s, 2, []byte("hello"))
	if _, err := testKeys.Unwrap(1, meta.KeyID, meta2.WrappedKey); !errors.Is(err, ErrKeyLost) {
		t.Fatalf("the wrapped key of another transfer: %v", err)
	}
	for _, mut := range []func([]byte) []byte{
		func(b []byte) []byte { b[0] ^= 1; return b },
		func(b []byte) []byte { b[len(b)-1] ^= 1; return b },
		func(b []byte) []byte { return b[:len(b)-1] },
		func(b []byte) []byte { return append(b, 0) },
		func([]byte) []byte { return nil },
	} {
		w := mut(append([]byte(nil), meta.WrappedKey...))
		if _, err := testKeys.Unwrap(1, meta.KeyID, w); !errors.Is(err, ErrKeyLost) {
			t.Fatalf("a damaged wrapped key: %v", err)
		}
	}
	if _, err := testKeys.Unwrap(1, "0000000000000000", meta.WrappedKey); !errors.Is(err, ErrKeyLost) {
		t.Fatalf("an unknown key id: %v", err)
	}
}

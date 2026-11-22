// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package staging

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

var boundarySizes = []int{0, 1, 2, SegmentSize - 1, SegmentSize, SegmentSize + 1, 2*SegmentSize - 1, 2 * SegmentSize, 2*SegmentSize + 1, 3*SegmentSize + 17, 5 * SegmentSize}

func TestRoundTripAtEveryBoundary(t *testing.T) {
	s, _ := testStore(t, nil)
	for i, n := range boundarySizes {
		id := int64(i + 1)
		data := payload(n, int64(n))
		_, dk := sealFile(t, s, id, data)
		st, disk, _ := s.State(id)
		nseg, _, plain, ok := layout(disk)
		if st != StateFinal || !ok || plain != int64(n) {
			t.Fatalf("n=%d: state %v disk %d layout %d %v", n, st, disk, plain, ok)
		}
		if want := int64(n/SegmentSize + 1); n%SegmentSize == 0 && n > 0 {
			if nseg != want-1 {
				t.Fatalf("n=%d: %d segments, want %d (the last one is full)", n, nseg, want-1)
			}
		} else if nseg != want {
			t.Fatalf("n=%d: %d segments, want %d", n, nseg, want)
		}
		r, err := s.OpenReader(id, dk)
		if err != nil {
			t.Fatalf("n=%d: OpenReader: %v", n, err)
		}
		if r.Size() != int64(n) {
			t.Fatalf("n=%d: Size = %d", n, r.Size())
		}
		got, err := io.ReadAll(r)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("n=%d: ReadAll differs (%v)", n, err)
		}
		_ = r.Close()
	}
}

func TestSealedResultMatchesTheDisk(t *testing.T) {
	s, _ := testStore(t, nil)
	for i, n := range boundarySizes {
		id := int64(100 + i)
		res, _ := s.Reserve(id, "o", 1<<20)
		w, _, err := s.Create(id, res, testKeys)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(payload(n, 1))
		sealed, err := w.Finish()
		if err != nil {
			t.Fatal(err)
		}
		_, disk, _ := s.State(id)
		if sealed.Plaintext != int64(n) || sealed.Disk != disk {
			t.Fatalf("n=%d: sealed %+v, disk %d", n, sealed, disk)
		}
		if res.Used() != disk {
			t.Fatalf("n=%d: the reservation counted %d bytes, the disk has %d", n, res.Used(), disk)
		}
	}
}

func TestWritesInOddPiecesGiveTheSameFile(t *testing.T) {
	s, _ := testStore(t, nil)
	data := payload(3*SegmentSize+1234, 5)
	_, dk := sealFile(t, s, 1, data)
	rnd := rand.New(rand.NewSource(1)) //nolint:gosec // test
	for _, piece := range []int{1, 7, 1000, SegmentSize - 1, SegmentSize, SegmentSize + 1} {
		id := int64(10 + piece%1000)
		res, _ := s.Reserve(id, "o", 1<<21)
		w, meta, err := s.Create(id, res, testKeys)
		if err != nil {
			t.Fatal(err)
		}
		for off := 0; off < len(data); {
			n := piece
			if piece == 1000 {
				n = 1 + rnd.Intn(5000)
			}
			if off+n > len(data) {
				n = len(data) - off
			}
			if _, err := w.Write(data[off : off+n]); err != nil {
				t.Fatal(err)
			}
			off += n
		}
		if _, err := w.Finish(); err != nil {
			t.Fatal(err)
		}
		k2, _ := testKeys.Unwrap(id, meta.KeyID, meta.WrappedKey)
		r, err := s.OpenReader(id, k2)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(r)
		if !bytes.Equal(got, data) {
			t.Fatalf("piece %d: plaintext differs", piece)
		}
	}
	_ = dk
}

func TestRangeAtEveryOffset(t *testing.T) {
	s, _ := testStore(t, nil)
	data := payload(3*SegmentSize+301, 9)
	_, dk := sealFile(t, s, 1, data)
	r, err := s.OpenReader(1, dk)
	if err != nil {
		t.Fatal(err)
	}
	// Every offset in a window around each boundary and the ends, with lengths that stay inside, cross one boundary and reach the end.
	var offsets []int
	for _, b := range []int{0, SegmentSize, 2 * SegmentSize, 3 * SegmentSize, len(data)} {
		for d := -3; d <= 3; d++ {
			if o := b + d; o >= 0 && o <= len(data) {
				offsets = append(offsets, o)
			}
		}
	}
	rnd := rand.New(rand.NewSource(2)) //nolint:gosec // test
	for i := 0; i < 200; i++ {
		offsets = append(offsets, rnd.Intn(len(data)+1))
	}
	for _, off := range offsets {
		for _, ln := range []int{0, 1, 2, SegmentSize, SegmentSize + 5, len(data)} {
			buf := make([]byte, ln)
			n, err := r.ReadAt(buf, int64(off))
			want := ln
			if off+ln > len(data) {
				want = len(data) - off
			}
			if n != want || !bytes.Equal(buf[:n], data[off:off+n]) {
				t.Fatalf("ReadAt(%d, %d): n=%d want %d", ln, off, n, want)
			}
			if n < ln && !errors.Is(err, io.EOF) {
				t.Fatalf("a short read must say EOF, got %v", err)
			}
			if n == ln && err != nil && !errors.Is(err, io.EOF) {
				t.Fatalf("a full read gave %v", err)
			}
		}
	}
	// Seek and Read.
	if pos, err := r.Seek(-10, io.SeekEnd); err != nil || pos != int64(len(data)-10) {
		t.Fatalf("seek: %d %v", pos, err)
	}
	tail, _ := io.ReadAll(r)
	if !bytes.Equal(tail, data[len(data)-10:]) {
		t.Fatal("tail differs")
	}
	if _, err := r.Seek(-1, io.SeekStart); err == nil {
		t.Fatal("negative seek accepted")
	}
	if _, err := r.ReadAt(make([]byte, 1), -1); err == nil {
		t.Fatal("negative offset accepted")
	}
}

func TestConcurrentRangeReads(t *testing.T) {
	s, _ := testStore(t, nil)
	data := payload(4*SegmentSize+9, 3)
	_, dk := sealFile(t, s, 1, data)
	r, _ := s.OpenReader(1, dk)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			rnd := rand.New(rand.NewSource(int64(g))) //nolint:gosec // test
			for i := 0; i < 200; i++ {
				off := rnd.Intn(len(data))
				buf := make([]byte, 1+rnd.Intn(100_000))
				n, _ := r.ReadAt(buf, int64(off))
				if !bytes.Equal(buf[:n], data[off:off+n]) {
					t.Errorf("concurrent read differs at %d", off)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}

func TestPlaintextNeverReachesTheDisk(t *testing.T) {
	s, _ := testStore(t, nil)
	secret := bytes.Repeat([]byte("TOP-SECRET-CANARY-0123456789abcdef"), 4000)
	sealFile(t, s, 1, secret)
	raw := mustRead(t, filepath.Join(s.Dir(), FinalName(1)))
	if bytes.Contains(raw, []byte("TOP-SECRET-CANARY")) {
		t.Fatal("plaintext found in the sealed file")
	}
	// Equal plaintext segments give different ciphertext (a counter in the nonce).
	a := raw[HeaderSize : HeaderSize+SegmentDisk]
	b := raw[HeaderSize+SegmentDisk : HeaderSize+2*SegmentDisk]
	if bytes.Equal(a[:64], b[:64]) {
		t.Fatal("two segments have the same ciphertext prefix")
	}
}

func TestFilesAreMode0600AndPartThenKdx(t *testing.T) {
	s, _ := testStore(t, nil)
	res, _ := s.Reserve(1, "o", 1<<20)
	w, _, err := s.Create(1, res, testKeys)
	if err != nil {
		t.Fatal(err)
	}
	if st, _, _ := s.State(1); st != StatePart {
		t.Fatalf("state during the write = %v", st)
	}
	if modesEnforced {
		info, _ := os.Stat(filepath.Join(s.Dir(), PartName(1)))
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %v", info.Mode().Perm())
		}
	}
	_, _ = w.Write([]byte("x"))
	if _, err := w.Finish(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), PartName(1))); err == nil {
		t.Fatal(".part left after Finish")
	}
	if modesEnforced {
		info, _ := os.Stat(filepath.Join(s.Dir(), FinalName(1)))
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("mode after rename = %v", info.Mode().Perm())
		}
	}
	// A reader cannot open a file that is still .part: no last segment exists yet.
	res2, _ := s.Reserve(2, "o", 1<<20)
	if _, _, err := s.Create(2, res2, testKeys); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenReader(2, make([]byte, 32)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("OpenReader on a .part: %v", err)
	}
}

func TestEveryFileHasItsOwnNoncePrefixAndKey(t *testing.T) {
	s, _ := testStore(t, nil)
	m1, _ := sealFile(t, s, 1, []byte("a"))
	m2, _ := sealFile(t, s, 2, []byte("a"))
	h1 := mustRead(t, filepath.Join(s.Dir(), FinalName(1)))
	h2 := mustRead(t, filepath.Join(s.Dir(), FinalName(2)))
	if bytes.Equal(h1[8:12], h2[8:12]) { // 2^-32 of failing by chance
		t.Fatal("two files share a nonce prefix")
	}
	if bytes.Equal(m1.WrappedKey, m2.WrappedKey) {
		t.Fatal("two files share a wrapped key")
	}
}

func TestNonceIsUniqueOverAMillionSegments(t *testing.T) {
	prefix := [4]byte{1, 2, 3, 4}
	seen := make(map[[nonceSize]byte]struct{}, 1_000_000)
	for i := int64(0); i < 1_000_000; i++ {
		var k [nonceSize]byte
		copy(k[:], nonceFor(prefix, i))
		if _, dup := seen[k]; dup {
			t.Fatalf("nonce repeated at segment %d", i)
		}
		seen[k] = struct{}{}
	}
	// Another prefix never collides with the same counters (the prefix is part of the nonce).
	var k [nonceSize]byte
	copy(k[:], nonceFor([4]byte{9, 9, 9, 9}, 5))
	if _, dup := seen[k]; dup {
		t.Fatal("a different prefix produced a nonce of the first file")
	}
}

func TestCreateTwiceAndConcurrentWriter(t *testing.T) {
	s, _ := testStore(t, nil)
	res, _ := s.Reserve(1, "o", 1<<20)
	w, _, err := s.Create(1, res, testKeys)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Create(1, res, testKeys); !errors.Is(err, ErrBusy) {
		t.Fatalf("second writer: %v", err)
	}
	_, _ = w.Write([]byte("x"))
	if _, err := w.Finish(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Create(1, res, testKeys); !errors.Is(err, ErrExists) {
		t.Fatalf("create over a sealed file: %v", err)
	}
	other, _ := s.Reserve(2, "o", 10)
	if _, _, err := s.Create(1, other, testKeys); err == nil {
		t.Fatal("a reservation of another transfer was accepted")
	}
}

func TestMaxFileBytes(t *testing.T) {
	s, _ := testStore(t, func(c *Config) { c.MaxFileBytes = 1000 })
	res, _ := s.Reserve(1, "o", 1<<20)
	w, _, _ := s.Create(1, res, testKeys)
	if _, err := w.Write(make([]byte, 1000)); err != nil {
		t.Fatalf("exactly the limit: %v", err)
	}
	if _, err := w.Write([]byte{1}); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("one byte over: %v", err)
	}
	if _, err := w.Finish(); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("Finish after the refusal: %v", err)
	}
}

// Uploads: a file is written in requests; every request but the last ends on a segment boundary.
func TestUploadAcrossRequests(t *testing.T) {
	s, _ := testStore(t, nil)
	data := payload(3*SegmentSize+100, 11)
	res, _ := s.Reserve(1, "o", 1<<20)
	w, meta, err := s.Create(1, res, testKeys)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write(data[:2*SegmentSize])
	off, err := w.Suspend()
	if err != nil || off != 2*SegmentSize {
		t.Fatalf("Suspend = %d %v", off, err)
	}
	if got, _ := s.PartOffset(1); got != 2*SegmentSize {
		t.Fatalf("PartOffset = %d", got)
	}
	w, err = s.OpenAppend(1, res, testKeys, meta)
	if err != nil {
		t.Fatal(err)
	}
	if w.Offset() != 2*SegmentSize {
		t.Fatalf("Offset after resume = %d", w.Offset())
	}
	_, _ = w.Write(data[2*SegmentSize:])
	if _, err := w.Finish(); err != nil {
		t.Fatal(err)
	}
	dk, _ := testKeys.Unwrap(1, meta.KeyID, meta.WrappedKey)
	r, err := s.OpenReader(1, dk)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(r); !bytes.Equal(got, data) {
		t.Fatal("plaintext differs")
	}
}

func TestUploadWhoseSizeIsAMultipleOfTheSegment(t *testing.T) {
	s, _ := testStore(t, nil)
	data := payload(3*SegmentSize, 12)
	res, _ := s.Reserve(1, "o", 1<<20)
	w, meta, _ := s.Create(1, res, testKeys)
	_, _ = w.Write(data[:SegmentSize])
	if off, err := w.Suspend(); err != nil || off != SegmentSize {
		t.Fatalf("Suspend = %d %v", off, err)
	}
	w, _ = s.OpenAppend(1, res, testKeys, meta)
	_, _ = w.Write(data[SegmentSize:])
	if _, err := w.Finish(); err != nil { // the final request ends exactly on a boundary and its last segment is full
		t.Fatal(err)
	}
	dk, _ := testKeys.Unwrap(1, meta.KeyID, meta.WrappedKey)
	r, err := s.OpenReader(1, dk)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(r); !bytes.Equal(got, data) {
		t.Fatal("plaintext differs")
	}
}

func TestUnalignedChunkIsDroppedAndTheOffsetSaysWhere(t *testing.T) {
	s, _ := testStore(t, nil)
	res, _ := s.Reserve(1, "o", 1<<20)
	w, meta, _ := s.Create(1, res, testKeys)
	_, _ = w.Write(payload(SegmentSize+100, 1)) // one whole segment and a tail
	off, err := w.Suspend()
	if !errors.Is(err, ErrUnaligned) || off != SegmentSize {
		t.Fatalf("Suspend = %d %v", off, err)
	}
	if _, err := w.Write([]byte("x")); !errors.Is(err, ErrClosed) {
		t.Fatalf("write after Suspend: %v", err)
	}
	_, size, _ := s.State(1)
	if size != HeaderSize+SegmentDisk {
		t.Fatalf("file size %d: the unaligned tail must not be kept", size)
	}
	// The client resumes at the offset it was told.
	data := payload(SegmentSize, 2)
	w, err = s.OpenAppend(1, res, testKeys, meta)
	if err != nil {
		t.Fatal(err)
	}
	if w.Offset() != SegmentSize {
		t.Fatalf("resume offset = %d", w.Offset())
	}
	_, _ = w.Write(data[:10])
	if _, err := w.Finish(); err != nil {
		t.Fatal(err)
	}
}

func TestAbortKeepsWholeSegmentsOnly(t *testing.T) {
	s, _ := testStore(t, nil)
	res, _ := s.Reserve(1, "o", 1<<20)
	w, _, _ := s.Create(1, res, testKeys)
	_, _ = w.Write(payload(2*SegmentSize+5, 1))
	if off := w.Abort(); off != 2*SegmentSize {
		t.Fatalf("Abort = %d", off)
	}
	if off, _ := s.PartOffset(1); off != 2*SegmentSize {
		t.Fatalf("PartOffset = %d", off)
	}
	// A broken request whose buffer holds exactly one full segment keeps it: it is whole plaintext.
	res2, _ := s.Reserve(2, "o", 1<<20)
	w2, _, _ := s.Create(2, res2, testKeys)
	_, _ = w2.Write(payload(SegmentSize, 1))
	if off := w2.Abort(); off != SegmentSize {
		t.Fatalf("Abort on a full buffer = %d", off)
	}
}

func TestCrashLeavesAPartialSegmentWhichResumeCutsOff(t *testing.T) {
	s, _ := testStore(t, nil)
	res, _ := s.Reserve(1, "o", 1<<20)
	w, meta, _ := s.Create(1, res, testKeys)
	_, _ = w.Write(payload(SegmentSize, 1))
	if _, err := w.Suspend(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(s.Dir(), PartName(1))
	f, _ := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
	_, _ = f.Write(bytes.Repeat([]byte{0xAA}, 5000)) // half a segment written when the process died
	_ = f.Close()
	if off, _ := s.PartOffset(1); off != SegmentSize {
		t.Fatalf("PartOffset must come from the whole segments: %d", off)
	}
	w, err := s.OpenAppend(1, res, testKeys, meta)
	if err != nil {
		t.Fatal(err)
	}
	if w.Offset() != SegmentSize {
		t.Fatalf("Offset = %d", w.Offset())
	}
	if info, _ := os.Stat(p); info.Size() != HeaderSize+SegmentDisk {
		t.Fatalf("the partial segment was not cut off: %d", info.Size())
	}
	_ = w.Abort()
}

func TestFinishRefusesAnEmptyLastSegmentAfterSealedOnes(t *testing.T) {
	s, _ := testStore(t, nil)
	res, _ := s.Reserve(1, "o", 1<<20)
	w, meta, _ := s.Create(1, res, testKeys)
	_, _ = w.Write(payload(SegmentSize, 1))
	_, _ = w.Suspend()
	w, _ = s.OpenAppend(1, res, testKeys, meta)
	if _, err := w.Finish(); !errors.Is(err, ErrEmptyTail) {
		t.Fatalf("Finish with nothing after sealed segments: %v", err)
	}
	if st, _, _ := s.State(1); st != StatePart {
		t.Fatal("the file must stay .part")
	}
}

func TestResumeNeedsTheKeyAndTheFile(t *testing.T) {
	s, _ := testStore(t, nil)
	res, _ := s.Reserve(1, "o", 1<<20)
	w, meta, _ := s.Create(1, res, testKeys)
	_, _ = w.Write(payload(SegmentSize, 1))
	_, _ = w.Suspend()
	if _, err := s.OpenAppend(1, res, NewKeysForTest(8), meta); !errors.Is(err, ErrKeyLost) {
		t.Fatalf("another master key: %v", err)
	}
	if _, err := s.OpenAppend(2, mustRes(t, s, 2), testKeys, meta); !errors.Is(err, ErrKeyLost) {
		t.Fatalf("the wrapped key of another transfer: %v", err)
	}
	// The failed attempts must not leave the file marked as being written.
	w, err := s.OpenAppend(1, res, testKeys, meta)
	if err != nil {
		t.Fatalf("a good resume after the failures: %v", err)
	}
	_ = w.Abort()
	// A damaged header stops a resume.
	p := filepath.Join(s.Dir(), PartName(1))
	raw := mustRead(t, p)
	raw[0] ^= 1
	mustWrite(t, p, raw)
	if _, err := s.OpenAppend(1, res, testKeys, meta); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("damaged header: %v", err)
	}
	if _, err := s.OpenAppend(77, mustRes(t, s, 77), testKeys, meta); !errors.Is(err, ErrKeyLost) && !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing file: %v", err)
	}
}

func mustRes(t *testing.T, s *Store, id int64) *Reservation {
	t.Helper()
	r, err := s.Reserve(id, "o", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A full disk, simulated through the test file system.
func TestFullDiskAtEveryPointOfAWrite(t *testing.T) {
	data := payload(2*SegmentSize+500, 4)
	total := int64(HeaderSize + len(data) + 3*tagSize)
	budgets := []int64{0, 1, HeaderSize - 1, HeaderSize, HeaderSize + 1, HeaderSize + SegmentDisk - 1, HeaderSize + SegmentDisk, HeaderSize + SegmentDisk + 1,
		HeaderSize + 2*SegmentDisk - 1, HeaderSize + 2*SegmentDisk, total - 1}
	for _, b := range budgets {
		s, ffs := testStore(t, nil)
		ffs.setBudget(b)
		res := mustRes(t, s, 1)
		w, _, err := s.Create(1, res, testKeys)
		if err == nil {
			_, err = w.Write(data)
		}
		if err == nil {
			_, err = w.Finish()
		}
		if !errors.Is(err, ErrInsufficientDisk) {
			t.Fatalf("budget %d: err = %v, want ErrInsufficientDisk", b, err)
		}
		var de *DiskError
		if !errors.As(err, &de) || !isDiskFull(de) {
			t.Fatalf("budget %d: the error must wrap the system's no-space error: %v", b, err)
		}
		if w != nil {
			if err := w.Discard(); err != nil {
				t.Fatalf("budget %d: Discard: %v", b, err)
			}
		}
		if st, _, _ := s.State(1); st != StateNone {
			t.Fatalf("budget %d: a failed export left a file", b)
		}
		// No .part is left for a rename either.
		entries, _ := os.ReadDir(s.Dir())
		for _, e := range entries {
			if e.Name() != MarkerName {
				t.Fatalf("budget %d: leftover %s", b, e.Name())
			}
		}
	}
}

func TestFullDiskDuringAnUploadKeepsTheWholeSegmentsAndResumes(t *testing.T) {
	s, ffs := testStore(t, nil)
	res := mustRes(t, s, 1)
	w, meta, _ := s.Create(1, res, testKeys)
	data := payload(4*SegmentSize, 8)
	_, _ = w.Write(data[:SegmentSize])
	if _, err := w.Suspend(); err != nil {
		t.Fatal(err)
	}
	w, _ = s.OpenAppend(1, res, testKeys, meta)
	ffs.setBudget(SegmentDisk + SegmentDisk/2) // one more segment fits, the next is cut in the middle
	_, err := w.Write(data[SegmentSize:])
	if err == nil {
		_, err = w.Suspend()
	}
	if !errors.Is(err, ErrInsufficientDisk) {
		t.Fatalf("err = %v", err)
	}
	off := w.Abort()
	if off != 2*SegmentSize {
		t.Fatalf("the offset must be the last whole segment, got %d", off)
	}
	if got, _ := s.PartOffset(1); got != off {
		t.Fatalf("PartOffset = %d, writer said %d", got, off)
	}
	_, size, _ := s.State(1)
	if size != HeaderSize+2*SegmentDisk {
		t.Fatalf("the cut segment is still on the disk: %d", size)
	}
	// Space comes back; the client resumes at the offset and the file completes.
	ffs.setBudget(-1)
	w, err = s.OpenAppend(1, res, testKeys, meta)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write(data[2*SegmentSize:])
	if _, err := w.Finish(); err != nil {
		t.Fatal(err)
	}
	dk, _ := testKeys.Unwrap(1, meta.KeyID, meta.WrappedKey)
	r, err := s.OpenReader(1, dk)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(r); !bytes.Equal(got, data) {
		t.Fatal("plaintext differs after the resume")
	}
}

func TestWriterThatExceedsTheQuotaFailsAndLeavesNoGarbage(t *testing.T) {
	s, _ := testStore(t, func(c *Config) { c.Quota = HeaderSize + 3*SegmentDisk })
	res, _ := s.Reserve(1, "o", 10)
	neighbour, _ := s.Reserve(2, "o", 0)
	w, _, err := s.Create(1, res, testKeys)
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.Write(payload(5*SegmentSize, 1))
	if err == nil {
		_, err = w.Finish()
	}
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("err = %v", err)
	}
	if neighbour.Reserved() != 0 || neighbour.Used() != 0 {
		t.Fatal("the neighbour was touched")
	}
	if res.Used() > HeaderSize+3*SegmentDisk {
		t.Fatalf("the writer used %d bytes past its quota", res.Used())
	}
	_ = w.Discard()
	res.Release()
	if holds := s.Holds(); len(holds) != 1 || holds[0].ID != 2 {
		t.Fatalf("holds = %+v", holds)
	}
}

func TestSealedFileDigestIsStableForTheSamePlaintext(t *testing.T) {
	// The SHA-256 of a download is the SHA-256 of the plaintext, not of the sealed file.
	s, _ := testStore(t, nil)
	data := payload(SegmentSize+3, 21)
	_, dk := sealFile(t, s, 1, data)
	r, _ := s.OpenReader(1, dk)
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(data)
	if !bytes.Equal(h.Sum(nil), want[:]) {
		t.Fatal("digest of the plaintext differs")
	}
}

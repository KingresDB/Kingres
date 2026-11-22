// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package staging

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
)

// The format of a sealed file: a header of 16 bytes, then segments. Every segment but the last holds 64 KiB of plaintext; the last holds
// 1 to 64 KiB (0 only in an empty file). A segment on the disk is its ciphertext followed by the 16-byte tag.
const (
	// SegmentSize is the plaintext of a full segment.
	SegmentSize = 64 << 10
	tagSize     = 16
	// SegmentDisk is the size of a full segment on the disk.
	SegmentDisk = SegmentSize + tagSize
	// HeaderSize is the size of the header.
	HeaderSize = 16
	nonceSize  = 12

	formatVersion = 1
)

var headerMagic = [4]byte{'K', 'D', 'X', 'F'}

// FileMeta is what the row keeps of a file's encryption: the id of the master key and the wrapped data key.
type FileMeta struct {
	KeyID      string
	WrappedKey []byte
}

func encodeHeader(prefix [4]byte) []byte {
	h := make([]byte, HeaderSize)
	copy(h, headerMagic[:])
	h[4] = formatVersion
	copy(h[8:12], prefix[:])
	return h
}

func parseHeader(h []byte) (prefix [4]byte, err error) {
	if len(h) != HeaderSize || [4]byte(h[:4]) != headerMagic || h[4] != formatVersion {
		return prefix, fmt.Errorf("%w: header", ErrCorrupt)
	}
	for _, i := range []int{5, 6, 7, 12, 13, 14, 15} {
		if h[i] != 0 {
			return prefix, fmt.Errorf("%w: header", ErrCorrupt)
		}
	}
	copy(prefix[:], h[8:12])
	return prefix, nil
}

func newAEAD(dataKey []byte) (cipher.AEAD, error) {
	b, err := aes.NewCipher(dataKey)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

// nonceFor is the 32-bit random prefix of the file and the 64-bit number of the segment.
func nonceFor(prefix [4]byte, seq int64) []byte {
	n := make([]byte, nonceSize)
	copy(n, prefix[:])
	binary.BigEndian.PutUint64(n[4:], uint64(seq)) //nolint:gosec // seq is not negative
	return n
}

// aadFor binds a segment to its file, its place and whether it is the last.
func aadFor(transferID, seq int64, last bool) []byte {
	a := make([]byte, 17)
	binary.BigEndian.PutUint64(a[0:], uint64(transferID)) //nolint:gosec
	binary.BigEndian.PutUint64(a[8:], uint64(seq))        //nolint:gosec
	if last {
		a[16] = 1
	}
	return a
}

// layout derives the segments from the size of the file on the disk; it never trusts a count kept elsewhere.
func layout(diskSize int64) (nseg int64, lastLen int, plain int64, ok bool) {
	if diskSize < HeaderSize+tagSize {
		return 0, 0, 0, false
	}
	l := diskSize - HeaderSize
	full, rem := l/SegmentDisk, l%SegmentDisk
	if rem == 0 {
		nseg, lastLen = full, SegmentSize
	} else {
		if rem < tagSize {
			return 0, 0, 0, false
		}
		nseg, lastLen = full+1, int(rem-tagSize)
	}
	if lastLen == 0 && nseg > 1 {
		return 0, 0, 0, false
	}
	return nseg, lastLen, (nseg-1)*SegmentSize + int64(lastLen), true
}

// Writer seals bytes into a file. It holds one segment of plaintext and nothing more.
type Writer struct {
	s      *Store
	id     int64
	f      File
	aead   cipher.AEAD
	prefix [4]byte
	res    *Reservation
	buf    []byte
	n      int
	seq    int64 // segments sealed and written
	err    error
	closed bool
}

func (s *Store) claimWriter(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open[id] {
		return ErrBusy
	}
	s.open[id] = true
	return nil
}

func (s *Store) releaseWriter(id int64) {
	s.mu.Lock()
	delete(s.open, id)
	s.mu.Unlock()
}

// Create starts the file of transfer id as transfer-<id>.part, with a new data key wrapped by keys. The reservation is the room it
// writes into; Use is called for every segment before it is written. The file is made with O_EXCL and mode 0600.
func (s *Store) Create(id int64, res *Reservation, keys *Keys) (*Writer, FileMeta, error) {
	if res == nil || res.id != id {
		return nil, FileMeta{}, fmt.Errorf("staging: the reservation is not the one of transfer %d", id)
	}
	if err := s.claimWriter(id); err != nil {
		return nil, FileMeta{}, err
	}
	w, meta, err := s.create(id, res, keys)
	if err != nil {
		s.releaseWriter(id)
		return nil, FileMeta{}, err
	}
	return w, meta, nil
}

func (s *Store) create(id int64, res *Reservation, keys *Keys) (*Writer, FileMeta, error) {
	st, _, err := s.State(id)
	if err != nil {
		return nil, FileMeta{}, err
	}
	if st != StateNone {
		return nil, FileMeta{}, ErrExists
	}
	dk, wrapped, err := keys.NewFileKey(id)
	if err != nil {
		return nil, FileMeta{}, err
	}
	aead, err := newAEAD(dk)
	if err != nil {
		return nil, FileMeta{}, err
	}
	var prefix [4]byte
	if _, err := io.ReadFull(rand.Reader, prefix[:]); err != nil {
		return nil, FileMeta{}, err
	}
	if err := res.Use(HeaderSize); err != nil {
		return nil, FileMeta{}, err
	}
	p := s.path(PartName(id))
	f, err := s.fs.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, FileMeta{}, mapDisk(err)
	}
	if _, err := f.Write(encodeHeader(prefix)); err != nil {
		_ = f.Close()
		_ = s.fs.Remove(p)
		return nil, FileMeta{}, mapDisk(err)
	}
	w := &Writer{s: s, id: id, f: f, aead: aead, prefix: prefix, res: res, buf: make([]byte, SegmentSize)}
	return w, FileMeta{KeyID: keys.ID(), WrappedKey: wrapped}, nil
}

// OpenAppend resumes a .part file (an upload between two requests). The segments that are whole stay; a partial segment that a crash left
// is cut off. The offset is then Offset(), computed from the file.
func (s *Store) OpenAppend(id int64, res *Reservation, keys *Keys, meta FileMeta) (*Writer, error) {
	if res == nil || res.id != id {
		return nil, fmt.Errorf("staging: the reservation is not the one of transfer %d", id)
	}
	dk, err := keys.Unwrap(id, meta.KeyID, meta.WrappedKey)
	if err != nil {
		return nil, err
	}
	if err := s.claimWriter(id); err != nil {
		return nil, err
	}
	w, err := s.openAppend(id, res, dk)
	if err != nil {
		s.releaseWriter(id)
		return nil, err
	}
	return w, nil
}

func (s *Store) openAppend(id int64, res *Reservation, dk []byte) (*Writer, error) {
	st, _, err := s.State(id)
	if err != nil {
		return nil, err
	}
	if st != StatePart {
		return nil, ErrNotFound
	}
	aead, err := newAEAD(dk)
	if err != nil {
		return nil, err
	}
	f, err := s.fs.OpenFile(s.path(PartName(id)), os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	fail := func(e error) (*Writer, error) { _ = f.Close(); return nil, e }
	info, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	if !info.Mode().IsRegular() {
		return fail(ErrNotRegular)
	}
	if info.Size() < HeaderSize {
		return fail(fmt.Errorf("%w: header", ErrCorrupt))
	}
	h := make([]byte, HeaderSize)
	if _, err := f.ReadAt(h, 0); err != nil {
		return fail(fmt.Errorf("%w: header", ErrCorrupt))
	}
	prefix, err := parseHeader(h)
	if err != nil {
		return fail(err)
	}
	seq := (info.Size() - HeaderSize) / SegmentDisk
	end := HeaderSize + seq*SegmentDisk
	if info.Size() != end {
		if err := f.Truncate(end); err != nil {
			return fail(err)
		}
	}
	if _, err := f.Seek(end, io.SeekStart); err != nil {
		return fail(err)
	}
	return &Writer{s: s, id: id, f: f, aead: aead, prefix: prefix, res: res, buf: make([]byte, SegmentSize), seq: seq}, nil
}

func mapDisk(err error) error {
	if isDiskFull(err) {
		return &DiskError{Cause: err}
	}
	return err
}

// Offset is the number of plaintext bytes this writer holds, sealed or not.
func (w *Writer) Offset() int64 { return w.seq*SegmentSize + int64(w.n) }

// Write adds plaintext. A full buffer is sealed only when more bytes follow, because the last segment must carry the last flag.
func (w *Writer) Write(p []byte) (int, error) {
	if w.closed {
		return 0, ErrClosed
	}
	if w.err != nil {
		return 0, w.err
	}
	if max := w.s.cfg.MaxFileBytes; max > 0 && w.Offset()+int64(len(p)) > max {
		w.err = ErrFileTooLarge
		return 0, w.err
	}
	done := 0
	for len(p) > 0 {
		if w.n == SegmentSize {
			if err := w.seal(false); err != nil {
				return done, err
			}
		}
		c := copy(w.buf[w.n:], p)
		w.n += c
		done += c
		p = p[c:]
	}
	return done, nil
}

// seal encrypts the buffer as segment w.seq and writes it. The reservation is charged first, so a refused growth writes nothing.
func (w *Writer) seal(last bool) error {
	if err := w.res.Use(int64(w.n) + tagSize); err != nil {
		w.err = err
		return err
	}
	ct := w.aead.Seal(nil, nonceFor(w.prefix, w.seq), w.buf[:w.n], aadFor(w.id, w.seq, last))
	if _, err := w.f.Write(ct); err != nil {
		w.err = mapDisk(err)
		return w.err
	}
	w.seq++
	w.n = 0
	return nil
}

func (w *Writer) finishFile() {
	if w.closed {
		return
	}
	w.closed = true
	_ = w.f.Close()
	w.s.releaseWriter(w.id)
}

// Sealed describes a finished file.
type Sealed struct {
	Plaintext int64 // bytes of plaintext
	Disk      int64 // bytes on the disk
}

// Finish seals the last segment (a full buffer too), syncs, and renames transfer-<id>.part to transfer-<id>.kdx. After an error the file is
// left as .part for Discard or for the janitor, and the writer is closed.
func (w *Writer) Finish() (Sealed, error) {
	if w.closed {
		return Sealed{}, ErrClosed
	}
	defer w.finishFile()
	if w.err != nil {
		return Sealed{}, w.err
	}
	if w.n == 0 && w.seq > 0 {
		w.err = ErrEmptyTail
		return Sealed{}, w.err
	}
	plain := w.Offset()
	if err := w.seal(true); err != nil {
		return Sealed{}, err
	}
	if err := w.f.Sync(); err != nil {
		return Sealed{}, mapDisk(err)
	}
	if err := w.f.Close(); err != nil {
		return Sealed{}, mapDisk(err)
	}
	if err := w.s.fs.Rename(w.s.path(PartName(w.id)), w.s.path(FinalName(w.id))); err != nil {
		return Sealed{}, err
	}
	return Sealed{Plaintext: plain, Disk: HeaderSize + plain + w.seq*tagSize}, nil
}

// Suspend ends a request of an upload that is not the last. A full buffer is sealed as a segment that is not the last, the file is synced
// and closed, and the new offset is returned. A buffer that is not full is the tail of an unaligned chunk: it is dropped and the call
// returns the offset of the last whole segment with ErrUnaligned, so the client is told where to resume.
func (w *Writer) Suspend() (int64, error) {
	if w.closed {
		return 0, ErrClosed
	}
	defer w.finishFile()
	if w.err != nil {
		return w.cutBack(), w.err
	}
	var result error
	if w.n == SegmentSize {
		if err := w.seal(false); err != nil {
			return w.cutBack(), err
		}
	} else if w.n != 0 {
		result = ErrUnaligned
		w.n = 0
	}
	if err := w.f.Sync(); err != nil {
		return w.cutBack(), mapDisk(err)
	}
	return w.seq * SegmentSize, result
}

// Abort ends a request that broke off: the whole segments stay, the rest is dropped, and the offset of the last whole segment is
// returned. A full buffer is whole plaintext and is sealed as a segment that is not the last.
func (w *Writer) Abort() int64 {
	if w.closed {
		return w.seq * SegmentSize
	}
	defer w.finishFile()
	if w.err == nil && w.n == SegmentSize {
		_ = w.seal(false)
	}
	return w.cutBack()
}

// cutBack removes whatever a failed write left after the last whole segment and returns the offset.
func (w *Writer) cutBack() int64 {
	_ = w.f.Truncate(HeaderSize + w.seq*SegmentDisk)
	_ = w.f.Sync()
	return w.seq * SegmentSize
}

// Discard closes the writer and deletes the file: the cleanup of a failed export.
func (w *Writer) Discard() error {
	w.finishFile()
	err := w.s.Remove(w.id)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

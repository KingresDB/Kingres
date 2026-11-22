// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package staging

import (
	"crypto/cipher"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sync"
)

// Reader reads the plaintext of a sealed file. It is an io.ReaderAt, an io.Reader and an io.Seeker, so a Range download is a ReadAt and
// archive/zip can open it. It keeps one decrypted segment. It is safe for concurrent use.
type Reader struct {
	f        File
	id       int64
	aead     cipher.AEAD
	prefix   [4]byte
	nseg     int64
	lastLen  int
	size     int64
	mu       sync.Mutex
	cacheSeq int64
	cache    []byte
	pos      int64
}

// OpenReader opens transfer-<id>.kdx with the data key (see Keys.Unwrap). It decrypts the last segment at once, so a truncated, extended
// or wrongly keyed file fails here and not in the middle of a download.
func (s *Store) OpenReader(id int64, dataKey []byte) (*Reader, error) {
	p := s.path(FinalName(id))
	info, err := s.fs.Lstat(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, ErrNotRegular
	}
	f, err := s.fs.OpenFile(p, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	r, err := NewReader(f, id, dataKey)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return r, nil
}

// NewReader reads a sealed file from f. It does not close f unless Close is called on the reader.
func NewReader(f File, id int64, dataKey []byte) (*Reader, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, ErrNotRegular
	}
	nseg, lastLen, plain, ok := layout(info.Size())
	if !ok {
		return nil, fmt.Errorf("%w: size", ErrCorrupt)
	}
	h := make([]byte, HeaderSize)
	if _, err := f.ReadAt(h, 0); err != nil {
		return nil, fmt.Errorf("%w: header", ErrCorrupt)
	}
	prefix, err := parseHeader(h)
	if err != nil {
		return nil, err
	}
	aead, err := newAEAD(dataKey)
	if err != nil {
		return nil, err
	}
	r := &Reader{f: f, id: id, aead: aead, prefix: prefix, nseg: nseg, lastLen: lastLen, size: plain, cacheSeq: -1}
	if _, err := r.segment(nseg - 1); err != nil {
		return nil, err
	}
	return r, nil
}

// Size is the length of the plaintext, known from the size of the file.
func (r *Reader) Size() int64 { return r.size }

// segment returns the plaintext of segment i. The slice is valid until the next call; callers hold r.mu or are in NewReader.
func (r *Reader) segment(i int64) ([]byte, error) {
	if i == r.cacheSeq {
		return r.cache, nil
	}
	n := int64(SegmentDisk)
	last := i == r.nseg-1
	if last {
		n = int64(r.lastLen) + tagSize
	}
	ct := make([]byte, n)
	if _, err := r.f.ReadAt(ct, HeaderSize+i*SegmentDisk); err != nil {
		return nil, fmt.Errorf("%w: segment %d", ErrCorrupt, i)
	}
	pt, err := r.aead.Open(ct[:0], nonceFor(r.prefix, i), ct, aadFor(r.id, i, last))
	if err != nil {
		return nil, fmt.Errorf("%w: segment %d", ErrCorrupt, i)
	}
	r.cacheSeq, r.cache = i, pt
	return pt, nil
}

// ReadAt implements io.ReaderAt. A read that reaches the end returns io.EOF with the bytes it has.
func (r *Reader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("staging: negative offset")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for n < len(p) {
		pos := off + int64(n)
		if pos >= r.size {
			return n, io.EOF
		}
		seg, err := r.segment(pos / SegmentSize)
		if err != nil {
			return n, err
		}
		n += copy(p[n:], seg[pos%SegmentSize:])
	}
	return n, nil
}

// Read implements io.Reader.
func (r *Reader) Read(p []byte) (int, error) {
	r.mu.Lock()
	pos := r.pos
	r.mu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	n, err := r.ReadAt(p, pos)
	r.mu.Lock()
	r.pos = pos + int64(n)
	r.mu.Unlock()
	if n > 0 && errors.Is(err, io.EOF) {
		err = nil
	}
	return n, err
}

// Seek implements io.Seeker.
func (r *Reader) Seek(offset int64, whence int) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var base int64
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		base = r.pos
	case io.SeekEnd:
		base = r.size
	default:
		return 0, fmt.Errorf("staging: bad whence")
	}
	np := base + offset
	if np < 0 {
		return 0, fmt.Errorf("staging: negative position")
	}
	r.pos = np
	return np, nil
}

// Close closes the file.
func (r *Reader) Close() error { return r.f.Close() }

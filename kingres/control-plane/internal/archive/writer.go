// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package archive

import (
	"compress/flate"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
)

// The records of the zip format that the writer produces and the validator accepts.
const (
	sigLocal      = 0x04034b50
	sigCentral    = 0x02014b50
	sigEOCD       = 0x06054b50
	sigEOCD64     = 0x06064b50
	sigLocator64  = 0x07064b50
	sigDescriptor = 0x08074b50

	flagDescriptor = 0x0008
	methodStore    = 0
	methodDeflate  = 8
	versionPlain   = 20
	versionZip64   = 45
	madeByUnix     = 3 << 8
	modeRegular    = 0o100644
	dosDate        = 0x0021 // 1980-01-01; the archive carries no time of the files
	dosTime        = 0
	mark32         = 0xFFFFFFFF
)

// ErrLimit is returned by the writer when the archive would pass the byte limit set with SetLimit.
var ErrLimit = errors.New("archive: the byte limit was reached")

// ErrOrder is returned for an entry that is not the next one the archive format allows.
var ErrOrder = errors.New("archive: the entry is not the next one of the archive")

type written struct {
	name       string
	offset     int64
	crc        uint32
	csize      uint64
	usize      uint64
	sha        string
	zip64      bool
	headerSize int64
}

// Writer writes an archive to a destination that is not seekable. Entries are written one after another in the fixed order (catalog.json, the three SQL
// files, the data entries from 1, and the manifest, which Finish writes); the writer never writes a shape that Validate refuses.
type Writer struct {
	w        io.Writer
	n        int64
	limit    int64
	force64  bool
	entries  []written
	cur      *entryWriter
	nextData int
	done     bool
	err      error
}

// NewWriter returns a writer to w.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w, nextData: 1} }

// SetLimit makes the writer fail with ErrLimit when it would write more than max bytes in all (the largest staged file). Zero is no limit.
func (w *Writer) SetLimit(max int64) { w.limit = max }

// ForceZip64 makes every entry and the end of the archive use the Zip64 form (a test hook, and for callers that know the archive will be above 4 GiB).
func (w *Writer) ForceZip64() { w.force64 = true }

// Offset is the number of bytes written so far.
func (w *Writer) Offset() int64 { return w.n }

func (w *Writer) raw(p []byte) error {
	if w.err != nil {
		return w.err
	}
	if w.limit > 0 && w.n+int64(len(p)) > w.limit {
		w.err = ErrLimit
		return w.err
	}
	n, err := w.w.Write(p)
	w.n += int64(n)
	if err != nil {
		w.err = err
	}
	return w.err
}

type entryWriter struct {
	w      *Writer
	name   string
	fw     *flate.Writer
	crc    hash.Hash32
	sha    hash.Hash
	usize  uint64
	csize  uint64
	off    int64
	closed bool
	// usizeLie is a test hook: the size that the descriptor and the central record say, when a test needs an entry that lies about its size.
	usizeLie *uint64
}

// compressedSink counts the compressed bytes that the deflate writer hands to the destination.
type compressedSink struct{ e *entryWriter }

func (s compressedSink) Write(p []byte) (int, error) {
	if err := s.e.w.raw(p); err != nil {
		return 0, err
	}
	s.e.csize += uint64(len(p))
	return len(p), nil
}

func (w *Writer) expect(name string) error {
	if w.done {
		return ErrOrder
	}
	switch k := len(w.entries); {
	case k < len(FixedNames):
		if name != FixedNames[k] {
			return fmt.Errorf("%w: %s", ErrOrder, safe(name))
		}
	default:
		if n, ok := IsData(name); !ok || n != w.nextData || n > MaxEntries-len(FixedNames)-1 {
			return fmt.Errorf("%w: %s", ErrOrder, safe(name))
		}
	}
	return nil
}

// Create starts the next entry and returns its writer. The previous entry is finished first. The entry is complete when the next Create or Finish is called
// (or when the returned writer is closed, if it is an io.Closer).
func (w *Writer) Create(name string) (io.Writer, error) {
	if err := w.closeCurrent(); err != nil {
		return nil, err
	}
	if err := w.expect(name); err != nil {
		return nil, err
	}
	return w.start(name)
}

func (w *Writer) start(name string) (*entryWriter, error) {
	e := &entryWriter{w: w, name: name, crc: crc32.NewIEEE(), sha: sha256.New(), off: w.n}
	hdr := make([]byte, 30, 30+len(name))
	binary.LittleEndian.PutUint32(hdr[0:], sigLocal)
	ver := uint16(versionPlain)
	if w.force64 {
		ver = versionZip64
	}
	binary.LittleEndian.PutUint16(hdr[4:], ver)
	binary.LittleEndian.PutUint16(hdr[6:], flagDescriptor)
	binary.LittleEndian.PutUint16(hdr[8:], methodDeflate)
	binary.LittleEndian.PutUint16(hdr[10:], dosTime)
	binary.LittleEndian.PutUint16(hdr[12:], dosDate)
	// crc, compressed and uncompressed size are zero: they follow in the descriptor.
	binary.LittleEndian.PutUint16(hdr[26:], uint16(len(name)))
	binary.LittleEndian.PutUint16(hdr[28:], 0)
	hdr = append(hdr, name...)
	if err := w.raw(hdr); err != nil {
		return nil, err
	}
	fw, err := flate.NewWriter(compressedSink{e}, flate.DefaultCompression)
	if err != nil {
		return nil, err
	}
	e.fw = fw
	w.cur = e
	return e, nil
}

func (e *entryWriter) Write(p []byte) (int, error) {
	if e.closed {
		return 0, errors.New("archive: the entry is closed")
	}
	n, err := e.fw.Write(p)
	e.crc.Write(p[:n])
	e.sha.Write(p[:n])
	e.usize += uint64(n)
	return n, err
}

// Close finishes the entry. It is called by the writer when the next entry starts; a caller may call it earlier.
func (e *entryWriter) Close() error {
	if e.closed {
		return nil
	}
	e.closed = true
	w := e.w
	if err := e.fw.Close(); err != nil {
		return err
	}
	usize := e.usize
	if e.usizeLie != nil {
		usize = *e.usizeLie
	}
	z := w.force64 || e.csize >= mark32 || usize >= mark32
	var d []byte
	if z {
		d = make([]byte, 24)
		binary.LittleEndian.PutUint64(d[8:], e.csize)
		binary.LittleEndian.PutUint64(d[16:], usize)
	} else {
		d = make([]byte, 16)
		binary.LittleEndian.PutUint32(d[8:], uint32(e.csize))
		binary.LittleEndian.PutUint32(d[12:], uint32(usize))
	}
	binary.LittleEndian.PutUint32(d[0:], sigDescriptor)
	binary.LittleEndian.PutUint32(d[4:], e.crc.Sum32())
	if err := w.raw(d); err != nil {
		return err
	}
	w.entries = append(w.entries, written{name: e.name, offset: e.off, crc: e.crc.Sum32(), csize: e.csize, usize: usize, sha: hex.EncodeToString(e.sha.Sum(nil)), zip64: z})
	if _, ok := IsData(e.name); ok {
		w.nextData++
	}
	w.cur = nil
	return nil
}

func (w *Writer) closeCurrent() error {
	if w.cur != nil {
		return w.cur.Close()
	}
	return nil
}

// Finish writes the manifest as the last entry and then the central directory. m is the manifest without its entries and totals; meta has, for every
// data entry, the members that only the exporter knows (table, columns, rows, consistency, groups, ledgerHistory), by entry name. The writer fills in the names,
// sizes and SHA-256 of what it wrote, and the totals, checks the structure of the manifest as the validator would, and returns the manifest it wrote.
func (w *Writer) Finish(m Manifest, meta map[string]ManifestEntry) (Manifest, error) {
	if err := w.closeCurrent(); err != nil {
		return m, err
	}
	if w.done {
		return m, ErrOrder
	}
	if len(w.entries) < len(FixedNames) {
		return m, fmt.Errorf("%w: the fixed entries are missing", ErrOrder)
	}
	m.Format, m.CatalogVersion = Format, CatalogVersion
	m.Entries = nil
	m.Totals = Totals{}
	for _, e := range w.entries {
		me := meta[e.name]
		me.Name, me.Size, me.SHA256 = e.name, int64(e.usize), e.sha
		if _, data := IsData(e.name); !data {
			me = ManifestEntry{Name: e.name, Size: int64(e.usize), SHA256: e.sha}
		} else if !me.LedgerHistory {
			m.Totals.Tables++
			m.Totals.Rows += me.Rows
		}
		m.Totals.UncompressedBytes += me.Size
		m.Entries = append(m.Entries, me)
	}
	if m.Warnings == nil {
		m.Warnings = []string{}
	}
	if m.Extensions == nil {
		m.Extensions = []ManifestExtension{}
	}
	if err := m.check(); err != nil {
		return m, err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return m, err
	}
	if len(b) > MaxManifestBytes {
		return m, invalid("manifest_size", ManifestName, "the manifest is larger than %d bytes", MaxManifestBytes)
	}
	e, err := w.start(ManifestName)
	if err != nil {
		return m, err
	}
	if _, err := e.Write(b); err != nil {
		return m, err
	}
	if err := e.Close(); err != nil {
		return m, err
	}
	if err := w.central(); err != nil {
		return m, err
	}
	w.done = true
	return m, nil
}

func (w *Writer) central() error {
	cdOff := w.n
	var cd []byte
	for _, e := range w.entries {
		z := e.zip64
		var extra []byte
		csize, usize, off := uint32(e.csize), uint32(e.usize), uint32(e.offset)
		if z {
			csize, usize = mark32, mark32
			extra = binary.LittleEndian.AppendUint16(extra, 1)
			if e.offset >= mark32 {
				off = mark32
				extra = binary.LittleEndian.AppendUint16(extra, 24)
				extra = binary.LittleEndian.AppendUint64(extra, e.usize)
				extra = binary.LittleEndian.AppendUint64(extra, e.csize)
				extra = binary.LittleEndian.AppendUint64(extra, uint64(e.offset))
			} else {
				extra = binary.LittleEndian.AppendUint16(extra, 16)
				extra = binary.LittleEndian.AppendUint64(extra, e.usize)
				extra = binary.LittleEndian.AppendUint64(extra, e.csize)
			}
		} else if e.offset >= mark32 {
			off = mark32
			extra = binary.LittleEndian.AppendUint16(extra, 1)
			extra = binary.LittleEndian.AppendUint16(extra, 8)
			extra = binary.LittleEndian.AppendUint64(extra, uint64(e.offset))
		}
		ver := uint16(versionPlain)
		if z || len(extra) > 0 {
			ver = versionZip64
		}
		h := make([]byte, 46)
		binary.LittleEndian.PutUint32(h[0:], sigCentral)
		binary.LittleEndian.PutUint16(h[4:], madeByUnix|versionZip64)
		binary.LittleEndian.PutUint16(h[6:], ver)
		binary.LittleEndian.PutUint16(h[8:], flagDescriptor)
		binary.LittleEndian.PutUint16(h[10:], methodDeflate)
		binary.LittleEndian.PutUint16(h[12:], dosTime)
		binary.LittleEndian.PutUint16(h[14:], dosDate)
		binary.LittleEndian.PutUint32(h[16:], e.crc)
		binary.LittleEndian.PutUint32(h[20:], csize)
		binary.LittleEndian.PutUint32(h[24:], usize)
		binary.LittleEndian.PutUint16(h[28:], uint16(len(e.name)))
		binary.LittleEndian.PutUint16(h[30:], uint16(len(extra)))
		binary.LittleEndian.PutUint32(h[38:], uint32(modeRegular)<<16)
		binary.LittleEndian.PutUint32(h[42:], off)
		cd = append(cd, h...)
		cd = append(cd, e.name...)
		cd = append(cd, extra...)
	}
	if err := w.raw(cd); err != nil {
		return err
	}
	cdSize := int64(len(cd))
	n := len(w.entries)
	need64 := w.force64 || cdOff >= mark32 || cdSize >= mark32 || n >= 0xFFFF
	var tail []byte
	if need64 {
		eocd64Off := w.n
		r := make([]byte, 56)
		binary.LittleEndian.PutUint32(r[0:], sigEOCD64)
		binary.LittleEndian.PutUint64(r[4:], 44)
		binary.LittleEndian.PutUint16(r[12:], madeByUnix|versionZip64)
		binary.LittleEndian.PutUint16(r[14:], versionZip64)
		binary.LittleEndian.PutUint64(r[24:], uint64(n))
		binary.LittleEndian.PutUint64(r[32:], uint64(n))
		binary.LittleEndian.PutUint64(r[40:], uint64(cdSize))
		binary.LittleEndian.PutUint64(r[48:], uint64(cdOff))
		l := make([]byte, 20)
		binary.LittleEndian.PutUint32(l[0:], sigLocator64)
		binary.LittleEndian.PutUint64(l[8:], uint64(eocd64Off))
		binary.LittleEndian.PutUint32(l[16:], 1)
		tail = append(tail, r...)
		tail = append(tail, l...)
	}
	e := make([]byte, 22)
	binary.LittleEndian.PutUint32(e[0:], sigEOCD)
	if need64 {
		binary.LittleEndian.PutUint16(e[8:], 0xFFFF)
		binary.LittleEndian.PutUint16(e[10:], 0xFFFF)
		binary.LittleEndian.PutUint32(e[12:], mark32)
		binary.LittleEndian.PutUint32(e[16:], mark32)
	} else {
		binary.LittleEndian.PutUint16(e[8:], uint16(n))
		binary.LittleEndian.PutUint16(e[10:], uint16(n))
		binary.LittleEndian.PutUint32(e[12:], uint32(cdSize))
		binary.LittleEndian.PutUint32(e[16:], uint32(cdOff))
	}
	tail = append(tail, e...)
	return w.raw(tail)
}

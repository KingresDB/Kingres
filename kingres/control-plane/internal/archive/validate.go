// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package archive

import (
	"bufio"
	"bytes"
	"compress/flate"
	"crypto/sha256"
	"encoding/binary"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"io"
	"sort"
)

// SQLResult is what the caller's checker found in one SQL file.
type SQLResult struct {
	ByKind  map[string]int
	Refused []RefusedStatement
}

// RefusedStatement is a statement the shape grammar refused: its kind, its position, a fingerprint of it and the reason; never its text.
type RefusedStatement struct {
	Kind        string
	Position    string
	Fingerprint string
	Reason      string
}

// SQLChecker is called once for each of the three SQL files with the stream of the file, bounded by its declared size. It need not read all of it.
type SQLChecker func(name string, r io.Reader) (SQLResult, error)

// Limits are the limits of a validation. A zero value is the default of the design.
type Limits struct {
	MaxImportBytes int64 // the sum of the declared sizes of the entries; default 100 GiB
	MaxEntries     int   // default MaxEntries
	SQL            SQLChecker
	// SQLFor, when set, makes the checker of the SQL files from the catalog document (which is read before them): the shape grammar needs to know which
	// tables inherit. It is used instead of SQL.
	SQLFor func(cat *Catalog) SQLChecker
}

// EntryFact is what the validator found out about one entry.
type EntryFact struct {
	Name           string
	Offset         int64
	CompressedSize int64
	Size           int64
	SHA256         string
	CRC32          uint32
	Zip64          bool
}

// Result is the outcome of a validation that found the archive well formed.
type Result struct {
	Manifest Manifest
	Catalog  Catalog
	Entries  []EntryFact
	SQL      map[string]SQLResult
	// DataBytes is the sum of the sizes of the entries (without the manifest).
	DataBytes int64
	// FileSHA256 is set by the caller; Validate reads the file through an io.ReaderAt and does not hash it as a whole.
	FileSHA256 string
}

type central struct {
	name   string
	crc    uint32
	csize  uint64
	usize  uint64
	offset uint64
	zip64  bool
	method uint16
	time   uint16
	date   uint16
	flags  uint16
}

type ctx struct {
	ra   io.ReaderAt
	size int64
}

func (c ctx) read(off int64, n int) ([]byte, error) {
	if off < 0 || n < 0 || off+int64(n) > c.size {
		return nil, invalid("extent", "", "a record reaches outside the file")
	}
	b := make([]byte, n)
	if _, err := c.ra.ReadAt(b, off); err != nil && !(errors.Is(err, io.EOF) && len(b) == n) {
		return nil, invalid("read", "", "the file cannot be read")
	}
	return b, nil
}

func le16(b []byte) uint16 { return binary.LittleEndian.Uint16(b) }
func le32(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }
func le64(b []byte) uint64 { return binary.LittleEndian.Uint64(b) }

// Validate checks an archive from the first byte to the last. It reads every entry once, writes nothing, and never uses a name of the archive as a path.
// The error is an *Error with CodeInvalid or CodeIncompatible. The shape it accepts is exactly the one Writer produces.
func Validate(ra io.ReaderAt, size int64, lim Limits) (*Result, error) {
	if lim.MaxImportBytes <= 0 {
		lim.MaxImportBytes = 100 << 30
	}
	if lim.MaxEntries <= 0 || lim.MaxEntries > MaxEntries {
		lim.MaxEntries = MaxEntries
	}
	c := ctx{ra: ra, size: size}
	ents, cdOff, err := c.directory(lim)
	if err != nil {
		return nil, err
	}
	if err := c.extents(ents, cdOff); err != nil {
		return nil, err
	}
	if err := checkNames(ents); err != nil {
		return nil, err
	}
	var sum uint64
	for _, e := range ents {
		sum += e.usize
		if e.usize > uint64(lim.MaxImportBytes) || sum > uint64(lim.MaxImportBytes) {
			return nil, invalid("size", "", "the declared sizes of the entries are above the limit")
		}
	}
	last := ents[len(ents)-1]
	if last.usize > MaxManifestBytes {
		return nil, invalid("manifest_size", ManifestName, "the manifest is larger than %d bytes", MaxManifestBytes)
	}
	var mbuf bytes.Buffer
	if _, _, err := c.stream(last, func(r io.Reader) error { _, e := io.Copy(&mbuf, r); return e }); err != nil {
		return nil, err
	}
	m, err := ParseManifest(mbuf.Bytes())
	if err != nil {
		return nil, err
	}
	if len(m.Entries) != len(ents)-1 {
		return nil, invalid("manifest", ManifestName, "the manifest does not list the entries of the archive")
	}
	res := &Result{Manifest: m, SQL: map[string]SQLResult{}}
	var cat *Catalog
	byTable := map[string]*CatalogTable{}
	for i, e := range ents[:len(ents)-1] {
		me := m.Entries[i]
		if me.Name != e.name || me.Size != int64(e.usize) {
			return nil, invalid("manifest", e.name, "the manifest does not agree with the archive on an entry")
		}
		var sha string
		var fact EntryFact
		var herr error
		handler := func(r io.Reader) error {
			switch {
			case e.name == CatalogName:
				if e.usize > MaxCatalogBytes {
					return invalid("catalog_size", CatalogName, "the catalog is larger than %d bytes", MaxCatalogBytes)
				}
				b, err := io.ReadAll(r)
				if err != nil {
					return err
				}
				cc, err := ParseCatalog(b)
				if err != nil {
					return err
				}
				cat = &cc
				for k := range cat.Tables {
					byTable[cat.Tables[k].Qualified()] = &cat.Tables[k]
				}
				return nil
			case IsSQL(e.name):
				if e.usize > MaxSQLBytes {
					return invalid("sql_size", e.name, "an SQL file is larger than %d bytes", MaxSQLBytes)
				}
				check := lim.SQL
				if lim.SQLFor != nil && cat != nil {
					check = lim.SQLFor(cat)
				}
				if check == nil {
					return nil
				}
				sr, err := check(e.name, r)
				if err != nil {
					herr = err
					return nil
				}
				res.SQL[e.name] = sr
				return nil
			default:
				if cat == nil {
					return invalid("order", e.name, "a data entry comes before the catalog")
				}
				return checkHeader(r, me, cat, byTable, m.Options)
			}
		}
		sha, fact, err = c.stream2(e, handler)
		if err != nil {
			return nil, err
		}
		if herr != nil {
			return nil, herr
		}
		if sha != me.SHA256 {
			return nil, invalid("sha256", e.name, "the SHA-256 of an entry is not the one of the manifest")
		}
		fact.SHA256 = sha
		res.Entries = append(res.Entries, fact)
		res.DataBytes += fact.Size
	}
	if cat == nil {
		return nil, invalid("catalog", CatalogName, "the catalog is missing")
	}
	if err := coherent(m, *cat, byTable); err != nil {
		return nil, err
	}
	res.Catalog = *cat
	return res, nil
}

// directory finds the end record, reads the central directory and returns its entries and its offset.
func (c ctx) directory(lim Limits) ([]central, int64, error) {
	if c.size < 22 {
		return nil, 0, invalid("eocd", "", "the file is too short to be an archive")
	}
	// The writer writes no comment, so the end record is the last 22 bytes of the file; a comment could hide bytes.
	tail, err := c.read(c.size-22, 22)
	if err != nil {
		return nil, 0, err
	}
	if le32(tail) != sigEOCD || le16(tail[20:]) != 0 {
		return nil, 0, invalid("eocd", "", "the file has no end record at its end, or the end record has a comment")
	}
	eocd := 0
	eocdOff := c.size - 22
	e := tail[eocd:]
	if le16(e[4:]) != 0 || le16(e[6:]) != 0 {
		return nil, 0, invalid("eocd", "", "an archive of several disks")
	}
	n, cdSize, cdOff := uint64(le16(e[10:])), uint64(le32(e[12:])), uint64(le32(e[16:]))
	if le16(e[8:]) != le16(e[10:]) {
		return nil, 0, invalid("eocd", "", "the end record counts entries of another disk")
	}
	cdEnd := eocdOff
	if n == 0xFFFF || cdSize == mark32 || cdOff == mark32 {
		// Zip64: the locator must be right before the end record and name the record right before it.
		if eocdOff < 20+56 {
			return nil, 0, invalid("eocd", "", "the Zip64 records are missing")
		}
		loc, err := c.read(eocdOff-20, 20)
		if err != nil {
			return nil, 0, err
		}
		if le32(loc) != sigLocator64 || le32(loc[4:]) != 0 || le32(loc[16:]) != 1 {
			return nil, 0, invalid("eocd", "", "the Zip64 locator is not valid")
		}
		off64 := le64(loc[8:])
		if off64 != uint64(eocdOff-20-56) {
			return nil, 0, invalid("eocd", "", "the Zip64 end record is not where the locator says, or has an extension")
		}
		r, err := c.read(int64(off64), 56)
		if err != nil {
			return nil, 0, err
		}
		if le32(r) != sigEOCD64 || le64(r[4:]) != 44 || le32(r[16:]) != 0 || le32(r[20:]) != 0 || le64(r[24:]) != le64(r[32:]) {
			return nil, 0, invalid("eocd", "", "the Zip64 end record is not valid")
		}
		if (n != 0xFFFF && n != le64(r[24:])) || (cdSize != mark32 && cdSize != le64(r[40:])) || (cdOff != mark32 && cdOff != le64(r[48:])) {
			return nil, 0, invalid("eocd", "", "the two end records disagree")
		}
		n, cdSize, cdOff = le64(r[24:]), le64(r[40:]), le64(r[48:])
		cdEnd = int64(off64)
	}
	if n > uint64(lim.MaxEntries) || n < uint64(len(FixedNames)+1) {
		return nil, 0, invalid("entries", "", "the number of entries is out of range")
	}
	if cdSize > n*(46+MaxNameBytes+28) || cdOff > uint64(c.size) || cdOff+cdSize != uint64(cdEnd) {
		return nil, 0, invalid("eocd", "", "the central directory is not where the end record says, or is not exact")
	}
	cd, err := c.read(int64(cdOff), int(cdSize))
	if err != nil {
		return nil, 0, err
	}
	var out []central
	pos := 0
	seen := map[string]bool{}
	for i := uint64(0); i < n; i++ {
		if len(cd)-pos < 46 || le32(cd[pos:]) != sigCentral {
			return nil, 0, invalid("central", "", "a record of the central directory is not valid")
		}
		h := cd[pos:]
		nameLen, extraLen, comLen := int(le16(h[28:])), int(le16(h[30:])), int(le16(h[32:]))
		if nameLen == 0 || nameLen > MaxNameBytes {
			return nil, 0, invalid("name", "", "an entry name is empty or longer than %d bytes", MaxNameBytes)
		}
		if comLen != 0 || le16(h[34:]) != 0 || le16(h[36:]) != 0 {
			return nil, 0, invalid("central", "", "an entry has a comment, a disk number or internal attributes")
		}
		if len(cd)-pos < 46+nameLen+extraLen {
			return nil, 0, invalid("central", "", "a record of the central directory is cut")
		}
		name := string(h[46 : 46+nameLen])
		extra := h[46+nameLen : 46+nameLen+extraLen]
		pos += 46 + nameLen + extraLen
		if seen[name] {
			return nil, 0, invalid("name", name, "an entry name is used twice")
		}
		seen[name] = true
		made, ver := le16(h[4:]), le16(h[6:])
		if made>>8 == 3 { // Unix: the mode says what the entry is
			if mode := le32(h[38:]) >> 16; mode&0xF000 != 0x8000 {
				return nil, 0, invalid("mode", name, "an entry is not a regular file")
			}
		} else if le32(h[38:])&0x10 != 0 {
			return nil, 0, invalid("mode", name, "an entry is a directory")
		}
		if ver != versionPlain && ver != versionZip64 {
			return nil, 0, invalid("central", name, "an entry needs a version of the format that is not used")
		}
		en := central{name: name, flags: le16(h[8:]), method: le16(h[10:]), time: le16(h[12:]), date: le16(h[14:]), crc: le32(h[16:])}
		cs, us, off := uint64(le32(h[20:])), uint64(le32(h[24:])), uint64(le32(h[42:]))
		wantExtra := 0
		if cs == mark32 || us == mark32 {
			if cs != mark32 || us != mark32 {
				return nil, 0, invalid("zip64", name, "the sizes of an entry are marked Zip64 on one side only")
			}
			wantExtra += 16
			en.zip64 = true
		}
		if off == mark32 {
			wantExtra += 8
		}
		if wantExtra == 0 {
			if extraLen != 0 {
				return nil, 0, invalid("extra", name, "an entry has extra fields")
			}
		} else {
			if extraLen != 4+wantExtra || le16(extra) != 1 || int(le16(extra[2:])) != wantExtra {
				return nil, 0, invalid("extra", name, "the Zip64 field of an entry is not the one the writer makes")
			}
			p := extra[4:]
			if en.zip64 {
				us, cs = le64(p), le64(p[8:])
				p = p[16:]
			}
			if off == mark32 {
				off = le64(p)
			}
		}
		en.csize, en.usize, en.offset = cs, us, off
		if en.csize > uint64(c.size) || en.offset > uint64(c.size) || en.usize > 1<<62 {
			return nil, 0, invalid("extent", name, "an entry reaches outside the file")
		}
		out = append(out, en)
	}
	if pos != len(cd) {
		return nil, 0, invalid("central", "", "the central directory has bytes that no entry accounts for")
	}
	return out, int64(cdOff), nil
}

// extents reads and compares every local header and descriptor, and checks that the extents of the entries, the central directory and the end records
// account for every byte of the file.
func (c ctx) extents(ents []central, cdOff int64) error {
	order := make([]int, len(ents))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return ents[order[a]].offset < ents[order[b]].offset })
	pos := int64(0)
	for k, i := range order {
		e := &ents[i]
		if k != i {
			return invalid("order", e.name, "the entries are not in the order of their central records")
		}
		if int64(e.offset) != pos {
			return invalid("extent", e.name, "an entry does not start where the one before it ends (hidden or overlapping bytes)")
		}
		hdrLen := 30 + len(e.name)
		h, err := c.read(pos, hdrLen)
		if err != nil {
			return err
		}
		if le32(h) != sigLocal {
			return invalid("local", e.name, "a local header is not valid")
		}
		if le16(h[26:]) != uint16(len(e.name)) || string(h[30:]) != e.name {
			return invalid("local", e.name, "the name in a local header is not the name in the central directory")
		}
		if le16(h[28:]) != 0 {
			return invalid("local", e.name, "a local header has an extra field")
		}
		if le16(h[6:]) != e.flags {
			return invalid("local", e.name, "the flags of a local header are not those of the central directory")
		}
		if e.flags&flagDescriptor == 0 {
			return invalid("flags", e.name, "an entry has no data descriptor; the archive format of this server always has one")
		}
		if e.flags != flagDescriptor {
			return invalid("flags", e.name, "an entry has flags that the writer does not set")
		}
		if le16(h[8:]) != e.method || (e.method != methodDeflate && e.method != methodStore) {
			return invalid("method", e.name, "an entry has a method that is not Store or Deflate, or one that differs from the central one")
		}
		if le16(h[10:]) != e.time || le16(h[12:]) != e.date {
			return invalid("local", e.name, "the time of a local header is not the central one")
		}
		if le32(h[14:]) != 0 || le32(h[18:]) != 0 || le32(h[22:]) != 0 {
			return invalid("local", e.name, "a local header holds sizes or a CRC although a descriptor follows")
		}
		if v := le16(h[4:]); v != versionPlain && v != versionZip64 {
			return invalid("local", e.name, "a local header needs a version that is not used")
		}
		dlen := 16
		if e.zip64 {
			dlen = 24
		}
		descOff := pos + int64(hdrLen) + int64(e.csize)
		if descOff < pos || descOff+int64(dlen) > cdOff {
			return invalid("extent", e.name, "an entry reaches into the central directory or outside the file")
		}
		d, err := c.read(descOff, dlen)
		if err != nil {
			return err
		}
		var cs, us uint64
		if e.zip64 {
			cs, us = le64(d[8:]), le64(d[16:])
		} else {
			cs, us = uint64(le32(d[8:])), uint64(le32(d[12:]))
		}
		if le32(d) != sigDescriptor || le32(d[4:]) != e.crc || cs != e.csize || us != e.usize {
			return invalid("descriptor", e.name, "the data descriptor of an entry is not the central record")
		}
		if e.method == methodStore && e.csize != e.usize {
			return invalid("method", e.name, "a stored entry has two sizes")
		}
		pos = descOff + int64(dlen)
	}
	if pos != cdOff {
		return invalid("extent", "", "bytes between the last entry and the central directory")
	}
	return nil
}

func checkNames(ents []central) error {
	if len(ents) < len(FixedNames)+1 {
		return invalid("names", "", "the archive lacks required entries")
	}
	for i, n := range FixedNames {
		if ents[i].name != n {
			return invalid("names", ents[i].name, "the fixed entries are not the expected ones in the expected order")
		}
	}
	data := ents[len(FixedNames) : len(ents)-1]
	for i, e := range data {
		if n, ok := IsData(e.name); !ok || n != i+1 {
			return invalid("names", e.name, "a name is not a data entry, or the data entries are not numbered from 1 without a gap")
		}
	}
	if ents[len(ents)-1].name != ManifestName {
		return invalid("names", ents[len(ents)-1].name, "the manifest is not the last entry")
	}
	return nil
}

// countingByteReader reads from a section of the file and counts what the decompressor takes.
type countingByteReader struct {
	r *bufio.Reader
	n int64
}

func (c *countingByteReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *countingByteReader) ReadByte() (byte, error) {
	b, err := c.r.ReadByte()
	if err == nil {
		c.n++
	}
	return b, err
}

// stream is stream2 without the digest.
func (c ctx) stream(e central, h func(io.Reader) error) (string, EntryFact, error) {
	return c.stream2(e, h)
}

// stream2 reads one entry once: the compressed bytes exactly, the decompressed bytes counted with a limit at the declared size plus one, the CRC-32 and the
// SHA-256 on the way. The handler sees the bounded stream; what it does not read is read afterwards.
func (c ctx) stream2(e central, handler func(io.Reader) error) (string, EntryFact, error) {
	data := e.offset + uint64(30+len(e.name))
	sr := io.NewSectionReader(c.ra, int64(data), int64(e.csize))
	cb := &countingByteReader{r: bufio.NewReaderSize(sr, 32<<10)}
	var src io.Reader
	var fr io.ReadCloser
	if e.method == methodDeflate {
		fr = flate.NewReader(cb)
		src = fr
	} else {
		src = cb
	}
	sha := sha256.New()
	crc := crc32.NewIEEE()
	lr := &io.LimitedReader{R: src, N: int64(e.usize) + 1}
	tee := io.TeeReader(lr, io.MultiWriter(sha, crc))
	hErr := handler(tee)
	if hErr != nil {
		var ae *Error
		if !errors.As(hErr, &ae) {
			hErr = invalid("data", e.name, "the data of an entry cannot be read")
		}
		return "", EntryFact{}, hErr
	}
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return "", EntryFact{}, invalid("data", e.name, "the data of an entry cannot be read")
	}
	got := int64(e.usize) + 1 - lr.N
	if got != int64(e.usize) {
		return "", EntryFact{}, invalid("size", e.name, "an entry does not have the size that its header declares")
	}
	// The compressed stream must end exactly at the end of the compressed extent.
	if e.method == methodDeflate {
		var one [1]byte
		if n, _ := fr.Read(one[:]); n != 0 {
			return "", EntryFact{}, invalid("size", e.name, "an entry has more data than its header declares")
		}
		_ = fr.Close()
	}
	if cb.n != int64(e.csize) {
		return "", EntryFact{}, invalid("size", e.name, "the compressed data of an entry does not end where its header says")
	}
	if crc.Sum32() != e.crc {
		return "", EntryFact{}, invalid("crc", e.name, "the CRC-32 of an entry is wrong")
	}
	s := hex.EncodeToString(sha.Sum(nil))
	return s, EntryFact{Name: e.name, Offset: int64(e.offset), CompressedSize: int64(e.csize), Size: int64(e.usize), CRC32: e.crc, Zip64: e.zip64}, nil
}

// checkHeader compares the first record of a data entry with the columns of the manifest.
func checkHeader(r io.Reader, me ManifestEntry, cat *Catalog, byTable map[string]*CatalogTable, o Options) error {
	t := byTable[me.Table]
	if t == nil {
		return invalid("data", me.Name, "a data entry is for a table that is not in the catalog")
	}
	cols := t.Columns
	if me.LedgerHistory {
		if t.Ledger == nil || len(t.Ledger.HistoryColumns) == 0 {
			return invalid("data", me.Name, "a ledger history entry is for a table that has no history")
		}
		cols = t.Ledger.HistoryColumns
	}
	if !equalStrings(me.Columns, cols) {
		return invalid("columns", me.Name, "the columns of a data entry are not the columns of the schema")
	}
	cr := csv.NewReader(io.LimitReader(r, 1<<20))
	cr.FieldsPerRecord = -1
	rec, err := cr.Read()
	if err != nil || !equalStrings(rec, cols) {
		return invalid("header", me.Name, "the first record of a data entry is not the header of the columns")
	}
	return nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// coherent checks the manifest against the catalog: the data entries are exactly the tables that have data, the history entries exist for ledger tables
// that are updatable, and the groups and the options make sense.
func coherent(m Manifest, cat Catalog, byTable map[string]*CatalogTable) error {
	if m.CatalogVersion != cat.CatalogVersion {
		return invalid("manifest", ManifestName, "the manifest and the catalog have different catalog versions")
	}
	excluded := map[string]bool{}
	for _, t := range m.Options.ExcludedTables {
		if byTable[t] == nil {
			return invalid("options", t, "an excluded table is not in the catalog")
		}
		excluded[t] = true
	}
	want := map[string]bool{}
	for _, t := range cat.Tables {
		if t.hasData(m.Options, excluded) {
			want[t.Qualified()] = true
		}
	}
	have := map[string]bool{}
	history := map[string]bool{}
	for _, e := range m.Entries {
		if _, ok := IsData(e.Name); !ok {
			continue
		}
		if e.LedgerHistory {
			if history[e.Table] || byTable[e.Table] == nil || byTable[e.Table].Ledger == nil {
				return invalid("data", e.Name, "a ledger history entry is duplicated or for a table without a ledger")
			}
			history[e.Table] = true
			continue
		}
		if have[e.Table] || !want[e.Table] {
			return invalid("data", e.Name, "a data entry is for a table twice, for a leaf, for an excluded table, or for a table that is not in the catalog")
		}
		have[e.Table] = true
	}
	for t := range want {
		if !have[t] {
			return invalid("data", t, "a table of the catalog has no data entry")
		}
	}
	if m.Options.DataMode == "none" && len(history) != 0 {
		return invalid("data", ManifestName, "an archive without data has history entries")
	}
	return nil
}

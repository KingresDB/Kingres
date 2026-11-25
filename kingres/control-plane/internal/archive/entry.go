// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package archive

import (
	"bufio"
	"bytes"
	"compress/flate"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"hash/crc32"
	"io"
)

// ErrEntryChanged is the error of an EntryReader whose bytes are not the bytes that Validate saw: another size, another CRC-32 or another SHA-256.
var ErrEntryChanged = errors.New("archive: an entry is not what the validation found")

// EntryReader reads the decompressed bytes of one entry of an archive that Validate accepted. It is how the importer reads the SQL files and the data entries
// after the validation: the entry is checked again while it is read (the size, the CRC-32 and the SHA-256 against what the validation found and what the
// manifest says), and a difference is an error at the end of the stream, so a consumer that stops reading at the first error has never used the tail of a
// changed file without being told. An entry is never written to a file system.
type EntryReader struct {
	rc    io.ReadCloser
	src   io.Reader
	lr    *io.LimitedReader
	sha   hash.Hash
	crc   hash.Hash32
	want  EntryFact
	sum   string
	err   error
	ended bool
}

// OpenEntry opens one entry. f is the fact that Validate found for it (Result.Entries) and wantSHA the SHA-256 of the manifest (hex); the reader refuses to end
// cleanly unless both agree with the bytes it read. The archive file is the same io.ReaderAt that Validate read.
func OpenEntry(ra io.ReaderAt, f EntryFact, wantSHA string) *EntryReader {
	data := f.Offset + int64(30+len(f.Name))
	sr := io.NewSectionReader(ra, data, f.CompressedSize)
	var src io.Reader = bufio.NewReaderSize(sr, 32<<10)
	e := &EntryReader{want: f, sum: wantSHA, sha: sha256.New(), crc: crc32.NewIEEE()}
	if f.Method == methodDeflate {
		fr := flate.NewReader(src.(flate.Reader))
		e.rc = fr
		src = fr
	}
	e.lr = &io.LimitedReader{R: src, N: f.Size + 1}
	e.src = io.TeeReader(e.lr, io.MultiWriter(e.sha, e.crc))
	return e
}

// Read implements io.Reader. At the end of the entry it returns io.EOF only when the entry has exactly the declared size, the declared CRC-32 and the SHA-256
// of the manifest; otherwise ErrEntryChanged.
func (e *EntryReader) Read(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	if e.ended {
		return 0, io.EOF
	}
	n, err := e.src.Read(p)
	if got := e.want.Size + 1 - e.lr.N; got > e.want.Size {
		e.err = ErrEntryChanged
		return 0, e.err
	}
	if err == io.EOF {
		if e.want.Size+1-e.lr.N != e.want.Size || e.crc.Sum32() != e.want.CRC32 || hex.EncodeToString(e.sha.Sum(nil)) != e.sum {
			e.err = ErrEntryChanged
			return n, e.err
		}
		e.ended = true
		return n, io.EOF
	}
	if err != nil {
		e.err = ErrEntryChanged
		return n, e.err
	}
	return n, nil
}

// Close releases the decompressor. It does not read the rest of the entry.
func (e *EntryReader) Close() error {
	if e.rc != nil {
		return e.rc.Close()
	}
	return nil
}

// ReadManifest reads the structure of the archive and its manifest and nothing else: the end record, the central directory, the extents, the names and the
// manifest. It reads no other entry, so it is cheap for an archive of any size. It is what a request needs to know the name of the database in the file;
// it is NOT a validation (Validate is), and what it returns is only used to propose a name and to choose messages.
func ReadManifest(ra io.ReaderAt, size int64, lim Limits) (Manifest, error) {
	if lim.MaxImportBytes <= 0 {
		lim.MaxImportBytes = 100 << 30
	}
	if lim.MaxEntries <= 0 || lim.MaxEntries > MaxEntries {
		lim.MaxEntries = MaxEntries
	}
	c := ctx{ra: ra, size: size}
	ents, cdOff, err := c.directory(lim)
	if err != nil {
		return Manifest{}, err
	}
	if err := c.extents(ents, cdOff); err != nil {
		return Manifest{}, err
	}
	if err := checkNames(ents); err != nil {
		return Manifest{}, err
	}
	last := ents[len(ents)-1]
	if last.usize > MaxManifestBytes {
		return Manifest{}, invalid("manifest_size", ManifestName, "the manifest is larger than %d bytes", MaxManifestBytes)
	}
	var buf bytes.Buffer
	if _, _, err := c.stream(last, func(r io.Reader) error { _, e := io.Copy(&buf, r); return e }); err != nil {
		return Manifest{}, err
	}
	return ParseManifest(buf.Bytes())
}

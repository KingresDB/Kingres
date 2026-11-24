// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package archive

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The corpus of hostile archives (design "The corpus of hostile archives [14b]"). Every case is generated from a good archive; each must be refused with
// archive_invalid or archive_incompatible before any statement or database (there is none in this package), within bounded memory and time.

type located struct {
	local   []int // offset of each local header
	central []int // offset of each central record
	cdOff   int
	eocd    int
}

func locate(t testing.TB, b []byte) located {
	t.Helper()
	var l located
	l.eocd = len(b) - 22
	l.cdOff = int(binary.LittleEndian.Uint32(b[l.eocd+16:]))
	n := int(binary.LittleEndian.Uint16(b[l.eocd+10:]))
	pos := l.cdOff
	for i := 0; i < n; i++ {
		l.central = append(l.central, pos)
		nl := int(binary.LittleEndian.Uint16(b[pos+28:]))
		el := int(binary.LittleEndian.Uint16(b[pos+30:]))
		l.local = append(l.local, int(binary.LittleEndian.Uint32(b[pos+42:])))
		pos += 46 + nl + el
	}
	return l
}

type corpusCase struct {
	name string
	want string // archive_invalid or archive_incompatible
	make func(t testing.TB) []byte
	lim  Limits
}

// renamed makes an archive in which entry i has another name (of the same or any length) in both headers and in the manifest's list.
func renamed(i int, name string) func(t testing.TB) []byte {
	return func(t testing.TB) []byte {
		ents := goodRaw(t, nil)
		ents[i].name = name
		return rawArchive(t, ents, rawOpts{})
	}
}

func corpus() []corpusCase {
	good := func(t testing.TB) []byte { b, _ := buildGood(t, nil); return b }
	patch := func(f func(t testing.TB, b []byte, l located)) func(t testing.TB) []byte {
		return func(t testing.TB) []byte {
			b := good(t)
			f(t, b, locate(t, b))
			return b
		}
	}
	withManifest := func(mod func(*Manifest)) func(t testing.TB) []byte {
		return func(t testing.TB) []byte { return rawArchive(t, goodRaw(t, mod), rawOpts{}) }
	}
	withCatalog := func(mod func(*Catalog)) func(t testing.TB) []byte {
		return func(t testing.TB) []byte {
			parts := goodParts(t)
			c := goodCatalog()
			mod(&c)
			parts[0].data, _ = json.Marshal(c)
			var ents []raw
			for _, p := range parts {
				ents = append(ents, raw{name: p.name, data: p.data})
			}
			ents = append(ents, raw{name: ManifestName, data: manifestJSON(t, parts, nil)})
			return rawArchive(t, ents, rawOpts{})
		}
	}
	u := func(v uint64) *uint64 { return &v }
	inv, inc := CodeInvalid, CodeIncompatible
	cs := []corpusCase{
		{"path with ..", inv, renamed(4, "../../etc/pw.csv"), Limits{}},
		{"path with .. in the middle", inv, renamed(4, "data/../x.csv"), Limits{}},
		{"absolute path", inv, renamed(4, "/etc/passwd"), Limits{}},
		{"drive letter", inv, renamed(4, "C:/data.csv"), Limits{}},
		{"backslash", inv, renamed(4, `data\00001.csv`), Limits{}},
		{"NUL in a name", inv, renamed(4, "data/0000\x00.csv"), Limits{}},
		{"unknown name", inv, renamed(4, "data/extra.txt"), Limits{}},
		{"name longer than 64 bytes", inv, renamed(4, "data/"+strings.Repeat("a", 70)+".csv"), Limits{}},
		{"directory entry", inv, renamed(4, "data/"), Limits{}},
		{"duplicate name", inv, func(t testing.TB) []byte {
			ents := goodRaw(t, nil)
			ents[5].name = ents[4].name
			return rawArchive(t, ents, rawOpts{})
		}, Limits{}},
		{"data numbering with a gap", inv, renamed(5, DataName(7)), Limits{}},
		{"missing manifest", inv, func(t testing.TB) []byte {
			ents := goodRaw(t, nil)
			return rawArchive(t, ents[:len(ents)-1], rawOpts{})
		}, Limits{}},
		{"manifest not last", inv, func(t testing.TB) []byte {
			ents := goodRaw(t, nil)
			ents[0], ents[len(ents)-1] = ents[len(ents)-1], ents[0]
			return rawArchive(t, ents, rawOpts{})
		}, Limits{}},
		{"symbolic link", inv, patch(func(t testing.TB, b []byte, l located) {
			binary.LittleEndian.PutUint32(b[l.central[4]+38:], 0o120777<<16)
		}), Limits{}},
		{"oversized manifest", inv, func(t testing.TB) []byte {
			ents := goodRaw(t, nil)
			m := ents[len(ents)-1].data
			ents[len(ents)-1].data = append(append([]byte{}, m...), bytes.Repeat([]byte(" "), MaxManifestBytes)...)
			return rawArchive(t, ents, rawOpts{})
		}, Limits{}},
		{"oversized catalog", inv, func(t testing.TB) []byte {
			parts := goodParts(t)
			c := append(bytes.TrimRight(parts[0].data, "}"), '}')
			parts[0].data = append(c, bytes.Repeat([]byte(" "), MaxCatalogBytes)...)
			var ents []raw
			for _, p := range parts {
				ents = append(ents, raw{name: p.name, data: p.data})
			}
			ents = append(ents, raw{name: ManifestName, data: manifestJSON(t, parts, nil)})
			return rawArchive(t, ents, rawOpts{})
		}, Limits{}},
		{"json nested 10000 deep", inv, func(t testing.TB) []byte {
			ents := goodRaw(t, nil)
			ents[len(ents)-1].data = []byte(strings.Repeat("[", 10000) + strings.Repeat("]", 10000))
			return rawArchive(t, ents, rawOpts{})
		}, Limits{}},
		{"json string of 5000 bytes", inv, withManifest(func(m *Manifest) { m.Warnings = []string{strings.Repeat("w", 5000)} }), Limits{}},
		{"unknown member in the manifest", inv, func(t testing.TB) []byte {
			ents := goodRaw(t, nil)
			var m map[string]any
			json.Unmarshal(ents[len(ents)-1].data, &m)
			m["surprise"] = 1
			ents[len(ents)-1].data, _ = json.Marshal(m)
			return rawArchive(t, ents, rawOpts{})
		}, Limits{}},
		{"unknown member in the catalog", inv, func(t testing.TB) []byte {
			parts := goodParts(t)
			var m map[string]any
			json.Unmarshal(parts[0].data, &m)
			m["surprise"] = 1
			parts[0].data, _ = json.Marshal(m)
			var ents []raw
			for _, p := range parts {
				ents = append(ents, raw{name: p.name, data: p.data})
			}
			ents = append(ents, raw{name: ManifestName, data: manifestJSON(t, parts, nil)})
			return rawArchive(t, ents, rawOpts{})
		}, Limits{}},
		{"header declares 1 KiB over a stream that is longer", inv, func(t testing.TB) []byte {
			ents := goodRaw(t, nil)
			ents[3].data = bytes.Repeat([]byte("-- x\n"), 400000)
			parts := goodParts(t)
			parts[3].data = ents[3].data
			ents[3].lieUsize = u(1024)
			ents[len(ents)-1].data = manifestJSON(t, parts, func(m *Manifest) {
				m.Entries[3].Size = 1024
				m.Totals.UncompressedBytes = m.Totals.UncompressedBytes - int64(len(ents[3].data)) + 1024
			})
			return rawArchive(t, ents, rawOpts{})
		}, Limits{}},
		{"header declares more than the stream holds", inv, func(t testing.TB) []byte {
			ents := goodRaw(t, nil)
			parts := goodParts(t)
			ents[3].lieUsize = u(uint64(len(ents[3].data)) + 4096)
			ents[len(ents)-1].data = manifestJSON(t, parts, func(m *Manifest) {
				m.Entries[3].Size += 4096
				m.Totals.UncompressedBytes += 4096
			})
			return rawArchive(t, ents, rawOpts{})
		}, Limits{}},
		{"deflate bomb above the import limit", inv, func(t testing.TB) []byte {
			parts := goodParts(t)
			parts[4].data = append([]byte("id,customer,total\n"), bytes.Repeat([]byte("0"), 40<<20)...)
			var ents []raw
			for _, p := range parts {
				ents = append(ents, raw{name: p.name, data: p.data})
			}
			ents = append(ents, raw{name: ManifestName, data: manifestJSON(t, parts, nil)})
			return rawArchive(t, ents, rawOpts{})
		}, Limits{MaxImportBytes: 1 << 20}},
		{"a data entry that is an archive", inv, func(t testing.TB) []byte {
			inner, _ := buildGood(t, nil)
			parts := goodParts(t)
			parts[4].data = inner
			var ents []raw
			for _, p := range parts {
				ents = append(ents, raw{name: p.name, data: p.data})
			}
			ents = append(ents, raw{name: ManifestName, data: manifestJSON(t, parts, nil)})
			return rawArchive(t, ents, rawOpts{})
		}, Limits{}},
		{"overlapping entries", inv, patch(func(t testing.TB, b []byte, l located) {
			copy(b[l.central[5]+42:], b[l.central[4]+42:l.central[4]+46])
		}), Limits{}},
		{"hidden bytes before the first entry", inv, func(t testing.TB) []byte {
			return rawArchive(t, goodRaw(t, nil), rawOpts{prefix: []byte("MZ-hidden-polyglot")})
		}, Limits{}},
		{"hidden bytes between entries", inv, func(t testing.TB) []byte {
			ents := goodRaw(t, nil)
			ents[3].gapAfter = []byte("hidden")
			return rawArchive(t, ents, rawOpts{})
		}, Limits{}},
		{"hidden bytes between the last entry and the central directory", inv, func(t testing.TB) []byte {
			ents := goodRaw(t, nil)
			ents[len(ents)-1].gapAfter = []byte("hidden")
			return rawArchive(t, ents, rawOpts{})
		}, Limits{}},
		{"junk inside the compressed extent after the deflate stream", inv, func(t testing.TB) []byte {
			ents := goodRaw(t, nil)
			ents[4].junk = []byte("junkjunk")
			return rawArchive(t, ents, rawOpts{})
		}, Limits{}},
		{"end record claims a comment that the file lacks", inv, patch(func(t testing.TB, b []byte, l located) { binary.LittleEndian.PutUint16(b[len(b)-2:], 9) }), Limits{}},
		{"hidden bytes after the central directory", inv, func(t testing.TB) []byte {
			return rawArchive(t, goodRaw(t, nil), rawOpts{beforeEnd: []byte("hidden")})
		}, Limits{}},
		{"hidden bytes after the end record", inv, func(t testing.TB) []byte { return append(good(t), "tail"...) }, Limits{}},
		{"comment in the end record", inv, func(t testing.TB) []byte {
			b := good(t)
			binary.LittleEndian.PutUint16(b[len(b)-2:], 6)
			return append(b, "hidden"...)
		}, Limits{}},
		{"second end record inside a comment", inv, func(t testing.TB) []byte {
			b := good(t)
			fake := append([]byte{0x50, 0x4b, 5, 6}, make([]byte, 18)...)
			binary.LittleEndian.PutUint16(b[len(b)-2:], uint16(len(fake)))
			return append(b, fake...)
		}, Limits{}},
		{"local name differs from the central name", inv, patch(func(t testing.TB, b []byte, l located) { b[l.local[4]+30+6]++ }), Limits{}},
		{"local extra field hides bytes", inv, patch(func(t testing.TB, b []byte, l located) { binary.LittleEndian.PutUint16(b[l.local[4]+28:], 8) }), Limits{}},
		{"local flags differ", inv, patch(func(t testing.TB, b []byte, l located) { binary.LittleEndian.PutUint16(b[l.local[4]+6:], 0) }), Limits{}},
		{"descriptor that the flag does not announce", inv, patch(func(t testing.TB, b []byte, l located) {
			binary.LittleEndian.PutUint16(b[l.local[4]+6:], 0)
			binary.LittleEndian.PutUint16(b[l.central[4]+8:], 0)
		}), Limits{}},
		{"descriptor that the flag announces and the data lacks", inv, patch(func(t testing.TB, b []byte, l located) {
			// the descriptor of entry 4 starts where entry 5 starts, minus 16
			binary.LittleEndian.PutUint32(b[l.local[5]-16:], 0xDEADBEEF)
		}), Limits{}},
		{"descriptor with other sizes", inv, patch(func(t testing.TB, b []byte, l located) { b[l.local[5]-4]++ }), Limits{}},
		{"local header with sizes", inv, patch(func(t testing.TB, b []byte, l located) { b[l.local[4]+18] = 1 }), Limits{}},
		{"method that is not Store or Deflate", inv, patch(func(t testing.TB, b []byte, l located) {
			binary.LittleEndian.PutUint16(b[l.local[4]+8:], 12)
			binary.LittleEndian.PutUint16(b[l.central[4]+10:], 12)
		}), Limits{}},
		{"central extra field", inv, patch(func(t testing.TB, b []byte, l located) { binary.LittleEndian.PutUint16(b[l.central[4]+30:], 4) }), Limits{}},
		{"multi-disk archive", inv, patch(func(t testing.TB, b []byte, l located) { binary.LittleEndian.PutUint16(b[l.eocd+4:], 1) }), Limits{}},
		{"100000 entries declared", inv, patch(func(t testing.TB, b []byte, l located) {
			binary.LittleEndian.PutUint16(b[l.eocd+8:], 0xFFFE)
			binary.LittleEndian.PutUint16(b[l.eocd+10:], 0xFFFE)
		}), Limits{}},
		{"entry limit lowered", inv, func(t testing.TB) []byte { return good(t) }, Limits{MaxEntries: 5}},
		{"Zip64 sizes that lie", inv, func(t testing.TB) []byte {
			return rawArchive(t, goodRaw(t, nil), rawOpts{force64: true, patchCD: func(cd []byte) {
				// the first central record: name length 12, extra: id, size, usize, csize
				nl := int(binary.LittleEndian.Uint16(cd[28:]))
				binary.LittleEndian.PutUint64(cd[46+nl+4:], 1<<40)
			}})
		}, Limits{}},
		{"wrong CRC in both places", inv, patch(func(t testing.TB, b []byte, l located) {
			b[l.central[4]+16] ^= 0xFF
			b[l.local[5]-16+4] ^= 0xFF
		}), Limits{}},
		{"wrong SHA-256", inv, withManifest(func(m *Manifest) { m.Entries[4].SHA256 = strings.Repeat("0", 64) }), Limits{}},
		{"wrong size in the manifest", inv, withManifest(func(m *Manifest) { m.Entries[4].Size++; m.Totals.UncompressedBytes++ }), Limits{}},
		{"totals that do not add up", inv, withManifest(func(m *Manifest) { m.Totals.Rows++ }), Limits{}},
		{"manifest disagrees with the catalog on a table", inv, withManifest(func(m *Manifest) { m.Entries[4].Table = "public.nothing" }), Limits{}},
		{"data entry whose columns differ from the schema", inv, withManifest(func(m *Manifest) { m.Entries[4].Columns = []string{"id", "total", "customer"} }), Limits{}},
		{"data entry whose header differs from its columns", inv, func(t testing.TB) []byte {
			parts := goodParts(t)
			parts[4].data = csvOf([]string{"id", "total", "customer"}, `"1","a","b"`)
			var ents []raw
			for _, p := range parts {
				ents = append(ents, raw{name: p.name, data: p.data})
			}
			ents = append(ents, raw{name: ManifestName, data: manifestJSON(t, parts, nil)})
			return rawArchive(t, ents, rawOpts{})
		}, Limits{}},
		{"a table of the catalog without data", inv, func(t testing.TB) []byte {
			parts := goodParts(t)
			parts = parts[:len(parts)-1]
			var ents []raw
			for _, p := range parts {
				ents = append(ents, raw{name: p.name, data: p.data})
			}
			ents = append(ents, raw{name: ManifestName, data: manifestJSON(t, parts, nil)})
			return rawArchive(t, ents, rawOpts{})
		}, Limits{}},
		{"a leaf with data", inv, withManifest(func(m *Manifest) {
			m.Entries[8].Table = "public.events_2026"
		}), Limits{}},
		{"an extension that is not listed", inv, withManifest(func(m *Manifest) {
			m.Extensions = append(m.Extensions, ManifestExtension{Name: "dblink", Version: "1.2", Schema: "public"})
		}), Limits{}},
		{"an extension twice", inv, withManifest(func(m *Manifest) { m.Extensions = append(m.Extensions, m.Extensions[0]) }), Limits{}},
		{"an identifier with a newline", inv, withCatalog(func(c *Catalog) { c.Tables[1].Columns[1] = "na\nme" }), Limits{}},
		{"an identifier longer than 63 bytes", inv, withCatalog(func(c *Catalog) { c.Tables[1].Name = strings.Repeat("n", 64) }), Limits{}},
		{"a table in schema kingres", inv, withCatalog(func(c *Catalog) { c.Tables[1].Schema = "kingres" }), Limits{}},
		{"a bucket count of 0", inv, withCatalog(func(c *Catalog) { c.ColocationGroups[0].BucketCount = 0 }), Limits{}},
		{"a bucket count of 4097", inv, withCatalog(func(c *Catalog) { c.ColocationGroups[0].BucketCount = 4097 }), Limits{}},
		{"a hash table in an unknown colocation group", inv, withCatalog(func(c *Catalog) { c.Tables[0].ColocationRef = "nope" }), Limits{}},
		{"a gigantic row count", inv, withManifest(func(m *Manifest) { m.Entries[4].Rows = 1 << 62 }), Limits{}},
		{"a bad date", inv, withManifest(func(m *Manifest) { m.CreatedAt = "yesterday" }), Limits{}},
		{"a bad fingerprint", inv, withManifest(func(m *Manifest) { m.Fingerprint = "x" }), Limits{}},
		{"format kingres-export/2", inc, withManifest(func(m *Manifest) { m.Format = "kingres-export/2" }), Limits{}},
		{"format of another family", inv, withManifest(func(m *Manifest) { m.Format = "tar/1" }), Limits{}},
		{"an unknown catalog version", inc, withManifest(func(m *Manifest) { m.CatalogVersion = 2 }), Limits{}},
		{"a required feature that is not known", inc, withManifest(func(m *Manifest) { m.Requires = []string{"timetravel"} }), Limits{}},
		{"not a zip file", inv, func(t testing.TB) []byte { return bytes.Repeat([]byte("x"), 5000) }, Limits{}},
		{"empty file", inv, func(t testing.TB) []byte { return nil }, Limits{}},
		{"truncated file", inv, func(t testing.TB) []byte { b := good(t); return b[:len(b)/2] }, Limits{}},
	}
	return cs
}

func TestHostileCorpus(t *testing.T) {
	for _, c := range corpus() {
		t.Run(c.name, func(t *testing.T) {
			b := c.make(t)
			lim := c.lim
			if lim.SQL == nil {
				lim.SQL = sqlCount
			}
			var m0, m1 runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&m0)
			start := time.Now()
			res, err := validate(b, lim)
			el := time.Since(start)
			runtime.ReadMemStats(&m1)
			if err == nil {
				t.Fatalf("accepted (%d entries)", len(res.Entries))
			}
			if got := code(err); got != c.want {
				t.Fatalf("code %s, want %s: %v", got, c.want, err)
			}
			if e := err.(*Error); len(e.Reasons) == 0 || e.Reasons[0].Message == "" {
				t.Fatal("no reason")
			}
			if alloc := m1.TotalAlloc - m0.TotalAlloc; alloc > 160<<20 {
				t.Errorf("allocated %d MiB", alloc>>20)
			}
			if el > 10*time.Second {
				t.Errorf("took %s", el)
			}
		})
	}
}

// The corpus has the cases the design lists; this keeps the count honest.
func TestCorpusSize(t *testing.T) {
	if n := len(corpus()); n < 30 {
		t.Fatalf("%d cases", n)
	}
}

// A deflate bomb is stopped at the declared size plus one byte: the checker of the SQL file never sees more.
func TestDeflateBombStoppedAtDeclaredSize(t *testing.T) {
	parts := goodParts(t)
	var ents []raw
	for _, p := range parts {
		ents = append(ents, raw{name: p.name, data: p.data})
	}
	ents[1].data = bytes.Repeat([]byte("x"), 20<<20)
	d := uint64(10)
	ents[1].lieUsize = &d
	parts[1].data = []byte("0123456789")
	ents = append(ents, raw{name: ManifestName, data: manifestJSON(t, parts, nil)})
	b := rawArchive(t, ents, rawOpts{})
	var seen int64
	_, err := validate(b, Limits{SQL: func(name string, r io.Reader) (SQLResult, error) {
		n, _ := io.Copy(io.Discard, r)
		if name == PreDataName {
			seen = n
		}
		return SQLResult{}, nil
	}})
	if code(err) != CodeInvalid {
		t.Fatalf("%v", err)
	}
	if seen != 11 {
		t.Fatalf("the checker saw %d bytes, want declared+1 = 11", seen)
	}
}

// The hostile corpus is also refused when the archive is Zip64.
func TestZip64CorpusSample(t *testing.T) {
	ents := goodRaw(t, func(m *Manifest) { m.Entries[4].SHA256 = strings.Repeat("1", 64) })
	b := rawArchive(t, ents, rawOpts{force64: true})
	if _, err := validate(b, Limits{}); code(err) != CodeInvalid {
		t.Fatal(err)
	}
	// and its good form is accepted
	b = rawArchive(t, goodRaw(t, nil), rawOpts{force64: true})
	if _, err := validate(b, Limits{}); err != nil {
		t.Fatal(err)
	}
}

// The extent check on its own: a central record that says an entry starts anywhere but where the one before it ended is refused by the check of the
// extents itself, not only by what is read afterwards.
func TestExtentsRefuseAMovedOffset(t *testing.T) {
	b, _ := buildGood(t, nil)
	c := ctx{ra: bytes.NewReader(b), size: int64(len(b))}
	ents, cdOff, err := c.directory(Limits{MaxEntries: MaxEntries})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.extents(ents, cdOff); err != nil {
		t.Fatal(err)
	}
	for _, d := range []int64{-1, 1, 30} {
		moved := append([]central(nil), ents...)
		moved[5].offset = uint64(int64(moved[5].offset) + d)
		if err := c.extents(moved, cdOff); err == nil {
			t.Errorf("an offset moved by %d was accepted", d)
		}
	}
}

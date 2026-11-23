// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package archive

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// opaque hides the Seek and ReadAt methods of a buffer: the writer must work on a destination that is not seekable.
type opaque struct{ w io.Writer }

func (o opaque) Write(p []byte) (int, error) { return o.w.Write(p) }

func goodCatalog() Catalog {
	return Catalog{
		CatalogVersion:   1,
		ColocationGroups: []ColocationGroup{{Ref: "g1", BucketCount: 32, DistColumnType: "bigint", Collation: ""}},
		Tables: []CatalogTable{
			{Schema: "public", Name: "orders", Kind: "hash", TreeRole: "standalone", Columns: []string{"id", "customer", "total"}, DistributionColumn: "id", ColocationRef: "g1", AccessMethod: "heap", EstimatedRows: 3, EstimatedBytes: 100},
			{Schema: "public", Name: "customers", Kind: "reference", TreeRole: "standalone", Columns: []string{"id", "name"}, AccessMethod: "heap"},
			{Schema: "public", Name: "audit", Kind: "local", TreeRole: "standalone", Columns: []string{"id", "note"}, AccessMethod: "heap",
				Ledger: &LedgerInfo{Mode: "updatable", Policy: "optional", HeadHeight: 4, HeadBlockHash: "abcd", HistoryColumns: []string{"id", "note", "ledger_txid"}}},
			{Schema: "public", Name: "events", Kind: "local", TreeRole: "root", Columns: []string{"id", "at"}, AccessMethod: "columnar", Columnar: map[string]string{"compression": "zstd"}},
			{Schema: "public", Name: "events_2026", Kind: "local", TreeRole: "leaf", Parent: "public.events", Columns: []string{"id", "at"}, AccessMethod: "heap"},
		},
		TimepartPolicies: []TimepartPolicy{{Schema: "public", Table: "events", PartitionEvery: "1 month", Timezone: "UTC", CreateAhead: "3", Retention: "1 year", RetentionAction: "drop", RunEvery: "1 hour", Enabled: true}},
		Sequences:        []CatalogSequence{{Schema: "public", Name: "orders_id_seq", OwnedBy: "public.orders.id", LastValue: 3, IsCalled: true}},
	}
}

// goodParts are the contents of a good archive, in the order of the entries.
type part struct {
	name string
	data []byte
	meta ManifestEntry
}

func csvOf(cols []string, rows ...string) []byte {
	var b strings.Builder
	b.WriteString(strings.Join(cols, ",") + "\n")
	for _, r := range rows {
		b.WriteString(r + "\n")
	}
	return []byte(b.String())
}

func goodParts(t testing.TB) []part {
	cat, err := json.Marshal(goodCatalog())
	if err != nil {
		t.Fatal(err)
	}
	return []part{
		{name: CatalogName, data: cat},
		{name: PreDataName, data: []byte("CREATE TABLE public.orders (id bigint, customer text, total numeric);\nCREATE TABLE public.customers (id int, name text);\n")},
		{name: ViewsName, data: []byte("")},
		{name: PostDataName, data: []byte("ALTER TABLE public.orders ADD CONSTRAINT o_pk PRIMARY KEY (id);\n")},
		{name: DataName(1), data: csvOf([]string{"id", "customer", "total"}, `"1","ada","12.50"`, `"2",\N,"0"`), meta: ManifestEntry{Table: "public.orders", Columns: []string{"id", "customer", "total"}, Rows: 2, Consistency: "per-group", Groups: 3}},
		{name: DataName(2), data: csvOf([]string{"id", "name"}, `"1","x"`), meta: ManifestEntry{Table: "public.customers", Columns: []string{"id", "name"}, Rows: 1, Consistency: "single-snapshot", Groups: 1}},
		{name: DataName(3), data: csvOf([]string{"id", "note"}, `"1","a"`), meta: ManifestEntry{Table: "public.audit", Columns: []string{"id", "note"}, Rows: 1, Consistency: "single-snapshot", Groups: 1}},
		{name: DataName(4), data: csvOf([]string{"id", "note", "ledger_txid"}, `"1","a","9"`), meta: ManifestEntry{Table: "public.audit", Columns: []string{"id", "note", "ledger_txid"}, Rows: 1, Consistency: "single-snapshot", Groups: 1, LedgerHistory: true}},
		{name: DataName(5), data: csvOf([]string{"id", "at"}), meta: ManifestEntry{Table: "public.events", Columns: []string{"id", "at"}, Consistency: "single-snapshot", Groups: 1}},
	}
}

func goodManifest() Manifest {
	return Manifest{
		CreatedAt:   "2026-11-23T09:30:00Z",
		Source:      Source{Database: "shop", KingresVersion: "0.1.0", ContractVersion: "0.40.0", PostgresMajor: 18, GroupCount: 3, Encoding: "UTF8", LcCollate: "C", LcCtype: "C", LocaleProvider: "libc"},
		Extensions:  []ManifestExtension{{Name: "kingres_shard", Version: "0.7", Schema: "kingres"}, {Name: "citext", Version: "1.6", Schema: "public"}},
		Fingerprint: strings.Repeat("a", 64),
		Options:     Options{DataMode: "all"},
	}
}

// buildGood writes a good archive with the Writer on a destination that is not seekable.
func buildGood(t testing.TB, mod func(*Writer)) ([]byte, Manifest) {
	t.Helper()
	var buf bytes.Buffer
	w := NewWriter(opaque{&buf})
	if mod != nil {
		mod(w)
	}
	meta := map[string]ManifestEntry{}
	for _, p := range goodParts(t) {
		ew, err := w.Create(p.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ew.Write(p.data); err != nil {
			t.Fatal(err)
		}
		if p.meta.Table != "" {
			meta[p.name] = p.meta
		}
	}
	m, err := w.Finish(goodManifest(), meta)
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), m
}

func sqlCount(name string, r io.Reader) (SQLResult, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return SQLResult{}, err
	}
	n := strings.Count(string(b), ";")
	return SQLResult{ByKind: map[string]int{"statement": n}}, nil
}

func validate(b []byte, lim Limits) (*Result, error) {
	return Validate(bytes.NewReader(b), int64(len(b)), lim)
}

func code(err error) string {
	if e, ok := err.(*Error); ok {
		return e.Code
	}
	if err == nil {
		return ""
	}
	return "other: " + err.Error()
}

// ---- a raw writer for hostile archives: it uses the writer's own records but takes any names, contents, sizes and gaps.

type raw struct {
	name     string
	data     []byte
	lieUsize *uint64 // what the descriptor and the central record say, when it is not the truth
	junk     []byte  // bytes inside the compressed extent after the end of the deflate stream
	gapAfter []byte  // bytes between this entry and the next that no record accounts for
}

type rawOpts struct {
	force64   bool
	prefix    []byte
	beforeEnd []byte // bytes between the central directory and the end record
	patchCD   func(cd []byte)
}

func sha(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// manifestJSON builds a manifest for the parts as the writer would, then lets the test change it.
func manifestJSON(t testing.TB, parts []part, mod func(*Manifest)) []byte {
	m := goodManifest()
	m.Format, m.CatalogVersion = Format, CatalogVersion
	m.Warnings, m.Entries = []string{}, nil
	for _, p := range parts {
		e := p.meta
		e.Name, e.Size, e.SHA256 = p.name, int64(len(p.data)), sha(p.data)
		if _, ok := IsData(p.name); ok && !e.LedgerHistory {
			m.Totals.Tables++
			m.Totals.Rows += e.Rows
		}
		m.Totals.UncompressedBytes += e.Size
		m.Entries = append(m.Entries, e)
	}
	if mod != nil {
		mod(&m)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func rawArchive(t testing.TB, ents []raw, o rawOpts) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.Write(o.prefix)
	w := NewWriter(&buf)
	w.n = int64(len(o.prefix))
	w.force64 = o.force64
	for _, r := range ents {
		e, err := w.start(r.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.Write(r.data); err != nil {
			t.Fatal(err)
		}
		e.junk = r.junk
		if r.lieUsize != nil {
			e.usizeLie = r.lieUsize
		}
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
		if len(r.gapAfter) > 0 {
			buf.Write(r.gapAfter)
			w.n += int64(len(r.gapAfter))
		}
	}
	if len(o.beforeEnd) > 0 || o.patchCD != nil {
		// write the central directory by hand so that the gap and the patch can be placed
		var cdBuf bytes.Buffer
		w2 := &Writer{w: &cdBuf, n: w.n, entries: w.entries, force64: w.force64}
		if err := w2.central(); err != nil {
			t.Fatal(err)
		}
		all := cdBuf.Bytes()
		cdLen := len(all) - 22
		if w.force64 {
			cdLen -= 76
		}
		cd := all[:cdLen]
		if o.patchCD != nil {
			o.patchCD(cd)
		}
		buf.Write(cd)
		buf.Write(o.beforeEnd)
		buf.Write(all[cdLen:])
		return buf.Bytes()
	}
	if err := w.central(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// goodRaw is the good archive as raw entries with a manifest that the test can change.
func goodRaw(t testing.TB, mod func(*Manifest)) []raw {
	parts := goodParts(t)
	var out []raw
	for _, p := range parts {
		out = append(out, raw{name: p.name, data: p.data})
	}
	out = append(out, raw{name: ManifestName, data: manifestJSON(t, parts, mod)})
	return out
}

func le16p(b []byte, v uint16) { binary.LittleEndian.PutUint16(b, v) }
func le32p(b []byte, v uint32) { binary.LittleEndian.PutUint32(b, v) }

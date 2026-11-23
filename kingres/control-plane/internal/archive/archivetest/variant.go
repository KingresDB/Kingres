// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

// Package archivetest makes archives that are not good, for the tests of the importer: it reads a good archive, lets the test change any part of it (the catalog,
// the SQL files, the data, the manifest, even to make it lie) and writes it again in the exact shape of the writer. It is test support; nothing in the server
// imports it.
package archivetest

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/archive"
)

// Variant is the content of an archive that a test may change before it is written again.
type Variant struct {
	Cat   archive.Catalog
	Man   archive.Manifest
	Files map[string]string // every entry but the manifest and the catalog, by name
	Names []string          // the entries, in order, without the manifest
	// Patch changes the manifest as it is about to be written (after the sizes and sums were recomputed), so that it can lie.
	Patch func(m *archive.Manifest)
}

// Read reads a good archive.
func Read(t testing.TB, b []byte) *Variant {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	v := &Variant{Files: map[string]string{}}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(rc)
		_ = rc.Close()
		switch f.Name {
		case archive.ManifestName:
			if err := json.Unmarshal(data, &v.Man); err != nil {
				t.Fatal(err)
			}
		case archive.CatalogName:
			if err := json.Unmarshal(data, &v.Cat); err != nil {
				t.Fatal(err)
			}
			v.Names = append(v.Names, f.Name)
		default:
			v.Files[f.Name] = string(data)
			v.Names = append(v.Names, f.Name)
		}
	}
	return v
}

// Bytes writes the archive again: the catalog as JSON, the sizes and sums of the manifest recomputed, then Patch.
func (v *Variant) Bytes(t testing.TB) []byte {
	t.Helper()
	var parts []archive.Part
	m := v.Man
	m.Entries = nil
	old := map[string]archive.ManifestEntry{}
	for _, e := range v.Man.Entries {
		old[e.Name] = e
	}
	m.Totals = archive.Totals{}
	for _, n := range v.Names {
		var data []byte
		if n == archive.CatalogName {
			var err error
			if data, err = json.Marshal(v.Cat); err != nil {
				t.Fatal(err)
			}
		} else {
			data = []byte(v.Files[n])
		}
		parts = append(parts, archive.Part{Name: n, Data: data})
		sum := sha256.Sum256(data)
		e := old[n]
		e.Name, e.Size, e.SHA256 = n, int64(len(data)), hex.EncodeToString(sum[:])
		if _, ok := archive.IsData(n); ok && !e.LedgerHistory {
			m.Totals.Tables++
			m.Totals.Rows += e.Rows
		}
		m.Totals.UncompressedBytes += e.Size
		m.Entries = append(m.Entries, e)
	}
	if v.Patch != nil {
		v.Patch(&m)
	}
	mj, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := archive.Assemble(&buf, parts, mj); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// AddTable lists a table in the catalog and in the options as a table without data (so that no data entry is needed).
func (v *Variant) AddTable(schema, name string, cols ...string) {
	v.Cat.Tables = append(v.Cat.Tables, archive.CatalogTable{Schema: schema, Name: name, Kind: "local", TreeRole: "standalone", Columns: cols, AccessMethod: "heap"})
	v.Man.Options.ExcludedTables = append(v.Man.Options.ExcludedTables, schema+"."+name)
}

// LieAboutRows makes the manifest say that a table has n rows (the totals follow, so that the only lie is the one about the table).
func (v *Variant) LieAboutRows(table string, n int64) {
	v.Patch = func(m *archive.Manifest) {
		for i := range m.Entries {
			if m.Entries[i].Table == table && !m.Entries[i].LedgerHistory {
				m.Totals.Rows += n - m.Entries[i].Rows
				m.Entries[i].Rows = n
			}
		}
	}
}

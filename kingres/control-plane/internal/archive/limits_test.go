// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package archive

import (
	"bytes"
	"encoding/json"
	"testing"
)

// An entry that declares to expand more than the cap is refused before it is read (a ratio is a limit of its own, besides the sum).
func TestRatioCap(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(opaque{&buf})
	meta := map[string]ManifestEntry{}
	for _, p := range goodParts(t) {
		data := p.data
		if p.name == DataName(5) {
			data = append(append([]byte{}, data...), bytes.Repeat([]byte(`"1","a"`+"\n"), 700000)...)
		}
		ew, err := w.Create(p.name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = ew.Write(data)
		if p.meta.Table != "" {
			meta[p.name] = p.meta
		}
	}
	if _, err := w.Finish(goodManifest(), meta); err != nil {
		t.Fatal(err)
	}
	if _, err := validate(buf.Bytes(), Limits{MaxRatio: 100}); err == nil || code(err) != CodeInvalid {
		t.Errorf("an entry of a ratio above the cap was accepted: %v", err)
	}
	if _, err := validate(buf.Bytes(), Limits{}); err != nil {
		t.Errorf("without the cap: %v", err)
	}
	// The sum is bounded by the limit that the caller derives from the size of the file.
	if _, err := validate(buf.Bytes(), Limits{MaxImportBytes: int64(buf.Len()) * 2}); err == nil {
		t.Error("a sum above the limit was accepted")
	}
}

// A dot in the name of a schema or a table is refused: "a.b" and "c" would be the same text as "a" and "b.c".
func TestDottedNamesAreRefused(t *testing.T) {
	c := goodCatalog()
	c.Tables[0].Schema = "a.b"
	b, _ := json.Marshal(c)
	if _, err := ParseCatalog(b); err == nil {
		t.Error("a schema with a dot was accepted")
	}
	c = goodCatalog()
	c.Tables[0].Name = "o.rders"
	b, _ = json.Marshal(c)
	if _, err := ParseCatalog(b); err == nil {
		t.Error("a table with a dot was accepted")
	}
}

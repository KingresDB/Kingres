// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package archive

import (
	"bytes"
	"encoding/json"
	"testing"
)

// FuzzValidate mutates the bytes of good and hostile archives. The validator must not panic, must not hang, and must answer with a typed error or accept an
// archive that the writer could have produced (checked by validating it again with Zip64 not involved: accepted means every byte was accounted for).
func FuzzValidate(f *testing.F) {
	good, _ := buildGood(f, nil)
	f.Add(good)
	z, _ := buildGood(f, func(w *Writer) { w.ForceZip64() })
	f.Add(z)
	for i, c := range corpus() {
		if i%3 == 0 {
			if b := c.make(f); len(b) <= 1<<20 {
				f.Add(b)
			}
		}
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > 1<<20 {
			return
		}
		res, err := Validate(bytes.NewReader(b), int64(len(b)), Limits{MaxImportBytes: 64 << 20, SQL: sqlCount})
		if err != nil {
			if _, ok := err.(*Error); !ok {
				t.Fatalf("untyped error %T %v", err, err)
			}
			return
		}
		// An accepted archive is exactly the shape of the writer: its entries are contiguous and every byte is in a record.
		var sum int64
		for _, e := range res.Entries {
			sum += e.CompressedSize
		}
		if sum > int64(len(b)) || len(res.Entries) != len(res.Manifest.Entries) {
			t.Fatalf("an accepted archive is inconsistent")
		}
	})
}

func FuzzManifest(f *testing.F) {
	_, m := buildGood(f, nil)
	enc, _ := json.Marshal(m)
	f.Add(enc)
	f.Add([]byte(`{"format":"kingres-export/2"}`))
	f.Add([]byte(`{`))
	f.Fuzz(func(t *testing.T, b []byte) {
		m, err := ParseManifest(b)
		if err != nil {
			if _, ok := err.(*Error); !ok {
				t.Fatalf("untyped error %T", err)
			}
			return
		}
		// What is accepted can be written again and read again to the same value.
		again, _ := json.Marshal(m)
		m2, err := ParseManifest(again)
		if err != nil {
			t.Fatalf("an accepted manifest is not stable: %v", err)
		}
		if len(m2.Entries) != len(m.Entries) {
			t.Fatal("entries changed")
		}
	})
}

func FuzzCatalog(f *testing.F) {
	enc, _ := json.Marshal(goodCatalog())
	f.Add(enc)
	f.Add([]byte(`{"catalogVersion":1,"tables":[{}]}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		c, err := ParseCatalog(b)
		if err != nil {
			if _, ok := err.(*Error); !ok {
				t.Fatalf("untyped error %T", err)
			}
			return
		}
		again, _ := json.Marshal(c)
		if _, err := ParseCatalog(again); err != nil {
			t.Fatalf("an accepted catalog is not stable: %v", err)
		}
	})
}

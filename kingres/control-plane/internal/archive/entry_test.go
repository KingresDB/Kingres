// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package archive

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"testing"
)

func validated(t *testing.T) ([]byte, *Result) {
	t.Helper()
	b, _ := buildGood(t, nil)
	res, err := validate(b, Limits{MaxImportBytes: 1 << 30, SQL: sqlCount})
	if err != nil {
		t.Fatal(err)
	}
	return b, res
}

func sumOf(res *Result, name string) string {
	for _, e := range res.Manifest.Entries {
		if e.Name == name {
			return e.SHA256
		}
	}
	return ""
}

// The entries that Validate found are read back byte for byte, and the reader checks them again as it reads.
func TestEntryReaderReadsEveryEntry(t *testing.T) {
	b, res := validated(t)
	want := map[string][]byte{}
	for _, p := range goodParts(t) {
		want[p.name] = p.data
	}
	for _, f := range res.Entries {
		er := OpenEntry(bytes.NewReader(b), f, sumOf(res, f.Name))
		got, err := io.ReadAll(er)
		_ = er.Close()
		if err != nil || !bytes.Equal(got, want[f.Name]) {
			t.Errorf("%s: %v, %d bytes, want %d", f.Name, err, len(got), len(want[f.Name]))
		}
	}
}

func TestEntryReaderRefusesWhatChanged(t *testing.T) {
	b, res := validated(t)
	var f EntryFact
	for _, e := range res.Entries {
		if e.Name == DataName(1) {
			f = e
		}
	}
	sum := sumOf(res, f.Name)
	read := func(ra io.ReaderAt, fact EntryFact, sum string) error {
		er := OpenEntry(ra, fact, sum)
		defer func() { _ = er.Close() }()
		_, err := io.ReadAll(er)
		return err
	}
	if err := read(bytes.NewReader(b), f, sum); err != nil {
		t.Fatalf("the good entry: %v", err)
	}
	// A wrong SHA-256 (the manifest says another file).
	if err := read(bytes.NewReader(b), f, sha([]byte("other"))); !errors.Is(err, ErrEntryChanged) {
		t.Errorf("wrong sha: %v", err)
	}
	// A declared size that is not the size of the data, both ways.
	for _, d := range []int64{-1, 1} {
		g := f
		g.Size += d
		if err := read(bytes.NewReader(b), g, sum); !errors.Is(err, ErrEntryChanged) {
			t.Errorf("size %+d: %v", d, err)
		}
	}
	// A wrong CRC-32.
	g := f
	g.CRC32 ^= 1
	if err := read(bytes.NewReader(b), g, sum); !errors.Is(err, ErrEntryChanged) {
		t.Errorf("crc: %v", err)
	}
	// A byte of the compressed data that changed after the validation: every position of the entry.
	start := int(f.Offset) + 30 + len(f.Name)
	for i := 0; i < int(f.CompressedSize); i++ {
		c := append([]byte(nil), b...)
		c[start+i] ^= 0x55
		if err := read(bytes.NewReader(c), f, sum); err == nil {
			t.Fatalf("a changed byte at %d of the entry is not found", i)
		}
	}
	// The error is sticky: a reader that failed does not give bytes again.
	er := OpenEntry(bytes.NewReader(b), f, sha([]byte("other")))
	_, _ = io.ReadAll(er)
	if n, err := er.Read(make([]byte, 8)); n != 0 || !errors.Is(err, ErrEntryChanged) {
		t.Errorf("after the failure: %d %v", n, err)
	}
}

func TestReadManifestAgreesWithValidate(t *testing.T) {
	b, res := validated(t)
	m, err := ReadManifest(bytes.NewReader(b), int64(len(b)), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	x, _ := json.Marshal(m)
	y, _ := json.Marshal(res.Manifest)
	if !bytes.Equal(x, y) {
		t.Errorf("manifests differ:\n%s\n%s", x, y)
	}
	// A file that is not an archive is refused.
	if _, err := ReadManifest(bytes.NewReader([]byte("not a zip file at all")), 21, Limits{}); err == nil {
		t.Error("garbage accepted")
	}
}

// Assemble writes the shape of the writer with a manifest that it does not check: a good manifest gives a good archive and a lying one is refused by the validator.
func TestAssembleMakesArchivesThatLie(t *testing.T) {
	good, res := validated(t)
	var parts []Part
	for _, p := range goodParts(t) {
		parts = append(parts, Part{Name: p.name, Data: p.data})
	}
	mj, _ := json.Marshal(res.Manifest)
	var buf bytes.Buffer
	if err := Assemble(&buf, parts, mj); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), good) && len(buf.Bytes()) == 0 {
		t.Fatal("empty")
	}
	if _, err := validate(buf.Bytes(), Limits{MaxImportBytes: 1 << 30}); err != nil {
		t.Errorf("assembled from the manifest of the writer: %v", err)
	}
	for name, mod := range map[string]func(m *Manifest){
		"rows": func(m *Manifest) { m.Totals.Rows += 5 },
		"extension": func(m *Manifest) {
			m.Extensions = append(m.Extensions, ManifestExtension{Name: "dblink", Version: "1", Schema: "public"})
		},
		"size": func(m *Manifest) { m.Entries[1].Size++ },
		"sha":  func(m *Manifest) { m.Entries[2].SHA256 = sha([]byte("x")) },
	} {
		m := res.Manifest
		m.Entries = append([]ManifestEntry(nil), m.Entries...)
		mod(&m)
		lie, _ := json.Marshal(m)
		var out bytes.Buffer
		if err := Assemble(&out, parts, lie); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := validate(out.Bytes(), Limits{MaxImportBytes: 1 << 30}); code(err) != CodeInvalid {
			t.Errorf("a manifest that lies about %s: %v", name, err)
		}
	}
}

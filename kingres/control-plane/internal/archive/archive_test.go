// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package archive

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"strings"
	"testing"
)

func TestRoundTripAcceptedExactly(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(*Writer)
	}{
		{"plain", nil},
		{"zip64 forced", func(w *Writer) { w.ForceZip64() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, m := buildGood(t, tc.mod)
			res, err := validate(b, Limits{SQL: sqlCount})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Manifest.Entries) != 9 || res.Manifest.Totals.Tables != 4 || res.Manifest.Totals.Rows != 4 {
				t.Fatalf("manifest %+v", res.Manifest.Totals)
			}
			if len(res.Entries) != 9 || res.Manifest.Entries[0].Name != CatalogName || res.Manifest.Entries[4].Name != DataName(1) {
				t.Fatalf("entries %+v", res.Entries)
			}
			for i, e := range res.Entries {
				if e.SHA256 != m.Entries[i].SHA256 || e.Size != m.Entries[i].Size || e.Zip64 != (tc.mod != nil) {
					t.Fatalf("entry %d: %+v vs %+v", i, e, m.Entries[i])
				}
			}
			if res.SQL[PreDataName].ByKind["statement"] != 2 || res.SQL[PostDataName].ByKind["statement"] != 1 {
				t.Fatalf("sql %+v", res.SQL)
			}
			if len(res.Catalog.Tables) != 5 {
				t.Fatal("catalog")
			}
		})
	}
}

func TestRoundTripSizes(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for _, n := range []int{0, 1, 100, 64 << 10, 3<<20 + 17} {
		parts := goodParts(t)
		big := make([]byte, 0, n+64)
		big = append(big, "id,customer,total\n"...)
		for len(big) < n {
			big = append(big, byte('a'+rng.Intn(26)))
			if rng.Intn(40) == 0 {
				big = append(big, '\n')
			}
		}
		parts[4].data = big
		var buf bytes.Buffer
		w := NewWriter(opaque{&buf})
		meta := map[string]ManifestEntry{}
		for _, p := range parts {
			ew, err := w.Create(p.name)
			if err != nil {
				t.Fatal(err)
			}
			// write in odd slices
			for d := p.data; len(d) > 0; {
				k := 1 + rng.Intn(70000)
				if k > len(d) {
					k = len(d)
				}
				if _, err := ew.Write(d[:k]); err != nil {
					t.Fatal(err)
				}
				d = d[k:]
			}
			if p.meta.Table != "" {
				meta[p.name] = p.meta
			}
		}
		if _, err := w.Finish(goodManifest(), meta); err != nil {
			t.Fatal(err)
		}
		if _, err := validate(buf.Bytes(), Limits{SQL: sqlCount}); err != nil {
			t.Fatalf("size %d: %v", n, err)
		}
	}
}

func TestWriterRefusals(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	for _, name := range []string{PreDataName, "x", DataName(1), ManifestName, "../a"} {
		if _, err := w.Create(name); !errors.Is(err, ErrOrder) {
			t.Errorf("%q first: %v", name, err)
		}
	}
	for _, n := range FixedNames {
		if _, err := w.Create(n); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Create(FixedNames[0]); !errors.Is(err, ErrOrder) {
		t.Error("duplicate")
	}
	if _, err := w.Create(DataName(2)); !errors.Is(err, ErrOrder) {
		t.Error("out of order numbering")
	}
	if _, err := w.Create(DataName(1)); err != nil {
		t.Fatal(err)
	}
	// A data entry without table and columns is not a manifest the validator accepts.
	if _, err := w.Finish(goodManifest(), nil); err == nil {
		t.Error("finish accepted a data entry without a table")
	}
	// Finish without the fixed entries.
	w2 := NewWriter(&bytes.Buffer{})
	if _, err := w2.Finish(goodManifest(), nil); !errors.Is(err, ErrOrder) {
		t.Errorf("finish early: %v", err)
	}
}

func TestWriterByteLimit(t *testing.T) {
	var full bytes.Buffer
	b, _ := buildGood(t, nil)
	full.Write(b)
	for _, lim := range []int64{10, 100, 500, int64(len(b)) - 1} {
		var buf bytes.Buffer
		w := NewWriter(&buf)
		w.SetLimit(lim)
		var err error
		for _, p := range goodParts(t) {
			var ew interface{ Write([]byte) (int, error) }
			ew, err = w.Create(p.name)
			if err == nil {
				_, err = ew.Write(p.data)
			}
			if err != nil {
				break
			}
		}
		if err == nil {
			_, err = w.Finish(goodManifest(), nil)
		}
		if !errors.Is(err, ErrLimit) && err == nil {
			t.Errorf("limit %d: no error", lim)
		}
		if int64(buf.Len()) > lim {
			t.Errorf("limit %d: wrote %d bytes", lim, buf.Len())
		}
	}
	// exactly enough passes
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.SetLimit(int64(len(b)))
	meta := map[string]ManifestEntry{}
	for _, p := range goodParts(t) {
		ew, err := w.Create(p.name)
		if err != nil {
			t.Fatal(err)
		}
		ew.Write(p.data)
		if p.meta.Table != "" {
			meta[p.name] = p.meta
		}
	}
	if _, err := w.Finish(goodManifest(), meta); err != nil {
		t.Fatalf("limit equal to the size: %v", err)
	}
}

func TestDataNamesAndIsData(t *testing.T) {
	for n, want := range map[string]bool{"data/00001.csv": true, "data/00000.csv": false, "data/0001.csv": false, "data/00001.csvx": false, "data/0000a.csv": false, "xdata/00001.csv": false, "data/99999.csv": true} {
		if _, ok := IsData(n); ok != want {
			t.Errorf("%s: %v", n, ok)
		}
	}
	if DataName(12) != "data/00012.csv" {
		t.Error(DataName(12))
	}
}

func TestSQLCheckerSeesBoundedStream(t *testing.T) {
	var seen int64
	b, _ := buildGood(t, nil)
	_, err := validate(b, Limits{SQL: func(name string, r io.Reader) (SQLResult, error) {
		n, _ := io.Copy(io.Discard, r)
		seen += n
		return SQLResult{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if seen != int64(len(goodParts(t)[1].data)+len(goodParts(t)[2].data)+len(goodParts(t)[3].data)) {
		t.Errorf("the checker saw %d bytes", seen)
	}
	// A checker that reads nothing still gets the entry drained and verified.
	if _, err := validate(b, Limits{SQL: func(string, io.Reader) (SQLResult, error) { return SQLResult{}, nil }}); err != nil {
		t.Fatal(err)
	}
	// A checker error is the error of the validation.
	boom := errors.New("boom")
	if _, err := validate(b, Limits{SQL: func(string, io.Reader) (SQLResult, error) { return SQLResult{}, boom }}); !errors.Is(err, boom) {
		t.Errorf("%v", err)
	}
}

func TestReportOfAGoodArchive(t *testing.T) {
	b, _ := buildGood(t, nil)
	res, err := validate(b, Limits{SQL: func(name string, r io.Reader) (SQLResult, error) {
		return SQLResult{ByKind: map[string]int{"CREATE TABLE": 1}, Refused: nil}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	res.FileSHA256 = strings.Repeat("0", 64)
	tg := Target{PostgresMajor: 19, Extensions: map[string]string{"kingres_shard": "0.8", "citext": "1.6"}}
	rs := CheckCompatibility(res.Manifest, tg)
	rep := BuildReport(res, rs, tg)
	if !rep.Valid || rep.Format != Format || rep.Tables.Hash != 1 || rep.Tables.Reference != 1 || rep.Tables.Ledger != 1 || rep.Tables.Columnar != 1 || rep.Tables.Local != 2 {
		t.Fatalf("%+v", rep.Tables)
	}
	if !rep.Needs.LedgerRestart || len(rep.Versions.Extensions) != 2 || rep.Versions.Extensions[0].Installed == nil || rep.Statements.ByKind["CREATE TABLE"] != 3 || rep.Reasons == nil {
		t.Fatalf("%+v", rep)
	}
	// A refused statement makes the report invalid, and carries no text.
	res.SQL[PreDataName] = SQLResult{Refused: []RefusedStatement{{Kind: "CREATE FUNCTION", Position: "schema/pre-data.sql#3", Fingerprint: "len=40 hash=ab", Reason: "not allowed"}}}
	rep = BuildReport(res, nil, tg)
	if rep.Valid || len(rep.Statements.Refused) != 1 {
		t.Fatalf("%+v", rep)
	}
	bad := ReportFromError(invalid("x", "o", "m"), "")
	if bad.Valid || len(bad.Reasons) != 1 {
		t.Fatalf("%+v", bad)
	}
}

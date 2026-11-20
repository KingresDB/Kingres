// SPDX-License-Identifier: AGPL-3.0-only

package anchorev

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/anchortest"
)

var dig = sha256.Sum256([]byte("x"))

const aid = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
const origin = "ledger.example.com/kingres/a/" + aid

func goodFiles(t testing.TB) map[string][]byte {
	a := anchortest.NewTSA(t, anchortest.TSAOpts{Digest: dig})
	frag, root := anchortest.Fragment(dig, []byte{1}, []byte("0123456789abcdef0123456789abcdef"), anchortest.BitcoinAttestation(10))
	return map[string][]byte{
		"tsa": anchortest.TSAFile(origin, 5, dig, a),
		"ots": anchortest.OTSFile(origin, 5, dig, [][]byte{frag}, 10, &root),
		"worm": anchortest.WORMFile(origin, 5, dig, anchortest.WORMEvidence{Key: "k/" + aid + "/00000000000000000005.note", VersionID: "v1", Body: []byte("note"), LockMode: "COMPLIANCE",
			RetainUntil: time.Date(2036, 1, 1, 0, 0, 0, 0, time.UTC), LastModified: time.Date(2026, 11, 19, 11, 0, 0, 0, time.UTC)}),
	}
}

func TestParseReadsTheThreeKinds(t *testing.T) {
	fs := goodFiles(t)
	for kind, b := range fs {
		f, err := Parse(b)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if f.Kind != kind || f.Origin != origin || f.Size != 5 || f.PayloadSHA256 != dig {
			t.Fatalf("%+v", f)
		}
	}
	f, _ := Parse(fs["worm"])
	if f.WORM.VersionID != "v1" || f.WORM.LockMode != "COMPLIANCE" || f.WORM.Attempt == nil || f.WORM.ContentLength != 4 {
		t.Fatalf("%+v", f.WORM)
	}
	o, _ := Parse(fs["ots"])
	if o.OTS.Bitcoin == nil || o.OTS.Bitcoin.Height != 10 || len(o.OTS.Calendars) != 1 {
		t.Fatalf("%+v", o.OTS)
	}
}

func TestParseRefusesWhatThePublisherNeverWrites(t *testing.T) {
	fs := goodFiles(t)
	rep := func(kind, from, to string) []byte {
		s := string(fs[kind])
		if !strings.Contains(s, from) {
			t.Fatalf("%s lacks %q", kind, from)
		}
		return []byte(strings.Replace(s, from, to, 1))
	}
	hx := hex.EncodeToString(dig[:])
	cases := map[string][]byte{
		"format 2":                   rep("tsa", `"format":1`, `"format":2`),
		"unknown kind":               rep("tsa", `"kind":"tsa"`, `"kind":"gcs"`),
		"unknown member of the file": rep("tsa", `"format":1,`, `"format":1,"extra":true,`),
		"unknown member":             rep("tsa", `"recorded_by":"cluster"}`, `"recorded_by":"cluster","x":1}`),
		"recorded by other":          rep("tsa", `"recorded_by":"cluster"`, `"recorded_by":"me"`),
		"upper case digest":          []byte(strings.Replace(string(fs["tsa"]), hx, strings.ToUpper(hx), 1)),
		"time with fraction":         rep("tsa", `"created_at":"2026-11-19T11:05:00Z"`, `"created_at":"2026-11-19T11:05:00.5Z"`),
		"time with offset":           rep("tsa", `"created_at":"2026-11-19T11:05:00Z"`, `"created_at":"2026-11-19T11:05:00+00:00"`),
		"negative size":              rep("tsa", `"size":5`, `"size":-5`),
		"bad lock mode":              rep("worm", `"COMPLIANCE"`, `"NONE"`),
		"zero length":                rep("worm", `"content_length":4`, `"content_length":0`),
		"header without status":      rep("ots", `"status":"claimed","header":""`, `"status":"header_checked","header":""`),
		"bad serial":                 rep("tsa", `"serial":"424242"`, `"serial":"0424242"`),
		"duplicate key":              rep("tsa", `"format":1,`, `"format":1,"format":1,`),
		"trailing":                   append(append([]byte{}, fs["tsa"]...), '\n', 'x'),
		"empty":                      nil,
	}
	for name, b := range cases {
		if _, err := Parse(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Parse(make([]byte, MaxFileSize+1)); err == nil {
		t.Error("over-long accepted")
	}
}

func FuzzParse(f *testing.F) {
	for _, b := range goodFiles(f) {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		ev, err := Parse(b)
		if err != nil {
			return
		}
		if (ev.WORM != nil) != (ev.Kind == KindWORM) || (ev.TSA != nil) != (ev.Kind == KindTSA) || (ev.OTS != nil) != (ev.Kind == KindOTS) {
			t.Fatal("the member of the kind is not the only one")
		}
	})
}

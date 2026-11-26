// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package anchor

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var (
	goldenWORM = WORMEvidence{
		Endpoint: "https://s3.example.net", Bucket: "anchors", Key: "kingres-anchors/ab12/00000000000000000017.note",
		VersionID: "3HL4kqtJlcpXroDTDmJ+rmSpXd3dIbrHY", ETag: `"d41d8cd98f00b204e9800998ecf8427e"`,
		ObjectSHA256: strings.Repeat("cd", 32), ContentLength: 321,
		LockMode: "COMPLIANCE", RetainUntil: "2036-12-01T10:00:00Z", LastModified: "2026-12-01T10:00:01Z", SubmitStart: "2026-12-01T10:00:00Z", RecordedBy: "cluster",
	}
	goldenWORMJSON = `{"endpoint":"https://s3.example.net","bucket":"anchors","key":"kingres-anchors/ab12/00000000000000000017.note",` +
		`"version_id":"3HL4kqtJlcpXroDTDmJ+rmSpXd3dIbrHY","etag":"\"d41d8cd98f00b204e9800998ecf8427e\"",` +
		`"object_sha256":"` + strings.Repeat("cd", 32) + `","content_length":321,"lock_mode":"COMPLIANCE",` +
		`"retain_until":"2036-12-01T10:00:00Z","last_modified":"2026-12-01T10:00:01Z","submit_start":"2026-12-01T10:00:00Z","attempt":null,"generation":0,"found":false,"recorded_by":"cluster"}`

	goldenTSA = TSAEvidence{
		TSAURL: "https://tsa.example.net/tsr", TokenDERB64: base64.StdEncoding.EncodeToString([]byte{0x30, 0x03, 0x02, 0x01, 0x01}),
		GenTime: "2026-12-01T10:00:02Z", Serial: "123456789012345678901234567890", Policy: "1.2.3.4.1", SignerKeySHA256: strings.Repeat("ab", 32), RecordedBy: "cluster",
	}
	goldenTSAJSON = `{"tsa_url":"https://tsa.example.net/tsr","token_der_b64":"MAMCAQE=","gen_time":"2026-12-01T10:00:02Z",` +
		`"serial":"123456789012345678901234567890","policy":"1.2.3.4.1","signer_key_sha256":"` + strings.Repeat("ab", 32) + `","recorded_by":"cluster"}`

	otsDigest  = strings.Repeat("ab", 32)
	otsRoot    = strings.Repeat("12", 32)
	goldenOTS  = OTSEvidence{Digest: otsDigest, SubmittedAt: "2026-12-01T10:00:00Z", Calendars: []OTSCalendar{{URI: "https://a.example", FragmentB64: "AQID"}, {URI: "https://b.example", FragmentB64: "BAUG"}}, Bitcoin: &OTSBitcoin{Height: 958391, CalendarURI: "https://b.example", MerkleRoot: otsRoot, Status: BitcoinClaimed}}
	goldenOTSJ = `{"digest":"` + otsDigest + `","submitted_at":"2026-12-01T10:00:00Z","calendars":[{"uri":"https://a.example","fragment_b64":"AQID"},{"uri":"https://b.example","fragment_b64":"BAUG"}],` +
		`"bitcoin":{"height":958391,"calendar_uri":"https://b.example","merkle_root":"` + otsRoot + `","status":"claimed","header":""},"contradicted":[],"dropped":[]}`
)

func TestEvidenceGoldenBytes(t *testing.T) {
	for name, tc := range map[string]struct {
		v    any
		want string
	}{
		"worm": {goldenWORM, goldenWORMJSON}, "tsa": {goldenTSA, goldenTSAJSON}, "ots": {goldenOTS, goldenOTSJ},
	} {
		got, err := EncodeEvidence(tc.v)
		if err != nil || string(got) != tc.want {
			t.Fatalf("%s:\n got %s\nwant %s\n%v", name, got, tc.want, err)
		}
	}
	// pending OTS: bitcoin is null, calendars is [] and never null
	p := OTSEvidence{Digest: otsDigest, SubmittedAt: "2026-12-01T10:00:00Z"}
	got, err := EncodeEvidence(&p)
	if err != nil || string(got) != `{"digest":"`+otsDigest+`","submitted_at":"2026-12-01T10:00:00Z","calendars":[],"bitcoin":null,"contradicted":[],"dropped":[]}` {
		t.Fatalf("%s %v", got, err)
	}
	// AWS endpoint is the empty string, not absent
	w := goldenWORM
	w.Endpoint = ""
	if got, _ := EncodeEvidence(w); !strings.HasPrefix(string(got), `{"endpoint":"","bucket"`) {
		t.Fatal(string(got))
	}
}

func TestEvidenceRoundTrip(t *testing.T) {
	b, _ := EncodeEvidence(goldenWORM)
	w, err := ParseWORMEvidence(b)
	if err != nil || w != goldenWORM {
		t.Fatalf("%+v %v", w, err)
	}
	b, _ = EncodeEvidence(goldenTSA)
	ts, err := ParseTSAEvidence(b)
	if err != nil || ts != goldenTSA {
		t.Fatal(err)
	}
	b, _ = EncodeEvidence(&goldenOTS)
	o, err := ParseOTSEvidence(b)
	if err != nil || o.Digest != goldenOTS.Digest || len(o.Calendars) != 2 || *o.Bitcoin != *goldenOTS.Bitcoin {
		t.Fatal(err)
	}
	for _, v := range []any{goldenWORM, &goldenWORM, goldenTSA, &goldenTSA, goldenOTS, &goldenOTS} {
		a, _ := EncodeEvidence(v)
		b, _ := EncodeEvidence(v)
		if !bytes.Equal(a, b) {
			t.Fatal("not deterministic")
		}
	}
	if _, err := EncodeEvidence(42); err == nil {
		t.Fatal("foreign type")
	}
	if _, err := EncodeEvidence((*WORMEvidence)(nil)); err == nil {
		t.Fatal("nil")
	}
}

func TestEvidenceRejects(t *testing.T) {
	mut := func(f func(*WORMEvidence)) WORMEvidence { w := goldenWORM; f(&w); return w }
	for name, w := range map[string]WORMEvidence{
		"no-bucket":     mut(func(w *WORMEvidence) { w.Bucket = "" }),
		"no-key":        mut(func(w *WORMEvidence) { w.Key = "" }),
		"no-version":    mut(func(w *WORMEvidence) { w.VersionID = "" }),
		"no-sha":        mut(func(w *WORMEvidence) { w.ObjectSHA256 = "" }),
		"upper-sha":     mut(func(w *WORMEvidence) { w.ObjectSHA256 = strings.Repeat("CD", 32) }),
		"bad-mode":      mut(func(w *WORMEvidence) { w.LockMode = "compliance" }),
		"sub-second":    mut(func(w *WORMEvidence) { w.RetainUntil = "2036-12-01T10:00:00.5Z" }),
		"offset":        mut(func(w *WORMEvidence) { w.LastModified = "2026-12-01T11:00:01+01:00" }),
		"recorded-by":   mut(func(w *WORMEvidence) { w.RecordedBy = "me" }),
		"non-ascii":     mut(func(w *WORMEvidence) { w.Key = "k\xc3\xa9" }),
		"control":       mut(func(w *WORMEvidence) { w.ETag = "a\nb" }),
		"long-endpoint": mut(func(w *WORMEvidence) { w.Endpoint = strings.Repeat("e", 2049) }),
	} {
		if _, err := EncodeEvidence(w); err == nil {
			t.Errorf("worm %s accepted", name)
		}
	}
	tm := func(f func(*TSAEvidence)) TSAEvidence { w := goldenTSA; f(&w); return w }
	for name, e := range map[string]TSAEvidence{
		"no-url":      tm(func(e *TSAEvidence) { e.TSAURL = "" }),
		"empty-tok":   tm(func(e *TSAEvidence) { e.TokenDERB64 = "" }),
		"bad-b64":     tm(func(e *TSAEvidence) { e.TokenDERB64 = "!!" }),
		"unpadded":    tm(func(e *TSAEvidence) { e.TokenDERB64 = "MAMCAQE" }),
		"b64-newline": tm(func(e *TSAEvidence) { e.TokenDERB64 = "MAMC\nAQE=" }),
		"serial-0x":   tm(func(e *TSAEvidence) { e.Serial = "0x10" }),
		"serial-neg":  tm(func(e *TSAEvidence) { e.Serial = "-1" }),
		"serial-lz":   tm(func(e *TSAEvidence) { e.Serial = "007" }),
		"no-policy":   tm(func(e *TSAEvidence) { e.Policy = "" }),
		"gen-time":    tm(func(e *TSAEvidence) { e.GenTime = "2026-12-01 10:00:02" }),
	} {
		if _, err := EncodeEvidence(e); err == nil {
			t.Errorf("tsa %s accepted", name)
		}
	}
	om := func(f func(*OTSEvidence)) OTSEvidence {
		w := goldenOTS
		w.Calendars = append([]OTSCalendar(nil), w.Calendars...)
		b := *w.Bitcoin
		w.Bitcoin = &b
		f(&w)
		return w
	}
	for name, e := range map[string]OTSEvidence{
		"short-digest": om(func(e *OTSEvidence) { e.Digest = "ab" }),
		"upper-digest": om(func(e *OTSEvidence) { e.Digest = strings.ToUpper(otsDigest) }),
		"dup-calendar": om(func(e *OTSEvidence) { e.Calendars[1].URI = e.Calendars[0].URI }),
		"empty-frag":   om(func(e *OTSEvidence) { e.Calendars[0].FragmentB64 = "" }),
		"unknown-cal": om(func(e *OTSEvidence) {
			e.Bitcoin = &OTSBitcoin{Height: 1, CalendarURI: "https://z.example", MerkleRoot: otsRoot, Status: BitcoinClaimed}
		}),
		"huge-height": om(func(e *OTSEvidence) {
			e.Bitcoin = &OTSBitcoin{Height: 1 << 40, CalendarURI: "https://a.example", MerkleRoot: otsRoot, Status: BitcoinClaimed}
		}),
		"limit+1": om(func(e *OTSEvidence) {
			e.Bitcoin = &OTSBitcoin{Height: MaxBitcoinHeight + 1, CalendarURI: "https://a.example", MerkleRoot: otsRoot, Status: BitcoinClaimed}
		}),
		"no-status":    om(func(e *OTSEvidence) { e.Bitcoin.Status = "" }),
		"anchored":     om(func(e *OTSEvidence) { e.Bitcoin.Status = "anchored" }),
		"short-root":   om(func(e *OTSEvidence) { e.Bitcoin.MerkleRoot = "12" }),
		"claim-header": om(func(e *OTSEvidence) { e.Bitcoin.Header = strings.Repeat("12", 80) }),
		"checked-none": om(func(e *OTSEvidence) { e.Bitcoin.Status = BitcoinHeaderChecked }),
		"checked-root": om(func(e *OTSEvidence) {
			e.Bitcoin.Status, e.Bitcoin.Header = BitcoinHeaderChecked, strings.Repeat("00", 80)
		}),
		"no-submitted": om(func(e *OTSEvidence) { e.SubmittedAt = "" }),
		"frag-b64-nl":  om(func(e *OTSEvidence) { e.Calendars[0].FragmentB64 = "AQ\nID" }),
	} {
		if _, err := EncodeEvidence(e); err == nil {
			t.Errorf("ots %s accepted", name)
		}
	}
	many := goldenOTS
	many.Bitcoin = nil
	many.Calendars = nil
	for i := 0; i <= MaxOTSCalendars; i++ {
		many.Calendars = append(many.Calendars, OTSCalendar{URI: "https://c" + string(rune('a'+i)) + ".example", FragmentB64: "AQID"})
	}
	if _, err := EncodeEvidence(many); err == nil {
		t.Error("too many calendars accepted")
	}
	many.Calendars = many.Calendars[:MaxOTSCalendars]
	if _, err := EncodeEvidence(many); err != nil {
		t.Errorf("exactly MaxOTSCalendars refused: %v", err)
	}
	// the token limit
	big := goldenTSA
	big.TokenDERB64 = base64.StdEncoding.EncodeToString(make([]byte, MaxTokenDER))
	if _, err := EncodeEvidence(big); err != nil {
		t.Errorf("token at the limit refused: %v", err)
	}
	big.TokenDERB64 = base64.StdEncoding.EncodeToString(make([]byte, MaxTokenDER+1))
	if _, err := EncodeEvidence(big); err == nil {
		t.Error("token over the limit accepted")
	}
}

func TestParseRejectsUnknownAndTrailing(t *testing.T) {
	for _, s := range []string{
		strings.Replace(goldenWORMJSON, `"recorded_by"`, `"extra":1,"recorded_by"`, 1),
		goldenWORMJSON + `{}`, goldenWORMJSON + ` x`, `[]`, `null`, ``, `{`,
		strings.Replace(goldenWORMJSON, `"bucket":"anchors"`, `"bucket":5`, 1),
	} {
		if _, err := ParseWORMEvidence([]byte(s)); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
	if _, err := ParseWORMEvidence([]byte(goldenWORMJSON)); err != nil {
		t.Fatal(err)
	}
}

// A1-14: the parsers accept the canonical encoding and nothing else, so that a
// stored file cannot be altered without changing what it parses to, and a
// verifier written from the text (exact names, no duplicates, compact) reads
// the same as this one.
func TestParseIsCanonicalOnly(t *testing.T) {
	digest := hex.EncodeToString(bytes.Repeat([]byte{7}, 32))
	tev, _ := EncodeEvidence(goldenTSA)
	oev, _ := EncodeEvidence(goldenOTS)
	cases := map[string]string{
		"upper-case-name": strings.Replace(goldenWORMJSON, `"bucket"`, `"BUCKET"`, 1),
		"escaped-name":    strings.Replace(goldenWORMJSON, `"bucket"`, `"buck\u0065t"`, 1),
		"duplicate":       strings.Replace(goldenWORMJSON, `"bucket":"anchors"`, `"bucket":"x","bucket":"anchors"`, 1),
		"spaces":          strings.ReplaceAll(goldenWORMJSON, ",", ", "),
		"leading-space":   " " + goldenWORMJSON,
		"trailing-nl":     goldenWORMJSON + "\n",
		"escaped-value":   strings.Replace(goldenWORMJSON, `"anchors"`, `"anchor\u0073"`, 1),
		"reordered":       `{"bucket":"anchors",` + strings.TrimPrefix(strings.Replace(goldenWORMJSON, `"bucket":"anchors",`, "", 1), "{"),
	}
	for name, s := range cases {
		if _, err := ParseWORMEvidence([]byte(s)); err == nil {
			t.Errorf("worm evidence %s accepted", name)
		}
	}
	if _, err := ParseTSAEvidence([]byte(strings.Replace(string(tev), `"serial"`, `"Serial"`, 1))); err == nil {
		t.Error("tsa evidence with a name in another case accepted")
	}
	if _, err := ParseOTSEvidence([]byte(strings.Replace(string(oev), `"bitcoin"`, `"Bitcoin"`, 1))); err == nil {
		t.Error("ots evidence with a name in another case accepted")
	}
	if _, err := ParseOTSEvidence([]byte(strings.Replace(string(oev), `"calendars":[`, `"calendars":null,"x":[`, 1))); err == nil {
		t.Error("ots evidence with null calendars accepted")
	}
	// the same for a whole file: every variation of a canonical file is refused
	ev, _ := EncodeEvidence(goldenWORM)
	file, err := EncodeEvidenceFile(EvidenceFile{Kind: KindWORM, Origin: "o", Size: 1, PayloadSHA256: digest, CreatedAt: t0, Evidence: ev})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseEvidenceFile(file); err != nil {
		t.Fatal(err)
	}
	fs := string(file)
	for name, s := range map[string]string{
		"upper-case-name": strings.Replace(fs, `"origin"`, `"Origin"`, 1),
		"escaped-name":    strings.Replace(fs, `"origin"`, `"orig\u0069n"`, 1),
		"duplicate":       strings.Replace(fs, `"size":1`, `"size":2,"size":1`, 1),
		"spaces":          strings.Replace(fs, `"size":1`, `"size": 1`, 1),
		"newline":         fs + "\n",
		"inner-spaces":    strings.Replace(fs, `"bucket":"anchors"`, `"bucket" : "anchors"`, 1),
		"size-1.0":        strings.Replace(fs, `"size":1,`, `"size":1.0,`, 1),
		"invalid-utf8":    strings.Replace(fs, `"origin":"o"`, "\"origin\":\"o\xff\"", 1),
	} {
		if _, err := ParseEvidenceFile([]byte(s)); err == nil {
			t.Errorf("file %s accepted", name)
		}
	}
}

func TestEvidenceFile(t *testing.T) {
	digest := hex.EncodeToString(bytes.Repeat([]byte{7}, 32))
	ev, _ := EncodeEvidence(goldenWORM)
	f := EvidenceFile{Kind: KindWORM, Origin: "kingres.example/agg", Size: 17, PayloadSHA256: digest, CreatedAt: time.Date(2026, 12, 1, 10, 0, 5, 999, time.FixedZone("x", 3600)), Evidence: ev}
	b, err := EncodeEvidenceFile(f)
	want := `{"format":1,"kind":"worm","origin":"kingres.example/agg","size":17,"payload_sha256":"` + digest + `","created_at":"2026-12-01T09:00:05Z","evidence":` + goldenWORMJSON + `}`
	if err != nil || string(b) != want {
		t.Fatalf("%s\n%s\n%v", b, want, err)
	}
	g, err := ParseEvidenceFile(b)
	if err != nil || g.Kind != KindWORM || g.Size != 17 || g.Origin != f.Origin || !g.CreatedAt.Equal(time.Date(2026, 12, 1, 9, 0, 5, 0, time.UTC)) {
		t.Fatalf("%+v %v", g, err)
	}
	if b2, _ := EncodeEvidenceFile(g); !bytes.Equal(b, b2) {
		t.Fatal("round trip")
	}
	// the evidence is re-encoded canonically whatever the input spacing
	f.Evidence = json.RawMessage(strings.ReplaceAll(goldenWORMJSON, ",", " , "))
	if b3, err := EncodeEvidenceFile(f); err != nil || !bytes.Equal(b3, b) {
		t.Fatal("not canonical")
	}
	// kinds are checked against their evidence
	f.Kind = KindTSA
	if _, err := EncodeEvidenceFile(f); err == nil {
		t.Fatal("kind and evidence disagree")
	}
	f.Kind = "x"
	if _, err := EncodeEvidenceFile(f); err == nil {
		t.Fatal("unknown kind")
	}
	for name, s := range map[string]string{
		"format-2":   strings.Replace(want, `"format":1`, `"format":2`, 1),
		"extra":      strings.Replace(want, `"size":17`, `"size":17,"x":1`, 1),
		"bad-digest": strings.Replace(want, digest, "zz", 1),
		"bad-time":   strings.Replace(want, "2026-12-01T09:00:05Z", "yesterday", 1),
		"no-origin":  strings.Replace(want, `"origin":"kingres.example/agg"`, `"origin":""`, 1),
		"null-ev":    strings.Replace(want, goldenWORMJSON, "null", 1),
	} {
		if _, err := ParseEvidenceFile([]byte(s)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// size limit
	if _, err := ParseEvidenceFile(make([]byte, MaxEvidenceFile+1)); err == nil {
		t.Fatal("oversized file accepted")
	}
	// a TSA and an OTS file parse too
	tev, _ := EncodeEvidence(goldenTSA)
	oev, _ := EncodeEvidence(goldenOTS)
	for kind, e := range map[string]json.RawMessage{KindTSA: tev, KindOTS: oev} {
		b, err := EncodeEvidenceFile(EvidenceFile{Kind: kind, Origin: "o", Size: 1, PayloadSHA256: digest, CreatedAt: time.Unix(0, 0), Evidence: e})
		if err != nil {
			t.Fatal(err)
		}
		if g, err := ParseEvidenceFile(b); err != nil || g.Kind != kind || string(g.Evidence) != string(e) {
			t.Fatalf("%s: %v", kind, err)
		}
	}
}

func TestTimeFormat(t *testing.T) {
	if FormatTime(time.Date(2026, 1, 2, 3, 4, 5, 6, time.FixedZone("x", -7200))) != "2026-01-02T05:04:05Z" {
		t.Fatal("format")
	}
	for _, s := range []string{"2026-01-02T05:04:05Z"} {
		if _, err := ParseTime(s); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []string{"2026-01-02T05:04:05.0Z", "2026-01-02t05:04:05z", "2026-01-02T05:04:05+00:00", "2026-1-2T05:04:05Z", ""} {
		if _, err := ParseTime(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}

func FuzzParseEvidenceFile(f *testing.F) {
	digest := hex.EncodeToString(bytes.Repeat([]byte{7}, 32))
	for _, e := range []struct {
		kind string
		v    any
	}{{KindWORM, goldenWORM}, {KindTSA, goldenTSA}, {KindOTS, goldenOTS}} {
		ev, _ := EncodeEvidence(e.v)
		b, _ := EncodeEvidenceFile(EvidenceFile{Kind: e.kind, Origin: "o", Size: 1, PayloadSHA256: digest, CreatedAt: time.Unix(0, 0), Evidence: ev})
		f.Add(b)
	}
	f.Add([]byte(`{}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		g, err := ParseEvidenceFile(b)
		if err != nil {
			return
		}
		b2, err := EncodeEvidenceFile(g)
		if err != nil {
			t.Fatalf("parsed but cannot encode: %v", err)
		}
		g2, err := ParseEvidenceFile(b2)
		if err != nil {
			t.Fatal(err)
		}
		b3, _ := EncodeEvidenceFile(g2)
		if !bytes.Equal(b2, b3) {
			t.Fatal("encoding is not a fixed point")
		}
	})
}

func FuzzNewPayload(f *testing.F) {
	f.Add([]byte("o\n1\n" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)) + "\n\n\xe2\x80\x94 n AAAA\n"))
	f.Add([]byte(""))
	f.Fuzz(func(t *testing.T, b []byte) {
		p, err := NewPayload("ab", b, t0)
		if err != nil {
			return
		}
		if !bytes.Equal(p.SignedNote, b) || !bytes.HasPrefix(b, p.NoteText) || bytes.Count(p.NoteText, []byte("\n")) != 3 {
			t.Fatal("payload does not match its input")
		}
		if _, err := NewPayload("ab", p.SignedNote, t0); err != nil {
			t.Fatal(err)
		}
	})
}

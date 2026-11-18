// SPDX-License-Identifier: AGPL-3.0-only

package anchor

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// Tests of the fourth review that belong to this package: the generation of the
// payload (A4-01) and the new members of the WORM evidence (A4-01, A4-02).

func TestPayloadGeneration(t *testing.T) {
	p, err := NewPayload("ab12", note("17", root32(), sigLine), t0)
	if err != nil {
		t.Fatal(err)
	}
	if p.Generation != 0 {
		t.Fatal("a new payload is generation 0")
	}
	t1 := t0.Add(time.Hour)
	q, err := p.WithGeneration(1, t1.Add(500*time.Millisecond))
	if err != nil || q.Generation != 1 || !q.SubmitStart.Equal(t1) || q.Check() != nil {
		t.Fatalf("%+v %v", q, err)
	}
	if p.Generation != 0 || !p.SubmitStart.Equal(t0) {
		t.Fatal("WithGeneration changed its receiver")
	}
	if _, err := p.WithGeneration(1, time.Time{}); err == nil {
		t.Fatal("a new generation needs a new start")
	}
	pm := p
	pm.Generation = MaxGeneration - 1
	if _, err := pm.WithGeneration(MaxGeneration, t1); err != nil {
		t.Fatal(err)
	}
	pm.Generation = MaxGeneration
	if _, err := pm.WithGeneration(MaxGeneration+1, t1); err == nil {
		t.Fatal("generation over the limit accepted")
	}
	bad := p
	bad.Generation = MaxGeneration + 1
	if bad.Check() == nil || CheckPayload(KindWORM, bad) == nil {
		t.Fatal("Check accepted a generation over the limit")
	}
}

func TestWORMEvidenceAttemptAndGeneration(t *testing.T) {
	ev := goldenWORM
	at := "2026-12-01T10:00:00Z"
	ev.Attempt, ev.Generation = &at, 3
	raw, err := EncodeEvidence(ev)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(goldenWORMJSON, `"attempt":null,"generation":0,`, `"attempt":"2026-12-01T10:00:00Z","generation":3,`, 1)
	if string(raw) != want {
		t.Fatalf("\n got %s\nwant %s", raw, want)
	}
	got, err := ParseWORMEvidence(raw)
	if err != nil || got.Attempt == nil || *got.Attempt != at || got.Generation != 3 {
		t.Fatalf("%+v %v", got, err)
	}
	// the null attempt is written, never left out
	if !strings.Contains(goldenWORMJSON, `"attempt":null,"generation":0,`) {
		t.Fatal("the golden text lacks the null attempt")
	}
	// refused: an attempt that is not TimeLayout, a generation over the limit,
	// the members missing, in another order, or the attempt as a number
	for name, s := range map[string]string{
		"fraction":       strings.Replace(want, `"attempt":"2026-12-01T10:00:00Z"`, `"attempt":"2026-12-01T10:00:00.5Z"`, 1),
		"zone":           strings.Replace(want, `"attempt":"2026-12-01T10:00:00Z"`, `"attempt":"2026-12-01T10:00:00+00:00"`, 1),
		"empty":          strings.Replace(want, `"attempt":"2026-12-01T10:00:00Z"`, `"attempt":""`, 1),
		"generation-big": strings.Replace(want, `"generation":3`, `"generation":1001`, 1),
		"negative":       strings.Replace(want, `"generation":3`, `"generation":-1`, 1),
		"no-attempt":     strings.Replace(want, `"attempt":"2026-12-01T10:00:00Z",`, ``, 1),
		"no-generation":  strings.Replace(want, `"generation":3,`, ``, 1),
		"swapped":        strings.Replace(want, `"attempt":"2026-12-01T10:00:00Z","generation":3`, `"generation":3,"attempt":"2026-12-01T10:00:00Z"`, 1),
		"number":         strings.Replace(want, `"attempt":"2026-12-01T10:00:00Z"`, `"attempt":5`, 1),
	} {
		if _, err := ParseWORMEvidence([]byte(s)); err == nil {
			t.Errorf("%s: accepted %s", name, s)
		}
	}
	over := goldenWORM
	over.Generation = MaxGeneration + 1
	if _, err := EncodeEvidence(over); err == nil {
		t.Fatal("encoded a generation over the limit")
	}
	badAt := "yesterday"
	over = goldenWORM
	over.Attempt = &badAt
	if _, err := EncodeEvidence(over); err == nil {
		t.Fatal("encoded a bad attempt")
	}
}

func TestOTSEvidenceDropped(t *testing.T) {
	ev := OTSEvidence{Digest: otsDigest, SubmittedAt: "2026-12-01T10:00:00Z",
		Calendars: []OTSCalendar{{URI: "https://a.example", FragmentB64: "AQID"}},
		Dropped:   []OTSDropped{{CalendarURI: "https://gone.example", Height: 958391, MerkleRoot: otsRoot, At: "2026-12-01T11:00:00Z"}}}
	raw, err := EncodeEvidence(ev)
	if err != nil {
		t.Fatal(err)
	}
	want := `"contradicted":[],"dropped":[{"calendar_uri":"https://gone.example","height":958391,"merkle_root":"` + otsRoot + `","at":"2026-12-01T11:00:00Z"}]}`
	if !strings.HasSuffix(string(raw), want) {
		t.Fatalf("%s", raw)
	}
	if got, err := ParseOTSEvidence(raw); err != nil || len(got.Dropped) != 1 {
		t.Fatalf("%v", err)
	}
	// the member is required, and the canonical order is kept
	if _, err := ParseOTSEvidence([]byte(strings.Replace(string(raw), `,"dropped":[`, `,"dropx":[`, 1))); err == nil {
		t.Fatal("unknown member accepted")
	}
	noDropped := strings.Replace(string(raw), `,"dropped":[{"calendar_uri":"https://gone.example","height":958391,"merkle_root":"`+otsRoot+`","at":"2026-12-01T11:00:00Z"}]`, ``, 1)
	if _, err := ParseOTSEvidence([]byte(noDropped)); err == nil {
		t.Fatal("evidence without the dropped member accepted")
	}
	bad := map[string]func(*OTSEvidence){
		"repeat":   func(e *OTSEvidence) { e.Dropped = append(e.Dropped, e.Dropped[0]) },
		"bad-root": func(e *OTSEvidence) { e.Dropped[0].MerkleRoot = "ab" },
		"bad-time": func(e *OTSEvidence) { e.Dropped[0].At = "yesterday" },
		"no-uri":   func(e *OTSEvidence) { e.Dropped[0].CalendarURI = "" },
		"height":   func(e *OTSEvidence) { e.Dropped[0].Height = MaxBitcoinHeight + 1 },
		"too-many": func(e *OTSEvidence) { e.Dropped = make([]OTSDropped, MaxOTSDropped+1) },
	}
	for name, f := range bad {
		c := ev
		c.Dropped = append([]OTSDropped(nil), ev.Dropped...)
		f(&c)
		if _, err := EncodeEvidence(c); err == nil {
			t.Errorf("%s: encoded", name)
		}
	}
	full := ev
	full.Dropped = nil
	for i := 0; i < MaxOTSDropped; i++ {
		full.Dropped = append(full.Dropped, OTSDropped{CalendarURI: "https://gone" + strconv.Itoa(i/MaxOTSDroppedPerCalendar) + ".example", Height: uint64(i), MerkleRoot: otsRoot, At: "2026-12-01T11:00:00Z"})
	}
	if _, err := EncodeEvidence(full); err != nil {
		t.Fatalf("exactly MaxOTSDropped refused: %v", err)
	}
}

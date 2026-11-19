// SPDX-License-Identifier: AGPL-3.0-only

package events

import (
	"strings"
	"testing"
)

const good = "kingres-ledger-event/1\nseq: 3\ntime: 2026-11-19T10:03:00Z\ntype: chain-retired\nchain: " +
	"abababababababababababababababababababababababababababababababab\nepoch: 1\n"

func TestParseReadsTheFourFixedLinesAndTheFields(t *testing.T) {
	ev, err := Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Seq != 3 || ev.Type != "chain-retired" || ev.Fields["epoch"] != "1" || ev.Time.Year() != 2026 {
		t.Fatalf("%+v", ev)
	}
}

func TestParseRefusals(t *testing.T) {
	for name, s := range map[string]string{
		"no final newline":   strings.TrimSuffix(good, "\n"),
		"wrong header":       strings.Replace(good, "event/1", "event/2", 1),
		"seq not first":      strings.Replace(good, "seq: 3\ntime: 2026-11-19T10:03:00Z", "time: 2026-11-19T10:03:00Z\nseq: 3", 1),
		"leading zero seq":   strings.Replace(good, "seq: 3", "seq: 03", 1),
		"negative seq":       strings.Replace(good, "seq: 3", "seq: -3", 1),
		"fraction in time":   strings.Replace(good, "10:03:00Z", "10:03:00.5Z", 1),
		"offset in time":     strings.Replace(good, "10:03:00Z", "10:03:00+01:00", 1),
		"upper case type":    strings.Replace(good, "chain-retired", "Chain-Retired", 1),
		"duplicate field":    good + "epoch: 2\n",
		"empty value":        good + "reason: \n",
		"no value separator": good + "reason\n",
		"control character":  good + "reason: a\tb\n",
		"non ASCII":          good + "reason: café\n",
		"too few lines":      "kingres-ledger-event/1\nseq: 3\ntime: 2026-11-19T10:03:00Z\n",
		"too many fields": good + strings.Repeat("x: y\n", 0) + func() string {
			var b strings.Builder
			for i := 0; i < 20; i++ {
				b.WriteString("f" + strings.Repeat("a", i+1) + ": v\n")
			}
			return b.String()
		}(),
	} {
		if _, err := Parse([]byte(s)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func FuzzParseEvent(f *testing.F) {
	f.Add([]byte(good))
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = Parse(b) })
}

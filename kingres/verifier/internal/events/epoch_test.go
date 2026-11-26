// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package events

import (
	"strings"
	"testing"
)

const (
	hexA = "abababababababababababababababababababababababababababababababab"
	hexB = "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"
)

// epochText is an epoch-closed event as the publisher writes it (design 9.4), with the lines of the test replacing the defaults.
func epochText(over map[string]string, drop ...string) string {
	lines := [][2]string{{"chain", hexA}, {"epoch", "1"}, {"final-size", "5"}, {"final-root", hexB}, {"common-height", "unknown"}, {"kind", "restore"}, {"reason", "the nightly backup was restored"}, {"acknowledged-by", "admin"}}
	var b strings.Builder
	b.WriteString("kingres-ledger-event/1\nseq: 0\ntime: 2026-11-19T10:00:00Z\ntype: epoch-closed\n")
outer:
	for _, l := range lines {
		for _, d := range drop {
			if d == l[0] {
				continue outer
			}
		}
		v := l[1]
		if o, ok := over[l[0]]; ok {
			v = o
		}
		b.WriteString(l[0] + ": " + v + "\n")
	}
	return b.String()
}

func readEpoch(t testing.TB, text string) (*Walk, error) {
	ev, err := Parse([]byte(text))
	if err != nil {
		t.Fatalf("the event does not parse: %v", err)
	}
	w := &Walk{EpochClosed: map[string]EpochClosed{}}
	return w, w.epochClosed(ev, nil)
}

func TestEpochClosedIsReadFieldByField(t *testing.T) {
	w, err := readEpoch(t, epochText(nil))
	if err != nil {
		t.Fatal(err)
	}
	var ec EpochClosed
	for _, e := range w.EpochClosed {
		ec = e
	}
	if ec.Epoch != 1 || ec.FinalSize != 5 || ec.Kind != "restore" || ec.CommonKnown || ec.AcknowledgedBy != "admin" || ec.Reason != "the nightly backup was restored" || ec.FinalRoot[0] != 0xcd {
		t.Fatalf("%+v", ec)
	}
	if got := ec.CommonText(); got != "common height not stated" {
		t.Fatal(got)
	}
	for in, want := range map[string]string{"-1": "no common block", "0": "common height 0", "4": "common height 4"} {
		w, err := readEpoch(t, epochText(map[string]string{"common-height": in}))
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		for _, e := range w.EpochClosed {
			if !e.CommonKnown || e.CommonText() != want {
				t.Fatalf("%s: %+v %s", in, e, e.CommonText())
			}
		}
	}
	for _, k := range []string{"restore", "failover-loss", "other"} {
		if _, err := readEpoch(t, epochText(map[string]string{"kind": k})); err != nil {
			t.Fatalf("%s: %v", k, err)
		}
	}
}

func TestEpochClosedRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		over map[string]string
		drop []string
	}{
		"no chain line":          {nil, []string{"chain"}},
		"no final-size line":     {nil, []string{"final-size"}},
		"no common-height line":  {nil, []string{"common-height"}},
		"no acknowledged-by":     {nil, []string{"acknowledged-by"}},
		"no reason":              {nil, []string{"reason"}},
		"upper case chain":       {map[string]string{"chain": strings.ToUpper(hexA)}, nil},
		"short chain":            {map[string]string{"chain": hexA[:62]}, nil},
		"epoch 0":                {map[string]string{"epoch": "0"}, nil},
		"epoch with a zero":      {map[string]string{"epoch": "01"}, nil},
		"epoch with a sign":      {map[string]string{"epoch": "+1"}, nil},
		"final size 0":           {map[string]string{"final-size": "0"}, nil},
		"final size too large":   {map[string]string{"final-size": "99999999999999999999"}, nil},
		"final root short":       {map[string]string{"final-root": hexB[:60]}, nil},
		"kind unknown":           {map[string]string{"kind": "rollback"}, nil},
		"kind upper case":        {map[string]string{"kind": "Restore"}, nil},
		"common above the final": {map[string]string{"common-height": "5"}, nil},
		"common below -1":        {map[string]string{"common-height": "-2"}, nil},
		"common with zero":       {map[string]string{"common-height": "02"}, nil},
		"common words":           {map[string]string{"common-height": "none"}, nil},
		"reason too long":        {map[string]string{"reason": strings.Repeat("x", 201)}, nil},
		"acknowledged-by long":   {map[string]string{"acknowledged-by": strings.Repeat("x", 201)}, nil},
	} {
		if _, err := readEpoch(t, epochText(tc.over, tc.drop...)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// a line that is not one of the event
	ev, err := Parse([]byte(epochText(nil) + "extra: 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := (&Walk{EpochClosed: map[string]EpochClosed{}}).epochClosed(ev, nil); err == nil {
		t.Error("an unknown line was accepted")
	}
	// the same epoch closed twice
	w := &Walk{EpochClosed: map[string]EpochClosed{}}
	ev, _ = Parse([]byte(epochText(nil)))
	if err := w.epochClosed(ev, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.epochClosed(ev, nil); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("a second event for the same epoch: %v", err)
	}
}

func FuzzEpochClosedEvent(f *testing.F) {
	f.Add([]byte(epochText(nil)))
	f.Add([]byte(epochText(map[string]string{"common-height": "-1", "kind": "failover-loss"})))
	f.Add([]byte(epochText(map[string]string{"common-height": "4"})))
	f.Fuzz(func(t *testing.T, b []byte) {
		ev, err := Parse(b)
		if err != nil {
			return
		}
		w := &Walk{EpochClosed: map[string]EpochClosed{}}
		if w.epochClosed(ev, nil) == nil {
			// a verified-shaped event must read back what it said
			for _, ec := range w.EpochClosed {
				if ec.Epoch < 1 || ec.FinalSize < 1 || (ec.CommonKnown && ec.CommonHeight >= int64(ec.FinalSize)) || ec.CommonHeight < -1 { // #nosec G115 -- bounded by the parser
					t.Fatalf("%+v", ec)
				}
			}
		}
	})
}

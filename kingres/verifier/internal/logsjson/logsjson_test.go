// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package logsjson

import (
	"fmt"
	"strings"
	"testing"
)

const cid = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const aid = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// good is the form the publisher writes (ledgerpub.LogsJSON): one line, members in this order.
var good = fmt.Sprintf(`{"format":1,"logs":[{"kind":"aggregate","origin":"ledger.example.com/kingres/a/%s","state":"active","size":3},{"kind":"chain","origin":"ledger.example.com/kingres/c/%s/e2","state":"closed","size":0}]}`+"\n", aid, cid)

func TestParseReadsTheFormatOfThePublisher(t *testing.T) {
	logs, err := Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 2 {
		t.Fatalf("%+v", logs)
	}
	a, c := logs[0], logs[1]
	if a.Kind != "aggregate" || a.ID != aid || a.Prefix != "ledger.example.com/kingres" || a.State != "active" || a.Size != 3 {
		t.Fatalf("%+v", a)
	}
	if c.Kind != "chain" || c.ID != cid || c.Epoch != 2 || c.State != "closed" {
		t.Fatalf("%+v", c)
	}
	if l, err := Parse([]byte(`{"format":1,"logs":[]}`)); err != nil || len(l) != 0 {
		t.Fatal(l, err)
	}
}

func TestParseRefusesWhatItDoesNotKnow(t *testing.T) {
	chain := func(extra string) string {
		return fmt.Sprintf(`{"format":1,"logs":[{"kind":"chain","origin":"p/c/%s/e1","state":"active","size":1%s}]}`, cid, extra)
	}
	cases := map[string]string{
		"format 2":              strings.Replace(good, `"format":1`, `"format":2`, 1),
		"format 0":              strings.Replace(good, `"format":1`, `"format":0`, 1),
		"format as a string":    strings.Replace(good, `"format":1`, `"format":"1"`, 1),
		"no format":             `{"logs":[]}`,
		"unknown member":        `{"format":1,"logs":[],"extra":1}`,
		"unknown log member":    chain(`,"note":"x"`),
		"unknown kind":          strings.Replace(good, `"kind":"aggregate"`, `"kind":"digest"`, 1),
		"unknown state":         strings.Replace(good, `"active"`, `"paused"`, 1),
		"negative size":         strings.Replace(good, `"size":3`, `"size":-3`, 1),
		"size as a float":       strings.Replace(good, `"size":3`, `"size":3.5`, 1),
		"origin not of a chain": strings.Replace(good, "/c/"+cid+"/e2", "/c/"+cid, 1),
		"epoch 0":               strings.Replace(good, "/e2", "/e0", 1),
		"epoch with a zero":     strings.Replace(good, "/e2", "/e02", 1),
		"upper case hex":        strings.Replace(good, cid, strings.ToUpper(cid), 1),
		"short id":              strings.Replace(good, aid, aid[:62], 1),
		"duplicate origin":      fmt.Sprintf(`{"format":1,"logs":[{"kind":"aggregate","origin":"p/a/%s","state":"active","size":1},{"kind":"aggregate","origin":"p/a/%s","state":"active","size":2}]}`, aid, aid),
		"duplicate key":         `{"format":1,"format":1,"logs":[]}`,
		"not an object":         `[]`,
		"trailing text":         good + "x",
		"logs not an array":     `{"format":1,"logs":{}}`,
		"empty":                 ``,
	}
	for name, doc := range cases {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Parse(make([]byte, MaxSize+1)); err == nil {
		t.Error("an over-long file was accepted")
	}
	// the format message says that nothing is guessed
	_, err := Parse([]byte(strings.Replace(good, `"format":1`, `"format":7`, 1)))
	if err == nil || !strings.Contains(err.Error(), "does not guess") {
		t.Errorf("%v", err)
	}
}

func FuzzParse(f *testing.F) {
	f.Add([]byte(good))
	f.Add([]byte(`{"format":1,"logs":[]}`))
	f.Add([]byte(`{"format":2}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		logs, err := Parse(data)
		if err != nil {
			return
		}
		for _, l := range logs {
			if l.Kind != "chain" && l.Kind != "aggregate" {
				t.Fatalf("kind %q", l.Kind)
			}
			if len(l.ID) != 64 || l.Prefix == "" {
				t.Fatalf("%+v", l)
			}
			if l.Kind == "chain" && l.Epoch == 0 {
				t.Fatalf("%+v", l)
			}
		}
	})
}

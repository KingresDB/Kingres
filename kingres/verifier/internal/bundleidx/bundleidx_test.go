// SPDX-License-Identifier: AGPL-3.0-only

package bundleidx

import (
	"strings"
	"testing"
)

const id = "abababababababababababababababababababababababababababababababab"

func doc(proofs, extra string) string {
	return `{"format":1,"created_at":"2026-12-01T10:05:00Z","cluster_id":"00ff","origin_prefix":"p.example/k","logs":[` +
		`{"kind":"chain","origin":"p.example/k/c/` + id + `/e1","chain_id":"` + id + `","epoch":1,"checkpoint":"c/` + id + `/e1/checkpoint","size":4},` +
		`{"kind":"aggregate","origin":"p.example/k/a/` + id + `","checkpoint":"a/` + id + `/checkpoint","size":2}],` +
		`"anchors":["a/` + id + `/anchors/2/1.json"],"proofs":[` + proofs + `],"notes":"hi"` + extra + `}`
}

func TestParse(t *testing.T) {
	ix, err := Parse([]byte(doc(`{"file":"proofs/a.json","attestation_file":"proofs/a.att.json","attestation_columns":"proofs/a.cols.json"}`, "")))
	if err != nil {
		t.Fatal(err)
	}
	if len(ix.Logs) != 2 || ix.Logs[0].Epoch != 1 || ix.Logs[1].Kind != "aggregate" || len(ix.Proofs) != 1 || ix.Proofs[0].AttestationColumns == "" || ix.Notes != "hi" {
		t.Fatalf("%+v", ix)
	}
}

func TestRefusals(t *testing.T) {
	base := doc("", "")
	rep := func(old, new string) string { return strings.Replace(base, old, new, 1) }
	for name, d := range map[string]string{
		"unknown member":       doc("", `,"extra":1`),
		"duplicate key":        rep(`"format":1`, `"format":1,"format":1`),
		"path up":              doc(`{"file":"../x"}`, ""),
		"path absolute":        doc(`{"file":"/etc/passwd"}`, ""),
		"path backslash":       doc(`{"file":"a\\b"}`, ""),
		"path colon":           doc(`{"file":"C:x"}`, ""),
		"path control":         doc(`{"file":"a\u0001b"}`, ""),
		"path empty":           doc(`{"file":""}`, ""),
		"attestation alone":    doc(`{"file":"a","attestation_file":"b"}`, ""),
		"proof without file":   doc(`{}`, ""),
		"format 2":             rep(`"format":1`, `"format":2`),
		"notes too long":       rep(`"notes":"hi"`, `"notes":"`+strings.Repeat("a", 501)+`"`),
		"bad kind":             rep(`"kind":"chain"`, `"kind":"x"`),
		"chain id upper":       rep(`"chain_id":"`+id, `"chain_id":"AB`+id[2:]),
		"aggregate with epoch": rep(`"origin":"p.example/k/a/`, `"epoch":1,"origin":"p.example/k/a/`),
		"epoch zero":           rep(`"epoch":1`, `"epoch":0`),
		"negative size":        rep(`"size":4`, `"size":-4`),
		"fractional size":      rep(`"size":4`, `"size":4.5`),
		"not json":             "garbage",
		"cluster id not hex":   rep(`"00ff"`, `"zz"`),
		"time not rfc3339":     rep(`2026-12-01T10:05:00Z`, `yesterday`),
		"too deep path":        doc(`{"file":"`+strings.Repeat("a/", 20)+`b"}`, ""),
	} {
		if _, err := Parse([]byte(d)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Parse(make([]byte, MaxSize+1)); err == nil {
		t.Error("a file over its limit")
	}
}

func FuzzParseBundleIndex(f *testing.F) {
	f.Add([]byte(doc(`{"file":"a"}`, "")))
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = Parse(b) })
}

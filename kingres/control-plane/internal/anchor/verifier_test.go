// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package anchor_test

// A verifier that has only the text of doc.go: a file written by hand for each
// kind is read with the exported functions, and every way of writing the same
// content differently is refused.

import (
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
)

const digestHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func fileOf(kind, evidence string) string {
	return `{"format":1,"kind":"` + kind + `","origin":"kingres.example/agg","size":17,"payload_sha256":"` + digestHex +
		`","created_at":"2026-12-01T10:00:05Z","evidence":` + evidence + `}`
}

func TestIndependentReaderOfEvidenceFiles(t *testing.T) {
	files := map[string]string{
		"worm": fileOf("worm", `{"endpoint":"","bucket":"anchors","key":"p/ab12/00000000000000000017.note","version_id":"v1","etag":"\"e\"",`+
			`"object_sha256":"`+digestHex+`","content_length":123,"lock_mode":"GOVERNANCE","retain_until":"2036-12-01T10:00:00Z",`+
			`"last_modified":"2026-12-01T10:00:01Z","submit_start":"2026-12-01T10:00:00Z","attempt":null,"generation":0,"found":false,"recorded_by":"cluster"}`),
		"tsa":         fileOf("tsa", `{"tsa_url":"https://tsa.example/tsr","token_der_b64":"MAMCAQE=","gen_time":"2026-12-01T10:00:02Z","serial":"4242","policy":"1.2.3.4.5","signer_key_sha256":"`+digestHex+`","recorded_by":"cluster"}`),
		"ots-pending": fileOf("ots", `{"digest":"`+digestHex+`","submitted_at":"2026-12-01T10:00:00Z","calendars":[{"uri":"https://a.example","fragment_b64":"AQID"}],"bitcoin":null,"contradicted":[],"dropped":[]}`),
		"ots-claimed": fileOf("ots", `{"digest":"`+digestHex+`","submitted_at":"2026-12-01T10:00:00Z","calendars":[{"uri":"https://a.example","fragment_b64":"AQID"}],`+
			`"bitcoin":{"height":960000,"calendar_uri":"https://a.example","merkle_root":"`+digestHex+`","status":"claimed","header":""},"contradicted":[],"dropped":[]}`),
	}
	for name, s := range files {
		f, err := anchor.ParseEvidenceFile([]byte(s))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if f.Size != 17 || f.PayloadSHA256 != digestHex || f.Origin != "kingres.example/agg" || f.CreatedAt.Format(anchor.TimeLayout) != "2026-12-01T10:00:05Z" {
			t.Errorf("%s: %+v", name, f)
		}
		// the writer gives the same bytes back
		again, err := anchor.EncodeEvidenceFile(f)
		if err != nil || string(again) != s {
			t.Errorf("%s: not a fixed point: %v", name, err)
		}
	}
	// the variations that a text-based verifier would not accept are refused too
	base := files["tsa"]
	for name, s := range map[string]string{
		"leading-newline":  "\n" + base,
		"trailing-newline": base + "\n",
		"spaces":           strings.Replace(base, `"size":17`, `"size": 17`, 1),
		"member-case":      strings.Replace(base, `"kind"`, `"Kind"`, 1),
		"member-escape":    strings.Replace(base, `"kind"`, `"k\u0069nd"`, 1),
		"member-twice":     strings.Replace(base, `"size":17,`, `"size":17,"size":17,`, 1),
		"unknown-member":   strings.Replace(base, `"size":17,`, `"size":17,"x":1,`, 1),
		"time-offset":      strings.Replace(base, "2026-12-01T10:00:05Z", "2026-12-01T10:00:05+00:00", 1),
		"time-fraction":    strings.Replace(base, "2026-12-01T10:00:05Z", "2026-12-01T10:00:05.0Z", 1),
		"digest-upper":     strings.Replace(base, digestHex, strings.ToUpper(digestHex), 1),
		"size-float":       strings.Replace(base, `"size":17`, `"size":17.0`, 1),
		"format-2":         strings.Replace(base, `"format":1`, `"format":2`, 1),
		"serial-leading-0": strings.Replace(base, `"serial":"4242"`, `"serial":"04242"`, 1),
		"token-unpadded":   strings.Replace(base, `MAMCAQE=`, `MAMCAQE`, 1),
		"kind-other":       strings.Replace(base, `"kind":"tsa"`, `"kind":"worm"`, 1),
	} {
		if _, err := anchor.ParseEvidenceFile([]byte(s)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package worm_test

// A verifier that has nothing but the bytes of an evidence file written from
// the text of doc.go (not by the package) and the note: only exported
// functions are used.

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor/worm"
)

func TestIndependentVerifierOfStoredEvidence(t *testing.T) {
	root := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))
	sig := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 68))
	note := []byte("kingres.example/agg\n17\n" + root + "\n\n" + string(rune(0x2014)) + " kingres.example/agg " + sig + "\n")
	p, err := anchor.NewPayload("ab12", note, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(note)
	ev := fmt.Sprintf(`{"endpoint":"https://s3.example.net","bucket":"anchors","key":"kingres-anchors/ab12/00000000000000000017.note",`+
		`"version_id":"3HL4kqtJlcpXroDTDmJ+rmSpXd3dIbrHY","etag":"\"0123456789abcdef\"","object_sha256":"%s","content_length":%d,`+
		`"lock_mode":"COMPLIANCE","retain_until":"2036-12-01T10:00:00Z","last_modified":"2026-12-01T10:00:01Z","submit_start":"2026-12-01T10:00:00Z","attempt":null,"generation":0,"found":false,"recorded_by":"cluster"}`,
		hex.EncodeToString(sum[:]), len(note))
	file := fmt.Sprintf(`{"format":1,"kind":"worm","origin":"kingres.example/agg","size":17,"payload_sha256":"%s","created_at":"2026-12-01T10:00:05Z","evidence":%s}`, p.DigestHex(), ev)

	f, err := anchor.ParseEvidenceFile([]byte(file))
	if err != nil {
		t.Fatal(err)
	}
	we, err := anchor.ParseWORMEvidence(f.Evidence)
	if err != nil {
		t.Fatal(err)
	}
	// the note is the object that the evidence describes, and belongs to the file
	if err := worm.VerifyNote(we, "ab12", note, f.PayloadSHA256); err != nil {
		t.Fatal(err)
	}
	// what the verifier fetched with its own HEAD of the version
	h := http.Header{}
	h.Set("X-Amz-Version-Id", we.VersionID)
	h.Set("X-Amz-Object-Lock-Mode", "COMPLIANCE")
	h.Set("X-Amz-Object-Lock-Retain-Until-Date", "2036-12-01T10:00:00.000Z")
	h.Set("Content-Length", strconv.Itoa(len(note)))
	if err := worm.VerifyHead(we, h); err != nil {
		t.Fatal(err)
	}
	h.Set("X-Amz-Object-Lock-Retain-Until-Date", "2026-12-02T10:00:00.000Z")
	if worm.VerifyHead(we, h) == nil {
		t.Fatal("another retention date accepted")
	}
	// a note that is not the object, a payload digest of another note, another key
	altered := bytes.Replace(note, []byte("17"), []byte("18"), 1)
	if worm.VerifyNote(we, "ab12", altered, f.PayloadSHA256) == nil {
		t.Error("an altered note accepted")
	}
	if worm.VerifyNote(we, "ab13", note, f.PayloadSHA256) == nil {
		t.Error("another aggregate accepted")
	}
	// a file that was edited in a way that does not change what it parses to is refused
	for name, s := range map[string]string{
		"upper-case-member": strings.Replace(file, `"bucket"`, `"Bucket"`, 1),
		"added-space":       strings.Replace(file, `"size":17,`, `"size": 17,`, 1),
		"duplicate-member":  strings.Replace(file, `"size":17,`, `"size":17,"size":17,`, 1),
		"unknown-member":    strings.Replace(file, `"size":17,`, `"size":17,"note":"x",`, 1),
		"no-version":        strings.Replace(file, `"version_id":"3HL4kqtJlcpXroDTDmJ+rmSpXd3dIbrHY"`, `"version_id":""`, 1),
	} {
		if _, err := anchor.ParseEvidenceFile([]byte(s)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

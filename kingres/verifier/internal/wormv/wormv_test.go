// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package wormv

import (
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/anchorev"
)

func evidence(body []byte) *anchorev.WORM {
	return &anchorev.WORM{VersionID: "v7", ObjectSHA256: sha256.Sum256(body), ContentLength: uint64(len(body)), LockMode: "COMPLIANCE",
		RetainUntil: time.Date(2036, 1, 1, 0, 0, 0, 0, time.UTC), LastModified: time.Date(2026, 11, 19, 11, 0, 0, 0, time.UTC), Key: "p/" + strings.Repeat("a", 64) + "/00000000000000000005.note"}
}

func serve(t *testing.T, body []byte, h map[string]string, status int) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range h {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(s.Close)
	return s
}

var full = map[string]string{"x-amz-version-id": "v7", "x-amz-object-lock-mode": "COMPLIANCE", "x-amz-object-lock-retain-until-date": "2036-01-01T00:00:00Z", "Last-Modified": "Thu, 19 Nov 2026 11:00:00 GMT"}

func TestFetchAndCompare(t *testing.T) {
	body := []byte("a note")
	o, err := Fetch(serve(t, body, full, 200).URL)
	if err != nil {
		t.Fatal(err)
	}
	probs, notes := Compare(evidence(body), o)
	if len(probs) != 0 || len(notes) != 0 || !Confirmed(o) {
		t.Fatalf("%v %v", probs, notes)
	}
	// every header that disagrees is a problem
	for _, c := range []struct{ k, v string }{{"x-amz-version-id", "v8"}, {"x-amz-object-lock-mode", "GOVERNANCE"}, {"x-amz-object-lock-retain-until-date", "2030-01-01T00:00:00Z"}, {"Last-Modified", "Thu, 19 Nov 2026 12:00:00 GMT"}} {
		h := map[string]string{}
		for k, v := range full {
			h[k] = v
		}
		h[c.k] = c.v
		o, err := Fetch(serve(t, body, h, 200).URL)
		if err != nil {
			t.Fatal(err)
		}
		if probs, _ := Compare(evidence(body), o); len(probs) != 1 {
			t.Errorf("%s: %v", c.k, probs)
		}
	}
	// other bytes
	o, _ = Fetch(serve(t, []byte("another"), full, 200).URL)
	if probs, _ := Compare(evidence(body), o); len(probs) < 1 {
		t.Error("other bytes accepted")
	}
	// headers the store does not return are said, not assumed
	o, _ = Fetch(serve(t, body, nil, 200).URL)
	probs, notes = Compare(evidence(body), o)
	if len(probs) != 0 || len(notes) != 4 || Confirmed(o) {
		t.Fatalf("%v %v", probs, notes)
	}
}

func TestFetchStatusesAndURLs(t *testing.T) {
	if _, err := Fetch(serve(t, nil, nil, 404).URL); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("%v", err)
	}
	if _, err := Fetch(serve(t, nil, nil, 503).URL); err == nil || !strings.Contains(err.Error(), "could not be reached") {
		t.Errorf("%v", err)
	}
	if _, err := Fetch(serve(t, nil, map[string]string{"Location": "http://127.0.0.1:1/"}, 302).URL); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Errorf("%v", err)
	}
	if _, err := Fetch(serve(t, make([]byte, MaxObject+1), nil, 200).URL); err == nil {
		t.Error("an object over the limit was read")
	}
	for _, u := range []string{"http://example.com/x", "ftp://x/y", "https://u:p@example.com/x", "https://example.com/x#f", "https:///x", "file:///etc/passwd"} {
		if _, err := CheckURL(u); err == nil {
			t.Errorf("%s accepted", u)
		}
	}
	if _, err := CheckURL("https://example.com/x?X-Amz-Signature=abc"); err != nil {
		t.Errorf("a presigned URL: %v", err)
	}
	// the error of an unreachable store does not carry the presigned URL
	_, err := Fetch("http://127.0.0.1:1/x?X-Amz-Signature=SECRETSECRET")
	if err == nil || strings.Contains(err.Error(), "SECRETSECRET") {
		t.Errorf("%v", err)
	}
}

func TestCheckKey(t *testing.T) {
	id := strings.Repeat("b", 64)
	f := &anchorev.File{Size: 5, WORM: &anchorev.WORM{Key: "kingres-anchors/" + id + "/00000000000000000005.note"}}
	if err := CheckKey(f, id); err != nil {
		t.Fatal(err)
	}
	f.WORM.Key = "kingres-anchors/" + id + "/00000000000000000006.note"
	if err := CheckKey(f, id); err == nil {
		t.Error("another size")
	}
	f.WORM.Key = "kingres-anchors/" + strings.Repeat("c", 64) + "/00000000000000000005.note"
	if err := CheckKey(f, id); err == nil {
		t.Error("another aggregate")
	}
	f.WORM.Generation = 2
	f.WORM.Key = "x/" + id + "/00000000000000000005.g2.note"
	if err := CheckKey(f, id); err != nil {
		t.Fatal(err)
	}
	f.WORM.Key = "x/" + id + "/00000000000000000005.note"
	if err := CheckKey(f, id); err == nil {
		t.Error("generation 2 with the key of generation 0")
	}
}

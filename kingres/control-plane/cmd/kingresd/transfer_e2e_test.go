// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// End-to-end tests of the data transfer (contract 0.19.0) against a real kingresd that serves HTTPS and a real PostgreSQL instance. The cluster
// of the test binary that serves HTTPS is shared; the setting is turned on at the start of each test and off at its end.

// transferOn turns the data transfer on for the shared TLS cluster and returns a function that turns it off.
func transferOn(t testing.TB) (*cp, func()) {
	t.Helper()
	e := sharedWithTLS(t)
	e.expect(t, 200, "PATCH", "/settings/cluster", map[string]any{"dataTransfer": map[string]any{"enabled": true, "retentionHours": 24, "maxFileBytes": 1 << 30, "stagingQuotaBytes": 1 << 30, "maxConcurrent": 4}})
	return e, func() {
		e.expect(t, 200, "PATCH", "/settings/cluster", map[string]any{"dataTransfer": map[string]any{"enabled": false}})
	}
}

// raw sends a request with a binary body and returns the status, the headers and the body.
func (e *cp) raw(method, path string, body io.Reader, length int64, headers map[string]string) (int, http.Header, []byte, error) {
	req, _ := http.NewRequest(method, e.base+path, body)
	if length >= 0 {
		req.ContentLength = length
	}
	if e.token != "" {
		req.Header.Set("Authorization", "Bearer "+e.token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b, err
}

func sumHex(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31) ^ seed
	}
	return b
}

func (e *cp) chunk(id string, offset int64, data []byte) (int, http.Header, map[string]any) {
	st, h, b, err := e.raw("PATCH", "/data-uploads/"+id, bytes.NewReader(data), int64(len(data)), map[string]string{"Upload-Offset": strconv.FormatInt(offset, 10), "Content-Type": "application/octet-stream"})
	if err != nil {
		return 0, nil, map[string]any{"transportError": err.Error()}
	}
	return st, h, jsonOf(b)
}

// The setting off: every operation answers 403 data_transfer_disabled, whatever the role.
func TestDataTransferIsOffByDefault(t *testing.T) {
	e := sharedWithTLS(t)
	for _, c := range []struct{ m, p string }{{"POST", "/data-uploads"}, {"GET", "/data-exports"}, {"GET", "/data-exports/1"}, {"GET", "/data-uploads/1"}, {"DELETE", "/data-exports/1"}} {
		st, out, _ := e.call(c.m, c.p, map[string]any{}, nil)
		if st != 403 || problemCode(out) != "data_transfer_disabled" {
			t.Errorf("%s %s: %d %v", c.m, c.p, st, out)
		}
	}
	// A role below kingres_admin is refused before the setting is read.
	ro := e.accountWithRole(t, "xfer_ro_off", "kingres_readonly")
	if st, out, _ := e.as(ro).call("GET", "/data-exports", nil, nil); st != 403 || problemCode(out) != "forbidden" {
		t.Errorf("readonly: %d %v", st, out)
	}
	e.expect(t, 204, "DELETE", "/roles/xfer_ro_off", nil)
}

// A resumable upload from the first byte to the checksum: the offset rules, the alignment, the limits, a request that breaks off, the owner, the
// declared checksum, and the delete.
func TestUploadLifecycle(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	const seg = 65536
	data := pattern(3*seg+1000, 5)
	up := e.expect(t, 201, "POST", "/data-uploads", map[string]any{"purpose": "tableImport", "sizeBytes": len(data), "fileName": "../../etc/passwd"})
	valid(t, "createDataUpload", "201", up)
	id := up["uploadId"].(string)
	if up["state"] != "receiving" || up["offset"] != float64(0) || up["chunkAlign"] != float64(seg) || up["sha256"] != nil || up["sizeBytes"] != float64(len(data)) {
		t.Fatalf("created: %v", up)
	}
	if got := e.expect(t, 200, "GET", "/data-uploads/"+id, nil); got["offset"] != float64(0) || got["state"] != "receiving" {
		t.Errorf("get: %v", got)
	}

	// The rules of a chunk.
	if st, h, out := e.chunk(id, 0, data[:seg-1]); st != 400 || problemCode(out) != "validation_failed" {
		t.Errorf("an unaligned chunk that is not the last: %d %v %v", st, h, out)
	}
	if st, _, out := e.chunk(id, 5, data[:seg]); st != 409 || problemCode(out) != "upload_offset_mismatch" {
		t.Errorf("a wrong offset: %d %v", st, out)
	}
	if st, h, _ := e.chunk(id, seg, data[:seg]); st != 409 || h.Get("Upload-Offset") != "0" {
		t.Errorf("a chunk ahead of the file: %d, header %q", st, h.Get("Upload-Offset"))
	}
	if st, _, out := e.chunk(id, 0, pattern(len(data)+1, 1)); st != 413 || problemCode(out) != "chunk_too_large" {
		t.Errorf("a chunk past the declared size: %d %v", st, out)
	}
	if st, _, out := e.chunk(id, 0, nil); st != 400 {
		t.Errorf("an empty chunk: %d %v", st, out)
	}
	if st, _, _, _ := e.raw("PATCH", "/data-uploads/"+id, bytes.NewReader(data[:seg]), seg, map[string]string{}); st != 400 {
		t.Errorf("no Upload-Offset: %d", st)
	}

	// A request that breaks off: the client promises two segments and sends one and a half. The whole segment stays, the rest is dropped.
	e.breakOff(t, "/data-uploads/"+id, 0, 2*seg, data[:seg+seg/2])
	// The server finishes the request after the client left; the offset is the whole segments it kept.
	var got map[string]any
	for i := 0; i < 100; i++ {
		got = e.expect(t, 200, "GET", "/data-uploads/"+id, nil)
		if got["offset"] == float64(seg) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got["offset"] != float64(seg) {
		t.Fatalf("after a break at 1.5 segments the offset is %v, want %d", got["offset"], seg)
	}

	// Resume at the offset the server holds; the second chunk of two segments, then the last.
	// The offset shows as soon as the first segment is on the disk, while the abandoned request may still be closing the file (truncate,
	// sync, close) and holds the writer of the upload; a resume in that window is answered 409 "Another request is writing this upload",
	// which a client retries. The test retries the same way, for a bounded time.
	var st int
	var h http.Header
	var out map[string]any
	for i := 0; i < 100; i++ {
		st, h, out = e.chunk(id, seg, data[seg:3*seg])
		if detail, _ := out["detail"].(string); st != 409 || !strings.Contains(detail, "Another request is writing") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if st != 204 || h.Get("Upload-Offset") != strconv.Itoa(3*seg) {
		t.Fatalf("resume: %d %v %v", st, h, out)
	}
	// A second request at the same offset loses.
	if st, _, out := e.chunk(id, seg, data[seg:2*seg]); st != 409 || problemCode(out) != "upload_offset_mismatch" {
		t.Errorf("a duplicate chunk: %d %v", st, out)
	}
	// Another role sees nothing of it.
	other := e.accountWithRole(t, "xfer_admin2", "kingres_admin")
	o2 := e.as(other)
	for _, c := range []struct{ m, p string }{{"GET", "/data-uploads/" + id}, {"DELETE", "/data-uploads/" + id}} {
		if st, out, _ := o2.call(c.m, c.p, nil, nil); st != 404 || problemCode(out) != "not_found" {
			t.Errorf("%s %s by another administrator: %d %v", c.m, c.p, st, out)
		}
	}
	if st, _, out := o2.chunk(id, 3*seg, data[3*seg:]); st != 404 {
		t.Errorf("a chunk by another administrator: %d %v", st, out)
	}
	if st, _, out := e.chunk(id, 3*seg, data[3*seg:]); st != 204 {
		t.Fatalf("the last chunk: %d %v", st, out)
	}
	done := e.expect(t, 200, "GET", "/data-uploads/"+id, nil)
	valid(t, "getDataUpload", "200", done)
	if done["state"] != "complete" || done["sha256"] != sumHex(data) || done["offset"] != float64(len(data)) {
		t.Errorf("complete: %v want sha %s", done, sumHex(data))
	}
	if st, _, out := e.chunk(id, int64(len(data)), data[:1]); st < 400 {
		t.Errorf("a chunk after the end: %d %v", st, out)
	}
	e.expect(t, 204, "DELETE", "/data-uploads/"+id, nil)
	if st, _, _ := e.call("GET", "/data-uploads/"+id, nil, nil); st != 404 {
		t.Errorf("a deleted upload: %d", st)
	}
	e.expect(t, 204, "DELETE", "/roles/xfer_admin2", nil)
}

// breakOff sends the head of a request for a chunk of total bytes, then only part of the body, and closes the connection: a client that went away.
func (e *cp) breakOff(t testing.TB, path string, offset, total int, part []byte) {
	t.Helper()
	u, err := url.Parse(e.base)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := tls.Dial("tcp", u.Host, &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	crlf := string([]byte{13, 10})
	head := strings.Join([]string{
		"PATCH " + u.Path + path + " HTTP/1.1", "Host: " + u.Host, "Authorization: Bearer " + e.token, "Upload-Offset: " + strconv.Itoa(offset),
		"Content-Type: application/octet-stream", "Content-Length: " + strconv.Itoa(total), "", "",
	}, crlf)
	if _, err := conn.Write(append([]byte(head), part...)); err != nil {
		t.Fatal(err)
	}
	// Let the server read what was sent before the end of the connection arrives.
	time.Sleep(300 * time.Millisecond)
	_ = conn.Close()
}

// A declared checksum that is not the file's: 422, and the upload is gone. A right one passes. Limits of the request.
func TestUploadChecksumAndLimits(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	data := pattern(100000, 9)
	bad := e.expect(t, 201, "POST", "/data-uploads", map[string]any{"purpose": "tableImport", "sizeBytes": len(data), "sha256": strings.Repeat("0", 64)})
	id := bad["uploadId"].(string)
	if st, _, out := e.chunk(id, 0, data[:65536]); st != 204 {
		t.Fatalf("first: %d %v", st, out)
	}
	if st, _, out := e.chunk(id, 65536, data[65536:]); st != 422 || problemCode(out) != "upload_checksum_mismatch" {
		t.Errorf("last chunk of a file with a wrong checksum: %d %v", st, out)
	}
	if st, _, _ := e.call("GET", "/data-uploads/"+id, nil, nil); st != 404 {
		t.Errorf("the upload with a wrong checksum is still there: %d", st)
	}
	good := e.expect(t, 201, "POST", "/data-uploads", map[string]any{"purpose": "tableImport", "sizeBytes": len(data), "sha256": sumHex(data)})
	gid := good["uploadId"].(string)
	e.chunk(gid, 0, data[:65536])
	if st, _, out := e.chunk(gid, 65536, data[65536:]); st != 204 {
		t.Errorf("a right checksum: %d %v", st, out)
	}
	e.expect(t, 204, "DELETE", "/data-uploads/"+gid, nil)

	for name, body := range map[string]map[string]any{
		"unknown purpose":     {"purpose": "databaseArchive", "sizeBytes": 10},
		"no size":             {"purpose": "tableImport"},
		"zero size":           {"purpose": "tableImport", "sizeBytes": 0},
		"above the file max":  {"purpose": "tableImport", "sizeBytes": (1 << 30) + 1},
		"bad checksum":        {"purpose": "tableImport", "sizeBytes": 10, "sha256": "XYZ"},
		"upper-case checksum": {"purpose": "tableImport", "sizeBytes": 10, "sha256": strings.Repeat("A", 64)},
		"unknown member":      {"purpose": "tableImport", "sizeBytes": 10, "extra": 1},
	} {
		if st, out, _ := e.call("POST", "/data-uploads", body, nil); st != 400 && st != 422 {
			t.Errorf("%s: %d %v", name, st, out)
		}
	}
	// The quota: 1 GiB in all; a second upload of 700 MiB does not fit beside a first of 700 MiB.
	first := e.expect(t, 201, "POST", "/data-uploads", map[string]any{"purpose": "tableImport", "sizeBytes": 700 << 20})
	st, out, _ := e.call("POST", "/data-uploads", map[string]any{"purpose": "tableImport", "sizeBytes": 700 << 20}, nil)
	if st != 409 || problemCode(out) != "transfer_quota_exceeded" {
		t.Errorf("a second upload beyond the quota: %d %v", st, out)
	}
	if d, _ := out["detail"].(string); strings.Contains(d, "root_admin") {
		t.Errorf("the refusal names a role: %v", d)
	}
	e.expect(t, 204, "DELETE", "/data-uploads/"+first["uploadId"].(string), nil)
	// The reservation was released with the delete: the same upload fits again.
	again := e.expect(t, 201, "POST", "/data-uploads", map[string]any{"purpose": "tableImport", "sizeBytes": 700 << 20})
	e.expect(t, 204, "DELETE", "/data-uploads/"+again["uploadId"].(string), nil)
}

func jsonOf(b []byte) map[string]any {
	out := map[string]any{}
	if len(b) > 0 {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

// The staging use is a metric and an alert: reservations that hold 95 percent of the quota raise transferStagingLow (warning) and show in the metrics;
// the alert clears when the room is freed.
func TestStagingMetricsAndAlert(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	e.expect(t, 200, "PATCH", "/settings/cluster", map[string]any{"dataTransfer": map[string]any{"stagingQuotaBytes": 800 << 20}})
	up := e.expect(t, 201, "POST", "/data-uploads", map[string]any{"purpose": "tableImport", "sizeBytes": 760 << 20})
	alert := func() bool {
		out := e.expect(t, 200, "GET", "/alerts?limit=200", nil)
		for _, it := range out["items"].([]any) {
			a := it.(map[string]any)
			if a["kind"] == "transferStagingLow" && a["resolvedAt"] == nil {
				return a["severity"] == "warning"
			}
		}
		return false
	}
	deadline := time.Now().Add(60 * time.Second)
	for !alert() && time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
	}
	if !alert() {
		t.Fatal("no transferStagingLow alert although 95 percent of the quota is reserved")
	}
	now := time.Now().UTC()
	q := "/metrics/query?metric=transferStagingUsedBytes&from=" + now.Add(-5*time.Minute).Format(time.RFC3339) + "&to=" + now.Add(time.Minute).Format(time.RFC3339) + "&step=60"
	m := e.expect(t, 200, "GET", q, nil)
	if !strings.Contains(toJSON(m), "transferStagingUsedBytes") || len(m["series"].([]any)) == 0 {
		t.Errorf("the metric: %v", m)
	}
	e.expect(t, 204, "DELETE", "/data-uploads/"+up["uploadId"].(string), nil)
	deadline = time.Now().Add(60 * time.Second)
	for alert() && time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
	}
	if alert() {
		now2 := time.Now().UTC()
		m2 := e.expect(t, 200, "GET", "/metrics/query?metric=transferStagingUsedBytes&step=60&from="+now2.Add(-3*time.Minute).Format(time.RFC3339)+"&to="+now2.Add(time.Minute).Format(time.RFC3339), nil)
		t.Errorf("the alert did not clear after the room was freed; metric %v", toJSON(m2))
	}
	e.expect(t, 200, "PATCH", "/settings/cluster", map[string]any{"dataTransfer": map[string]any{"stagingQuotaBytes": 1 << 30}})
}

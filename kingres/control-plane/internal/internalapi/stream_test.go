// SPDX-License-Identifier: AGPL-3.0-only

package internalapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fwdproof"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/relay"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scram"
)

// streamCall posts a frame and then body to /forward-stream as the server named by the certificate; it returns the status of the HTTP
// answer, the decoded frame of the answer when there is one, and the raw bytes after it.
func (e *env) streamCall(c *http.Client, version string, frame any, body io.Reader) (int, relay.Response, []byte, error) {
	var buf bytes.Buffer
	switch f := frame.(type) {
	case []byte:
		buf.Write(f)
	default:
		if err := relay.WriteFrame(&buf, frame); err != nil {
			e.t.Fatal(err)
		}
	}
	var rd io.Reader = &buf
	if body != nil {
		rd = io.MultiReader(&buf, body)
	}
	req, _ := http.NewRequest("POST", e.srv.URL+Prefix+"/forward-stream", rd)
	req.Header.Set(HeaderVersion, version)
	resp, err := c.Do(req)
	if err != nil {
		return 0, relay.Response{}, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, relay.Response{}, b, nil
	}
	var out relay.Response
	if err := relay.ReadFrame(resp.Body, &out); err != nil {
		return resp.StatusCode, relay.Response{}, nil, err
	}
	rest, err := io.ReadAll(resp.Body)
	return resp.StatusCode, out, rest, err
}

func admin(op string) ForwardRequest {
	return ForwardRequest{OperationID: op, PathParams: map[string]string{"uploadId": "42"}, Identity: ForwardIdentity{Username: "alice", ClusterRoles: []string{"kingres_admin"}}}
}

// An upload: the body after the frame is not limited to 16 MiB (the limit of the other calls) and reaches the backend whole; the answer is
// a frame with the status and the allowed headers and no body for a 204.
func TestForwardStreamUploadHasNoSizeLimit(t *testing.T) {
	e := newEnv(t)
	peer := cert(t, e.ca, "server2")
	var seen int64
	var seenReq ForwardRequest
	e.stream = func(_ context.Context, p string, r ForwardRequest, body io.Reader, w http.ResponseWriter) error {
		seenReq = r
		n, err := io.Copy(io.Discard, body)
		seen = n
		if p != "server2" {
			t.Errorf("peer %q: it must be the name in the certificate", p)
		}
		w.Header().Set("Upload-Offset", strconv.FormatInt(n, 10))
		w.Header().Set("Set-Cookie", "x=1")
		w.WriteHeader(http.StatusNoContent)
		return err
	}
	req := admin("uploadDataChunk")
	req.Header = map[string]string{"Upload-Offset": "0", "Content-Type": "application/octet-stream"}
	size := int64(20 << 20)
	st, out, rest, err := e.streamCall(e.client(&peer), Version, req, io.LimitReader(zeros{}, size))
	if err != nil || st != 200 || out.Status != 204 || out.Header["Upload-Offset"] != strconv.FormatInt(size, 10) || len(rest) != 0 {
		t.Fatalf("upload: %d %+v %d bytes %v", st, out, len(rest), err)
	}
	if _, bad := out.Header["Set-Cookie"]; bad {
		t.Errorf("a header outside the allowlist was relayed: %v", out.Header)
	}
	if seen != size {
		t.Errorf("the backend read %d of %d bytes: a limit cut the body", seen, size)
	}
	if seenReq.OperationID != "uploadDataChunk" || seenReq.Identity.Username != "alice" || seenReq.Header["Upload-Offset"] != "0" || seenReq.PathParams["uploadId"] != "42" {
		t.Errorf("the backend saw %+v", seenReq)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// A download: the frame comes first, then the bytes as the backend writes them, in order.
func TestForwardStreamDownload(t *testing.T) {
	e := newEnv(t)
	peer := cert(t, e.ca, "server2")
	e.stream = func(_ context.Context, _ string, r ForwardRequest, _ io.Reader, w http.ResponseWriter) error {
		if r.Header["Range"] != "bytes=0-9" {
			t.Errorf("header Range: %v", r.Header)
		}
		w.Header().Set("Content-Length", "10")
		w.Header().Set("Content-Range", "bytes 0-9/100")
		w.Header().Set("ETag", `"abc"`)
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "0123456789")
		return nil
	}
	req := admin("downloadDataExport")
	req.Header = map[string]string{"Range": "bytes=0-9"}
	st, out, rest, err := e.streamCall(e.client(&peer), Version, req, nil)
	if err != nil || st != 200 || out.Status != 206 || out.Header["Content-Range"] != "bytes 0-9/100" || out.Header["Etag"] != `"abc"` || string(rest) != "0123456789" {
		t.Fatalf("download: %d %+v %q %v", st, out, rest, err)
	}
}

// What the leader refuses before anything runs: a frame that is too large, broken, with a member it does not know, with a body, with a
// header of another call; a proof that does not hold; a server that is not the leader; no certificate; another version.
func TestForwardStreamRefusals(t *testing.T) {
	e := newEnv(t)
	peer := cert(t, e.ca, "server2")
	pc := e.client(&peer)
	calls := 0
	e.stream = func(context.Context, string, ForwardRequest, io.Reader, http.ResponseWriter) error {
		calls++
		return nil
	}
	length := func(n uint32, tail string) []byte {
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], n)
		return append(l[:], tail...)
	}
	good := admin("downloadDataExport")
	withBody := good
	withBody.Body = []byte("x")
	badHeader := good
	badHeader.Header = map[string]string{"Authorization": "Bearer x"}
	cases := []struct {
		name  string
		frame any
		want  int
		code  string
	}{
		{"frame larger than 64 KiB", length(relay.MaxFrame+1, ""), 413, "frame_too_large"},
		{"length of 4 GiB", length(1<<31, ""), 413, "frame_too_large"},
		{"empty frame", length(0, ""), 400, "bad_request"},
		{"cut frame", length(50, `{"operationId":"x"}`), 400, "bad_request"},
		{"not json", length(5, "hello"), 400, "bad_request"},
		{"unknown member", length(60, `{"operationId":"x","identity":{"username":"a"},"sql":"drop"}`[:60]), 400, "bad_request"},
		{"no operation", ForwardRequest{Identity: good.Identity}, 400, "bad_request"},
		{"no identity", ForwardRequest{OperationID: "downloadDataExport"}, 400, "bad_request"},
		{"a body in the frame", withBody, 400, "bad_request"},
		{"a header of another call", badHeader, 400, "bad_request"},
	}
	for _, tc := range cases {
		st, _, raw, err := e.streamCall(pc, Version, tc.frame, nil)
		if err != nil || st != tc.want || !strings.Contains(string(raw), tc.code) {
			t.Errorf("%s: %d %s %v", tc.name, st, raw, err)
		}
	}
	// An assertion has no place in this call.
	asr := good
	asr.Assertion = &fwdproof.Assertion{}
	if st, _, _, _ := e.streamCall(pc, Version, asr, nil); st != 400 {
		t.Errorf("assertion: %d", st)
	}
	// The proof is checked before anything runs.
	e.unproven = fwdproof.ErrInvalid
	if st, _, raw, _ := e.streamCall(pc, Version, good, nil); st != 401 || !strings.Contains(string(raw), "identity_unproven") {
		t.Errorf("unproven: %d %s", st, raw)
	}
	e.unproven = fwdproof.ErrUnavailable
	if st, _, raw, _ := e.streamCall(pc, Version, good, nil); st != 503 {
		t.Errorf("proof unavailable: %d %s", st, raw)
	}
	e.unproven = nil
	// Not the leader, no leader, no certificate, another version, a GET.
	e.leader = "someone-else"
	if st, _, raw, _ := e.streamCall(pc, Version, good, nil); st != 409 || !strings.Contains(string(raw), "not_leader") {
		t.Errorf("non-leader: %d %s", st, raw)
	}
	e.leader = ""
	if st, _, _, _ := e.streamCall(pc, Version, good, nil); st != 409 {
		t.Errorf("unknown leader: %d", st)
	}
	e.leader = "leader"
	if st, _, _, err := e.streamCall(e.client(nil), Version, good, nil); err == nil && st != 401 {
		t.Errorf("no certificate: %d", st)
	}
	if st, _, _, _ := e.streamCall(pc, "99", good, nil); st != 409 {
		t.Errorf("version: %d", st)
	}
	if st, _, _ := e.do(pc, "GET", "/forward-stream", Version, nil); st != 404 {
		t.Errorf("GET: %d", st)
	}
	if calls != 0 {
		t.Errorf("a refused call reached the backend %d times", calls)
	}
}

// A backend that fails before it answered gets a plain 400; one that fails after the frame went out cuts the connection, so that the other
// side sees a short body and not a complete one.
func TestForwardStreamBackendFailures(t *testing.T) {
	e := newEnv(t)
	peer := cert(t, e.ca, "server2")
	pc := e.client(&peer)
	e.stream = func(context.Context, string, ForwardRequest, io.Reader, http.ResponseWriter) error {
		return errors.New("no")
	}
	st, _, raw, err := e.streamCall(pc, Version, admin("downloadDataExport"), nil)
	if err != nil || st != 400 || !strings.Contains(string(raw), "refused") || strings.Contains(string(raw), "no\"") {
		t.Errorf("failure before the answer: %d %s %v", st, raw, err)
	}
	e.stream = func(_ context.Context, _ string, _ ForwardRequest, _ io.Reader, w http.ResponseWriter) error {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("only a part"))
		return errors.New("disk")
	}
	_, out, rest, err := e.streamCall(pc, Version, admin("downloadDataExport"), nil)
	if err == nil && len(rest) == 1000 {
		t.Errorf("a failed stream looked complete: %+v %d", out, len(rest))
	}
	// A backend that answers nothing is answered 200 with a frame.
	e.stream = func(context.Context, string, ForwardRequest, io.Reader, http.ResponseWriter) error { return nil }
	if st, out, rest, err := e.streamCall(pc, Version, admin("downloadDataExport"), nil); err != nil || st != 200 || out.Status != 200 || len(rest) != 0 {
		t.Errorf("silent backend: %d %+v %v", st, out, err)
	}
}

type fixedKey struct{ stored []byte }

func (k fixedKey) StoredKey(context.Context, string) ([]byte, error) { return k.stored, nil }

// The call is in the authorization table as a call to the leader, and the proof of the call is its own kind: a proof made for /forward does not
// hold for it, and the headers it carries are part of what the proof binds.
func TestForwardStreamProofBinding(t *testing.T) {
	if Calls["POST /forward-stream"] != ToLeader {
		t.Errorf("policy %v", Calls["POST /forward-stream"])
	}
	clientKey := bytes.Repeat([]byte{7}, 32)
	check := func(signed, checked fwdproof.Message) error {
		p, err := fwdproof.Sign(clientKey, signed, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return fwdproof.NewChecker(fixedKey{stored: scram.Stored(clientKey)}, 0, nil).Check(context.Background(), checked, p)
	}
	req := admin("uploadDataChunk")
	req.Header = map[string]string{"Upload-Offset": "0"}
	stream := req.ProofMessage(fwdproof.KindForwardStream, "a", "b")
	if err := check(stream, stream); err != nil {
		t.Fatalf("a proof does not hold for its own message: %v", err)
	}
	if err := check(req.ProofMessage(fwdproof.KindForward, "a", "b"), stream); !errors.Is(err, fwdproof.ErrInvalid) {
		t.Errorf("a proof of /forward holds for /forward-stream: %v", err)
	}
	other := req
	other.Header = map[string]string{"Upload-Offset": "65536"}
	if err := check(stream, other.ProofMessage(fwdproof.KindForwardStream, "a", "b")); !errors.Is(err, fwdproof.ErrInvalid) {
		t.Errorf("the headers are not part of the proof: %v", err)
	}
	if err := check(stream, req.ProofMessage(fwdproof.KindForwardStream, "a", "c")); !errors.Is(err, fwdproof.ErrInvalid) {
		t.Errorf("the receiver is not part of the proof: %v", err)
	}
}

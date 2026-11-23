// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/contract"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/relay"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/transfer"
)

func transferOps() []contract.Operation {
	var out []contract.Operation
	for _, op := range contract.Operations() {
		if op.Tag == tagDataTransfer {
			out = append(out, op)
		}
	}
	return out
}

func setTransfer(e *env, enabled bool) {
	s := transfer.DefaultSettings()
	s.Enabled = enabled
	e.mem.Settings.DataTransfer = s.API()
}

// The guards of every data transfer operation, in their order: no credential is 401; a role below kingres_admin is 403 forbidden before
// anything else is said; a request that did not arrive over TLS is 403 insecure_transport; the setting off is 403 data_transfer_disabled.
// The operations of the tag are the ten of the contract.
func TestDataTransferGuards(t *testing.T) {
	ops := transferOps()
	if len(ops) != 10 {
		t.Fatalf("%d operations have the tag %q, want 10", len(ops), tagDataTransfer)
	}
	e := newEnv(t)
	e.initialize()
	admin, ops2, ro := e.token(auth.RoleAdmin), e.token(auth.RoleOps), e.token(auth.RoleReadonly)
	for _, op := range ops {
		path := concretePath(op.Path)
		do := func(bearer string, tls bool) map[string]any {
			t.Helper()
			w := e.do(req{method: op.Method, path: path, bearer: bearer, body: bodyFor0(op.Method), tls: tls})
			return map[string]any{"status": w.Code, "body": w.Body.String()}
		}
		has := func(m map[string]any, status int, code string) bool {
			return m["status"] == status && strings.Contains(m["body"].(string), `"code":"`+code+`"`)
		}
		setTransfer(e, false)
		if m := do("", true); m["status"] != 401 {
			t.Errorf("%s anonymous: %v", op.ID, m)
		}
		for _, tok := range []string{ro, ops2} {
			for _, tls := range []bool{false, true} {
				if m := do(tok, tls); !has(m, 403, "forbidden") {
					t.Errorf("%s below admin (tls %v): %v", op.ID, tls, m)
				}
			}
		}
		if m := do(admin, false); !has(m, 403, "insecure_transport") {
			t.Errorf("%s over plain HTTP: %v", op.ID, m)
		}
		if m := do(admin, true); !has(m, 403, "data_transfer_disabled") {
			t.Errorf("%s with the setting off: %v", op.ID, m)
		}
		setTransfer(e, true)
		if m := do(admin, true); has(m, 403, "data_transfer_disabled") || has(m, 403, "insecure_transport") || has(m, 403, "forbidden") || m["status"] == 401 {
			t.Errorf("%s with the setting on is still refused by a guard: %v", op.ID, m)
		}
		// Over plain HTTP the answer stays the TLS one whatever the setting says.
		if m := do(admin, false); !has(m, 403, "insecure_transport") {
			t.Errorf("%s over plain HTTP with the setting on: %v", op.ID, m)
		}
	}
	// The refusals are audited as denied operations where the operation is audited, and say nothing of data.
	for _, ev := range e.audit.Events() {
		if ev.Action == "exportTable" && ev.Outcome == "success" {
			t.Errorf("a refused export is audited as a success: %+v", ev)
		}
	}
}

// A guard that reads the setting fails closed: when the setting cannot be read the operation is not run.
func TestDataTransferSettingUnreadableFailsClosed(t *testing.T) {
	e := newEnv(t)
	e.initialize()
	e.mem.Settings.DataTransfer = nil // a store that knows no setting counts as off
	w := e.do(req{method: "POST", path: "/api/v1/data-uploads", bearer: e.token(auth.RoleAdmin), body: map[string]any{}, tls: true})
	if w.Code != 403 || !strings.Contains(w.Body.String(), "data_transfer_disabled") {
		t.Errorf("%d %s", w.Code, w.Body.String())
	}
}

// directStream is a Forwarder whose leader is another Server in the same process, for both the ordinary and the streaming call: the stream
// goes through the frame (relay.ResponseWriter) as the internal API does.
type directStream struct {
	direct
	streamCalls int
	last        internalapi.ForwardRequest
	streamErr   error
	bodyBytes   int64
}

func (d *directStream) ForwardStream(ctx context.Context, _ string, req internalapi.ForwardRequest, body io.Reader) (relay.Response, io.ReadCloser, error) {
	d.streamCalls++
	d.last = req
	if d.streamErr != nil {
		return relay.Response{}, nil, d.streamErr
	}
	if body == nil {
		body = bytes.NewReader(nil)
	}
	counted := &countReader{r: body}
	rec := httptest.NewRecorder()
	rw := relay.NewResponseWriter(rec)
	if err := d.target.ServeForwardedStream(ctx, "follower-1", req, counted, rw); err != nil {
		return relay.Response{}, nil, err
	}
	rw.Finish()
	d.bodyBytes = counted.n
	var out relay.Response
	b := rec.Body
	if err := relay.ReadFrame(b, &out); err != nil {
		return relay.Response{}, nil, err
	}
	return out, io.NopCloser(b), nil
}

type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// streamLeader is a leader whose two streaming operations are handlers of the test: the upload reads its whole body, the download writes
// what the test says.
func streamLeader(t *testing.T, mods ...func(*Deps)) (*env, *streamProbe) {
	t.Helper()
	p := &streamProbe{}
	leader := newEnvWith(t, func(s *Server) map[string]handlerFunc {
		m := s.handlers()
		m["uploadDataChunk"] = func(c *call, w http.ResponseWriter, r *http.Request) error {
			p.mu.Lock()
			p.deadline, p.hasDeadline = r.Context().Deadline()
			p.offset = r.Header.Get("Upload-Offset")
			p.contentLength = r.ContentLength
			p.uploadID = r.PathValue("uploadId")
			p.mu.Unlock()
			if p.delay > 0 {
				time.Sleep(p.delay)
			}
			n, err := io.Copy(io.Discard, r.Body)
			if err != nil {
				return err
			}
			p.mu.Lock()
			p.read = n
			p.mu.Unlock()
			w.Header().Set("Upload-Offset", strconv.FormatInt(n, 10))
			w.Header().Set("X-Secret", "must-not-pass")
			w.WriteHeader(http.StatusNoContent)
			return nil
		}
		m["downloadDataExport"] = func(c *call, w http.ResponseWriter, r *http.Request) error {
			p.mu.Lock()
			p.deadline, p.hasDeadline = r.Context().Deadline()
			p.rng = r.Header.Get("Range")
			p.mu.Unlock()
			c.addDetails(map[string]any{"transferId": r.PathValue("exportId"), "bytes": 10})
			w.Header().Set("Content-Type", "text/csv")
			w.Header().Set("Content-Length", "10")
			w.Header().Set("ETag", `"abc"`)
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Set-Cookie", "x=1")
			status := http.StatusOK
			if p.rng != "" {
				w.Header().Set("Content-Range", "bytes 0-9/100")
				status = http.StatusPartialContent
			}
			w.WriteHeader(status)
			_, _ = io.WriteString(w, "a,b\n1,2\n3,4")
			return nil
		}
		return m
	}, mods...)
	leader.initialize()
	leader.testUser([]string{auth.RoleAdmin})
	setTransfer(leader, true)
	return leader, p
}

type streamProbe struct {
	mu            sync.Mutex
	deadline      time.Time
	hasDeadline   bool
	offset        string
	rng           string
	read          int64
	contentLength int64
	uploadID      string
	delay         time.Duration
}

func streamPair(t *testing.T, mods ...func(*Deps)) (leader, follower *env, fw *directStream, probe *streamProbe) {
	t.Helper()
	leader, probe = streamLeader(t, mods...)
	fw = &directStream{direct: direct{leader: "leader-1"}}
	fw.direct.target = leader.srv
	follower = newEnv(t, func(d *Deps) { d.Forwarder = fw })
	follower.initialize()
	// The follower cannot read the setting (it is in the leader's database): it is off here, and the operation must still be relayed.
	setTransfer(follower, false)
	return leader, follower, fw, probe
}

// An upload through a follower: the body is not limited to MaxBodyBytes (64 KiB in this test) and does not end with the request timeout, the
// headers cross in the frame, the answer comes back with the allowed headers only, and the leader decides on the setting.
func TestRelayedUploadLeavesTheDefaultLimits(t *testing.T) {
	leader, follower, fw, probe := streamPair(t, func(d *Deps) { d.RequestTimeout = 50 * time.Millisecond })
	probe.delay = 200 * time.Millisecond // longer than the request timeout of both servers
	body := bytes.Repeat([]byte("x"), 3<<20)
	w := follower.do(req{method: "PATCH", path: "/api/v1/data-uploads/42", bearer: follower.token(auth.RoleAdmin), body: body, tls: true,
		header: map[string]string{"Upload-Offset": "65536", "Content-Type": "application/octet-stream"}})
	if w.Code != http.StatusNoContent || w.Header().Get("Upload-Offset") != strconv.Itoa(len(body)) {
		t.Fatalf("%d %v %s", w.Code, w.Header(), w.Body.String())
	}
	if w.Header().Get("X-Secret") != "" {
		t.Error("a header outside the allowlist crossed the relay")
	}
	if fw.streamCalls != 1 || fw.direct.calls != 0 {
		t.Errorf("stream calls %d, ordinary forwards %d", fw.streamCalls, fw.direct.calls)
	}
	if fw.last.OperationID != "uploadDataChunk" || fw.last.PathParams["uploadId"] != "42" || fw.last.Header["Upload-Offset"] != "65536" ||
		fw.last.Header["Content-Length"] != strconv.Itoa(len(body)) || fw.last.Identity.Username != "tester" || fw.last.Source != "cli" || len(fw.last.Body) != 0 {
		t.Errorf("frame: %+v", fw.last)
	}
	if probe.read != int64(len(body)) || probe.offset != "65536" || probe.uploadID != "42" || probe.contentLength != int64(len(body)) {
		t.Errorf("the leader saw %d bytes at offset %q of upload %q (length %d)", probe.read, probe.offset, probe.uploadID, probe.contentLength)
	}
	// The chunk limit of 15 minutes replaces the request timeout of 50 ms.
	if !probe.hasDeadline || time.Until(probe.deadline) < 10*time.Minute {
		t.Errorf("the upload has deadline %v (%v), want about 15 minutes", probe.deadline, probe.hasDeadline)
	}
	// The upload is not an audit event.
	for _, ev := range append(leader.audit.Events(), follower.audit.Events()...) {
		if ev.Action == "uploadDataChunk" {
			t.Errorf("a chunk was audited: %+v", ev)
		}
	}
}

// A chunk of more than 256 MiB is refused at once, with no read of the rest.
func TestUploadChunkBodyLimit(t *testing.T) {
	leader, _ := streamLeader(t)
	body := io.LimitReader(zeroReader{}, MaxChunkBytes+1)
	hr := httptest.NewRequest("PATCH", "/api/v1/data-uploads/42", body)
	hr.TLS = &tls.ConnectionState{Version: tls.VersionTLS13}
	hr.Header.Set("Authorization", "Bearer "+leader.token(auth.RoleAdmin))
	hr.Header.Set("Upload-Offset", "0")
	hr.RemoteAddr = "127.0.0.1:4000"
	w := httptest.NewRecorder()
	leader.srv.ServeHTTP(w, hr)
	if w.Code == http.StatusNoContent {
		t.Errorf("a chunk above the limit was accepted: %d", w.Code)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// A download through a follower: the status of the leader (206), the allowed headers with the Content-Length, the bytes; the leader audits it
// once, naming the follower, and the follower audits nothing; the download has no deadline of the request timeout.
func TestRelayedDownload(t *testing.T) {
	leader, follower, fw, probe := streamPair(t, func(d *Deps) { d.RequestTimeout = 50 * time.Millisecond })
	w := follower.do(req{method: "GET", path: "/api/v1/data-exports/57/content", bearer: follower.token(auth.RoleAdmin), tls: true, header: map[string]string{"Range": "bytes=0-9"}})
	if w.Code != 206 || w.Body.String() != "a,b\n1,2\n3,4" || w.Header().Get("Content-Length") != "10" || w.Header().Get("Content-Range") != "bytes 0-9/100" ||
		w.Header().Get("ETag") != `"abc"` || w.Header().Get("Accept-Ranges") != "bytes" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %v %q", w.Code, w.Header(), w.Body.String())
	}
	if w.Header().Get("Set-Cookie") != "" {
		t.Error("Set-Cookie crossed the relay")
	}
	if probe.rng != "bytes=0-9" || probe.hasDeadline {
		t.Errorf("the leader saw Range %q and deadline %v (%v)", probe.rng, probe.deadline, probe.hasDeadline)
	}
	if fw.streamCalls != 1 {
		t.Errorf("stream calls %d", fw.streamCalls)
	}
	if n := len(follower.audit.Events()); n != 0 {
		t.Errorf("the follower audited %d events", n)
	}
	var n int
	for _, ev := range leader.audit.Events() {
		if ev.Action == "downloadDataExport" {
			n++
			if ev.Details["forwardedBy"] != "follower-1" || ev.Actor != "tester" || ev.Details["transferId"] != "57" || ev.Outcome != "success" {
				t.Errorf("audit event: %+v", ev)
			}
		}
	}
	if n != 1 {
		t.Errorf("the leader audited %d downloads", n)
	}
}

// The relay fails the way the ordinary forward does: a leader that cannot be reached is 503 with Retry-After, a session that cannot prove
// itself is 401, an unknown leader is 503 and nothing is run locally.
func TestRelayFailures(t *testing.T) {
	_, follower, fw, _ := streamPair(t)
	tok := follower.token(auth.RoleAdmin)
	get := func() *httptest.ResponseRecorder {
		return follower.do(req{method: "GET", path: "/api/v1/data-exports/57/content", bearer: tok, tls: true})
	}
	fw.streamErr = errors.New("connection refused to 10.0.0.1")
	w := get()
	if w.Code != 503 || w.Header().Get("Retry-After") == "" || strings.Contains(w.Body.String(), "10.0.0.1") {
		t.Errorf("unreachable leader: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	fw.streamErr = &peerclient.APIError{Status: 401, Code: "identity_unproven"}
	if w := get(); w.Code != 401 || w.Header().Get("WWW-Authenticate") == "" {
		t.Errorf("unproven: %d %v", w.Code, w.Header())
	}
	fw.streamErr = nil
	fw.direct.err = errors.New("no leader")
	calls := fw.streamCalls
	if w := get(); w.Code != 503 || fw.streamCalls != calls {
		t.Errorf("unknown leader: %d, stream calls %d -> %d", w.Code, calls, fw.streamCalls)
	}
	// The ordinary forward is not used for a streaming operation, and a body is never read by the follower itself.
	if fw.direct.calls != 0 {
		t.Errorf("ordinary forwards: %d", fw.direct.calls)
	}
}

// The leader decides on the setting: with it off the leader's 403 is relayed to the client of the follower.
func TestRelayedRefusalOfTheSetting(t *testing.T) {
	leader, follower, _, _ := streamPair(t)
	setTransfer(leader, false)
	w := follower.do(req{method: "PATCH", path: "/api/v1/data-uploads/42", bearer: follower.token(auth.RoleAdmin), body: []byte("abc"), tls: true, header: map[string]string{"Upload-Offset": "0"}})
	if w.Code != 403 || !strings.Contains(w.Body.String(), "data_transfer_disabled") {
		t.Errorf("%d %s", w.Code, w.Body.String())
	}
}

// ServeForwardedStream runs only the two streaming operations, with a verified identity, and nothing else.
func TestServeForwardedStreamRefusals(t *testing.T) {
	leader, _ := streamLeader(t)
	ctx := context.Background()
	ok := internalapi.ForwardRequest{OperationID: "downloadDataExport", PathParams: map[string]string{"exportId": "57"}, Identity: internalapi.ForwardIdentity{Username: "tester", ClusterRoles: []string{auth.RoleAdmin}}}
	for name, mod := range map[string]func(*internalapi.ForwardRequest){
		"an operation that does not stream": func(r *internalapi.ForwardRequest) { r.OperationID = "getDataExport" },
		"an unknown operation":              func(r *internalapi.ForwardRequest) { r.OperationID = "nope" },
		"login":                             func(r *internalapi.ForwardRequest) { r.OperationID = "login" },
		"a role that does not exist":        func(r *internalapi.ForwardRequest) { r.Identity.ClusterRoles = []string{"root"} },
		"no user":                           func(r *internalapi.ForwardRequest) { r.Identity.Username = "" },
		"a missing path parameter":          func(r *internalapi.ForwardRequest) { r.PathParams = nil },
		"a header with a line break":        func(r *internalapi.ForwardRequest) { r.Header = map[string]string{"Range": "bytes=0-1\r\nX: y"} },
	} {
		r := ok
		mod(&r)
		if err := leader.srv.ServeForwardedStream(ctx, "p", r, bytes.NewReader(nil), httptest.NewRecorder()); err == nil {
			t.Errorf("%s was run", name)
		}
	}
	// A readonly identity is stopped by the pipeline, which answers 403 in the frame.
	r := ok
	r.Identity = internalapi.ForwardIdentity{Username: "bob", ClusterRoles: []string{auth.RoleReadonly}}
	rec := httptest.NewRecorder()
	rw := relay.NewResponseWriter(rec)
	if err := leader.srv.ServeForwardedStream(ctx, "p", r, bytes.NewReader(nil), rw); err != nil || rw.Status() != 403 {
		t.Errorf("readonly: %v %d", err, rw.Status())
	}
}

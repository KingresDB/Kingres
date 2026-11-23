// SPDX-License-Identifier: AGPL-3.0-only

package relay_test

// Spike S1 (design data-export-import.md, "Spikes first"): the relay between a follower and the leader. A follower and a leader run as two TLS
// servers with the limits of the real ones (read timeout, write timeout, header timeout), the follower relays an upload and a download to the
// leader through the frame, a client sends and receives through the follower on HTTP/1.1 and on HTTP/2. The tests measure the memory of the
// process while 5 GiB go through each way (KINGRES_SPIKE_BYTES), end the connection of each side in the middle of each direction, stall a
// stream, and send frames that are too large or broken. The same scenarios run in cluster tests later (CP3); this file is the part that
// does not need a cluster.

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/relay"
)

// stream is a deterministic stream of n bytes (AES-CTR of zeros: cheap, incompressible, the same every time).
func stream(n int64, seed byte) io.Reader {
	key := make([]byte, 32)
	key[0] = seed
	blk, _ := aes.NewCipher(key)
	ctr := cipher.NewCTR(blk, make([]byte, 16))
	return io.LimitReader(readerFunc(func(p []byte) (int, error) {
		for i := range p {
			p[i] = 0
		}
		ctr.XORKeyStream(p, p)
		return len(p), nil
	}), n)
}

type readerFunc func(p []byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

func sumOf(n int64, seed byte) string {
	h := sha256.New()
	_, _ = io.Copy(h, stream(n, seed))
	return hex.EncodeToString(h.Sum(nil))
}

func selfSigned(t testing.TB) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "spike"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// limits are the timeouts of a server; the real servers use 30 s and 60 s, the tests that prove the deadlines move use short ones.
type limits struct{ readHeader, read, write, idle time.Duration }

var realLimits = limits{10 * time.Second, 30 * time.Second, 60 * time.Second, relay.IdleTimeout}

type rig struct {
	leader, follower *httptest.Server
	leaderSeen       struct{ upBytes, downBytes atomic.Int64 }
	leaderUpSum      chan string
	leaderErr        chan error
	followerClient   *http.Client
	lim              limits
	leaderHits       atomic.Int64
	leaderDone       atomic.Int64
}

// request is the frame of the request in the spike: the operation and what it needs.
type request struct {
	Operation string            `json:"operation"`
	Header    map[string]string `json:"header,omitempty"`
	Size      int64             `json:"size,omitempty"`
	Seed      int               `json:"seed,omitempty"`
}

func newRig(t *testing.T, lim limits, h2 bool) *rig {
	t.Helper()
	cert := selfSigned(t)
	r := &rig{lim: lim, leaderUpSum: make(chan string, 16), leaderErr: make(chan error, 16)}

	// The leader: the internal listener (HTTP/1.1, the limits of NewHTTPServer), a call that reads the frame and then runs the operation.
	r.leader = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.leaderHits.Add(1)
		defer r.leaderDone.Add(1)
		rc := http.NewResponseController(w)
		var fr request
		if err := relay.ReadFrame(req.Body, &fr); err != nil {
			http.Error(w, `{"error":"bad_request"}`, http.StatusBadRequest)
			return
		}
		out := relay.NewResponseWriter(w)
		switch fr.Operation {
		case "upload":
			body := relay.NewIdleBody(req.Body, rc, lim.idle)
			h := sha256.New()
			n, err := relay.Copy(h, body)
			r.leaderSeen.upBytes.Add(n)
			if err != nil {
				r.leaderErr <- err
				return
			}
			r.leaderUpSum <- hex.EncodeToString(h.Sum(nil))
			out.Header().Set("Upload-Offset", strconv.FormatInt(n, 10))
			out.WriteHeader(http.StatusNoContent)
		case "download":
			out.Header().Set("Content-Length", strconv.FormatInt(fr.Size, 10))
			out.Header().Set("Content-Type", "text/csv")
			out.Header().Set("ETag", `"x"`)
			out.Header().Set("Set-Cookie", "must-not-pass=1") // not in the allowlist
			out.WriteHeader(http.StatusOK)
			iw := relay.NewIdleWriter(out, rc, lim.idle)
			n, err := relay.Copy(iw, stream(fr.Size, byte(fr.Seed)))
			r.leaderSeen.downBytes.Add(n)
			if err != nil {
				r.leaderErr <- err
			}
		case "refuse":
			out.Header().Set("Retry-After", "5")
			out.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(out, `{"code":"upload_offset_mismatch"}`)
		}
		out.Finish()
	}))
	r.leader.EnableHTTP2 = false
	r.leader.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}
	r.leader.Config.ReadHeaderTimeout, r.leader.Config.ReadTimeout, r.leader.Config.IdleTimeout = lim.readHeader, lim.read, 2*time.Minute
	r.leader.StartTLS()
	t.Cleanup(r.leader.Close)

	// The follower reaches the leader as the peer client does: one POST, no overall timeout, its own transport.
	pool := x509.NewCertPool()
	pool.AddCert(r.leader.Certificate())
	peer := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS13}, MaxIdleConnsPerHost: 2}}
	dial := func(ctx context.Context, body io.Reader) (*http.Response, error) {
		rq, err := http.NewRequestWithContext(ctx, http.MethodPost, r.leader.URL+"/internal/v1/forward-stream", body)
		if err != nil {
			return nil, err
		}
		return peer.Do(rq)
	}

	// The follower's public server: the limits of the real one, and the handler of the two operations.
	mux := http.NewServeMux()
	mux.HandleFunc("PATCH /up", func(w http.ResponseWriter, req *http.Request) {
		rc := http.NewResponseController(w)
		ctx, cancel := context.WithTimeout(req.Context(), relay.ChunkTimeout)
		defer cancel()
		body := relay.NewIdleBody(req.Body, rc, lim.idle)
		resp, rest, err := relay.Call(ctx, dial, request{Operation: "upload", Header: relay.Pick(req.Header, relay.RequestHeaders)}, body)
		r.finish(w, resp, rest, err, rc)
	})
	mux.HandleFunc("GET /down", func(w http.ResponseWriter, req *http.Request) {
		rc := http.NewResponseController(w)
		size, _ := strconv.ParseInt(req.URL.Query().Get("size"), 10, 64)
		seed, _ := strconv.Atoi(req.URL.Query().Get("seed"))
		resp, rest, err := relay.Call(req.Context(), dial, request{Operation: "download", Size: size, Seed: seed}, nil)
		r.finish(w, resp, rest, err, rc)
	})
	mux.HandleFunc("GET /refuse", func(w http.ResponseWriter, req *http.Request) {
		rc := http.NewResponseController(w)
		resp, rest, err := relay.Call(req.Context(), dial, request{Operation: "refuse"}, nil)
		r.finish(w, resp, rest, err, rc)
	})
	mux.HandleFunc("GET /bigframe", func(w http.ResponseWriter, req *http.Request) {
		_, _, err := relay.Call(req.Context(), dial, request{Operation: strings.Repeat("x", relay.MaxFrame+1)}, nil)
		if errors.Is(err, relay.ErrFrameTooLarge) {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusTeapot)
	})
	r.follower = httptest.NewUnstartedServer(mux)
	r.follower.EnableHTTP2 = h2
	r.follower.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}
	r.follower.Config.ReadHeaderTimeout, r.follower.Config.ReadTimeout, r.follower.Config.WriteTimeout = lim.readHeader, lim.read, lim.write
	r.follower.StartTLS()
	t.Cleanup(r.follower.Close)
	r.followerClient = r.follower.Client()
	if h2 {
		r.followerClient.Transport.(*http.Transport).ForceAttemptHTTP2 = true
	}
	return r
}

// finish answers the client with the leader's frame and raw bytes, the way the public handler of the real server does.
func (r *rig) finish(w http.ResponseWriter, resp relay.Response, rest io.ReadCloser, err error, rc *http.ResponseController) {
	if err != nil {
		var ce *relay.CallError
		if errors.As(err, &ce) {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	defer func() { _ = rest.Close() }()
	h := w.Header()
	for k, v := range resp.Header {
		if relay.Allowed(k, relay.ResponseHeaders) {
			h.Set(k, v)
		}
	}
	w.WriteHeader(resp.Status)
	if resp.Status == http.StatusNoContent || resp.Status == http.StatusNotModified {
		return
	}
	iw := relay.NewIdleWriter(w, rc, r.lim.idle)
	if _, cerr := relay.Copy(iw, rest); cerr != nil {
		// The status is out; a stream that fails half way can only be cut so that the client sees a short body.
		panic(http.ErrAbortHandler)
	}
}

func spikeBytes() int64 {
	if v := os.Getenv("KINGRES_SPIKE_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return 256 << 20
}

// heapWatcher samples the heap of the process while a transfer runs.
type heapWatcher struct {
	stop chan struct{}
	done chan struct{}
	max  atomic.Uint64
}

func watchHeap() *heapWatcher {
	runtime.GC()
	h := &heapWatcher{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(h.done)
		var m runtime.MemStats
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-h.stop:
				return
			case <-tick.C:
				runtime.ReadMemStats(&m)
				if m.HeapInuse > h.max.Load() {
					h.max.Store(m.HeapInuse)
				}
			}
		}
	}()
	return h
}

func (h *heapWatcher) end() uint64 { close(h.stop); <-h.done; return h.max.Load() }

func baseline() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapInuse
}

// An upload of the full size in chunks and a download through the follower, on both protocols: every byte arrives and the heap is flat.
func TestS1UploadAndDownloadFlatMemory(t *testing.T) {
	total := spikeBytes()
	for _, h2 := range []bool{false, true} {
		name := map[bool]string{false: "HTTP/1.1", true: "HTTP/2"}[h2]
		t.Run(name, func(t *testing.T) {
			r := newRig(t, realLimits, h2)
			base := baseline()
			hw := watchHeap()
			start := time.Now()

			// Upload: chunks of 64 MiB (the console's size), every chunk summed by the leader.
			const chunk = 64 << 20
			var want [][]byte
			all := stream(total, 7)
			var sent int64
			for sent < total {
				n := int64(chunk)
				if total-sent < n {
					n = total - sent
				}
				h := sha256.New()
				body := io.TeeReader(io.LimitReader(all, n), h)
				req, _ := http.NewRequest(http.MethodPatch, r.follower.URL+"/up", body)
				req.ContentLength = n
				req.Header.Set("Upload-Offset", strconv.FormatInt(sent, 10))
				resp, err := r.followerClient.Do(req)
				if err != nil {
					t.Fatalf("chunk at %d: %v", sent, err)
				}
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Upload-Offset") != strconv.FormatInt(n, 10) {
					t.Fatalf("chunk at %d: %d %q", sent, resp.StatusCode, resp.Header.Get("Upload-Offset"))
				}
				if got := <-r.leaderUpSum; got != hex.EncodeToString(h.Sum(nil)) {
					t.Fatalf("chunk at %d: the leader's sum differs", sent)
				}
				want = append(want, h.Sum(nil))
				sent += n
			}
			if r.leaderSeen.upBytes.Load() != total {
				t.Fatalf("the leader received %d of %d bytes", r.leaderSeen.upBytes.Load(), total)
			}

			// Download: Content-Length, the sum of the bytes, the headers of the allowlist only.
			resp, err := r.followerClient.Get(fmt.Sprintf("%s/down?size=%d&seed=3", r.follower.URL, total))
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != 200 || resp.ContentLength != total || resp.Header.Get("ETag") != `"x"` || resp.Header.Get("Content-Type") != "text/csv" {
				t.Fatalf("download: %d %d %v", resp.StatusCode, resp.ContentLength, resp.Header)
			}
			if resp.Header.Get("Set-Cookie") != "" {
				t.Errorf("a header outside the allowlist was relayed: %v", resp.Header)
			}
			h := sha256.New()
			n, err := io.Copy(h, resp.Body)
			_ = resp.Body.Close()
			if err != nil || n != total || hex.EncodeToString(h.Sum(nil)) != sumOf(total, 3) {
				t.Fatalf("download: %d bytes, %v", n, err)
			}
			peak := hw.end()
			grew := int64(peak) - int64(base)
			t.Logf("%s: %d MiB up and %d MiB down in %s (%.0f MiB/s each way), heap before %d MiB, peak %d MiB, growth %d MiB",
				name, total>>20, total>>20, time.Since(start).Round(time.Millisecond), float64(2*(total>>20))/time.Since(start).Seconds(), base>>20, peak>>20, grew>>20)
			// Flat: nothing in the relay grows with the size. A 64 KiB segment per stream and the buffers of TLS and HTTP are far below this.
			if grew > 96<<20 {
				t.Errorf("the heap grew by %d MiB", grew>>20)
			}
			_ = want
		})
	}
}

// The deadlines move with every read and write: with a read timeout of 1 s and a write timeout of 1.5 s on the follower and on the leader, a
// stream of 4 s that never stops for more than the idle time is not ended, a stream that stalls for the idle time is.
func TestS1IdleDeadlinesMove(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		name := map[bool]string{false: "HTTP/1.1", true: "HTTP/2"}[h2]
		t.Run(name, func(t *testing.T) {
			lim := limits{readHeader: 5 * time.Second, read: time.Second, write: 1500 * time.Millisecond, idle: 800 * time.Millisecond}
			r := newRig(t, lim, h2)

			// A slow but steady upload: 4 s in 40 steps of 100 ms.
			pr, pw := io.Pipe()
			go func() {
				blk := make([]byte, 4096)
				for i := 0; i < 40; i++ {
					if _, err := pw.Write(blk); err != nil {
						return
					}
					time.Sleep(100 * time.Millisecond)
				}
				_ = pw.Close()
			}()
			req, _ := http.NewRequest(http.MethodPatch, r.follower.URL+"/up", pr)
			req.ContentLength = 40 * 4096
			req.Header.Set("Upload-Offset", "0")
			resp, err := r.followerClient.Do(req)
			if err != nil {
				t.Fatalf("a steady slow upload was ended: %v", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				t.Fatalf("status %d", resp.StatusCode)
			}
			<-r.leaderUpSum

			// A steady slow download is not ended either: the leader writes 4 s of data in 40 steps.
			// (The leader's download here is fast; the follower's write to a slow client is what the deadline guards.)
			resp, err = r.followerClient.Get(fmt.Sprintf("%s/down?size=%d&seed=1", r.follower.URL, int64(8<<20)))
			if err != nil {
				t.Fatal(err)
			}
			var n int64
			blk := make([]byte, 256<<10)
			for {
				time.Sleep(100 * time.Millisecond)
				m, rerr := io.ReadFull(resp.Body, blk)
				n += int64(m)
				if rerr != nil {
					if errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) && n == 8<<20 {
						err = nil
					} else {
						err = rerr
					}
					break
				}
			}
			_ = resp.Body.Close()
			if err != nil || n != 8<<20 {
				t.Fatalf("a steady slow download was ended after %d bytes: %v", n, err)
			}

			// A stalled upload: the client stops sending for longer than the idle time; the request ends, the leader sees an error.
			pr2, pw2 := io.Pipe()
			go func() { _, _ = pw2.Write(make([]byte, 1024)); time.Sleep(3 * time.Second); _ = pw2.Close() }()
			req, _ = http.NewRequest(http.MethodPatch, r.follower.URL+"/up", pr2)
			req.ContentLength = 4096
			req.Header.Set("Upload-Offset", "0")
			start := time.Now()
			resp, err = r.followerClient.Do(req)
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusNoContent {
					t.Fatal("a stalled upload was accepted")
				}
			}
			if time.Since(start) > 2500*time.Millisecond {
				t.Errorf("the stalled upload ended after %s, the idle time is %s", time.Since(start), lim.idle)
			}
			select {
			case err := <-r.leaderErr:
				t.Logf("the leader saw: %v", err)
			case <-time.After(3 * time.Second):
				t.Error("the leader did not see the end of the stalled upload")
			}
		})
	}
}

// The connection of each side ends in the middle of each direction: the other sides see an error, nobody hangs, no goroutine is left, and a
// download that is cut is a short body with its Content-Length, which a client sees as an error.
func TestS1KillsInTheMiddle(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		name := map[bool]string{false: "HTTP/1.1", true: "HTTP/2"}[h2]
		t.Run(name, func(t *testing.T) {
			before := runtime.NumGoroutine()

			// The client ends its connection in the middle of an upload.
			r := newRig(t, realLimits, h2)
			pr, pw := io.Pipe()
			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				blk := make([]byte, 1<<20)
				for i := 0; i < 64; i++ {
					if _, err := pw.Write(blk); err != nil {
						return
					}
					if i == 8 {
						cancel()
					}
				}
			}()
			req, _ := http.NewRequestWithContext(ctx, http.MethodPatch, r.follower.URL+"/up", pr)
			req.ContentLength = 64 << 20
			req.Header.Set("Upload-Offset", "0")
			if _, err := r.followerClient.Do(req); err == nil {
				t.Error("an upload whose client went away succeeded")
			}
			select {
			case err := <-r.leaderErr:
				if err == nil {
					t.Error("nil error")
				}
			case <-time.After(5 * time.Second):
				t.Error("the leader never saw the client go away")
			}

			// The leader ends the connection in the middle of a download: the client gets less than the Content-Length and an error.
			r2 := newRig(t, realLimits, h2)
			resp, err := r2.followerClient.Get(fmt.Sprintf("%s/down?size=%d&seed=1", r2.follower.URL, int64(512<<20)))
			if err != nil {
				t.Fatal(err)
			}
			var got int64
			buf := make([]byte, 1<<20)
			var rerr error
			for {
				n, e := resp.Body.Read(buf)
				got += int64(n)
				if got > 16<<20 && !r2killed(r2) {
					r2.leader.CloseClientConnections()
				}
				if e != nil {
					rerr = e
					break
				}
			}
			_ = resp.Body.Close()
			if rerr == io.EOF || got >= 512<<20 {
				t.Errorf("a download cut by the leader looked complete: %d bytes, %v", got, rerr)
			}
			if resp.ContentLength != 512<<20 {
				t.Errorf("Content-Length %d", resp.ContentLength)
			}

			// The follower ends its connection to the client in the middle of a download: the leader's write fails and its handler ends.
			r3 := newRig(t, realLimits, h2)
			resp, err = r3.followerClient.Get(fmt.Sprintf("%s/down?size=%d&seed=1", r3.follower.URL, int64(512<<20)))
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.ReadFull(resp.Body, make([]byte, 4<<20))
			_ = resp.Body.Close()
			r3.follower.CloseClientConnections()
			deadline := time.Now().Add(10 * time.Second)
			for r3.leaderDone.Load() < 1 && time.Now().Before(deadline) {
				time.Sleep(50 * time.Millisecond)
			}
			if r3.leaderDone.Load() < 1 {
				t.Error("the leader kept streaming after the follower dropped the client")
			}
			if r3.leaderSeen.downBytes.Load() >= 512<<20 {
				t.Error("the leader sent the whole file to nobody")
			}

			for _, x := range []*rig{r, r2, r3} {
				x.leader.Close()
				x.follower.Close()
			}
			time.Sleep(500 * time.Millisecond)
			if after := runtime.NumGoroutine(); after > before+20 {
				t.Errorf("goroutines: %d before, %d after", before, after)
			}
		})
	}
}

var killed sync.Map

func r2killed(r *rig) bool {
	_, was := killed.LoadOrStore(r, true)
	return was
}

// A leader that refuses the call, a frame that is too large, a frame that is cut and a frame that is broken.
func TestS1FrameLimitsAndRefusals(t *testing.T) {
	r := newRig(t, realLimits, false)

	// A refusal of the operation (409 with a body) passes with its status, its allowed headers and its body.
	resp, err := r.followerClient.Get(r.follower.URL + "/refuse")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 409 || resp.Header.Get("Retry-After") != "5" || !strings.Contains(string(b), "upload_offset_mismatch") {
		t.Errorf("refusal: %d %v %s", resp.StatusCode, resp.Header, b)
	}
	// A frame of 64 KiB + 1 is refused by the sender before anything is sent.
	if resp, err = r.followerClient.Get(r.follower.URL + "/bigframe"); err != nil || resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("big frame: %v %v", resp, err)
	}
	if r.leaderHits.Load() != 1 {
		t.Errorf("the leader saw %d calls; the oversized frame must not reach it", r.leaderHits.Load())
	}

	// The receiver side: the frame is read strictly.
	type fr struct {
		A int `json:"a"`
	}
	enc := func(n uint32, body string) io.Reader {
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], n)
		return io.MultiReader(strings.NewReader(string(l[:])), strings.NewReader(body))
	}
	cases := []struct {
		name string
		in   io.Reader
		want error
	}{
		{"ok", enc(7, `{"a":1}`), nil},
		{"too large", enc(relay.MaxFrame+1, ""), relay.ErrFrameTooLarge},
		{"huge length, no data", enc(1<<31, ""), relay.ErrFrameTooLarge},
		{"empty", enc(0, ""), relay.ErrFrameInvalid},
		{"cut length", strings.NewReader("\x00\x00"), relay.ErrFrameShort},
		{"cut body", enc(20, `{"a":1}`), relay.ErrFrameShort},
		{"no bytes", strings.NewReader(""), relay.ErrFrameShort},
		{"not json", enc(5, `hello`), relay.ErrFrameInvalid},
		{"unknown member", enc(14, `{"a":1,"b":2}`+" "), relay.ErrFrameInvalid},
		{"data after the value", enc(9, `{"a":1} 1`), relay.ErrFrameInvalid},
		{"wrong type", enc(9, `{"a":"x"}`), relay.ErrFrameInvalid},
	}
	for _, tc := range cases {
		var v fr
		err := relay.ReadFrame(tc.in, &v)
		if !errors.Is(err, tc.want) && !(tc.want == nil && err == nil) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
	// The reader is left at the first byte after the frame.
	in := io.MultiReader(enc(7, `{"a":1}`), strings.NewReader("RAW"))
	var v fr
	if err := relay.ReadFrame(in, &v); err != nil {
		t.Fatal(err)
	}
	if rest, _ := io.ReadAll(in); string(rest) != "RAW" {
		t.Errorf("rest %q", rest)
	}
}

// SPDX-License-Identifier: AGPL-3.0-only

package ots

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCal is a calendar server of the tests. mode selects what it does.
type fakeCal struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	mode     string // ok, 500, 503, 429, huge, slow, cut, redirect-other, redirect-same, garbage, noattest, 403, unknownop
	bitcoin  bool   // GET answers with a Bitcoin attestation
	height   uint64
	other    string // redirect target
	posts    int
	gets     int
	badReq   []string
	lastBody []byte
	echo     string // text put in error bodies
}

func newFakeCal(t *testing.T, tls bool) *fakeCal {
	f := &fakeCal{t: t, mode: "ok", height: 950000}
	h := http.HandlerFunc(f.handle)
	if tls {
		f.srv = httptest.NewTLSServer(h)
	} else {
		f.srv = httptest.NewServer(h)
	}
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCal) url() string { return f.srv.URL }

func (f *fakeCal) set(mode string) {
	f.mu.Lock()
	f.mode = mode
	f.mu.Unlock()
}

func (f *fakeCal) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.posts, f.gets
}

func (f *fakeCal) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	mode, bitcoin, height, other, echo := f.mode, f.bitcoin, f.height, f.other, f.echo
	f.mu.Unlock()
	if r.Header.Get("Accept") != AcceptHeader {
		f.mu.Lock()
		f.badReq = append(f.badReq, "accept "+r.Header.Get("Accept"))
		f.mu.Unlock()
	}
	count := func(post bool, body []byte) {
		f.mu.Lock()
		if post {
			f.posts++
			f.lastBody = body
		} else {
			f.gets++
		}
		f.mu.Unlock()
	}
	switch mode {
	case "500", "503", "403", "429":
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			count(true, body)
		} else {
			count(false, nil)
		}
		if mode == "429" {
			w.Header().Set("Retry-After", "7")
			mode = "429"
		}
		code := map[string]int{"500": 500, "503": 503, "403": 403, "429": 429}[mode]
		w.WriteHeader(code)
		io.WriteString(w, "error "+echo)
		return
	case "huge":
		count(r.Method == http.MethodPost, nil)
		w.Write(make([]byte, MaxCalendarResponse+1))
		return
	case "slow":
		io.Copy(io.Discard, r.Body)
		count(r.Method == http.MethodPost, nil)
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
		return
	case "cut":
		count(r.Method == http.MethodPost, nil)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.(net.Conn).Close()
		}
		return
	case "redirect-other":
		count(r.Method == http.MethodPost, nil)
		http.Redirect(w, r, other, http.StatusTemporaryRedirect)
		return
	case "garbage":
		count(r.Method == http.MethodPost, nil)
		w.Write([]byte("this is not a timestamp"))
		return
	case "noattest":
		count(r.Method == http.MethodPost, nil)
		// a valid tree whose only leaf is an unknown attestation
		var wr writer
		wr.byte(0)
		wr.raw([]byte{1, 2, 3, 4, 5, 6, 7, 8})
		wr.varbytes([]byte("x"))
		w.Write(wr.b)
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/digest":
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1000))
		count(true, body)
		if mode == "redirect-same" {
			http.Redirect(w, r, "/digest2", http.StatusTemporaryRedirect)
			return
		}
		f.answerDigest(w, body)
	case r.Method == http.MethodPost && r.URL.Path == "/digest2":
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1000))
		f.answerDigest(w, body)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/timestamp/"):
		count(false, nil)
		c, err := hex.DecodeString(strings.TrimPrefix(r.URL.Path, "/timestamp/"))
		if err != nil || !bitcoin {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, "Pending confirmation in Bitcoin blockchain")
			return
		}
		n := NewNode(c)
		x, _ := n.Child(Op{Tag: TagAppend, Arg: []byte("sibling-from-bitcoin-merkle-path")})
		x, _ = x.Child(Op{Tag: TagSHA256})
		x, _ = x.Child(Op{Tag: TagSHA256}) // a merkle node is a double SHA-256
		a, _ := NewBitcoin(height)
		x.AddAttestation(a)
		b, _ := n.Serialize()
		w.Write(b)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeCal) answerDigest(w http.ResponseWriter, digest []byte) {
	n := NewNode(digest)
	nonce := sha256.Sum256(append([]byte(f.srv.URL), digest...))
	x, _ := n.Child(Op{Tag: TagAppend, Arg: nonce[:8]})
	x, _ = x.Child(Op{Tag: TagSHA256})
	p, err := NewPending(f.srv.URL)
	if err != nil {
		f.t.Errorf("pending uri %q: %v", f.srv.URL, err)
	}
	x.AddAttestation(p)
	b, _ := n.Serialize()
	w.Write(b)
}

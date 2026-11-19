// SPDX-License-Identifier: AGPL-3.0-only

package ledgeranchor

import (
	"bytes"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
)

const (
	tsaUser = "tsa-user-name"
	tsaPass = "Zx9-very-secret-password/+="
)

// tsaEnv is a service with a target of kind tsa on a fake authority that signs real tokens (a local authority built by the test).
type tsaEnv struct {
	*env
	a      *authority
	srv    *httptest.Server
	mu     sync.Mutex
	issue  issueOpts
	handle func(w http.ResponseWriter, r *http.Request, req *tsReq)
	auth   []string
	bundle string
}

func newTSAEnv(t *testing.T) *tsaEnv {
	t.Helper()
	e := &tsaEnv{env: newEnv(t), a: newAuthority(t, authOpts{})}
	e.srv = httptest.NewServer(http.HandlerFunc(e.serve))
	t.Cleanup(e.srv.Close)
	e.bundle = writeBundle(t, e.a.bundlePEM())
	return e
}

func writeBundle(t *testing.T, pem []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tsa-roots.pem")
	if err := os.WriteFile(p, pem, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func (e *tsaEnv) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	e.mu.Lock()
	h, iss := e.handle, e.issue
	e.auth = append(e.auth, r.Header.Get("Authorization"))
	e.mu.Unlock()
	req := parseTSReq(e.t, body)
	if h != nil {
		h(w, r, req)
		return
	}
	w.Header().Set("Content-Type", "application/timestamp-reply")
	_, _ = w.Write(e.a.response(0, e.a.token(req, iss)))
}

func (e *tsaEnv) config(bundle string) []byte {
	return mustRaw(map[string]any{"url": e.srv.URL + "/tsa", "caBundlePath": bundle, "timeoutSeconds": 5, "allowInsecure": true})
}

func (e *tsaEnv) add(name, bundle string) api.LedgerAnchorTarget {
	e.t.Helper()
	tg, err := e.svc.CreateTarget(e.ctx(), "admin", api.LedgerAnchorTargetCreate{Kind: "tsa", Name: name, Enabled: true, Config: e.config(bundle),
		Credentials: mustRaw(map[string]string{"username": tsaUser, "password": tsaPass})})
	if err != nil {
		e.t.Fatalf("creating the tsa target: %v", err)
	}
	return tg
}

func (e *tsaEnv) oneRecord(bundle string) int64 {
	e.t.Helper()
	e.add("authority", bundle)
	e.putAgg("shop", 3)
	if err := e.runEnqueue(); err != nil {
		e.t.Fatal(err)
	}
	if err := e.runSubmit(); err != nil {
		e.t.Fatal(err)
	}
	return e.rows()[0].ID
}

func TestAnAuthorityTokenAnchorsTheCheckpointAndIsExportedAsTsr(t *testing.T) {
	e := newTSAEnv(t)
	id := e.oneRecord(e.bundle)
	r := e.row(id)
	if r.State != anchor.StateAnchored || r.EvidenceTime == nil {
		t.Fatalf("%+v", r)
	}
	ev, err := anchor.ParseTSAEvidence(r.Evidence)
	if err != nil || ev.RecordedBy != "cluster" {
		t.Fatalf("evidence %v %+v", err, ev)
	}
	// the credentials reached the authority as HTTP Basic, and nowhere else
	if len(e.auth) == 0 || !strings.HasPrefix(e.auth[0], "Basic ") {
		t.Fatalf("authorization %v", e.auth)
	}
	noSecret(t, "the texts", e.allText(), tsaUser+"-x", tsaPass)
	tsr, err := e.svc.Evidence(e.ctx(), "shop", id, "tsr")
	if err != nil || len(tsr.Data) < 100 || tsr.Data[0] != 0x30 {
		t.Fatalf("tsr: %d bytes, %v", len(tsr.Data), err)
	}
	if _, err := e.svc.Evidence(e.ctx(), "shop", id, "ots"); err == nil {
		t.Error("an ots file was made for a tsa record")
	}
	f, err := e.svc.Evidence(e.ctx(), "shop", id, "json")
	if err != nil {
		t.Fatal(err)
	}
	if pf, err := anchor.ParseEvidenceFile(f.Data); err != nil || pf.Kind != "tsa" {
		t.Fatalf("%v %+v", err, pf)
	}
	// the Test button asks for a real token and verifies it
	tg, _ := e.svc.Targets(e.ctx())
	if res, err := e.svc.TestTarget(e.ctx(), tg.Items[0].ID); err != nil || !res.OK {
		t.Fatalf("test: %+v %v", res, err)
	}
}

// What the authority can do wrong: the record is never anchored on it, and the job neither hangs nor allocates without a bound.
func TestAnAuthorityThatAnswersWrongAnchorsNothing(t *testing.T) {
	other := newAuthority(t, authOpts{})
	cases := map[string]func(e *tsaEnv) string{
		"a wrong nonce":                    func(e *tsaEnv) string { e.issue.nonceOverride = big.NewInt(5); return e.bundle },
		"a wrong message imprint":          func(e *tsaEnv) string { e.issue.imprint = bytes.Repeat([]byte{1}, 32); return e.bundle },
		"a certificate outside the bundle": func(e *tsaEnv) string { return writeBundle(t, other.bundlePEM()) },
		"a token with no nonce":            func(e *tsaEnv) string { e.issue.dropNonce = true; return e.bundle },
		"a time far from the clock":        func(e *tsaEnv) string { e.issue.genTime = time.Now().Add(-6 * time.Hour); return e.bundle },
		"a hostile oversized answer": func(e *tsaEnv) string {
			e.handle = func(w http.ResponseWriter, _ *http.Request, _ *tsReq) {
				for i := 0; i < 64; i++ {
					if _, err := w.Write(bytes.Repeat([]byte{0x30}, 1<<20)); err != nil {
						return
					}
				}
			}
			return e.bundle
		},
		"a refusal": func(e *tsaEnv) string {
			e.handle = func(w http.ResponseWriter, _ *http.Request, _ *tsReq) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte("go away " + tsaUser + " " + tsaPass))
			}
			return e.bundle
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			e := newTSAEnv(t)
			bundle := setup(e)
			done := make(chan int64, 1)
			go func() { done <- e.oneRecord(bundle) }()
			var id int64
			select {
			case id = <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("the job hangs")
			}
			if r := e.row(id); r.State == anchor.StateAnchored {
				t.Fatalf("anchored: %+v", r)
			}
			_ = e.runMonitor()
			noSecret(t, name, e.allText(), tsaPass)
		})
	}
}

func TestASlowAuthorityCannotHoldAJob(t *testing.T) {
	e := newTSAEnv(t)
	release := make(chan struct{})
	defer close(release)
	e.handle = func(w http.ResponseWriter, r *http.Request, _ *tsReq) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}
	e.svc.cfg.SubmitTimeout = 300 * time.Millisecond
	e.add("slow", e.bundle)
	e.putAgg("shop", 3)
	_ = e.runEnqueue()
	start := time.Now()
	if err := e.runSubmit(); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("took %s", time.Since(start))
	}
	if r := e.rows()[0]; r.State != anchor.StatePending || r.Attempts != 1 {
		t.Fatalf("%+v", r)
	}
}

func TestATsaTargetNeedsABundleThatHoldsATrustAnchor(t *testing.T) {
	e := newTSAEnv(t)
	for name, cfg := range map[string][]byte{
		"no bundle":      mustRaw(map[string]any{"url": e.srv.URL}),
		"a missing file": e.config(filepath.Join(t.TempDir(), "none.pem")),
		"not a bundle":   e.config(writeBundle(t, []byte("nothing"))),
	} {
		if _, err := e.svc.CreateTarget(e.ctx(), "a", api.LedgerAnchorTargetCreate{Kind: "tsa", Name: "x", Config: cfg}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// a username without a password
	if _, err := e.svc.CreateTarget(e.ctx(), "a", api.LedgerAnchorTargetCreate{Kind: "tsa", Name: "y", Config: e.config(e.bundle), Credentials: mustRaw(map[string]string{"username": "u"})}); err == nil {
		t.Error("a username alone was accepted")
	}
}

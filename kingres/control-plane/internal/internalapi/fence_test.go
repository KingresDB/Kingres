// SPDX-License-Identifier: PostgreSQL

package internalapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fence"
)

// call makes GET /resources (a leader-only call) with the fence header (when not empty).
func (e *env) callFenced(c *http.Client, token string) (int, string) {
	req, _ := http.NewRequest("GET", e.srv.URL+Prefix+"/resources", nil)
	req.Header.Set(HeaderVersion, Version)
	if token != "" {
		req.Header.Set(fence.Header, token)
	}
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct{ Error string }
	_ = jsonDecode(resp, &out)
	return resp.StatusCode, out.Error
}

func TestSupersededRunIsRefused(t *testing.T) {
	e := newEnv(t)
	leaderC := cert(t, e.ca, "leader")
	peerC := cert(t, e.ca, "server2")
	lc, pc := e.client(&leaderC), e.client(&peerC)

	if st, _ := e.callFenced(lc, ""); st != 200 {
		t.Fatalf("a call without a fence must be unaffected: %d", st)
	}
	if st, _ := e.callFenced(lc, "9:2"); st != 200 {
		t.Fatalf("epoch 2: %d", st)
	}
	if st, _ := e.callFenced(lc, "9:2"); st != 200 {
		t.Fatalf("the same epoch again: %d", st)
	}
	if st, code := e.callFenced(lc, "9:1"); st != 409 || code != "fenced" {
		t.Fatalf("the late call of epoch 1 after epoch 2: %d %q", st, code)
	}
	if st, _ := e.callFenced(lc, "9:3"); st != 200 {
		t.Fatalf("a newer epoch: %d", st)
	}
	if st, code := e.callFenced(lc, "nonsense"); st != 400 || code != "bad_request" {
		t.Fatalf("a malformed token: %d %q", st, code)
	}
	if st, _ := e.callFenced(lc, ""); st != 200 {
		t.Fatalf("a call without a fence after fenced ones: %d", st)
	}
	// Only the leader's header counts: a peer cannot raise the epoch to block the leader,
	// its call is refused for what it is.
	if st, code := e.callFenced(pc, "9:99"); st != 403 || code != "not_leader" {
		t.Fatalf("a peer's fenced call: %d %q", st, code)
	}
	if st, _ := e.callFenced(lc, "9:3"); st != 200 {
		t.Fatalf("the leader's current epoch must still be accepted: %d", st)
	}
}

func jsonDecode(resp *http.Response, v any) error { return json.NewDecoder(resp.Body).Decode(v) }

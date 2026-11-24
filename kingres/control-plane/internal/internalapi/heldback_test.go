// SPDX-License-Identifier: AGPL-3.0-only

package internalapi

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeVerifier struct{ peer string }

func (f fakeVerifier) VerifyPeer(*x509.Certificate, time.Time) (string, error) { return f.peer, nil }

func heldBackRequest(s *Server, method, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, Prefix+path, strings.NewReader("{}"))
	r.Header.Set(HeaderVersion, Version)
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{}}}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

// The leader of a server that is held back: the three standby calls are accepted from the server the other members name
// as the leader, and refused with the usual 503 when they name nobody or another server; every other leader call of the
// same server stays 503. Without the policy the rebuild of an old primary, whose instance may not start before it is a
// standby, could never reach it.
func TestAHeldBackServerAcceptsTheStandbyCallsFromTheLeaderTheOthersName(t *testing.T) {
	named := "s1"
	handled := ""
	be := Backend{
		Leader:      func() string { return "" },
		Unavailable: func() bool { return true },
		PeerLeader:  func(context.Context) string { return named },
		Handle: func(_ context.Context, key, peer string, _ []byte) (any, error) {
			handled = key + " from " + peer
			return map[string]any{}, nil
		},
	}
	s := New(fakeVerifier{peer: "s1"}, be, nil, nil)
	for _, c := range []struct{ method, path string }{{http.MethodPost, "/standby/rebuild"}, {http.MethodPost, "/standby/plan"}, {http.MethodGet, "/standby/status"}} {
		handled = ""
		if w := heldBackRequest(s, c.method, c.path); w.Code != 200 || !strings.Contains(handled, c.path) {
			t.Errorf("%s %s from the leader the others name: %d %s (handled %q)", c.method, c.path, w.Code, w.Body.String(), handled)
		}
	}
	// another server than the one named
	s = New(fakeVerifier{peer: "s2"}, be, nil, nil)
	handled = ""
	if w := heldBackRequest(s, http.MethodPost, "/standby/rebuild"); w.Code != 503 || handled != "" {
		t.Errorf("a call of a server that is not the named leader: %d (handled %q)", w.Code, handled)
	}
	// nobody named
	named = ""
	s = New(fakeVerifier{peer: "s1"}, be, nil, nil)
	if w := heldBackRequest(s, http.MethodPost, "/standby/rebuild"); w.Code != 503 {
		t.Errorf("no leader named: %d", w.Code)
	}
	// a leader call that has no such policy stays 503 while held back, also from the named leader
	named = "s1"
	if w := heldBackRequest(s, http.MethodPost, "/instance/start"); w.Code != 503 || handled != "" {
		t.Errorf("another leader call of a held-back server: %d (handled %q)", w.Code, handled)
	}
	// a server that is not held back keeps the old rule: only the leader of its own catalog
	be.Unavailable = func() bool { return false }
	be.Leader = func() string { return "s9" }
	s = New(fakeVerifier{peer: "s1"}, be, nil, nil)
	if w := heldBackRequest(s, http.MethodPost, "/standby/rebuild"); w.Code != 403 {
		t.Errorf("a server that is not held back accepted a call of a server that is not its leader: %d", w.Code)
	}
}

// The calls that a switchover makes to the old primary's server after the promotion (the state, the end of the hold, the end of the freeze) are accepted
// from the leader the other members name while that server is held back: it forgot the leader it had verified, its instance cannot be read, and the
// end of the freeze must not wait for the instance that waits for the rebuild, which waits for the end of the freeze. A caller that is not the named
// leader, or nobody named, still gets the 503; the calls that stop or start an instance stay leader only.
func TestAHeldBackServerAcceptsTheCallsThatEndASwitchover(t *testing.T) {
	named := "s1"
	handled := ""
	be := Backend{
		Leader:      func() string { return "" },
		Unavailable: func() bool { return true },
		PeerLeader:  func(context.Context) string { return named },
		Handle: func(_ context.Context, key, peer string, _ []byte) (any, error) {
			handled = key + " from " + peer
			return map[string]any{}, nil
		},
	}
	s := New(fakeVerifier{peer: "s1"}, be, nil, nil)
	calls := []struct{ method, path string }{{http.MethodPost, "/failover/unfreeze"}, {http.MethodPost, "/failover/unquiesce"}, {http.MethodGet, "/failover/state"}}
	for _, c := range calls {
		handled = ""
		if w := heldBackRequest(s, c.method, c.path); w.Code != 200 || !strings.Contains(handled, c.path) {
			t.Errorf("%s %s from the leader the others name: %d %s (handled %q)", c.method, c.path, w.Code, w.Body.String(), handled)
		}
	}
	// another server than the one named, and nobody named
	for _, c := range calls {
		handled = ""
		if w := heldBackRequest(New(fakeVerifier{peer: "s2"}, be, nil, nil), c.method, c.path); w.Code != 503 || handled != "" {
			t.Errorf("%s %s from a server that is not the named leader: %d (handled %q)", c.method, c.path, w.Code, handled)
		}
	}
	named = ""
	for _, c := range calls {
		if w := heldBackRequest(s, c.method, c.path); w.Code != 503 {
			t.Errorf("%s %s with no leader named: %d", c.method, c.path, w.Code)
		}
	}
	// the calls that stop, promote or hand over an instance are not among them
	named = "s1"
	for _, p := range []string{"/failover/demote", "/failover/promote", "/failover/quiesce", "/failover/freeze"} {
		handled = ""
		if w := heldBackRequest(s, http.MethodPost, p); w.Code != 503 || handled != "" {
			t.Errorf("POST %s of a held-back server: %d (handled %q)", p, w.Code, handled)
		}
	}
	// a server that is not held back keeps the rule: only the leader of its own catalog
	be.Unavailable = func() bool { return false }
	be.Leader = func() string { return "s9" }
	for _, c := range calls {
		if w := heldBackRequest(New(fakeVerifier{peer: "s1"}, be, nil, nil), c.method, c.path); w.Code != 403 {
			t.Errorf("%s %s: a server that is not held back accepted a call of a server that is not its leader: %d", c.method, c.path, w.Code)
		}
	}
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package internalapi

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
)

// TestTableOfMembershipCalls pins the authorization table of the calls that
// join, add and remove servers: nothing may be added to it without a decision
// about who may call.
func TestTableOfMembershipCalls(t *testing.T) {
	want := map[string]Policy{
		"POST /bootstrap/prepare": Bootstrap, "POST /bootstrap/install": Bootstrap, "POST /bootstrap/join": Bootstrap,
		"POST /add-server": ToLeader, "GET /instance/status": LeaderOrJoining, "POST /instance/create": LeaderOrJoining,
		"POST /instance/start": LeaderOnly, "POST /instance/stop": LeaderOnly, "POST /instance/remove": LeaderOnly,
		"POST /instance/trust": LeaderOrJoining, "POST /pki/install": LeaderOrJoining, "POST /peers": LeaderOrJoining,
		"POST /globals/apply": LeaderOrJoining, "POST /node/finish": LeaderOrJoining, "POST /node/reset": LeaderOrJoining,
		"POST /apply": LeaderOnly,
	}
	for k, p := range want {
		if got, ok := Calls[k]; !ok || got != p {
			t.Errorf("%s: policy %v (present %v), want %v", k, got, ok, p)
		}
	}
	// Every call that is not a Bootstrap call needs a certificate, and only
	// the three bootstrap calls do without.
	n := 0
	for k, p := range Calls {
		if p == Bootstrap {
			n++
			if k != "POST /bootstrap/prepare" && k != "POST /bootstrap/install" && k != "POST /bootstrap/join" {
				t.Errorf("%s is a call without a certificate", k)
			}
		}
	}
	if n != 3 {
		t.Errorf("%d bootstrap calls", n)
	}
	// Nothing takes a path or SQL: the body types of the dispatched calls are fixed structs
	// (see package wire); the table has no call named like exec except the leader-only /exec.
	for k := range Calls {
		if k != "POST /exec" && len(k) > 5 && (k[len(k)-4:] == "exec" || k[len(k)-3:] == "sql") {
			t.Errorf("%s looks like a call that runs what it is given", k)
		}
	}
}

func membersServer(t *testing.T, joining bool, leader string) (*httptest.Server, *pki.CA, *[]string) {
	t.Helper()
	ca, err := pki.CreateCA(filepath.Join(t.TempDir(), "pki"), "t", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	be := Backend{
		Self: "me", Leader: func() string { return leader }, Revoked: ca.Revoked,
		Joining: func() bool { return joining },
		Handle: func(_ context.Context, key, peer string, _ []byte) (any, error) {
			seen = append(seen, key+"@"+peer)
			return map[string]bool{"ok": true}, nil
		},
		Bootstrap: func(_ context.Context, key string, r *http.Request, _ []byte) (any, error) {
			seen = append(seen, key+"@bootstrap:"+r.Header.Get("X-Setup-Token"))
			return map[string]bool{"ok": true}, nil
		},
	}
	s := httptest.NewUnstartedServer(New(ca, be, nil, nil))
	s.TLS = TLSConfig(cert(t, ca, "leader"))
	s.StartTLS()
	t.Cleanup(s.Close)
	return s, ca, &seen
}

func call(t *testing.T, s *httptest.Server, ca *pki.CA, pair *tls.Certificate, method, path string) int {
	t.Helper()
	e := &env{srv: s, ca: ca, t: t}
	st, _, err := e.do(e.client(pair), method, path, Version, map[string]any{})
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return st
}

func TestBootstrapCallsNeedNoCertificateOthersDo(t *testing.T) {
	s, ca, seen := membersServer(t, false, "leader")
	if st := call(t, s, ca, nil, "POST", "/bootstrap/prepare"); st != 200 {
		t.Errorf("bootstrap without a certificate: %d", st)
	}
	for _, p := range []string{"/instance/create", "/globals/apply", "/peers", "/add-server"} {
		if st := call(t, s, ca, nil, "POST", p); st != 401 {
			t.Errorf("%s without a certificate: %d", p, st)
		}
	}
	if len(*seen) != 1 {
		t.Errorf("the backend saw %v", *seen)
	}
}

func TestJoiningServerAcceptsAnyPeerClusterServerOnlyTheLeader(t *testing.T) {
	// While joining there is no catalog, so no leader is known: any server with a
	// certificate of the cluster may make the joining calls (and only those).
	s, ca, seen := membersServer(t, true, "")
	peer := cert(t, ca, "db1")
	for _, p := range []string{"/instance/create", "/pki/install", "/peers", "/globals/apply", "/node/finish", "/instance/trust", "/node/reset"} {
		if st := call(t, s, ca, &peer, "POST", p); st != 200 {
			t.Errorf("joining server, %s: %d", p, st)
		}
	}
	for _, p := range []string{"/instance/stop", "/instance/remove", "/apply"} {
		if st := call(t, s, ca, &peer, "POST", p); st != 403 {
			t.Errorf("joining server, leader-only %s: %d", p, st)
		}
	}
	if len(*seen) != 7 {
		t.Errorf("handled %v", *seen)
	}

	// A member (not joining) answers the same calls only to the leader.
	s2, ca2, _ := membersServer(t, false, "db1")
	leader, other := cert(t, ca2, "db1"), cert(t, ca2, "db2")
	if st := call(t, s2, ca2, &leader, "POST", "/instance/create"); st != 200 {
		t.Errorf("leader: %d", st)
	}
	if st := call(t, s2, ca2, &other, "POST", "/instance/create"); st != 403 {
		t.Errorf("a member that is not the leader: %d", st)
	}
	// With no known leader nothing leader-only is accepted at all.
	s3, ca3, _ := membersServer(t, false, "")
	any1 := cert(t, ca3, "db1")
	if st := call(t, s3, ca3, &any1, "POST", "/apply"); st != 403 {
		t.Errorf("no leader known: %d", st)
	}
}

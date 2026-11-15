// SPDX-License-Identifier: PostgreSQL

package clustertest_test

import (
	"crypto/tls"
	"net/http"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// fencedCall makes the leader-only call GET /resources on node as the leader would
// from a step of the task run named by token.
func fencedCall(t *testing.T, node *tls.Certificate, addr, token string) (int, string) {
	t.Helper()
	cfg := &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{*node}} //nolint:gosec // the server under test, on loopback
	hc := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}
	defer hc.CloseIdleConnections()
	req, _ := http.NewRequest("GET", "https://"+addr+"/internal/v1/resources", nil)
	req.Header.Set("X-Kingres-Internal-Version", "1")
	req.Header.Set("X-Kingres-Fence", token)
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	var b [512]byte
	n, _ := resp.Body.Read(b[:])
	return resp.StatusCode, string(b[:n])
}

// TestSupersededTaskRunIsRefusedByTheCallee: the leader's run of a task at epoch 2
// touched a follower; a late call of the same task at epoch 1 (a leader that was paused,
// whose lease another run took over) is refused with 409 fenced, also after the follower's
// kingresd was restarted (the epoch is kept on disk).
func TestSupersededTaskRunIsRefusedByTheCallee(t *testing.T) {
	// Its own cluster: the test restarts a server, which would end the sessions of the shared one.
	c := clustertest.Form(t, 3)
	leader, follower := c.Leader(), c.Nodes[1]
	lp := loadPair(t, leader)
	const task = "900001" // a task id no real task has
	if st, body := fencedCall(t, lp, follower.InternalAddr(), task+":2"); st != 200 {
		t.Fatalf("epoch 2: %d %s", st, body)
	}
	if st, body := fencedCall(t, lp, follower.InternalAddr(), task+":2"); st != 200 {
		t.Fatalf("epoch 2 again: %d %s", st, body)
	}
	if st, body := fencedCall(t, lp, follower.InternalAddr(), task+":1"); st != 409 {
		t.Fatalf("the late call of epoch 1: %d %s, want 409 fenced", st, body)
	}
	// The other follower never saw epoch 2: the rule is per callee (documented limit).
	if st, body := fencedCall(t, lp, c.Nodes[2].InternalAddr(), task+":1"); st != 200 {
		t.Fatalf("a callee that saw nothing newer accepts epoch 1: %d %s", st, body)
	}
	follower.Stop()
	follower.Start()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		st, body := fencedCall(t, lp, follower.InternalAddr(), task+":1")
		if st == 409 {
			break
		}
		if st == 200 {
			t.Fatalf("the restarted callee forgot epoch 2 and accepted epoch 1: %s", body)
		}
		if time.Now().After(deadline) {
			t.Fatalf("after a restart the late call of epoch 1 got %d %s, want 409 fenced", st, body)
		}
		time.Sleep(time.Second)
	}
}

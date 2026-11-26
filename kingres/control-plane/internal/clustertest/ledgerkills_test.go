// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package clustertest_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fault"
)

// checkPublishedState compares the record of what was published (published.json in the key directory of the leader, correction 171) with the system
// database: every log of the record has a checkpoint of that size with that root, and the events are at least as many, the last of the record being
// the one the database holds.
func checkPublishedState(t *testing.T, n *clustertest.Node) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(n.DataDir, "ledger", "keys", "published.json"))
	if err != nil {
		t.Fatalf("the record of what was published: %v", err)
	}
	var st struct {
		Logs map[string]struct {
			Size int64
			Root string
		} `json:"logs"`
		Events    int64  `json:"events"`
		EventHash string `json:"eventHash"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c := n.ServicePG("kingres_system")
	defer func() { _ = c.Close(ctx) }()
	for origin, m := range st.Logs {
		var root []byte
		if err := c.QueryRow(ctx, `SELECT k.root FROM kingres_cp.ledger_checkpoint k JOIN kingres_cp.ledger_log l USING (log_id) WHERE l.origin = $1 AND k.size = $2`, origin, m.Size).Scan(&root); err != nil {
			t.Errorf("the record has size %d of %s and the database has no such checkpoint: %v", m.Size, origin, err)
			continue
		}
		if hex.EncodeToString(root) != m.Root {
			t.Errorf("size %d of %s: the record and the database disagree on the root", m.Size, origin)
		}
	}
	var count int64
	if err := c.QueryRow(ctx, `SELECT count(*) FROM kingres_cp.ledger_event`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count < st.Events {
		t.Errorf("the record has %d events and the database %d", st.Events, count)
	}
	if st.Events > 0 {
		var note []byte
		if err := c.QueryRow(ctx, `SELECT note FROM kingres_cp.ledger_event WHERE seq = $1`, st.Events-1).Scan(&note); err != nil {
			t.Fatalf("event %d: %v", st.Events-1, err)
		}
		h := sha256.Sum256(note)
		if hex.EncodeToString(h[:]) != st.EventHash {
			t.Errorf("the last event of the record is not the one of the database")
		}
	}
}

// killAt ends the leader at the first write of a kind that the trigger causes, restarts it and waits until it answers.
func killAt(t *testing.T, leader *clustertest.Node, kind string, trigger func()) {
	t.Helper()
	killWith(t, leader, fault.Spec{LedgerExitKind: kind, LedgerExitSkip: 0}, kind, trigger)
}

// killWith is killAt with a fault spec of its own (a cut of the network that stays in force while the leader waits for the write).
func killWith(t *testing.T, leader *clustertest.Node, spec fault.Spec, kind string, trigger func(), afterExit ...func()) {
	t.Helper()
	leader.SetFaults(spec)
	trigger()
	if !leader.WaitExit(4 * time.Minute) {
		t.Fatalf("the leader was not killed at a write of kind %s in four minutes; log tail: %s", kind, leader.LogTail(4000))
	}
	leader.SetFaults(fault.Spec{})
	for _, f := range afterExit {
		f()
	}
	os.Remove(leader.FaultDir() + "/" + fault.FileName)
	leader.Start()
	until(t, 3*time.Minute, "the restarted leader accepts a sign-in", func() bool { return leader.Login(clustertest.AdminUser, clustertest.AdminPass) })
	until(t, 3*time.Minute, "the leader to answer again", func() bool {
		st, _ := leader.Do("GET", "/ledger/publication", nil, nil)
		return st == 200
	})
}

// primaryInstanceOf is the id of the ordinary (not map group) primary instance of a server, which holds one shard group.
func primaryInstanceOf(t *testing.T, n *clustertest.Node) string {
	t.Helper()
	out := n.Expect(200, "GET", "/this-server/instances", nil)
	items, _ := out["items"].([]any)
	for _, it := range items {
		m := it.(map[string]any)
		if m["isMapGroup"] != true && m["role"] == "primary" {
			return fmt.Sprint(int64(m["id"].(float64)))
		}
	}
	t.Fatalf("node %d: no primary instance in %v", n.Index, out)
	return ""
}

// afterRestart is what must hold after a kill and a restart: no log is stopped that was not stopped on purpose, the stored logs audit against their tiles,
// the standalone verifier accepts every log, and the record and the database agree.
func afterRestart(t *testing.T, p *pubCluster, kind string, allowClosed map[string]bool) {
	t.Helper()
	leader := p.c.Nodes[0]
	until(t, 3*time.Minute, "the publication to settle after the kill at "+kind, func() bool {
		for _, l := range publicationLogs(t, leader) {
			if l.State == "diverged" {
				t.Fatalf("after a kill at a write of kind %s the log %s is diverged", kind, l.Origin)
			}
		}
		return true
	})
	auditStored(t, leader)
	for _, l := range publicationLogs(t, leader) {
		if l.State == "closed" && !allowClosed[l.Kind] {
			t.Fatalf("after a kill at %s the log %s is closed", kind, l.Origin)
		}
		verifyPublication(t, p, 0, logPath(l.Origin), uint64(l.Size))
	}
	checkPublishedState(t, leader)
}

// P8-09: a kill of the leader at each kind of write that a round or a key task makes, each followed by a restart and the checks above. The kinds log,
// digest, unread, state, retired, note and event are each caused by the act that makes that write (a new database, a digest, a group cut off, a drop, a
// rotation of the key); the kinds tile, frontier, checkpoint and pending are those of TestLedgerRoundSurvivesAKillAtEachKindOfWrite.
func TestLedgerSurvivesAKillAtEveryOtherKindOfWrite(t *testing.T) {
	t.Setenv("KINGRES_LEDGER_MISSING_FOR", "20s")
	p := formPublishing(t, "kill_a", "kill_state", "kill_retired", "kill_late")
	leader, follower := p.c.Nodes[0], p.c.Nodes[1]
	until(t, 90*time.Second, "both servers to be listed", func() bool { return len(leader.Servers()) == 2 })
	for _, db := range []string{"kill_a", "kill_state", "kill_retired"} {
		ledgerTable(t, leader, db, "t0", 2)
		ledgerTable(t, follower, db, "t1", 2)
	}
	p.startPublishing(t)
	until(t, 3*time.Minute, "the first publication of every database", func() bool {
		n := 0
		for _, l := range publicationLogs(t, leader) {
			if l.Kind == "aggregate" {
				n++
			}
		}
		return n == 3
	})

	// digest: the record of a digest of the database
	killAt(t, leader, "digest", func() { leader.Do("POST", "/databases/kill_a/ledger/digests", nil, nil) })
	afterRestart(t, p, "digest", nil)
	if st, _ := leader.Do("POST", "/databases/kill_a/ledger/digests", nil, nil); st != 201 {
		t.Fatalf("a digest after the kill: %d", st)
	}

	// log: a database that gets its ledger table now has chain logs made
	killAt(t, leader, "log", func() {
		ledgerTable(t, leader, "kill_late", "t0", 2)
		ledgerTable(t, follower, "kill_late", "t1", 2)
	})
	afterRestart(t, p, "log", nil)
	until(t, 3*time.Minute, "the logs of the new database", func() bool {
		n := 0
		for _, l := range publicationLogs(t, leader) {
			if l.Kind == "chain" {
				n++
			}
		}
		return n == 8
	})

	// unread: the follower's group is cut off; the leader notes since when
	fid := primaryInstanceOf(t, follower)
	killAt(t, leader, "unread", func() {
		st, out := follower.Do("POST", "/this-server/instances/"+fid+"/stop", nil, nil)
		if st == 409 {
			follower.Do("POST", "/this-server/instances/"+fid+"/stop", map[string]any{"force": true}, nil)
		}
		_ = out
	})
	// the stop is a task: a start while it still runs is refused (409), so the start is asked for again until the instance has stopped, was
	// started, and runs
	started := false
	until(t, 4*time.Minute, "the instance of the follower to be started again and to run", func() bool {
		_, out := follower.Do("GET", "/this-server/instances", nil, nil)
		state := ""
		for _, it := range anyList(out["items"]) {
			if m := asMap(it); fmt.Sprint(int64(m["id"].(float64))) == fid {
				state, _ = m["state"].(string)
			}
		}
		if state == "stopped" {
			st, _ := follower.Do("POST", "/this-server/instances/"+fid+"/start", nil, nil)
			started = started || st == 202
			return false
		}
		return started && state == "running"
	})
	var lastSt int
	var lastBody map[string]any
	until(t, 4*time.Minute, "the group to be read again", func() bool {
		lastSt, lastBody = leader.Do("POST", "/databases/kill_a/ledger/digests", nil, nil)
		return lastSt == 201 && lastBody["complete"] == true
	})
	t.Logf("the digest after the restart: %d %v", lastSt, lastBody)
	afterRestart(t, p, "unread", nil)

	// state: a drop closes the logs of a database
	killAt(t, leader, "state", func() { follower.Do("DELETE", "/databases/kill_state", nil, nil) })
	until(t, 4*time.Minute, "the drop of kill_state to be finished", func() bool {
		st, _ := follower.Do("GET", "/databases/kill_state", nil, nil)
		return st == 404
	})
	afterRestart(t, p, "state", map[string]bool{"chain": true})

	// retired: the aggregate round that records in which leaf a closed chain was retired
	killAt(t, leader, "retired", func() { follower.Do("DELETE", "/databases/kill_retired", nil, nil) })
	until(t, 4*time.Minute, "the drop of kill_retired to be finished", func() bool {
		st, _ := follower.Do("GET", "/databases/kill_retired", nil, nil)
		return st == 404
	})
	afterRestart(t, p, "retired", map[string]bool{"chain": true})

	// event: a rotation of the key appends the event of the new key; note: the re-signing of the latest checkpoints replaces their notes
	killAt(t, leader, "event", func() { leader.Do("POST", "/ledger/signing/keys", map[string]any{"overlapDays": 1}, nil) })
	until(t, 3*time.Minute, "the key task to be finished after the restart", func() bool {
		st, b := leader.Do("GET", "/ledger/signing", nil, nil)
		return st == 200 && len(anyList(b["keys"])) == 2
	})
	afterRestart(t, p, "event", map[string]bool{"chain": true})
	killAt(t, leader, "note", func() { leader.Do("POST", "/ledger/signing/keys", map[string]any{"overlapDays": 1}, nil) })
	until(t, 3*time.Minute, "the key task to be finished after the second restart", func() bool {
		st, b := leader.Do("GET", "/ledger/signing", nil, nil)
		return st == 200 && len(anyList(b["keys"])) >= 2
	})
	afterRestart(t, p, "note", map[string]bool{"chain": true})
	_ = strings.TrimSpace
}

// A follower forwards a request to the leader whatever it holds: a hostile body (truncated, oversized, an unknown member, wrong types) with credentials in
// it makes an error answer that holds none of them, and the credentials are in no log of either server (item 3 of the round).
func TestAFollowerThatForwardsAHostileBodyLeaksNoCredential(t *testing.T) {
	p := formPublishing(t)
	leader, follower := p.c.Nodes[0], p.c.Nodes[1]
	until(t, 90*time.Second, "both servers to be listed", func() bool { return len(leader.Servers()) == 2 })
	const key, secret = "AKIDHOSTILEKEY0123", "h0stile/secret+value=with&chars"
	cred := `{"accessKeyId":"` + key + `","secretAccessKey":"` + secret + `"}`
	bodies := []string{
		`{"kind":"worm","name":"v","config":{"bucket":"b"},"credentials":{"secretAccessKey":"` + secret,               // truncated
		`{"kind":"worm","name":"v","config":{},"credentials":` + cred + `,"extra":"` + secret + `"}`,                  // an unknown member
		`{"kind":5,"name":"v","config":{},"credentials":` + cred + `}`,                                                // a wrong type
		`{"kind":"worm","name":"v","config":{},"credentials":` + cred + `} trailing`,                                  // data after the document
		`{"kind":"worm","name":"v","config":{"pad":"` + strings.Repeat("x", 3<<20) + `"},"credentials":` + cred + `}`, // oversized
		`{"kind":"worm","name":"v","config":{"endpoint":"http://` + secret + `@host"},"credentials":` + cred + `}`,    // credentials in the config
	}
	var seen strings.Builder
	for _, b := range bodies {
		st, answer := follower.DoRaw("POST", "/ledger/anchor-targets", []byte(b))
		if (st < 400 && st != 0) || st >= 500 {
			t.Errorf("a hostile body: %d %s", st, answer)
		}
		seen.Write(answer)
	}
	for _, n := range []*clustertest.Node{leader, follower} {
		if b, err := os.ReadFile(n.LogFile); err == nil {
			seen.Write(b)
		}
	}
	au := leader.Expect(200, "GET", "/audit?limit=200", nil)
	raw, _ := json.Marshal(au)
	seen.Write(raw)
	for _, s := range []string{key, secret, hex.EncodeToString([]byte(secret)), strings.ReplaceAll(strings.ReplaceAll(secret, "/", "%2F"), "+", "%2B")} {
		if strings.Contains(seen.String(), s) {
			t.Errorf("a credential (%d characters) is in an answer, a log or the audit", len(s))
		}
	}
	// and a good body through the follower works
	leader.Expect(201, "POST", "/ledger/anchor-targets", map[string]any{"kind": "worm", "name": "good", "config": map[string]any{"endpoint": "http://127.0.0.1:9", "region": "r", "bucket": "bucket-1", "allowInsecure": true}, "credentials": map[string]any{"accessKeyId": key, "secretAccessKey": secret}})
}

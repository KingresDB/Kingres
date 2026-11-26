// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package clustertest_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerkeys"
)

// The signing keys of the ledger on real servers (design ledger-phase7.md, steps 3 and 4): kingresd processes and
// PostgreSQL instances, the REST API on both servers (the follower forwards to the manager leader), the key files
// on the servers that run an instance of shard group 0, the events and the trust file, the term of the leader.

func keyFile(n *clustertest.Node, fp string) string {
	return filepath.Join(n.DataDir, "ledger", "keys", "signing", fp+".key")
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }

// signingKeys returns the keys of getLedgerSigning by fingerprint.
func signingKeys(t *testing.T, n *clustertest.Node) (map[string]map[string]any, map[string]any) {
	t.Helper()
	body := n.Expect(200, "GET", "/ledger/signing", nil)
	out := map[string]map[string]any{}
	keys, _ := body["keys"].([]any)
	for _, k := range keys {
		m := asMap(k)
		out[m["fingerprint"].(string)] = m
	}
	return out, body
}

// ledgerTerm reads the term the leader's scheduler holds, from the database.
func ledgerTerm(t *testing.T, n *clustertest.Node) int64 {
	t.Helper()
	var term int64
	if err := n.ServicePG("kingres_system").QueryRow(context.Background(), `SELECT term FROM kingres_cp.ledger_cluster`).Scan(&term); err != nil {
		t.Fatal(err)
	}
	return term
}

// startKeyTask posts a key operation and waits for the task; a 503 (the scheduler of a new leader waits one lease
// margin before it takes its term) is tried again.
func startKeyTask(t *testing.T, via *clustertest.Node, path string, body any) map[string]any {
	t.Helper()
	var task map[string]any
	until(t, 90*time.Second, "the key operation "+path+" to be accepted", func() bool {
		st, b := via.Do("POST", path, body, nil)
		if st == 503 {
			return false
		}
		if st != 202 {
			t.Fatalf("POST %s: %d %v", path, st, b)
		}
		task = b
		return true
	})
	return via.WaitTask(clustertest.TaskID(task), 3*time.Minute)
}

func TestLedgerSigningKeysAreMadeDistributedRotatedAndRevokedAcrossTwoServers(t *testing.T) {
	c := formStandbys(t, 2, 1)
	leader, follower := c.Nodes[0], c.Nodes[1]
	until(t, 2*time.Minute, "both servers to run an instance of shard group 0", func() bool {
		var n int
		err := leader.ServicePG("kingres_system").QueryRow(context.Background(), `SELECT count(DISTINCT host) FROM kingres.node WHERE group_id = 0 AND is_active`).Scan(&n)
		return err == nil && n == 2
	})

	// Before the first key: nothing to sign with, no cluster id, no trust file; the follower asks the leader.
	_, view := signingKeys(t, follower)
	if view["clusterId"] != nil || len(view["keys"].([]any)) != 0 || view["mode"] != "file" || view["passphraseProtected"] != false {
		t.Fatalf("the signing view before any key: %v", view)
	}
	if st, b := follower.Do("GET", "/ledger/signing/trust-file", nil, nil); st != 409 || b["code"] != "signing_key_missing" {
		t.Fatalf("the trust file before any key: %d %v", st, b)
	}
	settings := follower.Expect(200, "GET", "/ledger/settings", nil)
	if pub := asMap(settings["publication"]); pub["originPrefixLocked"] != false || pub["enabled"] != false || !strings.HasSuffix(pub["originPrefix"].(string), "/kingres-ledger") {
		t.Fatalf("the settings before any key: %v", settings)
	}

	// The first key, started on the follower: the leader makes it, gives it to the other candidate, announces and
	// activates it.
	done := startKeyTask(t, follower, "/ledger/signing/keys", nil)
	if done["state"] != "done" {
		t.Fatalf("the creation: %v\n%s", done["error"], c.Dump())
	}
	if s := asMap(done["subject"]); s["type"] != "ledgerKey" || s["id"] != "signing" || done["kind"] != "ledger.key.create" {
		t.Errorf("the task: kind %v subject %v", done["kind"], done["subject"])
	}
	keys, view := signingKeys(t, follower)
	if len(keys) != 1 {
		t.Fatalf("%d keys after the creation: %v", len(keys), view)
	}
	var fp1 string
	for fp := range keys {
		fp1 = fp
	}
	k1 := keys[fp1]
	if k1["state"] != "active" || view["backupAdvised"] != true || len(view["events"].([]any)) != 1 || view["signerAlarm"] != nil || len(view["clusterId"].(string)) != 64 {
		t.Fatalf("the signing view after the creation: %v", view)
	}
	holders := fmt.Sprint(view["holders"])
	if holders != fmt.Sprintf("[%s %s]", leader.Host, follower.Host) {
		t.Errorf("holders %s", holders)
	}
	// The private key is a file of mode 0600 on both servers, in the format of the key library, and it is the key
	// whose public part the API shows (this is what a new leader signs with).
	for _, n := range c.Nodes {
		p := keyFile(n, fp1)
		st, err := os.Stat(p)
		if err != nil {
			t.Fatalf("node %d has no key file: %v", n.Index, err)
		}
		if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
			t.Errorf("node %d: the key file has the mode %v", n.Index, st.Mode().Perm())
		}
		s, err := ledgerkeys.ReadNamed(filepath.Dir(p), fp1, ledgerkeys.Passphrase{})
		if err != nil {
			t.Fatalf("node %d: %v", n.Index, err)
		}
		if base64.StdEncoding.EncodeToString(s.Public()) != k1["publicKey"] {
			t.Errorf("node %d holds another key than the one the API shows", n.Index)
		}
		_ = s.Close()
	}
	// The trust file through the follower: the headers survive the forwarding, the verifier reads it.
	st, trust, hdr := follower.DoH("GET", "/ledger/signing/trust-file", nil, nil)
	if st != 200 || hdr.Get("Content-Disposition") != `attachment; filename="trust.json"` || hdr.Get("Content-Type") != "application/json" {
		t.Fatalf("the trust file: %d %v %v", st, hdr, trust)
	}
	tb, _ := json.MarshalIndent(trust, "", "  ")
	if keysT, _ := trust["keys"].([]any); len(keysT) != 1 || asMap(keysT[0])["fingerprint"] != fp1 {
		t.Errorf("the trust file: %s", tb)
	}
	if bin := os.Getenv("KINGRES_VERIFY_BIN"); bin != "" {
		tf := filepath.Join(c.Root, "trust.json")
		if err := os.WriteFile(tf, append(tb, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(bin, "trust", "show", "--trust", tf).CombinedOutput() //nolint:gosec // the verifier under test
		if err != nil || !strings.Contains(string(out), fp1[:16]) {
			t.Errorf("kingres-verify trust show: %v\n%s", err, out)
		}
	}

	// What must be nowhere: the seed. It is in the key files only.
	seeds := map[string]string{}
	for _, n := range c.Nodes {
		data, _ := os.ReadFile(keyFile(n, fp1))
		if i := strings.IndexByte(string(data), '\n'); i >= 0 {
			var j struct {
				Seed string `json:"seed"`
			}
			if json.Unmarshal(data[i+1:], &j) == nil && j.Seed != "" {
				seeds[fmt.Sprintf("node %d", n.Index)] = j.Seed
			}
		}
	}
	if len(seeds) != 2 {
		t.Fatalf("the seeds could not be read from the key files: %v", seeds)
	}
	leaks := func(what, text string) {
		for who, seed := range seeds {
			if strings.Contains(text, seed) || strings.Contains(text, strings.ReplaceAll(seed, "/", `\/`)) {
				t.Errorf("%s holds the seed of %s", what, who)
			}
		}
	}
	for _, n := range c.Nodes {
		leaks(fmt.Sprintf("the log of node %d", n.Index), n.Log())
		if b, err := os.ReadFile(filepath.Join(n.DataDir, "audit.jsonl")); err == nil {
			leaks(fmt.Sprintf("the audit file of node %d", n.Index), string(b))
		}
	}
	tasks, _ := json.Marshal(follower.Expect(200, "GET", "/tasks", nil))
	leaks("the task list", string(tasks))
	vb, _ := json.Marshal(view)
	leaks("the signing view", string(vb))
	leaks("the trust file", string(tb))
	audit, _ := json.Marshal(follower.Expect(200, "GET", "/audit?limit=200", nil))
	leaks("the audit list", string(audit))
	if !strings.Contains(string(audit), "createLedgerSigningKey") {
		t.Errorf("the creation is not in the audit log: %s", audit)
	}
	rows, err := leader.ServicePG("kingres_system").Query(context.Background(), `SELECT t::text FROM (SELECT * FROM kingres_cp.ledger_cluster) t
		UNION ALL SELECT t::text FROM (SELECT * FROM kingres_cp.ledger_signing_key) t UNION ALL SELECT t::text FROM (SELECT * FROM kingres_cp.ledger_event) t
		UNION ALL SELECT t::text FROM (SELECT * FROM kingres_cp.task) t`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		leaks("a row of kingres_cp", s)
	}
	rows.Close()

	// The settings: with a key made, the origin prefix is part of a signed event and cannot change.
	if st, b := follower.Do("PUT", "/ledger/settings", map[string]any{"publication": map[string]any{"originPrefix": "other/prefix"}}, nil); st != 409 || b["code"] != "origin_prefix_locked" {
		t.Errorf("changing the locked prefix: %d %v", st, b)
	}
	if st, b := follower.Do("PUT", "/ledger/settings", map[string]any{"publication": map[string]any{"intervalSeconds": 5}}, nil); st != 400 || b["code"] != "validation_failed" {
		t.Errorf("an interval out of range: %d %v", st, b)
	}
	got := follower.Expect(200, "PUT", "/ledger/settings", map[string]any{"publication": map[string]any{"intervalSeconds": 90}, "anchor": map[string]any{"required": true}})
	if asMap(got["publication"])["intervalSeconds"] != float64(90) || asMap(got["anchor"])["required"] != true || asMap(got["publication"])["originPrefixLocked"] != true {
		t.Errorf("the settings after the update: %v", got)
	}

	// The last key can be neither retired nor revoked.
	if st, b := follower.Do("POST", "/ledger/signing/keys/"+fp1+"/retire", map[string]any{}, nil); st != 409 || b["code"] != "last_signing_key" {
		t.Errorf("retiring the last key: %d %v", st, b)
	}
	if st, b := follower.Do("POST", "/ledger/signing/keys/"+fp1+"/revoke", map[string]any{"compromisedSince": time.Now().UTC().Format(time.RFC3339), "reason": "x", "confirm": fp1}, nil); st != 409 || b["code"] != "last_signing_key" {
		t.Errorf("revoking the last key: %d %v", st, b)
	}

	// A rotation: the second key is on both servers, announced by an event signed by both keys, and both sign for the
	// overlap; then the first key is retired and its file leaves both servers.
	done = startKeyTask(t, leader, "/ledger/signing/keys", map[string]any{"overlapDays": 1})
	if done["state"] != "done" || done["kind"] != "ledger.key.rotate" {
		t.Fatalf("the rotation: %v %v", done["kind"], done["error"])
	}
	keys, view = signingKeys(t, follower)
	var fp2 string
	for fp := range keys {
		if fp != fp1 {
			fp2 = fp
		}
	}
	if len(keys) != 2 || keys[fp2]["state"] != "active" || keys[fp1]["state"] != "active" || len(view["events"].([]any)) != 2 {
		t.Fatalf("after the rotation: %v", view)
	}
	for _, n := range c.Nodes {
		if !fileExists(keyFile(n, fp1)) || !fileExists(keyFile(n, fp2)) {
			t.Errorf("node %d lacks a key file during the overlap", n.Index)
		}
	}
	done = startKeyTask(t, follower, "/ledger/signing/keys/"+fp1+"/retire", map[string]any{})
	if done["state"] != "done" {
		t.Fatalf("the retirement: %v", done["error"])
	}
	keys, view = signingKeys(t, leader)
	if keys[fp1]["state"] != "retired" || keys[fp1]["maxTreeSize"] != float64(1) || keys[fp2]["state"] != "active" || len(view["events"].([]any)) != 3 {
		t.Fatalf("after the retirement: %v", view)
	}
	for _, n := range c.Nodes {
		if fileExists(keyFile(n, fp1)) || !fileExists(keyFile(n, fp2)) {
			t.Errorf("node %d: the file of the retired key is %v, the file of the active key is %v", n.Index, fileExists(keyFile(n, fp1)), fileExists(keyFile(n, fp2)))
		}
	}

	// A third key, then the revocation of the second: another active key signs the event, the file of the revoked
	// key leaves both servers.
	done = startKeyTask(t, leader, "/ledger/signing/keys", map[string]any{"overlapDays": 1})
	if done["state"] != "done" {
		t.Fatalf("the third key: %v", done["error"])
	}
	if st, b := follower.Do("POST", "/ledger/signing/keys/"+fp2+"/revoke", map[string]any{"compromisedSince": time.Now().UTC().Format(time.RFC3339), "reason": "test", "confirm": "not the fingerprint"}, nil); st != 400 || b["code"] != "validation_failed" {
		t.Errorf("a wrong confirmation: %d %v", st, b)
	}
	done = startKeyTask(t, follower, "/ledger/signing/keys/"+fp2+"/revoke", map[string]any{"compromisedSince": time.Now().UTC().Add(-time.Minute).Format(time.RFC3339), "reason": "revoked by the cluster test", "confirm": fp2})
	if done["state"] != "done" {
		t.Fatalf("the revocation: %v", done["error"])
	}
	keys, view = signingKeys(t, follower)
	if keys[fp2]["state"] != "revoked" || keys[fp2]["revokedAt"] == nil || len(view["events"].([]any)) != 5 {
		t.Fatalf("after the revocation: %v", view)
	}
	for _, n := range c.Nodes {
		if fileExists(keyFile(n, fp2)) {
			t.Errorf("node %d still has the file of the revoked key", n.Index)
		}
	}
	for _, n := range c.Nodes {
		leaks(fmt.Sprintf("the log of node %d after the whole life of the keys", n.Index), n.Log())
	}

	// A leader that is killed and started again takes a new term and signs with the keys it finds.
	before := ledgerTerm(t, leader)
	leader.Kill()
	leader.Start()
	if !leader.Login(clustertest.AdminUser, clustertest.AdminPass) {
		t.Fatal("the administrator cannot sign in after the restart")
	}
	until(t, 2*time.Minute, "the restarted leader to take a new term and show the keys without an alarm", func() bool {
		if ledgerTerm(t, leader) <= before {
			return false
		}
		st, b := leader.Do("GET", "/ledger/signing", nil, nil)
		return st == 200 && b["signerAlarm"] == nil && len(b["keys"].([]any)) == 3
	})

	// A leader that was deposed cannot write: another leader's term is stored; the task of this one ends with the
	// code term_over and changes nothing. A restart takes a new term and writes again.
	var rowsBefore int
	_ = leader.ServicePG("kingres_system").QueryRow(context.Background(), `SELECT (SELECT count(*) FROM kingres_cp.ledger_signing_key) + (SELECT count(*) FROM kingres_cp.ledger_event)`).Scan(&rowsBefore)
	if _, err := leader.ServicePG("kingres_system").Exec(context.Background(), `UPDATE kingres_cp.ledger_cluster SET term = term + 1000`); err != nil {
		t.Fatal(err)
	}
	// Retire the third key's predecessor is not possible; a rotation is: one key is active.
	done = startKeyTask(t, leader, "/ledger/signing/keys", map[string]any{})
	e, _ := done["error"].(map[string]any)
	if done["state"] != "failed" || e == nil || e["code"] != "term_over" {
		t.Fatalf("the task of a deposed leader: %v %v", done["state"], done["error"])
	}
	var rowsAfter int
	_ = leader.ServicePG("kingres_system").QueryRow(context.Background(), `SELECT (SELECT count(*) FROM kingres_cp.ledger_signing_key) + (SELECT count(*) FROM kingres_cp.ledger_event)`).Scan(&rowsAfter)
	if rowsAfter != rowsBefore {
		t.Errorf("a deposed leader wrote: %d rows before, %d after", rowsBefore, rowsAfter)
	}
	if st, b := leader.Do("PUT", "/ledger/settings", map[string]any{"anchor": map[string]any{"minimumAnchored": 3}}, nil); st == 200 {
		t.Errorf("a deposed leader changed the settings: %v", b)
	}
	leader.Kill()
	leader.Start()
	if !leader.Login(clustertest.AdminUser, clustertest.AdminPass) {
		t.Fatal("the administrator cannot sign in after the second restart")
	}
	until(t, 2*time.Minute, "the leader to write again under its new term", func() bool {
		st, _ := leader.Do("PUT", "/ledger/settings", map[string]any{"anchor": map[string]any{"minimumAnchored": 2}}, nil)
		return st == 200
	})
}

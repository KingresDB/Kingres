// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package clustertest_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

var bundleNumberRE = regexp.MustCompile(`\bbundle (\d{1,15})\b`)

// Step 20: a bundle of the publication of a database, built by a task on the leader and kept 24 hours, which the standalone verifier accepts.
func TestLedgerBundleOfAPublishedDatabaseIsVerifiedByTheVerifier(t *testing.T) {
	p := formPublishing(t, "bundle_db", "bundle_empty_db")
	leader, follower := p.c.Nodes[0], p.c.Nodes[1]
	until(t, 90*time.Second, "both servers to be listed", func() bool { return len(leader.Servers()) == 2 })
	ledgerTable(t, leader, "bundle_db", "t0", 3)
	ledgerTable(t, follower, "bundle_db", "t1", 3)
	// a database whose ledger is not published has no bundle
	if st, e := leader.Do("POST", "/databases/bundle_empty_db/ledger/bundles", map[string]any{"includeAnchors": false}, nil); st != 409 && st != 422 {
		t.Errorf("a bundle of a database that is not published: %d %v", st, e)
	}
	p.startPublishing(t)
	leader.Expect(201, "POST", "/databases/bundle_db/ledger/digests", nil)
	until(t, 3*time.Minute, "the aggregate to be published", func() bool {
		for _, l := range publicationLogs(t, leader) {
			if l.Kind == "aggregate" && l.Size > 0 && strings.Contains(l.Origin, "/a/") {
				return true
			}
		}
		return false
	})

	base := "/databases/bundle_db/ledger/bundles"
	for _, bad := range []map[string]any{
		{"includeAnchors": false, "sinceSize": -1},
		{"includeAnchors": false, "unknown": 1},
		{},
	} {
		if st, _ := follower.Do("POST", base, bad, nil); st != 400 {
			t.Errorf("%v: %d", bad, st)
		}
	}
	// through the follower: the task runs on the leader and the bundle is read from the leader
	task := follower.Expect(202, "POST", base, map[string]any{"includeAnchors": false})
	if task["kind"] != "ledger.bundle.create" {
		t.Fatalf("the task: %v", task)
	}
	done := leader.WaitTaskDone(clustertest.TaskID(task), 4*time.Minute)
	if done["state"] != "done" {
		t.Fatalf("the bundle task: %v", done)
	}
	detail, _ := done["detail"].(string)
	m := bundleNumberRE.FindStringSubmatch(detail)
	if m == nil {
		t.Fatalf("the task detail does not name the bundle: %q", detail)
	}
	path := base + "/" + m[1]
	for _, n := range []*clustertest.Node{leader, follower} {
		st, raw := rawGet(t, n, path)
		if st != 200 || len(raw) < 100 {
			t.Fatalf("node %d: the bundle: %d (%d bytes)", n.Index, st, len(raw))
		}
	}
	_, raw := rawGet(t, follower, path)
	dir := t.TempDir()
	names := untar(t, raw, dir)
	for _, want := range []string{"bundle.json", "keyring.json", "events/000000"} {
		if !containsString(names, want) {
			t.Errorf("the bundle has no %s: %v", want, names)
		}
	}
	var idx struct {
		Format int `json:"format"`
		Logs   []struct {
			Kind string `json:"kind"`
			Size int64  `json:"size"`
		} `json:"logs"`
	}
	b, _ := os.ReadFile(filepath.Join(dir, "bundle.json"))
	if err := json.Unmarshal(b, &idx); err != nil || idx.Format != 1 || len(idx.Logs) != 3 {
		t.Errorf("bundle.json: %v %s", err, b)
	}
	// the verifier accepts it, from the archive itself
	tf := filepath.Join(t.TempDir(), "trust.json")
	st, trust := rawGet(t, leader, "/ledger/signing/trust-file")
	if st != 200 {
		t.Fatalf("trust file: %d", st)
	}
	if err := os.WriteFile(tf, trust, 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "bundle.tar.gz")
	if err := os.WriteFile(archive, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(verifierBinary(t), "bundle", "verify", "--trust", tf, archive).CombinedOutput() //nolint:gosec // the verifier under test
	if err != nil {
		t.Fatalf("kingres-verify bundle verify: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "VERIFIED") {
		t.Errorf("the verifier did not say VERIFIED:\n%s", out)
	}

	// a bundle that does not exist, another database's bundle and an origin that is not a log of the database
	leader.Expect(404, "GET", base+"/999999", nil)
	leader.Expect(404, "GET", "/databases/bundle_empty_db/ledger/bundles/"+m[1], nil)
	bad := follower.Expect(202, "POST", base, map[string]any{"includeAnchors": false, "logs": []string{"example.org/not/a/log"}})
	if d := leader.WaitTask(clustertest.TaskID(bad), 2*time.Minute); d["state"] != "failed" {
		t.Errorf("a bundle of an origin that is not a log: %v", d)
	}
}

func containsString(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// untar unpacks a .tar.gz below dir (the names are checked) and returns the names.
func untar(t *testing.T, raw []byte, dir string) []string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(h.Name, "..") || strings.HasPrefix(h.Name, "/") {
			t.Fatalf("a name outside the bundle: %s", h.Name)
		}
		p := filepath.Join(dir, filepath.FromSlash(h.Name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(f, tr); err != nil { //nolint:gosec // a test bundle
			t.Fatal(err)
		}
		_ = f.Close()
		names = append(names, h.Name)
	}
	return names
}

// Step 20: the check publication of a verification audits the stored tiles of the logs of the database against the roots of their
// checkpoints; it passes on an intact publication and names the log whose tile was changed behind the system database's guard.
func TestVerificationOfThePublicationFindsAChangedTile(t *testing.T) {
	p := formPublishing(t, "audit_db")
	leader, follower := p.c.Nodes[0], p.c.Nodes[1]
	until(t, 90*time.Second, "both servers to be listed", func() bool { return len(leader.Servers()) == 2 })
	ledgerTable(t, leader, "audit_db", "t0", 4)
	ledgerTable(t, follower, "audit_db", "t1", 4)
	p.startPublishing(t)
	leader.Expect(201, "POST", "/databases/audit_db/ledger/digests", nil)
	until(t, 3*time.Minute, "the chain logs to be published", func() bool {
		n := 0
		for _, l := range publicationLogs(t, leader) {
			if l.Kind == "chain" && l.Size > 0 {
				n++
			}
		}
		return n == 2
	})
	base := "/databases/audit_db/ledger/verifications"
	// requests that cannot be served
	if st, _ := leader.Do("POST", base, map[string]any{"checks": []string{"bogus"}}, nil); st != 400 {
		t.Errorf("an unknown check: %d", st)
	}
	if st, _ := leader.Do("POST", base, map[string]any{"checks": []string{"publication", "publication"}}, nil); st != 400 {
		t.Errorf("a check twice: %d", st)
	}
	if st, e := leader.Do("POST", base, map[string]any{"checks": []string{"anchors"}}, nil); st != 422 || e["code"] != "unsupported_operation" {
		t.Errorf("the check of the anchors: %d %v", st, e)
	}
	// intact
	task := leader.Expect(202, "POST", base, map[string]any{"scope": "full", "checks": []string{"chain", "publication"}})
	if done := leader.WaitTaskDone(clustertest.TaskID(task), 4*time.Minute); done["state"] != "done" {
		t.Fatalf("the audit of an intact publication: %v", done)
	}
	// a tile changed behind the guard
	ctx := context.Background()
	c := leader.ServicePG("kingres_system")
	defer func() { _ = c.Close(ctx) }()
	tamper := []string{
		`ALTER TABLE kingres_cp.ledger_tile DISABLE TRIGGER USER`,
		`UPDATE kingres_cp.ledger_tile SET data = set_byte(data, 0, get_byte(data, 0) # 1) WHERE (log_id, level, idx, width) = (SELECT log_id, level, idx, width FROM kingres_cp.ledger_tile t
			WHERE level = 0 AND log_id IN (SELECT log_id FROM kingres_cp.ledger_log WHERE database_name = 'audit_db') ORDER BY log_id, idx LIMIT 1)`,
		`ALTER TABLE kingres_cp.ledger_tile ENABLE TRIGGER USER`,
	}
	for _, q := range tamper {
		if _, err := c.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	task = leader.Expect(202, "POST", base, map[string]any{"scope": "full", "checks": []string{"publication"}})
	done := leader.WaitTask(clustertest.TaskID(task), 4*time.Minute)
	e, _ := done["error"].(map[string]any)
	if done["state"] != "failed" || !strings.Contains(fmt.Sprint(e["message"]), "audit of the publication found") {
		t.Fatalf("the audit of a changed tile: %v", done)
	}
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package clustertest_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// fixtureDir is where the whole publication of a moment is kept: <KINGRES_PUBLICATION_FIXTURES>/<name>, emptied first. Without the variable the
// files go to a temporary directory (the checks of savePublication still run).
func fixtureDir(t *testing.T, name string) string {
	t.Helper()
	base := os.Getenv("KINGRES_PUBLICATION_FIXTURES")
	if base == "" {
		base = t.TempDir()
	}
	dir := filepath.Join(base, name)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeFile(t *testing.T, dir, rel string, data []byte) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// savePublication copies the whole publication of this moment through the listener of a node into dir, in its own layout: logs.json, keyring.json, the
// events, for every log in logs.json its checkpoint and the tiles that the verifier needs, the evidence files of the anchored records, and the trust
// file next to them. It checks logs.json against the logs the API lists.
func savePublication(t *testing.T, p *pubCluster, node int, dir, db string) {
	t.Helper()
	base := p.base(node)
	st, lj := httpGet(t, base+"/logs.json")
	if st != 200 {
		t.Fatalf("logs.json: %d", st)
	}
	var doc struct {
		Format int `json:"format"`
		Logs   []struct {
			Kind   string `json:"kind"`
			Origin string `json:"origin"`
			State  string `json:"state"`
			Size   int64  `json:"size"`
		} `json:"logs"`
	}
	if err := json.Unmarshal(lj, &doc); err != nil || doc.Format != 1 {
		t.Fatalf("logs.json is not in the defined format: %v %s", err, lj)
	}
	var listed, api []string
	for _, l := range doc.Logs {
		listed = append(listed, l.Origin)
	}
	for _, l := range publicationLogs(t, p.c.Nodes[0]) {
		api = append(api, l.Origin)
	}
	sort.Strings(listed)
	sort.Strings(api)
	if fmt.Sprint(listed) != fmt.Sprint(api) {
		t.Fatalf("logs.json lists %v, the API %v", listed, api)
	}
	writeFile(t, dir, "logs.json", lj)
	if st, kr := httpGet(t, base+"/keyring.json"); st != 200 {
		t.Fatalf("keyring.json: %d", st)
	} else {
		writeFile(t, dir, "keyring.json", kr)
	}
	events := 0
	for ; ; events++ {
		st, b := httpGet(t, base+fmt.Sprintf("/events/%06d", events))
		if st == 404 {
			break
		}
		if st != 200 {
			t.Fatalf("event %d: %d", events, st)
		}
		writeFile(t, dir, fmt.Sprintf("events/%06d", events), b)
	}
	if events == 0 {
		t.Fatal("the publication has no event")
	}
	for _, l := range doc.Logs {
		lp := logPath(l.Origin)
		if lp == "" {
			t.Fatalf("origin %q", l.Origin)
		}
		download(t, base, lp, dir)
	}
	st, raw := rawGet(t, p.c.Nodes[0], "/ledger/signing/trust-file")
	if st != 200 {
		t.Fatalf("trust file: %d", st)
	}
	writeFile(t, dir, "trust.json", raw)
	// the evidence files of the anchored records of the database
	page := p.c.Nodes[0].Expect(200, "GET", "/databases/"+db+"/ledger/anchors?state=anchored&limit=100", nil)
	for _, it := range anyList(page["items"]) {
		m := asMap(it)
		if m["targetId"] == nil {
			continue
		}
		agg, ok := logOf(publicationLogs(t, p.c.Nodes[0]), "aggregate", 0)
		if !ok {
			t.Fatal("no aggregate")
		}
		rel := fmt.Sprintf("%s/anchors/%d/%d.json", logPath(agg.Origin), int64(m["aggregateSize"].(float64)), int64(m["targetId"].(float64)))
		st, ev := httpGet(t, base+"/"+rel)
		if st != 200 {
			t.Fatalf("the evidence file %s: %d", rel, st)
		}
		writeFile(t, dir, rel, ev)
	}
}

// anchorOne makes sure a WORM target on the fake store exists, anchors the newest aggregate of the database now and waits until at least want records
// of the database are anchored.
func anchorOne(t *testing.T, p *pubCluster, db string, s3 *fakeS3, want int) {
	t.Helper()
	leader := p.c.Nodes[0]
	tg := leader.Expect(200, "GET", "/ledger/anchor-targets", nil)
	if len(anyList(tg["items"])) == 0 {
		leader.Expect(201, "POST", "/ledger/anchor-targets", map[string]any{"kind": "worm", "name": "fixture-store", "enabled": true,
			"config":      map[string]any{"endpoint": s3.srv.URL, "region": fakeRegion, "bucket": fakeBucket, "allowInsecure": true, "retentionDays": 30, "mode": "compliance", "ifNoneMatch": "on"},
			"credentials": map[string]any{"accessKeyId": fakeKeyID, "secretAccessKey": fakeSecret}})
	}
	leader.Expect(201, "POST", "/databases/"+db+"/ledger/anchors", map[string]any{})
	until(t, 4*time.Minute, "the records to be anchored", func() bool {
		page := leader.Expect(200, "GET", "/databases/"+db+"/ledger/anchors?state=anchored&limit=100", nil)
		return len(anyList(page["items"])) >= want
	})
}

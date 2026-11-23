// SPDX-License-Identifier: AGPL-3.0-only

package clustertest_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// The data transfer through a follower (contract 0.19.0): three servers that serve HTTPS; every request goes to a server that is not the manager
// leader, which relays the upload and the download to the leader through the mutual TLS listener without buffering them. The leader decides on the setting
// and the role, audits, and holds the files.

func xfSum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func xfData(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func xfChunk(t *testing.T, n *clustertest.Node, id string, off int, data []byte) (int, string) {
	t.Helper()
	st, h, _, err := n.DoBody("PATCH", "/data-uploads/"+id, data, map[string]string{"Upload-Offset": strconv.Itoa(off), "Content-Type": "application/octet-stream"})
	if err != nil {
		t.Fatalf("chunk at %d: %v", off, err)
	}
	return st, h.Get("Upload-Offset")
}

func TestDataTransferThroughAFollower(t *testing.T) {
	c := clustertest.FormTLS(t, 3, nil, func(leader *clustertest.Node) {
		body := leader.Expect(202, "POST", "/databases", map[string]any{"name": "xf", "sharding": map[string]any{"enabled": true, "bucketCount": 12}})
		leader.WaitTaskDone(clustertest.TaskID(body), 4*time.Minute)
	})
	leader, f1, f2 := c.Leader(), c.Nodes[1], c.Nodes[2]
	for _, n := range c.Nodes {
		n.Login(clustertest.AdminUser, clustertest.AdminPass)
	}
	q := func(sql string) map[string]any {
		return leader.Expect(200, "POST", "/query", map[string]any{"database": "xf", "sql": sql})
	}
	leader.Expect(200, "PATCH", "/settings/cluster", map[string]any{"dataTransfer": map[string]any{"enabled": true, "maxConcurrent": 4}})

	// The setting is read by the leader: a follower relays the request and the answer is the leader's.
	if st, out := f1.Do("GET", "/data-exports", nil, nil); st != 200 {
		t.Fatalf("list through a follower: %d %v", st, out)
	}
	leader.Expect(200, "PATCH", "/settings/cluster", map[string]any{"dataTransfer": map[string]any{"enabled": false}})
	if st, out := f1.Do("POST", "/data-uploads", map[string]any{"purpose": "tableImport", "sizeBytes": 10}, nil); st != 403 || out["code"] != "data_transfer_disabled" {
		t.Errorf("setting off, through a follower: %d %v", st, out)
	}
	leader.Expect(200, "PATCH", "/settings/cluster", map[string]any{"dataTransfer": map[string]any{"enabled": true}})

	// An upload of 5 MiB plus a tail, through follower 1, in chunks of 1 MiB; the offset is asked of the other follower; a break and a resume.
	q("create table xf_t (id int, note text)")
	var csv bytes.Buffer
	for i := 0; i < 120000; i++ {
		fmt.Fprintf(&csv, "%d,note number %d with some text\n", i, i)
	}
	data := csv.Bytes()
	up := f1.Expect(201, "POST", "/data-uploads", map[string]any{"purpose": "tableImport", "sizeBytes": len(data), "sha256": xfSum(data)})
	id := up["uploadId"].(string)
	const mb = 1 << 20
	off := 0
	for off+mb < len(data) {
		if st, next := xfChunk(t, f1, id, off, data[off:off+mb]); st != 204 || next != strconv.Itoa(off+mb) {
			t.Fatalf("chunk at %d: %d %q", off, st, next)
		}
		off += mb
		if off == 2*mb {
			// The client asks another server where the upload is: the answer is the leader's, read from the file.
			if got := f2.Expect(200, "GET", "/data-uploads/"+id, nil); got["offset"] != float64(off) {
				t.Fatalf("offset asked of another follower: %v, want %d", got["offset"], off)
			}
			// A chunk at a wrong offset gets the right one in the header, through the relay.
			if st, next := xfChunk(t, f2, id, off+65536, data[off:off+65536]); st != 409 || next != strconv.Itoa(off) {
				t.Errorf("a chunk at a wrong offset through a follower: %d, header %q", st, next)
			}
		}
	}
	if st, _ := xfChunk(t, f2, id, off, data[off:]); st != 204 {
		t.Fatalf("last chunk through the second follower: %d", st)
	}
	done := leader.Expect(200, "GET", "/data-uploads/"+id, nil)
	if done["state"] != "complete" || done["sha256"] != xfSum(data) {
		t.Fatalf("complete upload: %v", done)
	}
	// Another administrator sees nothing, on any server.
	other := leader.Expect(201, "POST", "/roles", map[string]any{"name": "xf_other", "password": "Passw0rd-xf-other", "clusterRoles": []string{"kingres_admin"}})
	_ = other
	for _, n := range c.Nodes {
		n.Login("xf_other", "Passw0rd-xf-other")
		if st, _ := n.Do("GET", "/data-uploads/"+id, nil, nil); st != 404 {
			t.Errorf("node %d: the upload of another role: %d", n.Index, st)
		}
		n.Login(clustertest.AdminUser, clustertest.AdminPass)
	}

	// The import through a follower; the task is the leader's.
	st, task := f1.Do("POST", "/databases/xf/schemas/public/tables/xf_t/import", map[string]any{"uploadId": id, "header": "none"}, nil)
	if st != 202 {
		t.Fatalf("import through a follower: %d %v", st, task)
	}
	res := f2.WaitTask(clustertest.TaskID(task), 5*time.Minute)
	if res["state"] != "done" {
		t.Fatalf("import task: %v", res["error"])
	}
	if got := fmt.Sprint(q("select count(*) from xf_t")["rows"]); got != "[[120000]]" {
		t.Errorf("rows after the import: %s", got)
	}

	// The export through a follower, downloaded through both followers and the leader: the same bytes, the same sum, ranges through the relay.
	st, task = f2.Do("POST", "/databases/xf/schemas/public/tables/xf_t/export", map[string]any{"spreadsheetSafe": false}, nil)
	if st != 202 {
		t.Fatalf("export through a follower: %d %v", st, task)
	}
	res = f1.WaitTask(clustertest.TaskID(task), 5*time.Minute)
	if res["state"] != "done" || !strings.HasPrefix(fmt.Sprint(res["detail"]), "transfer ") {
		t.Fatalf("export task: %v", res)
	}
	xid := strings.TrimPrefix(res["detail"].(string), "transfer ")
	var first []byte
	for _, n := range []*clustertest.Node{f1, f2, leader} {
		st, h, body, err := n.DoBody("GET", "/data-exports/"+xid+"/content", nil, nil)
		if err != nil || st != 200 || h.Get("Content-Length") != strconv.Itoa(len(body)) || len(body) < len(data) {
			t.Fatalf("node %d download: %d %v len %d err %v", n.Index, st, h, len(body), err)
		}
		if first == nil {
			first = body
		} else if !bytes.Equal(first, body) {
			t.Errorf("node %d got other bytes", n.Index)
		}
		if h.Get("ETag") != `"`+xfSum(body)+`"` {
			t.Errorf("node %d: ETag %q", n.Index, h.Get("ETag"))
		}
	}
	st, h, part, _ := f1.DoBody("GET", "/data-exports/"+xid+"/content", nil, map[string]string{"Range": "bytes=100-199"})
	if st != 206 || !bytes.Equal(part, first[100:200]) || !strings.HasPrefix(h.Get("Content-Range"), "bytes 100-199/") {
		t.Errorf("range through a follower: %d %v", st, h)
	}
	// The creator only, on every server; any administrator may delete.
	for _, n := range c.Nodes {
		n.Login("xf_other", "Passw0rd-xf-other")
		if st, _, _, _ := n.DoBody("GET", "/data-exports/"+xid+"/content", nil, nil); st != 404 {
			t.Errorf("node %d: the download of another role: %d", n.Index, st)
		}
		n.Login(clustertest.AdminUser, clustertest.AdminPass)
	}
	// The audit log of the leader has the events once, with the follower named, and no value of the data.
	ev := leader.Expect(200, "GET", "/audit?action=downloadDataExport", nil)
	items, _ := ev["items"].([]any)
	if len(items) < 3 {
		t.Errorf("download events: %d, want at least 3", len(items))
	}
	for _, it := range items {
		raw := fmt.Sprint(it)
		if strings.Contains(raw, "note number") {
			t.Errorf("a value in the audit log: %s", raw)
		}
	}
	f2.Expect(204, "DELETE", "/data-exports/"+xid, nil)

	// A follower that dies in the middle of an upload: the offset is the whole segments the leader kept, and the upload goes on through another server.
	up = leader.Expect(201, "POST", "/data-uploads", map[string]any{"purpose": "tableImport", "sizeBytes": 3 * mb})
	uid := up["uploadId"].(string)
	body := xfData(3*mb, 3)
	if st, _ := xfChunk(t, f1, uid, 0, body[:mb]); st != 204 {
		t.Fatalf("first chunk: %d", st)
	}
	f1.Kill()
	deadline := time.Now().Add(20 * time.Second)
	var got map[string]any
	for time.Now().Before(deadline) {
		if _, got = f2.Do("GET", "/data-uploads/"+uid, nil, nil); got["offset"] == float64(mb) {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if got["offset"] != float64(mb) {
		t.Fatalf("after the follower died: %v", got)
	}
	if st, _ := xfChunk(t, f2, uid, mb, body[mb:]); st != 204 {
		t.Fatalf("the rest through another server: %d", st)
	}
	if got := leader.Expect(200, "GET", "/data-uploads/"+uid, nil); got["sha256"] != xfSum(body) {
		t.Errorf("the upload that went on after the follower died: %v", got)
	}
	leader.Expect(204, "DELETE", "/data-uploads/"+uid, nil)
}

// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ops"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

const mbi = 1 << 20

// A record of 15.5 MiB goes through PostgreSQL and arrives whole; a record of 16 MiB and one byte is refused by the scanner with its line.
func TestImportRecordNearTheLimit(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	q := xferDB(t, e, "db_xfer")
	q("create table if not exists public.rec_t (id int, v text)")
	q("truncate public.rec_t")
	big := append(append([]byte("1,\""), bytes.Repeat([]byte("x"), 15*mbi+mbi/2)...), []byte("\"\n2,small\n")...)
	uid := e.uploadFile(t, big)
	task := e.startImport(t, "db_xfer", "rec_t", map[string]any{"uploadId": uid, "header": "none"})
	if task["state"] != "done" {
		t.Fatalf("a record of 15.5 MiB: %v", task["error"])
	}
	if got := fmt.Sprint(q("select id, length(v)::text from public.rec_t order by id")["rows"]); got != "[[1 16252928] [2 5]]" {
		t.Errorf("rows: %s", got)
	}
	q("truncate public.rec_t")
	over := append(append([]byte("2,ok\n1,\""), bytes.Repeat([]byte("x"), 16*mbi)...), []byte("\"\n")...)
	uid = e.uploadFile(t, over)
	task = e.startImport(t, "db_xfer", "rec_t", map[string]any{"uploadId": uid, "header": "none"})
	if task["state"] != "failed" || taskCode(task) != "import_record_too_large" || !strings.Contains(taskMessage(task), "line 2") {
		t.Errorf("a record above 16 MiB: %v", task["error"])
	}
	if count(t, q, "rec_t") != "[[0]]" {
		t.Errorf("rows after the refused record: %s", count(t, q, "rec_t"))
	}
}

// activeImportID finds the task of the import that runs now.
func (e *cp) activeImportID(t testing.TB) string {
	t.Helper()
	out := e.expect(t, 200, "GET", "/tasks?kind=table.import&state=running", nil)
	items, _ := out["items"].([]any)
	if len(items) == 0 {
		t.Fatalf("no running import: %v", out)
	}
	return fmt.Sprint(int64(items[0].(map[string]any)["id"].(float64)))
}

// A cancel that arrives after the intent is refused (the commit is on its way and is not tied to the request), and the import completes exactly once.
func TestImportCancelAfterTheIntent(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	q := xferDB(t, e, "db_xfer")
	q("create table if not exists public.ci_t (id int, v text)")
	q("truncate public.ci_t")
	uid := e.uploadFile(t, []byte("1,a\n2,b\n"))
	var status int
	var code string
	ops.ImportFault = func(p string, _ uint32) error {
		if p == "afterIntent" {
			ops.ImportFault = nil
			id := e.activeImportID(t)
			st, out, _ := e.call("POST", "/tasks/"+id+"/cancel", nil, nil)
			status, code = st, problemCode(out)
		}
		return nil
	}
	defer func() { ops.ImportFault = nil }()
	task := e.startImport(t, "db_xfer", "ci_t", map[string]any{"uploadId": uid, "header": "none"})
	if status != 409 || code != "task_not_cancellable" {
		t.Errorf("a cancel after the intent: %d %s", status, code)
	}
	if task["state"] != "done" || count(t, q, "ci_t") != "[[2]]" {
		t.Errorf("the import after the refused cancel: %v rows %s", task["error"], count(t, q, "ci_t"))
	}
}

// A leader that lost its lease before the intent cannot write the intent, so it cannot commit: the hook stands for the refusal of the write of the intent. Nothing is
// stored, the task does not fail with a code that claims an outcome, and a repeat loads the rows exactly once.
func TestImportLeaseLostBeforeTheIntent(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	q := xferDB(t, e, "db_xfer")
	q("create table if not exists public.ll_t (id int, v text)")
	q("truncate public.ll_t")
	uid := e.uploadFile(t, []byte("1,a\n2,b\n3,c\n"))
	ops.ImportFault = func(p string, _ uint32) error {
		if p == "intentWrite" {
			ops.ImportFault = nil
			return task.ErrLeaseLost
		}
		return nil
	}
	defer func() { ops.ImportFault = nil }()
	st, out, _ := e.call("POST", "/databases/db_xfer/schemas/public/tables/ll_t/import", map[string]any{"uploadId": uid, "header": "none", "keepUpload": true}, nil)
	if st != 202 {
		t.Fatalf("%d %v", st, out)
	}
	time.Sleep(3 * time.Second)
	if got := count(t, q, "ll_t"); got != "[[0]]" {
		t.Errorf("rows after the intent could not be written: %s", got)
	}
	// The run stood down: the task is not failed with a verdict about the rows. Whoever takes it over (a new epoch) finds no open transaction
	// and no intent, and fails with the retryable code, nothing stored; a repeat then loads three rows, not six.
	deadline := time.Now().Add(2 * time.Minute)
	var done map[string]any
	for time.Now().Before(deadline) {
		_, cur, _ := e.call("GET", "/tasks/"+fmt.Sprint(int64(out["id"].(float64))), nil, nil)
		if cur["state"] == "failed" || cur["state"] == "done" {
			done = cur
			break
		}
		time.Sleep(2 * time.Second)
	}
	if done == nil {
		t.Fatal("the task did not end")
	}
	if got := count(t, q, "ll_t"); got != "[[0]]" && got != "[[3]]" {
		t.Errorf("half the rows after the lease was lost: %s", got)
	}
	if done["state"] == "failed" && taskCode(done) == "import_outcome_unknown" {
		t.Errorf("an unknown outcome was claimed although the commit was never sent: %v", done["error"])
	}
	// The task that took it over found no intent, loaded the rows once and is done; only a task that failed needs a repeat.
	if done["state"] != "done" {
		if task := e.startImport(t, "db_xfer", "ll_t", map[string]any{"uploadId": uid, "header": "none"}); task["state"] != "done" {
			t.Errorf("the repeat: %v", task["error"])
		}
	}
	if got := count(t, q, "ll_t"); got != "[[3]]" {
		t.Errorf("rows after the take-over or the repeat: %s", got)
	}
}

// DX14-01: the process ends after COMMIT and before the step is recorded. The run that takes the task over finds the intent, asks the marker, and finishes the
// task as done with exactly the rows that were committed; it never loads again and never claims that nothing was stored.
func TestImportCrashAfterCommit(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	q := xferDB(t, e, "db_xfer")
	q("create table if not exists public.ac_t (id int, v text)")
	q("truncate public.ac_t")
	uid := e.uploadFile(t, []byte("1,a\n2,b\n3,c\n"))
	ops.ImportFault = func(p string, _ uint32) error {
		if p == "afterCommit" {
			ops.ImportFault = nil
			return task.ErrLeaseLost
		}
		return nil
	}
	defer func() { ops.ImportFault = nil }()
	st, out, _ := e.call("POST", "/databases/db_xfer/schemas/public/tables/ac_t/import", map[string]any{"uploadId": uid, "header": "none", "keepUpload": true}, nil)
	if st != 202 {
		t.Fatalf("%d %v", st, out)
	}
	deadline := time.Now().Add(3 * time.Minute)
	var done map[string]any
	for time.Now().Before(deadline) {
		_, cur, _ := e.call("GET", "/tasks/"+fmt.Sprint(int64(out["id"].(float64))), nil, nil)
		if cur["state"] == "failed" || cur["state"] == "done" {
			done = cur
			break
		}
		time.Sleep(2 * time.Second)
	}
	if done == nil {
		t.Fatal("the task did not end")
	}
	if done["state"] != "done" {
		t.Fatalf("a commit that happened must end the task done, got %v", done["error"])
	}
	if got := count(t, q, "ac_t"); got != "[[3]]" {
		t.Errorf("rows after the crash after COMMIT: %s", got)
	}
	var state string
	var rows int64
	if err := e.serviceConn(t, "db_xfer").QueryRow(context.Background(), "select state, rows from kingres_transfer.import_marker order by at desc limit 1").Scan(&state, &rows); err != nil || state != "committed" || rows != 3 {
		t.Errorf("marker: %s %d %v", state, rows, err)
	}
}

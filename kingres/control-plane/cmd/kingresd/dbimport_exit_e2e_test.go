// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"context"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ops"
)

// The exits of a paused import through the REST API (the router of kingresd, a real PostgreSQL instance, the importing role signed in over HTTP): the repeat of the
// import request that resumes it, the answers for another role and another database, the repeat while the task runs, and a cancel that races a repeat.

// forwardFails makes the part of the last step after the commit point fail (as a server that refuses to open the database) while it is set.
type forwardFails struct{ on atomic.Bool }

func (f *forwardFails) install() {
	ops.ImportForwardHook = func(context.Context) error {
		if f.on.Load() {
			return errors.New("a server refuses to open the database")
		}
		return nil
	}
}

func (f *forwardFails) remove() { ops.ImportForwardHook = nil }

func taskState(t testing.TB, e *cp, id string) map[string]any {
	t.Helper()
	st, out, _ := e.call("GET", "/tasks/"+id, nil, nil)
	if st != 200 {
		t.Fatalf("GET /tasks/%s: %d %v", id, st, out)
	}
	return out
}

// waitState waits until the task is in one of the states.
func waitState(t testing.TB, e *cp, id string, states ...string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		cur := taskState(t, e, id)
		for _, s := range states {
			if cur["state"] == s {
				return cur
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %s did not reach %v: %v", id, states, cur)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// pausedImport starts an import of a new upload of the archive and pauses it after its commit point: the roll forward fails (the hook is set), the operator
// cancels the running task, and the task is paused. It returns the upload and the task id.
func pausedImport(t testing.TB, imp *cp, archive []byte, db string) (upload, taskID string) {
	t.Helper()
	upload = imp.uploadArchive(t, archive)
	st, out := imp.importDB(t, map[string]any{"uploadId": upload, "database": db})
	if st != 202 {
		t.Fatalf("importDatabase: %d %v", st, out)
	}
	taskID = idOf(out)
	// the import commits and then waits for the servers: its detail says so
	deadline := time.Now().Add(2 * time.Minute)
	for {
		cur := taskState(t, imp, taskID)
		if d, _ := cur["detail"].(string); strings.Contains(d, "is committed and finishes again") {
			break
		}
		if cur["state"] == "failed" || cur["state"] == "done" || time.Now().After(deadline) {
			t.Fatalf("the import did not reach its commit point: %v", cur)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if st, out, _ := imp.call("POST", "/tasks/"+taskID+"/cancel", nil, nil); st != 200 {
		t.Fatalf("the cancel of the committed import: %d %v", st, out)
	}
	paused := waitState(t, imp, taskID, "paused")
	if d, _ := paused["detail"].(string); !strings.HasPrefix(d, "A cancel was requested") && !strings.Contains(d, "committed") {
		t.Fatalf("the detail of the paused import: %q", d)
	}
	return upload, taskID
}

func dropImported(e *cp, names ...string) {
	for _, n := range names {
		_, _, _ = e.call("DELETE", "/databases/"+n, nil, nil)
	}
}

// M3: the exits through the API. A role that does not own the upload gets 404 and the task stays paused; a repeat for another database is refused (409, the upload is
// used) and changes nothing; a repeat that is the same request resumes the paused task (202, the same task id, the other fields ignored); a repeat while the resumed
// task runs gets 409; the import then finishes.
func TestDatabaseImportRepeatThroughTheAPI(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	imp := importerOf(t, e)
	archive := smallArchive(t, e, "dbexsrc")
	fails := &forwardFails{}
	fails.on.Store(true)
	fails.install()
	defer fails.remove()
	defer dropImported(e, "dbex_a")

	upload, id := pausedImport(t, imp, archive, "dbex_a")

	// another owner: the admin is not the role that made the upload
	if st, out := e.importDB(t, map[string]any{"uploadId": upload, "database": "dbex_a"}); st != 404 {
		t.Errorf("another owner repeats the request: %d %v", st, out)
	}
	// another database name is not a resume of this task
	if st, out := imp.importDB(t, map[string]any{"uploadId": upload, "database": "dbex_other"}); st != 409 || problemCode(out) != "operation_in_progress" {
		t.Errorf("a repeat for another database: %d %v", st, out)
	}
	if got := taskState(t, imp, id); got["state"] != "paused" {
		t.Fatalf("a refused repeat changed the task: %v", got)
	}
	// the same request resumes it: 202, the same task, whatever the other fields say
	st, out := imp.importDB(t, map[string]any{"uploadId": upload, "database": "dbex_a", "keepOnFailure": true, "expectedSha256": strings.Repeat("a", 64)})
	if st != 202 || idOf(out) != id {
		t.Fatalf("the repeat of the request: %d %v (the task was %s)", st, out, id)
	}
	validTask(t, out)
	resumed := out
	// the hook still fails, so the resumed task runs and retries: a repeat now gets 409
	cur := waitState(t, imp, id, "running", "pending")
	_ = cur
	if st, out := imp.importDB(t, map[string]any{"uploadId": upload, "database": "dbex_a"}); st != 409 || problemCode(out) != "operation_in_progress" {
		t.Errorf("a repeat while the task runs: %d %v", st, out)
	}
	// the servers answer again: the import finishes, the database is open to its owner
	fails.on.Store(false)
	done := e.waitTask(t, resumed)
	if done["state"] != "done" {
		t.Fatalf("the resumed import: %v", done)
	}
	if got := query(t, e, "dbex_a", "select count(*) from public.victim"); got != "[[2]]" {
		t.Errorf("the data of the imported database: %s", got)
	}
}

// M3: a cancel and a repeat of the request race for the same paused import: only one of them takes the paused task. Either the cancel ended it (failed, code
// cancelled) and the repeat was refused, or the repeat resumed it (202, the same task, pending again) and the cancel then acted on what it found (it ended the
// pending committed task, or paused the running one); no round ends with the task untouched or with another task, and the committed database is never dropped.
func TestDatabaseImportCancelRacesTheRepeat(t *testing.T) {
	e, off := transferOn(t)
	defer off()
	imp := importerOf(t, e)
	archive := smallArchive(t, e, "dbracesrc")
	fails := &forwardFails{}
	fails.on.Store(true)
	fails.install()
	defer fails.remove()
	var names []string
	defer func() { dropImported(e, names...) }()
	cancelWins, repeatWins := 0, 0
	for round := 0; round < 4; round++ {
		db := fmt.Sprintf("dbrace_%d", round)
		names = append(names, db)
		upload, id := pausedImport(t, imp, archive, db)
		var wg sync.WaitGroup
		var cst, rst int
		var rout map[string]any
		wg.Add(2)
		start := make(chan struct{})
		go func() {
			defer wg.Done()
			<-start
			// the odd rounds let the repeat start first, the even ones the cancel
			time.Sleep(time.Duration(round%2) * 40 * time.Millisecond)
			cst, _, _ = imp.call("POST", "/tasks/"+id+"/cancel", nil, nil)
		}()
		go func() {
			defer wg.Done()
			<-start
			rst, rout = imp.importDB(t, map[string]any{"uploadId": upload, "database": db})
		}()
		close(start)
		wg.Wait()
		cur := taskState(t, imp, id)
		cancelled := cur["state"] == "failed" && taskCode(cur) == "cancelled"
		switch {
		case cancelled && rst == 202:
			// the repeat took the paused task (202, the same task, pending again) and the cancel then found it pending and ended it: each acted on the state it found
			if idOf(rout) != id || cst != 200 {
				t.Errorf("round %d: the repeat resumed another task or the cancel did not answer 200 (%d, %v)", round, cst, idOf(rout))
			}
			repeatWins++
		case cancelled:
			cancelWins++
			if rst != 409 && rst != 404 {
				t.Errorf("round %d: the repeat after the cancel: %d %v", round, rst, rout)
			}
		case rst == 202 && idOf(rout) == id:
			repeatWins++
			if cst != 200 && cst != 409 {
				t.Errorf("round %d: the cancel after the repeat: %d", round, cst)
			}
		default:
			t.Errorf("round %d: nobody won: the task is %v, the cancel %d, the repeat %d %v", round, cur["state"], cst, rst, rout)
		}
		if !e.databaseExists(t, db) {
			t.Errorf("round %d: the committed database is gone", round)
		}
		// the import of the round ends before the next round (a resumed one counts as a transfer that runs): the servers answer again, a task that a cancel paused again
		// after the repeat is finished by the repeat of its request, and the hook fails again for the next round
		fails.on.Store(false)
		for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(200 * time.Millisecond) {
			cur := taskState(t, imp, id)
			if cur["state"] == "paused" {
				imp.importDB(t, map[string]any{"uploadId": upload, "database": db})
			}
			if s := cur["state"]; s == "done" || s == "failed" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("import task %s does not end: %v", id, cur)
			}
		}
		fails.on.Store(true)
	}
	t.Logf("the cancel took the paused task in %d rounds, the repeat in %d", cancelWins, repeatWins)

}

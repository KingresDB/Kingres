// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

func committedTask(t *testing.T, mark string, state task.State) task.Task {
	t.Helper()
	p := dbImportParams{Database: "shop", Nonce: importTestNonce, Servers: []string{"s2", "s3"}, Origin: "s1"}
	in, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	tk := task.Task{ID: 7, Kind: KindDatabaseImport, State: state, Input: in}
	switch mark {
	case "commit":
		tk.Mark = importCommitMark(p.private(7))
	default:
		tk.Mark = mark
	}
	return tk
}

// The note of a committed import names the final database and the servers, and says what to do.
func TestImportCommittedNoteSaysWhatHappened(t *testing.T) {
	note, ok := importCommittedNote(committedTask(t, "commit", task.Running))
	if !ok {
		t.Fatal("a task with the commit mark is committed")
	}
	for _, want := range []string{"shop", "committed but not finished", "s1", "s2", "s3", "Repeat the same import request"} {
		if !strings.Contains(note, want) {
			t.Errorf("the note %q lacks %q", note, want)
		}
	}
	if len(note) > 330 {
		t.Errorf("the note is %d bytes and would be cut", len(note))
	}
	if _, ok := importCommittedNote(committedTask(t, "", task.Running)); ok {
		t.Error("a task without a mark is not committed")
	}
	note, ok = importCommittedNote(committedTask(t, task.MarkUnreadable, task.Running))
	if !ok || !strings.Contains(note, "may be committed") {
		t.Errorf("an unreadable mark counts as possibly committed: %q %v", note, ok)
	}
}

// A committed import is cancelled by nobody while it is paused (it is resumed), and a cancel of a running one is accepted (it pauses the task).
func TestImportCancelGuardOfACommittedImport(t *testing.T) {
	o := &Ops{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	def := o.dbImportDefinition()
	if !def.Cancellable(committedTask(t, "commit", task.Running)) {
		t.Error("a cancel of a running committed import is accepted")
	}
	if def.Cancellable(committedTask(t, "commit", task.Paused)) {
		t.Error("a cancel of a paused committed import is refused: it is resumed, not cancelled")
	}
	if !def.Cancellable(committedTask(t, "", task.Running)) {
		t.Error("a task before its steps is cancellable as before")
	}
}

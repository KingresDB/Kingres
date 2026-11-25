// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/transfer"
)

// Unit tests of the exits of a paused import that need no database: the guard of the cancel, the notes, the servers that left, the archive that is closed.

// H1a: a paused import is cancellable, committed or not; the rest of the guard is as it was.
func TestImportCancellableOfAPausedImport(t *testing.T) {
	o := &Ops{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	def := o.dbImportDefinition()
	for _, mark := range []string{"commit", "", task.MarkUnreadable} {
		if !def.Cancellable(committedTask(t, mark, task.Paused)) {
			t.Errorf("a paused import with the mark %q is not cancellable", mark)
		}
	}
	if !def.Cancellable(committedTask(t, "commit", task.Pending)) {
		t.Error("a committed import that was resumed and waits for a run is not cancellable")
	}
	if !def.Cancellable(committedTask(t, "commit", task.Running)) {
		t.Error("a running committed import is not cancellable (the cancel pauses it)")
	}
	if def.CanceledNote == nil {
		t.Error("the import has no note for the cancel of a paused task")
	}
}

// M3: the instruction comes first and only the list of servers is shortened, whole names, with the number that was left out; a note never exceeds its budget.
func TestImportCommittedNoteKeepsTheInstructionAndShortensTheServers(t *testing.T) {
	var servers []string
	for i := 0; i < 40; i++ {
		servers = append(servers, fmt.Sprintf("server-u00e9-%02d", i))
	}
	for _, db := range []string{strings.Repeat("d", 63), "shop"} {
		for _, mark := range []string{"commit", task.MarkUnreadable} {
			p := dbImportParams{Database: db, Nonce: importTestNonce, Servers: servers, Origin: "origin-u00fc"}
			tk := task.Task{ID: 7, Kind: KindDatabaseImport, State: task.Paused, Input: mustJSON(t, p), Mark: mark}
			if mark == "commit" {
				tk.Mark = importCommitMark(p.private(7))
			}
			note, ok := importCommittedNote(tk)
			if !ok {
				t.Fatal("not committed")
			}
			if len(note) > importNoteBudget {
				t.Errorf("the note is %d bytes: %q", len(note), note)
			}
			if !utf8.ValidString(note) || !strings.HasPrefix(note, "Repeat the same import request to finish it") || !strings.Contains(note, db) {
				t.Errorf("the instruction is not first, the database is not named or the note is not text: %q", note)
			}
			if !strings.Contains(note, " more.") && !strings.HasSuffix(note, "41 servers.") {
				t.Errorf("the list of servers is not shortened with a count: %q", note)
			}
			if db == "shop" && (!strings.Contains(note, "server-u00e9-00") || !strings.Contains(note, " more.")) {
				t.Errorf("a short name keeps the first servers: %q", note)
			}
		}
	}
}

func TestShortListCutsWholeNames(t *testing.T) {
	names := []string{"alpha", "betaé", "gamma", "delta"}
	if got := shortList("on: ", names, 100); got != "on: alpha, betaé, gamma, delta" {
		t.Errorf("a list that fits: %q", got)
	}
	got := shortList("on: ", names, 20)
	if !utf8.ValidString(got) || len(got) > 20 || !strings.HasSuffix(got, " more") || !strings.HasPrefix(got, "on: alpha") {
		t.Errorf("a list that does not fit: %q", got)
	}
	if got := shortList("on: ", names, 6); got != "on: 4 servers" {
		t.Errorf("a list of which no name fits: %q", got)
	}
	if got := shortList("on: ", nil, 20); got != "on: none" {
		t.Errorf("an empty list: %q", got)
	}
}

// listFan is a fanout that only knows the members of the cluster.
type listFan struct {
	others []OtherServer
	err    error
}

func (f *listFan) Others(context.Context) ([]OtherServer, error)       { return f.others, f.err }
func (f *listFan) Apply(context.Context, string, string, any) error    { return nil }
func (f *listFan) Register(context.Context, string, OtherServer) error { return nil }

// H1d: a server that is no longer a member is not in the list, the starting server is in it only while it is a member, a member that cannot be reached stays, and
// when the members cannot be read nobody is dropped.
func TestImportServersSkipsTheServersThatLeft(t *testing.T) {
	p := dbImportParams{Servers: []string{"s2", "s3", "s4"}, Origin: "s1"}
	o := &Ops{ServerHost: "s9", Fanout: &listFan{others: []OtherServer{{ID: "s2"}, {ID: "s5"}}}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	got, left := o.importServersLeft(context.Background(), p)
	if strings.Join(got, ",") != "s2,s5" || strings.Join(left, ",") != "s3,s4,s1" {
		t.Errorf("servers %v, left %v", got, left)
	}
	o.Fanout = &listFan{others: []OtherServer{{ID: "s1"}, {ID: "s2"}, {ID: "s3"}, {ID: "s4"}}}
	if got, left := o.importServersLeft(context.Background(), p); strings.Join(got, ",") != "s2,s3,s4,s1" || len(left) != 0 {
		t.Errorf("all members: servers %v, left %v", got, left)
	}
	o.Fanout = &listFan{err: fmt.Errorf("the system database does not answer")}
	if got, left := o.importServersLeft(context.Background(), p); strings.Join(got, ",") != "s2,s3,s4,s1" || len(left) != 0 {
		t.Errorf("unreadable members: servers %v, left %v", got, left)
	}
}

type fakeRemote struct {
	rows map[string][][]any
	errs map[string]error
}

func (f fakeRemote) Query(_ context.Context, server, _, _ string) ([][]any, error) {
	return f.rows[server], f.errs[server]
}

// H1a: the message of a cancelled committed import starts with the instruction and names the servers where the database is open, where it is not and which could
// not be asked; it stays within the length of the task row for a long name and many servers.
func TestImportCancelNoteNamesTheServers(t *testing.T) {
	p := dbImportParams{Database: "shop", Owner: transfer.Identity{Name: "imp"}, Nonce: importTestNonce, Servers: []string{"s2", "s3", "s4"}, Origin: "s1"}
	tk := task.Task{ID: 7, Kind: KindDatabaseImport, State: task.Paused, Input: mustJSON(t, p), Mark: importCommitMark(p.private(7))}
	o := &Ops{ServerHost: "s9", Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Fanout: &listFan{others: []OtherServer{{ID: "s2"}, {ID: "s3"}, {ID: "s4"}, {ID: "s1"}}},
		Remote: fakeRemote{
			rows: map[string][][]any{"s2": {{"imp"}}, "s3": {{"postgres"}}, "s1": {}},
			errs: map[string]error{"s4": fmt.Errorf("%w: down", peerclient.ErrUnreachable)}}}
	note := o.importCancelNote(context.Background(), tk)
	for _, want := range []string{"Ended by a cancel after the commit point", "nothing was undone", "shop", "operator guide", "Open on: s2.", "Not open on: s3, s1.", "Not reached: s4."} {
		if !strings.Contains(note, want) {
			t.Errorf("the note %q lacks %q", note, want)
		}
	}
	// a long name and many servers: the instruction stays and the note is not cut
	p.Database = strings.Repeat("d", 63)
	p.Servers, o.Fanout = nil, &listFan{}
	var members []OtherServer
	rows := map[string][][]any{}
	for i := 0; i < 60; i++ {
		id := fmt.Sprintf("server-number-%03d", i)
		p.Servers = append(p.Servers, id)
		members = append(members, OtherServer{ID: id})
		rows[id] = [][]any{{"imp"}}
		if i%2 == 1 {
			rows[id] = [][]any{{"postgres"}}
		}
	}
	o.Fanout, o.Remote = &listFan{others: members}, fakeRemote{rows: rows}
	tk.Input = mustJSON(t, p)
	note = o.importCancelNote(context.Background(), tk)
	if len(note) > 500 || !utf8.ValidString(note) || !strings.HasPrefix(note, "Ended by a cancel after the commit point") || !strings.Contains(note, "operator guide") ||
		!strings.Contains(note, " more.") {
		t.Errorf("a long note (%d bytes): %q", len(note), note)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// H1c: before the commit point the wait for a server is bounded. A server that cannot be reached fails the step after ImportServerWait, with a message that says
// that the import was undone and what stays on that server; a server that answers in time is not a failure; the other kinds of task keep waiting.
func TestImportOnServerGivesUpOnAServerThatIsGone(t *testing.T) {
	old := unreachablePause
	unreachablePause = time.Millisecond
	defer func() { unreachablePause = old }()
	o := &Ops{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), ImportServerWait: 30 * time.Millisecond}
	calls := 0
	err := o.importOnServer(context.Background(), &task.Env{}, "s2", "take the final name", func() error {
		calls++
		return fmt.Errorf("%w: connection refused", peerclient.ErrUnreachable)
	})
	if err == nil || calls < 2 {
		t.Fatalf("err %v after %d calls", err, calls)
	}
	var xe *transfer.Error
	if !errors.As(err, &xe) || xe.Retryable {
		t.Fatalf("the error is not a final transfer error: %#v", err)
	}
	for _, want := range []string{"server s2 did not answer", "the import was undone", "private database", "keeps the server from being removed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message %q lacks %q", err.Error(), want)
		}
	}
	// a server that comes back in time is not a failure
	calls = 0
	if err := o.importOnServer(context.Background(), &task.Env{}, "s2", "take the final name", func() error {
		if calls++; calls < 3 {
			return fmt.Errorf("%w: down", peerclient.ErrUnreachable)
		}
		return nil
	}); err != nil || calls != 3 {
		t.Fatalf("a server that came back: %v after %d calls", err, calls)
	}
	// the unbounded wait of the other kinds of task is not changed: it ends only with the context
	ctx, stop := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer stop()
	if err := o.onServer(ctx, &task.Env{}, "s2", "x", func() error { return peerclient.ErrUnreachable }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("onServer gave up before its context ended: %v", err)
	}
}

// L3: the cancel note stays within its bound whatever the names: the instruction is whole, the three lists share what is left, and nothing is cut.
func TestImportCancelNoteWithManyLongNamesIsNotCut(t *testing.T) {
	p := dbImportParams{Database: strings.Repeat("d", 63), Owner: transfer.Identity{Name: "imp"}, Nonce: importTestNonce}
	tk := task.Task{ID: 7, Kind: KindDatabaseImport, State: task.Paused, Mark: task.MarkUnreadable}
	var members []OtherServer
	rows := map[string][][]any{}
	errs := map[string]error{}
	for i := 0; i < 90; i++ {
		id := fmt.Sprintf("a-server-with-a-quite-long-host-name-%03d.example.internal", i)
		p.Servers = append(p.Servers, id)
		members = append(members, OtherServer{ID: id})
		switch i % 3 {
		case 0:
			rows[id] = [][]any{{"imp"}}
		case 1:
			rows[id] = [][]any{{"postgres"}}
		default:
			errs[id] = fmt.Errorf("%w: down", peerclient.ErrUnreachable)
		}
	}
	tk.Input = mustJSON(t, p)
	o := &Ops{ServerHost: "s9", Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Fanout: &listFan{others: members}, Remote: fakeRemote{rows: rows, errs: errs}}
	note := o.importCancelNote(context.Background(), tk)
	if len(note) > importCancelNoteMax || !utf8.ValidString(note) || strings.HasSuffix(note, "...") {
		t.Errorf("the note is %d bytes or is cut: %q", len(note), note)
	}
	for _, want := range []string{"Ended by a cancel after the commit point", "operator guide says (a database import that cannot be finished).", "Open on: ", "Not open on: ", "Not reached: "} {
		if !strings.Contains(note, want) {
			t.Errorf("the note lacks %q: %q", want, note)
		}
	}
	if !strings.HasSuffix(note, ".") {
		t.Errorf("the last list is not whole: %q", note)
	}
}

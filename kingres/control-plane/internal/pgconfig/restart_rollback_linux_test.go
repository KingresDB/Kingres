// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

//go:build linux

package pgconfig

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
)

// fakeInstance builds an agent over a fake pg_ctl and a fake postgres. pg_ctl start fails while the override file
// holds the word BAD_START (a value the host cannot honour); postgres -C fails with a FATAL line while it holds
// BAD_PARSE (a value the parser refuses). The commands that run are appended to the returned log file.
func fakeInstance(t *testing.T) (a *Agent, id int, logFile string) {
	t.Helper()
	root, bin := t.TempDir(), t.TempDir()
	logFile = filepath.Join(root, "calls.log")
	script := `#!/bin/sh
cmd=$1; shift
D=""
while [ $# -gt 0 ]; do if [ "$1" = "-D" ]; then D=$2; fi; shift; done
pid="$D/postmaster.pid"
f="$D/kingres-config.conf"
echo "$cmd $(cat "$f" 2>/dev/null | tr '\n' ' ')" >> "` + logFile + `"
case "$cmd" in
status) [ -f "$pid" ] && exit 0; exit 3 ;;
start) if grep -q BAD_START "$f" 2>/dev/null; then echo "FATAL: could not map shared memory" >&2; exit 1; fi; echo 1 > "$pid"; exit 0 ;;
stop) rm -f "$pid"; exit 0 ;;
esac
exit 0
`
	pgScript := `#!/bin/sh
D=""
while [ $# -gt 0 ]; do if [ "$1" = "-D" ]; then D=$2; fi; shift; done
echo "postgres -C" >> "` + logFile + `"
if grep -q BAD_PARSE "$D/kingres-config.conf" 2>/dev/null; then echo "FATAL:  invalid value for parameter" >&2; exit 1; fi
exit 0
`
	for _, p := range []string{"pg_ctl", "initdb", "pg_basebackup", "pg_rewind"} {
		if err := os.WriteFile(filepath.Join(bin, p), []byte(script), 0o755); err != nil { //nolint:gosec // a test script
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bin, "postgres"), []byte(pgScript), 0o755); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	svc, err := cluster.New(cluster.Config{DataDir: root, BinDir: bin}, nil)
	if err != nil {
		t.Fatal(err)
	}
	id = 101
	m := svc.Manager()
	dir, _ := m.Dir(id)
	data, _ := m.DataDir(id)
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	mk, _ := json.Marshal(instance.Marker{NodeID: id, Port: 5433, CreatedAt: time.Now(), Magic: "kingresd-instance-v1"})
	if err := os.WriteFile(filepath.Join(dir, instance.MarkerName), mk, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "postmaster.pid"), []byte("1\n"), 0o600); err != nil { // running
		t.Fatal(err)
	}
	return &Agent{Svc: svc, Self: "s1", backWait: 300 * time.Millisecond}, id, logFile
}

// P13R-02: a restart that cannot start the instance puts the last file it ran with back and starts it again.
func TestRestartThatCannotStartPutsTheLastGoodFileBack(t *testing.T) {
	a, id, logFile := fakeInstance(t)
	m := a.mgr()
	if err := m.WriteConfig(id, "# good\nwork_mem = '8MB'\n"); err != nil {
		t.Fatal(err)
	}
	if err := m.Restart(context.Background(), id); err != nil { // an instance that started with this file: it is the last good one
		t.Fatal(err)
	}
	if err := m.WriteConfig(id, "# new\nshared_buffers = 'BAD_START'\n"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := a.RestartInstance(ctx, id, false)
	if err == nil || !strings.Contains(err.Error(), "did not start with the new configuration") {
		t.Fatalf("the restart must fail and say so: %v", err)
	}
	got, _ := m.ReadConfig(id)
	if strings.Contains(got, "BAD_START") || !strings.Contains(got, "work_mem") {
		t.Errorf("the last good file was not put back: %q", got)
	}
	if up, _ := m.Running(ctx, id); !up {
		t.Error("the instance must run again with the previous file")
	}
	calls, _ := os.ReadFile(logFile)
	if strings.Count(string(calls), "start") < 2 {
		t.Errorf("the instance was not started again: %q", calls)
	}
}

// P13R-02: a file that the parser of PostgreSQL refuses stops the restart before the instance is stopped.
func TestRestartIsRefusedWhenPostgreSQLRefusesTheFile(t *testing.T) {
	a, id, logFile := fakeInstance(t)
	m := a.mgr()
	if err := m.WriteConfig(id, "work_mem = 'BAD_PARSE'\n"); err != nil {
		t.Fatal(err)
	}
	err := a.RestartInstance(context.Background(), id, false)
	if err == nil || !strings.Contains(err.Error(), "was not restarted") {
		t.Fatalf("the restart must be refused: %v", err)
	}
	calls, _ := os.ReadFile(logFile)
	if strings.Contains(string(calls), "stop") {
		t.Errorf("the instance was stopped although the file is refused: %q", calls)
	}
	if up, _ := m.Running(context.Background(), id); !up {
		t.Error("the instance must still run")
	}
}

// P13R-13: a value that the parser of PostgreSQL refuses is not left in the file of an instance that is not running
// (it would stop the instance from starting), and the apply says which values were refused.
func TestApplyToAStoppedInstanceChecksTheFile(t *testing.T) {
	a, id, _ := fakeInstance(t)
	m := a.mgr()
	data, _ := m.DataDir(id)
	_ = os.Remove(filepath.Join(data, "postmaster.pid")) // stopped
	if err := os.WriteFile(filepath.Join(data, "kingres-managed.conf"), []byte("include_if_exists 'kingres-config.conf'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteConfig(id, "# before\n"); err != nil {
		t.Fatal(err)
	}
	res, err := a.Apply(context.Background(), ApplyRequest{Revision: 3, Values: map[string]string{"work_mem": "BAD_PARSE"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.State != StateFailed || len(res.Parameters) != 1 || res.Parameters[0].State != StateFailed || !strings.Contains(res.Parameters[0].Error, "does not accept") {
		t.Errorf("the refused file must fail the apply: %+v", res)
	}
	if got, _ := m.ReadConfig(id); strings.Contains(got, "BAD_PARSE") {
		t.Errorf("the refused value stayed in the file: %q", got)
	}
	// A value the parser accepts is written and the instance is reported as waiting.
	res, err = a.Apply(context.Background(), ApplyRequest{Revision: 4, Values: map[string]string{"work_mem": "8MB"}})
	if err != nil || res.State != StateWaiting {
		t.Fatalf("%+v %v", res, err)
	}
	if got, _ := m.ReadConfig(id); !strings.Contains(got, "work_mem = '8MB'") {
		t.Errorf("the file: %q", got)
	}
}

// P13R-02: CheckConfig names the complaint of PostgreSQL.
func TestCheckConfigNamesTheComplaint(t *testing.T) {
	a, id, _ := fakeInstance(t)
	m := a.mgr()
	if err := m.WriteConfig(id, "work_mem = 'BAD_PARSE'\n"); err != nil {
		t.Fatal(err)
	}
	if err := m.CheckConfig(context.Background(), id); err == nil || !strings.Contains(err.Error(), "invalid value") {
		t.Errorf("CheckConfig: %v", err)
	}
	if err := m.WriteConfig(id, "work_mem = '8MB'\n"); err != nil {
		t.Fatal(err)
	}
	if err := m.CheckConfig(context.Background(), id); err != nil {
		t.Errorf("a clean file is refused: %v", err)
	}
}

func readGood(t *testing.T, a *Agent, id int) string {
	t.Helper()
	p, _ := a.mgr().ConfigPath(id)
	b, err := os.ReadFile(p + ".good")
	if err != nil {
		t.Fatalf("no last good file: %v", err)
	}
	return string(b)
}

// P13S-01: every successful start keeps the file it started with as the last good one.
func TestASuccessfulStartKeepsTheFileAsGood(t *testing.T) {
	a, id, _ := fakeInstance(t)
	m := a.mgr()
	if err := m.WriteConfig(id, "work_mem = '8MB'\n"); err != nil {
		t.Fatal(err)
	}
	if err := m.Restart(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if got := readGood(t, a, id); !strings.Contains(got, "work_mem") {
		t.Errorf("the last good file: %q", got)
	}
}

// P13S-01: two changes before one restart. The first holds a value the host cannot honour; the file that the second
// change replaced (the .prev copy) is that bad one, and the rollback must never use it.
func TestTwoWritesBeforeARestartRollBackToTheLastGoodFile(t *testing.T) {
	a, id, _ := fakeInstance(t)
	m := a.mgr()
	if err := m.WriteConfig(id, "# good\nwork_mem = '8MB'\n"); err != nil {
		t.Fatal(err)
	}
	if err := m.Restart(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteConfig(id, "work_mem = '8MB'\nshared_buffers = 'BAD_START'\n"); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteConfig(id, "work_mem = '9MB'\nshared_buffers = 'BAD_START'\n"); err != nil {
		t.Fatal(err)
	}
	err := a.RestartInstance(context.Background(), id, false)
	if err == nil || !strings.Contains(err.Error(), "did not start with the new configuration") {
		t.Fatalf("the restart must fail and say so: %v", err)
	}
	if got, _ := m.ReadConfig(id); strings.Contains(got, "BAD_START") || !strings.Contains(got, "8MB") {
		t.Errorf("the last good file was not put back: %q", got)
	}
	if up, _ := m.Running(context.Background(), id); !up {
		t.Error("the instance must run again")
	}
}

// P13S-01: when no file was kept (an instance that was never started by kingresd) the rollback writes the empty file, not the
// previous one.
func TestRollbackWithoutAGoodFileWritesTheEmptyFile(t *testing.T) {
	a, id, _ := fakeInstance(t)
	m := a.mgr()
	if err := m.WriteConfig(id, "shared_buffers = 'BAD_START'\n"); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteConfig(id, "shared_buffers = 'BAD_START'\nwork_mem = '9MB'\n"); err != nil {
		t.Fatal(err)
	}
	err := a.RestartInstance(context.Background(), id, false)
	if err == nil || !strings.Contains(err.Error(), "did not start with the new configuration") {
		t.Fatalf("the restart must fail and say so: %v", err)
	}
	if got, _ := m.ReadConfig(id); got != instance.EmptyConfigText {
		t.Errorf("the file after the rollback: %q", got)
	}
}

// P13S-01: the second start fails as well (the last good file is no good either): the task fails and says so, and nothing loops.
func TestRollbackWhoseSecondStartFailsToo(t *testing.T) {
	a, id, _ := fakeInstance(t)
	m := a.mgr()
	p, _ := m.ConfigPath(id)
	if err := os.WriteFile(p+".good", []byte("BAD_START old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteConfig(id, "BAD_START new\n"); err != nil {
		t.Fatal(err)
	}
	err := a.RestartInstance(context.Background(), id, false)
	if err == nil || !strings.Contains(err.Error(), "did not start") || strings.Contains(err.Error(), "runs again") {
		t.Fatalf("the restart must fail without claiming the instance runs: %v", err)
	}
	if up, _ := m.Running(context.Background(), id); up {
		t.Error("the instance cannot run with a file that does not start")
	}
}

// P13S-01: a start that is not a restart of the task (a reboot, a crash, an operator) goes back to the last good file as well.
func TestAnOrdinaryStartGoesBackToTheLastGoodFile(t *testing.T) {
	a, id, _ := fakeInstance(t)
	m := a.mgr()
	ctx := context.Background()
	if err := m.WriteConfig(id, "work_mem = '8MB'\n"); err != nil {
		t.Fatal(err)
	}
	if err := m.Restart(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteConfig(id, "shared_buffers = 'BAD_START'\n"); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(ctx, id); err != nil {
		t.Fatalf("the start must go back to the last good file: %v", err)
	}
	if got, _ := m.ReadConfig(id); strings.Contains(got, "BAD_START") {
		t.Errorf("the file: %q", got)
	}
	if why, ok := m.TakeRollbackMark(id); !ok || !strings.Contains(why, "shared memory") {
		t.Errorf("the mark: %q %v", why, ok)
	}
}

// P13S-05: the hash that the leader compares covers the override file of every instance of the server: a standby that holds
// another file makes the server differ, and a standby that has no data directory yet does not.
func TestConfigIdentityCoversEveryInstance(t *testing.T) {
	a, id, _ := fakeInstance(t)
	m := a.mgr()
	text := instance.EmptyConfigText + "# kingresd configuration revision 7\nwork_mem = '8MB'\n"
	if err := m.WriteConfig(id, text); err != nil {
		t.Fatal(err)
	}
	rev, h := a.configIdentity()
	if h != HashOf(text) || rev != 7 {
		t.Fatalf("one instance: %d %q", rev, h)
	}
	// A standby that is being created has no data directory: it is not looked at.
	const sid = 102
	dir, _ := m.Dir(sid)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	mk, _ := json.Marshal(instance.Marker{NodeID: sid, Port: 5434, CreatedAt: time.Now(), Magic: "kingresd-instance-v1"})
	if err := os.WriteFile(filepath.Join(dir, instance.MarkerName), mk, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, h := a.configIdentity(); h != HashOf(text) {
		t.Errorf("an instance without a data directory changed the hash: %q", h)
	}
	// Its file is another one (a copy of another server's): the server differs.
	data, _ := m.DataDir(sid)
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteConfig(sid, instance.EmptyConfigText+"# kingresd configuration revision 3\nwork_mem = '99MB'\n"); err != nil {
		t.Fatal(err)
	}
	rev, h = a.configIdentity()
	if !strings.HasPrefix(h, "mixed:") || rev != 3 {
		t.Errorf("two different files: %d %q", rev, h)
	}
	// The same file on both: the hash of that file again.
	if err := m.WriteConfig(sid, text); err != nil {
		t.Fatal(err)
	}
	if _, h := a.configIdentity(); h != HashOf(text) {
		t.Errorf("equal files: %q", h)
	}
}

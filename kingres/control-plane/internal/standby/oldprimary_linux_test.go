// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

//go:build linux

package standby

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
)

// formerRig is a server whose home instance (directory 1) was the primary of shard group 1 and is stopped, with fake
// PostgreSQL programs: pg_ctl keeps a pid file, pg_rewind exits with the code in <root>/rewind-exit (default 0) and
// prints a dry-run line, pg_basebackup makes the data directory. Every call is appended to <root>/calls.log.
type formerRig struct {
	a      *Agent
	m      *instance.Manager
	root   string
	e      Entry
	mu     sync.Mutex
	became []int32
	holds  error
	slots  int
}

func newFormerRig(t *testing.T) *formerRig {
	t.Helper()
	root, bin := t.TempDir(), t.TempDir()
	script := `#!/bin/sh
ROOT="` + root + `"
prog=$(basename "$0")
echo "$prog $*" >> "$ROOT/calls.log"
case "$prog" in
pg_ctl)
  cmd=$1; shift
  D=""
  while [ $# -gt 0 ]; do if [ "$1" = "-D" ]; then D=$2; fi; shift; done
  pid="$D/postmaster.pid"
  case "$cmd" in
  status) [ -f "$pid" ] && exit 0; exit 3 ;;
  start) echo 1 > "$pid"; exit 0 ;;
  stop) rm -f "$pid"; exit 0 ;;
  esac
  exit 0 ;;
pg_rewind)
  for a in "$@"; do if [ "$a" = "--dry-run" ]; then if [ -f "$ROOT/dryrun-fail-once" ]; then rm "$ROOT/dryrun-fail-once"; echo "pg_rewind: error: target server must be shut down cleanly"; exit 1; fi; echo "pg_rewind: servers diverged at WAL location 0/3000060 on timeline 1"; echo "pg_rewind: need to copy 7 MB (total source directory size is 30 MB)"; exit 0; fi; done
  for a in "$@"; do case "$a" in --target-pgdata=*) T=${a#--target-pgdata=};; esac; done
  [ -f "$(dirname "$T")/kingres-copy-unfinished" ] && echo "mark present at rewind" >> "$ROOT/calls.log"
  [ -f "$ROOT/rewind-exit" ] && exit $(cat "$ROOT/rewind-exit")
  exit 0 ;;
pg_basebackup)
  D=""
  while [ $# -gt 0 ]; do if [ "$1" = "-D" ]; then D=$2; fi; shift; done
  mkdir -p "$D" && echo 18 > "$D/PG_VERSION"; exit 0 ;;
pg_controldata)
  echo "Latest checkpoint location:           0/3100000"; echo "Database cluster state:               shut down"; exit 0 ;;
esac
exit 0
`
	for _, p := range []string{"pg_ctl", "initdb", "pg_basebackup", "pg_rewind", "pg_controldata", "postgres"} {
		if err := os.WriteFile(filepath.Join(bin, p), []byte(script), 0o755); err != nil { //nolint:gosec // a test script
			t.Fatal(err)
		}
	}
	svc, err := cluster.New(cluster.Config{DataDir: root, BinDir: bin}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := &formerRig{root: root, m: svc.Manager()}
	a, err := NewAgent(&Agent{Svc: svc, Self: "s1", DataDir: root})
	if err != nil {
		t.Fatal(err)
	}
	r.a = a
	a.Conninfo = func(host string, port int, app, db string) (string, error) {
		return "host=" + host + " port=" + itoa(port) + " application_name=" + app, nil
	}
	a.EnsureSlot = func(context.Context, Entry, string) error { r.mu.Lock(); r.slots++; r.mu.Unlock(); return nil }
	a.FormerPrimary = func(int32) error { return r.holds }
	a.BecameStandby = func(g int32, id int) { r.mu.Lock(); r.became = append(r.became, g); r.mu.Unlock() }
	// directory 1: the former primary of group 1
	dir, _ := r.m.Dir(1)
	data, _ := r.m.DataDir(1)
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	mk, _ := json.Marshal(instance.Marker{NodeID: 1, Port: 5433, CreatedAt: time.Now(), Magic: "kingresd-instance-v1"})
	if err := os.WriteFile(filepath.Join(dir, instance.MarkerName), mk, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.m.WriteRole(1, instance.Role{Group: 1, Role: instance.RolePrimary, Origin: instance.OriginOwn, Term: 0}); err != nil {
		t.Fatal(err)
	}
	settings := instance.ManagedSettings{Port: 5433, Extra: map[string]string{"synchronous_standby_names": "ANY 1 (kingres_standby_9)", "shared_buffers": "16MB"}}
	if err := r.m.WriteManaged(1, settings); err != nil {
		t.Fatal(err)
	}
	// The entry names the directory (the leader learned it from the old primary's server or from an earlier rebuild).
	r.e = Entry{ID: 12, Group: 1, Server: "s1", PrimaryServer: "s2", PrimaryPort: 5434, Generation: 1, InstanceID: 1}
	return r
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func (r *formerRig) calls() string {
	b, _ := os.ReadFile(filepath.Join(r.root, "calls.log"))
	return string(b)
}

func (r *formerRig) role(t *testing.T) instance.Role {
	t.Helper()
	ro, _, err := r.m.RoleOf(1)
	if err != nil {
		t.Fatal(err)
	}
	return ro
}

// The refusals: a primary that is not proved to be the former primary of the group is not touched (role file,
// data directory and calls to the PostgreSQL programs stay as they were).
func TestAPrimaryThatIsNotProvedToBeTheFormerPrimaryIsNotRebuilt(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		setup func(r *formerRig)
		term  int64
		want  error
	}{
		{"the instance runs", func(r *formerRig) { _ = r.m.Start(ctx, 1) }, 3, ErrOldPrimaryRunning},
		{"another group's role file", func(r *formerRig) {
			_ = r.m.WriteRole(1, instance.Role{Group: 2, Role: instance.RolePrimary, Origin: instance.OriginOwn})
		}, 3, ErrNotFormerPrimary},
		{"this server holds the lease", func(r *formerRig) { r.holds = errors.New("this server holds the lease of the group") }, 3, ErrNotFormerPrimary},
		{"the plan names this server the primary", func(r *formerRig) { r.e.PrimaryServer = "s1" }, 3, ErrNotFormerPrimary},
		{"the term is not newer than the role file's", func(r *formerRig) {
			_ = r.m.WriteRole(1, instance.Role{Group: 1, Role: instance.RolePrimary, Origin: instance.OriginOwn, Term: 3})
		}, 3, ErrNotFormerPrimary},
		{"no term asked under", func(*formerRig) {}, 0, ErrNotFormerPrimary},
		{"no proof of the lease at all", func(r *formerRig) { r.a.FormerPrimary = nil }, 3, ErrNotFormerPrimary},
	} {
		r := newFormerRig(t)
		tc.setup(r)
		before := r.role(t)
		_, err := r.a.rebuild(ctx, r.e, true, tc.term)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, err, tc.want)
		}
		if r.role(t) != before {
			t.Errorf("%s: the role file changed to %+v", tc.name, r.role(t))
		}
		if strings.Contains(r.calls(), "pg_rewind") || strings.Contains(r.calls(), "pg_basebackup") {
			t.Errorf("%s: a program was run on the directory:\n%s", tc.name, r.calls())
		}
		if r.m.CopyUnfinished(1) {
			t.Errorf("%s: the mark of an unfinished copy was written", tc.name)
		}
	}
}

// The rebuild with pg_rewind: the order is mark, role file, measure, rewind, the standby's own managed settings (the
// rewind copies the new primary's over them), meta, the markers of the old primary cleared, the mark removed, start.
func TestTheFormerPrimaryIsRewoundAndBecomesAStandby(t *testing.T) {
	ctx := context.Background()
	r := newFormerRig(t)
	res, err := r.a.rebuild(ctx, r.e, true, 3)
	if err != nil {
		t.Fatal(err)
	}
	if res.OldPrimary == nil || res.OldPrimary.Method != "pgRewind" {
		t.Fatalf("answer: %+v", res.OldPrimary)
	}
	if res.OldPrimary.DivergenceLsn != "0/3000060" || res.OldPrimary.OldEndLsn != "0/3100000" {
		t.Errorf("divergence and end of the old WAL: %+v", res.OldPrimary)
	}
	if res.OldPrimary.RewindBytes == nil || *res.OldPrimary.RewindBytes != 7<<20 {
		t.Errorf("rewind bytes: %+v", res.OldPrimary.RewindBytes)
	}
	if res.OldPrimary.DivergenceBytes == nil || *res.OldPrimary.DivergenceBytes != 0x3100000-0x3000060 {
		t.Errorf("divergence bytes: %+v", res.OldPrimary.DivergenceBytes)
	}
	if res.InstanceID != 1 {
		t.Errorf("instance: %d", res.InstanceID)
	}
	ro := r.role(t)
	if ro.Role != instance.RoleStandby || ro.Origin != instance.OriginCopy || ro.Group != 1 || ro.Term != 3 {
		t.Errorf("role: %+v", ro)
	}
	if r.m.CopyUnfinished(1) {
		t.Error("the mark of an unfinished copy is still there")
	}
	if m, err := r.a.metaOf(1); err != nil || m.ID != 12 || m.InstanceID != 1 {
		t.Errorf("meta: %+v %v", m, err)
	}
	data, _ := r.m.DataDir(1)
	if _, err := os.Stat(filepath.Join(data, "kingres-managed.conf")); err != nil {
		t.Errorf("no managed configuration: %v", err)
	}
	conf, _ := os.ReadFile(filepath.Join(data, "kingres-managed.conf"))
	if !strings.Contains(string(conf), "primary_conninfo") || !strings.Contains(string(conf), "kingres_standby_12") {
		t.Errorf("the managed configuration does not point at the new primary:\n%s", conf)
	}
	if strings.Contains(string(conf), "synchronous_standby_names") {
		t.Errorf("the standby keeps the list of the old primary:\n%s", conf)
	}
	if len(r.became) != 1 || r.became[0] != 1 {
		t.Errorf("BecameStandby: %v", r.became)
	}
	if up, _ := r.m.Running(ctx, 1); !up {
		t.Error("the standby was not started")
	}
	c := r.calls()
	dry := strings.Index(c, "--dry-run")
	rewind := strings.LastIndex(c, "pg_rewind --progress --target-pgdata")
	if dry < 0 || rewind < 0 || dry > rewind {
		t.Errorf("the divergence is measured before the rewind:\n%s", c)
	}
	if !strings.Contains(c, "mark present at rewind") {
		t.Errorf("the mark of an unfinished copy was not on disk when pg_rewind ran:\n%s", c)
	}
	if strings.Contains(c, "pg_basebackup") {
		t.Errorf("a base backup was taken although pg_rewind worked:\n%s", c)
	}
	// A repeat of the call (its answer was lost) is a no-op that only makes sure the instance runs.
	calls := len(r.calls())
	if _, err := r.a.rebuild(ctx, r.e, true, 3); err != nil {
		t.Fatalf("repeat: %v", err)
	}
	if len(r.calls()) != calls && strings.Contains(r.calls()[calls:], "pg_rewind") {
		t.Error("the repeat rewound again")
	}
}

// pg_rewind fails: the data directory is replaced by a base backup and the answer says so.
func TestTheFallbackIsABaseBackup(t *testing.T) {
	ctx := context.Background()
	r := newFormerRig(t)
	if err := os.WriteFile(filepath.Join(r.root, "rewind-exit"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := r.a.rebuild(ctx, r.e, true, 3)
	if err != nil {
		t.Fatal(err)
	}
	if res.OldPrimary == nil || res.OldPrimary.Method != "baseBackup" || !strings.Contains(res.OldPrimary.Message, "pg_rewind was not possible") {
		t.Fatalf("answer: %+v", res.OldPrimary)
	}
	data, _ := r.m.DataDir(1)
	if b, _ := os.ReadFile(filepath.Join(data, "PG_VERSION")); !strings.Contains(string(b), "18") {
		t.Error("the data directory is not the one of the base backup")
	}
	if _, err := os.Stat(filepath.Join(data, "standby.signal")); err != nil {
		t.Error("no standby.signal after the base backup")
	}
	if r.role(t).Role != instance.RoleStandby || r.m.CopyUnfinished(1) {
		t.Errorf("role %+v unfinished %v", r.role(t), r.m.CopyUnfinished(1))
	}
}

// A crash in the middle of the rewind leaves the directory with the standby role file and the mark at the step
// "rewinding": the next attempt does not trust the half rewound data and takes a base backup; a crash after the
// rewind finished (step "rewound") is not rewound again.
func TestAnInterruptedRewindIsRepairedByABaseBackupAndAFinishedOneIsNotRepeated(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		step       string
		wantMethod string
		wantRewind bool
	}{
		{instance.CopyRewinding, "baseBackup", false},
		{instance.CopyRewound, "pgRewind", false},
		{instance.CopyStarted, "pgRewind", true},
	} {
		r := newFormerRig(t)
		// the state a crash leaves: role standby, the mark with its step, the data
		if err := r.m.MarkCopyUnfinished(1); err != nil {
			t.Fatal(err)
		}
		if err := r.m.SetCopyStep(1, tc.step); err != nil {
			t.Fatal(err)
		}
		if err := r.m.WriteRole(1, instance.Role{Group: 1, Role: instance.RoleStandby, Origin: instance.OriginCopy, Term: 3}); err != nil {
			t.Fatal(err)
		}
		// the proof is the role file and the mark: no lease or term is asked for
		r.a.FormerPrimary = func(int32) error { return errors.New("must not be asked") }
		res, err := r.a.rebuild(ctx, r.e, true, 3)
		if err != nil {
			t.Fatalf("%s: %v", tc.step, err)
		}
		if res.OldPrimary == nil || res.OldPrimary.Method != tc.wantMethod {
			t.Errorf("%s: method %+v, want %s", tc.step, res.OldPrimary, tc.wantMethod)
		}
		real := strings.Contains(r.calls(), "pg_rewind --progress --target-pgdata")
		if real != tc.wantRewind {
			t.Errorf("%s: pg_rewind run = %v, want %v\n%s", tc.step, real, tc.wantRewind, r.calls())
		}
		if tc.wantMethod == "baseBackup" != strings.Contains(r.calls(), "pg_basebackup") {
			t.Errorf("%s: base backup mismatch:\n%s", tc.step, r.calls())
		}
	}
}

// The agent never rewinds a standby it cannot prove: a role file that is broken stays untouched also for a diverged
// rebuild (the proof of a standby, FO-04, applies to the ordinary diverged standby as before).
func TestADivergedRebuildOfADirectoryThatIsNotProvedIsRefused(t *testing.T) {
	ctx := context.Background()
	r := newFormerRig(t)
	dir, _ := r.m.Dir(1)
	if err := os.WriteFile(filepath.Join(dir, instance.RoleFileName), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.a.rebuild(ctx, r.e, true, 3); !errors.Is(err, ErrNotProvedStandby) {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(r.calls(), "pg_rewind") {
		t.Errorf("pg_rewind ran:\n%s", r.calls())
	}
}

func TestParsingWhatPgRewindPrints(t *testing.T) {
	lsn, mb := ParseRewind("pg_rewind: servers diverged at WAL location 0/3000060 on timeline 1\npg_rewind: need to copy 55 MB (total source directory size is 90 MB)\n")
	if lsn != "0/3000060" || mb == nil || *mb != 55 {
		t.Fatalf("%q %v", lsn, mb)
	}
	if lsn, mb := ParseRewind("pg_rewind: no rewind required\n"); lsn != "" || mb != nil {
		t.Fatalf("no divergence: %q %v", lsn, mb)
	}
	if d, ok := lsnDiff("0/3000060", "1/0"); !ok || d != 0x100000000-0x3000060 {
		t.Fatalf("%d %v", d, ok)
	}
}

// The leader may not know the directory of the old primary (its server did not answer when the failover recorded the
// standby): the server finds it by the role file, and says which it was.
func TestTheDirectoryOfTheFormerPrimaryIsFoundByItsRoleFile(t *testing.T) {
	r := newFormerRig(t)
	r.e.InstanceID = 0
	res, err := r.a.rebuild(context.Background(), r.e, true, 3)
	if err != nil {
		t.Fatal(err)
	}
	if res.InstanceID != 1 || r.role(t).Role != instance.RoleStandby {
		t.Fatalf("instance %d, role %+v", res.InstanceID, r.role(t))
	}
}

// A primary that was fenced needs crash recovery: the first pg_rewind --dry-run does it and then refuses ("target server
// must be shut down cleanly"); the divergence is measured by the second one.
func TestTheDivergenceIsMeasuredAgainAfterTheCrashRecoveryOfTheFirstDryRun(t *testing.T) {
	r := newFormerRig(t)
	if err := os.WriteFile(filepath.Join(r.root, "dryrun-fail-once"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := r.a.rebuild(context.Background(), r.e, true, 3)
	if err != nil {
		t.Fatal(err)
	}
	if res.OldPrimary == nil || res.OldPrimary.DivergenceLsn != "0/3000060" || res.OldPrimary.DivergenceBytes == nil {
		t.Fatalf("the divergence was not measured by the second dry run: %+v", res.OldPrimary)
	}
	if n := strings.Count(r.calls(), "--dry-run"); n != 2 {
		t.Errorf("%d dry runs, want 2", n)
	}
}

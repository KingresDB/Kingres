// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package standby

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
)

// F2-08, leg 1: an unfinished copy is recognised by the positive mark InitStandby writes first, not by a meta file that
// cannot be read. A directory whose role file says standby and whose bookkeeping file is broken, with no mark and no
// standby.signal, is not proved to be anything: it is not deleted (the probe of the review deleted it).
func TestAMetaFileThatCannotBeReadIsNotTheMarkOfAnUnfinishedCopy(t *testing.T) {
	ctx := context.Background()
	a, m, e, data := fakeAgent(t)
	const id = 101
	dir, _ := m.Dir(id)
	if err := m.WriteRole(id, instance.Role{Group: 1, Role: instance.RoleStandby, Origin: instance.OriginCopy}); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(data, "standby.signal"))
	if err := os.WriteFile(filepath.Join(dir, metaFile), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.Remove(ctx, e); !errors.Is(err, ErrNotProvedStandby) {
		t.Fatalf("Remove of a directory with a broken meta file and no mark = %v, want ErrNotProvedStandby", err)
	}
	home, _ := m.Dir(1)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	mk, _ := json.Marshal(instance.Marker{NodeID: 1, Port: 5432, CreatedAt: time.Now(), Magic: "kingresd-instance-v1"})
	if err := os.WriteFile(filepath.Join(home, instance.MarkerName), mk, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Create(ctx, e); !errors.Is(err, ErrNotProvedStandby) {
		t.Fatalf("Create over it = %v, want ErrNotProvedStandby", err)
	}
	if !m.Exists(id) {
		t.Fatal("the directory was deleted")
	}
	// With the positive mark it is an unfinished copy and goes.
	if err := m.MarkCopyUnfinished(id); err != nil {
		t.Fatal(err)
	}
	if err := a.Remove(ctx, e); err != nil && !errors.Is(err, ErrNotProvedStandby) {
		t.Log("the slot of the fake setup cannot be dropped:", err)
	}
	if m.Exists(id) {
		t.Fatal("an unfinished copy (positive mark) was not removed")
	}
}

// F2-08, leg 2: the two legs of a running instance. A running directory is a standby only when it answers that it is in
// recovery: "not in recovery" (a promoted instance) and "cannot be asked" both refuse.
func TestARunningInstanceIsProvedAStandbyOnlyByItsOwnAnswer(t *testing.T) {
	ctx := context.Background()
	a, m, e, data := fakeAgent(t)
	const id = 101
	if err := m.WriteRole(id, instance.Role{Group: 1, Role: instance.RoleStandby, Origin: instance.OriginCopy}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "standby.signal"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(ctx, id); err != nil { // the fake pg_ctl writes postmaster.pid
		t.Fatal(err)
	}
	if up, _ := m.Running(ctx, id); !up {
		t.Fatal("the fake instance does not run")
	}

	a.AskRecovery = func(context.Context, int) (bool, error) { return false, nil }
	if err := a.Remove(ctx, e); !errors.Is(err, ErrNotProvedStandby) {
		t.Fatalf("a running instance that is not in recovery: Remove = %v", err)
	}
	a.AskRecovery = func(context.Context, int) (bool, error) { return false, errors.New("connection refused") }
	if err := a.Remove(ctx, e); !errors.Is(err, ErrNotProvedStandby) {
		t.Fatalf("a running instance that cannot be asked: Remove = %v", err)
	}
	if !m.Exists(id) {
		t.Fatal("the directory was deleted")
	}
	a.AskRecovery = func(context.Context, int) (bool, error) { return true, nil }
	if err := a.provedStandby(ctx, id, false); err != nil {
		t.Fatalf("a running instance in recovery with the role standby is proved: %v", err)
	}
}

// F2-08, leg 3: a promotion takes the agent's lifecycle lock, so the proof of a removal and its delete are never
// interleaved with it.
func TestThePromotionWaitsForARemoveInProgress(t *testing.T) {
	a, _, _, _ := fakeAgent(t)
	a.opMu.Lock() // a remove or rebuild is running
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	go func() {
		unlock, err := a.LockLifecycle(ctx)
		if err == nil {
			unlock()
		}
		got <- err
	}()
	cancel() // the caller gives up while the lock is held elsewhere
	if err := <-got; err == nil {
		t.Fatal("the lifecycle lock was granted while a remove was running")
	}
	a.opMu.Unlock()
	unlock, err := a.LockLifecycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}

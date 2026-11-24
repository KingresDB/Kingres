// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scram"
)

func keyOf(seed byte) []byte { return scram.Stored(lostKey(seed)) }

// Phase 5 review F1: a read that finds no administrator, or that fails, does not touch the cache; the instance that is read is the one of the map group and no other.
func TestTheCacheOfTheAdministratorsKeysIsNeverEmptiedByAWrongRead(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	a := openAdminKeys(dir, nil, 0, nil)
	if err := a.Replace(map[string][]byte{"alice": keyOf(1), "bob": keyOf(2)}); err != nil {
		t.Fatal(err)
	}
	// the read of a shard instance answers no administrator: the cache is kept
	if err := a.refresh(ctx, func(context.Context) (map[string]string, error) { return map[string]string{}, nil }); err == nil {
		t.Fatal("an empty answer replaced the cache")
	}
	if err := a.Replace(nil); err == nil {
		t.Fatal("an empty set replaced the cache")
	}
	if err := a.refresh(ctx, func(context.Context) (map[string]string, error) { return nil, errors.New("down") }); err == nil {
		t.Fatal("a failed read was not reported")
	}
	reopened := openAdminKeys(dir, nil, 0, nil)
	for _, u := range []string{"alice", "bob"} {
		if _, err := reopened.StoredKey(ctx, u); err != nil {
			t.Fatalf("%s is gone after the wrong reads: %v", u, err)
		}
	}
	// an administrator that was removed leaves the cache and the file at the next good read
	if err := a.Replace(map[string][]byte{"alice": keyOf(1)}); err != nil {
		t.Fatal(err)
	}
	reopened = openAdminKeys(dir, nil, 0, nil)
	if _, err := reopened.StoredKey(ctx, "bob"); !errors.Is(err, scram.ErrNotSCRAM) {
		t.Fatalf("a removed administrator is still in the file: %v", err)
	}
	if _, err := reopened.StoredKey(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
}

func TestOnlyTheInstanceOfTheMapGroupIsRead(t *testing.T) {
	li := func(id, group int, role instance.Role, port int, unfinished bool) cluster.LocalInstance {
		role.Group = int32(group)
		return cluster.LocalInstance{ID: id, Port: port, Role: role, Unfinished: unfinished}
	}
	primary := instance.Role{Role: instance.RolePrimary}
	standby := instance.Role{Role: instance.RoleStandby}
	// the group-0 primary is stopped for the hold, a shard primary runs: nothing is read
	if id, ok := pickMapGroupInstance([]cluster.LocalInstance{li(2, 3, primary, 5000, false)}); ok {
		t.Fatalf("a shard primary was chosen: %d", id)
	}
	if id, ok := pickMapGroupInstance([]cluster.LocalInstance{li(2, 3, primary, 5000, false), li(4, 0, standby, 5001, false)}); !ok || id != 4 {
		t.Fatalf("the standby of the map group was not chosen: %d %v", id, ok)
	}
	if id, ok := pickMapGroupInstance([]cluster.LocalInstance{li(4, 0, standby, 5001, false), li(1, 0, primary, 5000, false)}); !ok || id != 1 {
		t.Fatalf("the primary of the map group is preferred: %d %v", id, ok)
	}
	if _, ok := pickMapGroupInstance([]cluster.LocalInstance{li(4, 0, standby, 5001, true), li(5, 0, standby, 0, false)}); ok {
		t.Fatal("an unfinished copy or an instance without a port was chosen")
	}
}

// Phase 5 review F2: the cache is refused when it is older than the bound or of another cluster, a file of another cluster is dropped at the start, and the age counts from
// the last good read.
func TestTheCacheOfTheAdministratorsKeysHasAnAgeAndACluster(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	clusterID := "c1"
	now := time.Now()
	a := openAdminKeys(dir, func() string { return clusterID }, 72*time.Hour, nil)
	a.now = func() time.Time { return now }
	if err := a.Replace(map[string][]byte{"alice": keyOf(1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.StoredKey(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	// 71 hours without a read: still good; a good read renews the age without a change of the set
	now = now.Add(71 * time.Hour)
	if _, err := a.StoredKey(ctx, "alice"); err != nil {
		t.Fatalf("71 hours: %v", err)
	}
	if err := a.Replace(map[string][]byte{"alice": keyOf(1)}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(71 * time.Hour)
	if _, err := a.StoredKey(ctx, "alice"); err != nil {
		t.Fatalf("a good read did not renew the age: %v", err)
	}
	// more than the bound
	now = now.Add(2 * time.Hour)
	if _, err := a.StoredKey(ctx, "alice"); err == nil || !strings.Contains(err.Error(), "admin-keys-max-age") {
		t.Fatalf("a cache older than the bound was accepted: %v", err)
	}
	// the file keeps the time of the last write: a restart does not make it younger
	re := openAdminKeys(dir, func() string { return clusterID }, 72*time.Hour, nil)
	re.now = func() time.Time { return now }
	if _, err := re.StoredKey(ctx, "alice"); err == nil {
		t.Fatal("a restart made an old cache young")
	}
	// there is no unbounded mode: a bound of 0 is the default (72 hours), and the cache is refused beyond it
	nb := openAdminKeys(dir, func() string { return clusterID }, 0, nil)
	nb.now = func() time.Time { return now.Add(1000 * time.Hour) }
	if _, err := nb.StoredKey(ctx, "alice"); err == nil {
		t.Fatal("a bound of 0 switched the age check off")
	}
	if nb.maxAge != defaultAdminKeysMaxAge {
		t.Fatalf("the bound of 0 is %s, not the default", nb.maxAge)
	}
	// another cluster: refused while the server runs, dropped at the next start
	clusterID = "c2"
	if _, err := a.StoredKey(ctx, "alice"); err == nil {
		t.Fatal("a cache of another cluster was accepted")
	}
	other := openAdminKeys(dir, func() string { return "c2" }, 0, nil)
	if _, err := other.StoredKey(ctx, "alice"); err == nil {
		t.Fatal("a file of another cluster was loaded")
	}
	if _, err := os.Stat(other.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the file of another cluster was not removed: %v", err)
	}
}

// Phase 5 review F8 and F7: an existing file with a wide mode is narrowed when it is opened; the read takes only logins that are still valid.
func TestTheCacheFileIsNarrowedAndTheReadSkipsExpiredLogins(t *testing.T) {
	if !strings.Contains(adminKeysSQL, "rolvaliduntil IS NULL OR x.rolvaliduntil > now()") || !strings.Contains(adminKeysSQL, "rolcanlogin") {
		t.Fatal("the read does not skip a login whose password expired")
	}
	dir := t.TempDir()
	a := openAdminKeys(dir, nil, 0, nil)
	if err := a.Replace(map[string][]byte{"alice": keyOf(1)}); err != nil {
		t.Fatal(err)
	}
	if os.PathSeparator != '/' {
		t.Skip("modes are not kept on this system")
	}
	if err := os.Chmod(a.path, 0o644); err != nil {
		t.Fatal(err)
	}
	openAdminKeys(dir, nil, 0, nil)
	if fi, err := os.Stat(filepath.Join(dir, adminKeysName)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the mode after opening: %v %v", fi, err)
	}
}

// Third review L2: a time in the file that is ahead of the clock is not believed; a small skew is allowed.
func TestACacheWithATimeInTheFutureIsRefused(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	now := time.Now()
	a := openAdminKeys(dir, nil, 0, nil)
	a.now = func() time.Time { return now.Add(2 * time.Hour) } // written while the clock was ahead
	if err := a.Replace(map[string][]byte{"alice": keyOf(1)}); err != nil {
		t.Fatal(err)
	}
	re := openAdminKeys(dir, nil, 0, nil)
	re.now = func() time.Time { return now }
	if _, err := re.StoredKey(ctx, "alice"); err == nil || !strings.Contains(err.Error(), "future") {
		t.Fatalf("a time two hours ahead was believed: %v", err)
	}
	re.now = func() time.Time { return now.Add(2*time.Hour - time.Minute) }
	if _, err := re.StoredKey(ctx, "alice"); err != nil {
		t.Fatalf("a skew of a minute was refused: %v", err)
	}
}

// Third review L7: a file written before the server knew its cluster has no id and is kept; the cache is gone for a process that keeps running when the server is reset.
func TestTheCacheGoesWithAResetAndKeepsAFileWithoutAClusterId(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	a := openAdminKeys(dir, nil, 0, nil) // no cluster id yet
	if err := a.Replace(map[string][]byte{"alice": keyOf(1)}); err != nil {
		t.Fatal(err)
	}
	known := openAdminKeys(dir, func() string { return "c1" }, 0, nil)
	if _, err := known.StoredKey(ctx, "alice"); err != nil {
		t.Fatalf("a file without a cluster id was dropped when the id became known: %v", err)
	}
	if err := known.Replace(map[string][]byte{"alice": keyOf(1)}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(known.path)
	if !strings.Contains(string(b), `"clusterId":"c1"`) {
		t.Fatalf("the next write did not fill the cluster id: %s", b)
	}
	// ResetMember removes the file; the running process stops answering
	if err := os.Remove(known.path); err != nil {
		t.Fatal(err)
	}
	if _, err := known.StoredKey(ctx, "alice"); err == nil || !strings.Contains(err.Error(), "removed") {
		t.Fatalf("a removed cache still answers: %v", err)
	}
}

// Third review L8: a file that is readable by others and cannot be narrowed is not used (unix); a directory that is wide is narrowed.
func TestTheCacheDirectoryIsNarrowed(t *testing.T) {
	if os.PathSeparator != '/' {
		t.Skip("modes are not kept on this system")
	}
	dir := filepath.Join(t.TempDir(), "failover")
	a := openAdminKeys(dir, nil, 0, nil)
	if err := a.Replace(map[string][]byte{"alice": keyOf(1)}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	openAdminKeys(dir, nil, 0, nil)
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("the directory mode: %v %v", fi, err)
	}
}

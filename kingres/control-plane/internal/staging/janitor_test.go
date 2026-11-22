// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package staging

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func putOld(t *testing.T, dir, name string, d time.Duration) {
	t.Helper()
	p := filepath.Join(dir, name)
	mustWrite(t, p, []byte("x"))
	age(t, p, d)
}

func TestJanitorDeletesOnlyWhatMatchesAndIsOldAndHasNoRow(t *testing.T) {
	s, _ := testStore(t, nil)
	d := s.Dir()
	old := 3 * time.Hour
	putOld(t, d, "transfer-1.kdx", old)         // no row: deleted
	putOld(t, d, "transfer-2.part", old)        // no row: deleted
	putOld(t, d, "transfer-3.kdx", old)         // a row that wants it: kept
	putOld(t, d, "transfer-4.kdx", old)         // row says remove: deleted
	putOld(t, d, "transfer-5.kdx", time.Minute) // young, no row: kept
	putOld(t, d, "transfer-6.kdx", time.Minute) // young, row says remove: kept
	foreign := []string{"notes.txt", "transfer-7.kdx.bak", "Transfer-8.kdx", "transfer-.kdx", "transfer-9.KDX", "transfer-10x.kdx", "transfer-11.kdx~",
		"xtransfer-12.kdx", "transfer-1000000000000000000.kdx", "transfer-13.kdx.part", ".hidden", "transfer-14.tmp", "pg_wal", "transfer 15.kdx"}
	for _, n := range foreign {
		putOld(t, d, n, 100*time.Hour)
	}
	if err := os.Mkdir(filepath.Join(d, "transfer-20.kdx"), 0o700); err != nil { // a directory with a matching name
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(d, "transfer-20.kdx", "inner"), []byte("x"))
	age(t, filepath.Join(d, "transfer-20.kdx"), 100*time.Hour)
	victim := filepath.Join(t.TempDir(), "victim")
	mustWrite(t, victim, []byte("keep me"))
	age(t, victim, 100*time.Hour)
	if err := os.Symlink(victim, filepath.Join(d, "transfer-21.kdx")); err != nil {
		t.Logf("no symbolic links here: %v", err)
	}

	before := names(t, d)
	rep, err := s.Sweep(SweepInput{HighestID: 100, Rows: map[int64]RowInfo{3: {}, 4: {Remove: true}, 6: {Remove: true}}})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(rep.Deleted)
	if got := strings.Join(rep.Deleted, ","); got != "transfer-1.kdx,transfer-2.part,transfer-4.kdx" {
		t.Fatalf("deleted = %s", got)
	}
	after := names(t, d)
	for _, n := range before {
		gone := n == "transfer-1.kdx" || n == "transfer-2.part" || n == "transfer-4.kdx"
		present := false
		for _, a := range after {
			present = present || a == n
		}
		if gone == present {
			t.Errorf("%s: present=%v after the sweep, want %v", n, present, !gone)
		}
	}
	if string(mustRead(t, victim)) != "keep me" {
		t.Fatal("the target of a link was touched")
	}
	if string(mustRead(t, filepath.Join(d, "transfer-20.kdx", "inner"))) != "x" {
		t.Fatal("a directory with a matching name was entered")
	}
	if len(rep.Young) != 2 || rep.Kept != 1 {
		t.Errorf("young %v kept %d", rep.Young, rep.Kept)
	}
}

func TestJanitorAgeBoundary(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	s, _ := testStore(t, func(c *Config) { c.Now = func() time.Time { return now }; c.MinAge = time.Hour })
	for name, mod := range map[string]time.Duration{"transfer-1.kdx": time.Hour, "transfer-2.kdx": time.Hour - time.Second, "transfer-3.kdx": time.Hour + time.Second} {
		p := filepath.Join(s.Dir(), name)
		mustWrite(t, p, []byte("x"))
		if err := os.Chtimes(p, now.Add(-mod), now.Add(-mod)); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := s.Sweep(SweepInput{HighestID: 10})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(rep.Deleted)
	if got := strings.Join(rep.Deleted, ","); got != "transfer-1.kdx,transfer-3.kdx" {
		t.Fatalf("deleted = %s (a file exactly one hour old may go, one that is a second younger may not)", got)
	}
}

func TestJanitorStopsWhenTheDatabaseKnowsFewerIdsThanTheFiles(t *testing.T) {
	s, _ := testStore(t, nil)
	putOld(t, s.Dir(), "transfer-1.kdx", 5*time.Hour)
	putOld(t, s.Dir(), "transfer-2.kdx", 5*time.Hour)
	putOld(t, s.Dir(), "transfer-9.kdx", time.Minute) // a young file with a higher id is enough
	before := names(t, s.Dir())
	rep, err := s.Sweep(SweepInput{HighestID: 8})
	if !errors.Is(err, ErrRestoredDatabase) {
		t.Fatalf("err = %v", err)
	}
	if len(rep.Deleted) != 0 || strings.Join(names(t, s.Dir()), ",") != strings.Join(before, ",") {
		t.Fatalf("a restored database must delete nothing: %v", rep.Deleted)
	}
	// Equal is not lower.
	if _, err := s.Sweep(SweepInput{HighestID: 9}); err != nil {
		t.Fatalf("highest id equal to the files: %v", err)
	}
	// A foreign file with a large number in its name does not count.
	putOld(t, s.Dir(), "transfer-99.kdx.bak", 5*time.Hour)
	if _, err := s.Sweep(SweepInput{HighestID: 9}); err != nil {
		t.Fatalf("a foreign name raised the highest id: %v", err)
	}
}

func TestJanitorRefusesWithoutTheMarker(t *testing.T) {
	s, _ := testStore(t, nil)
	putOld(t, s.Dir(), "transfer-1.kdx", 5*time.Hour)
	marker := filepath.Join(s.Dir(), MarkerName)
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sweep(SweepInput{HighestID: 5}); !errors.Is(err, ErrDirNotMarked) {
		t.Fatalf("no marker: %v", err)
	}
	mustWrite(t, marker, []byte("not ours\n"))
	if _, err := s.Sweep(SweepInput{HighestID: 5}); !errors.Is(err, ErrBadMarker) {
		t.Fatalf("bad marker: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), "transfer-1.kdx")); err != nil {
		t.Fatal("a file was deleted from a directory that lost its marker")
	}
}

func TestJanitorLeavesFilesHeldByAWriterOrAReservation(t *testing.T) {
	s, _ := testStore(t, nil)
	res := mustRes(t, s, 1)
	w, _, err := s.Create(1, res, testKeys)
	if err != nil {
		t.Fatal(err)
	}
	age(t, filepath.Join(s.Dir(), PartName(1)), 10*time.Hour)
	putOld(t, s.Dir(), "transfer-2.kdx", 10*time.Hour)
	mustRes(t, s, 2)
	putOld(t, s.Dir(), "transfer-3.kdx", 10*time.Hour)
	rep, err := s.Sweep(SweepInput{HighestID: 10})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(rep.Deleted, ",") != "transfer-3.kdx" || rep.Kept != 2 {
		t.Fatalf("deleted %v kept %d", rep.Deleted, rep.Kept)
	}
	_, _ = w.Write([]byte("x"))
	if _, err := w.Finish(); err != nil {
		t.Fatalf("the writer's file was deleted under it: %v", err)
	}
}

func TestJanitorReportsLostReadyRows(t *testing.T) {
	s, _ := testStore(t, nil)
	putOld(t, s.Dir(), "transfer-1.kdx", 10*time.Hour)
	putOld(t, s.Dir(), "transfer-3.part", 10*time.Hour) // a part file is not a ready file
	rep, err := s.Sweep(SweepInput{HighestID: 10, Rows: map[int64]RowInfo{1: {}, 2: {}, 3: {}, 4: {}}, ReadyIDs: []int64{4, 1, 2, 3}})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(rep.Lost) != "[2 3 4]" {
		t.Fatalf("lost = %v", rep.Lost)
	}
	// A ready row whose file the sweep itself just deleted is lost too.
	putOld(t, s.Dir(), "transfer-5.kdx", 10*time.Hour)
	rep, _ = s.Sweep(SweepInput{HighestID: 10, Rows: map[int64]RowInfo{1: {}, 3: {}, 5: {Remove: true}}, ReadyIDs: []int64{5}})
	if fmt.Sprint(rep.Lost) != "[5]" || len(rep.Deleted) != 1 {
		t.Fatalf("lost %v deleted %v", rep.Lost, rep.Deleted)
	}
}

// A hostile directory: random names, near misses, whatever. After a sweep every name that does not match the pattern still exists.
func TestJanitorNeverDeletesAFileThatDoesNotMatch(t *testing.T) {
	s, _ := testStore(t, nil)
	rnd := rand.New(rand.NewSource(99)) //nolint:gosec // test
	alphabet := []string{"transfer-", "transfer", "-", ".", "kdx", "part", "KDX", "0", "1", "9", "42", " ", "~", "bak", "tmp", "x", "é", "..", "_"}
	want := map[string]bool{}
	matching := 0
	for i := 0; i < 600; i++ {
		var b strings.Builder
		for j, n := 0, 1+rnd.Intn(6); j < n; j++ {
			b.WriteString(alphabet[rnd.Intn(len(alphabet))])
		}
		name := b.String()
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\:`) || name == MarkerName {
			continue
		}
		if _, _, ok := ParseName(name); ok {
			matching++
			continue
		}
		p := filepath.Join(s.Dir(), name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			continue // a name this file system does not allow
		}
		age(t, p, 1000*time.Hour)
		want[name] = true
	}
	for _, n := range []string{"transfer-1.kdx", "transfer-2.part"} {
		putOld(t, s.Dir(), n, 1000*time.Hour)
	}
	if _, err := s.Sweep(SweepInput{HighestID: 1 << 40}); err != nil {
		t.Fatal(err)
	}
	for n := range want {
		if _, err := os.Lstat(filepath.Join(s.Dir(), n)); err != nil {
			t.Errorf("the janitor deleted %q, which does not match the pattern", n)
		}
	}
	for _, n := range []string{"transfer-1.kdx", "transfer-2.part"} {
		if _, err := os.Lstat(filepath.Join(s.Dir(), n)); err == nil {
			t.Errorf("%s should have been deleted", n)
		}
	}
	t.Logf("%d hostile names kept, %d matching names skipped", len(want), matching)
}

func TestJanitorLeavesTheMarkerAndForeignFilesOutsideTheDirectory(t *testing.T) {
	s, _ := testStore(t, nil)
	sibling := filepath.Join(filepath.Dir(s.Dir()), "transfer-1.kdx")
	mustWrite(t, sibling, []byte("outside"))
	age(t, sibling, 1000*time.Hour)
	if _, err := s.Sweep(SweepInput{HighestID: 100}); err != nil {
		t.Fatal(err)
	}
	if string(mustRead(t, sibling)) != "outside" {
		t.Fatal("a file outside the directory was deleted")
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), MarkerName)); err != nil {
		t.Fatal("the marker was deleted")
	}
}

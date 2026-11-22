// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package staging

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestOpenCreatesMarkedDirectory(t *testing.T) {
	s, _ := testStore(t, nil)
	if got := string(mustRead(t, filepath.Join(s.Dir(), MarkerName))); got != markerText {
		t.Fatalf("marker = %q", got)
	}
	if modesEnforced {
		info, err := os.Stat(s.Dir())
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("directory mode = %v, %v", info.Mode().Perm(), err)
		}
	}
	// A second start accepts its own directory.
	if _, err := Open(Config{Dir: s.Dir(), FS: s.fs}); err != nil {
		t.Fatalf("reopen: %v", err)
	}
}

func TestOpenAcceptsEmptyUnmarkedDirectory(t *testing.T) {
	d := filepath.Join(t.TempDir(), "empty")
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(Config{Dir: d}); err != nil {
		t.Fatalf("empty directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(d, MarkerName)); err != nil {
		t.Fatalf("marker not written: %v", err)
	}
}

func TestOpenRefusesNonEmptyUnmarkedDirectory(t *testing.T) {
	d := filepath.Join(t.TempDir(), "foreign")
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(d, "precious.db"), []byte("data"))
	if _, err := Open(Config{Dir: d}); !errors.Is(err, ErrDirNotMarked) {
		t.Fatalf("err = %v, want ErrDirNotMarked", err)
	}
	if _, err := os.Stat(filepath.Join(d, MarkerName)); err == nil {
		t.Fatal("a marker was written into a foreign directory")
	}
	if string(mustRead(t, filepath.Join(d, "precious.db"))) != "data" {
		t.Fatal("foreign file changed")
	}
}

func TestOpenRefusesBadMarker(t *testing.T) {
	for name, content := range map[string][]byte{
		"other text":  []byte("something else\n"),
		"extra bytes": []byte(markerText + "x"),
		"empty":       {},
	} {
		d := filepath.Join(t.TempDir(), "bad")
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(d, MarkerName), content)
		if _, err := Open(Config{Dir: d}); !errors.Is(err, ErrBadMarker) {
			t.Errorf("%s: err = %v, want ErrBadMarker", name, err)
		}
	}
	// A marker that is a directory is not a marker either.
	d := filepath.Join(t.TempDir(), "dirmarker")
	if err := os.MkdirAll(filepath.Join(d, MarkerName), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(Config{Dir: d}); !errors.Is(err, ErrBadMarker) {
		t.Errorf("directory as marker: err = %v", err)
	}
}

func TestOpenRefusesRootAndRelative(t *testing.T) {
	root := string(filepath.Separator)
	if v := filepath.VolumeName(os.TempDir()); v != "" {
		root = v + root
	}
	if _, err := Open(Config{Dir: root}); !errors.Is(err, ErrDirForbidden) {
		t.Errorf("root: %v", err)
	}
	if _, err := Open(Config{Dir: "relative/dir"}); err == nil {
		t.Error("a relative directory was accepted")
	}
}

func TestOpenForbiddenDirectories(t *testing.T) {
	base := t.TempDir()
	dataDir := filepath.Join(base, "data")
	pgData := filepath.Join(base, "pgdata")
	for _, d := range []string{dataDir, pgData} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cfg := func(dir string) Config { return Config{Dir: dir, Forbidden: []string{dataDir, pgData}} }

	// The data directory itself, a directory that contains it, and a directory that contains a PostgreSQL data directory.
	if _, err := Open(cfg(dataDir)); !errors.Is(err, ErrDirForbidden) {
		t.Errorf("the data directory: %v", err)
	}
	if _, err := Open(cfg(pgData)); !errors.Is(err, ErrDirForbidden) {
		t.Errorf("a PostgreSQL data directory: %v", err)
	}
	if _, err := Open(cfg(base)); !errors.Is(err, ErrDirForbidden) {
		t.Errorf("a directory that contains the data directories: %v", err)
	}
	// Spelled with dot segments it is the same directory.
	if _, err := Open(cfg(filepath.Join(dataDir, "sub", ".."))); !errors.Is(err, ErrDirForbidden) {
		t.Errorf("dot segments: %v", err)
	}
	// A directory inside the data directory is the default (<DataDir>/transfer) and is allowed.
	in := filepath.Join(dataDir, "transfer")
	if _, err := Open(cfg(in)); err != nil {
		t.Errorf("a directory inside the data directory: %v", err)
	}
	// A PostgreSQL data directory that the configuration does not know is recognised by PG_VERSION, even when the marker is there.
	other := filepath.Join(base, "otherpg")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(other, "PG_VERSION"), []byte("18\n"))
	mustWrite(t, filepath.Join(other, MarkerName), []byte(markerText))
	if _, err := Open(Config{Dir: other}); !errors.Is(err, ErrDirForbidden) {
		t.Errorf("PG_VERSION: %v", err)
	}
}

func TestOpenRefusesSymbolicLinkAndOpenMode(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
	if _, err := Open(Config{Dir: link}); !errors.Is(err, ErrDirForbidden) {
		t.Errorf("symbolic link: %v", err)
	}
	unixOnly(t)
	open := filepath.Join(base, "open")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(Config{Dir: open}); !errors.Is(err, ErrDirMode) {
		t.Errorf("mode 0755: %v", err)
	}
}

func TestDefaults(t *testing.T) {
	if q := DefaultQuota(1000 * uint64(GiB)); q != 100*GiB {
		t.Errorf("quota of a big disk = %d", q)
	}
	if q := DefaultQuota(100 * uint64(GiB)); q != 25*GiB {
		t.Errorf("quota of a small disk = %d", q)
	}
	if r := DefaultReserve(10 * uint64(GiB)); r != uint64(5*GiB) {
		t.Errorf("reserve of a small disk = %d", r)
	}
	if r := DefaultReserve(1000 * uint64(GiB)); r != uint64(100*GiB) {
		t.Errorf("reserve of a big disk = %d", r)
	}
}

func TestQuotaArithmetic(t *testing.T) {
	s, _ := testStore(t, func(c *Config) { c.Quota = 1000 })
	a, err := s.Reserve(1, "alice", 600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reserve(2, "bob", 500); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("500 over 600 of 1000: %v", err)
	}
	b, err := s.Reserve(2, "bob", 400)
	if err != nil {
		t.Fatalf("exactly the quota: %v", err)
	}
	if _, err := s.Reserve(3, "alice", 1); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("one byte over: %v", err)
	}
	a.Release()
	if _, err := s.Reserve(3, "alice", 600); err != nil {
		t.Fatalf("after a release: %v", err)
	}
	b.Release()
	b.Release() // idempotent
	if _, err := s.Reserve(3, "x", 1); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate id: %v", err)
	}
}

func TestQuotaErrorNamesOnlyOwnReservations(t *testing.T) {
	s, _ := testStore(t, func(c *Config) { c.Quota = 1000 })
	for _, h := range []struct {
		id    int64
		owner string
		n     int64
	}{{10, "alice", 300}, {11, "bob", 300}, {12, "carol", 300}, {13, "alice", 50}} {
		if _, err := s.Reserve(h.id, h.owner, h.n); err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.Reserve(14, "alice", 200)
	var qe *QuotaError
	if !errors.As(err, &qe) {
		t.Fatalf("err = %v", err)
	}
	if len(qe.Own) != 2 || qe.Own[0].ID != 10 || qe.Own[1].ID != 13 || qe.Others != 2 {
		t.Fatalf("own = %+v others = %d", qe.Own, qe.Others)
	}
	if qe.Quota != 1000 || qe.Reserved != 950 || qe.Requested != 200 {
		t.Fatalf("numbers = %+v", qe)
	}
}

func TestReservationGrowsAndTheOffenderFailsNotItsNeighbours(t *testing.T) {
	s, _ := testStore(t, func(c *Config) { c.Quota = 1000 })
	offender, _ := s.Reserve(1, "a", 100)
	neighbour, _ := s.Reserve(2, "b", 100)
	if err := offender.Use(100); err != nil {
		t.Fatal(err)
	}
	if err := offender.Use(300); err != nil { // grows to 400; sum 500
		t.Fatalf("growth inside the quota: %v", err)
	}
	if offender.Reserved() != 400 || offender.Used() != 400 {
		t.Fatalf("reserved %d used %d", offender.Reserved(), offender.Used())
	}
	if err := offender.Use(700); !errors.Is(err, ErrQuotaExceeded) { // would be 1100 + 100
		t.Fatalf("growth past the quota: %v", err)
	}
	if offender.Reserved() != 400 || offender.Used() != 400 {
		t.Fatalf("a refused growth changed the reservation: %d %d", offender.Reserved(), offender.Used())
	}
	// The neighbour is untouched and can still use its own room and grow into what is left.
	if err := neighbour.Use(100); err != nil {
		t.Fatalf("neighbour inside its room: %v", err)
	}
	if err := neighbour.Use(400); err != nil { // sum 400+100 -> 400+500
		t.Fatalf("neighbour growth: %v", err)
	}
	// The quota counts the growth of everybody.
	if err := offender.Use(101); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("growth into the neighbour's room: %v", err)
	}
}

func TestSettleGivesTheRestBack(t *testing.T) {
	s, _ := testStore(t, func(c *Config) { c.Quota = 1000 })
	r, _ := s.Reserve(1, "a", 900)
	if err := r.Use(100); err != nil {
		t.Fatal(err)
	}
	r.Settle()
	if r.Reserved() != 100 {
		t.Fatalf("reserved after settle = %d", r.Reserved())
	}
	if _, err := s.Reserve(2, "b", 900); err != nil {
		t.Fatalf("the room should be free: %v", err)
	}
	if holds := s.Holds(); len(holds) != 2 || holds[0].Bytes != 100 {
		t.Fatalf("holds = %+v", holds)
	}
}

func TestAdoptDoesNotRefuseForQuota(t *testing.T) {
	s, _ := testStore(t, func(c *Config) { c.Quota = 100 })
	if _, err := s.Adopt(1, "a", 500, 400); err != nil {
		t.Fatalf("adopt over the quota: %v", err)
	}
	if _, err := s.Reserve(2, "a", 1); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("a new reservation must see the adopted room: %v", err)
	}
	if _, err := s.Adopt(1, "a", 1, 1); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate adopt: %v", err)
	}
}

func TestReserveRefusedForTheFreeSpaceReserve(t *testing.T) {
	const reserve = 1 << 20
	s, ffs := testStore(t, func(c *Config) { c.Quota = 1 << 40 })
	ffs.setSpace(1<<34, reserve+1000)
	if _, err := s.Reserve(1, "a", 1001); !errors.Is(err, ErrInsufficientDisk) {
		t.Fatalf("one byte too many: %v", err)
	}
	var de *DiskError
	_, err := s.Reserve(1, "a", 5000)
	if !errors.As(err, &de) || de.Free != reserve+1000 || de.Reserve != reserve || de.Needed != 5000 {
		t.Fatalf("disk error = %+v (%v)", de, err)
	}
	if _, err := s.Reserve(1, "a", 600); err != nil {
		t.Fatalf("a reservation that fits: %v", err)
	}
	// 600 are promised and not yet written, so 401 more would cross the reserve.
	if _, err := s.Reserve(2, "a", 401); !errors.Is(err, ErrInsufficientDisk) {
		t.Fatalf("the unwritten part of the first reservation must count: %v", err)
	}
	if _, err := s.Reserve(2, "a", 400); err != nil {
		t.Fatalf("exactly up to the reserve: %v", err)
	}
}

func TestRunningWriterStopsAtTheReserve(t *testing.T) {
	const reserve = 1 << 20
	s, ffs := testStore(t, func(c *Config) { c.CheckEvery = 1000; c.Quota = 1 << 40 })
	ffs.setSpace(1<<34, reserve+10_000)
	r, err := s.Reserve(1, "a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Use(2000); err != nil { // checks the disk; free is still fine
		t.Fatal(err)
	}
	ffs.setSpace(1<<34, reserve+300) // something else filled the disk; 300 bytes are left above the reserve
	if err := r.Use(500); err != nil {
		t.Fatalf("under the check interval nothing is looked at: %v", err)
	}
	err = r.Use(600) // the room (10) is used up, so these bytes are growth and must fit above the reserve
	if !errors.Is(err, ErrInsufficientDisk) {
		t.Fatalf("at the interval the reserve must stop the writer: %v", err)
	}
	if r.Used() != 2500 {
		t.Fatalf("a refused Use changed the count: %d", r.Used())
	}
}

func TestConcurrentReservationsNeverExceedTheQuota(t *testing.T) {
	s, _ := testStore(t, func(c *Config) { c.Quota = 250 })
	var ok atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 80; i++ {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			if _, err := s.Reserve(id, "o", 10); err == nil {
				ok.Add(1)
			}
		}(int64(i + 1))
	}
	wg.Wait()
	if ok.Load() != 25 {
		t.Fatalf("%d reservations fit, want exactly 25", ok.Load())
	}
}

func TestNamesAreDerivedFromTheIdOnly(t *testing.T) {
	if PartName(42) != "transfer-42.part" || FinalName(42) != "transfer-42.kdx" {
		t.Fatal("names")
	}
	for _, n := range []string{"transfer-1.part", "transfer-1.kdx", "transfer-007.kdx", "transfer-999999999999999999.kdx"} {
		if _, _, ok := ParseName(n); !ok {
			t.Errorf("%q should parse", n)
		}
	}
	for _, n := range []string{"", "transfer-.kdx", "transfer-1.KDX", "Transfer-1.kdx", "transfer-1.kdx.bak", "xtransfer-1.kdx", "transfer-1x.kdx",
		"transfer--1.kdx", "transfer-1.kdx\n", "transfer-1.part ", "transfer-1000000000000000000.kdx", "../transfer-1.kdx", "transfer-1.kdx/", MarkerName} {
		if _, _, ok := ParseName(n); ok {
			t.Errorf("%q must not parse", n)
		}
	}
}

func TestStateAndRemove(t *testing.T) {
	s, _ := testStore(t, nil)
	if st, _, err := s.State(5); err != nil || st != StateNone {
		t.Fatalf("state of nothing = %v %v", st, err)
	}
	sealFile(t, s, 5, []byte("hello"))
	st, size, err := s.State(5)
	if err != nil || st != StateFinal || size != HeaderSize+5+tagSize {
		t.Fatalf("state = %v %d %v", st, size, err)
	}
	if err := s.Remove(5); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := s.State(5); st != StateNone {
		t.Fatal("file not removed")
	}
	if err := s.Remove(5); err != nil {
		t.Fatalf("removing nothing: %v", err)
	}
}

func TestRemoveRefusesAnythingThatIsNotARegularFile(t *testing.T) {
	s, _ := testStore(t, nil)
	victim := filepath.Join(t.TempDir(), "victim")
	mustWrite(t, victim, []byte("keep me"))
	if err := os.Symlink(victim, filepath.Join(s.Dir(), FinalName(9))); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
	if err := s.Remove(9); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("Remove through a link: %v", err)
	}
	if string(mustRead(t, victim)) != "keep me" {
		t.Fatal("the target of the link was touched")
	}
	if _, err := s.OpenReader(9, make([]byte, 32)); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("OpenReader through a link: %v", err)
	}
	if _, _, err := s.State(9); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("State through a link: %v", err)
	}
}

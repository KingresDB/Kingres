// SPDX-License-Identifier: AGPL-3.0-only

package instance

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// exitError returns a real *exec.ExitError with the exit code.
func exitError(t *testing.T, code int) error {
	t.Helper()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", "exit "+strconv.Itoa(code))
	} else {
		cmd = exec.Command("sh", "-c", "exit "+strconv.Itoa(code))
	}
	err := cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("no exit error: %v", err)
	}
	return err
}

// fakeServer replaces the PostgreSQL programs of a Manager: pg_ctl status and start, pg_basebackup.
type fakeServer struct {
	t       *testing.T
	mu      sync.Mutex
	running bool
	// starts holds, for each pg_ctl start in turn, the text the postmaster writes to the startup log and whether the
	// start works; the last one repeats.
	starts []fakeStart
	nStart int
	events []string
	// onStart runs inside a start with the context pg_ctl got.
	onStart func(ctx context.Context)
	// ctxSeen is the context of the last pg_ctl start.
	ctxSeen context.Context
	ctxErr  error
}

type fakeStart struct {
	log string
	ok  bool
	// runs: the start fails (pg_ctl -w ran out of time) but the postmaster goes on and is running afterwards.
	runs bool
}

func (f *fakeServer) run(ctx context.Context, prog string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case prog == "pg_ctl" && args[0] == "status":
		if f.running {
			return "pg_ctl: server is running", nil
		}
		return "pg_ctl: no server running", exitError(f.t, 3)
	case prog == "pg_ctl" && args[0] == "start":
		f.events = append(f.events, "start")
		f.ctxSeen = ctx
		if f.onStart != nil {
			f.onStart(ctx)
		}
		f.ctxErr = ctx.Err() // after the request was cancelled in the middle of the start
		var logPath string
		for i, a := range args {
			if a == "-l" {
				logPath = args[i+1]
			}
		}
		st := f.starts[min(f.nStart, len(f.starts)-1)]
		f.nStart++
		if logPath != "" {
			fh, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				f.t.Fatal(err)
			}
			_, _ = fh.WriteString(st.log)
			_ = fh.Close()
		}
		if st.runs {
			f.running = true
		}
		if st.ok {
			f.running = true
			return "waiting for server to start.... done", nil
		}
		return "waiting for server to start.... stopped waiting\npg_ctl: could not start server", exitError(f.t, 1)
	case prog == "pg_basebackup":
		f.events = append(f.events, "basebackup")
		for i, a := range args {
			if a == "-D" {
				if err := os.MkdirAll(args[i+1], 0o700); err != nil {
					f.t.Fatal(err)
				}
			}
		}
		return "", nil
	}
	return "", errors.New("not faked: " + prog)
}

func newFakeManager(t *testing.T, fs *fakeServer) *Manager {
	t.Helper()
	bin := t.TempDir()
	for _, p := range []string{"initdb", "pg_ctl"} {
		if err := os.WriteFile(filepath.Join(bin, p), nil, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	m, err := New(Options{Root: t.TempDir(), BinDir: bin, AllowRoot: true, SocketDir: shortDir(t), StartTimeout: 90 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	fs.t = t
	m.runner = fs.run
	return m
}

// shortDir is a directory with a short path: a unix socket path may not be longer than about 100 bytes, and the
// temporary directory of a Windows user is longer.
func shortDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "windows" {
		return t.TempDir()
	}
	d, err := os.MkdirTemp(filepath.VolumeName(os.TempDir())+"\\", "kt")
	if err != nil {
		t.Skipf("no short directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

func standbySettings(port int) ManagedSettings {
	return ManagedSettings{Port: port, ListenAddresses: "127.0.0.1", MaxConnections: 50}
}

// newStandbyDir makes the directory of a complete standby that has not run yet (neverStarted) or has.
func newStandbyDir(t *testing.T, m *Manager, port int, neverStarted bool) int {
	t.Helper()
	const id = 101
	dir, _ := m.Dir(id)
	data := filepath.Join(dir, "pgdata")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	mk, _ := json.Marshal(Marker{NodeID: id, Port: port, CreatedAt: time.Now().UTC(), Magic: markerMagic})
	if err := os.WriteFile(filepath.Join(dir, MarkerName), mk, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteManaged(id, standbySettings(port)); err != nil {
		t.Fatal(err)
	}
	if neverStarted {
		if err := m.markNeverStarted(id); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

const bindLog = "LOG:  starting PostgreSQL\nLOG:  could not bind IPv4 address \"127.0.0.1\": Address already in use\nHINT:  Is another postmaster already running on port 57002?\nFATAL:  could not create any TCP/IP sockets\n"

// wantPort checks that the marker, the settings and kingres-managed.conf say the same port.
func wantPort(t *testing.T, m *Manager, id, want int) {
	t.Helper()
	mk, err := m.Managed(id)
	if err != nil {
		t.Fatal(err)
	}
	s, err := m.Settings(id)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := m.DataDir(id)
	b, err := os.ReadFile(filepath.Join(data, "kingres-managed.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if mk.Port != want || s.Port != want || !strings.Contains(string(b), "port = '"+strconv.Itoa(want)+"'\n") {
		t.Fatalf("the port must be %d everywhere: marker %d, settings %d, conf:\n%s", want, mk.Port, s.Port, b)
	}
}

func TestInitStandbyChoosesThePortAfterTheCopy(t *testing.T) {
	fs := &fakeServer{}
	m := newFakeManager(t, fs)
	picked := 0
	spec := StandbySpec{ID: 101, Settings: standbySettings(0), Conninfo: "host=x", Slot: "s",
		PickPort: func() (int, error) {
			fs.mu.Lock()
			defer fs.mu.Unlock()
			fs.events = append(fs.events, "pick")
			// During the copy the instance had no port yet: nothing could have been started on one.
			if mk, err := m.Managed(101); err != nil || mk.Port != 0 {
				t.Errorf("the marker named port %d before the pick (%v)", mk.Port, err)
			}
			picked++
			return 57123, nil
		}}
	if err := m.InitStandby(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fs.events, ","); got != "basebackup,pick" {
		t.Fatalf("the port must be chosen after the base backup: %s", got)
	}
	if picked != 1 {
		t.Fatalf("pick called %d times", picked)
	}
	wantPort(t, m, 101, 57123)
	if !m.NeverStarted(101) {
		t.Fatal("a new standby has not run")
	}
}

func TestInitStandbyWithoutPickKeepsThePortOfTheSpec(t *testing.T) {
	fs := &fakeServer{}
	m := newFakeManager(t, fs)
	spec := StandbySpec{ID: 101, Settings: standbySettings(57001), Conninfo: "host=x", Slot: "s"}
	if err := m.InitStandby(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	wantPort(t, m, 101, 57001)
}

func TestAFailedPickLeavesNoStandby(t *testing.T) {
	fs := &fakeServer{}
	m := newFakeManager(t, fs)
	spec := StandbySpec{ID: 101, Settings: standbySettings(0), Conninfo: "host=x", Slot: "s",
		PickPort: func() (int, error) { return 0, errors.New("no free port") }}
	if err := m.InitStandby(context.Background(), spec); err == nil {
		t.Fatal("no port, no standby")
	}
	if m.Exists(101) {
		t.Fatal("the directory of the failed standby is still there")
	}
}

func TestStartStandbyChoosesAnotherPortOnABindError(t *testing.T) {
	fs := &fakeServer{starts: []fakeStart{{log: bindLog}, {ok: true}}}
	m := newFakeManager(t, fs)
	id := newStandbyDir(t, m, 57002, true)
	picks := 0
	err := m.StartStandby(context.Background(), id, func() (int, error) { picks++; return 57077, nil })
	if err != nil {
		t.Fatal(err)
	}
	if picks != 1 || fs.nStart != 2 {
		t.Fatalf("picks %d, starts %d", picks, fs.nStart)
	}
	wantPort(t, m, id, 57077) // the managed file, the settings and the marker, in the one step
	if m.NeverStarted(id) {
		t.Fatal("it ran: the port is final now")
	}
}

func TestTheNewPortIsUsedOnlyOnce(t *testing.T) {
	fs := &fakeServer{starts: []fakeStart{{log: bindLog}}} // every start fails on the bind
	m := newFakeManager(t, fs)
	id := newStandbyDir(t, m, 57002, true)
	picks := 0
	err := m.StartStandby(context.Background(), id, func() (int, error) { picks++; return 57070 + picks, nil })
	if !IsPortInUse(err) {
		t.Fatalf("the second failure is returned: %v", err)
	}
	if picks != 1 || fs.nStart != 2 {
		t.Fatalf("one new port and one more start per call: picks %d, starts %d", picks, fs.nStart)
	}
	wantPort(t, m, id, 57071)
}

func TestNoNewPortForAnotherStartError(t *testing.T) {
	for _, log := range []string{
		"FATAL:  could not locate a valid checkpoint record\n",
		"FATAL:  hot standby is not possible because of insufficient parameter settings\n",
		"",
	} {
		fs := &fakeServer{starts: []fakeStart{{log: log}}}
		m := newFakeManager(t, fs)
		id := newStandbyDir(t, m, 57002, true)
		err := m.StartStandby(context.Background(), id, func() (int, error) { t.Fatal("picked a port"); return 0, nil })
		if err == nil || IsPortInUse(err) {
			t.Fatalf("log %q: %v", log, err)
		}
		if fs.nStart != 1 {
			t.Fatalf("log %q: started %d times", log, fs.nStart)
		}
		wantPort(t, m, id, 57002)
	}
}

func TestAnOldBindErrorInTheLogIsNotThisStartsError(t *testing.T) {
	fs := &fakeServer{starts: []fakeStart{{log: "FATAL:  could not locate a valid checkpoint record\n"}}}
	m := newFakeManager(t, fs)
	id := newStandbyDir(t, m, 57002, true)
	data, _ := m.DataDir(id)
	if err := os.WriteFile(filepath.Join(data, "kingres-startup.log"), []byte(bindLog), 0o600); err != nil {
		t.Fatal(err)
	}
	err := m.StartStandby(context.Background(), id, func() (int, error) { t.Fatal("picked a port"); return 0, nil })
	if err == nil || IsPortInUse(err) {
		t.Fatalf("only what this start wrote counts: %v", err)
	}
}

func TestNoNewPortForAStandbyThatHasRun(t *testing.T) {
	fs := &fakeServer{starts: []fakeStart{{log: bindLog}}}
	m := newFakeManager(t, fs)
	id := newStandbyDir(t, m, 57002, false)
	err := m.StartStandby(context.Background(), id, func() (int, error) { t.Fatal("picked a port"); return 0, nil })
	if !IsPortInUse(err) || fs.nStart != 1 {
		t.Fatalf("the port of a standby that ran stays: %v, starts %d", err, fs.nStart)
	}
	wantPort(t, m, id, 57002)
}

func TestNoNewPortForAPrimaryInstance(t *testing.T) {
	fs := &fakeServer{starts: []fakeStart{{log: bindLog}}}
	m := newFakeManager(t, fs)
	dir, _ := m.Dir(1)
	if err := os.MkdirAll(filepath.Join(dir, "pgdata"), 0o700); err != nil {
		t.Fatal(err)
	}
	mk, _ := json.Marshal(Marker{NodeID: 1, Port: 5432, Magic: markerMagic})
	if err := os.WriteFile(filepath.Join(dir, MarkerName), mk, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.markNeverStarted(1); err != nil {
		t.Fatal(err)
	}
	err := m.StartStandby(context.Background(), 1, func() (int, error) { t.Fatal("picked a port"); return 0, nil })
	if !IsPortInUse(err) {
		t.Fatalf("%v", err)
	}
}

func TestAFirstSuccessfulStartEndsTheProvisionalPort(t *testing.T) {
	fs := &fakeServer{starts: []fakeStart{{ok: true}}}
	m := newFakeManager(t, fs)
	id := newStandbyDir(t, m, 57002, true)
	if err := m.Start(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if m.NeverStarted(id) {
		t.Fatal("the mark stays after a start that worked")
	}
}

func TestAPickFailureKeepsTheStartError(t *testing.T) {
	fs := &fakeServer{starts: []fakeStart{{log: bindLog}}}
	m := newFakeManager(t, fs)
	id := newStandbyDir(t, m, 57002, true)
	err := m.StartStandby(context.Background(), id, func() (int, error) { return 0, errors.New("no free port") })
	if !IsPortInUse(err) || !strings.Contains(err.Error(), "no free port") {
		t.Fatalf("%v", err)
	}
	wantPort(t, m, id, 57002)
	if fs.nStart != 1 {
		t.Fatalf("started %d times", fs.nStart)
	}
}

func TestPgCtlStartRunsWithoutTheCancellationOfTheRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fs := &fakeServer{starts: []fakeStart{{ok: true}}}
	// The request is cancelled in the middle of the start (the caller gave up): pg_ctl must not see it.
	fs.onStart = func(context.Context) { cancel() }
	m := newFakeManager(t, fs)
	id := newStandbyDir(t, m, 57002, true)
	if err := m.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := fs.ctxErr; err != nil {
		t.Fatalf("pg_ctl got a cancelled context: %v", err)
	}
	dl, ok := fs.ctxSeen.Deadline()
	if !ok || time.Until(dl) > 90*time.Second+startBackstop || time.Until(dl) < 60*time.Second {
		t.Fatalf("pg_ctl runs under its own deadline of the start timeout plus the backstop: %v %v", dl, ok)
	}
}

func TestStartupLogSince(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "kingres-startup.log")
	if tail, bind := startupLogSince(log, 0); tail != "" || bind {
		t.Fatalf("no log: %q %v", tail, bind)
	}
	if err := os.WriteFile(log, []byte("LOG: starting\nFATAL: could not bind IPv4 address: Address already in use\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tail, bind := startupLogSince(log, 0)
	if tail != "LOG: starting | FATAL: could not bind IPv4 address: Address already in use |" || !bind {
		t.Fatalf("short log: %q %v", tail, bind)
	}
	from := int64(len("LOG: starting\n"))
	if tail, _ := startupLogSince(log, from); !strings.HasPrefix(tail, "FATAL") {
		t.Fatalf("from an offset: %q", tail)
	}
	// An offset past the end (the log was replaced by a shorter one): all of it is read.
	if tail, _ := startupLogSince(log, 1<<20); !strings.HasPrefix(tail, "LOG: starting") {
		t.Fatalf("offset past the end: %q", tail)
	}
	for _, text := range []string{"LOG:  could not bind IPv4 address \"127.0.0.1\": Address already in use"} {
		if err := os.WriteFile(log, []byte("x\n"+text+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, b := startupLogSince(log, 0); !b {
			t.Errorf("%q is a bind error", text)
		}
	}
	// A long log: the tail is bounded, and a bind error before the tail is still found.
	if err := os.WriteFile(log, []byte("could not bind IPv4 address: Address already in use\n"+strings.Repeat("x", 5000)+"\nthe end\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tail, bind = startupLogSince(log, 0)
	if len(tail) > 2100 || !strings.HasSuffix(tail, "the end |") || !bind {
		t.Fatalf("long log: len %d bind %v", len(tail), bind)
	}
}

func TestBindFailure(t *testing.T) {
	for name, tc := range map[string]struct {
		log  string
		want bool
	}{
		"IPv4":                        {"LOG:  could not bind IPv4 address \"127.0.0.1\": Address already in use\nHINT:  Is another postmaster already running on port 5432?\n", true},
		"IPv6":                        {"LOG:  could not bind IPv6 address \"::1\": Address already in use\n", true},
		"the hint alone":              {"HINT:  Is another postmaster already running on port 5432? If not, wait a few seconds and retry.\n", true},
		"a Unix socket":               {"LOG:  could not bind Unix address \"/tmp/.s.PGSQL.5432\": Address already in use\nHINT:  Is another postmaster already running on port 5432? If not, remove socket file \"/tmp/.s.PGSQL.5432\" and retry.\nFATAL:  could not create any Unix-domain sockets\n", false},
		"an address that is not ours": {"LOG:  could not bind IPv4 address \"10.9.9.9\": Cannot assign requested address\nFATAL:  could not create any TCP/IP sockets\n", false},
		"the word bind elsewhere":     {"LOG:  could not bind to the replication slot\n", false},
		"address in use elsewhere":    {"LOG:  could not connect to the primary: Address already in use\n", false},
		"another family":              {"LOG:  could not bind AF_NETLINK socket: Address already in use\n", false},
		"nothing":                     {"LOG:  database system is ready to accept read-only connections\n", false},
		"Unix and then IPv4":          {"LOG:  could not bind Unix address \"/s/.s.PGSQL.5432\": Address already in use\nLOG:  could not bind IPv4 address \"127.0.0.1\": Address already in use\n", true},
	} {
		if got := bindFailure(tc.log); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}

func TestNoNewPortForABindErrorThatAnotherPortWouldNotFix(t *testing.T) {
	for name, log := range map[string]string{
		"unix":   "LOG:  could not bind Unix address \"/s/.s.PGSQL.57002\": Address already in use\nHINT:  Is another postmaster already running on port 57002?\n",
		"assign": "LOG:  could not bind IPv4 address \"10.9.9.9\": Cannot assign requested address\n",
	} {
		fs := &fakeServer{starts: []fakeStart{{log: log}}}
		m := newFakeManager(t, fs)
		id := newStandbyDir(t, m, 57002, true)
		err := m.StartStandby(context.Background(), id, func() (int, error) { t.Fatal(name + ": picked a port"); return 0, nil })
		if err == nil || IsPortInUse(err) || fs.nStart != 1 {
			t.Fatalf("%s: %v, starts %d", name, err, fs.nStart)
		}
	}
}

// confPortAtStart is the port in kingres-managed.conf when pg_ctl start runs: the one the postmaster would listen on.
func confPortAtStart(t *testing.T, m *Manager, id int, into *int) func(context.Context) {
	return func(context.Context) {
		data, _ := m.DataDir(id)
		b, err := os.ReadFile(filepath.Join(data, "kingres-managed.conf"))
		if err != nil {
			t.Error(err)
			return
		}
		sm := confPortLine.FindSubmatch(b)
		if sm == nil {
			t.Error("no port line in the conf")
			return
		}
		*into, _ = strconv.Atoi(string(sm[1]))
	}
}

// A crash after each of the three writes of setStandbyPort: the marker is the authority, so the next start listens on the
// marker's port, whatever the other two files say, and the three agree afterwards.
func TestACrashInTheChangeOfThePortIsRepairedBeforeTheNextStart(t *testing.T) {
	for _, step := range []string{"marker", "settings", "conf"} {
		t.Run(step, func(t *testing.T) {
			fs := &fakeServer{starts: []fakeStart{{log: bindLog}, {ok: true}}}
			m := newFakeManager(t, fs)
			id := newStandbyDir(t, m, 57002, true)
			m.afterPortStep = func(s string) error {
				if s == step {
					return errors.New("crash after " + s)
				}
				return nil
			}
			err := m.StartStandby(context.Background(), id, func() (int, error) { return 57077, nil })
			if err == nil || !strings.Contains(err.Error(), "crash after "+step) {
				t.Fatalf("the change must end at the crash: %v", err)
			}
			// What the crash left: the marker already has the new port; no file may have it while the marker has the old one.
			mk, _ := m.Managed(id)
			data, _ := m.DataDir(id)
			conf, _ := os.ReadFile(filepath.Join(data, "kingres-managed.conf"))
			if sm := confPortLine.FindSubmatch(conf); sm == nil || (string(sm[1]) == "57077" && mk.Port != 57077) {
				t.Fatalf("the conf says %q while the marker says %d", sm, mk.Port)
			}
			if mk.Port != 57077 {
				t.Fatalf("the marker comes first: %d", mk.Port)
			}
			// The next start (kingresd restarted, the supervision round).
			m.afterPortStep = nil
			var seen int
			fs.onStart = confPortAtStart(t, m, id, &seen)
			if err := m.StartStandby(context.Background(), id, func() (int, error) { t.Fatal("picked again"); return 0, nil }); err != nil {
				t.Fatal(err)
			}
			if seen != 57077 {
				t.Fatalf("the postmaster was started with the port %d of the conf, the marker says 57077", seen)
			}
			wantPort(t, m, id, 57077)
		})
	}
}

func TestReconcileLeavesAnInstanceThatRanAlone(t *testing.T) {
	fs := &fakeServer{starts: []fakeStart{{ok: true}}}
	m := newFakeManager(t, fs)
	id := newStandbyDir(t, m, 57002, false)
	// Files that disagree with the marker: for an instance that has run the marker is not the news, nothing is touched.
	s, _ := m.Settings(id)
	s.Port = 57555
	if err := m.saveSettings(id, s); err != nil {
		t.Fatal(err)
	}
	if err := m.reconcileStandbyPort(id); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Settings(id); got.Port != 57555 {
		t.Fatalf("settings of an instance that ran were rewritten: %d", got.Port)
	}
}

func TestAStartThatFindsTheInstanceRunningEndsTheProvisionalPort(t *testing.T) {
	// pg_ctl -w ran out of time (not a bind error), the postmaster went on: the next start finds it running.
	fs := &fakeServer{starts: []fakeStart{{log: "LOG:  starting\n", runs: true}}}
	m := newFakeManager(t, fs)
	id := newStandbyDir(t, m, 57002, true)
	if err := m.Start(context.Background(), id); err == nil {
		t.Fatal("the first start timed out")
	}
	if !m.NeverStarted(id) {
		t.Fatal("a failed start does not end the provisional port")
	}
	if err := m.Start(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if fs.nStart != 1 {
		t.Fatalf("the second call must not start again: %d starts", fs.nStart)
	}
	if m.NeverStarted(id) {
		t.Fatal("found running: the port is final")
	}
	// And it cannot be re-picked afterwards, even if a later start reports a bind error.
	if err := m.setStandbyPort(id, 57999); err == nil {
		t.Fatal("the port of an instance that ran must not change")
	}
}

func TestReconcileRepairsTheSettingsAlone(t *testing.T) {
	fs := &fakeServer{}
	m := newFakeManager(t, fs)
	id := newStandbyDir(t, m, 57002, true)
	s, _ := m.Settings(id)
	s.Port = 57555 // the conf and the marker say 57002
	if err := m.saveSettings(id, s); err != nil {
		t.Fatal(err)
	}
	if err := m.reconcileStandbyPort(id); err != nil {
		t.Fatal(err)
	}
	wantPort(t, m, id, 57002)
}

func TestReconcileRepairsTheConfAlone(t *testing.T) {
	fs := &fakeServer{}
	m := newFakeManager(t, fs)
	id := newStandbyDir(t, m, 57002, true)
	s, _ := m.Settings(id)
	s.Port = 57555
	if err := m.writeManagedConf(id, s); err != nil { // only the conf has the other port
		t.Fatal(err)
	}
	if err := m.reconcileStandbyPort(id); err != nil {
		t.Fatal(err)
	}
	wantPort(t, m, id, 57002)
}

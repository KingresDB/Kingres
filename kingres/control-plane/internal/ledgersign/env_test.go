// SPDX-License-Identifier: AGPL-3.0-only

package ledgersign_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerkeys"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgersign"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// fakeClock is the clock of the server and, unless a test makes them differ, of the database.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// t0 is the start of the fake clock: the real time, so that the modification time of a key file (the real clock)
// and the creation time of a task (the fake one) can be compared as they are in the daemon.
var t0 = time.Now().UTC().Truncate(time.Second)

// server is one simulated server: a key directory, an agent and a switch that makes it unreachable.
type server struct {
	id    string
	kr    *ledgersign.Keyring
	agent *ledgersign.Agent
	down  bool
}

// logSink collects every log line so that a test can search it.
type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}
func (l *logSink) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.buf.String() }

// env is a leader (server "s1") with the store, the task engine and some peers.
type env struct {
	t      *testing.T
	store  ledgersign.Store
	bump   func(context.Context) (scheduler.Term, error)
	mem    *ledgersign.MemStore // nil for the database store
	clk    *fakeClock
	dbClk  *fakeClock
	dir    string
	pass   string
	skew   time.Duration
	svc    *ledgersign.Service
	kr     *ledgersign.Keyring
	tasks  *task.MemStore
	store2 task.Store // the store the engine uses (a test wraps it to simulate a crash); nil: tasks
	eng    *task.Engine
	peers  map[string]*server
	hold   []string // servers that run an instance of group 0 (besides s1)
	logs   *logSink
	audits []audit.Event
	alerts []string

	mu       sync.Mutex
	term     scheduler.Term
	hasTerm  bool
	autorun  bool
	started  []task.Task
	resigned int
	fault    task.FaultFunc
}

type envOpt func(*env)

func withPassphrase(p string) envOpt { return func(e *env) { e.pass = p } }
func withPeers(ids ...string) envOpt { return func(e *env) { e.hold = ids } }

// withSkew sets how far the clock may differ from the database's: the database store has the real clock.
func withSkew(d time.Duration) envOpt { return func(e *env) { e.skew = d } }

func newEnv(t *testing.T, mk func(t *testing.T) (ledgersign.Store, func(context.Context) (scheduler.Term, error), *ledgersign.MemStore), opts ...envOpt) *env {
	t.Helper()
	e := &env{t: t, clk: &fakeClock{t: t0}, dbClk: nil, peers: map[string]*server{}, logs: &logSink{}}
	e.dbClk = e.clk
	for _, o := range opts {
		o(e)
	}
	e.store, e.bump, e.mem = mk(t)
	if e.mem != nil {
		e.mem.Now = e.dbClk.Now
	}
	e.dir = tempDir(t)
	e.kr = e.newKeyring(filepath.Join(e.dir, "s1"))
	e.tasks = task.NewMemStoreClock(e.clk.Now)
	for _, id := range e.hold {
		kr := e.newKeyring(filepath.Join(e.dir, id))
		e.peers[id] = &server{id: id, kr: kr, agent: &ledgersign.Agent{Keyring: kr}}
	}
	e.rebuild()
	e.becomeLeader()
	return e
}

func (e *env) newKeyring(dir string) *ledgersign.Keyring {
	pf := ""
	if e.pass != "" {
		pf = filepath.Join(e.dir, "pass-"+filepath.Base(dir))
		if err := os.WriteFile(pf, []byte(e.pass+"\n"), 0o600); err != nil {
			e.t.Fatal(err)
		}
	}
	kr, err := ledgersign.NewKeyring(dir, pf)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(kr.Close)
	return kr
}

// rebuild makes the service and the engine (a "restart" of the daemon keeps the store, the directories and the tasks).
func (e *env) rebuild() {
	e.t.Helper()
	svc, err := ledgersign.New(ledgersign.Config{
		Self: "s1", Store: e.store, Keyring: e.kr,
		Term:  func() (scheduler.Term, bool) { e.mu.Lock(); defer e.mu.Unlock(); return e.term, e.hasTerm },
		Clock: e.clk.Now, MaxClockSkew: e.skew, DefaultPrefix: "s1/kingres-ledger", TermWait: 200 * time.Millisecond,
		Holders: func(context.Context) ([]string, error) { return append([]string(nil), e.hold...), nil },
		Call:    e.call,
		StartTask: func(ctx context.Context, kind, st, sid string, in any, actor string) (task.Task, error) {
			tk, err := e.eng.Submit(ctx, kind, task.Subject{Type: st, ID: sid}, in, actor)
			if err != nil {
				return tk, err
			}
			e.started = append(e.started, tk)
			if e.autorun {
				if err := e.eng.RunNow(ctx, tk.ID, nil); err != nil {
					return tk, err
				}
			}
			return tk, nil
		},
		Resign: func(context.Context, ledgerkeys.Set) error { e.resigned++; return nil },
		Alert: func(_ context.Context, kind, sev, subj, msg string, raise bool) {
			e.alerts = append(e.alerts, kind+"/"+map[bool]string{true: "raise", false: "clear"}[raise]+"/"+msg)
		},
		Audit: func(_ context.Context, ev audit.Event) { e.audits = append(e.audits, ev) },
		Log:   slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		e.t.Fatal(err)
	}
	e.svc = svc
	var ts task.Store = e.tasks
	if e.store2 != nil {
		ts = e.store2
	}
	eng, err := task.NewEngine(ts, svc.Definitions(), task.Options{Holder: "A", Lease: 30 * time.Second, Now: e.clk.Now, StoreTries: 1, Fault: func(p string, id int64, step int) {
		if e.fault != nil {
			e.fault(p, id, step)
		}
	}})
	if err != nil {
		e.t.Fatal(err)
	}
	e.eng = eng
}

func (e *env) becomeLeader() {
	e.t.Helper()
	term, err := e.bump(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	e.mu.Lock()
	e.term, e.hasTerm = term, true
	e.mu.Unlock()
}

func (e *env) stopLeading() { e.mu.Lock(); e.hasTerm = false; e.mu.Unlock() }

func (e *env) call(ctx context.Context, srv, method, path string, in, out any) error {
	p := e.peers[srv]
	if p == nil || p.down {
		return errors.New("the server cannot be reached")
	}
	body, _ := json.Marshal(in)
	res, err := p.agent.Handle(ctx, method+" "+path, body)
	if err != nil {
		return err
	}
	if out != nil {
		b, _ := json.Marshal(res)
		return json.Unmarshal(b, out)
	}
	return nil
}

// run submits a task and runs it to its end; it returns the task row.
func (e *env) run(kind string, in any) task.Task {
	e.t.Helper()
	tk, err := e.eng.Submit(context.Background(), kind, task.Subject{Type: ledgersign.SubjectType, ID: ledgersign.SubjectID}, in, "tester")
	if err != nil {
		e.t.Fatalf("submit %s: %v", kind, err)
	}
	if err := e.eng.RunNow(context.Background(), tk.ID, nil); err != nil {
		e.t.Fatalf("run %s: %v", kind, err)
	}
	got, _ := e.eng.Get(context.Background(), tk.ID)
	return got
}

func (e *env) mustDone(tk task.Task) {
	e.t.Helper()
	if tk.State != task.Done {
		e.t.Fatalf("task %s is %s: %+v", tk.Kind, tk.State, tk.Error)
	}
}

// create runs the creation of a key through the operation, as the REST handler would, and the task.
func (e *env) create(days int) task.Task {
	e.t.Helper()
	e.autorun = false
	var req api.CreateLedgerSigningKeyRequest
	if days > 0 {
		req.OverlapDays = &days
	}
	tk, err := e.svc.CreateKey(context.Background(), "tester", req)
	if err != nil {
		e.t.Fatalf("CreateKey: %v", err)
	}
	if err := e.eng.RunNow(context.Background(), tk.ID, nil); err != nil {
		e.t.Fatalf("run: %v", err)
	}
	got, _ := e.eng.Get(context.Background(), tk.ID)
	e.mustDone(got)
	return got
}

func (e *env) signing() api.LedgerSigning {
	e.t.Helper()
	s, err := e.svc.Signing(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

// secrets returns the byte strings that must appear nowhere: the content of every key file of every server
// (it holds the seed, in clear or sealed) and the passphrase.
func (e *env) secrets() [][]byte {
	var out [][]byte
	dirs := []*ledgersign.Keyring{e.kr}
	for _, p := range e.peers {
		dirs = append(dirs, p.kr)
	}
	for _, kr := range dirs {
		fps, _ := kr.List()
		for _, fp := range fps {
			data, err := kr.File(fp)
			if err != nil {
				continue
			}
			out = append(out, data, []byte(base64.StdEncoding.EncodeToString(data)), []byte(hex.EncodeToString(data)))
			// the seed of a plain file is its base64 member
			var j struct {
				Seed string `json:"seed"`
			}
			if i := bytes.IndexByte(data, '\n'); i >= 0 && json.Unmarshal(data[i+1:], &j) == nil && j.Seed != "" {
				out = append(out, []byte(j.Seed))
				if raw, err := base64.StdEncoding.DecodeString(j.Seed); err == nil {
					out = append(out, raw, []byte(hex.EncodeToString(raw)))
				}
			}
		}
	}
	if e.pass != "" {
		out = append(out, []byte(e.pass))
	}
	return out
}

// noSecret fails when any secret is in text.
func (e *env) noSecret(what string, text []byte) {
	e.t.Helper()
	for _, s := range e.secretsAtStart() {
		if len(s) > 0 && bytes.Contains(text, s) {
			e.t.Errorf("%s contains a secret (%d bytes)", what, len(s))
		}
	}
}

// seen is a copy of the secrets that existed (a key that was removed later cannot be read again).
var seenSecrets struct {
	mu sync.Mutex
	s  map[*testing.T][][]byte
}

func (e *env) remember() {
	seenSecrets.mu.Lock()
	defer seenSecrets.mu.Unlock()
	if seenSecrets.s == nil {
		seenSecrets.s = map[*testing.T][][]byte{}
	}
	seenSecrets.s[e.t] = append(seenSecrets.s[e.t], e.secrets()...)
}

func (e *env) secretsAtStart() [][]byte {
	e.remember()
	seenSecrets.mu.Lock()
	defer seenSecrets.mu.Unlock()
	return seenSecrets.s[e.t]
}

// tempDir is t.TempDir with a removal that tries again: a scanner or an indexer on Windows may hold a file of the
// directory for a moment after the test (a known outside problem that fails the plain cleanup).
func tempDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "ledgersign-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for i := 0; i < 20; i++ {
			if os.RemoveAll(d) == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	})
	return d
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func joined(s []string) string { return strings.Join(s, ",") }

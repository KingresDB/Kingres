// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ledgeranchor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerkeys"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
)

// testClock is a clock that tests move.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *testClock { return &testClock{t: time.Now().UTC().Truncate(time.Second)} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// alertLog records what the service raises and clears, and the open set it is asked about.
type alertLog struct {
	mu     sync.Mutex
	open   map[[2]string]string // kind, subject -> severity
	msgs   map[[2]string]string
	writes int
}

func newAlertLog() *alertLog {
	return &alertLog{open: map[[2]string]string{}, msgs: map[[2]string]string{}}
}

func (a *alertLog) Alert(_ context.Context, kind, severity, subject, message string, raise bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.writes++
	k := [2]string{kind, subject}
	if raise {
		a.open[k], a.msgs[k] = severity, message
	} else {
		delete(a.open, k)
		delete(a.msgs, k)
	}
}

func (a *alertLog) Open(context.Context) ([]OpenAlert, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []OpenAlert
	for k, sev := range a.open {
		out = append(out, OpenAlert{Kind: k[0], Subject: k[1], Severity: sev})
	}
	return out, nil
}

func (a *alertLog) severity(kind, subject string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.open[[2]string{kind, subject}]
}

func (a *alertLog) all() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var sb strings.Builder
	for k, m := range a.msgs {
		sb.WriteString(k[0] + " " + k[1] + " " + a.open[k] + " " + m + "\n")
	}
	return sb.String()
}

type env struct {
	t  *testing.T
	st *MemStore
	// store is what the service uses: st, or the store in PostgreSQL of a database test (pg is then the connection that adds checkpoints)
	store   Store
	pg      *pgx.Conn
	svc     *Service
	s3      *fakeS3
	clock   *testClock
	dir     string
	logs    *bytes.Buffer
	alerts  *alertLog
	creds   *CredStore
	term    scheduler.Term
	leading bool
	dbs     []string
	cfg     Config
}

// secrets used by the tests: distinctive, with characters that change under URL encoding and base64.
const (
	testKeyID  = fakeKeyID
	testSecret = fakeSecret
)

// newEnv builds a service over a memory store and the fake store. passFile "" writes the credentials in clear.
func newEnv(t *testing.T, mutate ...func(*env)) *env {
	t.Helper()
	e := &env{t: t, st: NewMemStore(), clock: newClock(), alerts: newAlertLog(), logs: &bytes.Buffer{}, term: 1, leading: true, dbs: []string{"shop"}}
	e.store = e.st
	e.s3 = newFakeS3()
	e.s3.clock = e.clock.Now
	t.Cleanup(e.s3.close)
	e.dir = t.TempDir()
	for _, m := range mutate {
		m(e)
	}
	e.rebuild("")
	return e
}

// rebuild makes the service again (a restart), with a passphrase file when passFile is not empty.
func (e *env) rebuild(passFile string) {
	e.t.Helper()
	cs, err := NewCredStore(e.dir, passFile)
	if err != nil {
		e.t.Fatal(err)
	}
	cs.Iterations, cs.MinIterations = 2000, 1000
	e.creds = cs
	e.cfg = Config{
		Store: e.store, Creds: cs,
		Term:  func() (scheduler.Term, bool) { return e.term, e.leading },
		Clock: e.clock.Now,
		Log:   slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Alert: e.alerts.Alert, OpenAlerts: e.alerts.Open,
		Databases:      func(context.Context) ([]string, error) { return e.dbs, nil },
		DatabaseExists: func(_ context.Context, db string) (bool, error) { return db == "shop" || db == "other", nil },
		Rand:           func() float64 { return 0.5 },
		SubmitTimeout:  5 * time.Second,
	}
	e.svc, err = New(e.cfg)
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) ctx() context.Context { return context.Background() }

// wormConfig is the configuration of a target on the fake store.
func (e *env) wormConfig(extra map[string]any) json.RawMessage {
	m := map[string]any{"endpoint": e.s3.srv.URL, "region": fakeRegion, "bucket": fakeBucket, "allowInsecure": true,
		"retentionDays": 30, "mode": "compliance", "ifNoneMatch": "on"}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return b
}

func wormCreds() json.RawMessage {
	b, _ := json.Marshal(map[string]string{"accessKeyId": testKeyID, "secretAccessKey": testSecret})
	return b
}

// addWorm creates and enables a WORM target on the fake store.
func (e *env) addWorm(name string) api.LedgerAnchorTarget {
	e.t.Helper()
	tg, err := e.svc.CreateTarget(e.ctx(), "admin", api.LedgerAnchorTargetCreate{Kind: "worm", Name: name, Enabled: true, Config: e.wormConfig(nil), Credentials: wormCreds()})
	if err != nil {
		e.t.Fatalf("creating the target: %v", err)
	}
	return tg
}

var aggHexCounter int

// putAgg adds a checkpoint of size to the aggregate log of a database.
func (e *env) putAgg(db string, size int64) {
	e.t.Helper()
	id := sha256.Sum256([]byte("aggregate " + db))
	hexID := hex.EncodeToString(id[:])
	origin := "kingres.test/kingres-ledger/a/" + hexID
	if e.pg != nil {
		var logID int64
		if err := e.pg.QueryRow(bg(), `INSERT INTO kingres_cp.ledger_log (kind, database_name, epoch, origin) VALUES ($1, $2, 1, $3) ON CONFLICT (origin) DO UPDATE SET kind = excluded.kind RETURNING log_id`, "aggregate", db, origin).Scan(&logID); err != nil {
			e.t.Fatal(err)
		}
		if _, err := e.pg.Exec(bg(), `INSERT INTO kingres_cp.ledger_checkpoint (log_id, size, root, note, signed_at) VALUES ($1, $2, $3, $4, $5)`, logID, size, id[:], makeNote(origin, uint64(size)), e.clock.Now()); err != nil {
			e.t.Fatal(err)
		}
		return
	}
	e.st.PutCheckpoint(db, origin, hexID, size, makeNote(origin, uint64(size)), e.clock.Now())
}

// makeNote is a signed note of the shape the libraries require.
func makeNote(origin string, size uint64) []byte {
	root := sha256.Sum256([]byte(fmt.Sprintf("%s root %d", origin, size)))
	sig := base64.StdEncoding.EncodeToString(append([]byte{1, 2, 3, 4}, bytes.Repeat([]byte{7}, 64)...))
	return []byte(fmt.Sprintf("%s\n%d\n%s\n\n— kingres-test %s\n", origin, size, base64.StdEncoding.EncodeToString(root[:]), sig))
}

// runEnqueue, runSubmit and runUpgrade run one pass of a job as the scheduler would.
func (e *env) runEnqueue() error {
	e.t.Helper()
	e.svc.mu.Lock()
	e.svc.lastEnqueue = time.Time{}
	e.svc.mu.Unlock()
	return e.svc.jobEnqueue(e.ctx(), e.term)
}
func (e *env) runSubmit() error  { return e.svc.jobSubmit(e.ctx(), e.term) }
func (e *env) runUpgrade() error { return e.svc.jobUpgrade(e.ctx(), e.term) }
func (e *env) runMonitor() error { return e.svc.jobMonitor(e.ctx(), e.term) }

func (e *env) rows() []Row {
	rs, err := e.store.Records(e.ctx(), Filter{})
	if err != nil {
		e.t.Fatal(err)
	}
	// oldest first
	for i, j := 0, len(rs)-1; i < j; i, j = i+1, j-1 {
		rs[i], rs[j] = rs[j], rs[i]
	}
	return rs
}

func (e *env) row(id int64) Row {
	r, err := e.store.Record(e.ctx(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

// allText is every text the service could have leaked a credential into: the log, the alerts, the rows of the
// store as the program would print and encode them, and the last tests of the targets.
func (e *env) allText() string {
	var sb strings.Builder
	sb.WriteString(e.logs.String())
	sb.WriteString(e.alerts.all())
	rows, _ := e.store.Records(e.ctx(), Filter{})
	for _, r := range rows {
		rr, _ := e.store.Record(e.ctx(), r.ID)
		fmt.Fprintf(&sb, "%+v %v\n", rr, rr)
		b, _ := json.Marshal(rr)
		sb.Write(b)
		a, _ := json.Marshal(apiAnchor(rr))
		sb.Write(a)
	}
	ts, _ := e.store.Targets(e.ctx())
	for _, t := range ts {
		fmt.Fprintf(&sb, "%+v\n", t)
		b, _ := json.Marshal(t)
		sb.Write(b)
	}
	if l, err := e.svc.Targets(e.ctx()); err == nil {
		b, _ := json.Marshal(l)
		sb.Write(b)
	}
	return sb.String()
}

// forms is a secret in every spelling the tests look for: raw, hex, base64 (standard and URL), and URL-encoded.
func forms(secret string) []string {
	return []string{secret, hex.EncodeToString([]byte(secret)), base64.StdEncoding.EncodeToString([]byte(secret)),
		base64.RawStdEncoding.EncodeToString([]byte(secret)), base64.URLEncoding.EncodeToString([]byte(secret)),
		base64.RawURLEncoding.EncodeToString([]byte(secret)), urlEncode(secret), strings.ToUpper(hex.EncodeToString([]byte(secret)))}
}

func urlEncode(s string) string {
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			sb.WriteByte(c)
		} else {
			fmt.Fprintf(&sb, "%%%02X", c)
		}
	}
	return sb.String()
}

// noSecret fails when text holds any spelling of any of the secrets.
func noSecret(t *testing.T, where, text string, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		for _, f := range forms(s) {
			if len(f) >= 8 && strings.Contains(text, f) {
				t.Errorf("%s holds a credential (%d characters of it, as %q...)", where, len(s), f[:6])
			}
		}
	}
}

// keyFiles reads every file below the key directory.
func (e *env) keyFiles() map[string][]byte {
	out := map[string][]byte{}
	_ = filepath.Walk(e.dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			b, _ := os.ReadFile(p)
			out[p] = b
		}
		return nil
	})
	return out
}

func mustRaw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

var _ = ledgerkeys.Iterations
var _ = anchor.KindWORM

func bg() context.Context { return context.Background() }

func filepathBase(p string) string { return filepath.Base(p) }

func mustString(v any) string { return string(mustRaw(v)) }

func writePassphraseWith(t *testing.T, text string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pass")
	if err := os.WriteFile(p, []byte(text+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func writePassphrase(t *testing.T) string {
	return writePassphraseWith(t, "the passphrase of this test server")
}

// checkMode fails when a credentials file can be read by group or others (systems with modes only).
func checkMode(t *testing.T, p string) {
	t.Helper()
	if filepath.Separator != '/' {
		return
	}
	if st, err := os.Stat(p); err != nil || st.Mode().Perm()&0o077 != 0 {
		t.Errorf("the mode of %s: %v %v", filepath.Base(p), st.Mode(), err)
	}
}

func pageOf(n int) store.Page { return store.Page{Limit: n} }

func writeFileMode(p, body string) error { return os.WriteFile(p, []byte(body), 0o600) }

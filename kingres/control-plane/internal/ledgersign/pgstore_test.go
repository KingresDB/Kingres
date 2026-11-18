// SPDX-License-Identifier: AGPL-3.0-only

package ledgersign_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerkeys"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgersign"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// pgInstance starts a PostgreSQL instance with the system database and the schema kingres_cp (every migration, so
// the test also runs the migration 0030 on a real database). The tests skip without KINGRES_PG_BIN.
func pgInstance(t *testing.T) (ledgersign.PGStore, *pgx.Conn) {
	t.Helper()
	in := pgtest.Start(t, nil)
	ctx := context.Background()
	if _, err := in.Connect(t, "postgres").Exec(ctx, "CREATE DATABASE kingres_system"); err != nil {
		t.Fatal(err)
	}
	admin := in.Connect(t, "kingres_system")
	if _, err := sysdb.Migrate(ctx, admin); err != nil {
		t.Fatal(err)
	}
	// Every call opens a connection of its own, as the daemon does (it runs as the service role).
	sys := func(ctx context.Context, f func(c *pgx.Conn) error) error {
		cfg, err := pgx.ParseConfig(in.DSN("kingres_system"))
		if err != nil {
			return err
		}
		c, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			return err
		}
		defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
		return f(c)
	}
	return ledgersign.PGStore{Sys: sys}, admin
}

func pgFactoryOf(t *testing.T) func(*testing.T) (ledgersign.Store, func(context.Context) (scheduler.Term, error), *ledgersign.MemStore) {
	st, _ := pgInstance(t)
	return func(*testing.T) (ledgersign.Store, func(context.Context) (scheduler.Term, error), *ledgersign.MemStore) {
		return st, st.BumpTerm, nil
	}
}

// The whole life of the keys, on the database store: the first key, a rotation, the retirement of the older key and
// the revocation of the second together with the creation of a third. Every row is read back and the trust file
// compared with the one built from the rows.
func TestTheKeyLifecycleOnTheDatabaseStore(t *testing.T) {
	mk := pgFactoryOf(t)
	e := newEnv(t, mk, withPeers("s2"), withSkew(100*365*24*time.Hour))
	e.create(0)
	k1 := keyIn(t, load(t, e), ledgerkeys.StateActive)
	e.clk.Add(time.Hour)
	e.create(1)
	checkSet(t, e, 2, 0, 0, 2)
	e.clk.Add(25 * time.Hour)
	e.mustDone(e.run("ledger.key.retire", map[string]any{"fingerprint": k1.Fingerprint()}))
	checkSet(t, e, 1, 1, 0, 3)
	s := load(t, e)
	r, _ := s.Set().Get(k1.Fingerprint())
	if r.MaxTreeSize != 1 || r.State != ledgerkeys.StateRetired || !r.RetiredAt.Equal(e.clk.Now()) {
		t.Errorf("the retired key as read back: %+v", r)
	}
	if err := s.Set().Validate(); err != nil {
		t.Error(err)
	}
	if signers, _, err := e.svc.Signers(context.Background()); err != nil || len(signers) != 1 {
		t.Errorf("Signers: %v %d", err, len(signers))
	}
	// A second service over the same rows (a new leader after a restart) sees the same state and can sign.
	e.rebuild()
	if signers, _, err := e.svc.Signers(context.Background()); err != nil || len(signers) != 1 {
		t.Errorf("Signers after the rebuild: %v %d", err, len(signers))
	}
}

// A deposed leader cannot write: the term is checked under a share lock in the transaction of the write, the
// increment of the next leader waits for a write in flight, and a write with the old term fails afterwards.
func TestADeposedWriterCannotWriteOnTheDatabaseStore(t *testing.T) {
	st, admin := pgInstance(t)
	ctx := context.Background()
	t1, err := st.BumpTerm(ctx)
	if err != nil || t1 != 1 {
		t.Fatalf("the first term: %d %v", t1, err)
	}
	snap, err := st.Load(ctx)
	if err != nil || snap.Term != 1 || snap.Revision != 0 {
		t.Fatalf("Load: %+v %v", snap, err)
	}
	// A write in flight: a transaction that has run the guard and not committed yet.
	tx, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := t1.Guard(ctx, tx); err != nil {
		t.Fatal(err)
	}
	bumped := make(chan scheduler.Term, 1)
	go func() {
		n, err := st.BumpTerm(ctx)
		if err != nil {
			t.Error(err)
		}
		bumped <- n
	}()
	select {
	case <-bumped:
		t.Fatal("the next leader took a term while a write of the old one was in flight")
	case <-time.After(500 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var t2 scheduler.Term
	select {
	case t2 = <-bumped:
	case <-time.After(10 * time.Second):
		t.Fatal("the next leader never took its term")
	}
	if t2 != 2 {
		t.Fatalf("the second term is %d", t2)
	}
	// The deposed leader wakes up and writes: refused, and nothing changed.
	cl := snap.Cluster
	on := true
	cl.PublicationEnabled = on
	if err := st.Commit(ctx, t1, snap.Revision, ledgersign.Change{Cluster: &cl}); !errors.Is(err, scheduler.ErrDeposed) {
		t.Fatalf("the write of the deposed leader: %v", err)
	}
	after, _ := st.Load(ctx)
	if after.Cluster.PublicationEnabled || after.Revision != 0 {
		t.Error("the deposed leader changed the row")
	}
	// A guard of the old term fails by itself too (the first statement of every write of a job).
	tx2, _ := admin.Begin(ctx)
	if err := t1.Guard(ctx, tx2); !errors.Is(err, scheduler.ErrDeposed) {
		t.Errorf("Guard with the old term: %v", err)
	}
	_ = tx2.Rollback(ctx)
	// The new leader writes.
	if err := st.Commit(ctx, t2, after.Revision, ledgersign.Change{Cluster: &cl}); err != nil {
		t.Fatalf("the write of the new leader: %v", err)
	}
}

// Two changes that read the same state cannot both be written: the revision is a compare and swap.
func TestTwoChangesFromOneStoredStateCannotBothBeWritten(t *testing.T) {
	st, _ := pgInstance(t)
	ctx := context.Background()
	term, _ := st.BumpTerm(ctx)
	snap, _ := st.Load(ctx)
	var wg sync.WaitGroup
	results := make([]error, 4)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cl := snap.Cluster
			cl.AnchorMinimum = i + 1
			results[i] = st.Commit(ctx, term, snap.Revision, ledgersign.Change{Cluster: &cl})
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range results {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, ledgersign.ErrConflict):
			t.Errorf("a change that lost: %v", err)
		}
	}
	if ok != 1 {
		t.Errorf("%d of 4 changes from one state were written", ok)
	}
	after, _ := st.Load(ctx)
	if after.Revision != 1 {
		t.Errorf("revision %d", after.Revision)
	}
	// One after the other, without the help of a deadlock between two share locks: a change that names the revision that
	// was replaced is refused, and one that names the revision in force is written.
	stale := after.Cluster
	stale.AnchorMinimum = 7
	if err := st.Commit(ctx, term, snap.Revision, ledgersign.Change{Cluster: &stale}); !errors.Is(err, ledgersign.ErrConflict) {
		t.Errorf("a change that names a revision that was replaced: %v", err)
	}
	if err := st.Commit(ctx, term, after.Revision, ledgersign.Change{Cluster: &stale}); err != nil {
		t.Errorf("a change that names the revision in force: %v", err)
	}
	if again, _ := st.Load(ctx); again.Revision != 2 || again.Cluster.AnchorMinimum != 7 {
		t.Errorf("after two changes: revision %d, minimum %d", again.Revision, again.Cluster.AnchorMinimum)
	}
}

// What the store keeps is what it gives back, including the members the key set checks (times, bound, reason, the
// task) and the arrays of the cluster row; and the schema refuses what the daemon never writes.
func TestTheDatabaseStoreKeepsWhatItIsGiven(t *testing.T) {
	st, admin := pgInstance(t)
	ctx := context.Background()
	term, _ := st.BumpTerm(ctx)
	snap, _ := st.Load(ctx)
	g, _ := ledgerkeys.GenerateKey(nil)
	defer func() { _ = g.Close() }()
	created := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	k := ledgersign.KeyRow{Key: ledgerkeys.Key{Public: g.Public(), Label: "x", State: ledgerkeys.StateRetired, CreatedAt: created, ActivatedAt: created.Add(time.Minute),
		RetiredAt: created.Add(time.Hour), MaxTreeSize: 77, Reason: ""}, Mode: "file", TaskID: 9}
	url := "https://x.example"
	cl := snap.Cluster
	cl.ClusterID, cl.OriginPrefix, cl.PublicURL, cl.TrustFile, cl.Holders, cl.BackupAdvised = make([]byte, 32), "a/b", &url, []byte("{}\n"), []string{"s1", "s2"}, true
	ev := ledgersign.EventRow{Seq: 0, Type: "key-added", Subject: k.Fingerprint(), Note: []byte("note\n\n\u2014 x AAAA\n"), CreatedAt: created}
	if err := st.Commit(ctx, term, snap.Revision, ledgersign.Change{Cluster: &cl, Keys: []ledgersign.KeyRow{k}, Events: []ledgersign.EventRow{ev}}); err != nil {
		t.Fatal(err)
	}
	got, err := st.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Keys) != 1 || got.Keys[0].MaxTreeSize != 77 || got.Keys[0].State != ledgerkeys.StateRetired || !got.Keys[0].RetiredAt.Equal(k.RetiredAt) || got.Keys[0].TaskID != 9 || got.Keys[0].Label != "x" {
		t.Errorf("the key as read back: %+v", got.Keys)
	}
	c := got.Cluster
	if string(c.TrustFile) != "{}\n" || c.PublicURL == nil || *c.PublicURL != url || c.OriginPrefix != "a/b" || len(c.Holders) != 2 || !c.BackupAdvised || len(c.ClusterID) != 32 {
		t.Errorf("the cluster row as read back: %+v", c)
	}
	if len(got.Events) != 1 || string(got.Events[0].Note) != string(ev.Note) || got.Events[0].Subject != k.Fingerprint() {
		t.Errorf("events: %+v", got.Events)
	}
	// An event twice for one key, an event out of order, a second cluster row and a malformed fingerprint are refused.
	more := ledgersign.EventRow{Seq: 1, Type: "key-added", Subject: k.Fingerprint(), Note: []byte("n"), CreatedAt: created}
	if err := st.Commit(ctx, term, got.Revision, ledgersign.Change{Events: []ledgersign.EventRow{more}}); err == nil {
		t.Error("a second key-added event for one key was accepted")
	}
	skip := ledgersign.EventRow{Seq: 5, Type: "key-added", Subject: "", Note: []byte("n"), CreatedAt: created}
	_ = skip
	if _, err := admin.Exec(ctx, `INSERT INTO kingres_cp.ledger_cluster (id) VALUES (false)`); err == nil {
		t.Error("a second row of the cluster table was accepted")
	}
	if _, err := admin.Exec(ctx, `INSERT INTO kingres_cp.ledger_signing_key (fingerprint, public_key, state, created_at) VALUES ('abc', '\x00', 'next', now())`); err == nil {
		t.Error("a malformed fingerprint was accepted")
	}
	// The failed commit left nothing (one transaction): the revision did not move.
	after, _ := st.Load(ctx)
	if after.Revision != got.Revision || len(after.Events) != 1 {
		t.Errorf("a refused change left a trace: revision %d, %d events", after.Revision, len(after.Events))
	}
	// The database clock is read, and the largest checkpoint is 0 without a log.
	if d, err := st.DBTime(ctx); err != nil || time.Since(d) > time.Minute || time.Since(d) < -time.Minute {
		t.Errorf("DBTime: %v %v", d, err)
	}
	if n, err := st.MaxCheckpointSize(ctx); err != nil || n != 0 {
		t.Errorf("MaxCheckpointSize: %d %v", n, err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO kingres_cp.ledger_log (kind, origin, size) VALUES ('chain', 'a/b/c/x/e1', 10)`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO kingres_cp.ledger_checkpoint (log_id, size, root, note, signed_at) SELECT log_id, 10, '\x00', '\x00', now() FROM kingres_cp.ledger_log`); err != nil {
		t.Fatal(err)
	}
	if n, err := st.MaxCheckpointSize(ctx); err != nil || n != 10 {
		t.Errorf("MaxCheckpointSize with a checkpoint: %d %v", n, err)
	}
	if o, err := st.LogOrigins(ctx); err != nil || len(o) != 1 || o[0] != "a/b/c/x/e1" {
		t.Errorf("LogOrigins: %v %v", o, err)
	}
}

// The same kills on the database store: every task kind, killed when a step has run and when a step has started, over a
// store that is emptied between two runs (the term goes on growing, as it does after a restart of the daemon).
func TestKillingKeyTasksOnTheDatabaseStore(t *testing.T) {
	st, admin := pgInstance(t)
	mk := func(*testing.T) (ledgersign.Store, func(context.Context) (scheduler.Term, error), *ledgersign.MemStore) {
		return st, st.BumpTerm, nil
	}
	reset := func() {
		if _, err := admin.Exec(context.Background(), `TRUNCATE kingres_cp.ledger_event, kingres_cp.ledger_signing_key;
			UPDATE kingres_cp.ledger_cluster SET cluster_id = NULL, origin_prefix = NULL, trust_file = NULL, holders = '{}', keys_revision = 0, backup_advised = false`); err != nil {
			t.Fatal(err)
		}
	}
	for _, sc := range scenarios() {
		for _, point := range []string{task.FaultStepStarted, task.FaultStepRan} {
			for k := 0; k < sc.steps; k++ {
				t.Run(fmt.Sprintf("%s/%s#%d", sc.name, point, k), func(t *testing.T) {
					reset()
					crashRun(t, newEnv(t, mk, withPeers("s2", "s3"), withSkew(100*365*24*time.Hour)), sc, point, k)
				})
			}
		}
	}
}

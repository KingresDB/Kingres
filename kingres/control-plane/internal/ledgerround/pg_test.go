// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ledgerround

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerkeys"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerlog"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerpub"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgersign"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// pgKeys is the key side of the collector on a database: the term and the revision of the key set are read from kingres_cp.ledger_cluster, as
// the daemon's service reads them.
type pgKeys struct {
	admin   *pgx.Conn
	signers []ledgerkeys.Signer
	mu      sync.Mutex
	events  map[string]bool
	term    scheduler.Term
}

func (k *pgKeys) Publication(ctx context.Context) (ledgersign.PubContext, error) {
	var rev int64
	if err := k.admin.QueryRow(ctx, `SELECT keys_revision FROM kingres_cp.ledger_cluster`).Scan(&rev); err != nil {
		return ledgersign.PubContext{}, err
	}
	id := make([]byte, 32)
	id[0] = 9
	return ledgersign.PubContext{Term: k.term, Enabled: true, Interval: time.Minute, Prefix: testPrefix, ClusterID: id, Revision: rev, Signers: k.signers}, nil
}

func (k *pgKeys) AppendEvent(_ context.Context, typ, subject string, _ []ledgersign.EventField) (int64, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.events == nil {
		k.events = map[string]bool{}
	}
	k.events[typ+"|"+subject] = true
	return int64(len(k.events)), nil
}

type crashed struct{ kind string }

func pgEnv(t *testing.T) (*ledgerpub.PGStore, *pgKeys, *pgx.Conn) {
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
	var term int64
	if err := admin.QueryRow(ctx, `UPDATE kingres_cp.ledger_cluster SET term = term + 1 RETURNING term`).Scan(&term); err != nil {
		t.Fatal(err)
	}
	return &ledgerpub.PGStore{Sys: sys}, &pgKeys{admin: admin, signers: []ledgerkeys.Signer{signer(t, 1)}, term: scheduler.Term(term)}, admin
}

func pgCollector(t *testing.T, st ledgerpub.Store, k Keys, src Source) *Collector {
	t.Helper()
	c, err := New(Config{Store: st, Source: src, Keys: k, Clock: func() time.Time { return time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC) },
		Databases: func(context.Context) ([]Database, error) { return []Database{{Name: "shop"}}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func pgVerify(t *testing.T, st ledgerpub.Store, pub ed25519.PublicKey) {
	t.Helper()
	ctx := context.Background()
	logs, err := st.Logs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range logs {
		if l.State == ledgerpub.StateDiverged {
			continue
		}
		cp, err := st.LatestCheckpoint(ctx, l.ID)
		if err != nil {
			t.Fatalf("%s: %v", l.Origin, err)
		}
		c, _, err := ledgerlog.VerifyCheckpoint(cp.Note, l.Origin, pub)
		if err != nil {
			t.Fatalf("%s: %v", l.Origin, err)
		}
		if err := ledgerlog.Audit(ctx, st.Tiles(l.ID, 0), c.Size, c.Root); err != nil {
			t.Fatalf("audit of %s: %v", l.Origin, err)
		}
	}
}

// The collector on the database store: the rounds, the checkpoints, a round cut at each of its writes (the process ends after the term guard
// of the write and before its statements, as a kill does), a divergence and the end of a database, with the SQL of the store.
func TestRoundsCutAtEveryWriteOnTheDatabaseStore(t *testing.T) {
	st, keys, admin := pgEnv(t)
	ctx := context.Background()
	pub := keys.signers[0].Public()
	clean := func() {
		for _, tbl := range []string{"ledger_tile", "ledger_checkpoint", "ledger_log"} {
			if _, err := admin.Exec(ctx, `DELETE FROM kingres_cp.`+tbl); err != nil {
				t.Fatal(err)
			}
		}
	}
	// the reference: the writes of a round that adds 300 blocks to a chain at 3 and 1 to a chain at 2
	a, b := newChain(1, 3), newChain(40, 2)
	src := newSource(a, b)
	c := pgCollector(t, st, keys, src)
	if _, err := c.Round(ctx, "shop", OpHead); err != nil {
		t.Fatal(err)
	}
	pgVerify(t, st, pub)
	a.add(300)
	b.add(1)
	count := 0
	st.OnWrite = func(string) { count++ }
	if _, err := c.Round(ctx, "shop", OpHead); err != nil {
		t.Fatal(err)
	}
	st.OnWrite = nil
	pgVerify(t, st, pub)
	writes := count
	if writes < 10 {
		t.Fatalf("%d writes", writes)
	}
	refRoot := referenceRoot(a, 303)
	for cut := 1; cut <= writes; cut++ {
		clean()
		a, b = newChain(1, 3), newChain(40, 2)
		src = newSource(a, b)
		c = pgCollector(t, st, keys, src)
		if _, err := c.Round(ctx, "shop", OpHead); err != nil {
			t.Fatal(err)
		}
		a.add(300)
		b.add(1)
		n := 0
		st.OnWrite = func(kind string) {
			n++
			if n == cut {
				panic(crashed{kind})
			}
		}
		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("cut %d: the round was not cut", cut)
				}
			}()
			_, _ = c.Round(ctx, "shop", OpHead)
		}()
		st.OnWrite = nil
		c = pgCollector(t, st, keys, src) // a new process
		for i := 0; i < 2; i++ {
			if _, err := c.Round(ctx, "shop", OpHead); err != nil {
				t.Fatalf("cut %d: %v", cut, err)
			}
		}
		pgVerify(t, st, pub)
		logs, _ := st.Logs(ctx)
		for _, l := range logs {
			if l.State != ledgerpub.StateActive {
				t.Errorf("cut %d: %s is %s (%s)", cut, l.Origin, l.State, l.Detail)
			}
			if l.Kind == ledgerpub.KindChain && l.Size == 303 && fmt.Sprintf("%x", l.Root) != fmt.Sprintf("%x", refRoot[:]) {
				t.Errorf("cut %d: root %x", cut, l.Root)
			}
		}
	}
}

func TestDivergenceAndRetirementOnTheDatabaseStore(t *testing.T) {
	st, keys, _ := pgEnv(t)
	ctx := context.Background()
	pub := keys.signers[0].Public()
	a, b := newChain(1, 10), newChain(40, 3)
	src := newSource(a, b)
	c := pgCollector(t, st, keys, src)
	if _, err := c.Round(ctx, "shop", OpHead); err != nil {
		t.Fatal(err)
	}
	before, _ := st.Logs(ctx)
	// a lost tail: the chain of group 0 is cut to 6 blocks
	a.blocks = a.blocks[:6]
	b.add(1)
	res, err := c.Round(ctx, "shop", OpHead)
	if err != nil || !res.Groups[0].Diverged || res.Groups[1].Diverged {
		t.Fatalf("%+v %v", res, err)
	}
	after, _ := st.Logs(ctx)
	for i := range after {
		if after[i].Kind == ledgerpub.KindChain && after[i].ShardGroupID != nil && *after[i].ShardGroupID == 0 {
			if after[i].State != ledgerpub.StateDiverged || after[i].Size != before[i].Size || string(after[i].Root) != string(before[i].Root) {
				t.Errorf("the diverged log: %+v", after[i])
			}
		}
	}
	// a deposed leader: its term is not the stored one, and the first write of its round is refused
	b.add(1)
	keys.term += 5
	if _, err := c.Round(ctx, "shop", OpHead); !errors.Is(err, scheduler.ErrDeposed) {
		t.Errorf("a round under another term: %v", err)
	}
	keys.term -= 5
	// the end of the database: events, closed logs, one retired leaf
	if err := c.RetireDatabase(ctx, "shop", ReasonDropped, true); err != nil {
		t.Fatal(err)
	}
	if len(keys.events) != 2 {
		t.Errorf("events %v", keys.events)
	}
	logs, _ := st.Logs(ctx)
	for _, l := range logs {
		if l.Kind == ledgerpub.KindChain && (l.State != ledgerpub.StateClosed || l.RetiredInSeq == nil) {
			t.Errorf("%s: %s %v", l.Origin, l.State, l.RetiredInSeq)
		}
	}
	pgVerify(t, st, pub)
}

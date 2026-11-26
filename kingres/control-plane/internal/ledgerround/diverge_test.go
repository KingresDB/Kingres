// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ledgerround

import (
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerlog"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerpub"
)

// Divergence (design 4.7, 9.4): the chain the group holds no longer extends the log that was published. Whatever the cause, the publisher
// stops that log, flags its aggregate member diverged with the last good head, raises the critical alert, and signs nothing more for it:
// no tile and no checkpoint is touched, a head that comes back to what it was does not wake the log up, and the other chains go on.
func TestADivergedChainStopsItsLogAndNothingIsRepaired(t *testing.T) {
	cases := []struct {
		name   string
		damage func(c *fakeChain)
		why    string
	}{
		{"an older backup was restored (the head is lower)", func(c *fakeChain) { c.blocks = c.blocks[:6] }, "below the"},
		{"the tail was lost down to the first block", func(c *fakeChain) { c.blocks = c.blocks[:1] }, "below the"},
		{"an older backup was restored and the chain grew again with other blocks", func(c *fakeChain) {
			c.blocks = c.blocks[:6]
			c.salt++
			c.add(6)
		}, "not the block that was published"},
		{"a published block was rewritten, the later ones relinked", func(c *fakeChain) { c.rewrite(3) }, "not the block that was published"},
		{"the last published block was rewritten and the chain grew", func(c *fakeChain) { c.rewrite(9); c.add(2) }, "not the block that was published"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, b := newChain(1, 10), newChain(40, 4)
			e := newEnv(t, a, b)
			e.round()
			before := snapshot(t, e)
			good := e.chainLog(0)
			tc.damage(a)
			b.add(2)
			res := e.round()
			l := e.chainLog(0)
			if l.State != ledgerpub.StateDiverged || !strings.Contains(l.Detail, tc.why) {
				t.Fatalf("state %s detail %q", l.State, l.Detail)
			}
			if !res.Groups[0].Diverged || res.Groups[1].Diverged {
				t.Errorf("diverged flags %+v", res.Groups)
			}
			if !e.hasAlert(AlertDiverged + "/raise/critical/database:shop|group:0") {
				t.Errorf("alerts %v", e.alerts)
			}
			// nothing of the diverged log changed: the row, the tiles, the checkpoints (the snapshot differs only in the other chain and the
			// aggregate)
			after := snapshot(t, e)
			for k, v := range before {
				if !strings.HasPrefix(k, good.Origin) {
					continue
				}
				if strings.HasSuffix(k, "|row") {
					if !strings.HasPrefix(after[k], "diverged") || strings.TrimPrefix(after[k], "diverged") != strings.TrimPrefix(v, "active") {
						t.Errorf("the row of the diverged log changed: %q -> %q", v, after[k])
					}
					continue
				}
				if after[k] != v {
					t.Errorf("%s changed", k)
				}
			}
			for k := range after {
				if strings.HasPrefix(k, good.Origin) && before[k] == "" {
					t.Errorf("%s is new", k)
				}
			}
			// the other chain went on, and the aggregate says diverged with the last good head
			if got := e.chainLog(1).Size; got != 6 {
				t.Errorf("the other chain is at %d", got)
			}
			ar := e.aggregateLog()
			r, _ := ledgerlog.NewReader(bg, e.store.Tiles(ar.ID, 0), uint64(ar.Size))
			raw, _ := r.Entry(uint64(ar.Size) - 1)
			leaf, err := ledgerlog.ParseAggregateEntry(raw)
			if err != nil {
				t.Fatal(err)
			}
			var found bool
			for _, m := range leaf.Members {
				if m.Flags&ledgerlog.FlagDiverged != 0 {
					found = true
					if m.Height != 9 || m.BlockHash != func() [32]byte { var h [32]byte; copy(h[:], good.HeadBlockHash); return h }() {
						t.Errorf("the diverged member must carry the last good head, got height %d", m.Height)
					}
				}
			}
			if !found {
				t.Error("no diverged member in the newest leaf")
			}
			// the chain comes back to the history that was published: the log stays stopped, and nothing is written for it
			*a = *newChain(1, 10)
			w := e.store.Writes()
			a.add(5)
			e.round()
			if e.chainLog(0).State != ledgerpub.StateDiverged || e.chainLog(0).Size != 10 {
				t.Errorf("the log woke up: %s size %d", e.chainLog(0).State, e.chainLog(0).Size)
			}
			_ = w
			// a restart does not change it either
			e.restart()
			e.round()
			if e.chainLog(0).State != ledgerpub.StateDiverged {
				t.Errorf("after a restart: %s", e.chainLog(0).State)
			}
		})
	}
}

// A block that is missing from the range the group returns (a deleted block), an entry that is not a header, and a chain that does not link
// are divergences too: the log is stopped and the entries are not published.
func TestAnAnswerThatIsNotTheChainStopsTheLog(t *testing.T) {
	cases := map[string]func(req ReadRequest, resp ReadResponse) ReadResponse{
		"a deleted block": func(req ReadRequest, resp ReadResponse) ReadResponse {
			if req.Op == OpEntries && len(resp.Entries) > 3 {
				resp.Entries = append(append([]EntryRow(nil), resp.Entries[:2]...), resp.Entries[3:]...)
			}
			return resp
		},
		"an entry that is not a header": func(req ReadRequest, resp ReadResponse) ReadResponse {
			if req.Op == OpEntries && len(resp.Entries) > 3 {
				resp.Entries = append([]EntryRow(nil), resp.Entries...)
				resp.Entries[2].Entry = []byte("not a block header")
			}
			return resp
		},
		"blocks that do not link": func(req ReadRequest, resp ReadResponse) ReadResponse {
			if req.Op == OpEntries && len(resp.Entries) > 3 {
				resp.Entries = append([]EntryRow(nil), resp.Entries...)
				e := append([]byte(nil), resp.Entries[2].Entry...)
				e[20] ^= 0xff // prev_hash
				resp.Entries[2].Entry = e
			}
			return resp
		},
	}
	for name, mangle := range cases {
		t.Run(name, func(t *testing.T) {
			a := newChain(1, 3)
			e := newEnv(t, a)
			e.round()
			a.add(6)
			mangled := &mangledSource{inner: e.src, f: mangle}
			c, _ := New(Config{Store: e.store, Source: mangled, Keys: e.keys, Clock: func() time.Time { return e.now }})
			if _, err := c.Round(bg, "shop", OpHead); err != nil {
				t.Fatal(err)
			}
			l := e.chainLog(0)
			if l.State != ledgerpub.StateDiverged || l.Size != 3 {
				t.Fatalf("state %s size %d (%s)", l.State, l.Size, l.Detail)
			}
		})
	}
}

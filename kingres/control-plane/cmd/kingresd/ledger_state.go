// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerround"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerstate"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/membership"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
)

// pullState asks the other holders of the keys for their record of what was published and returns the union of what they answered (P8-01): the
// first start of a server that has none, before it takes what its database holds as the starting point.
func pullState(members *membership.Manager, holders func(context.Context) []string, log *slog.Logger) func(context.Context) (ledgerstate.State, bool) {
	return func(ctx context.Context) (ledgerstate.State, bool) {
		if members == nil {
			return ledgerstate.State{}, false
		}
		var out ledgerstate.State
		got := false
		for _, h := range holders(ctx) {
			var s ledgerstate.State
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := members.Client.Do(cctx, h, "POST", "/ledger/state/get", struct{}{}, &s)
			cancel()
			if err != nil {
				continue
			}
			u, err := ledgerstate.Union(out, s)
			if err != nil {
				log.Error("two holders disagree about what was published", "error", err.Error())
				continue
			}
			out, got = u, true
		}
		if out.Logs == nil {
			out.Logs = map[string]ledgerstate.Mark{}
		}
		return out, got
	}
}

// stateSyncJob sends the record of what was published to the holders at every pass (they merge it; a server that becomes the leader later then
// knows what was published).
func stateSyncJob(f *ledgerstate.File, members *membership.Manager, holders func(context.Context) []string, log *slog.Logger) scheduler.Job {
	return scheduler.Job{Name: "ledger.state.sync", Every: ledgerround.Tick, Run: func(ctx context.Context, _ scheduler.Term) error {
		if members == nil || !f.Initialized() {
			return nil
		}
		snap := f.Snapshot()
		for _, h := range holders(ctx) {
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := members.Client.Do(cctx, h, "POST", "/ledger/state/merge", snap, nil)
			cancel()
			if err != nil {
				log.Debug("a holder did not take the record of what was published", "server", h, "error", err.Error())
			}
		}
		return nil
	}}
}

// missingFor is the time every group must say that a database does not exist before the publication of its chains ends (default 30
// minutes). KINGRES_LEDGER_MISSING_FOR (a duration such as 90s) shortens it for tests and for sites that want another value; it is not a flag.
func missingFor(log *slog.Logger) time.Duration {
	v := os.Getenv("KINGRES_LEDGER_MISSING_FOR")
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < time.Second {
		log.Warn("KINGRES_LEDGER_MISSING_FOR is not a duration of at least a second: the default is used")
		return 0
	}
	return d
}

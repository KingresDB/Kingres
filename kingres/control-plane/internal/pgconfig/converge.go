// SPDX-License-Identifier: AGPL-3.0-only

package pgconfig

import (
	"context"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
)

// convergeEvery is how often the leader looks for a server whose override file is not the one the
// stored values give (a server that joined, changed its server group or came back from a failed
// apply); retryAfter keeps a server that fails again from being tried over and over.
const (
	convergeEvery = 30 * time.Second
	retryAfter    = 10 * time.Minute
)

// Run is the leader's loop that brings servers up to date. A server that was down during a change
// is already waited for by the task of that change (it never gives up); this loop covers the rest:
// a new server, a server that moved to another server group, a hand-edited file.
func (l *Leader) Run(ctx context.Context) {
	last := map[string]time.Time{}
	t := time.NewTicker(convergeEvery)
	defer t.Stop()
	every := l.ClockEvery
	if every == 0 {
		every = 10 * time.Second
	}
	ck := time.NewTicker(every)
	defer ck.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ck.C:
			l.CheckClocks(ctx)
			continue
		case <-t.C:
		}
		if l.IsLeader == nil || !l.IsLeader(ctx) || l.busy(ctx) {
			continue
		}
		var need []string
		sn, err := l.snapshot(ctx)
		if err != nil {
			continue
		}
		for _, s := range sn.servers {
			st := sn.states[s.ID]
			if st == nil {
				continue // unreachable servers are the business of the task that waits for them
			}
			req, err := l.buildRequest(ctx, s.ID)
			if err != nil {
				continue
			}
			text, err := instance.RenderConfig(0, req.Values)
			if err != nil || HashOf(text) == st.ConfigHash {
				continue
			}
			if time.Since(last[s.ID]) < retryAfter {
				continue
			}
			last[s.ID] = time.Now()
			need = append(need, s.ID)
		}
		if len(need) == 0 {
			continue
		}
		l.Log.Info("bringing servers up to date with the stored configuration", "servers", need)
		if _, err := l.StartTask(ctx, KindApply, SubjectType, SubjectApply, applyInput{Actor: "system", Servers: need}, "system"); err != nil {
			l.Log.Warn("starting the task that brings servers up to date failed", "error", err)
		}
	}
}

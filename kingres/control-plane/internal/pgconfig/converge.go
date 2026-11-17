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
	retryAfter    = time.Minute
	// maxRetryAfter is the longest wait between two attempts for a server that keeps failing (ST-08).
	maxRetryAfter = 10 * time.Minute
)

// Run is the leader's loop that brings servers up to date. A server that was down during a change
// is already waited for by the task of that change (it never gives up); this loop covers the rest:
// a new server, a server that moved to another server group, a hand-edited file.
func (l *Leader) Run(ctx context.Context) {
	last := map[string]attempt{}
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
			if err != nil {
				continue
			}
			if HashOf(text) == st.ConfigHash {
				delete(last, s.ID) // up to date: an earlier attempt worked, the next difference is a new matter
				continue
			}
			// The wait between two attempts is for an attempt that did not work: a server that is behind
			// a configuration that was not tried yet (its group changed since) is brought up to date at once.
			h := HashOf(text)
			p, seen := last[s.ID]
			if seen && p.hash == h && time.Since(p.at) < retryWait(p.tries) {
				continue
			}
			tries := 1
			if seen && p.hash == h {
				tries = p.tries + 1
			}
			last[s.ID] = attempt{at: time.Now(), hash: h, tries: tries}
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

// attempt is the last time a server was brought up to date, and the configuration it was brought up to.
type attempt struct {
	at   time.Time
	hash string
	// tries counts the attempts made for this configuration without the server ever being seen up to date.
	tries int
}

// retryWait is the wait before the next attempt for a server that was tried tries times for the same
// configuration and is still not up to date: one minute, then twice as long each time, at most ten minutes
// (ST-08: a server that can never apply the configuration must not cost a task every minute).
func retryWait(tries int) time.Duration {
	d := retryAfter
	for i := 1; i < tries && d < maxRetryAfter; i++ {
		d *= 2
	}
	if d > maxRetryAfter {
		d = maxRetryAfter
	}
	return d
}
